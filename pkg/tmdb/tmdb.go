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
	TMDBID       int      `json:"tmdb_id"`
	Title        string   `json:"title"`         // official title
	OriginalTitle string  `json:"original_title"`
	Year         int      `json:"year"`
	Type         string   `json:"type"`          // "movie" or "show"
	Overview     string   `json:"overview"`
	PosterPath   string   `json:"poster_path"`
	BackdropPath string   `json:"backdrop_path"`
	VoteAverage  float64  `json:"vote_average"`
	Genres       []string `json:"genres"`
	Runtime      int      `json:"runtime,omitempty"`      // movies only
	IMDBID       string   `json:"imdb_id,omitempty"`       // movies only
	Seasons      int      `json:"number_of_seasons,omitempty"` // TV only
}

// ---------------------------------------------------------------------------
// API methods
// ---------------------------------------------------------------------------

// SearchMovie searches for a movie by title and optional year.
// Returns the best match (highest popularity), or nil if none found.
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

	// Pick the best match: prefer exact year match, then highest popularity
	var best *SearchMovieResult
	for i := range resp.Results {
		r := &resp.Results[i]
		if year > 0 && r.ReleaseDate != "" {
			if releaseYear(r.ReleaseDate) == year {
				// Exact year match — take it
				return r, nil
			}
		}
		if best == nil || r.Popularity > best.Popularity {
			best = r
		}
	}
	return best, nil
}

// SearchTV searches for a TV show by name.
func (c *Client) SearchTV(name string) (*SearchTVResult, error) {
	q := gourl.Values{}
	q.Set("query", name)

	var resp searchTVResponse
	if err := c.get("/search/tv", q, &resp); err != nil {
		return nil, err
	}
	if len(resp.Results) == 0 {
		return nil, nil
	}

	// Return most popular match
	best := &resp.Results[0]
	for i := 1; i < len(resp.Results); i++ {
		if resp.Results[i].Popularity > best.Popularity {
			best = &resp.Results[i]
		}
	}
	return best, nil
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
		sr, err := c.SearchTV(title)
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
