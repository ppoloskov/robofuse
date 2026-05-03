package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// config.go loads, validates, and exposes application configuration.

var instance *Config

// Config holds the application configuration
type Config struct {
	Token              string `json:"token"`
	OutputDir          string `json:"output_dir"`
	OrganizedDir       string `json:"organized_dir"`
	CacheDir           string `json:"cache_dir"`
	ConcurrentRequests int    `json:"concurrent_requests"`
	GeneralRateLimit   int    `json:"general_rate_limit"`
	TorrentsRateLimit  int    `json:"torrents_rate_limit"`
	WatchMode          bool   `json:"watch_mode"`
	WatchModeInterval  int    `json:"watch_mode_interval"`
	RepairTorrents     bool   `json:"repair_torrents"`
	MinFileSizeMB      int    `json:"min_file_size_mb"`
	LogLevel           string `json:"log_level"`
	PttRename          bool   `json:"ptt_rename"`

	// File tracking
	TrackingFile   string `json:"tracking_file"`
	FileExpiryDays int    `json:"file_expiry_days"`

	// Retry queue
	RetryQueueFile   string `json:"retry_queue_file"`
	MaxRetryAttempts int    `json:"max_retry_attempts"`

	// ffprobe media probing
	EnableFFProbe bool   `json:"enable_ffprobe"` // whether to probe media streams
	FFProbePath   string `json:"ffprobe_path"`   // path to ffprobe binary (default "ffprobe")
	FFProbeTimeout int   `json:"ffprobe_timeout"` // timeout in seconds (default 15)

	// Content filtering
	ExcludeKeywordsFile string   `json:"exclude_keywords_file"` // path to file with one keyword per line (case-insensitive)
	AdultPatterns       []string `json:"adult_patterns"`        // torrent folder substrings that route to X/ folder, skip TMDB

	// TMDB integration
	TMDBAPIKey string `json:"tmdb_api_key"` // TheMovieDB API v3 key for metadata + renaming

	// Internal
	Path string `json:"-"` // Config file path
}

// defaults returns a Config with default values
func defaults() *Config {
	return &Config{
		Token:              "",
		OutputDir:          "./library",
		OrganizedDir:       "./library-organized",
		CacheDir:           "./cache",
		ConcurrentRequests: 10,
		GeneralRateLimit:   60,
		TorrentsRateLimit:  25,
		WatchMode:          false,
		WatchModeInterval:  60,
		RepairTorrents:     true,
		MinFileSizeMB:      150,
		LogLevel:           "info",
		PttRename:          true,

		TrackingFile:   "./cache/file_tracking.json",
		FileExpiryDays: 6,

		RetryQueueFile:   "./cache/retry_queue.json",
		MaxRetryAttempts: 3,

		EnableFFProbe:  false,
		FFProbePath:    "ffprobe",
		FFProbeTimeout: 15,
	}
}

// Load reads configuration from a JSON file. Environment variables with the
// prefix ROBOFUSE_ override any matching config values. The config file
// location can be set via the ROBOFUSE_CONFIG env var.
func Load(configPath string) (*Config, error) {
	cfg := defaults()

	// Try to find config file — ROBOFUSE_CONFIG env var takes priority
	// over the default search paths.
	envConfigPath := os.Getenv("ROBOFUSE_CONFIG")
	paths := []string{
		configPath,
		envConfigPath,
		"config.json",
		"/data/config.json",
		filepath.Join(os.Getenv("HOME"), ".config/robofuse/config.json"),
	}

	var configFile string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			configFile = p
			break
		}
	}

	if configFile == "" {
		return nil, fmt.Errorf("config file not found in any of: %v", paths)
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	cfg.Path = filepath.Dir(configFile)

	// Apply environment variable overrides (ROBOFUSE_*).
	// These take precedence over file-based values.
	cfg.applyEnvOverrides()

	// Validate
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks the configuration for required fields
func (c *Config) Validate() error {
	if c.Token == "" || c.Token == "YOUR_RD_API_TOKEN" {
		return fmt.Errorf("Real-Debrid API token is required")
	}

	if c.ConcurrentRequests < 1 {
		c.ConcurrentRequests = 10
	}

	if c.GeneralRateLimit < 1 {
		c.GeneralRateLimit = 60
	}

	if c.TorrentsRateLimit < 1 {
		c.TorrentsRateLimit = 25
	}

	if c.WatchModeInterval < 10 {
		c.WatchModeInterval = 60
	}

	return nil
}

// SetInstance sets the global config instance.
func SetInstance(cfg *Config) {
	instance = cfg
}

// IsAdultFolder returns true if the folder name matches any adult pattern.
func (c *Config) IsAdultFolder(folderName string) bool {
	for _, p := range c.AdultPatterns {
		if p == "" {
			continue
		}
		if strings.Contains(strings.ToLower(folderName), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// MinFileSizeBytes returns minimum file size in bytes
func (c *Config) MinFileSizeBytes() int64 {
	return int64(c.MinFileSizeMB) * 1024 * 1024
}

// applyEnvOverrides applies ROBOFUSE_* environment variables on top of the
// file-loaded config. Only set (non-empty) variables override; unset variables
// leave the existing value untouched.
func (c *Config) applyEnvOverrides() {
	// Helper closures to keep the code compact.
	envStr := func(key string, target *string) {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}
	envInt := func(key string, target *int) {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*target = n
			}
		}
	}
	envBool := func(key string, target *bool) {
		if v := os.Getenv(key); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*target = b
			}
		}
	}

	// Core settings
	envStr("ROBOFUSE_TOKEN", &c.Token)
	envStr("ROBOFUSE_OUTPUT_DIR", &c.OutputDir)
	envStr("ROBOFUSE_ORGANIZED_DIR", &c.OrganizedDir)
	envStr("ROBOFUSE_CACHE_DIR", &c.CacheDir)
	envInt("ROBOFUSE_CONCURRENT_REQUESTS", &c.ConcurrentRequests)
	envInt("ROBOFUSE_GENERAL_RATE_LIMIT", &c.GeneralRateLimit)
	envInt("ROBOFUSE_TORRENTS_RATE_LIMIT", &c.TorrentsRateLimit)
	envBool("ROBOFUSE_WATCH_MODE", &c.WatchMode)
	envInt("ROBOFUSE_WATCH_MODE_INTERVAL", &c.WatchModeInterval)
	envBool("ROBOFUSE_REPAIR_TORRENTS", &c.RepairTorrents)
	envInt("ROBOFUSE_MIN_FILE_SIZE_MB", &c.MinFileSizeMB)
	envStr("ROBOFUSE_LOG_LEVEL", &c.LogLevel)
	envBool("ROBOFUSE_PTT_RENAME", &c.PttRename)

	// Tracking
	envStr("ROBOFUSE_TRACKING_FILE", &c.TrackingFile)
	envInt("ROBOFUSE_FILE_EXPIRY_DAYS", &c.FileExpiryDays)

	// Retry queue
	envStr("ROBOFUSE_RETRY_QUEUE_FILE", &c.RetryQueueFile)
	envInt("ROBOFUSE_MAX_RETRY_ATTEMPTS", &c.MaxRetryAttempts)

	// ffprobe
	envBool("ROBOFUSE_ENABLE_FFPROBE", &c.EnableFFProbe)
	envStr("ROBOFUSE_FFPROBE_PATH", &c.FFProbePath)
	envInt("ROBOFUSE_FFPROBE_TIMEOUT", &c.FFProbeTimeout)
}
