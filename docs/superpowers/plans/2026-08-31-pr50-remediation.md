# PR #50 Review Remediation Plan

## Goal

Remediate review findings from the PR #50 audit (comment:
https://github.com/gustavokch/antigravity-claude-proxy-go/pull/50#issuecomment-5473892173).
No 🔴 bug or 🟡 risk was found; all findings are 🔵 test-hardening nits. This
plan locks the two behaviors the review flagged as unverified.

## Architecture

PR #50 adds (a) context-aware error classification in `internal/api`
(`Canceled` → 499, `DeadlineExceeded` → 504, Warn-level logging) and (b) a
singleflight, disconnect-surviving catalog fetch in `internal/accounts`
(`startModelFetch` + background `fetchCtx` bounded by `fetchModelsTimeout`).
Remediation touches only test files; production code is unchanged.

## Tech Stack

- Go 1.x standard library, `testing` + `httptest` (project style, no mocks framework)
- Verification: `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race`

## Spec Reference

- PR: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/50
- Review comment: https://github.com/gustavokch/antigravity-claude-proxy-go#issuecomment-5473892173
- Prior plan: `docs/superpowers/plans/2026-08-31-models-context-canceled-500.md` (main checkout)

## Working Directory

`/Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/context-cancelled-fix`
(branch `worktree-context-cancelled-fix`, PR head).

---

## Task 1 — Dispatcher: fetch-error path clears the shared slot

Review finding: `internal/accounts/dispatcher_test.go` — no test proves the
`modelsFetch` slot clears when the shared fetch FAILS, so the next caller
starts a fresh fetch instead of attaching to a dead call.

Target files:
- Modify: `internal/accounts/dispatcher_test.go`
- Production code: none (behavior already implemented — this is a
  characterization test, so it is expected green on first run; deviation from
  red-green is deliberate and noted)

Consumes: `dispatcher.startModelFetch()`, `blockingModelsClient` with a
controllable release + failure switch.
Produces: regression lock on slot cleanup after `err != nil`.

Steps:

1. Write test `TestSharedModelFetchErrorFreesSlotForNextCaller`:
   - `blockingModelsClient` gains a `fail atomic.Bool`; `FetchAvailableModels`
     returns `errors.New("upstream unavailable")` when `fail` is set.
   - Start shared fetch via `startModelFetch()`, set `fail = true`, close
     `release`, wait `<-call.done` (5s deadline).
   - Assert `first.err != nil`.
   - Assert slot cleared: lock `mu`, `modelsFetch == nil`.
   - Start a second fetch via `startModelFetch()`; assert it returns a NEW
     call (`second != first`) proving a dead call is not reused.
2. Run: `go test ./internal/accounts/ -run TestSharedModelFetchErrorFreesSlotForNextCaller -v`
   (expect PASS — characterization).
3. No production edit needed.
4. Re-run same command (PASS).
5. `git add internal/accounts/dispatcher_test.go && git commit -m "test(accounts): lock modelsFetch slot cleanup after fetch error"`

## Task 2 — API: assert decoded error body in disconnect test

Review finding: `internal/api/error_classification_test.go:113` —
`TestModelsHandlerReportsClientDisconnect` decodes JSON into `document` but
asserts nothing on it.

Target files:
- Modify: `internal/api/error_classification_test.go`
- Production code: none

Consumes: existing recorder body.
Produces: assertion that `document["error"].(map[string]any)["kind"] == "api_error"`.

Steps:

1. After `json.Unmarshal`, assert:
   ```go
   errObj, _ := document["error"].(map[string]any)
   if errObj["kind"] != "api_error" {
       t.Fatalf("error.kind=%v; want api_error", errObj["kind"])
   }
   ```
2. Run: `go test ./internal/api/ -run TestModelsHandlerReportsClientDisconnect -v` (expect PASS).
3. No production edit needed.
4. Re-run (PASS).
5. `git add internal/api/error_classification_test.go && git commit -m "test(api): assert error kind in client-disconnect response"`

## Task 3 — Verification gate

1. `go build ./...`
2. `go vet ./...`
3. `go test ./...` — must be 100% green
4. `go test -race ./internal/accounts/ ./internal/api/` — clean
5. `git push origin worktree-context-cancelled-fix`
6. Report: findings resolved, test counts, PR link.

Deferred (no action, documented in review comment):
- classifyError ordering comment — no wrap path currently produces the conflict.
- "API request aborted" wording for internal deadlines — cosmetic.
- Sequential-request catalog TTL — pre-existing, out of PR #50 scope.
