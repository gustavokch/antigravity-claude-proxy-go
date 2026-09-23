# Claude 5 Context Window (1M) & Max Output (128K) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Update Claude Code gateway configuration and model discovery to honor 1,000,000 (1M) token context window and 128,000 (128K) max_tokens output ceiling for `claude-opus-5`, `claude-sonnet-5`, and Fable series models (`claude-fable-5`, `claude-fable-5-1`).

**Architecture:** Update `DefaultAllowlist()` model definitions in `internal/claudecode/router.go` with `ContextLen: 1000000` and `MaxOutputTokens: 128000`. Ensure `DefaultClaudeCatalogue()` in `internal/claudecode/client.go` includes Fable 5.1. Update `/v1/models` discovery test expectations and `claudeCodeEntryMaxOutput` tests to verify 1M context window and 128K output limits are served and applied during request forwarding.

**Tech Stack:** Go 1.24, HTTP/JSON APIs, Claude Code gateway router, Alpine.js WebUI.

**Spec:** User requirements:
- Context Window: 1,000,000 (1M) tokens default for `opus-5`, `sonnet-5`, and Fable series models.
- Maximum Output (`max_tokens`): standard max output ceiling of 128K (128,000) tokens.

## Global Constraints

- Do not alter limits for older models (`claude-3-5-*`, `claude-3-7-sonnet`, `claude-3-*`, `haiku-4-5`).
- Ensure `applyMaxTokensPolicy` allows up to 128,000 output tokens for Opus 5, Sonnet 5, and Fable series without clamp down to 8192.
- Preserve backward compatibility and existing tests for other providers (Gemini, OpenRouter, Kimi).

---

### Task 1: Update Default Allowlist and Catalogue for Claude 5 & Fable Series

**Files:**
- Modify: `internal/claudecode/router.go:10-42`
- Modify: `internal/claudecode/client.go:85-107`
- Modify: `internal/modelcatalog/catalog.go:125-131`
- Test: `internal/claudecode/router_test.go`
- Test: `internal/claudecode/client_test.go`

**Interfaces:**
- Consumes: `claudecode.ModelConfig`, `claudecode.DiscoveredModel`
- Produces: `DefaultAllowlist()` returning 1,000,000 `ContextLen` and 128,000 `MaxOutputTokens` for `claude-fable-5`, `claude-fable-5-1`, `claude-opus-5`, `claude-sonnet-5`.

- [ ] **Step 1: Write failing unit test for DefaultAllowlist limits**

Add test in `internal/claudecode/router_test.go`:
```go
func TestDefaultAllowlist_Claude5Limits(t *testing.T) {
	models := DefaultAllowlist()
	byID := make(map[string]ModelConfig)
	for _, m := range models {
		byID[m.ID] = m
	}

	targets := []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-fable-5-1"}
	for _, id := range targets {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("expected model %s in DefaultAllowlist", id)
		}
		if m.ContextLen != 1000000 {
			t.Errorf("model %s ContextLen = %d, want 1000000", id, m.ContextLen)
		}
		if m.MaxOutputTokens != 128000 {
			t.Errorf("model %s MaxOutputTokens = %d, want 128000", id, m.MaxOutputTokens)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/claudecode -run TestDefaultAllowlist_Claude5Limits`
Expected: FAIL (ContextLen is 200000, MaxOutputTokens is 8192, and claude-fable-5-1 is missing).

- [ ] **Step 3: Update DefaultAllowlist, DefaultClaudeCatalogue, and routingAliases**

In `internal/claudecode/router.go`:
Update `claude-fable-5`, `claude-opus-5`, `claude-sonnet-5` to have `ContextLen: 1000000` and `MaxOutputTokens: 128000`.
Add `claude-fable-5-1`:
```go
		{
			ID:              "claude-fable-5-1",
			Alias:           "fable-5-1",
			Aliases:         []string{"fable-5-1", "claude-fable-5-1"},
			DisplayName:     "Claude Fable 5.1",
			ContextLen:      1000000,
			MaxOutputTokens: 128000,
			Thinking:        true,
			Enabled:         true,
		},
```

In `internal/claudecode/client.go`:
Add `claude-fable-5-1` to `DefaultClaudeCatalogue()`:
```go
		{
			ID:           "claude-fable-5-1",
			DisplayName:  "Claude Fable 5.1",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-fable-5-1", "fable-5-1", "claude-fable-5.1", "fable-5.1"},
			Family:       "fable",
		},
```

In `internal/modelcatalog/catalog.go`:
Add alias mapping for `claude-fable-5-1` and `fable-5-1`:
```go
	"claude-fable-5-1": "Claude Sonnet 4.6 (Thinking)",
	"fable-5-1":        "Claude Sonnet 4.6 (Thinking)",
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/claudecode`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/claudecode/router.go internal/claudecode/client.go internal/modelcatalog/catalog.go internal/claudecode/router_test.go
git commit -m "feat(claudecode): update Claude 5 and Fable models to 1M context and 128K max tokens"
```

---

### Task 2: Update Claude Code Gateway Max Output Policy & Discovery Tests

**Files:**
- Modify: `internal/api/claudecode_proxy_test.go:695-699`
- Modify: `internal/api/models_discovery_test.go:88-125`
- Test: `internal/api/claudecode_proxy_test.go`
- Test: `internal/api/models_discovery_test.go`

**Interfaces:**
- Consumes: `claudeCodeEntryMaxOutput(cfg, "claude-sonnet-5")`, `/v1/models` handler
- Produces: Correct fallback to 128000 for Claude 5 models and 1M context in `/v1/models` discovery

- [ ] **Step 1: Update TestClaudeCodeEntryMaxOutput test expectation**

In `internal/api/claudecode_proxy_test.go`:
Update test `empty allowlist falls back to defaults`:
```go
		t.Run("empty allowlist falls back to defaults", func(t *testing.T) {
			if got := claudeCodeEntryMaxOutput(claudecode.Config{}, "claude-sonnet-5"); got != 128000 {
				t.Errorf("got %d, want 128000 (default allowlist)", got)
			}
		})
```

- [ ] **Step 2: Add discovery assertion for 1M context and 128K max output**

In `internal/api/models_discovery_test.go`:
Add `"claude-fable-5-1", "fable-5-1"` to `expectedModels`.
Add assertions for context window and max output tokens:
```go
		targets := []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-fable-5-1"}
		for _, id := range targets {
			m := byID[id]
			if cw, _ := m["context_window"].(int); cw != 1000000 {
				t.Errorf("%s expected context_window 1000000 for %s, got %v", path, id, m["context_window"])
			}
			if mo, _ := m["max_output_tokens"].(int); mo != 128000 {
				t.Errorf("%s expected max_output_tokens 128000 for %s, got %v", path, id, m["max_output_tokens"])
			}
		}
```

- [ ] **Step 3: Run API tests to verify**

Run: `go test -v ./internal/api -run "TestClaudeCodeModelsDiscovery|TestClaudeCodeEntryMaxOutput"`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/api/claudecode_proxy_test.go internal/api/models_discovery_test.go
git commit -m "test(api): assert 1M context and 128K max tokens in models discovery and proxy tests"
```

---

### Task 3: Add Request Clamping and Passthrough Tests for 128K max_tokens

**Files:**
- Create/Modify: `internal/api/claudecode_proxy_test.go`
- Test: `internal/api/claudecode_proxy_test.go`

**Interfaces:**
- Consumes: `applyMaxTokensPolicy`
- Produces: Verified behavior that requests with `max_tokens: 128000` pass through unclamped for Claude 5 models

- [ ] **Step 1: Write unit test verifying max_tokens passthrough up to 128K**

Add test in `internal/api/claudecode_proxy_test.go`:
```go
func TestClaudeCodeForwarding_128KMaxTokens(t *testing.T) {
	cfg := claudecode.Config{}
	limit := claudeCodeEntryMaxOutput(cfg, "claude-opus-5")
	if limit != 128000 {
		t.Fatalf("expected limit 128000, got %d", limit)
	}

	body := []byte(`{"model":"claude-opus-5","max_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`)
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}

	out := applyMaxTokensPolicy(body, req, 0, limit)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}

	if mt, _ := parsed["max_tokens"].(float64); int(mt) != 128000 {
		t.Errorf("expected max_tokens 128000 preserved, got %v", parsed["max_tokens"])
	}
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test -v ./internal/api -run TestClaudeCodeForwarding_128KMaxTokens`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/api/claudecode_proxy_test.go
git commit -m "test(claudecode): verify 128K max_tokens passthrough policy"
```

---

### Task 4: Update WebUI Claude Code Defaults

**Files:**
- Modify: `internal/webui/public/js/components/models.js:890-905`
- Test: `internal/webui/translations_test.go`

**Interfaces:**
- Consumes: `importCCDefaults()`
- Produces: `claude-fable-5-1` in default UI list; 1M context / 128K max output representation.

- [ ] **Step 1: Update importCCDefaults in models.js**

In `internal/webui/public/js/components/models.js`:
Add `claude-fable-5-1`:
```javascript
            { id: 'claude-fable-5-1', displayName: 'Claude Fable 5.1', alias: 'claude-fable-5-1, fable-5-1, claude-fable-5.1', enabled: true },
```

- [ ] **Step 2: Run full test suite to ensure clean integration**

Run: `go test ./...`
Expected: ALL PASS

- [ ] **Step 3: Commit**

```bash
git add internal/webui/public/js/components/models.js
git commit -m "feat(webui): add claude-fable-5-1 to Claude Code UI defaults"
```

---

## Self-Review Checklist

1. **Spec coverage:**
   - 1,000,000 (1M) default context window covered for Opus 5, Sonnet 5, Fable 5, Fable 5.1? Yes, updated in `DefaultAllowlist()` and verified in discovery.
   - 128K (128,000) max_tokens ceiling honored? Yes, updated in `DefaultAllowlist()`, verified via `claudeCodeEntryMaxOutput` and `applyMaxTokensPolicy`.
2. **Placeholder scan:** None. Exact file paths, line ranges, and runnable test code provided.
3. **Type consistency:** Go types `ModelConfig`, `DiscoveredModel`, integer limits `1000000` and `128000` match throughout.
