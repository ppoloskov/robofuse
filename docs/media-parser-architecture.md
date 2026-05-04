# Robofuse Media Parser & Heuristic Matcher — Architecture

**Version:** 1.0  
**Date:** 2026-05-04  
**Status:** Plan  

---

## 1. Overview

### 1.1 Purpose

Replace the current single-source, linear-fallback TMDB matching pipeline with a
**heuristic, multi-source, confidence-driven** matching engine. Simultaneously,
upgrade the filename parser by incorporating the best ideas from the CineSync
parser while preserving PTT's multi-episode range support.

### 1.2 Goals

| # | Goal | Success Criteria |
|---|------|-----------------|
| 1 | **Heuristic matching** — no hardcoded fallback chains | Multi-hypothesis search scores all strategies, picks best |
| 2 | **Source federation** — add AniDB, TVDB, Kinopoisk without rewriting | New source = implement one interface, register it |
| 3 | **Localized title handling** — Cyrillic, CJK, Arabic folders | Script detection → language-aware TMDB search |
| 4 | **Multi-episode pack detection** — E01+E02 in one file | Detect, store in tracking, format in organizer paths |
| 5 | **API budget safety** — never exceed rate limits | Soft budget 8 calls/folder, hard budget 12, early termination |
| 6 | **Fuzzy scoring** — handle transliterations, typos, year offsets | Trigram similarity + year proximity + source weight |
| 7 | **Correctable** — fix bad matches without re-writing tracking DB | ID overrides in config, force-rematch flag |
| 8 | **Reusable** — parser usable outside robofuse | Separate Go module, clean public API |

### 1.3 Non-Goals

- Replacing the Real Debrid layer
- Rewriting the organizer/file-writer
- Real-time matching (batch sync is the primary use case)
- Lua embedding (deferred to future if community customization needed)

---

## 2. Repository Structure

### 2.1 Monorepo with Go Workspace

```
robofuse/
├── go.work                          # Go workspace
├── go.mod                           # module github.com/robofuse/robofuse
├── go.sum
│
├── mediaparser/                     # NEW: standalone module
│   ├── go.mod                       # module github.com/robofuse/mediaparser
│   ├── go.sum
│   │
│   ├── parser/                      # Enhanced filename parser
│   │   ├── parser.go                # Main Parse() entry point
│   │   ├── types.go                 # MediaInfo, ParseOptions
│   │   ├── title.go                 # Context-aware title extraction
│   │   ├── year.go                  # Year detection + disambiguation
│   │   ├── season.go                # Season/episode + range detection
│   │   ├── technical.go             # Resolution, codec, quality, audio
│   │   ├── anime.go                 # Anime-specific detection
│   │   ├── sports.go                # Sports content detection
│   │   ├── cleanup.go               # Keyword-driven title cleaning
│   │   ├── keywords.go              # Embedded keyword/config data
│   │   ├── multi_ep.go              # Multi-episode pack detection
│   │   └── parser_test.go
│   │
│   ├── matcher/                     # Heuristic matching engine
│   │   ├── matcher.go               # Orchestrator: run all hypotheses
│   │   ├── hypothesis.go            # Hypothesis generator
│   │   ├── scorer.go                # Fuzzy scoring (trigram, Levenshtein)
│   │   ├── budget.go                # Per-folder API call budget
│   │   ├── federated.go             # Multi-source search coordinator
│   │   ├── confidence.go            # Confidence thresholds + early termination
│   │   ├── multi_ep.go              # Multi-episode TMDB title matching
│   │   ├── language.go              # Script detection + language mapping
│   │   ├── translit.go              # Cyrillic/Latin transliteration tables
│   │   │
│   │   └── source/                  # Metadata source interface
│   │       ├── source.go            # MetadataSource interface
│   │       ├── unified.go           # UnifiedResult (common type)
│   │       ├── tmdb_source.go       # TMDB implementation
│   │       ├── tvmaze_source.go     # TVMaze implementation
│   │       └── tmdb_source_test.go
│   │
│   ├── internal/                    # Internal helpers
│   │   ├── trigram/                 # Trigram similarity
│   │   │   └── trigram.go
│   │   ├── script/                  # Unicode script detection
│   │   │   └── script.go
│   │   └── translit/                # Transliteration tables
│   │       ├── translit.go
│   │       └── tables.go
│   │
│   └── README.md
│
├── pkg/                             # Existing robofuse code (updated)
│   ├── sync/
│   │   └── sync.go                  # Use mediaparser.Matcher
│   ├── tracking/
│   │   └── tracking.go             # Add match provenance + multi-episode fields
│   ├── organizer/
│   │   └── organizer.go            # Multi-episode path formatting
│   ├── tmdb/
│   │   └── tmdb.go                 # Thin client — Match() removed, moves to matcher
│   ├── tvmaze/
│   │   └── tvmaze.go              # Adapt to MetadataSource interface
│   └── ...
│
└── internal/
    └── config/
        └── config.go               # New matching config section
```

---

## 3. Parser Module (`mediaparser/parser`)

### 3.1 Core Types

```go
// MediaInfo contains all parsed information from a media filename.
type MediaInfo struct {
    // Core
    Title            string   `json:"title"`
    AlternativeTitle string   `json:"alternative_title,omitempty"` // AKA extraction
    Year             int      `json:"year,omitempty"`

    // TV-specific
    Seasons        []int    `json:"seasons,omitempty"`        // [1] or [1,2,3] for ranges
    Episodes       []int    `json:"episodes,omitempty"`       // [1] or [1,2] for multi-ep
    EpisodeTitle   string   `json:"episode_title,omitempty"`  // from filename
    AirDate        string   `json:"air_date,omitempty"`       // YYYY-MM-DD for daily shows
    IsTV           bool     `json:"is_tv"`
    IsMovie        bool     `json:"is_movie"`
    IsDaily        bool     `json:"is_daily"`
    IsExtra        bool     `json:"is_extra"`                 // S00 episodes

    // Season bundle tokens (S02+SP, S01-S04+OVA+Movies)
    SeasonBundles  []string `json:"season_bundles,omitempty"`

    // Technical
    Resolution   string   `json:"resolution,omitempty"`    // "1080p"
    VideoCodec   string   `json:"video_codec,omitempty"`   // "x264"
    VideoProfile string   `json:"video_profile,omitempty"` // "Hi10P"
    AudioCodecs  []string `json:"audio_codecs,omitempty"`
    AudioChannels []string `json:"audio_channels,omitempty"`
    QualitySource string  `json:"quality_source,omitempty"` // "BluRay", "WEB-DL"
    HDR          string   `json:"hdr,omitempty"`            // "HDR10", "DV"
    BitDepth     string   `json:"bit_depth,omitempty"`      // "10bit"

    // Languages
    Languages  []string `json:"languages,omitempty"`
    IsDubbed   bool     `json:"is_dubbed"`
    IsSubbed   bool     `json:"is_subbed"`

    // Release
    ReleaseGroup string `json:"release_group,omitempty"`
    Edition      string `json:"edition,omitempty"`
    IsRepack     bool   `json:"is_repack"`
    IsProper     bool   `json:"is_proper"`

    // Content type
    Container string `json:"container,omitempty"`
    IsAnime   bool   `json:"is_anime"`
    IsAdult   bool   `json:"is_adult"`
    IsSports  bool   `json:"is_sports"`
    SportName string `json:"sport_name,omitempty"`

    // Other
    Network string   `json:"network,omitempty"`  // "Netflix", "Amazon"
    Volumes []int    `json:"volumes,omitempty"`
    Site    string   `json:"site,omitempty"`
    Size    string   `json:"size,omitempty"`
    Country string   `json:"country,omitempty"`
}

type ParseOptions struct {
    CleanTitle      bool   // apply keyword-based title cleaning (default true)
    DetectLanguage  bool   // detect language from filename
    TranslateLangs  bool   // convert lang codes to full names
}
```

### 3.2 Public API

```go
// Parse extracts all metadata from a media filename.
func Parse(filename string) *MediaInfo

// ParseWithOptions extracts metadata with custom options.
func ParseWithOptions(filename string, opts ParseOptions) *MediaInfo

// BatchParse parses multiple filenames. Uses internal cache for performance.
func BatchParse(filenames []string) []*MediaInfo
```

### 3.3 Parser Pipeline

```
filename
  │
  ▼
[1] Remove website prefixes (www.site.com patterns)
  │
  ▼
[2] Detect separator type (dot / space / underscore / mixed)
  │
  ▼
[3] Split into tokens (respecting bracket groups, hyphenated terms)
  │
  ▼
[4] Find all years (parentheses > brackets > standalone)
  │
  ▼
[5] Classify each token (technical / title / season-episode / language / year)
  │
  ▼
[6] Extract title (stop at first season/episode/technical marker)
  │
  ▼
[7] Clean title (remove years, keywords, brackets)
  │
  ▼
[8] Extract season/episode (all patterns including ranges + bundles)
  │
  ▼
[9] Extract technicals (resolution, codec, quality, audio, hdr)
  │
  ▼
[10] Detect content type (anime, sports, adult)
  │
  ▼
[11] Extract alternative title (AKA patterns)
  │
  ▼
[12] Detect multi-episode (E01-E02, multiple episode titles in name)
  │
  ▼
MediaInfo struct
```

### 3.4 Key Improvements Over Current PTT

| Feature | Current PTT | New Parser | CineSync Source |
|---------|------------|------------|----------------|
| Title extraction | Basic regex | Context-aware, separator-adaptive | extractor.py |
| Year disambiguation | Simple | Title-year vs release-year | parse_year.py |
| Alternative title | ❌ | AKA extraction | utils.py |
| Air date (daily) | ❌ | YYYY-MM-DD detection | extractor.py |
| Episode title | ❌ | From filename after season marker | extractor.py |
| Season bundles | ❌ | S02+SP, S01-S04+OVA | extractor.py |
| Multi-episode | ✅ Episodes slice | ✅ + episode title splitting | (PTT preserves) |
| Keyword cleaning | ❌ | keywords.json driven | keywords.json |
| Sports detection | ❌ | F1, UFC, MotoGP, etc. | patterns.py |
| Anime detection | ✅ | ✅ (enhanced with bracket/raw patterns) | parse_anime.py |

### 3.5 Keyword Config (`mediaparser/config/keywords.json`)

```json
{
    "keywords": [
        "1080p", "2160p", "720p", "480p",
        "BluRay", "BDRip", "BRRip", "WEB-DL", "WEBDL",
        "WEBRip", "HDTV", "DVDRip", "REMUX",
        "x264", "x265", "HEVC", "AVC", "AV1",
        "AAC", "AC3", "DTS", "FLAC", "Atmos",
        "HDR", "HDR10", "DV", "SDR",
        "10bit", "8bit", "Hi10P",
        "REPACK", "PROPER", "REAL", "FIX",
        "Extended", "Unrated", "Directors Cut",
        "Dubbed", "Subbed", "Dual Audio",
        "Complete", "Season"
    ],
    "editions": [
        "Directors Cut", "Extended Edition", "Unrated",
        "Theatrical", "IMAX", "Remastered", "Criterion",
        "Special Edition", "Collectors Edition", "Final Cut"
    ],
    "quality_sources": [
        "BluRay", "BDRip", "BRRip", "WEB-DL", "WEBDL",
        "WEBRip", "HDTV", "DVD", "DVDRip", "CAM",
        "TS", "TC", "REMUX"
    ],
    "release_groups": []
}
```

---

## 4. Matcher Module (`mediaparser/matcher`)

### 4.1 MetadataSource Interface

```go
// SearchQuery is the input to any metadata source search.
type SearchQuery struct {
    Title    string   // search query (cleaned)
    Year     int      // 0 = no year hint
    Language string   // ISO 639-1, empty = default (en)
    Type     string   // "movie", "show", "any"
}

// SearchCandidate is a lightweight search result from any source.
type SearchCandidate struct {
    Source     string          // "tmdb", "tvmaze", "anidb", etc.
    SourceID   string          // source-specific ID
    Title      string
    Year       int
    Popularity float64
    Raw        json.RawMessage // for detail fetch
}

// UnifiedResult is the full normalized result after detail fetch.
type UnifiedResult struct {
    Source        string   // which source produced this
    SourceID      string   // source-specific ID
    Title         string
    OriginalTitle string
    Year          int
    Type          string   // "movie" or "show"
    Overview      string
    PosterPath    string
    BackdropPath  string
    VoteAverage   float64
    Genres        []string
    Seasons       int
    Runtime       int
    IMDBID        string
    ContentRating string
    Episodes      map[int]EpisodeInfo // season → episode number → info
}

type EpisodeInfo struct {
    Title    string
    Overview string
    Number   int
}

// MetadataSource is the interface all metadata providers implement.
type MetadataSource interface {
    // Name returns the source identifier (e.g. "tmdb")
    Name() string

    // Weight returns the default weight for this source in scoring (0.0-1.0)
    Weight() float64

    // Supports returns true if this source handles the given media type
    Supports(mediaType string) bool

    // Search executes a search query. Returns candidates sorted by relevance.
    Search(ctx context.Context, query SearchQuery) ([]SearchCandidate, error)

    // FetchDetails fetches full metadata for a search result.
    FetchDetails(ctx context.Context, sourceID string) (*UnifiedResult, error)

    // FetchEpisodeTitles fetches episode titles for a show's season.
    // Returns episode_number → title. Used for multi-episode detection.
    FetchEpisodeTitles(ctx context.Context, sourceID string, season int) (map[int]string, error)

    // GetContentRating fetches age rating (G, PG, TV-Y, etc.)
    GetContentRating(ctx context.Context, sourceID string, mediaType string) (string, error)
}
```

### 4.2 Hypothesis Generator

```go
// SearchHypothesis is one search strategy to try.
type SearchHypothesis struct {
    Query    SearchQuery
    Strategy string // "exact", "no_year", "translit", "language", "filename", "raw_folder"
    Priority int    // lower = try first (cheaper/conservative strategies first)
}

// GenerateHypotheses creates all search strategies for a folder + candidates.
func GenerateHypotheses(folderName string, folderParsed *parser.MediaInfo,
    candidates []Candidate, overrides *OverrideConfig) []SearchHypothesis
```

**Strategy Priority Order:**

```
Priority 0: ID override config (user-specified exact TMDB ID)       // 0 API calls
Priority 1: Exact match — folder title + year, detected language     // 1 API call
Priority 2: Exact match — folder title only (no year)               // 1 API call
Priority 3: Transliterated — if non-Latin script detected           // 1 API call
Priority 4: Year ± 1 — if year detected and Priority 1 returned nothing
Priority 5: First candidate filename — title + year (movies only)   // 1 API call
Priority 6: Raw folder name — last resort                           // 1 API call
Priority 7: TVMaze fallback — shows only, no TMDB match             // 1 API call
Priority 8: Source-specific — AniDB for anime, Kinopoisk for ru     // 1 API call
```

### 4.3 Scoring Engine

```go
// ScoredMatch is a search candidate with confidence scores.
type ScoredMatch struct {
    Source     string          // which source
    Candidate  SearchCandidate
    Score      float64         // 0.0-1.0 combined score
    Breakdown  ScoreBreakdown
}

type ScoreBreakdown struct {
    TitleSimilarity float64 // trigram Jaccard: query vs result title
    YearProximity   float64 // 0.0 if no year, 1.0 if exact, 0.7 if ±1, 0.3 if ±2
    PopularityNorm  float64 // normalized popularity score across candidates
    SourceWeight    float64 // source reliability (TMDB=1.0, TVMaze=0.7)
    Uniqueness      float64 // 1.0 if only 1 result, decreases with more results
}

// Default weights (user-configurable)
const (
    defaultTitleWeight = 0.35
    defaultYearWeight  = 0.25
    defaultPopWeight   = 0.15
    defaultSourceWt    = 0.15
    defaultUniqueWt    = 0.10
)

// Score calculates confidence for a search result.
func Score(query SearchQuery, result SearchCandidate, 
    resultSetSize int, sourceWeight float64) ScoredMatch
```

**Trigram Similarity:**
- Split both titles into trigrams
- Jaccard = intersection / union
- Handles transliterations well: "prostokvashino" vs "Простоквашино" still
  gets 0.0 naturally, but "prostokvashino" vs TMDB English title might still
  fail — that's what ID overrides are for
- Fallback: Levenshtein ratio for very short titles (< 6 chars)

**Confidence Thresholds:**
```
>= 0.90 → VERY_HIGH : accept immediately, stop searching
>= 0.75 → HIGH      : accept after current strategy set completes  
>= 0.50 → MEDIUM    : try more strategies, use if nothing better
<  0.50 → LOW       : try all, take best, log warning
```

### 4.4 API Budget Tracker

```go
// BudgetTracker manages API call limits per folder.
type BudgetTracker struct {
    softLimit int            // 8 — warn if exceeded
    hardLimit int            // 12 — stop calling if exceeded
    perSource int            // 4 — max calls per source
    total     int            // running total
    bySource  map[string]int // per-source counters
    mu        sync.Mutex
}

func (b *BudgetTracker) CanCall(source string) bool
func (b *BudgetTracker) RecordCall(source string)
func (b *BudgetTracker) Warning() bool   // true if soft limit exceeded
func (b *BudgetTracker) Exhausted() bool // true if hard limit exceeded
```

```
Per-folder API call budget breakdown (worst case):

Step                         Calls    Source
───────────────────────────  ──────   ──────
ID override check            0        local
Exact match + year           1        TMDB search
Exact match no year          1        TMDB search
Transliterated               1        TMDB search
Year ± 1 (if needed)         0-2      TMDB search (only if above returned nothing)
Filename match (movies)      1        TMDB search
Raw folder                   1        TMDB search
TVMaze fallback              1        TVMaze search
Detail fetch (best only)     1        TMDB detail
Content rating (if needed)   1        TMDB rating
───────────────────────────  ──────
Worst case total             9        (under hard limit of 12)
Typical case                 2-3      (early termination at >= 0.90)
```

### 4.5 Federated Searcher

```go
// FederatedSearcher orchestrates search across all sources.
type FederatedSearcher struct {
    sources []MetadataSource
    budget  *BudgetTracker
    matcher *HeuristicMatcher
}

// SearchAll executes all applicable hypotheses against all applicable sources.
// Respects API budget, confidence thresholds, and per-source rate limits.
func (fs *FederatedSearcher) SearchAll(ctx context.Context,
    folderName string, candidates []Candidate,
    overrides *OverrideConfig) (*MatchResult, error)
```

**Execution Flow:**

```
1. Generate ALL hypotheses for the folder
       │
2. Sort by priority (cheapest first)
       │
3. For each hypothesis:
       │
   ┌───▼────────────────────────────────────┐
   │ 3a. Check budget — can we call?         │
   │ 3b. Search all applicable sources       │
   │ 3c. Score all results                   │
   │ 3d. Collect ScoredMatch candidates      │
   │ 3e. If best score >= 0.90 → DONE (skip) │
   └─────────────────────────────────────────┘
       │
4. Pick highest-scored candidate across all hypotheses
       │
5. Fetch full details for the best candidate (1 API call)
       │
6. Fetch content rating (1 API call, if needed + not cached)
       │
7. If TV show: fetch episode titles for multi-ep detection
       │
8. Return MatchResult with confidence + provenance
```

### 4.6 Match Result

```go
// MatchResult is the final output of the matching engine.
type MatchResult struct {
    // The match
    Source       string       // "tmdb", "tvmaze"
    SourceID     string       // TMDB ID or equivalent
    Unified      *UnifiedResult

    // Confidence
    Confidence   float64      // 0.0-1.0
    ConfidenceLevel string    // "very_high", "high", "medium", "low"

    // Provenance (for debugging + re-match decisions)
    Strategy     string       // which hypothesis won
    Alternatives []ScoredMatch // all candidates considered (for logging)

    // Multi-episode (if detected)
    MultiEpisodes bool
    MatchedEpisodes []int     // [1, 2] for E01+E02 pack
    EpisodeTitles   []string  // matching titles from metadata

    // Override info
    IsOverride   bool         // true if ID override was used
}
```

### 4.7 Multi-Episode Detection

```go
// DetectMultiEpisode checks if a single file contains multiple episodes.
//
// Strategies (in order):
//
// 1. PTT already parsed Episodes: [1, 2] — example: "Show.S01E01E02.mkv"
// 2. Filename contains two episode titles from TMDB metadata
//    Example: "PJ Masks - S01E01- Blame it on the Train, Owlette & Catboy's
//              Cloudy Crisis"
//    → TMDB says E01 title = "Blame It on the Train, Owlette"
//    → TMDB says E02 title = "Catboy's Cloudy Crisis"
//    → Both found in filename → detected as E01+E02
// 3. Duration-based (optional, ffprobe required)
//    → Typical kids show = 11min, file = 22min → likely 2 episodes
//
func DetectMultiEpisode(filename string, parsed *parser.MediaInfo,
    episodeTitles map[int]string, duration float64) []int
```

**Multi-Episode Tracking Changes:**

```go
// New fields in FileTracking (pkg/tracking/tracking.go)
type FileTracking struct {
    // ... existing fields ...

    // Multi-episode support
    Episodes      []int    `json:"episodes,omitempty"`       // [1, 2]
    EpisodeTitles []string `json:"episode_titles,omitempty"` // from TMDB

    // Match provenance
    MatchSource     string  `json:"match_source,omitempty"`     // "tmdb"
    MatchStrategy   string  `json:"match_strategy,omitempty"`   // "exact", "translit"
    MatchConfidence float64 `json:"match_confidence,omitempty"` // 0.0-1.0
    MatchIsOverride bool    `json:"match_is_override,omitempty"`
    MatchSignature  string  `json:"match_signature,omitempty"`  // config hash
}
```

### 4.8 Script Detection & Language-Aware Search

```go
// DetectScript identifies the Unicode script of a title.
// Returns ISO 639-1 language code inferred from script.
// Uses Unicode range tables (stdlib unicode package).
//
// Mapping:
//   Cyrillic → "ru"   (try Russian TMDB search)
//   Han/Hangul → "ko" (try Korean) — falls back to "zh" if no results
//   Arabic → "ar"
//   Latin → "" (default, English)
func DetectScript(title string) string

// Transliterate converts Cyrillic text to Latin.
// Uses a mapping table (port from transliterator library or custom).
func Transliterate(cyrillic string, language string) string
```

**Language-Aware Search Flow:**

```
1. Parse folder name → title = "миньоны"
2. DetectScript("миньоны") → "ru" (Cyrillic)
3. Add hypothesis: SearchQuery{Title: "миньоны", Language: "ru"}
4. TMDB search with language=ru returns Russian-translated "Minions" entry
5. If no result: add hypothesis with Transliterate("миньоны", "ru") → "minony"
6. TMDB search with language=en for "minony"
```

---

## 5. Configuration

### 5.1 New `config.json` Sections

```json
{
    "sources": {
        "tmdb": {
            "enabled": true,
            "api_key": "YOUR_KEY",
            "weight": 1.0
        },
        "tvmaze": {
            "enabled": true,
            "weight": 0.7
        }
    },

    "matching": {
        "confidence_threshold": 0.75,
        "max_api_calls_per_folder": 12,
        "early_termination": true,
        "fuzzy_threshold": 0.6,
        "year_proximity_bonus": 0.25,
        "title_similarity_bonus": 0.35,
        "source_weight_bonus": 0.15,
        "popularity_bonus": 0.15,
        "uniqueness_bonus": 0.10,
        "language_detection": true,
        "strategies": [
            "exact",
            "no_year",
            "transliterated",
            "language_aware",
            "year_flex",
            "filename",
            "raw_folder"
        ]
    },

    "overrides": {
        "title_overrides": {
            "torrent_folder_name": "Correct Search Title"
        },
        "id_overrides": {
            "torrent_folder_name": {
                "source": "tmdb",
                "source_id": "12345"
            }
        }
    },

    "force_rematch": false,
    "rematch_below_confidence": 0.0
}
```

### 5.2 Config Struct Changes

```go
type Config struct {
    // ... existing fields ...

    // Matching configuration
    Matching   MatchingConfig          `json:"matching"`
    Overrides  OverrideConfig          `json:"overrides"`
    Sources    map[string]SourceConfig `json:"sources"`
    ForceRematch bool                  `json:"force_rematch"`
    RematchBelowConfidence float64    `json:"rematch_below_confidence"`
}

type MatchingConfig struct {
    ConfidenceThreshold   float64  `json:"confidence_threshold"`
    MaxAPICallsPerFolder  int      `json:"max_api_calls_per_folder"`
    EarlyTermination      bool     `json:"early_termination"`
    FuzzyThreshold        float64  `json:"fuzzy_threshold"`
    LanguageDetection     bool     `json:"language_detection"`
    Strategies            []string `json:"strategies"`
}

type OverrideConfig struct {
    TitleOverrides map[string]string        `json:"title_overrides"`
    IDOverrides    map[string]IDOverride    `json:"id_overrides"`
}

type IDOverride struct {
    Source   string `json:"source"`
    SourceID string `json:"source_id"`
}
```

---

## 6. Data Flow

### 6.1 End-to-End Flow (Updated `processTorrent`)

```
processTorrent(torrent)
  │
  ├─ [unchanged] Unrestrict links
  ├─ [unchanged] Build candidates
  ├─ [unchanged] Fetch RD media info
  │
  ├─ [NEW] matcher.Match(candidates, overrides)
  │   │
  │   ├─ Parse folder name → MediaInfo
  │   ├─ Generate hypotheses
  │   ├─ FederatedSearch across sources
  │   │   ├─ For each hypothesis, for each source:
  │   │   │   ├─ Check budget
  │   │   │   ├─ Search
  │   │   │   ├─ Score
  │   │   │   └─ If confidence >= 0.90 → early exit
  │   │   └─ Pick best candidate
  │   ├─ FetchDetails (best only)
  │   ├─ FetchContentRating (if needed)
  │   ├─ If show → FetchEpisodeTitles → DetectMultiEpisode
  │   └─ Return MatchResult
  │
  ├─ [NEW] strmService.SetMatch(path, matchResult) // match provenance
  ├─ [unchanged] Apply name templates
  ├─ [NEW] CalculateContentPath // supports multi-episode formatting
  └─ [unchanged] SyncCandidates, write STRM/NFO
```

### 6.2 Organizer Multi-Episode Paths

```
Single episode:
  Series/PJ Masks/Season 01/PJ Masks - S01E01.mkv.strm

Multi-episode pack:
  Series/PJ Masks/Season 01/PJ Masks - S01E01-E02.mkv.strm

NFO for multi-episode:
  Contains both episode titles, overviews merged
```

**Template changes (`namefmt`):**

```
{title} - S{season:02d}E{episode}
  → single: "PJ Masks - S01E01"
  → multi:  "PJ Masks - S01E01+E02" or "E01-E02"

{title} - S{season:02d}E{episode:02d} - {episode_title}
  → single: "PJ Masks - S01E01 - Blame it on the Train"
  → multi:  "PJ Masks - S01E01-E02"
             (too long for combined titles)
```

---

## 7. File Changes Summary

### 7.1 New Files

| File | Purpose | Est. Lines |
|------|---------|-----------|
| `mediaparser/go.mod` | Module definition | 10 |
| `mediaparser/parser/parser.go` | Main Parse() entry | 80 |
| `mediaparser/parser/types.go` | MediaInfo, ParseOptions | 80 |
| `mediaparser/parser/title.go` | Title extraction | 200 |
| `mediaparser/parser/year.go` | Year detection | 150 |
| `mediaparser/parser/season.go` | Season/episode + bundles | 180 |
| `mediaparser/parser/technical.go` | Resolution, codec, quality, etc. | 150 |
| `mediaparser/parser/anime.go` | Anime detection | 120 |
| `mediaparser/parser/sports.go` | Sports detection | 100 |
| `mediaparser/parser/cleanup.go` | Keyword-driven cleaning | 80 |
| `mediaparser/parser/keywords.go` | Embedded keyword config | 60 |
| `mediaparser/parser/multi_ep.go` | Multi-episode detection | 80 |
| `mediaparser/parser/parser_test.go` | Tests | 300 |
| `mediaparser/matcher/matcher.go` | Orchestrator | 150 |
| `mediaparser/matcher/hypothesis.go` | Hypothesis generator | 120 |
| `mediaparser/matcher/scorer.go` | Fuzzy scoring | 180 |
| `mediaparser/matcher/budget.go` | API budget | 60 |
| `mediaparser/matcher/federated.go` | Multi-source search | 120 |
| `mediaparser/matcher/confidence.go` | Confidence thresholds | 40 |
| `mediaparser/matcher/multi_ep.go` | Multi-ep TMDB matching | 100 |
| `mediaparser/matcher/language.go` | Script detection | 80 |
| `mediaparser/matcher/translit.go` | Transliteration | 60 |
| `mediaparser/matcher/source/source.go` | MetadataSource interface | 60 |
| `mediaparser/matcher/source/unified.go` | UnifiedResult type | 80 |
| `mediaparser/matcher/source/tmdb_source.go` | TMDB implementation | 250 |
| `mediaparser/matcher/source/tvmaze_source.go` | TVMaze implementation | 150 |
| `mediaparser/internal/trigram/trigram.go` | Trigram similarity | 60 |
| `mediaparser/internal/script/script.go` | Unicode script detection | 80 |
| `mediaparser/internal/translit/translit.go` | Transliteration | 40 |
| `mediaparser/internal/translit/tables.go` | Cyrillic→Latin tables | 100 |
| `mediaparser/README.md` | Module docs | 80 |
| `go.work` | Go workspace | 6 |

### 7.2 Modified Files

| File | Changes |
|------|---------|
| `pkg/sync/sync.go` | Replace `matchTMDBForGroup` with `matcher.Match()` call |
| `pkg/tracking/tracking.go` | Add `Episodes`, `EpisodeTitles`, `MatchSource`, `MatchStrategy`, `MatchConfidence`, `MatchIsOverride`, `MatchSignature` fields |
| `pkg/organizer/organizer.go` | Multi-episode path formatting (use `tracking.Episodes` slice instead of `season[0]/episode[0]`) |
| `pkg/namefmt/namefmt.go` | Multi-episode template support |
| `pkg/strm/strm.go` | Write multi-episode NFO, accept new tracking fields |
| `pkg/tmdb/tmdb.go` | Thin down — `Match()` removed, `Search*` and `Get*` remain as backing for tmdb_source |
| `pkg/tvmaze/tvmaze.go` | Adapt to `MetadataSource` interface |
| `internal/config/config.go` | Add `Matching`, `Overrides`, `Sources`, `ForceRematch`, `RematchBelowConfidence` |
| `config.json` | Add example sections |
| `go.mod` | Add `require github.com/robofuse/mediaparser` |

---

## 8. Testing Strategy

### 8.1 Parser Tests

- **Unit**: Each extraction function tested independently
- **Snapshot**: Known filenames → expected MediaInfo output (50+ test cases)
- **Regression**: PTT's existing test cases ported and extended
- **Multi-language**: Cyrillic, CJK, Arabic, Hindi filenames

### 8.2 Matcher Tests

- **Mock sources**: Testable without API calls (mock MetadataSource implementations)
- **Scoring**: Verify trigram similarity against known pairs
- **Hypothesis**: Verify correct strategy selection for various folder names
- **Budget**: Verify budget exhaustion stops calling
- **Confidence**: Verify early termination at each threshold
- **Integration**: Real TMDB API tests (gated behind `-tags=integration`)

### 8.3 End-to-End

- Torrent folder → MatchResult flow
- Multi-episode detection with real TMDB episode title data
- Language detection + language-aware search

---

## 9. Migration Path

### Phase 1: Parser Only (no user impact)

1. Create `mediaparser/parser/` module
2. Port PTT's core extraction, enhanced with CineSync features
3. Add keyword config, multi-episode detection
4. Comprehensive tests against known filenames
5. **Keeps PTT as dependency during transition** — the parser runs in parallel,
   output logged for comparison

### Phase 2: Matcher Core (no user impact)

1. Build `MetadataSource` interface + TMDB + TVMaze implementations
2. Build hypothesis generator, scorer, budget tracker, federated searcher
3. Mock-based unit tests for all components

### Phase 3: Integration (parallel run)

1. Wire into `sync.go` — run new matcher alongside existing, log both results
2. Compare match decisions, tune scoring weights
3. Handle edge cases discovered in real-world data

### Phase 4: Switch Over

1. Remove old `matchTMDBForGroup` code
2. Thin down `pkg/tmdb/tmdb.go` (remove `Match()`)
3. Update organizer for multi-episode support
4. Add `force_rematch` config for users to re-match all content

### Phase 5: Future Sources

1. AniDB (anime focus) — implement `MetadataSource`
2. TVDB — implement `MetadataSource`
3. Kinopoisk (Russian content) — implement `MetadataSource`

---

## 10. Dependencies

### New Dependencies

None. The architecture intentionally avoids external dependencies:

| Need | Solution |
|------|----------|
| Unicode script detection | `unicode` stdlib package (range tables) |
| Trigram similarity | Custom ~30 lines |
| Transliteration | Custom map (ports from transliterator library data) |
| Fuzzy matching | Custom Levenshtein + trigram algorithms |
| Rate limiting | Existing `golang.org/x/time/rate` |

### Removed Dependencies

- `github.com/itsrenoria/ptt-go` — eventually replaced by `mediaparser/parser`

---

## Appendix A: Example Scenarios

### A.1 Cyrillic Folder → Correct Match

```
Input:  folder "миньоны (2015)", candidate "Minions.2015.1080p.mkv"

Parse:  Title="миньоны", Year=2015
Detect: Script=Cyrillic → Language="ru"
Hypoth1: Search("миньоны", 2015, lang="ru") 
         → TMDB returns "Миньоны" (Minions, ru locale)
         → Score: title=0.95, year=1.0 → 0.92 confidence → VERY_HIGH → DONE

Result: MatchResult{SourceID: "211672", Title: "Minions", 
                    Confidence: 0.92, Strategy: "language"}
```

### A.2 Latin Transliterated Russian → Manual Override

```
Input:  folder "prostokvashino", candidate "Prostokvashino.S01.1080p.mkv"

Parse:  Title="prostokvashino"
Detect: Script=Latin (no language hint)
Hypoth1: Search("prostokvashino", 0) → TMDB: no results
Hypoth2: Search("prostokvashino", 0, lang="ru") 
         → TMDB: no results (Latin script, ru locale has Cyrillic)
Hypoth3: TVMaze → no results
→ UNMATCHED (confidence < 0.50)

Fix:    Add id_override: 
        {"prostokvashino": {"source": "tmdb", "source_id": "46310"}}
Next run: Priority 0 → override → DONE

Result: MatchResult{SourceID: "46310", 
                    Title: "Three from Prostokvashino", 
                    IsOverride: true}
```

### A.3 Multi-Episode Pack

```
Input:  folder "PJ Masks S01", 
        candidate "PJ Masks - S01E01- Blame it on the Train, Owlette & 
                   Catboy's Cloudy Crisis.mkv"

Parse:  Title="PJ Masks", Season=[1], Episode=[1],
        EpisodeTitle="Blame it on the Train, Owlette & Catboy's Cloudy Crisis"

Match:  TMDB returns show ID 63770 "PJ Masks"

MultiEp: Fetch episode titles for S01 from TMDB:
         E01 = "Blame It on the Train, Owlette"
         E02 = "Catboy's Cloudy Crisis"
         → Both titles found in filename EpisodeTitle string
         → Detected: Episodes = [1, 2]

Result: MatchResult{
           SourceID: "63770",
           MultiEpisodes: true,
           MatchedEpisodes: [1, 2],
           EpisodeTitles: [
               "Blame It on the Train, Owlette",
               "Catboy's Cloudy Crisis"
           ]
        }

Path:   Series/PJ Masks/Season 01/PJ Masks - S01E01-E02.mkv.strm
```

### A.4 API Budget Enforcement

```
Scenario:  Folder is borderline — TMDB returns low-confidence results 
           for all strategies

Hypoth1 (exact+year):   Score 0.45 → LOW → continue
Hypoth2 (no_year):      Score 0.38 → LOW → continue
Hypoth3 (translit):     Score 0.0  → no result
Hypoth4 (year±1):       Score 0.42 → LOW → continue
Hypoth5 (filename):     Score 0.51 → MEDIUM → continue
Hypoth6 (raw_folder):   Score 0.0  → no result
Hypoth7 (TVMaze):       Score 0.55 → MEDIUM

Budget:     7 search calls used (under soft limit of 8)
Best:       TVMaze (0.55) > filename (0.51)
Detail:     1 call for TVMaze detail fetch
Total:      8 calls

→ Matched with MEDIUM confidence, logged for review
→ If force_rematch later with updated config, might match higher
```
