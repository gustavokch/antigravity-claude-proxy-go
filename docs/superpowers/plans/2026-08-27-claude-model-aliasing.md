# Plan: Model Discovery and Aliasing for Claude Code Gateway

## Context & Problem Statement
When using Claude Code or other API clients against `antigravity-claude-proxy-go`, users switch models (e.g. via `/model claude-sonnet-5`, `/model gemini-3.7-flash-high`, `/model sonnet-3-7`). Currently, this encounters several limitations:
1. **Missing Model Discovery in `/v1/models`**: The `/v1/models` endpoint in `internal/api/server.go` returns models for Cloud Code (agy), OpenRouter, Kimi, and Custom Endpoints, but completely omits Claude Code (`cfg.ClaudeCode.Allowlist`) models and their aliases. Clients querying `/v1/models` cannot discover available Claude Code models.
2. **Missing Aliases in Antigravity Route (`modelcatalog`)**: When `cfg.ClaudeCode.Enabled` is `false` (default when no direct Claude OAuth/API account is active) or when routing to agy backend, requests for standard Claude models (like `claude-sonnet-5`, `claude-opus-5`, `claude-3-7-sonnet`, `claude-3-5-sonnet`) fail with `400: model "claude-sonnet-5" is not in agy's selectable agent model list` because `routingAliases` in `internal/modelcatalog/catalog.go` only maps `claude-sonnet-4-6-thinking` and `claude-opus-4-6`.
3. **Model Aliasing Unification**: Model aliasing is fragmented across `cfg.ModelMapping`, `cfg.ClaudeCode.Allowlist[].Alias`, and `modelcatalog.routingAliases`. The gateway needs a clear, unified resolution flow and model catalog exposure.

---

## Proposed Architecture & Changes

### 1. Model Discovery (`/v1/models`) in `internal/api/server.go`
Update `server.models` handler to include Claude Code models when `cfg.ClaudeCode` is configured or enabled:
- Iterate over `cfg.ClaudeCode.Allowlist` (or `claudecode.DefaultAllowlist()` if empty):
  - For each enabled entry, append canonical model object with:
    - `id`: entry.ID
    - `object`: "model"
    - `created`: timestamp
    - `owned_by`: "anthropic"
    - `display_name`: human-friendly display name
    - `supports_thinking`: entry.Thinking
    - `aliases`: list of configured aliases (e.g. `[entry.Alias]`)
  - If `entry.Alias != ""`, also append an alias model entry or list it in aliases metadata so clients querying either name see valid models.
- Ensure deduplication across all providers so overlapping model IDs do not create malformed lists.

### 2. Modern Model Aliases in `internal/modelcatalog/catalog.go`
Update `routingAliases` in `internal/modelcatalog/catalog.go` to support standard Claude Code and Claude 5/3.7/3.5 model names mapping to agy's corresponding Cloud Code display models:
- `"claude-sonnet-5"` -> `"Claude Sonnet 4.6 (Thinking)"` (or best available Sonnet in agy)
- `"claude-opus-5"` -> `"Claude Opus 4.6 (Thinking)"` (or best available Opus in agy)
- `"sonnet-5"`, `"opus-5"`, `"sonnet-3-7"` -> corresponding display models.
- When `catalog.ResolveModel(requested)` fails, improve `SelectionError.Error()` to output a list of available selectable models to help users immediately diagnose valid model options.

### 3. Claude Code Allowlist & Router Updates in `internal/claudecode/`
- In `internal/claudecode/router.go`, ensure `DefaultAllowlist()` has complete definitions for:
  - `claude-sonnet-5` (alias: `sonnet-5`)
  - `claude-opus-5` (alias: `opus-5`)
  - `claude-fable-5` (alias: `fable-5`)
  - `claude-haiku-4-5-20251001` (alias: `haiku-4-5`)
- In `internal/api/claudecode_proxy.go`, ensure `matchClaudeCodeModel` handles both exact ID matches, alias matches, and prefix-stripped variants.

---

## Files to Modify
1. `internal/api/server.go`:
   - Extend `server.models` to include Claude Code allowlist models and aliases.
   - Update `groupQuotaWindows` / model list formatting if necessary.
2. `internal/modelcatalog/catalog.go`:
   - Expand `routingAliases` for Claude models and aliases.
   - Enhance `SelectionError` message to provide helpful suggestions of available models.
3. `internal/claudecode/router.go`:
   - Expand `DefaultAllowlist()` aliases and definitions.
4. `internal/api/claudecode_proxy.go`:
   - Ensure alias matching and allowlist checking are robust.
5. Tests:
   - `internal/api/server_test.go`: Add test verifying `/v1/models` includes Claude Code models, aliases, and proper metadata.
   - `internal/modelcatalog/catalog_test.go`: Add tests verifying new aliases resolve to valid models in catalog.
   - `internal/api/claudecode_proxy_test.go`: Add tests for alias routing to Claude Code.

---

## Verification Plan
1. **Unit Tests**:
   - `go test -v ./internal/modelcatalog/... -run 'TestCatalog|TestResolveModel|TestAliases'`
   - `go test -v ./internal/claudecode/... -run 'TestRouter|TestAllowlist'`
   - `go test -v ./internal/api/... -run 'TestModels|TestClaudeCode'`
2. **Full Test Suite**:
   - `go test ./...`
3. **End-to-End Gateway Verification**:
   - Verify `GET /v1/models` returns all models across Antigravity, Claude Code, OpenRouter, Kimi, and Custom Endpoints with alias metadata.
   - Verify `POST /v1/messages` with `model: "claude-sonnet-5"` succeeds on both Claude Code route (when enabled) and Antigravity fallback route (when Claude Code is disabled).
