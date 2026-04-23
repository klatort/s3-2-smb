package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
	sync := flag.Bool("sync", false, "Sync S3 bucket contents to local database on startup")

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

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		log.Error("Configuration error: %v", err)
		fmt.Fprintf(os.Stderr, "\nUsage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Run the gateway
	if err := run(cfg, *sync); err != nil {
		log.Error("Error: %v", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config, syncOnStart bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Enable debug logging if configured
	if cfg.Debug {
		log.EnableDebug()
	}

	log.Info("S3SMB-Gateway starting...")
	log.Info("  Bucket: %s", cfg.S3.Bucket)
	log.Info("  Region: %s", cfg.S3.Region)
	log.Info("  Mount:  %s", cfg.MountPoint)
	log.Info("  Cache:  %s", cfg.CacheDir)
	log.Info("  DB:     %s", cfg.DBPath)
	log.Info("  Chunk:  %d MB", cfg.ChunkSize/(1024*1024))

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

	// Sync S3 contents if requested (populate metadata from S3 in background)
	if syncOnStart {
		log.Info("Starting background sync of S3 bucket to metadata database...")
		
		go func() {
			syncOpts := metadata.DefaultSyncOptions()
			syncOpts.Prefix = cfg.S3.Prefix
			syncOpts.OnProgress = func(synced int, inProgress bool) {
				if inProgress && synced > 0 && synced%100 == 0 {
					log.Info("  Synced %d entries...", synced)
				}
			}
			if err := metadata.SyncFromS3(ctx, repo, s3Client, cfg.S3.Bucket, syncOpts); err != nil {
				log.Warn("Sync failed: %v", err)
			} else {
				log.Info("Sync complete")
			}
		}()
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
		return fmt.Errorf("failed to mount filesystem: %w", err)
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
