package config

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		CacheDir:  DefaultCacheDir,
		DBPath:    DefaultDBPath,
		ChunkSize: ChunkSize,
		Debug:     false,
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

	// Load credentials from environment variables (secure method)
	cfg.LoadCredentialsFromEnv()

	return cfg, nil
}

// LoadCredentialsFromEnv loads S3 credentials from environment variables
// This is the recommended secure method for providing credentials
// Supported environment variables:
//   - AWS_ACCESS_KEY_ID / S3_ACCESS_KEY
//   - AWS_SECRET_ACCESS_KEY / S3_SECRET_KEY
//   - AWS_PROFILE / S3_PROFILE
//   - AWS_REGION / S3_REGION (overrides config)
//   - S3_BUCKET (overrides config)
//   - S3_ENDPOINT (overrides config)
func (c *Config) LoadCredentialsFromEnv() {
	// Access Key (AWS standard or custom)
	if key := os.Getenv("AWS_ACCESS_KEY_ID"); key != "" {
		c.S3.AccessKey = key
	} else if key := os.Getenv("S3_ACCESS_KEY"); key != "" {
		c.S3.AccessKey = key
	}

	// Secret Key (AWS standard or custom)
	if key := os.Getenv("AWS_SECRET_ACCESS_KEY"); key != "" {
		c.S3.SecretKey = key
	} else if key := os.Getenv("S3_SECRET_KEY"); key != "" {
		c.S3.SecretKey = key
	}

	// Profile (AWS standard or custom)
	if profile := os.Getenv("AWS_PROFILE"); profile != "" {
		c.S3.Profile = profile
	} else if profile := os.Getenv("S3_PROFILE"); profile != "" {
		c.S3.Profile = profile
	}

	// Region override
	if region := os.Getenv("AWS_REGION"); region != "" {
		c.S3.Region = region
	} else if region := os.Getenv("S3_REGION"); region != "" {
		c.S3.Region = region
	}

	// Bucket override
	if bucket := os.Getenv("S3_BUCKET"); bucket != "" {
		c.S3.Bucket = bucket
	}

	// Endpoint override
	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		c.S3.Endpoint = endpoint
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
