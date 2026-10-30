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
| 4 | 🟡 the guard cancels the upstream stream; agy reads to the end | Task 6 (owner chose: finish the client at once, read upstream to EOF in the background) |
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

## Task 6 — Finish the client at its limit and read upstream to the end

Decision (owner, 2026-10-30): option B. The client gets the MAX_TOKENS finish at once. Upstream is read to EOF in the background, as agy does: no cancel and no RST_STREAM, and no new size or time limit (a hung stream keeps only the transport's own timeouts).

**Files:** Create `internal/accounts/outputlimit.go` (`Dispatcher.Close`, `goBackground`, `detach`, `sendStream`, `settle`); Modify `internal/accounts/dispatcher.go` (lifetime fields, the send loop, `startConversation` on the lifetime), `internal/format/outputguard.go` (drop `ErrOutputLimit`), `cmd/proxy/main.go` (`dispatcher.Close()` after the HTTP drain); Test `internal/accounts/agy_conversation_test.go`, `internal/api/output_limit_test.go`, `internal/accounts/dispatcher_test.go` (`t.Cleanup(dispatcher.Close)`).

**Produces:** `func (dispatcher *Dispatcher) Close()`.

- Step 1 (red): `TestDispatcherFinishesAtTheClientLimitAndReadsUpstreamToTheEnd`. The upstream fake streams a head, blocks until it is released, then streams a tail that carries `remainingCredits`. Assert: (a) the dispatcher returns while upstream still blocks, with 2 events + the MAX_TOKENS finish delivered; the in-flight count is 1. Then cancel the client context, release upstream, and assert: (b) upstream read to EOF, its context never cancelled; in-flight 0 and the trailing credits recorded; nothing delivered after the finish. `TestMessagesAnswerAtTheClientLimitWhileUpstreamIsReadToTheEnd` (stream and unary subtests; real dispatcher; request context cancelled after the handler returns, as net/http does) asserts (a), (b) and `stop_reason` `max_tokens` on both paths (c).
- Step 2: `go test -count=1 -run '<those tests>' ./internal/accounts ./internal/api` → FAIL on `65c685c` ("the client got its finish only after upstream ended (read to EOF false, cancelled false)").
- Step 3: guarded streams run the upstream call on a goroutine that `Close` waits for. Its context is `WithoutCancel(client ctx)`, cancelled by the dispatcher lifetime, and by the client context only until the finish (`context.AfterFunc`, stopped at the finish). `consume` still runs only before `sendStream` returns. A consume panic before the finish is raised again on the caller's goroutine. Unguarded streams keep the synchronous call.
- Step 4: what the background read holds. The in-flight count (`throttleMeter`) stays until EOF, because upstream still serves the request on that account and the 429 journal calibrates concurrency from it. The credits and the live quota reading are recorded at EOF, because upstream reports the consumption at the end. The client's success bookkeeping (`MarkSuccess`, recovery journal, execution metadata) and the client context are released at the finish.
- Step 5: add `TestDispatcherCloseEndsAReadPastTheClientLimit` (`Close` cancels a hung read and returns; a request after `Close` gets `errDispatcherClosed` at once) and `TestDispatcherRaisesAConsumePanicOnTheCallersGoroutine`.
- Step 6: re-run → PASS; `-race -count=20` on the new tests.
- Step 7: `git commit -m "fix(accounts): finish the client at its max_tokens and read upstream to the end"`.

## Verification

- `gofmt -l .` → empty; `go build ./...`; `go vet ./...`; `go test ./...`; `go test -race -count=1 ./internal/format ./internal/accounts ./internal/api`.
- `python3 scripts/test_client_drift.py` (no pinned value moved; must still pass).
- `trail check`, then `git push fork HEAD:feat/agy-generation-body`.
- Handover (user runs, needs live proxy / mitmdump): `scripts/client_drift.py check --clients agy` against the rebuilt proxy.
- Handover for Task 6 (live): a Claude Code or curl request with a small `max_tokens` through the rebuilt proxy. Expect `stop_reason: max_tokens` at once and no RST_STREAM in a packet capture (`tshark -Y 'http2.type == 3'`).
