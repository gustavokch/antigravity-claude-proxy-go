package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func unifiedFixture(base time.Time, status string, util5h, util7d float64) *Unified {
	fb := 0.5
	return &Unified{
		Status:              status,
		Reset:               base.Add(3 * time.Hour),
		RepresentativeClaim: UnifiedClaimFiveHour,
		FallbackPercentage:  &fb,
		OverageStatus:       "rejected",
		FiveHour:            UnifiedWindow{Utilization: &util5h, Reset: base.Add(3 * time.Hour), Status: status},
		SevenDay:            UnifiedWindow{Utilization: &util7d, Reset: base.Add(72 * time.Hour), Status: "allowed"},
		ObservedAt:          base,
	}
}

func TestStorage_UnifiedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claudecode_accounts.json")
	base := time.Now().Truncate(time.Second)

	pool := NewAccountPool(nil)
	pool.SetStoragePath(path)
	acc := pool.AddOrUpdateAccount(AccountConfig{ID: "cc-u", Token: "t", Type: "oauth", Enabled: true, Source: "oauth"})
	acc.mu.Lock()
	acc.RateLimits = RateLimits{LastUpdated: base, Unified: unifiedFixture(base, "allowed", 0.04, 0.22)}
	acc.mu.Unlock()

	if err := pool.SaveStoredAccounts(); err != nil {
		t.Fatalf("save: %v", err)
	}

	stored, err := LoadStoredAccounts(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(stored) != 1 || stored[0].Unified == nil {
		t.Fatalf("expected one stored account with unified data, got %+v", stored)
	}

	newPool := NewAccountPool(nil)
	newPool.SetStoragePath(path)
	if err := newPool.LoadStoredAccounts(); err != nil {
		t.Fatalf("pool load: %v", err)
	}
	got, ok := newPool.GetAccount("cc-u")
	if !ok {
		t.Fatal("account not reloaded")
	}
	got.mu.RLock()
	u := got.RateLimits.Unified
	lastUpdated := got.RateLimits.LastUpdated
	got.mu.RUnlock()
	if u == nil {
		t.Fatal("unified snapshot not restored")
	}
	if u.Status != "allowed" || u.RepresentativeClaim != UnifiedClaimFiveHour || u.OverageStatus != "rejected" {
		t.Errorf("scalar fields mismatch: %+v", u)
	}
	if u.FallbackPercentage == nil || *u.FallbackPercentage != 0.5 {
		t.Errorf("fallback percentage mismatch: %v", u.FallbackPercentage)
	}
	if u.FiveHour.Utilization == nil || *u.FiveHour.Utilization != 0.04 || !u.FiveHour.Reset.Equal(base.Add(3*time.Hour)) {
		t.Errorf("5h window mismatch: %+v", u.FiveHour)
	}
	if u.SevenDay.Utilization == nil || *u.SevenDay.Utilization != 0.22 || !u.SevenDay.Reset.Equal(base.Add(72*time.Hour)) {
		t.Errorf("7d window mismatch: %+v", u.SevenDay)
	}
	if !u.ObservedAt.Equal(base) || !u.Reset.Equal(base.Add(3*time.Hour)) {
		t.Errorf("times mismatch: observed=%v reset=%v", u.ObservedAt, u.Reset)
	}
	if !lastUpdated.Equal(base) {
		t.Errorf("expected LastUpdated %v from ObservedAt, got %v", base, lastUpdated)
	}
}

func TestRestoreUnified_DropsExpired(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	u5, u7 := 0.3, 0.6

	tests := []struct {
		name         string
		in           *Unified
		wantNil      bool
		want5hZeroed bool
		want7dZeroed bool
	}{
		{name: "nil", in: nil, wantNil: true},
		{
			name: "all live",
			in: &Unified{Reset: future, FiveHour: UnifiedWindow{Utilization: &u5, Reset: future},
				SevenDay: UnifiedWindow{Utilization: &u7, Reset: future}},
		},
		{
			name: "5h expired",
			in: &Unified{Reset: future, FiveHour: UnifiedWindow{Utilization: &u5, Reset: past, Status: "rejected"},
				SevenDay: UnifiedWindow{Utilization: &u7, Reset: future}},
			want5hZeroed: true,
		},
		{
			name: "both windows expired, overall live",
			in: &Unified{Status: "rejected", Reset: future, FiveHour: UnifiedWindow{Utilization: &u5, Reset: past},
				SevenDay: UnifiedWindow{Utilization: &u7, Reset: past}},
			want5hZeroed: true,
			want7dZeroed: true,
		},
		{
			name: "everything expired",
			in: &Unified{Status: "rejected", Reset: past, FiveHour: UnifiedWindow{Utilization: &u5, Reset: past},
				SevenDay: UnifiedWindow{Utilization: &u7, Reset: past}},
			wantNil: true,
		},
		{
			name:    "no resets at all",
			in:      &Unified{Status: "allowed", FiveHour: UnifiedWindow{Utilization: &u5}},
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var before Unified
			if tc.in != nil {
				before = *tc.in
			}
			got := restoreUnified(tc.in, now)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected a snapshot, got nil")
			}
			if got == tc.in {
				t.Error("restoreUnified must return a new value")
			}
			if *tc.in != before {
				t.Error("restoreUnified mutated its input")
			}
			if zeroed := got.FiveHour == (UnifiedWindow{}); zeroed != tc.want5hZeroed {
				t.Errorf("5h zeroed = %v, want %v (%+v)", zeroed, tc.want5hZeroed, got.FiveHour)
			}
			if zeroed := got.SevenDay == (UnifiedWindow{}); zeroed != tc.want7dZeroed {
				t.Errorf("7d zeroed = %v, want %v (%+v)", zeroed, tc.want7dZeroed, got.SevenDay)
			}
			if got.Status != tc.in.Status || !got.Reset.Equal(tc.in.Reset) {
				t.Errorf("top-level fields changed: %+v", got)
			}
		})
	}
}

func TestAccountPool_LoadStoredAccounts_DropsExpiredUnified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claudecode_accounts.json")
	now := time.Now()
	u5, u7 := 0.9, 0.4
	stored := []AccountConfig{
		{
			ID: "cc-expired", Token: "t", Type: "oauth", Enabled: true, Source: "oauth",
			Unified: &Unified{Status: "rejected", Reset: now.Add(-time.Hour),
				FiveHour: UnifiedWindow{Utilization: &u5, Reset: now.Add(-time.Hour)},
				SevenDay: UnifiedWindow{Utilization: &u7, Reset: now.Add(-time.Minute)}},
		},
		{
			ID: "cc-partial", Token: "t", Type: "oauth", Enabled: true, Source: "oauth",
			Unified: &Unified{Status: "allowed", Reset: now.Add(-time.Hour),
				FiveHour: UnifiedWindow{Utilization: &u5, Reset: now.Add(-time.Hour)},
				SevenDay: UnifiedWindow{Utilization: &u7, Reset: now.Add(48 * time.Hour)}},
		},
	}
	if err := SaveStoredAccounts(path, stored); err != nil {
		t.Fatalf("save: %v", err)
	}

	pool := NewAccountPool(nil)
	pool.SetStoragePath(path)
	if err := pool.LoadStoredAccounts(); err != nil {
		t.Fatalf("load: %v", err)
	}

	expired, _ := pool.GetAccount("cc-expired")
	if expired == nil {
		t.Fatal("cc-expired missing")
	}
	if u := expired.RateLimits.Unified; u != nil {
		t.Errorf("expected fully expired snapshot to be dropped, got %+v", u)
	}
	if expired.RateLimits.IsRateLimited(now) {
		t.Error("expired rejected snapshot must not rate-limit the account")
	}

	partial, _ := pool.GetAccount("cc-partial")
	if partial == nil {
		t.Fatal("cc-partial missing")
	}
	u := partial.RateLimits.Unified
	if u == nil {
		t.Fatal("expected partial snapshot to be kept")
	}
	if u.FiveHour != (UnifiedWindow{}) {
		t.Errorf("expected expired 5h window zeroed, got %+v", u.FiveHour)
	}
	if u.SevenDay.Utilization == nil || *u.SevenDay.Utilization != 0.4 {
		t.Errorf("expected live 7d window kept, got %+v", u.SevenDay)
	}
}

func TestStorage_OldFormatLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claudecode_accounts.json")
	old := `[
  {
    "id": "cc-old",
    "name": "Old",
    "token": "tok",
    "refreshToken": "ref",
    "type": "oauth",
    "priority": 1,
    "enabled": true,
    "source": "oauth"
  }
]`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	stored, err := LoadStoredAccounts(path)
	if err != nil {
		t.Fatalf("load old format: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != "cc-old" || stored[0].Unified != nil {
		t.Fatalf("unexpected old-format load: %+v", stored)
	}

	pool := NewAccountPool(nil)
	pool.SetStoragePath(path)
	if err := pool.LoadStoredAccounts(); err != nil {
		t.Fatalf("pool load old format: %v", err)
	}
	acc, ok := pool.GetAccount("cc-old")
	if !ok || acc.Token != "tok" || acc.RateLimits.Unified != nil {
		t.Fatalf("unexpected pool account from old format: %+v", acc)
	}

	// Saving an account with no snapshot keeps the old shape.
	if err := pool.SaveStoredAccounts(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "unified") {
		t.Errorf("expected no unified key without a snapshot, got %s", data)
	}
}

// throttlePool returns a pool with a fake clock and a counting persist seam.
func throttlePool(t *testing.T, source string) (*AccountPool, *time.Time, *atomic.Int32) {
	t.Helper()
	pool := NewAccountPool([]AccountConfig{{ID: "cc-t", Token: "t", Type: "oauth", Enabled: true, Source: source}})
	pool.SetStoragePath(filepath.Join(t.TempDir(), "claudecode_accounts.json"))
	clock := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	pool.now = func() time.Time { return clock }
	var saves atomic.Int32
	pool.persist = func() error {
		saves.Add(1)
		return nil
	}
	return pool, &clock, &saves
}

func TestAccountPool_UnifiedSaveThrottle(t *testing.T) {
	pool, clock, saves := throttlePool(t, "oauth")
	base := *clock

	step := func(offset time.Duration, fn func(), want int32, what string) {
		t.Helper()
		*clock = base.Add(offset)
		fn()
		pool.waitSaves()
		if got := saves.Load(); got != want {
			t.Fatalf("%s: saves = %d, want %d", what, got, want)
		}
	}
	allowed := func(u float64) func() {
		return func() {
			pool.RecordSuccess("cc-t", 10, 0, RateLimits{LastUpdated: *clock, Unified: unifiedFixture(base, "allowed", u, 0.1)})
		}
	}

	step(0, allowed(0.1), 1, "first snapshot saves")
	step(10*time.Second, allowed(0.2), 1, "change within 30s is throttled")
	step(12*time.Second, func() {
		pool.UpdateAccountRateLimits("cc-t", RateLimits{RequestsLimit: 10, RequestsRemaining: 5, LastUpdated: *clock})
	}, 1, "classic-only update carries the snapshot over without a save")
	step(15*time.Second, func() {
		pool.RecordRateLimit("cc-t", RateLimits{LastUpdated: *clock, Unified: unifiedFixture(base, "rejected", 1, 0.1)}, 10*time.Second)
	}, 2, "transition to rejected saves at once")
	step(20*time.Second, func() {
		pool.RecordRateLimit("cc-t", RateLimits{LastUpdated: *clock, Unified: unifiedFixture(base, "rejected", 1, 0.1)}, 10*time.Second)
	}, 2, "rejected to rejected within 30s is throttled")
	step(25*time.Second, allowed(0.3), 3, "transition from rejected saves at once")
	step(40*time.Second, allowed(0.4), 3, "change within 30s of the last save is throttled")
	step(56*time.Second, allowed(0.5), 4, "change after 30s saves")
}

func TestAccountPool_UnifiedSaveSkipped(t *testing.T) {
	t.Run("non-persistent source", func(t *testing.T) {
		pool, clock, saves := throttlePool(t, "auto_import")
		pool.RecordRateLimit("cc-t", RateLimits{Unified: unifiedFixture(*clock, "rejected", 1, 0.1)}, time.Second)
		pool.waitSaves()
		if saves.Load() != 0 {
			t.Errorf("expected no save for a non-persistent account, got %d", saves.Load())
		}
	})
	t.Run("no storage path", func(t *testing.T) {
		pool, clock, saves := throttlePool(t, "oauth")
		pool.SetStoragePath("")
		pool.RecordRateLimit("cc-t", RateLimits{Unified: unifiedFixture(*clock, "rejected", 1, 0.1)}, time.Second)
		pool.waitSaves()
		if saves.Load() != 0 {
			t.Errorf("expected no save without a storage path, got %d", saves.Load())
		}
	})
}

func TestAccountPool_UnifiedPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claudecode_accounts.json")
	now := time.Now()

	pool := NewAccountPool([]AccountConfig{{ID: "cc-r", Token: "t", Type: "oauth", Enabled: true, Source: "oauth"}})
	pool.SetStoragePath(path)
	pool.RecordRateLimit("cc-r", RateLimits{LastUpdated: now, Unified: unifiedFixture(now, "rejected", 1, 0.3)}, 10*time.Second)
	pool.waitSaves()

	// A pool rebuilt from config restores the snapshot without adding
	// store-only accounts.
	restarted := NewAccountPool([]AccountConfig{{ID: "cc-r", Token: "t", Type: "oauth", Enabled: true, Source: "oauth"}})
	restarted.SetStoragePath(path)
	if err := restarted.RestoreStoredUnified(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	acc, _ := restarted.GetAccount("cc-r")
	if acc == nil || acc.RateLimits.Unified == nil {
		t.Fatal("expected unified snapshot restored after restart")
	}
	if !acc.RateLimits.IsRateLimited(now.Add(time.Minute)) {
		t.Error("restored rejected snapshot should keep the account rate-limited until reset")
	}
	if acc.RateLimits.IsRateLimited(now.Add(4 * time.Hour)) {
		t.Error("restored rejected snapshot should stop limiting after the 5h reset")
	}

	other := NewAccountPool(nil)
	other.SetStoragePath(path)
	if err := other.RestoreStoredUnified(); err != nil {
		t.Fatalf("restore into empty pool: %v", err)
	}
	if n := len(other.ListAccounts()); n != 0 {
		t.Errorf("RestoreStoredUnified must not add accounts, got %d", n)
	}
}

func TestAccountPool_RestoreKeepsNewerUnified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claudecode_accounts.json")
	now := time.Now()
	if err := SaveStoredAccounts(path, []AccountConfig{{ID: "cc-n", Token: "t", Type: "oauth", Enabled: true, Source: "oauth",
		Unified: unifiedFixture(now, "rejected", 1, 0.3)}}); err != nil {
		t.Fatal(err)
	}

	pool := NewAccountPool([]AccountConfig{{ID: "cc-n", Token: "t", Type: "oauth", Enabled: true, Source: "oauth"}})
	pool.SetStoragePath(path)
	fresh := unifiedFixture(now, "allowed", 0.1, 0.1)
	pool.UpdateAccountRateLimits("cc-n", RateLimits{LastUpdated: now, Unified: fresh})
	pool.waitSaves()
	if err := pool.RestoreStoredUnified(); err != nil {
		t.Fatal(err)
	}
	acc, _ := pool.GetAccount("cc-n")
	if acc.RateLimits.Unified != fresh {
		t.Errorf("restore must not replace live unified data, got %+v", acc.RateLimits.Unified)
	}
}

func TestAccountPool_CooldownNeverShortened(t *testing.T) {
	now := time.Now()
	pool := NewAccountPool([]AccountConfig{{ID: "cc-c", Token: "t", Type: "oauth", Enabled: true}})

	pool.RecordRateLimit("cc-c", RateLimits{LastUpdated: now, Unified: unifiedFixture(now, "rejected", 1, 0.3)}, 10*time.Second)
	acc, _ := pool.GetAccount("cc-c")
	long := acc.CooldownUntil
	if time.Until(long) < 2*time.Hour {
		t.Fatalf("expected a multi-hour cooldown from the rejected window, got %v", time.Until(long))
	}

	pool.RecordFailure("cc-c", true, 30*time.Second)
	if !acc.CooldownUntil.Equal(long) {
		t.Errorf("5xx shortened cooldown: %v -> %v", long, acc.CooldownUntil)
	}

	pool.RecordRateLimit("cc-c", RateLimits{RetryAfter: 5, LastUpdated: time.Now()}, 10*time.Second)
	if !acc.CooldownUntil.Equal(long) {
		t.Errorf("shorter 429 shortened cooldown: %v -> %v", long, acc.CooldownUntil)
	}

	// A longer cooldown still extends it.
	pool.RecordRateLimit("cc-c", RateLimits{RetryAfter: 5 * 3600, LastUpdated: time.Now()}, 10*time.Second)
	if !acc.CooldownUntil.After(long) {
		t.Errorf("longer 429 should extend cooldown beyond %v, got %v", long, acc.CooldownUntil)
	}
}
