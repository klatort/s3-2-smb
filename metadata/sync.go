package metadata

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Client defines the interface for S3 operations needed by SyncFromS3
type S3Client interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// SyncOptions configures the S3 sync behavior
type SyncOptions struct {
	// Prefix to sync (empty string for entire bucket)
	Prefix string

	// MaxKeys per request (default 1000)
	MaxKeys int32

	// OnProgress is called periodically with sync progress
	OnProgress func(synced int, inProgress bool)

	// ClearExisting removes all existing entries before sync
	ClearExisting bool

	// ReconcileDeletions when true removes DB entries for objects that no
	// longer exist in S3. This is more expensive (requires building a full
	// in-memory set of all S3 keys) but ensures deleted files don't linger
	// in the local metadata DB after external removal from S3.
	ReconcileDeletions bool
}

// DefaultSyncOptions returns default sync options
func DefaultSyncOptions() *SyncOptions {
	return &SyncOptions{
		Prefix:             "",
		MaxKeys:            1000,
		ClearExisting:      false,
		ReconcileDeletions: true,
	}
}

// SyncFromS3 populates the metadata repository from an S3 bucket
// using ListObjectsV2 to enumerate all objects.
//
// When opts.ReconcileDeletions is true, any entry in the local DB that no
// longer has a corresponding object in S3 will be removed, keeping the two
// in sync even after external deletions.
func SyncFromS3(ctx context.Context, repo Repository, client S3Client, bucket string, opts *SyncOptions) error {
	if opts == nil {
		opts = DefaultSyncOptions()
	}

	if opts.MaxKeys <= 0 {
		opts.MaxKeys = 1000
	}

	// Clear existing entries if requested
	if opts.ClearExisting {
		if sqliteRepo, ok := repo.(*SQLiteRepository); ok {
			if err := sqliteRepo.Clear(ctx); err != nil {
				return fmt.Errorf("failed to clear existing entries: %w", err)
			}
		}
	}

	// Track directories we've seen (S3 doesn't always have explicit directory objects)
	seenDirs := make(map[string]bool)

	// When reconciling deletions we collect every S3 key we encounter, then
	// compare against DB entries and purge those that are absent from S3.
	var s3Keys map[string]struct{}
	if opts.ReconcileDeletions {
		s3Keys = make(map[string]struct{})
	}

	var continuationToken *string
	synced := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		input := &s3.ListObjectsV2Input{
			Bucket:  aws.String(bucket),
			MaxKeys: aws.Int32(opts.MaxKeys),
		}

		if opts.Prefix != "" {
			input.Prefix = aws.String(opts.Prefix)
		}

		if continuationToken != nil {
			input.ContinuationToken = continuationToken
		}

		resp, err := client.ListObjectsV2(ctx, input)
		if err != nil {
			return fmt.Errorf("failed to list objects: %w", err)
		}

		// Process each object
		for _, obj := range resp.Contents {
			key := aws.ToString(obj.Key)
			if key == "" {
				continue
			}

			// Normalize the path (remove prefix if specified)
			path := key
			if opts.Prefix != "" {
				path = strings.TrimPrefix(path, opts.Prefix)
				path = strings.TrimPrefix(path, "/")
			}

			if path == "" {
				continue
			}

			// Check if this is a directory marker (ends with / or size 0 with directory-like key)
			isDir := strings.HasSuffix(key, "/")
			if isDir {
				path = strings.TrimSuffix(path, "/")
			}

			// Track for deletion reconciliation
			if s3Keys != nil {
				s3Keys[path] = struct{}{}
			}

			// Ensure parent directories exist
			if err := ensureParentDirs(ctx, repo, path, seenDirs); err != nil {
				return err
			}

			// Create the entry from S3 listing data
			// Get accurate size from HeadObject if ListObjectsV2 returns suspicious size
			// Huawei OBS sometimes returns incorrect large sizes (~824GB) in ListObjectsV2
			size := aws.ToInt64(obj.Size)
			if size > 1024*1024*1024 { // If size > 1GB, verify with HeadObject
				headResp, err := client.HeadObject(ctx, &s3.HeadObjectInput{
					Bucket: aws.String(bucket),
					Key:    obj.Key,
				})
				if err == nil && headResp.ContentLength != nil {
					// Use the accurate size from HeadObject
					size = *headResp.ContentLength
				}
			}

			s3ModTime := aws.ToTime(obj.LastModified)

			// Guard: do NOT overwrite DB entries that are fresher than the S3 listing.
			// This prevents the background sync from clobbering metadata that was
			// just updated by Flush() after a user save.
			var existingXattrs XattrMap
			existing, getErr := repo.GetEntry(ctx, path)
			if getErr == nil {
				if existing.ModTime.After(s3ModTime) || existing.LocalDirty {
					// DB entry is newer or has pending local edits. Skip size/time
					// update but still record the key for deletion reconciliation.
					synced++
					continue
				}
				// Preserve extended attributes (ACLs, POSIX ownership) that
				// only exist in the DB and have no S3 equivalent.
				if existing.Xattrs != nil {
					existingXattrs = existing.Xattrs
				}
				// If S3 is newer but we have a cached local staging file, the cache is stale!
				if existing.LocalStagingPath != "" {
					_ = os.Remove(existing.LocalStagingPath)
					// LocalStagingPath will be reset to "" in the new FileEntry below
				}
			}

			// Create the entry
			entry := &FileEntry{
				Path:         path,
				Size:         size,
				ModTime:      s3ModTime,
				IsDir:        isDir,
				ETag:         strings.Trim(aws.ToString(obj.ETag), "\""),
				S3VerifiedAt: time.Now(),
			}

			// Carry over existing xattrs if the entry existed
			if existingXattrs != nil {
				entry.Xattrs = existingXattrs
			}

			if err := repo.UpdateEntry(ctx, entry); err != nil {
				return fmt.Errorf("failed to update entry %s: %w", path, err)
			}

			synced++

			// Report progress
			if opts.OnProgress != nil && synced%100 == 0 {
				opts.OnProgress(synced, true)
			}
		}

		// Check if there are more results
		if !aws.ToBool(resp.IsTruncated) {
			break
		}

		continuationToken = resp.NextContinuationToken
	}

	// Final progress report
	if opts.OnProgress != nil {
		opts.OnProgress(synced, false)
	}

	// Reconcile deletions: remove any DB entries that no longer exist in S3.
	// This handles files deleted externally (direct S3 console/CLI/API delete)
	// that would otherwise linger in the local DB indefinitely.
	if opts.ReconcileDeletions && s3Keys != nil {
		if err := reconcileDeletedEntries(ctx, repo, s3Keys); err != nil {
			// Non-fatal: log-worthy but don't abort the whole sync
			return fmt.Errorf("deletion reconciliation failed (sync data is still valid): %w", err)
		}
	}

	return nil
}

// reconcileDeletedEntries removes DB entries for paths that are no longer in S3.
// It only removes FILE entries (not directories) to avoid false-positive removal
// of directories that exist implicitly through their children.
func reconcileDeletedEntries(ctx context.Context, repo Repository, s3Keys map[string]struct{}) error {
	sqliteRepo, ok := repo.(*SQLiteRepository)
	if !ok {
		// Can't enumerate all entries without the SQLite-specific API; skip.
		return nil
	}

	// List all file entries (non-directory) from the DB
	var allEntries []*FileEntry
	result := sqliteRepo.GetDB().WithContext(ctx).
		Where("is_dir = ?", false).
		Find(&allEntries)
	if result.Error != nil {
		return fmt.Errorf("failed to list DB entries for reconciliation: %w", result.Error)
	}

	for _, entry := range allEntries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if _, existsInS3 := s3Keys[entry.Path]; !existsInS3 {
			// File is in the DB but not in S3 — remove it
			if err := repo.DeleteEntry(ctx, entry.Path); err != nil {
				// Log but continue — one failure shouldn't abort all reconciliation
				_ = err
			}
		}
	}

	return nil
}

// ensureParentDirs creates directory entries for all parent directories
func ensureParentDirs(ctx context.Context, repo Repository, path string, seenDirs map[string]bool) error {
	parts := strings.Split(path, "/")

	// Skip the last part (the file/dir itself)
	for i := 1; i < len(parts); i++ {
		dirPath := strings.Join(parts[:i], "/")

		if seenDirs[dirPath] {
			continue
		}

		// Check if directory already exists
		_, err := repo.GetEntry(ctx, dirPath)
		if err == nil {
			seenDirs[dirPath] = true
			continue
		}

		if err != ErrNotFound {
			return err
		}

		// Create directory entry
		entry := &FileEntry{
			Path:    dirPath,
			Size:    0,
			ModTime: time.Now(),
			IsDir:   true,
		}

		if err := repo.UpdateEntry(ctx, entry); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dirPath, err)
		}

		seenDirs[dirPath] = true
	}

	return nil
}

// SyncStats holds statistics from a sync operation
type SyncStats struct {
	TotalEntries int
	Files        int
	Directories  int
	Duration     time.Duration
}

// SyncFromS3WithStats performs sync and returns statistics
func SyncFromS3WithStats(ctx context.Context, repo Repository, client S3Client, bucket string, opts *SyncOptions) (*SyncStats, error) {
	start := time.Now()

	if err := SyncFromS3(ctx, repo, client, bucket, opts); err != nil {
		return nil, err
	}

	// Count entries
	stats := &SyncStats{
		Duration: time.Since(start),
	}

	// Get counts from repository
	if sqliteRepo, ok := repo.(*SQLiteRepository); ok {
		count, err := sqliteRepo.EntryCount(ctx)
		if err == nil {
			stats.TotalEntries = int(count)
		}

		// Count files vs directories
		var fileCount, dirCount int64
		sqliteRepo.GetDB().WithContext(ctx).Model(&FileEntry{}).Where("is_dir = ?", false).Count(&fileCount)
		sqliteRepo.GetDB().WithContext(ctx).Model(&FileEntry{}).Where("is_dir = ?", true).Count(&dirCount)
		stats.Files = int(fileCount)
		stats.Directories = int(dirCount)
	}

	return stats, nil
}

// StartPeriodicSync starts a background goroutine that re-runs SyncFromS3 at
// the given interval. It returns immediately; the goroutine runs until ctx is
// cancelled.
//
// intervalSeconds must be > 0. If it is 0 or negative this function is a no-op.
//
// Each sync run uses ReconcileDeletions=true so that files removed externally
// from S3 are eventually purged from the local DB.
func StartPeriodicSync(ctx context.Context, repo Repository, client S3Client, bucket, prefix string, intervalSeconds int, onError func(error)) {
	if intervalSeconds <= 0 {
		return
	}

	interval := time.Duration(intervalSeconds) * time.Second

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				opts := DefaultSyncOptions()
				opts.Prefix = prefix
				opts.ReconcileDeletions = true

				if err := SyncFromS3(ctx, repo, client, bucket, opts); err != nil {
					if ctx.Err() != nil {
						// Context was cancelled during sync — this is expected on shutdown
						return
					}
					if onError != nil {
						onError(err)
					}
				}
			}
		}
	}()
}
