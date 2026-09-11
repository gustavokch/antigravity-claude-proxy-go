# PR #72 Second Pass Review Remediation Plan

- **Goal:** Remediate second-pass code review findings for PR #72 (Gemini cache prefix stability).
- **Architecture:**
  1. Support snake_case `thought_signature` fallback on `tool_use` blocks in `internal/format/content.go`.
  2. Add unit tests in `internal/format/cacheprefix_test.go` confirming prefix stability with client-provided `thoughtSignature` and `thought_signature`.
  3. Enhance `scripts/verify-gemini-cache-prefix.sh` with a preflight reachability check.
- **Tech Stack:** Go 1.24, Bash.
- **Spec Reference:** `docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md`.

---

## Task 1: Support `thought_signature` Fallback in `tool_use` Content Conversion

- **Target Files:**
  - Modify: `internal/format/content.go`
  - Test: `internal/format/cacheprefix_test.go`
- **Interfaces:**
  - Consumes: `block["thoughtSignature"]` or `block["thought_signature"]`.
  - Produces: `part["thoughtSignature"]` containing preserved signature or `GeminiSkipSignature`.

### Step 1: Write failing test
In `internal/format/cacheprefix_test.go`:
Add `TestGeminiToolSignaturePreservesSnakeCaseClientSignature` asserting that `map[string]any{"type": "tool_use", "id": "t1", "name": "read", "thought_signature": "custom-sig-12345678901234567890123456789012345678901234567890"}` preserves `"custom-sig-..."` in converted `thoughtSignature`.

### Step 2: Run test to confirm failure
`go test -v ./internal/format -run TestGeminiToolSignaturePreservesSnakeCaseClientSignature`

### Step 3: Minimal implementation
In `internal/format/content.go`:
```go
signature := stringValue(block["thoughtSignature"])
if signature == "" {
    signature = stringValue(block["thought_signature"])
}
if signature == "" {
    signature = GeminiSkipSignature
}
part["thoughtSignature"] = signature
```

### Step 4: Run test to confirm pass
`go test -v ./internal/format -run TestGeminiToolSignaturePreservesSnakeCaseClientSignature`

### Step 5: Git commit command
`git commit -am "fix(format): support snake_case thought_signature fallback in tool_use converter"`

---

## Task 2: Extend Cache Prefix Tests for Client-Supplied Signatures

- **Target Files:**
  - Modify: `internal/format/cacheprefix_test.go`
- **Interfaces:**
  - Consumes: Multi-turn message history with camelCase and snake_case thought signatures.
  - Produces: Equal prefix between turn N and turn N+1.

### Step 1: Write test
In `internal/format/cacheprefix_test.go`:
Add `TestGeminiToolLoopPrefixIsStableWithClientSignatures` verifying that when client supplies `thoughtSignature` or `thought_signature`, turn N remains an exact prefix of turn N+1.

### Step 2: Run test
`go test -v ./internal/format -run TestGeminiToolLoopPrefixIsStableWithClientSignatures`

### Step 3: Minimal implementation
Verify existing conversion preserves prefix and test passes.

### Step 4: Git commit command
`git commit -am "test(format): assert cache prefix stability with client-supplied signatures"`

---

## Task 3: Strengthen Live Cache Prefix Probe Script Preflight Check

- **Target Files:**
  - Modify: `scripts/verify-gemini-cache-prefix.sh`
- **Interfaces:**
  - Consumes: `$PROXY_URL`.
  - Produces: Preflight check before test turns.

### Step 1: Update script
In `scripts/verify-gemini-cache-prefix.sh`:
Add preflight connectivity check before executing turn 1:
```bash
if ! curl -s -m 2 "$PROXY_URL/" >/dev/null 2>&1; then
  echo "Error: proxy not reachable at $PROXY_URL. Start proxy first." >&2
  exit 1
fi
```

### Step 2: Validate script syntax
`bash -n scripts/verify-gemini-cache-prefix.sh`

### Step 3: Git commit command
`git commit -am "test(scripts): add proxy preflight check to cache prefix probe"`
