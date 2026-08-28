# PR #36 Remediation — Headroom stream leak, verbatim reads, coding thinking budgets

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/36
**Branch:** `fix/headroom-stream-leak-verbatim-thinking`
**Base commit:** `82a0464`
**Review:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/36#issuecomment-5449274040

## Goal

Close the nine findings raised in review of PR #36 without changing the shape of
the three fixes the PR already lands. Three are blocking: one transport still
leaks `headroom_retrieve`, terminal turns can end in `stop_reason: "tool_use"`
with no `tool_use` block, and an unrelated config flag silently disables the
coding-continuation classifier.

## Architecture

Three subsystems are touched, in dependency order.

1. **`internal/api` — CCR transport layer.** `ccrStreamState` is the single
   owner of upstream-to-downstream content-block mapping. Every streaming
   transport must route through it; OpenRouter currently does not. A new
   `reconcileStopReason` helper sits alongside it and repairs the terminal
   `message_delta` / response `stop_reason` when suppression removed the last
   `tool_use` block.

2. **`internal/headroom` — classification.** `ToolInspector` is the tool-name
   and verbatim-shape oracle. It must exist whenever *either* consumer needs it
   (`PreserveVerbatimReads` for the skip guards, `EffortRouting` for the
   continuation classifier), not only for the first.

3. **`internal/headroom` — heuristics.** The text predicates
   (`looksLikeTestOutput`, `inputLooksLikeFileRead`, `looksLikeNumberedSource`)
   are tuned for precision, since both over-matching directions have a real
   cost: a false positive burns compression budget, a false negative breaks an
   `Edit`.

## Tech Stack

Go 1.x, stdlib only. `go test ./... -count=1`, `go vet ./...`.

## Invariants preserved

- **I1** — SmartCrusher and CodeCompressor stay position independent.
- **I3** — walkers never visit non-`tool_result` text.
- Downstream content-block indexes stay a gapless 0-based sequence.
- `AssistantBlocks()` keeps suppressed blocks so upstream `tool_use_id`s resolve.

---

## Task 1 — Reconcile `stop_reason` after suppression (🔴)

**Files:** modify `internal/api/ccr_stream_state.go`, `internal/api/ccr_proxy.go`,
`internal/api/server.go`; test `internal/api/ccr_leak_test.go`.

**Consumes:** terminal `message_delta` event map / decoded unary response map,
plus whether any visible `tool_use` block survived the iteration.
**Produces:** `stop_reason` rewritten to `end_turn` when the suppressed retrieve
call was the only `tool_use`.

**Step 1 — failing tests.** In `ccr_leak_test.go`:
`TestCCRLeak_Stream_StopReasonReconciledAtCap` drives a stream whose only
tool_use is `headroom_retrieve` with `MaxHydrations: 0`, and asserts the emitted
`message_delta` carries `stop_reason: "end_turn"`.
`TestCCRLeak_Unary_StopReasonReconciledAtCap` does the same over
`ProxyAnthropicJSONWithCCR`. A third case asserts a surviving real `tool_use`
leaves `stop_reason: "tool_use"` untouched.

**Step 2 — confirm red.** `go test ./internal/api/ -run 'StopReason' -v`

**Step 3 — implement.** Add `ccrStreamState.HasVisibleToolUse() bool` and a
package-level `reconcileStopReason(event map[string]any, hasVisibleToolUse bool)`.
Call it on the terminal `message_delta` flush in `ProxyAnthropicStreamWithCCR`
and in `streamMessage`; extend `stripRetrieveBlocks` to repair `stop_reason` on
the unary paths.

**Step 4 — confirm green.** `go test ./internal/api/ -count=1`

**Step 5 — commit.**
`git commit -m "fix(ccr): reconcile stop_reason when suppression removes last tool_use"`

---

## Task 2 — Migrate OpenRouter streaming to `ccrStreamState` (🔴)

**Files:** modify `internal/api/server.go` (`forwardToOpenRouter`, ~L1191-L1310);
test `internal/api/openrouter_ccr_leak_test.go` (new).

**Consumes:** upstream OpenRouter SSE events.
**Produces:** downstream SSE with retrieve blocks suppressed and gapless indexes.

**Step 1 — failing test.** `TestOpenRouter_Stream_NoRetrieveLeak` stands up an
`httptest` OpenRouter upstream emitting text / `headroom_retrieve` / `Read`
blocks, forwards through `forwardToOpenRouter` with CCR enabled, and asserts the
client body contains no `headroom_retrieve` and has `content_block_start`
indexes `[0, 1, ...]`.

**Step 2 — confirm red.**
`go test ./internal/api/ -run TestOpenRouter_Stream_NoRetrieveLeak -v`

**Step 3 — implement.** Replace `currentBlocks` / `currentJSONBufs` with
`state := newCCRStreamState(baseBlockIndex)`. Route `content_block_start`,
`content_block_delta`, and `content_block_stop` through `StartBlock` / `MapIndex`
/ `AppendJSON` / `AppendText`; take `retrieveCalls` from `state.Finalize()`;
advance with `state.VisibleCount()`; replay `state.AssistantBlocks()`. Apply the
Task 1 `stop_reason` reconciliation on the terminal flush.

**Step 4 — confirm green.** `go test ./internal/api/ -count=1`

**Step 5 — commit.**
`git commit -m "fix(openrouter): suppress headroom_retrieve on the streaming path"`

---

## Task 3 — Build `ToolInspector` when effort routing needs it (🔴)

**Files:** modify `internal/headroom/engine.go`; test
`internal/headroom/continuation_test.go`.

**Consumes:** `Config.PreserveVerbatimReads`, `Config.OutputShaper.EffortRouting`.
**Produces:** `reqCtx.Verbatim` non-nil whenever either consumer needs it.

**Step 1 — failing test.** `TestEngine_EffortRoutingKeepsInspector` runs the
engine with `PreserveVerbatimReads: false`, `EffortRouting: true`, and an `Edit`
tool-result continuation; asserts `reqCtx.ContinuationKind == "coding"` and that
thinking was not clamped.

**Step 2 — confirm red.**
`go test ./internal/headroom/ -run TestEngine_EffortRoutingKeepsInspector -v`

**Step 3 — implement.** In `Engine.Process`, build the inspector when
`cfg.PreserveVerbatimReads || cfg.OutputShaper.EffortRouting`. `skipVerbatim`
already gates on `cfg.PreserveVerbatimReads`, so the skip guards stay off.

**Step 4 — confirm green.** `go test ./internal/headroom/ -count=1`

**Step 5 — commit.**
`git commit -m "fix(headroom): keep tool inspector when effort routing is on"`

---

## Task 4 — Tighten `looksLikeTestOutput` (🟡)

**Files:** modify `internal/headroom/output_shaper.go`; test
`internal/headroom/output_shaper_test.go`.

**Step 1 — failing test.** `TestLooksLikeTestOutput_ProseIsNotTestOutput` asserts
false for `"Please pass the auth token to the handler."`, `"warning: none"`, and
`"The user does not have an error: field here"`; a companion positive case keeps
real `go test`, `pytest`, and compiler output true.

**Step 2 — confirm red.**
`go test ./internal/headroom/ -run TestLooksLikeTestOutput -v`

**Step 3 — implement.** Drop `(?i)` from `PASS` / `FAIL`, anchor the diagnostic
patterns as `(?m)^\s*(error|warning):`, and delete the `---\s*FAIL` and
`ok\s+\t` duplicates (folds in the Task 8 nit).

**Step 4 — confirm green.** `go test ./internal/headroom/ -count=1`

**Step 5 — commit.**
`git commit -m "fix(headroom): stop classifying prose as test output"`

---

## Task 5 — Bound `inputLooksLikeFileRead` over-match (🟡)

**Files:** modify `internal/headroom/verbatim.go`; test
`internal/headroom/verbatim_test.go`.

**Step 1 — failing test.** `TestToolInspector_MutatingToolsAreNotVerbatim`
asserts `Edit`, `Write`, and `Glob` calls carrying `file_path` do not mark their
results verbatim, while `Read` and unknown-name path-carrying tools still do.

**Step 2 — confirm red.**
`go test ./internal/headroom/ -run TestToolInspector_MutatingToolsAreNotVerbatim -v`

**Step 3 — implement.** Add a `nonVerbatimToolNames` set (edit / write /
multiedit / glob / grep / apply_patch / str_replace and friends). Consult the
input-shape signal only when the normalized name is not in it. An explicit
`verbatimToolNames` hit still wins.

**Step 4 — confirm green.** `go test ./internal/headroom/ -count=1`

**Step 5 — commit.**
`git commit -m "fix(headroom): do not mark mutating tool results verbatim"`

---

## Task 6 — Replace the variadic optional parameter (🟡)

**Files:** modify `internal/headroom/output_shaper.go`; test
`internal/headroom/output_shaper_test.go`.

**Step 1 — failing test.** `TestClassifyContinuation_ZeroMaxBytesUsesDefault`
calls `classifyContinuation(req, insp, 0)` and asserts the 2048 default applies.
The signature change breaks compilation until Step 3 — that is the red.

**Step 2 — confirm red.** `go test ./internal/headroom/ -run TestClassifyContinuation -v`

**Step 3 — implement.** Change to
`classifyContinuation(req map[string]any, inspector *ToolInspector, mechanicalMaxBytes int)`
and update `isMechanicalContinuation` to pass `nil, 0`.

**Step 4 — confirm green.** `go test ./internal/headroom/ -count=1`

**Step 5 — commit.**
`git commit -m "refactor(headroom): take mechanicalMaxBytes as a plain parameter"`

---

## Task 7 — Overflow-safe line-number parse (🔵)

**Files:** modify `internal/headroom/verbatim.go`; test
`internal/headroom/verbatim_test.go`.

**Step 1 — failing test.** `TestLooksLikeNumberedSource_HugeCountersDoNotMatch`
feeds three lines whose leading number is 20 digits and asserts false.

**Step 2 — confirm red.**
`go test ./internal/headroom/ -run TestLooksLikeNumberedSource_HugeCounters -v`

**Step 3 — implement.** Delete the hand-rolled `atoi`; use `strconv.Atoi` and
treat a parse error as a non-match that resets the run.

**Step 4 — confirm green.** `go test ./internal/headroom/ -count=1`

**Step 5 — commit.**
`git commit -m "fix(headroom): use strconv.Atoi for line-number detection"`

---

## Task 8 — Fold into Task 4

The redundant `---\s*FAIL` and `ok\s+\t` patterns are removed as part of Task 4;
no separate commit.

---

## Task 9 — Honest telemetry label for large payloads (🔵)

**Files:** modify `internal/headroom/output_shaper.go`; test
`internal/headroom/continuation_test.go`.

**Step 1 — failing test.** `TestClassifyContinuation_LargeNonCodeIsLarge` feeds a
4 KB prose tool result and asserts `kindLarge`, and that the shaper leaves the
budget alone for it.

**Step 2 — confirm red.**
`go test ./internal/headroom/ -run TestClassifyContinuation_LargeNonCode -v`

**Step 3 — implement.** Add `kindLarge` with `String() == "large"`, return it
from the byte-ceiling branch, and include it in the no-clamp set alongside
`kindInteractive`. `ClampCodingContinuations` keeps applying to `kindCoding`
only.

**Step 4 — confirm green.** `go test ./internal/headroom/ -count=1`

**Step 5 — commit.**
`git commit -m "refactor(headroom): label large non-code continuations distinctly"`

---

## Verification gate

```
go build ./...
go vet ./...
go test ./... -count=1
git push origin fix/headroom-stream-leak-verbatim-thinking
```

All three must be clean before the push.
