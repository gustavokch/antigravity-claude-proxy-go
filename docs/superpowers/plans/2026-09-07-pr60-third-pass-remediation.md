# PR 60 Third Pass Review Remediation Plan

Remediate all findings from the PR 60 third pass code review:
1. `NewEmpty` duplicates `New` init; `New` now accepts empty pools, so delete `NewEmpty`.
2. Dead commented-out validation in `New`.
3. `UpdateClaudeConfig` mutates caller's `updates` map and aliases it into persisted config.
4. `PublicModels()` loop lacks `m.ID != ""` guard in `handleAccountLimits`.
5. Stale comment in `TestManagement_HealthAndLimits` contradicts assertion.

## Task 1: Delete NewEmpty, route NewDefault through New

- **Files:**
  - Modify: `internal/accounts/manager.go`
  - Test: `internal/accounts/manager_test.go`
- **Consumes:** `New(Options)` accepting zero accounts (already the case).
- **Produces:** `NewDefault` with empty pool built by `New` itself.
- **Step 1 (Red):** Add test `TestNewDefault_EmptyPoolUsesNew` — with no pool file, no agy token (temp HOME/config dir), `NewDefault` returns manager with `Count() == 0` and non-nil `now`. Verify existing behavior unchanged.
- **Step 2:** Run `go test -run TestNewDefault -v ./internal/accounts` to confirm baseline.
- **Step 3 (Green):** Delete `NewEmpty` and its call site; `NewDefault` returns `New(Options{ConfigPath: configPath, Strategy: strategy, Now: now})`.
- **Step 4:** Run `go test ./internal/accounts -v` — all pass.
- **Step 5:** Commit: `git commit -m "refactor(accounts): drop redundant NewEmpty, build empty pool via New"`

## Task 2: Remove dead commented-out validation in New

- **Files:**
  - Modify: `internal/accounts/manager.go`
- **Step 1 (Green-only):** Delete commented `if len(options.Accounts) == 0` block (lines ~260-262).
- **Step 2:** Run `go build ./... && go test ./internal/accounts`.
- **Step 3:** Commit (folded into Task 1 commit or separate `chore(accounts): remove dead validation comment`).

## Task 3: Stop UpdateClaudeConfig from mutating caller's map

- **Files:**
  - Modify: `internal/config/claude.go`
  - Test: `internal/config/claude_test.go`
- **Step 1 (Red):** Add test `TestUpdateClaudeConfig_DoesNotMutateUpdatesInput` — pass updates map with `env.ANTHROPIC_MODEL = "gemini-3.8-flash-high[1m]"`; after call, assert caller's map still holds original suffixed value.
- **Step 2:** Run `go test -run TestUpdateClaudeConfig_DoesNotMutateUpdatesInput -v ./internal/config` — expect failure.
- **Step 3 (Green):** In else-branch, build sanitized copy of `vMap` before assigning to `current[k]`.
- **Step 4:** Run `go test ./internal/config -v` — all pass.
- **Step 5:** Commit: `git commit -m "fix(config): sanitize into copy, stop mutating caller's updates map"`

## Task 4: Guard empty model IDs in handleAccountLimits catalog loops

- **Files:**
  - Modify: `internal/api/management.go`
- **Step 1 (Red):** Extend test: catalog fixture entry with empty model ID key `""` must not appear in `models` or `modelContext`.
- **Step 2:** Run `go test -run TestManagement_HealthAndLimits -v ./internal/api` — expect failure.
- **Step 3 (Green):** Add `m.ID != ""` guards to `Selectable()` and `PublicModels()` loops in `handleAccountLimits`.
- **Step 4:** Run `go test ./internal/api -v` — all pass.
- **Step 5:** Commit: `git commit -m "fix(api): skip empty model IDs in account-limits catalog loops"`

## Task 5: Fix stale comment in management_test.go

- **Files:**
  - Modify: `internal/api/management_test.go`
- **Step 1 (Green-only):** Replace comment with accurate wording: fixture has maxTokens 250000 (positive), so entry must exist.
- **Step 2:** Run `go test ./internal/api` — pass.
- **Step 3:** Commit (folded into Task 4 commit or separate).

## Task 6: Full suite verification & push

- **Step 1:** `go test ./...` — 100% green gate.
- **Step 2:** `git push fork feat/remove-1m-model-suffix`.
