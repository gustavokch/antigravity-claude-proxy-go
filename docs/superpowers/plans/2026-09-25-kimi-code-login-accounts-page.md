# Kimi Code Login on Accounts Page — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the Kimi Code device-flow login as a third provider tab in the accounts page "Add Account" modal, following the existing Google / Claude Code provider pattern.

**Architecture:** The backend device-flow endpoints (`/api/kimi/auth/start|status|cancel`) already exist (merged in PR #99, commit `1d7847e`) and are consumed today only by the settings page (`models.js` + `settings.html`). This plan ports that flow into the shared `add_account_modal` (defined in `index.html`, component in `add-account-modal.js`) as an inline panel — no nested dialog, no backend changes. On completion the modal refreshes `Alpine.store('data').kimi` from `/api/kimi/config` and closes, mirroring how the Claude Code flow refreshes accounts and closes.

**Tech Stack:** Vanilla Alpine.js 3 + daisyUI/Tailwind, served as static files by the Go proxy. No JS build step, no JS test harness.

**Spec:** No standalone spec. Ground truth is the existing implementation:
- Device-flow UI reference: `internal/webui/public/js/components/models.js:716-826` (`startKimiOAuthLogin`, `_pollKimiOAuth`, `cancelKimiOAuthLogin`) and `internal/webui/public/views/settings.html:2651-2674` (`kimi_oauth_modal`).
- Provider-tab pattern: `internal/webui/public/index.html:274-459` (`add_account_modal`) + `internal/webui/public/js/components/add-account-modal.js`.
- Backend contract: `internal/api/kimi_oauth_handlers.go` (see Global Constraints for exact payloads).

## Global Constraints

- **No backend changes.** Reuse exactly:
  - `POST /api/kimi/auth/start` (body `{}`) → `{status:"ok", session_id, user_code, verification_uri, verification_uri_complete, expires_in, interval}`
  - `GET /api/kimi/auth/status?session_id=<id>` → `{status:"pending"}` | `{status:"completed", account:{email,user_id,nickname,expires_at}}` | `{status:"expired"|"denied"|"cancelled"|"error", error}`
  - `POST /api/kimi/auth/cancel` (body `{session_id}`) → best-effort
  - `GET /api/kimi/config` → `{config:{enabled, baseUrl, hasApiKey, allowlist, oauth: {email,userId,nickname,expiresAt} | null}}`
- All API calls MUST go through `window.utils.request(url, opts, Alpine.store('global').webuiPassword)` and propagate `newPassword` back to the store — the established pattern in every existing flow.
- Kimi Code login is a **global singleton credential** (not a pooled account). The modal tab must not pretend it adds an account row; on success it refreshes `Alpine.store('data').kimi` only.
- Only one device login can run at a time server-side (commit `9682a85`). Closing the modal or switching provider tabs mid-flow MUST best-effort POST cancel.
- Translations: only `en.js` and `pt.js` exist. New keys go in both; reuse existing `kimiOAuth*` keys wherever they fit.
- No JS test harness exists in this repo — verification is `node --check` syntax gate plus a real browser smoke against the running proxy (Task 3). Do not invent a test framework.
- No Go files change; the gofmt pre-commit hook is irrelevant here but commits still go through it.

---

### Task 1: Kimi device-flow logic in the add-account modal component

**Files:**
- Modify: `internal/webui/public/js/components/add-account-modal.js`

**Interfaces:**
- Consumes: backend endpoints listed in Global Constraints; `Alpine.store('global').webuiPassword/showToast/t`; `Alpine.store('data').kimi`.
- Produces (used by Task 2 markup): state `kimiStarting: boolean`, `kimiOAuth: { sessionId, userCode, verificationUri, status, error, polling }`; methods `startKimiLogin()`, `setProvider(prov)` (extended), `resetState()` (extended). Private: `_pollKimiLogin()`, `_cancelKimiSession()`, `_refreshKimiStore()`.

- [x] **Step 1: Add Kimi state fields**

In `window.Components.addAccountModal = () => ({ ... })`, after the `claudeCodeSubmitting: false,` line add:

```js
    // Kimi Code Device-Flow Login State
    kimiStarting: false,
    kimiOAuth: { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false },
```

Also update the `provider` comment on line 8 from `// 'google' | 'claudecode'` to `// 'google' | 'claudecode' | 'kimi'`.

- [x] **Step 2: Extend `resetState()` to cancel and clear Kimi state**

At the top of `resetState()` (before `this.provider = 'google';`) add:

```js
        this._cancelKimiSession();
        this.kimiStarting = false;
        this.kimiOAuth = { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false };
```

(`_cancelKimiSession` is defined in Step 4; it is a no-op unless a session is pending, so calling it on every reset is safe and idempotent.)

- [x] **Step 3: Extend `setProvider()` to cancel when leaving the Kimi tab**

Replace the existing `setProvider` body with:

```js
    setProvider(prov) {
        if (this.provider === 'kimi' && prov !== 'kimi' && this.kimiOAuth.status === 'pending') {
            this._cancelKimiSession();
            this.kimiOAuth = { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false };
        }
        this.provider = prov;
    },
```

- [x] **Step 4: Add the Kimi device-flow methods**

Append after `completeClaudeCodeManualAuth()` (before the closing `});`):

```js
    ,

    // --- Kimi Code Device-Flow Login Methods ---
    async startKimiLogin() {
        const store = Alpine.store('global');
        this.kimiStarting = true;
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/auth/start', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: '{}'
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json().catch(() => ({}));
            if (!response.ok || data.status !== 'ok') {
                store.showToast(data.error || `HTTP ${response.status}`, 'error');
                return;
            }
            this.kimiOAuth.sessionId = data.session_id || '';
            this.kimiOAuth.userCode = data.user_code || '';
            this.kimiOAuth.verificationUri = data.verification_uri_complete || data.verification_uri || '';
            this.kimiOAuth.status = 'pending';
            this.kimiOAuth.error = '';
            this.kimiOAuth.polling = true;
            this._pollKimiLogin();
        } catch (e) {
            store.showToast(e.message || 'Failed to start Kimi Code login', 'error');
        } finally {
            this.kimiStarting = false;
        }
    },

    async _pollKimiLogin() {
        const store = Alpine.store('global');
        while (this.kimiOAuth.polling) {
            await new Promise(resolve => setTimeout(resolve, 2000));
            if (!this.kimiOAuth.polling) return;
            try {
                const { response, newPassword } = await window.utils.request(
                    `/api/kimi/auth/status?session_id=${encodeURIComponent(this.kimiOAuth.sessionId)}`,
                    {}, store.webuiPassword);
                if (newPassword) store.webuiPassword = newPassword;
                const data = await response.json().catch(() => ({}));
                if (!response.ok) {
                    this.kimiOAuth.status = 'error';
                    this.kimiOAuth.error = data.error || `HTTP ${response.status}`;
                    this.kimiOAuth.polling = false;
                    return;
                }
                if (data.status === 'completed') {
                    this.kimiOAuth.status = 'completed';
                    this.kimiOAuth.polling = false;
                    const email = data.account && (data.account.email || data.account.nickname || data.account.user_id);
                    store.showToast(
                        (store.t('kimiOAuthSuccess') || 'Kimi Code login complete') + (email ? ': ' + email : ''),
                        'success');
                    await this._refreshKimiStore();
                    document.getElementById('add_account_modal')?.close();
                    this.resetState();
                    return;
                }
                if (['expired', 'denied', 'cancelled', 'error'].includes(data.status)) {
                    this.kimiOAuth.status = data.status;
                    this.kimiOAuth.error = data.error || '';
                    this.kimiOAuth.polling = false;
                    return;
                }
            } catch (e) {
                this.kimiOAuth.status = 'error';
                this.kimiOAuth.error = e.message || 'Login status check failed';
                this.kimiOAuth.polling = false;
                return;
            }
        }
    },

    async _cancelKimiSession() {
        if (this.kimiOAuth.status !== 'pending' || !this.kimiOAuth.sessionId) {
            this.kimiOAuth.polling = false;
            return;
        }
        const store = Alpine.store('global');
        const sessionId = this.kimiOAuth.sessionId;
        this.kimiOAuth.polling = false;
        this.kimiOAuth.status = 'cancelled';
        try {
            const { newPassword } = await window.utils.request('/api/kimi/auth/cancel', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ session_id: sessionId })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
        } catch (e) {
            // Best-effort; the session expires on its own.
        }
    },

    async _refreshKimiStore() {
        const store = Alpine.store('global');
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/config', {}, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) return;
            const data = await response.json();
            if (data.config) Alpine.store('data').kimi = data.config;
        } catch (e) {
            // Non-fatal: the settings page refetches the config on load.
        }
    }
```

Note the leading `,` on the first line — `completeClaudeCodeManualAuth()` currently ends without a trailing comma before `});`.

Ordering detail that matters: in `_pollKimiLogin` the `completed` branch sets `status = 'completed'` **before** `resetState()` runs, so `_cancelKimiSession()` inside `resetState()` no-ops and we never cancel an already-completed login.

- [x] **Step 5: Syntax-check the file**

Run: `node --check internal/webui/public/js/components/add-account-modal.js`
Expected: no output, exit 0. (If `node` is unavailable: `bun --check` is not a thing — use `bun -e "import('./internal/webui/public/js/components/add-account-modal.js')"`. The script touches only `window`, so stub first if needed; `node --check` is preferred.)

- [x] **Step 6: Commit**

```bash
git add internal/webui/public/js/components/add-account-modal.js
git commit -m "feat(webui): add Kimi Code device-flow logic to add-account modal"
```

---

### Task 2: Kimi provider tab markup + translations

**Files:**
- Modify: `internal/webui/public/index.html:279-291` (tab bar) and insert panel before `index.html:450` (`<div class="modal-action mt-6">`)
- Modify: `internal/webui/public/js/translations/en.js` (next to `providerClaudeCode`)
- Modify: `internal/webui/public/js/translations/pt.js` (next to the existing `kimiOAuthLogin` block — pt.js has NO `providerGoogle`/`providerClaudeCode` keys; those tabs fall back to inline English there)

**Interfaces:**
- Consumes: everything Task 1 produces (`setProvider('kimi')`, `startKimiLogin`, `kimiStarting`, `kimiOAuth.*`); translation keys `providerKimiCode`, `connectKimiCodeDesc` (new), `kimiOAuthLogin`, `kimiOAuthLoggedInAs`, `kimiOAuthEnterCode`, `kimiOAuthWaiting` (already in both locales).
- Produces: a visible third provider tab in `add_account_modal`.

- [x] **Step 1: Add the translation keys**

In `internal/webui/public/js/translations/en.js`, immediately after the `providerClaudeCode` entry add:

```js
    providerKimiCode: "Kimi Code",
    connectKimiCodeDesc: "Sign in with your Kimi Code account (kimi.ai) using device authorization. The login is global and applies to all Kimi Code gateway requests.",
```

In `internal/webui/public/js/translations/pt.js`, immediately after the `kimiOAuthLogin` entry (inside the existing Kimi key block, `kimiOAuthLogin` is at line 561) add:

```js
    providerKimiCode: "Kimi Code",
    connectKimiCodeDesc: "Entre com sua conta Kimi Code (kimi.ai) usando autorização por dispositivo. O login é global e vale para todas as requisições do gateway Kimi Code.",
```

- [x] **Step 2: Add the provider tab button**

In `internal/webui/public/index.html`, immediately after the Claude Code tab button (the `</button>` closing the `providerClaudeCode` tab, currently line 290) add:

```html
                <button type="button" class="px-4 py-2 text-sm font-medium transition-colors border-b-2"
                        :class="provider === 'kimi' ? 'text-cyan-400 border-cyan-400' : 'text-gray-400 border-transparent hover:text-gray-200'"
                        @click="setProvider('kimi')">
                    <span x-text="$store.global.t('providerKimiCode') || 'Kimi Code'">Kimi Code</span>
                </button>
```

(Cyan accent matches the settings-page Kimi section, e.g. `settings.html` `border-cyan-500/50`, `text-cyan-400`.)

- [x] **Step 3: Add the Kimi flow panel**

In `internal/webui/public/index.html`, immediately before `<div class="modal-action mt-6">` (currently line 450) add:

```html
            <!-- Kimi Code Device-Flow Login -->
            <div class="flex flex-col gap-4" x-show="provider === 'kimi'">
                <p class="text-sm text-gray-400 leading-relaxed" x-text="$store.global.t('connectKimiCodeDesc') || 'Sign in with your Kimi Code account (kimi.ai) using device authorization. The login is global and applies to all Kimi Code gateway requests.'">Sign in with your Kimi Code account (kimi.ai) using device authorization. The login is global and applies to all Kimi Code gateway requests.</p>

                <!-- Already-logged-in hint (credential is global, managed on Settings) -->
                <template x-if="$store.data.kimi && $store.data.kimi.oauth">
                    <p class="text-xs text-gray-300">
                        <span class="text-gray-500" x-text="$store.global.t('kimiOAuthLoggedInAs') || 'Logged in as'">Logged in as</span>
                        <span class="font-mono text-cyan-300" x-text="$store.data.kimi.oauth.email || $store.data.kimi.oauth.nickname || $store.data.kimi.oauth.userId"></span>
                        <span class="text-gray-500" x-show="$store.data.kimi.oauth.expiresAt" x-text="'· ' + $store.data.kimi.oauth.expiresAt"></span>
                    </p>
                </template>

                <button class="btn bg-cyan-600 hover:bg-cyan-500 text-white border-none flex items-center justify-center gap-3 h-11"
                        @click="startKimiLogin()"
                        :disabled="kimiStarting || kimiOAuth.status === 'pending'"
                        :class="{ 'loading': kimiStarting }">
                    <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <path stroke-linecap="round" stroke-linejoin="round" d="M15 7a3 3 0 11-6 0 3 3 0 016 0zM6 21v-1a4 4 0 014-4h4a4 4 0 014 4v1" />
                    </svg>
                    <span x-text="$store.global.t('kimiOAuthLogin') || 'Login with Kimi Code'">Login with Kimi Code</span>
                </button>

                <!-- Pending device authorization -->
                <template x-if="kimiOAuth.status === 'pending'">
                    <div class="p-3 bg-space-950 rounded border border-space-border text-xs">
                        <p class="text-gray-400 mb-2" x-text="$store.global.t('kimiOAuthEnterCode') || 'Open the link and confirm this code:'">Open the link and confirm this code:</p>
                        <div class="text-center my-3">
                            <div class="font-mono text-2xl tracking-widest text-white" x-text="kimiOAuth.userCode"></div>
                            <a class="link link-primary break-all" :href="kimiOAuth.verificationUri" target="_blank" rel="noopener" x-text="kimiOAuth.verificationUri"></a>
                        </div>
                        <div class="flex items-center justify-center gap-2 text-gray-400">
                            <span class="loading loading-spinner loading-xs"></span>
                            <span x-text="$store.global.t('kimiOAuthWaiting') || 'Waiting for authorization…'">Waiting for authorization…</span>
                        </div>
                    </div>
                </template>

                <p class="text-xs text-rose-400 font-mono" x-show="kimiOAuth.error" x-text="kimiOAuth.error"></p>
            </div>

```

- [x] **Step 4: Cancel pending login on native dialog close (Esc key)**

The existing Close button and backdrop both call `resetState()`, but pressing Esc fires the dialog's `close` event without either. Add `@close="resetState()"` to the dialog tag so a pending Kimi session is cancelled on Esc too:

```html
    <dialog id="add_account_modal" class="modal backdrop-blur-sm" x-data="addAccountModal" @close="resetState()">
```

This is safe for the Google/Claude Code flows: `resetState()` only clears form state, and both flows already tolerate the modal closing independently (their progress lives in `$store.global.oauthProgress`).

- [x] **Step 5: Syntax-check translations and eyeball the HTML**

Run:
```bash
node --check internal/webui/public/js/translations/en.js
node --check internal/webui/public/js/translations/pt.js
```
Expected: no output, exit 0 for both.

- [x] **Step 6: Commit**

```bash
git add internal/webui/public/index.html internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js
git commit -m "feat(webui): add Kimi Code provider tab to add-account modal"
```

---

### Task 3: Browser smoke verification

**Files:** none (verification only; fix forward in Tasks 1–2 files if a step fails).

**Interfaces:**
- Consumes: Tasks 1–2; the running proxy on port 8091.

- [x] **Step 1: Build and start the proxy**

```bash
go build -o bin/proxy ./cmd/proxy
./bin/proxy
```
Expected: build clean; proxy serving the webui on `http://localhost:8091`.

- [x] **Step 2: Verify the tab renders**

Open `http://localhost:8091` → Accounts → "Add Account" button.
Expected: three provider tabs — Google (Antigravity), Claude Code (Claude.ai), **Kimi Code**. Selecting Kimi Code shows the description, the cyan "Login with Kimi Code" button, and (if a credential already exists) the "Logged in as …" line. No console errors.

- [x] **Step 3: Verify start + pending UI**

Click "Login with Kimi Code".
Expected: button disables, a bordered panel appears with a large user code, a clickable verification link (`https://` — the backend rejects non-HTTPS URIs, commit `6f8b83c`), and the "Waiting for authorization…" spinner. DevTools Network shows `POST /api/kimi/auth/start` → 200 and repeating `GET /api/kimi/auth/status` → `{status:"pending"}` every ~2s.

- [x] **Step 4: Verify cancel paths**

While pending, close the modal via the Close button. Re-open, start again, then switch to the Google tab mid-flow. Then start once more and press Esc.
Expected each time: polling stops (no further `/api/kimi/auth/status` requests in Network), one `POST /api/kimi/auth/cancel` fires, and the proxy log shows the session being dropped (`Kimi Code auth session` cancel/drop message). Starting a fresh login after any of these succeeds — the server allows only one pending session (commit `9682a85`), so a missed cancel would surface here as a start failure.

- [x] **Step 5: Verify completion (requires a real Kimi Code account)**

Start the login, open the verification link, confirm the code.
Expected: success toast "Kimi Code login complete[: email]", modal closes, and Settings → Kimi Code shows the credential (same global config the settings page reads). If no account is available for testing, state this explicitly in the completion report — the completion code path is identical to the already-shipped settings-page flow hitting the same endpoints, but it has not been exercised end-to-end from the modal.

- [x] **Step 6: Verify the settings-page flow still works**

Settings → Kimi Code → "Login with Kimi Code": starts, shows `kimi_oauth_modal`, cancel works. (No files it uses were modified; this is a regression sanity check only.)

- [x] **Step 7: Final commit (only if fixes were needed)**

```bash
git add -A internal/webui
git commit -m "fix(webui): address Kimi Code accounts-modal smoke findings"
```

---

## Self-Review Notes

- **Spec coverage:** request = "new kimi code login flow on the accounts page, existing pattern" → provider tab in `add_account_modal` (Task 2) + ported device-flow logic (Task 1) + smoke (Task 3). Backend untouched by design.
- **Placeholder scan:** all code steps carry full code; verification steps carry exact commands and expected output.
- **Type consistency:** `kimiOAuth` field names (`sessionId/userCode/verificationUri/status/error/polling`) and method names (`startKimiLogin`, `setProvider`, `resetState`) match verbatim between Task 1 and Task 2. Status values match the backend: `pending/completed/expired/denied/cancelled/error`.
- **Deliberate exclusions:** no logout button in the modal (logout stays on the settings page; duplicating it here adds a second convention). No Kimi entry in the accounts list/tables (the credential is not a pooled account).
