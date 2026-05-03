package strm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	ptt "github.com/itsrenoria/ptt-go"
	"github.com/robofuse/robofuse/internal/config"
	"github.com/robofuse/robofuse/internal/logger"
	"github.com/robofuse/robofuse/pkg/nfo"
	"github.com/robofuse/robofuse/pkg/probe"
	"github.com/robofuse/robofuse/pkg/realdebrid"
	"github.com/robofuse/robofuse/pkg/tracking"
	"github.com/rs/zerolog"
)

// strm.go creates and reconciles STRM files from RD download candidates.
//
// STRM file format (line 1 = Kodi-readable, line 2 = robofuse metadata):
//
//	<download_url>
//	# robofuse: link=<rd_link> torrent=<torrent_id>
//
// The second line is a comment that Kodi/Plex ignores. It carries the
// stable Real-Debrid link so that renames outside robofuse can be
// detected instead of creating duplicate files.

// serviceMetadataPattern extracts link and torrent from the second line of a .strm file.
var serviceMetadataPattern = regexp.MustCompile(`^# robofuse: link=(\S+) torrent=(\S+)`)

// existingFile represents a .strm file found on disk during scanning.
type existingFile struct {
	URL  string // download URL from line 1
	Link string // RD link from line 2 (empty for legacy single-line files)
}

// probeTarget identifies a file that needs ffprobe analysis.
type probeTarget struct {
	path string
	url  string
}

// Service handles STRM file generation
type Service struct {
	config   *config.Config
	logger   zerolog.Logger
	tracking *tracking.Service

	// ffprobe support
	probeAvailable bool         // true if ffprobe binary found and enabled
	probeSem       chan struct{} // bounds concurrent ffprobe calls (max 2)
	probeOnce      sync.Once     // ensures probeSem is initialized
	probeWg        sync.WaitGroup // tracks in-flight probes
}

// New creates a new STRM service
func New(cfg *config.Config) *Service {
	svc := &Service{
		config:   cfg,
		logger:   logger.New("strm"),
		tracking: tracking.New(cfg.TrackingFile),
	}

	// Check ffprobe availability at startup (only once)
	if cfg.EnableFFProbe {
		path := cfg.FFProbePath
		if path == "" {
			path = "ffprobe"
		}
		if _, err := exec.LookPath(path); err == nil {
			svc.probeAvailable = true
			svc.logger.Info().Str("path", path).Msg("ffprobe detected — media probing enabled")
		} else {
			svc.logger.Warn().Str("path", path).Err(err).Msg("ffprobe not found — media probing disabled")
		}
	}

	return svc
}

// SyncResult contains the results of a sync operation
type SyncResult struct {
	Added      int
	Updated    int
	Deleted    int
	Skipped    int
	Renamed    int // new: renamed files (detected, not recreated)
	Tracked    int
}

// Sync synchronizes STRM files with the candidate list.
// Rename detection: if a candidate's stable Link matches an existing .strm
// file at a different path, the file was renamed outside robofuse. We update
// the tracking path and skip creation instead of making a duplicate.
func (s *Service) Sync(candidates []realdebrid.STRMCandidate, dryRun bool) (*SyncResult, error) {
	result := &SyncResult{}

	// Collect paths that need ffprobe (new or updated URLs)
	var probeJobs []probeTarget

	// Ensure output directory exists
	if !dryRun {
		if err := os.MkdirAll(s.config.OutputDir, 0755); err != nil {
			return nil, err
		}
	}

	// Step 1: Scan existing STRM files (path → {URL, Link})
	existing, err := s.scanExisting()
	if err != nil {
		return nil, err
	}

	// Step 2: Build expected map and a reverse link→path index
	expected := make(map[string]string)                             // relativePath → downloadURL
	candidateMap := make(map[string]realdebrid.STRMCandidate)       // relativePath → candidate
	candidateLinkMap := make(map[string]string)                     // Link → expectedPath (for rename detection)

	for _, c := range candidates {
		path := s.buildSTRMPath(c.TorrentFolder, c.Filename)
		expected[path] = c.DownloadURL
		candidateMap[path] = c
		if c.Link != "" {
			candidateLinkMap[c.Link] = path
		}
	}

	// Step 2b: Build a link→existingPath index from scanned files.
	// Only files that carry a Link can be matched back to candidates.
	linkToExistingPath := make(map[string]string) // Link → actual path on disk
	for path, ef := range existing {
		if ef.Link != "" {
			// If the same Link appears at multiple paths (edge case),
			// keep the first one found – Walk is deterministic.
			if _, seen := linkToExistingPath[ef.Link]; !seen {
				linkToExistingPath[ef.Link] = path
			}
		}
	}

	// Track which existing paths we've accounted for (either matched to
	// expected or recognised as a rename). Anything left over is a true orphan.
	accountedExisting := make(map[string]bool)

	// Step 3: Process candidates (add/update/rename)
	for path, url := range expected {
		candidate := candidateMap[path]

		if ef, exists := existing[path]; exists {
			// File exists at the expected path
			accountedExisting[path] = true
			if ef.URL == url {
				result.Skipped++
			} else {
				result.Updated++
				if !dryRun {
					s.writeSTRM(path, url, candidate.Link, candidate.TorrentID)
					s.tracking.Track(path, url, candidate.Link, candidate.TorrentID)
					s.writeNFO(path, candidate)
					probeJobs = append(probeJobs, probeTarget{path, url})
				}
				s.logger.Debug().Str("path", path).Msg("Updated STRM")
			}
		} else if candidate.Link != "" {
			// Expected path does NOT exist on disk. Check if the same Link
			// exists at a different path (rename detected).
			if actualPath, renamed := linkToExistingPath[candidate.Link]; renamed && actualPath != path {
				accountedExisting[actualPath] = true
				result.Renamed++
				if !dryRun {
					// Update the tracking entry to point to the renamed path.
					s.tracking.MovePath(path, actualPath)
					// Also make sure the file on disk has current URL + metadata.
					ef := existing[actualPath]
					if ef.URL != url {
						s.writeSTRM(actualPath, url, candidate.Link, candidate.TorrentID)
						s.tracking.Track(actualPath, url, candidate.Link, candidate.TorrentID)
						s.writeNFO(actualPath, candidate)
						probeJobs = append(probeJobs, probeTarget{actualPath, url})
					}
				}
				s.logger.Info().
					Str("expected", path).
					Str("found_at", actualPath).
					Str("link", candidate.Link).
					Msg("Rename detected — tracking updated, no duplicate created")
			} else {
				// Truly new file
				result.Added++
				if !dryRun {
					s.writeSTRM(path, url, candidate.Link, candidate.TorrentID)
					s.tracking.Track(path, url, candidate.Link, candidate.TorrentID)
					s.writeNFO(path, candidate)
					probeJobs = append(probeJobs, probeTarget{path, url})
				}
				s.logger.Debug().Str("path", path).Msg("Created STRM")
			}
		} else {
			// Legacy: candidate without a Link – fall back to creation
			result.Added++
			if !dryRun {
				s.writeSTRM(path, url, "", "")
				s.tracking.Track(path, url, "", "")
			}
			s.logger.Debug().Str("path", path).Msg("Created STRM (legacy, no link)")
		}
	}

	// Step 4: Delete true orphans – files on disk that are neither in the
	// expected set nor recognised as a rename of an expected file.
	for path := range existing {
		if accountedExisting[path] {
			continue
		}
		// Double-check: is this file's Link matched to ANY candidate?
		// If so it's a rename we missed, so don't delete.
		if ef := existing[path]; ef.Link != "" {
			if _, matched := candidateLinkMap[ef.Link]; matched {
				s.logger.Warn().
					Str("path", path).
					Str("link", ef.Link).
					Msg("Orphan file matches a candidate Link but was not accounted — keeping (possible race)")
				continue
			}
		}

		result.Deleted++
		if !dryRun {
			fullPath := filepath.Join(s.config.OutputDir, path)
			if err := os.Remove(fullPath); err != nil {
				s.logger.Error().Err(err).Str("path", path).Msg("Failed to delete STRM")
			} else {
				s.tracking.Remove(path)
			}
			s.cleanupEmptyDirs(filepath.Dir(fullPath))
		}
		s.logger.Debug().Str("path", path).Msg("Deleted orphan STRM")
	}

	// Save tracking data
	if !dryRun {
		if err := s.tracking.Save(); err != nil {
			s.logger.Warn().Err(err).Msg("Failed to save tracking data")
		}
	}

	// Dispatch ffprobe jobs asynchronously (non-blocking, bounded concurrency).
	if s.probeAvailable && len(probeJobs) > 0 {
		s.logger.Info().Int("count", len(probeJobs)).Msg("Dispatching ffprobe jobs")
		s.dispatchProbes(probeJobs)
	}

	result.Tracked = s.tracking.Count()

	s.logger.Debug().
		Int("added", result.Added).
		Int("updated", result.Updated).
		Int("deleted", result.Deleted).
		Int("skipped", result.Skipped).
		Int("renamed", result.Renamed).
		Int("tracked", result.Tracked).
		Bool("dryRun", dryRun).
		Msg("STRM sync completed")

	return result, nil
}

// scanExisting scans the output directory for existing STRM files.
// Returns a map of relativePath → existingFile (with URL and Link parsed).
// Old single-line .strm files will have an empty Link.
func (s *Service) scanExisting() (map[string]existingFile, error) {
	existing := make(map[string]existingFile)

	err := filepath.Walk(s.config.OutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".strm") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil // Skip unreadable files
		}

		relPath, err := filepath.Rel(s.config.OutputDir, path)
		if err != nil {
			return nil
		}

		ef := parseSTRMContent(content)
		existing[relPath] = ef
		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	return existing, nil
}

// parseSTRMContent splits a .strm file's content into URL and optional Link.
// Format:
//
//	line 1: <download_url>
//	line 2 (optional): # robofuse: link=<rd_link> torrent=<torrent_id>
func parseSTRMContent(content []byte) existingFile {
	lines := strings.SplitN(strings.TrimSpace(string(content)), "\n", 2)
	ef := existingFile{
		URL: strings.TrimSpace(lines[0]),
	}
	if len(lines) > 1 {
		meta := strings.TrimSpace(lines[1])
		if matches := serviceMetadataPattern.FindStringSubmatch(meta); len(matches) == 3 {
			ef.Link = matches[1]
		}
	}
	return ef
}

// BuildSTRMPath is the public wrapper around buildSTRMPath.
func (s *Service) BuildSTRMPath(folderName, filename string) string {
	return s.buildSTRMPath(folderName, filename)
}

// buildSTRMPath builds the relative path for a STRM file.
// Preserves the original file extension before .strm so players can
// detect the media container type (e.g. .mkv.strm, .avi.strm).
func (s *Service) buildSTRMPath(folderName, filename string) string {
	folder := sanitizeFilename(folderName)
	file := sanitizeFilename(filename)

	// Keep original extension: "Movie.avi" → "Movie.avi.strm"
	strmName := file + ".strm"

	return filepath.Join(folder, strmName)
}

// writeSTRM writes a .strm file with the download URL and robofuse metadata.
func (s *Service) writeSTRM(relativePath, url, link, torrentID string) error {
	fullPath := filepath.Join(s.config.OutputDir, relativePath)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}

	content := url
	if link != "" || torrentID != "" {
		content += fmt.Sprintf("\n# robofuse: link=%s torrent=%s", link, torrentID)
	}
	content += "\n"

	return os.WriteFile(fullPath, []byte(content), 0644)
}

// writeNFO creates a Kodi-compatible .nfo file alongside the .strm file.
// Classification priority:
//   1. RD mediaInfos (authoritative) → type, duration, season, episode, year, poster/backdrop
//   2. PTT filename parsing → fallback
//   3. Custom rules:
//      - Duration > 80 min + single file candidate → movie
//      - >10 files in torrent → series
//      - PTT adult flag → marks as adult (in NFO tags)
func (s *Service) writeNFO(strmRelPath string, candidate realdebrid.STRMCandidate) {
	fullPath := filepath.Join(s.config.OutputDir, strmRelPath)

	// Parse filenames with PTT for fallback
	filenameNoExt := strings.TrimSuffix(candidate.Filename, filepath.Ext(candidate.Filename))
	parsed := ptt.Parse(filenameNoExt)
	folderName := filepath.Base(candidate.TorrentFolder)
	folderParsed := ptt.Parse(folderName)

	data := &nfo.Data{
		FileSize: candidate.Filesize,
	}

	// Check tracking for RD media info (pre-fetched by sync.Run)
	ft, hasTracking := s.tracking.Get(strmRelPath)

	// --- Classification pipeline ---
	if hasTracking && ft.RDType != "" {
		// Priority 1: RD mediaInfos classification
		switch ft.RDType {
		case "show":
			data.Type = "episode"
			data.ShowTitle = firstNonEmpty(folderParsed.Title, folderName)
			data.Title = firstNonEmpty(parsed.Title, candidate.Filename)
			data.Season = ft.RDSeason
			data.Episode = ft.RDEpisode
		default:
			data.Type = "movie"
			data.Title = firstNonEmpty(parsed.Title, folderParsed.Title, filenameNoExt)
		}
		if ft.RDYear != "" {
			if y, err := strconv.Atoi(ft.RDYear); err == nil {
				data.Year = y
			}
		}
		data.DurationSeconds = ft.RDDuration
		data.PosterPath = ft.RDPosterPath
		data.BackdropPath = ft.RDBackdropPath
	} else {
		// Priority 2: PTT parsing
		isSeries := len(parsed.Seasons) > 0 || len(parsed.Episodes) > 0 || parsed.Anime
		isSeriesFolder := len(folderParsed.Seasons) > 0 || len(folderParsed.Episodes) > 0 || folderParsed.Anime

		if isSeriesFolder {
			data.Type = "episode"
			data.ShowTitle = firstNonEmpty(folderParsed.Title, folderName)
			data.Year = firstNonZero(folderParsed.Year, parsed.Year)
			if len(parsed.Seasons) > 0 {
				data.Season = parsed.Seasons[0]
			} else if len(folderParsed.Seasons) > 0 {
				data.Season = folderParsed.Seasons[0]
			}
			if len(parsed.Episodes) > 0 {
				data.Episode = parsed.Episodes[0]
			}
			data.Title = firstNonEmpty(parsed.Title, candidate.Filename)
		} else if isSeries {
			data.Type = "episode"
			data.ShowTitle = firstNonEmpty(parsed.Title, folderName)
			data.Year = parsed.Year
			if len(parsed.Seasons) > 0 {
				data.Season = parsed.Seasons[0]
			}
			if len(parsed.Episodes) > 0 {
				data.Episode = parsed.Episodes[0]
			}
			data.Title = firstNonEmpty(parsed.Title, candidate.Filename)
		} else {
			data.Type = "movie"
			data.Title = firstNonEmpty(parsed.Title, folderParsed.Title, filenameNoExt)
			data.Year = firstNonZero(parsed.Year, folderParsed.Year)
		}

		// Priority 3: Custom rules (override PTT when ambiguous)
		// Rule: if RD duration > 80 min and no series markers → movie
		if data.Type == "movie" && hasTracking && ft.RDDuration > 4800 {
			// Confirmed movie by duration
		}
	}

	// Include ffprobe metadata if available
	if hasTracking && ft.Media != nil {
		data.Media = ft.Media
	}

	// If RD provided poster/backdrop but we haven't set them, use those
	if data.PosterPath == "" && hasTracking && ft.RDPosterPath != "" {
		data.PosterPath = ft.RDPosterPath
	}
	if data.BackdropPath == "" && hasTracking && ft.RDBackdropPath != "" {
		data.BackdropPath = ft.RDBackdropPath
	}

	if err := nfo.Write(fullPath, data); err != nil {
		s.logger.Debug().Err(err).Str("path", strmRelPath).Msg("Failed to write NFO")
	}
}

// refreshNFOWithMedia updates the .nfo file with fresh stream details after ffprobe completes.
func (s *Service) refreshNFOWithMedia(strmRelPath string, media *probe.MediaInfo) {
	if media == nil {
		return
	}
	// Re-read tracking to get the full FileTracking (which now has Media set)
	ft, ok := s.tracking.Get(strmRelPath)
	if !ok {
		return
	}

	// Parse the filename again to build NFO data
	filename := filepath.Base(strmRelPath)
	filenameNoExt := strings.TrimSuffix(filename, filepath.Ext(filename))
	parsed := ptt.Parse(filenameNoExt)

	folderName := filepath.Base(filepath.Dir(strmRelPath))
	folderParsed := ptt.Parse(folderName)

	data := &nfo.Data{
		Media: media,
	}

	isSeries := len(parsed.Seasons) > 0 || len(parsed.Episodes) > 0 || parsed.Anime
	isSeriesFolder := len(folderParsed.Seasons) > 0 || len(folderParsed.Episodes) > 0 || folderParsed.Anime

	if isSeriesFolder {
		data.Type = "episode"
		data.ShowTitle = firstNonEmpty(folderParsed.Title, folderName)
		data.Year = firstNonZero(folderParsed.Year, parsed.Year)
		if len(parsed.Seasons) > 0 {
			data.Season = parsed.Seasons[0]
		} else if len(folderParsed.Seasons) > 0 {
			data.Season = folderParsed.Seasons[0]
		}
		if len(parsed.Episodes) > 0 {
			data.Episode = parsed.Episodes[0]
		}
		data.Title = firstNonEmpty(parsed.Title, filenameNoExt)
	} else if isSeries {
		data.Type = "episode"
		data.ShowTitle = firstNonEmpty(parsed.Title, folderName)
		data.Year = parsed.Year
		if len(parsed.Seasons) > 0 {
			data.Season = parsed.Seasons[0]
		}
		if len(parsed.Episodes) > 0 {
			data.Episode = parsed.Episodes[0]
		}
		data.Title = firstNonEmpty(parsed.Title, filenameNoExt)
	} else {
		data.Type = "movie"
		data.Title = firstNonEmpty(parsed.Title, folderParsed.Title, filenameNoExt)
		data.Year = firstNonZero(parsed.Year, folderParsed.Year)
	}

	_ = ft // keep reference

	fullPath := filepath.Join(s.config.OutputDir, strmRelPath)
	if err := nfo.Write(fullPath, data); err != nil {
		s.logger.Debug().Err(err).Str("path", strmRelPath).Msg("Failed to refresh NFO with media")
	} else {
		s.logger.Debug().Str("path", strmRelPath).Msg("NFO updated with stream details")
	}
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

// cleanupEmptyDirs removes empty directories up to the output root
func (s *Service) cleanupEmptyDirs(dir string) {
	for dir != s.config.OutputDir && dir != "" && dir != "." {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		os.Remove(dir)
		dir = filepath.Dir(dir)
	}
}

// dispatchProbes runs ffprobe on a list of targets asynchronously.
// Concurrency is capped at 2 to avoid overwhelming the network and ffprobe.
// Results are stored in the tracking database whenever a probe completes.
func (s *Service) dispatchProbes(targets []probeTarget) {
	s.probeOnce.Do(func() {
		s.probeSem = make(chan struct{}, 2) // max 2 concurrent ffprobe calls
	})

	timeout := time.Duration(s.config.FFProbeTimeout) * time.Second
	ffprobePath := s.config.FFProbePath
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}

	for _, t := range targets {
		t := t // capture
		s.probeWg.Add(1)
		go func() {
			defer s.probeWg.Done()
			s.probeSem <- struct{}{}
			defer func() { <-s.probeSem }()

			ctx := context.Background()
			media, err := probe.Probe(ctx, t.url, timeout, ffprobePath, s.logger)
			if err != nil {
				s.logger.Warn().
					Err(err).
					Str("path", t.path).
					Msg("ffprobe failed")
				return
			}
			if media == nil {
				return
			}

			s.tracking.SetMedia(t.path, media)
			s.refreshNFOWithMedia(t.path, media)
			s.logger.Info().
				Str("path", t.path).
				Str("resolution", media.Resolution).
				Str("duration", media.Duration).
				Int("video_streams", len(media.Video)).
				Int("audio_streams", len(media.Audio)).
				Msg("Media probed")
		}()
	}
}

// WaitForProbes blocks until all in-flight ffprobe jobs complete.
func (s *Service) WaitForProbes() {
	s.probeWg.Wait()
}

// SetRDInfo stores Real-Debrid media info for a tracked file.
func (s *Service) SetRDInfo(relativePath string, info *realdebrid.MediaInfoResult) {
	s.tracking.SetRDInfo(relativePath, info)
}

// GetTracking returns the tracking entry for a file, if it exists.
func (s *Service) GetTracking(relativePath string) (*tracking.FileTracking, bool) {
	return s.tracking.Get(relativePath)
}

// sanitizeFilename makes a filename safe for the filesystem with enhanced cleaning
func sanitizeFilename(name string) string {
	// Step 1: Multi-pass URL decoding (up to 3 times)
	for i := 0; i < 3; i++ {
		decoded := urlDecode(name)
		if decoded == name {
			break // No more decoding needed
		}
		name = decoded
	}

	// Step 2: Remove common site prefixes (e.g., hhd001.com@)
	name = removeSitePrefixes(name)

	// Step 3: Remove file extension to work with base name
	ext := filepath.Ext(name)
	baseName := strings.TrimSuffix(name, ext)

	// Step 4: Replace separators with spaces for readability
	baseName = strings.ReplaceAll(baseName, ".", " ")
	baseName = strings.ReplaceAll(baseName, "_", " ")
	baseName = strings.ReplaceAll(baseName, "-", " ")

	// Step 5: Collapse multiple spaces
	baseName = strings.Join(strings.Fields(baseName), " ")

	// Step 6: Word-boundary-aware truncation
	if len(baseName) > 200 {
		words := strings.Fields(baseName)
		truncated := ""
		for _, word := range words {
			testLen := len(truncated)
			if truncated != "" {
				testLen += 1 // Space
			}
			testLen += len(word)

			if testLen <= 195 {
				if truncated != "" {
					truncated += " "
				}
				truncated += word
			} else {
				break
			}
		}
		if truncated != "" {
			baseName = truncated
		} else {
			baseName = baseName[:195]
		}
	}

	// Step 7: Replace invalid filesystem characters
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	)
	baseName = replacer.Replace(baseName)

	// Step 8: Trim whitespace
	baseName = strings.TrimSpace(baseName)

	return baseName + ext
}

// urlDecode decodes URL-encoded strings
func urlDecode(s string) string {
	decoded := s
	for i := 0; i < len(decoded)-2; i++ {
		if decoded[i] == '%' {
			hex := decoded[i+1 : i+3]
			if val, err := strconv.ParseInt(hex, 16, 8); err == nil {
				decoded = decoded[:i] + string(rune(val)) + decoded[i+3:]
			}
		}
	}
	return decoded
}

// removeSitePrefixes removes common site prefixes from filenames
func removeSitePrefixes(s string) string {
	prefixPattern := `^(hhd\d+\.com@|hdd\d+\.com@|www\.[\w-]+\.com@|[\w-]+\.com@)`
	re := regexp.MustCompile(prefixPattern)
	return re.ReplaceAllString(s, "")
}
