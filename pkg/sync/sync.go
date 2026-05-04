package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sourcegraph/conc/pool"
	ptt "github.com/itsrenoria/ptt-go"
	"github.com/robofuse/robofuse/internal/config"
	"github.com/robofuse/robofuse/internal/console"
	"github.com/robofuse/robofuse/internal/logger"
	"github.com/robofuse/robofuse/internal/request"
	"github.com/robofuse/robofuse/pkg/organizer"
	"github.com/robofuse/robofuse/pkg/namefmt"
	"github.com/robofuse/robofuse/pkg/realdebrid"
	"github.com/robofuse/robofuse/pkg/repair"
	"github.com/robofuse/robofuse/pkg/retry"
	"github.com/robofuse/robofuse/pkg/strm"
	"github.com/robofuse/robofuse/pkg/tmdb"
	"github.com/rs/zerolog"
)

// sync.go orchestrates full sync cycles, watch mode, and summary reporting.

// Service orchestrates the entire sync process
type Service struct {
	rd            *realdebrid.Client
	repairService *repair.Service
	strmService   *strm.Service
	retryQueue    *retry.Queue
	config        *config.Config
	logger        zerolog.Logger
	tmdbClient    *tmdb.Client // nil if TMDB not configured
	// Reusable allocations for watch mode
	downloadMap map[string]*realdebrid.Download
	candidates  []realdebrid.STRMCandidate
}

// New creates a new sync service
func New(cfg *config.Config) *Service {
	rd := realdebrid.New(cfg)

	svc := &Service{
		rd:            rd,
		repairService: repair.New(rd, cfg),
		strmService:   strm.New(cfg),
		retryQueue:    retry.New(cfg.RetryQueueFile),
		config:        cfg,
		logger:        logger.New("sync"),
		downloadMap:   make(map[string]*realdebrid.Download),
		candidates:    make([]realdebrid.STRMCandidate, 0, 1024),
	}

	if cfg.TMDBAPIKey != "" {
		svc.tmdbClient = tmdb.New(cfg.TMDBAPIKey)
		svc.logger.Info().Msg("TMDB client initialized")
	}

	return svc
}

// RunResult contains the results of a sync run
type RunResult struct {
	TorrentsTotal      int
	TorrentsDownloaded int
	TorrentsDead       int
	TorrentsRepaired   int
	DownloadsTotal     int
	DownloadsAfter     int
	LinksUnrestricted  int
	LinksFailed        int
	LinksQueued        int
	STRMAdded          int
	STRMUpdated        int
	STRMDeleted        int
	STRMSkipped        int
	STRMRenamed        int
	Duration           time.Duration
	// Organizer results
	OrgProcessed int
	OrgNew       int
	OrgDeleted   int
	OrgUpdated   int
	OrgErrors    int
}

// Run executes the sync process
func (s *Service) Run(dryRun bool) (*RunResult, error) {
	startTime := time.Now()
	result := &RunResult{}

	s.logger.Debug().Msg("Starting sync...")

	// Step 1: Fetch all torrents
	s.logger.Debug().Msg("Fetching torrents...")
	downloaded, dead, err := s.rd.GetTorrents()
	if err != nil {
		return nil, fmt.Errorf("fetching torrents: %w", err)
	}
	result.TorrentsDownloaded = len(downloaded)
	result.TorrentsDead = len(dead)
	result.TorrentsTotal = result.TorrentsDownloaded + result.TorrentsDead

	// Step 1b: Populate original filenames for better folder naming
	if !dryRun {
		s.rd.PopulateOriginalFilenames(downloaded)
	}

	// Step 2: Process retry queue (cross-cycle retries)
	if !dryRun {
		retryStats := s.processRetryQueue(downloaded)
		if retryStats.Succeeded > 0 {
			s.logger.Info().
				Int("succeeded", retryStats.Succeeded).
				Int("failed", retryStats.Failed).
				Msg("Retry queue processed")
		}
	}

	// Step 3: Repair dead torrents if enabled
	if s.config.RepairTorrents && len(dead) > 0 {
		s.logger.Debug().Int("count", len(dead)).Msg("Repairing dead torrents...")
		repaired, _ := s.repairService.RepairTorrents(dead, dryRun)
		result.TorrentsRepaired = repaired

		// Re-fetch torrents after repair
		if repaired > 0 && !dryRun {
			downloaded, _, err = s.rd.GetTorrents()
			if err != nil {
				s.logger.Warn().Err(err).Msg("Failed to re-fetch torrents after repair")
			}
		}
	}

	// Step 4: Fetch all downloads
	s.logger.Debug().Msg("Fetching downloads...")
	downloads, err := s.rd.GetDownloads()
	if err != nil {
		return nil, fmt.Errorf("fetching downloads: %w", err)
	}
	result.DownloadsTotal = len(downloads)

	// Step 4: Build link -> download map (reuse existing map)
	s.logger.Debug().Msg("Matching torrents to downloads...")
	clear(s.downloadMap)
	for _, d := range downloads {
		s.downloadMap[d.Link] = d
	}

	// Step 5: Find links needing unrestriction
	var missingLinks []missingLink
	for _, torrent := range downloaded {
		for _, link := range torrent.Links {
			if _, exists := s.downloadMap[link]; !exists {
				missingLinks = append(missingLinks, missingLink{
					torrent: torrent,
					link:    link,
				})
			}
		}
	}

	s.logger.Debug().
		Int("total_torrent_links", countTotalLinks(downloaded)).
		Int("existing_downloads", len(s.downloadMap)).
		Int("missing", len(missingLinks)).
		Msg("Link matching complete")

	if logger.IsInfoEnabled() {
		s.logger.Info().Msgf("discovery | torrents_downloaded=%d torrents_dead=%d downloads_cached=%d missing_links=%d",
			result.TorrentsDownloaded, result.TorrentsDead, result.DownloadsTotal, len(missingLinks))
		if logger.IsTTY() {
			fmt.Println()
		}
	}

	// Step 6: Unrestrict missing links
	if len(missingLinks) > 0 {
		s.logger.Debug().Int("count", len(missingLinks)).Msg("Unrestricting missing links...")

		unrestricted, failed, queued := s.unrestrictLinks(missingLinks, dryRun)
		result.LinksUnrestricted = len(unrestricted)
		result.LinksFailed = len(failed)
		result.LinksQueued = queued

		// Add new downloads to map
		for _, d := range unrestricted {
			s.downloadMap[d.Link] = d
		}

		// Handle failed links - mark torrents for repair
		if len(failed) > 0 && s.config.RepairTorrents && !dryRun {
			failedTorrents := s.findTorrentsForLinks(downloaded, failed)
			if len(failedTorrents) > 0 {
				s.logger.Debug().Int("count", len(failedTorrents)).Msg("Repairing torrents with failed links...")
				s.repairService.RepairTorrents(failedTorrents, dryRun)
			}
		}
	}

	result.DownloadsAfter = len(s.downloadMap)

	// Step 7: Build STRM candidates (reuse existing slice)
	s.logger.Debug().Msg("Building STRM candidates...")
	var stats candidateStats
	s.candidates = s.buildCandidatesInto(downloaded, s.downloadMap, s.candidates[:0], &stats)
	s.logger.Debug().Int("count", len(s.candidates)).Msg("STRM candidates ready")

	if logger.IsInfoEnabled() {
		s.logger.Info().Msgf("strm_sync | candidates=%d filtered_small=%d filtered_other=%d", stats.Candidates, stats.FilteredSmall, stats.FilteredOther)
	}

	// Step 7b: Fetch RD media info for classification (async, bounded)
	if !dryRun {
		s.fetchMediaInfos(s.candidates)
	}

	// Step 7c: Match against TMDB for official titles and metadata
	if !dryRun && s.tmdbClient != nil {
		s.matchTMDB(s.candidates)
	}

	// Step 7d: Apply filename templates to candidates
	s.applyNameTemplates(s.candidates)

	// Step 8: Sync STRM files
	s.logger.Debug().Msg("Syncing STRM files...")
	strmResult, err := s.strmService.Sync(s.candidates, dryRun)
	if err != nil {
		return nil, fmt.Errorf("syncing STRM files: %w", err)
	}
	result.STRMAdded = strmResult.Added
	result.STRMUpdated = strmResult.Updated
	result.STRMDeleted = strmResult.Deleted
	result.STRMSkipped = strmResult.Skipped
	result.STRMRenamed = strmResult.Renamed
	if logger.IsInfoEnabled() {
		renamedPart := ""
		if strmResult.Renamed > 0 {
			renamedPart = fmt.Sprintf(" renamed=%d", strmResult.Renamed)
		}
		s.logger.Info().Msgf("strm_results | created=%d updated=%d removed=%d unchanged=%d%s tracked=%d",
			result.STRMAdded, result.STRMUpdated, result.STRMDeleted, result.STRMSkipped, renamedPart, strmResult.Tracked)
		if logger.IsTTY() {
			fmt.Println()
		}
	}

	result.Duration = time.Since(startTime)

	s.logger.Debug().
		Int("strm_added", result.STRMAdded).
		Int("strm_updated", result.STRMUpdated).
		Int("strm_deleted", result.STRMDeleted).
		Int("strm_renamed", result.STRMRenamed).
		Dur("duration", result.Duration).
		Msg("Sync completed")

	// PTT Rename / Organize
	if s.config.PttRename && !dryRun {
		orgResult := s.runOrganizer()
		result.OrgProcessed = orgResult.Processed
		result.OrgNew = orgResult.New
		result.OrgDeleted = orgResult.Deleted
		result.OrgUpdated = orgResult.Updated
		result.OrgErrors = orgResult.Errors
		if logger.IsInfoEnabled() {
			s.logger.Info().Msgf("organizer | processed=%d created=%d updated=%d removed=%d skipped=%d errors=%d",
				orgResult.Processed, orgResult.New, orgResult.Updated, orgResult.Deleted, orgResult.Skipped, orgResult.Errors)
		}
	}

	// Refresh expiring links (works in both manual and watch mode)
	if !dryRun {
		interval := time.Duration(s.config.WatchModeInterval) * time.Second
		s.refreshExpiringLinks(interval)
	}

	return result, nil

}

// Watch runs the sync process in a loop until the context is cancelled.
func (s *Service) Watch(ctx context.Context) error {
	interval := time.Duration(s.config.WatchModeInterval) * time.Second

	s.logger.Info().
		Dur("interval", interval).
		Msg("Starting watch mode")

	for {
		select {
		case <-ctx.Done():
			s.logger.Info().Msg("Shutting down watch mode gracefully")
			s.WaitForProbes()
			return nil
		default:
		}

		result, err := s.Run(false)
		if err != nil {
			s.logger.Error().Err(err).Msg("Sync failed")
		} else {
			s.printCycleSummary(result, interval)
		}

		s.logger.Info().
			Time("next_run", time.Now().Add(interval)).
			Msg("Waiting for next cycle")

		select {
		case <-ctx.Done():
			s.logger.Info().Msg("Shutting down during sleep")
			s.WaitForProbes()
			return nil
		case <-time.After(interval):
		}
	}
}

// WaitForProbes blocks until all in-flight ffprobe jobs complete.
// Call before exiting in single-run mode.
func (s *Service) WaitForProbes() {
	s.strmService.WaitForProbes()
}

// refreshExpiringLinks refreshes links that will expire before the next run
func (s *Service) refreshExpiringLinks(interval time.Duration) {
	// Get files older than configured expiry days
	expiryDuration := time.Duration(s.config.FileExpiryDays) * 24 * time.Hour
	expiredFiles := s.strmService.GetExpiredFiles(expiryDuration)

	if len(expiredFiles) == 0 {
		return
	}

	s.logger.Info().Int("count", len(expiredFiles)).Msg("Refreshing expired links")

	var refreshed, failed int
	for _, tracking := range expiredFiles {
		// Unrestrict the original link to get a fresh download URL
		download, err := s.rd.UnrestrictLink(tracking.Link, tracking.RelativePath)
		if err != nil {
			s.logger.Warn().
				Err(err).
				Str("path", tracking.RelativePath).
				Msg("Failed to refresh expired link")
			failed++
			continue
		}

		// Update the STRM file with the new URL
		if err := s.strmService.UpdateSTRM(tracking.RelativePath, download.Download, tracking.Link, tracking.TorrentID); err != nil {
			s.logger.Warn().
				Err(err).
				Str("path", tracking.RelativePath).
				Msg("Failed to update STRM file")
			failed++
		} else {
			refreshed++
		}
	}

	if refreshed > 0 {
		s.logger.Info().
			Int("refreshed", refreshed).
			Int("failed", failed).
			Msg("Link refresh completed")
	}
}

// printCycleSummary prints a clean cycle summary to stdout
func (s *Service) printCycleSummary(result *RunResult, interval time.Duration) {
	summary := FormatSummary(result, SummaryOptions{
		IncludeOrg: s.config.PttRename,
		NextRun:    time.Now().Add(interval),
	})

	if logger.IsInfoEnabled() {
		s.logger.Info().Msg(summary)
	} else {
		fmt.Println(summary)
	}
}

// missingLink represents a link that needs unrestriction
type missingLink struct {
	torrent *realdebrid.Torrent
	link    string
}

// unrestrictLinks unrestricts multiple links concurrently.
// Implements a circuit breaker: when consecutive 503/429 errors exceed
// a threshold, all workers pause briefly to let the server recover,
// preventing self-inflicted thundering herds.
func (s *Service) unrestrictLinks(links []missingLink, dryRun bool) ([]*realdebrid.Download, []string, int) {
	if dryRun {
		s.logger.Info().Int("count", len(links)).Msg("[DRY-RUN] Would unrestrict links")
		return nil, nil, 0
	}

	const (
		// Circuit breaker: if this many consecutive errors occur across workers,
		// pause all new requests to let the server recover.
		circuitBreakerThreshold = 10
		circuitCooldown         = 30 * time.Second
	)

	var mu sync.Mutex
	var results []*realdebrid.Download
	var failed []string
	completed := 0
	queued := 0

	// Per-torrent tracking for auto-repair decisions
	type torrentStats struct {
		attempted int
		code1924  int // failed with RD error code 19 or 24 (torrent data purged/nerfed)
	}
	torrentAttempts := make(map[string]*torrentStats) // torrentID → stats

	// Circuit breaker state: atomically tracked consecutive errors and cooldown deadline.
	var consecutiveErrors atomic.Int64
	var cooldownUntil atomic.Int64 // unix timestamp, 0 = not in cooldown

	var progress *console.ProgressBar
	if logger.IsInfoEnabled() && logger.IsTTY() && !logger.IsDebugEnabled() {
		progress = console.NewProgressBar("Unrestricting links", len(links))
		progress.Update(0)
	}

	concPool := pool.New().WithMaxGoroutines(s.config.ConcurrentRequests)

	for _, ml := range links {
		ml := ml // capture
		concPool.Go(func() {
			// --- Circuit breaker: check if we're in cooldown ---
			if cd := cooldownUntil.Load(); cd > 0 {
				remaining := time.Until(time.Unix(cd, 0))
				if remaining > 0 {
					s.logger.Warn().
						Dur("remaining", remaining).
						Msg("Circuit breaker open, pausing before request")
					time.Sleep(remaining)
				}
			}

			download, err := s.rd.UnrestrictLink(ml.link, ml.torrent.Filename)

			mu.Lock()
			defer mu.Unlock()

			completed++
			if err != nil {
				// Track per-torrent failures for auto-repair decisions
				if ml.torrent != nil && ml.torrent.ID != "" {
					ts, ok := torrentAttempts[ml.torrent.ID]
					if !ok {
						ts = &torrentStats{}
						torrentAttempts[ml.torrent.ID] = ts
					}
					ts.attempted++
					var httpErr *request.HTTPError
					if errors.As(err, &httpErr) && httpErr.RDErrorCode == 19 {
						ts.code1924++
					}
				}

				// Track consecutive errors for circuit breaker
				if isRetryableError(err) {
					consec := consecutiveErrors.Add(1)
					if consec >= circuitBreakerThreshold {
						cd := time.Now().Add(circuitCooldown).Unix()
						if cooldownUntil.Swap(cd) == 0 {
							s.logger.Warn().
								Int64("consecutive_errors", consec).
								Dur("cooldown", circuitCooldown).
								Msg("Circuit breaker tripped — too many consecutive server errors, pausing all workers")
						}
					}

					// Add to retry queue for next cycle
					s.addToRetryQueue(ml.link, ml.torrent, err)
					queued++
					// Log RD error detail if available
					logEvt := s.logger.Debug().
						Str("filename", ml.torrent.Filename)
					var httpErr *request.HTTPError
					if errors.As(err, &httpErr) && httpErr.RDErrorCode != 0 {
						logEvt.Int("rd_error_code", httpErr.RDErrorCode).
							Str("rd_error", httpErr.RDError)
					}
					logEvt.Msg("Added to retry queue (retryable error)")
				} else {
					// Non-retryable error resets the circuit
					consecutiveErrors.Store(0)
				}

				failed = append(failed, ml.link)
				if !errors.Is(err, request.HosterUnavailableError) && !errors.Is(err, request.TrafficExceededError) {
					s.logger.Debug().Err(err).Msg("Failed to unrestrict link")
				}
			} else {
				// Success resets the circuit breaker
				consecutiveErrors.Store(0)
				results = append(results, download)
			}

			if progress != nil {
				progress.Update(completed)
			} else if completed%100 == 0 || completed == len(links) {
				s.logger.Info().
					Int("completed", completed).
					Int("total", len(links)).
					Int("success", len(results)).
					Int("failed", len(failed)).
					Msg("Unrestriction progress")
			}
		})
	}

	concPool.Wait()

	// Auto-repair: if repair_torrents is enabled, repair torrents where
	// ALL attempted links failed with RD error code 19 (torrent data not cached).
	// Re-adding the magnet causes RD to re-download and re-cache the torrent.
	if !dryRun && s.config.RepairTorrents && len(torrentAttempts) > 0 {
		var repairCandidates []*realdebrid.Torrent
		for torrentID, stats := range torrentAttempts {
			if stats.attempted > 0 && stats.attempted == stats.code1924 {
				for _, ml := range links {
					if ml.torrent != nil && ml.torrent.ID == torrentID {
						repairCandidates = append(repairCandidates, ml.torrent)
						break
					}
				}
			}
		}
		if len(repairCandidates) > 0 {
			s.logger.Info().
				Int("count", len(repairCandidates)).
				Msg("Auto-repairing torrents with code 19 failures (torrent data not cached)")
			repaired, _ := s.repairService.RepairTorrents(repairCandidates, dryRun)
			if repaired > 0 {
				s.logger.Info().
					Int("repaired", repaired).
					Msg("Torrents repaired — re-added magnets for re-caching")
			}
		}
	}

	// Save retry queue if any items were added
	if !dryRun && s.retryQueue.Count() > 0 {
		if err := s.retryQueue.Save(); err != nil {
			s.logger.Warn().Err(err).Msg("Failed to save retry queue")
		}
	}

	return results, failed, queued
}

type candidateStats struct {
	Candidates    int
	FilteredSmall int
	FilteredOther int
}

// buildCandidatesInto builds STRM candidates from torrents and downloads, reusing the provided slice.
func (s *Service) buildCandidatesInto(torrents []*realdebrid.Torrent, downloadMap map[string]*realdebrid.Download, candidates []realdebrid.STRMCandidate, stats *candidateStats) []realdebrid.STRMCandidate {
	minSize := s.config.MinFileSizeBytes()
	if stats != nil {
		stats.Candidates = 0
		stats.FilteredSmall = 0
		stats.FilteredOther = 0
	}

	for _, torrent := range torrents {
		for _, link := range torrent.Links {
			download, exists := downloadMap[link]
			if !exists {
				continue
			}

			// Check file type
			isVid := isVideo(download.Filename)
			isSub := isSubtitle(download.Filename)

			// Apply size filter ONLY to videos (not subtitles)
			if isVid && download.Filesize < minSize {
				if stats != nil {
					stats.FilteredSmall++
				}
				s.logger.Debug().
					Str("filename", download.Filename).
					Int64("size_mb", download.Filesize/(1024*1024)).
					Int64("min_mb", minSize/(1024*1024)).
					Msg("Skipping small video (likely ad/sample)")
				continue
			}

			// Skip non-video, non-subtitle files
			if !isVid && !isSub {
				if stats != nil {
					stats.FilteredOther++
				}
				s.logger.Debug().
					Str("filename", download.Filename).
					Msg("Skipping non-video, non-subtitle file")
				continue
			}

			folderName := torrent.Filename
			if torrent.OriginalFilename != "" {
				folderName = torrent.OriginalFilename
			}

			candidates = append(candidates, realdebrid.STRMCandidate{
				TorrentID:     torrent.ID,
				TorrentFolder: folderName,
				Filename:      download.Filename,
				DownloadURL:   download.Download,
				Link:          download.Link,
				Filesize:      download.Filesize,
			})
		}
	}

	if stats != nil {
		stats.Candidates = len(candidates)
	}
	return candidates
}

// findTorrentsForLinks finds torrents that contain the given failed links
func (s *Service) findTorrentsForLinks(torrents []*realdebrid.Torrent, failedLinks []string) []*realdebrid.Torrent {
	failedSet := make(map[string]bool)
	for _, link := range failedLinks {
		failedSet[link] = true
	}

	torrentSet := make(map[string]*realdebrid.Torrent)
	for _, torrent := range torrents {
		for _, link := range torrent.Links {
			if failedSet[link] {
				torrentSet[torrent.ID] = torrent
				break
			}
		}
	}

	result := make([]*realdebrid.Torrent, 0, len(torrentSet))
	for _, t := range torrentSet {
		result = append(result, t)
	}
	return result
}

// countTotalLinks counts total links across all torrents
func countTotalLinks(torrents []*realdebrid.Torrent) int {
	count := 0
	for _, t := range torrents {
		count += len(t.Links)
	}
	return count
}

// OrganizerResult contains stats from the organizer.
type OrganizerResult struct {
	Processed int `json:"processed"`
	New       int `json:"new"`
	Deleted   int `json:"deleted"`
	Updated   int `json:"updated"`
	Skipped   int `json:"skipped"`
	Errors    int `json:"errors"`
}

// fetchMediaInfos fetches RD media info (/streaming/mediaInfos/{id}) for each
// candidate. Uses a circuit breaker: after 20 consecutive failures, aborts the
// entire phase (the endpoint is likely unavailable). Failed files are marked
// in tracking and skipped on subsequent runs.
func (s *Service) fetchMediaInfos(candidates []realdebrid.STRMCandidate) {
	const maxConsecutiveFailures = 20
	const maxDuration = 60 * time.Second // total time budget for this phase

	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var skipped, fetched, failed int
	var mu sync.Mutex
	var consecutiveFails atomic.Int64
	aborted := false

	// Context with deadline to bound total phase duration
	ctx, cancel := context.WithTimeout(context.Background(), maxDuration)
	defer cancel()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				f, sk, fa := fetched, skipped, failed
				mu.Unlock()
				s.logger.Info().
					Int("fetched", f).
					Int("skipped", sk).
					Int("failed", fa).
					Msg("RD media info progress")
			}
		}
	}()

	for _, c := range candidates {
		if aborted {
			break
		}

		c := c
		dl, ok := s.downloadMap[c.Link]
		if !ok || dl.ID == "" {
			continue
		}

		path := s.strmService.BuildSTRMPath(c.TorrentFolder, c.Filename)

		// Skip if already have RD info or previously marked as failed
		if ft, ok := s.strmService.GetTracking(path); ok {
			if ft.RDType != "" || ft.RDMediaFailed {
				mu.Lock()
				skipped++
				mu.Unlock()
				continue
			}
		}

		// Circuit breaker: abort if too many consecutive failures
		if consecutiveFails.Load() >= maxConsecutiveFailures {
			s.logger.Warn().
				Int64("consecutive_failures", consecutiveFails.Load()).
				Msg("RD media info — too many consecutive failures, aborting fetch phase")
			aborted = true
			break
		}

		// Time budget exhausted — stop spawning new work
		if ctx.Err() != nil {
			aborted = true
			break
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			info, err := s.rd.GetMediaInfo(dl.ID)
			if err != nil || info == nil {
				consecutiveFails.Add(1)
				s.strmService.MarkRDMediaFailed(path)
				mu.Lock()
				failed++
				mu.Unlock()
				// Log first few failures at WARN for diagnostics
				cf := consecutiveFails.Load()
				if cf <= 5 {
					logEvt := s.logger.Warn().
						Int64("consecutive", cf).
						Str("id", dl.ID)
					if errors.Is(err, realdebrid.ErrMediaInfoUnavailable) {
						logEvt.Msg("RD media info unavailable (503)")
					} else if err != nil {
						logEvt.Err(err).Msg("RD media info fetch error")
					} else {
						logEvt.Msg("RD media info returned empty")
					}
				}
				return
			}

			consecutiveFails.Store(0) // reset on success
			s.strmService.SetRDInfo(path, info)
			mu.Lock()
			fetched++
			mu.Unlock()
		}()
	}

	wg.Wait()
	close(done)

	s.logger.Info().
		Int("fetched", fetched).
		Int("skipped", skipped).
		Int("failed", failed).
		Bool("aborted", aborted).
		Dur("elapsed", time.Since(start).Round(time.Second)).
		Msg("RD media info sync complete")
}

// matchTMDB searches TMDB at the torrent level — once per torrent folder —
// then applies the match to all candidates within that torrent. This prevents
// episode filenames from being searched individually (which returns garbage
// like specials/movies instead of the actual series).
func (s *Service) matchTMDB(candidates []realdebrid.STRMCandidate) {
	var matched, skipped, unmatched int
	start := time.Now()

	// Group candidates by torrent folder
	type group struct {
		folder     string
		candidates []realdebrid.STRMCandidate
	}
	groups := make(map[string]*group)
	for _, c := range candidates {
		key := c.TorrentFolder
		g, ok := groups[key]
		if !ok {
			g = &group{folder: key}
			groups[key] = g
		}
		g.candidates = append(g.candidates, c)
	}

	for _, g := range groups {
		// Skip adult folders — never match against TMDB
		if s.config.IsAdultFolder(g.folder) {
			s.logger.Debug().Str("folder", g.folder).Msg("TMDB skipped (adult folder)")
			skipped += len(g.candidates)
			continue
		}

		// Check if any candidate in this group already has a TMDB match
		allMatched := true
		anyMatched := false
		for _, c := range g.candidates {
			path := s.strmService.BuildSTRMPath(c.TorrentFolder, c.Filename)
			if ft, ok := s.strmService.GetTracking(path); ok && ft.TMDBID != 0 {
				anyMatched = true
			} else {
				allMatched = false
			}
		}
		if allMatched {
			skipped += len(g.candidates)
			continue
		}
		if anyMatched {
			// Partial match — some files already matched, others not.
			// This shouldn't happen in normal operation. Match the unmatched ones.
		}

		// Parse folder name for search terms
		folderParsed := ptt.Parse(filepath.Base(g.folder))
		searchTitle := folderParsed.Title
		if searchTitle == "" {
			searchTitle = filepath.Base(g.folder)
		}
		searchYear := folderParsed.Year

		// Fallback: if folder name is just a season marker (no show title),
		// try the torrent's original_filename from RD. Example: folder
		// "Сезон 1 (1984-1985)" → original_filename "Miami.Vice.S01.1080p".
		if len(folderParsed.Seasons) > 0 && (searchTitle == "" || isSeasonOnlyTitle(searchTitle)) {
			if len(g.candidates) > 0 {
				torrentID := g.candidates[0].TorrentID
				if info, err := s.rd.GetTorrentInfo(torrentID); err == nil && info != nil {
					origParsed := ptt.Parse(info.OriginalFilename)
					if origParsed.Title != "" {
						searchTitle = origParsed.Title
						s.logger.Debug().
							Str("folder", g.folder).
							Str("original_title", origParsed.Title).
							Msg("TMDB using original torrent filename for title")
					}
				}
			}
		}

		// Determine type: if the folder or any file has season/episode → show
		mediaType := "movie"
		if len(folderParsed.Seasons) > 0 || len(folderParsed.Episodes) > 0 || folderParsed.Anime {
			mediaType = "show"
		} else {
			// Check first candidate's filename
			for _, c := range g.candidates {
				fn := strings.TrimSuffix(c.Filename, filepath.Ext(c.Filename))
				p := ptt.Parse(fn)
				if len(p.Seasons) > 0 || len(p.Episodes) > 0 || p.Anime {
					mediaType = "show"
					break
				}
			}
		}

		// Also check RD classification for any candidate
		if mediaType == "movie" {
			for _, c := range g.candidates {
				path := s.strmService.BuildSTRMPath(c.TorrentFolder, c.Filename)
				if ft, ok := s.strmService.GetTracking(path); ok && ft.RDType == "show" {
					mediaType = "show"
					break
				}
			}
		}

		// For shows: folder name is the show title (don't override with
		// episode filename — PTT extracts episode titles like "Brother's Keeper").
		// For movies: filename often has a better title than the folder.
		if mediaType == "show" {
			// Only use folder-based parsing for shows
			if folderParsed.Title != "" {
				searchTitle = folderParsed.Title
			}
			if folderParsed.Year > 0 {
				searchYear = folderParsed.Year
			}
		} else if len(g.candidates) > 0 {
			// For movies, try filename parsing for better title
			c := g.candidates[0]
			fn := strings.TrimSuffix(c.Filename, filepath.Ext(c.Filename))
			parsed := ptt.Parse(fn)
			if parsed.Title != "" {
				searchTitle = parsed.Title
			}
			if parsed.Year > 0 {
				searchYear = parsed.Year
			}
		}

		// Check manual title overrides
		if override, ok := s.config.TitleOverrides[g.folder]; ok {
			searchTitle = override
		}

		match, err := s.tmdbClient.Match(mediaType, searchTitle, searchYear)
		// Fallback chain: retry without year, then retry with raw folder name
		if match == nil && searchYear > 0 {
			match, err = s.tmdbClient.Match(mediaType, searchTitle, 0)
		}
		if match == nil && searchTitle != g.folder {
			match, err = s.tmdbClient.Match(mediaType, g.folder, 0)
		}
		if err != nil {
			s.logger.Warn().
				Err(err).
				Str("search_title", searchTitle).
				Str("type", mediaType).
				Msg("TMDB match error")
			unmatched += len(g.candidates)
			continue
		}
		if match == nil {
			s.logger.Warn().
				Str("search_title", searchTitle).
				Str("type", mediaType).
				Int("year", searchYear).
				Msg("TMDB no match found")
			unmatched += len(g.candidates)
			continue
		}

		// Apply match to all candidates in this group
		for _, c := range g.candidates {
			path := s.strmService.BuildSTRMPath(c.TorrentFolder, c.Filename)
			s.strmService.SetTMDBMatch(path, match)
		}
		matched += len(g.candidates)

		s.logger.Info().
			Str("folder", g.folder).
			Str("tmdb_title", match.Title).
			Int("tmdb_id", match.TMDBID).
			Int("files", len(g.candidates)).
			Msg("TMDB match found")
	}

	s.logger.Info().
		Int("matched", matched).
		Int("skipped", skipped).
		Int("unmatched", unmatched).
		Dur("elapsed", time.Since(start).Round(time.Second)).
		Msg("TMDB matching complete")
}

// applyNameTemplates applies user-configured filename templates to candidates.
// Reads tracking for TMDB/RD/ffprobe metadata to populate template values.
func (s *Service) applyNameTemplates(candidates []realdebrid.STRMCandidate) {
	movieTpl := s.config.MovieNameTemplate
	epTpl := s.config.EpisodeNameTemplate
	if movieTpl == "" && epTpl == "" {
		return // no templates configured, use default naming
	}
	if movieTpl == "" {
		movieTpl = namefmt.DefaultMovie
	}
	if epTpl == "" {
		epTpl = namefmt.DefaultEpisode
	}

	for i := range candidates {
		c := &candidates[i]
		path := s.strmService.BuildSTRMPath(c.TorrentFolder, c.Filename)
		ft, hasTracking := s.strmService.GetTracking(path)

		v := namefmt.Values{
			Extension: filepath.Ext(c.Filename),
		}

		// PTT parse for season/episode
		fn := strings.TrimSuffix(c.Filename, filepath.Ext(c.Filename))
		parsed := ptt.Parse(fn)
		folderParsed := ptt.Parse(filepath.Base(c.TorrentFolder))

		v.Title = firstNonEmpty(parsed.Title, folderParsed.Title, filepath.Base(c.TorrentFolder))
		v.Year = firstNonZero(parsed.Year, folderParsed.Year)
		if len(parsed.Seasons) > 0 {
			v.Season = parsed.Seasons[0]
		} else if len(folderParsed.Seasons) > 0 {
			v.Season = folderParsed.Seasons[0]
		}
		if len(parsed.Episodes) > 0 {
			v.Episode = parsed.Episodes[0]
		}

		// TMDB overrides
		if hasTracking && ft.TMDBID != 0 {
			if ft.TMDBTitle != "" {
				v.Title = ft.TMDBTitle
			}
			if ft.TMDBOriginalTitle != "" {
				v.OriginalTitle = ft.TMDBOriginalTitle
			}
			if ft.TMDBYear > 0 {
				v.Year = ft.TMDBYear
			}
		}

		// Stream metadata from ffprobe
		if hasTracking && ft.Media != nil {
			m := ft.Media
			if len(m.Video) > 0 {
				v.Resolution = namefmt.ResolutionLabel(m.Resolution)
				v.HDR = namefmt.HDRLabel(m.Video[0].HDR)
				v.Codec = namefmt.CodecLabel(m.Video[0].Codec)
				if m.Video[0].BitRate > 0 {
					v.Bitrate = namefmt.BitrateMbps(m.Video[0].BitRate)
				} else if m.BitRate > 0 {
					v.Bitrate = namefmt.BitrateMbps(m.BitRate)
				}
			}
			if len(m.Audio) > 0 {
				var langs []string
				for _, a := range m.Audio {
					if a.Language != "" {
						langs = append(langs, a.Language)
					}
				}
				v.AudioLangs = namefmt.LangCodes(langs)
			}
		}

		// Determine type for template selection
		isEpisode := v.Season > 0 || v.Episode > 0
		if hasTracking && ft.RDType == "show" {
			isEpisode = true
		}
		if hasTracking && ft.TMDBType == "show" {
			isEpisode = true
		}

		var formatted string
		if isEpisode {
			formatted = namefmt.Format(epTpl, v)
		} else {
			formatted = namefmt.Format(movieTpl, v)
		}
		if formatted != "" {
			formatted = namefmt.Clean(formatted)
			c.Filename = formatted + v.Extension
		}
	}
}

// runOrganizer executes the Go organizer to organize files using ptt-go.
func (s *Service) runOrganizer() OrganizerResult {
	s.logger.Debug().Msg("Running library organizer...")

	org := organizer.New(organizer.Config{
		BaseDir:       s.config.Path,
		OrganizedDir:  s.config.OrganizedDir,
		OutputDir:     s.config.OutputDir,
		TrackingFile:  s.config.TrackingFile,
		CacheDir:      s.config.CacheDir,
		AdultPatterns: s.config.AdultPatterns,
		FolderRules:   convertFolderRules(s.config.FolderRules),
		Logger:        s.logger,
	})

	result := org.Run()

	s.logger.Debug().
		Int("processed", result.Processed).
		Int("new", result.New).
		Int("deleted", result.Deleted).
		Int("skipped", result.Skipped).
		Int("errors", result.Errors).
		Msg("Organizer completed")

	return OrganizerResult{
		Processed: result.Processed,
		New:       result.New,
		Deleted:   result.Deleted,
		Updated:   result.Updated,
		Skipped:   result.Skipped,
		Errors:    result.Errors,
	}
}

// convertFolderRules converts config folder rules to organizer format.
func convertFolderRules(rules []config.FolderRule) []organizer.FolderRule {
	result := make([]organizer.FolderRule, len(rules))
	for i, r := range rules {
		result[i] = organizer.FolderRule{
			Pattern:  r.Pattern,
			Target:   r.Target,
			SkipTMDB: r.SkipTMDB,
		}
	}
	return result
}

// isSeasonOnlyTitle returns true if the title is just a season indicator
// with no actual show name. Covers multiple languages.
func isSeasonOnlyTitle(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	switch t {
	case "season", "сезон", "staffel", "saison", "temporada", "stagione", "sezona", "sæson", "sesong":
		return true
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
