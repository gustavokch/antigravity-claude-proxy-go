# PR #98 Review Remediation (Refined) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve the PR #98 review. Both lookup-by-token tests in `internal/claudecode/discovery_test.go` switch to the same slice-index lookup (`for i := range accounts` / `&accounts[i]`). Then push the branch, update the PR body, and reply to the review.

**Architecture:** One test-only refactor that keeps behaviour the same, in a single file. `internal/claudecode/discovery.go` is not touched. The `.gitignore` finding approves the change and asks for nothing, so no file changes for it. The last task handles delivery: the full gate, a fast-forward push, the PR body and the reply.

**Tech Stack:** Go 1.27rc2 (`go.mod:3`), `testing` from the standard library, git, `gh` CLI.

**Spec:** the review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/98#issuecomment-5825830298. The PR #98 body is also an input, because Task 2 rewrites it.

**Supersedes:** `docs/superpowers/plans/2026-09-24-pr98-review-remediation.md` (draft). See "Review of the draft" at the end.

## Global Constraints

- AGENTS.md: **do NOT touch TLS internals.** No task touches `internal/cloudcode`, the transport, or `tls.Config`.
- Branch `chore/omp-ignore-discovery-test`, remote `fork` (the only remote). Local `HEAD` and `fork/chore/omp-ignore-discovery-test` are both at `f7f7f5a`. Push fast-forward only and never force-push.
- The working tree has unrelated untracked files (`.gemini/`, `pr.sh`, `tools/quotaprobe/`, other plan files, …). Stage explicit paths only. Never run `git add -A` or `git add .`.
- `gofmt -l internal/claudecode` must print nothing. The opt-in hook (`core.hooksPath=scripts/git-hooks`) runs gofmt on staged Go files. Do **not** use `gofmt -l .` as a gate: `gen/exa/codeium_common_pb/codeium_common.pb.go` already has known generator drift, and the hook exempts `gen/`. Do **not** run gofmt on `.gitignore`: gofmt only parses Go and exits 2 on it.
- The discovery tests must pass with `-count=100` while `ANTHROPIC_API_KEY`, `CLAUDE_CODE_TOKEN` and `ANTHROPIC_AUTH_TOKEN` are set. `DiscoverLocalCredentials` builds its result by ranging over a map (`discovery.go:61-64`), so tests must never depend on slice order.
- Scope is limited to the two lookup loops the review names:
  - Do not extract a helper or switch to `slices.IndexFunc`. The review asks for `for i := range accounts` with `&accounts[i]`.
  - Do not add `t.Setenv` isolation.
  - Do not change the by-value scan in `TestDiscoverLocalCredentials` (`discovery_test.go:47-60`). It never takes an address and the review does not mention it.
- Run commands from the repo root. If the default Go build cache is not writable, prefix `go` with `GOCACHE=$PWD/.gocache`.

## Finding → Task map

| # | Location | Severity | Finding | Task |
|---|---|---|---|---|
| 1 | `internal/claudecode/discovery_test.go:149` | 🟡 risk | `acc = &a` takes the address of the range variable, so the variable escapes to the heap. Use `for i := range accounts` with `acc = &accounts[i]`. | 1 |
| 2 | `internal/claudecode/discovery_test.go:98` | 🔵 nit | `TestDiscoverLocalCredentials_OAuthWithRefreshToken` still has the `accCopy := a` workaround from before Go 1.22. Make both tests use the same lookup. | 1 |
| 3 | `.gitignore:20` | 🔵 nit | "Root-anchored `/.omp/` pattern is clean and correctly prevents committing local tool configs". This approves the change and requests nothing. | 2 (verify + acknowledge; no edit) |

---

### Task 1: Use one slice-index lookup in both discovery tests (Findings 1, 2)

**Files:**
- Modify: `internal/claudecode/discovery_test.go:97-104` (`TestDiscoverLocalCredentials_OAuthWithRefreshToken`)
- Modify: `internal/claudecode/discovery_test.go:146-152` (`TestDiscoverLocalCredentials_MillisecondTimestamp`)
- Test: the same file. No new tests.

**Interfaces:**
- Consumes: `func DiscoverLocalCredentials(customHome string) ([]AccountConfig, error)` (`internal/claudecode/discovery.go:16`), and `AccountConfig.Token string` (`internal/claudecode/types.go:19`).
- Produces: no new symbols. Task 2 uses the commit this task creates.

**Why there is no "failing test first" step:** this refactor keeps behaviour the same. The order-dependence bug is already fixed on the branch (`f7f7f5a`), so any test run against `HEAD` passes before this change too. Verification is bracketed instead: the tests pass before and after the edit, and escape analysis shows that the reviewer's concern (loop variables moved to the heap) is present before the edit and gone after it.

- [ ] **Step 1: Confirm the tests pass before the edit, with the env vars set**

Run:
```bash
ANTHROPIC_API_KEY=sk-ant-api-demo CLAUDE_CODE_TOKEN=sk-ant-demo ANTHROPIC_AUTH_TOKEN=sk-ant-auth \
  go test ./internal/claudecode/ -run '^TestDiscoverLocalCredentials' -count=100
```
Expected: `ok  	antigravity-go-proxy/internal/claudecode`.

- [ ] **Step 2: Record the escape-analysis baseline**

Run:
```bash
go test -gcflags=-m -c -o /dev/null ./internal/claudecode/ 2>&1 | grep 'discovery_test.go.*moved to heap'
```
Expected, exactly these two lines:
```
internal/claudecode/discovery_test.go:100:4: moved to heap: accCopy
internal/claudecode/discovery_test.go:147:9: moved to heap: a
```

- [ ] **Step 3: Replace the lookup in `TestDiscoverLocalCredentials_OAuthWithRefreshToken` (L97-104)**

Replace:
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

- [ ] **Step 4: Replace the lookup in `TestDiscoverLocalCredentials_MillisecondTimestamp` (L146-152)**

Replace:
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

Leave the rest of both functions unchanged. That includes the `oauthAcc == nil` / `acc == nil` `t.Fatalf` guards.

- [ ] **Step 5: Confirm the escape is gone**

Run:
```bash
go test -gcflags=-m -c -o /dev/null ./internal/claudecode/ 2>&1 | grep 'discovery_test.go.*moved to heap'
```
Expected: no output (`grep` exits 1).

- [ ] **Step 6: Confirm no old patterns remain**

Run:
```bash
grep -nE 'accCopy|= &a$' internal/claudecode/discovery_test.go
```
Expected: no output.

- [ ] **Step 7: Confirm the tests pass after the edit, with and without the env vars**

Run:
```bash
ANTHROPIC_API_KEY=sk-ant-api-demo CLAUDE_CODE_TOKEN=sk-ant-demo ANTHROPIC_AUTH_TOKEN=sk-ant-auth \
  go test ./internal/claudecode/ -run '^TestDiscoverLocalCredentials' -count=100
env -u ANTHROPIC_API_KEY -u CLAUDE_CODE_TOKEN -u ANTHROPIC_AUTH_TOKEN \
  go test ./internal/claudecode/ -count=1
```
Expected: both print `ok  	antigravity-go-proxy/internal/claudecode`.

- [ ] **Step 8: Format and vet**

Run:
```bash
gofmt -l internal/claudecode
go vet ./internal/claudecode/
```
Expected: no output from either command.

- [ ] **Step 9: Commit**

```bash
git add internal/claudecode/discovery_test.go
git commit -m "test(claudecode): index accounts directly in discovery tests"
```
Expected: the gofmt hook passes and `git show --stat HEAD` lists only `internal/claudecode/discovery_test.go`, with 6 insertions and 7 deletions.

---

### Task 2: Full gate, push, PR body, review reply (Finding 3 + delivery)

**Files:**
- Verify only: `.gitignore:19-20` (no edit)
- PR #98 body (`gh pr edit`)
- PR #98 conversation (`gh pr comment`)

**Interfaces:**
- Consumes: the Task 1 commit on `chore/omp-ignore-discovery-test`.
- Produces: `fork/chore/omp-ignore-discovery-test` updated, the PR body updated, and a reply to the review.

**Finding 3 needs no edit.** The reviewer approved `/.omp/` and asked for no change. `.git/info/exclude` also contains `/.omp/`. That file is machine-local and untracked, so it cannot be part of the PR. The tracked rule already takes precedence (`git check-ignore -v` reports `.gitignore:20`). Leave `.git/info/exclude` alone.

- [ ] **Step 1: Run the full test suite**

Run:
```bash
go test ./...
```
Expected: every package prints `ok` or `[no test files]`, with no `FAIL`. This took about 50 s on the reference machine.

- [ ] **Step 2: Vet the whole module**

Run:
```bash
go vet ./...
```
Expected: no output.

- [ ] **Step 3: Verify the `.gitignore` rule (Finding 3)**

Run:
```bash
git check-ignore -v .omp/lsp.json
git diff f7f7f5a -- .gitignore
```
Expected: the first command prints `.gitignore:20:/.omp/	.omp/lsp.json`. The second prints nothing, meaning `.gitignore` is unchanged since the PR commit.

- [ ] **Step 4: Push (fast-forward)**

Run:
```bash
git log --oneline main..HEAD
git push fork chore/omp-ignore-discovery-test
```
Expected: the log shows two commits, the Task 1 commit on top of `f7f7f5a chore: ignore machine-local .omp/; find discovery test account by token`. The push reports `f7f7f5a..<new-sha>  chore/omp-ignore-discovery-test -> chore/omp-ignore-discovery-test`. If the push is rejected as non-fast-forward, stop and report. Do not force-push.

- [ ] **Step 5: Update the PR body**

The current body says the fix is written "without the loop-variable copy (Go ≥1.22 gives each iteration its own variable)". It also points to `discovery_test.go:98-104` as the pattern to copy, and its Changes list leaves out the OAuth test. All three are wrong after Task 1. Replace the body:

```bash
cat > /tmp/pr98-body.md <<'EOF'
Split out of #97, where the two-axis review flagged these as out of scope.

## Changes
- `.gitignore`: ignore `/.omp/` (machine-local OMP config with absolute tool paths).
- `internal/claudecode/discovery_test.go`: `TestDiscoverLocalCredentials_MillisecondTimestamp` finds its account by token instead of taking `accounts[0]`. It and `TestDiscoverLocalCredentials_OAuthWithRefreshToken` now use the same lookup, `for i := range accounts` with `&accounts[i]`, which drops the OAuth test's `accCopy := a` copy.

## Why the test fix exists
`DiscoverLocalCredentials` collects accounts into the `foundTokens` map and builds the returned slice by ranging over it, and it also imports tokens from `ANTHROPIC_API_KEY` / `CLAUDE_CODE_TOKEN` / `ANTHROPIC_AUTH_TOKEN`. When any of those variables is set, `accounts[0]` is whichever entry map iteration yields first, so the test is order-dependent:

- `main`, `ANTHROPIC_API_KEY=sk-ant-api-demo go test ./internal/claudecode/ -run TestDiscoverLocalCredentials_MillisecondTimestamp -count=100`: 9 fail / 91 pass, failing with `discovery_test.go:148: expected non-nil ExpiresAt`.
- This branch, same command with `-count=20`: all pass.
- Without those env vars, both versions pass.

The lookup indexes the slice instead of taking the address of the range variable, so neither test moves a loop variable to the heap: `go test -gcflags=-m` reported `moved to heap: accCopy` and `moved to heap: a` in `discovery_test.go` before, and reports nothing there now.

## Verification
- `gofmt -l internal/claudecode` clean, `go vet ./...` clean, `go test ./...` ok.
- `ANTHROPIC_API_KEY=sk-ant-api-demo CLAUDE_CODE_TOKEN=sk-ant-demo ANTHROPIC_AUTH_TOKEN=sk-ant-auth go test ./internal/claudecode/ -run '^TestDiscoverLocalCredentials' -count=100`: ok.
- `git check-ignore -v .omp/lsp.json` → `.gitignore:20:/.omp/`.
EOF
gh pr edit 98 --body-file /tmp/pr98-body.md
gh pr view 98 --json body --jq .body | grep -c 'accounts\[i\]'
```
Expected: `gh pr edit` prints the PR URL, and the final `grep -c` prints `1` (the Changes bullet). If any Step 1–3 result differed from the Verification section above, correct the section before running `gh pr edit`.

- [ ] **Step 6: Reply to the review**

Run:
```bash
sha=$(git rev-parse --short HEAD)
printf '%s\n' "Addressed review https://github.com/gustavokch/antigravity-claude-proxy-go/pull/98#issuecomment-5825830298 in ${sha}:
- L149 🟡 / L98 🔵: both lookup-by-token tests use \`for i := range accounts\` + \`&accounts[i]\`; \`accCopy := a\` removed. \`go test -gcflags=-m\` no longer reports \`moved to heap\` in \`discovery_test.go\`.
- \`.gitignore\` L20 🔵: acknowledged, no change needed.
Gate: \`go test ./...\` ok, \`go vet ./...\` clean, discovery tests \`-count=100\` ok with all three token env vars set." \
  | gh pr comment 98 --body-file -
```
Expected: `gh` prints the comment URL, and the comment shows the new short SHA with the backticks rendered as code spans.

- [ ] **Step 7: Remove the temp file**

Run:
```bash
rm -f /tmp/pr98-body.md
```

---

## Review of the draft (`2026-09-24-pr98-review-remediation.md`)

Checked against the review comment, the PR body, the branch, and scratch-worktree runs on 2026-09-25.

1. **Draft Task 1 Step 1 expected the wrong result.** It said the test would fail on "the unpatched baseline", but it runs on `HEAD`, which already has the order fix (`f7f7f5a`). Measured: `HEAD` passes `-count=100` with all three env vars set. `main`'s version of the test fails 5 of 20 runs with `ANTHROPIC_API_KEY` set. Replaced with a passing-before bracket plus an escape-analysis baseline.
2. **Draft Task 2 invented a finding.** The review's `.gitignore:L20` item approves the change. Editing `.git/info/exclude` changes the user's machine-local state, which the PR cannot contain. The tracked rule already wins attribution (`git check-ignore -v` → `.gitignore:20`). The edit is dropped and the verification kept (Task 2 Step 3).
3. **Draft Task 3 Step 2 fails as written.** `gofmt -l internal/claudecode .gitignore` exits 2 (`.gitignore:1:1: expected 'package', found '/'`). Removed. The draft's narrowed `go vet ./internal/... ./cmd/...` is replaced by `go vet ./...`, which is clean.
4. **Draft Task 3 Step 4 was a placeholder** ("Reply … with commit SHAs"). Replaced with a concrete `gh pr comment` command, with the quoting checked locally.
5. **The draft missed the PR body.** Its rationale ("without the loop-variable copy …") and its `discovery_test.go:98-104` reference go stale after the change, and its Changes list leaves out the OAuth test. Added Task 2 Step 5.
6. **Smaller fixes:**
   - Added the required agentic-worker header.
   - Corrected the line ranges (`97-105` → `97-104`, `146-155` → `146-152`).
   - Anchored `-run '^TestDiscoverLocalCredentials'`.
   - Added the explicit-staging and no-force-push constraints.
   - Turned the draft's "avoids heap escape" claim into a checked step.

**Scratch-worktree verification of this plan's Task 1 edit** (worktree removed afterwards):
- `go test -gcflags=-m` reports no `moved to heap` in `discovery_test.go`.
- The env-var stress run passes 100 of 100.
- `gofmt -l internal/claudecode` is clean and `go vet ./internal/claudecode/` is clean.
- Diff: 6 insertions, 7 deletions.

`go test ./...` passes at `HEAD` (51 s), and `go vet ./...` is clean.
