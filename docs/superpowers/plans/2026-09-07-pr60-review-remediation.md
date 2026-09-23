# PR #60 Review Remediation — 1M Context Cleanup

## Goal

Fix findings from PR #60 review
(https://github.com/gustavokch/antigravity-claude-proxy-go/pull/60#issuecomment-5575955444):

1. 🔴 Dead `context_window` assertion in `TestGeminiModels_AdvertiseMaxContextWindow`
   (`json.Unmarshal` yields `float64`; `.(int)` never succeeds — test cannot fail).
2. 🟡 Unconditional "1M" badge for every gemini model in WebUI — no capability check.
3. 🔵 `isClaudeModelField` misses `ANTHROPIC_SMALL_FAST_MODEL`.
4. 🔵 `sanitizeModelValue("[1m]")` yields empty string env var.

## Architecture

- `internal/api/management.go` — account-limits handler already calls
  `server.fetchModelCatalog`; add `modelContext` map (model ID → MaxTokens) to response.
- `internal/webui/public/js/data-store.js` — store `modelContext` map.
- `internal/webui/public/views/settings.html` — gate 5 badge templates on
  `modelContext[modelId] >= 1000000`.
- `internal/api/models_discovery_test.go` — assert `float64`, require presence.
- `internal/config/claude.go` — extend field list, guard empty remainder.

## Tech Stack

Go 1.x (`go test ./...`), Alpine.js WebUI (no JS test infra — verify by grep + build).

## Tasks

### Task 1 — Fix dead context_window assertion (models_discovery_test.go)

Files: Modify `internal/api/models_discovery_test.go`.

1. Assert `float64`, fail when key absent for gemini models, fail when < 1000000.
2. `go test ./internal/api -run TestGeminiModels -v` — pass.
3. Mutation check: temporarily set one fixture `maxTokens` to 65536, expect test
   failure, restore, expect pass. Proves assertion is live.
4. Commit `test(api): make context_window assertion effective (float64, presence required)`.

### Task 2 — Gate 1M badge on real context window

Files: Modify `internal/api/management.go`, `internal/api/management_test.go`,
`internal/webui/public/js/data-store.js`, `internal/webui/public/views/settings.html`.

Consumes: `catalog.Selectable()` (`Model.MaxTokens`).
Produces: `modelContext: {<modelId>: <maxTokens>}` in `/account-limits` response;
`$store.data.modelContext`; badge rendered only when `>= 1000000`.

1. Red: extend management_test.go — new subtest asserting `modelContext` present,
   gemini catalog model maps to 1048576, and a fixture model with `maxTokens: 65536`
   is absent from `modelContext`. Run, expect failure (key missing).
2. Green: in account-limits handler, build `modelContext` from the same catalog
   fetch (`Selectable()`, raw IDs — matches `models` list entries); include only
   `MaxTokens > 0`; add to response map.
3. data-store.js: `modelContext: {}` state; set from `data.modelContext` in both
   fetch paths (line ~85 and ~143 regions).
4. settings.html ×5: badge `x-if` gains `&& ($store.data.modelContext[modelId] || 0) >= 1000000`.
5. `go test ./internal/api -run TestManagement -v` — pass.
6. Commit `fix(webui): gate 1M badge on real context window from catalog`.

### Task 3 — Config sanitize hardening (claude.go)

Files: Modify `internal/config/claude.go`, `internal/config/claude_test.go`.

1. Red: extend `TestUpdateClaudeConfig_CleansLegacy1mSuffix` — add case value
   `"[1m]"` alone must stay `"[1m]"` (no empty env var), and
   `ANTHROPIC_SMALL_FAST_MODEL: "gemini-3.8-flash[1m]"` must sanitize. Run, expect failure.
2. Green: add `ANTHROPIC_SMALL_FAST_MODEL` to `isClaudeModelField`;
   `sanitizeModelValue` returns original when remainder after strip is empty.
3. `go test ./internal/config -v` — pass.
4. Commit `fix(config): sanitize ANTHROPIC_SMALL_FAST_MODEL and guard empty [1m] value`.

### Task 4 — Verify and push

1. `go build ./...` && `go test ./...` — 100% green.
2. `git push origin feat/remove-1m-model-suffix`.
3. Report summary + PR link.

## Spec

Review comment: `docs/superpowers/plans/2026-09-07-pr60-review-remediation.md` header
links the GitHub comment; findings reproduced verbatim above.
