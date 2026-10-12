# agy 1.3.3 Model Catalog Upgrade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Update the proxy's model catalog parsing, tier resolution, alias routing, and formatting to support agy 1.3.3 and Cloud Code PA's updated model catalog (introducing Claude 5.5 tiers `claude-opus-5-5-{low,medium,high}` and `claude-sonnet-5-5-{low,medium,high}`, upstream `thinkingLevel` integers, `gemini-pro-agent` replacing `gemini-3.1-pro-high`, and backwards compatibility for deprecated Claude 4.6 models).

**Architecture:** Port agy 1.3.3 upstream catalog behavior test-first: parse integer `thinkingLevel` in `modelcatalog.Parse`, map Claude 5.5 family display names and aliases in `routingAliases`, handle effort-based tier routing in `ResolveWithRequest`, normalize public model IDs in `CleanModelIDAndName`, and adapt `format` thinking level emission so Claude 5.5 models receive `thinkingConfig.thinkingLevel`.

**Tech Stack:** Go 1.27, `internal/modelcatalog`, `internal/format`, `internal/accounts`, `cmd/cloudcodecheck`, `cmd/debug429`.

**Spec:** `.reference/agy-current-models.txt`, `.reference/agy-models-20260903.txt`, `AGENTS.md`.

## Global Constraints

- **Disguise invariant:** Do not touch TLS internals. Zero changes to `tls.Config{}`. Standard Go HTTP client remains the disguise.
- **Upstream parity:** Never fabricate proto schemas or invent model IDs that upstream does not accept.
- **Account compatibility:** The upstream catalog is account-dependent. Accounts that still publish legacy models (`gemini-3.5`, `claude-sonnet-4-6`) must continue resolving them via `byID`/`byDisplay` before routing aliases.
- **TDD:** Write failing unit tests before updating implementations; run package test suite after every change.
- **Commit gate:** `go build ./... && go vet ./... && go test -race ./...` must pass on each task.

---

### Task 1: Record agy 1.3.3 Ground Truth Baselines

**Files:**
- Create: `.reference/agy-models-20261011.txt`
- Modify: `.reference/agy-current-models.txt`

**Interfaces:**
- Consumes: `/Users/gus/.local/bin/agy models` and `go run ./cmd/cloudcodecheck -operation models -details`.
- Produces: Committed ground truth baselines for agy 1.3.3 and Cloud Code PA model catalog.

- [ ] **Step 1: Capture live `agy models` output from local agy 1.3.3**

Run:
```bash
agy models
```
Expected output (18 selectable models):
```
gemini-3.8-flash-high	Gemini 3.8 Flash (High)
gemini-3.8-flash-medium	Gemini 3.8 Flash (Medium)
gemini-3.8-flash-low	Gemini 3.8 Flash (Low)
gemini-3.7-flash-high	Gemini 3.7 Flash (High)
gemini-3.7-flash-medium	Gemini 3.7 Flash (Medium)
gemini-3.7-flash-low	Gemini 3.7 Flash (Low)
gemini-3.6-flash-high	Gemini 3.6 Flash (High)
gemini-3.6-flash-medium	Gemini 3.6 Flash (Medium)
gemini-3.6-flash-low	Gemini 3.6 Flash (Low)
gemini-3.1-pro-high	Gemini 3.1 Pro (High)
gemini-3.1-pro-low	Gemini 3.1 Pro (Low)
claude-opus-5-5-low	Claude Opus 5.5 (Low)
claude-opus-5-5-medium	Claude Opus 5.5 (Medium)
claude-opus-5-5-high	Claude Opus 5.5 (High)
claude-sonnet-5-5-low	Claude Sonnet 5.5 (Low)
claude-sonnet-5-5-medium	Claude Sonnet 5.5 (Medium)
claude-sonnet-5-5-high	Claude Sonnet 5.5 (High)
gpt-oss-120b-medium	GPT-OSS 120B (Medium)
```

- [ ] **Step 2: Create `.reference/agy-models-20261011.txt`**

Write detailed documentation recording:
1. Binary version (`agy 1.3.3`).
2. Order of selectable models from `agy models`.
3. Upstream `agentModelSorts` and `deprecatedModelIds` (`gemini-3.1-pro-high` -> `gemini-pro-agent`).
4. Claude 5.5 specifications: context length 1,000,000, max output 128,000, `thinkingLevel` integers (1=Low, 2=Medium, 3=High).
5. Claude 4.6 deprecation status (upstream returns `"Claude Sonnet 4.6 is no longer available. Please switch to Claude Sonnet 5.5."` on migrated accounts).

- [ ] **Step 3: Update `.reference/agy-current-models.txt`**

Update `.reference/agy-current-models.txt` header and baseline model list to reflect agy 1.3.3 and date 2026-10-11.

- [ ] **Step 4: Verify git status and commit**

```bash
git add .reference/agy-models-20261011.txt .reference/agy-current-models.txt
git commit -m "docs(models): record agy 1.3.3 model catalog ground truth"
```

---

### Task 2: ModelCatalog Upstream ThinkingLevel Integer Parsing

**Files:**
- Modify: `internal/modelcatalog/catalog.go:73-85,170-209,440-460`
- Modify: `internal/modelcatalog/catalog_test.go`

**Interfaces:**
- Consumes: JSON document from Cloud Code `FetchAvailableModels` containing `"thinkingLevel": 1|2|3`.
- Produces: `Model.ThinkingLevel` set to `"LOW"`, `"MEDIUM"`, `"HIGH"`, or `""`.

- [ ] **Step 1: Write the failing test**

In `internal/modelcatalog/catalog_test.go`, add `TestParsePopulatesThinkingLevelFromUpstreamInteger`:

```go
func TestParsePopulatesThinkingLevelFromUpstreamInteger(t *testing.T) {
	t.Parallel()
	raw := `{
		"defaultAgentModelId":"claude-sonnet-5-5-high",
		"agentModelSorts":[{"groups":[{"modelIds":[
			"claude-sonnet-5-5-high",
			"claude-sonnet-5-5-medium",
			"claude-sonnet-5-5-low",
			"claude-opus-5-5-high"
		]}]}],
		"models":{
			"claude-sonnet-5-5-high":{"displayName":"Claude Sonnet 5.5 (High)","supportsThinking":true,"thinkingLevel":3,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-sonnet-5-5-medium":{"displayName":"Claude Sonnet 5.5 (Medium)","supportsThinking":true,"thinkingLevel":2,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-sonnet-5-5-low":{"displayName":"Claude Sonnet 5.5 (Low)","supportsThinking":true,"thinkingLevel":1,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-opus-5-5-high":{"displayName":"Claude Opus 5.5 (High)","supportsThinking":true,"thinkingLevel":3,"maxTokens":1000000,"maxOutputTokens":128000}
		}
	}`

	catalog, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	high, err := catalog.Resolve("claude-sonnet-5-5-high")
	if err != nil {
		t.Fatalf("Resolve high: %v", err)
	}
	if high.ThinkingLevel != "HIGH" {
		t.Errorf("claude-sonnet-5-5-high ThinkingLevel = %q, want HIGH", high.ThinkingLevel)
	}

	med, err := catalog.Resolve("claude-sonnet-5-5-medium")
	if err != nil {
		t.Fatalf("Resolve medium: %v", err)
	}
	if med.ThinkingLevel != "MEDIUM" {
		t.Errorf("claude-sonnet-5-5-medium ThinkingLevel = %q, want MEDIUM", med.ThinkingLevel)
	}

	low, err := catalog.Resolve("claude-sonnet-5-5-low")
	if err != nil {
		t.Fatalf("Resolve low: %v", err)
	}
	if low.ThinkingLevel != "LOW" {
		t.Errorf("claude-sonnet-5-5-low ThinkingLevel = %q, want LOW", low.ThinkingLevel)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/modelcatalog -run TestParsePopulatesThinkingLevelFromUpstreamInteger`
Expected: FAIL with `ThinkingLevel = "", want HIGH`

- [ ] **Step 3: Implement minimal code to make test pass**

In `internal/modelcatalog/catalog.go`:
1. In `type modelDetails struct`, add:
```go
	ThinkingLevel            int       `json:"thinkingLevel"`
```
2. Add helper:
```go
func thinkingLevelString(level int) string {
	switch level {
	case 1:
		return "LOW"
	case 2:
		return "MEDIUM"
	case 3:
		return "HIGH"
	case 4:
		return "MINIMAL"
	default:
		return ""
	}
}
```
3. In `modelFromDetails` and in `Parse` model construction, set:
```go
	level := thinkingLevelString(details.ThinkingLevel)
	if level == "" {
		if strings.HasSuffix(id, "-high") {
			level = "HIGH"
		} else if strings.HasSuffix(id, "-medium") {
			level = "MEDIUM"
		} else if strings.HasSuffix(id, "-low") {
			level = "LOW"
		}
	}
	model.ThinkingLevel = level
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/modelcatalog -run TestParsePopulatesThinkingLevelFromUpstreamInteger`
Expected: PASS

- [ ] **Step 5: Run all modelcatalog tests**

Run: `go test -v ./internal/modelcatalog`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "feat(modelcatalog): parse upstream thinkingLevel integer into model ThinkingLevel"
```

---

### Task 3: Claude 5.5 Routing Aliases & Legacy Claude 4.6 Fallback

**Files:**
- Modify: `internal/modelcatalog/catalog.go:92-131`
- Modify: `internal/modelcatalog/catalog_test.go`

**Interfaces:**
- Consumes: `routingAliases` map and `catalog.Resolve()`.
- Produces:
  - Resolution of `claude-opus-5-5` and `claude-sonnet-5-5` (and tiers) to display names.
  - Repoint of legacy `claude-sonnet-4-6` and `claude-opus-4-6` to Claude 5.5 High when 4.6 is missing from upstream account.
  - Direct resolution of 4.6 when still published by an upstream account.

- [ ] **Step 1: Write the failing test**

In `internal/modelcatalog/catalog_test.go`, add `TestClaude55RoutingAliasesAndLegacy46Fallback`:

```go
func TestClaude55RoutingAliasesAndLegacy46Fallback(t *testing.T) {
	t.Parallel()
	// Account with only Claude 5.5 published:
	raw55 := `{
		"defaultAgentModelId":"claude-sonnet-5-5-high",
		"agentModelSorts":[{"groups":[{"modelIds":[
			"claude-sonnet-5-5-high",
			"claude-sonnet-5-5-medium",
			"claude-sonnet-5-5-low",
			"claude-opus-5-5-high",
			"claude-opus-5-5-medium",
			"claude-opus-5-5-low",
			"gemini-pro-agent"
		]}]}],
		"models":{
			"claude-sonnet-5-5-high":{"displayName":"Claude Sonnet 5.5 (High)","supportsThinking":true,"thinkingLevel":3,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-sonnet-5-5-medium":{"displayName":"Claude Sonnet 5.5 (Medium)","supportsThinking":true,"thinkingLevel":2,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-sonnet-5-5-low":{"displayName":"Claude Sonnet 5.5 (Low)","supportsThinking":true,"thinkingLevel":1,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-opus-5-5-high":{"displayName":"Claude Opus 5.5 (High)","supportsThinking":true,"thinkingLevel":3,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-opus-5-5-medium":{"displayName":"Claude Opus 5.5 (Medium)","supportsThinking":true,"thinkingLevel":2,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-opus-5-5-low":{"displayName":"Claude Opus 5.5 (Low)","supportsThinking":true,"thinkingLevel":1,"maxTokens":1000000,"maxOutputTokens":128000},
			"gemini-pro-agent":{"displayName":"Gemini 3.1 Pro (High)","supportsThinking":true,"maxTokens":1048576,"maxOutputTokens":65536}
		}
	}`

	cat55, err := Parse([]byte(raw55))
	if err != nil {
		t.Fatalf("Parse raw55: %v", err)
	}

	// Base alias resolves to High tier
	sonnet, err := cat55.Resolve("claude-sonnet-5-5")
	if err != nil {
		t.Fatalf("Resolve claude-sonnet-5-5: %v", err)
	}
	if sonnet.ID != "claude-sonnet-5-5-high" {
		t.Errorf("claude-sonnet-5-5 = %q, want claude-sonnet-5-5-high", sonnet.ID)
	}

	opus, err := cat55.Resolve("claude-opus-5-5")
	if err != nil {
		t.Fatalf("Resolve claude-opus-5-5: %v", err)
	}
	if opus.ID != "claude-opus-5-5-high" {
		t.Errorf("claude-opus-5-5 = %q, want claude-opus-5-5-high", opus.ID)
	}

	// Legacy 4.6 requests fall back to 5.5 High when 4.6 is absent
	legacySonnet, err := cat55.Resolve("claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("Resolve legacy claude-sonnet-4-6: %v", err)
	}
	if legacySonnet.ID != "claude-sonnet-5-5-high" {
		t.Errorf("legacy claude-sonnet-4-6 = %q, want claude-sonnet-5-5-high", legacySonnet.ID)
	}

	legacyOpus, err := cat55.Resolve("claude-opus-4-6-thinking")
	if err != nil {
		t.Fatalf("Resolve legacy claude-opus-4-6-thinking: %v", err)
	}
	if legacyOpus.ID != "claude-opus-5-5-high" {
		t.Errorf("legacy claude-opus-4-6-thinking = %q, want claude-opus-5-5-high", legacyOpus.ID)
	}

	// gemini-3.1-pro-high resolves to gemini-pro-agent
	geminiPro, err := cat55.Resolve("gemini-3.1-pro-high")
	if err != nil {
		t.Fatalf("Resolve gemini-3.1-pro-high: %v", err)
	}
	if geminiPro.ID != "gemini-pro-agent" {
		t.Errorf("gemini-3.1-pro-high = %q, want gemini-pro-agent", geminiPro.ID)
	}

	// Account still publishing legacy 4.6 resolves 4.6 directly (byID wins before routingAliases):
	rawLegacy := `{
		"defaultAgentModelId":"claude-sonnet-4-6",
		"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-4-6"]}]}],
		"models":{
			"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6","supportsThinking":true,"maxTokens":250000,"maxOutputTokens":64000}
		}
	}`
	catLegacy, err := Parse([]byte(rawLegacy))
	if err != nil {
		t.Fatalf("Parse rawLegacy: %v", err)
	}
	legacyDirect, err := catLegacy.Resolve("claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("Resolve legacyDirect: %v", err)
	}
	if legacyDirect.ID != "claude-sonnet-4-6" {
		t.Errorf("legacy account claude-sonnet-4-6 = %q, want claude-sonnet-4-6", legacyDirect.ID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/modelcatalog -run TestClaude55RoutingAliasesAndLegacy46Fallback`
Expected: FAIL with `claude-sonnet-5-5: model not found`

- [ ] **Step 3: Update `routingAliases` in `internal/modelcatalog/catalog.go`**

In `internal/modelcatalog/catalog.go:92-131`, update `routingAliases`:
```go
var routingAliases = map[string]string{
	// Cloud Code publishes gemini-3.1-pro-high in models, but current agy
	// selects the separate agent route for the same display model.
	"gemini-3.1-pro-high": "Gemini 3.1 Pro (High)",
	"gemini-3.1-pro":      "Gemini 3.1 Pro (High)",
	"gemini-pro":          "Gemini 3.1 Pro (High)",
	"gemini-pro-agent":    "Gemini 3.1 Pro (High)",
	"gemini-3.6-flash":    "Gemini 3.6 Flash (High)",
	"gemini-3.7-flash":    "Gemini 3.7 Flash (High)",
	"gpt-oss-120b":        "GPT-OSS 120B (Medium)",
	// gemini-3.8-flash-{high,medium,low} first published by agy 1.1.25
	// (.reference/agy-models-20260903.txt, View A).
	"gemini-3.8-flash":        "Gemini 3.8 Flash (High)",
	"gemini-3.8-flash-high":   "Gemini 3.8 Flash (High)",
	"gemini-3.8-flash-medium": "Gemini 3.8 Flash (Medium)",
	"gemini-3.8-flash-low":    "Gemini 3.8 Flash (Low)",
	// Legacy gemini-3.5 IDs repoint to the gemini-3.8 family tier-preserving
	// (user decision 2026-09-03). Accounts that still publish real 3.5 are
	// unaffected: byID/byDisplay lookups win before routingAliases.
	"gemini-3.5-flash":           "Gemini 3.8 Flash (High)",
	"gemini-3.5-flash-high":      "Gemini 3.8 Flash (High)",
	"gemini-3.5-flash-medium":    "Gemini 3.8 Flash (Medium)",
	"gemini-3.5-flash-low":       "Gemini 3.8 Flash (Low)",
	"gemini-3.5-flash-extra-low": "Gemini 3.8 Flash (Low)",
	"gemini-3.7-flash-high":      "Gemini 3.7 Flash (High)",
	"gemini-3.7-flash-medium":    "Gemini 3.7 Flash (Medium)",
	"gemini-3.7-flash-low":       "Gemini 3.7 Flash (Low)",

	// Claude Models & Aliases mapped to Cloud Code display names.
	// agy 1.3.3 publishes Claude 5.5 tiers:
	"claude-opus-5-5":        "Claude Opus 5.5 (High)",
	"claude-opus-5-5-high":   "Claude Opus 5.5 (High)",
	"claude-opus-5-5-medium": "Claude Opus 5.5 (Medium)",
	"claude-opus-5-5-low":    "Claude Opus 5.5 (Low)",
	"claude-sonnet-5-5":        "Claude Sonnet 5.5 (High)",
	"claude-sonnet-5-5-high":   "Claude Sonnet 5.5 (High)",
	"claude-sonnet-5-5-medium": "Claude Sonnet 5.5 (Medium)",
	"claude-sonnet-5-5-low":    "Claude Sonnet 5.5 (Low)",

	// Legacy Claude 4.6 IDs repoint to the Claude 5.5 family (tier-preserving: High).
	// Accounts that still publish real 4.6 are unaffected: byID/byDisplay lookups win before routingAliases.
	"claude-sonnet-4-6-thinking": "Claude Sonnet 5.5 (High)",
	"claude-sonnet-4-6":          "Claude Sonnet 5.5 (High)",
	"claude-opus-4-6":            "Claude Opus 5.5 (High)",
	"claude-opus-4-6-thinking":   "Claude Opus 5.5 (High)",
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/modelcatalog -run TestClaude55RoutingAliasesAndLegacy46Fallback`
Expected: PASS

- [ ] **Step 5: Run all modelcatalog tests**

Run: `go test -v ./internal/modelcatalog`
Expected: PASS (fix any existing test expectations if they assumed 4.6 aliases pointed to "Claude Sonnet 4.6 (Thinking)" when only 4.6 was published in that test's mock body).

- [ ] **Step 6: Commit**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "feat(modelcatalog): add Claude 5.5 routing aliases and legacy 4.6 fallback"
```

---

### Task 4: Clean Model ID and Name Normalization for Claude 5.5 and Gemini Pro Agent

**Files:**
- Modify: `internal/modelcatalog/catalog.go:370-405`
- Modify: `internal/modelcatalog/catalog_test.go`

**Interfaces:**
- Consumes: `CleanModelIDAndName(id, displayName string) (string, string)`.
- Produces: Normalized `(cleanID, cleanName)` for public endpoints (`/v1/models`, WebUI).

- [ ] **Step 1: Write the failing test**

In `internal/modelcatalog/catalog_test.go`, add `TestCleanModelIDAndNameClaude55`:

```go
func TestCleanModelIDAndNameClaude55(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id          string
		displayName string
		wantID      string
		wantName    string
	}{
		{"claude-sonnet-5-5-high", "Claude Sonnet 5.5 (High)", "claude-sonnet-5-5", "Claude Sonnet 5.5"},
		{"claude-sonnet-5-5-medium", "Claude Sonnet 5.5 (Medium)", "claude-sonnet-5-5", "Claude Sonnet 5.5"},
		{"claude-sonnet-5-5-low", "Claude Sonnet 5.5 (Low)", "claude-sonnet-5-5", "Claude Sonnet 5.5"},
		{"claude-opus-5-5-high", "Claude Opus 5.5 (High)", "claude-opus-5-5", "Claude Opus 5.5"},
		{"claude-opus-5-5-medium", "Claude Opus 5.5 (Medium)", "claude-opus-5-5", "Claude Opus 5.5"},
		{"claude-opus-5-5-low", "Claude Opus 5.5 (Low)", "claude-opus-5-5", "Claude Opus 5.5"},
		{"gemini-pro-agent", "Gemini 3.1 Pro (High)", "gemini-3.1-pro", "Gemini 3.1 Pro"},
		{"claude-opus-4-6-thinking", "Claude Opus 4.6 (Thinking)", "claude-opus-4-6", "Claude Opus 4.6"},
	}

	for _, tc := range cases {
		gotID, gotName := CleanModelIDAndName(tc.id, tc.displayName)
		if gotID != tc.wantID || gotName != tc.wantName {
			t.Errorf("CleanModelIDAndName(%q, %q) = (%q, %q), want (%q, %q)",
				tc.id, tc.displayName, gotID, gotName, tc.wantID, tc.wantName)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/modelcatalog -run TestCleanModelIDAndNameClaude55`
Expected: FAIL with `gotID = "claude-sonnet-5-5-high", want "claude-sonnet-5-5"`

- [ ] **Step 3: Implement Claude 5.5 in `CleanModelIDAndName`**

In `internal/modelcatalog/catalog.go:370-405`:
```go
func CleanModelIDAndName(id, displayName string) (string, string) {
	id = Strip1mSuffix(id)
	displayName = Strip1mSuffix(displayName)
	lowerID := strings.ToLower(id)
	cleanID := id
	cleanName := displayName

	switch {
	case strings.HasPrefix(lowerID, "gemini-3.8-flash"):
		cleanID = "gemini-3.8-flash"
		cleanName = "Gemini 3.8 Flash"
	case strings.HasPrefix(lowerID, "gemini-3.7-flash"):
		cleanID = "gemini-3.7-flash"
		cleanName = "Gemini 3.7 Flash"
	case strings.HasPrefix(lowerID, "gemini-3.6-flash"):
		cleanID = "gemini-3.6-flash"
		cleanName = "Gemini 3.6 Flash"
	case lowerID == "gemini-3-flash-agent" || lowerID == "gemini-3.5-flash-low" || lowerID == "gemini-3.5-flash-extra-low" || lowerID == "gemini-3.5-flash-high" || lowerID == "gemini-3.5-flash-medium":
		cleanID = "gemini-3.5-flash"
		cleanName = "Gemini 3.5 Flash"
	case lowerID == "gemini-pro-agent" || lowerID == "gemini-3.1-pro-high" || lowerID == "gemini-3.1-pro-low":
		cleanID = "gemini-3.1-pro"
		cleanName = "Gemini 3.1 Pro"
	case strings.HasPrefix(lowerID, "claude-opus-5-5"):
		cleanID = "claude-opus-5-5"
		cleanName = "Claude Opus 5.5"
	case strings.HasPrefix(lowerID, "claude-sonnet-5-5"):
		cleanID = "claude-sonnet-5-5"
		cleanName = "Claude Sonnet 5.5"
	case lowerID == "claude-opus-4-6-thinking" || lowerID == "claude-opus-4-6":
		cleanID = "claude-opus-4-6"
		cleanName = "Claude Opus 4.6"
	case lowerID == "claude-sonnet-4-6-thinking" || lowerID == "claude-sonnet-4-6":
		cleanID = "claude-sonnet-4-6"
		cleanName = "Claude Sonnet 4.6"
	case lowerID == "gpt-oss-120b-medium" || lowerID == "gpt-oss-120b":
		cleanID = "gpt-oss-120b"
		cleanName = "GPT-OSS 120B"
	default:
		for _, suffix := range []string{" (High)", " (Medium)", " (Low)", " (Thinking)"} {
			cleanName = strings.TrimSuffix(cleanName, suffix)
		}
	}
	return cleanID, cleanName
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/modelcatalog -run TestCleanModelIDAndNameClaude55`
Expected: PASS

- [ ] **Step 5: Run all modelcatalog tests**

Run: `go test -v ./internal/modelcatalog`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "feat(modelcatalog): normalize Claude 5.5 IDs in CleanModelIDAndName"
```

---

### Task 5: Claude 5.5 Effort-Based Tier Resolution in `ResolveWithRequest`

**Files:**
- Modify: `internal/modelcatalog/catalog.go:297-368`
- Modify: `internal/modelcatalog/catalog_test.go`

**Interfaces:**
- Consumes: `ResolveWithRequest(requested string, request map[string]any) (Model, error)`.
- Produces: Tiered Claude 5.5 model based on client reasoning effort (`low`, `medium`, `high`) or disabled thinking handling.

- [ ] **Step 1: Write the failing test**

In `internal/modelcatalog/catalog_test.go`, add `TestClaude55EffortTierResolution`:

```go
func TestClaude55EffortTierResolution(t *testing.T) {
	t.Parallel()
	raw := `{
		"defaultAgentModelId":"claude-sonnet-5-5-high",
		"agentModelSorts":[{"groups":[{"modelIds":[
			"claude-sonnet-5-5-high",
			"claude-sonnet-5-5-medium",
			"claude-sonnet-5-5-low",
			"claude-opus-5-5-high",
			"claude-opus-5-5-medium",
			"claude-opus-5-5-low"
		]}]}],
		"models":{
			"claude-sonnet-5-5-high":{"displayName":"Claude Sonnet 5.5 (High)","supportsThinking":true,"thinkingLevel":3,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-sonnet-5-5-medium":{"displayName":"Claude Sonnet 5.5 (Medium)","supportsThinking":true,"thinkingLevel":2,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-sonnet-5-5-low":{"displayName":"Claude Sonnet 5.5 (Low)","supportsThinking":true,"thinkingLevel":1,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-opus-5-5-high":{"displayName":"Claude Opus 5.5 (High)","supportsThinking":true,"thinkingLevel":3,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-opus-5-5-medium":{"displayName":"Claude Opus 5.5 (Medium)","supportsThinking":true,"thinkingLevel":2,"maxTokens":1000000,"maxOutputTokens":128000},
			"claude-opus-5-5-low":{"displayName":"Claude Opus 5.5 (Low)","supportsThinking":true,"thinkingLevel":1,"maxTokens":1000000,"maxOutputTokens":128000}
		}
	}`

	catalog, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	effort := func(level string) map[string]any {
		return map[string]any{"reasoning_effort": level}
	}

	// claude-sonnet-5-5 with effort low routes to low tier
	lowSonnet, err := catalog.ResolveWithRequest("claude-sonnet-5-5", effort("low"))
	if err != nil {
		t.Fatalf("Resolve lowSonnet: %v", err)
	}
	if lowSonnet.ID != "claude-sonnet-5-5-low" || lowSonnet.ThinkingLevel != "LOW" {
		t.Errorf("lowSonnet = (%q, %q), want (claude-sonnet-5-5-low, LOW)", lowSonnet.ID, lowSonnet.ThinkingLevel)
	}

	// claude-sonnet-5-5 with effort medium routes to medium tier
	medSonnet, err := catalog.ResolveWithRequest("claude-sonnet-5-5", effort("medium"))
	if err != nil {
		t.Fatalf("Resolve medSonnet: %v", err)
	}
	if medSonnet.ID != "claude-sonnet-5-5-medium" || medSonnet.ThinkingLevel != "MEDIUM" {
		t.Errorf("medSonnet = (%q, %q), want (claude-sonnet-5-5-medium, MEDIUM)", medSonnet.ID, medSonnet.ThinkingLevel)
	}

	// claude-opus-5-5 with effort low routes to low tier
	lowOpus, err := catalog.ResolveWithRequest("claude-opus-5-5", effort("low"))
	if err != nil {
		t.Fatalf("Resolve lowOpus: %v", err)
	}
	if lowOpus.ID != "claude-opus-5-5-low" || lowOpus.ThinkingLevel != "LOW" {
		t.Errorf("lowOpus = (%q, %q), want (claude-opus-5-5-low, LOW)", lowOpus.ID, lowOpus.ThinkingLevel)
	}

	// claude-sonnet-4-6 with effort medium routes to claude-sonnet-5-5-medium
	repSonnet, err := catalog.ResolveWithRequest("claude-sonnet-4-6", effort("medium"))
	if err != nil {
		t.Fatalf("Resolve repSonnet: %v", err)
	}
	if repSonnet.ID != "claude-sonnet-5-5-medium" {
		t.Errorf("repSonnet = %q, want claude-sonnet-5-5-medium", repSonnet.ID)
	}

	// disabled thinking clears ThinkingLevel and sets SupportsThinking = false
	disabled := map[string]any{"thinking": map[string]any{"type": "disabled"}}
	disSonnet, err := catalog.ResolveWithRequest("claude-sonnet-5-5-high", disabled)
	if err != nil {
		t.Fatalf("Resolve disabled: %v", err)
	}
	if disSonnet.SupportsThinking || disSonnet.ThinkingLevel != "" {
		t.Errorf("disSonnet SupportsThinking=%v ThinkingLevel=%q, want false, \"\"",
			disSonnet.SupportsThinking, disSonnet.ThinkingLevel)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/modelcatalog -run TestClaude55EffortTierResolution`
Expected: FAIL with `lowSonnet.ID = claude-sonnet-5-5-high, want claude-sonnet-5-5-low`

- [ ] **Step 3: Update `ResolveWithRequest` in `internal/modelcatalog/catalog.go`**

In `internal/modelcatalog/catalog.go:297-368`:
1. When `params.Disabled`:
```go
	if params.Disabled {
		if isGemini37Flash(model.ID) || isGemini37Flash(requested) {
			if variant, err := catalog.Resolve("gemini-3.7-flash-low"); err == nil {
				return variant, nil
			}
			model.ThinkingLevel = "LOW"
			model.SupportsThinking = true
			return model, nil
		}
		if isGemini38Flash(model.ID) || isGemini38Flash(requested) {
			if variant, err := catalog.Resolve("gemini-3.8-flash-low"); err == nil {
				return variant, nil
			}
			model.ThinkingLevel = "LOW"
			model.SupportsThinking = true
			return model, nil
		}
		model.SupportsThinking = false
		model.ThinkingBudget = 0
		model.ThinkingLevel = ""
		return model, nil
	}
```
2. In `effortTier` switch:
```go
		switch {
		case strings.HasPrefix(lowerReq, "gemini-3.8-flash"):
			targetID = "gemini-3.8-flash-" + tier
		case strings.HasPrefix(lowerReq, "gemini-3.7-flash"):
			targetID = "gemini-3.7-flash-" + tier
		case strings.HasPrefix(lowerReq, "gemini-3.6-flash"):
			targetID = "gemini-3.6-flash-" + tier
		case strings.HasPrefix(lowerReq, "gemini-3.5-flash"):
			targetID = "gemini-3.8-flash-" + tier
		case strings.HasPrefix(lowerReq, "claude-sonnet-5-5"):
			targetID = "claude-sonnet-5-5-" + tier
		case strings.HasPrefix(lowerReq, "claude-opus-5-5"):
			targetID = "claude-opus-5-5-" + tier
		case strings.HasPrefix(lowerReq, "claude-sonnet-4-6") || lowerReq == "claude-sonnet":
			targetID = "claude-sonnet-5-5-" + tier
		case strings.HasPrefix(lowerReq, "claude-opus-4-6") || lowerReq == "claude-opus":
			targetID = "claude-opus-5-5-" + tier
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/modelcatalog -run TestClaude55EffortTierResolution`
Expected: PASS

- [ ] **Step 5: Run all modelcatalog tests**

Run: `go test -v ./internal/modelcatalog`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "feat(modelcatalog): route Claude 5.5 reasoning effort to model tiers"
```

---

### Task 6: Format Converter Thinking & Family Support for Claude 5.5

**Files:**
- Modify: `internal/format/model.go:46-90`
- Modify: `internal/format/request.go:158-180`
- Modify: `internal/format/thinkingconfig_test.go`

**Interfaces:**
- Consumes: `ModelOptions` with `ThinkingLevel: "HIGH"|"MEDIUM"|"LOW"`.
- Produces: Request generation payload with `"thinkingConfig": {"includeThoughts": true, "thinkingLevel": level}` and beta header support.

- [ ] **Step 1: Write the failing test**

In `internal/format/thinkingconfig_test.go`, add `TestClaude55EmitsThinkingLevel`:

```go
func TestClaude55EmitsThinkingLevel(t *testing.T) {
	t.Parallel()
	builder := NewBuilder()

	// High tier options
	options := ModelOptions{
		SupportsThinking: true,
		ThinkingLevel:    "HIGH",
		MaxOutputTokens:  128000,
	}

	req := map[string]any{
		"model": "claude-sonnet-5-5-high",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}

	payload := builder.BuildCloudCodeRequestWithModel(req, "test-proj", "user@example.com", options)
	inner, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("missing inner request: %#v", payload)
	}
	gen, ok := inner["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing generationConfig: %#v", inner)
	}
	tc, ok := gen["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing thinkingConfig: %#v", gen)
	}
	if tc["thinkingLevel"] != "HIGH" {
		t.Errorf("thinkingLevel = %v, want HIGH", tc["thinkingLevel"])
	}
	if tc["includeThoughts"] != true {
		t.Errorf("includeThoughts = %v, want true", tc["includeThoughts"])
	}
	// thinkingBudget must NOT be present when thinkingLevel is set
	if _, hasBudget := tc["thinkingBudget"]; hasBudget {
		t.Errorf("unexpected thinkingBudget in thinkingConfig: %#v", tc)
	}

	// Disabled thinking removes thinkingConfig
	reqDisabled := map[string]any{
		"model": "claude-sonnet-5-5-high",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"thinking": map[string]any{"type": "disabled"},
	}
	optionsDisabled := ModelOptions{
		SupportsThinking: false,
		ThinkingLevel:    "",
		MaxOutputTokens:  128000,
	}
	payloadDis := builder.BuildCloudCodeRequestWithModel(reqDisabled, "test-proj", "user@example.com", optionsDisabled)
	innerDis := payloadDis["request"].(map[string]any)
	genDis := innerDis["generationConfig"].(map[string]any)
	if _, hasTC := genDis["thinkingConfig"]; hasTC {
		t.Errorf("disabled thinking must omit thinkingConfig, got %#v", genDis["thinkingConfig"])
	}
}
```

- [ ] **Step 2: Run test to verify it passes or fails**

Run: `go test -v ./internal/format -run TestClaude55EmitsThinkingLevel`
Check whether `IsThinkingModel` recognizes Claude 5.5.

- [ ] **Step 3: Update `internal/format/model.go`**

In `internal/format/model.go:64-70`:
```go
	isThinking := false
	if family == FamilyClaude {
		if strings.Contains(lower, "thinking") || strings.Contains(lower, "5-5") || strings.Contains(lower, "claude-5") {
			isThinking = true
		}
	} else if family == FamilyGemini {
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/format -run TestClaude55EmitsThinkingLevel`
Expected: PASS

- [ ] **Step 5: Run all format tests**

Run: `go test -v ./internal/format`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/format/model.go internal/format/thinkingconfig_test.go
git commit -m "feat(format): support Claude 5.5 thinkingLevel emission and model family classification"
```

---

### Task 7: Update Tool Defaults, WebUI Data & Paramprobe

**Files:**
- Modify: `cmd/paramprobe/main.go:32`
- Modify: `internal/webui/public/js/data-store.js:584-598`
- Modify: `internal/webui/public/js/components/models.js:1310-1320`

**Interfaces:**
- Consumes: Updated model names.
- Produces: Default flag values and webui mockup discovery entries.

- [ ] **Step 1: Update `cmd/paramprobe/main.go`**

In `cmd/paramprobe/main.go:32`:
Update default flag:
```go
	claude := flag.String("claude", "claude-sonnet-5-5-high", "upstream Claude thinking model ID")
```

- [ ] **Step 2: Update `internal/webui/public/js/data-store.js`**

In `_generatePlaceholderData()` (around line 584):
Add `'claude-opus-5-5-high'`, `'claude-sonnet-5-5-high'`, `'gemini-pro-agent'` to the placeholder models array.

- [ ] **Step 3: Update `internal/webui/public/js/components/models.js`**

In discovery models table (around line 1310):
Add:
```javascript
            { id: 'claude-opus-5-5', displayName: 'Claude Opus 5.5', aliases: ['claude-opus-5-5'], enabled: true },
            { id: 'claude-sonnet-5-5', displayName: 'Claude Sonnet 5.5', aliases: ['claude-sonnet-5-5'], enabled: true },
```

- [ ] **Step 4: Run all package tests**

Run: `go test ./...`
Expected: All packages pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/paramprobe/main.go internal/webui/public/js/data-store.js internal/webui/public/js/components/models.js
git commit -m "chore(tools,webui): update default Claude model to 5.5 and add discovery models"
```

---

### Task 8: End-to-End Live Verification & Smoke Test

**Files:** None (runtime validation).

**Interfaces:**
- Consumes: Live Cloud Code PA connection via `cmd/debug429` and `cmd/cloudcodecheck`.
- Produces: Packet/HTTP verified proof that proxy routes Claude 5.5 and Gemini Pro Agent end-to-end.

- [ ] **Step 1: Verify `cloudcodecheck` live catalog parse**

Run:
```bash
go run ./cmd/cloudcodecheck -operation models
```
Expected: `cloudcode_ok operation=models status=200` with successful decode.

- [ ] **Step 2: Verify `debug429` with `claude-sonnet-5-5-high`**

Run:
```bash
go run ./cmd/debug429 -model claude-sonnet-5-5-high -thinking HIGH
```
Expected: HTTP 200 with candidates streaming `OK` and `modelVersion: "claude-sonnet-5-5-high"`.

- [ ] **Step 3: Verify `debug429` with `claude-opus-5-5-high`**

Run:
```bash
go run ./cmd/debug429 -model claude-opus-5-5-high -thinking HIGH
```
Expected: HTTP 200 with candidates streaming `OK` and `modelVersion: "claude-opus-5-5-high"`.

- [ ] **Step 4: Verify full repository test suite**

Run:
```bash
go test -race ./...
```
Expected: All packages OK.

- [ ] **Step 5: Verify build**

Run:
```bash
go build -o bin/proxy ./cmd/proxy
```
Expected: Clean compilation with 0 warnings or errors.
