# Kimi Poll In-Flight Race Follow-ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the remaining in-flight poll races in both Kimi device-flow UI loops: the settings-page `_pollKimiOAuth` guard (issue #101), the cancel-path window in `cancelKimiOAuthLogin` that would leave that guard ineffective, and the unguarded `catch` blocks in both poll loops.

**Architecture:** Vanilla-JS Alpine.js components. `add-account-modal.js` (accounts page) was hardened in PR #100; this plan applies the identical pattern to `models.js` (settings page, `x-data="window.Components.models()"` in `views/settings.html:917`) plus two fixes PR #100 explicitly deferred. The settings page's only cancel path is the `<dialog id="kimi_oauth_modal" @close="cancelKimiOAuthLogin()">` (`views/settings.html:2652`) — Esc, ✕, and Cancel all close the dialog.

**Tech Stack:** Alpine.js 3, daisyUI, Go 1.27 backend (unchanged).

**Spec:**
- Issue: https://github.com/gustavokch/antigravity-claude-proxy-go/issues/101
- Prior plan (pattern + verification rationale): `docs/superpowers/plans/2026-09-25-pr100-kimi-modal-remediation.md`
- PR #100 (merged as `bd1aa15` into `main` — base this branch on current `main`).

## Global Constraints

- No JS unit-test runner exists. Verification = `node --check`, `go test ./...` (enforces en/pt translation parity; this plan adds no translation keys), and the Node smoke harness below (deterministic red/green, loads the real files from disk).
- Browser automation is NOT usable in this environment: the managed Chromium silently skips classic-script execution on the page (verified 2026-09-25 — all scripts fetch with valid `text/javascript`, zero execute, zero errors). Do not burn time on `browser.*`; the harness is the primary proof.
- No new UI strings → no `en.js`/`pt.js` edits; `translations_test.go` must stay green untouched.
- Do NOT remove `this.kimiOAuth.status = 'cancelled'` in `cancelKimiOAuthLogin`: unlike PR #100 Finding 2, it is observable here — the settings page mutates `kimiOAuth` in place (never replaces the object; only init at `models.js:371`), and the template reads `kimiOAuth.status`/`kimiOAuth.error`.
- Remote is named `fork` (points at `github.com/gustavokch/antigravity-claude-proxy-go`), not `origin`.
- External network to the Kimi OAuth host works from this machine (verified: real `/api/kimi/auth/start` returned a live session on 2026-09-25), so a real-backend smoke is possible.

## File Structure

- Modify: `internal/webui/public/js/components/models.js` — Task 1 (guard in `_pollKimiOAuth`), Task 2 (reorder `cancelKimiOAuthLogin`), Task 3 (catch guard in `_pollKimiOAuth`).
- Modify: `internal/webui/public/js/components/add-account-modal.js` — Task 3 only (catch guard in `_pollKimiLogin`).
- Create: `/tmp/kimi-race-followup-smoke.mjs` — throwaway harness (never committed).

Branch: `fix/kimi-poll-inflight-race` off latest `fork/main`. One PR closing #101.

---

## Task 1: Guard settings-page `_pollKimiOAuth` against in-flight cancel

**Files:**
- Modify: `internal/webui/public/js/components/models.js` (`_pollKimiOAuth`, status-request await, ~L753-756)
- Test: `/tmp/kimi-race-followup-smoke.mjs` (written in Task 1 Step 2)

**Interfaces:**
- Consumes: `this.kimiOAuth.polling` (set `false` by `cancelKimiOAuthLogin` and by every terminal branch of the loop), `window.utils.request`.
- Produces: poll loop that returns without mutating state once `polling` is `false` when the status response lands.

Context (current `models.js`):

```js
const { response, newPassword } = await window.utils.request(
    `/api/kimi/auth/status?session_id=${encodeURIComponent(this.kimiOAuth.sessionId)}`,
    {}, store.webuiPassword);
if (newPassword) store.webuiPassword = newPassword;
```

- [ ] **Step 1: Branch**

```bash
git fetch fork main
git checkout -b fix/kimi-poll-inflight-race fork/main
```

- [ ] **Step 2: Write the harness (failing test)**

Write `/tmp/kimi-race-followup-smoke.mjs` exactly as below. It loads the real component files (new code from the working tree, old code from `git show fork/main:...` as red controls) and drives the race with a delayable mock of `window.utils.request`. The models component scope is the whole settings page, so a pending state is injected directly (same technique as the PR #100 harness).

```js
// Throwaway smoke harness for the kimi poll-race follow-ups.
// Loads the REAL models.js / add-account-modal.js and drives the exact
// race scenarios with a controllable window.utils.request mock and a fake
// dialog whose close() fires a 'close' listener — simulating the Alpine
// @close bindings in views/settings.html and index.html.
import { execSync } from "node:child_process";

const sleep = (ms) => new Promise(r => setTimeout(r, ms));

function makeWorld({ dialogId, statusMode = "pending", statusDelay = 0, cancelDelay = 0, rejectStatus = false }) {
  const calls = { status: 0, cancel: 0, config: 0 };
  const cancelBodies = [];
  const toasts = [];
  const dialog = {
    open: false,
    _closeListeners: [],
    showModal() { this.open = true; },
    close() {
      if (!this.open) return;
      this.open = false;
      for (const fn of this._closeListeners) fn();
    },
    addEventListener(ev, fn) { if (ev === "close") this._closeListeners.push(fn); },
  };
  const globalStore = { webuiPassword: "pw", showToast(m, t) { toasts.push({ m, t }); }, t: (k) => k };
  const dataStore = { kimi: null };
  const world = { calls, cancelBodies, toasts, dialog, statusMode, statusDelay, cancelDelay, rejectStatus };
  global.window = {
    Components: {},
    utils: {
      request: async (url, opts) => {
        if (url.includes("/api/kimi/auth/status")) {
          calls.status++;
          await sleep(world.statusDelay);
          if (world.rejectStatus) throw new Error("network down");
          const payloads = {
            pending: { status: "pending" },
            completed: { status: "completed", account: { email: "late@x" } },
            error: { status: "error", error: "HTTP 500" },
          };
          return {
            response: { ok: world.statusMode !== "error", status: world.statusMode === "error" ? 500 : 200, json: async () => payloads[world.statusMode] },
            newPassword: null,
          };
        }
        if (url.includes("/api/kimi/auth/cancel")) {
          calls.cancel++;
          cancelBodies.push(JSON.parse(opts.body));
          await sleep(world.cancelDelay);
          return { response: { ok: true, status: 200, json: async () => ({ status: "ok" }) }, newPassword: null };
        }
        if (url.includes("/api/kimi/config")) {
          calls.config++;
          return { response: { ok: true, status: 200, json: async () => ({ status: "ok" }) }, newPassword: null };
        }
        throw new Error("unexpected url " + url);
      },
    },
  };
  global.Alpine = { store: (name) => (name === "global" ? globalStore : dataStore) };
  global.document = {
    getElementById: (id) => (id === dialogId ? dialog : null),
    querySelectorAll: () => [],
  };
  return world;
}

function pending(id) {
  return { sessionId: id, userCode: "U", verificationUri: "V", status: "pending", error: "", polling: true };
}

function loadModels(source, world) {
  (0, eval)(source);
  const comp = window.Components.models();
  world.dialog._closeListeners.length = 0;
  world.dialog.addEventListener("close", () => comp.cancelKimiOAuthLogin()); // settings.html @close
  return comp;
}

function loadModal(source, world) {
  (0, eval)(source);
  const comp = window.Components.addAccountModal();
  world.dialog._closeListeners.length = 0;
  world.dialog.addEventListener("close", () => comp.resetState()); // index.html @close
  return comp;
}

const results = [];
function check(name, cond, detail = "") {
  results.push({ name, pass: !!cond });
  console.log((cond ? "PASS" : "FAIL") + "  " + name + (detail ? "  -- " + detail : ""));
}

const newModels = (await import("node:fs")).readFileSync("internal/webui/public/js/components/models.js", "utf8");
const oldModels = execSync("git show fork/main:internal/webui/public/js/components/models.js", { encoding: "utf8" });
const newModal = (await import("node:fs")).readFileSync("internal/webui/public/js/components/add-account-modal.js", "utf8");
const oldModal = execSync("git show fork/main:internal/webui/public/js/components/add-account-modal.js", { encoding: "utf8" });

// M1: Esc on kimi_oauth_modal while status request in flight; late response says completed.
// Timeline: status req at t=2.0 (lands 3.5); Esc at t=2.2; cancel POST resolves 4.2.
// cancelDelay (2000) MUST exceed time-until-status-lands (1300): pre-Task-2 code still has
// polling===true at 3.5, so M1-new is red until Task 2 flips the flag synchronously at 2.2.
async function m1(src, label) {
  const world = makeWorld({ dialogId: "kimi_oauth_modal", statusMode: "completed", statusDelay: 1500, cancelDelay: 2000 });
  const comp = loadModels(src, world);
  comp.kimiOAuth = pending("m1");
  const pollDone = comp._pollKimiOAuth();
  await sleep(2200);
  world.dialog.showModal();
  world.dialog.close(); // Esc / X / Cancel button
  await sleep(2500);
  await pollDone;
  return { world, comp };
}

{
  const { world, comp } = await m1(newModels, "M1-new");
  check("M1-new (Task1+2): no success toast after Esc", world.toasts.length === 0, JSON.stringify(world.toasts));
  check("M1-new (Task1+2): no config refresh from late completed", world.calls.config === 0, `config=${world.calls.config}`);
  check("M1-new (Task1+2): exactly one cancel POST, right session", world.calls.cancel === 1 && world.cancelBodies[0]?.session_id === "m1", `cancels=${world.calls.cancel}`);
  check("M1-new (Task1+2): status settled as cancelled, no error", comp.kimiOAuth.status === "cancelled" && comp.kimiOAuth.error === "", `status='${comp.kimiOAuth.status}' error='${comp.kimiOAuth.error}'`);
}
{
  const { world } = await m1(oldModels, "M1-old-control");
  check("M1-old-control (red): late completed fired toast+config pre-fix",
    world.toasts.some(t => t.t === "success") && world.calls.config >= 1,
    `toasts=${JSON.stringify(world.toasts)} config=${world.calls.config}`);
}

// M2: settings catch guard — status fetch rejects after cancel.
async function m2(src, label) {
  const world = makeWorld({ dialogId: "kimi_oauth_modal", rejectStatus: true, statusDelay: 800, cancelDelay: 0 });
  const comp = loadModels(src, world);
  comp.kimiOAuth = pending("m2");
  const pollDone = comp._pollKimiOAuth();
  await sleep(2200);
  world.dialog.showModal();
  world.dialog.close();
  await sleep(1500);
  await pollDone;
  return { world, comp };
}

{
  const { world, comp } = await m2(newModels, "M2-new");
  check("M2-new (Task3): reject after cancel keeps status 'cancelled'", comp.kimiOAuth.status === "cancelled" && comp.kimiOAuth.error === "", `status='${comp.kimiOAuth.status}' error='${comp.kimiOAuth.error}'`);
}
{
  const { comp } = await m2(oldModels, "M2-old-control");
  check("M2-old-control (red): catch overwrote status with 'error' pre-fix", comp.kimiOAuth.status === "error", `status='${comp.kimiOAuth.status}'`);
}

// A1: accounts-modal catch guard — status fetch rejects after Esc-reset.
async function a1(src, label) {
  const world = makeWorld({ dialogId: "add_account_modal", rejectStatus: true, statusDelay: 800 });
  const comp = loadModal(src, world);
  comp.kimiOAuth = pending("a1");
  const pollDone = comp._pollKimiLogin();
  await sleep(2200);
  world.dialog.showModal();
  world.dialog.close(); // Esc -> @close -> resetState (replaces kimiOAuth)
  const fresh = comp.kimiOAuth;
  await sleep(1500);
  await pollDone;
  return { world, comp, fresh };
}

{
  const { world, comp, fresh } = await a1(newModal, "A1-new");
  check("A1-new (Task3): no stale error on fresh object", fresh.error === "" && fresh.status === "", `error='${fresh.error}'`);
  check("A1-new (Task3): no toast", world.toasts.length === 0);
  check("A1-new: exactly one cancel POST", world.calls.cancel === 1 && world.cancelBodies[0]?.session_id === "a1");
}
{
  const { comp } = await a1(oldModal, "A1-old-control");
  check("A1-old-control (red): catch wrote error into fresh object pre-fix", comp.kimiOAuth.error === "network down", `error='${comp.kimiOAuth.error}'`);
}

console.log("\n" + results.filter(r => r.pass).length + "/" + results.length + " checks passed");
process.exit(results.every(r => r.pass) ? 0 : 1);
```

- [ ] **Step 3: Run harness, verify M1/M2/A1 new-code checks fail**

Run: `node /tmp/kimi-race-followup-smoke.mjs` (from the repo root).

Expected: the three `*-old-control` red checks PASS, and the `M1-new`/`M2-new`/`A1-new` checks FAIL on pre-fix code (the M1 toast/config assertions fail deterministically: with `cancelDelay: 2000` the response lands at t≈3.5 while `polling` is still `true` until t≈4.2; the old-controls confirm the race fires).

- [ ] **Step 4: Add the guard**

In `internal/webui/public/js/components/models.js`, in `_pollKimiOAuth`, insert one line after the status-request await:

```js
const { response, newPassword } = await window.utils.request(
    `/api/kimi/auth/status?session_id=${encodeURIComponent(this.kimiOAuth.sessionId)}`,
    {}, store.webuiPassword);
if (!this.kimiOAuth.polling) return; // cancelled/reset while in flight
if (newPassword) store.webuiPassword = newPassword;
```

- [ ] **Step 5: Verify**

- `node --check internal/webui/public/js/components/models.js`
- `node /tmp/kimi-race-followup-smoke.mjs` — M1-new still fails on the toast/config assertions (cancel-order not yet fixed — Task 2); M2-new and A1-new still fail (catch guard not yet added — Task 3). Old-controls stay green. This is expected mid-state; the full suite goes green after Task 2 and Task 3.

- [ ] **Step 6: Commit**

```bash
git add internal/webui/public/js/components/models.js
git commit -m "fix(webui): guard settings-page kimi poll loop against in-flight cancel"
```

---

## Task 2: Stop polling before the cancel POST in `cancelKimiOAuthLogin`

**Files:**
- Modify: `internal/webui/public/js/components/models.js` (`cancelKimiOAuthLogin`, ~L788-806)

**Interfaces:**
- Consumes: `this.kimiOAuth.status`, `this.kimiOAuth.sessionId`, `this.kimiOAuth.polling`, `window.utils.request`.
- Produces: `polling === false` synchronously from dialog close, so the Task 1 guard is effective for the entire cancel round-trip. `status = 'cancelled'` is preserved (observable by the settings template).

**Why this task exists:** in the current code `this.kimiOAuth.polling = false` runs only after the `await`ed cancel POST. A status response landing during that round-trip sees `polling === true`, so the Task 1 guard lets it through — a completed response would still toast and refresh config after the user pressed Esc. Moving the flag first closes that window. This mirrors `add-account-modal.js` `_cancelKimiSession` (PR #100), which already sets `polling = false` before its cancel await.

Current code:

```js
async cancelKimiOAuthLogin() {
    const store = Alpine.store('global');
    if (this.kimiOAuth.status === 'pending') {
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/auth/cancel', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ session_id: this.kimiOAuth.sessionId })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
        } catch (e) {
            // Best-effort; the session expires on its own.
        }
        this.kimiOAuth.status = 'cancelled';
    }
    this.kimiOAuth.polling = false;
    const dialog = document.getElementById('kimi_oauth_modal');
    if (dialog && dialog.open) dialog.close();
}
```

Behavior notes for the reorder: (a) `polling = false` is now unconditional and synchronous — identical net effect, since the old code also reached it on every path; (b) if `status === 'pending'` but `sessionId` is empty, the old code sent a cancel POST the server no-ops on (`CancelSession("")`) — the new code skips that useless request (and also skips `status = 'cancelled'` in that edge, leaving `status` at `'pending'` — unreachable in practice since a pending session always has a sessionId); (c) on the normal path `status = 'cancelled'` stays after the await, unchanged semantics.

- [ ] **Step 1: Reorder the cancel flow**

Replace the method body with:

```js
async cancelKimiOAuthLogin() {
    const store = Alpine.store('global');
    const sessionId = this.kimiOAuth.sessionId;
    const wasPending = this.kimiOAuth.status === 'pending';
    this.kimiOAuth.polling = false; // stop the poll loop before any await
    if (wasPending && sessionId) {
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/auth/cancel', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ session_id: sessionId })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
        } catch (e) {
            // Best-effort; the session expires on its own.
        }
        this.kimiOAuth.status = 'cancelled';
    }
    const dialog = document.getElementById('kimi_oauth_modal');
    if (dialog && dialog.open) dialog.close();
}
```

- [ ] **Step 2: Verify**

- `node --check internal/webui/public/js/components/models.js`
- `node /tmp/kimi-race-followup-smoke.mjs` — M1-new now passes; M2-new and A1-new still fail (catch guard not yet added — Task 3). Old-controls stay green.

- [ ] **Step 3: Commit**

```bash
git add internal/webui/public/js/components/models.js
git commit -m "fix(webui): stop kimi settings polling before cancel request"
```

---

## Task 3: Guard both poll-loop catch blocks against post-cancel mutation

**Files:**
- Modify: `internal/webui/public/js/components/models.js` (`_pollKimiOAuth` catch, ~L779-784)
- Modify: `internal/webui/public/js/components/add-account-modal.js` (`_pollKimiLogin` catch, ~L259-264; note `add-account-modal.js:233` already contains an identical guard line from PR #100 — anchor this edit on the `catch` context, not the guard string)

**Interfaces:**
- Consumes: `this.kimiOAuth.polling` (already `false` after any cancel/reset/terminal state — inside the loop, `polling` is only cleared by those paths, all of which either return immediately or are exactly the conditions where the catch must not write).
- Produces: catch blocks that cannot overwrite cancelled/reset state when the fetch itself rejects mid-cancel.

Current `models.js` catch:

```js
} catch (e) {
    this.kimiOAuth.status = 'error';
    this.kimiOAuth.error = e.message || 'Login status check failed';
    this.kimiOAuth.polling = false;
    return;
}
```

Current `add-account-modal.js` catch (identical shape):

```js
} catch (e) {
    this.kimiOAuth.status = 'error';
    this.kimiOAuth.error = e.message || 'Login status check failed';
    this.kimiOAuth.polling = false;
    return;
}
```

- [ ] **Step 1: Add the guard to both catch blocks**

In **both** files, make the catch block's first line the guard (identical edit, same context string in each file):

```js
} catch (e) {
    if (!this.kimiOAuth.polling) return; // cancelled/reset while in flight
    this.kimiOAuth.status = 'error';
    this.kimiOAuth.error = e.message || 'Login status check failed';
    this.kimiOAuth.polling = false;
    return;
}
```

Safety: within each poll loop, `polling === false` at catch-entry can only mean cancel/reset already happened (every terminal branch and the two existing in-loop guards `return` before re-entering the try). No legitimate error-reporting path is suppressed.

- [ ] **Step 2: Verify**

- `node --check internal/webui/public/js/components/models.js`
- `node --check internal/webui/public/js/components/add-account-modal.js`
- `node /tmp/kimi-race-followup-smoke.mjs` — full suite green (new + old controls), exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/webui/public/js/components/models.js internal/webui/public/js/components/add-account-modal.js
git commit -m "fix(webui): guard kimi poll catch blocks against post-cancel mutation"
```

---

## Final gate

- [ ] `node --check` on both touched files.
- [ ] `go test ./...` — must stay green (no Go changes; `translations_test.go` re-validates en/pt parity — no keys added).
- [ ] `node /tmp/kimi-race-followup-smoke.mjs` — all checks pass, including the three old-code red controls.
- [ ] Real-backend smoke (external network available): `go build -o bin/proxy ./cmd/proxy && ./bin/proxy -listen 127.0.0.1:8092`, then
  - `curl -X POST http://127.0.0.1:8092/api/kimi/auth/start` → `status: ok`;
  - `curl -X POST http://127.0.0.1:8092/api/kimi/auth/cancel -d '{"session_id":"<id>"}'` → `status: ok`;
  - `curl -i 'http://127.0.0.1:8092/api/kimi/auth/status?session_id=<id>'` → expect **HTTP 404** with `status: "expired"` — the server drops the session on cancel (`kimi_oauth_handlers_test.go:220-223`), so 404 here is success, not a failure.
  This validates the server endpoints the guarded loops call; UI-level timing is covered by the harness.
- [ ] `git push -u fork fix/kimi-poll-inflight-race`
- [ ] Open PR against `main` with body referencing `Closes #101` and noting it implements the follow-ups deferred from PR #100.

## Deferred (out of scope, not in this plan)

- Settings-page Login button has no `:disabled` while a session is pending (`views/settings.html:1276-1281`); a second click attempts a second `/api/kimi/auth/start` (server allows exactly one pending session, so it errors). UX gap only.
- `x-load-view` may destroy/recreate the settings scope on tab switch, orphaning a running poll loop until terminal status; server session expires on its own. Pre-existing, low impact.
- Browser-level smoke of the settings modal remains unproven in this environment (managed Chromium blocks page scripts); manual check in a real browser is worth one minute after merge: Settings → Kimi Code Gateway → Login → Esc during pending → no toast/error afterward.
