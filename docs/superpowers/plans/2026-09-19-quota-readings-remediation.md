# Quota Readings Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop Cloud Code quota pool buckets from polluting per-model quota readings, keep readings fresh after traffic, and stop fabricating 0% for unknown quota.

**Architecture:** Add a separate `Quota.Pools` map beside `Quota.Models` in `internal/accounts`; route `RetrieveUserQuotaSummary` group buckets into pools and `RetrieveUserQuota` per-model buckets into models with one canonical key each. Add a 60s-throttled live-quota refresh on the GenerateContent success path. Drop the nil-fraction→0.0 coercion in the model catalog so "no fraction" stays unknown (UI renders N/A) instead of exhausted.

**Tech Stack:** Go (stdlib), repo test style = plain `testing` + table fakes implementing `CloudClient` (see `internal/accounts/quota_merge_test.go:91-121` for the fake pattern).

**Spec:** Investigation findings 2026-09-19 (this session): root causes A (phantom rows), B (stale readings), C (false 0% coercion). Evidence probes: `/tmp/quotaprobe.out`, `/tmp/quotaprobe2.out`; live repro via running proxy on :8080.

## Global Constraints

- Go module: `antigravity-go-proxy`; run tests with `go test ./...` from repo root.
- No new dependencies.
- `Quota.Models` keys and pool keys are lowercase-canonical; model lookups go through `accounts.ModelKeyCandidates` (manager.go:426) — do not change that contract.
- `MergeQuotaFraction` documented contract (nil fraction + reset = exhaustion 0.0) stays unchanged for per-model `RetrieveUserQuota` buckets; only the catalog/fallback fabrication (Task 4) changes.
- `quotaCriticalLocked` (manager.go:1068) must treat `RemainingFraction == nil` as "not critical" — already true (manager.go:1070); do not regress.
- `quotaCapableClient` test fake (quota_merge_test.go:91) is shared by several tests — extend it, do not rename fields other tests rely on.
- Commit style: conventional commits, subject ≤50 chars (repo history pattern, e.g. `fix(quota): ...`).
- Web UI files are plain JS served from `internal/webui/public`; no build step — do not touch UI in this plan (phantom rows disappear at the API layer).

---

### Task 1: `Quota.Pools` map + `Manager.MergeQuotaPool`

**Files:**
- Modify: `internal/accounts/manager.go:58-61` (Quota struct), `internal/accounts/manager.go:1504-1530` (MergeQuotaFraction)
- Test: `internal/accounts/quota_merge_test.go`

**Interfaces:**
- Consumes: existing `ModelQuota` (manager.go:53-56).
- Produces: `Quota.Pools map[string]ModelQuota` (JSON tag `pools,omitempty`); `func (manager *Manager) MergeQuotaPool(email, key string, fraction *float64, resetTime string)`. Task 2's dispatcher calls `MergeQuotaPool`.

- [x] **Step 1: Write the failing test**

Add to `internal/accounts/quota_merge_test.go` (after `TestMergeQuotaFraction_IgnoresEmpty`):

```go
func TestMergeQuotaPool_KeepsPoolsOutOfModels(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	account := testAccount("pools@example.com")
	manager := quotaTestManager(t, now, account)

	half := 0.5
	manager.MergeQuotaPool("pools@example.com", "gemini-5h", &half, "2026-09-20T01:00:00Z")
	manager.MergeQuotaPool("pools@example.com", "", &half, "2026-09-20T01:00:00Z")

	got, ok := account.Quota.Pools["gemini-5h"]
	if !ok || got.RemainingFraction == nil || *got.RemainingFraction != 0.5 {
		t.Fatalf("pool reading must land in Quota.Pools: %+v ok=%v", got, ok)
	}
	if got.ResetTime != "2026-09-20T01:00:00Z" {
		t.Fatalf("pool reset time not stored: %+v", got)
	}
	if len(account.Quota.Models) != 0 {
		t.Fatalf("pool reading must not enter Quota.Models: %+v", account.Quota.Models)
	}
	if account.Quota.LastChecked != now.UnixMilli() {
		t.Fatalf("pool merge must refresh LastChecked: %+v", account.Quota.LastChecked)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts/ -run TestMergeQuotaPool -v`
Expected: FAIL — `account.Quota.Pools` is an unknown field (compile error) or empty map.

- [x] **Step 3: Implement**

In `internal/accounts/manager.go`, change the `Quota` struct (line 58-61) to:

```go
type Quota struct {
	Models      map[string]ModelQuota `json:"models"`
	Pools       map[string]ModelQuota `json:"pools,omitempty"`
	LastChecked any                   `json:"lastChecked"`
}
```

Replace `MergeQuotaFraction` (lines 1504-1530) with the shared helper plus two thin wrappers. Keep the existing doc comment text for `MergeQuotaFraction`:

```go
// MergeQuotaFraction additively records one live quota reading under key.
// Quota-summary buckets are authoritative over static catalog fractions, so a
// merged reading overwrites whatever fetchAvailableModels stored. A nil
// fraction paired with a reset time records exhaustion (0.0), mirroring
// updateAccountQuota. Keys with neither fraction nor reset are ignored.
func (manager *Manager) MergeQuotaFraction(email, key string, fraction *float64, resetTime string) {
	manager.mergeQuota(email, key, fraction, resetTime, false)
}

// MergeQuotaPool records one live quota-pool reading under key. Pools are
// shared upstream buckets (e.g. gemini-5h, 3p-weekly) — never models — so
// they are kept out of Quota.Models and cannot surface as model rows.
func (manager *Manager) MergeQuotaPool(email, key string, fraction *float64, resetTime string) {
	manager.mergeQuota(email, key, fraction, resetTime, true)
}

func (manager *Manager) mergeQuota(email, key string, fraction *float64, resetTime string, pools bool) {
	if key == "" || (fraction == nil && resetTime == "") {
		return
	}
	if fraction == nil {
		zero := 0.0
		fraction = &zero
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, acc := range manager.accounts {
		if acc.Email != email {
			continue
		}
		target := &acc.Quota.Models
		if pools {
			target = &acc.Quota.Pools
		}
		if *target == nil {
			*target = make(map[string]ModelQuota)
		}
		(*target)[key] = ModelQuota{RemainingFraction: fraction, ResetTime: resetTime}
		acc.Quota.LastChecked = manager.now().UnixMilli()
		break
	}
}
```

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -v`
Expected: all PASS, including pre-existing `TestMergeQuotaFraction_*` (behavior unchanged for models).

- [x] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/quota_merge_test.go
git commit -m "feat(accounts): separate quota pools from model quotas"
```

---

### Task 2: Route summary buckets to pools; one canonical key per bucket

**Files:**
- Modify: `internal/accounts/dispatcher.go:766-821` (`refreshLiveQuota`, `mergeQuotaBucket`)
- Test: `internal/accounts/quota_merge_test.go`

**Interfaces:**
- Consumes: `Manager.MergeQuotaPool` (Task 1), `cloudcode.ParseQuotaSummary` / `cloudcode.ParseUserQuota` (internal/cloudcode/quota.go:38,86 — unchanged).
- Produces: `func (dispatcher *Dispatcher) mergeQuotaReading(email, key string, fraction *float64, amount *int64, resetTime string, pool bool)` — replaces `mergeQuotaBucket`. After this task `Quota.Models` contains only per-model `RetrieveUserQuota` bucket keys; summary group buckets land in `Quota.Pools` under `strings.ToLower(strings.TrimSpace(bucketID))`. `/account-limits` model list (management.go:379-392) iterates `Quota.Models` only, so phantom rows disappear with no API-layer change (the `quota` field at management.go:554 serializes `pools` automatically).

- [x] **Step 1: Rewrite the failing tests**

In `internal/accounts/quota_merge_test.go`, extend the fake (line 112-114) to also serve a per-model bucket:

```go
func (quotaCapableClient) RetrieveUserQuota(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: 200, Body: []byte(`{"buckets": [
		{"modelId": "gemini-3.8-flash-high", "remainingFraction": 0.6,
		 "resetTime": "2026-09-20T01:00:19Z", "tokenType": "WTUS"}
	]}`)}, nil
}
```

Update the summary fake's bucket (line 105-110) to a real group-pool bucket:

```go
func (quotaCapableClient) RetrieveUserQuotaSummary(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: 200, Body: []byte(`{
		"groups": [{"displayName": "Gemini Models",
			"buckets": [{"bucketId": "gemini-5h", "displayName": "Five Hour Limit Remaining",
				"remainingFraction": 0.4, "resetTime": "2026-09-20T00:00:00Z", "window": "5h"}]}]
	}`)}, nil
}
```

Rewrite `TestFetchAvailableModelsMergesLiveQuota` (line 123) and add the guard test:

```go
func TestFetchAvailableModelsMergesLiveQuota(t *testing.T) {
	dispatcher := newDispatcherWithClient(t, quotaCapableClient{})

	if _, err := dispatcher.FetchAvailableModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := dispatcher.manager.accounts[0]

	got, ok := account.Quota.Models["gemini-3.8-flash-high"]
	if !ok || got.RemainingFraction == nil || *got.RemainingFraction != 0.6 {
		t.Fatalf("per-model reading must overwrite static catalog 1.0: %+v ok=%v", got, ok)
	}

	pool, ok := account.Quota.Pools["gemini-5h"]
	if !ok || pool.RemainingFraction == nil || *pool.RemainingFraction != 0.4 {
		t.Fatalf("summary pool bucket must land in Quota.Pools: %+v ok=%v", pool, ok)
	}
	for key := range account.Quota.Models {
		if key == "gemini-5h" || key == "Five Hour Limit Remaining" || key == "five hour limit remaining" {
			t.Fatalf("pool bucket or display name leaked into Quota.Models: %q", key)
		}
	}
}

func TestRefreshLiveQuota_DisplayNamesDoNotDuplicateKeys(t *testing.T) {
	dispatcher := newDispatcherWithClient(t, quotaCapableClient{})
	account := dispatcher.manager.accounts[0]

	dispatcher.refreshLiveQuota(context.Background(), account,
		quotaCapableClient{}, "p")

	if _, dup := account.Quota.Pools["five hour limit remaining"]; dup {
		t.Fatalf("display name must not become a second pool key: %+v", account.Quota.Pools)
	}
	if n := len(account.Quota.Pools); n != 1 {
		t.Fatalf("expected exactly 1 pool key, got %d: %+v", n, account.Quota.Pools)
	}
}
```

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/accounts/ -run 'TestFetchAvailableModelsMergesLiveQuota|TestRefreshLiveQuota' -v`
Expected: FAIL — pools empty (buckets still merged into Models).

- [x] **Step 3: Implement**

In `internal/accounts/dispatcher.go`, replace `refreshLiveQuota` (lines 772-796) and `mergeQuotaBucket` (lines 798-821) with:

```go
// refreshLiveQuota best-effort merges live quota readings over the static
// catalog fractions. RetrieveUserQuotaSummary group buckets are shared pools
// (gemini-5h, 3p-weekly, ...) — they land in Quota.Pools, never in
// Quota.Models, so they cannot surface as phantom model rows.
// RetrieveUserQuota buckets are model-keyed and merge into Quota.Models.
// Upstream holds catalog remainingFraction at 1 until exhaustion, so without
// this the dashboard never shows consumption. Failures are swallowed: the
// catalog fractions remain the fallback. Clients without the capability
// (test fakes) skip silently.
func (dispatcher *Dispatcher) refreshLiveQuota(ctx context.Context, account *Account, client CloudClient, project string) {
	if account == nil || client == nil {
		return
	}
	if fetcher, ok := client.(quotaSummaryFetcher); ok {
		if response, err := fetcher.RetrieveUserQuotaSummary(ctx, project); err == nil &&
			response.StatusCode >= 200 && response.StatusCode < 300 {
			for _, bucket := range cloudcode.ParseQuotaSummary(response.Body) {
				dispatcher.mergeQuotaReading(account.Email,
					strings.ToLower(strings.TrimSpace(bucket.ID)),
					bucket.RemainingFraction, bucket.RemainingAmount, bucket.ResetTime, true)
			}
		}
	}
	if fetcher, ok := client.(userQuotaFetcher); ok {
		if response, err := fetcher.RetrieveUserQuota(ctx, project); err == nil &&
			response.StatusCode >= 200 && response.StatusCode < 300 {
			for _, bucket := range cloudcode.ParseUserQuota(response.Body) {
				dispatcher.mergeQuotaReading(account.Email,
					strings.ToLower(strings.TrimSpace(bucket.ModelID)),
					bucket.RemainingFraction, bucket.RemainingAmount, bucket.ResetTime, false)
			}
		}
	}
}

// mergeQuotaRecording stores one live reading under a single canonical key.
// An amount-only reading is actionable only at zero (exhausted): a positive
// amount without its total cannot produce a fraction, so it is skipped
// rather than fabricated.
func (dispatcher *Dispatcher) mergeQuotaReading(email, key string, fraction *float64, amount *int64, resetTime string, pool bool) {
	if fraction == nil {
		if amount == nil || *amount != 0 {
			return
		}
	}
	if pool {
		dispatcher.manager.MergeQuotaPool(email, key, fraction, resetTime)
		return
	}
	dispatcher.manager.MergeQuotaFraction(email, key, fraction, resetTime)
}
```

`strings` is already imported in dispatcher.go (used by truncateBodyForLog). No other callers of `mergeQuotaBucket` exist (verified by grep before editing).

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ ./internal/api/ -v`
Expected: all PASS. If `internal/api/management_test.go` tests asserted phantom keys in limits (grep for `3p-` / `Five Hour` first — none found during planning), fix expectations to match reality: pool keys only in `quota.pools`.

- [x] **Step 5: Commit**

```bash
git add internal/accounts/dispatcher.go internal/accounts/quota_merge_test.go
git commit -m "fix(quota): route summary pools to Quota.Pools with canonical keys"
```

---

### Task 3: Throttled live-quota refresh after GenerateContent

**Files:**
- Modify: `internal/accounts/dispatcher.go` (Dispatcher struct ~line 97-101; new const + method near `refreshLiveQuota`; success block at lines 409-422)
- Test: `internal/accounts/quota_merge_test.go`

**Interfaces:**
- Consumes: `refreshLiveQuota` (Task 2 shape), `dispatcher.now()`, `dispatcher.mu`.
- Produces: `const liveQuotaRefreshInterval = time.Minute`; `func (dispatcher *Dispatcher) refreshLiveQuotaThrottled(ctx context.Context, account *Account, client CloudClient, project string)`. `FetchAvailableModels` (dispatcher.go:292) and `RefreshAccount` (dispatcher.go:606) keep calling `refreshLiveQuota` directly — forced, unthrottled, behavior preserved.

- [x] **Step 1: Write the failing test**

Add to `internal/accounts/quota_merge_test.go`:

```go
type countingSummaryClient struct {
	quotaCapableClient
	summaryCalls int
}

func (c *countingSummaryClient) RetrieveUserQuotaSummary(ctx context.Context, project string) (cloudcode.Response, error) {
	c.summaryCalls++
	return quotaCapableClient{}.RetrieveUserQuotaSummary(ctx, project)
}

func TestStreamGenerateContent_RefreshesLiveQuotaThrottled(t *testing.T) {
	client := &countingSummaryClient{}
	dispatcher := newDispatcherWithClient(t, client)
	account := dispatcher.manager.accounts[0]

	consume := func(cloudcode.SSEEvent) error { return nil }
	if _, err := dispatcher.StreamGenerateContent(context.Background(),
		map[string]any{"model": "gemini-3.8-flash-high"}, consume); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.StreamGenerateContent(context.Background(),
		map[string]any{"model": "gemini-3.8-flash-high"}, consume); err != nil {
		t.Fatal(err)
	}

	if client.summaryCalls != 1 {
		t.Fatalf("expected 1 throttled summary fetch across 2 requests, got %d", client.summaryCalls)
	}
	if _, ok := account.Quota.Pools["gemini-5h"]; !ok {
		t.Fatalf("post-request refresh must record pools: %+v", account.Quota.Pools)
	}
}
```

Note: `countingSummaryClient` must satisfy `CloudClient` — embedding `quotaCapableClient` provides the other methods; the pointer receiver for the override is fine because the test passes `&countingSummaryClient{}`.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts/ -run TestStreamGenerateContent_RefreshesLiveQuotaThrottled -v`
Expected: FAIL — `refreshLiveQuotaThrottled` undefined (compile error).

- [x] **Step 3: Implement**

In `internal/accounts/dispatcher.go`:

1. Add a field to the `Dispatcher` struct, next to `clients` / `modelsFetch` (~line 97-101):

```go
	// liveQuotaRefreshMS is the last live-quota refresh per account email,
	// guarded by mu; it throttles post-request quota RPCs.
	liveQuotaRefreshMS map[string]int64
```

2. Initialize it in `NewDispatcher` alongside the other map initializations (`clients`):

```go
		liveQuotaRefreshMS: make(map[string]int64),
```

3. Add the const and method near `refreshLiveQuota`:

```go
// liveQuotaRefreshInterval throttles the two quota RPCs issued after a
// successful GenerateContent: upstream readings move slowly and
// per-request refreshes would triple hot-loop request cost. The catalog
// fetch path (fetchAvailableModels) and manual RefreshAccount stay
// unthrottled — they call refreshLiveQuota directly.
const liveQuotaRefreshInterval = time.Minute

func (dispatcher *Dispatcher) refreshLiveQuotaThrottled(ctx context.Context, account *Account, client CloudClient, project string) {
	if account == nil {
		return
	}
	dispatcher.mu.Lock()
	now := dispatcher.now().UnixMilli()
	last, seen := dispatcher.liveQuotaRefreshMS[account.Email]
	if seen && now-last < liveQuotaRefreshInterval.Milliseconds() {
		dispatcher.mu.Unlock()
		return
	}
	dispatcher.liveQuotaRefreshMS[account.Email] = now
	dispatcher.mu.Unlock()
	dispatcher.refreshLiveQuota(ctx, account, client, project)
}
```

4. Call it in the `StreamGenerateContent` success block, right after `UpdateAccountCredits` (dispatcher.go:415, inside `if requestErr == nil`):

```go
				dispatcher.refreshLiveQuotaThrottled(ctx, account, client, project)
```

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -v`
Expected: all PASS, including `TestStreamGenerateContentRecordsRemainingCredits` (unthrottled pre-existing test still works because its dispatcher is fresh — last refresh was never recorded).

- [x] **Step 5: Run the full suite (race detector)**

Run: `go test -race ./internal/accounts/ ./internal/api/`
Expected: PASS. The map is guarded by `mu`; `dispatcher.now()` is safe outside the lock.

- [x] **Step 6: Commit**

```bash
git add internal/accounts/dispatcher.go internal/accounts/quota_merge_test.go
git commit -m "feat(quota): throttled live-quota refresh after GenerateContent"
```

---

### Task 4: Unknown quota fraction stays unknown (no fabricated 0%)

**Files:**
- Modify: `internal/modelcatalog/catalog.go:178-182` (Parse coercion), `internal/accounts/dispatcher.go:717-741` (`updateAccountQuota` fallback branch)
- Test: `internal/modelcatalog/catalog_test.go`, `internal/accounts/dispatcher_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `modelcatalog.Model.QuotaRemainingFraction` stays `nil` when upstream sends `quotaInfo` with a `resetTime` but no `remainingFraction` (currently coerced to 0.0). Consumers verified nil-safe: `quotaCriticalLocked` (manager.go:1070), `scoreLocked` (manager.go:1108), `quotaUsableLocked` (manager.go:1061), `/account-limits` renders "N/A" (management.go:519-521), `/v1/usage` **omits** nil-fraction models (server.go:638-640) — behavior change documented in Step 6.

- [x] **Step 1: Rewrite the failing tests**

In `internal/modelcatalog/catalog_test.go`, replace `TestParseHandlesExhaustedQuotaWithNullRemainingFraction` (lines 55-80) — the same fixture now asserts unknown, not exhausted:

```go
func TestParseKeepsNilRemainingFractionUnknown(t *testing.T) {
	t.Parallel()
	catalog, err := Parse([]byte(`{
		"defaultAgentModelId":"gemini-3.5-flash-low",
		"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":["gemini-3.5-flash-low"]}]}],
		"models":{
			"gemini-3.5-flash-low":{"displayName":"Gemini 3.5 Flash (Low)","quotaInfo":{"resetTime":"2026-08-14T12:00:00Z"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.Selectable()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].QuotaRemainingFraction != nil {
		t.Fatalf("nil remainingFraction must stay nil (unknown), got %f", *models[0].QuotaRemainingFraction)
	}
	if models[0].QuotaResetTime != "2026-08-14T12:00:00Z" {
		t.Fatalf("reset time must be preserved: %s", models[0].QuotaResetTime)
	}
}
```

In `internal/accounts/dispatcher_test.go`, add a test for the non-catalog fallback branch (near `TestUpdateAccountQuotaPopulatesGemini38FlashFamily`, line 212):

```go
func TestUpdateAccountQuota_FallbackNilFractionStaysUnknown(t *testing.T) {
	dispatcher := newDispatcherWithClient(t, stubResolverClient{})
	acc := &Account{Email: "test@example.com", Enabled: true}
	fallbackBody := []byte(`{"models":{"gemini-3.7-flash-high":{"quotaInfo":{"resetTime":"2026-09-20T01:00:00Z"}}}}`)

	dispatcher.updateAccountQuota(acc, fallbackBody)

	got, ok := acc.Quota.Models["gemini-3.7-flash-high"]
	if !ok {
		t.Fatalf("reset-only entry must be recorded: %+v", acc.Quota.Models)
	}
	if got.RemainingFraction != nil {
		t.Fatalf("nil fraction must stay nil, got %f", *got.RemainingFraction)
	}
	if got.ResetTime != "2026-09-20T01:00:00Z" {
		t.Fatalf("reset time not preserved: %+v", got)
	}
}
```

Check what client stub `TestUpdateAccountQuotaPopulatesGemini38FlashFamily` uses for its dispatcher (line 212-238) and reuse the same stub type in place of `stubResolverClient` — the client is never called by `updateAccountQuota`, any working stub is fine.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/modelcatalog/ ./internal/accounts/ -run 'TestParseKeepsNil|TestUpdateAccountQuota_FallbackNil' -v`
Expected: FAIL — coercion produces non-nil 0.0.

- [x] **Step 3: Implement**

In `internal/modelcatalog/catalog.go` (lines 178-182), delete the coercion and keep only:

```go
		// remainingFraction nil means unknown: upstream sends resetTime-only
		// quotaInfo when the pool has no fraction to report. Do not fabricate
		// 0.0 — callers treat nil as "not critical" and the UI renders N/A.
		remaining := details.QuotaInfo.RemainingFraction
```

In `internal/accounts/dispatcher.go` `updateAccountQuota` fallback branch (lines 730-734), delete the equivalent zero-coercion:

```go
			fraction := mData.QuotaInfo.RemainingFraction
			if fraction == nil && mData.QuotaInfo.ResetTime != "" {
				fraction = nil //nolint:ineffassign // explicit: unknown stays unknown
			}
```

This is a no-op `if` — prefer the honest form: delete lines 731-734 entirely and keep `fraction := mData.QuotaInfo.RemainingFraction`; the following `if fraction != nil || mData.QuotaInfo.ResetTime != ""` guard (line 735) already records reset-only entries.

- [x] **Step 4: Run the full test suite**

Run: `go test ./...`
Expected: all PASS. If any test asserted a fabricated 0% for reset-only quota (grep `quota_test`, `management_test`, `server_test` for `resetTime` + `0` expectations), update it to expect `null` remainingFraction / "N/A" — the fabricated 0% was the bug.

- [x] **Step 5: Manual verification against the running proxy**

Run: `go build -o /tmp/antigravity-proxy-new ./cmd/proxy && curl -s http://localhost:8080/account-limits | python3 -c "import json,sys; d=json.load(sys.stdin); print([m for m in d['models'] if '3p-' in m or 'Limit' in m or 'chat_' in m])"`
Expected (after deploying the new binary to :8080 and refreshing an account): `[]` — no phantom model keys. (Deploy only with the user's go-ahead; the running proxy at :8080 is user-owned.)

- [x] **Step 6: Commit**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go internal/accounts/dispatcher.go internal/accounts/dispatcher_test.go
git commit -m "fix(quota): treat nil remainingFraction as unknown, not exhausted"
```

Commit body note: `/v1/usage` now omits reset-only models instead of listing them at 100% used — intentional, unknown ≠ exhausted.

---

## Self-Review

- Spec coverage: A phantom rows → Task 1+2; B stale readings → Task 3; C false-0 coercion → Task 4. Per-request credit recording already exists (dispatcher.go:402-415) — not duplicated.
- Placeholder scan: no TBDs; every code step carries full code. Task 4 Step 1 has one instructed lookup (reuse existing stub type) with the exact reference test named.
- Type consistency: `MergeQuotaPool(email, key string, fraction *float64, resetTime string)` matches Task 2 call; `mergeQuotaReading(email, key string, fraction *float64, amount *int64, resetTime string, pool bool)` matches both call sites; `refreshLiveQuotaThrottled(ctx, account, client, project)` matches Task 3 call site.
