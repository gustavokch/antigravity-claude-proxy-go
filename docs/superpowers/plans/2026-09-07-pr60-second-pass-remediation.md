# PR 60 Second Pass Review Remediation Plan

Remediate all findings from the PR 60 second pass code review:
1. `modelContext` population in `handleAccountLimits` for `PublicModels()` and allowlists.
2. `RestoreClaudeConfig` proxy vars list cleanup for `ANTHROPIC_SMALL_FAST_MODEL`.
3. Frontend model fields and preset relevant keys in `claude-config.js` and `dashboard.js`.
4. Comprehensive test coverage for `modelContext` advertising and `RestoreClaudeConfig`.

## Tasks

### Task 1: Populate modelContext for PublicModels and allowlists in handleAccountLimits

- **Files:**
  - Modify: `internal/api/management.go`
  - Test: `internal/api/management_test.go`
- **Step 1 (Red):** Add test assertions in `internal/api/management_test.go` checking `modelContext["gemini-3.8-flash"]` (clean public ID), allowlist models with custom context length.
- **Step 2:** Run `go test -v -run TestManagement_HealthAndLimits ./internal/api` to verify failure.
- **Step 3 (Green):** Update `internal/api/management.go` to iterate `catalog.PublicModels()` and allowlists (`ClaudeCode`, `OpenRouter`, `Kimi`), populating `modelContext` with positive token / context window limits.
- **Step 4:** Run `go test -v -run TestManagement_HealthAndLimits ./internal/api` to verify pass.
- **Step 5:** Commit fix: `git commit -m "fix(api): populate modelContext for public models and allowlists"`

### Task 2: Add ANTHROPIC_SMALL_FAST_MODEL to RestoreClaudeConfig

- **Files:**
  - Modify: `internal/config/claude.go`
  - Test: `internal/config/claude_test.go`
- **Step 1 (Red):** Add test `TestRestoreClaudeConfig_CleansSmallFastModel` in `internal/config/claude_test.go`.
- **Step 2:** Run `go test -v -run TestRestoreClaudeConfig_CleansSmallFastModel ./internal/config` to verify failure.
- **Step 3 (Green):** Add `"ANTHROPIC_SMALL_FAST_MODEL"` to `proxyVars` slice in `RestoreClaudeConfig()` in `internal/config/claude.go`.
- **Step 4:** Run `go test -v -run TestRestoreClaudeConfig_CleansSmallFastModel ./internal/config` to verify pass.
- **Step 5:** Commit fix: `git commit -m "fix(config): add ANTHROPIC_SMALL_FAST_MODEL to RestoreClaudeConfig proxyVars"`

### Task 3: Update frontend claude-config.js and dashboard.js for ANTHROPIC_SMALL_FAST_MODEL

- **Files:**
  - Modify: `internal/webui/public/js/components/claude-config.js`
  - Modify: `internal/webui/public/js/components/dashboard.js`
- **Step 1:** Add `'ANTHROPIC_SMALL_FAST_MODEL'` to `geminiModelFields` and `relevantKeys` in `claude-config.js` and `dashboard.js`.
- **Step 2:** Commit fix: `git commit -m "fix(webui): include ANTHROPIC_SMALL_FAST_MODEL in config fields and preset keys"`

### Task 4: Full project test suite verification & remote push

- **Step 1:** Run `go test ./...` across all packages.
- **Step 2:** Push to remote feature branch `feat/remove-1m-model-suffix`.
