# Multi-Alias Import Rows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make comma-separated aliases stored by the WebUI `importCCDefaults` button resolve individually in the Claude Code router, and stop producing new rows in the broken shape.

**Architecture:** Add one shared helper `claudecode.ExpandAliases(ModelConfig) []string` that splits the comma-joined `Alias` string and merges it with the `Aliases` slice. Switch all three consumers (router indexing, `isClaudeModel`, `/models` listing) to the helper — this also repairs rows already persisted in user configs, no migration needed. Then change the WebUI importer to emit the same shape as `importSelectedClaudeCodeModels` (`aliases` array plus derived `alias` display string).

**Tech Stack:** Go 1.27rc2 (std library only), Alpine.js WebUI (no JS test runner — manual browser verification).

**Spec:** PR #75 review follow-up comment, quoted verbatim:

> Surfaced, out of scope: importCCDefaults rows store comma-joined aliases in the alias string field; router matches Alias literally, so multi-alias import rows resolve only the full string. Pre-existing; flagged in PR comment for follow-up.

## Global Constraints

- Go version floor: `go 1.27rc2` (go.mod). Standard library only — no new dependencies.
- Commit messages: conventional commits (`fix:`, `feat:`).
- PR target: the fork `gustavokch/antigravity-claude-proxy-go`, NOT upstream. Branch: `fix/cc-import-multi-alias`.
- Out of scope: OpenRouter (`OpenRouterModelConfig.Alias`) and Kimi (`KimiModelConfig.Alias`) alias handling — same latent issue exists there (management.go:409, management.go:425) but those UIs never produce comma-joined aliases. Do not touch.
- `ExpandAliases` must preserve the original case of entries (the `/models` endpoint surfaces aliases verbatim); consumers that need case-insensitive matching lowercase at the point of use.

## File Structure

- `internal/claudecode/types.go` — gains `ExpandAliases(m ModelConfig) []string` (ModelConfig lives here; `strings` already imported).
- `internal/claudecode/router.go:172-189` — `UpdateAllowlist` replaces the `m.Alias` block and `m.Aliases` loop with one loop over `ExpandAliases(m)`.
- `internal/claudecode/router_test.go` — new tests: `TestExpandAliases`, `TestRouter_UpdateAllowlist_CommaSeparatedAlias`.
- `internal/api/management.go:236-240` — `isClaudeModel` allowlist loop uses `ExpandAliases`.
- `internal/api/management.go:378-385` — `/models` listing loop uses `ExpandAliases`.
- `internal/api/management_test.go` — new test `TestIsClaudeModel_CommaSeparatedAlias`.
- `internal/webui/public/js/components/models.js:889-915` — `importCCDefaults` emits `aliases` array + derived `alias` string.

---

### Task 1: `ExpandAliases` helper

**Files:**
- Modify: `internal/claudecode/types.go` (after the `ModelConfig` struct, line 38)
- Test: `internal/claudecode/router_test.go`

**Interfaces:**
- Produces: `func ExpandAliases(m ModelConfig) []string` — returns the comma-separated `Alias` field split into entries, followed by `Aliases` entries. Each entry trimmed; empties dropped; duplicates removed case-insensitively (first occurrence wins, original case kept). Returns `nil` when nothing present. Tasks 2 and 3 consume this exact signature.

- [ ] **Step 1: Write the failing test**

Append to `internal/claudecode/router_test.go`:

```go
func TestExpandAliases(t *testing.T) {
	cases := []struct {
		name string
		in   ModelConfig
		want []string
	}{
		{"comma separated alias", ModelConfig{Alias: "a, b ,c"}, []string{"a", "b", "c"}},
		{"single alias", ModelConfig{Alias: "a"}, []string{"a"}},
		{"aliases list", ModelConfig{Aliases: []string{"x", "y"}}, []string{"x", "y"}},
		{"both merged and deduped", ModelConfig{Alias: "a, b", Aliases: []string{"B", "c"}}, []string{"a", "b", "c"}},
		{"empty", ModelConfig{}, nil},
		{"whitespace only", ModelConfig{Alias: "  ,"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandAliases(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExpandAliases(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
```

Add `"reflect"` to the test file's imports if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/claudecode/ -run TestExpandAliases -v`
Expected: FAIL — compile error `undefined: ExpandAliases`

- [ ] **Step 3: Write minimal implementation**

Append to `internal/claudecode/types.go` (after the `ModelConfig` struct closing brace):

```go
// ExpandAliases returns every alias configured on m: the comma-separated
// Alias field split into individual entries, followed by the explicit
// Aliases list. Entries are trimmed, empty entries dropped, and duplicates
// removed case-insensitively (first occurrence wins, original case kept).
func ExpandAliases(m ModelConfig) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" {
			return
		}
		key := strings.ToLower(a)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	for _, part := range strings.Split(m.Alias, ",") {
		add(part)
	}
	for _, a := range m.Aliases {
		add(a)
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/claudecode/ -run TestExpandAliases -v`
Expected: PASS (6 subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/claudecode/types.go internal/claudecode/router_test.go
git commit -m "feat(claudecode): add ExpandAliases helper for comma-separated alias fields"
```

---

### Task 2: Router consumes `ExpandAliases`

**Files:**
- Modify: `internal/claudecode/router.go:172-189`
- Test: `internal/claudecode/router_test.go`

**Interfaces:**
- Consumes: `ExpandAliases(m ModelConfig) []string` from Task 1.
- Produces: unchanged `Router` API. After this task, `ResolveModel("fable")` resolves a row whose `Alias` is `"claude-fable-5, fable-5, fable, claude-fable"`.

- [ ] **Step 1: Write the failing test**

Append to `internal/claudecode/router_test.go`:

```go
func TestRouter_UpdateAllowlist_CommaSeparatedAlias(t *testing.T) {
	// Shape produced by the WebUI importCCDefaults button before the fix:
	// all aliases comma-joined into the Alias string, Aliases empty.
	router := NewRouter([]ModelConfig{{
		ID:      "claude-fable-5",
		Alias:   "claude-fable-5, fable-5, fable, claude-fable",
		Enabled: true,
	}})
	for _, req := range []string{"fable-5", "fable", "claude-fable", "FABLE-5"} {
		got, ok := router.ResolveModel(req)
		if !ok || got != "claude-fable-5" {
			t.Errorf("ResolveModel(%q) = %q, %v; want claude-fable-5, true", req, got, ok)
		}
	}
	// Dated suffix resolves through the per-alias prefix mapping.
	if got, ok := router.ResolveModel("fable-5-20260101"); !ok || got != "claude-fable-5" {
		t.Errorf("prefix ResolveModel(fable-5-20260101) = %q, %v; want claude-fable-5, true", got, ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/claudecode/ -run TestRouter_UpdateAllowlist_CommaSeparatedAlias -v`
Expected: FAIL — `ResolveModel("fable-5")` returns `"", false` (only the full joined string resolves today). Note: `"FABLE-5"` failing proves the current code lowercases the joined string into one junk key; the prefix assertion may pass or fail pre-fix depending on the junk prefix — that is fine, the alias assertions are the gate.

- [ ] **Step 3: Replace the alias-indexing blocks**

In `internal/claudecode/router.go`, `UpdateAllowlist`, replace lines 172-189 — this exact block:

```go
		if m.Alias != "" {
			alias := strings.ToLower(strings.TrimSpace(m.Alias))
			r.aliases[alias] = id
			if !seenPrefix[alias] {
				prefixes = append(prefixes, prefixMapping{prefix: alias, canonicalID: id})
				seenPrefix[alias] = true
			}
		}
		for _, a := range m.Aliases {
			alias := strings.ToLower(strings.TrimSpace(a))
			if alias != "" {
				r.aliases[alias] = id
				if !seenPrefix[alias] {
					prefixes = append(prefixes, prefixMapping{prefix: alias, canonicalID: id})
					seenPrefix[alias] = true
				}
			}
		}
```

with:

```go
		for _, a := range ExpandAliases(m) {
			alias := strings.ToLower(a)
			r.aliases[alias] = id
			if !seenPrefix[alias] {
				prefixes = append(prefixes, prefixMapping{prefix: alias, canonicalID: id})
				seenPrefix[alias] = true
			}
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/claudecode/ -v -run 'TestRouter|TestExpandAliases|TestDefaultAllowlist'`
Expected: PASS — new test plus all existing router/default-allowlist tests (alias behavior for well-formed rows must not regress).

- [ ] **Step 5: Commit**

```bash
git add internal/claudecode/router.go internal/claudecode/router_test.go
git commit -m "fix(claudecode): split comma-separated Alias when indexing router aliases"
```

---

### Task 3: Management endpoints consume `ExpandAliases`

**Files:**
- Modify: `internal/api/management.go:236-240` (`isClaudeModel`)
- Modify: `internal/api/management.go:378-385` (`/models` listing)
- Test: `internal/api/management_test.go`

**Interfaces:**
- Consumes: `claudecode.ExpandAliases(m ModelConfig) []string` from Task 1 (package `api` already imports `internal/claudecode`).
- Produces: unchanged handler APIs. `isClaudeModel` now matches individual entries of comma-joined `Alias` fields; `/models` now surfaces them individually.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/management_test.go`:

```go
func TestIsClaudeModel_CommaSeparatedAlias(t *testing.T) {
	// Shape produced by the WebUI importCCDefaults button before the fix.
	allowlist := []claudecode.ModelConfig{{
		ID:    "custom-proxy-model",
		Alias: "my-alias, my-other-alias",
	}}
	if !isClaudeModel("my-alias", allowlist) {
		t.Error("expected first comma-separated alias entry to match")
	}
	if !isClaudeModel("my-other-alias", allowlist) {
		t.Error("expected second comma-separated alias entry to match")
	}
	if isClaudeModel("unrelated-model", allowlist) {
		t.Error("unexpected match for unrelated model")
	}
}
```

Add `"github.com/.../internal/claudecode"` to the test file imports if missing (match the exact module path used by `management.go`'s own claudecode import).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestIsClaudeModel_CommaSeparatedAlias -v`
Expected: FAIL — both alias assertions fail (loop only checks `m.Aliases`, which is empty).

- [ ] **Step 3: Switch `isClaudeModel` to the helper**

In `internal/api/management.go`, `isClaudeModel`, replace lines 236-240:

```go
		for _, a := range m.Aliases {
			if strings.EqualFold(a, modelId) {
				return true
			}
		}
```

with:

```go
		for _, a := range claudecode.ExpandAliases(m) {
			if strings.EqualFold(a, modelId) {
				return true
			}
		}
```

Do NOT change the second loop (over `claudecode.DefaultClaudeCatalogue()`, lines 246-250) — catalogue rows never carry comma-joined aliases; keep the diff minimal.

- [ ] **Step 4: Switch the `/models` listing to the helper**

In `internal/api/management.go`, around line 378, replace:

```go
				for _, alias := range m.Aliases {
					if alias != "" {
						modelSet[alias] = true
						if m.ContextLen > 0 {
							modelContext[alias] = m.ContextLen
						}
					}
				}
```

with:

```go
				for _, alias := range claudecode.ExpandAliases(m) {
					modelSet[alias] = true
					if m.ContextLen > 0 {
						modelContext[alias] = m.ContextLen
					}
				}
```

(The `alias != ""` guard is now redundant — `ExpandAliases` never returns empty strings.) Leave the `else` branch over `DefaultClaudeCatalogue()` (lines 388-397) untouched.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/api/ -v -run 'TestIsClaudeModel|TestManagement'`
Expected: PASS — new test plus existing management suite.

- [ ] **Step 6: Commit**

```bash
git add internal/api/management.go internal/api/management_test.go
git commit -m "fix(api): resolve comma-separated aliases in isClaudeModel and /models listing"
```

---

### Task 4: WebUI importer emits `aliases` array

**Files:**
- Modify: `internal/webui/public/js/components/models.js:889-915` (`importCCDefaults`)

**Interfaces:**
- Consumes: nothing from earlier tasks (independent frontend change).
- Produces: allowlist rows in the same shape as `importSelectedClaudeCodeModels` (models.js:853-859): `alias` holds the comma-joined display string, `aliases` holds the array. Backend Tasks 1-3 make both fields resolve; this task stops writing rows that depend on the split.

- [ ] **Step 1: Rewrite `importCCDefaults`**

Replace the entire `importCCDefaults()` function (models.js:889-915) with:

```js
    importCCDefaults() {
        const defaults = [
            { id: 'claude-fable-5', displayName: 'Claude Fable 5', aliases: ['claude-fable-5', 'fable-5', 'fable', 'claude-fable'], contextLength: 1000000, maxOutputTokens: 128000, enabled: true },
            { id: 'claude-fable-5-1', displayName: 'Claude Fable 5.1', aliases: ['claude-fable-5-1', 'fable-5-1', 'claude-fable-5.1', 'fable-5.1'], contextLength: 1000000, maxOutputTokens: 128000, enabled: true },
            { id: 'claude-opus-5', displayName: 'Claude Opus 5', aliases: ['claude-opus-5', 'opus-5', 'opus', 'claude-5-opus'], contextLength: 1000000, maxOutputTokens: 128000, enabled: true },
            { id: 'claude-sonnet-5', displayName: 'Claude Sonnet 5', aliases: ['claude-sonnet-5', 'sonnet-5', 'sonnet', 'claude-5-sonnet'], contextLength: 1000000, maxOutputTokens: 128000, enabled: true },
            { id: 'claude-haiku-4-5-20251001', displayName: 'Claude Haiku 4.5', aliases: ['claude-haiku-4-5', 'claude-haiku-4.5', 'haiku'], enabled: true },
            { id: 'claude-opus-4-8', displayName: 'Claude Opus 4.8', aliases: ['claude-opus-4.8'], enabled: true },
            { id: 'claude-opus-4-7', displayName: 'Claude Opus 4.7', aliases: ['claude-opus-4.7'], enabled: true },
            { id: 'claude-sonnet-4-6', displayName: 'Claude Sonnet 4.6', aliases: ['claude-sonnet-4.6'], enabled: true },
            { id: 'claude-opus-4-6', displayName: 'Claude Opus 4.6', aliases: ['claude-opus-4.6'], enabled: true },
            { id: 'claude-3-7-sonnet-20250219', displayName: 'Claude 3.7 Sonnet', aliases: ['claude-3.7-sonnet', 'claude-3-7-sonnet', 'claude-3-7-sonnet-thinking'], enabled: true },
            { id: 'claude-3-5-sonnet-20241022', displayName: 'Claude 3.5 Sonnet v2', aliases: ['claude-3.5-sonnet', 'claude-3-5-sonnet'], enabled: true },
            { id: 'claude-3-5-haiku-20241022', displayName: 'Claude 3.5 Haiku', aliases: ['claude-3.5-haiku', 'claude-3-5-haiku'], enabled: true },
            { id: 'claude-3-opus-20240229', displayName: 'Claude 3 Opus', aliases: ['claude-3-opus'], enabled: true }
        ];
        if (!this.ccConfig.allowlist) this.ccConfig.allowlist = [];
        const existing = new Set(this.ccConfig.allowlist.map(m => m.id));
        defaults.forEach(d => {
            if (!existing.has(d.id)) {
                this.ccConfig.allowlist.push({
                    ...d,
                    alias: d.aliases.join(', '),
                    aliases: d.aliases
                });
                existing.add(d.id);
            }
        });
        this.saveCCConfig();
        Alpine.store('global').showToast('Claude Code default models imported', 'success');
    },
```

Rationale: `alias` keeps the joined display string because the allowlist edit dialog reads it (models.js:855 uses the same convention); `aliases` carries the machine-readable array.

- [ ] **Step 2: Verify the embedded asset still builds**

The webui assets are embedded via `go:embed`; no bundler step exists.

Run: `go build ./...`
Expected: build succeeds.

- [ ] **Step 3: Manual browser verification**

1. Run the proxy locally, open the WebUI models view.
2. Click the import-defaults button that calls `importCCDefaults`.
3. Open browser devtools, inspect the saved config via `GET /api/claudecode/config` — confirm each imported row has `aliases` as an array and `alias` as the joined string.
4. In the JS console or via curl, request a model by a single alias (e.g. send a chat request with `"model": "fable-5"`) and confirm it routes instead of 404/403.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/public/js/components/models.js
git commit -m "fix(webui): import CC defaults with aliases array instead of comma-joined alias only"
```

---

### Task 5: Full gate and PR

**Files:**
- None (verification + PR only)

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: PASS, no regressions.

- [ ] **Step 2: Vet**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 3: Push and open the PR against the fork**

```bash
git push -u origin fix/cc-import-multi-alias
gh pr create --repo gustavokch/antigravity-claude-proxy-go \
  --title "fix: resolve comma-separated aliases from WebUI CC defaults import" \
  --body "Follow-up to #75.

## Problem
\`importCCDefaults\` rows stored all aliases comma-joined in the \`alias\` string field with an empty \`aliases\` array. The router indexed \`Alias\` literally, so only the full joined string resolved — individual aliases (e.g. \`fable-5\`) 404'd. Same blind spot in \`isClaudeModel\` and the \`/models\` listing, which only iterate \`Aliases\`.

## Fix
- New \`claudecode.ExpandAliases\` helper splits the comma-joined \`Alias\` field and merges it with \`Aliases\` (trimmed, deduped case-insensitively, original case preserved).
- Router indexing, \`isClaudeModel\`, and the \`/models\` listing all consume the helper. Because the split happens at read time, rows already persisted in user configs are repaired without a migration.
- WebUI importer now emits the \`aliases\` array (matching \`importSelectedClaudeCodeModels\`), keeping \`alias\` as the joined display string the edit dialog reads.

## Out of scope
OpenRouter/Kimi single-\`Alias\` handling has the same latent shape but no UI produces comma-joined values there today."
```

- [ ] **Step 4: Comment on PR #75 linking the follow-up**

```bash
gh pr comment 75 --repo gustavokch/antigravity-claude-proxy-go \
  --body "Follow-up for the multi-alias import issue: <link to new PR>"
```
