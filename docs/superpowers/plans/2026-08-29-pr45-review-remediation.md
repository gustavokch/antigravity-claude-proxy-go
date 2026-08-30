# PR 45 Review Remediation Plan

## Goal
Remediate code review findings on PR #45:
1. Allow `ParseUsageFromSSELine` and `ExtractProviderFromSSELine` to handle bare JSON payloads (without `data:` prefix) and case-insensitive SSE comment lines.
2. Support parsed map objects for `metadata.user_id` in `ExtractSessionID`.
3. Add end-to-end integration tests verifying token and provider observability with CCR enabled.

## Architecture & Interfaces
- `internal/openrouter/observability.go`:
  - `ParseUsageFromSSELine(line string, inputTokens, outputTokens, cacheRead, cacheWrite *int)`: Strip `data:` if present, or parse directly if it starts with `{`.
  - `ExtractProviderFromSSELine(line string) string`: Case-insensitive prefix matching for SSE comments (`: provider:`, `: OpenRouter Processing:`, etc.), strip `data:` if present or parse directly if it starts with `{`.
- `internal/openrouter/session.go`:
  - `ExtractSessionID(req *http.Request, reqBody map[string]any) string`: Check both `string` (stringified JSON) and `map[string]any` (structured JSON) for `metadata.user_id`, `metadata.session_id`, `user_id`, `session_id`.
- `internal/api/openrouter_observability_test.go`:
  - Test streaming observability with CCR enabled (`headroom.enabled = true`, `headroom.ccr = true`).

---

## Tasks

### Task 1: Fix `ParseUsageFromSSELine` and `ExtractProviderFromSSELine` for Bare JSON and Case-Insensitive SSE Comments
- **Target files:**
  - Modify: `internal/openrouter/observability.go`
  - Test: `internal/openrouter/observability_test.go`
- **Step 1: Write failing test:**
  Add test cases in `internal/openrouter/observability_test.go` for:
  - Bare JSON payload (e.g. `{"type":"message_start","message":{"usage":{"prompt_tokens":300}}}`) passed to `ParseUsageFromSSELine`.
  - Bare JSON payload (e.g. `{"provider":"Together"}`) passed to `ExtractProviderFromSSELine`.
  - Mixed-case SSE comment lines (e.g. `: Provider: Together`, `: OpenRouter Processing: Novita`) passed to `ExtractProviderFromSSELine`.
- **Step 2: Run test to confirm failure:**
  `go test -v ./internal/openrouter -run TestParseUsageFromSSELine`
- **Step 3: Minimal implementation:**
  In `internal/openrouter/observability.go`:
  - Check if `line` starts with `data:` and strip it; else if `line` starts with `{`, use it as `payload`.
  - In `ExtractProviderFromSSELine`, for comments (`:`), lowercase the prefix for matching while preserving the extracted provider value casing.
- **Step 4: Run test to confirm pass:**
  `go test -v ./internal/openrouter`
- **Step 5: Git commit:**
  `git commit -m "fix(openrouter): support bare JSON and case-insensitive comments in SSE observability"`

---

### Task 2: Support Structured Map in `ExtractSessionID`
- **Target files:**
  - Modify: `internal/openrouter/session.go`
  - Test: `internal/openrouter/session_test.go`
- **Step 1: Write failing test:**
  Add test cases in `internal/openrouter/session_test.go` for:
  - `metadata.user_id` as `map[string]any{"session_id": "nested-session-uuid"}`
  - `metadata.user_id` as `map[string]any{"user_id": "inner-uid"}`
- **Step 2: Run test to confirm failure:**
  `go test -v ./internal/openrouter -run TestExtractSessionID`
- **Step 3: Minimal implementation:**
  In `internal/openrouter/session.go`:
  - Update `ExtractSessionID` and helper to check for both `string` and `map[string]any`.
- **Step 4: Run test to confirm pass:**
  `go test -v ./internal/openrouter`
- **Step 5: Git commit:**
  `git commit -m "fix(openrouter): support structured map in ExtractSessionID"`

---

### Task 3: Integration Test for Observability with CCR Enabled
- **Target files:**
  - Test: `internal/api/openrouter_observability_test.go`
- **Step 1: Write test:**
  Add `TestOpenRouterObservability_StreamingWithCCREnabled` in `internal/api/openrouter_observability_test.go`.
- **Step 2: Run test:**
  `go test -v ./internal/api -run TestOpenRouterObservability_StreamingWithCCREnabled`
- **Step 3: Verify pass:**
  Ensure token counts and provider are logged correctly when CCR stream processing is active.
- **Step 4: Git commit:**
  `git commit -m "test(api): add OpenRouter streaming observability test with CCR enabled"`
