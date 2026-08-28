# PR #33 Review Remediation — OAuth Auth Header

**Goal:** Resolve 3 review findings on PR #33 (`fix/claudecode-oauth-auth-header`).
**Tech Stack:** Go, `go test`.
**Spec:** PR review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/33#issuecomment-5447783168

## Task 1: Case-insensitive Bearer detection (🟡 risk)

- **Modify:** `internal/claudecode/client.go` (`IsOAuthToken`, `ApplyAuthHeaders` strip)
- **Test:** `internal/claudecode/client_test.go`

**Step 1 — Failing test:**
```go
{
    name:  "lowercase bearer prefix treated as OAuth",
    token: "bearer oat-token-lower",
    wantHeaders: map[string]string{
        "Authorization": "Bearer oat-token-lower",
        "anthropic-beta": OAuthBetaHeader,
    },
    missingHeaders: []string{"x-api-key"},
},
```
Plus direct `IsOAuthToken("bearer sk-ant-oat01-x") == true` case in `TestIsOAuthToken`.

**Step 2 — Run:** `go test ./internal/claudecode/ -run 'TestApplyAuthHeaders|TestIsOAuthToken' -v` → FAIL.

**Step 3 — Implement:** in `IsOAuthToken`, replace `strings.HasPrefix(trimmed, "Bearer ")` with case-insensitive check; in `ApplyAuthHeaders`, strip via `trimmed[len("bearer "):]` after lower-prefix confirmation.

**Step 4 — Run:** same command → PASS.

**Step 5 — Commit:**
```bash
git add internal/claudecode/client.go internal/claudecode/client_test.go
git commit -m "fix(claudecode): match Bearer scheme case-insensitively per RFC 7235"
```

## Task 2: Document bare `ant-oat` prefix branch (🔵 nit)

- **Modify:** `internal/claudecode/client.go` (`IsOAuthToken` comment)

**Step 1 — Implement:** add comment stating the branch defends against tokens persisted without the `sk-` prefix.

**Step 2 — Run:** `go test ./internal/claudecode/` → PASS (no behavior change).

**Step 3 — Commit:**
```bash
git add internal/claudecode/client.go
git commit -m "docs(claudecode): document unprefixed ant-oat token branch"
```

## Task 3: Beta dedup + lowercase bearer test coverage (🔵 nit)

Covered by Task 1 test additions plus dedup case:

```go
{
    name:  "oauth beta not duplicated when already present",
    token: "sk-ant-oat01-token",
    existingHeaders: map[string]string{
        "anthropic-beta": "claude-code-20250219," + OAuthBetaHeader,
    },
    wantHeaders: map[string]string{
        "Authorization":  "Bearer sk-ant-oat01-token",
        "anthropic-beta": "claude-code-20250219," + OAuthBetaHeader,
    },
    missingHeaders: []string{"x-api-key"},
},
```
(Requires extending the harness with an `existingHeaders` field.)

**Commit:**
```bash
git commit -m "test(claudecode): cover beta dedup and case-insensitive bearer"
```

## Gate

`go test ./...` 100% green, then `git push origin fix/claudecode-oauth-auth-header`.
