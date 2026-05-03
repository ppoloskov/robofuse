package tracking

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/natefinch/atomic"
	"github.com/robofuse/robofuse/internal/logger"
	"github.com/robofuse/robofuse/pkg/probe"
	"github.com/robofuse/robofuse/pkg/realdebrid"
	"github.com/robofuse/robofuse/pkg/tmdb"
	"github.com/rs/zerolog"
)

// tracking.go stores STRM lifecycle metadata for change detection and cleanup.

// FileTracking represents tracking data for a single STRM file
type FileTracking struct {
	RelativePath string           `json:"relative_path"`
	DownloadURL  string           `json:"download_url"`
	Link         string           `json:"link"`
	CreatedAt    time.Time        `json:"created_at"`
	LastChecked  time.Time        `json:"last_checked"`
	TorrentID    string           `json:"torrent_id"`
	Media        *probe.MediaInfo `json:"media,omitempty"` // ffprobe metadata (may be nil)

	// RD media info (from /streaming/mediaInfos/{id})
	RDType         string  `json:"rd_type,omitempty"`          // "movie", "show", "audio"
	RDSeason       int     `json:"rd_season,omitempty"`
	RDEpisode      int     `json:"rd_episode,omitempty"`
	RDYear         string  `json:"rd_year,omitempty"`
	RDDuration     float64 `json:"rd_duration,omitempty"`      // seconds
	RDBitrate      int     `json:"rd_bitrate,omitempty"`
	RDPosterPath   string  `json:"rd_poster_path,omitempty"`   // poster image URL
	RDBackdropPath string  `json:"rd_backdrop_path,omitempty"` // backdrop image URL
	RDMediaFailed  bool    `json:"rd_media_failed,omitempty"`  // true if mediaInfos returned 503

	// TMDB match (from themoviedb.org)
	TMDBID        int      `json:"tmdb_id,omitempty"`
	TMDBTitle     string   `json:"tmdb_title,omitempty"`       // official title
	TMDBOriginalTitle string `json:"tmdb_original_title,omitempty"` // original language title
	TMDBType      string   `json:"tmdb_type,omitempty"`        // "movie" or "show"
	TMDBYear      int      `json:"tmdb_year,omitempty"`
	TMDBOverview  string   `json:"tmdb_overview,omitempty"`
	TMDBPoster    string   `json:"tmdb_poster,omitempty"`
	TMDBBackdrop  string   `json:"tmdb_backdrop,omitempty"`
	TMDBRating    float64  `json:"tmdb_rating,omitempty"`
	TMDBGenres    []string `json:"tmdb_genres,omitempty"`
	IMDBID        string   `json:"imdb_id,omitempty"`        // IMDB ID (movies only)
	TMDBNFOGenerated bool  `json:"tmdb_nfo_generated,omitempty"`
}

// Service manages file tracking persistence
type Service struct {
	trackingFile string
	data         map[string]*FileTracking
	linkIndex    map[string]string // Link → RelativePath for O(1) lookup
	mu           sync.RWMutex
	logger       zerolog.Logger
}

// New creates a new tracking service
func New(trackingFile string) *Service {
	s := &Service{
		trackingFile: trackingFile,
		data:         make(map[string]*FileTracking),
		linkIndex:    make(map[string]string),
		logger:       logger.New("tracking"),
	}

	// Load existing data
	if err := s.Load(); err != nil {
		s.logger.Debug().Err(err).Msg("No existing tracking file, starting fresh")
	}

	return s
}

// Track records or updates tracking data for a file
func (s *Service) Track(relativePath, downloadURL, link, torrentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	if existing, exists := s.data[relativePath]; exists {
		// Update link index if link changed
		if existing.Link != "" && existing.Link != link {
			delete(s.linkIndex, existing.Link)
		}
		existing.DownloadURL = downloadURL
		existing.Link = link
		existing.LastChecked = now
		if link != "" {
			s.linkIndex[link] = relativePath
		}
		s.logger.Debug().Str("path", relativePath).Msg("Updated tracking")
	} else {
		s.data[relativePath] = &FileTracking{
			RelativePath: relativePath,
			DownloadURL:  downloadURL,
			Link:         link,
			CreatedAt:    now,
			LastChecked:  now,
			TorrentID:    torrentID,
		}
		if link != "" {
			s.linkIndex[link] = relativePath
		}
		s.logger.Debug().Str("path", relativePath).Msg("Started tracking")
	}
}

// GetExpired returns tracking data for files older than the specified duration.
// Uses LastChecked when available; falls back to CreatedAt if LastChecked is zero.
func (s *Service) GetExpired(olderThan time.Duration) []*FileTracking {
	s.mu.RLock()
	defer s.mu.RUnlock()

	threshold := time.Now().Add(-olderThan)
	var expired []*FileTracking

	for _, tracking := range s.data {
		basis := tracking.LastChecked
		if basis.IsZero() {
			basis = tracking.CreatedAt
		}
		if basis.Before(threshold) {
			expired = append(expired, tracking)
		}
	}

	return expired
}

// Get retrieves tracking data for a specific path
func (s *Service) Get(relativePath string) (*FileTracking, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tracking, exists := s.data[relativePath]
	return tracking, exists
}

// GetByLink retrieves tracking data by Link (stable RD link). O(1) via index.
func (s *Service) GetByLink(link string) (*FileTracking, bool) {
	if link == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if path, ok := s.linkIndex[link]; ok {
		if ft, ok := s.data[path]; ok {
			return ft, true
		}
	}
	return nil, false
}

// MovePath re-keys a tracking entry from oldPath to newPath.
// Used when a .strm file has been renamed outside robofuse.
// If oldPath doesn't exist, this is a no-op.
func (s *Service) MovePath(oldPath, newPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[oldPath]
	if !exists {
		return
	}
	entry.RelativePath = newPath
	s.data[newPath] = entry
	delete(s.data, oldPath)
	// Update link index
	if entry.Link != "" {
		s.linkIndex[entry.Link] = newPath
	}
	s.logger.Info().
		Str("old", oldPath).
		Str("new", newPath).
		Msg("Moved tracking entry (rename detected)")
}

// Remove deletes tracking data for a file
func (s *Service) Remove(relativePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry, exists := s.data[relativePath]; exists && entry.Link != "" {
		delete(s.linkIndex, entry.Link)
	}
	delete(s.data, relativePath)
	s.logger.Debug().Str("path", relativePath).Msg("Removed tracking")
}

// SetMedia stores ffprobe metadata for a tracked file.
func (s *Service) SetMedia(relativePath string, media *probe.MediaInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, exists := s.data[relativePath]; exists {
		entry.Media = media
	}
}

// SetRDInfo stores Real-Debrid media info. Creates entry if it doesn't exist.
func (s *Service) SetRDInfo(relativePath string, info *realdebrid.MediaInfoResult) {
	if info == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[relativePath]
	if !exists {
		entry = &FileTracking{RelativePath: relativePath, CreatedAt: time.Now(), LastChecked: time.Now()}
		s.data[relativePath] = entry
	}
	entry.RDType = info.Type
	entry.RDSeason = info.SeasonInt()
	entry.RDEpisode = info.EpisodeInt()
	entry.RDYear = string(info.Year)
	entry.RDDuration = info.Duration
	entry.RDBitrate = info.Bitrate
	entry.RDMediaFailed = false
	if info.PosterPath != "" {
		entry.RDPosterPath = info.PosterPath
	}
	if info.BackdropPath != "" {
		entry.RDBackdropPath = info.BackdropPath
	}
}

// MarkRDMediaFailed marks a file's RD media info as permanently unavailable.
func (s *Service) MarkRDMediaFailed(relativePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, exists := s.data[relativePath]; exists {
		entry.RDMediaFailed = true
	}
}

// SetTMDBMatch stores TMDB match result. Creates entry if it doesn't exist.
func (s *Service) SetTMDBMatch(relativePath string, match *tmdb.MatchResult) {
	if match == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[relativePath]
	if !exists {
		entry = &FileTracking{RelativePath: relativePath, CreatedAt: time.Now(), LastChecked: time.Now()}
		s.data[relativePath] = entry
	}
	entry.TMDBID = match.TMDBID
	entry.TMDBTitle = match.Title
	entry.TMDBOriginalTitle = match.OriginalTitle
	entry.TMDBType = match.Type
	entry.TMDBYear = match.Year
	entry.TMDBOverview = match.Overview
	entry.TMDBPoster = match.PosterPath
	entry.TMDBBackdrop = match.BackdropPath
	entry.TMDBRating = match.VoteAverage
	entry.TMDBGenres = match.Genres
	entry.IMDBID = match.IMDBID
}

// Count returns the number of tracked files
func (s *Service) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.data)
}

// Save persists tracking data to disk
func (s *Service) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.trackingFile), 0755); err != nil {
		return err
	}
	if err := atomic.WriteFile(s.trackingFile, bytes.NewReader(data)); err != nil {
		return err
	}

	s.logger.Debug().Int("count", len(s.data)).Msg("Saved tracking data")
	return nil
}

// Load reads tracking data from disk
func (s *Service) Load() error {
	data, err := os.ReadFile(s.trackingFile)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := json.Unmarshal(data, &s.data); err != nil {
		return err
	}

	// Rebuild link index
	for path, ft := range s.data {
		if ft.Link != "" {
			s.linkIndex[ft.Link] = path
		}
	}

	s.logger.Debug().Int("count", len(s.data)).Msg("Loaded tracking data")
	return nil
}
