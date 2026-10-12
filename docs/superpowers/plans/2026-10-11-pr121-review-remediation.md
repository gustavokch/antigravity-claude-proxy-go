# PR #121 Review Remediation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve every issue and reviewer note flagged on PR #121 (`feat/agy-1-3-3-catalog`), restoring unmigrated account Claude 4.6 model resolution, tying effort-tier retargeting to the resolved model family, documenting binary ground truth for `thinkingLevel`, populating WebUI Claude 5.5 default limits, and restoring weakened test coverage.

**Architecture:** Port agy 1.3.3 catalog changes faithfully to the Go proxy without regressions. Introduce `supersededRoutes` consulted only after direct ID, display name, and `routingAliases` miss, ensuring accounts that still publish real Claude 4.6 continue serving it while migrated accounts fall forward to Claude 5.5 High. Key Claude effort retargeting on the resolved model family (`claude55Family(model)`), preventing unmigrated Claude 4.6 models from being retiered to 5.5 on effort requests and eliminating unreachable switch arms. Document the agy binary's `ThinkingLevel` enum provenance and pin `thinkingLevelString` behavior with unit tests. Add context and max output limits to Claude 5.5 rows in WebUI `importCCDefaults()` with an accompanying Node harness test.

**Tech Stack:** Go 1.27rc2, standard library `testing`, Node.js (v22+) via existing `internal/webui/tests` harness, `internal/modelcatalog`, `internal/webui`.

**Spec:**
- PR #121 review comment: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/121#issuecomment-6115247275
- Upstream catalog ground truth: `.reference/agy-models-20261011.txt`
- Baseline model baseline: `.reference/agy-current-models.txt`
- Project core invariants: `AGENTS.md`
- Previous design doc: `docs/superpowers/plans/2026-10-11-agy-1-3-3-model-catalog-upgrade.md`

## Global Constraints

- **Disguise invariant:** Do not touch TLS internals. Zero changes to `tls.Config{}`. Standard Go HTTP client remains the disguise. (AGENTS.md)
- **Upstream parity:** Never fabricate proto schemas or invent model IDs that upstream does not accept. (AGENTS.md)
- **Account compatibility:** The upstream catalog is account-dependent. Accounts that still publish legacy models (`gemini-3.5`, `claude-sonnet-4-6`) must continue resolving them via `byID`/`byDisplay` and the account's own routing aliases before superseded routes are consulted. (`.reference/agy-current-models.txt:48-50`)
- **TDD:** Write failing unit tests before updating implementations; run package test suite after every change.
- **Commit gate:** `gofmt` clean (`make fmt-check`), `go vet ./...`, and `go test` must pass on each task.
- **Working directory:** Execute all commands in the dedicated PR worktree:
  `/Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog`
  on branch `feat/agy-1-3-3-catalog`.
- **Remote:** Push commits to remote `fork` on branch `feat/agy-1-3-3-catalog`.

## File Structure

- Create: `internal/modelcatalog/claude_legacy_test.go`
  - Unit tests for legacy Claude 4.6 name resolution across unmigrated, migrated, and dual-publishing accounts.
  - Unit tests for effort-tier routing following the resolved Claude model family.
- Create: `internal/webui/tests/import-cc-defaults.test.mjs`
  - Node harness test for `importCCDefaults()` limits and wire serialization.
- Create: `internal/webui/import_cc_defaults_test.go`
  - Go wrapper test executing the Node harness via `os/exec`.
- Modify: `internal/modelcatalog/catalog.go`
  - Restore Claude 4.6 entries in `routingAliases`.
  - Add `supersededRoutes` mapping legacy 4.6 IDs to 5.5 High for migrated accounts.
  - Consult `supersededRoutes` in `Resolve()` after `routingAliases` miss.
  - Update `effortTier` doc comment to mention Claude 5.5.
  - Route Claude effort tiers via `claude55Family(model)`, dropping dead switch arms.
  - Restore comment on `gemini-3.5-flash` case.
  - Add `claude55Family(Model) string` helper.
  - Document binary enum provenance on `thinkingLevelString`.
- Modify: `internal/modelcatalog/catalog_test.go`
  - Restore `claude-opus-4-6[1m]` row in `TestCatalogResolve_Strips1mSuffix`.
  - Add `TestThinkingLevelString` table-driven boundary test.
- Modify: `internal/webui/public/js/components/models.js`
  - Add `contextLength: 1000000, maxOutputTokens: 128000` to `claude-opus-5-5` and `claude-sonnet-5-5` rows in `importCCDefaults()`.
- Modify: `.reference/agy-current-models.txt`
  - Clarify account-dependent catalog resolution order (ID/display → routing aliases → superseded routes).
- Modify: `.reference/agy-models-20261011.txt`
  - Document binary enum provenance for `thinkingLevel` values 1–4.

---

### Task 1: Restore Legacy Claude 4.6 Name Resolution on Unmigrated Accounts & Recover Weakened Tests

**Files:**
- Create: `internal/modelcatalog/claude_legacy_test.go`
- Modify: `internal/modelcatalog/catalog.go:132-156,268-305`
- Modify: `internal/modelcatalog/catalog_test.go:604-607`
- Modify: `.reference/agy-current-models.txt:48-50`

**Interfaces:**
- Consumes: `Catalog.Resolve(requested string) (Model, error)`, `Strip1mSuffix(s string) string`.
- Produces: `supersededRoutes map[string]string` in `internal/modelcatalog`.
- Contract: For an unmigrated account publishing `claude-sonnet-4-6` and `claude-opus-4-6-thinking`, requests for `claude-sonnet-4-6-thinking`, `claude-sonnet-4.6`, `claude-opus-4-6`, `claude-opus-4-6-thinking`, and `claude-opus-4-6[1m]` must resolve to the account's real 4.6 models. For a migrated account publishing only Claude 5.5, the same requests must resolve to the corresponding Claude 5.5 High tier.

- [ ] **Step 1: Write the failing tests in `internal/modelcatalog/claude_legacy_test.go`**

Create `internal/modelcatalog/claude_legacy_test.go`:
```go
package modelcatalog

import (
	"fmt"
	"strings"
	"testing"
)

const (
	claude46SonnetID      = "claude-sonnet-4-6"
	claude46SonnetDisplay = "Claude Sonnet 4.6 (Thinking)"
	claude46OpusID        = "claude-opus-4-6-thinking"
	claude46OpusDisplay   = "Claude Opus 4.6 (Thinking)"

	claude55SonnetHighID      = "claude-sonnet-5-5-high"
	claude55SonnetHighDisplay = "Claude Sonnet 5.5 (High)"
	claude55SonnetMedID       = "claude-sonnet-5-5-medium"
	claude55SonnetMedDisplay  = "Claude Sonnet 5.5 (Medium)"
	claude55SonnetLowID       = "claude-sonnet-5-5-low"
	claude55SonnetLowDisplay  = "Claude Sonnet 5.5 (Low)"

	claude55OpusHighID      = "claude-opus-5-5-high"
	claude55OpusHighDisplay = "Claude Opus 5.5 (High)"
	claude55OpusMedID       = "claude-opus-5-5-medium"
	claude55OpusMedDisplay  = "Claude Opus 5.5 (Medium)"
	claude55OpusLowID       = "claude-opus-5-5-low"
	claude55OpusLowDisplay  = "Claude Opus 5.5 (Low)"
)

var (
	claude46Models = map[string]string{
		claude46SonnetID: claude46SonnetDisplay,
		claude46OpusID:   claude46OpusDisplay,
	}
	claude55Models = map[string]string{
		claude55SonnetHighID: claude55SonnetHighDisplay,
		claude55SonnetMedID:  claude55SonnetMedDisplay,
		claude55SonnetLowID:  claude55SonnetLowDisplay,
		claude55OpusHighID:   claude55OpusHighDisplay,
		claude55OpusMedID:    claude55OpusMedDisplay,
		claude55OpusLowID:    claude55OpusLowDisplay,
	}
)

func parseClaudeCatalog(t *testing.T, defs ...map[string]string) *Catalog {
	t.Helper()
	var ids []string
	var modelEntries []string
	defaultID := ""
	for _, m := range defs {
		for id, display := range m {
			if defaultID == "" {
				defaultID = id
			}
			ids = append(ids, fmt.Sprintf("%q", id))
			modelEntries = append(modelEntries, fmt.Sprintf(
				"%q:{\"displayName\":%q,\"supportsThinking\":true,\"maxTokens\":1000000,\"maxOutputTokens\":128000}",
				id, display,
			))
		}
	}
	raw := fmt.Sprintf(`{
		"defaultAgentModelId": %q,
		"agentModelSorts": [{"groups": [{"modelIds": [%s]}]}],
		"models": {%s}
	}`, defaultID, strings.Join(ids, ","), strings.Join(modelEntries, ","))
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parseClaudeCatalog: %v", err)
	}
	return c
}

// TestLegacyClaude46Resolution tests that legacy Claude 4.6 names resolve to
// real 4.6 models when an account still publishes them, and fall back to the
// corresponding 5.5 High model only when 4.6 is absent from the account.
func TestLegacyClaude46Resolution(t *testing.T) {
	t.Parallel()

	unmigrated := parseClaudeCatalog(t, claude46Models)
	migrated := parseClaudeCatalog(t, claude55Models)
	both := parseClaudeCatalog(t, claude46Models, claude55Models)

	cases := []struct {
		name    string
		catalog *Catalog
		request string
		wantID  string
	}{
		// Unmigrated account (real 4.6 only): every legacy spelling resolves to 4.6.
		{"unmigrated exact sonnet", unmigrated, "claude-sonnet-4-6", claude46SonnetID},
		{"unmigrated thinking-suffixed sonnet", unmigrated, "claude-sonnet-4-6-thinking", claude46SonnetID},
		{"unmigrated dotted sonnet", unmigrated, "claude-sonnet-4.6", claude46SonnetID},
		{"unmigrated base opus", unmigrated, "claude-opus-4-6", claude46OpusID},
		{"unmigrated exact opus", unmigrated, "claude-opus-4-6-thinking", claude46OpusID},
		{"unmigrated dotted opus", unmigrated, "claude-opus-4.6", claude46OpusID},

		// Migrated account (5.5 only): legacy 4.6 names fall back to 5.5 High.
		{"migrated base sonnet", migrated, "claude-sonnet-4-6", claude55SonnetHighID},
		{"migrated thinking-suffixed sonnet", migrated, "claude-sonnet-4-6-thinking", claude55SonnetHighID},
		{"migrated base opus", migrated, "claude-opus-4-6", claude55OpusHighID},
		{"migrated thinking-suffixed opus", migrated, "claude-opus-4-6-thinking", claude55OpusHighID},

		// Account publishing both: real 4.6 takes precedence.
		{"both-account sonnet", both, "claude-sonnet-4-6", claude46SonnetID},
		{"both-account thinking sonnet", both, "claude-sonnet-4-6-thinking", claude46SonnetID},
		{"both-account base opus", both, "claude-opus-4-6", claude46OpusID},
		{"both-account thinking opus", both, "claude-opus-4-6-thinking", claude46OpusID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.catalog.Resolve(tc.request)
			if err != nil {
				t.Fatalf("Resolve(%q) unexpected error: %v", tc.request, err)
			}
			if got.ID != tc.wantID {
				t.Errorf("Resolve(%q) = %q, want %q", tc.request, got.ID, tc.wantID)
			}
		})
	}
}
```

Also restore the weakened test row in `internal/modelcatalog/catalog_test.go:605` (`TestCatalogResolve_Strips1mSuffix`):
Replace:
```go
		{"claude-opus-4-6-thinking[1m]", "claude-opus-4-6-thinking", 250000},
```
With both rows (the restored alias-path test and the PR's exact-ID test):
```go
		{"claude-opus-4-6[1m]", "claude-opus-4-6-thinking", 250000},
		{"claude-opus-4-6-thinking[1m]", "claude-opus-4-6-thinking", 250000},
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
go test -v ./internal/modelcatalog -run 'TestLegacyClaude46Resolution|TestCatalogResolve_Strips1mSuffix'
```
Expected: FAIL on `TestLegacyClaude46Resolution/unmigrated_thinking-suffixed_sonnet`, `unmigrated_base_opus`, `both-account_base_opus`, and `TestCatalogResolve_Strips1mSuffix` (`claude-opus-4-6[1m]`: SelectionError because `routingAliases` repointed to 5.5 High which is absent from the 4.6 fixture).

- [ ] **Step 3: Implement minimal code in `internal/modelcatalog/catalog.go`**

In `internal/modelcatalog/catalog.go`:
1. Restore 4.6 targets in `routingAliases` (lines 132-138) and define `supersededRoutes`:
```go
	// Claude 4.6 routing aliases map published IDs to their Cloud Code
	// display names. Accounts that still publish real 4.6 resolve them here.
	// Names that agy/Cloud Code never publishes (Anthropic API spellings like
	// claude-3-5-sonnet, generic family names) remain deliberately absent.
	"claude-sonnet-4-6-thinking": "Claude Sonnet 4.6 (Thinking)",
	"claude-sonnet-4-6":          "Claude Sonnet 4.6 (Thinking)",
	"claude-opus-4-6":            "Claude Opus 4.6 (Thinking)",
	"claude-opus-4-6-thinking":   "Claude Opus 4.6 (Thinking)",
}

// supersededRoutes maps legacy model IDs that Cloud Code once published to the
// display name of their newer replacement. Consulted ONLY after direct ID,
// display name, and routingAliases lookups all miss, ensuring accounts that
// still serve the legacy model keep serving it while migrated accounts fall
// back to the current replacement without a hard failure.
var supersededRoutes = map[string]string{
	// Claude 4.6 was removed from agentModelSorts in agy 1.3.3 and replaced
	// by the Claude 5.5 family (.reference/agy-models-20261011.txt). Legacy
	// requests that miss the account catalog fall back to 5.5 High tier:
	"claude-sonnet-4-6-thinking": "Claude Sonnet 5.5 (High)",
	"claude-sonnet-4-6":          "Claude Sonnet 5.5 (High)",
	"claude-opus-4-6":            "Claude Opus 5.5 (High)",
	"claude-opus-4-6-thinking":   "Claude Opus 5.5 (High)",
}
```

2. In `Resolve()` (after checking `routingAliases` with `normalized`):
```go
	normalized := strings.ReplaceAll(key, ".", "-")
	if displayName := routingAliases[normalized]; displayName != "" {
		if model, exists := catalog.byDisplay[strings.ToLower(displayName)]; exists {
			return model, nil
		}
	}
	if displayName := supersededRoutes[key]; displayName != "" {
		if model, exists := catalog.byDisplay[strings.ToLower(displayName)]; exists {
			return model, nil
		}
	}
	if displayName := supersededRoutes[normalized]; displayName != "" {
		if model, exists := catalog.byDisplay[strings.ToLower(displayName)]; exists {
			return model, nil
		}
	}
```

3. Update `.reference/agy-current-models.txt:48-50` to document the resolution order:
```
- The upstream catalog is ACCOUNT-DEPENDENT. Accounts that still publish legacy
  models must continue resolving them via direct ID/display lookup and the
  account's own routing aliases before superseded routes are consulted.
```

- [ ] **Step 4: Run tests and verify they pass**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
gofmt -w internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go internal/modelcatalog/claude_legacy_test.go
make fmt-check
go vet ./internal/modelcatalog
go test -v ./internal/modelcatalog -run 'TestLegacyClaude46Resolution|TestCatalogResolve_Strips1mSuffix|TestClaude55RoutingAliasesAndLegacy46Fallback'
```
Expected: PASS across all suites with 0 failures.

- [ ] **Step 5: Commit**

```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go internal/modelcatalog/claude_legacy_test.go .reference/agy-current-models.txt
git commit -m "fix(modelcatalog): preserve Claude 4.6 alias resolution on unmigrated accounts via superseded routes"
```

---

### Task 2: Effort-Tier Routing Follows Resolved Model Family

**Files:**
- Modify: `internal/modelcatalog/claude_legacy_test.go`
- Modify: `internal/modelcatalog/catalog.go:284-330,370-400,454-466`

**Interfaces:**
- Consumes: `Model.DisplayName`, `reasoning.Params`.
- Produces: `claude55Family(m Model) string` helper.
- Contract: `ResolveWithRequest` for a model that resolved to a Claude 5.5 tier retargets to `claude55Family(model) + "-" + tier`. Requests that resolved to real Claude 4.6 (or other non-tiered models) remain on their resolved model without retiering. Unreachable arms `lowerReq == "claude-sonnet"` and `lowerReq == "claude-opus"` are removed.

- [ ] **Step 1: Write the failing tests in `internal/modelcatalog/claude_legacy_test.go`**

Append to `internal/modelcatalog/claude_legacy_test.go`:
```go
// TestEffortTierRoutingFollowsResolvedClaudeModel tests that effort-based tier
// routing applies to Claude requests when the resolved model belongs to the
// Claude 5.5 family, and does not repoint real Claude 4.6 models on accounts
// that still publish them.
func TestEffortTierRoutingFollowsResolvedClaudeModel(t *testing.T) {
	t.Parallel()

	unmigrated := parseClaudeCatalog(t, claude46Models)
	migrated := parseClaudeCatalog(t, claude55Models)
	both := parseClaudeCatalog(t, claude46Models, claude55Models)

	effort := func(level string) map[string]any {
		return map[string]any{"reasoning_effort": level}
	}
	budget := func(tokens int) map[string]any {
		return map[string]any{"thinking_budget": tokens}
	}

	cases := []struct {
		name    string
		catalog *Catalog
		request string
		params  map[string]any
		wantID  string
	}{
		// On 5.5-only accounts, 5.5 family models route to the requested tier:
		{"5.5 sonnet effort low", migrated, "claude-sonnet-5-5", effort("low"), claude55SonnetLowID},
		{"5.5 sonnet effort med", migrated, "claude-sonnet-5-5", effort("medium"), claude55SonnetMedID},
		{"5.5 opus effort low", migrated, "claude-opus-5-5", effort("low"), claude55OpusLowID},

		// On 5.5-only accounts, legacy 4.6 names resolve to 5.5 and then retier:
		{"migrated legacy sonnet effort med", migrated, "claude-sonnet-4-6", effort("medium"), claude55SonnetMedID},
		{"migrated legacy opus effort low", migrated, "claude-opus-4-6", effort("low"), claude55OpusLowID},
		{"migrated legacy sonnet budget maps to tier", migrated, "claude-sonnet-4-6", budget(1500), claude55SonnetLowID},

		// On accounts publishing both, real 4.6 is served and retains its route:
		{"both-account 4.6 sonnet with effort stays 4.6", both, "claude-sonnet-4-6", effort("low"), claude46SonnetID},
		{"both-account 4.6 opus with effort stays 4.6", both, "claude-opus-4-6", effort("medium"), claude46OpusID},
		{"both-account 4.6 sonnet with budget stays 4.6", both, "claude-sonnet-4-6", budget(8000), claude46SonnetID},

		// On accounts publishing both, explicit 5.5 requests retier normally:
		{"both-account 5.5 sonnet with effort", both, "claude-sonnet-5-5", effort("low"), claude55SonnetLowID},

		// On unmigrated accounts, real 4.6 models do not retier:
		{"unmigrated 4.6 opus with effort stays 4.6", unmigrated, "claude-opus-4-6", effort("low"), claude46OpusID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.catalog.ResolveWithRequest(tc.request, tc.params)
			if err != nil {
				t.Fatalf("ResolveWithRequest(%q) unexpected error: %v", tc.request, err)
			}
			if got.ID != tc.wantID {
				t.Errorf("ResolveWithRequest(%q) = %q, want %q", tc.request, got.ID, tc.wantID)
			}
		})
	}

	// Bare family names "claude-sonnet" and "claude-opus" fail honestly
	// as SelectionError even when an effort parameter is supplied.
	for _, bare := range []string{"claude-sonnet", "claude-opus"} {
		if _, err := migrated.ResolveWithRequest(bare, effort("low")); err == nil {
			t.Errorf("ResolveWithRequest(%q, effort=low) unexpectedly succeeded; want SelectionError", bare)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
go test -v ./internal/modelcatalog -run TestEffortTierRoutingFollowsResolvedClaudeModel
```
Expected: FAIL on `both-account_4.6_sonnet_with_effort_stays_4.6` and `both-account_4.6_opus_with_effort_stays_4.6` (returns 5.5 tiers instead of preserving real 4.6).

- [ ] **Step 3: Implement minimal code in `internal/modelcatalog/catalog.go`**

1. Update the `flashTiers` and `effortTier` doc comments:
```go
// flashTiers are the tiers published by the Gemini flash and Claude 5.5
// families. A stronger request (xhigh, max) falls back to high; minimal rides
// the low tier.
var flashTiers = [...]reasoning.Level{reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh}

// effortTier returns the tier suffix ("low", "medium" or "high") the request
// asks tiered families (Gemini flash, Claude 5.5) to route to, or "" when the
// model the client named stands. output_config.effort is ambient (Claude Code
// sends it on every request), so it only picks a tier for a family ID that does
// not already name one. reasoning_effort and a bare thinking budget are
// deliberate, so they override the name.
func effortTier(requested string, params reasoning.Params) string {
```

2. In `ResolveWithRequest`, replace the Claude branches of the effort-tier switch and restore the comment on `gemini-3.5-flash`:
```go
		case strings.HasPrefix(lowerReq, "gemini-3.5-flash"):
			// Legacy 3.5 IDs repoint to the 3.8 family (user decision
			// 2026-09-03); fall back to a real 3.5 tier only when this
			// account has no 3.8 at all.
			targetID = "gemini-3.8-flash-" + tier
		case claude55Family(model) != "":
			targetID = claude55Family(model) + "-" + tier
		}
```
(Removes the old cases for `claude-sonnet-5-5`, `claude-opus-5-5`, and the `|| lowerReq == "claude-sonnet"` / `"claude-opus"` arms).

3. Add `claude55Family` helper right after `CleanModelIDAndName`:
```go
// claude55Family returns the Claude 5.5 family stem ("claude-sonnet-5-5" or
// "claude-opus-5-5") when the model resolved to a Claude 5.5 tier, or "" for
// any other model (including real Claude 4.6 models on unmigrated accounts).
func claude55Family(m Model) string {
	lowerDisplay := strings.ToLower(m.DisplayName)
	switch {
	case strings.HasPrefix(lowerDisplay, "claude sonnet 5.5"):
		return "claude-sonnet-5-5"
	case strings.HasPrefix(lowerDisplay, "claude opus 5.5"):
		return "claude-opus-5-5"
	default:
		return ""
	}
}
```

- [ ] **Step 4: Run tests and verify they pass**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
gofmt -w internal/modelcatalog/catalog.go internal/modelcatalog/claude_legacy_test.go
make fmt-check
go vet ./internal/modelcatalog
go test -v ./internal/modelcatalog -run 'TestEffortTierRoutingFollowsResolvedClaudeModel|TestClaude55EffortTierResolution'
```
Expected: PASS on both tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
git add internal/modelcatalog/catalog.go internal/modelcatalog/claude_legacy_test.go
git commit -m "fix(modelcatalog): key Claude effort tier routing on resolved family and drop dead bare arms"
```

---

### Task 3: Document agy Binary ThinkingLevel Ground Truth & Pin Parser Mapping

**Files:**
- Modify: `internal/modelcatalog/catalog.go:462-476`
- Modify: `internal/modelcatalog/catalog_test.go:1095-1116`
- Modify: `.reference/agy-models-20261011.txt:79-84`

**Interfaces:**
- Consumes: `thinkingLevelString(level int) string`.
- Produces: Table-driven boundary tests for `thinkingLevelString`, documented binary enum provenance in source and reference doc.
- Ground truth recovered from `/Users/gus/.local/bin/agy 1.3.3`:
  - Three distinct `ThinkingLevel` enum descriptors in the binary (`PredictionService`, `content.proto`, `monitor_config.proto`) all assign:
    - `0 = THINKING_LEVEL_UNSPECIFIED`
    - `1 = LOW`
    - `2 = MEDIUM`
    - `3 = HIGH`
    - `4 = MINIMAL`
  - Values 5 (`EXTRA_HIGH`) and 6 (`MAX`) exist in the proto definition but are unmapped in the proxy because upstream Cloud Code has published no model using them. Unmapped levels safely return `""`.

- [ ] **Step 1: Write boundary test in `internal/modelcatalog/catalog_test.go`**

Append to `internal/modelcatalog/catalog_test.go`:
```go
func TestThinkingLevelString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		level int
		want  string
	}{
		{0, ""},
		{1, "LOW"},
		{2, "MEDIUM"},
		{3, "HIGH"},
		{4, "MINIMAL"},
		{5, ""},
		{6, ""},
		{-1, ""},
	}
	for _, tc := range cases {
		if got := thinkingLevelString(tc.level); got != tc.want {
			t.Errorf("thinkingLevelString(%d) = %q, want %q", tc.level, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Document provenance in `internal/modelcatalog/catalog.go` and `.reference/agy-models-20261011.txt`**

1. In `internal/modelcatalog/catalog.go`, add comment above `thinkingLevelString`:
```go
// thinkingLevelString maps the upstream integer thinkingLevel (found in
// Cloud Code PA ModelDetails.thinking_level) to the string required by
// thinkingConfig.thinkingLevel. Ground truth recovered from agy 1.3.3 binary
// descriptors (google/cloud/aiplatform/v1beta1/content.proto ThinkingLevel enum):
// 1=LOW, 2=MEDIUM, 3=HIGH, 4=MINIMAL. Unmapped levels return "".
func thinkingLevelString(level int) string {
```

2. In `.reference/agy-models-20261011.txt:79-84`, update the thinking levels bullet:
```
- Thinking levels: Upstream Cloud Code PA uses integer `thinkingLevel`
  (matches Vertex content.proto / PredictionService ThinkingLevel enum):
    * 1 = Low (`THINKING_LEVEL_LOW`)
    * 2 = Medium (`THINKING_LEVEL_MEDIUM`)
    * 3 = High (`THINKING_LEVEL_HIGH`)
    * 4 = Minimal (`THINKING_LEVEL_MINIMAL`, preserved in parser)
```

- [ ] **Step 3: Run tests and verify they pass**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
gofmt -w internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
make fmt-check
go vet ./internal/modelcatalog
go test -v ./internal/modelcatalog -run TestThinkingLevelString
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go .reference/agy-models-20261011.txt
git commit -m "docs(modelcatalog): document ThinkingLevel enum provenance and pin thinkingLevelString mapping"
```

---

### Task 4: WebUI `importCCDefaults` Claude 5.5 Limits & Node Harness Test

**Files:**
- Create: `internal/webui/tests/import-cc-defaults.test.mjs`
- Create: `internal/webui/import_cc_defaults_test.go`
- Modify: `internal/webui/public/js/components/models.js:1310-1311`

**Interfaces:**
- Consumes: `loadComponent` from `internal/webui/tests/kimi-poll-race.harness.mjs`.
- Produces: `TestImportCCDefaultsHarness` in package `webui`.
- Contract: Calling `c.importCCDefaults()` adds `claude-sonnet-5-5` and `claude-opus-5-5` with `contextLength: 1000000` and `maxOutputTokens: 128000`, matching `.reference/agy-models-20261011.txt` section 3 and the neighbouring Claude 5 rows. These limits must serialize to the `saveCCConfig` wire payload.

- [ ] **Step 1: Write the failing Node harness test and Go wrapper**

Create `internal/webui/tests/import-cc-defaults.test.mjs`:
```javascript
// Tests for importCCDefaults limits in models.js.
// Run: node internal/webui/tests/import-cc-defaults.test.mjs
import assert from 'node:assert/strict';
import { loadComponent } from './kimi-poll-race.harness.mjs';

function importedDefaultsHaveClaude55Limits() {
    const { component: c, requests } = loadComponent('models.js', 'models');
    c.importCCDefaults();

    // Verify in-memory allowlist state:
    const byID = new Map(c.ccConfig.allowlist.map((m) => [m.id, m]));
    const sonnet = byID.get('claude-sonnet-5-5');
    const opus = byID.get('claude-opus-5-5');

    assert.ok(sonnet, 'claude-sonnet-5-5 must be in imported allowlist');
    assert.equal(sonnet.contextLength, 1000000, 'sonnet contextLength');
    assert.equal(sonnet.maxOutputTokens, 128000, 'sonnet maxOutputTokens');

    assert.ok(opus, 'claude-opus-5-5 must be in imported allowlist');
    assert.equal(opus.contextLength, 1000000, 'opus contextLength');
    assert.equal(opus.maxOutputTokens, 128000, 'opus maxOutputTokens');

    // Verify saveCCConfig posted the limits over the wire:
    assert.equal(requests.length, 1, 'saveCCConfig must issue 1 POST');
    const body = JSON.parse(requests[0].options.body);
    const wireByID = new Map(body.allowlist.map((m) => [m.id, m]));
    assert.equal(wireByID.get('claude-sonnet-5-5').contextLength, 1000000);
    assert.equal(wireByID.get('claude-sonnet-5-5').maxOutputTokens, 128000);
    assert.equal(wireByID.get('claude-opus-5-5').contextLength, 1000000);
    assert.equal(wireByID.get('claude-opus-5-5').maxOutputTokens, 128000);
}

const cases = [
    ['imported defaults have Claude 5.5 limits', importedDefaultsHaveClaude55Limits],
];

for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`import-cc-defaults: ${cases.length}/${cases.length} passed`);
```

Create `internal/webui/import_cc_defaults_test.go`:
```go
package webui

import (
	"os/exec"
	"testing"
)

// TestImportCCDefaultsHarness runs the Node test verifying that Claude 5.5
// defaults imported in the WebUI carry contextLength and maxOutputTokens.
func TestImportCCDefaultsHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping importCCDefaults harness")
	}
	out, err := exec.Command(node, "tests/import-cc-defaults.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("importCCDefaults harness failed: %v\n%s", err, out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog/internal/webui
node tests/import-cc-defaults.test.mjs
```
Expected: FAIL with `AssertionError: sonnet contextLength (+ undefined - 1000000)`.

- [ ] **Step 3: Update `importCCDefaults` in `internal/webui/public/js/components/models.js`**

In `internal/webui/public/js/components/models.js:1310-1311`:
Replace:
```javascript
            { id: 'claude-opus-5-5', displayName: 'Claude Opus 5.5', aliases: ['claude-opus-5-5'], enabled: true },
            { id: 'claude-sonnet-5-5', displayName: 'Claude Sonnet 5.5', aliases: ['claude-sonnet-5-5'], enabled: true },
```
With:
```javascript
            { id: 'claude-opus-5-5', displayName: 'Claude Opus 5.5', aliases: ['claude-opus-5-5'], contextLength: 1000000, maxOutputTokens: 128000, enabled: true },
            { id: 'claude-sonnet-5-5', displayName: 'Claude Sonnet 5.5', aliases: ['claude-sonnet-5-5'], contextLength: 1000000, maxOutputTokens: 128000, enabled: true },
```

- [ ] **Step 4: Run tests and verify they pass**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
gofmt -w internal/webui/import_cc_defaults_test.go
make fmt-check
go vet ./internal/webui
node internal/webui/tests/import-cc-defaults.test.mjs
go test -v ./internal/webui -run TestImportCCDefaultsHarness
```
Expected: PASS (`import-cc-defaults: 1/1 passed`).

- [ ] **Step 5: Commit**

```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
git add internal/webui/public/js/components/models.js internal/webui/tests/import-cc-defaults.test.mjs internal/webui/import_cc_defaults_test.go
git commit -m "fix(webui): set Claude 5.5 contextLength and maxOutputTokens on imported defaults"
```

---

### Task 5: Full Verification Gate & PR Branch Push

**Files:**
- Entire repository.

**Interfaces:**
- Consumes: All commits from Tasks 1–4.
- Produces: Verified branch `feat/agy-1-3-3-catalog` pushed to remote `fork`, ready for review re-check on PR #121.

- [ ] **Step 1: Check formatting across the repo**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
make fmt-check
```
Expected: `gofmt clean.`

- [ ] **Step 2: Run `go vet` across the repo**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
go vet ./...
```
Expected: clean exit, 0 warnings/errors.

- [ ] **Step 3: Run tests in the touched packages**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
go test -v ./internal/modelcatalog ./internal/webui ./internal/format ./cmd/paramprobe
```
Expected: PASS across all four packages.

- [ ] **Step 4: Run full race test suite**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
go test -race ./...
```
Expected: PASS across all packages (matches baseline established at commit `6da5bf5`). Note: `internal/auth` takes ~7.5 minutes under `-race`; allow up to 10 minutes.

- [ ] **Step 5: Review git log and diff vs main**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
git log --oneline -5
git status --short
```
Expected: clean working tree, exactly 4 remediation commits on top of `6da5bf5`.

- [ ] **Step 6: Push branch to GitHub**

Run:
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go/.worktrees/agy-1-3-3-catalog
git push fork feat/agy-1-3-3-catalog
```
Expected: successful push updating PR #121 head on GitHub.

- [ ] **Step 7: Verify PR #121 state on GitHub**

Run:
```bash
gh pr view 121 --json state,headRefOid,commits --jq '{state, head: .headRefOid, ncommits: (.commits | length)}'
```
Expected: `state: "OPEN"`, `ncommits: 12` (8 original + 4 remediation).
