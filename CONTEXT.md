# Antigravity Claude Proxy

A Go reverse proxy and translation bridge between Anthropic Messages API clients (such as Claude Code CLI) and Google Cloud Code / Gemini backends with multi-account rotation and transparent forwarding.

## Language

### Core Routing

**Translation Route**:
The processing path where Anthropic `/v1/messages` requests are translated to Google Cloud Code payloads and translated back to Anthropic SSE events.
_Avoid_: Emulation, conversion path, Google adapter.

**Transparent Forwarding**:
The direct reverse-proxy path bypassing translation for configured models, sending unmodified Anthropic requests to specified external endpoints.
_Avoid_: Passthrough mode, bypass route, proxy bypass.

**Custom Endpoint**:
A user-configured target host URL and optional API key mapped to specific model names for transparent forwarding.
_Avoid_: Upstream target, external provider, third-party backend.

**Gateway Precedence**:
The configurable order in which the proxy tries each gateway (Kimi, Zen, Claude Code, Codex, OpenRouter, custom endpoints) when more than one would accept the same Requested Model; `cloudcode` marks the terminal account-backed route. Configured by `gatewayOrder.order`, overridden per model by `gatewayOrder.byModel`. The list is a hint: an omitted gateway is tried after the listed ones rather than disabled.
_Avoid_: provider priority, load order, gateway ranking, fallback chain.

### Identity & Account Management

**Account**:
A configured Google identity credential with quota metrics, rate-limit state, and subscription tier used to authenticate with Google Cloud Code.
_Avoid_: User, profile, client, credential pair.

**ChatGPT Account**:
A ChatGPT-subscription OAuth credential (access, refresh and id token, plus the `chatgpt_account_id` claim) in the Codex gateway's pool, with plan type, 5h and weekly usage windows, and cooldown state, used to authenticate with the Codex backend. Stored in `config.json` `codex.accounts[]` and added by the WebUI login, never imported from the codex CLI, because its refresh token is single-use. Distinct from an Account, which is a Google credential for Cloud Code.
_Avoid_: OpenAI account, Codex token, API key.

**Account Source**:
The origin mechanism used to discover or supply an Account's credentials (`local_db`, `oauth`, or `manual`).
_Avoid_: Auth provider, login type, account origin.

**Selection Strategy**:
The algorithm (`sticky`, `round-robin`, or `hybrid`) determining which Account handles a Translation Route request.
_Avoid_: Load balancing mode, routing policy, account picker.

**Account Health**:
A dynamic numerical rating reflecting an Account's operational reliability, rewarded on successful requests and penalized on rate limits or errors.
_Avoid_: Score, reliability index, priority weight.

**Account Cooldown**:
A temporary lockout duration during which an Account is ineligible for selection following rate limit or capacity exhaustion errors.
_Avoid_: Backoff window, mute period, lockout timer.

**Wire Identity**:
The header set and body shape a request presents to Anthropic, rewritten to match the committed Claude Code capture so a foreign harness is not advertised. Distinct from an Account, which is the credential a request authenticates with: a Wire Identity claims what the client is, an Account proves who the user is. Owned by `internal/ccidentity`, applied by `internal/claudecode` on the pooled path and `internal/api` on the custom-endpoint path, and scoped by ADR-0004. The Codex gateway presents a fixed one of its own to the Codex backend (`codex-tui` headers over a Chrome TLS hello), scoped by ADR-0005.
_Avoid_: Spoof, spoofing, normalize, normalization, identity (bare — it collides with Account).

**Start Burst**:
The control-plane calls sent the first time this proxy uses an Account, with or without a client session: `loadCodeAssist`; `fetchAdminControls`, `fetchUserInfo` and `retrieveUserQuotaSummary` together; `fetchAvailableModels`; `retrieveUserQuotaSummary`; `listExperiments`; `loadCodeAssist`; `GET cascadeNuxes`. Concurrent requests share one burst. Any HTTP answer counts as answered; an unanswered call leaves the Account unstarted and its next request retries the burst. Later sessions on the same Account do not repeat it. Its model list refreshes per-model quota, and its summary and user-info calls restart the recurring refresh timers. Based on the agy 1.5.6 cadence capture; endpoint order and repetition vary between real-client startup captures.
_Avoid_: Warmup, handshake, init calls.

**Recurring Control-Plane Calls**:
The calls sent beside generation while an Account is in use: `retrieveUserQuotaSummary` once its last summary is at least one minute old, and `fetchUserInfo` → `fetchAvailableModels` → `loadCodeAssist` at a conversation's first model call once its last user-info call is at least five minutes old. The latter does not require a title checkpoint. Summaries update shared quota pools; model lists update per-model quota; `loadCodeAssist` updates tier and project. Summaries from explicit catalog and account refreshes restart the same timer. Recurring calls do not delay generation and are bounded by their own timeouts and dispatcher shutdown.
_Avoid_: Per-turn calls, pre-flight, post-request refresh.

**Session Pin**:
The binding of one client session to the Account that serves all of its models, so a conversation keeps one cascade and one prompt cache. Made by the Selection Strategy on the session's first request; dropped after an hour without a request.
_Avoid_: Sticky session (collides with the `sticky` strategy), affinity entry, session lock.

**Overflow Pin**:
A per-model exception to a Session Pin: the Account one model of the session moved to when its pinned Account hit a long rate limit, a second short one with no success in between, exhausted quota or a failure lock. It stays until its own Account becomes unusable, even after the pinned Account recovers.
_Avoid_: Fallback account, spillover, failover pin.

### Models & Catalog

**Requested Model**:
The raw model name string provided in client API requests.
_Avoid_: Incoming model, user model, client string.

**Model Mapping**:
A configuration rule that rewrites a Requested Model to a target model name prior to routing or translation.
_Avoid_: Model alias, model override, model replacement.

**Upstream ID**:
The authoritative model identifier recognized by the Google Cloud Code backend.
_Avoid_: Internal model name, backend ID, Google model tag.

**Selectable Model**:
A model entry actively exposed and supported in the live Cloud Code catalog.
_Avoid_: Available model, active model, supported model.

**Alias Set**:
The collection of alternative names a client may use for one Selectable Model, all resolving to a single Upstream ID.
_Avoid_: Model mapping, nickname, shorthand.

### Format & Reasoning Adaptation

**Thinking Adaptation**:
The normalization of reasoning token budgets and synthesis/validation of thought signatures for reasoning-capable models. On the Codex gateway the thinking block's `signature` carries the backend's encrypted reasoning (`reasoning.encrypted_content`), which returns upstream as a `reasoning` input item on the next turn; the conversation itself carries it, so no cache is involved.
_Avoid_: Thought parsing, reasoning hook, budget patch.

**Thought Signature Cache**:
An in-memory store mapping generated thought blocks to validation signatures required by Gemini thinking models across conversational turns.
_Avoid_: Signature map, thought storage, reasoning cache.

**Stream Translation**:
The real-time transformation of upstream SSE event streams into compliant Anthropic SSE format events.
_Avoid_: Stream conversion, event mapping, chunk forwarding.

### Error & Quota State

**Rate Limit Exceeded**:
A temporary upstream rejection (HTTP 429), with or without retry hints, that imposes an Account/model eligibility delay. Without a hint, the escalating cooldown is inferred policy, not a measured Google reset or evidence of daily quota exhaustion. Hidden recovery is limited to one cumulative 10-second window (or a smaller positive `maxWaitBeforeErrorMs`), starting at the first retryable failure or blocked selection; it ends at the first forwarded upstream event and never caps an established stream. Terminal retry advice reflects the actual applicable cooldown, including escalation and additive jitter.
_Avoid_: Throttling, 429 error, rate block.

**Quota Exhausted**:
A depletion of model quota capacity (`remainingFraction` below threshold) that suspends an Account for that model until the quota reset timestamp. Per-model fractions come from the catalog fetch (24 h TTL), a manual account refresh, each Start Burst, and the five-minute account refresh, and stay near 1 until the model is exhausted.
_Avoid_: Usage limit, out of credits, quota burn.

**Model Capacity Exhausted**:
An upstream infrastructure overload condition (HTTP 503/529) that triggers exponential backoff and Account rotation.
_Avoid_: Server overload, Google capacity issue, backend down.

### Context Compression & Shaping (Headroom)

**Headroom Engine**:
The pre-dispatch request optimization pipeline applying deterministic compression, whitespace pruning, and output shaping.
_Avoid_: Context trimmer, optimizer middleware, token stripper.

**SmartCrusher**:
The deterministic JSON compactor that strips insignificant whitespace from tool results while preserving key order and numeric precision.
_Avoid_: JSON minifier, schema stripper.

**CodeCompressor**:
The deterministic whitespace and repetition pruning stage that collapses excessive newlines, trims line endings, and folds duplicate log runs.
_Avoid_: Code cleaner, log summarizer.

**OutputShaper**:
The stage that appends verbosity steering instructions to the system prompt and limits thinking budgets on mechanical continuation turns.
_Avoid_: Prompt injector, reasoning clamper.

### Reasoning

**Effort Level**:
One of `low`, `medium`, `high`, `xhigh`, `max` — Anthropic's `output_config.effort` vocabulary, which `agy --effort` also accepts. A model that does not publish a level runs the highest published level at or below it.
_Avoid_: Reasoning mode, thinking intensity, think level.

**Ambient Effort**:
The `output_config.effort` Claude Code sends on every request. It has the lowest precedence: it picks a tier for a bare family ID, and sets a budget for a budget-style model whose ID names no tier, but never overrides a tier named in the model ID (neither its route nor its catalog budget), an explicit `reasoning_effort`, or an explicit thinking budget.
_Avoid_: Default effort, client effort.

**Budget-Style Model**:
A Cloud Code route whose thinking is set by a token budget (`thinking_budget`) rather than a tier (`thinking_level`): Claude 4.6, Gemini 3.1 Pro, GPT-OSS, and the per-tier IDs upstream publishes directly (`gemini-3.8-flash-low`, `gemini-3.1-pro-high`), each of which carries its own catalog budget. The proxy turns an Effort Level into a budget unless the model ID already names a tier; only synthetic Gemini flash entries backed by a `-tiered` upstream entry get a `thinking_level` instead.
_Avoid_: Legacy thinking, budget thinking.

### Cache Management

**Cache Bump**:
The scheduler that replays a minimal version of a recorded session's last request shortly before its prompt cache entry expires, refreshing the entry at read pricing so an idle session returns to a warm cache. Bumping stops the moment it pays a cache write instead of a read.
_Avoid_: Cache warmer, cache keeper, TTL refresher, cache preheating.

### Security Monitor (Classifier)

**Interception Rule**:
A declared condition set matching autonomous Claude Code security-monitor prompts by system regex, footer marker, model, or max_tokens bounds, executing in declaration order before built-in fallbacks.
_Avoid_: Filter, classifier hook, security policy.

**Target Backend**:
A named external destination endpoint (`anthropic`, `openai`, `laya` or `jev` wire format) to which matched classifier requests are rerouted with format translation and optional synthetic streaming.
_Avoid_: Fallback host, reroute target, classifier upstream.

**Audit Event**:
A recorded decision entry (`rerouted`, `stubbed`, `passthrough`, or `error`) emitted by the classifier subsystem with a monotonic sequence identifier for live UI feed consumption.
_Avoid_: Classifier log, monitor trace, interception metric.


