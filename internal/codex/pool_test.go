package codex

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClock is a settable clock for the pool's now seam.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testStart is the test clock's starting time.
var testStart = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func newTestPool(t *testing.T, cfgs ...AccountConfig) (*AccountPool, *testClock) {
	t.Helper()
	clock := &testClock{t: testStart}
	p := NewAccountPool(cfgs)
	p.now = clock.now
	return p, clock
}

func account(id string, priority int) AccountConfig {
	return AccountConfig{ID: id, Name: id, Token: "at-" + id, RefreshToken: "rt-" + id, Priority: priority, Enabled: true}
}

func TestSelectAccountRanking(t *testing.T) {
	p, _ := newTestPool(t, account("a", 2), account("b", 1), account("c", 1))

	got, err := p.SelectAccount("", nil)
	if err != nil || got.ID != "b" {
		t.Fatalf("SelectAccount = %q, %v; want b (priority 1, then id)", got.ID, err)
	}
	p.Acquire("b")
	if got, _ := p.SelectAccount("", nil); got.ID != "c" {
		t.Errorf("with b busy: %q, want c (fewer in flight)", got.ID)
	}
	p.Release("b")
	p.RecordFailure("b", false, time.Minute)
	if got, _ := p.SelectAccount("", nil); got.ID != "c" {
		t.Errorf("with b failing: %q, want c (fewer consecutive failures)", got.ID)
	}
	p.RecordSuccess("b")
	if got, _ := p.SelectAccount("", map[string]bool{"b": true, "c": true}); got.ID != "a" {
		t.Errorf("with b and c excluded: %q, want a", got.ID)
	}
	if _, err := p.SelectAccount("", map[string]bool{"a": true, "b": true, "c": true}); !errors.Is(err, ErrNoAvailableAccounts) {
		t.Errorf("all excluded: err = %v, want ErrNoAvailableAccounts", err)
	}
}

func TestSelectAccountSticky(t *testing.T) {
	p, clock := newTestPool(t, account("a", 1), account("b", 1))

	first, _ := p.SelectAccount("session-1", map[string]bool{"a": true})
	if first.ID != "b" {
		t.Fatalf("first pick = %q, want b", first.ID)
	}
	if got, _ := p.SelectAccount("session-1", nil); got.ID != "b" {
		t.Errorf("sticky pick = %q, want b although a ranks first", got.ID)
	}
	p.RecordCooldown("b", clock.now().Add(time.Hour), "usage_limit_reached")
	if got, _ := p.SelectAccount("session-1", nil); got.ID != "a" {
		t.Errorf("sticky account in cooldown: %q, want a", got.ID)
	}
	if got, _ := p.SelectAccount("session-1", nil); got.ID != "a" {
		t.Errorf("session moved: %q, want a to stay", got.ID)
	}
}

func TestSelectAccountSkipsUnusableAccounts(t *testing.T) {
	disabled := account("disabled", 0)
	disabled.Enabled = false
	empty := AccountConfig{ID: "empty", Enabled: true}
	refreshOnly := AccountConfig{ID: "refresh-only", RefreshToken: "rt", Enabled: true, Priority: 5}
	p, _ := newTestPool(t, disabled, empty, refreshOnly)

	got, err := p.SelectAccount("", nil)
	if err != nil || got.ID != "refresh-only" {
		t.Errorf("SelectAccount = %q, %v; want refresh-only (EnsureFresh can mint its token)", got.ID, err)
	}
}

func TestCooldownRules(t *testing.T) {
	p, clock := newTestPool(t, account("a", 1))

	p.RecordFailure("a", false, time.Minute)
	p.RecordFailure("a", false, time.Minute)
	if snap := p.Snapshots()[0]; snap.Status != "healthy" {
		t.Fatalf("after two failures: status %q, want healthy", snap.Status)
	}
	p.RecordFailure("a", false, time.Minute)
	snap := p.Snapshots()[0]
	if snap.Status != "cooldown" || snap.CooldownReason != "consecutive_failures" || snap.ConsecutiveFailures != 3 {
		t.Fatalf("after three failures: %+v", snap)
	}

	until := clock.now().Add(3 * time.Hour)
	p.RecordCooldown("a", until, "usage_limit_reached")
	p.RecordFailure("a", true, time.Minute) // a 5xx must not cut the longer cooldown short
	p.RecordCooldown("a", clock.now().Add(time.Minute), "retry_after")
	snap = p.Snapshots()[0]
	if !snap.CooldownUntil.Equal(until) || snap.CooldownReason != "usage_limit_reached" {
		t.Errorf("cooldown = %v %q, want the longer usage-limit cooldown kept", snap.CooldownUntil, snap.CooldownReason)
	}
	if _, err := p.SelectAccount("", nil); !errors.Is(err, ErrNoAvailableAccounts) {
		t.Errorf("account in cooldown was selected (err %v)", err)
	}

	clock.advance(3*time.Hour + time.Second)
	snap = p.Snapshots()[0]
	if snap.Status != "healthy" || !snap.CooldownUntil.IsZero() || snap.CooldownReason != "" {
		t.Errorf("after the cooldown: %+v, want healthy with no cooldown shown", snap)
	}
	if got, err := p.SelectAccount("", nil); err != nil || got.ID != "a" {
		t.Errorf("after the cooldown: %q, %v", got.ID, err)
	}
}

func TestEnsureFreshKeepsATokenWithTimeLeft(t *testing.T) {
	cfg := account("a", 1)
	cfg.ExpiresAt = testStart.Add(RefreshLead + time.Hour).Format(time.RFC3339)
	p, _ := newTestPool(t, cfg)
	p.SetTokenRefresher(func(context.Context, string) (TokenSet, error) {
		t.Error("refresher called for a token with more than RefreshLead left")
		return TokenSet{}, nil
	})
	got, err := p.EnsureFresh(context.Background(), "a")
	if err != nil || got.Token != "at-a" {
		t.Errorf("EnsureFresh = %q, %v", got.Token, err)
	}
	if _, err := p.EnsureFresh(context.Background(), "missing"); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("unknown account: err = %v", err)
	}
}

func TestEnsureFreshSingleFlightPersistsBeforeUse(t *testing.T) {
	cfg := account("a", 1)
	cfg.Email = "user@example.com"
	cfg.ExpiresAt = testStart.Add(time.Hour).Format(time.RFC3339) // inside RefreshLead
	p, _ := newTestPool(t, cfg)

	var calls atomic.Int32
	release := make(chan struct{})
	newExpiry := testStart.Add(10 * 24 * time.Hour)
	p.SetTokenRefresher(func(_ context.Context, rt string) (TokenSet, error) {
		calls.Add(1)
		if rt != "rt-a" {
			t.Errorf("refresher got %q, want rt-a", rt)
		}
		<-release
		return TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: newExpiry}, nil
	})
	var persisted TokenSet
	p.SetTokenPersister(func(id string, ts TokenSet) error {
		if acc, _ := p.GetAccount(id); acc.Token != "at-a" {
			t.Errorf("pool already serves %q while persisting; the new token must wait for the save", acc.Token)
		}
		persisted = ts
		return nil
	})

	const callers = 8
	var wg sync.WaitGroup
	results := make([]Account, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = p.EnsureFresh(context.Background(), "a")
		}()
	}
	// Let every caller reach the refresh before it completes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("refresher called %d times, want 1: a refresh token is single-use", n)
	}
	for i := range callers {
		if errs[i] != nil || results[i].Token != "at-new" {
			t.Errorf("caller %d: %q, %v", i, results[i].Token, errs[i])
		}
	}
	want := TokenSet{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: newExpiry, Email: "user@example.com"}
	if persisted != want {
		t.Errorf("persisted %+v, want %+v (unknown fields kept from the account)", persisted, want)
	}
	if acc, _ := p.GetAccount("a"); acc.RefreshToken != "rt-new" || !acc.ExpiresAt.Equal(newExpiry) {
		t.Errorf("account after refresh: %+v", acc)
	}
}

func TestRefreshOutlivesACancelledCaller(t *testing.T) {
	p, _ := newTestPool(t, AccountConfig{ID: "a", RefreshToken: "rt-a", Enabled: true})
	release := make(chan struct{})
	saved := make(chan TokenSet, 1)
	p.SetTokenRefresher(func(ctx context.Context, _ string) (TokenSet, error) {
		<-release
		if err := ctx.Err(); err != nil {
			return TokenSet{}, err
		}
		return TokenSet{AccessToken: "at-new", RefreshToken: "rt-new"}, nil
	})
	p.SetTokenPersister(func(_ string, ts TokenSet) error {
		saved <- ts
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.ForceRefresh(ctx, "a")
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ForceRefresh err = %v, want the caller's cancellation", err)
	}
	close(release)
	select {
	case ts := <-saved:
		if ts.RefreshToken != "rt-new" {
			t.Errorf("saved %+v", ts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the rotated token was never saved after the caller left")
	}
	// The pool picks it up once the refresh settles.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if acc, _ := p.GetAccount("a"); acc.Token == "at-new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refreshed token never reached the pool")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTerminalRefreshErrorNeedsLogin(t *testing.T) {
	for _, msg := range []string{
		`token refresh failed (status 400): {"error":"invalid_grant"}`,
		`token refresh failed (status 400): {"error":{"code":"refresh_token_reused"}}`,
		`token refresh failed (status 401): {}`,
	} {
		t.Run(msg, func(t *testing.T) {
			p, _ := newTestPool(t, account("a", 1), account("b", 2))
			var calls atomic.Int32
			p.SetTokenRefresher(func(context.Context, string) (TokenSet, error) {
				calls.Add(1)
				return TokenSet{}, errors.New(msg)
			})
			_, err := p.ForceRefresh(context.Background(), "a")
			if !errors.Is(err, ErrNeedsLogin) {
				t.Fatalf("ForceRefresh err = %v, want ErrNeedsLogin", err)
			}
			snap := p.Snapshots()[0]
			if snap.ID != "a" || snap.Status != "needs_login" || snap.LastError == "" {
				t.Errorf("snapshot = %+v", snap)
			}
			if got, _ := p.SelectAccount("", nil); got.ID != "b" {
				t.Errorf("SelectAccount = %q, want the account that needs a login skipped", got.ID)
			}
			if _, err := p.EnsureFresh(context.Background(), "a"); !errors.Is(err, ErrNeedsLogin) {
				t.Errorf("EnsureFresh err = %v", err)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("refresher called %d times; a dead grant is not retried by EnsureFresh", n)
			}
		})
	}
}

func TestTransientRefreshErrorKeepsTheCurrentToken(t *testing.T) {
	cfg := account("a", 1)
	cfg.ExpiresAt = testStart.Add(6 * time.Hour).Format(time.RFC3339)
	p, _ := newTestPool(t, cfg)
	p.SetTokenRefresher(func(context.Context, string) (TokenSet, error) {
		return TokenSet{}, fmt.Errorf("token refresh failed (status 503): upstream busy")
	})

	got, err := p.EnsureFresh(context.Background(), "a")
	if err != nil || got.Token != "at-a" {
		t.Errorf("EnsureFresh = %q, %v; want the unexpired token kept", got.Token, err)
	}
	if _, err := p.ForceRefresh(context.Background(), "a"); err == nil || errors.Is(err, ErrNeedsLogin) {
		t.Errorf("ForceRefresh err = %v, want the transient error", err)
	}
	snap := p.Snapshots()[0]
	if snap.Status != "healthy" || snap.LastError == "" {
		t.Errorf("snapshot = %+v, want healthy with the error recorded", snap)
	}
}

func TestPersistFailureKeepsTheNewToken(t *testing.T) {
	p, _ := newTestPool(t, AccountConfig{ID: "a", RefreshToken: "rt-a", Enabled: true})
	p.SetTokenRefresher(func(context.Context, string) (TokenSet, error) {
		return TokenSet{AccessToken: "at-new", RefreshToken: "rt-new"}, nil
	})
	p.SetTokenPersister(func(string, TokenSet) error { return errors.New("disk full") })

	got, err := p.EnsureFresh(context.Background(), "a")
	if err != nil || got.Token != "at-new" || got.RefreshToken != "rt-new" {
		t.Errorf("EnsureFresh = %+v, %v; the spent refresh token's successor must not be dropped", got, err)
	}
	if snap := p.Snapshots()[0]; snap.LastError == "" {
		t.Error("the failed save is not shown on the account")
	}
}

func TestInherit(t *testing.T) {
	old, clock := newTestPool(t, account("a", 1), account("b", 1), account("gone", 1))
	old.Acquire("a")
	old.RecordFailure("a", true, time.Hour)
	old.UpdateQuota("a", Quota{PlanType: "plus", ObservedAt: clock.now()})
	old.SetTokenRefresher(func(context.Context, string) (TokenSet, error) {
		return TokenSet{}, errors.New("invalid_grant")
	})
	_, _ = old.ForceRefresh(context.Background(), "b")

	// b logs in again: the config now holds a new refresh token.
	relogged := account("b", 1)
	relogged.RefreshToken = "rt-b-2"
	p := NewAccountPool([]AccountConfig{account("a", 1), relogged})
	p.now = clock.now
	p.Inherit(old)

	snaps := p.Snapshots()
	a, b := snaps[0], snaps[1]
	if a.Status != "cooldown" || a.TotalRequests != 1 || a.ConsecutiveFailures != 1 || a.Quota == nil || a.Quota.PlanType != "plus" {
		t.Errorf("a = %+v, want cooldown, counters and quota carried", a)
	}
	if a.InFlight != 0 {
		t.Errorf("a in flight = %d; in-flight counts stay with the old pool", a.InFlight)
	}
	if b.Status != "healthy" || b.LastError != "" {
		t.Errorf("b = %+v, want a new login to clear needs_login", b)
	}

	// Without a new login the needs-login mark survives the rebuild.
	same := NewAccountPool([]AccountConfig{account("b", 1)})
	same.Inherit(old)
	if s := same.Snapshots()[0]; s.Status != "needs_login" {
		t.Errorf("unchanged grant: status %q, want needs_login", s.Status)
	}
}

func TestInheritJoinsARefreshInProgress(t *testing.T) {
	old, _ := newTestPool(t, AccountConfig{ID: "a", RefreshToken: "rt-a", Enabled: true})
	release := make(chan struct{})
	var calls atomic.Int32
	refresher := func(context.Context, string) (TokenSet, error) {
		calls.Add(1)
		<-release
		return TokenSet{AccessToken: "at-new", RefreshToken: "rt-new"}, nil
	}
	old.SetTokenRefresher(refresher)
	go func() { _, _ = old.ForceRefresh(context.Background(), "a") }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	p := NewAccountPool([]AccountConfig{{ID: "a", RefreshToken: "rt-a", Enabled: true}})
	p.SetTokenRefresher(refresher)
	p.Inherit(old)
	close(release)

	got, err := p.EnsureFresh(context.Background(), "a")
	if err != nil || got.Token != "at-new" || got.RefreshToken != "rt-new" {
		t.Errorf("EnsureFresh on the new pool = %+v, %v", got, err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("refresher called %d times; the new pool must not spend rt-a again", n)
	}
}
