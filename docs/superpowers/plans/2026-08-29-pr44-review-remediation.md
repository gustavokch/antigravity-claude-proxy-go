# PR 44 Review Findings Remediation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remediate nits identified during PR #44 code review: prevent redundant leading newlines when steering an empty system string in OutputShaper, and clone the injected RetrieveToolDefinition in CCR stage to protect against cross-request tool mutation.

**Architecture:**
1. `internal/headroom/stages/shaper/stage.go`: `applySteering` checks if `sys == ""` when `req["system"]` is string, setting `req["system"] = text` directly rather than prepending `\n\n`.
2. `internal/headroom/stages/ccr/stage.go`: `retrieveToolDefinition()` returns a fresh map / cloned structure when appending to `reqCtx.Request["tools"]`.

**Tech Stack:** Go 1.24+, `testing`.

---

## Task 1: Fix empty string system prompt steering in OutputShaper

- **Target Files:**
  - Modify: `internal/headroom/stages/shaper/stage.go`
  - Test: `internal/headroom/stages/shaper/shaper_test.go`
- **Consumes:** `req["system"]` string payload
- **Produces:** Clean system prompt without leading newlines when original was empty string

### Step 1: Write failing test
Add unit test to `internal/headroom/stages/shaper/shaper_test.go`:
```go
func TestOutputShaper_EmptyStringSystemPrompt(t *testing.T) {
	stage := NewStage()
	req := map[string]any{
		"system": "",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	cfg := &headroom.Config{
		OutputShaper: headroom.OutputShaperConfig{
			Enabled:           true,
			VerbositySteering: true,
			SteeringText:      "Custom Prompt",
		},
	}
	reqCtx := &headroom.RequestContext{Request: req}
	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sys, ok := req["system"].(string)
	if !ok {
		t.Fatalf("expected string system, got %T", req["system"])
	}
	if sys != "Custom Prompt" {
		t.Errorf("expected 'Custom Prompt', got %q", sys)
	}
}
```

### Step 2: Run test to confirm failure
```bash
go test -v ./internal/headroom/stages/shaper -run TestOutputShaper_EmptyStringSystemPrompt
```

### Step 3: Minimal implementation
In `internal/headroom/stages/shaper/stage.go`:
```go
func (s *OutputShaperStage) applySteering(req map[string]any, cfg *headroom.Config) {
	text := cfg.OutputShaper.SteeringText
	if text == "" {
		text = DefaultVerbosityPrompt
	}
	switch sys := req["system"].(type) {
	case string:
		if sys == "" {
			req["system"] = text
		} else {
			req["system"] = sys + "\n\n" + text
		}
	case []any:
		req["system"] = append(sys, map[string]any{"type": "text", "text": text})
	case nil:
		req["system"] = text
	}
}
```

### Step 4: Run test to confirm pass
```bash
go test -v ./internal/headroom/stages/shaper -run TestOutputShaper_EmptyStringSystemPrompt
```

### Step 5: Git commit
```bash
git add internal/headroom/stages/shaper/stage.go internal/headroom/stages/shaper/shaper_test.go
git commit -m "fix(headroom): avoid leading newlines when steering empty system string"
```

---

## Task 2: Clone injected RetrieveToolDefinition in CCR Stage

- **Target Files:**
  - Modify: `internal/headroom/stages/ccr/stage.go`
  - Test: `internal/headroom/stages/ccr/ccr_test.go`
- **Consumes:** `reqCtx.Request["tools"]`
- **Produces:** Cloned tool definition map appended to request tools list

### Step 1: Write failing test
Add unit test to `internal/headroom/stages/ccr/ccr_test.go`:
```go
func TestCCRStage_ToolDefinitionCloned(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewStage(store)
	req := map[string]any{
		"tools": []any{
			map[string]any{"name": "read_file"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	cfg := &headroom.Config{
		CCR: headroom.CCRConfig{Enabled: true},
	}
	reqCtx := &headroom.RequestContext{Request: req}
	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := req["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	injected, ok := tools[1].(map[string]any)
	if !ok {
		t.Fatalf("expected tool map, got %T", tools[1])
	}
	// Mutate injected tool in request and ensure package var RetrieveToolDefinition is unaffected.
	injected["name"] = "mutated_retrieve"
	if RetrieveToolDefinition["name"] == "mutated_retrieve" {
		t.Errorf("RetrieveToolDefinition was mutated by request mutation")
	}
}
```

### Step 2: Run test to confirm failure
```bash
go test -v ./internal/headroom/stages/ccr -run TestCCRStage_ToolDefinitionCloned
```

### Step 3: Minimal implementation
In `internal/headroom/stages/ccr/stage.go`:
Provide `cloneRetrieveToolDefinition()` and use it in `Execute`.

```go
func cloneRetrieveToolDefinition() map[string]any {
	return map[string]any{
		"name":        "headroom_retrieve",
		"description": "Retrieve the full content of a demoted context chunk by its chunk ID.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"chunk_id": map[string]any{
					"type":        "string",
					"description": "The chunk ID to retrieve, formatted as chunk_<hash>.",
				},
			},
			"required": []any{"chunk_id"},
		},
	}
}
```
And replace:
`reqCtx.Request["tools"] = append(tools, cloneRetrieveToolDefinition())`

### Step 4: Run test to confirm pass
```bash
go test -v ./internal/headroom/stages/ccr -run TestCCRStage_ToolDefinitionCloned
```

### Step 5: Git commit
```bash
git add internal/headroom/stages/ccr/stage.go internal/headroom/stages/ccr/ccr_test.go
git commit -m "fix(headroom): clone CCR tool definition to prevent shared map mutation"
```
