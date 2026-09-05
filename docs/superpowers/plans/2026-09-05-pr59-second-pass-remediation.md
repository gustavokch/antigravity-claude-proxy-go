# PR #59 Second-Pass Remediation Plan

## Goal
Remediate second-pass review findings on PR #59:
1. `parseRateLimitTimestamp` treats epoch-seconds resets as relative seconds (dead epoch branch).
2. Absent remaining-header + present limit-header reads as exhausted quota; `Wait` stalls.
3. 429-exhausted wrap-around skips cooldown — immediate re-hammer.
4. Dead exported `IsRetryableOpenRouterError`.

## Architecture & Tech Stack
- Go 1.24+ standard library (`testing`, `time`, `strconv`, `strings`).
- Packages: `internal/openrouter`, `internal/api`.

Findings posted at https://github.com/gustavokch/antigravity-claude-proxy-go/pull/59#issuecomment-5555497803.

Out of scope (flagged, needs owner sign-off): cycle-based wrap-around semantics.

---

## Tasks

### Task 1: Fix epoch + compound duration parsing in `parseRateLimitTimestamp`
- **Target Files:**
  - `internal/openrouter/ratelimit.go`
  - `internal/openrouter/ratelimit_test.go`
- **Step 1:** Failing tests: epoch seconds (`1757083200` → `time.Unix` exact), compound duration (`6m12s`).
- **Step 2:** Confirm failure: `go test ./internal/openrouter -run TestExtractRateLimits -v`
- **Step 3:** Reorder parsing: epoch int → `time.ParseDuration` → bare-seconds float; delete dead branch.
- **Step 4:** Confirm pass.
- **Step 5:** Commit `fix(openrouter): parse epoch and compound rate-limit reset values`.

### Task 2: Absent remaining header means full quota
- **Target Files:**
  - `internal/openrouter/ratelimit.go`
  - `internal/openrouter/ratelimit_test.go`
- **Step 1:** Failing test: limit present + remaining absent → `RequestsRemaining == limit`, not rate-limited.
- **Step 2:** Confirm failure.
- **Step 3:** In `ExtractRateLimits`, set remaining = limit when remaining header absent.
- **Step 4:** Confirm pass.
- **Step 5:** Commit `fix(openrouter): treat absent ratelimit remaining header as full quota`.

### Task 3: Record cooldown on 429-exhausted wrap-around
- **Target Files:**
  - `internal/api/server.go`
  - `internal/api/openrouter_routing_test.go`
- **Step 1:** Failing test: always-429 mock, `Retry429Max=1`, `maxRetries=3`, 1 candidate → wrap occurs (4 attempts, final 429).
- **Step 2:** Confirm failure (3 attempts without wrap cooldown change? — verify actual count red vs green).
- **Step 3:** In the `consec429 > max429` wrap branch, call `RecordRateLimit(model, rl, d)` with `computeBackoff(attempts, ...)`.
- **Step 4:** Confirm pass.
- **Step 5:** Commit `fix(openrouter): record cooldown on 429 wrap-around`.

### Task 4: Remove dead `IsRetryableOpenRouterError`
- **Target Files:**
  - `internal/openrouter/harness.go`
- **Step 1:** `go build ./... && go vet ./...` after deletion.
- **Step 2:** Full suite green.
- **Step 3:** Commit `chore(openrouter): remove unused IsRetryableOpenRouterError`.

---

## Verification
- `go test ./...` 100% green.
- Push to `fix/openrouter-retry-ratelimit`.
