# Audit — Accounts view (`/#accounts`)

Target: `internal/webui/public/views/accounts.html` (865 lines), `js/components/account-manager.js` (714 lines), `css/src/input.css`, `index.html` shell.
Date: 2026-08-28. Mode: Operate.

## Audit Health Score

| # | Dimension | Score | Key finding |
|---|-----------|-------|-------------|
| 1 | Accessibility | 1 | Enable/disable toggle explicitly kills its own focus ring (`peer-focus:outline-none`) and has no accessible name; Import is unreachable by keyboard |
| 2 | Performance | 2 | Quota bar is bound through a non-reactive `x-data` initializer, so it never updates after a refresh; three unpinned CDN dependencies with no SRI |
| 3 | Responsive Design | 1 | 7-column table with `min-w-[200px]` and no `overflow-x-auto` wrapper; touch targets 24–32px throughout |
| 4 | Theming | 2 | Design tokens exist in `input.css` but the view uses raw Tailwind palette classes; `gray-*` and `zinc-*` mixed in the same rows; `style.css` has no reproducible build |
| 5 | Implementation Integrity | 1 | Empty state instructs `npm run accounts:add` — there is no `package.json` in this Go repository |
| **Total** | | **7/20** | **Poor (major overhaul)** |

> Detector caveat: `scripts/detect.mjs` ran DEGRADED — `htmlparser2`, `css-select`, `css-tree` and `domutils` were unavailable, so it fell back to regex matching and returned `[]`. Custom properties, selector matching and computed contrast were not evaluated. Every finding below was verified by hand; contrast ratios were computed directly from the resolved hex values.

## Implementation Integrity Verdict

**Fail.** The view does not express one coherent system. Three separate visual vocabularies coexist inside a single card: hand-written Tailwind utility strings (Google table), the project's own `status-pill-*` component classes (tier and source cells), and stock DaisyUI `badge-*` classes (Claude Code table). The two tables in the same view style the same concepts differently — the Google row uses `hover:bg-white/5`, the Claude Code row uses `table-row-hover`, a class that is defined nowhere in `input.css` or the compiled `style.css`, so those rows have no hover feedback at all.

The strongest single piece of evidence is content, not styling: the Google empty state tells a new user to run `npm run accounts:add`. This repository has no `package.json` and no such script; the Node backend was replaced by Go in commit `0cb3daa`. The instruction survived the port. A user who follows the primary onboarding path of this view hits a dead end.

## Executive Summary

- Audit Health Score: **7/20** (Poor)
- Issues found: **6 P0/P1-blocking-class**, **9 P1**, **8 P2**, **5 P3**
- Top issues:
  1. `npm run accounts:add` in the empty state does not exist in this project (misleading content, blocks first-run).
  2. Google table empty state uses `colspan="6"` against a 7-column header.
  3. The account enable/disable toggle removes its own focus indicator and exposes no accessible name.
  4. Import is keyboard-unreachable — the file input is `class="hidden"` inside a non-focusable `<label>`.
  5. Quota percentage and bar are computed once in `x-data` and never recompute; after Refresh or Reload the numbers are stale.
  6. Body copy at `text-gray-600` on `--color-space-900` measures **2.42:1** — well below WCAG AA's 4.5:1.

## Detailed Findings by Severity

### P0 — Blocking

**[P0] Onboarding instruction points at a non-existent command**
- Location: `views/accounts.html:212`
- Category: Implementation Integrity
- Impact: The Google empty state is the first thing a user with zero accounts sees. Its documented alternative to the OAuth button is `npm run accounts:add`. There is no `package.json` at the repo root and no such target in the `Makefile`. A user who cannot complete OAuth has no working path forward.
- Recommendation: Replace with the real Go entry point, or delete the alternative and leave the OAuth button as the single path.
- Suggested command: `/impeccable clarify`

**[P0] Import control cannot be reached by keyboard**
- Location: `views/accounts.html:70-79`
- Category: Accessibility
- Impact: The `<input type="file">` carries `class="hidden"` (`display:none`), which removes it from the tab order. Its parent `<label class="btn ...">` is not a focusable element. Import is therefore mouse-only. Export, immediately beside it, is a real `<button>` and works.
- WCAG: 2.1.1 Keyboard (Level A)
- Recommendation: Keep the input in the accessible tree (`sr-only` with `absolute inset-0 opacity-0`, or a visible-on-focus clip), or replace the label with a `<button>` that calls `input.click()`.
- Suggested command: `/impeccable harden`

### P1 — Major

**[P1] Enable/disable toggle has no focus indicator and no accessible name**
- Location: `views/accounts.html:227-233` (Google), `409-417` (Claude Code)
- Category: Accessibility
- Impact: The `<input type="checkbox" class="sr-only peer">` is the only control that enables or disables an account. Its styled sibling `<div>` carries `peer-focus:outline-none`, which deliberately suppresses the only focus affordance the sr-only input could project. A keyboard user cannot tell which row's toggle is focused. The input also has no `aria-label`, no `<span class="sr-only">`, and no associated text, so a screen reader announces an unlabeled checkbox.
- WCAG: 2.4.7 Focus Visible (AA), 4.1.2 Name, Role, Value (A)
- Recommendation: Replace `peer-focus:outline-none` with `peer-focus-visible:ring-2 peer-focus-visible:ring-blue-500 peer-focus-visible:ring-offset-2` on the track, and add `:aria-label="'Enable account ' + acc.email"`.
- Suggested command: `/impeccable harden`

**[P1] Quota cell is click-only, with a click handler on a `<td>`**
- Location: `views/accounts.html:250`
- Category: Accessibility
- Impact: `@click="openQuotaModal(acc)"` sits on a `<td class="py-4 cursor-pointer">`. The cell is not focusable, has no role, and responds to no key. The per-model quota breakdown modal is unreachable without a mouse.
- WCAG: 2.1.1 Keyboard (A), 4.1.2 Name, Role, Value (A)
- Recommendation: Wrap the cell content in a `<button type="button">` with an accessible label naming the account.
- Suggested command: `/impeccable harden`

**[P1] Empty-state `colspan` does not match the header**
- Location: `views/accounts.html:182`
- Category: Implementation Integrity
- Impact: The Google `<thead>` declares 7 `<th>` (Enabled, Account, Source, Tier, Quota, Health, Operations). The empty state's `<td colspan="6">` covers six. The `max-w-lg mx-auto` centering is measured against the wrong width, and a stray seventh column is created. The Claude Code table's 6-column `colspan="6"` is correct, which is why only one of the two empty states looks off-center.
- Recommendation: `colspan="7"`.
- Suggested command: `/impeccable layout`

**[P1] Quota display is not reactive**
- Location: `views/accounts.html:251` — `<div x-data="{ quota: getMainModelQuota(acc) }">`
- Category: Performance / correctness
- Impact: Alpine evaluates an `x-data` object literal once, at element initialization. Inside `x-for`, the row's DOM node is reused when `$store.data.accounts` updates, so the initializer does not re-run. After Refresh, Reload, or the background `fetchData()` following a toggle, the quota bar and percentage still show the values from first paint. The row's status, tier and health cells all update correctly, so the stale bar looks authoritative.
- Recommendation: Bind the value directly (`x-data="{ get quota() { return getMainModelQuota(acc) } }"`) or drop the wrapper and call the method in each binding.
- Suggested command: `/impeccable optimize`

**[P1] Body copy fails WCAG AA contrast**
- Location: `views/accounts.html:207` (`noAccountsDesc`), `213` (`or`), `348` (`noSearchResults`), `524` (`disabledAccountsNote`), `756`, `789`, `604`
- Category: Accessibility
- Impact: `text-gray-600` (`#52525b`) on `--color-space-900` (`#121215`) measures **2.42:1**. Required: 4.5:1. This is the empty-state explanation, the "no results" message, the disabled-accounts note, and the threshold-modal help text — precisely the copy that explains what to do next.
- WCAG: 1.4.3 Contrast (Minimum) — AA
- Recommendation: Move to `--color-text-tertiary` (`#a1a1aa`, 7.30:1) for explanatory copy. Reserve `gray-600` for decorative rules only.
- Suggested command: `/impeccable colorize`

**[P1] Table column headers fail contrast**
- Location: `views/accounts.html:169-175`, `365-371`, `591-599`
- Category: Accessibility
- Impact: Every `<th>` is `text-[10px] font-bold text-gray-500` (`#6b7280`) on `#121215` — **3.87:1**, below the 4.5:1 required for 10px text. The same class colors the icon-button rest state and the `(acc.id)` monospace identifier.
- WCAG: 1.4.3 (AA)
- Recommendation: `--color-text-tertiary` (`#a1a1aa`) at 11px minimum.
- Suggested command: `/impeccable typeset`

**[P1] Banned-account status is the least readable text on the page**
- Location: `views/accounts.html:277-289`
- Category: Accessibility
- Impact: The BANNED label is `text-red-700` (`#b91c1c`) at **2.89:1**; the accompanying reason paragraph is `text-[9px] text-red-600/70` at roughly **2.15:1**. This is the highest-urgency state in the table and it is rendered less legibly than every neutral row. 9px also falls below any practical reading floor.
- WCAG: 1.4.3 (AA)
- Recommendation: `text-red-400` (`#f87171`, 5.9:1) for the label, 11px `text-red-300` for the reason, and keep the dark red for the dot only.
- Suggested command: `/impeccable colorize`

**[P1] Seven-column table with no horizontal scroll container**
- Location: `views/accounts.html:167`, `364`
- Category: Responsive Design
- Impact: `<table class="w-full">` sits directly inside `.view-card`, which sets `overflow: hidden`. The header row declares fixed widths (`w-16`, `w-20`, `w-32`) plus `min-w-[200px]` on the email column — roughly 620px of hard minimum. Below that width the table is clipped by the card, not scrolled. The main pane's `overflow-auto` cannot rescue content clipped by the card's own `overflow: hidden`.
- Recommendation: Wrap each table in `<div class="overflow-x-auto">`, or switch to a stacked card layout below `md`.
- Suggested command: `/impeccable adapt`

**[P1] Touch targets below the 44px minimum throughout**
- Location: `views/accounts.html:60-110` (header actions, `h-8` = 32px), `305-336` (row icon buttons, `p-2` = 32px), `227` (toggle, 36×20px), `447-460` (Claude Code `btn-xs` = 24px)
- Category: Responsive Design / Accessibility
- Impact: Delete, Refresh and Settings are 32px squares packed at `gap-2` in the right-hand column. On touch, adjacent-target error puts Delete one mis-tap away from Refresh, and Delete is destructive. The Claude Code table's `btn-xs` Test and Delete are 24px tall.
- WCAG: 2.5.8 Target Size (Minimum) — AA (24px absolute floor with spacing), 2.5.5 (AAA, 44px)
- Recommendation: Raise row actions to `min-h-11 min-w-11` on coarse pointers and increase the gap between Refresh and Delete.
- Suggested command: `/impeccable adapt`

**[P1] No `prefers-reduced-motion` handling anywhere in the project**
- Location: `css/src/input.css` (whole file), `views/accounts.html` — `animate-spin` at 87, 148, 316, 431; `animate-fade-in`; `x-transition` at 570
- Category: Accessibility
- Impact: Zero occurrences of `prefers-reduced-motion` in any source file. Reload, Refresh, Auto Import and Test all spin indefinitely while their request is in flight, and multiple rows can spin at once.
- WCAG: 2.3.3 Animation from Interactions (AAA), and a real vestibular-comfort concern at AA in practice
- Recommendation: Add a media query that swaps the spinner for a static opacity pulse and shortens transitions to ~1 frame — preserve the state change, do not delete the feedback.
- Suggested command: `/impeccable animate`

**[P1] Credential export fires with no confirmation**
- Location: `js/components/account-manager.js:454-484`
- Category: Implementation Integrity
- Impact: Every email in this view is masked through `Redact.email()`. One click on Export writes the unredacted `/api/accounts/export` payload — account records including credential material — to the user's Downloads folder, with no dialog, no warning and no mention of what the file contains. The deletion of a single account, by contrast, is gated behind a two-step "Dangerous Operation" modal. The severity model is inverted.
- Recommendation: Gate Export behind the same confirmation pattern, and state in the dialog what the file contains.
- Suggested command: `/impeccable harden`

### P2 — Minor

**[P2] Provider tabs are not tabs to assistive technology**
- Location: `views/accounts.html:3-27`, panels at `166` and `363`
- Category: Accessibility
- Impact: Two `<button>` elements switch `accountTab`. No `role="tablist"`, `role="tab"`, `aria-selected`, `aria-controls`, `role="tabpanel"`, and no arrow-key navigation. A screen-reader user gets two unrelated buttons with no indication that one of them is currently active — the active state is conveyed by background color alone.
- WCAG: 4.1.2 (A), 1.4.1 Use of Color (A)
- Recommendation: Apply the ARIA tabs pattern, or use `aria-pressed` on the two buttons as a lighter fix.
- Suggested command: `/impeccable harden`

**[P2] Invalid Tailwind class produces no padding**
- Location: `views/accounts.html:11`, `22`
- Category: Implementation Integrity
- Impact: `py-0.2` is not on Tailwind's spacing scale and compiles to nothing. The count pills in both provider tabs get horizontal padding and zero vertical padding, so they read as cramped against the tab label.
- Recommendation: `py-0.5`.
- Suggested command: `/impeccable layout`

**[P2] `flex-1` applied to a `<th>`**
- Location: `views/accounts.html:170`, `366`
- Category: Responsive Design
- Impact: `flex-1` has no effect on a `display: table-cell` element. The intent — let the email column absorb remaining width — is carried entirely by `min-w-[200px]` and by the other columns' fixed widths. The class is misleading to the next editor.
- Recommendation: Remove it; the fixed widths already produce the intended behavior.
- Suggested command: `/impeccable layout`

**[P2] Undefined CSS class on Claude Code rows**
- Location: `views/accounts.html:406` — `class="table-row-hover group"`
- Category: Theming
- Impact: `table-row-hover` appears in neither `input.css` nor the compiled `style.css`. Claude Code rows have no hover feedback; the Google table's rows do, via `hover:bg-white/5`. Two tables in one card behave differently on hover.
- Recommendation: Define `.table-row-hover` in `input.css` and apply it to both tables.
- Suggested command: `/impeccable polish`

**[P2] Translation string injected as HTML**
- Location: `views/accounts.html:527` — `x-html="$store.global.t('disabledAccountsNote')"`
- Category: Implementation Integrity
- Impact: `x-html` bypasses escaping. The source is a bundled local locale file today, so this is not currently exploitable, but it establishes an unescaped sink in the locale pipeline — a pattern that becomes a real injection the moment locales are user-supplied or fetched.
- Recommendation: Split the note into plain text plus a separate anchor element and use `x-text`.
- Suggested command: `/impeccable harden`

**[P2] A third of the view is untranslated**
- Location: `views/accounts.html:288` (APPEAL), `302` (appeal tooltip), `308` (Account Settings), `445`, `450` (Test / Testing…), `435-441` (cooldown / ready), `475-482` (token and name placeholders), `717` (Account Settings heading), `729-732` (threshold explanation), `741` (Minimum Quota), `760` (Reset to Default), `768` (Per-Model Overrides), `831` (Add Override), `593` (Email header), `798-801` (scoring reference)
- Category: Implementation Integrity
- Impact: `en.js` and `pt.js` both carry keys for the Google table, but the entire Claude Code tab, both modals' bodies, and the health inspector are hardcoded English. A `pt` user sees a half-translated screen. Both locale files are complete for the keys the view *does* call — `type` and `priority` are the only two missing, and both have inline fallbacks.
- Recommendation: Extract the remaining literals to `en.js`/`pt.js`.
- Suggested command: `/impeccable clarify`

**[P2] Unpinned third-party scripts with no integrity check**
- Location: `index.html:11-15`
- Category: Performance
- Impact: `chart.js` (unversioned), `@alpinejs/collapse@3.x.x` and `alpinejs@3.x.x` load from jsdelivr. `chart.js` is not deferred, so it blocks parsing. For a self-hosted proxy console this also means the whole UI depends on outbound internet access, and a floating major-version range can change the app without a commit.
- Recommendation: Vendor all three into `public/js/vendor/`, pin exact versions, and serve them from the Go embed.
- Suggested command: `/impeccable optimize`

**[P2] Silent failures in the Claude Code data path**
- Location: `js/components/account-manager.js:539` (`if (!response.ok) return;`), `445-447` (`catch` → `console.error` only)
- Category: Implementation Integrity
- Impact: If `/api/claudecode/accounts` returns non-2xx, the function returns without touching `ccAccounts`, so the table renders its "No Claude Code Accounts" empty state. A server error is indistinguishable from a genuinely empty pool, and the empty state's Auto Import button is the wrong next action.
- Recommendation: Set an error state and render a distinct "could not load" panel.
- Suggested command: `/impeccable harden`

### P3 — Polish

**[P3] Design tokens defined but bypassed** — `input.css:26-36` declares `--color-text-primary` through `--color-text-quaternary`; `accounts.html` uses none of them, reaching for `text-gray-*`, `text-zinc-*` and `text-white` directly. `gray-400` and `zinc-400` appear in the same row markup at effectively identical luminance. Category: Theming. → `/impeccable polish`

**[P3] Same semantic, two different colors** — the row quota bar uses `bg-emerald-500`; the health inspector's score bar uses `bg-neon-green` (`#10b981`) for the same "healthy" reading, with different thresholds (50/20 vs 70/50). Category: Theming. → `/impeccable colorize`

**[P3] Heading hierarchy skips a level** — `h1` at line 33, then `h3` in both empty states (204, 388) with no `h2` between. Category: Accessibility, WCAG 1.3.1. → `/impeccable harden`

**[P3] Dead component state** — `toggling: false` is declared at `account-manager.js:12` and referenced nowhere. Category: Implementation Integrity. → `/impeccable distill`

**[P3] Search box is shared across tabs** — both tabs bind to the same `searchQuery` (lines 47, 121). Switching from Google to Claude Code silently carries the filter over, and the Claude Code input, unlike the Google one, has no `aria-label`. Category: Implementation Integrity. → `/impeccable clarify`

## Patterns & Systemic Issues

1. **Contrast is systematically one step too dark.** `gray-600` for body copy and `gray-500` for labels recur across the whole view and fail AA in every instance. This is a single token decision repeated, not seven separate mistakes — fixing the two token choices fixes all of them.
2. **Keyboard access was never a design input.** Four aria attributes appear in 865 lines. The toggle, the quota cell, and the Import control are each mouse-only for a different structural reason, which means no keyboard pass was ever run over this view.
3. **The Claude Code tab was built to a different standard than the Google tab.** It uses DaisyUI `badge-*` where the Google tab uses project `status-pill-*`; a nonexistent hover class where the Google tab uses a utility; hardcoded English where the Google tab is fully translated; `btn-xs` where the Google tab uses `h-8`. It reads as a later addition that did not adopt the incumbent system.
4. **Node-era residue survives the Go port.** `npm run accounts:add` is the visible symptom; the absent Tailwind config is the structural one — `style.css` is a committed 200KB build artifact with no `tailwind.config.js`, no `package.json` and no Makefile target, so `input.css` cannot currently be rebuilt.

## Positive Findings

- **The token layer itself is well-built.** `input.css` defines a coherent flat dark palette with a real four-step text hierarchy, layered backgrounds, and a `status-pill` component family with consistent `10%` fill / `20%` border construction. The problem is adoption, not design.
- **Destructive delete is properly gated.** The two-step modal names the account, states the consequence in plain language, and disables itself while in flight. Database-sourced accounts are correctly non-deletable with an explanatory tooltip rather than a hidden button.
- **Optimistic updates with rollback are handled correctly.** `toggleAccount` (`account-manager.js:106-146`) applies the change immediately, rolls it back on both error paths, and re-fetches to confirm server state.
- **Email redaction is applied consistently in the UI** — table, tooltip, delete confirmation, quota modal, threshold modal and health inspector all route through `Redact.email()`.
- **Native `<dialog>` for all three modals** gives correct focus trapping, Escape handling and backdrop semantics for free.
- **Both empty states are genuinely designed** — icon, heading, explanation and a primary action, not a bare "no data" line.

## Recommended Actions

1. **[P0] `/impeccable clarify`** — Replace `npm run accounts:add` with the real Go command, and extract the ~15 hardcoded English strings in the Claude Code tab, both modals, and the health inspector into `en.js`/`pt.js`.
2. **[P0/P1] `/impeccable harden`** — Keyboard and ARIA pass: restore the toggle's focus ring and give it a name, make the file input focusable, convert the quota cell to a button, apply the tabs pattern, label the Claude Code token and name inputs, replace `x-html` with `x-text`, surface Claude Code load errors, and gate Export behind a confirmation that says what the file contains.
3. **[P1] `/impeccable colorize`** — Lift body copy from `gray-600` to `--color-text-tertiary`, lift the banned state from `red-700`/`red-600/70` to `red-400`/`red-300`, and unify the quota and health bars on one scale.
4. **[P1] `/impeccable adapt`** — Wrap both tables in `overflow-x-auto` (or stack below `md`), raise row actions to 44px on coarse pointers, widen the gap around Delete, and let the header action group wrap.
5. **[P1] `/impeccable optimize`** — Make the quota binding reactive, vendor and pin the three CDN scripts into the Go embed.
6. **[P1] `/impeccable animate`** — Add a `prefers-reduced-motion` branch that keeps the loading feedback but drops the continuous spin.
7. **[P2] `/impeccable typeset`** — Raise the 9px and 10px text to 11px minimum and re-check the header contrast at the new size.
8. **[P2] `/impeccable layout`** — Fix `colspan="7"`, `py-0.5`, and remove `flex-1` from the two `<th>`.
9. **[P3] `/impeccable polish`** — Define `.table-row-hover`, migrate the view onto the existing text tokens, settle `gray-*` vs `zinc-*`, and drop the dead `toggling` state.

Re-run `/impeccable audit /#accounts` after the fixes to confirm the score moves.
