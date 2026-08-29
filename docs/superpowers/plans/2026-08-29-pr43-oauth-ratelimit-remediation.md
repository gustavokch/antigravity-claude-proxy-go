# PR #43 Code Review Remediation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remediate findings from PR #43 code review: fix config preservation in `syncRefreshedAccountToConfig`, fix 401 retry error handling and body leaks, add `RetryAfter` evaluation to `IsRateLimited`, support millisecond Unix timestamps in CLI discovery, run immediate initial tick on background worker startup, and aggregate refresh errors.

**Architecture:**
- `internal/api/claudecode_proxy.go`: Update `syncRefreshedAccountToConfig` to retain `Allowlist` and `Routing` from existing `ClaudeCode` config. Guard against closed response body in 401 retry loop.
- `internal/claudecode/ratelimit.go`: Update `RateLimits.IsRateLimited(now)` to check `rl.RetryAfter > 0` against `rl.LastUpdated`.
- `internal/claudecode/discovery.go`: Update `extractTokensFromJSON` to detect millisecond epoch timestamps (`> 1e11`) and use `time.UnixMilli`.
- `internal/api/server.go`: Update `StartClaudeCodeBackgroundWorker` to run an initial `server.tickClaudeCodeBackgroundWorker()` immediately upon start.
- `internal/claudecode/pool.go`: Return joined error from `RefreshAllExpiringTokens` when individual account refreshes fail.
- `internal/api/claudecode_management.go`: Add empty ID validation in `handleClaudeCodeAccountRateLimits` and `handleClaudeCodeAccountRefresh`.

**Tech Stack:** Go 1.24+, `net/http`, `sync`, `time`, `encoding/json`, `errors`.

---

### Task 1: Preserve Allowlist and Routing in `syncRefreshedAccountToConfig`

**Files:**
- Modify: `internal/api/claudecode_proxy.go`
- Test: `internal/api/claudecode_proxy_test.go`

**Step 1: Write failing test**
Add test in `internal/api/claudecode_proxy_test.go` verifying that `syncRefreshedAccountToConfig` preserves `Allowlist` and `Routing` in `config.Get()`.

**Step 2: Run test to confirm failure**
`go test -v ./internal/api -run "TestSyncRefreshedAccountToConfig_PreservesAllowlistAndRouting"`

**Step 3: Implementation**
In `internal/api/claudecode_proxy.go`, populate `allowlist` and `routing` in `ccCfg` map before calling `config.Save`.

**Step 4: Run test to confirm pass**
`go test -v ./internal/api -run "TestSyncRefreshedAccountToConfig_PreservesAllowlistAndRouting"`

**Step 5: Git commit**
`git add internal/api/claudecode_proxy.go internal/api/claudecode_proxy_test.go`
`git commit -m "fix(api): preserve allowlist and routing when saving refreshed account config"`

---

### Task 2: Fix 401 Retry Response Handling and Body Leaks

**Files:**
- Modify: `internal/api/claudecode_proxy.go`
- Test: `internal/api/claudecode_proxy_test.go`

**Step 1: Write failing test**
Add test in `internal/api/claudecode_proxy_test.go` checking behavior when 401 retry `SendMessage` returns a network error or non-200 status.

**Step 2: Run test to confirm failure**
`go test -v ./internal/api -run "TestForwardToClaudeCode_401RetryFailure"`

**Step 3: Implementation**
In `internal/api/claudecode_proxy.go`, only replace and close old response when retry succeeds, and properly clean up/failover when retry returns an error.

**Step 4: Run test to confirm pass**
`go test -v ./internal/api -run "TestForwardToClaudeCode_401RetryFailure"`

**Step 5: Git commit**
`git add internal/api/claudecode_proxy.go internal/api/claudecode_proxy_test.go`
`git commit -m "fix(api): handle 401 retry errors cleanly without leaking closed response body"`

---

### Task 3: Support RetryAfter in RateLimits.IsRateLimited

**Files:**
- Modify: `internal/claudecode/ratelimit.go`
- Test: `internal/claudecode/ratelimit_test.go`

**Step 1: Write failing test**
Add test in `internal/claudecode/ratelimit_test.go` where `RequestsReset` is zero but `RetryAfter` is 60s and `LastUpdated` is recent.

**Step 2: Run test to confirm failure**
`go test -v ./internal/claudecode -run "TestRateLimits_IsRateLimited_RetryAfter"`

**Step 3: Implementation**
In `internal/claudecode/ratelimit.go`, check `if rl.RetryAfter > 0 && !rl.LastUpdated.IsZero() && rl.LastUpdated.Add(time.Duration(rl.RetryAfter)*time.Second).After(now) { return true }`.

**Step 4: Run test to confirm pass**
`go test -v ./internal/claudecode -run "TestRateLimits_IsRateLimited_RetryAfter"`

**Step 5: Git commit**
`git add internal/claudecode/ratelimit.go internal/claudecode/ratelimit_test.go`
`git commit -m "fix(claudecode): honor RetryAfter header in RateLimits.IsRateLimited"`

---

### Task 4: Support Millisecond Timestamps in Discovery

**Files:**
- Modify: `internal/claudecode/discovery.go`
- Test: `internal/claudecode/discovery_test.go`

**Step 1: Write failing test**
Add test in `internal/claudecode/discovery_test.go` with Unix millisecond `expiresAt: 1756500000000`.

**Step 2: Run test to confirm failure**
`go test -v ./internal/claudecode -run "TestDiscoverLocalCredentials_MillisecondTimestamp"`

**Step 3: Implementation**
In `internal/claudecode/discovery.go`, check if `expNum > 1e11` (or `> 1e10`) and call `time.UnixMilli(int64(expNum))`.

**Step 4: Run test to confirm pass**
`go test -v ./internal/claudecode -run "TestDiscoverLocalCredentials_MillisecondTimestamp"`

**Step 5: Git commit**
`git add internal/claudecode/discovery.go internal/claudecode/discovery_test.go`
`git commit -m "fix(claudecode): parse millisecond epoch timestamps in credential discovery"`

---

### Task 5: Immediate Initial Tick in Background Worker

**Files:**
- Modify: `internal/api/server.go`
- Test: `internal/api/server_test.go`

**Step 1: Write failing test**
Add test in `internal/api/server_test.go` verifying that `StartClaudeCodeBackgroundWorker` triggers an initial check without waiting 5 minutes.

**Step 2: Run test to confirm failure**
`go test -v ./internal/api -run "TestServer_ClaudeCodeBackgroundWorker_InitialTick"`

**Step 3: Implementation**
In `internal/api/server.go`, run `server.tickClaudeCodeBackgroundWorker()` before entering the `select` ticker loop.

**Step 4: Run test to confirm pass**
`go test -v ./internal/api -run "TestServer_ClaudeCodeBackgroundWorker_InitialTick"`

**Step 5: Git commit**
`git add internal/api/server.go internal/api/server_test.go`
`git commit -m "fix(api): run immediate initial tick on background token refresh worker startup"`

---

### Task 6: Aggregate Bulk Refresh Errors & Validate Empty IDs in Management

**Files:**
- Modify: `internal/claudecode/pool.go`
- Modify: `internal/api/claudecode_management.go`
- Test: `internal/claudecode/pool_test.go`
- Test: `internal/api/claudecode_management_test.go`

**Step 1: Write failing tests**
Add test in `internal/claudecode/pool_test.go` checking joined error return when individual account refresh fails.
Add test in `internal/api/claudecode_management_test.go` for empty account ID.

**Step 2: Run tests to confirm failure**
`go test -v ./internal/claudecode -run "TestAccountPool_RefreshAllExpiringTokens_Errors"`
`go test -v ./internal/api -run "TestClaudeCodeManagement_EmptyAccountID"`

**Step 3: Implementation**
In `internal/claudecode/pool.go`, aggregate non-nil errors using `errors.Join(errs...)`.
In `internal/api/claudecode_management.go`, return 400 when `accountID == ""`.

**Step 4: Run tests to confirm pass**
`go test -v ./internal/claudecode -run "TestAccountPool_RefreshAllExpiringTokens_Errors"`
`go test -v ./internal/api -run "TestClaudeCodeManagement_EmptyAccountID"`

**Step 5: Git commit**
`git add internal/claudecode/pool.go internal/claudecode/pool_test.go internal/api/claudecode_management.go internal/api/claudecode_management_test.go`
`git commit -m "fix(claudecode): aggregate bulk refresh errors and validate empty account IDs"`

---

### Task 7: Full Verification & Build Gate

**Files:**
- Test: `internal/...`
- Build: `cmd/proxy`

**Step 1: Run full test suite**
`go test -v ./internal/...`

**Step 2: Verify binary compilation**
`go build -o /dev/null ./cmd/proxy`
