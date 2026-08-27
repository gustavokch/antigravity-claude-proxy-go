# OpenRouter Provider Routing — Refined Plan

Refinement of `/Users/gus/.claude/plans/lets-write-a-plan-bright-pizza.md`, verified against the codebase. User decisions this round: failover window = pre-headers (status classification only, no SSE buffering); router stats persisted to disk.

## Context

Models on OpenRouter are served by multiple upstream providers. Unpinned requests load-balance per-request, so a CC session silently switches providers mid-conversation: inconsistent speed/quality, prompt-cache misses, frequent errors. `forwardToOpenRouter` (internal/api/server.go:627-728) is a single-attempt `httputil.ReverseProxy` pass with zero provider control and no retry.

Goal: pin each model to its best-ranked provider, sticky per CC session, deterministic failover, per-model provider control in the WebUI.

Verified API surface (probed live earlier):
- Pin: request body `"provider": {"order": ["<provider_name>"], "allow_fallbacks": false}`; also `provider.sort`: `price|throughput|latency`.
- Stats: `GET <base>/v1/models/{author}/{slug}/endpoints` → per-endpoint `provider_name`, `tag`, `context_length`, `max_completion_tokens`, `pricing`, `supported_parameters`, `status`, `uptime_last_5m/30m/1d`. `latency_last_30m`/`throughput_last_30m` usually null.
- Served provider: top-level `provider` field in non-stream JSON and SSE events.

## Prior user-approved decisions (unchanged)

1. **Retry split by class.** 400-class (400/404/413/422): immediate failover, no same-provider retry. 429: exponential backoff same provider (base 500ms, cap 2min, max 10 attempts), then failover. 5xx/network: short backoff then failover. Whole chain bounded by per-request budget (default 2min).
2. **Ranking hybrid.** Uptime (5m×.5/30m×.3/1d×.2) + context_length from endpoints API; latency/TPS = local EWMA from proxied traffic; API values as priors only; cold start = neutral mid-score.
3. **Stickiness until refresh if alive.** Sticky per session+model; catalog refresh keeps sticky provider if still ranked, drops if gone; consecutive-failure threshold (default 10) breaks stickiness immediately.
4. **GUI full.** Per-model routing panel: ranked providers with live stats, mode selector (auto/pinned/custom-order), ordering controls, persisted in allowlist config.

## New decisions (this round)

5. **Failover window = pre-headers.** Classify outcome from HTTP status before writing anything to the client. No first-SSE-event buffering (TTFT cost rejected). Consequence: a provider that 200s then dies mid-stream yields a truncated stream to the client — same as current behavior. **This constraint is load-bearing:** `SSEInterceptor.finalize` (observability.go:290-307) fires its metrics callback on any close/error, so any mid-stream retry would double-count metrics and session cost. Failover must stay pre-first-byte.
6. **Router stats persisted to disk.** EWMA latency/TPS, consecutive-fail counters, and sticky assignments survive restarts via a JSON file. Format is internal (no migration contract yet); versioned with a `version` field from day one.

## Verified codebase facts (exploration this round)

- **Session ID**: `openrouter.ExtractSessionID(request, anthropicRequest)` (internal/openrouter/session.go:136-170) already exists and is already called at server.go:636. Headers (`x-session-id` etc.) → `metadata.session_id`/`metadata.user_id` (CC sends user_id) → RemoteAddr+UA hash fallback. Reuse directly for stickiness.
- **Body re-readability**: `reqBody []byte` is pre-marshaled at server.go:536-541 and reused per attempt (server.go:649). Manual loop rebuilds body trivially.
- **Headers**: Director forwards client `anthropic-version`/`anthropic-beta` and overwrites `Authorization`/`x-api-key` with the OpenRouter key (server.go:651-662). Manual loop must clone headers per attempt and replicate overwrite ordering.
- **No existing retry** on the OpenRouter path; `MaxRetries` config is Google-side only.
- **No generic authenticated GET helper** in client.go; `FetchAvailableModels` hardcodes `/v1/models` with the auth pattern at client.go:105-108. New endpoints fetch replicates this plus the singleflight + detached-context pattern from `ResolveModelPricing` (client.go:200-245).
- **Catalog cache in-memory only**, 1h TTL (`DefaultClient`, client.go:67). Refresh triggers: cache expiry, POST `/api/openrouter/config` (management.go:1083-1085), POST `/api/openrouter/models/fetch`. Endpoints cache hooks into the same triggers.
- **Config save semantics**: POST `/api/openrouter/config` replaces the whole `openrouter` object (config.go:232-252), preserving apiKey via `hasApiKey`. New per-model and routing fields ride through the existing endpoint and `saveOpenRouterConfig()` (models.js:341-381) once added to the Go structs. JSON tags are camelCase.
- **Cost chokepoint**: pricing resolved at server.go:637, `finalPricing` at server.go:669, `ComputeFinalMetrics` at server.go:684/712. Per-endpoint `Pricing` (same struct, pricing.go:11-18 — no type change) substitutes after provider selection.
- **WebUI location**: OpenRouter UI is in Settings > Models tab — `internal/webui/public/views/settings.html:942-1070` (allowlist table 1015-1067), NOT models.html. Expand pattern to copy: Set-based `expandedModels` idiom (models.js:36-50) with `this.expandedModels = new Set(...)` reassignment; detail row as second `<tr x-show>` with colspan. No per-model stats endpoint exists today; `GET /api/stats/history` (management.go:147) exists but unused by JS.
- **`responseWriterRecorder`** (server.go:133-157) wraps the writer, implements Flush. Error status can be written any time before first byte.

## Architecture

New state machine in `internal/openrouter`; `forwardToOpenRouter` becomes an attempt loop with manual `http.Client.Do` (ReverseProxy removed for this path).

### New files

**`internal/openrouter/endpoints.go`**
- `ProviderEndpoint`: provider_name, tag, context_length, max_completion_tokens, uptime fields, optional latency/throughput priors, `Pricing` (existing type).
- `FetchModelEndpoints(ctx, modelID, apiKey, baseURL)` — GET `{author}/{slug}/endpoints`; slug from model ID, `CanonicalSlug` fallback via `matchModel`. Reuses singleflight + detached-context pattern from `ResolveModelPricing`.
- In-memory cache per model, same 1h TTL as catalog; refreshed by the same triggers (cache expiry, config save hook, models/fetch). Warmup fetches endpoints for enabled allowlist models.

**`internal/openrouter/router.go`** — `ProviderRouter` (mutex-guarded):
- `ranks map[model][]ranked{endpoint, score}` + `rankedAt`. Score = weighted normalize: availability (uptime blend, unhealthy status = hard penalty) + context_length + local EWMA latency (lower better) + local EWMA TPS (higher better) + API priors when local empty. Weights in config.
- `sticky map[session+model]provider`, `consecFails map[model|provider]int`, EWMA stats per model|provider (successes, failures, latency ms, tps).
- `Select(session, model)` — pinned/custom config order wins; else sticky if alive & under threshold; else rank[0]; records sticky.
- `RecordResult(model, provider, ok, latencyMs, tps)` — success resets counter + updates EWMAs; failure increments; threshold clears stickiness for all sessions on that pair.
- Refresh hook: drop rank entries whose providers vanished; preserve the rest.
- **Persistence**: `Load(path)` / `Save(path)` — versioned JSON (`{version, sticky, consecFails, ewma}`) written atomically (tmp+rename, matching config.go save style) on a debounced schedule (e.g. after RecordResult, max once per 30s) and on shutdown. Path next to config dir. Ranks are NOT persisted (rebuilt from endpoints fetch). Package-level `DefaultRouter` next to `DefaultClient` (client.go:67).

### Reworked `forwardToOpenRouter` (server.go:627-728)

Manual attempt loop:
1. `provider := DefaultRouter.Select(sessionID, model)` (sessionID from existing `ExtractSessionID` call).
2. Inject `"provider": {"order": [...], "allow_fallbacks": false}` into the outgoing body per attempt (pinned → single entry; custom → full order; auto → selected provider only). Body re-marshaled per attempt since the order can change on failover.
3. Per attempt: fresh `http.Request` with `request.Context()`, cloned headers (replicate auth-overwrite ordering at server.go:651-662), `bytes.NewReader(reqBody)`. Check `ctx.Err()` between attempts — client disconnect aborts retry.
4. Classify from status **before writing any byte to the client**:
   - network error / 5xx → short backoff, next provider.
   - 429 → exponential backoff (base 500ms, cap 2min) same provider, max 10, then failover.
   - 400-class → immediate next provider.
   - Every attempt checks request budget (default 2min); exhausted → return last error via `writeAPIError` (existing helper, used at server.go:723).
5. On 200: port the ModifyResponse branch (server.go:664-720) — SSE → `NewSSEInterceptor` wrap; unary → buffer + `ParseUsageFromJSON`. Capture `provider` from response/SSE (extend parsing), compute metrics with **per-endpoint pricing** from the rank entry (fallback `resolveEffectivePricing`, server.go:730-737), `RecordResult` with the real served provider.
6. Failed attempts now logged (currently upstream 4xx/5xx pass through with zero metrics — strict improvement).

### Observability (`internal/openrouter/observability.go`)
- Extend `ParseUsageFromSSELine` / `ParseUsageFromJSON` to extract top-level `provider`; add `Provider` to `RequestMetrics` and the log line in `LogObservability`.
- Feed latency/tokens/outcome into `DefaultRouter.RecordResult`.

### Config (`internal/config/config.go`)
```go
// OpenRouterConfig addition (camelCase tag, json:"routing,omitempty")
Routing OpenRouterRoutingConfig // strategy, failureThreshold(10), retry429Max(10),
                                // backoffBaseMs(500), backoffCapMs(120000), requestBudgetMs(120000),
                                // rankWeights{availability,context,latency,tps}
// OpenRouterModelConfig additions
ProviderMode   string   `json:"providerMode,omitempty"`   // "auto" | "pinned" | "custom"
PinnedProvider string   `json:"pinnedProvider,omitempty"`
ProviderOrder  []string `json:"providerOrder,omitempty"`
```
Defaults filled in `DefaultConfig()` (config.go:94-98). No validation exists for openrouter config today; add minimal sane-default clamping in the router, not a new validation layer.

### Management API (`internal/api/management.go`)
- New route in the switch at management.go:156-167: `GET /api/openrouter/providers?model=<id>` → ranked endpoints + live EWMA stats from `DefaultRouter` (powers GUI). Follow `handleOpenRouter<Thing><Verb>` naming, `writeJSON` response style.
- Routing config flows through existing `/api/openrouter/config` GET/POST unchanged (whole-object replace covers it).
- Post-save hook (management.go:1083-1085) also triggers endpoints-cache warmup for allowlisted models.

### WebUI (`internal/webui/public/views/settings.html` + `js/components/models.js`)
- Per-model expandable routing row inside the allowlist table (settings.html:1015-1067): second `<tr x-show="isRoutingExpanded(item.id)">` with colspan cell; expand state follows the Set idiom at models.js:36-50.
- Panel contents: mode selector (auto/pinned/custom), provider rows (uptime, context, local TPS/latency EWMA, per-endpoint cost), up/down reorder buttons in custom mode.
- Data: `GET /api/openrouter/providers?model=<id>` on expand, via `window.utils.request` (utils.js:7).
- Save: mutate `openRouterConfig.allowlist` item → existing `saveOpenRouterConfig()` (new fields ride through).
- i18n keys via `$store.global.t('key') || 'fallback'` per convention.

## Testing

- Unit (table-driven, matching `client_test.go`/`session_test.go`/`pricing_test.go` style): endpoints fetch/parse/canonical-slug fallback; ranker scoring/normalization/cold-start/priors; Select (pin, custom order, sticky, threshold break, vanished-provider reset); RecordResult EWMA math; persistence round-trip (save/load, version field, corrupt file → clean start); retry classifier matrix; backoff bounds and budget enforcement.
- Integration (`httptest` fake OpenRouter, alongside `internal/api/openrouter_proxy_test.go`): first provider 429×2 then succeeds (backoff + same-provider stick); 400 → immediate failover; 10 consecutive failures → stickiness moves; provider injection asserted in request body; streaming byte-passthrough with usage + provider interception; client-disconnect mid-retry aborts loop.
- Manual: real key, CC session through proxy, confirm log lines show stable provider across turns; kill stickiness via threshold; WebUI panel round-trip; restart proxy → stats/stickiness survive.

## Implementation phases

1. `endpoints.go` + tests (fetch/parse/cache/warmup hook).
2. `router.go` + tests (rank/select/record/EWMA) + disk persistence.
3. `forwardToOpenRouter` attempt-loop rework + provider injection + retry/failover + tests.
4. Provider capture in observability + stats feedback loop.
5. Config fields + management providers endpoint + tests.
6. WebUI routing panel in settings.html.

## Notes

- PRs target fork `gustavokch/antigravity-claude-proxy-go`.
- Cloud Code (Google) path untouched — no JA4/fingerprint impact.
- Non-stream and stream share the loop; only body forwarding differs. Failover pre-headers only; mid-stream death = truncated stream (status quo), never retried (metrics double-count via SSEInterceptor.finalize).
