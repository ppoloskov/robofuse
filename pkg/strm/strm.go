package strm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/robofuse/robofuse/internal/config"
	"github.com/robofuse/robofuse/internal/logger"
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

// Service handles STRM file generation
type Service struct {
	config   *config.Config
	logger   zerolog.Logger
	tracking *tracking.Service
}

// New creates a new STRM service
func New(cfg *config.Config) *Service {
	return &Service{
		config:   cfg,
		logger:   logger.New("strm"),
		tracking: tracking.New(cfg.TrackingFile),
	}
}

// SyncResult contains the results of a sync operation
type SyncResult struct {
	Added   int
	Updated int
	Deleted int
	Skipped int
	Renamed int // renamed files (detected, not recreated)
	Tracked int
}

// Sync synchronizes STRM files with the candidate list.
// Rename detection: if a candidate's stable Link matches an existing .strm
// file at a different path, the file was renamed outside robofuse. We update
// the tracking path and skip creation instead of making a duplicate.
func (s *Service) Sync(candidates []realdebrid.STRMCandidate, dryRun bool) (*SyncResult, error) {
	result := &SyncResult{}

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
	expected := make(map[string]string)                       // relativePath → downloadURL
	candidateMap := make(map[string]realdebrid.STRMCandidate) // relativePath → candidate
	candidateLinkMap := make(map[string]string)               // Link → expectedPath (for rename detection)

	for _, c := range candidates {
		path := s.buildSTRMPath(c.TorrentFolder, c.Filename)
		expected[path] = c.DownloadURL
		candidateMap[path] = c
		if c.Link != "" {
			candidateLinkMap[c.Link] = path
		}
	}

	// Step 2b: Build a link→existingPath index from scanned files.
	linkToExistingPath := make(map[string]string) // Link → actual path on disk
	for path, ef := range existing {
		if ef.Link != "" {
			if _, seen := linkToExistingPath[ef.Link]; !seen {
				linkToExistingPath[ef.Link] = path
			}
		}
	}

	// Track which existing paths we've accounted for.
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
					s.tracking.MovePath(path, actualPath)
					ef := existing[actualPath]
					if ef.URL != url {
						s.writeSTRM(actualPath, url, candidate.Link, candidate.TorrentID)
						s.tracking.Track(actualPath, url, candidate.Link, candidate.TorrentID)
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

	// Step 4: Delete true orphans
	for path := range existing {
		if accountedExisting[path] {
			continue
		}
		if ef := existing[path]; ef.Link != "" {
			if _, matched := candidateLinkMap[ef.Link]; matched {
				s.logger.Warn().
					Str("path", path).
					Str("link", ef.Link).
					Msg("Orphan file matches a candidate Link — keeping (possible race)")
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
func (s *Service) scanExisting() (map[string]existingFile, error) {
	existing := make(map[string]existingFile)

	err := filepath.Walk(s.config.OutputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".strm") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
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

// buildSTRMPath builds the relative path for a STRM file
func (s *Service) buildSTRMPath(folderName, filename string) string {
	folder := sanitizeFilename(folderName)
	file := sanitizeFilename(filename)

	ext := filepath.Ext(file)
	strmName := strings.TrimSuffix(file, ext) + ".strm"

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

// sanitizeFilename makes a filename safe for the filesystem with enhanced cleaning
func sanitizeFilename(name string) string {
	for i := 0; i < 3; i++ {
		decoded := urlDecode(name)
		if decoded == name {
			break
		}
		name = decoded
	}

	name = removeSitePrefixes(name)

	ext := filepath.Ext(name)
	baseName := strings.TrimSuffix(name, ext)

	baseName = strings.ReplaceAll(baseName, ".", " ")
	baseName = strings.ReplaceAll(baseName, "_", " ")
	baseName = strings.ReplaceAll(baseName, "-", " ")

	baseName = strings.Join(strings.Fields(baseName), " ")

	if len(baseName) > 200 {
		words := strings.Fields(baseName)
		truncated := ""
		for _, word := range words {
			testLen := len(truncated)
			if truncated != "" {
				testLen += 1
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

	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	)
	baseName = replacer.Replace(baseName)

	baseName = strings.TrimSpace(baseName)

	return baseName + ext
}

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

func removeSitePrefixes(s string) string {
	prefixPattern := `^(hhd\d+\.com@|hdd\d+\.com@|www\.[\w-]+\.com@|[\w-]+\.com@)`
	re := regexp.MustCompile(prefixPattern)
	return re.ReplaceAllString(s, "")
}
