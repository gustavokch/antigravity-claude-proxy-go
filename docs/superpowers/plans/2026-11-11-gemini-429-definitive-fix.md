# Gemini 429 and Silent Failure Repair Implementation Plan

> **For agentic workers:** Use `superpowers:executing-plans` for inline execution or `superpowers:subagent-driven-development` for independent implementation slices. Complete the evidence gate before choosing the upstream patch. This is a plan, not a claim that the upstream cause is known or the bug is fixed.

**Goal:** Make Gemini generation succeed wherever a matched `agy` request succeeds, and deliver a prompt, actionable client error when an upstream rejection cannot be recovered transparently.

**Architecture:** Repair the translation route, not TLS or the upstream host. Establish the upstream discrepancy with the real production dispatcher, then correct only the responsible layer. Give hidden recovery one request-local time budget and preserve the resulting error through the HTTP/SSE boundary.

**Tech Stack:** Existing Go packages, standard HTTP/SSE, `testing/synctest`, the existing mitmdump capture infrastructure, and packet capture/tshark. No new dependency.

**Spec:** The user's report and the Required Behavior section of this document. Capture references are evidence, not proof of the current rejection's cause.

## Global Constraints

- Cloud Code uses standard Go TLS through an empty `tls.Config{}`. No TLS tuning, utls, alternative host, invented protobuf, project substitution, or sibling-model fallback.
- Preserve the Requested Model and its documented effort/explicit-thinking precedence. A successful response from another model is not a fix.
- Do not redo the account-level Start Burst or recurring control-plane cadence work already in the tree. Change it only if new differential evidence identifies a remaining defect.
- Preserve Session Pins, Overflow Pins, cooldown escalation, additive jitter, quota distinctions, and the no-replay boundary once upstream events have been forwarded.
- Capture only synthetic prompts or sanitized shapes/hashes. Never persist access/refresh tokens, cookies, private prompts, generated private content, or OAuth client secrets. Never put credentials on a command line.
- Use a separate `ANTIGRAVITY_CONFIG_DIR` and listener for diagnosis; do not change `HOME`, production accounts, the production listener, or the live config during comparison.
- Do not call a bare `RESOURCE_EXHAUSTED` proof of exhausted daily quota, an account ban, a shared project limiter, or excessive RPM.

## Evidence Available Now

| Finding | Evidence | Consequence |
|---|---|---|
| The reported rejection is real and reaches both accounts. | Local `forensics/upstream-429.jsonl`: 20:45:58, 20:46:41 and 20:46:52 on Nov 11; `gemini-3.8-flash-high`, HTTP 429, the same bare 139-byte error; no `Retry-After`, `ErrorInfo` or `RetryInfo`. | The response does not identify the limiting dimension. Do not guess one. |
| The proxy hides the rejection while waiting. | Local runtime log: first request began 20:45:46.132, recorded a 429 and a 35 s pool wait, and ended with client cancellation after 39.469 s. The next request tried both accounts, entered another 34 s wait, and was canceled after 32.276 s. | The observed silence precedes any HTTP/SSE response; it is not evidence of a swallowed, already-sent SSE error. |
| The current wait setting is not an end-to-end recovery budget. | `internal/accounts/dispatcher.go:444-479`: each pool wait is compared independently with `maxWait`; `attempt--` means an unavailable-pool cycle does not spend an attempt. Capacity, switch and pacing sleeps are separate at `:508-576`. The deployed `maxWaitBeforeErrorMs` is 120000. | Repeated admissible waits can outlast the setting, and ordinary cooldowns already outlast the caller's patience. |
| Basic HTTP 429 mapping already exists. | `internal/api/server.go:3360-3425`; the existing pool/upstream classification and Retry-After tests. | Repeating the old 400-to-429 fix would miss the current failure. |
| Error delivery has adjacent correctness gaps. | `server.go:3262-3269` hardcodes `api_error` after stream start; `:3256-3261` discards the converter's completion error, allowing empty HTTP 200. `retryAfterSeconds` reconstructs a first-failure cooldown for a raw upstream 429 rather than using the actual applied, escalated, jittered account state. | Preserve the real outcome and retry eligibility; do not merely improve a log message. |
| A concrete thinking-shape divergence can be hidden by the probe. | Successful agy 1.5.6 captures send `thinkingLevel: HIGH`. `format/request.go:158-196` sends a budget when `ModelOptions.ThinkingLevel` is empty; `modelcatalog/catalog.go:759-761` preserves direct-tier entries without synthesizing their level. `tools/clientprobe/main.go:109-123` supplies `HIGH` itself, bypassing catalog resolution; the drift proxy requests intentionally receive dummy-token 401s. | First inspect the live catalog: if it publishes the level, this candidate is not live. Otherwise compare the production budget request against the captured level request. The cause of the 429 remains unproved. |

The account-wide prior-minute counters on the latest three rejects were 11, 11 and 10, with one in-flight request each. These are not measurements of per-model RPM or proof of a concurrency limiter. Today's earlier agy success and later proxy failures were not a controlled same-window comparison.

Existing baseline check run during planning:

```sh
go test ./internal/accounts ./internal/api -run 'TestForcedQuota429CoolsDownAndRotates|TestCapacityRetriesSameAccountWithTieredBackoff|TestClassifyErrorMapsPoolExhaustionTo429|TestRetryAfterSecondsRoundsUp' -count=1 -v
```

Observed: four tests passed in two packages. This proves those existing contracts, not the repair or the live root cause. No new live generation request was made to reconfirm the user's report.

## Required Behavior

1. **Upstream parity:** The production route must complete the matched Gemini scenarios that `agy` completes, with the same Google subject, project, exact Upstream ID, effort, egress and test window. The alias `gemini-3.8-flash` must resolve to the intended published tier without a hidden model change.
2. **Bounded hidden recovery:** This plan recommends a maximum **10-second** recovery window, using the existing fast-retry boundary rather than adding a new setting. The effective window is the smaller of 10 seconds and the configured positive `maxWaitBeforeErrorMs`. Start it on the first retryable failure or blocked account selection, not on a healthy request's first send. Never reset it on a new account or loop iteration. This is an intentional change from waiting silently through ordinary 30–120 s cooldowns; it does not change the manager's pin-wait threshold.
3. **No futile sleep:** If the next cooldown or delay cannot fit in the remaining window, return the actionable error immediately. Count pool/pinned waits, switch delay, pacing, capacity backoff and first-event waiting on a retry within the same window. Do not shorten or erase the actual account cooldown to make a retry fit.
4. **Correct HTTP failure:** Before stream commitment, a terminal upstream/pool 429 produces HTTP 429, the existing Anthropic `rate_limit_error` envelope, the model and retry guidance, and a positive, rounded-up `Retry-After` based on actual applicable eligibility. Do not replace it with generic 500, 504, or a false claim of daily quota exhaustion. A real 503 remains a capacity/API error; a client cancellation remains a cancellation.
5. **Correct streaming failure:** After commitment, classify the failure consistently and flush one terminal protocol error. No attempted HTTP status change, no successful `message_stop` after the error, no replay, no manufactured assistant response, and no heartbeat-only substitute for an error. Existing OpenAI-compatible translation must preserve the equivalent failure if that endpoint is used. Propagate the converter's existing completion error instead of returning an empty HTTP 200.
6. **Healthy streams remain uncapped:** Stop the recovery timer at the existing first-forwarded-upstream-event boundary, before calling the consumer. Do not attach an irrevocable timeout to an established healthy stream. This is not the same as the first client byte: an upstream metadata event may produce no converter output. Preserve that existing no-replay boundary, the normal transport timeout for subsequent stalls, reading past a client's output limit, and dispatcher shutdown ownership.
7. **Actual visibility:** The caller that exhibited the incident must visibly report the error or retry state. `curl`, a recorder, a server log, or an SSE ping alone is not that acceptance gate.

## Review Focus

- A first 429 carries no reset hint, has additive jitter, and is followed by an escalated second rejection: the client must not retry before its applicable account is eligible. Owned by Tasks 2 and 3.
- A Session Pin waits while another account exists, or concurrent work extends a cooldown: selection must not restart the recovery window. Owned by Task 2.
- Capacity backoff and a slow retry consume the remaining window; healthy first attempts and established streams must not inherit its deadline. Owned by Task 2.
- Error before commitment versus after text/thinking output: preserve protocol semantics and do not report success after failure. Owned by Task 3.
- A test/probe hardcodes the captured model options while the live catalog selects different options: the release must exercise production selection, including direct ID, alias, explicit thinking and a tool-result turn. Owned by Tasks 1 and 4.

## Task 1: Identify and Correct the Upstream Discrepancy

**Files and boundaries:**

- References: `.reference/client-drift/agy.jsonl`, `.reference/agy-1.5.6-control-plane-20261111.txt`, the latest local drift report.
- Production path: `internal/accounts/dispatcher.go:429-585`, `internal/modelcatalog/catalog.go`, `internal/format/request.go`, `internal/format/builder.go`, `internal/format/agyrequest.go`, `internal/cloudcode/client.go`.
- Diagnostic boundaries: `tools/clientprobe/main.go:88-168`, `cmd/debug429/main.go:23-168`, `scripts/client_drift.py`, `scripts/mitm_header_dump.py`.
- Add one sanitized evidence summary: `.reference/gemini-429-repair-20261111.txt`. No raw private captures in Git.
- Extend the existing test file in the layer actually corrected; for a thinking-shape correction use `internal/modelcatalog/catalog_test.go`, `internal/accounts/dispatcher_test.go` and `internal/format/thinkingconfig_test.go` at their respective real seams. Reconcile `docs/reasoning-parameters.md` and `CONTEXT.md` if capture-proven behavior changes the current budget-style description. Do not add a separate test framework or change every candidate layer.

**Interfaces:** Keep `Dispatcher.StreamGenerateContent` and the Cloud Code client interfaces unchanged. Compare the request emitted by that dispatcher, not just a manually populated `format.ModelOptions` value. Preserve explicit client thinking/budget precedence unless the matched capture demonstrates the current translation is invalid.

- [ ] **Step 1: Establish the controlled comparison.** Inventory the running binary and source revision; inspect the live catalog's exact `thinkingLevel`/`thinkingBudget` fields before asserting the candidate discrepancy. Verify the Google subject, account source/OAuth client identity, discovered project, exact model and effort without printing tokens. Make a single-account diagnostic config for the account used by agy. Hold model, subject, project and egress fixed. Use a synthetic short prompt, then the relevant large/tool-bearing shape and one tool-result continuation. Run both direct ID and user-facing alias through the real proxy HTTP route.
- [ ] **Step 2: Capture paired evidence.** Run agy and the production proxy in a bounded same-window comparison, alternating their order to expose temporal bias. Record response status/error details, first-output or terminal-error timing, resolved model, reasoning settings, envelope/labels/session correlations and control-plane endpoint set/cadence. Reuse capture sanitization. Treat dynamic identifiers as correlated values, not fields to delete indiscriminately. A MITM comparison is for request structure; confirm success/failure directly without MITM too, because the intermediary changes the server-facing TLS connection. Do not induce a burst or repeatedly hammer a known cooldown.
- [ ] **Step 3: Falsify the highest-value hypotheses one variable at a time.** Use the table below. Preserve the same authentication and request provenance for each comparison. An arbitrary minimal request from `debug429` is not equivalent to the production dispatcher; its old raw mode omits the agy envelope, and its builder mode does not resolve the live catalog.
- [ ] **Step 4: Write the causal verdict before editing.** Name the discrepant field/state/load behavior, its owner, and the comparison that changes 429 to successful output. If no controlled divergence is established, do not select an upstream patch or claim that longer backoff fixes it. Finish Tasks 2–3, but keep upstream resolution explicitly unaccepted until the comparison identifies either the defect or a matched upstream rejection.
- [ ] **Step 5: Add the narrow regression at the real seam, observe failure, then correct the identified owner.** For a model/reasoning defect, feed a sanitized real catalog through production resolution/building and assert the emitted request and relevant downstream behavior, including alias, explicit budget/effort and continuation cases. For envelope/auth/state defects, exercise the responsible production path rather than a copied fixture or source-text assertion. Use only capture-proven fields; do not hand-build a replacement client identity.
- [ ] **Step 6: Verify the correction.** Run the new named regression and its affected package once, then rerun the bounded direct comparison. Save the verdict, commands and sanitized observations in the evidence summary. The upstream task passes only with attributable success on the production route; a coincidental recovery, a dummy-token 401, or a different model does not pass.

### Ranked hypotheses and patch selection gate

| Priority | Hypothesis, not established cause | Distinguishing evidence | Patch owner only if confirmed |
|---|---|---|---|
| 1 | The actual catalog/build path emits a different Gemini request from the working agy path. The concrete candidate is `thinkingBudget` instead of captured `thinkingLevel: HIGH` when the direct-tier catalog entry has no level. | Inspect that entry first. If it has no level, isolate level versus budget on the same account/model/workload; do not conclude causality just because both forms later succeed. Compare other envelope/label differences only as needed. | `modelcatalog/catalog.go`, `format/request.go`, `format/builder.go` or `format/agyrequest.go`, whichever introduces the demonstrated difference. |
| 2 | Successful agy and the proxy are not using equivalent account/authentication/project/model inputs. | Compare subject and credential provenance, discovered project and exact Upstream ID in the same window; isolate account source/OAuth client identity only if they differ. | Existing account/authentication owner; consult ADR-0006 before any sign-in-client change. Do not rotate or replace credentials speculatively. |
| 3 | The proxy's complete workload differs from agy's cadence or request concurrency. | Count actual wire calls, including background calls and retained streams; compare isolated versus the observed workload without simultaneously changing payload/authentication. | Existing dispatcher/pacing owner, only if serialized or cadence-corrected traffic demonstrably removes the rejection. |
| 4 | The rejection is temporal/backend-scoped rather than a stable proxy defect. | Matched alternating comparisons fail or recover together, with equivalent inputs and transport. | No invented identity patch. Retain accurate bounded error handling and document the measured upstream behavior. |

## Task 2: Bound Hidden Recovery and Preserve the Applied Outcome

**Files:** `internal/accounts/dispatcher.go:429-585`, `internal/accounts/retry.go:260-277`, `internal/accounts/retry_test.go`; extend `internal/accounts/conversation_test.go` for the pin boundary when needed. Read `internal/accounts/outputlimit.go:76-172` before integrating cancellation with its existing reader ownership.

**Interfaces:** Preserve `StreamGenerateContent`'s signature. Extend `RateLimitError` with an optional-value `Cause error` field and `Unwrap() error` method to preserve the original upstream failure; keep its existing model, wait and scope fields and existing callers. The terminal retry duration comes from the relevant applied account/pool/pinned state at return time, not `UpstreamCooldown(..., failures=0)`. The internal recovery timer must not become a wrapped `context.DeadlineExceeded` that takes precedence over the preserved 429; a real parent deadline still takes precedence.

- [ ] **Step 1: Add behavioral regressions with the existing deterministic dispatcher/clock seams.** Name the core test `TestDispatcherBoundsSilentRecovery`, with cases for a bare 429 followed by a cooldown longer than the budget; repeated individually short waits; a pinned selection; a concurrently extended limit; capacity backoff; switch/pacing delay; and a retry that never delivers its first event. Assert elapsed recovery time, terminal error classification, preserved account eligibility, and no upstream send before that eligibility. Use a parent deadline only as a watchdog, not as the expected source of termination.
- [ ] **Step 2: Prove healthy/cancellation boundaries.** Add `TestDispatcherRecoveryBudgetEndsAtFirstEvent`: a recovered stream must continue beyond the recovery window without cancellation. Also cover a slow but healthy initial attempt, client cancellation during cooldown and dispatcher shutdown, using existing output-limit lifecycle tests rather than duplicating them.
- [ ] **Step 3: Run the new regressions to observe the intended failures.** `go test ./internal/accounts -run 'TestDispatcherBoundsSilentRecovery|TestDispatcherRecoveryBudgetEndsAtFirstEvent' -count=1 -v`. The old code must fail because of excessive hidden recovery, wrong terminal provenance, or a retry waiting until the watchdog, not because of a broken test fixture.
- [ ] **Step 4: Implement one lazy request-local recovery window.** Snapshot the relevant config coherently. Arm it on the first retryable failure/positive blocked selection. Apply the remaining budget to every retry-related wait and the pre-event portion of retries; do not reset it when rotating. Disarm the cancellation timer before handing the first forwarded event to the consumer, with synchronization that resolves a timer/event race consistently. Avoid an irrevocable `context.WithTimeout` on the lifetime of a successful stream. No additional state/allocations on an ordinary successful first attempt beyond what this boundary requires.
- [ ] **Step 5: Preserve cooldown and error provenance before leaving an attempt.** Apply and record the existing 429 outcome before a switch sleep or early budget return can lose it. Reuse existing forensics, once per rejection; do not add a telemetry subsystem. Return the typed 429 with the applicable remaining eligibility and original cause when recovery cannot fit. Preserve capacity versus rate-limit distinctions and caller cancellation. Keep the existing manager pin-selection policy; the new recovery window limits how long the HTTP request hides that state, not whether a pin or quota reading exists.
- [ ] **Step 6: Verify and document the behavior.** Run the new regressions plus the existing quota-rotation, short-capacity-recovery, pin/overflow and output-limit lifecycle checks. Update the active README wait/error contract and CONTEXT's rate-limit definition: retry hints can be absent, the inferred cooldown is not a measured Google reset, and internal recovery now has the 10 s ceiling. Do not rewrite historical evidence/specs.

## Task 3: Deliver the Error Through the Actual Client Protocol

**Files:** `internal/api/server.go:3162-3296,3360-3434`, `internal/api/server_test.go`; exercise `internal/api/openai_proxy.go:245-278` through `internal/api/openai_proxy_test.go` and change the adapter only if its consumer-visible error handling fails. Do not change unrelated forwarding gateways or redesign `stream_terminal.go`.

**Interfaces:** Reuse `classifyError` for HTTP and committed-stream errors. `retryAfterSeconds(error) int` must prefer Task 2's actual typed eligibility. Preserve the existing external Anthropic/OpenAI error shapes rather than inventing an extension event.

- [ ] **Step 1: Add `TestMessagesGemini429Feedback` at the handler → real dispatcher boundary.** Use a local upstream fixture returning the observed bare 429 and deterministic time; exercise both streaming-request and unary-request forms. Assert terminal HTTP 429, the parsed `rate_limit_error` envelope, no prior 200/message-start, the model/retry guidance, and an integer `Retry-After` that covers the actual jittered/escalated applicable cooldown. Include budget expiry while a retry is blocked and attempt exhaustion before a new selection. Testing `classifyError` alone does not cover the incident.
- [ ] **Step 2: Add `TestMessagesCommittedStreamPreservesErrorKind` and `TestMessagesEmptyStreamReportsFailure`.** For the first, deliver valid text/thinking output before a typed failure. Assert one terminal SSE error of the correct kind, preserved preceding content, no replay and no successful Anthropic message terminator. Include an actual write failure/client disconnect to prove the handler stops instead of treating delivery as success. For the second, let the upstream end without content and assert HTTP 502 with the existing `api_error` envelope, not an empty HTTP 200. Exercise the committed-error fixture through `/v1/chat/completions` and assert that its consumer observes the failure. That adapter currently appends `[DONE]` after an error; the sentinel alone is not proof of a masked error, so change termination only if a consumer-level check demonstrates the defect.
- [ ] **Step 3: Observe failure, then implement the boundary correction.** Run the new tests. Replace the hardcoded committed-stream `api_error` with the existing classifier's kind/message; use the typed eligibility for pre-commit HTTP retry advice. Fold `converter.Finish()`'s error into the existing error path and propagate failed writes. Ensure delivery errors are not swallowed or logged as successful completion. Do not promise an HTTP `Retry-After` header after headers have already been sent.
- [ ] **Step 4: Verify once at the affected boundary.** `go test ./internal/api -run 'TestMessagesGemini429Feedback|TestMessagesCommittedStreamPreservesErrorKind|TestMessagesEmptyStreamReportsFailure|TestClassifyErrorMaps.*429|TestRetryAfterSeconds|TestWriteErrorRetryAfterCoversDispatcherCooldown' -count=1 -v`. Retain semantic status/timing/eligibility checks; delete rather than re-pin tests that only assert incidental wording.

## Task 4: Prove the Repair End to End and Deploy It

- [ ] Run the affected Go package tests with race detection once after integration, using the exact package set touched by the causal patch plus `internal/accounts` and `internal/api`. Format only changed Go files. Update README/troubleshooting and any active contract documentation affected by the final upstream correction.
- [ ] Recheck request identity using a real catalog through the production dispatcher. Keep the existing dummy-token identity probe for what it measures; it must not be described as proof of authenticated Gemini success. If the new regression identifies a flaw in that probe's hardcoded options, correct the probe to derive them through production selection rather than adding another copied model fixture.
- [ ] Run the packet gate and inspect its result, not merely its exit code: `bash scripts/verify-ja4.sh`. Required observed JA4: `t13d1312h2_f57a46bbacb6_f50d94e863eb`. A reported SKIP is an unmet release gate until a real capture can be obtained. Do not tune TLS to resolve a mismatch.
- [ ] Launch the actual requesting client against the isolated production build. Observe a successful Gemini first turn and tool-result continuation, and the visible error/retry state from the controlled 429 scenario. Also observe the live service's standard HTTP 429/SSE failure behavior without an unrelated gateway intercepting the model. Raw HTTP tests do not replace this visibility check.
- [ ] Repeat the bounded matched direct agy/proxy success gate on the original failing request shape. Record the build/revision and redacted outcome. A mid-investigation Google recovery alone is not causal proof; retain the controlled differential result from Task 1.
- [ ] Install/restart only after these gates pass, preserving the existing listener/flags. Keep the previous binary and config for rollback; do not reset accounts, discard quota, delete pins to conceal the failure, or restart unrelated clients. Perform one production Gemini smoke request and inspect the terminal outcome.
- [ ] Run `trail check --base fork/main` before any commit/push/PR. Commit the patch, meaningful regressions, active docs and sanitized evidence together; push only to `fork` if requested. Remove temporary diagnostic configs/captures and any throwaway harness; never commit credentials or private content.

## Acceptance / Stop Conditions

The repair is complete only when **both** are demonstrated:

- **Success path:** matched production Gemini requests succeed where agy succeeds, and the responsible discrepancy has a causal explanation or a properly matched upstream verdict. No substituted account/model/project/host hides a failure.
- **Failure path:** unrecoverable 429s reach the actual caller as visible, truthful errors with applicable retry guidance; hidden recovery is bounded, and established streams are not cut off by that bound.

If the upstream cause is still unresolved, report that explicitly and retain the failed acceptance gate. Shipping only the feedback repair must not be called a definitive resolution of the Gemini 429s.

## Planning Review Verdict

**GO:** The confirmed feedback mechanism has an owned repair, deterministic timing/protocol regressions, and an actual-client acceptance check. The upstream investigation has bounded, controlled comparisons and a production-path test seam.

**NO-GO for a speculative upstream patch:** The current live catalog and a matched direct comparison must establish the responsible discrepancy before changing model reasoning, authentication, workload policy or request identity. Existing captures alone do not prove that `thinkingBudget` caused these 429s.

Self-review incorporated two independent read-only investigations. It rejected unnecessary new telemetry/pacing work and an unproved claim that an OpenAI `[DONE]` sentinel by itself masks an error. Product code, configuration, accounts and credentials were not changed during planning.
