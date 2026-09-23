package accounts

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
)

func TestLoadIsReadOnlyAndResetsTransientStartupState(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "accounts.json")
	original := []byte(`{
  "activeIndex": 99,
  "settings": {"strategy":"hybrid"},
  "accounts": [
    {"email":"reset@example.com","source":"agy","isInvalid":true,"invalidReason":"old failure"},
    {"email":"verify@example.com","source":"oauth","enabled":false,"isInvalid":true,"invalidReason":"verify","verifyUrl":"https://accounts.google.com/signin/continue?x=1","modelRateLimits":{"claude":{"isRateLimited":true,"resetTime":1234,"actualResetMs":1000}}}
  ]
}`)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveIndex != 0 || len(loaded.Accounts) != 2 {
		t.Fatalf("loaded=%#v", loaded)
	}
	if !loaded.Accounts[0].Enabled || loaded.Accounts[0].IsInvalid || loaded.Accounts[0].InvalidReason != "" {
		t.Fatalf("startup-reset account=%#v", loaded.Accounts[0])
	}
	if loaded.Accounts[1].Enabled || !loaded.Accounts[1].IsInvalid || loaded.Accounts[1].VerifyURL == "" {
		t.Fatalf("verification account=%#v", loaded.Accounts[1])
	}
	if limit := loaded.Accounts[1].ModelRateLimits["claude"]; limit == nil || limit.ResetTimeMS != 1234 {
		t.Fatalf("rate limit=%#v", limit)
	}
	afterContents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterContents, original) || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("Load changed the account file: mode %v -> %v, mtime %v -> %v", before.Mode(), after.Mode(), before.ModTime(), after.ModTime())
	}
}

func TestNewDefaultDoesNotAutoImportAgyTokenWithoutAccountFile(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	path := filepath.Join(directory, "antigravity-oauth-token")
	if err := os.WriteFile(path, []byte(`{"token":{"access_token":"token","expiry":"2030-01-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_TOKEN_PATH", path)
	manager, err := NewDefault("", StrategyHybrid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Count() != 0 {
		t.Fatalf("expected empty pool without accounts.json, got %d accounts", manager.Count())
	}
	selection := manager.Select("gemini")
	if selection.Account != nil {
		t.Fatalf("expected nil selection, got %#v", selection.Account)
	}
}

func TestNewDefaultEmptyPoolWithoutAnyAccount(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	t.Setenv("AGY_TOKEN_PATH", filepath.Join(directory, "missing-token"))
	manager, err := NewDefault("", StrategyHybrid, nil)
	if err != nil {
		t.Fatalf("NewDefault with no accounts anywhere failed: %v", err)
	}
	if manager.Count() != 0 {
		t.Fatalf("expected empty pool, got %d accounts", manager.Count())
	}
	if got := manager.Available("gemini"); got != 0 {
		t.Fatalf("expected 0 available, got %d", got)
	}
	if selection := manager.Select("gemini"); selection.Account != nil {
		t.Fatalf("expected nil selection, got %v", selection.Account)
	}
}

func TestRoundRobinAndPerModelRateLimits(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	first := testAccount("first@example.com")
	second := testAccount("second@example.com")
	manager, err := New(Options{Accounts: []*Account{first, second}, Strategy: StrategyRoundRobin, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	selection := manager.Select("claude-sonnet-4-6")
	if selection.Account != second {
		t.Fatalf("first round-robin selection=%v", selection.Account.Email)
	}
	manager.MarkRateLimited(second, "claude-sonnet-4-6", time.Minute)
	if got := manager.Select("claude-sonnet-4-6").Account; got != first {
		t.Fatalf("Claude selection=%v", got)
	}
	if got := manager.Select("gemini-3.5-flash-low").Account; got != second {
		t.Fatalf("model-specific limit leaked to Gemini: selection=%v", got)
	}
	now = now.Add(time.Minute + time.Millisecond)
	if manager.Available("claude-sonnet-4-6") != 2 {
		t.Fatal("expired per-model limit was not cleared")
	}
}

func TestStickyWaitsOnlyForShortCurrentLimit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	account := testAccount("sticky@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	manager.MarkRateLimited(account, "claude", 30*time.Second)
	selection := manager.Select("claude")
	if selection.Account != nil || selection.Wait != 30*time.Second {
		t.Fatalf("selection=%#v", selection)
	}
	manager.MarkRateLimited(account, "claude", 3*time.Minute)
	if selection := manager.Select("claude"); selection.Account != nil || selection.Wait != 0 {
		t.Fatalf("long cooldown should rotate/fail without waiting: %#v", selection)
	}
}

func TestMarkFailureDoesNotRateLimitEmptyModel(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	account := testAccount("empty-model@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		manager.MarkFailure(account, "")
	}
	if account.IsInvalid || account.ModelRateLimits[""] != nil {
		t.Fatalf("empty-model MarkFailure created a rate limit or invalidation: IsInvalid=%v ModelRateLimits[\"\"]=%#v", account.IsInvalid, account.ModelRateLimits[""])
	}

	account.ConsecutiveFailure = 0
	for i := 0; i < 2; i++ {
		manager.MarkFailure(account, "claude")
	}
	if account.ModelRateLimits["claude"] != nil {
		t.Fatalf("rate limit created before 3 consecutive failures: %#v", account.ModelRateLimits["claude"])
	}
	manager.MarkFailure(account, "claude")
	if limit := account.ModelRateLimits["claude"]; limit == nil || !limit.IsRateLimited {
		t.Fatalf("expected per-model rate limit after 3 consecutive failures, got %#v", limit)
	}
}

func TestMarkFailure_SuffixOnlyModelDoesNotCreateEmptyKey(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	account := testAccount("suffix-only@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// A model string that normalizes to empty (suffix only, whitespace only)
	// must not write into the empty-model namespace used by model listing.
	for _, model := range []string{"[1m]", "   "} {
		account.ConsecutiveFailure = 0
		for i := 0; i < 3; i++ {
			manager.MarkFailure(account, model)
		}
		if account.ModelRateLimits[""] != nil {
			t.Fatalf("MarkFailure(%q) created an empty-key rate limit: %#v", model, account.ModelRateLimits[""])
		}
	}
}

// TestRateLimitKeyNamespaceIsSuffixAndCaseInsensitive pins the contract that
// ModelRateLimits has a single canonical key namespace: writers and readers
// agree regardless of "[1m]" suffix, case, or whitespace in the argument.
func TestRateLimitKeyNamespaceIsSuffixAndCaseInsensitive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	account := testAccount("namespace@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	manager.MarkRateLimited(account, "gemini-3.8-flash-medium", time.Hour)

	if got := manager.Available("gemini-3.8-flash-medium[1m]"); got != 0 {
		t.Fatalf("suffixed lookup should see the limit: Available=%d", got)
	}
	if got := manager.Available("GEMINI-3.8-Flash-Medium[1M]"); got != 0 {
		t.Fatalf("case/suffix-insensitive lookup should see the limit: Available=%d", got)
	}
	if got := manager.MinWait("gemini-3.8-flash-medium[1m]"); got <= 0 {
		t.Fatalf("MinWait should report the remaining wait: %v", got)
	}
	if got := manager.Available("gemini-3.8-flash-low[1m]"); got != 1 {
		t.Fatalf("a different model must be unaffected: Available=%d", got)
	}

	manager.MarkSuccess(account, "gemini-3.8-flash-medium[1m]")
	if got := manager.Available("gemini-3.8-flash-medium"); got != 1 {
		t.Fatalf("success via suffixed string should clear the limit: Available=%d", got)
	}
}

func TestAccountCloningAndConcurrency(t *testing.T) {
	t.Parallel()
	account := testAccount("concurrent@example.com")
	account.ModelRateLimits = make(map[string]*RateLimit)
	account.ModelThreshold = make(map[string]float64)
	account.Quota.Models = make(map[string]ModelQuota)
	account.ModelRateLimits["claude"] = &RateLimit{IsRateLimited: true, ResetTimeMS: 1234}
	account.ModelThreshold["claude"] = 0.1
	quarter := 0.25
	account.Quota.Models["claude"] = ModelQuota{RemainingFraction: &quarter}

	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid})
	if err != nil {
		t.Fatal(err)
	}

	// Verify GetAllAccounts returns cloned objects
	list := manager.GetAllAccounts()
	if len(list) != 1 {
		t.Fatalf("expected 1 account, got %d", len(list))
	}
	if list[0] == account {
		t.Error("GetAllAccounts returned pointer to internal account struct, expected cloned pointer")
	}
	if list[0].ModelRateLimits["claude"] == account.ModelRateLimits["claude"] {
		t.Error("GetAllAccounts did not clone ModelRateLimits map values")
	}

	// Concurrent mutation and GetAllAccounts read should not trigger race detector
	done := make(chan bool)
	go func() {
		for i := 0; i < 100; i++ {
			manager.MarkRateLimited(account, "claude", time.Second)
			manager.MarkSuccess(account, "claude")
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 100; i++ {
			accs := manager.GetAllAccounts()
			for _, a := range accs {
				_ = a.ModelRateLimits["claude"]
				_ = a.Quota.Models["claude"]
			}
		}
		done <- true
	}()

	<-done
	<-done
}

func TestExtractSubscriptionAndTierDetection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		jsonBody      string
		wantTier      string
		wantProjectID string
	}{
		{
			name:          "Pro account from paidTier and currentTier",
			jsonBody:      `{"cloudaicompanionProject":"proj-pro","currentTier":{"id":"tier-pro","name":"Google One AI Premium"},"paidTier":{"id":"tier-pro"}}`,
			wantTier:      "pro",
			wantProjectID: "proj-pro",
		},
		{
			name:          "Pro account from g1Tier PRO",
			jsonBody:      `{"cloudaicompanionProject":"proj-g1","currentTier":{"id":"tier-1"},"g1Tier":"PRO"}`,
			wantTier:      "pro",
			wantProjectID: "proj-g1",
		},
		{
			name:          "Ultra account from currentTier name and g1Tier",
			jsonBody:      `{"cloudaicompanionProject":"proj-ultra","currentTier":{"id":"tier-ultra","name":"Gemini Ultra"},"g1Tier":"ULTRA"}`,
			wantTier:      "ultra",
			wantProjectID: "proj-ultra",
		},
		{
			name:          "Free account from currentTier free",
			jsonBody:      `{"cloudaicompanionProject":"proj-free","currentTier":{"id":"tier-free","name":"Free Tier"}}`,
			wantTier:      "free",
			wantProjectID: "proj-free",
		},
		{
			name:          "Project object format",
			jsonBody:      `{"cloudaicompanionProject":{"id":"proj-obj"},"currentTier":{"id":"tier-pro"}}`,
			wantTier:      "pro",
			wantProjectID: "proj-obj",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sub := ExtractSubscription([]byte(tc.jsonBody), now)
			if sub.Tier != tc.wantTier {
				t.Errorf("got Tier = %q, want %q", sub.Tier, tc.wantTier)
			}
			if sub.ProjectID != tc.wantProjectID {
				t.Errorf("got ProjectID = %q, want %q", sub.ProjectID, tc.wantProjectID)
			}
		})
	}
}

func testAccount(email string) *Account {
	return &Account{
		Email:           email,
		Source:          "manual",
		Enabled:         true,
		APIKey:          "token-" + email,
		ProjectID:       "project-" + email,
		ModelRateLimits: make(map[string]*RateLimit),
		ModelThreshold:  make(map[string]float64),
		Quota:           Quota{Models: make(map[string]ModelQuota)},
	}
}

func TestColdStartManagerPersistence(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tempDir)

	tokenPath := filepath.Join(tempDir, "antigravity-oauth-token")
	if err := os.WriteFile(tokenPath, []byte(`{"token":{"access_token":"token","expiry":"2030-01-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_TOKEN_PATH", tokenPath)

	mgr, err := NewDefault("", StrategyHybrid, nil)
	if err != nil {
		t.Fatalf("NewDefault failed: %v", err)
	}

	expectedConfigPath := filepath.Join(tempDir, "accounts.json")
	if mgr.ConfigPath() != expectedConfigPath {
		t.Errorf("expected configPath %s, got %s", expectedConfigPath, mgr.ConfigPath())
	}

	newAcc := &Account{
		Email:        "user@example.com",
		Source:       "oauth",
		RefreshToken: "refresh-token-123",
		Enabled:      true,
	}
	if err := mgr.AddOrUpdateAccount(newAcc); err != nil {
		t.Fatalf("AddOrUpdateAccount failed: %v", err)
	}

	if _, err := os.Stat(expectedConfigPath); err != nil {
		t.Fatalf("accounts.json not created on disk: %v", err)
	}

	// Simulate restart
	restartedMgr, err := NewDefault("", StrategyHybrid, nil)
	if err != nil {
		t.Fatalf("restarted NewDefault failed: %v", err)
	}

	accountsList := restartedMgr.GetAllAccounts()
	found := false
	for _, acc := range accountsList {
		if acc.Email == "user@example.com" && acc.RefreshToken == "refresh-token-123" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("user@example.com was not persisted across restart")
	}
}

func TestDynamicScoringAndThresholds(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	account := testAccount("custom-scoring@example.com")
	mgr, err := New(Options{
		Accounts: []*Account{account},
		Strategy: StrategyHybrid,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify default score calculation
	defaultScore := mgr.scoreLocked(account, "claude")

	// Update weights and verify score change
	mgr.SetSelectionConfig(config.AccountSelectionConfig{
		Strategy: StrategyHybrid,
		Weights: map[string]any{
			"health": 10.0,
			"tokens": 1.0,
			"quota":  1.0,
			"lru":    0.0,
		},
		HealthScore: map[string]any{
			"initial": 90.0,
		},
	}, 0.20)

	customScore := mgr.scoreLocked(account, "claude")
	if customScore == defaultScore {
		t.Errorf("expected score to change with custom weights, got %f == %f", customScore, defaultScore)
	}

	// Verify global quota threshold propagation
	half := 0.15
	account.Quota.Models["claude"] = ModelQuota{RemainingFraction: &half}
	account.Quota.LastChecked = float64(now.UnixMilli())

	// Threshold is 0.20, so 0.15 should be critical
	if !mgr.quotaCriticalLocked(account, "claude") {
		t.Errorf("expected 0.15 to be quota critical with 0.20 threshold")
	}

	// Lower threshold to 0.10, so 0.15 is no longer critical
	mgr.SetSelectionConfig(mgr.GetSelectionConfig(), 0.10)
	if mgr.quotaCriticalLocked(account, "claude") {
		t.Errorf("expected 0.15 not to be quota critical with 0.10 threshold")
	}

	// Verify tokensLocked fallback with non-positive maxTokens
	mgr.SetSelectionConfig(config.AccountSelectionConfig{
		Strategy: StrategyHybrid,
		TokenBucket: map[string]any{
			"maxTokens": 0.0,
		},
	}, 0.05)

	tokens := mgr.tokensLocked(account.Email)
	if tokens <= 0 {
		t.Errorf("expected tokens > 0 with fallback, got %f", tokens)
	}
	if tokens != 50 {
		t.Errorf("expected default tokens 50, got %f", tokens)
	}
}
func TestSelectStickyLocked_EmptyAccounts(t *testing.T) {
	manager := &Manager{accounts: []*Account{}, now: time.Now}
	selection := manager.selectStickyLocked("model")
	if selection.Account != nil {
		t.Error("expected nil account when empty")
	}
}

func TestSelectRoundRobinLocked_EmptyAccounts(t *testing.T) {
	manager := &Manager{accounts: []*Account{}, now: time.Now}
	selection := manager.selectRoundRobinLocked("model")
	if selection.Account != nil {
		t.Error("expected nil account when empty")
	}
}

func TestSelectHybridLocked_EmptyAccounts(t *testing.T) {
	manager := &Manager{accounts: []*Account{}, now: time.Now}
	selection := manager.selectHybridLocked("model")
	if selection.Account != nil {
		t.Error("expected nil account when empty")
	}
}

func TestAvailable_EmptyAccounts(t *testing.T) {
	manager := &Manager{accounts: []*Account{}, now: time.Now}
	count := manager.Available("model")
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestMarkRateLimited_EmptyModelWritesListingNamespace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := testAccount("listing-429@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// "" is the deliberate model-listing namespace written by rotateForError
	// (dispatcher.go:257 -> MarkRateLimited(account, "", wait)). Dropping the
	// write leaves the account selectable after a listing 429.
	manager.MarkRateLimited(account, "", time.Hour)
	if limit := account.ModelRateLimits[""]; limit == nil || !limit.IsRateLimited {
		t.Fatalf("MarkRateLimited(account, \"\") must write the listing namespace: %#v", account.ModelRateLimits[""])
	}

	// A non-blank string that normalizes to blank is a malformed model, not
	// the listing namespace, and must still be rejected.
	manager.MarkRateLimited(account, "[1m]", time.Hour)
	manager.MarkRateLimited(account, "   ", time.Hour)
	for key := range account.ModelRateLimits {
		if key != "" {
			t.Fatalf("unexpected key %q written by a blank-normalizing model", key)
		}
	}
}

func TestMarkSuccess_EmptyModelClearsListingNamespace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := testAccount("listing-clear@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	manager.MarkRateLimited(account, "", time.Hour)
	manager.MarkSuccess(account, "")
	if account.ModelRateLimits[""] != nil {
		t.Fatalf("MarkSuccess(account, \"\") must clear the listing namespace: %#v", account.ModelRateLimits[""])
	}

	// A blank-normalizing non-blank string must not touch the namespace.
	manager.MarkRateLimited(account, "", time.Hour)
	manager.MarkSuccess(account, "[1m]")
	if account.ModelRateLimits[""] == nil {
		t.Fatal("MarkSuccess(account, \"[1m]\") cleared the listing namespace")
	}
}

func sharedThrottleManager(t *testing.T, clock *time.Time, emails ...string) *Manager {
	t.Helper()
	pool := make([]*Account, 0, len(emails))
	for _, email := range emails {
		pool = append(pool, &Account{Email: email, Enabled: true})
	}
	manager, err := New(Options{
		Accounts:             pool,
		SharedThrottleWindow: 10 * time.Second,
		Now:                  func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// Two accounts rejected on the same model inside the window means one shared
// bucket. Selecting must then return no account and a wait, so the dispatcher
// waits instead of spending the rest of the pool on certain rejections.
func TestSharedThrottleBlocksSelectionAfterTwoAccounts(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	clock = clock.Add(time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	selection := manager.Select("gemini-3.8-flash-high")
	if selection.Account != nil {
		t.Fatalf("Select returned %s, want no account under a shared throttle", selection.Account.Email)
	}
	if selection.Wait <= 0 {
		t.Fatal("Select must report a wait under a shared throttle")
	}
	if got := manager.Available("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("Available = %d, want 0 under a shared throttle", got)
	}
}

// One account rejected is an ordinary per-account rate limit. Rotation to the
// other account must still happen.
func TestSharedThrottleNotSetForASingleAccount(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)

	selection := manager.Select("gemini-3.8-flash-high")
	if selection.Account == nil || selection.Account.Email != "b@example.com" {
		t.Fatal("one rejected account must still rotate to the other account")
	}
}

// Rejections far apart are two independent per-account limits, not one
// bucket. Treating them as shared would stall the pool on coincidence.
func TestSharedThrottleNotSetOutsideTheWindow(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	clock = clock.Add(11 * time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	if got := manager.SharedThrottleWait("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("SharedThrottleWait = %s, want 0 for rejections outside the window", got)
	}
}

// A success proves the bucket reopened; holding the pool-wide wait after that
// would idle a working model.
func TestMarkSuccessClearsTheSharedThrottle(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkSuccess(manager.GetAllAccounts()[0], "gemini-3.8-flash-high")

	if got := manager.SharedThrottleWait("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("SharedThrottleWait after a success = %s, want 0", got)
	}
}

// The wait must expire on its own even without a success.
func TestSharedThrottleExpires(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)
	clock = clock.Add(31 * time.Second)

	if got := manager.SharedThrottleWait("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("SharedThrottleWait after expiry = %s, want 0", got)
	}
	if manager.Select("gemini-3.8-flash-high").Account == nil {
		t.Fatal("Select must return an account once the shared throttle expires")
	}
}

// A shared throttle on one model must not stall a different model.
func TestSharedThrottleIsPerModel(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	if manager.Select("gemini-2.5-pro").Account == nil {
		t.Fatal("a shared throttle on one model must not block another model")
	}
}

// Intermediate Select calls during normal account rotation must not clear the
// record of the first rejection before the second rejection arrives.
func TestSharedThrottleBlocksSelectionAfterTwoAccountsWithIntermediateSelect(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	rot := manager.Select("gemini-3.8-flash-high")
	if rot.Account == nil || rot.Account.Email != "b@example.com" {
		t.Fatalf("expected rotation to b@example.com, got %#v", rot)
	}
	clock = clock.Add(time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	selection := manager.Select("gemini-3.8-flash-high")
	if selection.Account != nil {
		t.Fatalf("Select returned %s, want no account under a shared throttle", selection.Account.Email)
	}
	if selection.Wait <= 0 {
		t.Fatal("Select must report a wait under a shared throttle")
	}
	if got := manager.Available("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("Available = %d, want 0 under a shared throttle", got)
	}
}

// SharedThrottles reports active throttled models and remaining durations,
// and returns an empty map when throttles are cleared or expired.
func TestSharedThrottles(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	if got := manager.SharedThrottles(); len(got) != 0 {
		t.Fatalf("SharedThrottles initially = %v, want empty map", got)
	}

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	throttles := manager.SharedThrottles()
	if len(throttles) != 1 {
		t.Fatalf("SharedThrottles count = %d, want 1; got %v", len(throttles), throttles)
	}
	wait, ok := throttles["gemini-3.8-flash-high"]
	if !ok {
		t.Fatalf("SharedThrottles missing model gemini-3.8-flash-high; got %v", throttles)
	}
	if wait != 30*time.Second {
		t.Fatalf("SharedThrottles wait = %v, want %v", wait, 30*time.Second)
	}

	// Cleared via MarkSuccess
	manager.MarkSuccess(manager.GetAllAccounts()[0], "gemini-3.8-flash-high")
	if got := manager.SharedThrottles(); len(got) != 0 {
		t.Fatalf("SharedThrottles after MarkSuccess = %v, want empty map", got)
	}

	// Re-triggered and then expired
	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)
	if got := manager.SharedThrottles(); len(got) != 1 {
		t.Fatalf("SharedThrottles re-triggered count = %d, want 1", len(got))
	}

	clock = clock.Add(31 * time.Second)
	if got := manager.SharedThrottles(); len(got) != 0 {
		t.Fatalf("SharedThrottles after expiry = %v, want empty map", got)
	}
}

func TestManager_QuotaCritical_Normalizes1mSuffix(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	zero := 0.0
	full := 1.0

	exhaustedAcc := &Account{
		Email:   "exhausted@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &zero,
					ResetTime:         clock.Add(10 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: clock.UnixMilli(),
		},
	}
	freshAcc := &Account{
		Email:   "fresh@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &full,
				},
			},
			LastChecked: clock.UnixMilli(),
		},
	}

	manager, err := New(Options{
		Accounts: []*Account{exhaustedAcc, freshAcc},
		Strategy: StrategyHybrid,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// When selecting with the [1m] suffix, exhaustedAcc should be identified as quotaCritical
	// and skipped in favor of freshAcc.
	selection := manager.Select("gemini-3.8-flash-high[1m]")
	if selection.Account == nil {
		t.Fatalf("expected an account to be selected")
	}
	if selection.Account.Email != "fresh@example.com" {
		t.Fatalf("expected fresh@example.com, got %s (quota critical protection failed for [1m] suffix)", selection.Account.Email)
	}
}

func TestModelKeyCandidates(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{
			input: "gemini-3.8-flash-high[1m]",
			want:  []string{"gemini-3.8-flash-high[1m]", "gemini-3.8-flash-high"},
		},
		{
			input: "Gemini-3.8-Flash-High",
			want:  []string{"Gemini-3.8-Flash-High", "gemini-3.8-flash-high"},
		},
		{
			input: "gemini-3.8-flash-high",
			want:  []string{"gemini-3.8-flash-high"},
		},
		{
			input: "Claude-Opus-4-6[1m]",
			want:  []string{"Claude-Opus-4-6[1m]", "Claude-Opus-4-6", "claude-opus-4-6"},
		},
	}

	for _, tc := range cases {
		got := ModelKeyCandidates(tc.input)
		if len(got) != len(tc.want) {
			t.Fatalf("ModelKeyCandidates(%q) len = %d, want %d (%v vs %v)", tc.input, len(got), len(tc.want), got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ModelKeyCandidates(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

// Finding 1. A three-hour-old record claiming full capacity must not escape the
// freshness guard just because its ResetTime has not arrived yet.
func TestManager_StaleFullQuota_IsNotUsable(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	full := 1.0

	acc := &Account{
		Email:   "stale-full@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &full,
					ResetTime:         clock.Add(4 * time.Hour).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-3 * time.Hour).UnixMilli()),
		},
	}

	manager, err := New(Options{Accounts: []*Account{acc}, Strategy: StrategyHybrid, Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	usable := manager.quotaUsableLocked(acc.Quota.Models["gemini-3.8-flash-high"], acc.Quota.LastChecked, 0.05)
	manager.mu.Unlock()

	if usable {
		t.Errorf("stale full-capacity quota reported usable; the 5 minute freshness guard was bypassed")
	}

	// And the staleness penalty must still land in scoring.
	manager.mu.Lock()
	score := manager.scoreLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()
	wQuota := mapFloat(manager.selectionConfig.Weights, "quota", 3)
	penalizedQuota := 1.0 * 100 * 0.9 * wQuota
	// Base score without quota: health (70 * 2 = 140) + tokens (50/50 * 100 * 5 = 500) + lru (3600 * 0.1 = 360) = 1000
	// Total score with penalized quota = 1000 + 270 = 1270. Unpenalized would be 1000 + 300 = 1300.
	unpenalizedQuota := 1.0 * 100 * 1.0 * wQuota
	if score > 1000+penalizedQuota+1e-9 {
		t.Errorf("scoreLocked = %f, want <= %f (stale penalty not applied)", score, 1000+penalizedQuota)
	}
	if score >= 1000+unpenalizedQuota-1e-9 {
		t.Errorf("scoreLocked = %f, expected stale penalty below %f", score, 1000+unpenalizedQuota)
	}
}

// The behaviour the PR did intend: exhaustion outlives the freshness window.
func TestManager_StaleExhaustedQuota_RemainsCritical(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	zero := 0.0

	acc := &Account{
		Email:   "stale-exhausted@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &zero,
					ResetTime:         clock.Add(30 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-3 * time.Hour).UnixMilli()),
		},
	}

	manager, err := New(Options{Accounts: []*Account{acc}, Strategy: StrategyHybrid, Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	critical := manager.quotaCriticalLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()

	if !critical {
		t.Errorf("exhausted quota with a future ResetTime should stay critical past the freshness window")
	}
}

func TestManager_QuotaResetTimeInPast_FallsBackToQuotaFresh(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	one := 1.0

	// Account checked 1 minute ago (fresh), but ResetTime was 10 minutes ago.
	acc := &Account{
		Email:   "past-reset@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &one,
					ResetTime:         clock.Add(-10 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-1 * time.Minute).UnixMilli()),
		},
	}

	manager, err := New(Options{
		Accounts: []*Account{acc},
		Strategy: StrategyHybrid,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Score should not have the 10% stale quota penalty applied.
	manager.mu.Lock()
	score := manager.scoreLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()
	// Expected quotaScore = 100 * wQuota (3) = 300. If stale penalty was applied, quotaScore = 90 * 3 = 270.
	wQuota := mapFloat(manager.selectionConfig.Weights, "quota", 3)
	expectedQuotaComponent := 1.0 * 100 * wQuota
	if score < expectedQuotaComponent {
		t.Errorf("scoreLocked = %f, expected at least quota component %f (penalty was incorrectly applied)", score, expectedQuotaComponent)
	}
}

func TestManager_QuotaCritical_ResetTimeInPast(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	zero := 0.0

	// Account exhausted at 11:50 with reset scheduled for 11:55. Now is 12:00.
	acc := &Account{
		Email:   "exhausted-past-reset@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &zero,
					ResetTime:         clock.Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-10 * time.Minute).UnixMilli()),
		},
	}

	manager, err := New(Options{
		Accounts: []*Account{acc},
		Strategy: StrategyHybrid,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	critical := manager.quotaCriticalLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()
	if critical {
		t.Errorf("expected quotaCriticalLocked to return false when ResetTime is in the past")
	}
}

// Finding 1b. The mirror failure: an elapsed ResetTime must not revoke a
// reading that quotaFresh accepts. 2% remaining, measured one second ago.
func TestManager_FreshLowQuota_ElapsedResetTime_StaysCritical(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	low := 0.02

	acc := &Account{
		Email:   "fresh-low@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &low,
					ResetTime:         clock.Add(-1 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-1 * time.Second).UnixMilli()),
		},
	}

	manager, err := New(Options{Accounts: []*Account{acc}, Strategy: StrategyHybrid, Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	critical := manager.quotaCriticalLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()

	if !critical {
		t.Errorf("a one-second-old 2%%-remaining quota must stay critical; an elapsed ResetTime overrode the measured fraction")
	}
}

// Finding 4. The ModelThreshold normalization path shipped untested.
func TestModelThresholdFor_NormalizesSuffixAndCase(t *testing.T) {
	acc := &Account{
		Email: "threshold@example.com",
		ModelThreshold: map[string]float64{
			"gemini-3.8-flash-high": 0.25,
		},
	}

	for _, query := range []string{
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-high[1m]",
		"Gemini-3.8-Flash-High",
		"Gemini-3.8-Flash-High[1m]",
	} {
		got, ok := modelThresholdFor(acc, query)
		if !ok || got != 0.25 {
			t.Errorf("modelThresholdFor(%q) = (%v, %v), want (0.25, true)", query, got, ok)
		}
	}

	if _, ok := modelThresholdFor(acc, "claude-opus-4-6"); ok {
		t.Errorf("modelThresholdFor should not resolve an unrelated model")
	}
}
