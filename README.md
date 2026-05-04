<p align="center">
  <img src="assets/logo.png" alt="robofuse" width="400" />
</p>

<p align="center">
  <a href="https://github.com/itsrenoria/robofuse/stargazers"><img src="https://img.shields.io/github/stars/itsrenoria/robofuse?style=flat-square" alt="Stars"></a>
  <a href="https://github.com/itsrenoria/robofuse/issues"><img src="https://img.shields.io/github/issues/itsrenoria/robofuse?style=flat-square" alt="Issues"></a>
  <a href="https://github.com/itsrenoria/robofuse/blob/main/LICENSE"><img src="https://img.shields.io/github/license/itsrenoria/robofuse?style=flat-square" alt="License"></a>
  <a href="https://github.com/itsrenoria/robofuse"><img src="https://img.shields.io/badge/docker-ready-blue?style=flat-square" alt="Docker"></a>
</p>

# robofuse
> **A high-performance Real-Debrid STRM file generator for your media server.**

**robofuse** is a lightweight, blazing-fast service that interacts with the [Real-Debrid](https://real-debrid.com/) API to automatically organize your movie and TV library. It generates `.strm` files for use with media players like **Infuse**, **Jellyfin**, **Emby**, and ~~**Plex**~~ ([no longer supports `.strm` files](https://www.reddit.com/r/PleX/comments/8gtiv6/strm_file_support/)).

Rewritten from the ground up in **Go**, robofuse is designed for speed, efficiency, and stability.

---

## ✨ Features

- 🚀 **Blazing Fast**: Built with Go's concurrent worker pools for maximum performance.
- 🔄 **Smart Sync**: Only updates what's changed. Adds new files, updates modified ones, and cleans up orphans.
- 🔁 **Rename Tracking**: Detects `.strm` files renamed outside robofuse and preserves them — no duplicate creation or accidental deletion.
- 🛡️ **Intelligent Retries**: Exponential backoff with jitter for 503/429 errors prevents thundering herds. Circuit breaker pauses all workers when the API is struggling.
- 🎬 **Media Probing** *(optional)*: Uses `ffprobe` to capture codec, resolution, bitrate, duration, and audio metadata from your media streams.
- 📄 **Kodi NFO Files**: Automatically generates `.nfo` files alongside `.strm` files for rich metadata in Kodi, Jellyfin, and Emby. Auto-detects movies vs TV episodes.
- 🧹 **Auto-Repair**: Automatically detects dead downloads and re-adds them using cached magnet links.
- 📦 **Paginator**: Handles large libraries with ease by paginating through your Real-Debrid downloads.
- ⏱️ **Watch Mode**: Set it and forget it. Runs continuously in the background to keep your library fresh.
- 🎯 **Deduplication**: Automatically handles duplicate downloads, keeping only the latest version.
- 📝 **Metadata Parsing**: Integrated ptt-go parsing for cleaner, better-organized file names.
- 🔧 **Env Var Config**: Override any config setting via `ROBOFUSE_*` environment variables — ideal for Docker and CI/CD.

---

## 🚀 Installation

### Prerequisites

- **Real-Debrid Account**: You need an API token from your [Real-Debrid Account Panel](https://real-debrid.com/apitoken).
- **ffprobe** *(optional)*: Required only if you enable media probing (`enable_ffprobe: true`). Part of `ffmpeg`. Install via your package manager (`brew install ffmpeg`, `apt install ffmpeg`, etc.).
- **Install method**: Choose [Docker (recommended)](#install-docker), [Binary](#install-binary), or [Go Run](#install-go-run).
- **Go version**: `1.21+` is required only for [Go Run](#install-go-run).

<a id="install-docker"></a>
### 1) Docker (Recommended)

1. Clone the repository:
   ```bash
   git clone https://github.com/itsrenoria/robofuse.git
   cd robofuse
   ```

2. Edit `config.json` based on [Configuration](#configuration):
   ```bash
   nano config.json
   ```

3. Build and start robofuse:
   ```bash
   docker compose up -d --build
   ```

4. View logs:
   ```bash
   docker compose logs -f
   ```

<a id="install-binary"></a>
### 2) Binary

1. Open the [Releases page](https://github.com/itsrenoria/robofuse/releases) and download the asset for your platform.
2. Extract the archive.
3. Edit the included `config.json` based on [Configuration](#configuration).
4. Run directly from the extracted folder:
   ```bash
   ./robofuse run
   ```

<a id="install-go-run"></a>
### 3) Go Run

1. Clone the repository:
   ```bash
   git clone https://github.com/itsrenoria/robofuse.git
   cd robofuse
   ```
2. Edit `config.json` based on [Configuration](#configuration).
3. Run directly:
   ```bash
   go run ./cmd/robofuse run
   ```

### Platform Notes

> [!NOTE]
> Optional explicit config path: use `-c` (or `--config`) if your config file is not in the current directory. You can also set the `ROBOFUSE_CONFIG` environment variable.
> ```bash
> ./robofuse -c /absolute/path/to/config.json run
> ROBOFUSE_CONFIG=/path/to/config.json ./robofuse run
> ```

> [!NOTE]
> macOS: if the downloaded binary is quarantined and won't start:
> ```bash
> xattr -d com.apple.quarantine ./robofuse 2>/dev/null || true
> ```

> [!TIP]
> Linux/macOS optional global install (zsh example):
> ```bash
> mkdir -p ~/.local/bin
> install -m 755 ./robofuse ~/.local/bin/robofuse
> echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
> source ~/.zshrc
> ```

> [!TIP]
> Windows: run with `.\robofuse.exe run` and optionally add the folder containing `robofuse.exe` to your `PATH`.

---

<a id="configuration"></a>
## ⚙️ Configuration

Edit `config.json` to customize robofuse. All settings can also be overridden via environment variables (see [Environment Variables](#environment-variables) below).

### Configuration Options

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `token` | string | **Required** | Your Real-Debrid API Token. |
| `output_dir` | string | `./library` | Where raw STRM files will be generated. |
| `organized_dir` | string | `./library-organized` | Where renamed/organized STRM files will be placed if `ptt_rename` is set to `true`. |
| `cache_dir` | string | `./cache` | Directory for storing state/cache files. |
| `concurrent_requests` | int | `10` | Max concurrent worker threads for unrestrict operations. Lower values reduce API pressure during outages. |
| `general_rate_limit` | int | `60` | Request limit per minute for general API calls (downloads, unrestrict). |
| `torrents_rate_limit` | int | `25` | Request limit per minute for torrent endpoints. |
| `watch_mode` | bool | `false` | If `true`, runs continuously syncing every `watch_mode_interval` seconds. |
| `watch_mode_interval` | int | `60` | Seconds to wait between sync cycles in watch mode. |
| `repair_torrents` | bool | `true` | Automatically attempt to repair dead downloads by re-adding magnets. |
| `min_file_size_mb` | int | `150` | Ignore video files smaller than this size (prevents samples/ads). Subtitles are never filtered by size. |
| `ptt_rename` | bool | `true` | Use PTT parsing to clean filenames and organize into Movies/Series/Anime folders. |
| `log_level` | string | `"info"` | Logging verbosity (`debug`, `info`, `warn`, `error`). |
| `tracking_file` | string | `./cache/file_tracking.json` | Path to the file tracking database (stores per-file metadata and link freshness). |
| `file_expiry_days` | int | `6` | Days after which a download link is considered expired and refreshed. |
| `retry_queue_file` | string | `./cache/retry_queue.json` | Path to the persistent retry queue for cross-cycle retries. |
| `max_retry_attempts` | int | `3` | Maximum cross-cycle retry attempts for queued items before giving up. |
| `enable_ffprobe` | bool | `false` | Run `ffprobe` on download URLs to capture video/audio stream metadata (requires `ffprobe` installed). |
| `ffprobe_path` | string | `"ffprobe"` | Path to the `ffprobe` binary. |
| `ffprobe_timeout` | int | `15` | Timeout in seconds for each ffprobe call. |
| `tmdb_api_key` | string | — | TheMovieDB API v3 key for metadata enrichment, renaming, and poster/backdrop images. Get one at themoviedb.org. |
| `movie_name_template` | string | `"{title} ({year})"` | Filename template for movies. See [Filename Templates](#filename-templates). |
| `episode_name_template` | string | `"{title} S{season:02d}E{episode:02d}"` | Filename template for TV episodes. |
| `folder_rules` | array | `[]` | Custom routing rules. See [Folder Rules](#folder-rules). |
| `adult_patterns` | array | `[]` | Deprecated. Use `folder_rules` with `"target": "X"` and `"skip_tmdb": true`. |
| `title_overrides` | object | `{}` | Manual torrent folder → TMDB search title mappings. See [Title Overrides](#title-overrides). |
| `exclude_keywords_file` | string | — | Path to a text file with one keyword per line (case-insensitive). Torrents matching any keyword are skipped. |

> [!IMPORTANT]
> The default concurrency and rate limits are tuned for stability. With large libraries (800+ files), raising `concurrent_requests` or `general_rate_limit` can trigger Real-Debrid's server-side rate limiting (503 errors) and cause a retry cascade. Start low and increase gradually.

> [!IMPORTANT]
> Don't delete `library`, `library-organized`, or `cache` by hand. These folders are part of the state/tracking system. If you need to reset, stop the service, back up what you need, then clear them intentionally.

<a id="environment-variables"></a>
### Environment Variables

Every config key can be set via an environment variable with the `ROBOFUSE_` prefix. Env vars **override** the config file values. Only non-empty env vars are applied — unset vars leave the config file or default value intact.

| Env var | Maps to |
|---------|---------|
| `ROBOFUSE_CONFIG` | Path to config file (checked before default paths) |
| `ROBOFUSE_TOKEN` | `token` |
| `ROBOFUSE_OUTPUT_DIR` | `output_dir` |
| `ROBOFUSE_ORGANIZED_DIR` | `organized_dir` |
| `ROBOFUSE_CACHE_DIR` | `cache_dir` |
| `ROBOFUSE_CONCURRENT_REQUESTS` | `concurrent_requests` |
| `ROBOFUSE_GENERAL_RATE_LIMIT` | `general_rate_limit` |
| `ROBOFUSE_TORRENTS_RATE_LIMIT` | `torrents_rate_limit` |
| `ROBOFUSE_WATCH_MODE` | `watch_mode` |
| `ROBOFUSE_WATCH_MODE_INTERVAL` | `watch_mode_interval` |
| `ROBOFUSE_REPAIR_TORRENTS` | `repair_torrents` |
| `ROBOFUSE_MIN_FILE_SIZE_MB` | `min_file_size_mb` |
| `ROBOFUSE_LOG_LEVEL` | `log_level` |
| `ROBOFUSE_PTT_RENAME` | `ptt_rename` |
| `ROBOFUSE_TRACKING_FILE` | `tracking_file` |
| `ROBOFUSE_FILE_EXPIRY_DAYS` | `file_expiry_days` |
| `ROBOFUSE_RETRY_QUEUE_FILE` | `retry_queue_file` |
| `ROBOFUSE_MAX_RETRY_ATTEMPTS` | `max_retry_attempts` |
| `ROBOFUSE_ENABLE_FFPROBE` | `enable_ffprobe` |
| `ROBOFUSE_FFPROBE_PATH` | `ffprobe_path` |
| `ROBOFUSE_FFPROBE_TIMEOUT` | `ffprobe_timeout` |
| `ROBOFUSE_TMDB_API_KEY` | `tmdb_api_key` |
| `ROBOFUSE_MOVIE_NAME_TEMPLATE` | `movie_name_template` |
| `ROBOFUSE_EPISODE_NAME_TEMPLATE` | `episode_name_template` |

Bool values accept `true`, `false`, `1`, `0` (per `strconv.ParseBool`). Int values must be valid decimal integers. Strings are used as-is.

**Examples:**

```bash
# Override just the token and log level
ROBOFUSE_TOKEN=abc123 ROBOFUSE_LOG_LEVEL=debug ./robofuse run

# Run with a custom config file
ROBOFUSE_CONFIG=/etc/robofuse/config.json ./robofuse run

# Docker: pass env vars inline
docker run -e ROBOFUSE_TOKEN=abc123 -e ROBOFUSE_CONCURRENT_REQUESTS=5 ...
```

<a id="filename-templates"></a>
### Filename Templates

Customize how STRM files are named using metadata placeholders. Templates are applied after TMDB matching and ffprobe analysis.

**Movie template** (`movie_name_template`):
```
"{title} ({year}) [{resolution} {hdr} {bitrate}] [{audio_langs}][{sub_langs}]"
```

**Episode template** (`episode_name_template`):
```
"{title} - S{season:02d}E{episode:02d} - {episode_title} [{resolution} {hdr}] [{audio_langs}][{sub_langs}]"
```

**Available placeholders:**

| Placeholder | Source | Example |
|-------------|--------|---------|
| `{title}` | TMDB → PTT | `Arcane` |
| `{original_title}` | TMDB | `Arcane` |
| `{year}` | TMDB → PTT | `2024` |
| `{season}` | PTT | `2` |
| `{season:02d}` | PTT (zero-padded) | `02` |
| `{episode}` | PTT | `1` |
| `{episode:02d}` | PTT (zero-padded) | `01` |
| `{resolution}` | ffprobe | `2160p`, `1080p` |
| `{hdr}` | ffprobe | `Dolby Vision`, `HDR` |
| `{bitrate}` | ffprobe | `17 Mbps` |
| `{codec}` | ffprobe | `HEVC`, `AVC` |
| `{audio_codec}` | ffprobe | `DDP5.1` |
| `{audio_langs}` | ffprobe | `EN,RU` |
| `{sub_langs}` | — | *not yet populated* |
| `{extension}` | original file | `.mkv` |

Leave templates empty (or omit the keys) to keep the default naming.

<a id="folder-rules"></a>
### Folder Rules

Route torrents to custom folders based on name patterns. Each rule has:
- `pattern` — case-insensitive substring match on the torrent folder name
- `target` — destination folder (e.g. `"X"`, `"Anime"`, `"Documentary"`)
- `skip_tmdb` — skip TMDB matching for this content

```json
"folder_rules": [
  {"pattern": "Viv Thomas", "target": "X", "skip_tmdb": true},
  {"pattern": "Tushy",      "target": "X", "skip_tmdb": true},
  {"pattern": "Anime",      "target": "Anime"}
]
```

Files matching a rule go to `organized_dir/<target>/` with their original folder name preserved (no TMDB renaming).

<a id="title-overrides"></a>
### Title Overrides

Manual mappings for torrents whose folder name doesn't contain the show title (e.g. season-only folders like `"Сезон 1 (1984-1985)"`).

```json
"title_overrides": {
  "Сезон 1 (1984-1985)": "Miami Vice",
  "Season 1": "The Wire"
}
```

The torrent folder name (exact match) is replaced with the override value for TMDB search. This is equivalent to Sonarr/Radarr's manual series mapping.

---

## 📁 File Formats

### .strm files

STRM files contain the download URL on line 1 and robofuse metadata on line 2:

```
https://download.real-debrid.com/...
# robofuse: link=https://real-debrid.com/d/TP2D7RL7LPGTGE95 torrent=ABC123
```

- **Line 1**: The direct download URL — read by Kodi, Jellyfin, Emby, Infuse.
- **Line 2**: A comment (ignored by players) containing the stable Real-Debrid link and torrent ID. This is used internally for **rename tracking** — if you rename a `.strm` file outside robofuse, the next sync matches it by Link instead of path and preserves your rename.

### .nfo files *(with ffprobe enabled)*

When `enable_ffprobe: true`, robofuse generates Kodi-compatible `.nfo` files alongside every `.strm` file. The NFO contains:

- **Title, year** — parsed from the filename via ptt-go.
- **Season, episode** — for TV series, auto-detected from filename patterns.
- **Stream details** — codec, resolution, bitrate, duration, audio channels, and language from ffprobe.

Example movie NFO:
```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<movie>
  <title>Movie Title</title>
  <year>2024</year>
  <fileinfo>
    <streamdetails>
      <video>
        <codec>hevc</codec>
        <width>3840</width>
        <height>2160</height>
        <bitrate>25000000</bitrate>
        <durationinseconds>5535</durationinseconds>
      </video>
      <audio>
        <codec>aac</codec>
        <channels>6</channels>
        <language>eng</language>
      </audio>
    </streamdetails>
  </fileinfo>
</movie>
```

NFO files are written at `.strm` creation time and refreshed asynchronously after ffprobe completes.

---

## 🎮 Usage

### Docker

The included `docker-compose.yml` builds and runs robofuse:

```yaml
services:
  robofuse:
    build: .
    container_name: robofuse
    restart: unless-stopped
    volumes:
      - ./config.json:/data/config.json
      - ./library:/app/library
      - ./library-organized:/app/library-organized
      - ./cache:/app/cache
```

```bash
# Start in background
docker compose up -d --build

# View logs
docker compose logs -f

# Stop
docker compose down

# Rebuild after code changes
docker compose up -d --build
```

### Binary

```bash
# One sync run
./robofuse run

# Continuous watch mode
./robofuse watch

# Preview changes only
./robofuse dry-run

# Override settings via env vars
ROBOFUSE_CONCURRENT_REQUESTS=5 ROBOFUSE_LOG_LEVEL=debug ./robofuse watch
```

### Go Run

```bash
# One sync run
go run ./cmd/robofuse run

# Continuous watch mode
go run ./cmd/robofuse watch

# Preview changes only
go run ./cmd/robofuse dry-run
```

---

## ❤️ Support the Project

robofuse is a passion project developed and maintained for free. If you find it useful, please consider supporting its development.

- ⭐ **Star the Repository** on GitHub
- 🤝 **Contribute**:
  - **Bug Reports**: Open an issue describing the bug with steps to reproduce
  - **Feature Requests**: Open an issue describing the new feature and why it would be useful
  - **Code Contributions**: Submit a pull request with your improvements

## 🙏 Credits

This project wouldn't be possible without the foundational work of the open-source community.

- **[ptt-go](https://github.com/itsrenoria/ptt-go)**: Our Go port of the excellent [dreulavelle/PTT](https://github.com/dreulavelle/PTT) filename parsing library.
- **[Decypharr](https://github.com/sirrobot01/decypharr)**: Portions of this codebase were inspired by or repurposed from Decypharr (Copyright (c) 2025 Mukhtar Akere), used under the MIT License.

---

*Not affiliated with Real-Debrid.*
