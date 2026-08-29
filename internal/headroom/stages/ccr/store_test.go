package ccr

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCCRStore_PutAndGet(t *testing.T) {
	store := NewCCRStore(1024 * 1024) // 1MB

	payload := "hello world payload data"
	id, ok := store.Put(payload)
	if !ok || id == "" {
		t.Fatalf("expected successful Put, got id=%q ok=%v", id, ok)
	}

	if !strings.HasPrefix(id, "chunk_") {
		t.Errorf("expected id prefix 'chunk_', got %q", id)
	}

	got, found := store.Get(id)
	if !found || got != payload {
		t.Errorf("Get(%q) = (%q, %v), want (%q, true)", id, got, found, payload)
	}

	// Unknown ID returns not found
	if _, found := store.Get("chunk_nonexistent"); found {
		t.Error("expected nonexistent chunk to return false")
	}
}

func TestCCRStore_StableContentAddressedIDs(t *testing.T) {
	store := NewCCRStore(1024 * 1024)

	payload := "deterministic content"
	id1, _ := store.Put(payload)
	id2, _ := store.Put(payload)

	if id1 != id2 {
		t.Errorf("expected stable id across Puts: %q vs %q", id1, id2)
	}

	expectedID := ChunkID(payload)
	if id1 != expectedID {
		t.Errorf("expected id %q, got %q", expectedID, id1)
	}
	if len(id1) != len("chunk_")+12 {
		t.Errorf("expected id length %d, got %d (%q)", len("chunk_")+12, len(id1), id1)
	}
}

func TestCCRStore_OversizedEntryRejectedUpFront(t *testing.T) {
	// Store capacity: 100 bytes
	store := NewCCRStore(100)

	// Pre-populate with a valid small entry
	id1, ok1 := store.Put("small valid entry")
	if !ok1 {
		t.Fatal("expected small entry to be accepted")
	}
	initialBytes := store.Bytes()
	initialCount := store.Size()

	// Try inserting 200 bytes entry (> 100 capacity)
	oversized := strings.Repeat("x", 200)
	idOversized, okOversized := store.Put(oversized)
	if okOversized || idOversized != "" {
		t.Fatalf("expected oversized entry to be rejected, got id=%q ok=%v", idOversized, okOversized)
	}

	// Crucial: original entry must NOT be evicted when oversized entry was rejected
	if store.Bytes() != initialBytes || store.Size() != initialCount {
		t.Errorf("state changed after rejected put: bytes=%d size=%d", store.Bytes(), store.Size())
	}
	if got, found := store.Get(id1); !found || got != "small valid entry" {
		t.Error("pre-existing entry was incorrectly evicted by rejected oversized entry")
	}
}

func TestCCRStore_LRUEviction(t *testing.T) {
	// Capacity: 50 bytes
	store := NewCCRStore(50)

	// Insert 3 entries of 20 bytes each
	e1 := strings.Repeat("1", 20)
	e2 := strings.Repeat("2", 20)
	e3 := strings.Repeat("3", 20)

	id1, _ := store.Put(e1) // bytes: 20
	id2, _ := store.Put(e2) // bytes: 40

	// Access e1 so e2 becomes the least recently used
	if _, found := store.Get(id1); !found {
		t.Fatal("e1 not found")
	}

	// Insert e3 (20 bytes). Total would be 60 > 50, so e2 (LRU) should be evicted.
	id3, _ := store.Put(e3)

	if _, found := store.Get(id2); found {
		t.Error("expected e2 to be evicted as LRU")
	}
	if _, found := store.Get(id1); !found {
		t.Error("expected e1 to remain in store")
	}
	if _, found := store.Get(id3); !found {
		t.Error("expected e3 to be in store")
	}

	if store.Bytes() != 40 || store.Size() != 2 {
		t.Errorf("expected 40 bytes and 2 items, got %d bytes and %d items", store.Bytes(), store.Size())
	}
}

func TestCCRStore_ConcurrentAccess(t *testing.T) {
	store := NewCCRStore(100 * 1024) // 100KB

	var wg sync.WaitGroup
	workers := 16
	iterations := 100

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				payload := fmt.Sprintf("worker-%d-iter-%d-payload", workerID, i)
				id, ok := store.Put(payload)
				if ok {
					got, found := store.Get(id)
					if found && got != payload {
						t.Errorf("data corruption: got %q, want %q", got, payload)
					}
				}
			}
		}(w)
	}

	wg.Wait()
}

// TestCCRStore_IDCollisionKeepsNewestContent covers the truncated-id case:
// ChunkID keeps only 48 bits of the digest, so two distinct payloads can map to
// one id. The store must never serve the older payload under a token minted for
// the newer one, and must not double count its bytes.
func TestCCRStore_IDCollisionKeepsNewestContent(t *testing.T) {
	store := NewCCRStore(1024)
	const id = "chunk_collide0001"

	if _, ok := store.putWithID(id, "first payload"); !ok {
		t.Fatal("first put rejected")
	}
	second := "second payload, longer than the first"
	if _, ok := store.putWithID(id, second); !ok {
		t.Fatal("colliding put rejected")
	}

	got, found := store.Get(id)
	if !found {
		t.Fatal("chunk missing after colliding put")
	}
	if got != second {
		t.Fatalf("stale content served: got %q, want %q", got, second)
	}
	if store.Size() != 1 {
		t.Fatalf("expected 1 entry, got %d", store.Size())
	}
	if store.Bytes() != int64(len(second)) {
		t.Fatalf("byte accounting drifted: got %d, want %d", store.Bytes(), len(second))
	}
}

func TestCCRStore_SetMaxBytesShrinksAndEvictsOldest(t *testing.T) {
	store := NewCCRStore(1000)

	id1, ok1 := store.Put("payload_one_1234567890")   // 22 bytes
	id2, ok2 := store.Put("payload_two_1234567890")   // 22 bytes
	id3, ok3 := store.Put("payload_three_1234567890") // 24 bytes
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("failed initial puts")
	}
	if store.Size() != 3 {
		t.Fatalf("expected 3 items, got %d", store.Size())
	}

	// Shrink capacity so only the newest item fits.
	store.SetMaxBytes(30)

	if store.MaxBytes() != 30 {
		t.Fatalf("expected maxBytes 30, got %d", store.MaxBytes())
	}
	if store.Bytes() > 30 {
		t.Fatalf("expected store bytes <= 30, got %d", store.Bytes())
	}
	if _, found := store.Get(id1); found {
		t.Errorf("expected id1 to be evicted after shrink")
	}
	if _, found := store.Get(id2); found {
		t.Errorf("expected id2 to be evicted after shrink")
	}
	if _, found := store.Get(id3); !found {
		t.Errorf("expected newest item id3 to remain")
	}
}

func TestCCRStore_SetMaxBytesGrowKeepsExistingEntries(t *testing.T) {
	store := NewCCRStore(30)
	id1, ok1 := store.Put("payload_one_1234567890") // 22 bytes
	if !ok1 {
		t.Fatal("expected put to succeed")
	}

	store.SetMaxBytes(1000)

	if store.MaxBytes() != 1000 {
		t.Fatalf("expected maxBytes 1000, got %d", store.MaxBytes())
	}
	if _, found := store.Get(id1); !found {
		t.Error("expected id1 to survive a capacity increase")
	}
}

func TestCCRStore_SetMaxMBZeroOrNegativeUsesDefault(t *testing.T) {
	store := NewCCRStore(100)
	store.SetMaxMB(0)
	expectedBytes := int64(defaultMaxStoreMB) * 1024 * 1024
	if store.MaxBytes() != expectedBytes {
		t.Fatalf("expected %d bytes, got %d", expectedBytes, store.MaxBytes())
	}
}

func TestCCRStore_SetMaxMBResizesToExactValue(t *testing.T) {
	store := NewCCRStore(100)
	store.SetMaxMB(25)
	expectedBytes := int64(25) * 1024 * 1024
	if store.MaxBytes() != expectedBytes {
		t.Fatalf("expected %d bytes, got %d", expectedBytes, store.MaxBytes())
	}
}
