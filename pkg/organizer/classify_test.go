package organizer

import (
	"strings"
	"testing"

	ptt "github.com/itsrenoria/ptt-go"
)

func TestGetContentTypeAndPath_TableDriven(t *testing.T) {
	org := &Organizer{
		organizedDir: "/organized",
	}

	tests := []struct {
		name         string
		filename     string
		rdID         string
		wantType     string
		wantContains string
	}{
		{
			name:         "movie with year",
			filename:     "Inception.2010.1080p.mkv.strm",
			rdID:         "ABC123",
			wantType:     "movie",
			wantContains: "Movies",
		},
		{
			name:         "tv episode with S01E01",
			filename:     "Show.Name.S01E01.1080p.mkv.strm",
			rdID:         "DEF456",
			wantType:     "series",
			wantContains: "Season 01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nameNoExt := strings.TrimSuffix(tt.filename, ".strm")
			parsed := ptt.Parse(nameNoExt)
			contentType, destPath := org.getContentTypeAndPath(parsed, nil, tt.filename, tt.rdID, nil)

			if contentType != tt.wantType {
				t.Errorf("type = %q, want %q", contentType, tt.wantType)
			}
			if !strings.Contains(destPath, tt.wantContains) {
				t.Errorf("destPath %q does not contain %q", destPath, tt.wantContains)
			}
		})
	}
}
