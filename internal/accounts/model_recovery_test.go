package accounts

import (
	"context"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

const (
	recoveryModel = "claude-sonnet-5-5-high"
	// catalogWithoutModel is what a poisoned shared fetch returns.
	catalogWithoutModel = `{
		"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-4-6"]}]}],
		"models":{"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}}
	}`
	catalogWithModel = `{
		"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-4-6","claude-sonnet-5-5-high"]}]}],
		"models":{
			"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000},
			"claude-sonnet-5-5-high":{"displayName":"Claude Sonnet 5.5 (High)","supportsThinking":true,"supportsAdaptiveThinking":true,"thinkingLevel":3,"maxTokens":1000000,"maxOutputTokens":128000,"quotaInfo":{"remainingFraction":1}}
		}
	}`
)

// recoveryPool builds a two-account pool whose sticky selection seeds the
// shared catalog from account A (which does not publish recoveryModel) while
// the quota caches of A and B both list it. B publishes it.
func recoveryPool(t *testing.T, bBody string) (dispatcher *Dispatcher, a, b *scriptedClient, manager *Manager, clock *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	clock = &now
	accountA, accountB := testAccount("a@example.com"), testAccount("b@example.com")
	var err error
	manager, err = New(Options{Accounts: []*Account{accountA, accountB}, Strategy: StrategySticky, Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	a = &scriptedClient{modelsBody: []byte(catalogWithoutModel)}
	b = &scriptedClient{modelsBody: []byte(bBody)}
	resolver := &staticResolver{tokens: map[string]string{accountA.Email: "tok-a", accountB.Email: "tok-b"}}
	dispatcher = newTestDispatcher(t, manager, resolver, map[string]*scriptedClient{"tok-a": a, "tok-b": b}, now, nil)
	dispatcher.now = func() time.Time { return *clock }
	if _, err := dispatcher.fetchAvailableModels(context.Background()); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}
	for _, email := range []string{accountA.Email, accountB.Email} {
		manager.UpdateAccountQuota(email, Quota{Models: map[string]ModelQuota{recoveryModel: {}}}, nil)
	}
	return dispatcher, a, b, manager, clock
}

// A catalog poisoned by an account that does not publish a model recovers
// through an account whose quota cache lists it, and the recovery fetch's
// success is recorded on the live pool account.
func TestResolveModelRecoversPoisonedCatalogViaQuotaCache(t *testing.T) {
	t.Parallel()
	dispatcher, a, b, _, _ := recoveryPool(t, catalogWithModel)
	aSeed := a.modelsCallCount()

	model, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if model.ID != recoveryModel {
		t.Fatalf("resolved %q, want %q", model.ID, recoveryModel)
	}
	if got := b.modelsCallCount(); got != 1 {
		t.Errorf("B models calls = %d, want 1", got)
	}
	if got := a.modelsCallCount(); got != aSeed+1 {
		t.Errorf("A recovery fetches = %d, want 1 (A lists the model too and is tried first)", got-aSeed)
	}
}

// A client retrying a model that no account actually publishes must not drive
// an upstream fetch per request: attempts are debounced per model.
func TestResolveModelRecoveryIsDebouncedPerModel(t *testing.T) {
	t.Parallel()
	dispatcher, a, b, manager, clock := recoveryPool(t, catalogWithoutModel)
	seed := a.modelsCallCount()

	// A fetch rewrites the account's quota cache from the response, dropping
	// the model; re-seed so only the debounce can stop the next attempt.
	resolve := func() {
		t.Helper()
		for _, email := range []string{"a@example.com", "b@example.com"} {
			manager.UpdateAccountQuota(email, Quota{Models: map[string]ModelQuota{recoveryModel: {}}}, nil)
		}
		if _, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil); !isSelectionError(err, recoveryModel) {
			t.Fatalf("err = %v, want selection error", err)
		}
	}
	// One recovery pass visits both accounts that list the model (A, then B).
	for range 3 {
		resolve()
	}
	if got := a.modelsCallCount() - seed; got != 1 {
		t.Errorf("A recovery fetches inside the floor = %d, want 1", got)
	}
	if got := b.modelsCallCount(); got != 1 {
		t.Errorf("B recovery fetches inside the floor = %d, want 1", got)
	}

	*clock = clock.Add(missingModelFetchFloor + time.Second)
	resolve()
	if got := b.modelsCallCount(); got != 2 {
		t.Errorf("B fetches after the floor = %d, want 2", got)
	}
}

// A failing account is recorded on the live pool state and skipped; the next
// account that lists the model still recovers the catalog.
func TestResolveModelRecoverySkipsFailingAccountAndRecordsFailure(t *testing.T) {
	t.Parallel()
	dispatcher, a, b, manager, _ := recoveryPool(t, catalogWithModel)
	a.mu.Lock()
	a.modelsErrAfter = a.modelsCalls // every later call fails
	a.mu.Unlock()

	model, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if model.ID != recoveryModel {
		t.Fatalf("resolved %q, want %q", model.ID, recoveryModel)
	}
	if got := b.modelsCallCount(); got != 1 {
		t.Errorf("B models calls = %d, want 1", got)
	}
	var failures int
	for _, account := range manager.AccountsWithQuotaModel(recoveryModel) {
		if account.Email == "a@example.com" {
			failures = manager.FailureCount(account)
		}
	}
	if failures == 0 {
		t.Error("failed recovery fetch left no failure on the live pool account")
	}
}

func TestAccountsWithQuotaModel_MatchesTieredPrefix(t *testing.T) {
	t.Parallel()
	mgr, err := New(Options{
		Accounts: []*Account{
			{
				Email:   "capable@example.com",
				Enabled: true,
				Quota: Quota{
					Models: map[string]ModelQuota{
						"claude-sonnet-5-5-high": {},
					},
				},
			},
			{
				Email:   "legacy@example.com",
				Enabled: true,
				Quota: Quota{
					Models: map[string]ModelQuota{
						"claude-sonnet-4-6": {},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Bare family request "claude-sonnet-5-5" must find capable@example.com
	accs := mgr.AccountsWithQuotaModel("claude-sonnet-5-5")
	if len(accs) != 1 || accs[0].Email != "capable@example.com" {
		t.Fatalf("expected [capable@example.com], got %v", accs)
	}

	// Exact match "claude-sonnet-5-5-high" must also find capable@example.com
	accsExact := mgr.AccountsWithQuotaModel("claude-sonnet-5-5-high")
	if len(accsExact) != 1 || accsExact[0].Email != "capable@example.com" {
		t.Fatalf("expected [capable@example.com], got %v", accsExact)
	}

	// Request for "claude-sonnet-4-6" must find legacy@example.com
	accsLegacy := mgr.AccountsWithQuotaModel("claude-sonnet-4-6")
	if len(accsLegacy) != 1 || accsLegacy[0].Email != "legacy@example.com" {
		t.Fatalf("expected [legacy@example.com], got %v", accsLegacy)
	}
}

func TestAccountsWithQuotaModel_MatchesDotNotation(t *testing.T) {
	t.Parallel()
	mgr, err := New(Options{
		Accounts: []*Account{
			{
				Email:   "capable@example.com",
				Enabled: true,
				Quota: Quota{
					Models: map[string]ModelQuota{
						"claude-sonnet-5-5-high": {},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, req := range []string{"claude-sonnet-5.5", "claude-sonnet-5.5-high"} {
		accs := mgr.AccountsWithQuotaModel(req)
		if len(accs) != 1 || accs[0].Email != "capable@example.com" {
			t.Errorf("AccountsWithQuotaModel(%q) = %v, want [capable@example.com]", req, accs)
		}
	}
}

// A family prefix is not a tier: "claude-sonnet-5" must not match an account
// that only holds "claude-sonnet-5-5-high", and "claude" must match nothing.
func TestAccountsWithQuotaModel_RejectsNonTierPrefix(t *testing.T) {
	t.Parallel()
	mgr, err := New(Options{Accounts: []*Account{{
		Email:   "capable@example.com",
		Enabled: true,
		Quota:   Quota{Models: map[string]ModelQuota{"claude-sonnet-5-5-high": {}}},
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, req := range []string{"claude-sonnet-5", "claude", "claude-sonnet"} {
		if accs := mgr.AccountsWithQuotaModel(req); len(accs) != 0 {
			t.Errorf("AccountsWithQuotaModel(%q) = %v, want none", req, accs)
		}
	}
	for _, req := range []string{"claude-sonnet-5-5", "claude-sonnet-5-5-high", "claude-sonnet-5.5-high", "claude-sonnet-5-5-high[1m]", "Claude-Sonnet-5-5-High"} {
		if accs := mgr.AccountsWithQuotaModel(req); len(accs) != 1 {
			t.Errorf("AccountsWithQuotaModel(%q) = %v, want [capable@example.com]", req, accs)
		}
	}
}

// An account in a model-listing cooldown is not a recovery candidate: recovery
// must not spend a fetch on it, and still recovers through the other account.
func TestResolveModelRecoverySkipsCooledDownAccount(t *testing.T) {
	t.Parallel()
	dispatcher, a, b, manager, _ := recoveryPool(t, catalogWithModel)
	manager.MarkRateLimited(manager.accounts[0], "", time.Hour) // a@example.com
	aBefore := a.modelsCallCount()

	model, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil)
	if err != nil || model.ID != recoveryModel {
		t.Fatalf("resolveModel = %q, %v; want %q", model.ID, err, recoveryModel)
	}
	if got := a.modelsCallCount() - aBefore; got != 0 {
		t.Errorf("cooled-down account fetched %d times during recovery, want 0", got)
	}
	if got := b.modelsCallCount(); got != 1 {
		t.Errorf("B models calls = %d, want 1", got)
	}
}

func setModelsErr(c *scriptedClient, err error) {
	c.mu.Lock()
	c.modelsErr = err
	c.mu.Unlock()
}

// Each recovery fetch is bounded by the per-account budget, not just the
// caller's context.
func TestRecoveryFetchHasPerAccountDeadline(t *testing.T) {
	t.Parallel()
	dispatcher, _, b, _, _ := recoveryPool(t, catalogWithModel)
	if _, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil); err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	b.mu.Lock()
	budget := b.modelsBudget
	b.mu.Unlock()
	if budget <= 0 || budget > dispatcher.recoveryAccountTimeout {
		t.Errorf("recovery fetch budget = %v, want in (0, %v]", budget, dispatcher.recoveryAccountTimeout)
	}
}

// An account that hangs until its own budget expires is counted as a failure,
// so the next recovery tries it last; recovery still succeeds through B.
func TestRecoveryPenalizesHungAccount(t *testing.T) {
	t.Parallel()
	dispatcher, a, _, manager, _ := recoveryPool(t, catalogWithModel)
	dispatcher.recoveryAccountTimeout = 50 * time.Millisecond
	a.mu.Lock()
	a.modelsHang = true
	a.mu.Unlock()

	model, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil)
	if err != nil || model.ID != recoveryModel {
		t.Fatalf("resolveModel = %q, %v; want %q", model.ID, err, recoveryModel)
	}
	if got := manager.FailureCount(manager.accounts[0]); got != 1 {
		t.Errorf("hung account failures = %d, want 1", got)
	}
	order := manager.AccountsWithQuotaModel(recoveryModel)
	if len(order) != 2 || order[0].Email != "b@example.com" {
		t.Errorf("candidate order after hang = %v, want b@example.com first", order)
	}
}

// Upstream HTTP errors from a recovery fetch reach the pool state: 401
// invalidates the cached credentials, 429 puts the account in a listing
// cooldown that removes it from the next recovery.
func TestRecoveryRecordsUpstreamHTTPErrors(t *testing.T) {
	t.Parallel()
	t.Run("401 invalidates credentials", func(t *testing.T) {
		t.Parallel()
		dispatcher, a, _, _, _ := recoveryPool(t, catalogWithModel)
		setModelsErr(a, &cloudcode.HTTPError{StatusCode: 401, Status: "401 Unauthorized",
			Body: `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`})
		if _, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil); err != nil {
			t.Fatalf("resolveModel: %v", err)
		}
		resolver := dispatcher.resolver.(*staticResolver)
		resolver.mu.Lock()
		invalidated := append([]string(nil), resolver.invalidated...)
		resolver.mu.Unlock()
		if len(invalidated) != 1 || invalidated[0] != "a@example.com" {
			t.Errorf("invalidated = %v, want [a@example.com]", invalidated)
		}
	})
	t.Run("429 cools the account down", func(t *testing.T) {
		t.Parallel()
		dispatcher, a, _, manager, _ := recoveryPool(t, catalogWithModel)
		setModelsErr(a, &cloudcode.HTTPError{StatusCode: 429, Status: "429 Too Many Requests",
			Body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`})
		if _, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil); err != nil {
			t.Fatalf("resolveModel: %v", err)
		}
		for _, account := range manager.AccountsWithQuotaModel(recoveryModel) {
			if account.Email == "a@example.com" {
				t.Error("account that answered 429 is still a recovery candidate")
			}
		}
	})
}
