package metadata

import "errors"

// Common errors
var (
	// ErrNotFound is returned when a file entry is not found
	ErrNotFound = errors.New("entry not found")

	// ErrXattrNotFound is returned when an extended attribute is not found
	ErrXattrNotFound = errors.New("extended attribute not found")

	// ErrInvalidPath is returned when a path is invalid
	ErrInvalidPath = errors.New("invalid path")
)
