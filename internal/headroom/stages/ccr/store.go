package ccr

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// defaultMaxStoreMB is the fallback capacity if CCR maxStoreMB is <= 0.
const defaultMaxStoreMB = 64

type ccrEntry struct {
	id    string
	value string
	size  int64
}

// CCRStore is an in-memory, thread-safe LRU store for Content-Conditioned Retrieval.
// A plain sync.Mutex is used instead of sync.RWMutex because Get mutates the LRU order
// by promoting the accessed element to the front of the list.
type CCRStore struct {
	mu           sync.Mutex
	maxBytes     int64
	currentBytes int64
	ll           *list.List
	cache        map[string]*list.Element
}

// ChunkID computes the deterministic content-addressed identifier for a string payload:
// chunk_<sha256[:12]>
func ChunkID(content string) string {
	h := sha256.Sum256([]byte(content))
	return "chunk_" + hex.EncodeToString(h[:])[:12]
}

// NewCCRStore creates a new CCRStore with capacity in bytes.
func NewCCRStore(maxBytes int64) *CCRStore {
	if maxBytes <= 0 {
		maxBytes = int64(defaultMaxStoreMB) * 1024 * 1024
	}
	return &CCRStore{
		maxBytes: maxBytes,
		ll:       list.New(),
		cache:    make(map[string]*list.Element),
	}
}

// NewCCRStoreFromMB creates a new CCRStore with capacity in megabytes.
func NewCCRStoreFromMB(maxMB int) *CCRStore {
	if maxMB <= 0 {
		maxMB = defaultMaxStoreMB
	}
	return NewCCRStore(int64(maxMB) * 1024 * 1024)
}

// Put stores content in the LRU cache.
// Returns the chunk ID and true if stored, or ("", false) if rejected (e.g. entry exceeds total capacity).
func (s *CCRStore) Put(content string) (string, bool) {
	return s.putWithID(ChunkID(content), content)
}

// putWithID is Put with the identifier supplied by the caller, so the
// truncated-id collision path is directly testable.
func (s *CCRStore) putWithID(id, content string) (string, bool) {
	size := int64(len(content))

	s.mu.Lock()
	defer s.mu.Unlock()

	// Reject oversized entries up front without evicting existing entries.
	if size > s.maxBytes || s.maxBytes <= 0 {
		return "", false
	}

	if elem, ok := s.cache[id]; ok {
		entry := elem.Value.(*ccrEntry)
		if entry.value == content {
			// Same payload: promote to front.
			s.ll.MoveToFront(elem)
			return id, true
		}
		// ChunkID keeps 48 bits of the digest, so two payloads can share an id.
		// The token was minted for this content, so this content is what a later
		// retrieve must return; the older payload is dropped.
		s.currentBytes += size - entry.size
		entry.value = content
		entry.size = size
		s.ll.MoveToFront(elem)
		// Never evict the entry just written: it is at the front, so stop while
		// it is the only one left.
		for s.currentBytes > s.maxBytes && s.ll.Len() > 1 {
			s.evictOldestLocked()
		}
		return id, true
	}

	// Evict least recently used entries until there is enough space.
	for s.currentBytes+size > s.maxBytes && s.ll.Len() > 0 {
		s.evictOldestLocked()
	}

	entry := &ccrEntry{
		id:    id,
		value: content,
		size:  size,
	}
	elem := s.ll.PushFront(entry)
	s.cache[id] = elem
	s.currentBytes += size

	return id, true
}

// Get retrieves content by chunk ID, updating its LRU position.
func (s *CCRStore) Get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	elem, ok := s.cache[id]
	if !ok {
		return "", false
	}

	s.ll.MoveToFront(elem)
	entry := elem.Value.(*ccrEntry)
	return entry.value, true
}

// Size returns the count of items in the store.
func (s *CCRStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ll.Len()
}

// Bytes returns the total byte size of items in the store.
func (s *CCRStore) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentBytes
}

func (s *CCRStore) evictOldestLocked() {
	elem := s.ll.Back()
	if elem == nil {
		return
	}
	s.ll.Remove(elem)
	entry := elem.Value.(*ccrEntry)
	delete(s.cache, entry.id)
	s.currentBytes -= entry.size
}
