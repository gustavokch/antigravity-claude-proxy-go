# Conversation-Aware Cloud Code — Design

Status: approved section by section in conversation on 2026-11-07; not implemented.
Covers three changes that share one per-conversation record: session affinity for the Cloud Code account pool, the control-plane calls agy makes that the proxy does not, and `POST /v1/messages/count_tokens`.
Related: `.reference/client-drift/agy.jsonl` (agy 1.5.2 capture), `.reference/agy-1.4.10-wire-20261030.txt`, `.reference/claude-code-count-tokens-20261107.jsonl` (Claude Code 2.1.311 count_tokens capture), `internal/accounts/manager.go`, `internal/accounts/dispatcher.go`, `internal/claudecode/pool.go` (existing sticky pattern).

## 1. Goal and scope

Close three gaps found when comparing the proxy with agy and CLIProxyAPI on 2026-11-07:

1. A Cloud Code conversation can change account between turns. `hybrid` scores per request and `sticky` keeps one global account for every request (`selectStickyLocked`, `manager.go:845`). The cascade id is derived from the account email (`agyrequest.go:106`), so a switch starts a new cascade mid-conversation, which agy never does, and loses the prompt cache.
2. The proxy never sends `fetchAdminControls`, `fetchUserInfo`, `listExperiments` or `GET /v1internal/cascadeNuxes`, and it calls `retrieveUserQuotaSummary` after a stream (`settle`, `outputlimit.go:196`) where agy calls it before a turn.
3. `POST /v1/messages/count_tokens` returns 501 (`server.go:523-524`).

Out of scope here (separate specs, in this order): hot reload of `config.json`/`accounts.json`, secret redaction, automatic prompt-cache breakpoints. Cross-gateway fallback chains were considered and dropped.

## 2. Decisions taken

| Topic | Decision |
|---|---|
| Pin key | Session id; one pin per session for all models. |
| Strategies | Affinity sits on top of `hybrid`, `sticky`, `round-robin`, always on, no setting. The strategy chooses only when a pin is created, rebound or overflowed. Requests without a session id keep today's per-request choice. |
| Pin lifetime | In memory; dropped after 1 h without a request; cap 10,000 pins, oldest evicted. Lost on restart. |
| Wait vs move | A per-model rate limit (`MarkRateLimited`, not a failure lock) that resets within the pin wait bound makes the request wait for the pinned account, once: a second limit on that account with no success in between (`ConsecutiveFailure` ≥ 2) moves the model, so repeated short limits cannot use up the request's attempts. The bound is `maxStickyWait` (2 m, `manager.go:24`) lowered to the Dispatcher's `maxWait` (`maxWaitBeforeErrorMs`) when that is smaller, so a pinned wait never turns into a rate-limit error while another account is free. A longer limit, quota exhaustion (`ExhaustedUntilMS`), a failure lock (credential failures count toward it) or the 404 model lock moves that model to an overflow account. |
| Overflow | `(session, model) → account B`, chosen by the strategy, kept until B itself becomes unusable or the session expires. No flip-back when the home account recovers. |
| Control-plane cadence | Start burst when a session first uses an account; per-turn calls before each new user prompt; both in agy's order before generation. |
| Control-plane responses | Per-turn `loadCodeAssist` and `retrieveUserQuotaSummary` feed tier/project and quota; the post-stream quota call is removed. The other new calls are sent for parity and only their status is logged. |
| count_tokens | Forward to Claude Code, OpenRouter, Kimi and Anthropic-format custom endpoints; estimate for Cloud Code and Zen. |
| Estimate | Last real input count of the conversation plus a learned chars-per-token estimate of the new messages. |
| Architecture | Pins inside `accounts.Manager` under its mutex; control-plane calls in the Dispatcher; token memory in a new `internal/tokencount` package. |

## 3. Session key

- The server already extracts a session id for metrics: `ccExtractSessionID(request, body)` (`claudecode_proxy.go:194`), header first (`X-Claude-Code-Session-Id`, then other session headers), then `metadata.session_id`, then the session inside `metadata.user_id`. Affinity and count_tokens use the same function, so clients that send only a header still get a pin.
- `server.messages()` puts the key on the request context with a new `accounts.WithSessionKey(ctx, key)`; the Dispatcher reads it with `accounts.SessionKey(ctx)`. The cascade derivation in `internal/format` keeps reading the body (`clientSessionID`); for Claude Code both carry the same UUID (captures in `.reference/claude-code-headers-*.jsonl`).

## 4. Session affinity (`internal/accounts`)

### 4.1 API

- `Manager.Select(model string) Selection` becomes `Select(session, model string) Selection`. Callers: `dispatcher.go:429` (generation, passes the context key) and `dispatcher.go:338` (catalog fetch, passes `""`). No alias for the old signature.
- `selectStickyLocked`, `selectRoundRobinLocked` and `selectHybridLocked` take an exclusion set of account emails; existing behaviour is unchanged when the set is empty.

### 4.2 State (`internal/accounts/conversation.go`)

```go
type pin struct {
	home     string            // account email
	overflow map[string]string // rateLimitModelKey(model) -> account email
	started  map[string]bool   // account emails whose start burst ran for this session
	lastSeen time.Time
}
// Manager gains: pins map[string]*pin, guarded by manager.mu.
```

Pins hold emails, not `*Account` pointers: an email no longer in `manager.accounts` counts as a removed account. This keeps pins valid when accounts are reloaded.

### 4.3 Selection

Under `manager.mu`, after `clearExpiredLocked` and the existing shared-throttle check:

1. Drop pins idle for more than 1 h (lazy sweep); evict the oldest pins above 10,000.
2. No session key → today's strategy call.
3. No pin → strategy pick; create the pin with `home` = picked account.
4. Pin exists → `target` = `overflow[model key]` if set, else `home`.
   - Target usable for the model → return it; set `lastSeen`.
   - Target rate-limited for the model (a `MarkRateLimited` limit, not a failure lock, no exhausted quota) with reset ≤ the pin wait bound → `Selection{Wait: reset}`; the Dispatcher's loop sleeps and selects again (`dispatcher.go:434-447`).
   - Target invalid, disabled or removed → if it is `home`, strategy pick and replace `home` (overflow entries pointing at healthy accounts stay); if it is an overflow account, re-pick that overflow entry.
   - Otherwise (longer limit, quota, failure lock, 404 lock) → strategy pick excluding the target; set `overflow[model key]`. If no other account can serve the model, nothing changes in the pin and the result is what today's selection returns when every account is limited (`Selection{Wait: shortest reset}` or empty).
5. Every pick that creates or changes a pin logs at info: session prefix (first 8 chars), model, old and new account, reason.

`sticky` gives every new pin the global current account, `round-robin` rotates per new pin or overflow, `hybrid` scores as today.

### 4.4 Visibility

`/api/accounts` adds `activeSessions` per account: pins whose `home` or any overflow value is that email.

## 5. Control-plane parity

### 5.1 Calls (`internal/cloudcode/client.go`)

| Method | Request | Headers (order) |
|---|---|---|
| `FetchAdminControls(ctx, project)` | `POST /v1internal:fetchAdminControls`, `{"project":"<id>"}` | as today's unary POST: Host, User-Agent, Content-Length, Authorization, Content-Type, Accept-Encoding |
| `FetchUserInfo(ctx, project)` | `POST /v1internal:fetchUserInfo`, `{"project":"<id>"}` | same |
| `ListExperiments(ctx, project)` | `POST /v1internal:listExperiments`, `{"project":"<id>"}` | same |
| `ListCascadeNuxes(ctx)` | `GET /v1internal/cascadeNuxes?page_size=1000`, no body | Host, User-Agent, Authorization, Accept-Encoding |

Source: `.reference/client-drift/agy.jsonl` (all four recorded with these shapes and headers; JA4 `t13d131100_f57a46bbacb6_f50d94e863eb`). The client gains a GET path; transport and TLS are unchanged.

### 5.2 Sequences

From the two generation runs in `agy.jsonl` (calls in braces were seen in both orders, so agy sends them concurrently):

- **Start burst:** `loadCodeAssist` → {`fetchAdminControls`, `fetchUserInfo`, `retrieveUserQuotaSummary`} → `fetchAvailableModels` → `retrieveUserQuotaSummary` → `listExperiments` → `loadCodeAssist` → `GET cascadeNuxes`. (`cascadeNuxes` appears in the later run only; it is sent.)
- **Per turn:** `loadCodeAssist` → {`fetchUserInfo`, `retrieveUserQuotaSummary`} → generation.

The first turn of a session on an account gets the burst followed by the per-turn calls, as in the capture. `writeTrajectoryAcls` and the title checkpoint keep their current behaviour (`startConversation`, `dispatcher.go:573`).

The proxy sends this order, the order of the agy 1.5.2 baseline. Checks against agy compare the set of endpoints, not their order: captures of the same agy version (1.5.2, 2026-11-08) reorder calls beyond the braced groups and send `retrieveUserQuotaSummary` 8 or 9 times, so only the set is stable.

### 5.3 Triggers (`internal/accounts/controlplane.go`)

- `beginConversation(ctx, session, account, client, project)`: runs when `started[account]` is false for the session's pin, then sets it. Concurrent requests of one session on one account share one run (single-flight keyed by session + email). When the request that leads the run ends before the run does, a waiting request whose context is alive takes it over; a run that ends on its own deadline is not taken over.
- `beginTurn(ctx, session, account, client, project, request)`: runs when the request is a new user prompt, using the rule `ccidentity.promptOf` already applies to the Claude Code captures (`internal/ccidentity/turn.go:168-219`): the request declares tools, and its last message has role `user` with a non-empty `text` block that does not start with `<system-reminder>`, or an `image` or `document` block. Claude Code appends `<system-reminder>` text to tool-result messages, so a plain "has a text block" rule would fire on tool continuations. Tool continuations and tool-less side calls (titles, topic checks) get nothing; agy's own side call, the checkpoint, gets nothing either. Identical concurrent prompts share one run (single-flight keyed by session + SHA-256 of the last user message).
- Both are called in `StreamGenerateContent` after `resolveProject` and before `startConversation`, and block the generation until they finish. Requests without a session key trigger neither.

### 5.4 Responses

- Per-turn and burst `loadCodeAssist` → `ExtractSubscription`, `CacheProject`, `UpdateSubscription` (the code now at `dispatcher.go:909-918`, moved into one helper).
- `retrieveUserQuotaSummary` → `cloudcode.ParseQuotaSummary` + `mergeQuotaReading` (now in `refreshLiveQuota`, `dispatcher.go:1138`).
- Burst `fetchAvailableModels` → the account's catalog snapshot (existing per-account fetch).
- `settle` (`outputlimit.go:188`) keeps recording credits and drops `refreshLiveQuotaThrottled`; `refreshLiveQuotaThrottled` is deleted if no caller remains.
- `fetchAdminControls`, `fetchUserInfo`, `listExperiments`, `cascadeNuxes`: status logged at debug; bodies discarded.

### 5.5 Failures

Each call has a 10 s timeout, and a burst or a turn's calls have a 15 s deadline in all; both run on the request context (a client that disconnects stops the burst). An error, timeout or non-2xx is logged at debug and generation proceeds. A burst sets `started` once every call got an HTTP answer, whatever its status: a call that always answers 4xx for an account must not make every later request resend the whole burst. Only a transport error, a timeout or an ended request context leaves `started` unset, so the next request retries the burst.

## 6. count_tokens

### 6.1 Route (`internal/api`)

`POST /v1/messages/count_tokens` (and its `/anthropic` mirror) runs model mapping (`resolveModelMapping`) and the `gatewayOrder` matchers of `dispatchAlternateBackend`; no headroom, no classifier. The response is `{"input_tokens": N}`.

| Gateway | Action |
|---|---|
| Claude Code | Forward to `https://api.anthropic.com/v1/messages/count_tokens?beta=true` through `claudecode.Client` with the count_tokens identity of 6.2, not the `/v1/messages` normalization. |
| OpenRouter | Forward to `<baseUrl>/v1/messages/count_tokens`. |
| Kimi | Forward to `<base>/v1/messages/count_tokens` with the Kimi identity headers. |
| Custom (Anthropic format) | Forward to the endpoint URL with `/messages` replaced by `/messages/count_tokens`. |
| Zen, Cloud Code | Estimate (6.3). |

A forwarded call that answers 404, 405 or 501 marks that gateway and model (custom endpoints: that endpoint and model) as not counting for 1 h and returns the estimate: gateways answer 404 for one model they do not serve. A 429 returns the estimate and marks the gateway's counts for its Retry-After (10 s without one, at most 1 h). Each forward has a 10 s deadline. Any other failure (network, auth, timeout, 5xx) returns the estimate for that request. count_tokens never returns an error status for an upstream failure.

### 6.2 Claude Code count_tokens identity

Source: `.reference/claude-code-count-tokens-20261107.jsonl` and its `.meta.txt` (Claude Code 2.1.311, interactive, 2026-11-07, personal account). The real client calls count_tokens only from `/context`: 34 requests within 0.7 s, all 200. A tool turn, an `@` attachment, a second turn with an edit and `/compact` sent none. Its identity differs from `/v1/messages`:

| Field | count_tokens (captured) | `/v1/messages` main (same capture) |
|---|---|---|
| `x-claude-code-request-class` | `auxiliary` | `main` (what `ccidentity.staticHeaders` pins, `defaults.go:86`) |
| `User-Agent` | `claude-cli/2.1.311 (external, cli)` on all 34 counts | the same; the proxy's normalized `/v1/messages` sends `(external, sdk-cli)` by default, but a normalized count sends the captured `cli` form, since the real client counts only from interactive `/context` |
| `anthropic-beta` | `claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01` | longer list (18 entries on the first `main` call) |
| Body keys | `messages`, `model`, `tools` | `context_management`, `diagnostics`, `max_tokens`, `messages`, `metadata`, `model`, `output_config`, `stream`, `system`, `thinking`, `thread`, `tools` |
| `X-Claude-Code-Session-Id` | present | present |
| TLS / HTTP | JA4 `t13d1713h1_5b57614c22b0_7f527383e249`, HTTP/1.1 | same |
| Response | `200`, `application/json`, 19–22 bytes, no `anthropic-ratelimit-unified-*` headers | SSE, carries `anthropic-ratelimit-unified-*` |

- `ccidentity` gains a count_tokens profile: the captured header order, `x-claude-code-request-class: auxiliary`, and the captured beta list (version-pinned like the other betas, with a `client_drift.py` `Site` entry).
- The body is forwarded as the client sent it, with no billing header, identity block or `metadata` injected. The one change is the `model` value, which becomes the gateway's model id after model mapping (6.1), exactly as on `/v1/messages`; with the identity mapping the bytes are unchanged. The real client's count bodies carry only `messages`, `model` and `tools`; a caller that adds other keys (for example `system`) has them forwarded unchanged.
- `X-Claude-Code-Session-Id` is forwarded from the client when present, as on `/v1/messages`.
- A count response carries no unified rate-limit headers, so the Claude Code pool's rate-limit state is not updated from it. A 429 on a count marks the account as on `/v1/messages` only when it carries the unified headers (the subscription limiter); otherwise it backs off counts alone (6.1), because Anthropic documents count and message limits as separate.
- Load shape: counts arrive as bursts of about 34 concurrent requests per `/context`, never during turns. The Claude Code forward must not serialize them per account.

### 6.3 Estimator (`internal/tokencount`)

- **Characters measured:** text of `system`, every message's `text` and `thinking` blocks, `tool_use` input as compact JSON, `tool_result` content text, and each tool definition as compact JSON.
- **Ratio:** one chars-per-token value per family. `tokencount.Family(model)` returns `claude-4.7` for Claude 4.7 and later (a new tokenizer, read from the model id as for the image tier), `claude` for other ids containing `claude`, `gemini` when it contains `gemini`, `gpt-oss` when it contains `gpt-oss`, and otherwise the model id itself (so each other model, Zen ones included, learns its own ratio). Start value 4.0. Each observation of a request with at least 1,000 measured characters updates the ratio as an exponential moving average with α = 0.2, from an observed ratio held between 0.5 and 8. A request with content the measure does not price (any block but text, thinking, redacted thinking, tool use, tool result and image) or whose image tokens are half its real count or more is not learned from.
- **Images:** not part of the ratio. Claude and other non-Gemini families follow Anthropic's current rule (https://platform.claude.com/docs/en/build-with-claude/vision, "Resolution and token cost"): downscale to the tier's long-edge limit, then `ceil(w/28) × ceil(h/28)` tokens; standard tier 1,568 px long edge and at most 1,568 tokens, high-resolution tier (Claude 4.7 and later, read from the model id) 2,576 px and at most 4,784 tokens. Gemini family: 258 tokens per 768 × 768 tile, 258 for an image of 384 × 384 or less (https://ai.google.dev/gemini-api/docs/tokens). Dimensions come from `image.DecodeConfig` on base64 PNG, JPEG or GIF data; any other source (URL, file id, WebP, bad data) counts as the tier's maximum for the Anthropic rule and 1,600 for Gemini.
- **Conversation memory:** keyed by session + family; holds the real input count, the message count `n`, the SHA-256 of the first `n` messages as compact JSON, and the SHA-256 of `system` plus the tool list. Digests ignore `cache_control` at any depth, because Claude Code moves its cache marker to the newest message every turn (the same reason `ccidentity.digestPrompt` strips them). If a count request has the same `system` and tools digest and its first `n` messages digest the same, the estimate is the stored count plus the ratio estimate of the messages after `n`. Otherwise the whole request is estimated. Only `Observe` refreshes the 1 h idle TTL; a usage count of 0 or less is ignored. Cap 10,000 entries. Claude Code's own `/context` counts carry no `system` and come per component, so they always take the full-estimate path.
- **Real input count** = `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` of the converted Anthropic usage.
- **Feeding:** `tokencount.Counter.Observe(session, model, request, realInput)` (the family is derived from `model`) in `unaryMessage` and `streamMessage` (`server.go:3081`, `:3122`) next to the existing `cloudcode.RequestMetrics`, and in `zenUsageRecorder` (`server.go:1848`). Requests without a session key update the ratio but not the memory.

## 7. Error handling summary

| Situation | Behaviour |
|---|---|
| Pinned account removed, disabled or invalid | Rebind through the strategy; logged. |
| Control-plane call fails or times out | Logged at debug; generation proceeds; a failed burst retries next request. |
| count_tokens upstream unsupported | Estimate; gateway and model marked for 1 h. |
| count_tokens upstream error | Estimate for that request. |
| No session key | No pin, no burst, no per-turn calls, ratio-only estimate. |

## 8. Testing

Unit tests, deterministic (the Manager's `now` func, fake `CloudClient`, `httptest` upstreams):

- **Affinity:** pin creation and reuse; short limit waits on the pin; long limit, quota, failure lock and 404 lock overflow; overflow kept after home recovers; invalid, disabled and removed home rebinds while healthy overflow entries stay; 1 h expiry; eviction above the cap; each strategy honours the exclusion set; no session key keeps today's selection.
- **Control plane:** burst once per (session, account) under concurrent requests; burst again for an overflow account; per-turn calls for a new prompt with tools, none for tool continuations or tool-less requests; failures do not block generation and leave `started` unset; `settle` no longer calls `retrieveUserQuotaSummary`; responses update subscription, project and quota.
- **Wire:** `TestRequestHeadsOnTheWireMatchAgy` extended to the four new calls (header order, body bytes). A sequence test checks that the calls recorded by the fake client follow the 5.2 sequences in order (any order inside a braced group), and that every endpoint of them appears in each of the two generation runs of `.reference/client-drift/agy.jsonl` (`cascadeNuxes` in the later run only); it does not compare order against the capture (5.2).
- **tokencount:** ratio converges on synthetic observations; continuing-conversation estimate; full estimate after a changed prefix; image rules per family; TTL and cap.
- **count_tokens route:** each gateway row of 6.1; 404 → estimate plus 1 h mark; 5xx → estimate without a mark; Claude Code forward matches the 6.2 identity (header order and values compared with `.reference/claude-code-count-tokens-20261107.jsonl`, no injected body keys); a burst of 33 concurrent counts completes without serializing; `TestGateScopeIsTheManagementSurface` and `TestLoggingMiddleware` updated from the 501.

## 9. Verification

- Smoke run with real accounts and a separate config dir (`ANTIGRAVITY_CONFIG_DIR`, per `.trail/learnings/2026-10-20-cc-prev-req-comes-from-the-gustavo.md`): an interactive Claude Code session with tool use, a second concurrent session, and a forced long rate limit. Check pins, overflow and rebind in the logs and `activeSessions` in `/api/accounts`.
- Capture the proxy's Cloud Code traffic through mitmdump (`tools/clientprobe` setup used by `scripts/client_drift.py`) and compare it with `agy.jsonl`. `client_drift.py` `analyze_agy` compares the set of agy's Cloud Code endpoints with the baseline's and raises a manual item, naming added and removed endpoints, only when the sets differ; runs of one agy version reorder calls and vary the count of `retrieveUserQuotaSummary` (5.2).
- JA4 gate (`AGENTS.md`): tshark on a capture of the proxy, including the new GET.
- count_tokens: numbers instead of 501; log each estimate next to the next real input count of the same session to check accuracy.

## 10. Risks

- **Claude Code count_tokens:** the request side, status and response headers are captured (6.2); the success body is inferred from its length (`{"input_tokens":N}`, 19–22 bytes) because the capture addon keeps success bodies out by design. A `/context` burst sends about 34 counts through one pooled account.
- **`client_drift.py`'s Claude Code capture** used a token whose organization refused OAuth (403 `oauth_not_allowed_for_organization`) until `~/.claude-oat` was replaced on 2026-11-07; captures taken before then carry 403s.
- **Added latency:** about six sequential round trips before the first prompt of each conversation on an account, and up to two before each new prompt.
- **More upstream calls per turn** than today; they match agy's per-turn pattern.
- **Pins are lost on restart:** a resumed session may start on another account.

## 11. Docs

- `README.md`: affinity behaviour under "Multi-Account Pool & Selection Strategies"; count_tokens support; the new control-plane calls under "What Matching agy Means".
- `CONTEXT.md`: session pin, overflow pin, start burst, per-turn calls.
- `scripts/client_drift.py`: `Site` entries for any pinned value that moves.
