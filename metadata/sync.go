package metadata

import (
	"context"
	"fmt"
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
}

// DefaultSyncOptions returns default sync options
func DefaultSyncOptions() *SyncOptions {
	return &SyncOptions{
		Prefix:        "",
		MaxKeys:       1000,
		ClearExisting: false,
	}
}

// SyncFromS3 populates the metadata repository from an S3 bucket
// using ListObjectsV2 to enumerate all objects
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
				existing, getErr := repo.GetEntry(ctx, path)
				if getErr == nil {
					if existing.ModTime.After(s3ModTime) {
						// DB entry is newer — a recent Flush wrote it. Skip.
						synced++
						continue
					}
					// Preserve extended attributes (ACLs, POSIX ownership) that
					// only exist in the DB and have no S3 equivalent.
					if existing.Xattrs != nil {
						// Will be carried over to the new entry below
					}
				}

				// Create the entry
				entry := &FileEntry{
					Path:    path,
					Size:    size,
					ModTime: s3ModTime,
					IsDir:   isDir,
					ETag:    strings.Trim(aws.ToString(obj.ETag), "\""),
				}

				// Carry over existing xattrs if the entry existed
				if existing != nil && existing.Xattrs != nil {
					entry.Xattrs = existing.Xattrs
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
