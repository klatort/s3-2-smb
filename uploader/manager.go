package uploader

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/s3smb-gateway/config"
	"github.com/s3smb-gateway/db"
	"github.com/s3smb-gateway/s3client"
)

// Manager handles the "upload-on-close" strategy
// Writes are buffered locally and uploaded to S3 when the file is closed
type Manager struct {
	db        *db.Database
	s3        *s3client.Client
	cacheDir  string
	
	mu        sync.RWMutex
	handles   map[uint64]*WriteHandle // Active write handles by inode
	
	// Background upload worker
	uploadCh  chan *db.PendingUpload
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// WriteHandle represents an open file for writing
type WriteHandle struct {
	mu         sync.Mutex
	file       *os.File      // Local temp file for writes
	fileMeta   *db.FileMetadata
	localPath  string
	isDirty    bool
	size       int64
}

// NewManager creates a new upload manager
func NewManager(database *db.Database, s3Client *s3client.Client, cfg *config.Config) (*Manager, error) {
	// Create temp directory for write buffers
	tempDir := filepath.Join(cfg.CacheDir, "writes")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create write buffer directory: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	m := &Manager{
		db:       database,
		s3:       s3Client,
		cacheDir: tempDir,
		handles:  make(map[uint64]*WriteHandle),
		uploadCh: make(chan *db.PendingUpload, 100),
		ctx:      ctx,
		cancel:   cancel,
	}

	// Start background upload worker
	m.wg.Add(1)
	go m.uploadWorker()

	// Process any pending uploads from previous run
	go m.processPendingUploads()

	return m, nil
}

// OpenForWrite opens or creates a file for writing
func (m *Manager) OpenForWrite(fileMeta *db.FileMetadata) (*WriteHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if already open
	if handle, ok := m.handles[fileMeta.Inode]; ok {
		return handle, nil
	}

	// Create local temp file
	localPath := filepath.Join(m.cacheDir, fmt.Sprintf("write_%d.tmp", fileMeta.Inode))
	
	file, err := os.OpenFile(localPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to create write buffer: %w", err)
	}

	// Get current size
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	handle := &WriteHandle{
		file:      file,
		fileMeta:  fileMeta,
		localPath: localPath,
		isDirty:   false,
		size:      stat.Size(),
	}

	m.handles[fileMeta.Inode] = handle
	return handle, nil
}

// Write writes data at the specified offset
func (h *WriteHandle) Write(data []byte, offset int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Seek to offset
	if _, err := h.file.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}

	n, err := h.file.Write(data)
	if err != nil {
		return n, err
	}

	h.isDirty = true

	// Update size if we wrote past the end
	newEnd := offset + int64(n)
	if newEnd > h.size {
		h.size = newEnd
	}

	return n, nil
}

// Read reads data from the local buffer at the specified offset
func (h *WriteHandle) Read(data []byte, offset int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, err := h.file.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}

	return h.file.Read(data)
}

// Truncate truncates the file to the specified size
func (h *WriteHandle) Truncate(size int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.file.Truncate(size); err != nil {
		return err
	}

	h.size = size
	h.isDirty = true
	return nil
}

// Size returns the current size of the write buffer
func (h *WriteHandle) Size() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.size
}

// IsDirty returns whether the file has been modified
func (h *WriteHandle) IsDirty() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.isDirty
}

// Sync flushes any buffered data to disk
func (h *WriteHandle) Sync() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.file.Sync()
}

// Close closes the write handle and schedules upload
func (m *Manager) Close(inode uint64) error {
	m.mu.Lock()
	handle, ok := m.handles[inode]
	if !ok {
		m.mu.Unlock()
		return nil // Already closed
	}
	delete(m.handles, inode)
	m.mu.Unlock()

	handle.mu.Lock()
	defer handle.mu.Unlock()

	// Sync to disk
	if err := handle.file.Sync(); err != nil {
		return err
	}

	// Close the file
	if err := handle.file.Close(); err != nil {
		return err
	}

	// If dirty, schedule upload
	if handle.isDirty {
		return m.scheduleUpload(handle.fileMeta, handle.localPath)
	}

	// Clean up temp file if not dirty
	os.Remove(handle.localPath)
	return nil
}

// scheduleUpload schedules a file for upload to S3
func (m *Manager) scheduleUpload(fileMeta *db.FileMetadata, localPath string) error {
	// Create pending upload record
	upload := &db.PendingUpload{
		FileMetadataID: fileMeta.ID,
		S3Key:          fileMeta.S3Key,
		LocalPath:      localPath,
		Status:         db.UploadStatusPending,
		ScheduledTime:  time.Now(),
	}

	if err := m.db.CreatePendingUpload(upload); err != nil {
		return fmt.Errorf("failed to schedule upload: %w", err)
	}

	// Mark file as dirty
	fileMeta.IsDirty = true
	if err := m.db.UpdateFile(fileMeta); err != nil {
		return err
	}

	// Send to upload worker
	select {
	case m.uploadCh <- upload:
	default:
		// Channel full, worker will pick it up from database
	}

	return nil
}

// uploadWorker processes pending uploads in the background
func (m *Manager) uploadWorker() {
	defer m.wg.Done()

	for {
		select {
		case <-m.ctx.Done():
			return
		case upload := <-m.uploadCh:
			m.processUpload(upload)
		}
	}
}

// processUpload handles a single upload
func (m *Manager) processUpload(upload *db.PendingUpload) {
	// Update status
	upload.Status = db.UploadStatusInProgress
	m.db.UpdatePendingUpload(upload)

	// Open local file
	file, err := os.Open(upload.LocalPath)
	if err != nil {
		m.handleUploadError(upload, err)
		return
	}
	defer file.Close()

	// Get content type (default to binary)
	contentType := "application/octet-stream"

	// Upload to S3
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
	defer cancel()

	if err := m.s3.PutObject(ctx, upload.S3Key, file, contentType); err != nil {
		m.handleUploadError(upload, err)
		return
	}

	// Success - update file metadata
	fileMeta, err := m.db.GetFileByS3Key(upload.S3Key)
	if err == nil {
		// Update ETag and sync status
		info, err := m.s3.HeadObject(ctx, upload.S3Key)
		if err == nil {
			fileMeta.ETag = info.ETag
			fileMeta.Size = info.Size
			fileMeta.IsDirty = false
			fileMeta.IsSynced = true
			fileMeta.LastSyncTime = time.Now()
			m.db.UpdateFile(fileMeta)
		}
	}

	// Mark upload complete
	upload.Status = db.UploadStatusCompleted
	m.db.UpdatePendingUpload(upload)

	// Clean up local file
	os.Remove(upload.LocalPath)

	// Delete pending upload record
	m.db.DeletePendingUpload(upload.ID)
}

// handleUploadError handles upload failures
func (m *Manager) handleUploadError(upload *db.PendingUpload, err error) {
	upload.RetryCount++
	upload.LastError = err.Error()
	
	if upload.RetryCount >= 3 {
		upload.Status = db.UploadStatusFailed
	} else {
		upload.Status = db.UploadStatusPending
		// Exponential backoff
		upload.ScheduledTime = time.Now().Add(time.Duration(upload.RetryCount*upload.RetryCount) * time.Minute)
	}
	
	m.db.UpdatePendingUpload(upload)
}

// processPendingUploads processes any uploads left from previous run
func (m *Manager) processPendingUploads() {
	uploads, err := m.db.GetPendingUploads()
	if err != nil {
		return
	}

	for i := range uploads {
		upload := &uploads[i]
		// Check if local file still exists
		if _, err := os.Stat(upload.LocalPath); os.IsNotExist(err) {
			// File missing, mark as failed
			upload.Status = db.UploadStatusFailed
			upload.LastError = "Local file missing"
			m.db.UpdatePendingUpload(upload)
			continue
		}

		// Re-queue for upload
		select {
		case m.uploadCh <- upload:
		case <-m.ctx.Done():
			return
		}
	}
}

// GetWriteHandle returns an existing write handle if open
func (m *Manager) GetWriteHandle(inode uint64) (*WriteHandle, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	handle, ok := m.handles[inode]
	return handle, ok
}

// HasPendingWrites returns true if there are uncommitted writes
func (m *Manager) HasPendingWrites() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.handles) > 0
}

// FlushAll flushes all open handles
func (m *Manager) FlushAll() error {
	m.mu.Lock()
	inodes := make([]uint64, 0, len(m.handles))
	for inode := range m.handles {
		inodes = append(inodes, inode)
	}
	m.mu.Unlock()

	var lastErr error
	for _, inode := range inodes {
		if err := m.Close(inode); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Shutdown gracefully shuts down the upload manager
func (m *Manager) Shutdown() error {
	// Flush all pending writes
	if err := m.FlushAll(); err != nil {
		return err
	}

	// Cancel background worker
	m.cancel()
	m.wg.Wait()

	return nil
}

// WaitForUploads waits for all pending uploads to complete
func (m *Manager) WaitForUploads(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	
	for time.Now().Before(deadline) {
		uploads, err := m.db.GetPendingUploads()
		if err != nil {
			return err
		}
		
		if len(uploads) == 0 {
			return nil
		}
		
		time.Sleep(100 * time.Millisecond)
	}
	
	return fmt.Errorf("timeout waiting for uploads")
}
