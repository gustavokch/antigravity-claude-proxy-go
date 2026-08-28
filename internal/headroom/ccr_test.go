package headroom

import (
	"context"
	"strings"
	"testing"
)

func TestCCRStage_DemotesFrozenPrefixAboveMinSize(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewCCRStage(store)

	largePayload := "line 1: start of output\n" + strings.Repeat("log data line\n", 200) + "line end"
	livePayload := "live output: " + strings.Repeat("recent data\n", 200)

	req := map[string]any{
		"tools": []any{
			map[string]any{"name": "grep", "description": "search"},
		},
		"messages": []any{
			// Index 0: frozen message
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": largePayload},
			}},
			// Index 1: live message 1
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": livePayload},
			}},
			// Index 2: live message 2
			map[string]any{"role": "user", "content": "next action"},
		},
	}

	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: 0, // only message 0 is outside live window
	}

	cfg := &Config{
		Enabled: true,
		CCR: CCRConfig{
			Enabled:       true,
			MinChunkBytes: 1000,
		},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := req["messages"].([]any)
	msg0Content := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(msg0Content, `[HEADROOM_CHUNK id="chunk_`) {
		t.Errorf("expected demoted chunk token in message 0, got: %q", msg0Content)
	}

	msg1Content := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if msg1Content != livePayload {
		t.Errorf("live message 1 must remain untouched, got: %q", msg1Content)
	}

	if reqCtx.ChunksStored != 1 {
		t.Errorf("expected 1 chunk stored, got %d", reqCtx.ChunksStored)
	}

	// Verify store has the chunk and original payload can be retrieved
	chunkID := ChunkID(largePayload)
	retrieved, found := store.Get(chunkID)
	if !found || retrieved != largePayload {
		t.Errorf("chunk store retrieval mismatch: found=%v got=%q", found, retrieved)
	}

	// Verify tool definition was injected
	tools := req["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	t2 := tools[1].(map[string]any)
	if t2["name"] != "headroom_retrieve" {
		t.Errorf("expected injected tool name 'headroom_retrieve', got %v", t2["name"])
	}
}

func TestCCRStage_SmallPayloadNotDemoted(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewCCRStage(store)

	smallPayload := "small output under min chunk size"
	req := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": smallPayload},
			}},
		},
	}
	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: 0,
	}
	cfg := &Config{
		Enabled: true,
		CCR: CCRConfig{
			Enabled:       true,
			MinChunkBytes: 2048,
		},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg0Content := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if msg0Content != smallPayload {
		t.Errorf("small payload must not be demoted, got %q", msg0Content)
	}
	if reqCtx.ChunksStored != 0 {
		t.Errorf("expected 0 chunks stored, got %d", reqCtx.ChunksStored)
	}
}

func TestCCRStage_DoesNotInjectToolWhenNoTools(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewCCRStage(store)

	req := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	reqCtx := &RequestContext{Request: req}
	cfg := &Config{
		Enabled: true,
		CCR:     CCRConfig{Enabled: true},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := req["tools"]; exists {
		t.Error("must not inject tools when client provided no tools array (invariant I4)")
	}
}

func TestCCRStage_DisabledIsNoOp(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewCCRStage(store)

	largePayload := strings.Repeat("x", 5000)
	req := map[string]any{
		"tools": []any{map[string]any{"name": "bash"}},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": largePayload},
			}},
		},
	}
	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: 0,
	}
	cfg := &Config{
		Enabled: true,
		CCR:     CCRConfig{Enabled: false},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgContent := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if msgContent != largePayload {
		t.Errorf("disabled CCR should not alter message content, got %q", msgContent)
	}
	tools := req["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("disabled CCR should not inject tools, got %d tools", len(tools))
	}
}

func TestEngine_CCRStoresOriginalBeforeCompression(t *testing.T) {
	prettyJSON := "{\n  \"field\": 1,\n  \"nested\": [\n    " + strings.Repeat("\"long_data_item\",\n    ", 150) + "\"end\"\n  ]\n}"
	engine := NewEngine(Config{
		Enabled:        true,
		SmartCrusher:   true,
		CodeCompressor: true,
		LiveTurns:      1,
		CCR: CCRConfig{
			Enabled:       true,
			MinChunkBytes: 500,
		},
	})

	req := map[string]any{
		"tools": []any{map[string]any{"name": "test_tool"}},
		"messages": []any{
			// Frozen message 0 (len > 500)
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": prettyJSON},
			}},
			// Live message 1
			map[string]any{"role": "user", "content": "summarize please"},
		},
	}

	reqCtx, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}

	if reqCtx.ChunksStored != 1 {
		t.Errorf("expected 1 chunk stored, got %d", reqCtx.ChunksStored)
	}

	chunkID := ChunkID(prettyJSON)
	stored, found := engine.CCRStore().Get(chunkID)
	if !found {
		t.Fatalf("chunk not found in engine store")
	}
	// The stored payload must be the uncompressed pretty JSON (exact match)
	if stored != prettyJSON {
		t.Errorf("expected uncompacted original stored in CCR store, got:\n%s", stored)
	}

	// The message 0 in req should be rewritten to the chunk token
	msg0Content := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(msg0Content, `[HEADROOM_CHUNK id="chunk_`) {
		t.Errorf("expected chunk token in message 0, got: %s", msg0Content)
	}
}


func TestCCRStage_SkipsVerbatimReadResult(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewCCRStage(store)

	payload := realisticReadPayload()
	req := readEditRequest(payload)

	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: 2, // the tool_result message
		Verbatim:          NewToolInspector(req),
	}
	cfg := &Config{
		Enabled:               true,
		PreserveVerbatimReads: true,
		CCR:                   CCRConfig{Enabled: true, MinChunkBytes: 2048},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := toolResultText(t, req, 2)
	if got != payload {
		t.Errorf("verbatim Read result must stay inline byte-for-byte, got prefix: %.80q", got)
	}
	if reqCtx.ChunksStored != 0 {
		t.Errorf("expected 0 chunks stored, got %d", reqCtx.ChunksStored)
	}
	if reqCtx.VerbatimSkipped != 1 {
		t.Errorf("expected VerbatimSkipped 1, got %d", reqCtx.VerbatimSkipped)
	}
}

func TestCCRStage_StillDemotesNonVerbatim(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewCCRStage(store)

	largeLog := "grep output\n" + strings.Repeat("match line\n", 300)
	req := map[string]any{
		"tools": []any{map[string]any{"name": "Bash"}},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_b", "name": "Bash",
					"input": map[string]any{"command": "grep -r foo ."}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_b", "content": largeLog},
			}},
		},
	}
	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: 1,
		Verbatim:          NewToolInspector(req),
	}
	cfg := &Config{
		Enabled:               true,
		PreserveVerbatimReads: true,
		CCR:                   CCRConfig{Enabled: true, MinChunkBytes: 1000},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx.ChunksStored != 1 {
		t.Errorf("non-verbatim payload must still demote, ChunksStored=%d", reqCtx.ChunksStored)
	}
}
