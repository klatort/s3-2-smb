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
	a.Uid = uint32(os.Getuid())
	a.Gid = uint32(os.Getgid())
	a.Valid = 1 * time.Second // Force attribute TTL to 1 second

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
	if req.Mode != 0 {
		if modeEntry, err := d.fs.repo.GetEntry(ctx, childPath); err == nil {
			modeEntry.SetPosixMode(os.FileMode(req.Mode))
			_ = d.fs.repo.UpdateEntry(context.Background(), modeEntry)
		}
	}

	// Create directory marker in S3 (async, non-blocking)
	go func() {
		s3Key := childPath
		if err := d.fs.s3.CreateDirectory(context.Background(), s3Key); err != nil {
			log.Warn("Failed to create S3 directory %s: %v", s3Key, err)
		}
	}()

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

	// Set response flags for direct I/O to avoid kernel caching issues
	resp.Flags |= fuse.OpenDirectIO

	// Persist requested mode/owner if provided
	if req.Mode != 0 {
		if e, err := d.fs.repo.GetEntry(ctx, childPath); err == nil {
			e.SetPosixMode(os.FileMode(req.Mode))
			_ = d.fs.repo.UpdateEntry(context.Background(), e)
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

	// Delete from SQLite database
	if err := d.fs.repo.DeleteEntry(ctx, childPath); err != nil {
		return err
	}

	// Delete from S3 (async)
	go func() {
		if err := d.fs.s3.DeleteObject(context.Background(), childPath); err != nil {
			log.Warn("failed to delete S3 object %s: %v\n", childPath, err)
		}
	}()

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

	// Handle S3 rename (copy + delete) async
	go func() {
		if err := d.fs.s3.CopyObject(context.Background(), oldPath, newPath); err != nil {
			log.Warn("failed to copy S3 object: %v\n", err)
			return
		}
		if err := d.fs.s3.DeleteObject(context.Background(), oldPath); err != nil {
			log.Warn("failed to delete old S3 object: %v\n", err)
		}
	}()

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
	fs    *FS
	path  string
	entry *metadata.FileEntry
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
	a.Uid = uint32(os.Getuid())
	a.Gid = uint32(os.Getgid())
	a.Valid = 1 * time.Second // Force attribute TTL to 1 second

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

	// REAL-TIME S3 FETCHING FALLBACK FOR 0-BYTE ENTRIES
	if entry.Size == 0 {
		if s3Info, fetchErr := f.fs.s3.HeadObjectInfo(ctx, f.path); fetchErr == nil && s3Info.Size > 0 {
			// S3 contains a real file, database cache is out of sync!
			entry.Size = s3Info.Size
			entry.ModTime = s3Info.LastModified
			
			// Auto-cure database in background
			go func(e *metadata.FileEntry) {
				_ = f.fs.repo.UpdateEntry(context.Background(), e)
			}(entry)
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
	entry, err := f.fs.repo.GetEntry(ctx, f.path)
	if err != nil {
		return err
	}

	// Update size if requested (truncate)
	if req.Valid.Size() {
		entry.Size = int64(req.Size)
	}

	// Update modification time if requested
	if req.Valid.Mtime() {
		entry.ModTime = req.Mtime
	}

	// Update permissions/mode if requested
	if req.Valid.Mode() {
		entry.SetPosixMode(os.FileMode(req.Mode))
	}

	// Update ownership if requested
	if req.Valid.Uid() || req.Valid.Gid() {
		// Read existing uid/gid
		uid, gid, _ := entry.GetPosixOwner()
		if req.Valid.Uid() {
			uid = req.Uid
		}
		if req.Valid.Gid() {
			gid = req.Gid
		}
		entry.SetPosixOwner(uid, gid)
	}

	if err := f.fs.repo.UpdateEntry(ctx, entry); err != nil {
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

	// Always create a staging file for both read and write operations
	// This ensures we have local data to read from
	stagingPath := f.fs.pathToStagingFile(f.path)

	// Ensure staging directory exists
	if err := os.MkdirAll(f.fs.stagingDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create staging directory %s: %w", f.fs.stagingDir, err)
	}

	// Check if file exists in S3 and has content
	// We need to download the content for both read and write operations
	if f.entry != nil && f.entry.Size > 0 {
		// Download existing content to staging file for both read and write operations
		reader, _, err := f.fs.s3.GetObject(ctx, f.path)
		if err == nil && reader != nil {
			defer reader.Close()
			// Create or truncate the staging file
			stagingFile, err := os.Create(stagingPath)
			if err != nil {
				return nil, fmt.Errorf("failed to create staging file: %w", err)
			}
			defer stagingFile.Close()
			
			// Stream content from S3 to file
			_, err = io.Copy(stagingFile, reader)
			if err != nil {
				// Clean up the partially written file
				os.Remove(stagingPath)
				return nil, fmt.Errorf("failed to download file content: %w", err)
			}
		} else if err != nil {
			// Check if this is a "not found" error (file doesn't exist in S3)
			errStr := err.Error()
			if strings.Contains(errStr, "NoSuchKey") || strings.Contains(errStr, "NotFound") {
				// File doesn't exist in S3 (might be newly created or deleted)
				// Create empty staging file
				if err := os.WriteFile(stagingPath, []byte{}, 0644); err != nil {
					return nil, fmt.Errorf("failed to create empty staging file: %w", err)
				}
				log.Debug("File %s not found in S3, creating empty staging file\n", f.path)
			} else {
				// Other error (network, permissions, etc.)
				log.Warn("Failed to download file %s from S3: %v", f.path, err)
				// Don't fail open - create empty staging file and let read handle the error
				// This allows opening files even when S3 is temporarily unavailable
				if err := os.WriteFile(stagingPath, []byte{}, 0644); err != nil {
					return nil, fmt.Errorf("failed to create staging file: %w", err)
				}
			}
		}
	}

	// Determine file open mode
	// Always use O_CREATE to ensure staging file exists
	flags := os.O_RDONLY | os.O_CREATE
	if req.Flags.IsWriteOnly() || req.Flags.IsReadWrite() {
		flags = os.O_RDWR | os.O_CREATE
	}

	// Open or create the staging file
	stagingFile, err := os.OpenFile(stagingPath, flags, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open staging file: %w", err)
	}

	handle.stagingPath = stagingPath
	handle.stagingFile = stagingFile

	// Use DirectIO to avoid kernel caching issues with our staging approach
	resp.Flags |= fuse.OpenDirectIO

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

	// If we have a dirty staging file, read from it
	if h.dirty && h.stagingFile != nil {
		buf := make([]byte, req.Size)
		n, err := h.stagingFile.ReadAt(buf, req.Offset)
		if err != nil && err != io.EOF {
			return err
		}
		resp.Data = buf[:n]
		return nil
	}

	// If staging file exists (read-write mode), read from it
	if h.stagingFile != nil {
		buf := make([]byte, req.Size)
		n, err := h.stagingFile.ReadAt(buf, req.Offset)
		if err != nil && err != io.EOF {
			// Fall through to S3 read
			if h.file.fs.cfg.Debug {
				log.Debug("Failed to read from staging file for %s: %v\n", h.file.path, err)
			}
		} else if n > 0 {
			resp.Data = buf[:n]
			return nil
		}
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
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.stagingFile == nil {
		return syscall.EIO
	}

	// Write to the staging file at the specified offset
	n, err := h.stagingFile.WriteAt(req.Data, req.Offset)
	if err != nil {
		return fmt.Errorf("failed to write to staging file: %w", err)
	}

	h.dirty = true
	resp.Size = n

	// Update file size in database if needed
	endOffset := req.Offset + int64(n)
	if h.file.entry != nil && endOffset > h.file.entry.Size {
		h.file.entry.Size = endOffset
		h.file.entry.ModTime = time.Now()
		// Update in background to not block write
		go func() {
			_ = h.file.fs.repo.UpdateEntry(context.Background(), h.file.entry)
		}()
	}

	return nil
}

// Flush is called when the file descriptor is closed (may be called multiple times)
func (h *FileHandle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Sync staging file to disk if dirty
	if h.dirty && h.stagingFile != nil {
		if err := h.stagingFile.Sync(); err != nil {
			return fmt.Errorf("failed to sync staging file: %w", err)
		}
	}

	return nil
}

// Release releases the file handle and uploads to S3 if dirty (upload-on-close)
// This is SYNCHRONOUS - returns error to OS if upload fails
func (h *FileHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Close staging file first
	if h.stagingFile != nil {
		h.stagingFile.Close()
		h.stagingFile = nil
	}

	// If not dirty, just clean up
	if !h.dirty || h.stagingPath == "" {
		// Remove staging file if it exists
		if h.stagingPath != "" {
			os.Remove(h.stagingPath)
		}
		return nil
	}

	// Upload to S3 SYNCHRONOUSLY (upload-on-close strategy)
	// The user must know if the save failed
	s3Key := h.file.path

	// Invalidate cache for this file since it's being modified
	if h.file.fs.cache != nil {
		if h.file.fs.cfg.Debug {
			log.Debug("Invalidating cache for modified file: %s\n", s3Key)
		}
		h.file.fs.cache.InvalidateFile(s3Key)
	}

	// Open staging file for reading
	stagingFile, err := os.Open(h.stagingPath)
	if err != nil {
		return syscall.EIO // Return I/O error to OS
	}
	defer stagingFile.Close()

	// Get file size
	stat, err := stagingFile.Stat()
	if err != nil {
		return syscall.EIO
	}
	fileSize := stat.Size()

	// Upload to S3 using streaming (supports large files via multipart)
	if err := h.file.fs.s3.PutObjectFromReader(ctx, s3Key, stagingFile, fileSize, "application/octet-stream"); err != nil {
		log.Error("failed to upload %s to S3: %v\n", s3Key, err)
		return syscall.EIO // Return I/O error so user knows save failed
	}

	// S3 upload succeeded - update SQLite database with new Size/ModTime
	now := time.Now()
	if h.file.entry == nil {
		h.file.entry = &metadata.FileEntry{
			Path:  h.file.path,
			IsDir: false,
		}
	}
	h.file.entry.Size = fileSize
	h.file.entry.ModTime = now

	if err := h.file.fs.repo.UpdateEntry(ctx, h.file.entry); err != nil {
		log.Warn("failed to update metadata after upload: %v\n", err)
		// Don't return error here - S3 upload succeeded, that's what matters
	}

	// Delete the local staging file after successful upload
	if err := os.Remove(h.stagingPath); err != nil {
		log.Warn("failed to remove staging file %s: %v\n", h.stagingPath, err)
	}

	return nil // Success
}
