package tmdb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	gourl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robofuse/robofuse/internal/request"
)

// tmdb.go — TheMovieDB API v3 client for metadata matching and enrichment.

const baseURL = "https://api.themoviedb.org/3"

// Client makes requests to the TMDB API.
type Client struct {
	apiKey   string
	reqClient *request.Client
}

// New creates a new TMDB client.
func New(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		reqClient: request.New(
			request.WithTimeout(10*time.Second),
			request.WithMaxRetries(1),
		),
	}
}

// ---------------------------------------------------------------------------
// Search results
// ---------------------------------------------------------------------------

// SearchMovieResult is a single movie from /search/movie.
type SearchMovieResult struct {
	ID           int     `json:"id"`
	Title        string  `json:"title"`
	OriginalTitle string `json:"original_title"`
	Overview     string  `json:"overview"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	ReleaseDate  string  `json:"release_date"`
	VoteAverage  float64 `json:"vote_average"`
	GenreIDs     []int   `json:"genre_ids"`
	Popularity   float64 `json:"popularity"`
}

// SearchTVResult is a single TV show from /search/tv.
type SearchTVResult struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	OriginalName string  `json:"original_name"`
	Overview     string  `json:"overview"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	FirstAirDate string  `json:"first_air_date"`
	VoteAverage  float64 `json:"vote_average"`
	GenreIDs     []int   `json:"genre_ids"`
	Popularity   float64 `json:"popularity"`
}

// searchResponse is the wrapper for both /search/movie and /search/tv.
type searchMovieResponse struct {
	Results []SearchMovieResult `json:"results"`
}

type searchTVResponse struct {
	Results []SearchTVResult `json:"results"`
}

// ---------------------------------------------------------------------------
// Detail results
// ---------------------------------------------------------------------------

// MovieDetails from /movie/{id}.
type MovieDetails struct {
	ID           int              `json:"id"`
	Title        string           `json:"title"`
	OriginalTitle string          `json:"original_title"`
	Overview     string           `json:"overview"`
	PosterPath   string           `json:"poster_path"`
	BackdropPath string           `json:"backdrop_path"`
	ReleaseDate  string           `json:"release_date"`
	Runtime      int              `json:"runtime"`
	VoteAverage  float64          `json:"vote_average"`
	Genres       []Genre          `json:"genres"`
	IMDBID       string           `json:"imdb_id"`
}

// TVDetails from /tv/{id}.
type TVDetails struct {
	ID           int              `json:"id"`
	Name         string           `json:"name"`
	OriginalName string           `json:"original_name"`
	Overview     string           `json:"overview"`
	PosterPath   string           `json:"poster_path"`
	BackdropPath string           `json:"backdrop_path"`
	FirstAirDate string           `json:"first_air_date"`
	VoteAverage  float64          `json:"vote_average"`
	Genres       []Genre          `json:"genres"`
	NumberOfSeasons int           `json:"number_of_seasons"`
}

// Genre from TMDB.
type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ---------------------------------------------------------------------------
// Match result — unified across movie/show
// ---------------------------------------------------------------------------

// MatchResult holds the matched metadata ready for renaming and NFO.
type MatchResult struct {
	TMDBID         int      `json:"tmdb_id"`
	Title          string   `json:"title"`
	OriginalTitle  string   `json:"original_title"`
	Year           int      `json:"year"`
	Type           string   `json:"type"` // "movie" or "show"
	Overview       string   `json:"overview"`
	PosterPath     string   `json:"poster_path"`
	BackdropPath   string   `json:"backdrop_path"`
	VoteAverage    float64  `json:"vote_average"`
	Genres         []string `json:"genres"`
	Runtime        int      `json:"runtime,omitempty"`
	IMDBID         string   `json:"imdb_id,omitempty"`
	Seasons        int      `json:"number_of_seasons,omitempty"`
	ContentRating  string   `json:"content_rating,omitempty"` // US certification (G, PG, TV-Y, etc.)
}

// ---------------------------------------------------------------------------
// API methods
// ---------------------------------------------------------------------------

// SearchMovie searches for a movie by title and optional year.
// Prefers exact title matches over popularity to avoid wrong results.
func (c *Client) SearchMovie(title string, year int) (*SearchMovieResult, error) {
	q := gourl.Values{}
	q.Set("query", title)
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}

	var resp searchMovieResponse
	if err := c.get("/search/movie", q, &resp); err != nil {
		return nil, err
	}
	if len(resp.Results) == 0 {
		return nil, nil
	}

	// Score: exact year match > title similarity > popularity
	best := scoreResults(resp.Results, title, year, func(r SearchMovieResult) (string, int) {
		return r.Title, releaseYear(r.ReleaseDate)
	})
	if best == nil {
		return nil, nil
	}
	return best, nil
}

// SearchTV searches for a TV show by name and optional year.
// Prefers exact title matches over popularity.
func (c *Client) SearchTV(name string, year int) (*SearchTVResult, error) {
	q := gourl.Values{}
	q.Set("query", name)
	if year > 0 {
		q.Set("first_air_date_year", strconv.Itoa(year))
	}

	var resp searchTVResponse
	if err := c.get("/search/tv", q, &resp); err != nil {
		return nil, err
	}
	if len(resp.Results) == 0 {
		return nil, nil
	}

	best := scoreResults(resp.Results, name, year, func(r SearchTVResult) (string, int) {
		return r.Name, releaseYear(r.FirstAirDate)
	})
	if best == nil {
		return nil, nil
	}
	return best, nil
}

// scoreResults picks the best match from TMDB search results.
// Priority: 1) exact year match, 2) exact title match (case-insensitive),
// 3) highest popularity. Rejects results whose title doesn't contain
// the search query at all.
func scoreResults[T any](results []T, query string, year int, getInfo func(T) (string, int)) *T {
	queryLower := strings.ToLower(strings.TrimSpace(query))

	type scored struct {
		idx   int
		score int // higher = better
	}
	var best *scored

	for i := range results {
		title, resultYear := getInfo(results[i])
		titleLower := strings.ToLower(strings.TrimSpace(title))

		// Reject if titles share no common words (e.g. "Hilda Hurricane" vs "Hilda")
		if !titlesShareWord(queryLower, titleLower) {
			continue
		}

		s := 0
		// Exact year match = +100
		if year > 0 && resultYear == year {
			s += 100
		}
		// Exact title match = +50
		if titleLower == queryLower {
			s += 50
		} else if strings.Contains(titleLower, queryLower) {
			s += 25
		}

		if best == nil || s > best.score {
			best = &scored{idx: i, score: s}
		}
	}

	if best == nil {
		// Fallback: return first result
		if len(results) > 0 {
			r := results[0]
			return &r
		}
		return nil
	}
	r := results[best.idx]
	return &r
}

// titlesShareWord returns true if the two titles share at least one word.
// Prevents "Hilda Hurricane" from matching "Hilda" when the user searches for
// the TV series — "Hilda" and "Hurricane" are separate words, but we want
// exact or near-exact matches.
func titlesShareWord(query, title string) bool {
	queryWords := strings.Fields(query)
	titleWords := strings.Fields(title)
	for _, qw := range queryWords {
		for _, tw := range titleWords {
			if qw == tw {
				return true
			}
		}
	}
	// If query is a single word and appears anywhere in title, accept
	return len(queryWords) == 1 && strings.Contains(title, query)
}

// GetMovieDetails fetches full movie details.
func (c *Client) GetMovieDetails(tmdbID int) (*MovieDetails, error) {
	var resp MovieDetails
	if err := c.get(fmt.Sprintf("/movie/%d", tmdbID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTVDetails fetches full TV show details.
func (c *Client) GetTVDetails(tmdbID int) (*TVDetails, error) {
	var resp TVDetails
	if err := c.get(fmt.Sprintf("/tv/%d", tmdbID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Match searches TMDB for a media item and returns a unified MatchResult.
// mediaType: "movie" or "show"
// title: search query (from PTT or RD)
// year: optional year hint
func (c *Client) Match(mediaType, title string, year int) (*MatchResult, error) {
	switch mediaType {
	case "movie":
		sr, err := c.SearchMovie(title, year)
		if err != nil || sr == nil {
			return nil, err
		}
		details, err := c.GetMovieDetails(sr.ID)
		if err != nil {
			return nil, err
		}
		return movieToMatch(details), nil

	case "show":
		sr, err := c.SearchTV(title, year)
		if err != nil || sr == nil {
			return nil, err
		}
		details, err := c.GetTVDetails(sr.ID)
		if err != nil {
			return nil, err
		}
		return tvToMatch(details, sr.FirstAirDate), nil
	}
	return nil, fmt.Errorf("unknown media type: %s", mediaType)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (c *Client) get(path string, params gourl.Values, target interface{}) error {
	u, _ := gourl.Parse(baseURL + path)
	if params == nil {
		params = gourl.Values{}
	}
	params.Set("api_key", c.apiKey)
	u.RawQuery = params.Encode()

	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "application/json")

	resp, err := c.reqClient.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tmdb API %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("tmdb decode: %w", err)
	}
	return nil
}

func releaseYear(date string) int {
	if len(date) >= 4 {
		y, _ := strconv.Atoi(date[:4])
		return y
	}
	return 0
}

func movieToMatch(d *MovieDetails) *MatchResult {
	m := &MatchResult{
		TMDBID:        d.ID,
		Title:         d.Title,
		OriginalTitle: d.OriginalTitle,
		Type:          "movie",
		Overview:      d.Overview,
		PosterPath:    posterURL(d.PosterPath),
		BackdropPath:  backdropURL(d.BackdropPath),
		VoteAverage:   d.VoteAverage,
		Runtime:       d.Runtime,
		IMDBID:        d.IMDBID,
	}
	m.Year = releaseYear(d.ReleaseDate)
	for _, g := range d.Genres {
		m.Genres = append(m.Genres, g.Name)
	}
	return m
}

func tvToMatch(d *TVDetails, firstAir string) *MatchResult {
	m := &MatchResult{
		TMDBID:        d.ID,
		Title:         d.Name,
		OriginalTitle: d.OriginalName,
		Type:          "show",
		Overview:      d.Overview,
		PosterPath:    posterURL(d.PosterPath),
		BackdropPath:  backdropURL(d.BackdropPath),
		VoteAverage:   d.VoteAverage,
		Seasons:       d.NumberOfSeasons,
	}
	m.Year = releaseYear(firstAir)
	for _, g := range d.Genres {
		m.Genres = append(m.Genres, g.Name)
	}
	return m
}

func posterURL(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w500" + path
}

func backdropURL(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w1280" + path
}

// CleanTitle returns a filesystem-safe version of the title for use in paths.
func (m *MatchResult) CleanTitle() string {
	t := m.Title
	t = strings.ReplaceAll(t, "/", "_")
	t = strings.ReplaceAll(t, "\\", "_")
	t = strings.ReplaceAll(t, ":", "_")
	t = strings.ReplaceAll(t, "*", "_")
	t = strings.ReplaceAll(t, "?", "_")
	t = strings.ReplaceAll(t, "\"", "_")
	t = strings.ReplaceAll(t, "<", "_")
	t = strings.ReplaceAll(t, ">", "_")
	t = strings.ReplaceAll(t, "|", "_")
	return strings.TrimSpace(t)
}

// ---------------------------------------------------------------------------
// Content ratings
// ---------------------------------------------------------------------------

// movieReleaseDatesResponse from /movie/{id}/release_dates.
type movieReleaseDatesResponse struct {
	Results []movieReleaseCountry `json:"results"`
}
type movieReleaseCountry struct {
	Iso3166_1    string              `json:"iso_3166_1"`
	ReleaseDates []movieReleaseEntry `json:"release_dates"`
}
type movieReleaseEntry struct {
	Certification string `json:"certification"`
}

// tvContentRatingsResponse from /tv/{id}/content_ratings.
type tvContentRatingsResponse struct {
	Results []tvRatingCountry `json:"results"`
}
type tvRatingCountry struct {
	Iso3166_1 string `json:"iso_3166_1"`
	Rating    string `json:"rating"`
}

// GetMovieCertification returns the US certification for a movie (G, PG, PG-13, R, etc.).
func (c *Client) GetMovieCertification(tmdbID int) string {
	var resp movieReleaseDatesResponse
	if err := c.get(fmt.Sprintf("/movie/%d/release_dates", tmdbID), nil, &resp); err != nil {
		return ""
	}
	for _, country := range resp.Results {
		if country.Iso3166_1 == "US" {
			for _, entry := range country.ReleaseDates {
				if entry.Certification != "" {
					return entry.Certification
				}
			}
		}
	}
	return ""
}

// GetTVCertification returns the US content rating for a TV show (TV-Y, TV-PG, etc.).
func (c *Client) GetTVCertification(tmdbID int) string {
	var resp tvContentRatingsResponse
	if err := c.get(fmt.Sprintf("/tv/%d/content_ratings", tmdbID), nil, &resp); err != nil {
		return ""
	}
	for _, country := range resp.Results {
		if country.Iso3166_1 == "US" {
			return country.Rating
		}
	}
	return ""
}

// ratingIsKids returns true if the US rating is at or below the given threshold.
// Valid thresholds: "G", "PG", "PG-13", "TV-Y", "TV-Y7", "TV-G", "TV-PG"
func ratingIsKids(rating, max string) bool {
	return ratingIndex(rating) <= ratingIndex(max)
}

// ratingIndex maps US content ratings to an ordinal for comparison.
func ratingIndex(rating string) int {
	switch rating {
	case "TV-Y", "G":
		return 1
	case "TV-Y7", "PG":
		return 2
	case "TV-G":
		return 3
	case "TV-PG", "PG-13":
		return 4
	case "TV-14":
		return 5
	case "R", "TV-MA":
		return 6
	case "NC-17":
		return 7
	default:
		if rating == "" {
			return 0 // unrated → not kids
		}
		return 5 // unknown → assume adult
	}
}
