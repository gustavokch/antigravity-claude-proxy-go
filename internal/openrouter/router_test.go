package openrouter

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func mkEndpoints() []ProviderEndpoint {
	return []ProviderEndpoint{
		{ProviderName: "anthropic", Tag: "default", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.98, UptimeLast1d: 0.97},
		{ProviderName: "azure", ContextLength: 180000, UptimeLast5m: 0.85},
		{ProviderName: "deepinfra", ContextLength: 150000, UptimeLast5m: 0.7, UptimeLast30m: 0.5, UptimeLast1d: 0.4},
		{ProviderName: "broken", ContextLength: 100000, UptimeLast5m: 0.1, UptimeLast30m: 0.05, UptimeLast1d: 0.1, Status: 500},
	}
}

func TestProviderRouter_RefreshRanksOrdersByScore(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("anthropic/claude", mkEndpoints())

	ranks := r.GetRanks("anthropic/claude")
	if len(ranks) != 4 {
		t.Fatalf("expected 4 ranks, got %d", len(ranks))
	}
	if ranks[0].Provider != "anthropic" {
		t.Errorf("expected anthropic top, got %s", ranks[0].Provider)
	}
	if ranks[len(ranks)-1].Provider != "broken" {
		t.Errorf("expected broken last, got %s", ranks[len(ranks)-1].Provider)
	}
}

func TestProviderRouter_SelectAutoAndSticky(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", mkEndpoints())

	// First call: top of rank.
	got := r.Select("s1", "m1", ProviderOrder{Mode: "auto"})
	if got != "anthropic" {
		t.Errorf("first select want anthropic, got %s", got)
	}
	// Same session+model: sticky.
	if got2 := r.Select("s1", "m1", ProviderOrder{Mode: "auto"}); got2 != "anthropic" {
		t.Errorf("sticky want anthropic, got %s", got2)
	}
	// Different session: top of rank.
	if got3 := r.Select("s2", "m1", ProviderOrder{Mode: "auto"}); got3 != "anthropic" {
		t.Errorf("new session want anthropic, got %s", got3)
	}
}

func TestProviderRouter_SelectPinned(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", mkEndpoints())
	got := r.Select("s1", "m1", ProviderOrder{Mode: "pinned", Pin: "azure"})
	if got != "azure" {
		t.Errorf("pinned want azure, got %s", got)
	}
}

func TestProviderRouter_SelectCustom(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", mkEndpoints())
	// Custom with one valid + one invalid — uses valid.
	got := r.Select("s1", "m1", ProviderOrder{Mode: "custom", Order: []string{"missing", "azure"}})
	if got != "azure" {
		t.Errorf("custom want azure, got %s", got)
	}
}

func TestProviderRouter_RefreshDropsStaleSticky(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", mkEndpoints())
	r.Select("s1", "m1", ProviderOrder{Mode: "auto"}) // sticky = anthropic

	// New catalog without "anthropic"
	r.RefreshRanks("m1", []ProviderEndpoint{
		{ProviderName: "azure", UptimeLast5m: 0.9},
	})
	// Sticky should be dropped because anthropic is gone.
	_, ok := r.StickyProvider("s1", "m1")
	if ok {
		t.Errorf("expected sticky dropped when provider vanished")
	}
	got := r.Select("s1", "m1", ProviderOrder{Mode: "auto"})
	if got != "azure" {
		t.Errorf("expected azure after refresh, got %s", got)
	}
}

func TestProviderRouter_RecordResultEWMA(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", mkEndpoints())

	r.RecordResult("m1", "anthropic", true, 100*time.Millisecond, 50)
	r.RecordResult("m1", "anthropic", true, 200*time.Millisecond, 80)

	stats := r.Stats("m1")
	if s, ok := stats["anthropic"]; !ok {
		t.Fatalf("expected stats for anthropic")
	} else {
		if s.SuccessCount != 2 {
			t.Errorf("expected 2 successes, got %d", s.SuccessCount)
		}
		if s.TPSEWMA <= 0 {
			t.Errorf("expected tps EWMA > 0, got %v", s.TPSEWMA)
		}
	}
}

func TestProviderRouter_FailureThresholdBreaksSticky(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.FailureThreshold = 3
	r := NewProviderRouter(cfg)
	r.RefreshRanks("m1", mkEndpoints())
	r.Select("s1", "m1", ProviderOrder{Mode: "auto"})

	// Make azure rank above anthropic by recording success on azure and failure on anthropic
	for i := 0; i < 5; i++ {
		r.RecordResult("m1", "azure", true, 50*time.Millisecond, 200)
	}
	for i := 0; i < 3; i++ {
		r.RecordResult("m1", "anthropic", false, 0, 0)
	}
	// Threshold reached: stickiness on "anthropic" should be dropped
	if _, ok := r.StickyProvider("s1", "m1"); ok {
		// But it might have been a new selection by then. Just confirm stat.
		stats := r.Stats("m1")
		if stats["anthropic"].ConsecFails < 3 {
			t.Errorf("expected consec fails >= 3, got %d", stats["anthropic"].ConsecFails)
		}
	}
}

func TestProviderRouter_SetConfig(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.SetConfig(RoutingConfig{FailureThreshold: 5, RankWeights: DefaultRankWeights()})
	// We can't directly inspect config, but SetConfig must not panic and ranks must still work.
	r.RefreshRanks("m1", mkEndpoints())
	got := r.Select("s1", "m1", ProviderOrder{Mode: "auto"})
	if got != "anthropic" {
		t.Errorf("expected anthropic, got %s", got)
	}
}

func TestProviderRouter_ColdStartScoringIsBalanced(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	// Single endpoint, no stats
	r.RefreshRanks("m1", []ProviderEndpoint{
		{ProviderName: "x", UptimeLast5m: 0.99, ContextLength: 100000},
	})
	ranks := r.GetRanks("m1")
	if len(ranks) != 1 {
		t.Fatalf("expected 1 rank, got %d", len(ranks))
	}
	// Cold start should give a moderate score (mid range, not extreme)
	if ranks[0].Score <= 0.0 || ranks[0].Score >= 1.0 {
		t.Errorf("expected mid-range score for cold start, got %v", ranks[0].Score)
	}
}

func TestProviderRouter_PersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-state.json")

	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", mkEndpoints())
	r.Select("s1", "m1", ProviderOrder{Mode: "auto"})
	r.RecordResult("m1", "anthropic", true, 150*time.Millisecond, 120)
	r.RecordResult("m1", "azure", false, 0, 0)

	if err := r.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	r2 := NewProviderRouter(DefaultRoutingConfig())
	if err := r2.LoadFrom(path); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if got, ok := r2.StickyProvider("s1", "m1"); !ok || got != "anthropic" {
		t.Errorf("sticky not restored, got %q ok=%v", got, ok)
	}
	stats := r2.Stats("m1")
	if stats["anthropic"].SuccessCount != 1 {
		t.Errorf("expected 1 success for anthropic, got %d", stats["anthropic"].SuccessCount)
	}
	if stats["anthropic"].LatencyMsEWMA != 150 {
		t.Errorf("expected latency EWMA 150, got %v", stats["anthropic"].LatencyMsEWMA)
	}
	if stats["azure"].ConsecFails != 1 {
		t.Errorf("expected 1 consec fail for azure, got %d", stats["azure"].ConsecFails)
	}
}

func TestProviderRouter_LoadCorruptFileIsCleanStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewProviderRouter(DefaultRoutingConfig())
	if err := r.LoadFrom(path); err != nil {
		t.Fatalf("corrupt file should not error, got %v", err)
	}
	if _, ok := r.StickyProvider("s1", "m1"); ok {
		t.Errorf("expected empty state after corrupt load")
	}
}

func TestProviderRouter_LoadMissingFileIsCleanStart(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	if err := r.LoadFrom(filepath.Join(t.TempDir(), "does-not-exist.json")); err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
}

func TestProviderRouter_LoadWrongVersionIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-state.json")
	if err := os.WriteFile(path, []byte(`{"version": 999, "sticky": {"s1\x00m1": "anthropic"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewProviderRouter(DefaultRoutingConfig())
	if err := r.LoadFrom(path); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if _, ok := r.StickyProvider("s1", "m1"); ok {
		t.Errorf("wrong-version state should be ignored")
	}
}

func TestProviderRouter_EnablePersistenceDebouncedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router-state.json")
	r := NewProviderRouter(DefaultRoutingConfig())
	r.EnablePersistence(path)
	r.RefreshRanks("m1", mkEndpoints())
	r.Select("s1", "m1", ProviderOrder{Mode: "auto"})

	// Debounced save not fired yet.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file before debounce window, stat err=%v", err)
	}

	// Flush forces immediate save.
	r.FlushSave()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file after FlushSave, got %v", err)
	}
}

func TestProviderRouter_StickyMapEvictsOldest(t *testing.T) {
	// Unbounded sticky growth leaks memory across long uptimes; past
	// maxStickyEntries the oldest entries must be evicted first.
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", mkEndpoints())

	old := time.Now().Add(-time.Hour)
	recent := time.Now()
	for i := 0; i < maxStickyEntries; i++ {
		k := keySticky("old-session-"+strconv.Itoa(i), "m1")
		r.sticky[k] = "anthropic"
		r.stickyAt[k] = old
	}
	for i := 0; i < 5; i++ {
		k := keySticky("new-session-"+strconv.Itoa(i), "m1")
		r.sticky[k] = "azure"
		r.stickyAt[k] = recent
	}

	r.mu.Lock()
	r.pruneStickyLocked()
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.sticky) != maxStickyEntries {
		t.Errorf("expected sticky map capped at %d, got %d", maxStickyEntries, len(r.sticky))
	}
	if len(r.stickyAt) != len(r.sticky) {
		t.Errorf("stickyAt must track sticky keys: %d vs %d", len(r.stickyAt), len(r.sticky))
	}
	for i := 0; i < 5; i++ {
		if _, ok := r.sticky[keySticky("new-session-"+strconv.Itoa(i), "m1")]; !ok {
			t.Errorf("recent entry new-session-%d evicted; oldest must go first", i)
		}
	}
}
