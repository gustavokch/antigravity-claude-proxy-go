# Configurable Gateway Precedence for Colliding Model IDs

## Context

One model ID can exist on more than one gateway. `claude-sonnet-4-6` exists on
Cloud Code, on Kimi, and on OpenRouter. The proxy resolves such a collision by a
hard-coded if-chain in `internal/api/server.go:1022-1096`:

```
Kimi -> Zen -> ClaudeCode -> OpenRouter -> CustomEndpoints -> CloudCode
```

Each branch returns on the first match. The order is not a data structure, so an
operator cannot change it. The order is also not pinned by a test: no test in the
repository enables two gateways at the same time (verified: zero tests set two
gateway `Enabled` flags together). The order appears in documentation only in
`docs/plans/2026-09-21-opencode-zen-gateway-plan.md` (historical; its branch-insertion
instruction at `:224` was executed by PR #88), and it disagrees with `README.md:47-77`,
which omits both Kimi and Zen.

The OpenCode Zen gateway landed on fork/main (PR #88, `74e0bec`) between the writing
of this plan's first draft and its execution. The dispatch walk below is written for
the six-provider chain as it exists today.

**Outcome.** The precedence becomes data. The operator sets it in
`config.json` or in the WebUI. The default value reproduces today's behaviour
exactly.

## Decisions (locked)

1. **Standalone change.** Six live providers. Provider IDs: `kimi`, `zen`,
   `claudecode`, `openrouter`, `custom`, `cloudcode`.
2. **Two knobs.** A global order list. A per-model override map. The per-model
   entry wins.
3. **The list is a hint, never a disable list.** A provider omitted from the
   list is not disabled. It is appended after the listed providers, in default
   order. A partial config can never silently drop a gateway.
4. **Default order equals today's behaviour.** `kimi`, `zen`, `claudecode`,
   `openrouter`, `custom`, `cloudcode`.
5. **`cloudcode` is terminal.** The walk stops when it reaches `cloudcode` and
   continues to the account-backed path. Hoisting it is the supported way to say
   "prefer the account pool for this model". With the default order this is
   exactly today's behaviour.
6. **Every known gateway is wired.** Zen shipped with PR #88, so there is no
   reserved-slot comment to keep: `DefaultGatewayOrder()` lists all six IDs and
   `gatewayHandlers` covers every one except the terminal `cloudcode`. The
   coverage test (`TestGatewayHandlers_CoverEveryKnownGateway`) now guards the
   six live handlers and pins that a future gateway cannot merge half-wired.
7. **Ordering never widens a matcher.** The walk only chooses between gateways
   that already accept the model. The matchers are deliberately asymmetric and
   stay that way:
   - `matchKimiModelEntry` strips a trailing `[1m]` and compares
     case-insensitively.
   - `matchZenModelEntry` (`server.go:1827`) compares case-insensitively, strips
     an `opencode/` prefix via `zen.StripOpencodePrefix` on both entry and
     request, matches aliases raw and stripped, wire-gates entries on
     `zen.IsAnthropicWire`, and declines whenever no API key resolves
     (`zenAPIKey` empty) so a keyless allowlisted config falls through instead
     of claiming the route. It does not strip `[1m]`.
   - the OpenRouter branch compares literally.
8. **Zen's key check stays inside its handler.** The keyless fall-through is a
   matcher concern, not an ordering concern. The walk must not learn about keys.

## Non-goals

- The non-Anthropic-format custom-endpoint short-circuit in
  `internal/api/openai_proxy.go:41-56`. It runs before translation and cannot be
  ordered. `custom` in the order list covers only the Anthropic-format path.
- Renaming or changing any existing matcher.
- Any change to the Zen gateway itself beyond making its dispatch position
  configurable.

## Config schema

New top-level section. The name `gatewayOrder` avoids confusion with
`OpenRouterModelConfig.ProviderOrder` (`internal/config/config.go:35`), which
orders upstream OpenRouter providers, a different concept.

```json
{
  "gatewayOrder": {
    "order": ["kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"],
    "byModel": {
      "claude-sonnet-5": ["openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"]
    }
  }
}
```

- `byModel` is keyed by the model string the dispatcher sees: after
  `resolveModelMapping` and after any classifier reroute. Keys are stored
  normalised: trimmed, lowercased, trailing `[1m]` removed. Lookups normalise
  the request the same way.
- An empty or absent `order` means "no opinion" and yields the default order.
  `"order": []` is not "disable everything".

## Tasks

### T1 — Config schema and precedence semantics

New file `internal/config/gateway_order.go`. Edit `internal/config/config.go`:
the `Config` struct at `:121-157` (`zen` at `:151`), `DefaultConfig()` at
`:345` (Zen defaults at `:386-388`), `GetPublicConfig()` at `:769-924`.

```go
type GatewayID string

const (
    GatewayKimi       GatewayID = "kimi"
    GatewayZen        GatewayID = "zen"
    GatewayClaudeCode GatewayID = "claudecode"
    GatewayOpenRouter GatewayID = "openrouter"
    GatewayCustom     GatewayID = "custom"
    GatewayCloudCode  GatewayID = "cloudcode"
)

// DefaultGatewayOrder returns the precedence used when config.json says nothing,
// and the order in which providers omitted from a configured list are appended.
// It reproduces the historical dispatch if-chain exactly (Kimi, Zen, ClaudeCode,
// OpenRouter, custom endpoints; Cloud Code terminal).
func DefaultGatewayOrder() []GatewayID

func KnownGatewayIDs() []GatewayID
func IsKnownGatewayID(id GatewayID) bool

type GatewayOrderConfig struct {
    Order   []GatewayID            `json:"order,omitempty"`
    ByModel map[string][]GatewayID `json:"byModel,omitempty"`
}

// Effective returns the precedence for one model: the matching ByModel entry
// when one exists, otherwise Order; then every known provider that the result
// omits, appended in DefaultGatewayOrder order. An empty list yields
// DefaultGatewayOrder. Unknown IDs are dropped, so a config.json written by a
// newer build cannot break routing. Duplicates keep their first occurrence.
// An empty model selects the global order only.
func (c GatewayOrderConfig) Effective(model string) []GatewayID

// NormalizeModelKey trims, lowercases, and drops a trailing "[1m]" marker.
// It reuses modelcatalog.Strip1mSuffix (internal/modelcatalog/catalog.go:232) so
// one override covers every spelling a client sends for the same model. A second
// implementation would be free to drift from the one discovery already uses.
func NormalizeModelKey(model string) string
```

`config` gains the leaf import `modelcatalog`. No cycle: `modelcatalog` imports
only `encoding/json`, `errors`, `fmt`, `sort`, `strings`. `config` must not
import `internal/zen`: the Zen wire helpers stay owned by the `zen` package and
`config` needs none of them.

Config plumbing:

| Location | Change |
|---|---|
| `Config` struct `:121-157` | Add `GatewayOrder GatewayOrderConfig \`json:"gatewayOrder"\``. No `omitempty`: a struct always serialises, so the WebUI always sees the key |
| `DefaultConfig()` `:345` | Set `GatewayOrder.Order = DefaultGatewayOrder()` |
| `Load()` `:508` | None. `Load` starts from `DefaultConfig()` and `json.Unmarshal` replaces slices |
| `Save()` `:551` | **None.** Verified: the generic one-level map merge (now ~`:740`, `existingMap[vk] = vv`) handles a nested object, and the merge is per-key-present, so `{"gatewayOrder":{"order":[...]}}` replaces `order` and leaves `byModel` intact. `headroom` and `cacheBump` are precedent |
| `GetPublicConfig()` `:769-924` | One synthetic key: `result["knownProviders"] = KnownGatewayIDs()`. Precedent: `hasPassword` is injected the same way at `:777-781`. The WebUI needs the full vocabulary to render appended providers. No secret, so no redaction |

Tests: new `internal/config/gateway_order_test.go`, table-driven with `t.Run`.
Cases: zero value yields the default order; `Order: [kimi]` appends the rest in
default order; `Order: [openrouter]`; duplicate entry deduped first-wins; an
unknown `zen`-like entry (use a genuinely unknown ID such as `nope` — `zen` is
now known) dropped; a `byModel` entry beats `Order`; a non-matching model falls
back to `Order`; a `byModel` key matches a `[1m]` request; a hand-edited
unnormalised key still matches; an empty key matches nothing; `Order: []` with a
`byModel` entry; and a `NormalizeModelKey` table (`" Claude-Sonnet-5[1m] "` ->
`claude-sonnet-5`, `"[1M]"` -> `""`, a dotted name unchanged).

Filesystem tests, using the convention from `internal/config/config_test.go:180-188`
(`t.TempDir()` plus `t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)`; `CONFIG_DIR` is the
fallback var — no `HOME` juggling needed): no config file yields the default; a
partial file keeps both keys; `"order": []` yields the default.

**Acceptance:** `go test ./internal/config/...` green. No `api` change yet.

### T2 — Dispatch walk, behaviour-preserving

New file `internal/api/dispatch.go`. Edit `internal/api/server.go`: delete
`:1022-1096`, insert one call. Keep the classifier-fallback comment at
`:1098-1105` and the gate itself unchanged.

```go
// gatewayRequest carries what one alternate-backend dispatch pass needs. A
// gateway that accepts the request writes its response and returns true. A
// gateway that declines must leave every field untouched, so the next gateway
// sees the request exactly as the previous one did.
type gatewayRequest struct {
    writer  http.ResponseWriter
    request *http.Request
    cfg     config.Config
    body    map[string]any
    rawBody []byte
    mutated bool
    model   string
}

// gatewayHandlers maps every gateway this build can dispatch to onto its
// handler. cloudcode is deliberately absent: it is the terminal account-backed
// path, not a gateway. TestGatewayHandlers_CoverEveryKnownGateway pins that this
// table covers exactly config.KnownGatewayIDs() minus cloudcode.
var gatewayHandlers = map[config.GatewayID]func(*Server, *gatewayRequest) bool{
    config.GatewayKimi:       (*Server).tryKimiGateway,
    config.GatewayZen:        (*Server).tryZenGateway,
    config.GatewayClaudeCode: (*Server).tryClaudeCodeGateway,
    config.GatewayOpenRouter: (*Server).tryOpenRouterGateway,
    config.GatewayCustom:     (*Server).tryCustomEndpointGateway,
}

// dispatchAlternateBackend walks the configured precedence and hands the request
// to the first gateway that accepts it. It returns true when a gateway wrote the
// response, and false when no gateway handled the request: cloudcode was
// reached, or every listed gateway declined.
//
// Each handler re-checks its own enabled flag, its own matcher, and (for Zen)
// its own key resolution. The walk is responsible for order only. Keeping those
// conditions inside the handlers is deliberate: they are the exact conditions
// the pre-refactor if-chain used.
func (server *Server) dispatchAlternateBackend(g *gatewayRequest) bool
```

The five handlers port the branch bodies verbatim:

- `tryKimiGateway` — was `:1022-1035`. `matchKimiModelEntry`, then
  `stripKimi1mSuffix`, model rewrite, marshal, then
  `applyMaxTokensPolicy(body, body, entry.MaxOutputTokens, 0)`, then
  `forwardToKimi`.
- `tryZenGateway` — was `:1036-1049`. `matchZenModelEntry` (which already owns
  the keyless fall-through), `zenTargetModel`, model rewrite, marshal, then
  `forwardToZen(writer, request, cfg.Zen, reqBody, anthropicRequest, target,
  zenEntry)` — note it receives both the marshalled body and the original map.
- `tryClaudeCodeGateway` — was `:1050-1063`, including
  `claudeCodeEntryMaxOutput(cfg.ClaudeCode, ccMatch)` as the derived limit and
  the existing error text.
- `tryOpenRouterGateway` — was `:1064-1081`. Keep the allowlist walk and its
  literal ID/alias equality. Do **not** add a max-tokens policy call: it lives
  inside `forwardToOpenRouter` (`server.go:1621-1637`), which also owns the 400.
- `tryCustomEndpointGateway` — was `:1082-1093`, including the `!g.mutated`
  raw-body passthrough.

Call site:

```go
if server.dispatchAlternateBackend(&gatewayRequest{
    writer: writer, request: request, cfg: cfg,
    body: anthropicRequest, rawBody: rawBody, mutated: bodyMutated, model: model,
}) {
    return
}
```

An operator who hoists `cloudcode` ahead of a gateway is choosing to have the
classifier-fallback gate consulted first. State this in the comment.

Tests: new `internal/api/dispatch_test.go`. Build one fixture that points every
gateway at its own `httptest` server, all serving the same colliding model ID,
and records which upstream was hit. Reuse `fakeMsgServer`
(`internal/api/claudecode_proxy_test.go:23`) and the reset of
`ccPoolMu`/`ccPoolInst`/`ccHTTPClient` (`:53-58`). Drive requests through
`server.Handler().ServeHTTP`, as `internal/api/kimi_proxy_test.go:93` does.
Zen needs a key: set `zen.apiKey` in the fixture (a keyless Zen config
exercises the separate fall-through test below). This is the first test in the
repository that enables two gateways at once.

| Test | Assertion |
|---|---|
| `TestDispatch_DefaultOrderUnchanged` | No `gatewayOrder` key. All five gateways accept the ID: winner is `kimi`. Repeat with each of Kimi, Zen, ClaudeCode, OpenRouter disabled in turn |
| `TestDispatch_GlobalOrderReorders` | `order: [openrouter, ...]` routes to OpenRouter |
| `TestDispatch_PerModelOverrideBeatsGlobal` | Override wins for that model only, a second model still follows the global order |
| `TestDispatch_OmittedProviderStillDispatches` | `order: [kimi]` alone, request a ClaudeCode-only model: winner is `claudecode` |
| `TestDispatch_UnknownProviderIDsIgnored` | A hand-edited `nope` entry: no panic, no 500, Kimi still wins |
| `TestDispatch_OverrideCannotWidenAMatcher` | Override names only OpenRouter, request has a `[1m]` suffix: the walk falls through to Kimi |
| `TestDispatch_1mSuffixOverrideKey` | Override key normalised, forwarded body carries the canonical ID |
| `TestDispatch_CloudCodeTerminalStopsTheWalk` | `order: [cloudcode, kimi]`: no gateway hit, account path taken |
| `TestDispatch_DisabledGatewaySkippedEvenWhenListed` | A disabled gateway that is listed is skipped |
| `TestDispatch_CustomEndpointHonorsOrder` | Custom endpoint and Kimi allowlist share an ID: each wins when listed first |
| `TestDispatch_OpenAIChatCompletionsSharesTheOrder` | `/v1/chat/completions` gives the same winner |
| `TestDispatch_ForwardedModelUsesProviderCanonicalID` | A Kimi alias request forwards the canonical ID; a Zen `opencode/`-prefixed request forwards the canonical catalog spelling |
| `TestDispatch_ZenKeylessConfigFallsThrough` | Zen listed first with no resolvable key: walk continues, next gateway wins |
| `TestDispatch_ZenNonWireEntryFallsThrough` | Zen allowlist entry fails `zen.IsAnthropicWire`: walk continues, next gateway wins |
| `TestGatewayHandlers_CoverEveryKnownGateway` | `cloudcode` has no entry; every other known ID has one; no entry is unknown |
| `TestDispatch_ClassifierFallbackStillLosesToGatewaysUnderDefaultOrder` | Fallback active and no account capacity: Kimi is still hit, no canned stub is returned |
| `TestDispatch_ClassifierRerouteChangesTheOverrideKey` | A rerouted model uses the `byModel` entry of the target model |
| `TestDispatch_HotReload` | `config.Save` a new order, then the same request: the winner changes, with no restart |

**Acceptance:** `go test -race ./...` passes unchanged. The default-order pin
passes. `git diff` shows the five branch bodies moved, not rewritten.

### T3 — Save-time validation and the public vocabulary

Edit `internal/api/management.go`: add a block in `handleConfigSave` after the
classifier block (starts `:1003`) and before `config.Save` at `:1136`. New file
`internal/api/gateway_order_validation_test.go`.

```go
// normalizeGatewayOrderUpdate validates and normalises an incoming
// "gatewayOrder" section. It returns a map holding only the keys the client
// sent, deliberately not a typed struct: Save's generic merge is per-key
// present, and replacing the whole section here would make a partial
// {"order":[...]} POST silently discard every per-model override. Pointer fields
// distinguish "key absent" from "key empty".
func normalizeGatewayOrderUpdate(raw any) (map[string]any, error)
```

Policy:

| Rule | Action |
|---|---|
| Unknown provider ID | Reject, 400. The failure mode of accepting is silent misrouting |
| Duplicate entry after normalisation | Reject, 400 |
| Empty `order` | Accept. This equals unset |
| ID case or whitespace (`" Kimi "`) | Normalise: trim and lowercase |
| `byModel` key empty after trim | Reject, 400. A `""` key can never match |
| `byModel` key names no known model | Accept. There is no authoritative model registry, and an override for a model no gateway serves yet is legitimate forward planning |
| `byModel` key case, `[1m]`, whitespace | Normalise the same way as `NormalizeModelKey` |
| `cloudcode` in any position | Accept. It is a legal terminal position |
| A `byModel` value that is not a string array | Map the decode failure to 400, not 500 |

Error text names the offending value, in the shape of the classifier block:
`unknown provider "nope": must be one of kimi, zen, claudecode, openrouter,
custom, cloudcode`.

No validation in `config.Load`: a hand-edited `config.json` must never prevent
startup. `Effective` drops unknown IDs instead.

No new `applyXConfig` push is needed. `config.Get()` is read once per request
(`server.go:162`, `:212`, `:448`, `:834`, `:1164`), so ordering is hot by
construction. State this in a comment.

Tests, using a helper modelled on `postConfigRules`
(`internal/api/classifier_config_test.go:16-25`): accept a full list, a partial
`{"order":[...]}`, a mixed-case `byModel` key, an empty `order`, a `byModel`
entry naming `cloudcode`; reject an unknown provider, a duplicate, and an empty
model key; prove a partial `{"order":[...]}` POST keeps `byModel`; accept an
unknown model name; prove hot reload; and prove `GET /api/config` exposes
`knownProviders` and the default `order`.

**Acceptance:** all tables green, `go vet ./...` clean.

### T4 — Discovery correctness

Two separable commits. T4a alone is a safety fix and can ship without T4b.

**T4a — deduplicate `/v1/models`.** Edit the discovery section of
`internal/api/server.go:406-666`. The OpenRouter branch (`:512+`), the Kimi
branch (`:579+`), and the Zen branch (`:628+`) append without consulting the
`seen` map, so a colliding ID is advertised twice. Guard every append with
`seen` and mirror the ClaudeCode branch shape at `:451-508`. Zen appends two
entries per wire model (the `opencode/`-prefixed spelling and the bare wire ID
— see `:628-666`); the `seen` guard must treat both spellings of one model as
one advertised ID.

**T4b — advertised owner follows the configured order.** The advertised owner
must be the gateway the dispatcher picks. New file
`internal/api/discovery.go`:

```go
// gatewayOwnedModelIDs returns the IDs a dispatch-active gateway would accept.
// The Cloud Code catalog must not advertise an ID that a gateway wins, or the
// advertised owner disagrees with the dispatcher.
func gatewayOwnedModelIDs(cfg config.Config) map[string]bool

// gatewayModelAppenders appends one gateway's models to the /v1/models list.
// Coverage is pinned by TestGatewayModelAppenders_CoverEveryKnownGateway.
var gatewayModelAppenders map[config.GatewayID]func(...)
```

The four existing gateway sections become a walk over
`cfg.GatewayOrder.Effective("")`. Each appender keeps its existing
limit-derivation code and comments. Zen's appender keeps its wire-subset gate
and its `zen.DefaultMaxOutputTokens` fill. `custom` contributes no entries: no
discovery helper for custom endpoints exists, and this change does not add one.
Do not reorder the catalog to the end of the array: entry order is cosmetic,
ownership is functional. State that in a comment.

Tests, extending `internal/api/models_discovery_test.go`, which already has
`discoveryTestBackend` and `withOpenRouterCatalog`. No test today enables two
providers at once.

| Test | Assertion |
|---|---|
| `TestModelsDiscovery_NoDuplicateAcrossGateways` | Kimi and OpenRouter both enabled with a shared ID: exactly one entry |
| `TestModelsDiscovery_AdvertisedOwnerMatchesDispatchWinner` | `owned_by` equals the gateway the dispatcher picks, driven by one shared helper so the halves cannot drift |
| `TestModelsDiscovery_OwnerFollowsConfiguredOrder` | Flip `order`, `owned_by` flips with it |
| `TestModelsDiscovery_CatalogCollisionYieldsToGateway` | Catalog and Kimi share an ID: one entry, owned by `kimi` |
| `TestModelsDiscovery_ClaudeCodeAccountsWithoutEnabledDoesNotOwnIDs` | `claudecode.enabled=false` with an account: the catalog owns the ID |
| `TestModelsDiscovery_PerModelOverrideDoesNotAffectDiscovery` | The global order governs the advertised order |
| `TestModelsDiscovery_ZenAdvertisesWireSubsetOnly` | Zen enabled: non-wire allowlist entries produce no `/v1/models` entry, and a Zen ID shared with Kimi appears once with the order-winning owner |

**Acceptance:** `go test ./internal/api/...` green, including the pre-existing
duplicate check in `internal/api/models_discovery_test.go:80-85`. The moved
appenders show as moves in `git diff`.

### T5 — WebUI

New component `internal/webui/public/js/components/gateway-order.js`. Edit
`public/index.html` (script tag after `classifier-audit-feed.js` at `:527`,
before `app.js` at `:530`), `public/views/settings.html` (panel inside the
`settingsTab === 'models'` div at `:917`, beside the gateway allowlist panels —
OpenRouter, Kimi, and the Zen panel that PR #88 added), `public/js/translations/en.js`,
`public/js/translations/pt.js`, `internal/webui/translations_test.go`,
`internal/webui/embed_test.go`.

A new component, not a growth of `models.js`, which is now over 1700 lines and
already hosts three gateways' panels. The panel fetches its own state through
`window.utils.request` (`public/js/utils.js:7-32`).

Control: an ordered list with up and down buttons, following the `moveProvider`
precedent (`public/js/components/models.js:453`) and its 500 ms trailing
debounce (`:465-484`). No drag-and-drop: the repository has no drag-and-drop
primitive, and buttons are keyboard-reachable.

The panel always renders all six providers, in `Effective` order: the
configured ones first, then the omitted ones in a muted style under a hint that
they are tried after the listed ones. There is no remove control for the global
list, because removal would read as "disable", which decision 3 forbids. Every
provider stays movable, which is what adds it to `order`. Per-model overrides do
get add and remove controls, because an override is opt-in.

Provider labels come from the server vocabulary, not a hard-coded list:
`GET /api/config` returns `knownProviders`, and the label for ID `x` is
`t('gateway' + Capitalised(x))`. `cloudcode` renders a terminal hint, since the
walk stops there. `zen` renders its own short hint only if the terminal hint
does not cover it — the keyless fall-through is visible in the existing Zen
panel, not duplicated here.

New i18n keys, in both `en` and `pt`:
`gatewayOrderTitle`, `gatewayOrderDesc`, `gatewayOrderGlobal`,
`gatewayOrderGlobalHint`, `gatewayOrderPerModel`, `gatewayOrderPerModelHint`,
`gatewayOrderAddModel`, `gatewayOrderModelPlaceholder`,
`gatewayOrderRemoveOverride`, `gatewayOrderReset`, `gatewayOrderNoOverrides`,
`gatewayOrderSaved`, `gatewayOrderMoveUp`, `gatewayOrderMoveDown`,
`gatewayOrderTerminalHint`, `gatewayKimi`, `gatewayZen`, `gatewayClaudecode`,
`gatewayOpenrouter`, `gatewayCustom`, `gatewayCloudcode`.

`translations_test.go`: add a `gatewayOrderKeys` list (the pattern is
`openRouterPanelKeys`/`kimiPanelKeys` at `:13`/`:23`), add a
`TestTranslations_GatewayOrderKeys` case for both locales, and add a template
reference test following `TestTranslations_AppSpoofTemplateReferences`
(`:245`). The six provider labels render through `t(gatewayLabelKey(id))`, so a
literal `t('...')` check cannot match them. Exclude them from the template test
and say why in a comment, or the guardrail either false-fails or must be
weakened to a check that proves nothing.

`embed_test.go`: extend `TestHandler` (`:10`) to assert `GET /` contains
`gateway-order.js`, and add an assertion that `GET /views/settings.html`
contains `gatewayOrder`.

**Acceptance:** `go test ./internal/webui/...` green. Manual pass on port 8091:
the panel renders, saves, and survives a page reload.

### T6 — Docs

| File | Edit |
|---|---|
| `README.md:47-77` | The diagram is wrong today: no Kimi step, no Zen step, and the order disagrees with the code. Replace steps 2-5 with `kimi`, `zen`, `claudecode`, `openrouter`, `custom`, `cloudcode`, mark the last as terminal, and add a paragraph on `gatewayOrder`: the hint semantics, the per-model override, that an override selects only among gateways whose matcher already accepts the model, that a hoisted `cloudcode` stops the walk, that Zen falls through when no API key resolves, and that the non-Anthropic-format custom short-circuit for `/v1/chat/completions` is not orderable |
| `CONTEXT.md`, glossary, "Core Routing" after **Custom Endpoint** (`:17-19`) | Add **Gateway Precedence**: "The configurable order in which the proxy tries each gateway (Kimi, Zen, Claude Code, OpenRouter, custom endpoints) when more than one would accept the same Requested Model; `cloudcode` marks the terminal account-backed route. Configured by `gatewayOrder.order`, overridden per model by `gatewayOrder.byModel`. The list is a hint: an omitted gateway is tried after the listed ones rather than disabled." `_Avoid_:` provider priority, load order, gateway ranking, fallback chain |
| `docs/plans/2026-09-21-opencode-zen-gateway-plan.md` | Now a historical record: PR #88 executed it. (a) Mark the §4 instruction to insert a Zen branch between the Kimi and ClaudeCode blocks (`:224`) as superseded — the branch is data-ordered via `DefaultGatewayOrder()` and `gatewayHandlers`. (b) Replace the line stating that collisions resolve by documented first-match order with the configured precedence. (c) Note in the ASCII routing diagram that the arrows are the default order and are now configurable. Commit this file first — it is still untracked |
| `antigravity-go-proxy.env.example` | No change. The knob is `config.json` and WebUI only. State that in the README paragraph |

## Verification

1. `make build` — `go build -o bin/antigravity-proxy ./cmd/proxy`.
2. `go vet ./...`.
3. `make test` — `go test -v -race ./...`. All pre-existing tests stay green,
   specifically `internal/webui/translations_test.go`,
   `internal/webui/embed_test.go`, and `internal/api/models_discovery_test.go`.
4. Manual end-to-end on port 8091. Seed `~/.config/antigravity-proxy/config.json`
   with two gateways that share one model ID. Confirm the default order routes to
   Kimi. Reorder in the WebUI. Confirm the same request routes to the other
   gateway with no restart. Confirm `GET /v1/models` shows one entry whose
   `owned_by` matches the winner. Confirm a POST that names `nope` returns 400 and
   the message lists the six valid IDs. Confirm a keyless Zen config listed
   first falls through to the next gateway.
5. `git diff --stat`: `internal/api/server.go` must shrink by roughly the size of
   the five moved branches. There must be no second copy of any branch body.

## Sequencing

| # | Task | Depends on |
|---|---|---|
| 1 | T1 config schema and `Effective` | — |
| 2 | T2 dispatch walk and the default-order pin | T1 |
| 3 | T3 save validation and `knownProviders` | T1 |
| 4 | T4a discovery dedup | — |
| 5 | T4b discovery ownership | T4a, T1 |
| 6 | T5 WebUI, i18n, guardrails | T3 |
| 7 | T6 docs | T2, T5 |
| 8 | End-to-end verification | all |

## Risks

- **T2 has the largest blast radius.** It touches every request path, and the
  Zen branch it moves is newer than the rest. Control: write the default-order
  pin first and make it pass against unmodified code, then treat the diff as a
  move, not a rewrite.
- **T2 is the first multi-gateway test.** Nothing in the suite exercises two
  gateways at once, so the fixture may surface latent interactions between the
  gateway stubs (shared `ccPool` state, `zenKeylessWarned` one-shot warn). Control:
  reset both gateways' package-level warn/state flags in the fixture helper.
- **T4a's Zen double-entry.** Zen advertises two spellings per wire model; a
  naive `seen` guard keyed on the entry ID would not collapse the pair. Control:
  normalise both spellings through one key before the guard.
- **T4b changes shared discovery output.** T4a alone is safe to ship. If T4b
  proves contentious, defer it; the ordering knob works without it, and the
  duplicate-advertisement bug is fixed by T4a.
- **T5 cannot start before T3**, because `knownProviders` is the panel's only
  source of the provider vocabulary.
- **A hoisted `cloudcode` skips every later gateway**, which surprises an
  operator who reads the list as a hint rather than a sequence. Control: the
  panel renders `cloudcode` with a terminal hint, and the README states it.
