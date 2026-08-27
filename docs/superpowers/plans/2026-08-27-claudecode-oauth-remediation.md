# Remediation Plan: Claude Code OAuth Review Fixes (PR #30)

## Goal
Remediate code review findings on PR #30:
1. Prevent Claude Code accounts from corrupting Google `accounts.json` via invalid Google OAuth registration.
2. Eliminate data races by introducing thread-safe snapshot/getter methods on auth sessions.
3. Narrow mutex locking in `CompleteManualAuth` to prevent blocking network requests while holding locks.
4. Add session TTL cleanup in `ClaudeCodeOAuthManager` to prevent unbounded memory growth.
5. Restrict token storage file permissions to `0600`.
6. Add unit tests for edge case parsing, TTL pruning, and concurrent status polling.

## Tech Stack
- Language: Go 1.22+
- Packages: `internal/auth`, `internal/claudecode`, `internal/api`

---

## Tasks

### Task 1: Remove Google AccountManager registration for Claude Code OAuth
- **Target Files:**
  - Modify: `internal/api/claudecode_oauth_handlers.go`
  - Modify: `internal/api/claudecode_oauth_handlers_test.go`
- **Details:** Remove `server.accountManager.SaveOAuthAccount` call in `registerAuthenticatedClaudeCodeAccount`.
- **TDD Steps:**
  1. Write/verify test verifying Claude Code registration does not write to Google account manager.
  2. Modify `registerAuthenticatedClaudeCodeAccount` in `internal/api/claudecode_oauth_handlers.go`.
  3. Run `go test ./internal/api/`.

### Task 2: Thread-Safe Session Access & Snapshot
- **Target Files:**
  - Modify: `internal/auth/claudecode_oauth.go`
  - Modify: `internal/api/claudecode_oauth_handlers.go`
  - Test: `internal/auth/claudecode_oauth_test.go`
- **Details:** Add `Snapshot()` method on `ClaudeCodeAuthSession` that returns safe copy under `mu.Lock()`. Update `handleClaudeCodeAuthStatusGet` to use snapshot.
- **TDD Steps:**
  1. Add test for concurrent session snapshot and status mutation.
  2. Implement `SessionSnapshot` and `Snapshot()` on `ClaudeCodeAuthSession`.
  3. Update `handleClaudeCodeAuthStatusGet` to read from snapshot.
  4. Run `go test -race ./internal/auth/ ./internal/api/`.

### Task 3: Narrow Lock Scope in CompleteManualAuth
- **Target Files:**
  - Modify: `internal/auth/claudecode_oauth.go`
  - Test: `internal/auth/claudecode_oauth_test.go`
- **Details:** Validate state and retrieve verifier under lock, release lock, execute HTTP exchange, re-acquire lock to set status/account.
- **TDD Steps:**
  1. Verify manual auth tests pass with narrowed lock.
  2. Run `go test -race ./internal/auth/`.

### Task 4: Session TTL Cleanup in ClaudeCodeOAuthManager
- **Target Files:**
  - Modify: `internal/auth/claudecode_oauth.go`
  - Test: `internal/auth/claudecode_oauth_test.go`
- **Details:** Add `PruneExpiredSessions(maxAge time.Duration)` and periodic cleanup trigger.
- **TDD Steps:**
  1. Write unit test `TestPruneExpiredSessions`.
  2. Implement `PruneExpiredSessions` in `ClaudeCodeOAuthManager`.
  3. Run `go test ./internal/auth/`.

### Task 5: File Permissions 0600 on Credential Storage
- **Target Files:**
  - Modify: `internal/claudecode/storage.go`
  - Test: `internal/claudecode/storage_test.go`
- **Details:** Change `os.WriteFile(path, data, 0644)` to `0600`.
- **TDD Steps:**
  1. Add assertion in `storage_test.go` verifying file mode `0600`.
  2. Update `storage.go`.
  3. Run `go test ./internal/claudecode/`.

### Task 6: Comprehensive Verification & Race Check
- **Target Files:**
  - Modify/Test: `internal/auth/claudecode_oauth_test.go`
- **Details:** Add test cases for URL query param extraction `#state`, full URL extraction, whitespace trimming, and run full test suite with `-race`.
