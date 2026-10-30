# PR #142 Remediation — agy generation body: conversation ids, requestId time, output guard

**Goal:** Fix the review findings on PR #142 (`feat/agy-generation-body`): the conversation ids stay fixed when the first prompt changes from blocks to a string, the requestId time is the send time, the output guard keeps the full usage and counts tool-call output, and the conversation-start calls go out only with a labelled agent call.

**Architecture:** `internal/format/agyrequest.go` derives agy's conversation (`deriveConversation`, `firstUserText`, `conversation.requestID`) and the start calls (`Builder.ConversationStart`). `internal/format/outputguard.go` (`OutputGuard.Observe`) enforces the client's `max_tokens` on the stream. `internal/accounts/dispatcher.go` (`StreamGenerateContent`) builds the payload once per account, then sleeps (throttle delay, capacity backoff, short 429 retry) before each send.

**Tech stack:** Go 1.27, `go test`.

**Spec reference:** PR #142 review comment (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/142#issuecomment-6524802030); `.reference/agy-1.4.11-generation-20261030.txt` sections 1 (requestId time 1-160 ms before send), 3 (labels), 9 (ACL `trajectoryId` = first call's `labels.trajectory_id`).

## Findings and their tasks

| # | Finding | Task |
|---|---------|------|
| 1 | 🔴 string vs one-text-block first prompt gives two cascades | Task 1 |
| 2 | 🟡 requestId time fixed at build, stale after throttle/retry sleeps | Task 2 |
| 3 | 🟡 synthetic MAX_TOKENS usage loses prompt and thought counts | Task 3 |
| 4 | 🟡 the guard cancels the upstream stream; agy reads to the end | No change: decision for the owner (cancel vs. drain in the background) |
| 5 | 🔵 functionCall args not in the guard estimate | Task 4 |
| 6 | 🔵 start calls sent for an agent call without labels | Task 5 |

## Task 1 — One conversation for a string or a one-block first prompt

**Files:** Modify `internal/format/agyrequest.go` (`firstUserText`); Test `internal/format/format_test.go`.

**Consumes:** `buildAgyCall`. **Produces:** no API change.

- Step 1 (red): add `TestBuilderKeepsTheConversationWhenTheFirstPromptBecomesAString`: turn 1 sends `[{"type":"text","text":"start","cache_control":{...}}]`; turn 2 sends the same prompt as the string `"start"`, then an assistant reply and a new user message. Assert equal `cascade` and `trajectory`.
- Step 2: `go test -count=1 -run TestBuilderKeepsTheConversationWhenTheFirstPromptBecomesAString ./internal/format` → FAIL (different cascades).
- Step 3: in `firstUserText`, return `"text\x00" + text + "\x00"` for string content: the form a single text block has.
- Step 4: re-run Step 2 and `go test -count=1 -run 'TestBuilder|TestConversationStart' ./internal/format` → PASS.
- Step 5: `git commit -m "fix(format): keep agy's conversation ids when the first prompt arrives as a string"`.

## Task 2 — Stamp the requestId at send time

**Files:** Modify `internal/format/builder.go`, `internal/format/agyrequest.go` (add `Builder.Stamp`), `internal/accounts/dispatcher.go`; Test `internal/accounts/agy_conversation_test.go`.

**Consumes:** `Builder.Now`, `conversation.requestID`. **Produces:** `func (builder *Builder) Stamp(request CloudCodeRequest) CloudCodeRequest`.

- Step 1 (red): `TestDispatcherStampsTheRequestIDWhenItSends`: a dispatcher with throttling on (`RequestDelay` 200 ms), a fake `Sleep` that moves a shared clock forward, and a builder on that clock. Assert that the sent requestId's ms field is the clock after the delay, not the build time.
- Step 2: `go test -count=1 -run TestDispatcherStampsTheRequestIDWhenItSends ./internal/accounts` → FAIL (build time).
- Step 3: add `Builder.Stamp`, which rebuilds `RequestID` from the request's conversation with `builder.now()` (a request with no conversation stays as is). The dispatcher sends `dispatcher.builder.Stamp(payload)` on each attempt.
- Step 4: re-run Step 2, plus `go test -count=1 ./internal/accounts ./internal/format` → PASS.
- Step 5: `git commit -m "fix(accounts): stamp agy's requestId when the request is sent"`.

## Task 3 — The MAX_TOKENS finish keeps the last usage seen

**Files:** Modify `internal/format/outputguard.go`; Test `internal/format/format_test.go`.

**Produces:** `OutputGuard.usage` (last `usageMetadata` seen).

- Step 1 (red): `TestOutputGuardFinishKeepsTheLastUsage`: event 1 has usage `{promptTokenCount:10, candidatesTokenCount:4, thoughtsTokenCount:3}`. Event 2 has no `usageMetadata` and 40 bytes of text, so it crosses a limit of 10. Feed both events and the final event to a `ThinkingAccumulator`. Assert `InputTokens()==10`, `ThinkingTokens()==3`, `OutputTokens()==14`.
- Step 2: `go test -count=1 -run TestOutputGuard ./internal/format` → FAIL (input 0).
- Step 3: the guard stores each non-nil usage. The thought count and the final usage come from the stored usage.
- Step 4: re-run Step 2 → PASS.
- Step 5: `git commit -m "fix(format): build the output guard's finish from the last usage seen"`.

## Task 4 — Count tool-call arguments in the estimate

**Files:** Modify `internal/format/outputguard.go`; Test `internal/format/format_test.go`.

- Step 1 (red): `TestOutputGuardCountsToolCallArguments`: Claude-shaped events with a constant reported count of 4 and a limit of 10. The second event has a `functionCall` part with 40 bytes of `args` and no text. Assert that the guard stops.
- Step 2: `go test -count=1 -run TestOutputGuardCountsToolCallArguments ./internal/format` → FAIL.
- Step 3: `guardedEvent` parts also decode `functionCall.args` as `json.RawMessage`. Its length counts as streamed bytes.
- Step 4: re-run Step 2 and `-run TestOutputGuard` → PASS.
- Step 5: `git commit -m "fix(format): count tool-call arguments toward the client output limit"`.

## Task 5 — Start a conversation only with a labelled agent call

**Files:** Modify `internal/format/agyrequest.go` (`ConversationStart`); Test `internal/format/format_test.go`.

- Step 1 (red): `TestConversationStartNeedsLabels`: build the first turn with `ModelOptions{}` (no enum, so no labels). Assert that `ConversationStart(..., "gemini-3.5-flash-lite")` is not ok. Then build the same first turn with an enum. Assert that it is ok: the unlabelled call did not use up the start.
- Step 2: `go test -count=1 -run TestConversationStartNeedsLabels ./internal/format` → FAIL.
- Step 3: `ConversationStart` returns `ok=false` when `agent.Request` has no `labels`. It does this before `Conversations.Start`.
- Step 4: re-run Step 2 and `-run TestConversationStart` → PASS.
- Step 5: `git commit -m "fix(format): send agy's conversation start only with a labelled agent call"`.

## Verification

- `gofmt -l .` → empty; `go build ./...`; `go vet ./...`; `go test ./...`; `go test -race -count=1 ./internal/format ./internal/accounts ./internal/api`.
- `python3 scripts/test_client_drift.py` (no pinned value moved; must still pass).
- `trail check`, then `git push fork HEAD:feat/agy-generation-body`.
- Handover (user runs, needs live proxy / mitmdump): `scripts/client_drift.py check --clients agy` against the rebuilt proxy.
