# PR #100 Remediation: Kimi Modal Poll Race + Dead Code

## Goal

Fix the three review findings on PR #100 (`feat/kimi-code-accounts-page`):
one 🟡 in-flight poll race, two 🔵 dead/redundant resets. No behavior change
beyond closing the race window.

## Architecture

Vanilla-JS Alpine.js component (`window.Components.addAccountModal`) over a
`<dialog>`. Server allows exactly one pending Kimi device-flow session
(commit `9682a85`). No JS unit-test runner exists in this repo — verification
is `node --check`, the Go suite (`translations_test.go` enforces en/pt key
parity), and browser smoke. "Red/Green" below is therefore a manual
repro-driven loop, not pytest.

## Tech Stack

Alpine.js 3, daisyUI `<dialog>`, `window.utils.request` fetch wrapper,
Go 1.27 backend (unchanged).

## Spec / Review reference

- PR: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/100
- Review comment: posted 2026-09-25 (same thread).
- Shipped parity reference: `internal/webui/public/js/components/models.js`
  `_pollKimiOAuth` L747-786 — the pattern this PR ports (carries the same
  race; **follow-up issue, out of scope for this plan**).

## Findings → Tasks

| # | Finding | Severity |
|---|---------|----------|
| 1 | `_pollKimiLogin` doesn't re-check `polling` after in-flight status request → late response overwrites state reset by `resetState()` | 🟡 risk |
| 2 | `this.kimiOAuth.status = 'cancelled'` in `_cancelKimiSession` is dead (both callers replace the object immediately) | 🔵 nit |
| 3 | Redundant resets: explicit `resetState()` at success-path L250 and `@click="resetState()"` on Close button duplicate `@close="resetState()"` | 🔵 nit |

---

## Task 1: Guard poll loop against in-flight cancel/reset

- **Modify:** `internal/webui/public/js/components/add-account-modal.js`
  (`_pollKimiLogin`, after the `window.utils.request` await, ~L233)
- **Consumes:** `this.kimiOAuth.polling`, `window.utils.request`
- **Produces:** poll loop that cannot mutate state after cancel/reset

### Step 1 — Reproduce (Red)

1. Build and run: `go build -o bin/proxy ./cmd/proxy && ./bin/proxy -listen 127.0.0.1:8092`
2. Open the Web UI → Accounts → Add Account → Kimi Code tab → start login.
3. In DevTools, throttle network to "Slow 3G" (widens the in-flight window).
4. While a `/api/kimi/auth/status` request is in flight, press Esc (or switch
   to the Google tab).
5. Re-open the modal → Kimi tab. **Observe (bug):** stale
   `kimiOAuth.error` text may appear, or a success toast fires after cancel,
   because the late response overwrote the reset state.

### Step 2 — Minimal implementation (Green)

Add one line after the request await in `_pollKimiLogin`:

```js
const { response, newPassword } = await window.utils.request(
    `/api/kimi/auth/status?session_id=${encodeURIComponent(this.kimiOAuth.sessionId)}`,
    {}, store.webuiPassword);
if (!this.kimiOAuth.polling) return;   // <-- new: cancelled/reset while in flight
if (newPassword) store.webuiPassword = newPassword;
```

Also apply the same guard after `await response.json().catch(...)` is *not*
needed — `json()` resolution is microtask-fast relative to user action; the
single post-request guard closes the realistic window.

### Step 3 — Verify

- `node --check internal/webui/public/js/components/add-account-modal.js`
- Repeat Step 1 repro: after Esc mid-request, re-open modal → Kimi tab shows
  clean state (no error line), no late toast.
- Repeat the three cancel paths from the PR checklist (Close button, tab
  switch, Esc): exactly one `POST /api/kimi/auth/cancel` each, polling stops,
  fresh start succeeds.

### Step 4 — Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js
git commit -m "fix(webui): guard kimi poll loop against in-flight cancel/reset"
```

---

## Task 2: Remove dead `status = 'cancelled'` assignment

- **Modify:** `internal/webui/public/js/components/add-account-modal.js`
  (`_cancelKimiSession`, ~L266)

Both callers overwrite `this.kimiOAuth` with a fresh object immediately after
`_cancelKimiSession()` returns, so the assignment is unobservable.

### Step 1 — Confirm deadness (Red-equivalent)

`grep -n "kimiOAuth.status" internal/webui/public/js/components/add-account-modal.js`
— no reader can observe `'cancelled'` between the assignment and the callers'
object replacement.

### Step 2 — Edit (Green)

Delete the line `this.kimiOAuth.status = 'cancelled';` from
`_cancelKimiSession` (keep `polling = false`).

### Step 3 — Verify

- `node --check internal/webui/public/js/components/add-account-modal.js`
- Browser smoke: start → Esc → one cancel POST; start again → 200.

### Step 4 — Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js
git commit -m "refactor(webui): drop dead cancelled-status assignment in kimi cancel"
```

---

## Task 3: Deduplicate reset paths

- **Modify:**
  - `internal/webui/public/js/components/add-account-modal.js`
    (success path in `_pollKimiLogin`, ~L249-250)
  - `internal/webui/public/index.html` (Close button, ~L488)

`@close="resetState()"` on the `<dialog>` already covers every close path
(button, Esc, programmatic `close()`).

### Step 1 — Confirm coverage (Red-equivalent)

`<dialog>.close()` fires `close` synchronously; the button is inside
`<form method="dialog">`, so clicking it closes the dialog → `@close` fires.
Explicit calls are pure duplicates.

### Step 2 — Edit (Green)

- Remove `this.resetState();` right after
  `document.getElementById('add_account_modal')?.close();` in the
  `completed` branch of `_pollKimiLogin`.
- Remove `@click="resetState()"` from the Close button in `index.html`.

Keep `@close="resetState()"` as the single reset path.

### Step 3 — Verify

- `node --check internal/webui/public/js/components/add-account-modal.js`
- Browser smoke matrix (each must leave clean state for the next open):
  - Close button, Esc, backdrop-less programmatic close after completed login.
  - Confirm still exactly one cancel POST when pending (no double-cancel from
    a duplicated reset — the guard `status !== 'pending'` already protects,
    but verify the network tab shows one request).

### Step 4 — Commit

```bash
git add internal/webui/public/js/components/add-account-modal.js internal/webui/public/index.html
git commit -m "refactor(webui): single reset path via dialog @close handler"
```

---

## Final gate

1. `node --check` on all touched JS (both files).
2. `go test ./...` — must stay green (touches no Go code, but
   `translations_test.go` re-validates en/pt parity).
3. Full browser smoke of the PR checklist cancel paths + one settings-page
   Kimi OAuth regression pass.
4. `git push origin feat/kimi-code-accounts-page`

## Follow-up (file as issue, NOT this PR)

- `models.js:_pollKimiOAuth` (settings page) carries the same in-flight race
  as Finding 1 — apply the identical `polling` guard there in a separate PR.
