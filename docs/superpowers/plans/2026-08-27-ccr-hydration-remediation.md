# Remediation Plan: CCR Hydration Fixes for PR #31

**Goal:** Fix stream index monotonicity, assistant turn block ordering, stop reason metadata preservation, and stream error handling during CCR hydration loop.
**Architecture:** CCR proxy wrapper (`internal/api/ccr_proxy.go`) intercepting upstream `headroom_retrieve` tool calls.
**Tech Stack:** Go 1.22+, standard `net/http`, Server-Sent Events (SSE).
**Spec Reference:** PR #31 Review Remediation.

---

### Task 1: Fix Stream Client Block Index Scoping & Add Leading Text Monotonicity Test

- **Target Files:**
  - `internal/api/ccr_proxy.go` (Modify)
  - `internal/api/ccr_proxy_test.go` (Modify)
- **Problem:** `clientBlockIndex` was re-initialized to 0 inside the iteration loop, causing stream content block indices to reset on subsequent hydration turns.
- **Step 1 (Test):** Add `TestCCRProxyStream_MonotonicIndicesWithLeadingText` in `ccr_proxy_test.go` where iteration 0 streams text + CCR tool_use, and iteration 1 streams final text. Verify client receives continuous indices (index 0 for first text, index 1 for second text).
- **Step 2 (Verify Red):** Run `go test -v ./internal/api -run TestCCRProxyStream_MonotonicIndicesWithLeadingText` to verify failure.
- **Step 3 (Implement):** Move `clientBlockIndex` definition outside the hydration `for iter := 0; ...` loop in `ProxyAnthropicStreamWithCCR`. Use explicit `upstreamToClient` index mapping per iteration.
- **Step 4 (Verify Green):** Run `go test -v ./internal/api -run TestCCRProxyStream_MonotonicIndicesWithLeadingText`.
- **Step 5 (Commit):** `git commit -m "fix(ccr): preserve client block index monotonicity across stream hydration iterations"`

---

### Task 2: Deterministic Ordering of Reconstructed Assistant Blocks

- **Target Files:**
  - `internal/api/ccr_proxy.go` (Modify)
  - `internal/api/ccr_proxy_test.go` (Modify)
- **Problem:** Rebuilding assistant message blocks by iterating `currentBlocks map[int]map[string]any` produces randomized block order.
- **Step 1 (Test):** Add test in `ccr_proxy_test.go` verifying assistant turn sent in subsequent request preserves exact order of upstream blocks (e.g. index 0 text, index 1 tool_use).
- **Step 2 (Verify Red):** Verify test behavior with out-of-order check.
- **Step 3 (Implement):** Sort keys of `currentBlocks` before appending to `validBlocks` in `ProxyAnthropicStreamWithCCR`.
- **Step 4 (Verify Green):** Run `go test -v ./internal/api -run TestCCRProxyStream`.
- **Step 5 (Commit):** `git commit -m "fix(ccr): ensure deterministic ordering of reconstructed assistant blocks"`

---

### Task 3: Preserve Upstream Message Delta Metadata on Final Stream Iteration

- **Target Files:**
  - `internal/api/ccr_proxy.go` (Modify)
  - `internal/api/ccr_proxy_test.go` (Modify)
- **Problem:** Final `message_delta` hardcoded `stop_reason: "end_turn"` or lost upstream `stop_sequence` and `stop_reason: "tool_use"`.
- **Step 1 (Test):** Add test `TestCCRProxyStream_PreservesNonEndTurnStopReason` where final iteration has `stop_reason: "tool_use"` or custom `stop_sequence`.
- **Step 2 (Verify Red):** Run `go test -v ./internal/api -run TestCCRProxyStream_PreservesNonEndTurnStopReason`.
- **Step 3 (Implement):** Store final iteration's `delta` map from `message_delta` and merge with cumulative usage when emitting final `message_delta`. Default `stop_reason` to `"end_turn"` only if empty.
- **Step 4 (Verify Green):** Run `go test -v ./internal/api -run TestCCRProxyStream_PreservesNonEndTurnStopReason`.
- **Step 5 (Commit):** `git commit -m "fix(ccr): preserve upstream stop reason and delta metadata on stream completion"`

---

### Task 4: SSE Error Event on Mid-Stream Upstream Failure

- **Target Files:**
  - `internal/api/ccr_proxy.go` (Modify)
  - `internal/api/ccr_proxy_test.go` (Modify)
- **Problem:** If upstream fails on `iteration > 0`, connection dropped or wrote raw error text instead of structured SSE `error` event.
- **Step 1 (Test):** Add test `TestCCRProxyStream_MidStreamUpstreamError` where iteration 0 succeeds with CCR retrieval, and iteration 1 returns HTTP 500. Verify client receives SSE `error` event.
- **Step 2 (Verify Red):** Run `go test -v ./internal/api -run TestCCRProxyStream_MidStreamUpstreamError`.
- **Step 3 (Implement):** In `ProxyAnthropicStreamWithCCR`, if `iteration > 0` and `opts.ExecuteUpstream` or `resp.StatusCode != 200` fails, write SSE `event: error` and flush before returning.
- **Step 4 (Verify Green):** Run `go test -v ./internal/api -run TestCCRProxyStream_MidStreamUpstreamError`.
- **Step 5 (Commit):** `git commit -m "fix(ccr): emit SSE error event on mid-stream upstream failure"`

---

### Task 5: Verification & PR Push

- **Target Files:** Entire repository
- **Step 1:** Run full test suite: `go test -v ./...`
- **Step 2:** Push branch to remote: `git push origin worktree-feat-ccr-hydration`
