# Remediation Plan: PR #35 Code Review Findings

**Goal**: Fix all review findings on PR #35 (memory bounded reader, router endpoint alignment, empty accounts serialization, token trimming, and comprehensive unit tests).
**Tech Stack**: Go 1.22+, Vanilla JS / Alpine.js WebUI.
**Spec Reference**: PR #35 Review findings.

---

### Task 1: Bound `FetchModels` response reader and trim auth tokens
- **Target Files**:
  - Modify: `internal/claudecode/client.go`
  - Test: `internal/claudecode/client_test.go`
- **Interfaces**:
  - `FetchModels(ctx context.Context, token string, baseURL string) ([]ModelInfo, error)`
- **Step 1: Write failing test**
  Add unit tests in `internal/claudecode/client_test.go` for:
  - Untrimmed token strings (`"  Bearer ey...  "`).
  - Truncated/bounded response body reading.
- **Step 2: Run test to confirm failure**
  `go test -count=1 ./internal/claudecode -run TestFetchModels`
- **Step 3: Minimal implementation**
  In `internal/claudecode/client.go`:
  - `token = strings.TrimSpace(token)`
  - Use `io.ReadAll(io.LimitReader(resp.Body, 1<<20))` for 1MB read limit.
- **Step 4: Run test to confirm pass**
  `go test -count=1 ./internal/claudecode`
- **Step 5: Git commit**
  `git commit -m "fix(claudecode): bound response reader and trim auth tokens in FetchModels"`

---

### Task 2: Fix router support for query and path params on Claude Code accounts DELETE & empty array JSON
- **Target Files**:
  - Modify: `internal/api/claudecode_management.go`
  - Modify: `internal/webui/public/js/components/account-manager.js`
  - Test: `internal/api/claudecode_management_test.go`
- **Interfaces**:
  - `DELETE /api/claudecode/accounts?id={id}`
  - `DELETE /api/claudecode/accounts/{id}`
  - `GET /api/claudecode/accounts`
- **Step 1: Write failing test**
  Add unit tests in `internal/api/claudecode_management_test.go` for:
  - `DELETE /api/claudecode/accounts?id=test-acc`
  - `DELETE /api/claudecode/accounts/test-acc`
  - `GET /api/claudecode/accounts` returning `[]` instead of `null` when no accounts configured.
- **Step 2: Run test to confirm failure**
  `go test -count=1 ./internal/api -run TestClaudeCodeAccounts`
- **Step 3: Minimal implementation**
  In `internal/api/claudecode_management.go`:
  - Handle `path == "/api/claudecode/accounts" && method == http.MethodDelete` using query parameter `id`.
  - Handle `strings.HasPrefix(path, "/api/claudecode/accounts/") && method == http.MethodDelete` using path segment `id`.
  - In `handleClaudeCodeAccountsList`, initialize `accounts := make([]ClaudeCodeAccountResponse, 0)` so it marshals to `[]`.
  In `internal/webui/public/js/components/account-manager.js`:
  - Use `/api/claudecode/accounts/${encodeURIComponent(accountId)}` for delete request.
- **Step 4: Run test to confirm pass**
  `go test -count=1 ./internal/api`
- **Step 5: Git commit**
  `git commit -m "fix(api): support query and path params on accounts DELETE and empty array serialization"`
