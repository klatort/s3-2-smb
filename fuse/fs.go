package fuse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"

	"github.com/s3smb-gateway/cache"
	"github.com/s3smb-gateway/config"
	"github.com/s3smb-gateway/internal/log"
	"github.com/s3smb-gateway/metadata"
	"github.com/s3smb-gateway/s3client"
)

// Common xattr names used by Samba for Windows ACLs
const (
	// XattrSecurityNTACL is the xattr name used by Samba for storing Windows ACLs
	XattrSecurityNTACL = "security.NTACL"

	// XattrUserPrefix is the prefix for user-defined extended attributes
	XattrUserPrefix = "user."

	// XattrSystemPrefix is the prefix for system extended attributes
	XattrSystemPrefix = "system."
)

// FS implements the FUSE filesystem
type FS struct {
	repo  metadata.Repository // SQLite metadata repository
	s3    *s3client.Client
	cache *cache.ChunkManager // Cache manager for file chunks
	cfg   *config.Config

	mu   sync.RWMutex
	conn *fuse.Conn

	// Inode generation (for FUSE inode numbers)
	nextInode uint64

	// Staging directory for writes
	stagingDir string
}

// NewFS creates a new FUSE filesystem
// repo: SQLite metadata repository for cached file metadata
// s3Client: S3 client for actual data operations
// cfg: configuration
func NewFS(repo metadata.Repository, s3Client *s3client.Client, cfg *config.Config) (*FS, error) {
	// Enable debug logging if configured
	if cfg.Debug {
		log.EnableDebug()
	}

	// Create staging directory
	stagingDir := filepath.Join(cfg.CacheDir, "staging")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create staging directory %s: %w", stagingDir, err)
	}

	// Create cache manager
	var cacheMgr *cache.ChunkManager
	if cfg.ChunkSize > 0 {
		var err error
		cacheMgr, err = cache.NewChunkManagerWithSize(cfg.CacheDir, s3Client, cfg.MaxCacheSize)
		if err != nil {
			log.Warn("Failed to initialize cache manager: %v", err)
			// Don't fail entirely if cache manager fails, just log warning
			// Filesystem can still work without cache
		} else {
			log.Info("Cache manager initialized with chunk size: %d MB, max cache: %d MB",
				cfg.ChunkSize/(1024*1024), cfg.MaxCacheSize/(1024*1024))
		}
	}

	return &FS{
		repo:       repo,
		s3:         s3Client,
		cache:      cacheMgr,
		cfg:        cfg,
		nextInode:  2, // 1 is reserved for root
		stagingDir: stagingDir,
	}, nil
}

// GenerateInode generates a unique inode number
func (f *FS) GenerateInode() uint64 {
	return atomic.AddUint64(&f.nextInode, 1)
}

// PathToInode generates a stable inode from a path using FNV-1a hash
func PathToInode(path string) uint64 {
	if path == "" {
		return 1 // Root inode
	}
	// FNV-1a hash for stable inode numbers
	var hash uint64 = 14695981039346656037 // FNV offset basis
	for i := 0; i < len(path); i++ {
		hash ^= uint64(path[i])
		hash *= 1099511628211 // FNV prime
	}
	// Ensure non-zero and not 1 (reserved for root)
	if hash == 0 || hash == 1 {
		hash = 2
	}
	return hash
}

// pathToStagingFile generates the staging file path for a given S3 path
func (f *FS) pathToStagingFile(path string) string {
	h := sha256.Sum256([]byte(path))
	hash := hex.EncodeToString(h[:16])
	return filepath.Join(f.stagingDir, hash)
}

// Root returns the root directory node
func (f *FS) Root() (fs.Node, error) {
	return &Dir{
		fs:   f,
		path: "", // Empty path = root
	}, nil
}

// Statfs returns filesystem statistics
func (f *FS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {
	// Return reasonable defaults for S3 (essentially unlimited)
	resp.Blocks = 1 << 40 // Large number
	resp.Bfree = 1 << 39
	resp.Bavail = 1 << 39
	resp.Files = 1 << 30
	resp.Ffree = 1 << 29
	resp.Bsize = 4096
	resp.Namelen = 1024
	resp.Frsize = 4096
	return nil
}

// Mount mounts the filesystem
func (f *FS) Mount() error {
	// Ensure mount point exists
	if err := os.MkdirAll(f.cfg.MountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Mount options
	opts := []fuse.MountOption{
		fuse.FSName("s3smb-gateway"),
		fuse.Subtype("s3"),
		fuse.AllowOther(),
		fuse.DefaultPermissions(),
		fuse.MaxReadahead(1 << 20), // 1MB readahead for better sequential I/O
		fuse.AsyncRead(),            // Allow concurrent reads on the same handle
	}

	conn, err := fuse.Mount(f.cfg.MountPoint, opts...)
	if err != nil {
		return fmt.Errorf("failed to mount: %w", err)
	}

	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()

	return nil
}

// Serve starts serving FUSE requests
func (f *FS) Serve() error {
	f.mu.RLock()
	conn := f.conn
	f.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("filesystem not mounted")
	}

	return fs.Serve(conn, f)
}

// Unmount unmounts the filesystem
func (f *FS) Unmount() error {
	return fuse.Unmount(f.cfg.MountPoint)
}

// ============================================================================
// Directory Node
// ============================================================================

// Dir represents a directory in the filesystem
type Dir struct {
	fs   *FS
	path string // Full path (empty string = root)
}

var _ fs.Node = (*Dir)(nil)
var _ fs.NodeStringLookuper = (*Dir)(nil)
var _ fs.HandleReadDirAller = (*Dir)(nil)
var _ fs.NodeMkdirer = (*Dir)(nil)
var _ fs.NodeCreater = (*Dir)(nil)
var _ fs.NodeRemover = (*Dir)(nil)
var _ fs.NodeRenamer = (*Dir)(nil)
var _ fs.NodeGetxattrer = (*Dir)(nil)
var _ fs.NodeSetxattrer = (*Dir)(nil)
var _ fs.NodeListxattrer = (*Dir)(nil)
var _ fs.NodeRemovexattrer = (*Dir)(nil)
var _ fs.NodeSetattrer = (*Dir)(nil)

// Attr returns the directory attributes from the SQLite database (NOT from S3)
func (d *Dir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = PathToInode(d.path)
	a.Mode = os.ModeDir | 0777
	a.Nlink = 2
	a.Valid = 5 * time.Second // Attribute cache TTL — reduces kernel stat() calls

	// For root directory, use defaults
	if d.path == "" {
		a.Size = 4096
		a.Atime = time.Now()
		a.Mtime = time.Now()
		a.Ctime = time.Now()
		a.Blocks = (a.Size + 511) / 512
		return nil
	}

	// Query the SQLite database for directory metadata
	entry, err := d.fs.repo.GetEntry(ctx, d.path)
	if err != nil {
		if err == metadata.ErrNotFound {
			// Directory exists in FUSE tree but not in DB yet
			a.Size = 4096
			a.Atime = time.Now()
			a.Mtime = time.Now()
			a.Ctime = time.Now()
			return nil
		}
		return err
	}

	// Return attributes from DATABASE (crucial: NOT from S3)
	a.Size = uint64(entry.Size)
	if a.Size == 0 {
		a.Size = 4096 // Default directory size
	}
	a.Atime = entry.ModTime
	a.Mtime = entry.ModTime
	a.Ctime = entry.ModTime

	// If POSIX ownership/mode stored in xattrs, expose them via Attr
	if uid, gid, ok := entry.GetPosixOwner(); ok {
		a.Uid = uid
		a.Gid = gid
	}
	if mode, ok := entry.GetPosixMode(); ok {
		a.Mode = os.ModeDir | mode
	}

	a.Blocks = (a.Size + 511) / 512
	return nil
}

// Lookup looks up a child entry by name in the SQLite database
func (d *Dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	// Build the full path for the child
	childPath := metadata.JoinPath(d.path, name)

	// Query the SQLite database (NOT S3)
	entry, err := d.fs.repo.GetEntry(ctx, childPath)
	if err != nil {
		if err == metadata.ErrNotFound {
			return nil, syscall.ENOENT
		}
		return nil, syscall.EIO
	}

	// Return appropriate node type based on DB entry
	if entry.IsDir {
		return &Dir{fs: d.fs, path: childPath}, nil
	}
	return &File{fs: d.fs, path: childPath, entry: entry}, nil
}

// ReadDirAll returns all directory entries from the SQLite database
func (d *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	// Query children from SQLite database (NOT S3)
	children, err := d.fs.repo.ListEntries(ctx, d.path)
	if err != nil {
		return nil, err
	}

	// Pre-allocate with space for . and ..
	entries := make([]fuse.Dirent, 0, len(children)+2)

	// Add . (current directory)
	entries = append(entries, fuse.Dirent{
		Inode: PathToInode(d.path),
		Type:  fuse.DT_Dir,
		Name:  ".",
	})

	// Add .. (parent directory)
	parentPath := metadata.ParentPath(d.path)
	entries = append(entries, fuse.Dirent{
		Inode: PathToInode(parentPath),
		Type:  fuse.DT_Dir,
		Name:  "..",
	})

	// Add children from database
	for _, child := range children {
		if child.Path == d.path || child.Path == "" {
			continue // Skip adding the parent directory or root itself to avoid corrupting Dirent strings
		}
		entryType := fuse.DT_File
		if child.IsDir {
			entryType = fuse.DT_Dir
		}

		entries = append(entries, fuse.Dirent{
			Inode: PathToInode(child.Path),
			Type:  entryType,
			Name:  metadata.BaseName(child.Path),
		})
	}

	return entries, nil
}

// Mkdir creates a new directory
func (d *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	childPath := metadata.JoinPath(d.path, req.Name)
	now := time.Now()

	// Create entry in SQLite database
	entry := &metadata.FileEntry{
		Path:    childPath,
		Size:    0,
		ModTime: now,
		IsDir:   true,
		ETag:    "",
	}

	if err := d.fs.repo.UpdateEntry(ctx, entry); err != nil {
		return nil, err
	}

	// Persist requested mode/owner from request if provided
	if req.Mode != 0 || req.Uid != 0 || req.Gid != 0 {
		if attrEntry, err := d.fs.repo.GetEntry(ctx, childPath); err == nil {
			if req.Mode != 0 {
				attrEntry.SetPosixMode(os.FileMode(req.Mode))
			}
			attrEntry.SetPosixOwner(req.Uid, req.Gid)
			_ = d.fs.repo.UpdateEntry(context.Background(), attrEntry)
		}
	}

	// Create directory marker in S3 synchronously.
	// Using a background context with a short timeout so the FUSE request
	// context (which may have an aggressive deadline) doesn't abort the
	// S3 call.  If S3 creation fails we roll back the DB entry to keep
	// local and remote state consistent — preventing a silent divergence
	// that would only surface after a gateway restart+sync.
	s3Ctx, s3Cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer s3Cancel()
	if err := d.fs.s3.CreateDirectory(s3Ctx, childPath); err != nil {
		// Roll back the DB entry so the user doesn't see a directory that
		// doesn't exist in S3.
		_ = d.fs.repo.DeleteEntry(context.Background(), childPath)
		log.Warn("Mkdir: S3 directory creation failed for %s: %v", childPath, err)
		return nil, syscall.EIO
	}

	return &Dir{fs: d.fs, path: childPath}, nil
}


// Create creates a new file
func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	childPath := metadata.JoinPath(d.path, req.Name)
	now := time.Now()

	// Create entry in SQLite database
	entry := &metadata.FileEntry{
		Path:    childPath,
		Size:    0,
		ModTime: now,
		IsDir:   false,
		ETag:    "",
	}

	if err := d.fs.repo.UpdateEntry(ctx, entry); err != nil {
		return nil, nil, err
	}

	// Create empty staging file
	stagingPath := d.fs.pathToStagingFile(childPath)
	stagingFile, err := os.Create(stagingPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create staging file: %w", err)
	}

	file := &File{fs: d.fs, path: childPath, entry: entry}
	handle := &FileHandle{
		file:        file,
		stagingPath: stagingPath,
		stagingFile: stagingFile,
		dirty:       false,
	}

	// Persist requested mode/owner if provided
	if req.Mode != 0 || req.Uid != 0 || req.Gid != 0 {
		if attrEntry, err := d.fs.repo.GetEntry(ctx, childPath); err == nil {
			if req.Mode != 0 {
				attrEntry.SetPosixMode(os.FileMode(req.Mode))
			}
			attrEntry.SetPosixOwner(req.Uid, req.Gid)
			_ = d.fs.repo.UpdateEntry(context.Background(), attrEntry)
		}
	}

	return file, handle, nil
}

// Remove removes a file or empty directory
func (d *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	childPath := metadata.JoinPath(d.path, req.Name)

	// Check if entry exists in database
	entry, err := d.fs.repo.GetEntry(ctx, childPath)
	if err != nil {
		if err == metadata.ErrNotFound {
			return syscall.ENOENT
		}
		return err
	}

	// If directory, check if empty
	if req.Dir && entry.IsDir {
		children, err := d.fs.repo.ListEntries(ctx, childPath)
		if err != nil {
			return err
		}
		if len(children) > 0 {
			return syscall.ENOTEMPTY
		}
	}

	// Delete from S3 first (synchronous, short timeout).
	// Performing the S3 delete BEFORE the DB delete ensures that if S3
	// fails we leave the DB entry intact — the user's file remains visible
	// and they see an error rather than a silent phantom deletion that would
	// re-appear after the next sync.
	s3Ctx, s3Cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer s3Cancel()
	if err := d.fs.s3.DeleteObject(s3Ctx, childPath); err != nil {
		// Tolerate "not found" — the object may have already been deleted
		// externally, in which case the DB cleanup should still proceed.
		errStr := err.Error()
		if !strings.Contains(errStr, "NoSuchKey") &&
			!strings.Contains(errStr, "NotFound") &&
			!strings.Contains(errStr, "404") {
			log.Warn("Remove: S3 delete failed for %s: %v", childPath, err)
			return syscall.EIO
		}
	}

	// Invalidate cached chunks so deleted file can't serve ghost data
	if d.fs.cache != nil {
		d.fs.cache.InvalidateFile(childPath)
	}

	// Remove from DB only after S3 delete confirmed (or confirmed-not-found)
	if err := d.fs.repo.DeleteEntry(ctx, childPath); err != nil {
		return err
	}

	return nil
}

// Rename renames a file or directory
func (d *Dir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	newParent, ok := newDir.(*Dir)
	if !ok {
		return syscall.EINVAL
	}

	oldPath := metadata.JoinPath(d.path, req.OldName)
	newPath := metadata.JoinPath(newParent.path, req.NewName)

	log.Info("Rename: %s → %s", oldPath, newPath)

	// Get the source entry from database
	entry, err := d.fs.repo.GetEntry(ctx, oldPath)
	if err != nil {
		if err == metadata.ErrNotFound {
			return syscall.ENOENT
		}
		return err
	}

	// Check if destination exists and delete it
	if _, err := d.fs.repo.GetEntry(ctx, newPath); err == nil {
		if err := d.fs.repo.DeleteEntry(ctx, newPath); err != nil {
			return err
		}
	}

	// Create new entry with updated path
	newEntry := &metadata.FileEntry{
		Path:    newPath,
		Size:    entry.Size,
		ModTime: time.Now(),
		IsDir:   entry.IsDir,
		ETag:    entry.ETag,
		Xattrs:  entry.Xattrs, // Preserve extended attributes (including ACLs)
	}

	if err := d.fs.repo.UpdateEntry(ctx, newEntry); err != nil {
		return err
	}

	// Delete old entry
	if err := d.fs.repo.DeleteEntry(ctx, oldPath); err != nil {
		return err
	}

	// *** CRITICAL: Invalidate chunk cache for BOTH paths ***
	// Without this, the ChunkManager will serve stale cached chunks for the
	// destination path, causing "modification disappears after a few minutes"
	// when the SMB client's own cache expires and it re-reads from the gateway.
	if d.fs.cache != nil {
		d.fs.cache.InvalidateFile(oldPath)
		d.fs.cache.InvalidateFile(newPath)
	}

	// Handle S3 rename (copy + delete) synchronously.
	// Use a background context — the FUSE request context can be cancelled.
	s3Ctx, s3Cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer s3Cancel()

	if err := d.fs.s3.CopyObject(s3Ctx, oldPath, newPath); err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "NoSuchKey") || strings.Contains(errStr, "NotFound") || strings.Contains(errStr, "404") {
			// Source key doesn't exist in S3. This is common for Office
			// save-via-temp-rename. Try to upload from the staging file
			// directly under the new path so data is not stranded.
			stagingPath := d.fs.pathToStagingFile(oldPath)
			if sf, openErr := os.Open(stagingPath); openErr == nil {
				defer sf.Close()
				if stat, statErr := sf.Stat(); statErr == nil && stat.Size() > 0 {
					if uploadErr := d.fs.s3.PutObjectFromReader(s3Ctx, newPath, sf, stat.Size(), "application/octet-stream"); uploadErr != nil {
						log.Warn("failed to upload staging file for renamed %s: %v", newPath, uploadErr)
					} else {
						// Update the new entry size from the actual upload
						newEntry.Size = stat.Size()
						_ = d.fs.repo.UpdateEntry(s3Ctx, newEntry)
						log.Info("Rename (staging upload): %s (%d bytes)", newPath, stat.Size())
					}
				}
			} else {
				log.Debug("S3 copy skipped (source not in S3, no staging file): %s → %s", oldPath, newPath)
			}
		} else {
			log.Warn("Rename S3 copy failed %s → %s: %v", oldPath, newPath, err)
		}
	} else {
		// Copy succeeded — delete the old key
		log.Info("Rename S3 copy OK: %s → %s", oldPath, newPath)
		if err := d.fs.s3.DeleteObject(s3Ctx, oldPath); err != nil {
			log.Warn("failed to delete old S3 object %s: %v", oldPath, err)
		}
	}

	return nil
}

// ============================================================================
// Directory Extended Attributes (Xattr) - Critical for Samba ACL Support
// ============================================================================

// Getxattr retrieves an extended attribute from the SQLite database
// This is called by Samba when reading Windows ACLs (security.NTACL)
func (d *Dir) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	// Query the xattr from SQLite database
	value, err := d.fs.repo.GetXattr(ctx, d.path, req.Name)
	if err != nil {
		if err == metadata.ErrNotFound || err == metadata.ErrXattrNotFound {
			return syscall.ENODATA // ENOATTR on some systems
		}
		return err
	}

	// If req.Size is 0, return the size of the attribute
	if req.Size == 0 {
		resp.Xattr = nil
		// The caller just wants to know the size
		return nil
	}

	// Return the attribute value
	resp.Xattr = value
	return nil
}

// Setxattr sets an extended attribute in the SQLite database
// This is called by Samba when setting Windows ACLs (security.NTACL)
func (d *Dir) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	// Ensure the directory entry exists in the database
	_, err := d.fs.repo.GetEntry(ctx, d.path)
	if err != nil {
		if err == metadata.ErrNotFound {
			// Create the entry if it doesn't exist (for root or new directories)
			entry := &metadata.FileEntry{
				Path:    d.path,
				IsDir:   true,
				ModTime: time.Now(),
			}
			if err := d.fs.repo.UpdateEntry(ctx, entry); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	// Store the xattr in SQLite database
	// This includes security.NTACL for Windows ACLs from Samba
	if err := d.fs.repo.SetXattr(ctx, d.path, req.Name, req.Xattr); err != nil {
		return err
	}

	return nil
}

// Listxattr returns all extended attribute names for this directory
func (d *Dir) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	names, err := d.fs.repo.ListXattrs(ctx, d.path)
	if err != nil {
		if err == metadata.ErrNotFound {
			return nil // No xattrs is not an error
		}
		return err
	}

	// Build the response: null-terminated list of attribute names
	for _, name := range names {
		resp.Xattr = append(resp.Xattr, []byte(name)...)
		resp.Xattr = append(resp.Xattr, 0) // null terminator
	}

	return nil
}

// Removexattr removes an extended attribute from the directory
func (d *Dir) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	err := d.fs.repo.RemoveXattr(ctx, d.path, req.Name)
	if err != nil {
		if err == metadata.ErrNotFound || err == metadata.ErrXattrNotFound {
			return syscall.ENODATA
		}
		return err
	}
	return nil
}

// Setattr allows changing directory attributes such as owner, group, and mode
func (d *Dir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	entry, err := d.fs.repo.GetEntry(ctx, d.path)
	if err != nil {
		return err
	}

	if req.Valid.Mode() {
		entry.SetPosixMode(os.FileMode(req.Mode))
	}

	if req.Valid.Uid() || req.Valid.Gid() {
		uid, gid, _ := entry.GetPosixOwner()
		if req.Valid.Uid() {
			uid = req.Uid
		}
		if req.Valid.Gid() {
			gid = req.Gid
		}
		entry.SetPosixOwner(uid, gid)
	}

	if err := d.fs.repo.UpdateEntry(ctx, entry); err != nil {
		return err
	}

	resp.Attr.Inode = PathToInode(d.path)
	resp.Attr.Mode = os.ModeDir | 0777
	if mode, ok := entry.GetPosixMode(); ok {
		resp.Attr.Mode = os.ModeDir | mode
	}
	if uid, gid, ok := entry.GetPosixOwner(); ok {
		resp.Attr.Uid = uid
		resp.Attr.Gid = gid
	}

	return nil
}

// ============================================================================
// File Node
// ============================================================================

// File represents a file in the filesystem
type File struct {
	fs        *FS
	path      string
	entry     *metadata.FileEntry
	s3Checked bool // true once the HeadObject size-fallback has been attempted
}

var _ fs.Node = (*File)(nil)
var _ fs.NodeOpener = (*File)(nil)
var _ fs.NodeGetxattrer = (*File)(nil)
var _ fs.NodeSetxattrer = (*File)(nil)
var _ fs.NodeListxattrer = (*File)(nil)
var _ fs.NodeRemovexattrer = (*File)(nil)
var _ fs.NodeSetattrer = (*File)(nil)
var _ fs.NodeFsyncer = (*File)(nil)

// Attr returns the file attributes from the SQLite database (NOT from S3)
func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = PathToInode(f.path)
	a.Mode = 0666
	a.Nlink = 1
	a.Valid = 5 * time.Second // Attribute cache TTL — reduces kernel stat() calls

	// Get fresh entry from SQLite database
	entry, err := f.fs.repo.GetEntry(ctx, f.path)
	if err != nil {
		if err == metadata.ErrNotFound {
			// Use cached entry if DB lookup fails
			if f.entry != nil {
				a.Size = uint64(f.entry.Size)
				a.Atime = f.entry.ModTime
				a.Mtime = f.entry.ModTime
				a.Ctime = f.entry.ModTime
				a.Blocks = (a.Size + 511) / 512
				return nil
			}
			return syscall.ENOENT
		}
		return err
	}

	// S3 FALLBACK FOR 0-BYTE ENTRIES
	// If the DB has size=0 and the entry has never been verified against S3
	// (S3VerifiedAt is zero), perform a one-shot HeadObject to get the real
	// size and persist it.  The S3VerifiedAt timestamp is stored in the DB
	// so this check survives across FUSE Lookup calls (unlike the old
	// per-struct s3Checked bool which was reset on every Lookup).
	if entry.Size == 0 && !entry.IsDir && entry.S3VerifiedAt.IsZero() {
		if s3Info, fetchErr := f.fs.s3.HeadObjectInfo(ctx, f.path); fetchErr == nil && s3Info.Size > 0 {
			entry.Size = s3Info.Size
			entry.ModTime = s3Info.LastModified

			now := time.Now()
			// Persist both the corrected size and the verification timestamp so
			// future Attr() calls don't repeat the HeadObject round-trip.
			_ = f.fs.repo.UpdateEntryFields(context.Background(), f.path, map[string]interface{}{
				"size":           s3Info.Size,
				"mod_time":       s3Info.LastModified,
				"s3_verified_at": now,
			})
			entry.S3VerifiedAt = now
		} else if fetchErr != nil {
			// HeadObject failed (network blip, rate limit, etc.).
			// Do NOT set S3VerifiedAt so we retry next time Attr() is called.
			log.Debug("Attr: HeadObject fallback failed for %s: %v", f.path, fetchErr)
		}
	}

	// Return resolved attributes
	a.Size = uint64(entry.Size)
	a.Atime = entry.ModTime
	a.Mtime = entry.ModTime
	a.Ctime = entry.ModTime

	// Update cached entry
	f.entry = entry

	// If POSIX ownership/mode stored in xattrs, expose them via Attr
	if uid, gid, ok := entry.GetPosixOwner(); ok {
		a.Uid = uid
		a.Gid = gid
	}
	if mode, ok := entry.GetPosixMode(); ok {
		a.Mode = mode
	}

	a.Blocks = (a.Size + 511) / 512
	return nil
}

// Setattr sets file attributes
func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	// Build a map of ONLY the fields being changed. This prevents overwriting
	// fields (like Size) that may have been concurrently updated by Flush().
	updates := make(map[string]interface{})

	if req.Valid.Size() {
		updates["size"] = int64(req.Size)
		// Invalidate cached chunks — file content is changing size
		if f.fs.cache != nil {
			f.fs.cache.InvalidateFile(f.path)
		}
	}
	if req.Valid.Mtime() {
		updates["mod_time"] = req.Mtime
	}

	// Mode and ownership are stored as xattrs — we need to read the current
	// entry to merge xattr changes, but we only write back the xattrs column.
	if req.Valid.Mode() || req.Valid.Uid() || req.Valid.Gid() {
		entry, err := f.fs.repo.GetEntry(ctx, f.path)
		if err != nil {
			return err
		}

		if req.Valid.Mode() {
			entry.SetPosixMode(os.FileMode(req.Mode))
		}
		if req.Valid.Uid() || req.Valid.Gid() {
			uid, gid, _ := entry.GetPosixOwner()
			if req.Valid.Uid() {
				uid = req.Uid
			}
			if req.Valid.Gid() {
				gid = req.Gid
			}
			entry.SetPosixOwner(uid, gid)
		}

		updates["xattrs"] = entry.Xattrs
	}

	// Apply partial update — only the columns in the map are modified
	if len(updates) > 0 {
		if err := f.fs.repo.UpdateEntryFields(ctx, f.path, updates); err != nil {
			return err
		}
	}

	// Re-read the full entry for the response and in-memory cache
	entry, err := f.fs.repo.GetEntry(ctx, f.path)
	if err != nil {
		return err
	}
	f.entry = entry

	// Fill response
	resp.Attr.Inode = PathToInode(f.path)
	resp.Attr.Mode = 0666
	resp.Attr.Nlink = 1
	resp.Attr.Uid = uint32(os.Getuid())
	resp.Attr.Gid = uint32(os.Getgid())
	resp.Attr.Size = uint64(entry.Size)
	resp.Attr.Atime = entry.ModTime
	resp.Attr.Mtime = entry.ModTime
	resp.Attr.Ctime = entry.ModTime

	return nil
}

// Fsync syncs file data to storage
func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	// Fsync is handled by the file handle
	return nil
}

// Open opens the file
func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	handle := &FileHandle{
		file:  f,
		dirty: false,
	}

	isWrite := req.Flags.IsWriteOnly() || req.Flags.IsReadWrite()
	var stagingPath string
	var stagingFile *os.File
	var err error

	// If the file is dirty locally OR has a retained local read-cache staging file,
	// we use the local staging file.
	if f.entry != nil && f.entry.LocalStagingPath != "" {
		stagingPath = f.entry.LocalStagingPath
		flags := os.O_RDONLY
		if isWrite {
			flags = os.O_RDWR
		}
		stagingFile, err = os.OpenFile(stagingPath, flags, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open local dirty cache file: %w", err)
		}
	} else if isWrite {
		// New write operation on a clean file: create a staging file
		if err := os.MkdirAll(f.fs.stagingDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create staging directory %s: %w", f.fs.stagingDir, err)
		}
		stagingPath = f.fs.pathToStagingFile(f.path)
		stagingFile, err = os.OpenFile(stagingPath, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open staging file: %w", err)
		}
	}

	handle.stagingPath = stagingPath
	handle.stagingFile = stagingFile

	return handle, nil
}

// ============================================================================
// File Extended Attributes (Xattr) - Critical for Samba ACL Support
// ============================================================================

// Getxattr retrieves an extended attribute from the SQLite database
// This is called by Samba when reading Windows ACLs (security.NTACL)
func (f *File) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	// Query the xattr from SQLite database
	value, err := f.fs.repo.GetXattr(ctx, f.path, req.Name)
	if err != nil {
		if err == metadata.ErrNotFound || err == metadata.ErrXattrNotFound {
			return syscall.ENODATA // ENOATTR on some systems
		}
		return err
	}

	// If req.Size is 0, return the size of the attribute
	if req.Size == 0 {
		resp.Xattr = nil
		return nil
	}

	// Return the attribute value
	resp.Xattr = value
	return nil
}

// Setxattr sets an extended attribute in the SQLite database
// This is called by Samba when setting Windows ACLs (security.NTACL)
func (f *File) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	// Store the xattr in SQLite database
	// This includes security.NTACL for Windows ACLs from Samba
	if err := f.fs.repo.SetXattr(ctx, f.path, req.Name, req.Xattr); err != nil {
		return err
	}

	return nil
}

// Listxattr returns all extended attribute names for this file
func (f *File) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	names, err := f.fs.repo.ListXattrs(ctx, f.path)
	if err != nil {
		if err == metadata.ErrNotFound {
			return nil // No xattrs is not an error
		}
		return err
	}

	// Build the response: null-terminated list of attribute names
	for _, name := range names {
		resp.Xattr = append(resp.Xattr, []byte(name)...)
		resp.Xattr = append(resp.Xattr, 0) // null terminator
	}

	return nil
}

// Removexattr removes an extended attribute from the file
func (f *File) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	err := f.fs.repo.RemoveXattr(ctx, f.path, req.Name)
	if err != nil {
		if err == metadata.ErrNotFound || err == metadata.ErrXattrNotFound {
			return syscall.ENODATA
		}
		return err
	}
	return nil
}

// ============================================================================
// File Handle
// ============================================================================

// FileHandle represents an open file handle
type FileHandle struct {
	file        *File
	stagingPath string   // Path to local staging file
	stagingFile *os.File // Open staging file handle
	dirty       bool     // True if data has been written
	mu          sync.Mutex
	fetchOnce   sync.Once
}

var _ fs.Handle = (*FileHandle)(nil)
var _ fs.HandleReader = (*FileHandle)(nil)
var _ fs.HandleWriter = (*FileHandle)(nil)
var _ fs.HandleFlusher = (*FileHandle)(nil)
var _ fs.HandleReleaser = (*FileHandle)(nil)

// Read reads data from the file (from staging file if dirty, otherwise from S3)
func (h *FileHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// If we have a local staging file from a write operation OR an existing retained cache
	if h.stagingFile != nil && (h.dirty || (h.file.entry != nil && h.file.entry.LocalStagingPath != "")) {
		buf := make([]byte, req.Size)
		n, err := h.stagingFile.ReadAt(buf, req.Offset)
		if err != nil && err != io.EOF {
			return err
		}
		resp.Data = buf[:n]
		return nil
	}



	// Otherwise, read from S3 (lazy loading)
	s3Key := h.file.path
	
	// Use cache manager if available, otherwise read directly from S3
	if h.file.fs.cache != nil {
		// Use cache manager for chunked reading
		buf := make([]byte, req.Size)
		n, err := h.file.fs.cache.Read(ctx, s3Key, req.Offset, req.Size, buf)
		if err != nil {
			// Check if this is a "not found" error
			if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "NotFound") {
				// File doesn't exist in S3 (might be newly created)
				if h.file.fs.cfg.Debug {
					log.Debug("File %s not found in S3 during read: %v\n", s3Key, err)
				}
				// Return EOF (0 bytes) for non-existent files
				resp.Data = nil
				return nil
			}
			
			// For other errors (network issues, permissions, etc.), log and return I/O error
			if h.file.fs.cfg.Debug {
				log.Debug("Cache read error for %s at offset %d size %d: %v\n", 
					s3Key, req.Offset, req.Size, err)
			}
			return syscall.EIO
		}
		resp.Data = buf[:n]
		return nil
	} else {
		// Fall back to direct S3 read
		data, err := h.file.fs.s3.GetObjectChunk(ctx, s3Key, req.Offset, int64(req.Size))
		if err != nil {
			// Check if this is a "not found" error
			if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "NotFound") {
				// File doesn't exist in S3 (might be newly created)
				if h.file.fs.cfg.Debug {
					log.Debug("File %s not found in S3 during read: %v\n", s3Key, err)
				}
				// Return EOF (0 bytes) for non-existent files
				resp.Data = nil
				return nil
			}
			
			// For other errors (network issues, permissions, etc.), log and return I/O error
			if h.file.fs.cfg.Debug {
				log.Debug("S3 read error for %s at offset %d size %d: %v\n", 
					s3Key, req.Offset, req.Size, err)
			}
			return syscall.EIO
		}
		resp.Data = data
		return nil
	}
}

// Write writes data to the local staging file
func (h *FileHandle) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if h.stagingFile == nil {
		log.Warn("Write called with nil staging file for %s", h.file.path)
		return syscall.EIO
	}

	// First time modifying the file: securely fetch original S3 payload into staging file.
	// We do this OUTSIDE the handle mutex using sync.Once so that a slow download
	// doesn't freeze the entire FUSE mount. We use a background context so the download
	// isn't aborted (leaving a corrupted staging file) if the SMB client times out this specific write request.
	h.fetchOnce.Do(func() {
		if h.file.entry != nil && h.file.entry.Size > 0 {
			fetchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			reader, _, err := h.file.fs.s3.GetObject(fetchCtx, h.file.path)
			if err == nil && reader != nil {
				defer reader.Close()
				h.stagingFile.Seek(0, io.SeekStart)
				io.Copy(h.stagingFile, reader)
			} else if err != nil {
				errStr := err.Error()
				if !strings.Contains(errStr, "NoSuchKey") && !strings.Contains(errStr, "NotFound") {
					log.Warn("Lazy stage logic failed to fetch %s before write: %v", h.file.path, err)
				}
			}
		}
	})

	h.mu.Lock()
	defer h.mu.Unlock()


	// Write to the staging file at the specified offset
	n, err := h.stagingFile.WriteAt(req.Data, req.Offset)
	if err != nil {
		return fmt.Errorf("failed to write to staging file: %w", err)
	}

	resp.Size = n

	// Update in-memory file size tracking
	endOffset := req.Offset + int64(n)
	if h.file.entry != nil && endOffset > h.file.entry.Size {
		h.file.entry.Size = endOffset
		h.file.entry.ModTime = time.Now()
	}

	// On the first write, persist the size to DB so that Rename (which reads
	// from DB) always sees the correct size — not 0 from the initial Create.
	if !h.dirty {
		h.dirty = true
		if h.file.entry != nil {
			size := h.file.entry.Size
			modTime := h.file.entry.ModTime
			path := h.file.path
			go func() {
				_ = h.file.fs.repo.UpdateEntryFields(context.Background(), path, map[string]interface{}{
					"size":     size,
					"mod_time": modTime,
				})
			}()
		}
	}

	return nil
}

// Flush is called when the file descriptor is closed (may be called multiple times).
// Instead of uploading to S3 synchronously, we now implement a WRITE-BACK cache.
// We flush the staging file to disk, mark the file as LocalDirty in the SQLite database,
// and return immediately. The background writeback daemon handles the S3 upload later.
func (h *FileHandle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Nothing to do if the file was never modified by this handle
	if !h.dirty || h.stagingFile == nil {
		return nil
	}

	log.Info("Flush: saving local changes to cache %s", h.file.path)

	// Sync staging file to disk
	if err := h.stagingFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync staging file: %w", err)
	}

	// Get file size from the staging file
	stat, err := h.stagingFile.Stat()
	if err != nil {
		return syscall.EIO
	}
	fileSize := stat.Size()

	// Invalidate chunk cache BEFORE updating DB so no concurrent reader can serve stale S3 chunks
	if h.file.fs.cache != nil {
		h.file.fs.cache.InvalidateFile(h.file.path)
	}

	// Update SQLite database with new Size/ModTime and mark it dirty for writeback.
	now := time.Now()
	if h.file.entry == nil {
		h.file.entry = &metadata.FileEntry{
			Path:  h.file.path,
			IsDir: false,
		}
	}
	h.file.entry.Size = fileSize
	h.file.entry.ModTime = now
	h.file.entry.LocalDirty = true
	h.file.entry.LocalStagingPath = h.stagingPath

	if err := h.file.fs.repo.UpdateEntry(context.Background(), h.file.entry); err != nil {
		log.Warn("failed to update metadata after local write: %v\n", err)
	}

	// Mark as no longer dirty so repeated Flush() calls (e.g. dup'd fds) don't
	// trigger redundant updates.
	h.dirty = false

	return nil
}

// Release is called when the last reference to the file handle is dropped.
func (h *FileHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Close the staging file handle
	if h.stagingFile != nil {
		h.stagingFile.Close()
		h.stagingFile = nil
	}

	// Delete the local staging file ONLY if it's not marked for writeback.
	// If it is dirty, the background writeback daemon will upload and delete it.
	if h.stagingPath != "" {
		entry, err := h.file.fs.repo.GetEntry(context.Background(), h.file.path)
		if err != nil || !entry.LocalDirty {
			if err := os.Remove(h.stagingPath); err != nil && !os.IsNotExist(err) {
				log.Warn("failed to remove staging file %s: %v\n", h.stagingPath, err)
			}
		}
	}

	return nil
}
