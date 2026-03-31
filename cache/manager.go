package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/s3smb-gateway/internal/log"
)

// Constants for chunk management
const (
	// ChunkSize is the size of each chunk (16MB)
	ChunkSize = 16 * 1024 * 1024

	// DefaultMaxCacheSize is the default maximum cache size (10GB)
	DefaultMaxCacheSize = 10 * 1024 * 1024 * 1024

	// ChunkFilePattern is the pattern for chunk filenames
	ChunkFilePattern = "chunk_%d"
)

// S3ChunkReader defines the interface for reading chunks from S3
type S3ChunkReader interface {
	// GetObjectChunk retrieves a specific byte range from an S3 object
	GetObjectChunk(ctx context.Context, key string, offset, length int64) ([]byte, error)
}

// ChunkManager manages cached file chunks with LRU eviction
type ChunkManager struct {
	cacheDir     string
	maxCacheSize int64
	s3Client     S3ChunkReader

	mu           sync.RWMutex
	lru          *lruTracker
	currentSize  int64
	downloading  map[string]chan struct{} // Tracks in-progress downloads
}

// NewChunkManager creates a new chunk manager
func NewChunkManager(cacheDir string, s3Client S3ChunkReader) (*ChunkManager, error) {
	return NewChunkManagerWithSize(cacheDir, s3Client, DefaultMaxCacheSize)
}

// NewChunkManagerWithSize creates a new chunk manager with custom cache size
func NewChunkManagerWithSize(cacheDir string, s3Client S3ChunkReader, maxCacheSize int64) (*ChunkManager, error) {
	filesDir := filepath.Join(cacheDir, "files")
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	cm := &ChunkManager{
		cacheDir:     cacheDir,
		maxCacheSize: maxCacheSize,
		s3Client:     s3Client,
		lru:          newLRUTracker(),
		downloading:  make(map[string]chan struct{}),
	}

	// Scan existing cache to rebuild LRU state
	if err := cm.scanExistingCache(); err != nil {
		return nil, fmt.Errorf("failed to scan existing cache: %w", err)
	}

	return cm, nil
}

// Read reads data from a file, using cached chunks or downloading from S3 as needed
func (cm *ChunkManager) Read(ctx context.Context, path string, offset int64, size int, buffer []byte) (int, error) {
	if size <= 0 || len(buffer) < size {
		return 0, fmt.Errorf("invalid buffer size")
	}

	totalRead := 0
	remaining := size
	currentOffset := offset

	for remaining > 0 {
		// Calculate which chunk this offset falls into
		chunkID := cm.offsetToChunkID(currentOffset)
		chunkStart := int64(chunkID) * ChunkSize

		// Calculate offset within the chunk
		offsetInChunk := currentOffset - chunkStart

		// Calculate how many bytes to read from this chunk
		bytesAvailableInChunk := ChunkSize - int(offsetInChunk)
		bytesToRead := remaining
		if bytesToRead > bytesAvailableInChunk {
			bytesToRead = bytesAvailableInChunk
		}

		// Get the chunk (from cache or download)
		chunkPath, err := cm.ensureChunk(ctx, path, chunkID)
		if err != nil {
			if totalRead > 0 {
				return totalRead, nil // Return partial read
			}
			return 0, err
		}

		// Read from the chunk file
		n, err := cm.readFromChunk(chunkPath, offsetInChunk, buffer[totalRead:totalRead+bytesToRead])
		totalRead += n
		remaining -= n
		currentOffset += int64(n)

		if err != nil {
			if err == io.EOF {
				return totalRead, nil // End of file reached
			}
			return totalRead, err
		}

		// If we read less than expected, we've hit EOF
		if n < bytesToRead {
			return totalRead, nil
		}
	}

	return totalRead, nil
}

// offsetToChunkID calculates the chunk ID for a given byte offset
func (cm *ChunkManager) offsetToChunkID(offset int64) int {
	return int(offset / ChunkSize)
}

// pathToHash generates a hash of the path for directory naming
func (cm *ChunkManager) pathToHash(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:16]) // Use first 16 bytes (32 hex chars)
}

// getChunkDir returns the directory for a file's chunks
func (cm *ChunkManager) getChunkDir(path string) string {
	pathHash := cm.pathToHash(path)
	return filepath.Join(cm.cacheDir, "files", pathHash)
}

// getChunkPath returns the full path to a specific chunk file
func (cm *ChunkManager) getChunkPath(path string, chunkID int) string {
	chunkDir := cm.getChunkDir(path)
	return filepath.Join(chunkDir, fmt.Sprintf(ChunkFilePattern, chunkID))
}

// chunkKey generates a unique key for a chunk (used in LRU tracking)
func (cm *ChunkManager) chunkKey(path string, chunkID int) string {
	return fmt.Sprintf("%s:%d", path, chunkID)
}

// ensureChunk ensures a chunk is available locally, downloading if necessary
func (cm *ChunkManager) ensureChunk(ctx context.Context, path string, chunkID int) (string, error) {
	chunkPath := cm.getChunkPath(path, chunkID)
	key := cm.chunkKey(path, chunkID)

	// Check if chunk exists locally
	if cm.chunkExists(chunkPath) {
		// Update LRU access time
		cm.mu.Lock()
		cm.lru.touch(key, chunkPath)
		cm.mu.Unlock()
		return chunkPath, nil
	}

	// Check if another goroutine is already downloading this chunk
	cm.mu.Lock()
	if waitCh, exists := cm.downloading[key]; exists {
		cm.mu.Unlock()
		// Wait for the other download to complete
		select {
		case <-waitCh:
			if cm.chunkExists(chunkPath) {
				return chunkPath, nil
			}
			return "", fmt.Errorf("chunk download failed")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	// Mark this chunk as being downloaded
	waitCh := make(chan struct{})
	cm.downloading[key] = waitCh
	cm.mu.Unlock()

	// Download the chunk
	err := cm.downloadChunk(ctx, path, chunkID, chunkPath)

	// Signal completion
	cm.mu.Lock()
	delete(cm.downloading, key)
	close(waitCh)
	cm.mu.Unlock()

	if err != nil {
		return "", err
	}

	return chunkPath, nil
}

// chunkExists checks if a chunk file exists
func (cm *ChunkManager) chunkExists(chunkPath string) bool {
	_, err := os.Stat(chunkPath)
	return err == nil
}

// downloadChunk downloads a specific chunk from S3
func (cm *ChunkManager) downloadChunk(ctx context.Context, path string, chunkID int, chunkPath string) error {
	// Ensure cache has space (evict if necessary)
	cm.ensureCacheSpace(ChunkSize)

	// Create the chunk directory
	chunkDir := filepath.Dir(chunkPath)
	if err := os.MkdirAll(chunkDir, 0755); err != nil {
		return fmt.Errorf("failed to create chunk directory: %w", err)
	}

	// Calculate the byte range for this chunk
	startOffset := int64(chunkID) * ChunkSize

	// Download the chunk from S3 using Range header
	data, err := cm.s3Client.GetObjectChunk(ctx, path, startOffset, ChunkSize)
	if err != nil {
		return fmt.Errorf("failed to download chunk from S3: %w", err)
	}

	// Write to a temporary file first
	tempPath := chunkPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write chunk to disk: %w", err)
	}

	// Atomically rename to final path
	if err := os.Rename(tempPath, chunkPath); err != nil {
		os.Remove(tempPath) // Clean up temp file
		return fmt.Errorf("failed to finalize chunk file: %w", err)
	}

	// Update cache size and LRU
	cm.mu.Lock()
	cm.currentSize += int64(len(data))
	cm.lru.add(cm.chunkKey(path, chunkID), chunkPath, int64(len(data)))
	cm.mu.Unlock()

	return nil
}

// readFromChunk reads data from a local chunk file
func (cm *ChunkManager) readFromChunk(chunkPath string, offset int64, buffer []byte) (int, error) {
	f, err := os.Open(chunkPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open chunk file: %w", err)
	}
	defer f.Close()

	// Seek to the offset within the chunk
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("failed to seek in chunk file: %w", err)
	}

	// Read the requested bytes
	n, err := f.Read(buffer)
	return n, err
}

// ensureCacheSpace ensures there's enough space in the cache, evicting old chunks if necessary
func (cm *ChunkManager) ensureCacheSpace(neededBytes int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Keep evicting until we have enough space
	for cm.currentSize+neededBytes > cm.maxCacheSize {
		// Get the least recently used chunk
		entry := cm.lru.evictOldest()
		if entry == nil {
			break // No more entries to evict
		}

		// Delete the chunk file
		if err := os.Remove(entry.path); err != nil {
			if !os.IsNotExist(err) {
				log.Warn("failed to remove cached chunk %s: %v\n", entry.path, err)
			}
		}

		cm.currentSize -= entry.size

		// Try to remove empty parent directories
		cm.cleanupEmptyDirs(filepath.Dir(entry.path))
	}
}

// cleanupEmptyDirs removes empty directories up to the files directory
func (cm *ChunkManager) cleanupEmptyDirs(dir string) {
	filesDir := filepath.Join(cm.cacheDir, "files")

	for dir != filesDir && dir != cm.cacheDir {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		if err := os.Remove(dir); err != nil {
			break
		}
		dir = filepath.Dir(dir)
	}
}

// scanExistingCache scans the cache directory and rebuilds LRU state
func (cm *ChunkManager) scanExistingCache() error {
	filesDir := filepath.Join(cm.cacheDir, "files")

	if _, err := os.Stat(filesDir); os.IsNotExist(err) {
		return nil // No existing cache
	}

	err := filepath.Walk(filesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		if info.IsDir() {
			return nil
		}

		// Only process chunk files
		if filepath.Base(path)[:6] != "chunk_" {
			return nil
		}

		// Add to LRU with the file's modification time
		cm.lru.addWithTime(path, path, info.Size(), info.ModTime())
		cm.currentSize += info.Size()

		return nil
	})

	return err
}

// Evict explicitly evicts chunks to free up space
func (cm *ChunkManager) Evict(targetFreeBytes int64) int64 {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	var freedBytes int64

	for freedBytes < targetFreeBytes {
		entry := cm.lru.evictOldest()
		if entry == nil {
			break
		}

		if err := os.Remove(entry.path); err == nil {
			freedBytes += entry.size
			cm.currentSize -= entry.size
		}

		cm.cleanupEmptyDirs(filepath.Dir(entry.path))
	}

	return freedBytes
}

// InvalidateFile removes all cached chunks for a specific file
func (cm *ChunkManager) InvalidateFile(path string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	chunkDir := cm.getChunkDir(path)

	// Read all chunks in the directory
	entries, err := os.ReadDir(chunkDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		chunkPath := filepath.Join(chunkDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Remove from LRU
		cm.lru.remove(chunkPath)
		cm.currentSize -= info.Size()

		// Delete file
		os.Remove(chunkPath)
	}

	// Remove the directory
	os.Remove(chunkDir)

	return nil
}

// Stats returns current cache statistics
func (cm *ChunkManager) Stats() CacheStats {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	return CacheStats{
		CurrentSize:  cm.currentSize,
		MaxSize:      cm.maxCacheSize,
		ChunkCount:   cm.lru.len(),
		UtilizationPct: float64(cm.currentSize) / float64(cm.maxCacheSize) * 100,
	}
}

// CacheStats contains cache statistics
type CacheStats struct {
	CurrentSize    int64
	MaxSize        int64
	ChunkCount     int
	UtilizationPct float64
}

// Close cleans up resources
func (cm *ChunkManager) Close() error {
	return nil
}
