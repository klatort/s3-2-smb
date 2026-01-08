package cache

import (
	"container/list"
	"time"
)

// lruEntry represents an entry in the LRU cache
type lruEntry struct {
	key        string    // Unique key (path:chunkID)
	path       string    // File system path to the chunk
	size       int64     // Size of the chunk in bytes
	accessTime time.Time // Last access time
}

// lruTracker tracks chunk access for LRU eviction
type lruTracker struct {
	// Doubly-linked list for O(1) reordering
	// Front = most recently used, Back = least recently used
	list *list.List

	// Map for O(1) lookup by key
	entries map[string]*list.Element
}

// newLRUTracker creates a new LRU tracker
func newLRUTracker() *lruTracker {
	return &lruTracker{
		list:    list.New(),
		entries: make(map[string]*list.Element),
	}
}

// add adds a new entry to the LRU tracker (most recently used position)
func (l *lruTracker) add(key, path string, size int64) {
	l.addWithTime(key, path, size, time.Now())
}

// addWithTime adds a new entry with a specific access time
func (l *lruTracker) addWithTime(key, path string, size int64, accessTime time.Time) {
	// Check if entry already exists
	if elem, exists := l.entries[key]; exists {
		// Update existing entry and move to front
		entry := elem.Value.(*lruEntry)
		entry.accessTime = accessTime
		entry.size = size
		l.list.MoveToFront(elem)
		return
	}

	// Create new entry
	entry := &lruEntry{
		key:        key,
		path:       path,
		size:       size,
		accessTime: accessTime,
	}

	// Add to front of list
	elem := l.list.PushFront(entry)
	l.entries[key] = elem
}

// touch updates the access time of an entry and moves it to the front
func (l *lruTracker) touch(key, path string) {
	if elem, exists := l.entries[key]; exists {
		entry := elem.Value.(*lruEntry)
		entry.accessTime = time.Now()
		l.list.MoveToFront(elem)
	}
}

// remove removes an entry from the tracker
func (l *lruTracker) remove(key string) *lruEntry {
	if elem, exists := l.entries[key]; exists {
		entry := elem.Value.(*lruEntry)
		l.list.Remove(elem)
		delete(l.entries, key)
		return entry
	}
	return nil
}

// evictOldest removes and returns the least recently used entry
func (l *lruTracker) evictOldest() *lruEntry {
	// Get the back element (least recently used)
	elem := l.list.Back()
	if elem == nil {
		return nil
	}

	entry := elem.Value.(*lruEntry)
	l.list.Remove(elem)
	delete(l.entries, entry.key)

	return entry
}

// get returns an entry by key without modifying access time
func (l *lruTracker) get(key string) *lruEntry {
	if elem, exists := l.entries[key]; exists {
		return elem.Value.(*lruEntry)
	}
	return nil
}

// len returns the number of entries in the tracker
func (l *lruTracker) len() int {
	return l.list.Len()
}

// oldestAccessTime returns the access time of the least recently used entry
func (l *lruTracker) oldestAccessTime() (time.Time, bool) {
	elem := l.list.Back()
	if elem == nil {
		return time.Time{}, false
	}
	return elem.Value.(*lruEntry).accessTime, true
}

// totalSize returns the total size of all tracked entries
func (l *lruTracker) totalSize() int64 {
	var total int64
	for elem := l.list.Front(); elem != nil; elem = elem.Next() {
		total += elem.Value.(*lruEntry).size
	}
	return total
}
