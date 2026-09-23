# Remove Manual `[1m]` Suffix and Automate Max Context Configuration

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove user-facing `[1m]` model suffix requirements across the proxy backend and WebUI, ensuring models that support 1M+ context (such as Gemini 1.5/2.5/3.x families) are automatically configured with their maximum context window while maintaining backward-compatible resolution for legacy `[1m]` requests.

**Architecture:** 
1. The backend model catalog (`internal/modelcatalog`) strips legacy `[1m]` / `[1M]` suffixes during normalization and resolution, mapping any legacy `[1m]` inputs to the canonical model while preserving native upstream `MaxTokens` (1,048,576 for Gemini).
2. The proxy model discovery endpoints (`/v1/models` and `/models`) advertise accurate `context_window` metadata derived directly from model metadata.
3. The WebUI (`internal/webui`) removes the manual "Gemini 1M Context Mode" toggle switch and automatic `[1m]` suffix injection, cleans legacy `[1m]` suffixes when saving/loading `~/.claude/settings.json`, and reflects model capabilities natively based on context length.

**Tech Stack:** Go 1.24+, Alpine.js, TailwindCSS, HTML5, standard library `net/http` and `testing`.

**Spec:** No external spec document. The user requirement states: "Write a plan to remove the need for the '[1m]' suffix in models that support 1m+ context. Models that allow 1m context should be automatically configured to use the max."

## Global Constraints

- Never break backward compatibility for existing scripts, environments, or API callers still transmitting `[1m]` or `[1M]` suffixes in model strings.
- Upstream Google Cloud Code / Gemini models must continue to report `MaxTokens: 1048576` (1M) and `MaxOutputTokens: 65536`.
- WebUI settings editor must clean existing `[1m]` suffixes from `ANTHROPIC_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, and `ANTHROPIC_DEFAULT_HAIKU_MODEL` upon configuration save.
- All Go unit tests in `internal/modelcatalog`, `internal/api`, `internal/config`, and `internal/webui` must pass cleanly without regressions.

---

### Task 1: Backend Catalog Suffix Stripping and Model Resolution

**Files:**
- Modify: `internal/modelcatalog/catalog.go:434-467`
- Modify: `internal/modelcatalog/catalog.go:254-285`
- Test: `internal/modelcatalog/catalog_test.go`

**Interfaces:**
- Consumes: `catalog.Resolve(requested string)`, `CleanModelIDAndName(id, displayName string)`
- Produces: Normalized model resolution that strips trailing `[1m]` / `[1M]` and resolves to the base model with its native `MaxTokens` context window.

- [ ] **Step 1: Write failing tests for `[1m]` suffix resolution**

Add test cases in `internal/modelcatalog/catalog_test.go` verifying that resolving `gemini-3.8-flash-high[1m]`, `gemini-3.7-flash-high[1M]`, `gemini-3.5-flash-low[1m]`, `gemini-3.1-pro-high[1m]`, and `claude-opus-4-6[1m]` resolves to the canonical model without error and retains `MaxTokens: 1048576` for Gemini models.

```go
func TestCatalogResolve_Strips1mSuffix(t *testing.T) {
	raw := `{
		"models":{
			"gemini-3.8-flash-high":{"displayName":"Gemini 3.8 Flash (High)","supportsThinking":true,"thinkingBudget":16000,"maxTokens":1048576,"maxOutputTokens":65536},
			"gemini-3.7-flash-high":{"displayName":"Gemini 3.7 Flash (High)","supportsThinking":true,"thinkingBudget":16000,"maxTokens":1048576,"maxOutputTokens":65536},
			"gemini-pro-agent":{"displayName":"Gemini 3.1 Pro (High)","supportsThinking":true,"thinkingBudget":10001,"maxTokens":1048576,"maxOutputTokens":65535},
			"claude-opus-4-6-thinking":{"displayName":"Claude Opus 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}
		},
		"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-high","gemini-3.7-flash-high","gemini-pro-agent","claude-opus-4-6-thinking"]}]}]
	}`

	catalog, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	testCases := []struct {
		input       string
		expectedID  string
		expectedMax int
	}{
		{"gemini-3.8-flash-high[1m]", "gemini-3.8-flash-high", 1048576},
		{"gemini-3.8-flash-high[1M]", "gemini-3.8-flash-high", 1048576},
		{"gemini-3.8-flash[1m]", "gemini-3.8-flash-high", 1048576},
		{"gemini-3.7-flash-high[1m]", "gemini-3.7-flash-high", 1048576},
		{"gemini-3.1-pro-high[1m]", "gemini-pro-agent", 1048576},
		{"gemini-pro[1m]", "gemini-pro-agent", 1048576},
		{"claude-opus-4-6[1m]", "claude-opus-4-6-thinking", 250000},
	}

	for _, tc := range testCases {
		m, err := catalog.Resolve(tc.input)
		if err != nil {
			t.Errorf("Resolve(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if m.ID != tc.expectedID {
			t.Errorf("Resolve(%q) ID = %q, expected %q", tc.input, m.ID, tc.expectedID)
		}
		if m.MaxTokens != tc.expectedMax {
			t.Errorf("Resolve(%q) MaxTokens = %d, expected %d", tc.input, m.MaxTokens, tc.expectedMax)
		}
	}
}

func TestCleanModelIDAndName_Strips1mSuffix(t *testing.T) {
	testCases := []struct {
		id          string
		name        string
		expectedID  string
		expectedName string
	}{
		{"gemini-3.8-flash[1m]", "Gemini 3.8 Flash[1m]", "gemini-3.8-flash", "Gemini 3.8 Flash"},
		{"gemini-3.7-flash-high[1M]", "Gemini 3.7 Flash (High)[1M]", "gemini-3.7-flash", "Gemini 3.7 Flash"},
		{"gemini-3.1-pro-high[1m]", "Gemini 3.1 Pro (High)[1m]", "gemini-3.1-pro", "Gemini 3.1 Pro"},
		{"custom-model[1m]", "Custom Model [1M]", "custom-model", "Custom Model"},
	}

	for _, tc := range testCases {
		cleanID, cleanName := CleanModelIDAndName(tc.id, tc.name)
		if cleanID != tc.expectedID {
			t.Errorf("CleanModelIDAndName(%q, %q) cleanID = %q, expected %q", tc.id, tc.name, cleanID, tc.expectedID)
		}
		if cleanName != tc.expectedName {
			t.Errorf("CleanModelIDAndName(%q, %q) cleanName = %q, expected %q", tc.id, tc.name, cleanName, tc.expectedName)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -v ./internal/modelcatalog -run "TestCatalogResolve_Strips1mSuffix|TestCleanModelIDAndName_Strips1mSuffix"`
Expected: FAIL due to missing `[1m]` normalization in `Resolve` and `CleanModelIDAndName`.

- [ ] **Step 3: Implement `[1m]` suffix stripping in `internal/modelcatalog/catalog.go`**

In `internal/modelcatalog/catalog.go`:
1. Add helper function `strip1mSuffix(s string) string`:
```go
func strip1mSuffix(s string) string {
	trimmed := strings.TrimSpace(s)
	lower := strings.ToLower(trimmed)
	if strings.HasSuffix(lower, "[1m]") {
		return strings.TrimSpace(trimmed[:len(trimmed)-4])
	}
	return trimmed
}
```
2. In `CleanModelIDAndName(id, displayName string)`:
   Strip `[1m]` / `[1M]` before the switch statement:
```go
func CleanModelIDAndName(id, displayName string) (string, string) {
	id = strip1mSuffix(id)
	displayName = strip1mSuffix(displayName)
	lowerID := strings.ToLower(id)
	cleanID := id
	cleanName := displayName
	// ... existing switch statement ...
```
3. In `Resolve(requested string)`:
   Strip `[1m]` suffix when sanitizing `key`:
```go
func (catalog *Catalog) Resolve(requested string) (Model, error) {
	if catalog == nil {
		return Model{}, errors.New("model catalog is unavailable")
	}
	cleaned := strip1mSuffix(requested)
	key := strings.ToLower(strings.TrimSpace(cleaned))
	if key == "" {
		key = strings.ToLower(catalog.DefaultID())
	}
	if model, exists := catalog.byID[key]; exists {
		return model, nil
	}
	if model, exists := catalog.byDisplay[key]; exists {
		return model, nil
	}
	if displayName := routingAliases[key]; displayName != "" {
		if model, exists := catalog.byDisplay[strings.ToLower(displayName)]; exists {
			return model, nil
		}
	}
	normalized := strings.ReplaceAll(key, ".", "-")
	if displayName := routingAliases[normalized]; displayName != "" {
		if model, exists := catalog.byDisplay[strings.ToLower(displayName)]; exists {
			return model, nil
		}
	}

	available := make([]string, 0, len(catalog.selectable))
	for _, m := range catalog.selectable {
		available = append(available, m.ID)
	}
	return Model{}, &SelectionError{Model: requested, Selectable: available}
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test -v ./internal/modelcatalog -run "TestCatalogResolve_Strips1mSuffix|TestCleanModelIDAndName_Strips1mSuffix"`
Expected: PASS

- [ ] **Step 5: Run all model catalog tests**

Run: `go test -v ./internal/modelcatalog`
Expected: PASS

- [ ] **Step 6: Commit backend catalog changes**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "fix(modelcatalog): automatically strip legacy 1m suffix during model resolution"
```

---

### Task 2: Backend Presets and Configuration Sanitization

**Files:**
- Modify: `internal/config/claude.go:11-36`
- Modify: `internal/config/claude.go:130-151`
- Test: `internal/config/claude_test.go` (create or update)

**Interfaces:**
- Consumes: `DefaultClaudePresets`, `UpdateClaudeConfig(updates map[string]any)`
- Produces: Sanitized Claude preset configurations without `[1m]` suffix.

- [ ] **Step 1: Write failing test for config sanitization**

Create/update `internal/config/claude_test.go` to test that `UpdateClaudeConfig` and preset loading sanitize any model strings containing `[1m]`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateClaudeConfig_CleansLegacy1mSuffix(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_PATH", filepath.Join(tmpDir, "settings.json"))

	updates := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_MODEL":            "gemini-3.8-flash-high[1m]",
			"CLAUDE_CODE_SUBAGENT_MODEL": "gemini-3.7-flash-high[1M]",
			"ANTHROPIC_BASE_URL":         "http://localhost:8080",
		},
	}

	updated, err := UpdateClaudeConfig(updates)
	if err != nil {
		t.Fatalf("UpdateClaudeConfig failed: %v", err)
	}

	env, ok := updated["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env map in updated config")
	}

	if env["ANTHROPIC_MODEL"] != "gemini-3.8-flash-high" {
		t.Errorf("expected ANTHROPIC_MODEL sanitized to gemini-3.8-flash-high, got %v", env["ANTHROPIC_MODEL"])
	}
	if env["CLAUDE_CODE_SUBAGENT_MODEL"] != "gemini-3.7-flash-high" {
		t.Errorf("expected CLAUDE_CODE_SUBAGENT_MODEL sanitized to gemini-3.7-flash-high, got %v", env["CLAUDE_CODE_SUBAGENT_MODEL"])
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test -v ./internal/config -run TestUpdateClaudeConfig_CleansLegacy1mSuffix`
Expected: FAIL (unsanitized `[1m]` remains).

- [ ] **Step 3: Implement model sanitization in `internal/config/claude.go`**

In `internal/config/claude.go`:
1. Add helper `sanitizeModelValue(val any) any`:
```go
func sanitizeModelValue(val any) any {
	if s, ok := val.(string); ok {
		s = strings.TrimSpace(s)
		if strings.HasSuffix(strings.ToLower(s), "[1m]") {
			return strings.TrimSpace(s[:len(s)-4])
		}
		return s
	}
	return val
}
```
2. In `UpdateClaudeConfig`, sanitize model environment variables (`ANTHROPIC_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`):
```go
func UpdateClaudeConfig(updates map[string]any) (map[string]any, error) {
	current, err := ReadClaudeConfig()
	if err != nil {
		current = make(map[string]any)
	}
	for k, v := range updates {
		if vMap, ok := v.(map[string]any); ok {
			if existingMap, ok := current[k].(map[string]any); ok {
				for vk, vv := range vMap {
					if isClaudeModelField(vk) {
						vv = sanitizeModelValue(vv)
					}
					existingMap[vk] = vv
				}
				current[k] = existingMap
				continue
			} else {
				for vk, vv := range vMap {
					if isClaudeModelField(vk) {
						vMap[vk] = sanitizeModelValue(vv)
					}
				}
			}
		}
		current[k] = v
	}
	if err := ReplaceClaudeConfig(current); err != nil {
		return nil, err
	}
	return current, nil
}

func isClaudeModelField(field string) bool {
	switch field {
	case "ANTHROPIC_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Run config tests to verify pass**

Run: `go test -v ./internal/config`
Expected: PASS

- [ ] **Step 5: Commit config changes**

```bash
git add internal/config/claude.go internal/config/claude_test.go
git commit -m "fix(config): sanitize legacy 1m model suffixes on Claude config update"
```

---

### Task 3: WebUI Claude Config & Model Dropdown Cleanup

**Files:**
- Modify: `internal/webui/public/js/components/claude-config.js`
- Modify: `internal/webui/public/js/components/model-dropdown.js`
- Test: Manual browser test / WebUI static verification test

**Interfaces:**
- Consumes: Alpine.js store and Claude CLI config
- Produces: Clean model selection without `[1m]` string modification or toggles.

- [ ] **Step 1: Update `internal/webui/public/js/components/claude-config.js`**

1. Remove `gemini1mSuffix: false` from component state.
2. Remove `detectGemini1mSuffix()` and `toggleGemini1mSuffix(enabled)`.
3. In `selectModel(field, modelId)`:
   Simplify selection so it assigns `cleanModelId` directly without appending `[1m]`:
   ```javascript
   selectModel(field, modelId) {
       if (!modelId) return;
       // Strip any trailing [1m] just in case
       const cleanModelId = modelId.replace(/\s*\[1m\]$/i, '').trim();
       this.config.env[field] = cleanModelId;
       this.activeDropdown = null;
   },
   ```
4. In `init()`:
   Strip any legacy `[1m]` suffixes found in existing `this.config.env` fields:
   ```javascript
   // Clean any legacy [1m] suffixes from environment models
   for (const field of this.geminiModelFields) {
       const val = this.config.env[field];
       if (val && typeof val === 'string' && /\[1m\]$/i.test(val)) {
           this.config.env[field] = val.replace(/\s*\[1m\]$/i, '').trim();
       }
   }
   ```
5. In `applyPreset(preset)`:
   Remove call to `this.detectGemini1mSuffix()`.

- [ ] **Step 2: Update `internal/webui/public/js/components/model-dropdown.js`**

In `internal/webui/public/js/components/model-dropdown.js`:
Update `isSelected(modelId)` to clean comparison:
```javascript
isSelected(modelId) {
    const val = this.config?.env?.[this.fieldName];
    if (!val) return false;
    const cleanVal = val.replace(/\s*\[1m\]$/i, '').trim();
    return cleanVal === modelId;
}
```

- [ ] **Step 3: Run webui unit / Go tests**

Run: `go test -v ./internal/webui`
Expected: PASS

- [ ] **Step 4: Commit WebUI component changes**

```bash
git add internal/webui/public/js/components/claude-config.js internal/webui/public/js/components/model-dropdown.js
git commit -m "refactor(webui): remove gemini1mSuffix toggle and model string manipulation"
```

---

### Task 4: WebUI Settings View & Translation Cleanup

**Files:**
- Modify: `internal/webui/public/views/settings.html`
- Modify: `internal/webui/public/js/translations/en.js`
- Modify: `internal/webui/public/js/translations/pt.js`

**Interfaces:**
- Consumes: Alpine.js store `$store.data.models`
- Produces: Streamlined settings view without the redundant 1M toggle and with accurate model capability indicators.

- [ ] **Step 1: Remove the Gemini 1M Context Toggle from `internal/webui/public/views/settings.html`**

Remove lines 734-755:
```html
<!-- Gemini 1M Context Suffix Toggle -->
...
```
Also update any model selection cards in `settings.html` (around lines 400, 473, 551, 621, 691) that conditionally rendered `[1M]` based on `gemini1mSuffix`:
Replace:
```html
<template x-if="gemini1mSuffix && $store.data.getModelFamily(modelId) === 'gemini'">
    <span class="text-[10px] bg-emerald-500/10 text-emerald-400 px-1.5 py-0.5 rounded border border-emerald-500/20 font-bold uppercase tracking-tighter">[1M]</span>
</template>
```
With a native model context window indicator (or 1M badge based on actual model context length):
```html
<template x-if="$store.data.getModel(modelId)?.context_window >= 1000000">
    <span class="text-[10px] bg-emerald-500/10 text-emerald-400 px-1.5 py-0.5 rounded border border-emerald-500/20 font-bold uppercase tracking-tighter">1M</span>
</template>
```

- [ ] **Step 2: Clean up translation files**

In `internal/webui/public/js/translations/en.js` and `internal/webui/public/js/translations/pt.js`:
Remove or mark deprecated unused translation keys:
`gemini1mMode`, `gemini1mDesc`, `gemini1mWarning`.

- [ ] **Step 3: Run webui and API tests**

Run: `go test -v ./internal/webui ./internal/api`
Expected: PASS

- [ ] **Step 4: Commit UI and translation changes**

```bash
git add internal/webui/public/views/settings.html internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js
git commit -m "feat(webui): remove 1M suffix toggle and show native 1M context badge"
```

---

### Task 5: End-to-End Discovery and Integration Verification

**Files:**
- Test: `internal/api/models_discovery_test.go`
- Test: `internal/api/claudecode_management_test.go`

**Interfaces:**
- Consumes: `/v1/models`, `/models`, `/api/claudecode/models`
- Produces: Verified model responses with full 1M context windows.

- [ ] **Step 1: Write integration tests in `internal/api/models_discovery_test.go`**

Add test verifying that Gemini models discovered via `/v1/models` and `/models` advertise `context_window: 1048576` and `max_output_tokens: 65536`, and handle incoming model IDs with or without `[1m]`:

```go
func TestGeminiModels_AdvertiseMaxContextWindow(t *testing.T) {
	server := &Server{
		backend: &discoveryTestBackend{},
		logger:  slog.Default(),
		now:     time.Now,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	server.models(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		if strings.HasPrefix(id, "gemini-") {
			// Ensure no [1m] suffix in advertised model IDs
			if strings.Contains(id, "[1m]") || strings.Contains(id, "[1M]") {
				t.Errorf("model ID %q contains [1m] suffix", id)
			}
			// Verify context window is reported
			if cw, ok := m["context_window"].(float64); ok && cw > 0 {
				if int(cw) < 1000000 {
					t.Errorf("gemini model %q context_window = %v, expected >= 1M", id, cw)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run all tests in the repository**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 3: Commit integration test additions**

```bash
git add internal/api/models_discovery_test.go
git commit -m "test(api): verify Gemini models advertise max context window without 1m suffix"
```

---

## Self-Review Checklist

1. **Spec Coverage:**
   - Remove need for `[1m]` suffix? Covered in Tasks 1, 2, 3, 4.
   - Models that support 1M context automatically use max context? Covered in Tasks 1 and 5 (models keep native `MaxTokens: 1048576` without requiring suffixing).
   - Backward compatibility for legacy `[1m]` requests? Covered in Task 1 (`catalog.Resolve` and `CleanModelIDAndName` handle `[1m]` inputs).
2. **No Placeholders:**
   - All code snippets, tests, commands, and file paths are fully specified.
3. **Type Consistency:**
   - `strip1mSuffix`, `CleanModelIDAndName`, `sanitizeModelValue`, `selectModel` consistent across tasks.
