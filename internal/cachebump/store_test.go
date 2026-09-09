package cachebump

import (
	"testing"
	"time"
)

func TestStore_UpsertAndGet(t *testing.T) {
	store := NewStore(time.Hour, 100)
	now := time.Now()

	rec := Record{
		SessionID: "s1",
		Route:     RouteClaudeCode,
		Model:     "claude-sonnet-5",
		AccountID: "acc-1",
		Body:      []byte(`{"messages":[]}`),
		TTL:       5 * time.Minute,
	}
	rec.Key = RecordKey(RouteClaudeCode, "s1")
	rec.LastSeen = now

	store.Upsert(rec)

	got, ok := store.Get(rec.Key)
	if !ok {
		t.Fatal("expected record to exist")
	}
	if got.SessionID != "s1" || got.Route != RouteClaudeCode || got.AccountID != "acc-1" {
		t.Errorf("unexpected record: %+v", got)
	}
	if !bytesEqual(got.Body, []byte(`{"messages":[]}`)) {
		t.Errorf("body not preserved: %s", got.Body)
	}
}

func TestStore_UpsertRearmsStoppedRecord(t *testing.T) {
	store := NewStore(time.Hour, 100)
	now := time.Now()

	rec := Record{
		Key:       RecordKey(RouteClaudeCode, "s1"),
		SessionID: "s1",
		Route:     RouteClaudeCode,
		Body:      []byte(`{}`),
		TTL:       5 * time.Minute,
		LastSeen:  now,
		NextBump:  now.Add(time.Minute),
	}
	store.Upsert(rec)
	store.Stop(rec.Key, "paid_write")
	store.MarkBumped(rec.Key, now.Add(time.Minute), 100, 0)

	got, _ := store.Get(rec.Key)
	if !got.Stopped || got.StopReason != "paid_write" || got.Bumps != 1 {
		t.Fatalf("expected stopped record, got: %+v", got)
	}

	// A new real client request re-arms: fresh body, counters reset.
	rearmed := rec
	rearmed.Body = []byte(`{"messages":[{"role":"user","content":"new"}]}`)
	rearmed.LastSeen = now.Add(10 * time.Minute)
	store.Upsert(rearmed)

	got, ok := store.Get(rec.Key)
	if !ok {
		t.Fatal("expected record after re-arm")
	}
	if got.Stopped {
		t.Error("expected record re-armed (Stopped=false)")
	}
	if got.StopReason != "" {
		t.Errorf("expected StopReason reset, got %q", got.StopReason)
	}
	if got.Bumps != 0 {
		t.Errorf("expected Bumps reset, got %d", got.Bumps)
	}
	if !bytesEqual(got.Body, rearmed.Body) {
		t.Errorf("expected fresh body, got %s", got.Body)
	}
}

func TestStore_PruneExpiredOnInsert(t *testing.T) {
	store := NewStore(time.Hour, 100)
	now := time.Now()

	rec := Record{
		Key:       RecordKey(RouteKimi, "old"),
		SessionID: "old",
		Route:     RouteKimi,
		TTL:       5 * time.Minute,
		LastSeen:  now.Add(-2 * time.Hour),
	}
	store.Upsert(rec)

	// Inserting another record triggers the prune.
	fresh := Record{
		Key:       RecordKey(RouteKimi, "new"),
		SessionID: "new",
		Route:     RouteKimi,
		TTL:       5 * time.Minute,
		LastSeen:  now,
	}
	store.Upsert(fresh)

	if _, ok := store.Get(rec.Key); ok {
		t.Error("expected expired record to be pruned")
	}
	if _, ok := store.Get(fresh.Key); !ok {
		t.Error("expected fresh record to survive")
	}
}

func TestStore_EvictOldestAtCapacity(t *testing.T) {
	store := NewStore(time.Hour, 2)
	now := time.Now()

	for i, id := range []string{"a", "b", "c"} {
		store.Upsert(Record{
			Key:       RecordKey(RouteCustom, id),
			SessionID: id,
			Route:     RouteCustom,
			TTL:       5 * time.Minute,
			LastSeen:  now.Add(time.Duration(i) * time.Minute),
		})
	}

	if _, ok := store.Get(RecordKey(RouteCustom, "a")); ok {
		t.Error("expected oldest record evicted")
	}
	for _, id := range []string{"b", "c"} {
		if _, ok := store.Get(RecordKey(RouteCustom, id)); !ok {
			t.Errorf("expected record %q to survive", id)
		}
	}
}

func TestStore_ReArmAtCapacityKeepsOtherSessions(t *testing.T) {
	store := NewStore(time.Hour, 2)
	now := time.Now()

	for i, id := range []string{"a", "b"} {
		store.Upsert(Record{
			Key:       RecordKey(RouteCustom, id),
			SessionID: id,
			Route:     RouteCustom,
			TTL:       5 * time.Minute,
			LastSeen:  now.Add(time.Duration(i) * time.Minute),
		})
	}

	// Re-arming an already-recorded session does not grow the store, so it
	// must not cost another live session its slot.
	store.Upsert(Record{
		Key:       RecordKey(RouteCustom, "b"),
		SessionID: "b",
		Route:     RouteCustom,
		TTL:       5 * time.Minute,
		LastSeen:  now.Add(2 * time.Minute),
	})

	if store.Len() != 2 {
		t.Errorf("expected 2 records, got %d", store.Len())
	}
	for _, id := range []string{"a", "b"} {
		if _, ok := store.Get(RecordKey(RouteCustom, id)); !ok {
			t.Errorf("expected record %q to survive a re-arm at capacity", id)
		}
	}
}

func TestStore_ByteBudgetEvictsOldest(t *testing.T) {
	// 300 bytes of budget holds two 128-byte bodies, not three.
	store := NewStoreWithLimits(time.Hour, 100, 300)
	now := time.Now()
	body := make([]byte, 128)

	for i, id := range []string{"a", "b", "c"} {
		store.Upsert(Record{
			Key:       RecordKey(RouteCustom, id),
			SessionID: id,
			Route:     RouteCustom,
			Body:      body,
			TTL:       5 * time.Minute,
			LastSeen:  now.Add(time.Duration(i) * time.Minute),
		})
	}

	if _, ok := store.Get(RecordKey(RouteCustom, "a")); ok {
		t.Error("expected oldest record evicted to fit the byte budget")
	}
	if store.Bytes() > 300 {
		t.Errorf("expected Bytes() within budget, got %d", store.Bytes())
	}
	for _, id := range []string{"b", "c"} {
		if _, ok := store.Get(RecordKey(RouteCustom, id)); !ok {
			t.Errorf("expected record %q to survive", id)
		}
	}
}

func TestStore_RejectsBodyLargerThanBudget(t *testing.T) {
	store := NewStoreWithLimits(time.Hour, 100, 64)
	now := time.Now()

	store.Upsert(Record{
		Key:       RecordKey(RouteKimi, "huge"),
		SessionID: "huge",
		Route:     RouteKimi,
		Body:      make([]byte, 65),
		TTL:       5 * time.Minute,
		LastSeen:  now,
	})

	if _, ok := store.Get(RecordKey(RouteKimi, "huge")); ok {
		t.Error("expected a body larger than the whole budget to be refused")
	}
	if store.Bytes() != 0 {
		t.Errorf("expected Bytes() 0, got %d", store.Bytes())
	}
}

func TestStore_BytesTracksReplacementAndClear(t *testing.T) {
	store := NewStoreWithLimits(time.Hour, 100, 1<<20)
	now := time.Now()
	key := RecordKey(RouteClaudeCode, "s1")

	store.Upsert(Record{Key: key, SessionID: "s1", Route: RouteClaudeCode, Body: make([]byte, 500), LastSeen: now})
	if store.Bytes() != 500 {
		t.Fatalf("expected Bytes() 500, got %d", store.Bytes())
	}

	store.Upsert(Record{Key: key, SessionID: "s1", Route: RouteClaudeCode, Body: make([]byte, 40), LastSeen: now})
	if store.Bytes() != 40 {
		t.Errorf("expected Bytes() 40 after replacement, got %d", store.Bytes())
	}

	store.Clear()
	if store.Bytes() != 0 {
		t.Errorf("expected Bytes() 0 after Clear, got %d", store.Bytes())
	}
}

func TestStore_BytesDropsWithPrunedRecords(t *testing.T) {
	store := NewStoreWithLimits(time.Hour, 100, 1<<20)
	now := time.Now()

	store.Upsert(Record{
		Key:       RecordKey(RouteKimi, "old"),
		SessionID: "old",
		Route:     RouteKimi,
		Body:      make([]byte, 700),
		LastSeen:  now.Add(-2 * time.Hour),
	})
	store.Upsert(Record{
		Key:       RecordKey(RouteKimi, "new"),
		SessionID: "new",
		Route:     RouteKimi,
		Body:      make([]byte, 300),
		LastSeen:  now,
	})

	if store.Bytes() != 300 {
		t.Errorf("expected pruned record's bytes released, got %d", store.Bytes())
	}
}

func TestStore_Due(t *testing.T) {
	store := NewStore(time.Hour, 100)
	now := time.Now()

	due := Record{
		Key:       RecordKey(RouteClaudeCode, "due"),
		SessionID: "due",
		Route:     RouteClaudeCode,
		TTL:       5 * time.Minute,
		LastSeen:  now,
		NextBump:  now.Add(-time.Second),
	}
	notYet := Record{
		Key:       RecordKey(RouteClaudeCode, "notyet"),
		SessionID: "notyet",
		Route:     RouteClaudeCode,
		TTL:       5 * time.Minute,
		LastSeen:  now,
		NextBump:  now.Add(time.Minute),
	}
	stopped := Record{
		Key:       RecordKey(RouteClaudeCode, "stopped"),
		SessionID: "stopped",
		Route:     RouteClaudeCode,
		TTL:       5 * time.Minute,
		LastSeen:  now,
		NextBump:  now.Add(-time.Second),
	}
	store.Upsert(due)
	store.Upsert(notYet)
	store.Upsert(stopped)
	store.Stop(stopped.Key, "paid_write")

	got := store.Due(now)
	if len(got) != 1 || got[0].SessionID != "due" {
		t.Errorf("expected only 'due' record, got %+v", got)
	}
}

func TestStore_StopAndClear(t *testing.T) {
	store := NewStore(time.Hour, 100)
	now := time.Now()

	rec := Record{
		Key:       RecordKey(RouteClaudeCode, "s1"),
		SessionID: "s1",
		Route:     RouteClaudeCode,
		TTL:       5 * time.Minute,
		LastSeen:  now,
	}
	store.Upsert(rec)

	store.Stop(rec.Key, "idle")
	got, _ := store.Get(rec.Key)
	if !got.Stopped || got.StopReason != "idle" {
		t.Errorf("expected stopped record, got %+v", got)
	}

	store.Clear()
	if _, ok := store.Get(rec.Key); ok {
		t.Error("expected store cleared")
	}
}

func TestRecordKey(t *testing.T) {
	if got := RecordKey(RouteClaudeCode, "abc"); got != "claudecode|abc" {
		t.Errorf("unexpected key %q", got)
	}
}
