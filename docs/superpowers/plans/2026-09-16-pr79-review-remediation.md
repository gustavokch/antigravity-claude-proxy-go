# PR #79 Review Remediation — Clearable Stats + Blueprint Dashboard

- **Goal:** Resolve the 6 review findings on PR #79 (`worktree-dashboard-revamp`).
- **Worktree:** `/Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/dashboard-revamp` (all paths below are relative to it).
- **Architecture:** Go tracker + management API backend; Alpine.js dashboard frontend.
- **Tech Stack:** Go `testing` + `httptest`, Alpine.js, Chart.js.
- **Spec:** PR review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/79#issuecomment-5705277033
- **Test runner:** `go test ./...` (Go project — no pytest).

## Revision notes (what changed from the first draft and why)

Each finding was re-checked against the code before planning. Four claims did not survive
verification as originally stated:

1. **Finding 1 is not a live deadlock.** `normalizeHistory` (`internal/stats/tracker.go:160-211`)
   silently drops any family value that is not a `map[string]any`, and `TrackRequest`
   (`:220-277`) only ever writes maps. No load path or write path can put a non-map family value
   into `t.history` today, so the hard assertion at `:362` is unreachable. The fix is still worth
   making — but as panic-safety hardening, not as a bug fix, and the more valuable half of it is
   making the lock itself panic-safe (Task 1).
2. **Findings 2 and 3 rest on a false premise.** `processHistory` passes the *full*
   history to `computeModelMetrics` (`dashboard.js:173`), and `computeModelMetrics`
   (`components/dashboard/stats.js:118-213`) applies neither `timeRange` nor
   `selectedFamilies`/`selectedModels`, and never slices `rows`. Scope `'view'` therefore clears
   exactly what scope `'all'` clears. Rather than making the N-request loop report its partial
   failures more accurately, the loop is removed (Task 2). Finding 3's bucket count is then
   already correct and needs no code change (Task 3).
3. **Finding 4's suggested assertion is at the wrong line and is the weaker fix.** The loop is at
   `internal/api/management_test.go:1332-1336`, not 1371. `len(history) == 1` also makes the test
   flake if the three `Track` calls straddle an hour boundary; asserting over *all* buckets is both
   stricter and flake-proof (Task 4).
4. **Finding 6's suggested handler is wrong.** With `@keydown.tab.prevent`, default Tab is
   suppressed on *every* Tab, but the boundary-only branches move focus only at the ends — so
   forward Tab from the first button lands nowhere and focus stalls. Task 6 uses index arithmetic.

**Decisions taken (user-confirmed):**
- Scope `'view'` collapses into the existing all-clear endpoint; the now-duplicate menu item is removed.
- Task 5 is self-hosting, but **the user downloads the woff2 files**; this plan only wires them up.

---

## Task 1: Panic-safe `ResetModel` (🟡 → hardening, not a live bug)

- **Modify:** `internal/stats/tracker.go` (`ResetModel`, `:321-380`)
- **Test:** `internal/stats/tracker_test.go`

Two independent changes, both cheap:

- **(a) comma-ok** at the `_total` recompute (`:361-364`) — fixes the one known assertion.
- **(b) `defer t.mu.Unlock()`** — fixes the whole class. `Save()` takes `t.mu` itself
  (`:454-468`), so the locked body must be extracted into its own function to be able to defer.
  Without (b), the next hard assertion anyone adds re-opens the same hole; `net/http` recovers a
  handler panic per-connection, so the process survives and the tracker mutex stays locked forever.

**Step 1 — failing test.** White-box, in-package (`tracker_test.go` is `package stats`). It pins
a shape that no current code path can produce, so label it as such:

```go
// A non-map family value cannot be produced by Track or survive normalizeHistory
// today; this pins ResetModel against a future writer or hand-edited stats file.
func TestTracker_ResetModelSkipsNonMapFamilyValue(t *testing.T) {
	tracker, err := NewTracker("")
	if err != nil {
		t.Fatalf("NewTracker failed: %v", err)
	}
	tracker.Track("claude-opus-4-6")

	tracker.mu.Lock()
	for _, hourMap := range tracker.history {
		hourMap["legacy"] = 42 // int where a family map is expected
	}
	tracker.mu.Unlock()

	requests, _, err := tracker.ResetModel("claude", "opus-4-6")
	if err != nil {
		t.Fatalf("ResetModel failed: %v", err)
	}
	if requests != 1 {
		t.Errorf("Expected 1 request discarded, got %d", requests)
	}
	// Mutex must be usable afterwards — this blocks forever if a panic leaked it.
	tracker.Track("gemini-2.5-flash")
}
```

**Step 2 — confirm failure:** `go test ./internal/stats/ -run TestTracker_ResetModelSkipsNonMapFamilyValue -v -timeout 30s`
→ panic on `v.(map[string]any)`, which kills the test binary (in-process there is no `net/http`
recover, so this surfaces as a crash rather than the hang it would be in the running server).

**Step 3 — minimal implementation.** Split the locked body out so the unlock can be deferred:

```go
func (t *Tracker) ResetModel(family, model string) (requests int, buckets int, err error) {
	if model == "" {
		return 0, 0, nil
	}
	requests, buckets = t.removeModelFromHistory(family, model)
	return requests, buckets, t.Save()
}

// removeModelFromHistory takes t.mu itself; the deferred Unlock keeps the mutex
// recoverable if anything inside panics. Save() must be called by the caller,
// after this returns, because Save takes t.mu too.
func (t *Tracker) removeModelFromHistory(family, model string) (requests int, buckets int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// ... body unchanged from the current :327-378, except the _total recompute:
	total := 0
	for k, v := range hourMap {
		if k == "_total" {
			continue
		}
		fam, ok := v.(map[string]any)
		if !ok {
			continue
		}
		total += requestsOf(fam["_subtotal"])
	}
	hourMap["_total"] = total
	// ...
}
```

`ResetAll` (`:301-315`) needs no equivalent change — it only calls `requestsOf`, which handles
every shape without asserting.

**Step 4 — confirm pass:** same command, green. Then `go test ./internal/stats/ -race`.

**Step 5 — commit:**

```bash
git add internal/stats/tracker.go internal/stats/tracker_test.go
git commit -m "fix(stats): make ResetModel panic-safe and skip non-map family values"
```

---

## Task 2: Collapse 'view' scope into the single all-clear call (🟡)

- **Modify:** `internal/webui/public/js/components/dashboard.js` (`requestClearView` `:439-455`, `confirmClear` `:472-504`)
- **Modify:** `internal/webui/public/views/dashboard.html` (`:434-435`)
- **Modify:** `internal/webui/public/js/translations/en.js` (`:389`, `:394`), `pt.js` (`:26`, `:31`)

**Rationale:** the `'view'` row loop issues N sequential `DELETE`s, each triggering a synchronous
`tracker.Save()` (N disk writes), and a mid-loop failure leaves a partial clear under a
"Failed to clear stats." toast. Because `rows` is the unfiltered, unsliced set of every model in
history (see Revision note 2), the whole loop is equivalent to one `clearAllStats(false)` — which
is atomic server-side, writes once, and cannot partially fail. That removes the finding's cause
rather than improving its error message, and needs no new i18n key.

**Step 1 — remove the duplicate menu entry.** `requestClearAll(false)` ("All usage data") already
does exactly what "This view's models" does. Delete the `requestClearView()` button
(`dashboard.html:434-435`), the `requestClearView()` method, the `'view'` branch of `confirmClear`,
and the now-unused `clearScopeView` / `clearViewTitle` keys from both `en.js` and `pt.js`.
Per-model clearing stays available through the per-row ⟲ action (`requestClearModel`), which is
unaffected.

**Step 2 — confirm nothing else references the removed names:**

```bash
grep -rn "clearScopeView\|clearViewTitle\|requestClearView\|'view'" internal/webui/public/
```
→ must return no hits outside the deletions.

**Step 3 — confirm:** `go test ./...` green (the Go suite embeds `public/`, so a broken reference
would not be caught here — the grep in Step 2 is the real gate). Load the dashboard, open the
Clear-stats menu, verify two entries remain and both clear correctly.

**Step 4 — commit:**

```bash
git add internal/webui/public/js/components/dashboard.js internal/webui/public/views/dashboard.html internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js
git commit -m "fix(webui): drop redundant view-scope clear in favour of the atomic all-clear"
```

**Fallback if the menu entry must stay** (e.g. the table is later made filter-aware): keep
`requestClearView` and point its `confirmClear` branch at `store.clearAllStats(false)`, leaving the
title copy as-is. Revisit only when `computeModelMetrics` actually filters.

---

## Task 3: Bucket count in the clear dialog (🔵 — no change required)

**Finding:** `requestClearView` passes `Object.keys(this.historyData).length` (all buckets) where
per-model code uses `countBucketsFor`, allegedly overstating scope.

**Verification result: not a defect.** `rows` covers every model in `historyData`, and every
bucket in `historyData` holds at least one model (`TrackRequest` always creates a family, and
`removeModelFromHistory` deletes buckets left empty). The all-buckets count is therefore exactly
the number of buckets the action touches. After Task 2 the call site is deleted outright, so no
edit is needed either way.

**Action:** none. Reply on the review thread with the reasoning above so the finding is closed
explicitly rather than silently. No commit.

---

## Task 4: Assert across all buckets, not just the last (🔵 test rigor)

- **Modify:** `internal/api/management_test.go` (`TestManagement_ClearStatsRoutes`, first subtest, `:1332-1342`)

**Step 1 — the defect.** `for _, v := range history { hourMap = v.(map[string]any) }` keeps only
the last bucket, so a second surviving bucket escapes the assertion; and if `history` were empty,
`hourMap` is nil and both assertions pass vacuously.

The reviewer's suggested `len(history) == 1` guard closes the hole but introduces a rare flake:
the three `Track` calls at `:1308-1310` use wall-clock hour keys, so an hour-boundary straddle
yields 2 buckets and hard-fails a correct implementation. Assert the invariant over every bucket
instead — stricter *and* boundary-safe:

```go
history := server.tracker.GetHistory()
if len(history) == 0 {
	t.Fatal("expected history to survive a single-model clear, got 0 buckets")
}
geminiSeen := false
for hourKey, v := range history {
	hourMap, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("bucket %s is not a map: %T", hourKey, v)
	}
	if _, exists := hourMap["claude"]; exists {
		t.Errorf("bucket %s: expected claude family cleared", hourKey)
	}
	if _, exists := hourMap["gemini"]; exists {
		geminiSeen = true
	}
}
if !geminiSeen {
	t.Error("expected gemini family to survive in some bucket")
}
```

**Step 2 — confirm pass:** `go test ./internal/api/ -run TestManagement_ClearStatsRoutes -v`
green. Current code produces exactly 1 bucket; this hardens the test without pinning that count.

**Step 3 — commit:**

```bash
git add internal/api/management_test.go
git commit -m "test(api): assert cleared-model invariant across every bucket"
```

---

## Task 5: Self-host IBM Plex fonts (🔵 external dependency)

- **Modify:** `internal/webui/public/index.html` (`:21-24`), `internal/webui/public/css/blueprint.css` (`:110-125`)
- **Create:** `internal/webui/public/fonts/`

**Constraint:** `internal/webui/embed.go:11` is `//go:embed all:public`, so every byte added under
`public/` is compiled into the binary. Five latin-subset woff2 files ≈ 150–200 KB of permanent
binary growth. That is the whole cost of the change; it buys offline first paint and removes a
third-party request from a local admin UI.

**Step 1 — user supplies the files.** Per the decision above, the user downloads and places, in
`internal/webui/public/fonts/`:

| File | Face |
|---|---|
| `ibm-plex-sans-400.woff2` | IBM Plex Sans Regular, latin |
| `ibm-plex-sans-500.woff2` | IBM Plex Sans Medium, latin |
| `ibm-plex-sans-600.woff2` | IBM Plex Sans SemiBold, latin |
| `ibm-plex-mono-400.woff2` | IBM Plex Mono Regular, latin |
| `ibm-plex-mono-500.woff2` | IBM Plex Mono Medium, latin |

Weights are taken from the current Google Fonts URL (`index.html:24`: Sans 400/500/600,
Mono 400/500) — no face is added or dropped.

**Step 2 — licence.** IBM Plex ships under OFL-1.1, which requires the licence to travel with the
binaries. Add `internal/webui/public/fonts/LICENSE.txt` (the upstream OFL-1.1 text) in the same
commit. Do not skip this: the fonts are now redistributed inside every built binary.

**Step 3 — wire them up.** Prepend to the `/* === Type: IBM Plex === */` block in `blueprint.css`
one `@font-face` per file, each with `font-display: swap;` and
`src: url("../fonts/<file>.woff2") format("woff2");` (paths are relative to the CSS file, which
lives in `public/css/`). Then delete `index.html:22-24` — both `preconnect` links and the
Google Fonts stylesheet link.

**Step 4 — confirm:**
- `grep -rn "googleapis\|gstatic" internal/webui/public/` → no hits.
- Rebuild, load the dashboard with the network offline, run `document.fonts.check('1em "IBM Plex Sans"')`
  in the console → `true`; no failed font requests in the Network tab.
- `go test ./...` unaffected.

**Step 5 — commit:**

```bash
git add internal/webui/public/fonts internal/webui/public/css/blueprint.css internal/webui/public/index.html
git commit -m "feat(webui): self-host IBM Plex fonts, drop Google Fonts dependency"
```

---

## Task 6: Focus trap in the clear-stats dialog (🔵 a11y)

- **Modify:** `internal/webui/public/views/dashboard.html` (`:543`)

**Step 1 — the defect.** Tab from either dialog button moves focus behind the overlay. Escape
already closes the dialog (`:542`) and focus already lands on Keep when it opens
(`dashboard.js:414-421`), so only the trap is missing.

**Step 2 — implementation.** `.prevent` suppresses the browser's own Tab handling on *every* Tab,
so the handler must compute the next target on every Tab, not just at the ends — otherwise forward
Tab from the first button goes nowhere:

```html
<div class="bp-modal-box" role="alertdialog" aria-modal="true" :aria-label="clearConfirm.title"
    @keydown.tab.prevent="
        const btns = Array.from($el.querySelectorAll('button'));
        if (btns.length) {
            const i = btns.indexOf(document.activeElement);
            const step = $event.shiftKey ? -1 : 1;
            btns[(i + step + btns.length) % btns.length].focus();
        }
    ">
```

`i === -1` (focus outside the box) resolves to index `0` forward / `btns.length - 1` backward,
which is the correct recovery in both directions.

**Step 3 — confirm:** in headless Chrome, open the dialog and verify Tab cycles Keep → Clear stats
→ Keep and Shift+Tab cycles the reverse, with `document.activeElement` never leaving the box; then
re-verify the existing Escape-closes and focus-on-Keep behaviour still holds.

**Step 4 — commit:**

```bash
git add internal/webui/public/views/dashboard.html
git commit -m "fix(webui): trap Tab focus inside clear-stats dialog"
```

---

## Final gate

```bash
go test ./...
go vet ./...
gofmt -l internal/
grep -rn "clearScopeView\|clearViewTitle\|requestClearView" internal/webui/public/   # expect no hits
grep -rn "googleapis\|gstatic" internal/webui/public/                                # expect no hits
git push fork worktree-dashboard-revamp
```

Then reply on the PR thread closing Finding 3 with the Task 3 reasoning, and noting that Finding 1
was hardening rather than a reachable deadlock.
