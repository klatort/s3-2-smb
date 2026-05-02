package metadata

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/s3smb-gateway/internal/log"
)

// UploaderClient defines the S3 operations needed for the write-back daemon
type UploaderClient interface {
	PutObjectFromReader(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error
}

// StartWritebackDaemon runs a background loop that periodically scans for
// files with LocalDirty=true that have been unmodified for WritebackIdleTime,
// uploads them to S3. It also sweeps the local read cache and evicts files
// that have not been accessed for retentionTimeSeconds.
func StartWritebackDaemon(ctx context.Context, repo Repository, client UploaderClient, intervalSeconds int, idleTimeSeconds int, retentionTimeSeconds int, onError func(error)) {
	if intervalSeconds <= 0 {
		return
	}

	interval := time.Duration(intervalSeconds) * time.Second
	idleTime := time.Duration(idleTimeSeconds) * time.Second
	retentionTime := time.Duration(retentionTimeSeconds) * time.Second

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := processWriteback(ctx, repo, client, idleTime); err != nil {
					if ctx.Err() != nil {
						return
					}
					if onError != nil {
						onError(fmt.Errorf("writeback error: %w", err))
					}
				}
				if err := processCacheEviction(ctx, repo, retentionTime); err != nil {
					if ctx.Err() != nil {
						return
					}
					if onError != nil {
						onError(fmt.Errorf("cache eviction error: %w", err))
					}
				}
			}
		}
	}()
}

func processWriteback(ctx context.Context, repo Repository, client UploaderClient, idleTime time.Duration) error {
	sqliteRepo, ok := repo.(*SQLiteRepository)
	if !ok {
		return fmt.Errorf("writeback daemon requires SQLiteRepository")
	}

	var dirtyEntries []FileEntry
	if err := sqliteRepo.GetDB().WithContext(ctx).Where("local_dirty = ?", true).Find(&dirtyEntries).Error; err != nil {
		return fmt.Errorf("failed to list dirty files: %w", err)
	}

	now := time.Now()
	for _, entry := range dirtyEntries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check if file is "out of use" (idle)
		if now.Sub(entry.ModTime) < idleTime {
			continue
		}

		log.Info("Writeback: uploading idle file %s to S3", entry.Path)

		// Ensure staging file exists
		if entry.LocalStagingPath == "" {
			log.Warn("Writeback: dirty file %s has no staging path, unmarking dirty", entry.Path)
			_ = repo.UpdateEntryFields(ctx, entry.Path, map[string]interface{}{"local_dirty": false})
			continue
		}

		file, err := os.Open(entry.LocalStagingPath)
		if err != nil {
			if os.IsNotExist(err) {
				log.Warn("Writeback: staging file missing for %s, unmarking dirty", entry.Path)
				_ = repo.UpdateEntryFields(ctx, entry.Path, map[string]interface{}{"local_dirty": false})
			} else {
				log.Warn("Writeback: failed to open staging file %s for %s: %v", entry.LocalStagingPath, entry.Path, err)
			}
			continue
		}

		// Upload the file to S3
		uploadCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		err = client.PutObjectFromReader(uploadCtx, entry.Path, file, entry.Size, "application/octet-stream")
		file.Close()
		cancel()

		if err != nil {
			log.Error("Writeback: failed to upload %s: %v", entry.Path, err)
			continue
		}

		log.Info("Writeback: success for %s. File retained in local cache.", entry.Path)

		// Upload succeeded. Mark as clean and update S3 verification.
		// DO NOT clear LocalStagingPath — keep it for fast local reads!
		_ = repo.UpdateEntryFields(ctx, entry.Path, map[string]interface{}{
			"local_dirty":    false,
			"s3_verified_at": time.Now(),
		})
	}

	return nil
}

// processCacheEviction removes staging files that are no longer dirty and
// haven't been accessed for the configured retention time.
func processCacheEviction(ctx context.Context, repo Repository, retentionTime time.Duration) error {
	sqliteRepo, ok := repo.(*SQLiteRepository)
	if !ok {
		return nil
	}

	var cachedEntries []FileEntry
	if err := sqliteRepo.GetDB().WithContext(ctx).
		Where("local_dirty = ? AND local_staging_path != ?", false, "").
		Find(&cachedEntries).Error; err != nil {
		return fmt.Errorf("failed to list cached entries: %w", err)
	}

	now := time.Now()
	for _, entry := range cachedEntries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		stat, err := os.Stat(entry.LocalStagingPath)
		if err != nil {
			if os.IsNotExist(err) {
				// File already gone
				_ = repo.UpdateEntryFields(ctx, entry.Path, map[string]interface{}{"local_staging_path": ""})
			}
			continue
		}

		// Use Linux OS access time (Atime)
		atime := stat.ModTime()
		if stat_t, ok := stat.Sys().(*syscall.Stat_t); ok {
			atime = time.Unix(stat_t.Atim.Sec, stat_t.Atim.Nsec)
		}

		if now.Sub(atime) > retentionTime {
			log.Info("Cache eviction: removing idle local cache for %s", entry.Path)
			if err := os.Remove(entry.LocalStagingPath); err == nil || os.IsNotExist(err) {
				_ = repo.UpdateEntryFields(ctx, entry.Path, map[string]interface{}{"local_staging_path": ""})
			}
		}
	}

	return nil
}
