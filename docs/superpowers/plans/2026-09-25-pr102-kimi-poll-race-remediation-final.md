# PR #102 Remediation, Final Pass: Committed Completions + Modal Start/Close Races

## Goal

Fix the three final-pass findings on PR #102
(`fix/kimi-poll-inflight-race`, head `f31bc7b`):

1. 🟡 In both poll loops, the in-flight guard throws away a `completed` status
   that the server has already persisted, so the UI keeps showing logged-out
   state. The same guard also leaves a gap during `response.json()`.
2. 🟡 `add-account-modal.js startKimiLogin`: if the user closes the modal while
   `/auth/start` is in flight, the start response revives a hidden pending
   login and its poll loop.
3. 🔵 `add-account-modal.js` completion path: the modal close after the
   `_refreshKimiStore()` await is not guarded, so a stale close can reset a
   newer login.

All commits go on the PR branch.

## Architecture

Two Alpine.js components share the Kimi device-flow poll pattern:

- `window.Components.models` (settings page, `kimi_oauth_modal`,
  `@close="cancelKimiOAuthLogin()"` at `views/settings.html:2652`)
- `window.Components.addAccountModal` (accounts page, `add_account_modal`,
  `@close="resetState()"` at `index.html:275`; `resetState()` swaps in a fresh
  `kimiOAuth` object)

Each loop is tied to one `sessionId` through `live()`. It exits when a
cancel, reset or newer login makes it stale.

Server fact behind finding 1: `internal/api/kimi_oauth_handlers.go:78-90`.
`handleKimiAuthStatusGet` runs `ClaimCompletion` →
`registerAuthenticatedKimiOAuth` (persist) → `CancelSession` **before** it
writes `{"status":"completed"}`. So a `completed` body means the login is
already saved, and a later `/auth/cancel` cannot undo it
(`handleKimiAuthCancelPost` only drops the session). The client must resync
config from any `completed` it receives, stale or not. It must still leave
`kimiOAuth`, the dialog and toasts alone when the loop is stale.

## Tech Stack

Alpine.js 3, daisyUI `<dialog>`, `window.utils.request` fetch wrapper, Node ≥ 18
harness (`node:vm`, `node:assert/strict`), gated by Go test
`internal/webui/kimi_poll_race_test.go`.

## Spec / Review reference

- PR: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/102
- Final-pass review: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/102#issuecomment-5840391522
- Prior plans: `docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation.md`,
  `docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation-round2.md`

## Probe evidence (PR head `f31bc7b`)

```
A models committed-completion-after-cancel: { refreshed: 0, status: 'cancelled', toasts: 0 }
B modal committed-completion-after-reset:   { refreshed: 0 }
C modal start-in-flight-reset: { status: 'pending', polling: true,
    urls: ['/api/kimi/auth/start', '/api/kimi/auth/status?session_id=s1'] }
D modal stale close after refresh: { dialogOpen: false, status: '',
    extraCancelPOSTs: ['{"session_id":"new"}'] }
```

The red checks below (t9b, t11, t12) were run against head and fail with the
listed messages.

## Findings → Tasks

| # | Finding | Severity | Task |
|---|---------|----------|------|
| 1 | `models.js:760` guard drops a server-committed `completed`; the guard runs before `response.json()` | 🟡 risk | 1 |
| 2 | Same at `add-account-modal.js:237` | 🟡 risk | 2 |
| 3 | `add-account-modal.js:210` start response written into the post-reset object → hidden poll loop, orphan server session | 🟡 risk (pre-existing, #100) | 3 |
| 4 | `add-account-modal.js:253` stale `add_account_modal` close after refresh await → `@close` resets the new login | 🔵 nit | 4 |

Out of scope and unchanged:
- Empty `session_id` from `/auth/start`.
- The settings Login button has no `:disabled` while pending (double start).
- `x-load-view` orphaning a loop on tab switch.
- `kimiStarting` being cleared by a superseded start's `finally`.

## Setup

```bash
cd .claude/worktrees/kimi-poll-race-followups   # fix/kimi-poll-inflight-race @ f31bc7b
git fetch fork fix/kimi-poll-inflight-race && git status -sb   # must not be behind
node internal/webui/tests/kimi-poll-race.test.mjs              # baseline: 11/11 passed
```

Run every command below from the worktree root. All new tests go into
`internal/webui/tests/kimi-poll-race.test.mjs`, before the `const cases = [`
line, and each is registered at the end of `cases`.

---

## Task 1: Settings poll resyncs config from a committed completion (`models.js`)

- **Modify:** `internal/webui/public/js/components/models.js` (`_pollKimiOAuth`, L756-761)
- **Test:** `internal/webui/tests/kimi-poll-race.test.mjs` (new `deferredResponse`, `t9`, `t9b`)
- **Consumes:** `live()`, `sessionId` captured at loop entry; `fetchKimiConfig()`
- **Produces:** a stale loop (`!live()`) that receives `response.ok && data.status === 'completed'`
  awaits `fetchKimiConfig()` and returns. It never touches `kimiOAuth`, the dialog
  or toasts. The body is parsed before the guard.

### Step 1: Failing tests (red)

```js
// Response whose body read stays pending until release(): lets a test cancel mid-body-read.
function deferredResponse(data) {
    let release;
    const body = new Promise((r) => { release = () => r(data); });
    return { response: { ok: true, status: 200, json: () => body }, release };
}

// models.js: Esc while a status request is in flight; the server answers 'completed'
// (already persisted by ClaimCompletion). Settings must resync, without a toast.
async function t9_committedCompletionAfterCancel_models() {
    const { component: c, requests, toasts } = loadComponent(MODELS, 'models');
    let refreshed = 0;
    c.fetchKimiConfig = async () => { refreshed++; };
    c.kimiOAuth = { polling: true, sessionId: 's1', status: 'pending', error: '' };

    const loop = c._pollKimiOAuth();
    await pendingTick();                        // status request in flight
    const cancel = c.cancelKimiOAuthLogin();    // Esc → @close
    await pendingTick();
    assert.equal(requests.length, 2);           // status + cancel POST

    requests[0].resolve({ response: okResponse({ status: 'completed' }), newPassword: null });
    requests[1].resolve({ response: okResponse({ status: 'ok' }), newPassword: null });
    await loop;
    await cancel;

    assert.equal(refreshed, 1, 'server-committed login not reflected in settings');
    assert.equal(toasts.length, 0, 'toasted success for a cancelled login');
}

// models.js: cancel lands after headers but before the body is read.
async function t9b_cancelDuringBodyRead_models() {
    const { component: c, requests, toasts } = loadComponent(MODELS, 'models');
    let refreshed = 0;
    c.fetchKimiConfig = async () => { refreshed++; };
    c.kimiOAuth = { polling: true, sessionId: 's1', status: 'pending', error: '' };

    const loop = c._pollKimiOAuth();
    await pendingTick();
    const { response, release } = deferredResponse({ status: 'completed' });
    requests[0].resolve({ response, newPassword: null });
    await pendingTick();                        // headers in, body read pending
    c.cancelKimiOAuthLogin();                   // Esc mid-body-read
    release();
    await loop;

    assert.equal(toasts.length, 0, 'toasted success after cancel during body read');
    assert.equal(refreshed, 1, 'server-committed login not reflected in settings');
}
```

Register:

```js
    ['t9 committed completion after cancel resyncs config (models.js)', t9_committedCompletionAfterCancel_models],
    ['t9b cancel during body read (models.js)', t9b_cancelDuringBodyRead_models],
```

### Step 2: Confirm failure

```bash
node internal/webui/tests/kimi-poll-race.test.mjs
```

Expected: `AssertionError … server-committed login not reflected in settings`
(t9, probe A). With t9 temporarily skipped, t9b fails with
`toasted success after cancel during body read` (confirmed at head).

### Step 3: Minimal implementation

In `_pollKimiOAuth`, replace:

```js
                if (newPassword) store.webuiPassword = newPassword;
                if (!live()) return; // cancelled/reset/superseded while in flight
                const data = await response.json().catch(() => ({}));
```

with:

```js
                if (newPassword) store.webuiPassword = newPassword;
                const data = await response.json().catch(() => ({}));
                if (!live()) {
                    // Cancelled/reset/superseded while in flight. The server persists a
                    // login before answering 'completed', so resync config anyway.
                    if (response.ok && data.status === 'completed') await this.fetchKimiConfig();
                    return;
                }
```

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs   # 13/13 passed
```

t1 (old `completed` after re-login) must still pass. `fetchKimiConfig` only
writes `kimiConfig`, never `kimiOAuth`.

### Step 5: Commit

```bash
git add internal/webui/public/js/components/models.js internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "fix(webui): resync kimi settings when a cancelled poll receives a committed login"
```

---

## Task 2: Accounts-page poll resyncs store from a committed completion (`add-account-modal.js`)

- **Modify:** `internal/webui/public/js/components/add-account-modal.js` (`_pollKimiLogin`, L236-238)
- **Test:** `internal/webui/tests/kimi-poll-race.test.mjs` (new `t10`)
- **Consumes:** `live()`, `sessionId`; `_refreshKimiStore()`
- **Produces:** same contract as Task 1, with `_refreshKimiStore()` in place of `fetchKimiConfig()`

### Step 1: Failing test (red)

```js
// add-account-modal.js: modal closed (resetState) while a status request is in
// flight; the server answers 'completed'. The accounts store must resync.
async function t10_committedCompletionAfterReset_modal() {
    const { component: c, requests, toasts } = loadComponent(MODAL, 'addAccountModal');
    let refreshed = 0;
    c._refreshKimiStore = async () => { refreshed++; };
    c.kimiOAuth = { polling: true, sessionId: 's1', status: 'pending', error: '' };

    const loop = c._pollKimiLogin();
    await pendingTick();
    c.resetState();                             // modal closed
    requests[0].resolve({ response: okResponse({ status: 'completed' }), newPassword: null });
    await loop;

    assert.equal(refreshed, 1, 'server-committed login not reflected in accounts store');
    assert.equal(toasts.length, 0, 'toasted success for a cancelled login');
}
```

Register:

```js
    ['t10 committed completion after reset resyncs store (add-account-modal.js)', t10_committedCompletionAfterReset_modal],
```

### Step 2: Confirm failure

```bash
node internal/webui/tests/kimi-poll-race.test.mjs
```

Expected: `AssertionError … server-committed login not reflected in accounts store` (probe B).

### Step 3: Minimal implementation

In `_pollKimiLogin`, replace:

```js
                if (newPassword) store.webuiPassword = newPassword;
                if (!live()) return; // cancelled/reset/superseded while in flight
                const data = await response.json().catch(() => ({}));
```

with:

```js
                if (newPassword) store.webuiPassword = newPassword;
                const data = await response.json().catch(() => ({}));
                if (!live()) {
                    // Cancelled/reset/superseded while in flight. The server persists a
                    // login before answering 'completed', so resync the store anyway.
                    if (response.ok && data.status === 'completed') await this._refreshKimiStore();
                    return;
                }
```

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs   # 14/14 passed
```

### Step 5: Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "fix(webui): resync accounts kimi store when a reset poll receives a committed login"
```

---

## Task 3: Closing the modal during `/auth/start` does not revive a hidden login

- **Modify:** `internal/webui/public/js/components/add-account-modal.js`
  (`startKimiLogin` L195-222, `_cancelKimiSession` L272-290)
- **Test:** `internal/webui/tests/kimi-poll-race.test.mjs` (new `t11`)
- **Consumes:** `resetState()` replacing `this.kimiOAuth` with a new object
- **Produces:**
  - `_postKimiCancel(sessionId)`: a best-effort `POST /api/kimi/auth/cancel` that
    stores a rotated password. It is extracted from `_cancelKimiSession`.
  - `_cancelKimiSession()` keeps its behavior: `polling=false` always, and POST only
    when `status==='pending' && sessionId`.
  - `startKimiLogin()` snapshots `this.kimiOAuth` before the await. If the object was
    replaced, it cancels the just-created server session (when the start succeeded)
    and returns without writing state or starting a loop.

### Step 1: Failing test (red)

```js
// add-account-modal.js: user closes the modal while /auth/start is in flight.
// The start response must not revive a hidden pending login; cancel it server-side.
async function t11_resetDuringStart_modal() {
    const { component: c, requests, flushTimers } =
        loadComponent(MODAL, 'addAccountModal', { manualTimers: true });

    const start = c.startKimiLogin();
    await pendingTick();
    assert.equal(requests.length, 1);           // /auth/start in flight
    c.resetState();                             // user closes the modal
    requests[0].resolve({ response: okResponse({ status: 'ok', session_id: 's1', user_code: 'ABCD' }),
                          newPassword: null });
    await start;
    flushTimers();                              // a revived loop would poll now
    await pendingTick();

    assert.equal(c.kimiOAuth.status, '', 'closed modal revived a pending login');
    assert.equal(c.kimiOAuth.polling, false, 'hidden poll loop started after close');
    assert.deepEqual(requests.slice(1).map((r) => [r.url, r.options.body]),
        [['/api/kimi/auth/cancel', '{"session_id":"s1"}']],
        'orphaned server session not cancelled');
}
```

Register:

```js
    ['t11 reset during start does not revive a hidden login (add-account-modal.js)', t11_resetDuringStart_modal],
```

### Step 2: Confirm failure

```bash
node internal/webui/tests/kimi-poll-race.test.mjs
```

Expected: `AssertionError … closed modal revived a pending login` (confirmed at head; probe C).

### Step 3: Minimal implementation

`startKimiLogin`: snapshot the state before the request and check it after the
body is parsed:

```js
    async startKimiLogin() {
        const store = Alpine.store('global');
        const state = this.kimiOAuth; // resetState() swaps this object out
        this.kimiStarting = true;
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/auth/start', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: '{}'
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json().catch(() => ({}));
            if (this.kimiOAuth !== state) {
                // Modal closed while starting: drop the new server session, stay idle.
                if (response.ok && data.session_id) this._postKimiCancel(data.session_id);
                return;
            }
            if (!response.ok || data.status !== 'ok') {
```

(The rest of the method is unchanged.)

Replace `_cancelKimiSession` with:

```js
    async _cancelKimiSession() {
        const { status, sessionId } = this.kimiOAuth;
        this.kimiOAuth.polling = false;
        if (status === 'pending' && sessionId) await this._postKimiCancel(sessionId);
    },

    async _postKimiCancel(sessionId) {
        const store = Alpine.store('global');
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
```

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs   # 15/15 passed
```

t2, t8 and t10 exercise `resetState()` → `_cancelKimiSession()` and must still pass.

### Step 5: Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "fix(webui): closing add-account modal during kimi start cancels the new session"
```

---

## Task 4: A stale completion close must not reset a newer login (`add-account-modal.js`)

- **Modify:** `internal/webui/public/js/components/add-account-modal.js` (`_pollKimiLogin` completed branch, L252-254)
- **Test:** `internal/webui/tests/kimi-poll-race.test.mjs` (new `t12`)
- **Consumes:** loop-entry `sessionId`
- **Produces:** after `await this._refreshKimiStore()`, `add_account_modal` is closed
  only when `this.kimiOAuth.sessionId === sessionId`

### Step 1: Failing test (red)

```js
// add-account-modal.js: completion refresh in flight; the user closes the modal,
// reopens it and starts a new login. The stale close must not fire @close → resetState().
async function t12_staleCloseAfterRefresh_modal() {
    const { component: c, requests, dialog } = loadComponent(MODAL, 'addAccountModal');
    const dlg = dialog('add_account_modal');
    dlg.onclose = () => c.resetState();         // mirrors @close in index.html
    dlg.showModal();
    let releaseRefresh;
    c._refreshKimiStore = () => new Promise((r) => { releaseRefresh = r; });
    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };

    const loop = c._pollKimiLogin();
    await pendingTick();
    requests[0].resolve({ response: okResponse({ status: 'completed' }), newPassword: null });
    await pendingTick();                        // now awaiting _refreshKimiStore
    assert.equal(typeof releaseRefresh, 'function');

    dlg.close();                                // user closes → resetState
    dlg.showModal();                            // reopens and starts a new login
    c.kimiOAuth = { sessionId: 'new', userCode: 'X', verificationUri: '', status: 'pending', error: '', polling: true };
    const before = requests.length;

    releaseRefresh();
    await loop;

    assert.equal(dlg.open, true, 'stale close closed the new login modal');
    assert.equal(c.kimiOAuth.status, 'pending', 'stale close reset the new login');
    assert.equal(requests.length, before, 'stale close cancelled the new session');
}
```

Register:

```js
    ['t12 stale completion close keeps new login modal (add-account-modal.js)', t12_staleCloseAfterRefresh_modal],
```

### Step 2: Confirm failure

```bash
node internal/webui/tests/kimi-poll-race.test.mjs
```

Expected: `AssertionError … stale close closed the new login modal` (confirmed at head; probe D).

### Step 3: Minimal implementation

Replace:

```js
                    await this._refreshKimiStore();
                    document.getElementById('add_account_modal')?.close();
                    return;
```

with:

```js
                    await this._refreshKimiStore();
                    // A reset or newer login during the refresh owns the modal now.
                    if (this.kimiOAuth.sessionId === sessionId) {
                        document.getElementById('add_account_modal')?.close();
                    }
                    return;
```

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs   # 16/16 passed
```

### Step 5: Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "fix(webui): stale kimi completion no longer closes the reopened add-account modal"
```

---

## Final verification (Phase 5)

```bash
node --check internal/webui/public/js/components/models.js
node --check internal/webui/public/js/components/add-account-modal.js
node internal/webui/tests/kimi-poll-race.test.mjs                        # 16/16 passed
(cd /tmp && node "$OLDPWD/internal/webui/tests/kimi-poll-race.test.mjs")  # cwd-independent
go test -count=1 -run TestKimiPollRaceHarness -v ./internal/webui        # PASS (not SKIP)
go test ./...                                                            # green
```

Red-control spot check: revert Task 1's `fetchKimiConfig()` resync line and
confirm t9 fails, then restore it.

Manual browser smoke (after merge is fine):
- Settings → Kimi Code Gateway → Login → authorize in the other tab → press
  Esc right as it completes → the card shows "Logged in as …".
- Accounts → Add Account → Kimi → Login → close the modal before the code
  appears → reopen → the Kimi button is enabled and no code is shown.

Push and report:

```bash
git push fork fix/kimi-poll-inflight-race
gh pr comment 102 --body "…resolved findings, commits, 16/16…"
```
