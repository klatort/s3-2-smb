package chunks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/s3smb-gateway/config"
	"github.com/s3smb-gateway/db"
	"github.com/s3smb-gateway/s3client"
)

// Manager handles lazy loading and caching of file chunks
type Manager struct {
	db        *db.Database
	s3        *s3client.Client
	cacheDir  string
	chunkSize int64
	maxCache  int64 // Maximum cache size in bytes
	
	// In-memory tracking
	mu           sync.RWMutex
	currentSize  int64
	downloading  map[string]chan struct{} // Tracks chunks being downloaded
}

// NewManager creates a new chunk manager
func NewManager(database *db.Database, s3Client *s3client.Client, cfg *config.Config) (*Manager, error) {
	// Ensure cache directory exists
	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	chunkSize := cfg.ChunkSize
	if chunkSize == 0 {
		chunkSize = config.ChunkSize
	}

	return &Manager{
		db:          database,
		s3:          s3Client,
		cacheDir:    cfg.CacheDir,
		chunkSize:   chunkSize,
		maxCache:    cfg.MaxCacheSize,
		downloading: make(map[string]chan struct{}),
	}, nil
}

// GetChunkPath returns the local cache path for a chunk
func (m *Manager) GetChunkPath(fileID uint, chunkIndex int64) string {
	return filepath.Join(m.cacheDir, fmt.Sprintf("%d_%d.chunk", fileID, chunkIndex))
}

// CalculateChunkIndex calculates which chunk contains the given offset
func (m *Manager) CalculateChunkIndex(offset int64) int64 {
	return offset / m.chunkSize
}

// CalculateChunkOffset calculates the offset within a chunk
func (m *Manager) CalculateChunkOffset(offset int64) int64 {
	return offset % m.chunkSize
}

// GetChunkCount calculates the number of chunks for a file
func (m *Manager) GetChunkCount(fileSize int64) int64 {
	if fileSize == 0 {
		return 0
	}
	return (fileSize + m.chunkSize - 1) / m.chunkSize
}

// ReadData reads data from a file, downloading chunks as needed (lazy loading)
func (m *Manager) ReadData(ctx context.Context, file *db.FileMetadata, offset int64, size int64) ([]byte, error) {
	if offset >= file.Size {
		return nil, nil // EOF
	}

	// Adjust size if it would exceed file size
	if offset+size > file.Size {
		size = file.Size - offset
	}

	result := make([]byte, size)
	var bytesRead int64

	// Read chunk by chunk
	for bytesRead < size {
		chunkIndex := m.CalculateChunkIndex(offset + bytesRead)
		chunkOffset := m.CalculateChunkOffset(offset + bytesRead)
		
		// Calculate how much to read from this chunk
		remaining := size - bytesRead
		chunkRemaining := m.chunkSize - chunkOffset
		toRead := remaining
		if toRead > chunkRemaining {
			toRead = chunkRemaining
		}

		// Get chunk data (will download if needed)
		chunkData, err := m.GetChunk(ctx, file, chunkIndex)
		if err != nil {
			return nil, fmt.Errorf("failed to get chunk %d: %w", chunkIndex, err)
		}

		// Copy data from chunk to result
		copy(result[bytesRead:bytesRead+toRead], chunkData[chunkOffset:chunkOffset+toRead])
		bytesRead += toRead
	}

	return result, nil
}

// GetChunk retrieves a chunk, downloading from S3 if necessary
func (m *Manager) GetChunk(ctx context.Context, file *db.FileMetadata, chunkIndex int64) ([]byte, error) {
	chunkKey := fmt.Sprintf("%d_%d", file.ID, chunkIndex)
	
	// Check if we're already downloading this chunk
	m.mu.Lock()
	if ch, ok := m.downloading[chunkKey]; ok {
		m.mu.Unlock()
		// Wait for download to complete
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// Try again after download completes
		return m.GetChunk(ctx, file, chunkIndex)
	}
	
	// Check if chunk exists in database
	chunk, err := m.db.GetChunk(file.ID, chunkIndex)
	if err == nil && chunk.IsDownloaded {
		m.mu.Unlock()
		// Chunk exists, read from cache
		data, err := m.readChunkFromCache(chunk.CachePath)
		if err == nil {
			// Update access time for LRU
			m.db.UpdateChunkAccessTime(chunk.ID)
			return data, nil
		}
		// Cache file missing, need to re-download
	}
	
	// Need to download - mark as downloading
	downloadCh := make(chan struct{})
	m.downloading[chunkKey] = downloadCh
	m.mu.Unlock()
	
	defer func() {
		m.mu.Lock()
		delete(m.downloading, chunkKey)
		close(downloadCh)
		m.mu.Unlock()
	}()
	
	// Download chunk from S3
	data, err := m.downloadChunk(ctx, file, chunkIndex)
	if err != nil {
		return nil, err
	}
	
	return data, nil
}

// downloadChunk downloads a chunk from S3 and caches it
func (m *Manager) downloadChunk(ctx context.Context, file *db.FileMetadata, chunkIndex int64) ([]byte, error) {
	// Calculate byte range
	offset := chunkIndex * m.chunkSize
	length := m.chunkSize
	
	// Adjust length for last chunk
	if offset+length > file.Size {
		length = file.Size - offset
	}
	
	// Download from S3
	data, err := m.s3.GetObjectChunk(ctx, file.S3Key, offset, length)
	if err != nil {
		return nil, fmt.Errorf("failed to download chunk from S3: %w", err)
	}
	
	// Ensure cache space
	if err := m.ensureCacheSpace(int64(len(data))); err != nil {
		// Log error but continue - we have data in memory
		fmt.Printf("Warning: failed to ensure cache space: %v\n", err)
	}
	
	// Write to cache
	cachePath := m.GetChunkPath(file.ID, chunkIndex)
	if err := os.WriteFile(cachePath, data, 0644); err != nil {
		// Log error but continue - we have data in memory
		fmt.Printf("Warning: failed to write chunk to cache: %v\n", err)
	} else {
		// Calculate hash for integrity
		hash := sha256.Sum256(data)
		hashStr := hex.EncodeToString(hash[:])
		
		// Save chunk info to database
		chunkInfo := &db.ChunkInfo{
			FileMetadataID: file.ID,
			ChunkIndex:     chunkIndex,
			Offset:         offset,
			Size:           int64(len(data)),
			CachePath:      cachePath,
			Hash:           hashStr,
			IsDownloaded:   true,
			LastAccessTime: time.Now(),
		}
		
		if err := m.db.CreateOrUpdateChunk(chunkInfo); err != nil {
			fmt.Printf("Warning: failed to save chunk info: %v\n", err)
		}
		
		// Update current cache size
		m.mu.Lock()
		m.currentSize += int64(len(data))
		m.mu.Unlock()
	}
	
	return data, nil
}

// readChunkFromCache reads a chunk from the local cache
func (m *Manager) readChunkFromCache(cachePath string) ([]byte, error) {
	return os.ReadFile(cachePath)
}

// ensureCacheSpace ensures there's enough space in the cache
func (m *Manager) ensureCacheSpace(needed int64) error {
	if m.maxCache == 0 {
		return nil // Unlimited cache
	}
	
	m.mu.Lock()
	defer m.mu.Unlock()
	
	// Check if we need to evict
	if m.currentSize+needed <= m.maxCache {
		return nil
	}
	
	// Need to evict - get LRU chunks
	toEvict := m.currentSize + needed - m.maxCache
	
	chunks, err := m.db.GetLRUChunks(100) // Get up to 100 LRU chunks
	if err != nil {
		return fmt.Errorf("failed to get LRU chunks: %w", err)
	}
	
	var evicted int64
	for _, chunk := range chunks {
		if evicted >= toEvict {
			break
		}
		
		// Delete cache file
		if err := os.Remove(chunk.CachePath); err != nil && !os.IsNotExist(err) {
			continue
		}
		
		// Update database
		chunk.IsDownloaded = false
		if err := m.db.CreateOrUpdateChunk(&chunk); err != nil {
			continue
		}
		
		evicted += chunk.Size
		m.currentSize -= chunk.Size
	}
	
	return nil
}

// InvalidateChunks invalidates all chunks for a file
func (m *Manager) InvalidateChunks(ctx context.Context, fileID uint) error {
	chunks, err := m.db.GetFileChunks(fileID)
	if err != nil {
		return err
	}
	
	m.mu.Lock()
	defer m.mu.Unlock()
	
	for _, chunk := range chunks {
		// Delete cache file
		if err := os.Remove(chunk.CachePath); err != nil && !os.IsNotExist(err) {
			continue
		}
		
		// Delete from database
		if err := m.db.DeleteChunk(chunk.ID); err != nil {
			continue
		}
		
		m.currentSize -= chunk.Size
	}
	
	return nil
}

// PrefetchChunks prefetches chunks in the background
func (m *Manager) PrefetchChunks(ctx context.Context, file *db.FileMetadata, startChunk, count int64) {
	go func() {
		for i := startChunk; i < startChunk+count && i < m.GetChunkCount(file.Size); i++ {
			select {
			case <-ctx.Done():
				return
			default:
				_, _ = m.GetChunk(ctx, file, i)
			}
		}
	}()
}

// GetCacheSize returns the current cache size
func (m *Manager) GetCacheSize() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentSize
}

// GetChunkSize returns the configured chunk size
func (m *Manager) GetChunkSize() int64 {
	return m.chunkSize
}

// ClearCache clears all cached chunks
func (m *Manager) ClearCache() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	// Remove all files in cache directory
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		return err
	}
	
	for _, entry := range entries {
		if !entry.IsDir() {
			os.Remove(filepath.Join(m.cacheDir, entry.Name()))
		}
	}
	
	m.currentSize = 0
	return nil
}
