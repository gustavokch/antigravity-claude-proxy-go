# PR #59 Remediation Plan

## Goal
Remediate code review findings on PR #59:
1. Fix duration suffix trimming bug in `parseRateLimitTimestamp` when handling millisecond values (`"ms"` before `"s"`).
2. Ensure `RateLimiter.Reset()` resets `minInterval` to 0.
3. Support gateway-wide request pacing in `RateLimiter.Wait()` across different models.
4. Support retry wrap-around on stream header cutoff errors in `forwardToOpenRouter`.

## Architecture & Tech Stack
- Go 1.24+ standard library (`testing`, `time`, `strings`, `sync`, `net/http`).
- Existing packages: `internal/openrouter`, `internal/api`.

---

## Tasks

### Task 1: Fix `parseRateLimitTimestamp` millisecond suffix parsing
- **Target Files:**
  - `internal/openrouter/ratelimit.go`
  - `internal/openrouter/ratelimit_test.go`
- **Step 1:** Write failing test in `ratelimit_test.go` checking `"500ms"` and `"12.5s"` reset headers.
- **Step 2:** Run test to confirm failure: `go test -v ./internal/openrouter -run TestExtractRateLimits`
- **Step 3:** Fix `parseRateLimitTimestamp` by trimming `"ms"` before `"s"`.
- **Step 4:** Run test to confirm pass.
- **Step 5:** Git commit.

### Task 2: Reset `minInterval` in `RateLimiter.Reset()`
- **Target Files:**
  - `internal/openrouter/ratelimit.go`
  - `internal/openrouter/ratelimit_test.go`
- **Step 1:** Write test in `ratelimit_test.go` asserting `Reset()` resets `minInterval` to 0.
- **Step 2:** Run test to confirm failure.
- **Step 3:** Add `l.minInterval = 0` inside `RateLimiter.Reset()`.
- **Step 4:** Run test to confirm pass.
- **Step 5:** Git commit.

### Task 3: Support gateway-wide request pacing in `RateLimiter.Wait()`
- **Target Files:**
  - `internal/openrouter/ratelimit.go`
  - `internal/openrouter/ratelimit_test.go`
- **Step 1:** Write test in `ratelimit_test.go` verifying sequential requests to two different models are paced when `minInterval` is configured.
- **Step 2:** Run test to confirm failure.
- **Step 3:** In `Wait()`, check and update both `lastRequestAt[model]` and global `lastRequestAt["*"]`.
- **Step 4:** Run test to confirm pass.
- **Step 5:** Git commit.

### Task 4: Add wrap-around retry logic on stream header cutoff
- **Target Files:**
  - `internal/api/server.go`
  - `internal/api/openrouter_routing_test.go`
- **Step 1:** Add wrap-around retry condition when stream header cutoff context error occurs and `attempts < maxRetries`.
- **Step 2:** Run `go test -v ./internal/api -run TestOpenRouterRouting` to verify pass.
- **Step 3:** Git commit.
