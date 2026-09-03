# Settings / Models WebUI UX Enhancements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On the `/#settings/models` tab of the WebUI, show per-provider OpenRouter pricing in the provider-routing table and widen the System Configuration card to use horizontal space on wide screens.

**Architecture:** The backend already ships full OpenRouter endpoint data — including `pricing` — through `GET /api/openrouter/providers` (the handler embeds the whole `openrouter.ProviderEndpoint`, and `Pricing` is a struct field on it). So the pricing work is frontend-only: a formatter in the `models` Alpine component, a new table column, and translation keys. The width work is a single CSS value in two files (source + compiled artifact; the repo has no Tailwind build pipeline, so both must be edited by hand, always together).

**Tech Stack:** Go (net/http, embedded static assets), Alpine.js + vanilla JS components, Tailwind (pre-compiled `style.css` + hand-synced `src/input.css`), i18n via `en.js`/`pt.js` dictionaries with `|| 'fallback'` inline in markup.

**Spec:** No standalone spec — this plan is the brief. Design intent (Operate mode, existing flat dark "space/neon" world, no visual redesign): reuse existing tokens and classes; the routing table gains a `Price` column showing input / output price per 1M tokens; the settings container grows from 900px to 1400px, matching the `view-container` width used by dashboard/models/accounts views.

## Global Constraints

- No backend changes. The pricing contract test (Task 1) only locks existing behavior.
- No new dependencies; no Tailwind toolchain in the repo — edit BOTH `internal/webui/public/css/src/input.css` AND the compiled `internal/webui/public/css/style.css` for CSS changes.
- Dark theme only: use the same utility classes as sibling cells (`font-mono text-xs text-gray-300`); never invent new colors.
- Both locales updated: `internal/webui/public/js/translations/en.js` and `pt.js`. Markup keeps the `$store.global.t('key') || 'Fallback'` pattern.
- `settings.html` markup uses double quotes for HTML attributes; JS component code matches surrounding style (4-space indent, trailing commas).
- PRs target the fork `gustavokch/antigravity-claude-proxy-go` (branch from `main`).
- Do not touch anything outside the files listed per task.

---

### Task 1: Lock the API pricing contract in the handler test

**Files:**
- Modify: `internal/api/openrouter_routing_test.go` (inside `TestManagement_OpenRouterProvidersEndpoint`, after the stats assertion at lines 460-462)

**Interfaces:**
- Consumes: existing mock upstream fixture — `p1` endpoint carries `"pricing": {"prompt": "0.000003", "completion": "0.000015"}` (string values, exercising `Pricing.UnmarshalJSON`), `p2` carries no pricing (lines 411-414).
- Produces: confidence that `p.endpoint.pricing.{prompt,completion}` reaches the UI. No production code changes.

- [ ] **Step 1: Add the assertions**

In `TestManagement_OpenRouterProvidersEndpoint`, replace:

```go
	if _, hasStats := first["stats"].(map[string]any); !hasStats {
		t.Errorf("expected stats object on provider entry")
	}
```

with:

```go
	if _, hasStats := first["stats"].(map[string]any); !hasStats {
		t.Errorf("expected stats object on provider entry")
	}

	// The routing table renders pricing per provider from the served
	// endpoint object; lock the contract.
	ep, ok := first["endpoint"].(map[string]any)
	if !ok {
		t.Fatalf("expected endpoint object on provider entry")
	}
	pricing, ok := ep["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("expected pricing object on p1 endpoint")
	}
	if pr, ok := pricing["prompt"].(float64); !ok || pr != 0.000003 {
		t.Errorf("expected p1 prompt pricing 0.000003, got %v", pricing["prompt"])
	}
	if pc, ok := pricing["completion"].(float64); !ok || pc != 0.000015 {
		t.Errorf("expected p1 completion pricing 0.000015, got %v", pricing["completion"])
	}
	second, _ := providers[1].(map[string]any)
	if ep2, ok := second["endpoint"].(map[string]any); ok {
		if _, has := ep2["pricing"]; has {
			t.Errorf("expected no pricing object on p2 endpoint (absent upstream)")
		}
	}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/api/ -run TestManagement_OpenRouterProvidersEndpoint -v`
Expected: PASS. (This is a contract lock, not red-green — the handler already forwards `Endpoint`, which includes `Pricing *Pricing` with tag `json:"pricing,omitempty"`. If it FAILS, stop: the API surface regressed and the frontend plan is invalid.)

- [ ] **Step 3: Commit**

```bash
git add internal/api/openrouter_routing_test.go
git commit -m "test(api): lock pricing passthrough in OpenRouter providers response"
```

---

### Task 2: Price column in the provider-routing table

**Files:**
- Modify: `internal/webui/public/js/components/models.js` (insert after `formatUptime`, before `async fetchOpenRouterConfig() {`)
- Modify: `internal/webui/public/views/settings.html:1144-1165` (table header + row cells; also wrap table for overflow)
- Modify: `internal/webui/public/js/translations/en.js:555` (after `tps: "TPS",`)
- Modify: `internal/webui/public/js/translations/pt.js:484` (after `tps: "TPS",`)

**Interfaces:**
- Consumes: `/api/openrouter/providers` payload — `providers[].endpoint.pricing.{prompt,completion}` as USD per token (`Pricing` struct at `internal/openrouter/pricing.go:11`; serialized via `providerEntry.Endpoint` in `internal/api/management.go:1208`).
- Produces: `formatPrice(entry)` on the `models` Alpine component; table column header using translation key `price`.

- [ ] **Step 1: Add the `formatPrice` helper**

In `internal/webui/public/js/components/models.js`, `formatUptime` ends with `    },` followed by a blank line, then `    async fetchOpenRouterConfig() {`. Between them, insert:

```js
    // Format a provider's OpenRouter pricing (USD per token) as
    // "$in / $out" per 1M tokens. Missing pricing renders '-' and a zero
    // price renders 'FREE' (OpenRouter free variants).
    formatPrice(entry) {
        const pr = entry && entry.endpoint && entry.endpoint.pricing;
        if (!pr) return '-';
        const perM = (v) => {
            if (typeof v !== 'number' || !isFinite(v)) return '-';
            const m = v * 1e6;
            if (m === 0) return 'FREE';
            if (m >= 0.1) return '$' + m.toFixed(2);
            return '$' + parseFloat(m.toFixed(4));
        };
        return perM(pr.prompt) + ' / ' + perM(pr.completion);
    },

```

- [ ] **Step 2: Syntax-check the component**

Run: `node --check internal/webui/public/js/components/models.js`
Expected: exit 0, no output. (The repo has no JS test harness — this plus the manual check in Task 4 is the verification.)

- [ ] **Step 3: Add translation keys**

In `internal/webui/public/js/translations/en.js`, after `    tps: "TPS",` add:

```js
    price: "Price",
```

In `internal/webui/public/js/translations/pt.js`, after `    tps: "TPS",` add:

```js
    price: "Preço",
```

- [ ] **Step 4: Add the header cell**

In `internal/webui/public/views/settings.html`, replace:

```html
                                                                    <th x-text="$store.global.t('tps') || 'TPS'">TPS</th>
                                                                    <th x-text="$store.global.t('score') || 'Score'">Score</th>
```

with:

```html
                                                                    <th x-text="$store.global.t('tps') || 'TPS'">TPS</th>
                                                                    <th x-text="$store.global.t('price') || 'Price'">Price</th>
                                                                    <th x-text="$store.global.t('score') || 'Score'">Score</th>
```

- [ ] **Step 5: Add the body cell**

In the same file, replace:

```html
                                                                        <td class="font-mono text-xs text-gray-300" x-text="p.stats && p.stats.tpsEWMA ? p.stats.tpsEWMA.toFixed(1) : '-'"></td>
                                                                        <td class="font-mono text-xs text-neon-purple" x-text="p.score != null ? p.score.toFixed(3) : '-'"></td>
```

with:

```html
                                                                        <td class="font-mono text-xs text-gray-300" x-text="p.stats && p.stats.tpsEWMA ? p.stats.tpsEWMA.toFixed(1) : '-'"></td>
                                                                        <td class="font-mono text-xs text-gray-300" title="USD per 1M tokens — input / output" x-text="formatPrice(p)"></td>
                                                                        <td class="font-mono text-xs text-neon-purple" x-text="p.score != null ? p.score.toFixed(3) : '-'"></td>
```

- [ ] **Step 6: Wrap the table for narrow viewports**

Nine columns overflow a narrow window. Wrap the routing table in an `overflow-x-auto` container (established convention — see `views/models.html:118`). Replace:

```html
                                                    <template x-if="getRoutingPanel(item.id).data">
                                                        <table class="standard-table">
```

with:

```html
                                                    <template x-if="getRoutingPanel(item.id).data">
                                                        <div class="overflow-x-auto">
                                                        <table class="standard-table">
```

and replace the closing pair (note: this is the DEEPLY indented one at lines 1187-1189, inside the routing panel — the allowlist table above has the same tags at a shallower indent):

```html
                                                            </tbody>
                                                        </table>
                                                    </template>
```

with:

```html
                                                            </tbody>
                                                        </table>
                                                        </div>
                                                    </template>
```

- [ ] **Step 7: Commit**

```bash
git add internal/webui/public/js/components/models.js internal/webui/public/views/settings.html internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js
git commit -m "feat(webui): show in/out pricing per provider in OpenRouter routing table"
```

---

### Task 3: Widen the System Configuration card

**Files:**
- Modify: `internal/webui/public/css/src/input.css:196` (`.view-container-centered` max-width)
- Modify: `internal/webui/public/css/style.css` (same rule in the compiled artifact)

**Interfaces:**
- Consumes: nothing.
- Produces: `.view-container-centered` at `max-width: 1400px`, matching `.view-container` used by the dashboard/models/accounts views. The class is referenced only by `settings.html`, so this change is scoped to the settings page — and the whole settings page is the System Configuration card (one tabbed card), so this is exactly "widen the System Configuration card".

- [ ] **Step 1: Edit the source CSS**

In `internal/webui/public/css/src/input.css`, inside `.view-container-centered`, change:

```css
  max-width: 900px;
```

to:

```css
  max-width: 1400px;
```

- [ ] **Step 2: Edit the compiled artifact identically**

In `internal/webui/public/css/style.css`, change:

```css
.view-container-centered{margin-left:auto;margin-right:auto;max-width:900px}
```

to:

```css
.view-container-centered{margin-left:auto;margin-right:auto;max-width:1400px}
```

(Leave the shared `.view-container-centered,.view-container-full{...}` rule untouched — it has no width.)

- [ ] **Step 3: Verify the two files agree**

Run: `grep -o "view-container-centered{[^}]*}" internal/webui/public/css/style.css`
Expected output contains `max-width:1400px`. Also confirm `src/input.css` line under `.view-container-centered` reads `max-width: 1400px;`.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/public/css/src/input.css internal/webui/public/css/style.css
git commit -m "feat(webui): widen settings container to 1400px to use horizontal space"
```

---

### Task 4: End-to-end verification

**Files:** none (verification only).

- [ ] **Step 1: Build and run unit tests**

Run: `go build ./... && go test ./internal/api/ -run OpenRouter -v`
Expected: build OK, all OpenRouter tests PASS (including the extended contract test).

- [ ] **Step 2: Run the proxy and open the WebUI**

Run: `go run ./cmd/proxy` (default listen `127.0.0.1:8080`). Open the management UI, go to `/#settings/models`, expand an OpenRouter allowlist item, and set routing mode to anything that loads the provider table.

- [ ] **Step 3: Verify pricing (desktop viewport)**

Check in the provider table:
- A `Price` column sits between `TPS` and `Score`.
- Paid providers render like `$3.00 / $15.00`; cheap ones like `$0.03 / $0.15` (4 decimals below $0.1); free providers render `FREE / FREE`.
- Providers whose upstream reports no pricing render `-` (single dash, not the `in / out` pair).
- Hovering a price cell shows the tooltip `USD per 1M tokens — input / output`.

- [ ] **Step 4: Verify layout width**

- The System Configuration card now spans the same width as the Dashboard/Models views (1400px) on a wide window; the routing table columns no longer crowd.
- Narrow the window under ~900px: the settings page still fits without horizontal page scroll; the routing table scrolls inside its own container.
- Spot-check other settings tabs (General, Server) at 1400px for stretched but usable forms.

- [ ] **Step 5: Verify the pt locale**

Switch UI language to Portuguese (or inspect `pt.js` rendering) and confirm the column header shows `Preço` rather than a missing-key blank.

---

## Self-Review Notes

- Spec coverage: both requested changes map to Task 2 (pricing) and Task 3 (width); Task 1 protects the data dependency of Task 2; Task 4 verifies.
- Type consistency: `formatPrice(entry)` consumed in the template as `formatPrice(p)` where `p` is the `providerEntry` JSON (`{provider, tag, contextLength, uptime, score, endpoint: {pricing: {prompt, completion}, ...}, stats}`) — matches `internal/api/management.go:1202-1210` and `internal/openrouter/endpoints.go` (`Pricing *Pricing` json `pricing,omitempty`).
- No placeholders: all edit anchors were read verbatim from the current files (line numbers quoted from the working tree at plan time).
