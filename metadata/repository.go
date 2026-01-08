package metadata

import (
	"context"
	"strings"
)

// Repository defines the interface for metadata storage operations
type Repository interface {
	// GetEntry retrieves a file entry by its full path
	GetEntry(ctx context.Context, path string) (*FileEntry, error)

	// ListEntries returns all entries in a directory (direct children only)
	ListEntries(ctx context.Context, dirPath string) ([]*FileEntry, error)

	// UpdateEntry creates or updates a file entry
	UpdateEntry(ctx context.Context, entry *FileEntry) error

	// DeleteEntry removes a file entry by path
	DeleteEntry(ctx context.Context, path string) error

	// DeleteEntriesWithPrefix removes all entries with a given path prefix (for directory deletion)
	DeleteEntriesWithPrefix(ctx context.Context, prefix string) error

	// GetXattr retrieves an extended attribute for a path
	GetXattr(ctx context.Context, path, name string) ([]byte, error)

	// SetXattr sets an extended attribute for a path
	SetXattr(ctx context.Context, path, name string, value []byte) error

	// RemoveXattr removes an extended attribute from a path
	RemoveXattr(ctx context.Context, path, name string) error

	// ListXattrs returns all extended attribute names for a path
	ListXattrs(ctx context.Context, path string) ([]string, error)

	// Close closes the repository connection
	Close() error
}

// NormalizePath ensures consistent path formatting
// - Removes leading/trailing slashes for storage
// - Empty string represents root
func NormalizePath(path string) string {
	path = strings.Trim(path, "/")
	return path
}

// ParentPath returns the parent directory path
func ParentPath(path string) string {
	path = NormalizePath(path)
	if path == "" {
		return ""
	}
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

// BaseName returns the base name of a path
func BaseName(path string) string {
	path = NormalizePath(path)
	if path == "" {
		return ""
	}
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

// JoinPath joins path components
func JoinPath(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		p = NormalizePath(p)
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "/")
}
