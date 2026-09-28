# PR #98 Review Remediation: Order-Independent Discovery Tests & Machine-Local Ignore

- **Goal:** Resolve all review findings on PR #98 (`chore/omp-ignore-discovery-test`). Align credential lookup patterns across `internal/claudecode/discovery_test.go` to use slice indexing (avoiding taking the address of range loop variables and eliminating legacy pre-Go-1.22 workarounds), and verify repo-wide `.gitignore` hygiene.
- **Architecture:** Local unit test refactor in `internal/claudecode/discovery_test.go` and clean repository hygiene in `.gitignore`.
- **Tech Stack:** Go 1.27rc2 standard library (`testing`), git CLI.
- **Spec:** PR review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/98#issuecomment-5825830298.
- **Test runners:** `go test ./internal/claudecode/...` and full project `go test ./...`.

## Global Constraints

- Work on branch `chore/omp-ignore-discovery-test`. Remote: `fork`.
- Do NOT touch TLS internals or transport code (AGENTS.md).
- Staged Go files must be gofmt-clean: `gofmt -l internal/claudecode` prints nothing.
- Avoid taking address of range loop variables (`&a`) or creating redundant variable copies (`accCopy := a`). Use direct slice indexing `&accounts[i]` in `for i := range accounts`.
- Tests must pass deterministically under repeated executions (`-count=100`) even when ambient environment variables (`ANTHROPIC_API_KEY`, `CLAUDE_CODE_TOKEN`, `ANTHROPIC_AUTH_TOKEN`) are set in the runner environment.

## Findings → Tasks

| # | Finding | Severity | Task |
|---|---|---|---|
| 1 | `internal/claudecode/discovery_test.go:L149`: Taking address of range loop variable (`&a`). While safe in Go ≥1.22 due to per-iteration scoping, indexing slice directly (`&accounts[i]`) avoids heap escape and loop-variable pointer anti-patterns. | 🟡 | 1 |
| 2 | `internal/claudecode/discovery_test.go:L98`: Sibling test `TestDiscoverLocalCredentials_OAuthWithRefreshToken` retains redundant pre-Go-1.22 `accCopy := a` workaround. Align both tests to the same slice-index lookup pattern. | 🔵 | 1 |
| 3 | `.gitignore:L20`: Root-anchored `/.omp/` pattern is clean; ensure local `.git/info/exclude` duplicates are cleaned up. | 🔵 | 2 |

---

### Task 1: Unify Credential Lookups in `internal/claudecode/discovery_test.go` Using Slice Indexing

**Files:**
- Modify: `internal/claudecode/discovery_test.go:97-105`
- Modify: `internal/claudecode/discovery_test.go:146-155`
- Test: `internal/claudecode/discovery_test.go`

**Interfaces:**
- Consumes: `DiscoverLocalCredentials(customHome string) ([]AccountConfig, error)` (`internal/claudecode/discovery.go`)
- Produces: Deterministic, order-independent test assertions without taking addresses of loop variable copies.

- [ ] **Step 1: Verify order-dependence reproduction on unpatched baseline**

Run with ambient environment variable against the original implementation:
```bash
ANTHROPIC_API_KEY=sk-ant-api-demo go test ./internal/claudecode/ -run TestDiscoverLocalCredentials_MillisecondTimestamp -count=10
```
Expected: fails with `expected non-nil ExpiresAt` when `accounts[0]` is picked from map iteration order.

- [ ] **Step 2: Update `TestDiscoverLocalCredentials_OAuthWithRefreshToken` to use slice indexing**

In `internal/claudecode/discovery_test.go`, replace lines 97–104:
```go
	var oauthAcc *AccountConfig
	for _, a := range accounts {
		if a.Token == "sk-ant-oat-token-abc" {
			accCopy := a
			oauthAcc = &accCopy
			break
		}
	}
```
with:
```go
	var oauthAcc *AccountConfig
	for i := range accounts {
		if accounts[i].Token == "sk-ant-oat-token-abc" {
			oauthAcc = &accounts[i]
			break
		}
	}
```

- [ ] **Step 3: Update `TestDiscoverLocalCredentials_MillisecondTimestamp` to use slice indexing**

In `internal/claudecode/discovery_test.go`, replace lines 146–152:
```go
	var acc *AccountConfig
	for _, a := range accounts {
		if a.Token == "sk-ant-oat-token-milli" {
			acc = &a
			break
		}
	}
```
with:
```go
	var acc *AccountConfig
	for i := range accounts {
		if accounts[i].Token == "sk-ant-oat-token-milli" {
			acc = &accounts[i]
			break
		}
	}
```

- [ ] **Step 4: Run tests under stress with all discovery env vars set**

Run:
```bash
ANTHROPIC_API_KEY=sk-ant-api-demo CLAUDE_CODE_TOKEN=sk-ant-demo ANTHROPIC_AUTH_TOKEN=sk-ant-auth go test ./internal/claudecode/ -run 'TestDiscoverLocalCredentials.*' -count=100
```
Expected: `PASS` across 100 iterations.

- [ ] **Step 5: Check formatting and static analysis**

Run:
```bash
gofmt -l internal/claudecode
go vet ./internal/claudecode/
```
Expected: clean output with no diffs and no vet warnings.

- [ ] **Step 6: Commit test alignment**

```bash
git add internal/claudecode/discovery_test.go
git commit -m "test(claudecode): index accounts directly in discovery tests"
```

---

### Task 2: Verify `.gitignore` Entry and Deduplicate Local Git Exclude

**Files:**
- Modify / Verify: `.gitignore`
- Local cleanup: `.git/info/exclude`

**Interfaces:**
- Consumes: git ignore rules
- Produces: Clean git status with machine-local `.omp/` ignored via tracked `.gitignore`.

- [ ] **Step 1: Verify `.gitignore` formatting**

Inspect the last lines of `.gitignore`:
```gitignore
# Machine-local OMP config (absolute tool paths).
/.omp/
```
Ensure proper trailing newline exists and format matches existing repo rules (`/bin/`, `/proxy`, `/graft/`).

- [ ] **Step 2: Clean up redundant entry in local `.git/info/exclude`**

Remove `/.omp/` from `.git/info/exclude` (if present) to prevent duplicate local ignore specifications.

Verify git ignore attribution:
```bash
git check-ignore -v .omp/lsp.json
```
Expected: `.gitignore:20:/.omp/	.omp/lsp.json`

---

### Task 3: Full Suite Verification & Push (Execution Gate)

- [ ] **Step 1: Run full test suite across entire repository**

Run:
```bash
go test ./...
```
Expected: all packages pass, 0 failures.

- [ ] **Step 2: Run repo-wide vet and format checks**

Run:
```bash
go vet ./internal/... ./cmd/...
gofmt -l internal/claudecode .gitignore
```
Expected: clean output, no errors.

- [ ] **Step 3: Push updates to remote PR branch**

Run:
```bash
git push fork chore/omp-ignore-discovery-test
```

- [ ] **Step 4: Post completion comment on PR**

Reply to the PR review comment with commit SHAs, verification results, and passing test confirmation.
