# PR #73 Second Pass Review Remediation Plan

- **Goal:** Fix snake_case `thought_signature` preservation across multi-block assistant content reordering and Gemini history detection.
- **Architecture:**
  1. Add `"thought_signature"` to `copyFields` in `internal/format/thinking.go:reorderAssistantContent` for `tool_use` blocks.
  2. Support `thought_signature` in `internal/format/thinking.go:hasGeminiHistory`.
  3. Expand `internal/format/cacheprefix_test.go` with unit tests for multi-block assistant turns (text + tool_use) and history detection with snake_case signatures.
- **Tech Stack:** Go 1.24.
- **Spec Reference:** `docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md`.

---

## Task 1: Add Unit Tests for Multi-Block Tool Use and History Detection with snake_case Signature

- **Target Files:**
  - Modify: `internal/format/cacheprefix_test.go`
- **Interfaces:**
  - Consumes: Multi-block assistant message (`text` + `tool_use` with `thought_signature`), message history with `thought_signature`.
  - Produces: Assertions that `thought_signature` survives reordering and `hasGeminiHistory` detects it.

### Step 1: Write failing test
In `internal/format/cacheprefix_test.go`:
1. `TestGeminiToolSignaturePreservedInMultiBlockAssistant` tests that an assistant message with `[]any{map[string]any{"type": "text", "text": "let me read"}, map[string]any{"type": "tool_use", ..., "thought_signature": signature}}` retains the signature in converted parts.
2. `TestHasGeminiHistoryWithSnakeCaseSignature` tests that `hasGeminiHistory` returns true for messages containing `tool_use` with `thought_signature`.
3. Update `TestGeminiToolLoopPrefixIsStableWithClientSignatures` to include assistant text blocks before tool_use.

### Step 2: Run test to confirm failure
`go test -v ./internal/format -run "TestGeminiToolSignaturePreservedInMultiBlockAssistant|TestHasGeminiHistoryWithSnakeCaseSignature"`

### Step 3: Minimal implementation
Implement Task 2 changes.

### Step 4: Run test to confirm pass
`go test -v ./internal/format -run "TestGeminiToolSignaturePreservedInMultiBlockAssistant|TestHasGeminiHistoryWithSnakeCaseSignature"`

### Step 5: Git commit command
`git commit -am "test(format): assert snake_case signature in multi-block assistant turns and history detection"`

---

## Task 2: Preserve `thought_signature` in `reorderAssistantContent` and `hasGeminiHistory`

- **Target Files:**
  - Modify: `internal/format/thinking.go`
- **Interfaces:**
  - Consumes: `block["thought_signature"]` on `tool_use`.
  - Produces: Preserved `thought_signature` in `reorderAssistantContent`, `hasGeminiHistory` returning true.

### Step 1: Implementation
In `internal/format/thinking.go`:
1. In `hasGeminiHistory`, check `_, exists := block["thought_signature"]` in addition to `block["thoughtSignature"]`.
2. In `reorderAssistantContent`, add `"thought_signature"` to `copyFields(block, "type", "id", "name", "input", "thoughtSignature", "thought_signature")`.

### Step 2: Verify tests pass
`go test ./... -count=1`

### Step 3: Git commit command
`git commit -am "fix(format): preserve snake_case thought_signature in reordering and history detection"`
