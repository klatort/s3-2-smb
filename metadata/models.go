package metadata

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// FileEntry represents a file or directory entry cached from S3
type FileEntry struct {
	Path    string    `gorm:"primaryKey;not null" json:"path"`
	Size    int64     `gorm:"not null;default:0" json:"size"`
	ModTime time.Time `gorm:"not null" json:"mod_time"`
	IsDir   bool      `gorm:"not null;default:false" json:"is_dir"`
	ETag    string    `gorm:"" json:"etag,omitempty"`
	Xattrs  XattrMap  `gorm:"type:text" json:"xattrs,omitempty"` // JSON blob for extended attributes
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
