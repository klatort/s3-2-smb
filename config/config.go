package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ChunkSize is 16MB for chunked downloads
	ChunkSize = 16 * 1024 * 1024

	// DefaultCacheDir for local chunk cache
	DefaultCacheDir = "/var/cache/s3smb-gateway"

	// DefaultDBPath for SQLite metadata database
	DefaultDBPath = "/var/lib/s3smb-gateway/metadata.db"
)

// Config holds all configuration for S3SMB-Gateway
type Config struct {
	// S3 Configuration
	S3 S3Config `json:"s3"`

	// Mount point for FUSE filesystem
	MountPoint string `json:"mount_point"`

	// Local cache directory for chunks
	CacheDir string `json:"cache_dir"`

	// SQLite database path for metadata
	DBPath string `json:"db_path"`

	// ChunkSize in bytes (default 16MB)
	ChunkSize int64 `json:"chunk_size"`

	// MaxCacheSize in bytes (0 = unlimited)
	MaxCacheSize int64 `json:"max_cache_size"`

	// Debug enables verbose logging
	Debug bool `json:"debug"`

	// SyncOnStart controls whether the S3 bucket is synced into the local
	// metadata DB at startup. When true, the FUSE filesystem will not begin
	// serving requests until the initial sync completes (or SyncStartTimeout
	// is exceeded), ensuring all clients see a consistent directory listing.
	SyncOnStart bool `json:"sync_on_start"`

	// SyncInterval is the number of seconds between periodic background
	// re-syncs of the S3 bucket into the local metadata DB.
	// Set to 0 to disable periodic sync (only sync at startup if SyncOnStart
	// is true). A value like 3600 (1 hour) or 86400 (24 hours / nightly) is
	// recommended so that files added externally to S3 become visible.
	SyncInterval int `json:"sync_interval"`

	// SyncStartTimeout is the maximum number of seconds to wait for the
	// initial startup sync before allowing FUSE to start serving anyway.
	// Default 120 seconds. 0 means wait forever.
	SyncStartTimeout int `json:"sync_start_timeout"`

	// WritebackIdleTime is the number of seconds a file must remain unmodified
	// before the background daemon uploads it to S3 and cleans the local cache.
	// Default is 300 seconds (5 minutes).
	WritebackIdleTime int `json:"writeback_idle_time"`

	// WritebackInterval is the number of seconds between sweeps of the background
	// write-back daemon. Default is 60 seconds.
	WritebackInterval int `json:"writeback_interval"`

	// CacheRetentionTime is the number of seconds to keep the local staging file
	// around for fast reads after it has been uploaded to S3. 
	// Default is 604800 (7 days). The file is evicted if not accessed.
	CacheRetentionTime int `json:"cache_retention_time"`
}

// S3Config holds S3-specific configuration
type S3Config struct {
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Endpoint string `json:"endpoint,omitempty"` // For S3-compatible services
	Profile  string `json:"profile,omitempty"`  // AWS profile name (from ~/.aws/credentials)
	Prefix   string `json:"prefix,omitempty"`   // Optional prefix/folder in bucket

	// Credentials - DEPRECATED: Use environment variables or AWS profiles instead
	// These are only used as fallback and should not be stored in config files
	AccessKey string `json:"-"` // Excluded from JSON - use AWS_ACCESS_KEY_ID env var
	SecretKey string `json:"-"` // Excluded from JSON - use AWS_SECRET_ACCESS_KEY env var
}

// NewDefaultConfig returns a Config with sensible defaults
func NewDefaultConfig() *Config {
	return &Config{
		CacheDir:          DefaultCacheDir,
		DBPath:            DefaultDBPath,
		ChunkSize:         ChunkSize,
		Debug:             false,
		SyncOnStart:       false,
		SyncInterval:      0,
		SyncStartTimeout:  120,
		WritebackIdleTime: 300,
		WritebackInterval: 60,
		CacheRetentionTime: 604800,
	}
}

// LoadConfig loads configuration from a JSON file
func LoadConfig(path string) (*Config, error) {
	cfg := NewDefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Apply defaults if not set
	if cfg.CacheDir == "" {
		cfg.CacheDir = DefaultCacheDir
	}
	if cfg.DBPath == "" {
		cfg.DBPath = DefaultDBPath
	}
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = ChunkSize
	}

	// Hard-parse the corresponding .env file directly (bypassing Systemd backwards compatibility limitations)
	envPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".env"
	if envData, err := os.ReadFile(envPath); err == nil {
		lines := strings.Split(string(envData), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "AWS_ACCESS_KEY_ID=") {
				cfg.S3.AccessKey = strings.TrimPrefix(line, "AWS_ACCESS_KEY_ID=")
			} else if strings.HasPrefix(line, "AWS_SECRET_ACCESS_KEY=") {
				cfg.S3.SecretKey = strings.TrimPrefix(line, "AWS_SECRET_ACCESS_KEY=")
			}
		}
	}

	// Load credentials from environment variables as secondary override
	cfg.LoadCredentialsFromEnv()

	return cfg, nil
}

// LoadCredentialsFromEnv loads S3 credentials from environment variables
func (c *Config) LoadCredentialsFromEnv() {
	// Access Key (AWS standard or custom)
	if key := os.Getenv("AWS_ACCESS_KEY_ID"); key != "" {
		c.S3.AccessKey = strings.TrimSpace(key)
	} else if key := os.Getenv("S3_ACCESS_KEY"); key != "" {
		c.S3.AccessKey = strings.TrimSpace(key)
	}

	// Secret Key (AWS standard or custom)
	if key := os.Getenv("AWS_SECRET_ACCESS_KEY"); key != "" {
		c.S3.SecretKey = strings.TrimSpace(key)
	} else if key := os.Getenv("S3_SECRET_KEY"); key != "" {
		c.S3.SecretKey = strings.TrimSpace(key)
	}

	// Profile (AWS standard or custom)
	if profile := os.Getenv("AWS_PROFILE"); profile != "" {
		c.S3.Profile = strings.TrimSpace(profile)
	} else if profile := os.Getenv("S3_PROFILE"); profile != "" {
		c.S3.Profile = strings.TrimSpace(profile)
	}

	// Region override
	if region := os.Getenv("AWS_REGION"); region != "" {
		c.S3.Region = strings.TrimSpace(region)
	} else if region := os.Getenv("S3_REGION"); region != "" {
		c.S3.Region = strings.TrimSpace(region)
	}

	// Bucket override
	if bucket := os.Getenv("S3_BUCKET"); bucket != "" {
		c.S3.Bucket = strings.TrimSpace(bucket)
	}

	// Endpoint override
	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		c.S3.Endpoint = strings.TrimSpace(endpoint)
	}
}

// Save writes the configuration to a JSON file
func (c *Config) Save(path string) error {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.S3.Bucket == "" {
		return ErrMissingBucket
	}
	if c.S3.Region == "" {
		return ErrMissingRegion
	}
	if c.MountPoint == "" {
		return ErrMissingMountPoint
	}
	return nil
}

// Custom errors
type ConfigError string

func (e ConfigError) Error() string { return string(e) }

const (
	ErrMissingBucket     = ConfigError("S3 bucket is required")
	ErrMissingRegion     = ConfigError("S3 region is required")
	ErrMissingMountPoint = ConfigError("mount point is required")
)
