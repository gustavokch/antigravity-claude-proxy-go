# Headroom Native Context Compression & Shaping Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a native Go context compression, prompt-cache-stable shaping, content-conditioned retrieval (CCR), and output shaping engine (Headroom) integrated into `antigravity-claude-proxy-go` across all providers (Cloud Code, OpenRouter, Kimi, Custom Endpoints).

**Architecture:** A modular pipeline (`internal/headroom`) runs inside `Server.messages` after model mapping and **before every provider dispatch marshal**. It mutates the decoded Anthropic request map in place. `SmartCrusher` and `CodeCompressor` shrink `tool_result` payloads via *pure, deterministic, position-independent* transforms. `OutputShaper` appends steering text to the system prompt tail and clamps mechanical-turn thinking budgets. Phase 2 adds `CCR`: an LRU chunk store that demotes older oversized `tool_result` payloads to `[HEADROOM_CHUNK …]` references, injects a `headroom_retrieve` tool, and hydrates retrieval calls transparently on the two provider paths the proxy actually owns.

**Tech Stack:** Go 1.27 (`go.mod` says `go 1.27rc2`), `net/http`, `encoding/json`, `sync`, vanilla HTML/JS WebUI (`internal/webui/public`).

**Spec:** `docs/superpowers/specs/2026-08-27-headroom-integration-design.md`

---

## Design Invariants

These are the rules every stage must obey. They are the reason this plan differs structurally from the spec's stage ordering; **read them before writing code.**

### I1 — Cache stability comes from determinism, not from a "live zone"

A provider prompt cache hits only when the request prefix is **byte-identical** to the previous request's prefix. Therefore:

> Every transform must be a pure function of the *content of the block it rewrites*, independent of that block's position in the conversation and independent of the turn number.

A transform that only rewrites "the newest message" is **cache-hostile**: message `m5` is sent compressed on turn 1 (while it is newest) and raw on turn 2 (once it is history), so the prefix diverges at `m5` and every turn after the first is a full cache miss — the exact opposite of the stated goal.

Consequences:
- `SmartCrusher` and `CodeCompressor` apply to **all** `tool_result` blocks in the request, history included. This is both cache-safe *and* where the token savings actually are.
- Enabling, disabling, or retuning Headroom mid-conversation causes exactly one cache miss. Document this in the WebUI; do not try to avoid it.
- Every transform must be **idempotent**: `f(f(x)) == f(x)`. Tested explicitly in Task 6.

### I2 — CCR trades cache stability for context reduction, on purpose

CCR demotion is position-dependent by nature (a payload is only safe to demote once the model no longer needs it inline), so it *does* move the prefix each turn, invalidating the cache from the demotion boundary onward. That cost is bounded by the size of the last `liveTurns` turns. This is an explicit trade, which is why CCR is a separate phase, separately toggleable, and **off by default**.

### I3 — Only `tool_result` content is rewritten

Never touch:
- top-level user message strings (that is the human's literal prompt),
- assistant `text`, `thinking`, or `signature` blocks (signatures are verified upstream — see `internal/format/signature_cache.go`),
- `tool_use.input` (rewriting it would desync the client's tool call record).

### I4 — Never invent request fields

`OutputShaper` may modify `thinking.budget_tokens` / `reasoning.effort` **only when the client already sent them**. CCR may append to `tools` **only when the client already sent a `tools` array** — materialising a tool list where the client sent none makes the model emit `tool_use` blocks the client cannot service.

### I5 — Off by default

`headroom.enabled` defaults to `false`. This proxy rewrites other people's requests; opt-in is mandatory.

---

## Global Constraints
- Zero external Python/Cgo dependencies; 100% native Go, stdlib only.
- Thread-safe: pipeline stages are stateless; the CCR store and stats counters are mutex-guarded.
- Test-driven development for every component.
- Run tests with `go test ./internal/<pkg>/ -run <TestName>`. **Never** `go test ./path/to/file_test.go` — that compiles a single file without the rest of the package and fails to build.
- A Go toolchain is not installed in the authoring environment; the implementing session must have one.

---

## Phase 1 — Deterministic compression & output shaping

### Task 1: Headroom Configuration & Core Pipeline Types

**Files:**
- Create: `internal/headroom/types.go`
- Create: `internal/headroom/types_test.go`
- Modify: `internal/config/config.go` (`Config` struct ~line 90, `DefaultConfig` ~line 125)
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `headroom.Config`, `headroom.RequestContext`, `headroom.Stage`, `headroom.Pipeline`, `config.HeadroomConfig`

**Design notes:**
- `config.HeadroomConfig` is a **type alias** for `headroom.Config` (`type HeadroomConfig = headroom.Config`). `internal/headroom` imports only stdlib, so `config` → `headroom` introduces no cycle, and the alias removes the two-name ambiguity the rest of the plan depends on.
- JSON keys are **camelCase** to match this repo (`apiKey`, `maxRetries`, `openrouter`). The spec's snake_case sample in §4.1 is wrong for this codebase — update the spec as part of Task 10.
- `RequestContext` holds a **single** `Request` map, mutated in place. There is no separate "original" copy: the previous plan aliased both fields to the same map, so byte accounting was impossible and any retry path silently reused mutated data. Byte accounting is instead accumulated per rewritten block.

- [ ] **Step 1: Write failing tests for pipeline execution, gating, and config defaults**

```go
// internal/headroom/types_test.go
package headroom

import (
	"context"
	"errors"
	"testing"
)

type mockStage struct {
	name string
	runs int
	err  error
}

func (m *mockStage) Name() string { return m.name }
func (m *mockStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	m.runs++
	return m.err
}

func TestPipeline_RunsAllStagesInOrder(t *testing.T) {
	s1 := &mockStage{name: "stage1"}
	s2 := &mockStage{name: "stage2"}
	p := NewPipeline(s1, s2)

	reqCtx := &RequestContext{Request: map[string]any{"model": "claude-3-5-sonnet"}}

	if err := p.Run(context.Background(), reqCtx, &Config{Enabled: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s1.runs != 1 || s2.runs != 1 {
		t.Errorf("expected each stage to run once, got s1=%d s2=%d", s1.runs, s2.runs)
	}
}

func TestPipeline_SkipsEverythingWhenDisabled(t *testing.T) {
	s1 := &mockStage{name: "stage1"}
	p := NewPipeline(s1)

	if err := p.Run(context.Background(), &RequestContext{}, &Config{Enabled: false}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s1.runs != 0 {
		t.Errorf("expected no stages to run when disabled, got %d runs", s1.runs)
	}
}

func TestPipeline_StopsOnStageError(t *testing.T) {
	boom := errors.New("boom")
	s1 := &mockStage{name: "stage1", err: boom}
	s2 := &mockStage{name: "stage2"}
	p := NewPipeline(s1, s2)

	err := p.Run(context.Background(), &RequestContext{}, &Config{Enabled: true})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if s2.runs != 0 {
		t.Errorf("expected stage2 to be skipped after error")
	}
}
```

```go
// internal/config/config_test.go (add)
func TestDefaultConfig_HeadroomDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Headroom.Enabled {
		t.Error("headroom must default to disabled")
	}
	if cfg.Headroom.LiveTurns != 2 {
		t.Errorf("expected LiveTurns default 2, got %d", cfg.Headroom.LiveTurns)
	}
	if cfg.Headroom.CCR.MaxStoreMB != 64 || cfg.Headroom.CCR.MinChunkBytes != 2048 {
		t.Errorf("unexpected CCR defaults: %+v", cfg.Headroom.CCR)
	}
	if cfg.Headroom.OutputShaper.MechanicalThinkingBudget != 1024 {
		t.Errorf("unexpected shaper default: %+v", cfg.Headroom.OutputShaper)
	}
}

func TestSave_HeadroomRoundTrip(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	if _, err := Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	updated, err := Save(map[string]any{"headroom": map[string]any{
		"enabled":        true,
		"smartCrusher":   true,
		"codeCompressor": false,
	}})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !updated.Headroom.Enabled || !updated.Headroom.SmartCrusher || updated.Headroom.CodeCompressor {
		t.Errorf("unexpected persisted headroom config: %+v", updated.Headroom)
	}
}
```

> Note the replace semantics `TestSave_HeadroomRoundTrip` pins down: `config.Save` assigns the `headroom` subtree wholesale, so the WebUI must POST the **complete** `headroom` object (it already has it from `GET /api/config`). Task 9 depends on this.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/headroom/ ./internal/config/`
Expected: FAIL (package `headroom` does not exist; `Config.Headroom` undefined)

- [ ] **Step 3: Implement config structures and pipeline**

```go
// internal/headroom/types.go
package headroom

import "context"

// CCRConfig controls Content-Conditioned Retrieval (Phase 2).
type CCRConfig struct {
	Enabled       bool `json:"enabled"`
	MaxStoreMB    int  `json:"maxStoreMB,omitempty"`
	MinChunkBytes int  `json:"minChunkBytes,omitempty"`
}

// OutputShaperConfig controls verbosity steering and effort routing.
type OutputShaperConfig struct {
	Enabled                  bool   `json:"enabled"`
	VerbositySteering        bool   `json:"verbositySteering,omitempty"`
	SteeringText             string `json:"steeringText,omitempty"` // empty = DefaultVerbosityPrompt
	EffortRouting            bool   `json:"effortRouting,omitempty"`
	MechanicalThinkingBudget int    `json:"mechanicalThinkingBudget,omitempty"`
}

type Config struct {
	Enabled        bool               `json:"enabled"`
	SmartCrusher   bool               `json:"smartCrusher,omitempty"`
	CodeCompressor bool               `json:"codeCompressor,omitempty"`
	// LiveTurns is the number of trailing messages CCR leaves untouched.
	// It has no effect on SmartCrusher/CodeCompressor, which are position
	// independent by design (see invariant I1).
	LiveTurns    int                `json:"liveTurns,omitempty"`
	CCR          CCRConfig          `json:"ccr,omitempty"`
	OutputShaper OutputShaperConfig `json:"outputShaper,omitempty"`
}

// RequestContext carries the in-flight request and pipeline telemetry.
// Request is the caller's decoded Anthropic request map and is mutated in
// place; callers must run the pipeline before marshalling for any provider.
type RequestContext struct {
	Request map[string]any

	// FrozenPrefixIndex is the highest message index CCR is allowed to demote.
	// Messages with index > FrozenPrefixIndex are the live turns and stay
	// inline. -1 means "everything is live".
	FrozenPrefixIndex int

	// Byte accounting over rewritten blocks only (not whole-request sizes).
	BytesBefore int
	BytesAfter  int

	// OutputShaper telemetry.
	EffortClamped   bool
	OriginalThinking int
	ClampedThinking  int

	// CCR telemetry (Phase 2).
	ChunksStored int
}

// RecordRewrite accumulates byte accounting for one rewritten block.
func (r *RequestContext) RecordRewrite(before, after string) {
	r.BytesBefore += len(before)
	r.BytesAfter += len(after)
}

type Stage interface {
	Name() string
	Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error
}

type Pipeline struct {
	stages []Stage
}

func NewPipeline(stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages}
}

func (p *Pipeline) Run(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	for _, stage := range p.stages {
		if err := stage.Execute(ctx, reqCtx, cfg); err != nil {
			return err
		}
	}
	return nil
}
```

In `internal/config/config.go`:
1. `import "antigravity-go-proxy/internal/headroom"`.
2. Add `type HeadroomConfig = headroom.Config` next to the other config types.
3. Add `Headroom HeadroomConfig \`json:"headroom,omitempty"\`` to `Config`.
4. Add the defaults block to `DefaultConfig()` (enabled `false`, `LiveTurns: 2`, `CCR{Enabled:false, MaxStoreMB:64, MinChunkBytes:2048}`, `OutputShaper{Enabled:false, VerbositySteering:true, EffortRouting:true, MechanicalThinkingBudget:1024}`).

`GetPublicConfig()` marshals the whole `Config`, so `headroom` is exposed to the WebUI with no further change. There are no secrets in this subtree, so no redaction is needed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/ ./internal/config/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/types.go internal/headroom/types_test.go internal/config/config.go internal/config/config_test.go
git commit -m "feat(headroom): define core types, pipeline interface, and configuration"
```

---

### Task 2: Shared tool_result walker

**Files:**
- Create: `internal/headroom/walk.go`
- Create: `internal/headroom/walk_test.go`

**Interfaces:**
- Produces: `headroom.walkToolResults(req map[string]any, from int, fn func(idx int, block map[string]any))`

Every compressor needs the same traversal, and invariant I3 says every compressor must be careful about *which* blocks it touches. Centralise it once so the rule is enforced in one place.

Anthropic `tool_result.content` is either a string or an array of content blocks (`{"type":"text","text":…}`). The walker must handle both and must skip image blocks.

- [ ] **Step 1: Write failing tests for the walker**

```go
// internal/headroom/walk_test.go
package headroom

import "testing"

func TestWalkToolResults_VisitsStringAndArrayForms(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "raw user text"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "assistant prose"},
			map[string]any{"type": "thinking", "thinking": "private", "signature": "sig"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": "string form"},
			map[string]any{"type": "tool_result", "content": []any{
				map[string]any{"type": "text", "text": "array form"},
				map[string]any{"type": "image", "source": map[string]any{"data": "b64"}},
			}},
		}},
	}}

	var seen []string
	walkToolResultText(req, 0, func(idx int, get func() string, set func(string)) {
		seen = append(seen, get())
		set(get() + "!")
	})

	if len(seen) != 2 || seen[0] != "string form" || seen[1] != "array form" {
		t.Fatalf("unexpected visits: %#v", seen)
	}

	msgs := req["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "raw user text" {
		t.Error("must not touch top-level user text (invariant I3)")
	}
	assistant := msgs[1].(map[string]any)["content"].([]any)
	if assistant[0].(map[string]any)["text"] != "assistant prose" {
		t.Error("must not touch assistant text (invariant I3)")
	}
	if assistant[1].(map[string]any)["signature"] != "sig" {
		t.Error("must not touch thinking signatures (invariant I3)")
	}
	blocks := msgs[2].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["content"] != "string form!" {
		t.Error("string-form rewrite not applied")
	}
	inner := blocks[1].(map[string]any)["content"].([]any)
	if inner[0].(map[string]any)["text"] != "array form!" {
		t.Error("array-form rewrite not applied")
	}
	if _, ok := inner[1].(map[string]any)["text"]; ok {
		t.Error("image block must be left alone")
	}
}

func TestWalkToolResults_RespectsFromIndex(t *testing.T) {
	mk := func(s string) any {
		return map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": s},
		}}
	}
	req := map[string]any{"messages": []any{mk("a"), mk("b"), mk("c")}}

	var seen []string
	walkToolResultText(req, 2, func(idx int, get func() string, set func(string)) {
		seen = append(seen, get())
	})
	if len(seen) != 1 || seen[0] != "c" {
		t.Fatalf("expected only index 2, got %#v", seen)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/headroom/ -run TestWalkToolResults`
Expected: FAIL

- [ ] **Step 3: Implement the walker**

```go
// internal/headroom/walk.go
package headroom

// walkToolResultText visits every text payload inside every tool_result block
// of every message at index >= from, in document order. Per invariant I3 it
// never visits user prompt text, assistant text, thinking blocks, signatures,
// tool_use inputs, or images.
//
// get returns the current text; set replaces it. Both are closures over the
// underlying map so callers do not need to know which content shape they are in.
func walkToolResultText(req map[string]any, from int, fn func(idx int, get func() string, set func(string))) {
	messages, ok := req["messages"].([]any)
	if !ok {
		return
	}
	if from < 0 {
		from = 0
	}
	for i := from; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				continue
			}
			switch payload := block["content"].(type) {
			case string:
				b := block
				fn(i, func() string { return b["content"].(string) },
					func(s string) { b["content"] = s })
			case []any:
				for _, innerRaw := range payload {
					inner, ok := innerRaw.(map[string]any)
					if !ok || inner["type"] != "text" {
						continue
					}
					if _, ok := inner["text"].(string); !ok {
						continue
					}
					b := inner
					fn(i, func() string { return b["text"].(string) },
						func(s string) { b["text"] = s })
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/walk.go internal/headroom/walk_test.go
git commit -m "feat(headroom): add shared tool_result text walker"
```

---

### Task 3: SmartCrusher (JSON compactor)

**Files:**
- Create: `internal/headroom/smart_crusher.go`
- Create: `internal/headroom/smart_crusher_test.go`

**Interfaces:**
- Produces: `headroom.SmartCrusherStage`, `headroom.CompactJSON(input string) (string, bool)`

**Design notes:**
- Use `json.Compact`, which strips insignificant whitespace and **preserves key order and number literals verbatim**. Do *not* unmarshal-then-remarshal: that reorders keys (map iteration is sorted, but the source order is lost either way) and rewrites numbers through `float64`, corrupting large integers and trailing-zero decimals. The previous revision of this plan asserted reordered output (`{"items":…,"name":…}` from input `{"name":…,"items":…}`), which `json.Compact` does not and will not produce — that test could never pass.
- Compare against the **trimmed** input so a payload that is already compact but surrounded by whitespace still wins.
- Spec §3.2.3 (uniform object arrays → pipe/tab-delimited tables) is **deliberately out of scope**. It is lossy at the schema level, it makes the payload no longer valid JSON for a model that was told it is JSON, and its ">30% savings" gate makes the transform content-dependent in a way that is fine for I1 but hard to reason about. Track it as a follow-up; the spec should be amended in Task 10.

- [ ] **Step 1: Write failing tests for SmartCrusher**

```go
// internal/headroom/smart_crusher_test.go
package headroom

import (
	"context"
	"testing"
)

func TestCompactJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{
			name:    "pretty object compacts and preserves key order",
			in:      "{\n  \"name\": \"example\",\n  \"items\": [\n    {\"id\": 1, \"value\": \"a\"}\n  ]\n}",
			want:    `{"name":"example","items":[{"id":1,"value":"a"}]}`,
			changed: true,
		},
		{
			name:    "preserves large integer literals exactly",
			in:      "{\n  \"id\": 12345678901234567890\n}",
			want:    `{"id":12345678901234567890}`,
			changed: true,
		},
		{
			name:    "non-JSON passes through untouched",
			in:      "total 12\ndrwxr-xr-x  2 user user 4096 file",
			want:    "total 12\ndrwxr-xr-x  2 user user 4096 file",
			changed: false,
		},
		{
			name:    "malformed JSON passes through untouched",
			in:      `{"unterminated": `,
			want:    `{"unterminated": `,
			changed: false,
		},
		{
			name:    "already compact is not rewritten",
			in:      `{"a":1}`,
			want:    `{"a":1}`,
			changed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := CompactJSON(tc.in)
			if got != tc.want || changed != tc.changed {
				t.Errorf("CompactJSON(%q) = (%q, %v), want (%q, %v)", tc.in, got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestCompactJSON_Idempotent(t *testing.T) {
	in := "{\n  \"a\": [1, 2, 3]\n}"
	once, _ := CompactJSON(in)
	twice, changed := CompactJSON(once)
	if twice != once || changed {
		t.Errorf("not idempotent: %q -> %q (changed=%v)", once, twice, changed)
	}
}

func TestSmartCrusher_CompactsHistoryNotJustLastTurn(t *testing.T) {
	pretty := "{\n  \"ok\": true\n}"
	mk := func() any {
		return map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": pretty},
		}}
	}
	req := map[string]any{"messages": []any{mk(), mk(), mk()}}
	reqCtx := &RequestContext{Request: req, FrozenPrefixIndex: 0}

	stage := &SmartCrusherStage{}
	if err := stage.Execute(context.Background(), reqCtx, &Config{Enabled: true, SmartCrusher: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i, raw := range req["messages"].([]any) {
		blocks := raw.(map[string]any)["content"].([]any)
		got := blocks[0].(map[string]any)["content"].(string)
		if got != `{"ok":true}` {
			t.Errorf("message %d not compacted: %q", i, got)
		}
	}
	if reqCtx.BytesAfter >= reqCtx.BytesBefore || reqCtx.BytesBefore == 0 {
		t.Errorf("byte accounting not recorded: before=%d after=%d", reqCtx.BytesBefore, reqCtx.BytesAfter)
	}
}

func TestSmartCrusher_DisabledIsNoOp(t *testing.T) {
	pretty := "{\n  \"ok\": true\n}"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": pretty},
		}},
	}}
	stage := &SmartCrusherStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, &Config{Enabled: true, SmartCrusher: false}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != pretty {
		t.Errorf("expected no-op when disabled, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/headroom/ -run 'TestCompactJSON|TestSmartCrusher'`
Expected: FAIL

- [ ] **Step 3: Implement SmartCrusherStage**

```go
// internal/headroom/smart_crusher.go
package headroom

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

type SmartCrusherStage struct{}

func (s *SmartCrusherStage) Name() string { return "smart_crusher" }

// CompactJSON strips insignificant whitespace from a JSON payload. It uses
// json.Compact rather than an unmarshal/marshal round trip so key order and
// numeric literals survive byte-for-byte: both matter for prompt cache
// stability (invariant I1) and for not corrupting large integer IDs.
func CompactJSON(input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return input, false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		return input, false
	}
	compacted := buf.String()
	if len(compacted) < len(input) {
		return compacted, true
	}
	return input, false
}

func (s *SmartCrusherStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.SmartCrusher {
		return nil
	}
	// from=0: history included. Position independence is what keeps the
	// provider prompt cache warm across turns (invariant I1).
	walkToolResultText(reqCtx.Request, 0, func(_ int, get func() string, set func(string)) {
		before := get()
		if after, changed := CompactJSON(before); changed {
			set(after)
			reqCtx.RecordRewrite(before, after)
		}
	})
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/smart_crusher.go internal/headroom/smart_crusher_test.go
git commit -m "feat(headroom): implement SmartCrusher JSON compaction"
```

---

### Task 4: CodeCompressor (whitespace & repetition pruning)

**Files:**
- Create: `internal/headroom/code_compressor.go`
- Create: `internal/headroom/code_compressor_test.go`

**Interfaces:**
- Produces: `headroom.CodeCompressorStage`, `headroom.PruneText(input string) string`

**Design notes:**
- Trim **trailing** whitespace only. Spec §3.3.2 says "trims leading and trailing whitespace … while preserving indentation syntax", which is self-contradictory: leading whitespace *is* the indentation. Trailing-only is the resolution; the spec should be amended in Task 10.
- Collapse runs of 3+ newlines to 2 (i.e. 2+ blank lines become 1).
- Implement spec §3.3.3 repetition collapsing: 3+ consecutive identical non-empty lines become one line plus `[... repeated N times ...]`. Identical-line runs are the common case (progress ticks, repeated stack frames) and are cheap and deterministic. Fuzzy pattern matching is out of scope.
- `PruneText` must be idempotent. The repetition marker line is never itself a repeat candidate because a collapsed run is emitted once, and re-running finds no 3+ run.

- [ ] **Step 1: Write failing tests for CodeCompressor**

```go
// internal/headroom/code_compressor_test.go
package headroom

import (
	"context"
	"strings"
	"testing"
)

func TestPruneText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{
			name: "trims trailing whitespace, keeps indentation",
			in:   "func main() {   \n\tprintln(1)\t\n}   ",
			want: "func main() {\n\tprintln(1)\n}",
		},
		{
			name: "collapses multiple blank lines to one",
			in:   "line 1\n\n\n\nline 2\n\nline 3",
			want: "line 1\n\nline 2\n\nline 3",
		},
		{
			name: "collapses repeated identical lines",
			in:   "start\ntick\ntick\ntick\ntick\nend",
			want: "start\ntick\n[... repeated 3 times ...]\nend",
		},
		{
			name: "leaves a two-line run alone",
			in:   "start\ntick\ntick\nend",
			want: "start\ntick\ntick\nend",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PruneText(tc.in); got != tc.want {
				t.Errorf("PruneText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPruneText_Idempotent(t *testing.T) {
	in := "a   \n\n\n\nb\nb\nb\nb\nb\nc"
	once := PruneText(in)
	if twice := PruneText(once); twice != once {
		t.Errorf("not idempotent: %q -> %q", once, twice)
	}
}

func TestCodeCompressor_OnlyTouchesToolResults(t *testing.T) {
	userText := "please run   \n\n\n\nthe thing"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": userText},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": "out   \n\n\n\nput"},
		}},
	}}
	reqCtx := &RequestContext{Request: req}

	stage := &CodeCompressorStage{}
	if err := stage.Execute(context.Background(), reqCtx, &Config{Enabled: true, CodeCompressor: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := req["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != userText {
		t.Error("user prompt text must not be rewritten (invariant I3)")
	}
	got := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != "out\n\nput" {
		t.Errorf("tool_result not pruned, got %q", got)
	}
}

func TestCodeCompressor_LargeLogCollapses(t *testing.T) {
	log := "building\n" + strings.Repeat("  downloading...\n", 500) + "done\n"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": log},
		}},
	}}
	reqCtx := &RequestContext{Request: req}

	stage := &CodeCompressorStage{}
	if err := stage.Execute(context.Background(), reqCtx, &Config{Enabled: true, CodeCompressor: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, "[... repeated 499 times ...]") {
		t.Errorf("expected repetition marker, got %q", got)
	}
	if len(got) > 200 {
		t.Errorf("expected large collapse, got %d bytes", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/headroom/ -run 'TestPruneText|TestCodeCompressor'`
Expected: FAIL

- [ ] **Step 3: Implement CodeCompressorStage**

```go
// internal/headroom/code_compressor.go
package headroom

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var multipleBlankLinesRegex = regexp.MustCompile(`\n{3,}`)

// repeatThreshold is the run length at which identical consecutive lines are
// collapsed. Below this a run is cheaper to keep than to annotate.
const repeatThreshold = 3

type CodeCompressorStage struct{}

func (s *CodeCompressorStage) Name() string { return "code_compressor" }

// PruneText removes trailing whitespace, collapses blank-line runs, and folds
// runs of identical lines. Leading whitespace is preserved: it is the
// indentation. The function is pure and idempotent (invariant I1).
func PruneText(input string) string {
	lines := strings.Split(input, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}

	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		line := lines[i]
		run := 1
		for i+run < len(lines) && lines[i+run] == line {
			run++
		}
		out = append(out, line)
		if line != "" && run >= repeatThreshold {
			out = append(out, fmt.Sprintf("[... repeated %d times ...]", run-1))
		} else {
			for j := 1; j < run; j++ {
				out = append(out, line)
			}
		}
		i += run
	}

	return multipleBlankLinesRegex.ReplaceAllString(strings.Join(out, "\n"), "\n\n")
}

func (s *CodeCompressorStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.CodeCompressor {
		return nil
	}
	walkToolResultText(reqCtx.Request, 0, func(_ int, get func() string, set func(string)) {
		before := get()
		after := PruneText(before)
		if after != before {
			set(after)
			reqCtx.RecordRewrite(before, after)
		}
	})
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/code_compressor.go internal/headroom/code_compressor_test.go
git commit -m "feat(headroom): implement CodeCompressor whitespace and repetition pruning"
```

---

### Task 5: OutputShaper (verbosity steering & effort routing)

**Files:**
- Create: `internal/headroom/output_shaper.go`
- Create: `internal/headroom/output_shaper_test.go`

**Interfaces:**
- Produces: `headroom.OutputShaperStage`, `headroom.DefaultVerbosityPrompt`

**Design notes:**
- Steering text is appended unconditionally when enabled, so it is stable across turns and costs exactly one cache invalidation at toggle time.
- Effort routing only fires on a **mechanical continuation**: the last message is a `user` message whose content blocks are *all* `tool_result` and *none* carries `is_error: true`. A failed tool is precisely when the model needs to think — spec §3.5 says "User Query/Error vs Mechanical Tool Continuation" and this is the operative reading.
- Per invariant I4, only clamp knobs the client already sent. Additionally respect the Anthropic constraints: only clamp when `thinking.type == "enabled"`, never clamp below 1024, and never clamp to a value `>= max_tokens`.
- Write the clamped budget back as `float64` so the map stays homogeneous with the rest of the JSON-decoded request.
- Also handle OpenAI-shaped `reasoning.effort` / `reasoning_effort` (spec §3.5), demoting to `"low"` only when currently `"high"`.

- [ ] **Step 1: Write failing tests for OutputShaper**

```go
// internal/headroom/output_shaper_test.go
package headroom

import (
	"context"
	"strings"
	"testing"
)

func shaperCfg() *Config {
	return &Config{Enabled: true, OutputShaper: OutputShaperConfig{
		Enabled: true, VerbositySteering: true, EffortRouting: true,
		MechanicalThinkingBudget: 1024,
	}}
}

func TestOutputShaper_AppendsSteeringToStringSystem(t *testing.T) {
	req := map[string]any{"system": "You are a helpful assistant."}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	system := req["system"].(string)
	if !strings.HasPrefix(system, "You are a helpful assistant.") {
		t.Error("original system prompt must be preserved as the prefix")
	}
	if !strings.Contains(system, DefaultVerbosityPrompt) {
		t.Errorf("steering text missing: %q", system)
	}
}

func TestOutputShaper_AppendsBlockToArraySystem(t *testing.T) {
	req := map[string]any{"system": []any{
		map[string]any{"type": "text", "text": "base", "cache_control": map[string]any{"type": "ephemeral"}},
	}}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := req["system"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks, got %d", len(blocks))
	}
	first := blocks[0].(map[string]any)
	if first["text"] != "base" || first["cache_control"] == nil {
		t.Error("existing cached system block must be untouched")
	}
}

func TestOutputShaper_UsesCustomSteeringText(t *testing.T) {
	cfg := shaperCfg()
	cfg.OutputShaper.SteeringText = "Be terse."
	req := map[string]any{"system": "base"}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(req["system"].(string), "Be terse.") {
		t.Errorf("custom steering text not applied: %q", req["system"])
	}
}

func toolContinuation(isError bool) map[string]any {
	block := map[string]any{"type": "tool_result", "content": "ok"}
	if isError {
		block["is_error"] = true
	}
	return map[string]any{"role": "user", "content": []any{block}}
}

func TestOutputShaper_ClampsThinkingOnMechanicalTurn(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{toolContinuation(false)},
	}
	reqCtx := &RequestContext{Request: req}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["thinking"].(map[string]any)["budget_tokens"]
	if got != float64(1024) {
		t.Errorf("expected clamp to 1024, got %v", got)
	}
	if !reqCtx.EffortClamped || reqCtx.OriginalThinking != 16000 || reqCtx.ClampedThinking != 1024 {
		t.Errorf("clamp telemetry not recorded: %+v", reqCtx)
	}
}

func TestOutputShaper_DoesNotClampOnErrorResult(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{toolContinuation(true)},
	}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["thinking"].(map[string]any)["budget_tokens"] != float64(16000) {
		t.Error("must not clamp thinking when a tool result carries is_error")
	}
}

func TestOutputShaper_DoesNotClampOnUserTurn(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{map[string]any{"role": "user", "content": "think hard about this"}},
	}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["thinking"].(map[string]any)["budget_tokens"] != float64(16000) {
		t.Error("must not clamp thinking on a real user turn")
	}
}

func TestOutputShaper_NeverInventsThinking(t *testing.T) {
	req := map[string]any{"max_tokens": float64(8192), "messages": []any{toolContinuation(false)}}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, exists := req["thinking"]; exists {
		t.Error("must not add a thinking field the client did not send (invariant I4)")
	}
}

func TestOutputShaper_RespectsMaxTokensFloor(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(1024),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{toolContinuation(false)},
	}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["thinking"].(map[string]any)["budget_tokens"] != float64(16000) {
		t.Error("clamping to >= max_tokens would produce an invalid request; must skip")
	}
}

func TestOutputShaper_BypassWhenDisabled(t *testing.T) {
	req := map[string]any{"system": "Original"}
	stage := &OutputShaperStage{}
	cfg := &Config{Enabled: true, OutputShaper: OutputShaperConfig{Enabled: false, VerbositySteering: true}}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["system"] != "Original" {
		t.Error("system prompt must be untouched when the shaper is disabled")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/headroom/ -run TestOutputShaper`
Expected: FAIL

- [ ] **Step 3: Implement OutputShaperStage**

```go
// internal/headroom/output_shaper.go
package headroom

import "context"

const DefaultVerbosityPrompt = "Respond with concise technical precision. Avoid conversational filler, preamble, and meta-commentary. Focus directly on answering questions and executing actions."

// minThinkingBudget is the Anthropic API floor for thinking.budget_tokens.
const minThinkingBudget = 1024

type OutputShaperStage struct{}

func (s *OutputShaperStage) Name() string { return "output_shaper" }

func (s *OutputShaperStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.OutputShaper.Enabled {
		return nil
	}
	if cfg.OutputShaper.VerbositySteering {
		s.applySteering(reqCtx.Request, cfg)
	}
	if cfg.OutputShaper.EffortRouting && isMechanicalContinuation(reqCtx.Request) {
		s.clampEffort(reqCtx, cfg)
	}
	return nil
}

func (s *OutputShaperStage) applySteering(req map[string]any, cfg *Config) {
	text := cfg.OutputShaper.SteeringText
	if text == "" {
		text = DefaultVerbosityPrompt
	}
	// Appended at the tail so any cache_control breakpoint the client set on an
	// earlier system block keeps its exact bytes.
	switch sys := req["system"].(type) {
	case string:
		req["system"] = sys + "\n\n" + text
	case []any:
		req["system"] = append(sys, map[string]any{"type": "text", "text": text})
	case nil:
		req["system"] = text
	}
}

// isMechanicalContinuation reports whether the final message is a pure tool
// result turn with no errors: the model is resuming work, not being asked
// something new, and does not need a large thinking budget.
func isMechanicalContinuation(req map[string]any) bool {
	messages, ok := req["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	last, ok := messages[len(messages)-1].(map[string]any)
	if !ok || last["role"] != "user" {
		return false
	}
	blocks, ok := last["content"].([]any)
	if !ok || len(blocks) == 0 {
		return false
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			return false
		}
		if isErr, _ := block["is_error"].(bool); isErr {
			return false
		}
	}
	return true
}

func (s *OutputShaperStage) clampEffort(reqCtx *RequestContext, cfg *Config) {
	req := reqCtx.Request

	budget := cfg.OutputShaper.MechanicalThinkingBudget
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}

	// Anthropic shape. Only present-and-enabled thinking is clamped (I4).
	if thinking, ok := req["thinking"].(map[string]any); ok && thinking["type"] == "enabled" {
		if current, ok := thinking["budget_tokens"].(float64); ok && int(current) > budget {
			// max_tokens must stay strictly greater than budget_tokens.
			if maxTokens, ok := req["max_tokens"].(float64); !ok || int(maxTokens) > budget {
				thinking["budget_tokens"] = float64(budget)
				reqCtx.OriginalThinking = int(current)
				reqCtx.ClampedThinking = budget
				reqCtx.EffortClamped = true
			}
		}
	}

	// OpenAI-compatible shapes, for custom endpoints that speak them.
	if reasoning, ok := req["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort == "high" {
			reasoning["effort"] = "low"
			reqCtx.EffortClamped = true
		}
	}
	if effort, ok := req["reasoning_effort"].(string); ok && effort == "high" {
		req["reasoning_effort"] = "low"
		reqCtx.EffortClamped = true
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/output_shaper.go internal/headroom/output_shaper_test.go
git commit -m "feat(headroom): implement OutputShaper verbosity steering and effort routing"
```

---

### Task 6: Engine assembly & the cache-stability regression test

**Files:**
- Create: `internal/headroom/engine.go`
- Create: `internal/headroom/engine_test.go`

**Interfaces:**
- Produces: `headroom.Engine`, `headroom.NewEngine(Config) *Engine`, `Engine.Process(ctx, req) (*RequestContext, error)`, `Engine.UpdateConfig(Config)`, `Engine.GetConfig() Config`

**Design notes:**
- `TestEngine_PrefixBytesStableAcrossTurns` is the single most important test in this plan. It is the executable statement of the feature's central claim (invariant I1); if it regresses, Headroom is costing money rather than saving it.
- `Engine.Process` computes `FrozenPrefixIndex` from `LiveTurns` for CCR's benefit in Phase 2. The Phase 1 stages ignore it.
- `UpdateConfig` swaps the live config under a lock so a WebUI save takes effect without a restart.

- [ ] **Step 1: Write failing tests for the Engine**

```go
// internal/headroom/engine_test.go
package headroom

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func fullConfig() Config {
	return Config{
		Enabled: true, SmartCrusher: true, CodeCompressor: true, LiveTurns: 2,
		OutputShaper: OutputShaperConfig{Enabled: true, VerbositySteering: true, EffortRouting: true, MechanicalThinkingBudget: 1024},
	}
}

func toolResultMsg(payload string) any {
	return map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_" + payload[:1], "content": payload},
	}}
}

func TestEngine_CompressesToolResults(t *testing.T) {
	engine := NewEngine(fullConfig())
	req := map[string]any{"messages": []any{toolResultMsg("{\n  \"a\": 1\n}")}}

	reqCtx, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != `{"a":1}` {
		t.Errorf("expected compacted tool_result, got %q", got)
	}
	if reqCtx.BytesBefore <= reqCtx.BytesAfter {
		t.Errorf("expected savings, got before=%d after=%d", reqCtx.BytesBefore, reqCtx.BytesAfter)
	}
}

func TestEngine_DisabledIsCompleteBypass(t *testing.T) {
	engine := NewEngine(Config{Enabled: false, SmartCrusher: true, CodeCompressor: true})
	original := "{\n  \"a\": 1\n}"
	req := map[string]any{"system": "base", "messages": []any{toolResultMsg(original)}}

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != original || req["system"] != "base" {
		t.Error("disabled engine must not modify the request at all")
	}
}

// TestEngine_PrefixBytesStableAcrossTurns is the executable form of invariant
// I1: the serialized bytes of the shared conversation prefix must be identical
// between turn N and turn N+1, or the provider prompt cache misses every turn.
func TestEngine_PrefixBytesStableAcrossTurns(t *testing.T) {
	payloads := []string{
		"{\n  \"first\": true\n}",
		"log line   \n\n\n\nmore log",
		"{\n  \"third\": [1, 2, 3]\n}",
	}
	build := func(n int) map[string]any {
		msgs := make([]any, 0, n)
		for i := 0; i < n; i++ {
			msgs = append(msgs, toolResultMsg(payloads[i%len(payloads)]))
		}
		return map[string]any{"system": "base", "messages": msgs}
	}

	engine := NewEngine(fullConfig())

	turn1 := build(3)
	if _, err := engine.Process(context.Background(), turn1); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	turn2 := build(5)
	if _, err := engine.Process(context.Background(), turn2); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	prefix1, _ := json.Marshal(turn1["messages"].([]any))
	prefix2, _ := json.Marshal(turn2["messages"].([]any)[:3])
	if string(prefix1) != string(prefix2) {
		t.Errorf("prefix diverged between turns; prompt cache would miss\nturn1: %s\nturn2: %s", prefix1, prefix2)
	}
	if sys1, sys2 := turn1["system"], turn2["system"]; sys1 != sys2 {
		t.Errorf("system prompt diverged between turns: %v vs %v", sys1, sys2)
	}
}

func TestEngine_Idempotent(t *testing.T) {
	engine := NewEngine(fullConfig())
	req := map[string]any{"messages": []any{toolResultMsg("{\n  \"a\": 1\n}"), toolResultMsg("b   \n\n\n\nb2")}}

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	once, _ := json.Marshal(req["messages"])
	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	twice, _ := json.Marshal(req["messages"])
	if string(once) != string(twice) {
		t.Errorf("pipeline is not idempotent\nonce:  %s\ntwice: %s", once, twice)
	}
}

func TestEngine_UpdateConfigTakesEffect(t *testing.T) {
	engine := NewEngine(Config{Enabled: false})
	engine.UpdateConfig(fullConfig())
	if !engine.GetConfig().Enabled {
		t.Fatal("expected updated config to be live")
	}
	req := map[string]any{"messages": []any{toolResultMsg("{\n  \"a\": 1\n}")}}
	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != `{"a":1}` {
		t.Errorf("updated config not applied, got %q", got)
	}
}

func TestEngine_ConcurrentProcessIsSafe(t *testing.T) {
	engine := NewEngine(fullConfig())
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			req := map[string]any{"messages": []any{toolResultMsg(strings.Repeat("{\n \"x\": 1\n}", 1))}}
			_, _ = engine.Process(context.Background(), req)
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/headroom/ -run TestEngine`
Expected: FAIL

- [ ] **Step 3: Implement the Engine**

```go
// internal/headroom/engine.go
package headroom

import (
	"context"
	"sync"
)

// defaultLiveTurns is how many trailing messages CCR leaves inline.
const defaultLiveTurns = 2

type Engine struct {
	mu       sync.RWMutex
	config   Config
	pipeline *Pipeline
}

func NewEngine(cfg Config) *Engine {
	return &Engine{
		config: cfg,
		pipeline: NewPipeline(
			&SmartCrusherStage{},
			&CodeCompressorStage{},
			&OutputShaperStage{},
		),
	}
}

func (e *Engine) UpdateConfig(cfg Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.config = cfg
}

func (e *Engine) GetConfig() Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.config
}

// Process runs the pipeline over req, mutating it in place. The returned
// RequestContext carries telemetry only; the request itself is the caller's map.
func (e *Engine) Process(ctx context.Context, req map[string]any) (*RequestContext, error) {
	cfg := e.GetConfig()

	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: frozenPrefixIndex(req, cfg.LiveTurns),
	}
	if err := e.pipeline.Run(ctx, reqCtx, &cfg); err != nil {
		return nil, err
	}
	return reqCtx, nil
}

// frozenPrefixIndex returns the highest message index outside the live window.
// -1 means every message is live.
func frozenPrefixIndex(req map[string]any, liveTurns int) int {
	if liveTurns <= 0 {
		liveTurns = defaultLiveTurns
	}
	messages, ok := req["messages"].([]any)
	if !ok {
		return -1
	}
	if idx := len(messages) - liveTurns - 1; idx >= 0 {
		return idx
	}
	return -1
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/ -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/engine.go internal/headroom/engine_test.go
git commit -m "feat(headroom): assemble Engine with cache-stability regression tests"
```

---

### Task 7: Proxy integration across all four provider paths

**Files:**
- Modify: `internal/api/server.go` (`Options`/`Server` ~lines 66-102, `New` ~line 104, `messages` ~line 530)
- Modify: `internal/api/management.go` (`handleConfigSave` ~line 654)
- Create: `internal/api/headroom_proxy_test.go`

**Interfaces:**
- Consumes: `headroom.Engine`, `config.Get().Headroom`
- Produces: `Server.headroom`, `applyHeadroomConfig`

**Design notes — placement is the whole task:**

`Server.messages` marshals `anthropicRequest` **separately for each provider** (Kimi at `server.go:591`, OpenRouter at `:607`, custom endpoints at `:619`) and passes the live map to the Cloud Code path at `:631`/`:644`. The pipeline must therefore run **once**, after model mapping and the `max_tokens` default, and **before the `model := stringFrom(...)` provider dispatch block** — i.e. immediately after the `messages[0]["content"] == "count"` short-circuit at `server.go:583`. Running it any later means one or more providers marshal an uncompressed body.

Failure policy: a pipeline error must **not** fail the request. Log at WARN and continue with whatever state the request is in; Headroom is an optimisation, not a correctness requirement.

`handleConfigSave` currently propagates to `accountManager` and the `ConfigUpdater` backend but never re-applies router config; add an explicit `server.applyHeadroomConfig(updated.Headroom)` call so WebUI toggles take effect without a restart.

- [ ] **Step 1: Write failing integration tests**

```go
// internal/api/headroom_proxy_test.go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
)

// captureBackend records the request the proxy actually dispatched.
type captureBackend struct {
	mu   sync.Mutex
	last map[string]any
}

func (b *captureBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	b.mu.Lock()
	raw, _ := json.Marshal(req)
	_ = json.Unmarshal(raw, &b.last)
	b.mu.Unlock()
	return cloudcode.Response{}, nil
}

func (b *captureBackend) seen() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

func newHeadroomTestServer(t *testing.T, headroom map[string]any) (*Server, *captureBackend) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := config.Save(map[string]any{"headroom": headroom}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	backend := &captureBackend{}
	srv, err := New(Options{APIKey: "test-key", Backend: backend})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, backend
}

func postMessages(t *testing.T, srv *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	req.Header.Set("x-api-key", "test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func toolResultBody(payload string) map[string]any {
	return map[string]any{
		"model": "gemini-3.5-flash-low",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": payload},
			}},
		},
	}
}

func firstToolResult(t *testing.T, req map[string]any) string {
	t.Helper()
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("no messages in dispatched request: %#v", req)
	}
	blocks := msgs[0].(map[string]any)["content"].([]any)
	return blocks[0].(map[string]any)["content"].(string)
}

func TestHeadroom_CompressesCloudCodeDispatch(t *testing.T) {
	srv, backend := newHeadroomTestServer(t, map[string]any{
		"enabled": true, "smartCrusher": true, "codeCompressor": true,
	})
	if rec := postMessages(t, srv, toolResultBody("{\n  \"a\": 1\n}")); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := firstToolResult(t, backend.seen()); got != `{"a":1}` {
		t.Errorf("backend received uncompressed payload: %q", got)
	}
}

func TestHeadroom_DisabledLeavesRequestIntact(t *testing.T) {
	original := "{\n  \"a\": 1\n}"
	srv, backend := newHeadroomTestServer(t, map[string]any{"enabled": false, "smartCrusher": true})
	if rec := postMessages(t, srv, toolResultBody(original)); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := firstToolResult(t, backend.seen()); got != original {
		t.Errorf("expected untouched payload, got %q", got)
	}
}

// TestHeadroom_CompressesKimiDispatch and the OpenRouter / custom-endpoint
// variants assert the same thing against an httptest upstream, proving the
// pipeline runs before every json.Marshal in Server.messages.
func TestHeadroom_CompressesKimiDispatch(t *testing.T) {
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = readAllBody(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{}}`))
	}))
	defer upstream.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := config.Save(map[string]any{
		"headroom": map[string]any{"enabled": true, "smartCrusher": true},
		"kimi":     map[string]any{"enabled": true, "baseURL": upstream.URL, "apiKey": "k", "models": []any{"kimi-k2"}},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	srv, err := New(Options{APIKey: "test-key", Backend: &captureBackend{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := toolResultBody("{\n  \"a\": 1\n}")
	body["model"] = "kimi-k2"
	if rec := postMessages(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var dispatched map[string]any
	if err := json.Unmarshal(received, &dispatched); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if got := firstToolResult(t, dispatched); got != `{"a":1}` {
		t.Errorf("Kimi upstream received uncompressed payload: %q", got)
	}
}
```

> Adapt the Kimi config keys to whatever `config.KimiConfig` actually declares (see `internal/api/kimi_proxy_test.go:68` for the working shape) and add `readAllBody` or reuse the existing helper. Add the OpenRouter and custom-endpoint variants by the same pattern; `internal/api/openrouter_proxy_test.go` already has an `httptest` upstream harness to copy.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestHeadroom`
Expected: FAIL (`Server.headroom` undefined)

- [ ] **Step 3: Wire the Engine into the server**

1. Add `headroom *headroom.Engine` to `Server`.
2. In `New`, after `cfg := config.Get()`: `srv.headroom = headroom.NewEngine(cfg.Headroom)`.
3. Add:
   ```go
   func (server *Server) applyHeadroomConfig(cfg config.HeadroomConfig) {
       if server.headroom != nil {
           server.headroom.UpdateConfig(cfg)
       }
   }
   ```
4. In `Server.messages`, immediately after the `content == "count"` short-circuit and before `model := stringFrom(anthropicRequest["model"])`:
   ```go
   if server.headroom != nil {
       if hrCtx, err := server.headroom.Process(request.Context(), anthropicRequest); err != nil {
           server.logger.Warn("headroom pipeline failed; forwarding request unmodified", "error", err)
       } else if hrCtx.BytesBefore > 0 {
           server.logger.Debug("headroom compressed request",
               "bytesBefore", hrCtx.BytesBefore, "bytesAfter", hrCtx.BytesAfter,
               "effortClamped", hrCtx.EffortClamped)
           if server.tracker != nil {
               server.tracker.RecordHeadroom(stats.HeadroomSample{
                   BytesBefore: hrCtx.BytesBefore, BytesAfter: hrCtx.BytesAfter,
                   ThinkingTokensClamped: hrCtx.OriginalThinking - hrCtx.ClampedThinking,
               })
           }
       }
   }
   ```
   (The tracker call lands in Task 8; stub it out here or implement Task 8 first.)
5. In `handleConfigSave`, after the existing propagation calls, add `server.applyHeadroomConfig(updated.Headroom)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ ./internal/headroom/ ./internal/config/ -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/server.go internal/api/management.go internal/api/headroom_proxy_test.go
git commit -m "feat(api): run Headroom pipeline before every provider dispatch"
```

---

### Task 8: Metrics & observability

**Files:**
- Modify: `internal/stats/tracker.go`
- Modify: `internal/stats/tracker_test.go`

**Interfaces:**
- Produces: `stats.HeadroomSample`, `Tracker.RecordHeadroom(HeadroomSample)`, `Tracker.GetHeadroomStats() HeadroomStats`

**Design notes:**
- The spec's `headroom_output_tokens_saved` is **not measurable**: nobody can know how many output tokens a steering sentence prevented. Replace it with quantities the proxy actually observes:
  - `bytesBefore` / `bytesAfter` over rewritten blocks (input compression ratio),
  - `thinkingTokensClamped` (the budget delta actually applied — a bound on savings, honestly labelled),
  - `requestsShaped`, `requestsCompressed`,
  - `ccrRetrievals` (Phase 2).
  Amend the spec accordingly in Task 10.
- `Tracker` persists through `Save`/`normalizeHistory`; decide explicitly whether Headroom totals are per-process or persisted. Recommendation: **persist** them alongside `history` under a top-level `headroom` key, and make `parseModelMetrics`/`normalizeHistory` tolerate its absence in existing files on disk.

- [ ] **Step 1: Write failing tests for Headroom metrics**

```go
// internal/stats/tracker_test.go (add)
func TestTracker_RecordHeadroom(t *testing.T) {
	tracker, err := NewTracker("")
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	tracker.RecordHeadroom(HeadroomSample{BytesBefore: 1000, BytesAfter: 700, ThinkingTokensClamped: 50})
	tracker.RecordHeadroom(HeadroomSample{BytesBefore: 500, BytesAfter: 500})

	got := tracker.GetHeadroomStats()
	if got.BytesBefore != 1500 || got.BytesAfter != 1200 {
		t.Errorf("unexpected byte totals: %+v", got)
	}
	if got.ThinkingTokensClamped != 50 {
		t.Errorf("unexpected clamp total: %+v", got)
	}
	if got.RequestsCompressed != 2 {
		t.Errorf("unexpected request count: %+v", got)
	}
}

func TestTracker_HeadroomStatsPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	tracker, err := NewTracker(path)
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	tracker.RecordHeadroom(HeadroomSample{BytesBefore: 100, BytesAfter: 60})
	if err := tracker.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := NewTracker(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.GetHeadroomStats(); got.BytesBefore != 100 || got.BytesAfter != 60 {
		t.Errorf("headroom stats did not survive reload: %+v", got)
	}
}

func TestTracker_ConcurrentRecordHeadroom(t *testing.T) {
	tracker, _ := NewTracker("")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordHeadroom(HeadroomSample{BytesBefore: 10, BytesAfter: 5})
		}()
	}
	wg.Wait()
	if got := tracker.GetHeadroomStats(); got.BytesBefore != 1000 {
		t.Errorf("lost updates under concurrency: %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/stats/ -run TestTracker_.*Headroom`
Expected: FAIL

- [ ] **Step 3: Implement Headroom tracking**

Add to `internal/stats/tracker.go`:
```go
type HeadroomSample struct {
	BytesBefore           int
	BytesAfter            int
	ThinkingTokensClamped int
	CCRRetrievals         int
}

type HeadroomStats struct {
	BytesBefore           int `json:"bytesBefore"`
	BytesAfter            int `json:"bytesAfter"`
	ThinkingTokensClamped int `json:"thinkingTokensClamped"`
	CCRRetrievals         int `json:"ccrRetrievals"`
	RequestsCompressed    int `json:"requestsCompressed"`
}
```
Guard the running totals with the tracker's existing mutex, include them in `Save`/load under a top-level `headroom` key, and tolerate its absence when reading older stats files.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/stats/ -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/stats/tracker.go internal/stats/tracker_test.go
git commit -m "feat(stats): track Headroom compression ratio and clamped thinking budget"
```

---

### Task 9: WebUI settings, toggles & dashboard

**Files:**
- Modify: `internal/api/management.go`
- Modify: `internal/webui/public/views/settings.html`
- Modify: `internal/webui/public/views/dashboard.html`
- Modify: `internal/webui/public/js/` (settings store + translations)
- Modify: `internal/api/management_test.go`

**Interfaces:**
- Produces: Headroom settings panel, `GET /api/headroom/stats`, Headroom card on the dashboard.

**Design notes:**
- `GetPublicConfig()` already surfaces `headroom` (it marshals the whole `Config`), so `GET /api/config` needs no change.
- `config.Save` replaces the `headroom` subtree wholesale (Task 1), so the settings view must POST the **complete** object it received from `GET /api/config`, with only the changed fields altered. A test must pin this.
- Add a small `GET /api/headroom/stats` endpoint rather than overloading `/api/config`, so the dashboard card can poll cheaply.
- The panel must show a plain-language warning: changing any Headroom setting invalidates the provider prompt cache once for in-flight conversations (invariant I1).

- [ ] **Step 1: Write failing tests for the management endpoints**

```go
// internal/api/management_test.go (add)
func TestManagement_SaveHeadroomConfig(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	body, _ := json.Marshal(map[string]any{"headroom": map[string]any{
		"enabled": true, "smartCrusher": true, "codeCompressor": true, "liveTurns": 3,
		"ccr":          map[string]any{"enabled": false, "maxStoreMB": 32, "minChunkBytes": 4096},
		"outputShaper": map[string]any{"enabled": true, "verbositySteering": true, "effortRouting": false, "mechanicalThinkingBudget": 2048},
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := config.Get().Headroom
	if !got.Enabled || got.LiveTurns != 3 || got.CCR.MaxStoreMB != 32 || got.OutputShaper.MechanicalThinkingBudget != 2048 {
		t.Errorf("headroom config not persisted: %+v", got)
	}
	if srv.headroom.GetConfig().LiveTurns != 3 {
		t.Error("live engine was not updated after config save")
	}
}

func TestManagement_ConfigGetExposesHeadroom(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var payload struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload.Config["headroom"]; !ok {
		t.Error("GET /api/config must expose the headroom subtree for the settings view")
	}
}

func TestManagement_HeadroomStatsEndpoint(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/headroom/stats", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"bytesBefore", "bytesAfter", "requestsCompressed"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("missing %q in headroom stats payload", key)
		}
	}
}
```

Also extend `internal/webui/translations_test.go` with the new UI string keys so no locale is left with a missing entry.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestManagement_.*Headroom && go test ./internal/webui/`
Expected: FAIL

- [ ] **Step 3: Implement the endpoints and UI**

1. Route `GET /api/headroom/stats` in `handleManagement` next to the other `/api/...` cases, returning `tracker.GetHeadroomStats()`.
2. Add a **Headroom** section to `settings.html`: master toggle, SmartCrusher, CodeCompressor, CCR (+ `maxStoreMB`, `minChunkBytes`), OutputShaper (+ `verbositySteering`, `effortRouting`, `mechanicalThinkingBudget`, `steeringText`), `liveTurns`, and the cache-invalidation notice.
3. Wire the save handler to POST the full `headroom` object.
4. Add a dashboard card: bytes saved, compression ratio, requests compressed, thinking tokens clamped.
5. Add translation keys for every new label.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ ./internal/webui/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/management.go internal/api/management_test.go internal/webui/
git commit -m "feat(webui): add Headroom settings, bypass controls, and analytics card"
```

---

### Task 10: Documentation & spec reconciliation

**Files:**
- Modify: `docs/superpowers/specs/2026-08-27-headroom-integration-design.md`
- Modify: `README.md`
- Modify: `CONTEXT.md` (and `AGENTS.md` if it enumerates packages)

- [ ] **Step 1: Reconcile the spec with the implemented design**

Amend the spec where implementation diverged, with the reason:
1. §3.1 — CacheAligner is not a compression gate. Cache safety comes from deterministic, position-independent transforms (invariant I1); the "live zone" now scopes CCR only.
2. §3.2.3 — uniform-array→tabular conversion deferred; record as a follow-up.
3. §3.3.2 — trailing-whitespace trim only; "leading" contradicted "preserving indentation".
4. §3.4 — transparent hydration is limited to the Cloud Code and OpenRouter paths (see Phase 2 Task 13).
5. §4.1 — JSON keys are camelCase, matching the rest of `config.json`; default `enabled` is `false`.
6. §5 — `headroom_output_tokens_saved` replaced by measurable counters (Task 8).

- [ ] **Step 2: Document the feature for users**

Add a Headroom section to `README.md`: what each stage does, the `config.json` block with real defaults, the WebUI location, and an explicit note that enabling or retuning Headroom costs one prompt-cache miss per in-flight conversation.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/specs/2026-08-27-headroom-integration-design.md README.md CONTEXT.md AGENTS.md
git commit -m "docs(headroom): reconcile spec with implementation and document configuration"
```

---

## Phase 2 — Content-Conditioned Retrieval

> **Gate:** do not start Phase 2 until Phase 1 is merged and `TestEngine_PrefixBytesStableAcrossTurns` is green in CI. CCR is the only part of this design that knowingly trades cache stability (invariant I2), and it is worth shipping only once the cheap, strictly-beneficial compression is proven in production.

CCR's hard part is not the store — it is the **hydration loop**, which the previous revision of this plan named in two task titles and never specified. Detecting a `headroom_retrieve` tool call means buffering or teeing the SSE stream, suppressing the terminal events of the first response, issuing a *second* upstream request with a synthetic `tool_result` appended, renumbering the continuation's content-block indices, and merging usage totals. That is only possible on paths where the proxy owns the request loop:

| Path | Hydration feasible? | Why |
| --- | --- | --- |
| Cloud Code | Yes | `streamSender` closure is re-invocable |
| OpenRouter | Yes | `forwardToOpenRouter` already owns a retry/failover loop |
| Kimi | No | raw pass-through forward |
| Custom endpoint | No | `httputil.ReverseProxy`, no re-issue point |

**Therefore CCR must be disabled for the Kimi and custom-endpoint paths**, and the WebUI must say so. Chunk demotion without hydration would strand the model with an unreadable reference.

### Task 11: CCR chunk store

**Files:** create `internal/headroom/ccr_store.go`, `internal/headroom/ccr_store_test.go`.

Tests must cover: put/get round trip, stable content-addressed IDs (`chunk_<sha256[:12]>`), LRU eviction order, a single entry larger than the whole capacity (must be rejected, not evict-everything-then-insert), byte accounting after eviction, and `-race` concurrent put/get.

Notes on the previous draft's store: `Get` takes a write lock (it moves the LRU node), so the `RWMutex` bought nothing — use a plain `sync.Mutex` and say why; and the eviction loop admitted oversized entries after emptying the cache, so cap `Put` on `size > maxBytes` up front.

### Task 12: CCR demotion stage

**Files:** create `internal/headroom/ccr_stage.go`, `internal/headroom/ccr_test.go`.

- Demote only `tool_result` payloads at index `<= FrozenPrefixIndex` and `>= MinChunkBytes`. The newest `liveTurns` messages stay inline: truncating a tool result the model is about to read forces a guaranteed extra round trip, which costs more than it saves.
- **Store the payload before SmartCrusher/CodeCompressor touch it**, or the "original content" that `headroom_retrieve` returns is a compacted variant. Simplest correct ordering: run `CCRStage` **first** in the pipeline.
- Inject the `headroom_retrieve` tool definition **unconditionally whenever CCR is enabled and the client already sent a `tools` array** (invariants I2, I4). Conditional injection (`if chunkStored`) makes the tool list flip between turns, and the tool block sits ahead of system and messages in the cache hierarchy — flipping it invalidates the *entire* cached prefix.
- Verify the injected `input_schema` survives both translation paths: `internal/format/request.go:260` for Cloud Code `functionDeclarations`, and the Anthropic-native body for OpenRouter.

### Task 13: Transparent hydration loop

**Files:** modify `internal/api/server.go`, create `internal/api/headroom_ccr_test.go`.

Specify and test, at minimum:
1. Detecting `tool_use` with `name == "headroom_retrieve"` in a streamed response.
2. Suppressing `message_delta`/`message_stop` of the intercepted response.
3. Appending the assistant turn plus a synthetic `tool_result` and re-issuing upstream.
4. Renumbering `content_block_start`/`_delta`/`_stop` indices across the stitched stream.
5. Merging usage across both upstream calls so billing and `stats` stay correct.
6. A hard iteration cap (e.g. 3 hydrations per request) with a graceful fallthrough.
7. Cache miss on an evicted chunk → return an `is_error` tool_result explaining the payload is gone, never a proxy-level failure.
8. Recording `CCRRetrievals` in the tracker.

### Task 14: CCR configuration exposure

Enable the CCR controls in the WebUI panel from Task 9, add the retrieval counter to the dashboard card, and document the Kimi/custom-endpoint limitation in `README.md` and the spec.

---

## Plan Self-Review

Honest status, replacing the previous revision's checklist (which asserted four things that were not true):

- [x] **Every code block checked against the real codebase.** `Server.messages` decodes into `map[string]any` (`server.go:533`), so map-based stages are correct; `Options`/`Server` field names, `config.Save`, `config.GetPublicConfig`, `handleConfigSave`, and `stats.NewTracker` signatures are all as referenced.
- [x] **Test expectations corrected.** The previous revision's SmartCrusher test expected `json.Compact` to reorder keys (it does not) and its Engine test expected `PruneText` to strip leading whitespace (it trims trailing only). Both were unpassable; both are fixed here.
- [x] **Test commands corrected.** `go test ./path/file_test.go` compiles one file without its package and fails to build; all commands are now `go test ./internal/<pkg>/ [-run …]`.
- [x] **Type ambiguity resolved.** `config.HeadroomConfig` is a type alias for `headroom.Config`; the previous revision used both names for different things.
- [x] **Integration test rewritten against real helpers.** The previous revision called a nonexistent `config.Set` and a nonexistent `mockBackend`, and asserted only an HTTP 200 without checking that anything was compressed.
- [x] **Spec coverage stated explicitly**, including the three items deliberately deferred (§3.2.3 tabular conversion, §3.5 partial, §3.4 restricted to two provider paths) — recorded in Task 10 rather than silently dropped.
- [x] **Placeholder removed.** The previous revision's Task 10 test body was a bare comment.
- [ ] **Open decision for the implementer:** Phase 2 is gated on Phase 1 shipping. If CCR is wanted in the first release instead, Tasks 11-14 move ahead of Task 9 and the Kimi/custom-endpoint restriction must be surfaced in the UI from day one.
