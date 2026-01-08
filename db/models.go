package db

import (
	"time"

	"gorm.io/gorm"
)

// FileType represents the type of filesystem entry
type FileType uint8

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
)

// FileMetadata stores file/directory metadata cached from S3
// This avoids hitting S3 for directory listings
type FileMetadata struct {
	gorm.Model
	
	// Inode number (unique identifier in FUSE)
	Inode uint64 `gorm:"uniqueIndex;not null"`
	
	// Full S3 key path
	S3Key string `gorm:"index;not null"`
	
	// Parent directory inode (0 for root)
	ParentInode uint64 `gorm:"index;not null"`
	
	// File name (basename)
	Name string `gorm:"index;not null"`
	
	// File type (regular, directory, symlink)
	Type FileType `gorm:"not null"`
	
	// File size in bytes
	Size int64 `gorm:"not null;default:0"`
	
	// S3 ETag for cache validation
	ETag string `gorm:""`
	
	// S3 version ID (for versioned buckets)
	VersionID string `gorm:""`
	
	// POSIX-like permissions (mode) - 420 = 0644 octal
	Mode uint32 `gorm:"not null;default:420"`
	
	// Owner UID
	UID uint32 `gorm:"not null;default:0"`
	
	// Owner GID
	GID uint32 `gorm:"not null;default:0"`
	
	// Timestamps
	AccessTime time.Time `gorm:"not null"`
	ModifyTime time.Time `gorm:"not null"`
	ChangeTime time.Time `gorm:"not null"`
	
	// Link count (for hardlinks)
	LinkCount uint32 `gorm:"not null;default:1"`
	
	// Symlink target (if Type == FileTypeSymlink)
	SymlinkTarget string `gorm:""`
	
	// Content type (MIME type from S3)
	ContentType string `gorm:""`
	
	// Is this metadata synced with S3?
	IsSynced bool `gorm:"not null;default:false"`
	
	// Is file dirty (local changes not uploaded)?
	IsDirty bool `gorm:"not null;default:false"`
	
	// Last sync time with S3
	LastSyncTime time.Time `gorm:""`
}

// TableName specifies the table name for GORM
func (FileMetadata) TableName() string {
	return "file_metadata"
}

// ExtendedAttribute stores Windows ACLs and other xattrs
// These are preserved for Samba compatibility
type ExtendedAttribute struct {
	gorm.Model
	
	// Reference to FileMetadata
	FileMetadataID uint `gorm:"index;not null"`
	
	// Attribute namespace (user, system, security, trusted)
	Namespace string `gorm:"not null"`
	
	// Attribute name
	Name string `gorm:"not null"`
	
	// Attribute value (binary data)
	Value []byte `gorm:"type:blob"`
	
	// Combined unique index on file + namespace + name
	// GORM will create: CREATE UNIQUE INDEX idx_xattr ON extended_attributes(file_metadata_id, namespace, name)
}

// TableName specifies the table name for GORM
func (ExtendedAttribute) TableName() string {
	return "extended_attributes"
}

// ChunkInfo tracks downloaded chunks for lazy loading
type ChunkInfo struct {
	gorm.Model
	
	// Reference to FileMetadata
	FileMetadataID uint `gorm:"index;not null"`
	
	// Chunk index (0-based)
	ChunkIndex int64 `gorm:"not null"`
	
	// Chunk offset in file
	Offset int64 `gorm:"not null"`
	
	// Chunk size (may be less than ChunkSize for last chunk)
	Size int64 `gorm:"not null"`
	
	// Local cache path for this chunk
	CachePath string `gorm:"not null"`
	
	// SHA256 hash of chunk data for integrity
	Hash string `gorm:""`
	
	// Is chunk downloaded?
	IsDownloaded bool `gorm:"not null;default:false"`
	
	// Last access time (for LRU cache eviction)
	LastAccessTime time.Time `gorm:"not null"`
}

// TableName specifies the table name for GORM
func (ChunkInfo) TableName() string {
	return "chunk_info"
}

// PendingUpload tracks files that need to be uploaded to S3
// Used for the "upload-on-close" strategy
type PendingUpload struct {
	gorm.Model
	
	// Reference to FileMetadata
	FileMetadataID uint `gorm:"index;not null"`
	
	// S3 key to upload to
	S3Key string `gorm:"not null"`
	
	// Local temporary file path
	LocalPath string `gorm:"not null"`
	
	// Upload status
	Status UploadStatus `gorm:"not null;default:0"`
	
	// Retry count
	RetryCount int `gorm:"not null;default:0"`
	
	// Last error message
	LastError string `gorm:""`
	
	// Scheduled upload time
	ScheduledTime time.Time `gorm:""`
}

// UploadStatus represents the status of a pending upload
type UploadStatus uint8

const (
	UploadStatusPending UploadStatus = iota
	UploadStatusInProgress
	UploadStatusCompleted
	UploadStatusFailed
)

// TableName specifies the table name for GORM
func (PendingUpload) TableName() string {
	return "pending_uploads"
}

// DirectoryListing caches S3 directory listings
type DirectoryListing struct {
	gorm.Model
	
	// Directory inode
	DirectoryInode uint64 `gorm:"uniqueIndex;not null"`
	
	// S3 prefix for this directory
	S3Prefix string `gorm:"not null"`
	
	// Is listing complete?
	IsComplete bool `gorm:"not null;default:false"`
	
	// Last refresh time
	LastRefreshTime time.Time `gorm:"not null"`
	
	// Continuation token for incremental listing
	ContinuationToken string `gorm:""`
}

// TableName specifies the table name for GORM
func (DirectoryListing) TableName() string {
	return "directory_listings"
}
