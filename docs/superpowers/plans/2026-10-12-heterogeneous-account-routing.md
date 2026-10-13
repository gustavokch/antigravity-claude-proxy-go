# Heterogeneous Account Routing & Catalog Unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent upstream 404s and client fallbacks on Claude 5.5 by filtering account selection by model capability, rotating accounts on upstream 404, matching tiered prefixes in quota recovery, and merging catalog discoveries across accounts.

**Architecture:** Heterogeneous account pools where one account has been migrated to Claude 5.5 by Google Cloud Code and another remains on Claude 4.6 cause 404 errors when requests for Claude 5.5 route to unmigrated accounts. The solution unifies catalogs across account queries (`Catalog.Merge`), verifies model capability in account selection (`Manager.Select`), rotates on upstream 404 in `rotateForError` while marking the model unavailable on that account, and extends `AccountsWithQuotaModel` to match family tier prefixes.

**Tech Stack:** Go 1.27rc2, standard library `net/http`, internal packages `modelcatalog`, `accounts`, `cloudcode`.

**Spec:** Inline analysis from live probe data: Google Cloud Code provides Claude 5.5 only to migrated accounts and returns `404 Not Found: Requested entity was not found.` on unmigrated accounts.

## Global Constraints

- Never modify TLS internals: standard HTTP transport with empty `tls.Config{}` only.
- Adhere to ASD-STE100 Simplified Technical English in all communication.
- Backward compatibility: preserve all existing unit and integration test behaviors (`go test ./...` must pass).
- No placeholders, stubs, or TODOs.
- Each task must follow strict TDD: failing test first, verification of failure, minimal implementation, verification of pass, then commit.

---

### Task 1: Catalog Merging Across Accounts (`modelcatalog.Catalog.Merge`)

**Files:**
- Modify: `internal/modelcatalog/catalog.go:40-60, 230-260`
- Test: `internal/modelcatalog/catalog_test.go`

**Interfaces:**
- Consumes: `modelcatalog.Catalog`, `modelcatalog.Model`
- Produces: `func (c *Catalog) Merge(other *Catalog) *Catalog`

- [ ] **Step 1: Write the failing test**

Add `TestCatalog_MergeUnionsDisjointModels` to `internal/modelcatalog/catalog_test.go`:

```go
func TestCatalog_MergeUnionsDisjointModels(t *testing.T) {
	catA := &Catalog{
		defaultID: "claude-sonnet-5-5-high",
		selectable: []Model{
			{ID: "claude-sonnet-5-5-high", DisplayName: "Claude Sonnet 5.5 (High)"},
			{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
		},
		byID: map[string]Model{
			"claude-sonnet-5-5-high": {ID: "claude-sonnet-5-5-high", DisplayName: "Claude Sonnet 5.5 (High)"},
			"gemini-3.8-flash-high":  {ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
		},
		byDisplay: map[string]Model{
			"claude sonnet 5.5 (high)": {ID: "claude-sonnet-5-5-high", DisplayName: "Claude Sonnet 5.5 (High)"},
			"gemini 3.8 flash (high)":  {ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
		},
	}

	catB := &Catalog{
		defaultID: "claude-sonnet-4-6",
		selectable: []Model{
			{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
			{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High) Updated"},
		},
		byID: map[string]Model{
			"claude-sonnet-4-6":     {ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
			"gemini-3.8-flash-high": {ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High) Updated"},
		},
		byDisplay: map[string]Model{
			"claude sonnet 4.6":              {ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
			"gemini 3.8 flash (high) updated": {ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High) Updated"},
		},
	}

	merged := catA.Merge(catB)
	if merged == nil {
		t.Fatal("expected non-nil merged catalog")
	}

	// Must retain Claude 5.5 from catA
	if m, err := merged.Resolve("claude-sonnet-5-5-high"); err != nil || m.ID != "claude-sonnet-5-5-high" {
		t.Fatalf("expected claude-sonnet-5-5-high preserved, got err=%v m=%+v", err, m)
	}

	// Must include Claude 4.6 from catB
	if m, err := merged.Resolve("claude-sonnet-4-6"); err != nil || m.ID != "claude-sonnet-4-6" {
		t.Fatalf("expected claude-sonnet-4-6 merged in, got err=%v m=%+v", err, m)
	}

	// Overlapping model must be updated from catB
	if m, err := merged.Resolve("gemini-3.8-flash-high"); err != nil || m.DisplayName != "Gemini 3.8 Flash (High) Updated" {
		t.Fatalf("expected updated display name, got err=%v m=%+v", err, m)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestCatalog_MergeUnionsDisjointModels ./internal/modelcatalog`
Expected: FAIL with `catA.Merge undefined (type *Catalog has no field or method Merge)`

- [ ] **Step 3: Write minimal implementation**

In `internal/modelcatalog/catalog.go`, implement `Merge`:

```go
// Merge returns a new Catalog that combines models from c and other. Models in
// other override or update models in c with the same ID, while models unique to
// c are preserved. If c is nil, other is returned. If other is nil, c is returned.
func (catalog *Catalog) Merge(other *Catalog) *Catalog {
	if catalog == nil {
		return other
	}
	if other == nil {
		return catalog
	}

	merged := &Catalog{
		defaultID: catalog.defaultID,
		byID:      make(map[string]Model, len(catalog.byID)+len(other.byID)),
		byDisplay: make(map[string]Model, len(catalog.byDisplay)+len(other.byDisplay)),
	}
	if other.defaultID != "" {
		merged.defaultID = other.defaultID
	}

	// Copy base models
	for k, v := range catalog.byID {
		merged.byID[k] = v
	}
	for k, v := range catalog.byDisplay {
		merged.byDisplay[k] = v
	}

	// Overlay incoming models
	for k, v := range other.byID {
		merged.byID[k] = v
	}
	for k, v := range other.byDisplay {
		merged.byDisplay[k] = v
	}

	// Rebuild selectable list preserving order without duplicate IDs
	seen := make(map[string]bool)
	var selectable []Model
	for _, m := range other.selectable {
		idLower := strings.ToLower(m.ID)
		if !seen[idLower] {
			seen[idLower] = true
			if updated, ok := merged.byID[idLower]; ok {
				selectable = append(selectable, updated)
			} else {
				selectable = append(selectable, m)
			}
		}
	}
	for _, m := range catalog.selectable {
		idLower := strings.ToLower(m.ID)
		if !seen[idLower] {
			seen[idLower] = true
			if updated, ok := merged.byID[idLower]; ok {
				selectable = append(selectable, updated)
			} else {
				selectable = append(selectable, m)
			}
		}
	}
	merged.selectable = selectable

	return merged
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v -run TestCatalog_MergeUnionsDisjointModels ./internal/modelcatalog`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "feat(modelcatalog): support merging catalogs across heterogeneous accounts"
```

---

### Task 2: Tiered Prefix & Model Matching in `AccountsWithQuotaModel`

**Files:**
- Modify: `internal/accounts/manager.go:1315-1340`
- Test: `internal/accounts/model_recovery_test.go`

**Interfaces:**
- Consumes: `Account.Quota.Models`
- Produces: `AccountsWithQuotaModel(model string) []*Account` matching family prefixes

- [ ] **Step 1: Write the failing test**

Add `TestAccountsWithQuotaModel_MatchesTieredPrefix` to `internal/accounts/model_recovery_test.go`:

```go
func TestAccountsWithQuotaModel_MatchesTieredPrefix(t *testing.T) {
	mgr, err := New(Options{
		Accounts: []*Account{
			{
				Email:   "capable@example.com",
				Enabled: true,
				Quota: Quota{
					Models: map[string]ModelQuota{
						"claude-sonnet-5-5-high": {RemainingFraction: 1.0},
					},
				},
			},
			{
				Email:   "legacy@example.com",
				Enabled: true,
				Quota: Quota{
					Models: map[string]ModelQuota{
						"claude-sonnet-4-6": {RemainingFraction: 1.0},
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestAccountsWithQuotaModel_MatchesTieredPrefix ./internal/accounts`
Expected: FAIL on bare family lookup `claude-sonnet-5-5` returning empty slice.

- [ ] **Step 3: Write minimal implementation**

In `internal/accounts/manager.go`, update `AccountsWithQuotaModel`:

```go
// accountSupportsQuotaModel reports whether account's quota cache contains
// model, matching on the exact ID, its canonical rate-limit key, or as a
// tiered prefix (e.g. "claude-sonnet-5-5" matches "claude-sonnet-5-5-high").
func accountSupportsQuotaModel(account *Account, model string) bool {
	if account == nil || account.Quota.Models == nil {
		return false
	}
	canon := rateLimitModelKey(model)
	trimmed := strings.ToLower(strings.TrimSpace(model))
	prefix := trimmed + "-"
	canonPrefix := ""
	if canon != "" {
		canonPrefix = canon + "-"
	}

	for k := range account.Quota.Models {
		kLower := strings.ToLower(k)
		if kLower == trimmed || (canon != "" && kLower == canon) {
			return true
		}
		if trimmed != "" && strings.HasPrefix(kLower, prefix) {
			return true
		}
		if canonPrefix != "" && strings.HasPrefix(kLower, canonPrefix) {
			return true
		}
	}
	return false
}

func (manager *Manager) AccountsWithQuotaModel(model string) []*Account {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	var result []*Account
	for _, account := range manager.accounts {
		if !account.Enabled || account.IsInvalid {
			continue
		}
		if accountSupportsQuotaModel(account, model) {
			result = append(result, account)
		}
	}
	return result
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v -run TestAccountsWithQuotaModel_MatchesTieredPrefix ./internal/accounts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/model_recovery_test.go
git commit -m "feat(accounts): support prefix matching for tiered quota models in recovery"
```

---

### Task 3: Capability-Aware Account Selection in `Manager.Select`

**Files:**
- Modify: `internal/accounts/manager.go:885-915`
- Test: `internal/accounts/manager_test.go`

**Interfaces:**
- Consumes: `usableLocked(account *Account, model string) bool`
- Produces: Model capability filtering in `usableLocked` when pool accounts have recorded quota for the requested model.

- [ ] **Step 1: Write the failing test**

Add `TestManager_SelectFiltersAccountsByModelSupport` to `internal/accounts/manager_test.go`:

```go
func TestManager_SelectFiltersAccountsByModelSupport(t *testing.T) {
	capable := &Account{
		Email:   "capable@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"claude-sonnet-5-5-high": {RemainingFraction: 1.0},
				"gemini-3.8-flash-high":  {RemainingFraction: 1.0},
			},
		},
	}
	legacy := &Account{
		Email:   "legacy@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"claude-sonnet-4-6":     {RemainingFraction: 1.0},
				"gemini-3.8-flash-high": {RemainingFraction: 1.0},
			},
		},
	}
	mgr, err := New(Options{
		Accounts: []*Account{legacy, capable},
		Strategy: "round-robin",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Requesting Claude 5.5 must NEVER select legacy account
	for i := 0; i < 5; i++ {
		sel := mgr.Select("claude-sonnet-5-5-high")
		if sel.Account == nil || sel.Account.Email != "capable@example.com" {
			t.Fatalf("iteration %d: expected capable@example.com, got %v", i, sel.Account)
		}
	}

	// Requesting Claude 4.6 must select legacy account
	sel46 := mgr.Select("claude-sonnet-4-6")
	if sel46.Account == nil || sel46.Account.Email != "legacy@example.com" {
		t.Fatalf("expected legacy@example.com, got %v", sel46.Account)
	}

	// When capable is rate limited for claude-5-5, selection must NOT fall back to legacy
	mgr.MarkRateLimited(capable, "claude-sonnet-5-5-high", time.Minute)
	selBlocked := mgr.Select("claude-sonnet-5-5-high")
	if selBlocked.Account != nil {
		t.Fatalf("expected nil account when capable is rate limited, got %v", selBlocked.Account.Email)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v -run TestManager_SelectFiltersAccountsByModelSupport ./internal/accounts`
Expected: FAIL because `legacy@example.com` is selected by round-robin for `claude-sonnet-5-5-high`.

- [ ] **Step 3: Write minimal implementation**

In `internal/accounts/manager.go`:

Add `poolSupportsModelLocked` helper:
```go
// poolSupportsModelLocked reports whether any active, valid account in the
// manager's pool has explicitly recorded quota for model (or its prefix/key).
func (manager *Manager) poolSupportsModelLocked(model string) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	for _, account := range manager.accounts {
		if account == nil || !account.Enabled || account.IsInvalid {
			continue
		}
		if accountSupportsQuotaModel(account, model) {
			return true
		}
	}
	return false
}
```

Update `usableLocked`:
```go
func (manager *Manager) usableLocked(account *Account, model string) bool {
	if account == nil || !account.Enabled || account.IsInvalid {
		return false
	}
	now := manager.now().UnixMilli()
	if account.CoolingDownUntilMS > now {
		return false
	}
	if limit := account.ModelRateLimits[rateLimitModelKey(model)]; limit != nil && limit.IsRateLimited && limit.ResetTimeMS > now {
		return false
	}
	// In a heterogeneous pool where at least one account explicitly supports
	// model, filter out accounts whose non-empty quota models do not support it.
	// Accounts with uninitialized quota (len == 0) remain usable to allow initial discovery.
	if strings.TrimSpace(model) != "" && len(account.Quota.Models) > 0 {
		if manager.poolSupportsModelLocked(model) && !accountSupportsQuotaModel(account, model) {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v -run TestManager_SelectFiltersAccountsByModelSupport ./internal/accounts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/manager_test.go
git commit -m "feat(accounts): filter account selection by model quota capability"
```

---

### Task 4: Upstream HTTP 404 Rotation & Merged Catalog Storage in `Dispatcher`

**Files:**
- Modify: `internal/accounts/manager.go:580-600` (add `MarkModelUnavailable`)
- Modify: `internal/accounts/dispatcher.go:634-646, 855-875`
- Test: `internal/accounts/dispatcher_test.go`

**Interfaces:**
- Consumes: `rotateForError(account *Account, model string, err error) bool`, `storeCatalog(catalog *modelcatalog.Catalog)`
- Produces: HTTP 404 rotation with model-specific unavailability cooldown, merged catalog storage

- [ ] **Step 1: Write the failing tests**

In `internal/accounts/dispatcher_test.go`:

Add `TestStreamGenerateContent_404RotatesToCapableAccount`:
```go
type notFoundThenSuccessClient struct {
	blockingModelsClient
	email string
}

func (c *notFoundThenSuccessClient) StreamGenerateContent(ctx context.Context, request any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	if c.email == "acc1@example.com" {
		return cloudcode.Response{}, &cloudcode.HTTPError{
			StatusCode: 404,
			Body:       `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`,
		}
	}
	return cloudcode.Response{}, nil
}

func TestStreamGenerateContent_404RotatesToCapableAccount(t *testing.T) {
	release := make(chan struct{})
	close(release)
	manager, err := New(Options{
		Accounts: []*Account{
			{Email: "acc1@example.com", Enabled: true},
			{Email: "acc2@example.com", Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:  manager,
		Resolver: stubResolver{},
		NewClient: func(email string) CloudClient {
			return &notFoundThenSuccessClient{
				blockingModelsClient: blockingModelsClient{release: release},
				email:                email,
			}
		},
		ProjectID: "test-proj-404",
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}

	ctx, meta := cloudcode.WithExecutionMetadata(context.Background())
	req := map[string]any{
		"model": "claude-sonnet-5-5-high",
	}

	_, err = dispatcher.StreamGenerateContent(ctx, req, func(cloudcode.SSEEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamGenerateContent failed on 404 failover: %v", err)
	}

	if meta.Account != "acc2@example.com" {
		t.Errorf("meta.Account = %q, want %q", meta.Account, "acc2@example.com")
	}
}
```

Add `TestStoreCatalog_MergesWithExistingCatalog`:
```go
func TestStoreCatalog_MergesWithExistingCatalog(t *testing.T) {
	manager, err := New(Options{
		Accounts: []*Account{{Email: "acc1@example.com", Enabled: true}},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:  manager,
		Resolver: stubResolver{},
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}

	catA := []byte(`{
		"models": {
			"claude-sonnet-5-5-high": {"displayName": "Claude Sonnet 5.5 (High)"}
		},
		"agentModelSorts": [{"groups": [{"modelIds": ["claude-sonnet-5-5-high"]}]}]
	}`)
	catB := []byte(`{
		"models": {
			"claude-sonnet-4-6": {"displayName": "Claude Sonnet 4.6"}
		},
		"agentModelSorts": [{"groups": [{"modelIds": ["claude-sonnet-4-6"]}]}]
	}`)

	dispatcher.cacheCatalog(catA)
	dispatcher.cacheCatalog(catB)

	cached := dispatcher.CachedCatalog()
	if cached == nil {
		t.Fatal("expected cached catalog")
	}
	if _, err := cached.Resolve("claude-sonnet-5-5-high"); err != nil {
		t.Fatalf("expected claude-sonnet-5-5-high preserved after catB update, err=%v", err)
	}
	if _, err := cached.Resolve("claude-sonnet-4-6"); err != nil {
		t.Fatalf("expected claude-sonnet-4-6 present from catB, err=%v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v -run "TestStreamGenerateContent_404RotatesToCapableAccount|TestStoreCatalog_MergesWithExistingCatalog" ./internal/accounts`
Expected: FAIL (`TestStreamGenerateContent_404RotatesToCapableAccount` fails with 404 abort; `TestStoreCatalog_MergesWithExistingCatalog` fails because catA was overwritten by catB).

- [ ] **Step 3: Write minimal implementation**

1. In `internal/accounts/manager.go`, implement `MarkModelUnavailable`:
```go
// MarkModelUnavailable applies a 24-hour rate-limit block for model on account
// and removes it from account.Quota.Models when upstream returns 404 Not Found.
func (manager *Manager) MarkModelUnavailable(account *Account, model string) {
	if account == nil {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()

	key := rateLimitModelKey(model)
	if key != "" {
		account.ModelRateLimits[key] = &RateLimit{
			IsRateLimited: true,
			ResetTimeMS:   manager.now().Add(24 * time.Hour).UnixMilli(),
			ActualResetMS: (24 * time.Hour).Milliseconds(),
		}
	}
	if account.Quota.Models != nil {
		delete(account.Quota.Models, model)
		if key != "" {
			delete(account.Quota.Models, key)
		}
	}
}
```

2. In `internal/accounts/dispatcher.go`:
Update `storeCatalog`:
```go
func (dispatcher *Dispatcher) storeCatalog(catalog *modelcatalog.Catalog) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if dispatcher.catalog == nil {
		dispatcher.catalog = catalog
	} else {
		dispatcher.catalog = dispatcher.catalog.Merge(catalog)
	}
	dispatcher.catalogAt = dispatcher.now()
}
```

Update `rotateForError`:
```go
	case http.StatusBadRequest:
		return false
	case http.StatusNotFound:
		if model != "" && account != nil {
			dispatcher.manager.MarkModelUnavailable(account, model)
		}
		return true
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v -run "TestStreamGenerateContent_404RotatesToCapableAccount|TestStoreCatalog_MergesWithExistingCatalog" ./internal/accounts`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/dispatcher.go internal/accounts/dispatcher_test.go
git commit -m "fix(dispatcher): rotate on upstream 404 and merge catalogs across accounts"
```

---

### Task 5: End-to-End Test Suite Verification & Binary Build

**Files:**
- Entire repository

**Interfaces:**
- Consumes: All updated packages
- Produces: Clean `go test ./...` and compiled binary `bin/proxy`

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: All packages pass without errors or regressions.

- [ ] **Step 2: Build proxy binary**

Run: `go build -o bin/proxy ./cmd/proxy`
Expected: Clean compilation with 0 warnings.

- [ ] **Step 3: Verification commit (if needed)**

If any formatting or import adjustments were made:
```bash
git add -A
git commit -m "chore: verify test suite and build after heterogeneous routing fix"
```

---

## Plan Self-Review Checklist

1. **Spec coverage:**
   - [x] Account selection filters by model capability (Task 3).
   - [x] Upstream 404 triggers account rotation and marks model unavailable (Task 4).
   - [x] Bare family names match tiered prefix in quota recovery (Task 2).
   - [x] Multi-account catalog fetches unify instead of overwriting (Task 1 & Task 4).
2. **Placeholder scan:** No TBD, TODO, or omitted code found. All tasks contain complete code blocks.
3. **Type consistency:**
   - `Catalog.Merge(other *Catalog) *Catalog` defined in Task 1, consumed in Task 4.
   - `accountSupportsQuotaModel(account *Account, model string) bool` defined in Task 2, consumed in Task 3.
   - `manager.MarkModelUnavailable(account *Account, model string)` defined in Task 4, consumed in Task 4.
