package cachebump

import (
	"sync"
	"time"
)

// Store holds recorded sessions in memory. It mirrors the shape of
// openrouter.SessionTracker: prune expired entries on insert, evict the
// oldest entry when an insert would exceed capacity. A restart drops
// everything — after a restart gap the upstream cache is cold anyway.
type Store struct {
	mu         sync.RWMutex
	records    map[string]*Record
	ttl        time.Duration
	maxEntries int
}

// NewStore creates a Store. Non-positive ttl or maxEntries fall back to
// sane defaults (24h, 200 entries).
func NewStore(ttl time.Duration, maxEntries int) *Store {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = 200
	}
	return &Store{
		records:    make(map[string]*Record),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

// Upsert records a fresh body for a session. It re-arms a stopped record:
// fresh body, Stopped=false, counters reset. Expired entries are pruned and
// the oldest entry is evicted when the store is over capacity.
func (s *Store) Upsert(rec Record) {
	if rec.Key == "" {
		rec.Key = RecordKey(rec.Route, rec.SessionID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := rec.LastSeen
	if now.IsZero() {
		now = time.Now()
	}
	rec.LastSeen = now

	if len(s.records) > 0 {
		s.pruneLocked(now)
	}
	// Only an insert grows the store; re-arming an existing session must not
	// cost another live session its slot.
	if _, exists := s.records[rec.Key]; !exists && len(s.records) >= s.maxEntries {
		s.evictOldestLocked()
	}

	// A new real client request fully re-arms the record.
	rec.Bumps = 0
	rec.Retries = 0
	rec.Stopped = false
	rec.StopReason = ""
	rec.LastCacheReadTokens = 0
	rec.LastCacheCreationTokens = 0

	s.records[rec.Key] = &rec
}

// Get returns a copy of the record for a key, if present.
func (s *Store) Get(key string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[key]
	if !ok {
		return Record{}, false
	}
	return *rec, true
}

// Due returns records whose NextBump has arrived and that are not stopped.
func (s *Store) Due(now time.Time) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var due []Record
	for _, rec := range s.records {
		if rec.Stopped || rec.NextBump.IsZero() || rec.NextBump.After(now) {
			continue
		}
		due = append(due, *rec)
	}
	return due
}

// MarkBumped records a completed bump: counters, token usage, next schedule.
func (s *Store) MarkBumped(key string, next time.Time, cacheRead, cacheCreation int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return
	}
	rec.Bumps++
	rec.Retries = 0
	rec.LastCacheReadTokens = cacheRead
	rec.LastCacheCreationTokens = cacheCreation
	if !next.IsZero() {
		rec.NextBump = next
	}
}

// ScheduleRetry schedules the single retry attempt after a transient failure.
func (s *Store) ScheduleRetry(key string, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return
	}
	rec.Retries++
	rec.NextBump = next
}

// Stop marks a record stopped with a reason. It stays visible in the store
// until the TTL prune.
func (s *Store) Stop(key string, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return
	}
	rec.Stopped = true
	rec.StopReason = reason
}

// Clear removes every record.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = make(map[string]*Record)
}

// Snapshot returns copies of all records, stopped ones included.
func (s *Store) Snapshot() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, *rec)
	}
	return out
}

// Len returns the number of records currently held.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-s.ttl)
	for id, rec := range s.records {
		if rec.LastSeen.Before(cutoff) {
			delete(s.records, id)
		}
	}
}

func (s *Store) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	for id, rec := range s.records {
		if oldestKey == "" || rec.LastSeen.Before(oldestTime) {
			oldestKey = id
			oldestTime = rec.LastSeen
		}
	}
	if oldestKey != "" {
		delete(s.records, oldestKey)
	}
}
