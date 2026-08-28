# PR #32 Remediation: OAuth State Verification, Parser Order, Failure Logging

## Goal
Fix findings from code review of PR #32 (`fix/claudecode-oauth-token-url`):
1. State-preservation fix lacks wire-level test verification.
2. Manual code parser skips URL-query extraction when input contains `#`.
3. Loopback callback failure branches remain log-silent despite PR's stated goal.

## Architecture
- `internal/auth/claudecode_oauth.go` — `ClaudeCodeOAuthManager`: manual code parsing in `CompleteManualAuth`, loopback validation in `handleLoopbackCallback`.
- `internal/auth/claudecode_oauth_test.go` — `TestCompleteManualAuth` with mock Anthropic API server.

## Tech Stack
Go, `log/slog`, stdlib `httptest`, `go test`.

## Spec Reference
PR #32 review comment: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/32#issuecomment-5446810553

---

## Task 1: Verify `state` reaches token endpoint in manual auth tests

**Files:**
- Test: `internal/auth/claudecode_oauth_test.go` (`TestCompleteManualAuth`)

**Consumes:** existing mock token endpoint.
**Produces:** per-case assertion that POST body `state` equals expected value.

### Step 1: Write failing test
Extend `TestCompleteManualAuth` cases with `wantState string` field. In mock token handler, decode JSON body and record `state`. After each `CompleteManualAuth` call, assert recorded state equals expected:
- raw code case → `session.State` (session fallback)
- `code#state-fragment` case → `"state-fragment"`
- URL query case → `"manual-url-state"`

Current mock ignores the body — assertion will fail because no state is captured.

### Step 2: Run test to confirm failure
```bash
go test -count=1 -run TestCompleteManualAuth ./internal/auth/ -v
```

### Step 3: Minimal implementation
In mock token handler, add:
```go
var gotState string
// inside handler:
var reqBody map[string]any
_ = json.NewDecoder(r.Body).Decode(&reqBody)
gotState, _ = reqBody["state"].(string)
```
Add `wantState` to struct; set per case; assert after call. No production code change expected — implementation already sends state. If assertion passes immediately, the test is a verification addition (acceptable).

### Step 4: Run test to confirm pass
```bash
go test -count=1 -run TestCompleteManualAuth ./internal/auth/ -v
```

### Step 5: Commit
```bash
git add internal/auth/claudecode_oauth_test.go
git commit -m "test(claudecode): assert state reaches token endpoint in manual auth"
```

---

## Task 2: Fix parser order — URL-query extraction before fragment split

**Files:**
- Modify: `internal/auth/claudecode_oauth.go` (`CompleteManualAuth`, lines ~534-554)
- Test: `internal/auth/claudecode_oauth_test.go` (`TestCompleteManualAuth`)

**Consumes:** Task 1's state-assertion harness.
**Produces:** parser that handles full URL containing both query params and `#` fragment.

### Step 1: Write failing test
Add case:
```go
{
    name:       "manual url with query and fragment",
    manualCode: "https://platform.claude.com/oauth/code?code=url-frag-code&state=url-frag-state#ignored",
    wantState:  "url-frag-state",
},
```
Assert `account.Email == "manual-oauth@example.com"` (proves code extracted — mock rejects unknown codes) and state.

### Step 2: Run test to confirm failure
```bash
go test -count=1 -run TestCompleteManualAuth ./internal/auth/ -v
```
Fails: raw URL sent as `code`, mock returns 400.

### Step 3: Minimal implementation
In `CompleteManualAuth`, reorder: run the `strings.Contains(code, "code=")` URL-query extraction first, then the `#` fragment split as an independent `if` (not `else if`).

### Step 4: Run test to confirm pass
```bash
go test -count=1 -run TestCompleteManualAuth ./internal/auth/ -v
```

### Step 5: Commit
```bash
git add internal/auth/claudecode_oauth.go internal/auth/claudecode_oauth_test.go
git commit -m "fix(claudecode): extract code from URLs containing hash fragments"
```

---

## Task 3: Log loopback callback failure branches

**Files:**
- Modify: `internal/auth/claudecode_oauth.go` (`handleLoopbackCallback`, lines ~448-470)

**Consumes:** nothing. **Produces:** `slog.Warn` on missing code/state and on CSRF state mismatch.

### Step 1: Write failing test
Logging-only change; no behavioral assertion. Skip Red step (documented deviation).

### Step 3: Minimal implementation
Add `slog.Warn("Claude Code OAuth loopback callback rejected: missing code or state", "session_id", session.ID)` before the missing-param return, and `slog.Warn("Claude Code OAuth loopback callback rejected: state mismatch", "session_id", session.ID)` before the CSRF return.

### Step 4: Run full auth tests
```bash
go test -count=1 ./internal/auth/... -v
```

### Step 5: Commit
```bash
git add internal/auth/claudecode_oauth.go
git commit -m "chore(claudecode): log rejected loopback callbacks"
```

---

## Gate
`go test ./...` must be 100% green before push.
