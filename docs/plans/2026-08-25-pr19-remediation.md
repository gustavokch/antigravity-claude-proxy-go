# PR #19 Remediation Plan — OpenRouter Routing Fix

**Goal:** Fix deadline enforcement, stream failure tracking, router concurrency, and shutdown hygiene.
**Reference:** PR #19 diff at `gustavokch/antigravity-claude-proxy-go`.
**Tech:** Go 1.23, `internal/api/server.go`, `internal/openrouter/*`, `cmd/proxy/main.go`.

---

## Task 1: Per-Attempt Deadline Enforcement (Bug — server.go L643)
**Target:** `internal/api/server.go`
**Modify:** `forwardToOpenRouter`
- Derive `attemptCtx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))` before `httpClient.Do(upReq)`; pass `attemptCtx` to `NewRequestWithContext`.
- Ensure `cancel()` deferred per loop iteration.
**Test:** `internal/api/openrouter_routing_test.go` — add `TestRouting_DeadlineCutoff` that sets `RequestBudgetMs=1`, expects `StatusBadGateway` when upstream is slow.
**Steps:**
1. Write failing test.
2. Run `go test ./internal/api/ -run TestRouting_DeadlineCutoff -v`.
3. Apply `context.WithTimeout` fix.
4. Confirm pass.
5. Commit: `git commit -m "fix(api): enforce per-attempt routing deadline"`

---

## Task 2: Stream Failure Tracking (Risk — server.go stream path)
**Target:** `internal/openrouter/observability.go`
**Modify:** `SSEInterceptor` — add `terminalErr error` field; set in `Read` when `err != nil` (not `io.EOF`); expose `TerminalErr() error`; modify `proxyStreamResponse` callback to pass `interceptor.TerminalErr()` and only call `RecordResult(..., true, ...)` when `err == nil || err == io.EOF`.
**Modify:** `internal/api/server.go` `proxyStreamResponse`
**Test:** `internal/openrouter/observability_test.go` — add `TestInterceptor_TerminalErrorPropagated`.
**Steps:**
1. Test that `finalize` receives `io.EOF` vs network error.
2. Modify `SSEInterceptor` to track terminal error.
3. Update stream callback in server.
4. Confirm pass.
5. Commit: `git commit -m "fix(openrouter): track stream terminal errors for metrics"`

---

## Task 3: Router Lock Safety (Risk — router.go concurrency)
**Target:** `internal/openrouter/router.go`
**Modify:** Verify `refreshRanksLocked` deletes sticky under `r.mu.Lock()` (already holds); add comment. Check `SelectChain` reads `r.ranks` and `r.sticky` under same lock (holds `Lock`). No code change needed if verified safe; add defensive assertion in test.
**Test:** `internal/openrouter/router_test.go` — add `TestRouter_ConcurrentSelectNoRace` using `go test -race -count=10`.
**Steps:**
1. Add race test.
2. Confirm `-race` passes.
3. Commit: `git commit -m "test(openrouter): add concurrent select race guard"`

---

## Task 4: Finalize Lock Race (Risk — observability finalize)
**Target:** `internal/openrouter/observability.go`
**Modify:** `finalize` — wrap `s.buf` read in `s.mu.Lock()` / `defer s.mu.Unlock()` before accessing buffer.
**Test:** `internal/openrouter/observability_test.go` — add concurrent `Read` + `Close` race test.
**Steps:**
1. Add race test.
2. Apply `mu` lock in finalize.
3. Confirm `-race` passes.
4. Commit: `git commit -m "fix(openrouter): guard finalize with mutex"`

---

## Task 5: Shutdown Flush Hygiene (Nit — cmd/proxy/main.go)
**Target:** `cmd/proxy/main.go`
**Modify:** Wrap `FlushSave()` in `defer` inside shutdown handler so it executes even if `Shutdown` errors.
**Test:** Manual verification via `go build` and `go vet`.
**Steps:**
1. Add `defer openrouter.DefaultRouter.FlushSave()` before shutdown.
2. Build + vet pass.
3. Commit: `git commit -m "fix(proxy): defer FlushSave on shutdown"`

---

## Verification Gate
- `go test ./... -race -count=5` must pass 100% before any push.
- `go vet ./...` clean.
- Each fix committed atomically.
