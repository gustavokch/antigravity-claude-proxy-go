package accounts

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"
)

const (
	pinModel   = "claude-sonnet-4-6"
	otherModel = "gemini-3.8-flash-high"
)

// pinTestManager builds a pool of testAccounts on a settable clock and
// returns the accounts in pool order.
func pinTestManager(t *testing.T, clock *time.Time, strategy string, emails ...string) (*Manager, []*Account) {
	t.Helper()
	pool := make([]*Account, 0, len(emails))
	for _, email := range emails {
		pool = append(pool, testAccount(email))
	}
	manager, err := New(Options{
		Accounts:             pool,
		Strategy:             strategy,
		SharedThrottleWindow: 10 * time.Second,
		Now:                  func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, pool
}

// selected returns the email Select picks, or "" when it picks no account.
func selected(manager *Manager, session, model string) string {
	if account := manager.Select(session, model).Account; account != nil {
		return account.Email
	}
	return ""
}

func TestPinTargetClassification(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		setup  func(t *testing.T, manager *Manager, a, b *Account)
		state  targetState
		wait   time.Duration
		reason string
	}{
		{name: "healthy", setup: func(*testing.T, *Manager, *Account, *Account) {}, state: targetUsable},
		{name: "limit on another model", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkRateLimited(a, otherModel, time.Hour)
		}, state: targetUsable},
		{name: "default bound is maxStickyWait", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkRateLimited(a, pinModel, maxStickyWait)
		}, state: targetWait, wait: maxStickyWait},
		{name: "rate limit of exactly pinWait", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.setPinWait(30 * time.Second)
			m.MarkRateLimited(a, pinModel, 30*time.Second)
		}, state: targetWait, wait: 30 * time.Second},
		{name: "rate limit 1ms past pinWait", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.setPinWait(30 * time.Second)
			m.MarkRateLimited(a, pinModel, 30*time.Second+time.Millisecond)
		}, state: targetBlocked, reason: "rate limited 30s"},
		{name: "failure lock lifting in a minute", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			for range 3 {
				m.MarkFailure(a, pinModel)
			}
		}, state: targetBlocked, reason: "failure lock"},
		{name: "404 lock", setup: func(t *testing.T, m *Manager, a, _ *Account) {
			if !m.MarkModelUnavailable(a, pinModel) {
				t.Fatal("MarkModelUnavailable = false with a spare account")
			}
		}, state: targetBlocked, reason: "rate limited 24h0m0s"},
		{name: "exhausted quota behind a short limit", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkQuotaExhausted(a.Email, pinModel, start.Add(90*time.Second).Format(time.RFC3339))
			m.MarkRateLimited(a, pinModel, 90*time.Second)
		}, state: targetBlocked, reason: "quota exhausted"},
		{name: "cooling down", setup: func(_ *testing.T, _ *Manager, a, _ *Account) {
			a.CoolingDownUntilMS = start.Add(time.Minute).UnixMilli()
		}, state: targetBlocked, reason: "cooling down"},
		{name: "short limit on a model the account does not list", setup: func(_ *testing.T, m *Manager, a, b *Account) {
			a.Quota.Models[otherModel] = ModelQuota{}
			b.Quota.Models[pinModel] = ModelQuota{}
			m.MarkRateLimited(a, pinModel, 30*time.Second)
		}, state: targetBlocked, reason: "model not listed"},
		{name: "invalid", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkInvalid(a, "test", "")
		}, state: targetGone, reason: "invalid"},
		{name: "disabled", setup: func(_ *testing.T, _ *Manager, a, _ *Account) {
			a.Enabled = false
		}, state: targetGone, reason: "disabled"},
		{name: "removed", setup: func(t *testing.T, m *Manager, a, _ *Account) {
			if err := m.RemoveAccount(a.Email); err != nil {
				t.Fatal(err)
			}
		}, state: targetGone, reason: "removed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")
			test.setup(t, manager, pool[0], pool[1])
			manager.mu.Lock()
			got := manager.pinTargetLocked("a@example.com", pinModel)
			manager.mu.Unlock()
			if got.state != test.state || got.wait != test.wait || got.reason != test.reason {
				t.Fatalf("pinTargetLocked = state %d wait %s reason %q, want state %d wait %s reason %q",
					got.state, got.wait, got.reason, test.state, test.wait, test.reason)
			}
		})
	}
}

// The Dispatcher hands its maxWait to setPinWait. The pin bound never grows
// past maxStickyWait, and a missing value means maxStickyWait.
func TestPinWaitIsClampedToMaxStickyWait(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com")
	for _, test := range []struct {
		set, want time.Duration
	}{
		{set: 30 * time.Second, want: 30 * time.Second},
		{set: maxStickyWait, want: maxStickyWait},
		{set: maxStickyWait + time.Millisecond, want: maxStickyWait},
		{set: 10 * time.Minute, want: maxStickyWait},
		{set: time.Millisecond, want: time.Millisecond},
		{set: 0, want: maxStickyWait},
		{set: -time.Second, want: maxStickyWait},
	} {
		manager.setPinWait(test.set)
		if got := manager.pinWait; got != test.want {
			t.Errorf("setPinWait(%s) stored %s, want %s", test.set, got, test.want)
		}
	}
}

func TestPinKeepsASessionOnOneAccount(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")

	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("new pin = %q, want round-robin's next account b@example.com", got)
	}
	for turn := range 3 {
		if got := selected(manager, "s1", pinModel); got != "b@example.com" {
			t.Fatalf("turn %d = %q, want the pinned b@example.com", turn+2, got)
		}
	}
	if got := selected(manager, "s1", otherModel); got != "b@example.com" {
		t.Fatalf("another model in the same session = %q, want the pinned b@example.com", got)
	}
	if got := selected(manager, "s2", pinModel); got != "c@example.com" {
		t.Fatalf("second session = %q, want c@example.com: round-robin rotates per new pin", got)
	}
	if first, second := selected(manager, "", pinModel), selected(manager, "", pinModel); first != "a@example.com" || second != "b@example.com" {
		t.Fatalf("requests without a session = %q, %q; want per-request rotation a, b", first, second)
	}
}

func TestPinUnderStickyTakesTheGlobalCurrentAccount(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategySticky, "a@example.com", "b@example.com")

	if s1, s2 := selected(manager, "s1", pinModel), selected(manager, "s2", pinModel); s1 != "a@example.com" || s2 != "a@example.com" {
		t.Fatalf("new pins under sticky = %q, %q; want the current account a@example.com for both", s1, s2)
	}
	manager.MarkRateLimited(pool[0], pinModel, 10*time.Minute)
	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("overflow under sticky = %q, want b@example.com", got)
	}
	if got := selected(manager, "s3", pinModel); got != "b@example.com" {
		t.Fatalf("new pin after sticky moved = %q, want the new current account b@example.com", got)
	}
}

func TestPinWaitsOutAShortLimit(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")
	home := pool[1]

	if got := selected(manager, "s1", pinModel); got != home.Email {
		t.Fatalf("new pin = %q, want %s", got, home.Email)
	}
	manager.MarkRateLimited(home, pinModel, 30*time.Second)
	if got := manager.Select("s1", pinModel); got != (Selection{Wait: 30 * time.Second}) {
		t.Fatalf("pinned session under a 30s limit got %+v, want a 30s wait for its own account", got)
	}
	if got := selected(manager, "", pinModel); got != "a@example.com" {
		t.Fatalf("request without a session = %q, want a@example.com", got)
	}
	clock = clock.Add(30 * time.Second)
	if got := selected(manager, "s1", pinModel); got != home.Email {
		t.Fatalf("after the limit lifted the session got %q, want its home %s", got, home.Email)
	}
	if overflow := manager.pins["s1"].overflow; len(overflow) != 0 {
		t.Fatalf("a short limit must not create an overflow entry: %v", overflow)
	}
}

func TestPinOverflowsAndDoesNotFlipBack(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	blockers := []struct {
		name  string
		block func(t *testing.T, manager *Manager, home *Account)
	}{
		{name: "rate limit past maxStickyWait", block: func(_ *testing.T, m *Manager, home *Account) {
			m.MarkRateLimited(home, pinModel, maxStickyWait+time.Millisecond)
		}},
		{name: "exhausted quota", block: func(_ *testing.T, m *Manager, home *Account) {
			m.MarkQuotaExhausted(home.Email, pinModel, start.Add(90*time.Second).Format(time.RFC3339))
			m.MarkRateLimited(home, pinModel, 90*time.Second)
		}},
		{name: "failure lock", block: func(_ *testing.T, m *Manager, home *Account) {
			for range 3 {
				m.MarkFailure(home, pinModel)
			}
		}},
		{name: "404 lock", block: func(t *testing.T, m *Manager, home *Account) {
			if !m.MarkModelUnavailable(home, pinModel) {
				t.Fatal("MarkModelUnavailable = false with two spare accounts")
			}
		}},
	}
	for _, blocker := range blockers {
		t.Run(blocker.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
			home := pool[1]
			if got := selected(manager, "s1", pinModel); got != home.Email {
				t.Fatalf("new pin = %q, want %s", got, home.Email)
			}
			blocker.block(t, manager, home)

			if got := selected(manager, "s1", pinModel); got != "c@example.com" {
				t.Fatalf("blocked home: session moved to %q, want the overflow c@example.com", got)
			}
			entry := manager.pins["s1"]
			if entry.home != home.Email || entry.overflow[rateLimitModelKey(pinModel)] != "c@example.com" {
				t.Fatalf("pin = home %q overflow %v, want home %s and %s -> c@example.com", entry.home, entry.overflow, home.Email, pinModel)
			}
			if got := selected(manager, "s1", otherModel); got != home.Email {
				t.Fatalf("other model = %q, want the home %s: overflow is per model", got, home.Email)
			}
			manager.MarkSuccess(home, pinModel)
			if got := selected(manager, "s1", pinModel); got != "c@example.com" {
				t.Fatalf("after the home recovered the session moved to %q, want it to stay on c@example.com", got)
			}
		})
	}
}

func TestPinRebindsAGoneHomeAndKeepsOverflow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	gone := []struct {
		name   string
		remove func(t *testing.T, manager *Manager, home *Account)
	}{
		{name: "invalid", remove: func(_ *testing.T, m *Manager, home *Account) {
			m.MarkInvalid(home, "test", "")
		}},
		{name: "disabled", remove: func(t *testing.T, m *Manager, home *Account) {
			if err := m.SetAccountEnabled(home.Email, false); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "removed", remove: func(t *testing.T, m *Manager, home *Account) {
			if err := m.RemoveAccount(home.Email); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range gone {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
			home := pool[1]
			if got := selected(manager, "s1", pinModel); got != home.Email {
				t.Fatalf("new pin = %q, want %s", got, home.Email)
			}
			manager.MarkRateLimited(home, otherModel, 10*time.Minute)
			if got := selected(manager, "s1", otherModel); got != "c@example.com" {
				t.Fatalf("overflow for %s = %q, want c@example.com", otherModel, got)
			}

			test.remove(t, manager, home)
			rebound := selected(manager, "s1", pinModel)
			if rebound == "" || rebound == home.Email {
				t.Fatalf("session with a %s home got %q, want another account", test.name, rebound)
			}
			if got := manager.pins["s1"].home; got != rebound {
				t.Fatalf("pin home = %q, want the rebound %q", got, rebound)
			}
			if got := selected(manager, "s1", pinModel); got != rebound {
				t.Fatalf("next turn = %q, want the rebound home %q", got, rebound)
			}
			if got := manager.pins["s1"].overflow[rateLimitModelKey(otherModel)]; got != "c@example.com" {
				t.Fatalf("healthy overflow entry = %q, want it kept on c@example.com", got)
			}
			if got := selected(manager, "s1", otherModel); got != "c@example.com" {
				t.Fatalf("%s after the rebind = %q, want the kept overflow c@example.com", otherModel, got)
			}
		})
	}
}

func TestPinRepicksADeadOverflowAccount(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	deaths := []struct {
		name string
		kill func(manager *Manager, overflow *Account)
	}{
		{name: "invalid", kill: func(m *Manager, overflow *Account) { m.MarkInvalid(overflow, "test", "") }},
		{name: "long limit", kill: func(m *Manager, overflow *Account) { m.MarkRateLimited(overflow, pinModel, 10*time.Minute) }},
	}
	for _, death := range deaths {
		t.Run(death.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
			spare, home, overflow := pool[0], pool[1], pool[2]
			if got := selected(manager, "s1", pinModel); got != home.Email {
				t.Fatalf("new pin = %q, want %s", got, home.Email)
			}
			manager.MarkRateLimited(home, pinModel, 10*time.Minute)
			if got := selected(manager, "s1", pinModel); got != overflow.Email {
				t.Fatalf("first overflow = %q, want %s", got, overflow.Email)
			}
			// Two rejections inside one window would raise a pool-wide throttle.
			clock = clock.Add(11 * time.Second)
			death.kill(manager, overflow)

			if got := selected(manager, "s1", pinModel); got != spare.Email {
				t.Fatalf("after the overflow account died the session got %q, want %s", got, spare.Email)
			}
			entry := manager.pins["s1"]
			if entry.home != home.Email || entry.overflow[rateLimitModelKey(pinModel)] != spare.Email {
				t.Fatalf("pin = home %q overflow %v, want home %s and %s -> %s", entry.home, entry.overflow, home.Email, pinModel, spare.Email)
			}
		})
	}
}

func TestPinWithNoAlternativeGetsTodaysAnswer(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

	t.Run("round-robin, long limit", func(t *testing.T) {
		t.Parallel()
		clock := start
		manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "only@example.com")
		if got := selected(manager, "s1", pinModel); got != pool[0].Email {
			t.Fatalf("new pin = %q", got)
		}
		manager.MarkRateLimited(pool[0], pinModel, 10*time.Minute)
		got, today := manager.Select("s1", pinModel), manager.Select("", pinModel)
		if got != today || got != (Selection{}) {
			t.Fatalf("pinned answer %+v, answer without a session %+v; want both empty", got, today)
		}
		if entry := manager.pins["s1"]; entry.home != pool[0].Email || len(entry.overflow) != 0 {
			t.Fatalf("pin changed with nowhere to move: home %q overflow %v", entry.home, entry.overflow)
		}
	})

	t.Run("sticky, failure lock", func(t *testing.T) {
		t.Parallel()
		clock := start
		manager, pool := pinTestManager(t, &clock, StrategySticky, "only@example.com")
		if got := selected(manager, "s1", pinModel); got != pool[0].Email {
			t.Fatalf("new pin = %q", got)
		}
		for range 3 {
			manager.MarkFailure(pool[0], pinModel)
		}
		got, today := manager.Select("s1", pinModel), manager.Select("", pinModel)
		if got != today || got != (Selection{Wait: time.Minute}) {
			t.Fatalf("pinned answer %+v, answer without a session %+v; want both sticky's 1m wait", got, today)
		}
		if entry := manager.pins["s1"]; len(entry.overflow) != 0 {
			t.Fatalf("pin changed with nowhere to move: overflow %v", entry.overflow)
		}
	})
}

func TestPinExpiresAfterAnIdleHour(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")

	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("new pin = %q, want b@example.com", got)
	}
	clock = clock.Add(pinIdleTTL)
	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("after exactly an idle hour = %q, want the pin kept on b@example.com", got)
	}
	clock = clock.Add(pinIdleTTL + time.Millisecond)
	if got := selected(manager, "s1", pinModel); got != "a@example.com" {
		t.Fatalf("past an idle hour = %q, want a new pin on round-robin's next account a@example.com", got)
	}
}

func TestPinSweepDropsIdlePins(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")

	selected(manager, "old", pinModel)
	clock = clock.Add(pinIdleTTL + time.Millisecond)
	if active := manager.ActiveSessions(); len(active) != 0 {
		t.Fatalf("an idle pin counts as active: %v", active)
	}
	selected(manager, "new", pinModel)
	if _, kept := manager.pins["old"]; kept || len(manager.pins) != 1 {
		t.Fatalf("pins after creating one = %d (old kept: %v), want only the new pin", len(manager.pins), kept)
	}
}

func TestPinCapEvictsTheOldest(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	clock := start
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com")
	manager.pins = make(map[string]*pin, maxPins)
	for i := range maxPins {
		manager.pins[fmt.Sprintf("old-%05d", i)] = &pin{home: pool[0].Email, lastSeen: start.Add(-time.Duration(maxPins-i) * time.Millisecond)}
	}

	if got := selected(manager, "new", pinModel); got != pool[0].Email {
		t.Fatalf("new pin = %q, want %s", got, pool[0].Email)
	}
	if len(manager.pins) != maxPins {
		t.Fatalf("pins = %d, want the cap %d", len(manager.pins), maxPins)
	}
	if _, kept := manager.pins["old-00000"]; kept {
		t.Fatal("the least recently used pin survived the cap")
	}
	if _, kept := manager.pins["old-00001"]; !kept {
		t.Fatal("eviction dropped more than one pin")
	}
	if _, kept := manager.pins["new"]; !kept {
		t.Fatal("the new pin was not stored")
	}
}

func TestPinActiveSessionsCountsHomeAndOverflow(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
	a, b, c := pool[0], pool[1], pool[2]

	if got := selected(manager, "s1", pinModel); got != b.Email {
		t.Fatalf("s1 = %q, want %s", got, b.Email)
	}
	manager.MarkRateLimited(b, pinModel, 10*time.Minute)
	if got := selected(manager, "s1", pinModel); got != c.Email {
		t.Fatalf("s1 overflow = %q, want %s", got, c.Email)
	}
	if got := selected(manager, "s2", pinModel); got != a.Email {
		t.Fatalf("s2 = %q, want %s", got, a.Email)
	}
	// A pin whose overflow points back at its home counts once for that account.
	manager.pins["s3"] = &pin{home: a.Email, overflow: map[string]string{rateLimitModelKey(otherModel): a.Email}, lastSeen: clock}

	want := map[string]int{a.Email: 2, b.Email: 1, c.Email: 1}
	if got := manager.ActiveSessions(); !maps.Equal(got, want) {
		t.Fatalf("ActiveSessions = %v, want %v", got, want)
	}
}

func TestPinConversationStarted(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")

	if manager.conversationStarted("s1", "b@example.com") {
		t.Fatal("conversationStarted = true for a session with no pin")
	}
	manager.markConversationStarted("s1", "b@example.com")
	if len(manager.pins) != 0 {
		t.Fatal("markConversationStarted created a pin")
	}
	home := selected(manager, "s1", pinModel)
	if manager.conversationStarted("s1", home) {
		t.Fatal("conversationStarted = true before any start burst")
	}
	manager.markConversationStarted("s1", home)
	if !manager.conversationStarted("s1", home) {
		t.Fatalf("conversationStarted(s1, %s) = false after marking it", home)
	}
	if manager.conversationStarted("s1", "a@example.com") {
		t.Fatal("a start on one account marked another account started")
	}
	if manager.conversationStarted("", home) || manager.conversationStarted("s2", home) {
		t.Fatal("conversationStarted = true for an empty or unknown session")
	}
}

// Pin changes are logged at info so a smoke run can follow them. Only the
// first 8 characters of the session id appear in the log.
func TestPinChangesAreLogged(t *testing.T) {
	var logBuffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")
	const session = "0123456789abcdef"
	selected(manager, session, pinModel)
	manager.MarkRateLimited(pool[1], pinModel, 10*time.Minute)
	selected(manager, session, pinModel)

	out := logBuffer.String()
	for _, want := range []string{
		`msg="session pinned" session=01234567 model=claude-sonnet-4-6 from="" to=b@example.com reason=new`,
		`msg="session overflow" session=01234567 model=claude-sonnet-4-6 from=b@example.com to=a@example.com reason="rate limited 10m0s"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, session) {
		t.Errorf("log carries the full session id:\n%s", out)
	}
}
