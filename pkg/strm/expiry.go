package strm

import (
	"time"

	"github.com/robofuse/robofuse/pkg/tracking"
)

// expiry.go handles STRM update and expiry-related tracking helpers.

// GetExpiredFiles returns tracking data for files older than the specified duration
func (s *Service) GetExpiredFiles(olderThan time.Duration) []*tracking.FileTracking {
	return s.tracking.GetExpired(olderThan)
}

// UpdateSTRM updates an existing STRM file with a new URL and refreshes tracking.
// Writes both the URL (line 1) and robofuse metadata (line 2) so the file
// remains compatible with rename detection.
func (s *Service) UpdateSTRM(relativePath, newURL, link, torrentID string) error {
	if err := s.writeSTRM(relativePath, newURL, link, torrentID); err != nil {
		return err
	}

	// Update tracking with new URL and refresh timestamp
	s.tracking.Track(relativePath, newURL, link, torrentID)

	// Save tracking data
	if err := s.tracking.Save(); err != nil {
		s.logger.Warn().Err(err).Msg("Failed to save tracking after update")
	}

	s.logger.Debug().Str("path", relativePath).Msg("Refreshed STRM file")
	return nil
}
