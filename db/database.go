package db

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Database wraps GORM database connection with helper methods
type Database struct {
	*gorm.DB
}

// NewDatabase creates a new database connection and runs migrations
func NewDatabase(dbPath string, debug bool) (*Database, error) {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	// Configure logger
	logLevel := logger.Silent
	if debug {
		logLevel = logger.Info
	}

	// Open SQLite database with WAL mode for better concurrency
	db, err := gorm.Open(sqlite.Open(dbPath+"?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Run migrations
	if err := db.AutoMigrate(
		&FileMetadata{},
		&ExtendedAttribute{},
		&ChunkInfo{},
		&PendingUpload{},
		&DirectoryListing{},
	); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	// Create composite indexes
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	// Create unique index for xattrs
	_, err = sqlDB.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_xattr_unique 
		ON extended_attributes(file_metadata_id, namespace, name)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create xattr index: %w", err)
	}

	// Create unique index for chunks
	_, err = sqlDB.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_chunk_unique 
		ON chunk_info(file_metadata_id, chunk_index)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create chunk index: %w", err)
	}

	return &Database{db}, nil
}

// Close closes the database connection
func (d *Database) Close() error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ============================================================================
// FileMetadata Operations
// ============================================================================

// GetFileByInode retrieves file metadata by inode number
func (d *Database) GetFileByInode(inode uint64) (*FileMetadata, error) {
	var file FileMetadata
	if err := d.Where("inode = ?", inode).First(&file).Error; err != nil {
		return nil, err
	}
	return &file, nil
}

// GetFileByS3Key retrieves file metadata by S3 key
func (d *Database) GetFileByS3Key(s3Key string) (*FileMetadata, error) {
	var file FileMetadata
	if err := d.Where("s3_key = ?", s3Key).First(&file).Error; err != nil {
		return nil, err
	}
	return &file, nil
}

// GetFileByPath retrieves file metadata by parent inode and name
func (d *Database) GetFileByPath(parentInode uint64, name string) (*FileMetadata, error) {
	var file FileMetadata
	if err := d.Where("parent_inode = ? AND name = ?", parentInode, name).First(&file).Error; err != nil {
		return nil, err
	}
	return &file, nil
}

// ListDirectory returns all children of a directory
func (d *Database) ListDirectory(parentInode uint64) ([]FileMetadata, error) {
	var files []FileMetadata
	if err := d.Where("parent_inode = ?", parentInode).Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

// CreateFile creates a new file metadata entry
func (d *Database) CreateFile(file *FileMetadata) error {
	return d.Create(file).Error
}

// UpdateFile updates file metadata
func (d *Database) UpdateFile(file *FileMetadata) error {
	return d.Save(file).Error
}

// DeleteFile deletes file metadata and related data
func (d *Database) DeleteFile(inode uint64) error {
	return d.Transaction(func(tx *gorm.DB) error {
		// Get file ID first
		var file FileMetadata
		if err := tx.Where("inode = ?", inode).First(&file).Error; err != nil {
			return err
		}

		// Delete related xattrs
		if err := tx.Where("file_metadata_id = ?", file.ID).Delete(&ExtendedAttribute{}).Error; err != nil {
			return err
		}

		// Delete related chunks
		if err := tx.Where("file_metadata_id = ?", file.ID).Delete(&ChunkInfo{}).Error; err != nil {
			return err
		}

		// Delete pending uploads
		if err := tx.Where("file_metadata_id = ?", file.ID).Delete(&PendingUpload{}).Error; err != nil {
			return err
		}

		// Delete file metadata
		return tx.Delete(&file).Error
	})
}

// GetNextInode returns the next available inode number
func (d *Database) GetNextInode() (uint64, error) {
	var maxInode uint64
	if err := d.Model(&FileMetadata{}).Select("COALESCE(MAX(inode), 1)").Scan(&maxInode).Error; err != nil {
		return 0, err
	}
	return maxInode + 1, nil
}

// GetDirtyFiles returns files with local changes that need syncing
func (d *Database) GetDirtyFiles() ([]FileMetadata, error) {
	var files []FileMetadata
	if err := d.Where("is_dirty = ?", true).Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

// ============================================================================
// Extended Attributes Operations
// ============================================================================

// GetXattr retrieves an extended attribute
func (d *Database) GetXattr(fileID uint, namespace, name string) (*ExtendedAttribute, error) {
	var xattr ExtendedAttribute
	if err := d.Where("file_metadata_id = ? AND namespace = ? AND name = ?", fileID, namespace, name).
		First(&xattr).Error; err != nil {
		return nil, err
	}
	return &xattr, nil
}

// SetXattr sets an extended attribute (upsert)
func (d *Database) SetXattr(fileID uint, namespace, name string, value []byte) error {
	xattr := ExtendedAttribute{
		FileMetadataID: fileID,
		Namespace:      namespace,
		Name:           name,
		Value:          value,
	}
	return d.Where("file_metadata_id = ? AND namespace = ? AND name = ?", fileID, namespace, name).
		Assign(xattr).
		FirstOrCreate(&xattr).Error
}

// RemoveXattr removes an extended attribute
func (d *Database) RemoveXattr(fileID uint, namespace, name string) error {
	return d.Where("file_metadata_id = ? AND namespace = ? AND name = ?", fileID, namespace, name).
		Delete(&ExtendedAttribute{}).Error
}

// ListXattrs lists all extended attributes for a file
func (d *Database) ListXattrs(fileID uint) ([]ExtendedAttribute, error) {
	var xattrs []ExtendedAttribute
	if err := d.Where("file_metadata_id = ?", fileID).Find(&xattrs).Error; err != nil {
		return nil, err
	}
	return xattrs, nil
}

// ============================================================================
// Chunk Operations
// ============================================================================

// GetChunk retrieves chunk info
func (d *Database) GetChunk(fileID uint, chunkIndex int64) (*ChunkInfo, error) {
	var chunk ChunkInfo
	if err := d.Where("file_metadata_id = ? AND chunk_index = ?", fileID, chunkIndex).
		First(&chunk).Error; err != nil {
		return nil, err
	}
	return &chunk, nil
}

// CreateOrUpdateChunk creates or updates chunk info
func (d *Database) CreateOrUpdateChunk(chunk *ChunkInfo) error {
	return d.Where("file_metadata_id = ? AND chunk_index = ?", chunk.FileMetadataID, chunk.ChunkIndex).
		Assign(*chunk).
		FirstOrCreate(chunk).Error
}

// GetFileChunks returns all chunks for a file
func (d *Database) GetFileChunks(fileID uint) ([]ChunkInfo, error) {
	var chunks []ChunkInfo
	if err := d.Where("file_metadata_id = ?", fileID).Order("chunk_index").Find(&chunks).Error; err != nil {
		return nil, err
	}
	return chunks, nil
}

// UpdateChunkAccessTime updates the last access time for LRU cache
func (d *Database) UpdateChunkAccessTime(chunkID uint) error {
	return d.Model(&ChunkInfo{}).Where("id = ?", chunkID).
		Update("last_access_time", time.Now()).Error
}

// GetLRUChunks returns chunks ordered by last access time for eviction
func (d *Database) GetLRUChunks(limit int) ([]ChunkInfo, error) {
	var chunks []ChunkInfo
	if err := d.Where("is_downloaded = ?", true).
		Order("last_access_time ASC").
		Limit(limit).
		Find(&chunks).Error; err != nil {
		return nil, err
	}
	return chunks, nil
}

// DeleteChunk deletes a chunk record
func (d *Database) DeleteChunk(chunkID uint) error {
	return d.Delete(&ChunkInfo{}, chunkID).Error
}

// ============================================================================
// Pending Upload Operations
// ============================================================================

// CreatePendingUpload creates a pending upload record
func (d *Database) CreatePendingUpload(upload *PendingUpload) error {
	return d.Create(upload).Error
}

// GetPendingUploads returns all pending uploads
func (d *Database) GetPendingUploads() ([]PendingUpload, error) {
	var uploads []PendingUpload
	if err := d.Where("status IN ?", []UploadStatus{UploadStatusPending, UploadStatusFailed}).
		Order("scheduled_time").
		Find(&uploads).Error; err != nil {
		return nil, err
	}
	return uploads, nil
}

// UpdatePendingUpload updates a pending upload record
func (d *Database) UpdatePendingUpload(upload *PendingUpload) error {
	return d.Save(upload).Error
}

// DeletePendingUpload deletes a pending upload record
func (d *Database) DeletePendingUpload(uploadID uint) error {
	return d.Delete(&PendingUpload{}, uploadID).Error
}

// ============================================================================
// Directory Listing Operations
// ============================================================================

// GetDirectoryListing retrieves cached directory listing info
func (d *Database) GetDirectoryListing(dirInode uint64) (*DirectoryListing, error) {
	var listing DirectoryListing
	if err := d.Where("directory_inode = ?", dirInode).First(&listing).Error; err != nil {
		return nil, err
	}
	return &listing, nil
}

// CreateOrUpdateDirectoryListing creates or updates directory listing cache
func (d *Database) CreateOrUpdateDirectoryListing(listing *DirectoryListing) error {
	return d.Where("directory_inode = ?", listing.DirectoryInode).
		Assign(*listing).
		FirstOrCreate(listing).Error
}

// ============================================================================
// Initialization Helpers
// ============================================================================

// InitializeRoot creates the root directory entry if it doesn't exist
func (d *Database) InitializeRoot() error {
	var count int64
	if err := d.Model(&FileMetadata{}).Where("inode = ?", 1).Count(&count).Error; err != nil {
		return err
	}

	if count == 0 {
		now := time.Now()
		root := &FileMetadata{
			Inode:       1,
			S3Key:       "",
			ParentInode: 0,
			Name:        "",
			Type:        FileTypeDirectory,
			Size:        0,
			Mode:        0755,
			UID:         0,
			GID:         0,
			AccessTime:  now,
			ModifyTime:  now,
			ChangeTime:  now,
			LinkCount:   2,
			IsSynced:    true,
		}
		return d.Create(root).Error
	}
	return nil
}
