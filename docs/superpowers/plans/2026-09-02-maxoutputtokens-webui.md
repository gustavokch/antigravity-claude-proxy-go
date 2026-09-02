# Plan: Surface per-model `maxOutputTokens` in the WebUI

## Context

The proxy clamps `max_tokens` to the per-model `maxOutputTokens` value from the OpenRouter allowlist before forwarding to OpenRouter. The clamp lives in `internal/api/server.go` (`clampOpenRouterMaxTokens`, called from `forwardToOpenRouter`).

Today, the only way to set `maxOutputTokens` is by hand-editing `config.json` — the WebUI allowlist table (`internal/webui/public/views/settings.html`, around line 1050–1200) only shows `contextLength` per row, never `maxOutputTokens`. The Kimi allowlist table has the same gap. The discover modal *does* prefill the field on import (see `internal/webui/public/js/components/models.js` line 1115-1130: `addToAllowlist` derives `maxOutputTokens` from `discoveredModel.max_completion_tokens`).

**User impact**: with the new `muse-spark-1.3-contributor` model, the user gets `max_output_tokens The number must be >= 16` from OpenRouter. The clamp fix only activates when `maxOutputTokens` is set in the allowlist — the WebUI does not expose it.

## Goal

Let the user set per-model `maxOutputTokens` for the OpenRouter allowlist directly in the WebUI, including for entries already present (not just newly discovered ones). The Kimi allowlist has the same shape and gets the same treatment for consistency.

## Scope (in)

- OpenRouter allowlist table: new editable `Max Output` input in the existing per-row expanded panel.
- Kimi allowlist table: new per-row expand button (Kimi had no expand today) that reveals a single `Max Output` input, mirroring the OpenRouter panel pattern.
- Discover modal (`openrouter_discover_modal`): show the discovered `max_completion_tokens` value (read-only) so the user knows what gets prefilled.
- Translation keys (en + pt).
- Per-row save: `updateAllowlistMaxOutputTokens(item.id, value)` and `updateKimiMaxOutput(idx, value)`, both debounced.
- Server-side: no new endpoints — the existing `POST /api/openrouter/config` and `POST /api/kimi/config` already round-trip the field via the whole-allowlist replace (`config.go:361-381` for OpenRouter, `:382-401` for Kimi). `internal/openrouter/client.go:16,27,31` already has the upstream `max_completion_tokens` field; the proxy already trusts `perModel.MaxOutputTokens` as a floor in `clampOpenRouterMaxTokens`. Per user decision, no global 16 fallback when `maxOutputTokens` is 0.
- One new Go test in `internal/api/openrouter_proxy_test.go` confirming the floor is read from a save round-trip (lock the contract).

## Scope (out)

- Custom Endpoints, Claude Code allowlist — no `maxOutputTokens` in those types, not in scope.
- Cloud Code (Antigravity) catalog — different system.
- A bare "remove the floor" toggle — clamp stays, only the input is new.

## Design

### Data shape (already correct)

```go
// internal/config/config.go:24-35
type OpenRouterModelConfig struct {
    ID              string  `json:"id"`
    Alias           string  `json:"alias,omitempty"`
    DisplayName     string  `json:"displayName,omitempty"`
    ContextLen      int     `json:"contextLength,omitempty"`
    MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`  // already here
    Enabled         bool    `json:"enabled"`
    ProviderMode    string  `json:"providerMode,omitempty"`
    PinnedProvider  string  `json:"pinnedProvider,omitempty"`
    ProviderOrder   []string `json:"providerOrder,omitempty"`
    ResponseCache   *OpenRouterResponseCacheConfig `json:"responseCache,omitempty"`
}
```

`KimiModelConfig` (lines 71-79) mirrors the field. Both config and the WebUI client (`models.js:1115-1130`) already serialize/deserialize `maxOutputTokens`. No new struct fields needed.

### UI (expandable row approach)

Reuse the existing expandable per-row pattern (the one that already hosts "Provider Routing"). The expand button (line 1092) opens a panel beneath the model row. Add a **Limits** subsection to that panel with two inputs side-by-side: **Context** (was previously read-only in the table — now editable here) and **Max Output** (new). The visible Context column in the table stays read-only; editing happens in the panel.

This keeps the table compact (no new column, no width rebalance) and centralizes all per-model config in one expand target. The column structure stays: Status w-12 / Model ID w-5/12 / Local Alias w-4/12 / Context w-2/12 (read-only) / Actions w-12.

**Section order inside the expanded panel** (top to bottom):
1. Provider Routing (existing — auto / pinned / custom toggle, providers table)
2. Limits (new — Context input + Max Output input, side-by-side grid)

### Critical files (representative)

- `internal/webui/public/views/settings.html` — OpenRouter expanded panel at line ~1113 (after the existing `setProviderMode` row, before the providers table). Add a sibling `<div class="grid grid-cols-2 gap-4">` for the two inputs.
- `internal/webui/public/views/settings.html` — Kimi allowlist: Kimi has no per-row expand today. Add a single-row expand button to the Kimi table (mirror the OpenRouter pattern but smaller — just Max Output since Kimi models have no routing panel) OR add a new inline 6th column for Kimi only. Decision: **add a per-row expand button to Kimi** for consistency with the chosen OpenRouter pattern. The expand reveals only the Max Output input.
- `internal/webui/public/js/components/models.js` — add `updateAllowlistMaxOutputTokens(item.id, value)` next to `updateAllowlistAlias` (line 1147). Add `updateAllowlistContextLength(item.id, value)` for the OpenRouter panel-side Context editor. Add `updateKimiMaxOutput(idx, value)` for the Kimi expand path.
- `internal/webui/public/js/translations/en.js` (line ~519) — new keys: `openRouterLimits`, `openRouterMaxOutput`, `openRouterContextLength`, `kimiMaxOutput`, `openRouterDiscoverMaxOutput` (still surfaced in the discover modal so the user knows what gets prefilled).
- `internal/webui/public/js/translations/pt.js` (line ~453) — matching PT keys.
- `internal/api/openrouter_proxy_test.go` — one new test asserting the round-trip: save `maxOutputTokens: 16`, then a request with `max_tokens: 5` lands at the upstream with `max_tokens: 16`. (Closes the test gap that this whole change is built on.)

### Reused functions

- `config.Save` (`config.go:304`) — already merges allowlist items whole-object.
- `clampOpenRouterMaxTokens` (`server.go:1010`) — already reads `perModel.MaxOutputTokens` and floors `max_tokens`. No change. Per user decision: **no global fallback floor** when `maxOutputTokens` is 0.
- `addToAllowlist` (`models.js:1115-1130`) — already prefills from discover. No change.
- `saveOpenRouterConfigDebounced` (`models.js:418`) — reuse for the new panel inputs to coalesce rapid edits.
- `updateAllowlistAlias` (`models.js:1147-1153`) — copy this pattern for `updateAllowlistMaxOutputTokens` and `updateAllowlistContextLength`.
- `toggleRoutingExpanded` (`models.js:353-361`) — Kimi gets a parallel `toggleKimiExpanded(modelId)` or reuses the same `expandedRouting` set; the latter is simpler.

### Translation keys (en + pt)

```js
openRouterLimits: "Limits",                  // expanded-panel section header
openRouterMaxOutput: "Max Output",           // input label
openRouterContextLength: "Context Window",   // input label
kimiMaxOutput: "Max Output",                 // kimi panel label
openRouterDiscoverMaxOutput: "Max Output",   // discover modal column header (read-only)
```

PT translations added in `pt.js` near the existing `openRouterAllowlist` key (line ~453). PT defaults to the same string when missing (existing pattern is `t(key) || 'English fallback'`).

### Why an expandable section, not a column

- Keeps the allowlist table at 5 columns; no width rebalance needed and no horizontal-scroll risk on 1280px.
- Centralizes all per-model config in one expand target. A user who expanded the row to set routing already sees the new Limits section.
- The discover modal pre-fill is unchanged — the discover flow still writes `maxOutputTokens` to the item, and the panel reflects it on next expand.
- For Kimi, the per-row expand is new but trivially small (single input, one button).

## Implementation steps

1. **Settings HTML — OpenRouter panel** — add a Limits subsection inside the existing expanded panel (after the Provider Routing controls at line ~1130, before the providers table at line ~1138). Two side-by-side inputs: `Context Window` (`item.contextLength`) and `Max Output` (`item.maxOutputTokens`). Both call `updateAllowlistContextLength` / `updateAllowlistMaxOutputTokens` and reuse `saveOpenRouterConfigDebounced`.
2. **Settings HTML — Kimi** — add a per-row expand button to the Kimi allowlist (currently a flat table at line ~1280). On expand, show a single `Max Output` input bound to `m.maxOutputTokens` with `@change="updateKimiMaxOutput(idx, $event.target.value)"`. Reuse the existing `expandedRouting` set, or add `expandedKimi` — pick whichever minimizes new state.
3. **Settings HTML — discover modal** — add a `Max Output` cell to the discover modal table (line ~2120) showing `m.max_completion_tokens || (m.top_provider?.max_completion_tokens) || 0` rendered as `Nk` (e.g. `16k`, `128k`, `-` when zero). No control, just a label.
4. **Models JS** — add three handlers near `updateAllowlistAlias` (line 1147): `updateAllowlistContextLength(modelId, value)`, `updateAllowlistMaxOutputTokens(modelId, value)`, `updateKimiMaxOutput(idx, value)`. Add an `asInt` helper to coerce empty string to 0. Kimi expand state: a new `expandedKimi: new Set()` on the component, with a `toggleKimiExpanded(id)` method that mirrors `toggleRoutingExpanded`. (Separate set keeps the two expand states independent — no namespacing needed.)
5. **Translations** — add the five keys (`openRouterLimits`, `openRouterMaxOutput`, `openRouterContextLength`, `kimiMaxOutput`, `openRouterDiscoverMaxOutput`) to `en.js` and the matching five to `pt.js`.
6. **Server test** — extend `TestOpenRouterForwarding_ClampsMaxTokensBelowCatalogMax` to also assert: a `POST /api/openrouter/config` save with `maxOutputTokens: 16` is honored on the next request (round-trip the value through `config.Save` and `GetPublicConfig`). One new test, not a refactor of the existing two.
7. **Verification** — `go test -count=1 ./internal/api/...` (no JS test runner in repo; manual verify in WebUI after build).

## Verification

- `go test -count=1 ./internal/api/...` — full api suite still passes (target 233+ tests after the new round-trip test).
- Manual: open WebUI → Settings → OpenRouter → edit an existing entry's Max Output to `16` → save → restart not needed (config hot-reload already supported via `applyRouterConfig`); send a request with `max_tokens: 5` and confirm upstream receives `16`.
- Manual: Kimi side same flow.
- Manual: discover a model whose `max_completion_tokens` is 128000 — confirm the table shows `128000` after import; user can lower it to `16` to force the muse-spark-style floor.
- WebUI smoke: en + pt locale, two adjacent allowlist rows, no horizontal scroll on a 1280px viewport.
