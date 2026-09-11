# PR #73 Review Remediation Plan

- **Goal:** Resolve the review findings on PR #73 (second-pass remediation for Gemini cache prefix stability).
- **Architecture:**
  1. Make `TestGeminiToolLoopPrefixIsStableWithClientSignatures` a real regression test by asserting the client-supplied signature survives conversion.
  2. Apply the existing `MinSignatureLength` floor to the `tool_use` signature path in `internal/format/content.go`, matching the `thinking` path.
  3. Give `scripts/verify-gemini-cache-prefix.sh` a distinct preflight exit code and a probe route the proxy actually serves.
  4. Restore `gofmt` cleanliness and tighten the prefix-growth assertion.
- **Tech Stack:** Go 1.24, Bash.
- **Spec Reference:** `docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md`.

---

## Task 1: Assert the client signature survives the tool loop

- **Target Files:**
  - Modify: `internal/format/cacheprefix_test.go`
- **Interfaces:**
  - Consumes: converted `contents` from `ConvertAnthropicToGoogle`.
  - Produces: an assertion that every `functionCall` part carries the client's `sigVal`, not `GeminiSkipSignature`.

### Step 1: Write the failing assertion

In `TestGeminiToolLoopPrefixIsStableWithClientSignatures`, after the prefix comparison, walk the converted contents and assert each part that has a `functionCall` also has `thoughtSignature == sigVal` for its turn index.

### Step 2: Run the test against a reverted `content.go`

Temporarily delete the `thought_signature` fallback and run
`go test ./internal/format -run TestGeminiToolLoopPrefixIsStableWithClientSignatures -v`.
The `snake_case` subtest must fail. Restore the fallback afterwards.

### Step 3: Minimal implementation

None. The production fallback already exists; this task only closes the test gap.

### Step 4: Run the test to confirm pass

`go test ./internal/format -run TestGeminiToolLoopPrefixIsStableWithClientSignatures -v`

### Step 5: Commit

`git commit -am "test(format): assert client tool signature survives the tool loop"`

---

## Task 2: Apply the length floor to `tool_use` signatures

- **Target Files:**
  - Modify: `internal/format/content.go`
  - Test: `internal/format/cacheprefix_test.go`
- **Interfaces:**
  - Consumes: `block["thoughtSignature"]` or `block["thought_signature"]`.
  - Produces: `part["thoughtSignature"]` holding either a signature of at least `MinSignatureLength`, or `GeminiSkipSignature`.

### Step 1: Write the failing test

Add `TestGeminiToolSignatureBelowMinLengthFallsBackToSkip`, sending a `tool_use` block whose `thought_signature` is `"short"` and asserting the converted part carries `GeminiSkipSignature`.

### Step 2: Run the test to confirm failure

`go test ./internal/format -run TestGeminiToolSignatureBelowMinLengthFallsBackToSkip -v`

### Step 3: Minimal implementation

Gate the accepted signature on `len(signature) >= MinSignatureLength` before assigning it, so anything shorter falls through to `GeminiSkipSignature`. The substitution is deterministic per history, so the cache prefix stays stable.

### Step 4: Run the test to confirm pass

`go test ./internal/format -count=1`

### Step 5: Commit

`git commit -am "fix(format): apply MinSignatureLength floor to tool_use signatures"`

---

## Task 3: Fix the verify script preflight

- **Target Files:**
  - Modify: `scripts/verify-gemini-cache-prefix.sh`
- **Interfaces:**
  - Consumes: `$PROXY_URL`.
  - Produces: exit code 3 on an unreachable proxy, distinct from exit 1 (gate failure) and exit 2 (probe verdict).

### Step 1: Update the script

Probe `$PROXY_URL/v1/models` rather than `/`, which is not a registered route. Raise the timeout to 5 seconds so a non-local `PROXY_URL` is not reported as down. Exit 3 instead of 1, and add that code to the header comment block.

### Step 2: Validate the syntax

`bash -n scripts/verify-gemini-cache-prefix.sh`

### Step 3: Commit

`git commit -am "test(scripts): separate preflight failure from the cache prefix gate"`

---

## Task 4: Formatting and growth assertion

- **Target Files:**
  - Modify: `internal/format/cacheprefix_test.go`

### Step 1: Tighten the assertion

Replace `len(next) < len(prev)` with an exact `len(next) == len(prev)+2` check, so a conversion that collapses the history is caught rather than silently passing.

### Step 2: Format

`gofmt -w internal/format/cacheprefix_test.go && gofmt -l ./internal`

### Step 3: Run the full suite

`go test ./... -count=1`

### Step 4: Commit

`git commit -am "test(format): tighten prefix growth assertion and restore gofmt"`
