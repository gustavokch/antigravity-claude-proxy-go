# PR #102 Remediation, Round 2: Stale Cancel Close + Orphaned Poll Loops

## Goal

Fix the five round-2 review findings on PR #102
(`fix/kimi-poll-inflight-race`, head `6cf1799`): two 🟡 races left in the
cancel → immediate re-login timeline, and three 🔵 harness gaps that hid
them. All commits land on the PR branch.

## Architecture

Vanilla-JS Alpine.js components (`window.Components.models`,
`window.Components.addAccountModal`) poll
`GET /api/kimi/auth/status?session_id=…` every 2 s while
`kimiOAuth.polling` is true. Round 1 added a per-iteration session-identity
check. Two gaps remain:

1. `models.js cancelKimiOAuthLogin` closes `kimi_oauth_modal`
   unconditionally *after* the cancel-POST await. If a new login reopened
   the dialog meanwhile, the stale close fires `@close="cancelKimiOAuthLogin()"`
   (`views/settings.html:2652`), which cancels the **new** session.
2. Both poll loops capture `sessionId` per iteration *after* the 2 s sleep.
   A loop sleeping through cancel → re-login wakes, sees `polling=true`, and
   adopts the new session → two concurrent loops per session.

Fix: bind each loop to the session it was started for (captured once at
loop entry, re-checked after the sleep and after the request), and gate the
stale dialog close on the same identity check as the `cancelled` write.

Verification: the committed Node harness
(`internal/webui/tests/kimi-poll-race.test.mjs`), made capable of seeing
these races (stateful dialogs with an `@close` hook, manually flushed
timers), then gated through `go test` so `make test` runs it.

## Tech Stack

Alpine.js 3, daisyUI `<dialog>`, `window.utils.request` fetch wrapper,
Node ≥ 18 (harness, `node:vm` + `node:assert/strict`), Go 1.27 test wrapper.

## Spec / Review reference

- PR: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/102
- Round-2 review: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/102#issuecomment-5840010374
- Round-1 plan: `docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation.md`
- Server behavior relied on: `internal/api/kimi_oauth_handlers.go`
  `handleKimiAuthStatusGet` drops the session after `ClaimCompletion`
  (later polls → HTTP 404 `expired`) and rejects empty `session_id` (400).

## Probe evidence (PR head `6cf1799`)

Throwaway probe with a stateful dialog + queued timers:

```
A models stale cancel: { dialogOpen: false, closed: ['kimi_oauth_modal'], polling: false,
  cancelPOSTs: ['{"session_id":"old"}', '{"session_id":"new"}'] }
B models loops polling new: ['…status?session_id=new', '…status?session_id=new']
C modal loops polling new:  ['…status?session_id=new', '…status?session_id=new']
```

## Findings → Tasks

| # | Finding | Severity | Task |
|---|---------|----------|------|
| 1 | Harness can't see finding 2 or 3: instant `setTimeout`, and `getElementById` returns a fresh `{open:true}` per call with no `@close` emulation | 🔵 nit | 1 |
| 2 | Component paths in the test are cwd-relative; ENOENT unless run from repo root | 🔵 nit | 1 |
| 3 | `models.js:811` stale `dialog.close()` after cancel-POST await closes the reopened dialog → `@close` cancels the NEW session | 🟡 risk | 2 |
| 4 | `models.js:752` sleeping loop adopts the new session → duplicate poll loops; a reordered 404 `expired` can mark a persisted login `error` | 🟡 risk | 3 |
| 5 | Same at `add-account-modal.js:100` (`resetState` → `startKimiLogin` within 2 s) | 🟡 risk | 4 |
| 6 | Harness not wired into any gate (`make test` = `go test -v -race ./...`, no CI config) | 🔵 nit | 5 |

Out of scope, unchanged: empty `session_id` from `/api/kimi/auth/start`
(old and new loops both hold `''`, so identity can't separate them; server
answers 400 → `error` state); the settings Login button having no
`:disabled` while pending; `x-load-view` orphaning a loop on tab switch
(both deferred in the PR body).

## Setup

The PR branch is already checked out in a worktree (`git switch` in the
main checkout fails for that reason). Work there:

```bash
cd .claude/worktrees/kimi-poll-race-followups   # fix/kimi-poll-inflight-race @ 6cf1799
git fetch fork fix/kimi-poll-inflight-race && git status -sb   # must not be behind
node internal/webui/tests/kimi-poll-race.test.mjs   # baseline: 8/8 passed
```

Run every command below from that worktree root unless it states a `cwd`.

---

## Task 1: Harness can see dialog and timer races; cwd-independent paths

- **Modify:** `internal/webui/tests/kimi-poll-race.harness.mjs`,
  `internal/webui/tests/kimi-poll-race.test.mjs`
- **Consumes:** component files under `internal/webui/public/js/components/`
- **Produces:** `loadComponent(file, exportName, { manualTimers } = {})` →
  `{ component, requests, toasts, closedDialogs, store, dialog, flushTimers }`
  - `file` is a bare component filename (`'models.js'`), resolved relative to
    the harness via `import.meta.url`.
  - `dialog(id)` returns one stateful object per id (default `open:false`);
    `close()` records into `closedDialogs` and calls `onclose()` only when it
    was open, matching the native `close` event. A test sets
    `dialog(id).onclose = () => c.cancelKimiOAuthLogin()` to mirror `@close`.
  - `manualTimers: true` queues `setTimeout` callbacks; `flushTimers()` runs
    and clears the queue. Default stays instant so existing cases keep their
    timelines.

### Step 1: Failing test (red)

The red check is cwd-independence. The dialog/timer capabilities are
exercised by Tasks 2–4.

```bash
cd internal/webui && node tests/kimi-poll-race.test.mjs
```

Expected: `ENOENT … internal/webui/public/js/components/models.js`.

### Step 2: Confirm failure

Output from Step 1 shows ENOENT, exit ≠ 0.

### Step 3: Minimal implementation

`kimi-poll-race.harness.mjs`, full replacement:

```js
// kimi-poll-race.harness.mjs
import fs from 'node:fs';
import vm from 'node:vm';

const COMPONENTS = new URL('../public/js/components/', import.meta.url);

export function loadComponent(file, exportName, { manualTimers = false } = {}) {
    const requests = [];               // { url, options, resolve, reject }
    const toasts = [];
    const closedDialogs = [];
    const timers = [];
    const dialogs = new Map();
    const store = { webuiPassword: 'pw', t: (k) => k,
        showToast: (msg, kind) => toasts.push({ msg, kind }) };
    const dialog = (id) => {
        if (!dialogs.has(id)) {
            dialogs.set(id, {
                open: false,
                onclose: null,         // test hook mirroring the template's @close
                showModal() { this.open = true; },
                close() {
                    if (!this.open) return;   // native: no close event when already closed
                    this.open = false;
                    closedDialogs.push(id);
                    if (this.onclose) this.onclose();
                },
            });
        }
        return dialogs.get(id);
    };
    const sandbox = {
        console,
        setTimeout: (fn) => {
            if (manualTimers) timers.push(fn); else fn();
            return 0;
        },
        clearTimeout: () => {},
        Alpine: { store: (name) => (name === 'global' ? store : {}) },
        document: { getElementById: dialog, querySelectorAll: () => [] },
    };
    sandbox.window = {
        Components: {},
        utils: { request: (url, options) =>
            new Promise((resolve, reject) =>
                requests.push({ url, options, resolve, reject })) },
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    const url = new URL(file, COMPONENTS);
    vm.runInContext(fs.readFileSync(url, 'utf8'), sandbox, { filename: url.pathname });
    const flushTimers = () => timers.splice(0).forEach((fn) => fn());
    return { component: sandbox.window.Components[exportName](),
             requests, toasts, closedDialogs, store, dialog, flushTimers };
}

export const okResponse = (data) => ({
    ok: true, status: 200,
    json: async () => data,
});
export const pendingTick = () => new Promise((r) => setImmediate(r));
```

`kimi-poll-race.test.mjs` edits:

```js
const MODELS = 'models.js';
const MODAL = 'add-account-modal.js';
```

Dialogs now start closed, so the "not closed" assertions in the t1/t2
completion cases would pass vacuously. Open the dialog before resolving the
old request:

- `t1_oldSessionCompleted_models`: destructure `dialog`; after the
  re-login mutation add `dialog('kimi_oauth_modal').showModal();   // new login's dialog`.
- `t2_oldSessionCompleted_modal`: destructure `dialog`; after the
  `c.kimiOAuth = { … 'new' … }` line add `dialog('add_account_modal').showModal();`.

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs                  # 8/8 passed
(cd internal/webui && node tests/kimi-poll-race.test.mjs)          # 8/8 passed
(cd /tmp && node "$OLDPWD/internal/webui/tests/kimi-poll-race.test.mjs")   # 8/8 passed
```

Non-vacuity check for the t1/t2 edits: temporarily delete the
session-identity guard at `models.js:758` → t1 "old response closed the
reopened dialog" or an earlier t1 assertion must fail. Restore it.

### Step 5: Commit

```bash
git add internal/webui/tests/kimi-poll-race.harness.mjs internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "test(webui): stateful dialogs, manual timers, cwd-independent paths in kimi race harness"
```

---

## Task 2: Stale cancel must not close the new login's dialog (`models.js`)

- **Modify:** `internal/webui/public/js/components/models.js`
  (`cancelKimiOAuthLogin`, L791-813)
- **Test:** `internal/webui/tests/kimi-poll-race.test.mjs` (new case `t6`)
- **Consumes:** `sessionId`, `wasPending` captured before the await
- **Produces:** after the await, `status='cancelled'` and `dialog.close()`
  run only while `this.kimiOAuth.sessionId === sessionId`

### Step 1: Failing test (red)

Add to `kimi-poll-race.test.mjs`:

```js
// models.js: a stale cancel resolving after a new login reopened the dialog
// must not close it (closing fires @close → cancels the NEW session).
async function t6_staleCancelKeepsNewDialog() {
    const { component: c, requests, dialog } = loadComponent(MODELS, 'models');
    const dlg = dialog('kimi_oauth_modal');
    dlg.onclose = () => c.cancelKimiOAuthLogin();       // mirrors @close in settings.html

    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
    const cancel = c.cancelKimiOAuthLogin();            // Esc: dialog already closed
    await pendingTick();                                // cancel POST for 'old' in flight
    assert.equal(requests.length, 1);

    // New login starts and reopens the dialog during the cancel round-trip.
    c.kimiOAuth.sessionId = 'new';
    c.kimiOAuth.status = 'pending';
    c.kimiOAuth.polling = true;
    dlg.showModal();

    requests[0].resolve({ response: okResponse({ status: 'ok' }), newPassword: null });
    await cancel;

    assert.equal(dlg.open, true, 'stale cancel closed the new login dialog');
    assert.equal(c.kimiOAuth.polling, true, 'stale cancel stopped the new poll');
    assert.equal(requests.length, 1, 'stale cancel fired a cancel POST for the new session');
}
```

Register it in `cases`:

```js
    ['t6 stale cancel keeps new login dialog open (models.js)', t6_staleCancelKeepsNewDialog],
```

### Step 2: Confirm failure

```bash
node internal/webui/tests/kimi-poll-race.test.mjs
```

Expected: `AssertionError … stale cancel closed the new login dialog`
(probe A: dialog closed, `polling=false`, second cancel POST for `new`).

### Step 3: Minimal implementation

Replace the tail of `cancelKimiOAuthLogin` (L808-812):

```js
        if (wasPending && this.kimiOAuth.sessionId === sessionId) {
            this.kimiOAuth.status = 'cancelled';
        }
        const dialog = document.getElementById('kimi_oauth_modal');
        if (dialog && dialog.open) dialog.close();
```

with:

```js
        // A newer login may have started during the cancel round-trip; leave it alone.
        if (this.kimiOAuth.sessionId !== sessionId) return;
        if (wasPending) this.kimiOAuth.status = 'cancelled';
        const dialog = document.getElementById('kimi_oauth_modal');
        if (dialog && dialog.open) dialog.close();
```

Invariants kept: t3 (no `cancelled` stamp on the new login), t4 (empty
`sessionId`: `'' === ''` → `cancelled`), and a non-pending close (✕ after
`completed`/`error`: no await, identity trivially holds → dialog closes).

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs     # 9/9 passed
node --check internal/webui/public/js/components/models.js
```

### Step 5: Commit

```bash
git add internal/webui/public/js/components/models.js internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "fix(webui): stale kimi cancel no longer closes the new login dialog"
```

---

## Task 3: Bind the settings poll loop to its session (`models.js`)

- **Modify:** `internal/webui/public/js/components/models.js`
  (`_pollKimiOAuth`, L747-789)
- **Test:** `kimi-poll-race.test.mjs` (new case `t7`)
- **Consumes:** `this.kimiOAuth.{polling,sessionId}` at loop entry
- **Produces:** a loop that exits once its session is no longer current,
  whether it was sleeping or had a request in flight

### Step 1: Failing test (red)

```js
// models.js: a loop sleeping through cancel → re-login must exit, not adopt
// the new session (otherwise two loops poll it concurrently).
async function t7_orphanLoopExits_models() {
    const { component: c, requests, flushTimers } =
        loadComponent(MODELS, 'models', { manualTimers: true });
    c.fetchKimiConfig = async () => {};

    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
    c._pollKimiOAuth();                                 // loop A sleeping
    await pendingTick();

    c.kimiOAuth.polling = false;                        // cancel while A sleeps
    c.kimiOAuth.sessionId = 'new';                      // immediate re-login
    c.kimiOAuth.status = 'pending';
    c.kimiOAuth.polling = true;
    c._pollKimiOAuth();                                 // loop B sleeping
    await pendingTick();

    flushTimers();                                      // both sleeps elapse
    await pendingTick();

    assert.deepEqual(requests.map((r) => r.url),
        ['/api/kimi/auth/status?session_id=new'],
        'orphaned loop adopted the new session');
}
```

Register: `['t7 orphaned sleeping loop exits on re-login (models.js)', t7_orphanLoopExits_models],`

### Step 2: Confirm failure

```bash
node internal/webui/tests/kimi-poll-race.test.mjs
```

Expected: `AssertionError … orphaned loop adopted the new session` (two
`session_id=new` URLs; matches probe B).

### Step 3: Minimal implementation

In `_pollKimiOAuth`, move the capture to loop entry and route every
liveness check through one predicate:

```js
    async _pollKimiOAuth() {
        const store = Alpine.store('global');
        const sessionId = this.kimiOAuth.sessionId;
        // Live only while polling THIS session; a cancel or newer login ends the loop.
        const live = () => this.kimiOAuth.polling && this.kimiOAuth.sessionId === sessionId;
        while (live()) {
            await new Promise(resolve => setTimeout(resolve, 2000));
            if (!live()) return;
            try {
                const { response, newPassword } = await window.utils.request(
                    `/api/kimi/auth/status?session_id=${encodeURIComponent(sessionId)}`,
                    {}, store.webuiPassword);
                if (newPassword) store.webuiPassword = newPassword;
                if (!live()) return; // cancelled/reset/superseded while in flight
                // … unchanged body …
            } catch (e) {
                if (!live()) return; // cancelled/reset/superseded while in flight
                // … unchanged body …
            }
        }
    },
```

Delete the per-iteration `const sessionId = this.kimiOAuth.sessionId;`
(old L752) and replace both inline
`!this.kimiOAuth.polling || this.kimiOAuth.sessionId !== sessionId`
conditions with `!live()`.

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs     # 10/10 passed
node --check internal/webui/public/js/components/models.js
```

### Step 5: Commit

```bash
git add internal/webui/public/js/components/models.js internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "fix(webui): bind kimi settings poll loop to its session so re-login cannot fork it"
```

---

## Task 4: Bind the accounts-page poll loop to its session (`add-account-modal.js`)

- **Modify:** `internal/webui/public/js/components/add-account-modal.js`
  (`_pollKimiLogin`, L224-268 at PR head)
- **Test:** `kimi-poll-race.test.mjs` (new case `t8`)
- **Consumes / Produces:** same as Task 3. `resetState()`/`setProvider()`
  *replace* `this.kimiOAuth`, and `live()` reads `this.kimiOAuth` on every
  call, so it follows the replacement.

### Step 1: Failing test (red)

```js
// add-account-modal.js: resetState() → startKimiLogin() inside the 2 s sleep.
async function t8_orphanLoopExits_modal() {
    const { component: c, requests, flushTimers } =
        loadComponent(MODAL, 'addAccountModal', { manualTimers: true });
    c._refreshKimiStore = async () => {};

    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
    c._pollKimiLogin();                                 // loop A sleeping
    await pendingTick();

    // resetState() replaced the object; a fresh login is pending on it.
    c.kimiOAuth = { sessionId: 'new', userCode: '', verificationUri: '', status: 'pending', error: '', polling: true };
    c._pollKimiLogin();                                 // loop B sleeping
    await pendingTick();

    flushTimers();
    await pendingTick();

    assert.deepEqual(requests.map((r) => r.url),
        ['/api/kimi/auth/status?session_id=new'],
        'orphaned loop adopted the new session');
}
```

Register: `['t8 orphaned sleeping loop exits on re-login (add-account-modal.js)', t8_orphanLoopExits_modal],`

### Step 2: Confirm failure

Red: two `session_id=new` URLs (probe C).

### Step 3: Minimal implementation

Apply the Task 3 edit to `_pollKimiLogin`: hoist
`const sessionId = this.kimiOAuth.sessionId;` above the `while`, add the
same `live` predicate, use `while (live())`, and replace the post-sleep
check and both in-flight guards with `!live()`.

### Step 4: Confirm pass

```bash
node internal/webui/tests/kimi-poll-race.test.mjs     # 11/11 passed
node --check internal/webui/public/js/components/add-account-modal.js
```

### Step 5: Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js internal/webui/tests/kimi-poll-race.test.mjs
git commit -m "fix(webui): bind accounts-page kimi poll loop to its session"
```

---

## Task 5: Run the harness from `go test`

- **Create:** `internal/webui/kimi_poll_race_test.go`
- **Consumes:** `internal/webui/tests/kimi-poll-race.test.mjs` (exit ≠ 0 on
  any assertion: an unhandled top-level-await rejection)
- **Produces:** `TestKimiPollRaceHarness`, which runs under `make test` /
  `go test ./...` and skips when `node` is absent

### Step 1: Failing test (red)

```go
package webui

import (
	"os/exec"
	"testing"
)

// TestKimiPollRaceHarness runs the deterministic Node harness covering the
// Kimi OAuth poll/cancel races in models.js and add-account-modal.js.
func TestKimiPollRaceHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping Kimi poll race harness")
	}
	out, err := exec.Command(node, "tests/kimi-poll-race.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("kimi poll race harness failed: %v\n%s", err, out)
	}
}
```

Prove the gate catches a regression by temporarily reverting Task 3's
`live()` post-sleep check in `models.js` (restore `if (!this.kimiOAuth.polling) return;`).

### Step 2: Confirm failure

```bash
go test ./internal/webui/ -run TestKimiPollRaceHarness -v
```

Expected: `FAIL … kimi poll race harness failed: exit status 1` with the
t7 `AssertionError` in the output. Restore the Task 3 code.

### Step 3: Minimal implementation

The test file from Step 1 is the implementation. `go test` runs with cwd =
package dir, so `tests/…` resolves, and Task 1 made the component paths
cwd-independent.

### Step 4: Confirm pass

```bash
go test ./internal/webui/ -run TestKimiPollRaceHarness -v   # PASS
PATH=/usr/bin:/bin go test ./internal/webui/ -run TestKimiPollRaceHarness -v   # SKIP if node lives elsewhere (e.g. Homebrew)
gofmt -l internal/webui/                                    # no output
```

### Step 5: Commit

```bash
git add internal/webui/kimi_poll_race_test.go
git commit -m "test(webui): run kimi poll race harness under go test"
```

---

## Phase 5 gate (after all tasks)

```bash
node internal/webui/tests/kimi-poll-race.test.mjs     # 11/11 passed
node --check internal/webui/public/js/components/models.js
node --check internal/webui/public/js/components/add-account-modal.js
go test -race ./...                                   # 100% green
git push fork fix/kimi-poll-inflight-race
```

Manual browser smoke (unverifiable in the managed Chromium; note it in the
PR): Settings → Kimi Code Gateway → Login → Esc → Login again immediately.
The second dialog must stay open and keep polling, and the Network tab must
show one `status?session_id=<new>` request per 2 s with no cancel POST for
the new session.
