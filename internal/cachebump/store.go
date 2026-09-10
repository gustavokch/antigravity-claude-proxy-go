package cachebump

import (
	"sync"
	"time"
)

// defaultMaxBytes is the total replay-body budget a Store holds when the
// caller does not set one. A recorded body is a whole conversation, so the
// record count alone is a poor bound: 200 long Claude Code turns can be
// hundreds of megabytes.
const defaultMaxBytes = 64 << 20

// Store holds recorded sessions in memory. It mirrors the shape of
// openrouter.SessionTracker: prune expired entries on insert, evict the
// oldest entry when an insert would exceed capacity. Capacity is both a
// record count and a total byte budget over the recorded bodies. A restart
// drops everything — after a restart gap the upstream cache is cold anyway.
type Store struct {
	mu         sync.RWMutex
	records    map[string]*Record
	ttl        time.Duration
	maxEntries int
	maxBytes   int
	bytes      int
}

// NewStore creates a Store with the default byte budget. Non-positive ttl or
// maxEntries fall back to sane defaults (24h, 200 entries).
func NewStore(ttl time.Duration, maxEntries int) *Store {
	return NewStoreWithLimits(ttl, maxEntries, defaultMaxBytes)
}

// NewStoreWithLimits creates a Store with an explicit total byte budget over
// the recorded bodies. Non-positive values fall back to the defaults (24h,
// 200 entries, 64 MiB).
func NewStoreWithLimits(ttl time.Duration, maxEntries, maxBytes int) *Store {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = 200
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	return &Store{
		records:    make(map[string]*Record),
		ttl:        ttl,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

// Upsert records a fresh body for a session. It re-arms a stopped record:
// fresh body, Stopped=false, counters reset. Expired entries are pruned, and
// the oldest entries are evicted until the insert fits both the record count
// and the byte budget. A single body larger than the whole budget is refused.
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

	size := len(rec.Body)
	if size > s.maxBytes {
		// Nothing can be evicted to make this fit; recording it would only
		// starve every other session. The record already held for this
		// session — if any — is still good, so it stays.
		return
	}

	if len(s.records) > 0 {
		s.pruneLocked(now)
	}
	// The record it replaces, if any, releases its bytes and its slot first.
	s.dropLocked(rec.Key)
	if len(s.records) >= s.maxEntries {
		s.evictOldestLocked()
	}
	for s.bytes+size > s.maxBytes && len(s.records) > 0 {
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
	s.bytes += size
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
// until the TTL prune. The replay body is released: a stopped session is
// never replayed, so holding its body would only pin byte budget that live
// sessions need. A later Upsert re-arm supplies a fresh body.
func (s *Store) Stop(key string, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return
	}
	rec.Stopped = true
	rec.StopReason = reason
	s.bytes -= len(rec.Body)
	rec.Body = nil
	rec.Headers = nil
}

// Clear removes every record.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = make(map[string]*Record)
	s.bytes = 0
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

// Bytes returns the total size of the recorded bodies currently held.
func (s *Store) Bytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bytes
}

// MaxBytes returns the store's total body budget.
func (s *Store) MaxBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxBytes
}

// dropLocked removes one record and releases its bytes. It is a no-op when
// the key is absent.
func (s *Store) dropLocked(key string) {
	rec, ok := s.records[key]
	if !ok {
		return
	}
	s.bytes -= len(rec.Body)
	delete(s.records, key)
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-s.ttl)
	for id, rec := range s.records {
		if rec.LastSeen.Before(cutoff) {
			s.bytes -= len(rec.Body)
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
	s.dropLocked(oldestKey)
}
