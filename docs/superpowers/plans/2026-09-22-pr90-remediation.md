# PR #90 Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the quota exhaustion recorded from an upstream 429 actually survive and actually land on the rows the UI and the scheduler read. PR #90 records the reading correctly but a catalog refresh erases it within minutes, the pool propagation ignores the reset horizon, and for a tier-mapped model the key it writes is invisible to `ModelKeyCandidates`.

**Architecture:** Four behaviour fixes inside `internal/accounts`, no new packages and no dependencies.

1. `Manager.UpdateAccountQuota` stops being a wholesale assignment: unexpired `ExhaustedUntilMS` records and the `Pools` map carry across a catalog snapshot.
2. `quotaPoolsForModel` takes the reset horizon and returns only the pools that horizon can justify (≤ 5 h → the 5h pool alone).
3. `Dispatcher.recordQuotaExhaustion` resolves the upstream model id back to the catalog id the account's quota rows are keyed by, so a tiered model marks the row the UI shows.
4. `MarkQuotaExhausted` persists the record, so a restart inside the window does not resurrect the phantom 100%.

Plus two hygiene fixes (shared reset-time parser, one debug log) and the two missing tests.

**Tech Stack:** Go (stdlib only). Repo test style is plain `testing` with table fakes implementing `CloudClient`; helpers already exist — `testAccount` (`internal/accounts/manager_test.go:341`), `quotaTestManager` (`internal/accounts/quota_merge_test.go:12`), `poolTestAccount` (`internal/accounts/quota_exhaustion_test.go:134`), `newTestDispatcher` (`internal/accounts/retry_test.go:340`).

**Spec:** Review of PR #90 posted at https://github.com/gustavokch/antigravity-claude-proxy-go/pull/90#issuecomment-5784361603. Findings addressed here: the 🔴 on `manager.go:1496`, the three 🟡 on `manager.go:1531`, `dispatcher.go:881` and `manager.go:1554`, and the four 🔵.

## Global Constraints

- Go module `antigravity-go-proxy`; run everything from the repo root. Package tests: `go test ./internal/accounts/`. Full gate: `go build ./... && go vet ./... && go test ./...`.
- Branch is `fix/gemini-quota-exhaustion`; commit on top of it, one commit per task, conventional commits with a subject ≤ 50 characters.
- No new dependencies, no new packages.
- Quota keys stay lowercase-canonical and reads keep going through `ModelKeyCandidates` (`internal/accounts/manager.go:432`) — do not change that contract.
- The nine tests added by PR #90 (`internal/accounts/quota_exhaustion_test.go`) must stay green throughout. Task 2 is designed around them: every existing case uses a reset more than 5 h out, so the horizon rule leaves them unchanged.
- `SaveToDisk` (`manager.go:1275`) no-ops on an empty `configPath`, so tests that do not call `SetConfigPath` never touch the real `accounts.json`. Any test that does must point at `t.TempDir()`.
- Never call `SaveToDisk` while holding `manager.mu` — it takes `RLock` itself. Follow the `UpdateThresholds` pattern (`manager.go:1476`): unlock, then save.

---

### Task 1: Carry exhaustion and pools across a catalog snapshot

The 🔴. `UpdateAccountQuota` assigns `acc.Quota = quota`, which drops every `ExhaustedUntilMS` and nils `Pools`. `dispatcher.updateAccountQuota` runs it on each catalog fetch (`dispatcher.go:295`) and on manual account refresh (`dispatcher.go:611`), each immediately followed by `refreshLiveQuota` merging the phantom `1` back. `resolveModel` refetches the catalog once `modelCacheTTL` expires (default 5 minutes, `dispatcher.go:136`) and `/v1/models` refetches unconditionally, so today a 51 h exhaustion lives for minutes.

**Files:**
- Modify: `internal/accounts/manager.go:1496-1508` (`UpdateAccountQuota`)
- Test: `internal/accounts/quota_exhaustion_test.go`

**Interfaces:**
- Consumes: `ModelQuota.ExhaustedUntilMS` (`manager.go:56`), `manager.now()`.
- Produces: unexported `carryQuotaExhaustions(previous, incoming map[string]ModelQuota, nowMS int64) map[string]ModelQuota`. No exported signature changes.

- [ ] **Step 1: Write the failing test**

Add to `internal/accounts/quota_exhaustion_test.go`:

```go
func TestExhaustionSurvivesCatalogRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := poolTestAccount("catalog@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("catalog@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")

	// What a /v1/models fetch does: a whole-catalog snapshot, which reports
	// the refused model at 1, followed by the live refresh merging 1 again.
	full := 1.0
	manager.UpdateAccountQuota("catalog@example.com", Quota{
		Models:      map[string]ModelQuota{"gemini-3.8-flash-high": {RemainingFraction: &full}},
		LastChecked: now.UnixMilli(),
	}, nil)
	manager.MergeQuotaFraction("catalog@example.com", "gemini-3.8-flash-high", &full, "2026-09-22T21:53:12Z")

	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("a catalog snapshot must not erase an unexpired exhaustion: %+v", got)
	}
	if got.ExhaustedUntilMS == 0 {
		t.Fatal("the exhaustion marker itself must survive, or the next merge clobbers it")
	}
	if len(account.Quota.Pools) != 4 {
		t.Fatalf("a catalog snapshot carries no pool readings and must not drop them: %+v", account.Quota.Pools)
	}
}

func TestCatalogRefreshDropsExpiredExhaustion(t *testing.T) {
	t.Parallel()
	current := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("expired@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}

	manager.MarkQuotaExhausted("expired@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")
	current = time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)

	full := 1.0
	manager.UpdateAccountQuota("expired@example.com", Quota{
		Models:      map[string]ModelQuota{"gemini-3.8-flash-high": {RemainingFraction: &full}},
		LastChecked: current.UnixMilli(),
	}, nil)

	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 1 {
		t.Fatalf("past its reset time the catalog reading must win: %+v", got)
	}
	if got.ExhaustedUntilMS != 0 {
		t.Fatalf("an expired marker must not be carried: %+v", got)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

```bash
go test ./internal/accounts/ -run 'TestExhaustionSurvivesCatalogRefresh|TestCatalogRefreshDropsExpiredExhaustion' -v
```

Expected: `TestExhaustionSurvivesCatalogRefresh` fails on the fraction (it reads 1) and `TestCatalogRefreshDropsExpiredExhaustion` passes already (it asserts the behaviour that a wholesale replace happens to produce). Both must pass after Step 3.

- [ ] **Step 3: Minimal implementation**

In `internal/accounts/manager.go`, replace the body of `UpdateAccountQuota`:

```go
// carryQuotaExhaustions moves unexpired 429-recorded exhaustions from the
// previous reading onto an incoming one. A catalog snapshot reports what
// upstream publishes, and upstream publishes remainingFraction 1 for a model
// it is already refusing — so replacing the map wholesale erases the one
// truthful reading and lets the next live refresh restore a phantom 100%.
// A nil incoming map means the source carried no reading of that kind at all
// (the catalog fetch never reports pools), so the previous map stands.
func carryQuotaExhaustions(previous, incoming map[string]ModelQuota, nowMS int64) map[string]ModelQuota {
	if incoming == nil {
		return previous
	}
	for key, record := range previous {
		if record.ExhaustedUntilMS > nowMS {
			incoming[key] = record
		}
	}
	return incoming
}

func (manager *Manager) UpdateAccountQuota(email string, quota Quota, subscription *Subscription) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	nowMS := manager.now().UnixMilli()
	for _, acc := range manager.accounts {
		if acc.Email == email {
			quota.Models = carryQuotaExhaustions(acc.Quota.Models, quota.Models, nowMS)
			quota.Pools = carryQuotaExhaustions(acc.Quota.Pools, quota.Pools, nowMS)
			acc.Quota = quota
			if subscription != nil {
				acc.Subscription = *subscription
			}
			break
		}
	}
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/accounts/ -run 'TestExhaustionSurvivesCatalogRefresh|TestCatalogRefreshDropsExpiredExhaustion' -v
go test ./internal/accounts/
```

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/quota_exhaustion_test.go
git commit -m "fix(quota): keep exhaustion across catalog refresh"
```

---

### Task 2: Pool propagation follows the reset horizon

The 🟡 on `manager.go:1531`. Both pools go to 0 whatever the horizon is. `.reference/cloudcode-429-probe-20260917.jsonl:12` holds the same "Individual quota reached" wording with `Resets in 17m31s`; a 17-minute individual quota cannot mean the weekly bucket is empty, so today's code would swap one wrong reading for another.

**Files:**
- Modify: `internal/accounts/manager.go:1526-1539` (`quotaPoolsForModel`), `internal/accounts/manager.go:1554-1585` (its caller)
- Test: `internal/accounts/quota_exhaustion_test.go`

**Interfaces:**
- Consumes: the parsed reset time already computed in `MarkQuotaExhausted`.
- Produces: `quotaPoolsForModel(model string, window time.Duration) []string`. Unexported; the only caller is `MarkQuotaExhausted`.

- [ ] **Step 1: Write the failing test**

```go
func TestShortExhaustionSparesTheWeeklyPool(t *testing.T) {
	t.Parallel()
	// The 2026-09-17 probe: "Individual quota reached ... Resets in 17m31s".
	now := time.Date(2026, 9, 18, 0, 40, 49, 0, time.UTC)
	account := poolTestAccount("short@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("short@example.com", "gemini-3.8-flash-high", "2026-09-18T00:58:20Z")

	if got := account.Quota.Pools["gemini-5h"]; got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("a 17-minute exhaustion still empties the 5h pool: %+v", got)
	}
	if got := account.Quota.Pools["gemini-weekly"]; got.RemainingFraction == nil || *got.RemainingFraction != 1 {
		t.Fatalf("a 17-minute reset cannot mean the weekly bucket is empty: %+v", got)
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

```bash
go test ./internal/accounts/ -run TestShortExhaustionSparesTheWeeklyPool -v
```

Expected failure: `gemini-weekly` reads 0.

- [ ] **Step 3: Minimal implementation**

```go
// quotaFiveHourWindow is the published span of the short pools (gemini-5h,
// 3p-5h). A reset inside it cannot be evidence about the weekly bucket.
const quotaFiveHourWindow = 5 * time.Hour

// quotaPoolsForModel names the shared upstream buckets a model is charged
// against, filtered by how far out the 429's own reset is. Upstream publishes
// the grouping only as prose ("Models within this group: Gemini Flash, Gemini
// Pro"), never as model ids, so the family prefix is the only available
// mapping. A model outside both published groups (the chat_* and tab_*
// internal ids) maps to nothing. A reset inside the 5h window says nothing
// about the weekly bucket, so only the short pool follows the model down.
func quotaPoolsForModel(model string, window time.Duration) []string {
	var weekly, fiveHour string
	switch {
	case strings.HasPrefix(model, "gemini-"):
		weekly, fiveHour = "gemini-weekly", "gemini-5h"
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "gpt-oss"):
		weekly, fiveHour = "3p-weekly", "3p-5h"
	default:
		return nil
	}
	if window <= quotaFiveHourWindow {
		return []string{fiveHour}
	}
	return []string{weekly, fiveHour}
}
```

In `MarkQuotaExhausted`, pass the horizon:

```go
	for _, pool := range quotaPoolsForModel(key, reset.Sub(manager.now())) {
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/accounts/ -run 'TestShortExhaustionSparesTheWeeklyPool|TestMarkQuotaExhaustedPropagatesToFamilyPools|TestClaudeQuota429PropagatesTo3pPools' -v
go test ./internal/accounts/
```

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/quota_exhaustion_test.go
git commit -m "fix(quota): scope pool exhaustion to reset window"
```

---

### Task 3: Record under the catalog-visible key

The 🟡 on `dispatcher.go:881` and the 🔵 on `dispatcher.go:879`. `MarkQuotaExhausted` is keyed on upstream's model id, but quota rows are read through `ModelKeyCandidates` over catalog ids. When upstream publishes `gemini-3.8-flash-tiered`, the catalog exposes `gemini-3.8-flash-{high,medium,low}` — all with `UpstreamID = gemini-3.8-flash-tiered` (`internal/modelcatalog/catalog.go:606`) — and `updateAccountQuota` writes one row per tier (proven by `internal/accounts/dispatcher_test.go:215`). An exhaustion filed under `gemini-3.8-flash-tiered` therefore reaches no candidate list: the scheduler ignores it and all three visible rows keep reading 100%. The same code path also needs `Strip1mSuffix` on the fallback key, which otherwise writes a `[1m]`-suffixed row no live reading ever refreshes.

**Files:**
- Modify: `internal/accounts/dispatcher.go:869-882` (`recordQuotaExhaustion`)
- Test: `internal/accounts/quota_exhaustion_test.go`

**Interfaces:**
- Consumes: `modelcatalog.Catalog.Resolve` (`catalog.go:241`), `Model.GetUpstreamID` (`catalog.go:31`), `modelcatalog.Strip1mSuffix` (`catalog.go:232`), `dispatcher.catalog` under `dispatcher.mu`.
- Produces: `(*Dispatcher).currentCatalog() *modelcatalog.Catalog` and `(*Dispatcher).quotaKeyFor(requested, upstream string) string`. Both unexported.

- [ ] **Step 1: Write the failing test**

```go
func TestQuotaKeyPrefersTheCatalogTierId(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("tiered@example.com")
	manager := quotaTestManager(t, now, account)
	dispatcher := newTestDispatcher(t, manager, &staticResolver{tokens: map[string]string{}},
		map[string]*scriptedClient{}, now, func(context.Context, time.Duration) error { return nil })

	// Upstream publishes only the tiered entry; the catalog fans it out into
	// three selectable tier ids that all send gemini-3.8-flash-tiered.
	catalog, err := modelcatalog.Parse([]byte(`{
		"defaultAgentModelId":"gemini-3.8-flash-high",
		"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":["gemini-3.8-flash-high"]}]}],
		"models":{"gemini-3.8-flash-tiered":{"supportsThinking":true,"quotaInfo":{"remainingFraction":1}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.storeCatalog(catalog)

	body := `{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
		`"reason":"QUOTA_EXHAUSTED","metadata":{"model":"gemini-3.8-flash-tiered",` +
		`"quotaResetTimeStamp":"2026-09-24T19:58:20Z"}}]}}`
	dispatcher.recordQuotaExhaustion(account, "gemini-3.8-flash-high", body)

	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("the exhaustion must land on the tier row the UI and the scheduler read: %+v", account.Quota.Models)
	}
	if _, invented := account.Quota.Models["gemini-3.8-flash-tiered"]; invented {
		t.Fatal("the upstream-only id is not a selectable model and must not become a row")
	}
}

func TestQuotaKeyFallbackStripsThe1mSuffix(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("suffix@example.com")
	manager := quotaTestManager(t, now, account)
	dispatcher := newTestDispatcher(t, manager, &staticResolver{tokens: map[string]string{}},
		map[string]*scriptedClient{}, now, func(context.Context, time.Duration) error { return nil })

	// ErrorInfo without a model: the requested id is the only key available,
	// and it must be normalised the way ModelKeyCandidates normalises reads.
	body := `{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
		`"metadata":{"quotaResetTimeStamp":"2026-09-24T19:58:20Z"}}]}}`
	dispatcher.recordQuotaExhaustion(account, "claude-sonnet-4-6[1m]", body)

	if _, suffixed := account.Quota.Models["claude-sonnet-4-6[1m]"]; suffixed {
		t.Fatalf("a [1m]-suffixed row is never refreshed by a live reading: %+v", account.Quota.Models)
	}
	got := account.Quota.Models["claude-sonnet-4-6"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("the stripped id must carry the exhaustion: %+v", account.Quota.Models)
	}
}
```

Add `"antigravity-go-proxy/internal/modelcatalog"` to the test file imports.

- [ ] **Step 2: Run the tests and confirm they fail**

```bash
go test ./internal/accounts/ -run 'TestQuotaKeyPrefersTheCatalogTierId|TestQuotaKeyFallbackStripsThe1mSuffix' -v
```

Expected: the first fails with the row under `gemini-3.8-flash-tiered`, the second with the row under `claude-sonnet-4-6[1m]`.

- [ ] **Step 3: Minimal implementation**

In `internal/accounts/dispatcher.go`:

```go
// currentCatalog returns the cached catalog, possibly nil before the first
// successful fetch.
func (dispatcher *Dispatcher) currentCatalog() *modelcatalog.Catalog {
	dispatcher.mu.RLock()
	defer dispatcher.mu.RUnlock()
	return dispatcher.catalog
}

// quotaKeyFor picks the key an exhaustion is filed under. Quota rows are keyed
// by catalog id — that is what ModelKeyCandidates looks up and what the UI
// lists — while upstream names its own model id, and for a tier-mapped model
// the two differ: every gemini-3.8-flash-* tier can share one
// gemini-3.8-flash-tiered upstream id. When the requested catalog model is the
// one upstream charged, the catalog id wins, so the row that goes to 0 is a
// row something actually reads.
func (dispatcher *Dispatcher) quotaKeyFor(requested, upstream string) string {
	requested = strings.ToLower(strings.TrimSpace(modelcatalog.Strip1mSuffix(requested)))
	if upstream == "" {
		return requested
	}
	if requested != "" {
		if catalog := dispatcher.currentCatalog(); catalog != nil {
			if model, err := catalog.Resolve(requested); err == nil &&
				strings.ToLower(model.GetUpstreamID()) == upstream {
				return requested
			}
		}
	}
	return upstream
}
```

and in `recordQuotaExhaustion` replace the key block with:

```go
	dispatcher.manager.MarkQuotaExhausted(account.Email, dispatcher.quotaKeyFor(model, exhaustion.Model), exhaustion.ResetTime)
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/accounts/ -run 'TestQuotaKey|TestIndividualQuota429RecordsModelExhaustion' -v
go test ./internal/accounts/
```

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/dispatcher.go internal/accounts/quota_exhaustion_test.go
git commit -m "fix(quota): file exhaustion under the catalog id"
```

---

### Task 4: Persist the exhaustion

The 🟡 on `manager.go:1554`. `Quota` is on disk (`diskAccount.Quota`, `manager.go:113`) and `exhaustedUntilMs` serialises, but nothing saves after the mark, so a restart inside the window brings the phantom 100% back. Save only when the record actually changed, so a 429 storm cannot turn into a write storm.

**Files:**
- Modify: `internal/accounts/manager.go:1554-1585` (`MarkQuotaExhausted`)
- Test: `internal/accounts/quota_exhaustion_test.go`

**Interfaces:**
- Consumes: `Manager.SaveToDisk` (`manager.go:1275`), `Manager.SetConfigPath` (`manager.go:1299`).
- Produces: no signature change; `MarkQuotaExhausted` gains a best-effort save after the lock is released.

- [ ] **Step 1: Write the failing test**

```go
func TestMarkQuotaExhaustedPersists(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("persist@example.com")
	manager := quotaTestManager(t, now, account)
	path := filepath.Join(t.TempDir(), "accounts.json")
	manager.SetConfigPath(path)

	manager.MarkQuotaExhausted("persist@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20Z")

	file, err := Load(path)
	if err != nil {
		t.Fatalf("the exhaustion must be on disk before the next restart: %v", err)
	}
	got := file.Accounts[0].Quota.Models["gemini-3.8-flash-high"]
	if got.ExhaustedUntilMS == 0 || got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("exhaustion did not round-trip through accounts.json: %+v", got)
	}
}
```

Add `"path/filepath"` to the test file imports.

- [ ] **Step 2: Run the test and confirm it fails**

```bash
go test ./internal/accounts/ -run TestMarkQuotaExhaustedPersists -v
```

Expected failure: `Load` cannot read the file, because none was written.

- [ ] **Step 3: Minimal implementation**

Restructure `MarkQuotaExhausted` to release the lock before saving (`SaveToDisk` takes `RLock` itself) and to save only on a real change:

```go
	manager.mu.Lock()
	changed := false
	for _, acc := range manager.accounts {
		if acc.Email != email {
			continue
		}
		if acc.Quota.Models == nil {
			acc.Quota.Models = make(map[string]ModelQuota)
		}
		exhausted := func() ModelQuota {
			zero := 0.0
			return ModelQuota{RemainingFraction: &zero, ResetTime: resetTime, ExhaustedUntilMS: reset.UnixMilli()}
		}
		changed = acc.Quota.Models[key].ExhaustedUntilMS != reset.UnixMilli()
		acc.Quota.Models[key] = exhausted()
		for _, pool := range quotaPoolsForModel(key, reset.Sub(manager.now())) {
			if _, seen := acc.Quota.Pools[pool]; !seen {
				continue
			}
			acc.Quota.Pools[pool] = exhausted()
		}
		acc.Quota.LastChecked = manager.now().UnixMilli()
		break
	}
	manager.mu.Unlock()
	if !changed {
		// Repeat 429s inside one window must not become a write storm.
		return
	}
	if err := manager.SaveToDisk(); err != nil {
		slog.Warn("persist quota exhaustion", "email", email, "model", key, "error", err)
	}
```

Extend the doc comment with one line: the record is persisted so a restart inside the window does not restore the phantom reading.

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/accounts/ -run TestMarkQuotaExhausted -v
go test ./internal/accounts/
```

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/quota_exhaustion_test.go
git commit -m "fix(quota): persist recorded exhaustion"
```

---

### Task 5: Shared reset-time parser and a diagnosable guard

The two 🔵 on `manager.go:1559` and `manager.go:1610`. One parser per package, and one debug line so a bar stuck at 0 can be explained from the logs.

**Files:**
- Modify: `internal/accounts/manager.go:1559` (`MarkQuotaExhausted`), `internal/accounts/manager.go:1608-1612` (`mergeQuota` guard)
- Test: `internal/accounts/quota_exhaustion_test.go`

**Interfaces:**
- Consumes: `parseQuotaResetTime` (`manager.go:1026`), which accepts RFC3339Nano and RFC3339.
- Produces: no signature change.

- [ ] **Step 1: Write the failing test**

```go
func TestMarkQuotaExhaustedAcceptsFractionalSeconds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("nano@example.com")
	manager := quotaTestManager(t, now, account)

	manager.MarkQuotaExhausted("nano@example.com", "gemini-3.8-flash-high", "2026-09-24T19:58:20.582931120Z")

	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.ExhaustedUntilMS == 0 {
		t.Fatalf("a fractional-second reset must parse like every other reset in the package: %+v", got)
	}
}
```

- [ ] **Step 2: Run the test and confirm it holds the contract**

```bash
go test ./internal/accounts/ -run TestMarkQuotaExhaustedAcceptsFractionalSeconds -v
```

This one may already pass — `time.Parse` with the RFC3339 layout tolerates a fractional part. It is a regression guard for the swap in Step 3, not a red test. If it passes now, keep it and go straight to Step 3; it must still pass after.

- [ ] **Step 3: Minimal implementation**

In `MarkQuotaExhausted`:

```go
	reset, ok := parseQuotaResetTime(resetTime)
	if !ok {
		return
	}
```

In the `mergeQuota` guard:

```go
		if previous, seen := (*target)[key]; seen && previous.ExhaustedUntilMS > manager.now().UnixMilli() {
			// A recorded exhaustion outranks the live reading until it expires.
			slog.Debug("live quota reading ignored; exhaustion still open",
				"email", email, "key", key, "until", time.UnixMilli(previous.ExhaustedUntilMS).UTC().Format(time.RFC3339))
			break
		}
```

Confirm `log/slog` is imported in `manager.go` (it is — used by `Load`).

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/accounts/
```

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/quota_exhaustion_test.go
git commit -m "refactor(quota): share reset parser, log guard"
```

---

### Task 6: Cover the second hook site

The last 🔵. Only the `StreamGenerateContent` hook (`dispatcher.go:468`) is tested; the `rotateForError` hook (`dispatcher.go:706`) — the path every non-stream 429 takes — has no test at all.

**Files:**
- Test only: `internal/accounts/quota_exhaustion_test.go`

**Interfaces:**
- Consumes: `(*Dispatcher).rotateForError` (`dispatcher.go:667`), `cloudcode.HTTPError`, `individualQuotaBody` (already in the test file).

- [ ] **Step 1: Write the test**

```go
func TestRotateForErrorRecordsQuotaExhaustion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 16, 53, 0, 0, time.UTC)
	account := testAccount("rotate@example.com")
	manager := quotaTestManager(t, now, account)
	dispatcher := newTestDispatcher(t, manager, &staticResolver{tokens: map[string]string{}},
		map[string]*scriptedClient{}, now, func(context.Context, time.Duration) error { return nil })

	quotaError := &cloudcode.HTTPError{
		Endpoint: cloudcode.DailyEndpoint, StatusCode: http.StatusTooManyRequests,
		Status: "429 Too Many Requests", Body: individualQuotaBody,
	}
	if !dispatcher.rotateForError(account, "gemini-3.8-flash-high", quotaError) {
		t.Fatal("a 429 must rotate")
	}

	got := account.Quota.Models["gemini-3.8-flash-high"]
	if got.RemainingFraction == nil || *got.RemainingFraction != 0 {
		t.Fatalf("the non-stream 429 path must record the exhaustion too: %+v", got)
	}
	if got.ResetTime != "2026-09-24T19:58:20Z" {
		t.Fatalf("reset time = %q", got.ResetTime)
	}
}
```

- [ ] **Step 2: Run it**

```bash
go test ./internal/accounts/ -run TestRotateForErrorRecordsQuotaExhaustion -v
```

It should pass against the code from Tasks 1–5 (`rotateForError` already calls `recordQuotaExhaustion`). If it fails, the hook is broken — fix the hook, not the test.

- [ ] **Step 3: Commit**

```bash
git add internal/accounts/quota_exhaustion_test.go
git commit -m "test(quota): cover non-stream 429 hook site"
```

---

## Final verification

- [ ] Full gate, all three green:

```bash
go build ./...
go vet ./...
go test ./...
```

- [ ] Race check on the package the plan touches:

```bash
go test -race ./internal/accounts/
```

- [ ] Push:

```bash
git push fork fix/gemini-quota-exhaustion
```

- [ ] Reply on the PR with what changed per finding and the final test count.

## Out of scope

- The judgement call that one model's 429 marks its whole family's pools — upstream gives no per-model pool signal, and Task 2 already bounds the damage by horizon.
- The `UpstreamID`/catalog-id split in the *live* quota readings (`dispatcher.go:829-841` keys pools and models by upstream bucket id). It is the same key-space mismatch as Task 3 but predates PR #90; Task 3 fixes only the path this PR adds.
- Any web UI change. The rows and pools are rendered from the API payload; nothing in `internal/webui` needs to know about `exhaustedUntilMs`.
