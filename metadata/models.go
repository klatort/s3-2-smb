package metadata

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// FileEntry represents a file or directory entry cached from S3
type FileEntry struct {
	Path             string    `gorm:"primaryKey;not null" json:"path"`
	Size             int64     `gorm:"not null;default:0" json:"size"`
	ModTime          time.Time `gorm:"not null" json:"mod_time"`
	IsDir            bool      `gorm:"not null;default:false" json:"is_dir"`
	ETag             string    `gorm:"" json:"etag,omitempty"`
	Xattrs           XattrMap  `gorm:"type:text" json:"xattrs,omitempty"` // JSON blob for extended attributes
	// S3VerifiedAt is the timestamp of the last successful HeadObject check
	// for this entry. Used to suppress redundant HeadObject round-trips when
	// the entry has already been confirmed accurate recently.
	S3VerifiedAt     time.Time `gorm:"default:null" json:"s3_verified_at,omitempty"`
	
	// Write-back caching fields
	LocalDirty       bool      `gorm:"not null;default:false" json:"local_dirty"`
	LocalStagingPath string    `gorm:"default:null" json:"local_staging_path,omitempty"`
}

// TableName specifies the table name for GORM
func (FileEntry) TableName() string {
	return "file_entries"
}

// XattrMap is a map of extended attributes that serializes to JSON
type XattrMap map[string][]byte

// Value implements driver.Valuer for database serialization
func (x XattrMap) Value() (driver.Value, error) {
	if x == nil {
		return nil, nil
	}
	// Convert []byte values to base64 strings for JSON storage
	strMap := make(map[string]string)
	for k, v := range x {
		strMap[k] = string(v)
	}
	return json.Marshal(strMap)
}

// Scan implements sql.Scanner for database deserialization
func (x *XattrMap) Scan(value interface{}) error {
	if value == nil {
		*x = nil
		return nil
	}

	var data []byte
	switch v := value.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return errors.New("invalid type for XattrMap")
	}

	if len(data) == 0 {
		*x = nil
		return nil
	}

	strMap := make(map[string]string)
	if err := json.Unmarshal(data, &strMap); err != nil {
		return err
	}

	result := make(XattrMap)
	for k, v := range strMap {
		result[k] = []byte(v)
	}
	*x = result
	return nil
}

// GetXattr retrieves an extended attribute by name
func (e *FileEntry) GetXattr(name string) ([]byte, bool) {
	if e.Xattrs == nil {
		return nil, false
	}
	val, ok := e.Xattrs[name]
	return val, ok
}

// SetXattr sets an extended attribute
func (e *FileEntry) SetXattr(name string, value []byte) {
	if e.Xattrs == nil {
		e.Xattrs = make(XattrMap)
	}
	e.Xattrs[name] = value
}

// RemoveXattr removes an extended attribute
func (e *FileEntry) RemoveXattr(name string) {
	if e.Xattrs != nil {
		delete(e.Xattrs, name)
	}
}

// ListXattrNames returns all extended attribute names
func (e *FileEntry) ListXattrNames() []string {
	if e.Xattrs == nil {
		return nil
	}
	names := make([]string, 0, len(e.Xattrs))
	for name := range e.Xattrs {
		names = append(names, name)
	}
	return names
}

// Posix ownership and mode helpers stored as xattrs so they persist in SQLite.
// Keys used: user.posix.uid, user.posix.gid, user.posix.mode

// SetPosixOwner stores uid and gid as decimal strings in xattrs.
func (e *FileEntry) SetPosixOwner(uid, gid uint32) {
	if e.Xattrs == nil {
		e.Xattrs = make(XattrMap)
	}
	e.Xattrs["user.posix.uid"] = []byte(fmt.Sprintf("%d", uid))
	e.Xattrs["user.posix.gid"] = []byte(fmt.Sprintf("%d", gid))
}

// GetPosixOwner returns uid,gid and whether they were found.
func (e *FileEntry) GetPosixOwner() (uint32, uint32, bool) {
	if e.Xattrs == nil {
		return 0, 0, false
	}
	uidb, uok := e.Xattrs["user.posix.uid"]
	gidb, gok := e.Xattrs["user.posix.gid"]
	if !uok || !gok {
		return 0, 0, false
	}
	var uid, gid uint32
	if _, err := fmt.Sscanf(string(uidb), "%d", &uid); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(string(gidb), "%d", &gid); err != nil {
		return 0, 0, false
	}
	return uid, gid, true
}

// SetPosixMode stores the file mode as an octal/decimal string in xattrs.
func (e *FileEntry) SetPosixMode(mode os.FileMode) {
	if e.Xattrs == nil {
		e.Xattrs = make(XattrMap)
	}
	e.Xattrs["user.posix.mode"] = []byte(fmt.Sprintf("%o", uint32(mode)))
}

// GetPosixMode returns the stored mode and whether it was found.
func (e *FileEntry) GetPosixMode() (os.FileMode, bool) {
	if e.Xattrs == nil {
		return 0, false
	}
	mb, ok := e.Xattrs["user.posix.mode"]
	if !ok {
		return 0, false
	}
	var m uint32
	if _, err := fmt.Sscanf(string(mb), "%o", &m); err != nil {
		return 0, false
	}
	return os.FileMode(m), true
}
