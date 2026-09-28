# PR #102 Remediation: Kimi Poll Session-Identity Races

## Goal

Fix the five review findings on PR #102 (`fix/kimi-poll-inflight-race`):
three 🟡 residual races the PR's `polling`-flag guards do not cover
(cancel → immediate re-login timelines), two 🔵 nits. All commits land on
the PR branch.

## Architecture

Vanilla-JS Alpine.js components (`window.Components.models`,
`window.Components.addAccountModal`) polling
`GET /api/kimi/auth/status?session_id=…` every 2 s while
`kimiOAuth.polling` is true. PR #102 guards on `polling` only; `polling` is
a *boolean*, so it cannot distinguish "still cancelled" from "a new login
started". The fix is a session-identity check: capture `sessionId` before
each await, compare after.

No JS unit-test runner exists in this repo — verification is a
deterministic Node smoke harness (same approach the PR author used,
throwaway), `node --check`, the Go suite (`translations_test.go` enforces
en/pt key parity — no keys added here), and browser smoke. "Red/Green"
below is harness-driven, not pytest.

## Tech Stack

Alpine.js 3, daisyUI `<dialog>`, `window.utils.request` fetch wrapper,
Node ≥ 18 (harness only), Go 1.27 backend (unchanged).

## Spec / Review reference

- PR: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/102
- Review comment: posted 2026-09-25 (same thread, comment 5839547973).
- Prior art: `docs/superpowers/plans/2026-09-25-pr100-kimi-modal-remediation.md`
  (same harness style; `_cancelKimiSession` is the parity mirror).

## Findings → Tasks

| # | Finding | Severity | Task |
|---|---------|----------|------|
| 1 | `models.js _pollKimiOAuth` post-await guard checks `polling`, not session identity → late response for OLD session passes guard after re-login: old `completed` closes reopened dialog, sets `polling=false` (kills new poll loop), toasts for a cancelled session; catch guard lets an old rejection stamp `error` on the fresh session | 🟡 risk | 1 |
| 2 | Same latent pattern at `add-account-modal.js _pollKimiLogin` L233/L259 (from #100) | 🟡 risk | 2 |
| 3 | `cancelKimiOAuthLogin` writes `status='cancelled'` after the cancel-POST await from a pre-await `wasPending` snapshot → clobbers a fresh `pending` login started during the round-trip | 🟡 risk | 3 |
| 4 | `status='cancelled'` gated on non-empty `sessionId`; `startKimiOAuthLogin` sets `sessionId = data.session_id || ''` then `status='pending'`, so ok-without-`session_id` leaves `status='pending'` forever, polling stopped → settings template stuck | 🟡 risk | 4 |
| 5 | `newPassword` store assignment sits below the polling guard (both files) → 401→prompt rotation during an in-flight-then-cancelled poll drops the in-memory password (localStorage already set; one re-prompt impact) | 🔵 nit | 5 |
| 6 | Race harness (3 red controls, 11 checks) was throwaway; races are regression-prone | 🔵 nit | 6 |

---

## Shared test harness (used by every task)

Create `internal/webui/tests/kimi-poll-race.harness.mjs` (throwaway until
Task 6 decides its permanent home). It loads a component file in a fake
browser environment with a manually-resolvable `window.utils.request` and
an instant `setTimeout`:

```js
// kimi-poll-race.harness.mjs
import fs from 'node:fs';
import vm from 'node:vm';

export function loadComponent(path, exportName) {
    const requests = [];               // { url, options, resolve, reject }
    const toasts = [];
    const closedDialogs = [];
    const store = { webuiPassword: 'pw', t: (k) => k,
        showToast: (msg, kind) => toasts.push({ msg, kind }) };
    const sandbox = {
        console,
        setTimeout: (fn) => { fn(); return 0; },   // instant 2 s sleep
        clearTimeout: () => {},
        Alpine: { store: (name) => (name === 'global' ? store : {}) },
        document: { getElementById: (id) => ({
            open: true,
            showModal() {},
            close() { closedDialogs.push(id); this.open = false; },
        }) },
    };
    sandbox.window = {
        Components: {},
        utils: { request: (url, options) =>
            new Promise((resolve, reject) =>
                requests.push({ url, options, resolve, reject })) },
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(fs.readFileSync(path, 'utf8'), sandbox, { filename: path });
    return { component: sandbox.window.Components[exportName](),
             requests, toasts, closedDialogs, store };
}

export const okResponse = (data) => ({
    ok: true, status: 200,
    json: async () => data,
});
export const pendingTick = () => new Promise((r) => setImmediate(r));
```

Notes:
- `setTimeout` override makes the poll sleep resolve synchronously, so one
  `await pendingTick()` advances the loop to the in-flight request.
- `fetchKimiConfig` / `_refreshKimiStore` on the success path issue further
  requests through the same mock; resolve them with `okResponse({})` or
  stub the method on the component instance (`c.fetchKimiConfig = async () => {}`).
- Confirm `const { response, newPassword } = await …` destructuring works
  with the mock resolving to `{ response: okResponse(...), newPassword: null }`.

Run per task: `node internal/webui/tests/<task-file>.mjs` (assert via
`node:assert/strict`; non-zero exit = red).

---

## Task 1: Session-identity guard in `models.js _pollKimiOAuth`

- **Modify:** `internal/webui/public/js/components/models.js`
  (`_pollKimiOAuth`, request await ~L753-756 and catch ~L779-784)
- **Consumes:** `this.kimiOAuth.{polling,sessionId,status}`, `window.utils.request`
- **Produces:** poll iteration that cannot mutate state for any session
  other than the one that issued the request

### Step 1 — Failing test (Red)

`internal/webui/tests/t1-old-session-completed.mjs`:

```js
import assert from 'node:assert/strict';
import { loadComponent, okResponse, pendingTick } from './kimi-poll-race.harness.mjs';

const { component: c, requests, toasts, closedDialogs } =
    loadComponent('internal/webui/public/js/components/models.js', 'models');

c.fetchKimiConfig = async () => {};
c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };

const loop = c._pollKimiOAuth();
await pendingTick();                        // request for 'old' now in flight
assert.equal(requests.length, 1);
assert.ok(requests[0].url.includes('session_id=old'));

// User cancels, then immediately starts a NEW login before 'old' resolves.
c.kimiOAuth.sessionId = 'new';
c.kimiOAuth.status = 'pending';
c.kimiOAuth.polling = true;

requests[0].resolve({ response: okResponse({ status: 'completed' }), newPassword: null });
await loop;

// New session must be untouched.
assert.equal(c.kimiOAuth.polling, true, 'old response killed the new poll loop');
assert.equal(c.kimiOAuth.status, 'pending', 'old response clobbered new status');
assert.equal(toasts.length, 0, 'toasted success for a cancelled session');
assert.equal(closedDialogs.length, 0, 'old response closed the reopened dialog');
console.log('t1 ok');
```

Second case in the same file — old in-flight rejection after re-login:
`requests[0].reject(new Error('boom'))` → assert `status` stays `'pending'`
and `error` stays `''`.

### Step 2 — Confirm failure

`node internal/webui/tests/t1-old-session-completed.mjs` → assertion
failures on PR-head code (guard passes because `polling === true`).

### Step 3 — Minimal implementation (Green)

In `_pollKimiOAuth`, capture identity before the await and compare after —
in both the try and the catch:

```js
const sessionId = this.kimiOAuth.sessionId;
const { response, newPassword } = await window.utils.request(
    `/api/kimi/auth/status?session_id=${encodeURIComponent(sessionId)}`,
    {}, store.webuiPassword);
if (!this.kimiOAuth.polling || this.kimiOAuth.sessionId !== sessionId) return;
```

```js
} catch (e) {
    if (!this.kimiOAuth.polling || this.kimiOAuth.sessionId !== sessionId) return;
    this.kimiOAuth.status = 'error';
    ...
```

`sessionId` must be declared before the `try` so the catch can see it
(hoist `const sessionId = this.kimiOAuth.sessionId;` above `try {`).

### Step 4 — Confirm pass

`node internal/webui/tests/t1-old-session-completed.mjs` → `t1 ok`.
`node --check internal/webui/public/js/components/models.js`.

### Step 5 — Commit

```bash
git add internal/webui/public/js/components/models.js
git commit -m "fix(webui): guard kimi settings poll by session identity, not just polling flag"
```

---

## Task 2: Same guard in `add-account-modal.js _pollKimiLogin`

- **Modify:** `internal/webui/public/js/components/add-account-modal.js`
  (`_pollKimiLogin`, request await ~L230-234 and catch ~L259-264)
- **Consumes / Produces:** as Task 1, for the accounts-page modal

### Step 1 — Failing test (Red)

`t2-old-session-completed-modal.mjs`: identical timeline to Task 1 against
`add-account-modal.js` export `addAccountModal`; stub
`c._refreshKimiStore = async () => {}`. Note this modal's reset path
*replaces* `this.kimiOAuth` with a fresh object — the re-login timeline is:
old loop in flight → `resetState()` (new object, `polling:false`) →
`startKimiOAuthLogin()` sets `polling:true` on that same new object → old
response resolves. Assert the new object's `status` stays `'pending'` and
`add_account_modal` is not closed by the old `completed`.

### Step 2 — Confirm failure

`node internal/webui/tests/t2-old-session-completed-modal.mjs` → red.

### Step 3 — Minimal implementation (Green)

Identical edit as Task 1 (hoisted `sessionId` + dual-condition guard in try
and catch).

### Step 4 — Confirm pass

Test green; `node --check internal/webui/public/js/components/add-account-modal.js`.

### Step 5 — Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js
git commit -m "fix(webui): guard accounts-page kimi poll by session identity"
```

---

## Task 3: Stale-cancel guard in `cancelKimiOAuthLogin`

- **Modify:** `internal/webui/public/js/components/models.js`
  (`cancelKimiOAuthLogin`, ~L788-808)
- **Consumes:** captured `sessionId`, `wasPending`
- **Produces:** `status='cancelled'` written only if the session is still
  the one being cancelled

### Step 1 — Failing test (Red)

`t3-stale-cancel-clobber.mjs`:

```js
c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
const cancel = c.cancelKimiOAuthLogin();
await pendingTick();                        // cancel POST in flight
// New login starts during the cancel round-trip.
c.kimiOAuth.sessionId = 'new';
c.kimiOAuth.status = 'pending';
c.kimiOAuth.polling = true;
requests[0].resolve({ response: okResponse({ status: 'ok' }), newPassword: null });
await cancel;
assert.equal(c.kimiOAuth.status, 'pending', 'stale cancel clobbered new login');
```

### Step 2 — Confirm failure

Red on PR-head code (`status` becomes `'cancelled'`).

### Step 3 — Minimal implementation (Green)

Gate the write on identity:

```js
if (wasPending && sessionId) {
    try { /* cancel POST */ } catch (e) { /* best-effort */ }
    if (this.kimiOAuth.sessionId === sessionId) {
        this.kimiOAuth.status = 'cancelled';
    }
}
```

(`wasPending` is then implied by the identity check + prior state, but keep
both conditions for readability; do not re-check `status === 'pending'`
alone — Task 4's empty-sessionId case must still reach `'cancelled'`, see
there.)

### Step 4 — Confirm pass

Test green; `node --check` the file.

### Step 5 — Commit

```bash
git add internal/webui/public/js/components/models.js
git commit -m "fix(webui): skip stale cancelled write when a new kimi login supersedes the cancel"
```

---

## Task 4: `cancelled` status even with empty `sessionId`

- **Modify:** `internal/webui/public/js/components/models.js`
  (`cancelKimiOAuthLogin`, same hunk as Task 3)
- **Consumes:** `wasPending`, `sessionId`
- **Produces:** pending-with-empty-sessionId cancels to `'cancelled'`;
  cancel POST still gated on non-empty `sessionId`

### Step 1 — Failing test (Red)

`t4-empty-session-cancel.mjs`:

```js
c.kimiOAuth = { polling: true, sessionId: '', status: 'pending', error: '' };
await c.cancelKimiOAuthLogin();
assert.equal(c.kimiOAuth.status, 'cancelled', 'stuck pending with empty sessionId');
assert.equal(requests.length, 0, 'cancel POST must not fire without sessionId');
```

### Step 2 — Confirm failure

Red: `status` stays `'pending'` (both PR-head code and the Task-3 shape,
which nests the write inside `wasPending && sessionId`).

### Step 3 — Minimal implementation (Green)

Restructure so the POST is gated on `sessionId`, the status write on
`wasPending` + identity:

```js
const sessionId = this.kimiOAuth.sessionId;
const wasPending = this.kimiOAuth.status === 'pending';
this.kimiOAuth.polling = false;
if (wasPending && sessionId) {
    try { /* cancel POST with sessionId */ } catch (e) { /* best-effort */ }
}
if (wasPending && this.kimiOAuth.sessionId === sessionId) {
    this.kimiOAuth.status = 'cancelled';
}
```

This subsumes Task 3's guard; if Tasks 3+4 land together, write this final
shape directly and keep both tests.

### Step 4 — Confirm pass

`t3` and `t4` both green; `node --check`.

### Step 5 — Commit

```bash
git add internal/webui/public/js/components/models.js
git commit -m "fix(webui): mark kimi login cancelled even when start returned no session id"
```

---

## Task 5 (nit): Apply rotated password before the polling guard

- **Modify:** `internal/webui/public/js/components/models.js` (~L756),
  `internal/webui/public/js/components/add-account-modal.js` (~L233)
- **Produces:** `store.webuiPassword` updated from `newPassword` even when
  the poll was cancelled in flight

### Step 1 — Failing test (Red)

`t5-password-rotation.mjs` (per file): request in flight → cancel
(`polling = false`) → resolve with `{ response: okResponse({status:'pending'}), newPassword: 'rotated' }`
→ assert `store.webuiPassword === 'rotated'`.

### Step 2 — Confirm failure

Red (early return skips the assignment).

### Step 3 — Minimal implementation (Green)

Move `if (newPassword) store.webuiPassword = newPassword;` above the
guard line in both poll loops. Guard conditions from Tasks 1-2 unchanged.

### Step 4 — Confirm pass

`t5` green; all earlier tests still green; `node --check` both files.

### Step 5 — Commit

```bash
git add internal/webui/public/js/components/models.js internal/webui/public/js/components/add-account-modal.js
git commit -m "fix(webui): store rotated webui password before kimi poll cancel guard"
```

---

## Task 6 (nit, optional): Commit the race harness permanently

- **Create:** `internal/webui/tests/kimi-poll-race.harness.mjs` +
  consolidated `internal/webui/tests/kimi-poll-race.test.mjs` (all t1-t5
  cases, `node:assert/strict`, exits non-zero on failure)
- **Modify:** nothing else; document the run command in the PR body

### Steps

1. Consolidate t1-t5 into one file with a tiny runner
   (`for (const [name, fn] of cases) { await fn(); console.log('ok', name); }`).
2. Verify: `node internal/webui/tests/kimi-poll-race.test.mjs` → all ok.
3. Revert Tasks 1-5 mentally: spot-check one red control by temporarily
   removing the Task-1 guard (do not commit) → harness must fail.
4. Commit:

```bash
git add internal/webui/tests/
git commit -m "test(webui): commit deterministic kimi poll race harness"
```

Skip this task if the team prefers no committed JS tests without a runner;
the harness then stays throwaway per repo convention.

---

## Final verification (all tasks)

1. `node internal/webui/tests/*.mjs` (or consolidated file) — all green.
2. `node --check` on both touched component files.
3. `go test ./...` — green (no Go changes, no new translation keys).
4. Browser smoke on the PR branch: Settings → Kimi Code Gateway → Login →
   Esc during pending → immediately Login again → new poll runs to
   completion or clean cancel; no toast for the old session; dialog not
   closed by the old response; Accounts-page modal: same timeline.
5. `git push origin fix/kimi-poll-inflight-race`, then reply on the PR
   review comment listing the resolving commit per finding.
