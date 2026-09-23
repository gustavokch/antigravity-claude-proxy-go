# PR #87 Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve all six code review findings on PR #87 (3 🟡, 3 🟢), covering the `REQUEST_DELAY_MAX` 24x widening, the `SHARED_THROTTLE_WINDOW_MAX` vs calibrate mismatch, the out-of-scope `TB_MAX_TOKENS_MIN` change, and three WebUI readability nits.

**Architecture:** Frontend-only remediation in `internal/webui/public/`. No Go changes expected: `internal/config/config.go` declares `RequestDelayMs`/`SharedThrottleWindowMs` as plain `int` with no server-side range check, and `internal/accounts/manager.go:304-311` falls back to 10s only when `ms <= 0`. The UI constants are therefore the single enforcement point — they must admit every value the backend can legitimately hold (notably calibrate fragments up to 1200000ms).

**Tech Stack:** Alpine.js 3, HTML5/TailwindCSS. No Go toolchain step beyond `go build ./...` as a smoke check.

**Spec:** The code review posted on PR #87 —
https://github.com/gustavokch/antigravity-claude-proxy-go/pull/87#issuecomment-5752779234
Findings L101, L105, L134 block merge; the three 🟢 nits are safe as same-PR polish. Section "Findings coverage" at the end maps every finding to the task that closes it.

## Global Constraints
- Zero external dependencies added.
- No TLS/networking changes (project rule: empty `tls.Config{}`, standard transport).
- Backend behaviour unchanged: defaults stay `RequestDelayMs: 200`, `SharedThrottleWindowMs: 10000` (`internal/config/config.go:332-333`).
- Slider fill formula must remain `(value - MIN) / ((MAX - MIN) / 100)` — currently correct (`1199`, `2990`) but hard-coded.

---

### Task 1: Reconcile range constants with producers and scope

**Closes findings:** L101 (🟡), L105 (🟡 blocking), L134 (🟡 blocking).

**Files:**
- Modify: `internal/webui/public/js/config/constants.js:99-105, 133-135`

**Interfaces:**
- Consumes: `internal/calibrate/derive.go:204-206` (`windowMs`, observed up to `1_200_000` in `derive_test.go:268` and `fragment_test.go:36`), `internal/config/config.go:332-333` (defaults)
- Produces: `window.AppConstants.VALIDATION.{REQUEST_DELAY_MIN/MAX, SHARED_THROTTLE_WINDOW_MIN/MAX, TB_MAX_TOKENS_MIN}`

**Decision required before editing — L101:**
`REQUEST_DELAY_MAX` 5000→120000 allows a 2-minute sleep before *every* API call (`dispatcher.go:189-190`). Pick one and record the choice in the commit message:
- **(a) Keep 120s** — add a warning line under the Delay slider ("Values above ~5s will stall every request") so the footgun is explicit. Recommended only if a real operator asked for >5s delays.
- **(b) Cap at a modest ceiling (e.g. 10000 or 30000)** — keeps the new ms/s display work, removes the stall risk. Recommended default.

**Decision required before editing — L105:**
Calibrate already emits `sharedThrottleWindowMs` up to `1_200_000` (20min). The shipped `SHARED_THROTTLE_WINDOW_MAX: 300000` makes the validator reject those server values (`saveConfigField` rolls back to the clamp) and pushes slider fill past 100%. Pick one:
- **(a) Raise max to `1200000`** — UI admits everything calibrate can produce. Recommended.
- **(b) Clamp calibrate output to 300000** — changes `derive.go:206` and its two tests; only if 5min is a deliberate product cap. Heavier, not recommended.

**L134 — revert, no decision:**
`TB_MAX_TOKENS_MIN: 5→1` is outside the PR title and enables a bucket-of-1 rate-limit posture. Revert to `5`. If bucket-of-1 is wanted, open it as its own PR with a backend justification.

- [ ] **Step 1: Apply the three constant edits** per the decisions above (L134 always reverts to `5`).
- [ ] **Step 2: Grep for stale copies** — `rg "5000|120000|300000" internal/webui/public/js/config/constants.js internal/webui/public/views/settings.html` and confirm slider `min`/`max` attributes (Task 2) match the final constants.
- [ ] **Step 3: Commit**

```bash
git add internal/webui/public/js/config/constants.js
git commit -m "fix(webui): reconcile delay/throttle ranges with calibrate output, revert token-bucket scope creep"
```

---

### Task 2: Slider polish — duration format, fill math, repeated expression

**Closes findings:** L3638 (🟢), L3672 (🟢), L3644 (🟢).

**Files:**
- Modify: `internal/webui/public/views/settings.html:3635-3684`
- Modify (preferred): `internal/webui/public/js/utils/` — add `formatDurationMs(ms)` helper, or co-locate next to `window.Validators` if a utils module already exists there

**Interfaces:**
- Consumes: `window.AppConstants.VALIDATION` (Task 1 output), `serverConfig.requestDelayMs`, `serverConfig.sharedThrottleWindowMs`
- Produces: human duration labels, slider `background-size` fill

- [ ] **Step 1: Unify duration labels (L3672 + half of L3638).** Replace the inline ternary at `:3638` and the seconds-only render at `:3668` with one helper, e.g. `formatDurationMs(ms)` → `<1000 ? "Nms" : <60000 ? "N.Ns" : "N.Nm"`. Then `120000` renders `2.0m`, `300000` renders `5.0m`, `10000` still renders `10.0s`. Both labels call it; no per-template arithmetic.
- [ ] **Step 2: Kill the 3x repetition (L3638).** With the helper in place each `x-text` references `serverConfig.requestDelayMs` once (inside the call). If Alpine scope allows, prefer `x-data="{ get delayMs() { return serverConfig.requestDelayMs || 200 } }"` on the wrapping div and bind label/slider/number to `delayMs` — same pattern as the `get quota()` getter used in `accounts.html`.
- [ ] **Step 3: Derive fill from constants (L3644).** Replace hard-coded `/ 1199` and `/ 2990` with an expression over the constants, e.g. `:style="\`background-size: ${(delayMs - REQUEST_DELAY_MIN) / ((REQUEST_DELAY_MAX - REQUEST_DELAY_MIN) / 100)}% 100%\`"`. Same for the throttle slider. Destructure the constants once in the component or reference `window.AppConstants.VALIDATION` directly — either is fine, duplication of the magic number is what must go.
- [ ] **Step 4: Verify in browser.** Load `/settings`, drag both sliders end-to-end, type out-of-range values in both number inputs (expect clamp toast + rollback via `saveConfigField`), and set `sharedThrottleWindowMs: 1200000` via the API to confirm the slider/label degrade gracefully (fill capped at 100%, label shows `20.0m`) rather than overflowing.
- [ ] **Step 5: Commit**

```bash
git add internal/webui/public/views/settings.html internal/webui/public/js/utils/
git commit -m "polish(webui): shared duration formatter and derived slider fill"
```

---

### Task 3: Full verification

**Files:**
- Test: `go build ./...` + manual browser pass (no Go unit covers this WebUI-only PR; `internal/calibrate` tests are untouched unless decision (b) on L105 was taken, in which case update `derive_test.go:268,361-362` and `fragment_test.go:36-37` to the new cap and expect PASS).

- [ ] **Step 1: Build smoke check**

Run: `go build ./...`
Expected: PASS.

- [ ] **Step 2: Confirm no backend drift**

Run: `git diff main -- internal/config/ internal/accounts/ internal/calibrate/`
Expected: empty (unless L105 decision (b) was taken, then only the deliberate `derive.go` clamp + test updates).

- [ ] **Step 3: Confirm the blocking findings are actually closed**

Run: `git diff main -- internal/webui/public/js/config/constants.js` and read the three hunks rather than the exit code.
Expected: `TB_MAX_TOKENS_MIN` back at `5`; `SHARED_THROTTLE_WINDOW_MAX` admits `1200000` (decision a) or calibrate is clamped with tests updated (decision b); `REQUEST_DELAY_MAX` carries the recorded decision from Task 1.

- [ ] **Step 4: Push and report**

Push to the PR branch and reply on the review comment thread with the verification output — the constant values and the `go build` PASS line, not a summary claim.

---

## Findings coverage

Maps every finding in the PR #87 review to the task that closes it. Severity as posted: 🟡 warn, 🟢 nit.

| # | Severity | Finding | Task | Closed by |
|---|---|---|---|---|
| L101 | 🟡 | `REQUEST_DELAY_MAX` 5s→120s with no backend guard | 1 | Decision (a)/(b) recorded; warning text or reduced ceiling |
| L105 | 🟡 | `SHARED_THROTTLE_WINDOW_MAX` 300s vs calibrate 1200000ms | 1 | Max raised to `1200000` (a) or calibrate clamped + tests updated (b) |
| L134 | 🟡 | `TB_MAX_TOKENS_MIN` 5→1 out of scope | 1 | Reverted to `5`; follow-up PR if wanted |
| L3638 | 🟢 | 3x repeated `requestDelayMs \|\| 200` | 2 | `formatDurationMs` + `delayMs` getter, single reference |
| L3672 | 🟢 | `300.0s` instead of minutes | 2 | Shared `formatDurationMs` with m/s branches |
| L3644 | 🟢 | Magic `1199`/`2990` fill divisors | 2 | Fill derived from `(MAX-MIN)/100` |

**Deliberately left open:** no server-side range validation exists for these fields (`config.go` accepts any `int`; enforcement is UI-only). Adding backend clamping is a separate hardening PR, not part of this remediation.
