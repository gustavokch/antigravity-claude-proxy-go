package cachebump

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Sender fires one bump for a record against the upstream that owns the
// cache entry. Implementations must pin the request to the same
// account/endpoint that served the original turn.
type Sender func(ctx context.Context, rec Record) (BumpResult, error)

// BumpResult carries the cache accounting from a bump response.
type BumpResult struct {
	CacheReadTokens     int
	CacheCreationTokens int
}

// UpstreamError classifies an upstream HTTP status from a bump attempt.
type UpstreamError struct{ Status int }

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("cachebump: upstream status %d", e.Status)
}

// IsUpstreamRejected reports a definitive 4xx rejection (other than 429):
// bumping again will not help.
func IsUpstreamRejected(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status >= 400 && ue.Status < 500 && ue.Status != http.StatusTooManyRequests
	}
	return false
}

// IsUpstreamUnavailable reports a transient failure (429, 5xx) worth one
// retry. Plain (network) errors are handled by the scheduler's default
// branch, which also retries once.
func IsUpstreamUnavailable(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == http.StatusTooManyRequests || ue.Status >= 500
	}
	return false
}

// ErrAccountUnavailable is returned by a Sender when the recorded account
// (Claude Code) is disabled, cooling, or missing.
var ErrAccountUnavailable = errors.New("cachebump: account unavailable")

// SchedulerConfig carries the knobs the scheduler applies on top of the store.
type SchedulerConfig struct {
	// LeadSeconds is how long before TTL expiry a bump fires; clamped to
	// TTL/5 (see NextBumpTime).
	LeadSeconds int
	// MaxBumpsPerSession caps bumps between real client turns; 0 disables
	// the cap.
	MaxBumpsPerSession int
	// MaxIdleMinutes stops bumping a session with no real client activity
	// for this long; 0 disables the idle check.
	MaxIdleMinutes int
}

// Stats aggregates bump outcomes for the management API.
type Stats struct {
	TotalBumps               int64            `json:"total_bumps"`
	CacheReadTokensRefreshed int64            `json:"cache_read_tokens_refreshed"`
	StopsByReason            map[string]int64 `json:"stops_by_reason"`
}

// Scheduler fires due bumps through the Sender and applies the stop rules.
type Scheduler struct {
	store  *Store
	sender Sender
	cfg    SchedulerConfig

	// Now is injected for tests; defaults to time.Now.
	Now func() time.Time

	// OnEvent, when set, receives every bump outcome: a successful bump
	// ("bump") or the stop reason that ended a session's bumping.
	OnEvent func(rec Record, outcome string)

	mu    sync.Mutex
	busy  bool
	stats Stats

	// RetryAfter is how long a transient failure waits before its single
	// retry. Exposed for tests.
	RetryAfter time.Duration
}

// NewScheduler builds a Scheduler over the given store and sender.
func NewScheduler(store *Store, sender Sender, cfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		store:      store,
		sender:     sender,
		cfg:        cfg,
		Now:        time.Now,
		RetryAfter: 30 * time.Second,
		stats:      Stats{StopsByReason: make(map[string]int64)},
	}
}

// Run drives Tick on a ticker until ctx is done. Ticks never overlap: if a
// pass is still running (a slow upstream), the next tick is skipped.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick runs one scheduling pass: collect due records, fire each through the
// Sender, classify, reschedule or stop.
func (s *Scheduler) Tick(ctx context.Context) {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	now := s.Now()
	for _, rec := range s.store.Due(now) {
		s.bump(ctx, rec)
	}
}

func (s *Scheduler) bump(ctx context.Context, rec Record) {
	now := s.Now()

	// Idle sessions are stopped before firing — a bump for a session the
	// client abandoned is wasted traffic.
	if s.cfg.MaxIdleMinutes > 0 && now.Sub(rec.LastSeen) > time.Duration(s.cfg.MaxIdleMinutes)*time.Minute {
		s.stop(rec.Key, "idle")
		return
	}

	// Only allowlisted headers ever reach upstream, regardless of what the
	// recorder captured.
	rec.Headers = AllowlistHeaders(rec.Headers)

	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := s.sender(sendCtx, rec)

	if err != nil {
		switch {
		case errors.Is(err, ErrAccountUnavailable):
			s.stop(rec.Key, "account_unavailable")
		case IsUpstreamRejected(err):
			s.stop(rec.Key, "upstream_rejected")
		default:
			// 429/5xx/network: one retry after a short wait, then give up.
			if rec.Retries >= 1 {
				s.stop(rec.Key, "upstream_unavailable")
			} else {
				s.store.ScheduleRetry(rec.Key, now.Add(s.RetryAfter))
			}
		}
		return
	}

	// The entry was already gone: this bump paid a cache write, so
	// continuing to bump is a cost, not a saving.
	if result.CacheCreationTokens > 0 && result.CacheReadTokens == 0 {
		s.store.MarkBumped(rec.Key, time.Time{}, result.CacheReadTokens, result.CacheCreationTokens)
		s.stop(rec.Key, "paid_write")
		return
	}

	// NextBumpTime clamps a non-positive or oversized lead to TTL/5, so the
	// next bump always lands strictly before the entry expires — the same
	// rule the recorder used for the first bump.
	next := NextBumpTime(now, rec.TTL, s.cfg.LeadSeconds)
	s.store.MarkBumped(rec.Key, next, result.CacheReadTokens, result.CacheCreationTokens)

	s.mu.Lock()
	s.stats.TotalBumps++
	s.stats.CacheReadTokensRefreshed += int64(result.CacheReadTokens)
	s.mu.Unlock()

	var fired Record
	if updated, ok := s.store.Get(rec.Key); ok {
		fired = updated
		if s.cfg.MaxBumpsPerSession > 0 && updated.Bumps >= s.cfg.MaxBumpsPerSession {
			s.stop(rec.Key, "bump_cap")
			return
		}
	} else {
		fired = rec
	}
	s.emit(fired, "bump")
}

func (s *Scheduler) emit(rec Record, outcome string) {
	if s.OnEvent != nil {
		s.OnEvent(rec, outcome)
	}
}

// StopSession stops bumping one record and accounts the stop in
// StopsByReason. Management handlers stop through here so manual stops are
// counted like any other stop rule.
func (s *Scheduler) StopSession(key, reason string) {
	s.stop(key, reason)
}

func (s *Scheduler) stop(key, reason string) {
	s.store.Stop(key, reason)
	s.mu.Lock()
	s.stats.StopsByReason[reason]++
	s.mu.Unlock()
	if rec, ok := s.store.Get(key); ok {
		s.emit(rec, reason)
	}
}

// StatsSnapshot returns a copy of the aggregated stats.
func (s *Scheduler) StatsSnapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{
		TotalBumps:               s.stats.TotalBumps,
		CacheReadTokensRefreshed: s.stats.CacheReadTokensRefreshed,
		StopsByReason:            make(map[string]int64, len(s.stats.StopsByReason)),
	}
	for k, v := range s.stats.StopsByReason {
		out.StopsByReason[k] = v
	}
	return out
}
