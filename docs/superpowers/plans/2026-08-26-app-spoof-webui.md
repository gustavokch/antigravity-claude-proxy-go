# OR Harness-Gate Spoof Settings in WebUI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a collapsible App Spoof panel to the OpenRouter Gateway card in the WebUI Settings → Models view so operators can edit `openrouter.appSpoof.title` and `openrouter.appSpoof.categories` from the console without hand-editing `config.json`.

**Architecture:** Extend the existing `openRouterConfig` Alpine model in `models.js` with an `appSpoof: { title, categories }` sub-object; render a new `<details>` block inside the OpenRouter card in `views/settings.html` with two text inputs bound to those fields; round-trip the object through the existing `GET/POST /api/openrouter/config` endpoints (the public config sanitizer already passes `appSpoof` through unchanged — `apiKey` is the only redacted field); add i18n keys for the four locales already shipped.

**Tech Stack:** Alpine.js (x-data, x-model, x-show, x-cloak), Tailwind utility classes, existing `Alpine.store('global').t()` i18n, existing `window.utils.request()` wrapper for fetch. No new dependencies.

**Spec:** `docs/superpowers/plans/2026-08-26-app-spoof-webui.md` (this file).
**Related:** `internal/openrouter/harness.go` (server-side defaults), `internal/config/config.go` (`OpenRouterAppSpoofConfig`), `internal/api/management.go` (`handleOpenRouterConfigGet/Save`).

## Global Constraints

- All new strings go through `$store.global.t(key)` with English fallback to the literal in `x-text` (mirroring existing `||` fallback pattern).
- All four locales (`en`, `zh`, `tr`, `id`, `pt`) must define every new key — `internal/webui/translations_test.go` `TestTranslations_OpenRouterKeys` enforces this when keys are added to its allowlist.
- No new files unless listed below. No new script tags. No new endpoints (existing `GET/POST /api/openrouter/config` round-trips the whole `openrouter` map).
- The visible UI must remain the existing OpenRouter card (purple accent). No new color tokens.
- The defaults rendered on first load must equal the server defaults: `Title="Claude Code"`, `Categories="cli-agent"` (from `openrouter.DefaultSpoofAppTitle` / `DefaultSpoofAppCategories`).

---

## Task 1: Add `appSpoof` to the `openRouterConfig` Alpine model

**Files:**
- Modify: `internal/webui/public/js/components/models.js:303-310` (the `openRouterConfig` initializer)
- Modify: `internal/webui/public/js/components/models.js:416-437` (`fetchOpenRouterConfig` — populate from server)
- Modify: `internal/webui/public/js/components/models.js:439-484` (`saveOpenRouterConfig` — include in payload)

**Interfaces:**
- Consumes: `GET /api/openrouter/config` returns `data.config.appSpoof: { title: string, categories: string }` when set on the server (the public-config sanitizer passes it through; see `internal/config/config.go:393-405`).
- Produces: `this.openRouterConfig.appSpoof = { title: string, categories: string }` on the Alpine component instance; `payload.appSpoof` on `POST /api/openrouter/config` only when at least one field is non-empty (mirrors how `apiKey` is omitted when blank to avoid clobbering the saved value).

- [ ] **Step 1: Extend the initializer**

In `models.js:303-310`, replace the `openRouterConfig` object literal so it includes the `appSpoof` sub-object with empty strings:

```js
    openRouterConfig: {
        enabled: false,
        baseUrl: 'https://openrouter.ai/api',
        apiKey: '',
        hasApiKey: false,
        allowlist: [],
        routing: null,
        appSpoof: {
            title: '',
            categories: ''
        }
    },
```

- [ ] **Step 2: Populate from server**

In `models.js:424-431` (inside the `if (data.config)` branch of `fetchOpenRouterConfig`), extend the assignment so `appSpoof` is read from the server response, defaulting both fields to `''` when absent:

```js
                this.openRouterConfig = {
                    enabled: !!data.config.enabled,
                    baseUrl: data.config.baseUrl || 'https://openrouter.ai/api',
                    apiKey: '',
                    hasApiKey: !!data.config.hasApiKey,
                    allowlist: data.config.allowlist || [],
                    routing: data.config.routing || null,
                    appSpoof: {
                        title: (data.config.appSpoof && data.config.appSpoof.title) || '',
                        categories: (data.config.appSpoof && data.config.appSpoof.categories) || ''
                    }
                };
```

- [ ] **Step 3: Include in save payload**

In `models.js:445-450` (inside `saveOpenRouterConfig`, the `payload` literal), append the `appSpoof` block. Only send it when at least one field is non-empty — this keeps the save idempotent for users who never touched the panel and avoids wiping the saved config with empty strings:

```js
            const payload = {
                enabled: this.openRouterConfig.enabled,
                baseUrl: this.openRouterConfig.baseUrl,
                hasApiKey: this.openRouterConfig.hasApiKey,
                allowlist: this.openRouterConfig.allowlist || []
            };
            const spoof = this.openRouterConfig.appSpoof || {};
            if ((spoof.title && spoof.title.trim()) || (spoof.categories && spoof.categories.trim())) {
                payload.appSpoof = {
                    title: (spoof.title || '').trim(),
                    categories: (spoof.categories || '').trim()
                };
            }
```

Place this `if` block immediately after the `routing` block at `models.js:453-455`.

- [ ] **Step 4: Run the webui tests to verify nothing broke**

Run: `cd /Users/gus/Git/antigravity-claude-proxy-go && go test ./internal/webui/...`
Expected: PASS (the existing tests don't assert on `openRouterConfig` shape; they assert on translations and embed).

- [ ] **Step 5: Commit**

```bash
git add internal/webui/public/js/components/models.js
git commit -m "feat(webui): round-trip openrouter appSpoof through the Alpine config model"
```

---

## Task 2: Render the App Spoof panel in `views/settings.html`

**Files:**
- Modify: `internal/webui/public/views/settings.html:1006` (immediately after the closing `</div>` of the Base URL + API Key grid, before the `<!-- OpenRouter Allowlist Table -->` block)

**Interfaces:**
- Consumes: `this.openRouterConfig.appSpoof.title`, `this.openRouterConfig.appSpoof.categories` (set in Task 1); i18n keys defined in Task 3.
- Produces: Two `<input type="text">` elements bound via `x-model`, a chevron-collapsed details block that defaults to open when either field is non-empty, and a status chip showing "default" (dim) or "custom" (green).

- [ ] **Step 1: Insert the panel**

After the closing `</div>` of the Base URL + API Key grid at `views/settings.html:1006` (the line that reads `</div>` and is followed by the `<!-- OpenRouter Allowlist Table -->` comment), insert the following block. Match the existing card's `bg-space-900/30 border border-neon-purple/40` palette, the `input input-sm input-bordered bg-space-800 border-space-border text-white font-mono text-xs` input tokens, and the `btn btn-sm bg-neon-purple` save-button styling. Keep the `text-[10px] font-semibold text-gray-400 uppercase tracking-wider` label convention from the allowlist section.

```html
                    <!-- App Spoof (harness-gated model retry headers) -->
                    <div class="pt-2 border-t border-space-border/40">
                        <details class="group" :open="(openRouterConfig.appSpoof.title || openRouterConfig.appSpoof.categories) || null">
                            <summary class="cursor-pointer flex items-center gap-2 select-none py-2">
                                <svg xmlns="http://www.w3.org/2000/svg" class="w-3 h-3 text-gray-400 transition-transform group-open:rotate-90" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7" />
                                </svg>
                                <span class="text-[10px] font-semibold text-gray-400 uppercase tracking-wider" x-text="$store.global.t('appSpoofTitle') || 'Harness-Gate Spoof'">Harness-Gate Spoof</span>
                                <span class="badge badge-xs font-mono text-[9px]"
                                    :class="(openRouterConfig.appSpoof.title || openRouterConfig.appSpoof.categories) ? 'bg-neon-green/10 text-neon-green border-neon-green/30' : 'bg-space-800 text-gray-500 border-space-border'"
                                    x-text="(openRouterConfig.appSpoof.title || openRouterConfig.appSpoof.categories) ? ($store.global.t('appSpoofCustom') || 'CUSTOM') : ($store.global.t('appSpoofDefault') || 'DEFAULT')">DEFAULT</span>
                            </summary>
                            <div class="space-y-3 pb-2">
                                <p class="text-[11px] text-gray-500 leading-relaxed" x-text="$store.global.t('appSpoofDesc') || 'Headers sent on the retry of harness-gated free models (X-OpenRouter-Title / X-OpenRouter-Categories). Leave both blank to use the built-in defaults.'">
                                    Headers sent on the retry of harness-gated free models (X-OpenRouter-Title / X-OpenRouter-Categories). Leave both blank to use the built-in defaults.
                                </p>
                                <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
                                    <div class="form-control">
                                        <label class="label pt-0 pb-1 text-xs text-gray-400 font-semibold" x-text="$store.global.t('appSpoofFieldTitle') || 'App Title'">App Title</label>
                                        <input type="text" x-model="openRouterConfig.appSpoof.title"
                                            class="input input-sm input-bordered bg-space-800 border-space-border text-white font-mono text-xs w-full"
                                            placeholder="Claude Code">
                                    </div>
                                    <div class="form-control">
                                        <label class="label pt-0 pb-1 text-xs text-gray-400 font-semibold" x-text="$store.global.t('appSpoofFieldCategories') || 'App Categories'">App Categories</label>
                                        <input type="text" x-model="openRouterConfig.appSpoof.categories"
                                            class="input input-sm input-bordered bg-space-800 border-space-border text-white font-mono text-xs w-full"
                                            placeholder="cli-agent">
                                    </div>
                                </div>
                            </div>
                        </details>
                    </div>
```

- [ ] **Step 2: Run the embed test**

Run: `cd /Users/gus/Git/antigravity-claude-proxy-go && go test ./internal/webui/...`
Expected: PASS (no new keys added yet to the translation test, so this just confirms `views/settings.html` still embeds and the new HTML parses).

- [ ] **Step 3: Visual smoke test**

Open the WebUI, navigate to Settings → Models, scroll to the OpenRouter card. Verify: panel renders under the Base URL / API Key grid; chevron rotates; both inputs are present with the correct placeholders; typing into either field changes the badge from DEFAULT to CUSTOM; saving (clicking the existing purple Save button) persists the values.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/public/views/settings.html
git commit -m "feat(webui): render appSpoof panel under OpenRouter Base URL/Key grid"
```

---

## Task 3: Add i18n keys for the four locales

**Files:**
- Modify: `internal/webui/public/js/translations/en.js:512` (append to the `// OpenRouter Gateway & Discovery` block)
- Modify: `internal/webui/public/js/translations/zh.js`, `tr.js`, `id.js`, `pt.js` (add the same five keys, translated)
- Modify: `internal/webui/translations_test.go:13-17` (extend the `openRouterPanelKeys` allowlist)

**Interfaces:**
- Consumes: nothing (pure data file).
- Produces: Five keys: `appSpoofTitle`, `appSpoofDefault`, `appSpoofCustom`, `appSpoofDesc`, `appSpoofFieldTitle`, `appSpoofFieldCategories`. The first four are user-facing panel labels; the last two are the field labels.

- [ ] **Step 1: Add English keys**

In `en.js`, after the line `gatewayDiscoveryDesc: "Allows Claude Code to discover allowlisted OpenRouter and proxy models dynamically.",` (line 525), add:

```js
    // App Spoof (harness-gate retry headers)
    appSpoofTitle: "Harness-Gate Spoof",
    appSpoofDefault: "DEFAULT",
    appSpoofCustom: "CUSTOM",
    appSpoofDesc: "Headers sent on the retry of harness-gated free models (X-OpenRouter-Title / X-OpenRouter-Categories). Leave both blank to use the built-in defaults.",
    appSpoofFieldTitle: "App Title",
    appSpoofFieldCategories: "App Categories",
```

- [ ] **Step 2: Add the same keys to the other four locales**

Append the same six keys to `zh.js`, `tr.js`, `id.js`, `pt.js`. Place them in the same relative position (inside the OpenRouter Gateway & Discovery block; if a block delimiter is missing, place immediately after the `gatewayDiscoveryDesc` line, or at the end of the object if no equivalent line exists). Suggested translations:
  - `zh.js`: "Harness-Gate 欺骗" / "默认" / "自定义" / "在触发 harness-gate 重试时发送给 OpenRouter 的归属头（X-OpenRouter-Title / X-OpenRouter-Categories）。两项留空则使用内置默认值。" / "应用标题" / "应用分类"
  - `tr.js`: "Harness-Gate Spoof" / "VARSAYILAN" / "ÖZEL" / "Harness-gate'lenen ücretsiz modellerin yeniden denemesinde gönderilen başlıklar (X-OpenRouter-Title / X-OpenRouter-Categories). Yerleşik varsayılanları kullanmak için her iki alanı da boş bırakın." / "Uygulama Başlığı" / "Uygulama Kategorileri"
  - `id.js`: "Harness-Gate Spoof" / "DEFAULT" / "KUSTOM" / "Header yang dikirim saat percobaan ulang model gratis yang di-gate harness (X-OpenRouter-Title / X-OpenRouter-Categories). Biarkan kosong untuk memakai bawaan." / "Judul Aplikasi" / "Kategori Aplikasi"
  - `pt.js`: "Harness-Gate Spoof" / "PADRÃO" / "PERSONALIZADO" / "Cabeçalhos enviados na retentativa de modelos gratuitos bloqueados por harness (X-OpenRouter-Title / X-OpenRouter-Categories). Deixe ambos em branco para usar os padrões internos." / "Título do App" / "Categorias do App"

(Translations may be refined in review — the values used here are correct and idiomatic placeholders.)

- [ ] **Step 3: Extend the translation test allowlist**

In `internal/webui/translations_test.go:13-17`, replace the `openRouterPanelKeys` slice so the new panel keys are enforced for every locale:

```go
var openRouterPanelKeys = []string{
	"save", "remove", "alias", "localAlias", "loading", "contextLength",
	"providerRouting", "provider", "uptime", "latency", "tps", "score",
	"order", "pin", "pinned", "pinnedTo", "openRouterBadge",
	"appSpoofTitle", "appSpoofDefault", "appSpoofCustom", "appSpoofDesc",
	"appSpoofFieldTitle", "appSpoofFieldCategories",
}
```

- [ ] **Step 4: Run the translation test**

Run: `cd /Users/gus/Git/antigravity-claude-proxy-go && go test ./internal/webui/... -run TestTranslations_OpenRouterKeys -v`
Expected: PASS for all five locales. FAIL with a clear "locale XX missing key YY" if any translation was skipped.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/public/js/translations/en.js internal/webui/public/js/translations/zh.js internal/webui/public/js/translations/tr.js internal/webui/public/js/translations/id.js internal/webui/public/js/translations/pt.js internal/webui/translations_test.go
git commit -m "feat(webui): add appSpoof i18n keys for en/zh/tr/id/pt"
```

---

## Task 4: End-to-end verification

**Files:** none (read-only smoke test).

- [ ] **Step 1: Start the proxy with a fresh config dir**

```bash
cd /Users/gus/Git/antigravity-claude-proxy-go
ANTIGRAVITY_CONFIG_DIR=$(mktemp -d) go run ./cmd/... &
sleep 2
```

- [ ] **Step 2: Hit the API without appSpoof, confirm defaults ride through**

```bash
curl -sS -u :$PASSWORD http://localhost:8080/api/openrouter/config | jq '.config.appSpoof'
```
Expected: `null` (no `appSpoof` block in the public config until the user sets it).

- [ ] **Step 3: POST a custom appSpoof, confirm round-trip**

```bash
curl -sS -u :$PASSWORD -X POST -H 'Content-Type: application/json' \
  -d '{"appSpoof":{"title":"My Harness","categories":"cli-agent,cloud-agent"}}' \
  http://localhost:8080/api/openrouter/config | jq '.config.appSpoof'
```
Expected: `{"title":"My Harness","categories":"cli-agent,cloud-agent"}`.

- [ ] **Step 4: Run the OR routing unit test for the custom-spoof case**

```bash
cd /Users/gus/Git/antigravity-claude-proxy-go
go test ./internal/api/... -run TestOpenRouterForwarding_HarnessGateCustomSpoofConfig -v
```
Expected: PASS (this test was added in the prior commit; it now also exercises the path the WebUI just opened up).

- [ ] **Step 5: Manually verify the WebUI round-trip**

1. Open the WebUI, Settings → Models.
2. Expand "Harness-Gate Spoof" under the OpenRouter card.
3. Type `My Harness` into App Title, `cli-agent,cloud-agent` into App Categories.
4. Badge switches from DEFAULT to CUSTOM.
5. Click Save. Reload the page. Inputs retain the values. Badge stays CUSTOM.
6. Clear both inputs. Save. Reload. Inputs are blank, badge is DEFAULT. The saved config still carries the values (empty inputs are not sent; server keeps whatever was last set).

- [ ] **Step 6: Stop the proxy, clean up**

```bash
kill %1
rm -rf $ANTIGRAVITY_CONFIG_DIR
```

---

## Self-Review

- **Spec coverage:** Backend defaults (`Claude Code` / `cli-agent`) match `openrouter.DefaultSpoofAppTitle` / `DefaultSpoofAppCategories`. Public-config sanitizer passes `appSpoof` through unchanged — verified at `internal/config/config.go:393-405`. The `forwardToOpenRouter` server-side handler at `internal/api/server.go:717-727,809-812,903-907` already reads `openRouterCfg.AppSpoof.{Title,Categories}` with empty-string fallback, so empty `appSpoof` from the WebUI correctly yields the server defaults. No server changes needed.
- **Type consistency:** `openRouterConfig.appSpoof` is defined once at `models.js:303-310`, populated at `models.js:424-432`, persisted at `models.js:445-455`, and consumed at `views/settings.html` panel. All four sites agree on shape `{ title: string, categories: string }`.
- **No placeholders:** every step has the actual code, file path, line range, and run command. No "TBD" or "add appropriate error handling."
- **i18n discipline:** all five user-facing strings use `$store.global.t(key)` with English fallback in the literal `x-text` attribute, matching the existing pattern in the same file.
- **Idempotency:** the save payload only includes `appSpoof` when at least one field is non-empty, so users who never expand the panel never overwrite a saved config with blanks.

**Plan complete and saved to `docs/superpowers/plans/2026-08-26-app-spoof-webui.md`.**
