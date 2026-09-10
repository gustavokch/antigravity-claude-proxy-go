package cachebump

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func newTestRecord(now time.Time, ttl time.Duration, lead int) Record {
	rec := Record{
		Key:       RecordKey(RouteClaudeCode, "s1"),
		SessionID: "s1",
		Route:     RouteClaudeCode,
		Model:     "claude-sonnet-5",
		Body:      []byte(`{"messages":[]}`),
		TTL:       ttl,
		LastSeen:  now,
		NextBump:  now, // due immediately
	}
	return rec
}

func newTestScheduler(store *Store, sender Sender, lead, maxBumps, maxIdle int) *Scheduler {
	return NewScheduler(store, sender, SchedulerConfig{
		LeadSeconds:        lead,
		MaxBumpsPerSession: maxBumps,
		MaxIdleMinutes:     maxIdle,
	})
}

func TestScheduler_FiresDueBumpAndReschedules(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	var gotRec Record
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		gotRec = rec
		return BumpResult{CacheReadTokens: 12000, CacheCreationTokens: 0}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	if gotRec.SessionID != "s1" {
		t.Errorf("expected sender to receive record s1, got %+v", gotRec)
	}
	rec, ok := store.Get(gotRec.Key)
	if !ok {
		t.Fatal("record missing")
	}
	if rec.Bumps != 1 {
		t.Errorf("expected Bumps 1, got %d", rec.Bumps)
	}
	if rec.Stopped {
		t.Error("expected record still active")
	}
	// Next bump fires one full TTL after this bump.
	want := now.Add(5 * time.Minute).Add(-60 * time.Second)
	if !rec.NextBump.Equal(want) {
		t.Errorf("expected NextBump %v, got %v", want, rec.NextBump)
	}
	stats := sched.StatsSnapshot()
	if stats.TotalBumps != 1 || stats.CacheReadTokensRefreshed != 12000 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}

func TestScheduler_ZeroLeadStillSchedulesBeforeExpiry(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 0))

	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{CacheReadTokens: 100}, nil
	}
	sched := newTestScheduler(store, sender, 0, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	rec, ok := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !ok {
		t.Fatal("record missing")
	}
	// A non-positive lead must fall back to the TTL/5 clamp, the same rule
	// the recorder uses. Scheduling at now+TTL would land exactly on expiry
	// and guarantee a paid write.
	want := NextBumpTime(now, 5*time.Minute, 0)
	if !rec.NextBump.Equal(want) {
		t.Errorf("expected NextBump %v, got %v", want, rec.NextBump)
	}
	if !rec.NextBump.Before(now.Add(5 * time.Minute)) {
		t.Errorf("NextBump %v is not before the cache expiry %v", rec.NextBump, now.Add(5*time.Minute))
	}
}

func TestScheduler_ReconfigureChangesReschedule(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, time.Hour, 60))

	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{CacheReadTokens: 100}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 0)
	sched.Now = func() time.Time { return now }

	// The operator edits the config; the running scheduler must pick it up
	// instead of rescheduling with the value captured at construction.
	sched.Reconfigure(SchedulerConfig{LeadSeconds: 600})
	sched.Tick(context.Background())

	rec, ok := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !ok {
		t.Fatal("record missing")
	}
	// 1h TTL, lead 600s (within the TTL/5 clamp of 12m): bump+50m.
	want := now.Add(time.Hour - 600*time.Second)
	if !rec.NextBump.Equal(want) {
		t.Errorf("expected NextBump %v after reconfigure, got %v", want, rec.NextBump)
	}
}

func TestScheduler_ReconfigureConcurrentWithTick(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, time.Hour, 60))

	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{CacheReadTokens: 1}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			sched.Reconfigure(SchedulerConfig{LeadSeconds: 60 + i, MaxIdleMinutes: 240})
		}
	}()
	for i := 0; i < 200; i++ {
		store.Upsert(newTestRecord(now, time.Hour, 60))
		sched.Tick(context.Background())
	}
	<-done
}

func TestScheduler_SenderReceivesBodyAndAllowlistedHeaders(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)

	rec := newTestRecord(now, 5*time.Minute, 60)
	hdr := http.Header{}
	hdr.Set("anthropic-version", "2023-06-01")
	hdr.Set("anthropic-beta", "extended-cache-ttl-2025-04-11")
	hdr.Set("authorization", "Bearer secret") // not allowlisted
	rec.Headers = hdr
	store.Upsert(rec)

	var gotHdr http.Header
	var gotBody []byte
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		gotHdr = rec.Headers
		gotBody = rec.Body
		return BumpResult{CacheReadTokens: 1}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	if !bytesEqual(gotBody, rec.Body) {
		t.Errorf("expected replay body passed through, got %s", gotBody)
	}
	if gotHdr.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("expected anthropic-version header, got %v", gotHdr)
	}
	if gotHdr.Get("authorization") != "" {
		t.Error("expected non-allowlisted header stripped")
	}
}

func TestScheduler_PaidWriteStops(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{CacheReadTokens: 0, CacheCreationTokens: 9000}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !rec.Stopped || rec.StopReason != "paid_write" {
		t.Errorf("expected paid_write stop, got %+v", rec)
	}
	if sched.StatsSnapshot().StopsByReason["paid_write"] != 1 {
		t.Error("expected stop counted in stats")
	}
}

func TestScheduler_UpstreamRejected4xxStops(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{}, &UpstreamError{Status: 400}
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !rec.Stopped || rec.StopReason != "upstream_rejected" {
		t.Errorf("expected upstream_rejected stop, got %+v", rec)
	}
}

func TestScheduler_429RetriesOnceThenStops(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	attempts := 0
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		attempts++
		return BumpResult{}, &UpstreamError{Status: 429}
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }

	// First tick: retry scheduled 30s out, record not stopped.
	sched.Tick(context.Background())
	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if rec.Stopped {
		t.Fatalf("expected retry after first 429, got %+v", rec)
	}
	if want := now.Add(30 * time.Second); !rec.NextBump.Equal(want) {
		t.Errorf("expected NextBump %v, got %v", want, rec.NextBump)
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt, got %d", attempts)
	}

	// Second attempt still fails: stop.
	now = now.Add(31 * time.Second)
	sched.Tick(context.Background())
	rec, _ = store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !rec.Stopped || rec.StopReason != "upstream_unavailable" {
		t.Errorf("expected upstream_unavailable stop, got %+v", rec)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestScheduler_5xxRecoversAfterRetry(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	attempts := 0
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		attempts++
		if attempts == 1 {
			return BumpResult{}, &UpstreamError{Status: 503}
		}
		return BumpResult{CacheReadTokens: 500}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	now = now.Add(31 * time.Second)
	sched.Tick(context.Background())

	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if rec.Stopped {
		t.Errorf("expected recovery, got %+v", rec)
	}
	if rec.Bumps != 1 || rec.Retries != 0 {
		t.Errorf("expected Bumps 1 Retries 0, got %+v", rec)
	}
}

func TestScheduler_NetworkErrorRetriesLike5xx(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	attempts := 0
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		attempts++
		return BumpResult{}, errors.New("connection refused")
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if rec.Stopped {
		t.Fatal("expected retry after network error")
	}

	now = now.Add(31 * time.Second)
	sched.Tick(context.Background())
	rec, _ = store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !rec.Stopped || rec.StopReason != "upstream_unavailable" {
		t.Errorf("expected upstream_unavailable stop, got %+v", rec)
	}
}

func TestScheduler_BumpCapStops(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{CacheReadTokens: 100}, nil
	}
	sched := newTestScheduler(store, sender, 60, 2, 240)

	now = now.Add(1 * time.Minute)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())
	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if rec.Stopped || rec.Bumps != 1 {
		t.Fatalf("expected first bump active, got %+v", rec)
	}

	now = now.Add(5 * time.Minute)
	sched.Tick(context.Background())
	rec, _ = store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !rec.Stopped || rec.StopReason != "bump_cap" {
		t.Errorf("expected bump_cap stop after 2 bumps, got %+v", rec)
	}
}

func TestScheduler_IdleStops(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	fired := false
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		fired = true
		return BumpResult{CacheReadTokens: 100}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)

	// 5 hours later: past MaxIdleMinutes, no bump fires.
	now = now.Add(5 * time.Hour)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	if fired {
		t.Error("expected no bump for idle session")
	}
	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !rec.Stopped || rec.StopReason != "idle" {
		t.Errorf("expected idle stop, got %+v", rec)
	}
}

func TestScheduler_AccountUnavailableStops(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{}, ErrAccountUnavailable
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	rec, _ := store.Get(RecordKey(RouteClaudeCode, "s1"))
	if !rec.Stopped || rec.StopReason != "account_unavailable" {
		t.Errorf("expected account_unavailable stop, got %+v", rec)
	}
}

func TestScheduler_SkipsStoppedRecords(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	rec := newTestRecord(now, 5*time.Minute, 60)
	store.Upsert(rec)
	store.Stop(rec.Key, "paid_write")

	called := false
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		called = true
		return BumpResult{CacheReadTokens: 100}, nil
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	if called {
		t.Error("expected stopped record skipped")
	}
}

func TestScheduler_OnEventReportsBumpsAndStops(t *testing.T) {
	now := time.Now()
	store := NewStore(time.Hour, 100)
	store.Upsert(newTestRecord(now, 5*time.Minute, 60))

	var events []string
	sender := func(ctx context.Context, rec Record) (BumpResult, error) {
		return BumpResult{}, &UpstreamError{Status: 400}
	}
	sched := newTestScheduler(store, sender, 60, 0, 240)
	sched.OnEvent = func(rec Record, outcome string) {
		events = append(events, outcome)
	}
	sched.Now = func() time.Time { return now }
	sched.Tick(context.Background())

	if len(events) != 1 || events[0] != "upstream_rejected" {
		t.Errorf("expected stop event, got %v", events)
	}
}

func TestUpstreamErrorClassification(t *testing.T) {
	var e error = &UpstreamError{Status: 400}
	if !IsUpstreamRejected(e) || IsUpstreamUnavailable(e) {
		t.Error("400 should be rejected, not unavailable")
	}
	e = &UpstreamError{Status: 429}
	if IsUpstreamRejected(e) || !IsUpstreamUnavailable(e) {
		t.Error("429 should be unavailable, not rejected")
	}
	e = &UpstreamError{Status: 503}
	if !IsUpstreamUnavailable(e) {
		t.Error("503 should be unavailable")
	}
	e = errors.New("boom")
	if IsUpstreamRejected(e) || IsUpstreamUnavailable(e) {
		t.Error("plain errors are neither")
	}
}

func TestAllowlistHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("anthropic-version", "2023-06-01")
	in.Set("anthropic-beta", "beta-1,beta-2")
	in.Set("x-api-key", "secret")
	in.Set("cookie", "session=abc")

	out := AllowlistHeaders(in)
	if out.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("expected anthropic-version kept, got %v", out)
	}
	if out.Get("anthropic-beta") != "beta-1,beta-2" {
		t.Errorf("expected anthropic-beta kept, got %v", out)
	}
	if out.Get("x-api-key") != "" || out.Get("cookie") != "" {
		t.Errorf("expected auth/cookie stripped, got %v", out)
	}
	if in.Get("x-api-key") != "secret" {
		t.Error("expected input header set unmutated")
	}
}
