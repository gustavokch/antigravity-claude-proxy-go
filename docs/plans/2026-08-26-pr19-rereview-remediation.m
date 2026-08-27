# PR #19 Re-Review Remediation Plan

**Goal:** Resolve the 6 residual findings from the re-review of PR #19 at `fa464c3`.
**Reference:** Review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/19#issuecomment-5421207251
**Tech:** Go 1.23 (`internal/api/server.go`, `internal/openrouter/*`), Alpine.js WebUI.
**Worktree:** `/tmp/pr19-review` on branch `feat/openrouter-provider-routing`.

---

## Task 1: Stream-Path Cutoff Timer Race (Risk — server.go:822)
**Target:** `internal/api/server.go`
**Modify:** `forwardToOpenRouter` stream success branch
- After `headersCutoff.Stop()`, check `attemptCtx.Err() != nil`. If cancelled (timer fired between header arrival and `Stop()`), close the body, record a provider failure, advance `providerIdx`, continue the failover loop instead of handing a dead context to `proxyStreamResponse`.
**Test note:** The race window (cancel lands after transport receives headers, before `Stop()`) is not deterministically reproducible via black-box httptest — no dedicated test is sound here. Guarded by existing suite (`BudgetExemptsActiveStream`, `DeadlineEnforcedOnSlowUpstream`) staying green.
**Steps:**
1. Apply guard.
2. `go test ./internal/api/ -run TestOpenRouterRouting -v` — all green.
3. Commit: `fix(api): fail over when cutoff fires at header arrival`

---

## Task 2: Vacuous Health Pass for Unknown Providers (Risk — router.go:465)
**Target:** `internal/openrouter/router.go`
**Modify:** `providerHealthyUnderThresholdLocked`
- When ranks exist for the model and the provider name is not found in them, return false (unknown names must not pass).
**Test:** `internal/openrouter/router_test.go` — `TestProviderRouter_UnknownStickyProviderDropped`: seed sticky `"ghost"`, rank `[p1]`, assert `SelectChain` auto returns `["p1"]` only and sticky is dropped.
**Steps:**
1. Write failing test; confirm red.
2. Add membership requirement.
3. Confirm green.
4. Commit: `fix(openrouter): reject providers absent from ranks`

---

## Task 3: Leader Fetch Timeout Plumbing (Nit — endpoints.go:290)
**Target:** `internal/openrouter/endpoints.go`
**Modify:** `ResolveModelEndpoints` leader path
- Replace hardcoded `15*time.Second` fetch ctx with the client's configured `httpClient.Timeout`.
**Test note:** Behavior-equivalent today (client timeout already bounds the fetch); existing `TestResolveModelEndpoints_WaiterHonorsContext` covers the waiter side. Refactor only.
**Commit:** `refactor(openrouter): use configured timeout for endpoint fetch`

---

## Task 4: Follower Goroutine Leak (Nit — endpoints.go:304)
**Target:** `internal/openrouter/endpoints.go`
**Modify:** `endpointsCall` gains a `done chan struct{}` closed by the leader; followers select on `flight.done` vs `ctx.Done()` directly — no per-follower goroutine.
**Test:** existing `TestResolveModelEndpoints_WaiterHonorsContext` + `-race` suite.
**Commit:** `refactor(openrouter): broadcast endpoint flight completion via channel`

## Task 5: Silent Persistence Failures (Nit — router.go:716)
**Target:** `internal/openrouter/router.go`
**Modify:** add `logger *slog.Logger` field (default `slog.Default()`); log `SaveTo` errors in the debounce callback and `FlushSave`.
**Test:** `internal/openrouter/router_test.go` — `TestProviderRouter_SaveFailureLogged`: persist to a path that cannot be written (existing directory as file target), assert log records the failure.
**Steps:**
1. Failing test (no log emitted today).
2. Implement logging.
3. Green.
4. Commit: `fix(openrouter): log router state persistence failures`

---

## Task 6: formatUptime Zero Rendering (Nit — models.js:401)
**Target:** `internal/webui/public/js/components/models.js`
**Modify:** `formatUptime` — treat any numeric `entry.uptime` (including 0) as authoritative; keep endpoint-field fallback only when uptime is absent.
**Verification:** eyeball diff; no JS test harness in repo.
**Commit:** `fix(webui): render 0% uptime distinctly from unknown`

---

## Verification Gate
- `go build ./...`, `go vet ./...` clean.
- `go test ./...` green; `-race -count=2` green on openrouter/api/config.
- Push to fork `feat/openrouter-provider-routing`.
