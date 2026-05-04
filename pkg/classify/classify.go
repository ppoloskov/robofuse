package classify

import (
	"path/filepath"
	"strings"

	ptt "github.com/itsrenoria/ptt-go"
)

// classify.go — single source of truth for movie vs series classification.

// Result holds the classification output.
type Result struct {
	Type      string // "movie" or "episode"
	Title     string // primary title
	ShowTitle string // series name (episodes only)
	Year      int
	Season    int
	Episode   int
}

// Classify determines whether a file is a movie or TV episode.
// Priority: RD type → TMDB type → PTT parsing → custom rules.
func Classify(filename, folderName string, rdType, tmdbType string) *Result {
	r := &Result{}

	// Parse with PTT
	fn := strings.TrimSuffix(filename, filepath.Ext(filename))
	parsed := ptt.Parse(fn)
	folderBase := filepath.Base(folderName)
	folderParsed := ptt.Parse(folderBase)

	isSeries := len(parsed.Seasons) > 0 || len(parsed.Episodes) > 0 || parsed.Anime
	isSeriesFolder := len(folderParsed.Seasons) > 0 || len(folderParsed.Episodes) > 0 || folderParsed.Anime

	if isSeriesFolder {
		r.Type = "episode"
		r.ShowTitle = firstNonEmpty(folderParsed.Title, folderBase)
		r.Year = firstNonZero(folderParsed.Year, parsed.Year)
		r.Season = firstSeason(parsed.Seasons, folderParsed.Seasons)
		r.Episode = firstEpisode(parsed.Episodes)
		r.Title = firstNonEmpty(parsed.Title, filename)
	} else if isSeries {
		r.Type = "episode"
		r.ShowTitle = firstNonEmpty(parsed.Title, folderBase)
		r.Year = parsed.Year
		r.Season = firstSeason(parsed.Seasons, nil)
		r.Episode = firstEpisode(parsed.Episodes)
		r.Title = firstNonEmpty(parsed.Title, filename)
	} else {
		r.Type = "movie"
		r.Title = firstNonEmpty(parsed.Title, folderParsed.Title, fn)
		r.Year = firstNonZero(parsed.Year, folderParsed.Year)
	}

	// RD override
	if rdType == "show" {
		r.Type = "episode"
	} else if rdType == "movie" {
		r.Type = "movie"
	}

	// TMDB override (highest priority)
	if tmdbType == "show" {
		r.Type = "episode"
	} else if tmdbType == "movie" {
		r.Type = "movie"
	}

	return r
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

func firstSeason(a, b []int) int {
	if len(a) > 0 {
		return a[0]
	}
	if len(b) > 0 {
		return b[0]
	}
	return 0
}

func firstEpisode(a []int) int {
	if len(a) > 0 {
		return a[0]
	}
	return 0
}
