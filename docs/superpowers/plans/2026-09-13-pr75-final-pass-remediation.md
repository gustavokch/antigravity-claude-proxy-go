# PR #75 Final-Pass Remediation — WebUI Import Limits + Alias Parity

**Goal:** Close the two remaining gaps found in the final review pass of PR #75: (1) WebUI "Import defaults" produces Claude 5 allowlist entries with no token limits, so the 1M/128K policy silently does not apply on that path; (2) the catalogue↔router alias-parity invariant is guarded only for `claude-fable-5-1` while `sonnet-5`/`opus-5`/`fable-5` violate it today.

**Architecture:** Go proxy. Claude Code endpoint resolves client model names via `claudecode.Router` (allowlist ID/Alias/Aliases only). `/v1/models` discovery advertises `DefaultClaudeCatalogue()`. WebUI `importCCDefaults()` posts user allowlists that replace `DefaultAllowlist()` when non-empty. `max_tokens` policy: `claudeCodeEntryMaxOutput` → `applyMaxTokensPolicy` (server.go).

**Tech Stack:** Go 1.x, `go test ./internal/...`, Alpine.js WebUI (no JS test harness — JS changes verified by Go-side fallback tests + manual check).

**Spec reference:** PR #75 review comment (final pass), commits `499b325`/`eed2a5a`/`d1235ac`/`23f6b4d`.

---

## Task 1: Extend alias-parity guard to all Claude 5 entries (Red)

**Files:**
- Test: `internal/claudecode/router_test.go`

**Consumes:** `DefaultAllowlist()`, `DefaultClaudeCatalogue()`
**Produces:** Failing test proving sonnet-5/opus-5/fable-5 alias sets drift.

**Step 1 — Write failing test:**

Generalize `TestDefaultAllowlist_Fable51AliasesMatchCatalogue` into a table test over all four entries. Rename to `TestDefaultAllowlist_Claude5AliasesMatchCatalogue` (keep the existing normalize helper and both-direction set comparison; delete the fable-5-1-only test):

```go
func TestDefaultAllowlist_Claude5AliasesMatchCatalogue(t *testing.T) {
	normalize := func(names ...string) map[string]bool { /* unchanged */ }

	for _, id := range []string{"claude-fable-5", "claude-fable-5-1", "claude-opus-5", "claude-sonnet-5"} {
		t.Run(id, func(t *testing.T) {
			var routerEntry *ModelConfig
			for i, m := range DefaultAllowlist() {
				if m.ID == id {
					routerEntry = &DefaultAllowlist()[i]
				}
			}
			if routerEntry == nil {
				t.Fatalf("%s missing from DefaultAllowlist", id)
			}
			routerSet := normalize(append([]string{routerEntry.ID, routerEntry.Alias}, routerEntry.Aliases...)...)

			var catalogueEntry *DiscoveredModel
			for i, m := range DefaultClaudeCatalogue() {
				if m.ID == id {
					catalogueEntry = &DefaultClaudeCatalogue()[i]
				}
			}
			if catalogueEntry == nil {
				t.Fatalf("%s missing from DefaultClaudeCatalogue", id)
			}
			catalogueSet := normalize(append([]string{catalogueEntry.ID}, catalogueEntry.Aliases...)...)

			for name := range catalogueSet {
				if !routerSet[name] {
					t.Errorf("alias %q in catalogue but not in router default allowlist", name)
				}
			}
			for name := range routerSet {
				if !catalogueSet[name] {
					t.Errorf("alias %q in router default allowlist but not in catalogue", name)
				}
			}
		})
	}
}
```

**Step 2 — Confirm failure:**

```bash
go test ./internal/claudecode/ -run TestDefaultAllowlist_Claude5AliasesMatchCatalogue -v
```

Expected: FAIL for `claude-sonnet-5` (`sonnet`, `claude-5-sonnet` vs `sonnet-5`), `claude-opus-5` (`opus`, `claude-5-opus` vs `opus-5`), `claude-fable-5` (`fable`, `claude-fable` vs `fable-5`). PASS for `claude-fable-5-1`.

**Step 3 — No implementation (Red only).**

**Step 4 — n/a.**

**Step 5 — Commit:**

```bash
git add internal/claudecode/router_test.go
git commit -m "test(claudecode): extend alias-parity guard to all Claude 5 entries"
```

---

## Task 2: Reconcile catalogue and router alias sets (Green)

**Decision (made in review):** union the sets. Bare short names (`sonnet`, `opus`, `fable`) stay at the Claude Code layer — Claude Code CLI sends them — and fail honestly upstream on accounts that do not publish the model. `sonnet-5`/`opus-5`/`fable-5` join the catalogue so discovery advertises what the router matches.

**Files:**
- Modify: `internal/claudecode/client.go` (catalogue aliases for fable-5, opus-5, sonnet-5)
- Modify: `internal/claudecode/router.go` (allowlist aliases: add `sonnet`, `opus`, `fable`, `claude-5-sonnet`, `claude-5-opus`, `claude-fable` to the matching entries)
- Modify: `internal/webui/public/js/components/models.js` (`importCCDefaults` alias strings — same union)

**Consumes:** Task 1 failing test.
**Produces:** Identical alias sets on both sides; WebUI import matches.

**Step 1 — Test already failing (Task 1).**

**Step 2 — Re-run to confirm still red:** `go test ./internal/claudecode/ -run Claude5Aliases -v`

**Step 3 — Minimal implementation:**

`router.go`:
- fable-5: `Aliases: []string{"fable-5", "claude-fable-5", "fable", "claude-fable"}`
- opus-5: `Aliases: []string{"opus-5", "claude-opus-5", "opus", "claude-5-opus"}`
- sonnet-5: `Aliases: []string{"sonnet-5", "claude-sonnet-5", "sonnet", "claude-5-sonnet"}`

`client.go`:
- fable-5: `Aliases: []string{"claude-fable-5", "fable-5", "fable", "claude-fable"}`
- opus-5: `Aliases: []string{"claude-opus-5", "opus-5", "opus", "claude-5-opus"}`
- sonnet-5: `Aliases: []string{"claude-sonnet-5", "sonnet-5", "sonnet", "claude-5-sonnet"}`

`models.js` `importCCDefaults`:
- fable-5 row alias: `'claude-fable-5, fable-5, fable, claude-fable'`
- opus-5 row alias: `'claude-opus-5, opus-5, opus, claude-5-opus'`
- sonnet-5 row alias: `'claude-sonnet-5, sonnet-5, sonnet, claude-5-sonnet'`

Note: bare `sonnet`/`opus`/`fable` added to the CC allowlist only — NOT to `modelcatalog.routingAliases`. `TestNonCloudCodeModelsHaveNoFixedRoutingAliases` and the unpublished-names assertions in `TestClaudeRoutingAliases` must keep passing unchanged; if they fail, the wrong layer was edited.

**Step 4 — Confirm pass:**

```bash
go test ./internal/claudecode/ -v
go test ./internal/modelcatalog/ -v
```

**Step 5 — Commit:**

```bash
git add internal/claudecode/client.go internal/claudecode/router.go internal/webui/public/js/components/models.js
git commit -m "fix(claudecode): align Claude 5 alias sets across catalogue, router, and WebUI"
```

---

## Task 3: Server-side limit fallback for user allowlist entries (TDD)

**Files:**
- Modify: `internal/api/server.go` (`claudeCodeEntryMaxOutput`, ~L1546)
- Test: `internal/api/claudecode_proxy_test.go`

**Consumes:** `claudecode.Config.Allowlist` entries with `MaxOutputTokens: 0` (WebUI-imported).
**Produces:** Entries with a zero limit inherit the `DefaultAllowlist` limit for the same ID.

**Step 1 — Write failing test:**

```go
func TestClaudeCodeEntryMaxOutput_ZeroLimitFallsBackToDefault(t *testing.T) {
	cfg := claudecode.Config{Allowlist: []claudecode.ModelConfig{{
		ID:      "claude-opus-5",
		Enabled: true,
		// MaxOutputTokens: 0 — as produced by WebUI importCCDefaults
	}}}
	if got := claudeCodeEntryMaxOutput(cfg, "claude-opus-5"); got != 128000 {
		t.Errorf("got %d, want 128000 (default fallback for zero limit)", got)
	}
}
```

Also assert explicit caps still win (admin-set cap below default is preserved — policy doc says admin cap always wins):

```go
	cfg.Allowlist[0].MaxOutputTokens = 64000
	if got := claudeCodeEntryMaxOutput(cfg, "claude-opus-5"); got != 64000 {
		t.Errorf("got %d, want 64000 (explicit cap preserved)", got)
	}
```

**Step 2 — Confirm failure:**

```bash
go test ./internal/api/ -run TestClaudeCodeEntryMaxOutput_ZeroLimitFallsBackToDefault -v
```

Expected: FAIL — got 0.

**Step 3 — Minimal implementation in `claudeCodeEntryMaxOutput`:**

```go
for _, item := range allowlist {
	if item.ID == canonicalID {
		if item.MaxOutputTokens > 0 {
			return item.MaxOutputTokens
		}
		for _, d := range claudecode.DefaultAllowlist() {
			if d.ID == canonicalID {
				return d.MaxOutputTokens
			}
		}
		return 0
	}
}
```

Do NOT apply the fallback when `allowlist` itself is the default (already the 128000 values — harmless either way).

**Step 4 — Confirm pass:**

```bash
go test ./internal/api/ -run TestClaudeCodeEntryMaxOutput -v
```

**Step 5 — Commit:**

```bash
git add internal/api/server.go internal/api/claudecode_proxy_test.go
git commit -m "fix(api): fall back to default token limits for zero-value allowlist entries"
```

---

## Task 4: Populate limits in WebUI import rows (display parity)

**Files:**
- Modify: `internal/webui/public/js/components/models.js` (`importCCDefaults`)

**Consumes:** Task 3 (server fallback covers correctness).
**Produces:** Imported rows display the real limits in the WebUI instead of 0.

**Step 1 — No JS test harness; Go-side behavior already guarded by Task 3.** Manual check after edit.

**Step 2 — n/a.**

**Step 3 — Implementation:** add `contextWindow: 1000000, maxOutputTokens: 128000` to the four Claude 5 rows in `importCCDefaults`; verify the save path serializes these fields (check `saveCCConfig` payload keys against `claudecode.ModelConfig` JSON tags — if keys differ, map them).

**Step 4 — Verify:** import defaults in WebUI, confirm an imported `claude-opus-5` entry shows 128000 and a 200000 `max_tokens` request clamps (can be verified via the Task 3 test path plus a manual curl through the proxy if a live account is available).

**Step 5 — Commit:**

```bash
git add internal/webui/public/js/components/models.js
git commit -m "fix(webui): include 1M/128K limits in imported Claude Code defaults"
```

---

## Task 5: Nits

**Files:**
- Modify: `internal/claudecode/router_test.go` — collapse double blank line after `TestDefaultAllowlist_Claude5Limits`.
- Modify: `internal/modelcatalog/catalog_test.go` — remove overlap: keep Resolve-behavior assertions in `TestClaudeRoutingAliases` and map-contents assertions in `TestNonCloudCodeModelsHaveNoFixedRoutingAliases`; ensure no name is asserted in both.

**Step 1–2:** existing tests must stay green after the dedupe (`go test ./internal/modelcatalog/ -v`).

**Step 5 — Commit:**

```bash
git commit -am "style: tidy PR 75 review nits"
```

---

## Gate

```bash
go test ./internal/...
```

Must be 100% green (21 packages). Then `git push origin feat/claude-5-context-max-tokens` and reply on the PR review comment thread.

## Out of scope (previously accepted)

- DefaultAllowlist still advertises 3.x/haiku-4-5 models at 200k that 4.6-only upstreams cannot serve (honest SelectionError). Trimming those is a separate product decision.
