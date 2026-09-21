# OpenCode Zen Gateway — Implementation Plan

## Context

OpenCode Zen (`https://opencode.ai/zen`) is a curated AI gateway with
per-request billing. The proxy gains a Zen gateway so Anthropic-compatible
clients (Claude Code, Hermes) can reach Zen models through the existing
`POST /v1/messages` path with zero payload translation.

Live Zen contract (verified 2026-09-20 from `https://opencode.ai/docs/zen/`
and from the live `https://opencode.ai/zen/v1/models` endpoint):

- Anthropic-compatible chat: `POST https://opencode.ai/zen/v1/messages`
  (Claude models, Qwen Anthropic variants). This is the only Phase 1 target.
- Other wire formats are explicitly out of Phase 1:
  `POST /zen/v1/responses` (GPT, Grok, Muse),
  `POST /zen/v1/chat/completions` (DeepSeek, MiniMax, GLM, Kimi, Big Pickle),
  `POST /zen/v1/models/<gemini-id>` (Gemini-native),
  `POST /zen/v1/systemone` (Jev `state`/`questions`, non-chat structured decisions).
- Auth: `Authorization: Bearer $OPENCODE_API_KEY`.
- Catalog: `GET https://opencode.ai/zen/v1/models` — **shape confirmed live
  2026-09-20, no spike needed.** The endpoint is **unauthenticated** (HTTP 200
  with no `Authorization` header) and returns an OpenAI-style list:

  ```json
  {"object":"list","data":[{"id":"claude-sonnet-4-6","object":"model",
    "created":1789950557,"owned_by":"opencode"}]}
  ```

  74 entries. There is **no context length, no max output tokens, no pricing,
  and no wire-format field** — `id` is the only usable datum. Two consequences
  drive the design below: a `GetModelLimits` catalog lookup can never succeed,
  and the catalog cannot tell an Anthropic-wire model from a Responses-wire one.
- Anthropic-wire subset (the only ids Phase 1 may forward), from the docs
  endpoint table cross-checked against the live catalog:
  `claude-fable-5-1`, `claude-fable-5`, `claude-opus-5`, `claude-opus-4-8`,
  `claude-opus-4-7`, `claude-opus-4-6`, `claude-opus-4-5`, `claude-sonnet-5`,
  `claude-sonnet-4-6`, `claude-sonnet-4-5`, `claude-sonnet-4`,
  `claude-haiku-4-5`, `qwen3.8-flash`, `qwen3.6-plus`, `qwen3.5-plus`.
  (`qwen3.7-max` and `qwen3.7-plus` appear in the docs table but not in the
  live catalog; treat the live catalog as authoritative.) Everything else in
  the catalog (`gpt-*`, `gemini-*`, `grok-*`, `deepseek-*`, `glm-*`,
  `minimax-*`, `kimi-*`, `muse-*`, `jev-*`, `big-pickle`, `*-free`) is a
  different wire format and must never reach `/zen/v1/messages`.
- OpenCode config id format is `opencode/<model-id>`; the proxy allowlist
  stores the raw Zen id and tolerates the `opencode/` prefix on match.
- No key-generation logic exists anywhere (upstream repo is consumer-side:
  point user at `https://opencode.ai/zen`, store key via client auth state,
  send as Bearer). This gateway does the same.

This plan mirrors the Kimi gateway (`internal/kimi/`, `forwardToKimi`,
`matchKimiModelEntry`), not the OpenRouter provider-router. Zen is a
single-key Anthropic forwarder with an allowlist.

Decisions locked with operator:

1. Phase 1 is messages-only transparent forward (Kimi pattern).
2. Model selection is allowlist + alias (same as Kimi/OpenRouter).
3. Key handling is `OPENCODE_API_KEY` env fallback + `config.json zen.apiKey`
   with `hasApiKey` redaction (same as Kimi, plus the env fallback — see the
   open item on `keySource` in §2).
4. `max_tokens` falls back to a package default, not a 400. The catalog
   carries no output limit, so precedence is: client value → allowlist entry
   `maxOutputTokens` → `zen.DefaultMaxOutputTokens`. The request never fails
   for a missing `max_tokens`. See §1 for the constant and §4 for the
   fill-not-clamp rule.

---

## Routing Architecture

```
POST /v1/messages { model }
  -> ModelMapping (<=5 hops)
  -> Kimi allowlist?
  -> Zen allowlist?            <-- NEW, immediately after Kimi
  -> ClaudeCode allowlist?
  -> OpenRouter allowlist?
  -> CustomEndpoints?
  -> CloudCode translation (account pool)
```
(The arrows above are the default order. Gateway precedence is now
configurable via `gatewayOrder.order` / `gatewayOrder.byModel`; see the
configurable-gateway-order plan.)

Zen is checked before ClaudeCode/OpenRouter/CloudCode so an explicit Zen
allowlist entry wins on ID collision (`claude-sonnet-4-6`, `claude-sonnet-4-5`,
`claude-opus-4-5` and `claude-haiku-4-5` all exist in both Zen and CloudCode).
First match wins; the forward is logged with the model.
Zen traffic never consumes Google account capacity, so the classifier
fallback gate (`ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK`) never stubs or
fast-fails it — same exemption as Kimi/OpenRouter today. This is automatic,
not a code change: the gate is read at `server.go:1039`, after every
alternate-backend branch (Kimi `971`, ClaudeCode `985`, OpenRouter `1001`,
custom endpoints `1019`) has already returned.

TLS rule from `AGENTS.md` applies unchanged: standard Go transport, empty
`tls.Config{}`, no `utls`/cipher/curve/ALPN overrides. Zen needs no
fingerprint work (plain HTTPS client, unlike the CloudCode path).

---

## Technical Specification

### 1. New package `internal/zen/` (clone of `internal/kimi/`)

- `zen.go`
  - `const DefaultBaseURL = "https://opencode.ai/zen"`.
  - `const DefaultMaxOutputTokens = 32768` — the value used when neither the
    client nor the allowlist entry states a cap. It must be a value every id
    in the Anthropic-wire subset accepts, so it cannot be
    `defaultDiscoveryMaxOutputTokens` (200000, `server.go:412`): that is a
    discovery-advertisement number, and sending it as a real `max_tokens`
    would make Zen reject the request. 32768 is under the published output
    cap of every Claude and Qwen model in the subset. Document it as a floor
    to raise per model via the allowlist entry, not as a model capability.
  - `NormalizeBaseURL(raw string) string`: trim space, default when empty,
    trim trailing `/`, strip trailing `/v1` (endpoint paths append
    `/v1/messages` at call time).
- `client.go`
  - `ModelItem{ID string}` only — the catalog carries nothing else. Do **not**
    add `ContextLen`/`MaxOutputTokens`/`DisplayName` fields that can never be
    populated.
  - `Client` with `FetchModels(ctx, apiKey, baseURL)`, `GetCachedModels()`,
    `IsCacheValid()`; 5m TTL; `var DefaultClient`. `apiKey` is accepted for
    signature symmetry with Kimi and sent when non-empty, but the endpoint
    answers unauthenticated, so `FetchModels` must work with an empty key —
    the WebUI can populate its picker before the operator pastes a key.
  - **No `GetModelLimits`.** The catalog has no limits, so the lookup would be
    dead code that always returns `ok=false`.
  - `AnthropicWireIDs` / `IsAnthropicWire(id string) bool`: the static
    allowlist of the 15 ids in the Context section, matched after stripping
    `opencode/` and lowercasing. `FetchModels` returns the full catalog;
    callers filter. This is the only defence against an operator allowlisting
    `gpt-5.5` and getting an opaque upstream error.
- `passthrough.go`
  - `ForwardMessages(w, r, baseURL, apiKey, body)`,
    `ForwardMessagesWithHook`, `ForwardMessagesWithModify(w, r, baseURL,
    apiKey, body, modify)` via `httputil.ReverseProxy`, `FlushInterval: -1`.
  - Director: target `NormalizeBaseURL(baseURL)+"/v1/messages"`; set
    `Authorization: Bearer <key>`, delete `x-api-key`; forward
    `anthropic-version` (client value or default `2023-06-01`) and
    `anthropic-beta` when present.
  - `ErrorHandler`: 502 `api_error` with structured body (no import cycle —
    local `writeAPIError` helper like Kimi's).
- `observability.go`
  - `RequestMetrics{Model, SessionID, Input/Output/CacheRead/CacheCreation,
    Latency}`, `ComputeFinalMetrics`, `LogObservability` — a straight clone of
    `internal/kimi/observability.go`. Tokens/latency only in Phase 1; Zen
    publishes prices as a human-readable docs table with no machine-readable
    endpoint, so cost stays zero/unknown rather than invented.
  - The package imports **no other internal package** (`internal/kimi` imports
    none today; keep that property). SSE usage extraction therefore lives in
    `internal/api`, not here: `zenInstrumentResponse` in `server.go` is the
    clone of `kimiInstrumentResponse` (`server.go:1371`) and is what calls
    `openrouter.NewSSEInterceptor`.

### 2. Config (`internal/config/config.go`)

```go
type ZenModelConfig struct {
    ID              string `json:"id"`
    Alias           string `json:"alias,omitempty"`
    DisplayName     string `json:"displayName,omitempty"`
    ContextLen      int    `json:"contextLength,omitempty"`
    MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
    Enabled         bool   `json:"enabled"`
}
type ZenConfig struct {
    Enabled   bool             `json:"enabled"`
    BaseURL   string           `json:"baseUrl"`
    APIKey    string           `json:"apiKey,omitempty"`
    Allowlist []ZenModelConfig `json:"allowlist,omitempty"`
}
// Config struct: add Zen ZenConfig `json:"zen,omitempty"`
```

- `DefaultConfig()` (`config.go:359` area, next to the `Kimi` literal):
  `Zen{BaseURL: zen.DefaultBaseURL}` (disabled, empty allowlist). The import
  is safe — `internal/zen` will import no internal package, so there is no
  cycle. Note the existing `Kimi` literal hardcodes its URL string instead;
  the constant is the better pattern, the asymmetry is intentional.
- `Save()`: add a `zen` branch mirroring the `kimi` branch (`config.go:602-621`)
  — when incoming has `hasApiKey:true` and empty `apiKey`, keep the persisted
  key; strip `hasApiKey` before write.
- `GetPublicConfig()`: add a `zen` branch mirroring Kimi (`config.go:802-813`)
  — drop `apiKey`, emit `hasApiKey:true` when set.
- Key resolution (at forward time, not in the config package):
  `key = cfg.Zen.APIKey; if key == "" { key = os.Getenv("OPENCODE_API_KEY") }`.
  Empty after fallback → 400 `invalid_request_error`, never send `"public"`.
  (Upstream `opencode.ts` falls back to `"public"`; the proxy must not —
  that would bill to a shared key and leak requests.)
  **This env fallback is a new pattern for this repo**: no other provider key
  reads an env var (`config.go` only calls `os.Getenv` for
  `ANTIGRAVITY_CONFIG_DIR`, `CONFIG_DIR` and the classifier gate). The
  consequence is a WebUI lie — `hasApiKey` is computed from `cfg.Zen.APIKey`
  alone, so the panel shows "no key" while forwards succeed from the env.
  Fix in `handleZenConfigGet` (§5): report `hasApiKey:true` when either
  source is set, plus `keySource:"config"|"env"|"none"` so the panel can say
  which. Never echo the env value itself.
- `CacheBumpConfig`: add `Routes.Zen bool` (`CacheBumpRoutesConfig`,
  `config.go:265`) + a `case "zen"` in `EnabledFor` (`config.go:305`);
  default off (today only `claudecode:true` defaults on). Add
  `cachebump.RouteZen` constant alongside `RouteKimi`/`RouteCustom`
  (`internal/cachebump/record.go:22`).

### 3. Cache-bump replay (`internal/api/cachebump_server.go`)

Recording a bump under a route the replay dispatcher does not know silently
fails: `cacheBumpSender` (`cachebump_server.go:167-179`) falls through its
switch and returns `cachebump.ErrAccountUnavailable`. So `RouteZen` is not
config-only work:

- Add `case cachebump.RouteZen: return server.sendZenBump(ctx, rec)` to the
  switch.
- `sendZenBump`: clone `sendKimiBump` (`238-246`) — read `config.Get().Zen`,
  bail when `BaseURL` is empty, `postBumpRequest` to
  `zen.NormalizeBaseURL(cfg.BaseURL)+"/v1/messages"` with
  `Authorization: Bearer <key>` resolved through the same env fallback as the
  forward path (factor that resolution into one `zenAPIKey(cfg)` helper so the
  two call sites cannot drift).
- Replay floor is `minMaxTokensFloor` (16), same as Kimi, not the Claude Code
  floor of 1.

### 4. Router (`internal/api/server.go`)

> **Historical note (superseded):** this section instructed inserting a Zen
> branch between the Kimi and ClaudeCode blocks. PR #88 executed it, and the
> configurable-gateway-order change later replaced the whole if-chain with a
> data-ordered walk (`DefaultGatewayOrder()` + `gatewayHandlers` in
> `internal/api/dispatch.go`). The branch below is preserved as the record of
> what was inserted, not as a current editing instruction.

- `messages()`: insert the Zen branch between the Kimi block (ends
  `server.go:984`) and the ClaudeCode block (`server.go:985`):
  ```go
  if cfg.Zen.Enabled {
      if zenEntry, ok := matchZenModelEntry(cfg.Zen, model); ok {
          target := zenTargetModel(zenEntry) // raw ID, opencode/ prefix stripped
          anthropicRequest["model"] = target
          reqBody, err := json.Marshal(anthropicRequest)
          if err != nil {
              writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
                  "Failed to marshal Zen request: "+err.Error())
              return
          }
          server.forwardToZen(writer, request, cfg.Zen, reqBody, anthropicRequest, target, zenEntry)
          return
      }
  }
  ```
  Unlike the Kimi branch, the `max_tokens` work does **not** happen here: the
  fill step needs `zenEntry` and re-marshals the body, and it sits next to the
  key check and the wire-format guard that already 400 from inside
  `forwardToZen`. Keeping all body mutation in one function stops the branch
  and the forwarder from each owning half the request — hence the extra
  `anthropicRequest` and `zenEntry` arguments above.
- `matchZenModelEntry(cfg ZenConfig, model string) (ZenModelConfig, bool)`:
  clone `matchKimiModelEntry` (`server.go:1538-1558`) minus the `[1m]` suffix
  logic; case-insensitive ID/alias compare over enabled entries; strip a
  leading `opencode/` (case-insensitive) from both sides before compare so
  `opencode/claude-sonnet-4-6` matches allowlist id `claude-sonnet-4-6`.
  Do **not** clone `matchKimiModel` (`server.go:1526`) — it has no non-test
  caller and is dead code.
- `forwardToZen()`: clone `forwardToKimi` (`server.go:1285-1369`):
  - Key resolution via `zenAPIKey(cfg)` (config, then `OPENCODE_API_KEY`).
    Empty → 400, no forward.
  - Wire-format guard: `!zen.IsAnthropicWire(target)` → 400
    `invalid_request_error` naming the model and pointing at the Anthropic
    subset. Without this an operator who allowlists `gpt-5.5` gets whatever
    `/zen/v1/messages` returns for a Responses-wire model, which is not a
    diagnosable error.
  - `max_tokens` policy. The catalog has no limits, so the derived value is
    always 0 and the sources are the client value, `zenEntry.MaxOutputTokens`,
    and `zen.DefaultMaxOutputTokens`. **Fill, do not clamp:**
    ```go
    if _, present := anthropicRequest["max_tokens"]; !present {
        fill := zenEntry.MaxOutputTokens
        if fill <= 0 {
            fill = zen.DefaultMaxOutputTokens
        }
        anthropicRequest["max_tokens"] = fill
        if reqBody, err = json.Marshal(anthropicRequest); err != nil { /* 400 */ }
    }
    reqBody = applyMaxTokensPolicy(reqBody, anthropicRequest, zenEntry.MaxOutputTokens, 0)
    ```
    The default must **not** be passed as `applyMaxTokensPolicy`'s
    `derivedLimit`: that parameter clamps a *present* client value down
    (`server.go:1489-1491`), so a client asking for 64000 would be silently
    cut to 32768. Passing `derivedLimit: 0` leaves an explicit client value
    untouched while the operator's `zenEntry.MaxOutputTokens` still caps it,
    which is the Kimi behaviour (`server.go:980`). No `max_tokens` 400 exists
    on this route.
  - CCR-disabled path: `zen.ForwardMessagesWithModify` with a `modify` that
    records the cache bump (`cachebump.RouteZen`) and calls
    `zenInstrumentResponse`.
  - CCR-enabled path: `sender` closure posting to
    `zen.NormalizeBaseURL(base)+"/v1/messages"` with Bearer +
    version/beta headers; `defaultCCROptions(sender)` +
    `ProxyAnthropicStreamWithCCR` / `ProxyAnthropicJSONWithCCR`; `OnUsage`
    computes metrics + `tracker.TrackRequest`.
- `models()`: append enabled Zen entries + aliases with `owned_by:"zen"`,
  using the **Kimi** fallback shape (`server.go:576-624`), not the OpenRouter
  one (`509-574`). OpenRouter's block exists to fold live catalog limits in;
  the Zen catalog has none, so there is nothing to warm up and no
  `WarmupCacheAsync` call belongs in `models()` or in `NewServer`
  (`server.go:172-178`). Limits come from the allowlist entry or the
  `defaultDiscoveryContextWindow`/`defaultDiscoveryMaxOutputTokens` fallbacks.
- `usage()`/`handleAccountLimits`: Zen carries no account quota; no per-model
  quota rows. Only effect is the shared `modelSet`/`modelContext` addition
  (see §5).

### 5. Management (`internal/api/management.go`)

- Routes in `handleManagement`: `GET/POST /api/zen/config`,
  `POST /api/zen/models/fetch` (mirror Kimi `management.go:209-217`,
  `handleKimiConfigGet/Save/ModelsFetch` at `1759-1840`). Optionally
  `GET /api/zen/models/cached` if the WebUI needs it — Kimi has no cached
  endpoint; skip unless UI work shows need.
- `handleZenConfigGet`: as Kimi's, plus the `keySource` field from §2 so the
  panel does not report "no key" when `OPENCODE_API_KEY` is set.
- `handleZenModelsFetch`: clone `handleKimiModelsFetch` (`1807-1840`), but
  return the catalog split into `anthropic` and `other` buckets via
  `zen.IsAnthropicWire`, so the picker can grey out or warn on the ~59
  non-forwardable ids instead of offering them as if they worked. An empty
  key is not an error for this endpoint.
- `handleAccountLimits`: add the Zen allowlist loop to
  `modelSet`/`modelContext` (mirror Kimi `440-455`); add
  `"zen": publicCfg["zen"]` to the response map (mirror `681`).
- `handleConfigSave`: no new validation beyond `config.Save`; the Zen key
  preservation lives in `config.Save` (§2).

### 6. WebUI (`internal/webui/public/`)

The Kimi panel is spread across six files, not two. Mirror every one:

- `views/settings.html`: Zen section mirroring the Kimi panel — enable
  toggle, key password input driven by `hasApiKey`/`keySource`, baseUrl
  field, allowlist table (id/alias/display/context/maxOutput/enabled),
  fetch-models button wired to the new endpoints.
- `js/components/server-config.js`: the settings panel logic (save/load/fetch
  handlers) — the bulk of the work.
- `js/data-store.js`: the client-side config shape and defaults.
- `js/components/models.js` and `js/components/model-dropdown.js`: render and
  offer `owned_by:"zen"` entries.
- `js/translations/en.js` **and** `js/translations/pt.js`: every new label.
  A missing `pt` key is a visible regression, not a cosmetic one.
- `views/models.html`: render `owned_by:"zen"` entries; no new discovery
  modal in Phase 1.

### 7. Docs / env

- `antigravity-go-proxy.env.example`: add `# OPENCODE_API_KEY=` with a comment
  (Bearer key from `https://opencode.ai/zen`; env fallback when `zen.apiKey`
  is unset). Also update the classifier-fallback comment at lines 18-20,
  which enumerates the exempt routes ("Kimi, Claude Code, OpenRouter and
  custom endpoints") — add Zen.
- `docs/classifier-fallback-notes.md`: same enumeration, same edit.
- `README.md`: Zen Gateway section — the 15-id Anthropic subset, a
  `config.json` example, the routing-order line, key instructions, and an
  explicit note that the other Zen wire formats are out of scope. No
  `CONTEXT.md` glossary change unless a new domain term survives review.

---

## Task List

| # | Task | Files | Acceptance |
|---|------|-------|------------|
| ~~T1~~ | ~~Spike Zen catalog~~ — **done 2026-09-20**, shape recorded in Context above; no longer gates T2 | — | — |
| T2 | New `internal/zen` package (zen, client with `IsAnthropicWire`, passthrough, observability + unit tests) | `internal/zen/*.go` | `go test ./internal/zen/...` green against `httptest` mock (headers, path, Bearer, empty-key fetch, wire-format predicate) |
| T3 | Config schema + redaction + key-preserve merge + `Routes.Zen` + `cachebump.RouteZen` | `internal/config/config.go`, `internal/cachebump/record.go` | Round-trip test: save with `hasApiKey` preserves key; public view redacts; `EnabledFor("zen", …)` honors the flag |
| T4 | Router: match, forwardToZen (CCR + non-CCR), wire-format guard, max_tokens fill | `internal/api/server.go` | `zen_proxy_test.go`: unary/SSE/alias/`opencode/`-prefix/400-no-key/400-non-Anthropic-id green; omitted `max_tokens` forwards `zen.DefaultMaxOutputTokens`; allowlist cap wins over the default; an explicit client `max_tokens` above the default is **not** clamped |
| T5 | Cache-bump replay sender | `internal/api/cachebump_server.go` | A recorded `RouteZen` record replays to the Zen URL with Bearer; unknown-route fallthrough no longer reachable for Zen |
| T6 | Management endpoints + account-limits exposure + `keySource` | `internal/api/management.go` | `zen_config_test.go`: WebUI save round-trip honored on next forward; `keySource:"env"` reported when only `OPENCODE_API_KEY` is set |
| T7 | WebUI Zen settings panel (all six files) | `views/settings.html`, `js/components/server-config.js`, `js/data-store.js`, `js/components/models.js`, `js/components/model-dropdown.js`, `js/translations/en.js`, `js/translations/pt.js` | Enable/key/allowlist/fetch operable against local proxy; non-Anthropic catalog ids marked unusable; no untranslated `pt` key |
| T8 | Docs + env example | `README.md`, `antigravity-go-proxy.env.example`, `docs/classifier-fallback-notes.md` | Key source, routing order, Phase 1 limits, and the updated classifier-exemption route list documented |
| T9 | Full verify | — | `go build`, `go vet ./...`, `go test ./internal/zen/... ./internal/api/... ./internal/config/... ./internal/cachebump/...` green; live `GET /v1/models` shows Zen entries; live `POST /v1/messages` to a Zen alias returns 200 via key |

---

## Risks / Open Items

- **Resolved:** the `/zen/v1/models` schema. Confirmed live, unauthenticated,
  and metadata-free (see Context). The catalog is a *name list*, not a
  capability source.
- The Anthropic-wire subset is a **static list in code**, because nothing in
  the catalog marks wire format. It will drift when OpenCode adds a model.
  Mitigation: the docs endpoint table is the source of truth; add a note in
  `README.md` telling operators to open an issue when a new
  `@ai-sdk/anthropic` row appears. A stale list fails closed (400 on an
  unknown id), which is the safe direction.
- `max_tokens` has no catalog fallback, so the route ships a package default
  (`zen.DefaultMaxOutputTokens = 32768`). Two failure modes to watch: the
  constant is a guess at the subset's common floor and will need raising if
  OpenCode adds a model with a lower cap (fails loud — Zen 400s the request),
  and it must never be wired into `applyMaxTokensPolicy`'s `derivedLimit`
  argument or it silently truncates explicit client requests (fails quiet —
  hence the dedicated test in T4).
- The `OPENCODE_API_KEY` env fallback is the first provider-key env var in
  this repo. It splits the key into two sources that the WebUI must report
  honestly (`keySource`) and that the forward path and the cache-bump replay
  must resolve identically — hence the single `zenAPIKey` helper in §3.
- No machine-readable Zen pricing endpoint exists — Phase 1 observability
  is tokens/latency only; cost stays zero/unknown rather than invented.
- ID collisions across gateways (`claude-sonnet-4-6`, `claude-sonnet-4-5`,
  `claude-opus-4-5`, `claude-haiku-4-5` in both Zen and CloudCode) resolve by
  the configured gateway precedence (`gatewayOrder.order`, per-model
  `gatewayOrder.byModel`); operators use aliases to disambiguate.
- Jev `/v1/systemone`, `/v1/responses`, `/v1/chat/completions`, and
  Gemini-native paths are deferred — each needs its own translation design,
  not transparent forwarding.

