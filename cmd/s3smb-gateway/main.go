package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/s3smb-gateway/config"
	fusepkg "github.com/s3smb-gateway/fuse"
	"github.com/s3smb-gateway/internal/log"
	"github.com/s3smb-gateway/metadata"
	"github.com/s3smb-gateway/s3client"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Command line flags
	configPath := flag.String("config", "", "Path to configuration file")
	bucket := flag.String("bucket", "", "S3 bucket name")
	region := flag.String("region", "", "AWS region")
	endpoint := flag.String("endpoint", "", "S3-compatible endpoint URL")
	mountPoint := flag.String("mount", "", "Mount point path")
	cacheDir := flag.String("cache", config.DefaultCacheDir, "Cache directory path")
	dbPath := flag.String("db", config.DefaultDBPath, "Database path")
	debug := flag.Bool("debug", false, "Enable debug logging")
	showVersion := flag.Bool("version", false, "Show version information")

	// Sync flags — kept for backwards compatibility; also settable in config JSON
	syncOnStart := flag.Bool("sync", false, "Sync S3 bucket contents to local database on startup (blocks FUSE until complete)")
	syncInterval := flag.Int("sync-interval", 0, "Periodic re-sync interval in seconds (0 = disabled)")

	flag.Parse()

	// Show version
	if *showVersion {
		log.Info("S3SMB-Gateway %s (commit: %s, built: %s)", version, commit, date)
		os.Exit(0)
	}

	// Load or create configuration
	cfg := config.NewDefaultConfig()

	if *configPath != "" {
		loadedCfg, err := config.LoadConfig(*configPath)
		if err != nil {
			log.Error("Error loading config: %v", err)
			os.Exit(1)
		}
		cfg = loadedCfg
	}

	// Override with command line flags
	if *bucket != "" {
		cfg.S3.Bucket = *bucket
	}
	if *region != "" {
		cfg.S3.Region = *region
	}
	if *endpoint != "" {
		cfg.S3.Endpoint = *endpoint
	}
	if *mountPoint != "" {
		cfg.MountPoint = *mountPoint
	}
	if *cacheDir != config.DefaultCacheDir {
		cfg.CacheDir = *cacheDir
	}
	if *dbPath != config.DefaultDBPath {
		cfg.DBPath = *dbPath
	}
	if *debug {
		cfg.Debug = true
	}
	// CLI flags override JSON config for sync settings
	if *syncOnStart {
		cfg.SyncOnStart = true
	}
	if *syncInterval > 0 {
		cfg.SyncInterval = *syncInterval
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Error("Configuration error: %v", err)
		fmt.Fprintf(os.Stderr, "\nUsage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Run the gateway
	if err := run(cfg); err != nil {
		log.Error("Error: %v", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Enable debug logging if configured
	if cfg.Debug {
		log.EnableDebug()
	}

	log.Info("S3SMB-Gateway starting...")
	log.Info("  Bucket:        %s", cfg.S3.Bucket)
	log.Info("  Region:        %s", cfg.S3.Region)
	log.Info("  Mount:         %s", cfg.MountPoint)
	log.Info("  Cache:         %s", cfg.CacheDir)
	log.Info("  DB:            %s", cfg.DBPath)
	log.Info("  Chunk:         %d MB", cfg.ChunkSize/(1024*1024))
	log.Info("  SyncOnStart:   %v", cfg.SyncOnStart)
	if cfg.SyncInterval > 0 {
		log.Info("  SyncInterval:  %ds", cfg.SyncInterval)
	} else {
		log.Info("  SyncInterval:  disabled")
	}

	// Initialize metadata repository (SQLite)
	log.Info("Initializing metadata repository...")
	repo, err := metadata.NewSQLiteRepository(cfg.CacheDir, cfg.Debug)
	if err != nil {
		return fmt.Errorf("failed to initialize metadata repository: %w", err)
	}
	defer repo.Close()

	// Initialize S3 client
	log.Info("Connecting to S3...")
	s3Client, err := s3client.NewClient(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize S3 client: %w", err)
	}

	// Verify S3 connection
	if err := s3Client.BucketExists(ctx); err != nil {
		log.Warn("Could not verify S3 connection: %v", err)
	} else {
		log.Info("S3 connection verified")
	}

	// -------------------------------------------------------------------------
	// Initial sync
	//
	// When SyncOnStart is true we BLOCK here until SyncFromS3 completes (or
	// SyncStartTimeout is exceeded). This ensures every SMB client gets a
	// consistent, fully-populated directory listing from the very first
	// connection — eliminating the "different users see different folders"
	// race that occurred when sync ran as a background goroutine while FUSE
	// was already serving requests.
	// -------------------------------------------------------------------------
	if cfg.SyncOnStart {
		log.Info("Performing initial S3 sync (FUSE will start after this completes)...")

		// Determine the effective timeout for the blocking sync
		syncTimeout := time.Duration(cfg.SyncStartTimeout) * time.Second
		var syncCtx context.Context
		var syncCancel context.CancelFunc
		if syncTimeout > 0 {
			syncCtx, syncCancel = context.WithTimeout(ctx, syncTimeout)
		} else {
			syncCtx, syncCancel = context.WithCancel(ctx)
		}
		defer syncCancel()

		syncOpts := metadata.DefaultSyncOptions()
		syncOpts.Prefix = cfg.S3.Prefix
		syncOpts.ReconcileDeletions = true
		syncOpts.OnProgress = func(synced int, inProgress bool) {
			if inProgress && synced > 0 && synced%100 == 0 {
				log.Info("  Synced %d entries...", synced)
			}
		}

		if err := metadata.SyncFromS3(syncCtx, repo, s3Client, cfg.S3.Bucket, syncOpts); err != nil {
			if syncCtx.Err() == context.DeadlineExceeded {
				// Timeout reached — warn but continue with a partially-synced DB
				// (better than refusing to start at all)
				log.Warn("Initial sync timed out after %v — proceeding with partial metadata. Consider raising sync_start_timeout.", syncTimeout)
			} else {
				// Hard error (S3 unreachable, credentials wrong, etc.)
				return fmt.Errorf("initial S3 sync failed: %w", err)
			}
		} else {
			log.Info("Initial sync complete")
		}
	}

	// -------------------------------------------------------------------------
	// Periodic re-sync
	//
	// After the gateway is running, re-sync at the configured interval so
	// that files added/deleted externally in S3 eventually appear/disappear
	// in the SMB share. Also removes DB entries for objects deleted in S3
	// (ReconcileDeletions=true inside StartPeriodicSync).
	// -------------------------------------------------------------------------
	if cfg.SyncInterval > 0 {
		log.Info("Starting periodic sync every %d seconds", cfg.SyncInterval)
		metadata.StartPeriodicSync(
			ctx,
			repo,
			s3Client,
			cfg.S3.Bucket,
			cfg.S3.Prefix,
			cfg.SyncInterval,
			func(err error) {
				log.Warn("Periodic sync error: %v", err)
			},
		)
	}

	// -------------------------------------------------------------------------
	// Background Write-back Daemon
	// -------------------------------------------------------------------------
	if cfg.WritebackInterval > 0 {
		log.Info("Starting background write-back daemon (interval: %ds, idle timeout: %ds, retention: %ds)", 
			cfg.WritebackInterval, cfg.WritebackIdleTime, cfg.CacheRetentionTime)
		metadata.StartWritebackDaemon(
			ctx,
			repo,
			s3Client,
			cfg.WritebackInterval,
			cfg.WritebackIdleTime,
			cfg.CacheRetentionTime,
			func(err error) {
				log.Warn("Writeback daemon error: %v", err)
			},
		)
	}

	// Create filesystem with metadata repository
	log.Info("Creating filesystem...")
	filesystem, err := fusepkg.NewFS(repo, s3Client, cfg)
	if err != nil {
		return fmt.Errorf("failed to create filesystem: %w", err)
	}

	// Mount filesystem
	log.Info("Mounting filesystem...")
	if err := filesystem.Mount(); err != nil {
		return fmt.Errorf("failed to mount: %w", err)
	}

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start serving in a goroutine
	errCh := make(chan error, 1)
	go func() {
		log.Info("Filesystem mounted at %s", cfg.MountPoint)
		log.Info("Press Ctrl+C to unmount and exit")
		errCh <- filesystem.Serve()
	}()

	// Wait for shutdown signal or error
	select {
	case sig := <-sigCh:
		log.Info("Received signal %v, shutting down...", sig)
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("filesystem error: %w", err)
		}
	}

	// Unmount
	log.Info("Unmounting filesystem...")
	if err := filesystem.Unmount(); err != nil {
		return fmt.Errorf("failed to unmount: %w", err)
	}

	log.Info("Shutdown complete")
	return nil
}
