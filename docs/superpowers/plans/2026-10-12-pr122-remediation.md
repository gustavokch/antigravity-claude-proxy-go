# PR #122 Remediation (Round 2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close every review finding still open on PR #122 at `976270d`, plus the three defects the heterogeneous-routing commits introduced, so the model-catalog recovery path and the Claude Code router are correct under concurrency, failure, and mixed account pools.

**Architecture:** Recovery becomes a single-flight background pass per model (like `startModelFetch`), with a total budget, a per-account budget, cooldown-aware candidates, and a debounce stamped only on a definitive negative. The shared catalog becomes the union of per-account snapshots so a withdrawn model leaves when its publisher refetches. Quota matching accepts only exact ids or a thinking-tier suffix, and a 404 blocks only the missing tier. The router drops the dead `isTierVariantOf` guard and refuses tier-suffixed requests in prefix matching, including `[1m]`/dated/`-latest` tails.

**Tech Stack:** Go 1.27rc2 (`go.mod`), standard library, `go test -race`.

**Spec:** Review findings below (§ Findings under remediation), derived from the PR #122 review comment (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/122#issuecomment-6134733732) and the round-2 review of `3947f8f..976270d`.

## Global Constraints

- Do NOT touch TLS internals or the HTTP transport (`AGENTS.md`: empty `tls.Config{}` only). No change in this plan touches transport, so the JA4 gate is not required.
- Push only to the `fork` remote: `git push fork fix/claude-5-5-cloud-code-routing`. Never push to `origin`.
- Staged Go files must be gofmt-clean (the opt-in hook in `scripts/git-hooks` refuses otherwise).
- Every task's tests must pass under `go test -race`.
- Do not add tests that pin wording or wiring; each test below fails on a consumer-visible defect.

## Round-1 record (corrected)

The first remediation round (`3947f8f`) is kept here for traceability, with the errors in the original text fixed:

| # | Review comment | Status |
|---|---|---|
| 1 | Unlocked read of `dispatcher.catalog` in `resolveModel` | Fixed in `3947f8f` by deleting the `dispatcher.catalog != nil` guard. Regression test added in Task 3. |
| 2 | `GetAllAccounts()` clones lose `Mark*` state | Fixed (`AccountsWithQuotaModel` returns live pointers). |
| 3 | No debounce | Debounce added, but stamped before the fetch — reworked in Task 6 (BLOCK 1). |
| 4 | Fetch unbounded by `fetchModelsTimeout` | Per-call bound added; total bound missing — Task 5 and Task 6. |
| 5 | Upstream errors dropped | `rotateForError` added; HTTP branches untested — Task 5. |
| 6 | `break` after first account | Fixed. |
| 7 | `isTierVariantOf` slices without `HasPrefix` | **Label corrected:** the old code had `len(requested) <= len(canonical)`, so there was no out-of-bounds slice; the defect was a false match on a string that does not start with `canonical`. The guard is dead in practice and is deleted in Task 7. |
| 8 | Pass `normalized` | Applied; superseded by Task 7 (guard deleted). |
| 9 | Duplicate router test entries | Consolidated; the moved `claude-sonnet-5.5-high` case passes without any guard — Task 7 adds real cases. |
| 10 | Recovery has no tests | Partial; Tasks 3, 5, 6 add the missing ones. |
| 11 | `isThinkingTier`/`isTierVariantOf` duplicate the suffix list | Superseded by Task 7. |
| 12 | `claudecode_503_logging_test.go:118` names nonexistent `cc503Request` | Fixed by `6d1f2f7` before `3947f8f`; the original plan omitted it. |

## Findings under remediation

Line numbers are at `976270d`.

| ID | Finding | Task |
|---|---|---|
| BLOCK 1 | `claimRecoveryAttempt` stamps before any fetch (`dispatcher.go:583`), no single flight: a failed pass, or a concurrent caller, gets HTTP 400 `SelectionError` (`server.go:3117`) for up to 5 min. Recovery runs on the request context. | 6 |
| H1 | `AccountsWithQuotaModel` (`manager.go:1441`) ignores cooldown and listing rate limits; recovery fetches cooled-down accounts. | 4 |
| H2 | No total recovery budget; fixed candidate order; a hung account is never penalized (`9f49434` skips it when `fetchCtx.Err() != nil`); recovery issues two live-quota RPCs per success. | 5, 6 |
| H3 | No tests for round-1 items 1, 4, 5, 7, 8. | 3, 5, 7 |
| H4 | `isTierVariantOf` protects only the dotted-alias case; step 4 re-admits exact aliases; `-high[1m]`, `-high-latest`, `-high-20260101` collapse to the bare id; no test on `DefaultAllowlist()`. | 7 |
| NOTE 5 | `missingModelFetchFloor` comment says "shared by all models" (`dispatcher.go:587-590`). | 6 |
| NEW 1 | `accountSupportsQuotaModel` accepts any `model + "-"` prefix: `claude-sonnet-5` matches an account holding only `claude-sonnet-5-5-high`; `claude` matches every Claude key. Affects selection and recovery. | 1 |
| NEW 2 | `storeCatalog` merges forever (`dispatcher.go:648-657`): a withdrawn model stays until restart. | 3 |
| NEW 3 | `MarkModelUnavailable` (`manager.go:615-661`) blocks the whole family for 24 h and deletes every sibling tier from the quota cache after one 404. | 2 |

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `internal/accounts/manager.go` | Quota matching, 404 blocking, recovery candidates | 1, 2, 4 |
| `internal/accounts/dispatcher.go` | Catalog snapshots, per-account fetch, single-flight recovery | 3, 5, 6 |
| `internal/accounts/model_recovery_test.go` | Recovery and quota-matching behavior tests | 1, 4, 5, 6 |
| `internal/accounts/manager_test.go` | `MarkModelUnavailable` test | 2 |
| `internal/accounts/dispatcher_test.go` | Catalog snapshot and race tests | 3 |
| `internal/accounts/retry_test.go` | `scriptedClient` test double (gains hang + budget capture) | 5 |
| `internal/accounts/quota_exhaustion_test.go`, `quota_merge_test.go` | Callers of `storeCatalog` | 3 |
| `internal/claudecode/router.go`, `router_test.go` | Tier refusal | 7 |
| `README.md`, `docs/reasoning-parameters.md` | Stale-catalog window description | 3 |

---

### Task 1: Quota matching accepts only exact ids or a thinking-tier suffix (NEW 1)

**Files:**
- Modify: `internal/accounts/manager.go` (`accountSupportsQuotaModel`, ~lines 1403-1435)
- Test: `internal/accounts/model_recovery_test.go`

**Interfaces:**
- Consumes: `rateLimitModelKey(model string) string`, `stripTierSuffix(model string) string` (both existing in `manager.go`).
- Produces: `quotaMatchKey(model string) string` — trimmed, `[1m]`-stripped, lowercased id with dots replaced by hyphens. Task 2 uses it.

- [ ] **Step 1: Write the failing test**

Append to `internal/accounts/model_recovery_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts/ -run TestAccountsWithQuotaModel_RejectsNonTierPrefix -count=1 -v`
Expected: FAIL — `AccountsWithQuotaModel("claude-sonnet-5") = [...], want none` and the same for `"claude"`.

- [ ] **Step 3: Write minimal implementation**

Replace the whole `accountSupportsQuotaModel` function in `internal/accounts/manager.go` with:

```go
// quotaMatchKey normalizes a model id for quota-cache matching: trimmed,
// "[1m]" stripped, lowercased, dots as hyphens ("claude-sonnet-5.5-high"
// and "claude-sonnet-5-5-high" are the same key).
func quotaMatchKey(model string) string {
	return strings.ReplaceAll(rateLimitModelKey(strings.TrimSpace(model)), ".", "-")
}

// accountSupportsQuotaModel reports whether account's quota cache lists model:
// the same key, or the key plus one thinking tier (a bare family id such as
// "claude-sonnet-5-5" matches "claude-sonnet-5-5-high"). Any other prefix is a
// different model ("claude-sonnet-5" does not match "claude-sonnet-5-5-high").
func accountSupportsQuotaModel(account *Account, model string) bool {
	if account == nil || len(account.Quota.Models) == 0 {
		return false
	}
	want := quotaMatchKey(model)
	if want == "" {
		return false
	}
	for key := range account.Quota.Models {
		have := quotaMatchKey(key)
		if have == want || stripTierSuffix(have) == want {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -run 'TestAccountsWithQuotaModel|TestManager_SelectFiltersAccountsByModelSupport' -count=1 -race -v`
Expected: PASS (new test plus the existing `MatchesTieredPrefix`, `MatchesDotNotation`, `SelectFiltersAccountsByModelSupport`).

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/model_recovery_test.go
git commit -m "fix(accounts): match quota models only by exact id or thinking-tier suffix"
```

---

### Task 2: A 404 blocks only the missing tier (NEW 3)

**Files:**
- Modify: `internal/accounts/manager.go` (`MarkModelUnavailable`, lines 613-661)
- Test: `internal/accounts/manager_test.go` (replace `TestMarkModelUnavailable_GuardsNilMapAndPurgesFamilyVariants`, ~lines 1104-1144)

**Interfaces:**
- Consumes: `quotaMatchKey` (Task 1), `rateLimitModelKey`.
- Produces: `(*Manager).MarkModelUnavailable(account *Account, model string)` — same signature, narrower effect.

- [ ] **Step 1: Write the failing test**

In `internal/accounts/manager_test.go`, delete `TestMarkModelUnavailable_GuardsNilMapAndPurgesFamilyVariants` and add:

```go
// One withdrawn tier must not take its siblings with it: after a 404 on
// claude-sonnet-5-5-high the account still serves claude-sonnet-5-5-low.
func TestMarkModelUnavailable_BlocksOnlyTheMissingTier(t *testing.T) {
	t.Parallel()
	mgr, err := New(Options{
		Strategy: StrategySticky,
		Accounts: []*Account{{
			Email:   "test@example.com",
			Enabled: true,
			// ModelRateLimits is nil: MarkModelUnavailable must allocate it.
			Quota: Quota{Models: map[string]ModelQuota{
				"Claude-Sonnet-5-5-High": {},
				"claude-sonnet-5-5-low":  {},
				"gemini-3.8-flash-high":  {},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	acc := mgr.accounts[0]
	mgr.MarkModelUnavailable(acc, "claude-sonnet-5-5-high")

	if limit := acc.ModelRateLimits["claude-sonnet-5-5-high"]; limit == nil || !limit.IsRateLimited {
		t.Errorf("claude-sonnet-5-5-high not blocked: %+v", acc.ModelRateLimits)
	}
	if _, blocked := acc.ModelRateLimits["claude-sonnet-5-5"]; blocked {
		t.Error("family key claude-sonnet-5-5 blocked; only the 404ed tier may be")
	}
	if _, ok := acc.Quota.Models["Claude-Sonnet-5-5-High"]; ok {
		t.Error("404ed tier still in the quota cache")
	}
	for _, kept := range []string{"claude-sonnet-5-5-low", "gemini-3.8-flash-high"} {
		if _, ok := acc.Quota.Models[kept]; !ok {
			t.Errorf("%s purged; only the 404ed tier may be", kept)
		}
	}
	if sel := mgr.Select("claude-sonnet-5-5-low"); sel.Account != acc {
		t.Errorf("Select(claude-sonnet-5-5-low) = %v, want the account", sel.Account)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts/ -run TestMarkModelUnavailable_BlocksOnlyTheMissingTier -count=1 -v`
Expected: FAIL — `family key claude-sonnet-5-5 blocked` and `claude-sonnet-5-5-low purged`.

- [ ] **Step 3: Write minimal implementation**

Replace the whole `MarkModelUnavailable` function in `internal/accounts/manager.go` with:

```go
// MarkModelUnavailable records an upstream 404 for model on account: the exact
// model key is blocked for 24 hours and dropped from the account's quota cache.
// Sibling tiers stay: one withdrawn tier says nothing about the others.
func (manager *Manager) MarkModelUnavailable(account *Account, model string) {
	key := rateLimitModelKey(model)
	if account == nil || key == "" {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if account.ModelRateLimits == nil {
		account.ModelRateLimits = make(map[string]*RateLimit)
	}
	account.ModelRateLimits[key] = &RateLimit{
		IsRateLimited: true,
		ResetTimeMS:   manager.now().Add(24 * time.Hour).UnixMilli(),
		ActualResetMS: (24 * time.Hour).Milliseconds(),
	}
	want := quotaMatchKey(model)
	for cached := range account.Quota.Models {
		if quotaMatchKey(cached) == want {
			delete(account.Quota.Models, cached)
		}
	}
}
```

`stripTierSuffix` stays: Task 1 uses it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -run 'TestMarkModelUnavailable|TestStreamGenerateContent_404RotatesToCapableAccount' -count=1 -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/manager_test.go
git commit -m "fix(accounts): block only the 404ed tier instead of the whole family"
```

---

### Task 3: Shared catalog is the union of per-account snapshots (NEW 2, H3 item 1)

**Files:**
- Modify: `internal/accounts/dispatcher.go` (struct lines 96-108; `fetchAvailableModels` line 318; `fetchAvailableModelsForAccount` line 359; `resolveModel` line 621; `cacheCatalog`/`storeCatalog` lines 641-657; add `"sort"` import)
- Modify callers: `internal/accounts/dispatcher_test.go:691`, `internal/accounts/quota_exhaustion_test.go:322`, `internal/accounts/quota_merge_test.go:230`
- Test: `internal/accounts/dispatcher_test.go` (replace `TestStoreCatalog_MergesWithExistingCatalog`, lines 715-759)
- Docs: `README.md:703`, `docs/reasoning-parameters.md:109`

**Interfaces:**
- Consumes: `(*modelcatalog.Catalog).Merge(other *Catalog) *Catalog` (existing; nil receiver returns `other`).
- Produces:
  - `(*Dispatcher).cacheCatalog(email string, body []byte)`
  - `(*Dispatcher).storeCatalog(email string, catalog *modelcatalog.Catalog)` — `email` is the account whose list this is; `""` means "no account known" (the parse fallback in `resolveModel` and test seeds).
  - `CachedCatalog()` returns the union of snapshots stored within `modelCacheTTL`; the snapshot just stored wins on overlapping ids and `defaultID`.

- [ ] **Step 1: Write the failing tests**

In `internal/accounts/dispatcher_test.go`, delete `TestStoreCatalog_MergesWithExistingCatalog` and add:

```go
// The shared catalog is the union of each account's latest list: a model one
// account publishes survives another account's refresh, and leaves when its
// own publisher's next list drops it.
func TestStoreCatalog_UnionsPerAccountSnapshots(t *testing.T) {
	t.Parallel()
	manager, err := New(Options{Accounts: []*Account{{Email: "acc1@example.com", Enabled: true}}})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return &blockingModelsClient{} },
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }

	with55 := []byte(`{"models":{"claude-sonnet-5-5-high":{"displayName":"Claude Sonnet 5.5 (High)"}},
		"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-5-5-high"]}]}]}`)
	only46 := []byte(`{"models":{"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6"}},
		"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-4-6"]}]}]}`)
	resolves := func(id string) bool {
		_, err := dispatcher.CachedCatalog().Resolve(id)
		return err == nil
	}

	dispatcher.cacheCatalog("acc1@example.com", with55)
	dispatcher.cacheCatalog("acc2@example.com", only46)
	if !resolves("claude-sonnet-5-5-high") || !resolves("claude-sonnet-4-6") {
		t.Fatal("union lost a model published by one account")
	}

	// acc1 withdraws 5.5: its next list replaces its snapshot.
	dispatcher.cacheCatalog("acc1@example.com", only46)
	if resolves("claude-sonnet-5-5-high") {
		t.Error("withdrawn model still resolves after its publisher refetched")
	}
	if !resolves("claude-sonnet-4-6") {
		t.Error("claude-sonnet-4-6 lost")
	}

	// A snapshot older than modelCacheTTL no longer contributes.
	dispatcher.cacheCatalog("acc3@example.com", with55)
	now = now.Add(dispatcher.modelCacheTTL + time.Second)
	dispatcher.cacheCatalog("acc2@example.com", only46)
	if resolves("claude-sonnet-5-5-high") {
		t.Error("snapshot older than modelCacheTTL still contributes")
	}
}

// Regression guard for round-1 item 1: resolveModel must not read the catalog
// fields without the dispatcher lock. Run under -race.
func TestResolveModelDoesNotRaceStoreCatalog(t *testing.T) {
	t.Parallel()
	dispatcher, _, _, _, _ := recoveryPool(t, catalogWithModel)
	catalog := dispatcher.CachedCatalog()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				dispatcher.storeCatalog("a@example.com", catalog)
			}
		}
	}()
	for range 200 {
		_, _ = dispatcher.resolveModel(context.Background(), "unknown-model-xyz", nil)
	}
	close(stop)
	wg.Wait()
}
```

Add `"sync"` to the `dispatcher_test.go` imports if it is not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/accounts/ -run 'TestStoreCatalog_UnionsPerAccountSnapshots|TestResolveModelDoesNotRaceStoreCatalog' -count=1 -race -v`
Expected: build FAIL — `too many arguments in call to dispatcher.cacheCatalog` / `storeCatalog`. (After Step 3, `TestResolveModelDoesNotRaceStoreCatalog` is a regression guard: it passes at HEAD and reports `DATA RACE` if the old unlocked `dispatcher.catalog != nil` read returns.)

- [ ] **Step 3: Write minimal implementation**

In `internal/accounts/dispatcher.go`:

1. Add `"sort"` to the standard-library import block (between `"path/filepath"` and `"strings"`).

2. In `type Dispatcher`, below `catalogAt time.Time`, add:

```go
	// catalogs holds each account's latest model list, keyed by email and
	// guarded by mu. catalog is their union; see storeCatalog.
	catalogs map[string]catalogSnapshot
```

3. Add below the `Dispatcher` struct:

```go
// catalogSnapshot is one account's model list and when it was fetched.
type catalogSnapshot struct {
	email   string
	catalog *modelcatalog.Catalog
	at      time.Time
}
```

4. Replace `cacheCatalog` and `storeCatalog` (lines 641-657) with:

```go
func (dispatcher *Dispatcher) cacheCatalog(email string, body []byte) {
	catalog, err := modelcatalog.Parse(body)
	if err == nil {
		dispatcher.storeCatalog(email, catalog)
	}
}

// storeCatalog records email's model list and rebuilds the shared catalog as
// the union of every snapshot fetched within modelCacheTTL. Accounts publish
// different lists, so one account's list must not hide another's models; a
// model an account withdraws leaves once that account's next list replaces its
// snapshot (or the snapshot ages past modelCacheTTL). The snapshot just stored
// is merged last, so it wins on overlapping ids and the default model.
func (dispatcher *Dispatcher) storeCatalog(email string, catalog *modelcatalog.Catalog) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	now := dispatcher.now()
	if dispatcher.catalogs == nil {
		dispatcher.catalogs = make(map[string]catalogSnapshot)
	}
	current := catalogSnapshot{email: email, catalog: catalog, at: now}
	dispatcher.catalogs[email] = current
	others := make([]catalogSnapshot, 0, len(dispatcher.catalogs))
	for key, snapshot := range dispatcher.catalogs {
		if key == email {
			continue
		}
		if now.Sub(snapshot.at) >= dispatcher.modelCacheTTL {
			delete(dispatcher.catalogs, key)
			continue
		}
		others = append(others, snapshot)
	}
	sort.Slice(others, func(i, j int) bool {
		if !others[i].at.Equal(others[j].at) {
			return others[i].at.Before(others[j].at)
		}
		return others[i].email < others[j].email
	})
	var union *modelcatalog.Catalog
	for _, snapshot := range others {
		union = union.Merge(snapshot.catalog)
	}
	dispatcher.catalog = union.Merge(current.catalog)
	dispatcher.catalogAt = now
}
```

5. Update the three production callers:
   - line 318 (`fetchAvailableModels`): `dispatcher.cacheCatalog(selection.Account.Email, response.Body)`
   - line 359 (`fetchAvailableModelsForAccount`): `dispatcher.cacheCatalog(account.Email, response.Body)`
   - line 621 (`resolveModel`): `dispatcher.storeCatalog("", catalog)`

6. Update the test callers:
   - `internal/accounts/dispatcher_test.go:691`: `dispatcher.cacheCatalog("acc1@example.com", []byte(`…unchanged body…`))`
   - `internal/accounts/quota_exhaustion_test.go:322`: `dispatcher.storeCatalog("", catalog)`
   - `internal/accounts/quota_merge_test.go:230`: `dispatcher.storeCatalog("", catalog)`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/accounts/ ./internal/api/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Update the stale-catalog docs**

In `README.md:703`, replace the last sentence (`The catalog is cached for 24 hours, …until it refreshes.`) with:

```markdown
The catalog is the union of each account's latest model list (cached up to 24 hours per account), so a list that still contains 4.6 keeps resolving the real 4.6 ID until that account's list is fetched again or ages past 24 hours.
```

In `docs/reasoning-parameters.md:109`, replace the bullet with:

```markdown
- The shared catalog is the union of each account's latest model list, each kept up to the 24 h TTL (`storeCatalog` in `internal/accounts/dispatcher.go`). An account list that still contains Claude 4.6 keeps resolving the real 4.6 ID after that account migrates, until the account's list is fetched again or ages out; meanwhile upstream answers 404, which blocks that model on that account for 24 h (`MarkModelUnavailable`) and rotates the request. The upstream error shape has not been captured.
```

- [ ] **Step 6: Commit**

```bash
git add internal/accounts/dispatcher.go internal/accounts/dispatcher_test.go internal/accounts/quota_exhaustion_test.go internal/accounts/quota_merge_test.go README.md docs/reasoning-parameters.md
git commit -m "fix(accounts): build the shared catalog from per-account snapshots so withdrawn models expire"
```

---

### Task 4: Recovery candidates respect cooldown and prefer healthy accounts (H1)

**Files:**
- Modify: `internal/accounts/manager.go` (`AccountsWithQuotaModel`, ~lines 1437-1454)
- Test: `internal/accounts/model_recovery_test.go`

**Interfaces:**
- Consumes: `(*Manager).usableLocked(account *Account, model string, poolSupports bool) bool` (existing), `accountSupportsQuotaModel` (Task 1).
- Produces: `(*Manager).AccountsWithQuotaModel(model string) []*Account` — live pointers, only accounts usable for model listing (`usableLocked(account, "", false)`), ordered by ascending `ConsecutiveFailure` (stable). Task 5 relies on the ordering; Task 6 relies on the filter.

- [ ] **Step 1: Write the failing test**

Append to `internal/accounts/model_recovery_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts/ -run TestResolveModelRecoverySkipsCooledDownAccount -count=1 -v`
Expected: FAIL — `cooled-down account fetched 1 times during recovery, want 0`.

- [ ] **Step 3: Write minimal implementation**

Replace `AccountsWithQuotaModel` in `internal/accounts/manager.go` with:

```go
// AccountsWithQuotaModel returns the live (not cloned) accounts whose quota
// cache lists model and that may make a model-listing call now: enabled, valid,
// not cooling down, and not rate limited in the listing ("") namespace. They
// are ordered by fewest consecutive failures, so an account that keeps failing
// or hanging is tried last. Live pointers match what Select returns, so Mark*
// calls on the result land on pool state.
func (manager *Manager) AccountsWithQuotaModel(model string) []*Account {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	var result []*Account
	for _, account := range manager.accounts {
		if !manager.usableLocked(account, "", false) || !accountSupportsQuotaModel(account, model) {
			continue
		}
		result = append(result, account)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].ConsecutiveFailure < result[j].ConsecutiveFailure
	})
	return result
}
```

(`sort` is already imported by `manager.go`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -run 'TestResolveModelRecover|TestAccountsWithQuotaModel' -count=1 -race -v`
Expected: PASS (new test and all existing recovery tests).

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/model_recovery_test.go
git commit -m "fix(accounts): skip cooled-down accounts and try healthy ones first in catalog recovery"
```

---

### Task 5: Per-account recovery fetch: own budget, penalize hangs, record HTTP errors (H2, H3 items 4-5)

**Files:**
- Modify: `internal/accounts/dispatcher.go` (struct; `NewDispatcher` literal lines 144-165; `fetchAvailableModelsForAccount` lines 331-363)
- Modify: `internal/accounts/retry_test.go` (`scriptedClient` struct lines 94-116 and `FetchAvailableModels` lines 122-149)
- Test: `internal/accounts/model_recovery_test.go`

**Interfaces:**
- Consumes: `cacheCatalog(email, body)` (Task 3), `AccountsWithQuotaModel` ordering (Task 4), `rotateForError(account *Account, model string, err error) bool` (existing).
- Produces:
  - `const defaultRecoveryAccountTimeout = 10 * time.Second`
  - `Dispatcher.recoveryAccountTimeout time.Duration` (tests override it in-package)
  - `fetchAvailableModelsForAccount(ctx, account)` penalty rule: parent `ctx` ended → no penalty; per-account budget expired → `MarkFailure(account, "")`; other error → `rotateForError(account, "", err)`. No live-quota RPCs.
  - `scriptedClient.modelsHang bool`, `scriptedClient.modelsBudget time.Duration`; test helper `setModelsErr(c *scriptedClient, err error)` (Task 6 uses it).

- [ ] **Step 1: Extend the test double**

In `internal/accounts/retry_test.go`, add to `type scriptedClient struct` (after `modelsBlockFrom int`):

```go
	// modelsHang makes FetchAvailableModels wait for its context to end and
	// return the context error: an account that never answers.
	modelsHang bool
	// modelsBudget is the time left on the context deadline of the most
	// recent FetchAvailableModels call (0 when it had no deadline).
	modelsBudget time.Duration
```

Replace the first lines of `FetchAvailableModels` — the signature and the first lock block — so it reads:

```go
func (client *scriptedClient) FetchAvailableModels(ctx context.Context, _ string) (cloudcode.Response, error) {
	client.mu.Lock()
	block, blockFrom, pending := client.modelsBlock, client.modelsBlockFrom, client.modelsCalls+1
	hang := client.modelsHang
	if deadline, ok := ctx.Deadline(); ok {
		client.modelsBudget = time.Until(deadline)
	} else {
		client.modelsBudget = 0
	}
	client.mu.Unlock()
	if hang {
		<-ctx.Done()
		client.mu.Lock()
		client.modelsCalls++
		client.mu.Unlock()
		return cloudcode.Response{}, ctx.Err()
	}
```

Keep the rest of the function unchanged (from `if block != nil && blockFrom > 0 …` on).

- [ ] **Step 2: Write the failing tests**

Append to `internal/accounts/model_recovery_test.go` (add `"antigravity-go-proxy/internal/cloudcode"` to its imports):

```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/accounts/ -run 'TestRecovery' -count=1 -v`
Expected: build FAIL — `dispatcher.recoveryAccountTimeout undefined`.

- [ ] **Step 4: Write minimal implementation**

In `internal/accounts/dispatcher.go`:

1. Below `const fetchModelsTimeout = 30 * time.Second` add:

```go
// defaultRecoveryAccountTimeout bounds one account's list-models call inside a
// catalog recovery pass. The pass as a whole is bounded by fetchModelsTimeout,
// so one account that never answers cannot consume the whole budget.
const defaultRecoveryAccountTimeout = 10 * time.Second
```

2. In `type Dispatcher`, below `modelCacheTTL time.Duration`, add:

```go
	recoveryAccountTimeout   time.Duration
```

3. In the `NewDispatcher` return literal, below `modelCacheTTL: options.ModelCacheTTL,`, add:

```go
		recoveryAccountTimeout:   defaultRecoveryAccountTimeout,
```

4. Replace the whole `fetchAvailableModelsForAccount` function (doc comment included) with:

```go
// fetchAvailableModelsForAccount fetches the selectable model list with the
// given account's credentials instead of selecting one via the strategy. Each
// call is bounded by recoveryAccountTimeout inside the caller's context.
// Failures reach pool state like any other pool call, except when the
// caller's context ended (an abandoned or out-of-budget pass is not the
// account's fault). An account that uses its whole own budget is marked
// failed, so recovery tries it last next time. No live-quota RPCs ride along:
// recovery sits on a request path, and the post-request refresh covers them.
func (dispatcher *Dispatcher) fetchAvailableModelsForAccount(ctx context.Context, account *Account) (cloudcode.Response, error) {
	if err := ctx.Err(); err != nil {
		return cloudcode.Response{}, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, dispatcher.recoveryAccountTimeout)
	defer cancel()
	credentials, err := dispatcher.resolver.Resolve(fetchCtx, account)
	if err != nil {
		if ctx.Err() == nil {
			dispatcher.manager.MarkFailure(account, "")
		}
		return cloudcode.Response{}, err
	}
	modelsClient := dispatcher.client(account, credentials.AccessToken)
	response, err := dispatcher.metered(account.Email, func() (cloudcode.Response, error) {
		return modelsClient.FetchAvailableModels(fetchCtx, dispatcher.project(account))
	})
	if err != nil {
		switch {
		case ctx.Err() != nil:
		case fetchCtx.Err() != nil:
			dispatcher.manager.MarkFailure(account, "")
		default:
			dispatcher.rotateForError(account, "", err)
		}
		return cloudcode.Response{}, err
	}
	dispatcher.manager.MarkSuccess(account, "")
	dispatcher.cacheCatalog(account.Email, response.Body)
	dispatcher.updateAccountQuota(account, response.Body)
	return response, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -run 'TestRecovery|TestResolveModelRecover|TestFetchAvailableModelsForAccount' -count=1 -race -v`
Expected: PASS, including the existing `TestFetchAvailableModelsForAccount_DoesNotRotateOnContextCancellation`.

- [ ] **Step 6: Commit**

```bash
git add internal/accounts/dispatcher.go internal/accounts/retry_test.go internal/accounts/model_recovery_test.go
git commit -m "fix(accounts): bound each recovery fetch, penalize hung accounts, and test upstream errors"
```

---

### Task 6: Single-flight background recovery, debounced only on a definitive negative (BLOCK 1, H2 total budget, NOTE 5)

**Files:**
- Modify: `internal/accounts/dispatcher.go` (struct lines 100-102; replace `recoverModelFromQuotaCache` and `claimRecoveryAttempt`, lines 541-585; comment lines 587-590)
- Test: `internal/accounts/model_recovery_test.go`

**Interfaces:**
- Consumes: `fetchAvailableModelsForAccount` (Task 5), `AccountsWithQuotaModel` (Task 4), `CachedCatalog()`, `setModelsErr` (Task 5), `scriptedClient.modelsBlock`/`modelsBlockFrom` (existing).
- Produces:
  - `type modelRecoveryCall struct { done chan struct{}; found bool }`
  - `(*Dispatcher).recoverModelFromQuotaCache(ctx, requested, request) (modelcatalog.Model, bool)` — same signature; joins or starts one pass per model key and waits for it or for `ctx`.
  - `(*Dispatcher).startModelRecovery(requested string) *modelRecoveryCall` — nil when there is nothing to do.
  - `(*Dispatcher).runModelRecovery(key, requested string, accounts []*Account, call *modelRecoveryCall)`.
  - `claimRecoveryAttempt` is deleted.

- [ ] **Step 1: Write the failing tests**

Append to `internal/accounts/model_recovery_test.go`:

```go
// A pass in which every candidate failed is not a verdict on the model: the
// next request, once upstream answers again, must recover instead of waiting
// out missingModelFetchFloor.
func TestRecoveryFailureDoesNotConsumeTheDebounce(t *testing.T) {
	t.Parallel()
	dispatcher, a, b, _, clock := recoveryPool(t, catalogWithModel)
	setModelsErr(a, errors.New("transient 503"))
	setModelsErr(b, errors.New("transient 503"))
	if _, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil); !isSelectionError(err, recoveryModel) {
		t.Fatalf("attempt with upstream down: err = %v, want selection error", err)
	}
	setModelsErr(a, nil)
	setModelsErr(b, nil)
	*clock = clock.Add(time.Second)
	model, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil)
	if err != nil || model.ID != recoveryModel {
		t.Fatalf("attempt after upstream healed = %q, %v; want %q", model.ID, err, recoveryModel)
	}
}

// A request that misses while another request's recovery is in flight waits
// for that pass and resolves, instead of failing with HTTP 400.
func TestConcurrentRequestJoinsInFlightRecovery(t *testing.T) {
	t.Parallel()
	dispatcher, a, b, _, _ := recoveryPool(t, catalogWithModel)
	aSeed := a.modelsCallCount()
	release := make(chan struct{})
	b.mu.Lock()
	b.modelsBlock = release
	b.modelsBlockFrom = 1 // B's first recovery fetch parks
	b.mu.Unlock()

	first := make(chan error, 1)
	go func() {
		_, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil)
		first <- err
	}()
	// A is tried first and does not publish the model; once its fetch is done
	// the pass is parked on B and still in flight.
	deadline := time.Now().Add(5 * time.Second)
	for a.modelsCallCount() == aSeed {
		if time.Now().After(deadline) {
			t.Fatal("recovery never reached account A")
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() {
		_, err := dispatcher.resolveModel(context.Background(), recoveryModel, nil)
		second <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the second request reach the join
	close(release)
	if err := <-first; err != nil {
		t.Errorf("request owning the recovery: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("concurrent request: %v, want it to wait for the in-flight recovery", err)
	}
	if got := b.modelsCallCount(); got != 1 {
		t.Errorf("B models calls = %d, want 1 (one shared pass)", got)
	}
}
```

Add `"errors"` to the `model_recovery_test.go` imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/accounts/ -run 'TestRecoveryFailureDoesNotConsumeTheDebounce|TestConcurrentRequestJoinsInFlightRecovery' -count=1 -race -v`
Expected: FAIL — first test: `attempt after upstream healed = "", model "claude-sonnet-5-5-high" is not in agy's selectable agent model list…`; second test: `concurrent request: model "claude-sonnet-5-5-high" is not in agy's selectable agent model list…`.

- [ ] **Step 3: Write minimal implementation**

In `internal/accounts/dispatcher.go`:

1. In `type Dispatcher`, replace the `modelRecoveryAt` field and its comment (lines 100-102) with:

```go
	// modelRecoveryAt is the time of the last definitive negative recovery
	// pass per model key (every candidate answered and none publishes the
	// model), guarded by mu; it debounces recoverModelFromQuotaCache.
	modelRecoveryAt map[string]time.Time
	// modelRecoveries is the in-flight recovery pass per model key, guarded
	// by mu; concurrent misses for one model share it.
	modelRecoveries map[string]*modelRecoveryCall
```

2. Replace `recoverModelFromQuotaCache` and `claimRecoveryAttempt` (lines 541-585, doc comments included) with:

```go
// modelRecoveryCall is one shared recovery pass for a model key. found is set
// before done is closed, so every waiter reads it without locking.
type modelRecoveryCall struct {
	done  chan struct{}
	found bool
}

// recoverModelFromQuotaCache handles a model missing from the shared catalog
// that an account's quota cache still lists: a list fetched with that account
// can add the model to the catalog. Concurrent misses for one model share one
// pass, which runs in the background so a caller that disconnects does not
// abort it for the others. The caller waits for the pass or for its own ctx.
func (dispatcher *Dispatcher) recoverModelFromQuotaCache(ctx context.Context, requested string, request map[string]any) (modelcatalog.Model, bool) {
	call := dispatcher.startModelRecovery(requested)
	if call == nil {
		return modelcatalog.Model{}, false
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		return modelcatalog.Model{}, false
	}
	if !call.found {
		return modelcatalog.Model{}, false
	}
	catalog := dispatcher.CachedCatalog()
	if catalog == nil {
		return modelcatalog.Model{}, false
	}
	model, err := catalog.ResolveWithRequest(requested, request)
	return model, err == nil
}

// startModelRecovery returns the in-flight pass for requested, or starts one.
// It returns nil when no usable account lists the model, or when a definitive
// negative pass for it ran within missingModelFetchFloor.
func (dispatcher *Dispatcher) startModelRecovery(requested string) *modelRecoveryCall {
	key := rateLimitModelKey(requested)
	if key == "" {
		return nil
	}
	accounts := dispatcher.manager.AccountsWithQuotaModel(requested)
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if call := dispatcher.modelRecoveries[key]; call != nil {
		return call
	}
	if len(accounts) == 0 {
		return nil
	}
	if last, seen := dispatcher.modelRecoveryAt[key]; seen && dispatcher.now().Sub(last) < missingModelFetchFloor {
		return nil
	}
	call := &modelRecoveryCall{done: make(chan struct{})}
	if dispatcher.modelRecoveries == nil {
		dispatcher.modelRecoveries = make(map[string]*modelRecoveryCall)
	}
	dispatcher.modelRecoveries[key] = call
	go dispatcher.runModelRecovery(key, requested, accounts, call)
	return call
}

// runModelRecovery tries each candidate in order, on a background context
// bounded by fetchModelsTimeout, until one's list adds the model. Only a
// definitive negative — every candidate answered and none publishes the
// model — stamps the debounce; a pass with any failed fetch is retried by the
// next miss. Each failure is recorded on the account (cooldown, invalidation,
// failure count), which removes or demotes it as a later candidate.
func (dispatcher *Dispatcher) runModelRecovery(key, requested string, accounts []*Account, call *modelRecoveryCall) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchModelsTimeout)
	defer cancel()
	definitive := true
	for _, account := range accounts {
		if _, err := dispatcher.fetchAvailableModelsForAccount(ctx, account); err != nil {
			definitive = false
			continue
		}
		if catalog := dispatcher.CachedCatalog(); catalog != nil {
			if _, err := catalog.Resolve(requested); err == nil {
				call.found = true
				break
			}
		}
	}
	dispatcher.mu.Lock()
	delete(dispatcher.modelRecoveries, key)
	if !call.found && definitive {
		if dispatcher.modelRecoveryAt == nil {
			dispatcher.modelRecoveryAt = make(map[string]time.Time)
		}
		dispatcher.modelRecoveryAt[key] = dispatcher.now()
	}
	dispatcher.mu.Unlock()
	close(call.done)
}
```

3. Replace the `missingModelFetchFloor` comment (lines 587-590) with:

```go
// missingModelFetchFloor spaces two kinds of catalog work a model miss can
// trigger. The background refresh in resolveModel: at most one upstream
// list-models call per floor, shared by all models, keyed on the last
// successful fetch. And quota-cache recovery: per model, a definitive negative
// pass suppresses another pass for that model for one floor.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -count=1 -race`
Expected: PASS (whole package, including `TestResolveModelRecoveryIsDebouncedPerModel`: both accounts answer without the model, so that pass is a definitive negative and is still debounced).

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/dispatcher.go internal/accounts/model_recovery_test.go
git commit -m "fix(accounts): single-flight catalog recovery and debounce only definitive negatives"
```

---

### Task 7: Router refuses tier-suffixed requests in prefix matching; delete the dead alias guard (H4, H3 items 7-8)

**Decision:** an operator alias whose id is a tiered name (for example alias `claude-sonnet-5-5-high` → `claude-sonnet-5-5`) is explicit configuration and is honored. `isTierVariantOf` only blocked the dotted spelling of such an alias while step 4 re-admitted the exact spelling, and `DefaultAllowlist()` has no such alias, so it is deleted. Tier refusal lives in one place: step 4.

**Files:**
- Modify: `internal/claudecode/router.go` (`ResolveModel` doc + steps 2-4, lines 198-241; replace `isThinkingTier` and `isTierVariantOf`, lines 269-284)
- Test: `internal/claudecode/router_test.go`

**Interfaces:**
- Consumes: `continuesVersion(rest string) bool` (existing), `DefaultAllowlist() []ModelConfig` (existing).
- Produces: `startsWithThinkingTier(rest string) bool`; `isThinkingTier` and `isTierVariantOf` are deleted.

- [ ] **Step 1: Write the failing tests**

Append to `internal/claudecode/router_test.go`:

```go
// The production allowlist must never serve a Claude 5.5 tiered id through
// the Claude Code gateway, with or without the 1M-context marker.
func TestRouter_DefaultAllowlistRefusesClaude55Tiers(t *testing.T) {
	r := NewRouter(DefaultAllowlist())
	for _, family := range []string{"claude-sonnet-5-5", "claude-opus-5-5"} {
		for _, tier := range []string{"-low", "-medium", "-high"} {
			for _, tail := range []string{"", "[1m]"} {
				req := family + tier + tail
				if got, ok := r.ResolveModel(req); ok {
					t.Errorf("ResolveModel(%q) = %q; Claude 5.5 tiers must stay on Cloud Code", req, got)
				}
			}
		}
	}
}

// A tier followed by another suffix is still a tiered id, not a variant of
// the bare family.
func TestRouter_RefusesTierWithTrailingSuffix(t *testing.T) {
	r := NewRouter([]ModelConfig{{ID: "claude-sonnet-5-5", Enabled: true}})
	for _, req := range []string{
		"claude-sonnet-5-5-high[1m]",
		"claude-sonnet-5-5-medium-latest",
		"claude-sonnet-5-5-low-20260101",
	} {
		if got, ok := r.ResolveModel(req); ok {
			t.Errorf("ResolveModel(%q) = %q; tiered ids must be refused", req, got)
		}
	}
	for _, req := range []string{"claude-sonnet-5-5[1m]", "claude-sonnet-5-5-latest", "claude-sonnet-5-5-20260101", "claude-sonnet-5-5-highlander"} {
		if got, ok := r.ResolveModel(req); !ok || got != "claude-sonnet-5-5" {
			t.Errorf("ResolveModel(%q) = %q, %v; want claude-sonnet-5-5, true", req, got, ok)
		}
	}
}

// An operator who lists a tiered id as an alias has asked for that routing;
// every spelling of the alias resolves the same way.
func TestRouter_HonorsExplicitTieredAlias(t *testing.T) {
	r := NewRouter([]ModelConfig{{ID: "claude-sonnet-5-5", Aliases: []string{"claude-sonnet-5-5-high"}, Enabled: true}})
	for _, req := range []string{"claude-sonnet-5-5-high", "claude-sonnet-5.5-high", "claude-sonnet-5-5-high[1m]"} {
		if got, ok := r.ResolveModel(req); !ok || got != "claude-sonnet-5-5" {
			t.Errorf("ResolveModel(%q) = %q, %v; want claude-sonnet-5-5, true", req, got, ok)
		}
	}
	if got, ok := r.ResolveModel("claude-sonnet-5-5-medium"); ok {
		t.Errorf("ResolveModel(claude-sonnet-5-5-medium) = %q; only the listed tier is aliased", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/claudecode/ -run 'TestRouter_DefaultAllowlistRefusesClaude55Tiers|TestRouter_RefusesTierWithTrailingSuffix|TestRouter_HonorsExplicitTieredAlias' -count=1 -v`
Expected: FAIL — `ResolveModel("claude-sonnet-5-5-high[1m]") = "claude-sonnet-5-5"; tiered ids must be refused` (and the `-latest`/dated cases), and `ResolveModel("claude-sonnet-5.5-high") = "", false; want claude-sonnet-5-5, true`. `TestRouter_DefaultAllowlistRefusesClaude55Tiers` already passes; it is the regression guard for the production allowlist.

- [ ] **Step 3: Write minimal implementation**

In `internal/claudecode/router.go`:

1. Replace the `ResolveModel` doc comment (lines 198-202) with:

```go
// ResolveModel maps a requested model ID or alias to the canonical model ID.
// Tiered family variants (claude-sonnet-5-5-high, claude-opus-5-5-medium[1m],
// ...) are never matched by prefix, so they stay on the Cloud Code gateway
// instead of being rewritten to a bare family id. Only an exact allowlist id
// or an alias the operator configured can resolve a tiered name.
```

2. Step 2 (line 218): `if canonical, ok := r.aliases[req]; ok {`

3. Step 3 (line 227): `if canonical, ok := r.aliases[normalized]; ok {`

4. Step 4 loop condition (line 238):

```go
		if rest, ok := strings.CutPrefix(req, pm.prefix); ok && !continuesVersion(rest) && !startsWithThinkingTier(rest) {
```

5. Replace `isThinkingTier` and `isTierVariantOf` (lines 269-284, doc comments included) with:

```go
// startsWithThinkingTier reports whether rest, the tail of a request after a
// matched prefix, begins with a thinking tier (-low, -medium, -high) that ends
// the id or is followed by another suffix ("[1m]", "-latest", "-20260101").
// Such a request names a tiered model, not a variant of the matched one.
func startsWithThinkingTier(rest string) bool {
	t := strings.ToLower(rest)
	for _, tier := range []string{"-low", "-medium", "-high"} {
		if after, ok := strings.CutPrefix(t, tier); ok {
			return after == "" || after[0] == '[' || after[0] == '-'
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/claudecode/ ./internal/api/ -count=1 -race`
Expected: PASS, including the existing `TestRouter_RefusesTieredVariants`.

- [ ] **Step 5: Commit**

```bash
git add internal/claudecode/router.go internal/claudecode/router_test.go
git commit -m "fix(claudecode): refuse tier-suffixed ids in prefix matching and drop the dead alias guard"
```

---

### Task 8: Full verification and push

**Files:** none modified (graph refresh only).

**Interfaces:**
- Consumes: all previous tasks.
- Produces: a pushed branch with a clean race run.

- [ ] **Step 1: Format, vet, build, race-test**

Run:

```bash
gofmt -l internal/ && go vet ./internal/accounts/ ./internal/claudecode/ ./internal/modelcatalog/ ./internal/api/ && go build ./... && go test ./... -race -count=1
```

Expected: `gofmt -l` prints nothing; vet clean; build succeeds; all tests PASS.

- [ ] **Step 2: Confirm no transport change**

Run: `git diff 976270d --stat -- internal/cloudcode/ internal/auth/`
Expected: no output (no TLS/transport file touched, so the JA4 gate in `AGENTS.md` does not apply).

- [ ] **Step 3: Refresh the graft graph**

Run: `graft build`
Expected: exit 0.

- [ ] **Step 4: Push to the fork**

```bash
git push fork fix/claude-5-5-cloud-code-routing
```

Expected: push succeeds; PR #122 shows the new commits.
