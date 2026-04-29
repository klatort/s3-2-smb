package metadata

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// SQLiteRepository implements Repository using SQLite/GORM
type SQLiteRepository struct {
	db *gorm.DB
	mu sync.RWMutex
}

// Ensure SQLiteRepository implements Repository
var _ Repository = (*SQLiteRepository)(nil)

// NewSQLiteRepository creates a new SQLite-backed repository
// The database file is created at {cacheDir}/metadata.db
func NewSQLiteRepository(cacheDir string, debug bool) (*SQLiteRepository, error) {
	// Ensure cache directory exists
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	dbPath := filepath.Join(cacheDir, "metadata.db")

	// Configure logger
	logLevel := logger.Silent
	if debug {
		logLevel = logger.Info
	}

	// Open SQLite database with WAL mode for better concurrency
	db, err := gorm.Open(sqlite.Open(dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON"), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Run migrations
	if err := db.AutoMigrate(&FileEntry{}); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	// Create index for parent directory lookups
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	// Index for efficient directory listing (find all entries starting with prefix)
	_, err = sqlDB.Exec(`CREATE INDEX IF NOT EXISTS idx_file_entries_path ON file_entries(path)`)
	if err != nil {
		return nil, fmt.Errorf("failed to create path index: %w", err)
	}

	return &SQLiteRepository{db: db}, nil
}

// GetEntry retrieves a file entry by its full path
func (r *SQLiteRepository) GetEntry(ctx context.Context, path string) (*FileEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	path = NormalizePath(path)

	var entry FileEntry
	if err := r.db.WithContext(ctx).Where("path = ?", path).First(&entry).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get entry: %w", err)
	}

	return &entry, nil
}

// ListEntries returns all entries in a directory (direct children only)
func (r *SQLiteRepository) ListEntries(ctx context.Context, dirPath string) ([]*FileEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	dirPath = NormalizePath(dirPath)

	var entries []*FileEntry
	var prefix string
	if dirPath == "" {
		// Root directory - find entries with no "/" in path
		prefix = ""
	} else {
		prefix = dirPath + "/"
	}

	query := r.db.WithContext(ctx)

	if prefix == "" {
		// Root: get entries that don't contain "/"
		if err := query.Where("path NOT LIKE ?", "%/%").Find(&entries).Error; err != nil {
			return nil, fmt.Errorf("failed to list entries: %w", err)
		}
	} else {
		// Subdirectory: get only direct children by excluding deeper paths.
		// The second LIKE condition filters out entries with additional "/"
		// separators, so only immediate children of the directory are returned.
		if err := query.Where("path LIKE ? AND path NOT LIKE ?", prefix+"%", prefix+"%/%").Find(&entries).Error; err != nil {
			return nil, fmt.Errorf("failed to list entries: %w", err)
		}
	}

	return entries, nil
}

// UpdateEntry creates or updates a file entry
func (r *SQLiteRepository) UpdateEntry(ctx context.Context, entry *FileEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry.Path = NormalizePath(entry.Path)

	// Upsert: update if exists, create if not
	result := r.db.WithContext(ctx).Where("path = ?", entry.Path).Assign(entry).FirstOrCreate(entry)
	if result.Error != nil {
		return fmt.Errorf("failed to update entry: %w", result.Error)
	}

	return nil
}

// UpdateEntryFields updates only the specified columns for an existing entry.
// This is used by operations like Setattr that should NOT overwrite fields
// (like Size) that may have been concurrently updated by Flush.
func (r *SQLiteRepository) UpdateEntryFields(ctx context.Context, path string, updates map[string]interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path = NormalizePath(path)

	result := r.db.WithContext(ctx).Model(&FileEntry{}).Where("path = ?", path).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("failed to update entry fields: %w", result.Error)
	}

	return nil
}

// DeleteEntry removes a file entry by path
func (r *SQLiteRepository) DeleteEntry(ctx context.Context, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path = NormalizePath(path)

	result := r.db.WithContext(ctx).Where("path = ?", path).Delete(&FileEntry{})
	if result.Error != nil {
		return fmt.Errorf("failed to delete entry: %w", result.Error)
	}

	return nil
}

// DeleteEntriesWithPrefix removes all entries with a given path prefix
func (r *SQLiteRepository) DeleteEntriesWithPrefix(ctx context.Context, prefix string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	prefix = NormalizePath(prefix)

	// Delete the entry itself and all children
	result := r.db.WithContext(ctx).Where("path = ? OR path LIKE ?", prefix, prefix+"/%").Delete(&FileEntry{})
	if result.Error != nil {
		return fmt.Errorf("failed to delete entries: %w", result.Error)
	}

	return nil
}

// GetXattr retrieves an extended attribute for a path
func (r *SQLiteRepository) GetXattr(ctx context.Context, path, name string) ([]byte, error) {
	entry, err := r.GetEntry(ctx, path)
	if err != nil {
		return nil, err
	}

	val, ok := entry.GetXattr(name)
	if !ok {
		return nil, ErrXattrNotFound
	}

	return val, nil
}

// SetXattr sets an extended attribute for a path
func (r *SQLiteRepository) SetXattr(ctx context.Context, path, name string, value []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path = NormalizePath(path)

	var entry FileEntry
	if err := r.db.WithContext(ctx).Where("path = ?", path).First(&entry).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("failed to get entry: %w", err)
	}

	entry.SetXattr(name, value)

	if err := r.db.WithContext(ctx).Save(&entry).Error; err != nil {
		return fmt.Errorf("failed to save xattr: %w", err)
	}

	return nil
}

// RemoveXattr removes an extended attribute from a path
func (r *SQLiteRepository) RemoveXattr(ctx context.Context, path, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path = NormalizePath(path)

	var entry FileEntry
	if err := r.db.WithContext(ctx).Where("path = ?", path).First(&entry).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("failed to get entry: %w", err)
	}

	entry.RemoveXattr(name)

	if err := r.db.WithContext(ctx).Save(&entry).Error; err != nil {
		return fmt.Errorf("failed to save entry: %w", err)
	}

	return nil
}

// ListXattrs returns all extended attribute names for a path
func (r *SQLiteRepository) ListXattrs(ctx context.Context, path string) ([]string, error) {
	entry, err := r.GetEntry(ctx, path)
	if err != nil {
		return nil, err
	}

	return entry.ListXattrNames(), nil
}

// Close closes the database connection
func (r *SQLiteRepository) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// GetDB returns the underlying GORM database (for advanced queries)
func (r *SQLiteRepository) GetDB() *gorm.DB {
	return r.db
}

// EntryCount returns the total number of entries in the database
func (r *SQLiteRepository) EntryCount(ctx context.Context) (int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var count int64
	if err := r.db.WithContext(ctx).Model(&FileEntry{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// Clear removes all entries from the database
func (r *SQLiteRepository) Clear(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.db.WithContext(ctx).Where("1 = 1").Delete(&FileEntry{}).Error; err != nil {
		return fmt.Errorf("failed to clear entries: %w", err)
	}
	return nil
}
