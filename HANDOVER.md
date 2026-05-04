# Robofuse — Handover Document

**Date**: May 2026  
**Branch**: `feature/three-commits`  
**Fork**: `github.com/ppoloskov/robofuse`  
**Upstream**: `github.com/itsrenoria/robofuse`  

---

## 1. What It Does

Robofuse is a Go CLI tool that generates `.strm` files from Real-Debrid torrents. It watches a Real-Debrid account, unrestricts links, generates STRM files with rich metadata, and organizes them into a Plex/Kodi/Jellyfin-compatible library structure.

```
Real-Debrid API → robofuse → library/ (STRM + NFO files)
                           → library-organized/ (organized copies)
                           → cache/ (state + metadata)
```

## 2. Package Map

```
cmd/robofuse/main.go          Cobra CLI (run/watch/dry-run, --rebuild-organized)
internal/
  config/config.go             JSON config + ROBOFUSE_* env vars + validation
  console/progress.go          Terminal progress bar (single-line)
  health/health.go             GET /healthz → {"status":"ok"}
  logger/logger.go             Zerolog + lumberjack log rotation
  metrics/metrics.go           Prometheus counters (/metrics on :9090)
  request/request.go           HTTP client with rate limiting, retry, backoff
  request/errors.go            Typed HTTP errors (HTTPError with RD error codes)
pkg/
  classify/classify.go         Single source: movie vs episode classification
  namefmt/namefmt.go           Filename template engine ({title}, {year}, etc.)
  nfo/nfo.go                   Kodi-compatible .nfo XML generation
  organizer/organizer.go       PTT-based library organization (Movies/Series/Anime/Kids/X)
  probe/probe.go               ffprobe media stream analysis
  realdebrid/                  Real-Debrid API client
    client.go                  HTTP clients (general/torrents/media)
    unrestrict.go              Link unrestriction with dual retry strategy
    torrents.go                Torrent fetch + original_filename lookup
    downloads.go               Paginated downloads fetch
    media.go                   /streaming/mediaInfos + flexString types
    types.go                   All RD API types
  repair/repair.go             Dead torrent repair via magnet re-add
  retry/retry.go               Persistent JSON retry queue
  strm/strm.go                 STRM file creation, rename detection, NFO generation
  strm/expiry.go               Expired link refresh
  sync/sync.go                 Main orchestrator (10-step Run loop)
  sync/retry_handler.go        Cross-cycle retry queue processing
  sync/summary.go              Human-readable cycle summary
  tmdb/tmdb.go                 TheMovieDB API v3 — search, details, ratings
  tracking/tracking.go         File metadata persistence (O(1) link index)
  tvmaze/tvmaze.go             TVMaze fallback when TMDB doesn't match
```

## 3. Sync Flow

```
Step 1:   GetTorrents() → downloaded[], dead[]
Step 1b:  PopulateOriginalFilenames() — /torrents/info/{id} per torrent
Step 2:   processRetryQueue() — cross-cycle retries
Step 3:   RepairTorrents() — dead torrent magnet re-add
Step 4:   GetDownloads() — paginated, streamable filter
Step 5:   Match torrent links → downloadMap
Step 6:   unrestrictLinks() — concurrent with circuit breaker
Step 7:   buildCandidatesInto() — filter by type/size
Step 7b:  fetchMediaInfos() — RD /streaming/mediaInfos (60s cap)
Step 7c:  matchTMDB() — torrent-level TMDB matching + TVMaze fallback
Step 7d:  applyNameTemplates() — user-configured filename formatting
Step 8:   Sync() — write STRM + NFO files, rename detection
Step 9:   runOrganizer() — copy to library-organized/
Step 10:  refreshExpiringLinks() — re-unrestrict expired links
```

## 4. STRM File Format

```
https://download.real-debrid.com/...
#robofuse:{"link":"https://real-debrid.com/d/...","torrent":"ID","tmdb_id":12345,...}
```

- Line 1: Download URL (read by Kodi/Jellyfin/Emby/Infuse)
- Line 2: JSON metadata blob (comment — skipped by all players)
- Backward compatible with old `# robofuse: link=... torrent=...` format
- Jellyfin confirmed: `ProbeProvider.cs` skips `#` lines

## 5. Key Design Decisions

| Decision | Rationale |
|----------|-----------|
| **PTT-go for parsing** | Already a dependency, handles 100+ filename patterns |
| **TMDB over TVDB** | Simpler API (just an API key), covers 95% of content |
| **TVMaze fallback** | No API key needed, good anime/older show coverage |
| **Torrent-level TMDB matching** | One search per torrent folder, not per file (67 calls vs 909) |
| **Dual-layer retry (HTTP + app)** | HTTP: fast retry for transient; App: long backoff for server overload |
| **Circuit breaker** | 10 consecutive 503/429 → 30s pause for all workers |
| **rd_error_code extraction** | Parses 503 bodies for specific RD error codes (19=hoster unavailable, etc.) |
| **flexString type** | RD returns year/season/episode as string OR number inconsistently |
| **flexStreamMap type** | RD returns video/audio/subtitles as object OR array |
| **Self-describing STRM** | Each file standalone — corruption = lose 1 file, not 900 |
| **Incremental saves** | Every 25 files, then at phase end — Ctrl+C safe |
| **conc.Pool** | Replaced custom worker pool — gets panic safety for free |

## 6. Config Reference

All keys with defaults are in `config.json`. Key features:

- `tmdb_api_key` — enables TMDB matching, poster/backdrop, ratings, content ratings
- `folder_rules` — regex-capable routing (`~` prefix = regex)
- `movie_name_template` / `episode_name_template` — custom filenames
- `kids_max_rating` — PG-and-below → Kids/ folder
- `title_overrides` — manual torrent→title mapping for stubborn cases
- `ROBOFUSE_*` env vars override all settings

## 7. Health & Metrics

- `:9090/healthz` — `{"status":"ok"}`
- `:9090/metrics` — Prometheus counters (CycleDuration wired)

## 8. Known Gaps

| Item | Priority | Notes |
|------|----------|-------|
| Organizer reads tracking independently | Medium | Stale data risk, ~30 line refactor |
| 4 routing mechanisms overlap | Low | folder_rules + adult_patterns + kids + anime |
| Name templates not used in organizer | Low | Organizer uses hardcoded patterns |
| TVDB/AniDB not implemented | Low | TMDB+TVMaze cover most content |
| Context propagation incomplete | Low | Only ffprobe uses context |
| Full test coverage | Low | Pure functions tested, integration not |

## 9. OpenCode Agents

To work on this codebase with OpenCode, use these Task prompts:

### Architect-Analyst Agent

```
task(
  subagent_type: "general",
  description: "Analyze robofuse architecture",
  prompt: """
You are a Go systems architect analyzing the robofuse codebase at /Users/paul/projects/robofuse.

Your job:
1. Read HANDOVER.md first for context
2. Analyze the codebase structure and data flow
3. Identify architectural risks, inconsistencies, and optimization opportunities
4. Propose concrete refactoring plans with file paths and line counts
5. Prioritize by impact vs effort

Focus on: data flow integrity, error handling gaps, concurrency safety,
state persistence, API rate limiting, and classification consistency.

Report back with findings ordered by severity.
"""
)
```

### Developer Agent

```
task(
  subagent_type: "general", 
  description: "Implement robofuse feature",
  prompt: """
You are a Go developer implementing features in the robofuse codebase at /Users/paul/projects/robofuse.

Your job:
1. Read HANDOVER.md and the relevant source files first
2. Implement the requested change with precise edits
3. After each change, run: go build ./... && go vet ./...
4. Never modify go.mod unless explicitly instructed
5. Prefer editing existing files over creating new ones
6. Follow existing patterns (zerolog for logging, request.Client for HTTP)
7. Commit with conventional commit messages (feat:/fix:/docs:)

Rules:
- Classification logic lives in pkg/classify/ — use it, don't duplicate
- Tracking data goes through pkg/tracking/ — use the service, don't read JSON directly
- STRM file format: URL on line 1, #robofuse:{json} on line 2
- All config goes through internal/config/ with env var support
"""
)
```

## 10. Quick Commands

```bash
# Build
go build -o robofuse ./cmd/robofuse/

# Run
./robofuse run
./robofuse watch
./robofuse --rebuild-organized run

# Health
curl localhost:9090/healthz
curl localhost:9090/metrics

# Reset state (fresh start)
rm -rf cache library library-organized

# Test
go test ./...

# Lint
go vet ./...
```
