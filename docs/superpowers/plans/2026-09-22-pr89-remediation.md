# PR #89 Remediation Plan — catalog throttle exemption & cached `/account-limits`

- **PR:** [#89 — fix(proxy): exempt catalog refresh from request throttle; serve account-limits from cache](https://github.com/gustavokch/antigravity-claude-proxy-go/pull/89)
- **Branch:** `fix/catalog-throttle-exemption` (head `c0fb792`, base `main`)
- **Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/89#issuecomment-5782253221
- **Date:** 2026-09-22

## Goal

Keep the PR's core win — a model-catalog refresh that can no longer deadlock on
`requestDelayMs`, and an `/account-limits` poll that never blocks on upstream I/O —
while closing the regressions and fragilities the fresh review found:

1. Idle-proxy quota data must stay fresh even though `/account-limits` no longer
   blocks on a fetch.
2. A permanently failing upstream must not reintroduce a 30 s stall per poll.
3. A failed catalog refresh must degrade to the stale catalog instead of 504ing
   `/v1/messages`.
4. Removing the throttle sleep must not remove all spacing between account retries.

## Architecture

The catalog lives in `accounts.Dispatcher` (`catalog`, `catalogAt`, guarded by
`dispatcher.mu`), is written by `storeCatalog`, and is read by two very different
consumers:

| Consumer | Path | Requirement |
| --- | --- | --- |
| Hot generation | `resolveModel` → `FetchAvailableModels` | Must never 504 when a stale catalog exists |
| Status poll | `/account-limits` → `cachedModelCatalog` | Must never block; must still drive quota refresh |

`FetchAvailableModels` already single-flights through `startModelFetch` and bounds
each fetch to `fetchModelsTimeout` (30 s) on a background context, so a fire-and-forget
refresh is cheap and self-limiting. That is the lever for keeping the poll non-blocking
*and* the quota data live: the poll asks for a refresh, it does not wait for one.

Quota freshness is a side effect of the catalog fetch — `fetchAvailableModels` calls
`updateAccountQuota` and `refreshLiveQuota` on success (`internal/accounts/dispatcher.go:292-293`).
That side effect is what the PR accidentally dropped for idle proxies.

## Tech stack

Go 1.x, standard library `testing`, `net/http/httptest`. Test command is
`go test ./<pkg>/ -run <Name> -count=1` (add `-race` for the API package).

## Spec reference

`AGENTS.md` / repo conventions; the PR body; review findings in the comment linked above.

---

## Task 1 — Async catalog refresh keeps idle quota data fresh

Fixes the 🔴 finding: `/account-limits` no longer triggers `updateAccountQuota` /
`refreshLiveQuota`, so an idle proxy freezes the dashboard's quota percentages.

**Files**
- Modify: `internal/accounts/dispatcher.go`
- Modify: `internal/api/server.go`
- Modify: `internal/api/management.go`
- Test: `internal/accounts/retry_test.go`, `internal/api/management_test.go`

**Consumes:** `dispatcher.catalog`, `dispatcher.catalogAt`, `dispatcher.modelCacheTTL`, `startModelFetch`.
**Produces:** `Dispatcher.RefreshCatalogIfStale()`, extended `cachedCatalogBackend` interface.

### Step 1 — Write the failing tests

In `internal/accounts/retry_test.go`:

```go
// A status poll must not leave quota data frozen: when the cached catalog has
// aged past modelCacheTTL, RefreshCatalogIfStale kicks the shared single-flight
// fetch (which also runs updateAccountQuota/refreshLiveQuota) without blocking
// the caller.
func TestRefreshCatalogIfStaleKicksBackgroundFetch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	current := now
	account := testAccount("stale-catalog@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{}
	resolver := &staticResolver{tokens: map[string]string{account.Email: "tok-stale"}}
	dispatcher := newTestDispatcher(t, manager, resolver, map[string]*scriptedClient{"tok-stale": client}, now, nil)
	dispatcher.now = func() time.Time { return current }

	if _, err := dispatcher.FetchAvailableModels(context.Background()); err != nil {
		t.Fatalf("seed fetch failed: %v", err)
	}
	first := dispatcher.CachedCatalog()
	if first == nil {
		t.Fatal("seed fetch did not populate the catalog")
	}

	// Fresh catalog: no refresh.
	if dispatcher.RefreshCatalogIfStale() {
		t.Fatal("RefreshCatalogIfStale kicked a fetch while the catalog was still fresh")
	}

	// Age it past the TTL: refresh is requested, and the caller is not blocked.
	current = now.Add(10 * time.Minute)
	if !dispatcher.RefreshCatalogIfStale() {
		t.Fatal("RefreshCatalogIfStale did not kick a fetch for a stale catalog")
	}
	waitForCatalogAt(t, dispatcher, current)
}
```

Add the small helper next to the test (polls `catalogAt` under the lock with a
deadline, so the assertion does not race the background goroutine).

In `internal/api/management_test.go`, extend `cachedCatalogStub` with a
`refreshCalls` counter and a `RefreshCatalogIfStale() bool` method, then:

```go
func TestAccountLimitsRequestsRefreshForStaleCatalog(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	stub := &cachedCatalogStub{catalog: testCatalog(t), stale: true}
	server.backend = stub

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account-limits", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if stub.fetchCalls != 0 {
		t.Fatalf("blocking FetchAvailableModels calls=%d; want 0", stub.fetchCalls)
	}
	if stub.refreshCalls != 1 {
		t.Fatalf("RefreshCatalogIfStale calls=%d; want 1 (idle quota data must not freeze)", stub.refreshCalls)
	}
}
```

Factor the inline catalog JSON from `TestAccountLimitsServesCachedCatalogWithoutRefresh`
into a `testCatalog(t *testing.T) *modelcatalog.Catalog` helper so both tests share it.

### Step 2 — Confirm failure

```bash
go test ./internal/accounts/ -run TestRefreshCatalogIfStaleKicksBackgroundFetch -count=1
go test ./internal/api/ -run TestAccountLimitsRequestsRefreshForStaleCatalog -count=1
```

Expect compile failure: `RefreshCatalogIfStale` undefined.

### Step 3 — Minimal implementation

`internal/accounts/dispatcher.go`, next to `CachedCatalog`:

```go
// RefreshCatalogIfStale starts a background catalog refresh when the cached
// catalog is missing or older than modelCacheTTL, and reports whether one was
// requested. It never blocks: startModelFetch single-flights the work on a
// context bounded by fetchModelsTimeout. Status endpoints call this so a
// non-blocking poll still drives the quota refresh that rides along with a
// successful fetch (updateAccountQuota + refreshLiveQuota).
func (dispatcher *Dispatcher) RefreshCatalogIfStale() bool {
	dispatcher.mu.RLock()
	fresh := dispatcher.catalog != nil && dispatcher.now().Sub(dispatcher.catalogAt) < dispatcher.modelCacheTTL
	dispatcher.mu.RUnlock()
	if fresh {
		return false
	}
	dispatcher.startModelFetch()
	return true
}
```

`internal/api/server.go` — extend the interface:

```go
type cachedCatalogBackend interface {
	CachedCatalog() *modelcatalog.Catalog
	RefreshCatalogIfStale() bool
}

// refreshModelCatalogIfStale asks a retaining backend to refresh a stale
// catalog in the background. It never blocks and is a no-op for backends that
// do not retain a catalog.
func (server *Server) refreshModelCatalogIfStale() {
	if backend, ok := server.backend.(cachedCatalogBackend); ok {
		backend.RefreshCatalogIfStale()
	}
}
```

`internal/api/management.go` — call it right after reading the cache:

```go
catalog := server.cachedModelCatalog()
server.refreshModelCatalogIfStale()
if catalog == nil {
	...
}
```

Note the interface widening means `cachedCatalogStub` in the existing test must
gain the new method or `cachedModelCatalog` silently degrades to nil — the
existing `TestAccountLimitsServesCachedCatalogWithoutRefresh` is the guard for that.

### Step 4 — Confirm pass

```bash
go test ./internal/accounts/ ./internal/api/ -count=1
go test -race ./internal/api/ -run 'TestAccountLimits' -count=1
```

### Step 5 — Commit

```bash
git add internal/accounts/dispatcher.go internal/accounts/retry_test.go internal/api/server.go internal/api/management.go internal/api/management_test.go
git commit -m "fix(limits): refresh a stale catalog in the background so idle quota data cannot freeze"
```

---

## Task 2 — Cold-start fetch must not stall every poll forever

Fixes the 🟡 finding: the `catalog == nil` guard is not "an attempt was made", so a
permanently failing upstream reintroduces a 30 s blocking fetch on every poll.

**Files**
- Modify: `internal/api/management.go`, `internal/api/server.go`
- Test: `internal/api/management_test.go`

**Consumes:** `server.cachedModelCatalog`, `server.fetchModelCatalog`.
**Produces:** a cold-start attempt gate on `Server` (`coldCatalogAttemptAt time.Time`, guarded by an existing/new mutex).

### Step 1 — Write the failing test

```go
// With no catalog ever fetched, the first poll may pay one blocking fetch —
// but a permanently failing upstream must not charge every later poll 30s.
func TestAccountLimitsColdStartFetchesAtMostOncePerCooldown(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	stub := &cachedCatalogStub{catalog: nil} // FetchAvailableModels returns DeadlineExceeded
	server.backend = stub

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account-limits", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("poll %d: expected 200, got %d", i, rec.Code)
		}
	}
	if stub.fetchCalls != 1 {
		t.Fatalf("blocking fetch calls=%d; want 1 (a failing upstream must not stall every poll)", stub.fetchCalls)
	}
}
```

### Step 2 — Confirm failure

```bash
go test ./internal/api/ -run TestAccountLimitsColdStartFetchesAtMostOncePerCooldown -count=1
```

Expect `fetchCalls=3`.

### Step 3 — Minimal implementation

Add to `Server`:

```go
// coldCatalogCooldown bounds how often a catalog-less server pays a blocking
// catalog fetch on a status poll. Without it, an upstream that never recovers
// charges every poll the full fetchModelsTimeout.
const coldCatalogCooldown = 60 * time.Second
```

and a small guarded helper (`server.allowColdCatalogFetch() bool`) that records the
attempt time under the server mutex and returns false inside the cooldown. Wire it
into `handleAccountLimits`:

```go
if catalog == nil && server.allowColdCatalogFetch() {
	if fresh, err := server.fetchModelCatalog(request.Context()); err == nil {
		catalog = fresh
	}
}
```

Reuse the server's existing mutex if one is already in scope; otherwise add a
dedicated `catalogMu sync.Mutex` rather than widening an unrelated lock.

### Step 4 — Confirm pass

```bash
go test ./internal/api/ -count=1
go test -race ./internal/api/ -run 'TestAccountLimits' -count=1
```

### Step 5 — Commit

```bash
git add internal/api/server.go internal/api/management.go internal/api/management_test.go
git commit -m "fix(limits): cooldown the cold-start catalog fetch so a dead upstream cannot stall every poll"
```

---

## Task 3 — `resolveModel` falls back to the stale catalog instead of 504ing

Fixes the 🟡 finding: the throttle was only one trigger; any refresh failure past
the TTL still turns every `/v1/messages` into `refresh selectable models: ...` → 504.

**Files**
- Modify: `internal/accounts/dispatcher.go`
- Test: `internal/accounts/retry_test.go`

**Consumes:** `dispatcher.catalog`, `FetchAvailableModels`.
**Produces:** degrade-to-stale behavior in `resolveModel`.

### Step 1 — Write the failing test

```go
// A catalog refresh that fails must not take generation down with it: a stale
// catalog still resolves models. Only a total absence of catalog is fatal.
func TestResolveModelFallsBackToStaleCatalogOnRefreshFailure(t *testing.T) {
	t.Parallel()
	// Seed a catalog, age it past the TTL, then make the upstream models call fail.
	// StreamGenerateContent must still resolve "claude-sonnet-4-6" and reach the
	// client rather than returning a "refresh selectable models" error.
}
```

Implement with a `scriptedClient` variant whose `FetchAvailableModels` succeeds once
then returns an error (add a `modelsErrAfter int` field, or a dedicated
`failingModelsClient`), plus a movable `now` so the TTL can be crossed.

### Step 2 — Confirm failure

```bash
go test ./internal/accounts/ -run TestResolveModelFallsBackToStaleCatalogOnRefreshFailure -count=1
```

Expect `refresh selectable models: ...`.

### Step 3 — Minimal implementation

```go
	if !fresh {
		response, err := dispatcher.FetchAvailableModels(ctx)
		if err != nil {
			// Degrade, do not fail: a stale catalog resolves models correctly
			// for every model that already existed. Returning here instead
			// turned any refresh failure into a 504 on every request past the
			// TTL. Only a total absence of catalog is fatal.
			if catalog == nil {
				return modelcatalog.Model{}, fmt.Errorf("refresh selectable models: %w", err)
			}
			slog.Warn("[Server] Model catalog refresh failed; serving the stale catalog", "error", err)
			return catalog.ResolveWithRequest(requested, request)
		}
		...
	}
```

Leave the caller's context-cancellation semantics alone: when `ctx` itself is done,
the request is already going away and the stale resolve is harmless.

### Step 4 — Confirm pass

```bash
go test ./internal/accounts/ -count=1
```

### Step 5 — Commit

```bash
git add internal/accounts/dispatcher.go internal/accounts/retry_test.go
git commit -m "fix(catalog): serve the stale catalog when a refresh fails instead of 504ing generation"
```

---

## Task 4 — Clamp retry spacing in `fetchAvailableModels` instead of removing it

Fixes the 🟡 finding: deleting the sleep also deleted the only spacing between
account retries, so a 429 wave now fires one immediate list-models call per account.

**Files**
- Modify: `internal/accounts/dispatcher.go`
- Test: `internal/accounts/retry_test.go`

**Consumes:** `dispatcher.requestThrottlingEnabled`, `dispatcher.requestDelay`, `dispatcher.sleep`.
**Produces:** a bounded inter-attempt pause (`catalogRetryPauseCeiling`).

### Step 1 — Write the failing test

```go
// Retries across accounts still need spacing — just not a full requestDelayMs,
// which can exceed the whole fetch budget. The first attempt pays nothing; each
// retry pays at most catalogRetryPauseCeiling.
func TestFetchAvailableModelsClampsRetrySpacing(t *testing.T) {
	// Two accounts, first client fails with a rotate-worthy error, second succeeds.
	// Assert: exactly one sleep, and its duration == catalogRetryPauseCeiling
	// (not the configured 60s requestDelayMs).
}
```

Keep the existing `TestFetchAvailableModelsIgnoresRequestThrottle` as the guard that
the *first* attempt never sleeps.

### Step 2 — Confirm failure

```bash
go test ./internal/accounts/ -run TestFetchAvailableModelsClampsRetrySpacing -count=1
```

Expect zero sleeps recorded.

### Step 3 — Minimal implementation

```go
// catalogRetryPauseCeiling caps the spacing between catalog-fetch retries. The
// full requestDelayMs cannot apply here: a delay above fetchModelsTimeout would
// guarantee "context deadline exceeded" on every refresh. Dropping the pause
// entirely is the other extreme — the loop would then fire one immediate
// list-models call per account during a 429 wave.
const catalogRetryPauseCeiling = time.Second
```

and inside the loop, gated on `attempt > 0`:

```go
		if attempt > 0 {
			dispatcher.mu.RLock()
			throttling := dispatcher.requestThrottlingEnabled
			delay := dispatcher.requestDelay
			dispatcher.mu.RUnlock()
			if throttling && delay > 0 {
				if err := dispatcher.sleep(ctx, min(delay, catalogRetryPauseCeiling)); err != nil {
					return cloudcode.Response{}, err
				}
			}
		}
```

Rewrite the existing block comment to describe the clamp rather than the removal.

### Step 4 — Confirm pass

```bash
go test ./internal/accounts/ -count=1
```

### Step 5 — Commit

```bash
git add internal/accounts/dispatcher.go internal/accounts/retry_test.go
git commit -m "fix(catalog): clamp retry spacing instead of dropping it entirely"
```

---

## Task 5 — Make the throttle-exemption test reproduce the real failure

Fixes the 🔵 finding: the current assertion ("slept 0 times") passes trivially
against a stub sleep that never blocks, so it never exercises the deadline.

**Files**
- Modify: `internal/accounts/retry_test.go`

### Step 1 — Strengthen the test

Replace the no-op sleep with one that honors the context and actually waits:

```go
	sleep := func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		sleepDurations = append(sleepDurations, d)
		mu.Unlock()
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
```

and drive `fetchAvailableModels` directly on a context whose deadline is far
shorter than the configured delay, asserting the fetch still succeeds:

```go
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := dispatcher.fetchAvailableModels(ctx); err != nil {
		t.Fatalf("catalog fetch failed under a budget shorter than requestDelayMs: %v", err)
	}
```

With the sleep restored this test would hang until the deadline and fail with
`context deadline exceeded` — which is exactly the production symptom.

### Step 2 — Confirm pass

```bash
go test ./internal/accounts/ -run TestFetchAvailableModelsIgnoresRequestThrottle -count=1
go test -race ./internal/accounts/ -run TestFetchAvailableModels -count=1
```

### Step 3 — Commit

```bash
git add internal/accounts/retry_test.go
git commit -m "test(catalog): reproduce the deadline failure the throttle exemption prevents"
```

---

## Task 6 — Warn when `requestDelayMs` exceeds the fetch budget

Fixes the 🔵 finding: the PR asks operators to lower an extreme `requestDelayMs`,
but nothing in the process tells them it is extreme.

**Files**
- Modify: `internal/accounts/dispatcher.go`
- Test: `internal/accounts/retry_test.go`

### Step 1 — Write the failing test

Assert that `UpdateConfig` with `RequestDelayMs: 60000` emits a warning (capture via
an `slog` handler installed with `slog.SetDefault` for the duration of the test, or
via an injected logger if the dispatcher grows one).

### Step 2 — Confirm failure

```bash
go test ./internal/accounts/ -run TestUpdateConfigWarnsOnExtremeRequestDelay -count=1
```

### Step 3 — Minimal implementation

At the end of `UpdateConfig`:

```go
	if dispatcher.requestThrottlingEnabled && dispatcher.requestDelay >= fetchModelsTimeout {
		slog.Warn("[Dispatcher] requestDelayMs paces every generation slower than one request per catalog-fetch budget; lower it unless this is deliberate",
			"requestDelayMs", dispatcher.requestDelay.Milliseconds(),
			"fetchModelsTimeoutMs", fetchModelsTimeout.Milliseconds())
	}
```

### Step 4 — Confirm pass

```bash
go test ./internal/accounts/ -count=1
```

### Step 5 — Commit

```bash
git add internal/accounts/dispatcher.go internal/accounts/retry_test.go
git commit -m "feat(config): warn when requestDelayMs paces slower than the catalog fetch budget"
```

---

## Task 7 — Drop the double parse in `resolveModel`

Fixes the 🔵 finding: `fetchAvailableModels` already parsed and stored the catalog
via `cacheCatalog`; `resolveModel` parses the same body again and re-stores it.

**Files**
- Modify: `internal/accounts/dispatcher.go`

**Note:** behavior-preserving cleanup only. Guard it with the existing catalog tests;
add no new test. Keep the parse as the fallback when `CachedCatalog()` returns nil
(a backend whose `cacheCatalog` parse failed), so a malformed body still surfaces its
decode error rather than a nil dereference.

```bash
go test ./internal/accounts/ -count=1
git add internal/accounts/dispatcher.go
git commit -m "refactor(catalog): read back the cached catalog instead of parsing the body twice"
```

---

## Verification gate

Before pushing:

```bash
gofmt -l . | grep -v '^gen/' ; go vet ./... && go build ./... && go test ./... -count=1
go test -race ./internal/accounts/ ./internal/api/ -count=1
```

All must be clean, and `go test ./...` must be green across every package (the PR
baseline is 25 packages, 0 failures). Then:

```bash
git push fork fix/catalog-throttle-exemption
```

## Out of scope

- Reworking `refreshLiveQuotaThrottled`'s per-account interval.
- Any change to the hot-loop throttle in `StreamGenerateContent` — it is correct and
  the PR rightly leaves it alone.
- A dedicated periodic catalog refresher goroutine. Task 1's poll-driven refresh
  covers the observed case; a timer-driven refresher is a larger design change and
  should be its own PR if wanted.
