package ccr

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
)

func realisticReadPayload() string {
	var b strings.Builder
	b.WriteString("     1\tpackage main\n")
	b.WriteString("     2\t\n")
	b.WriteString("     3\timport \"fmt\"   \n")
	for i := 4; i < 8; i++ {
		b.WriteString("     " + itoa(i) + "\t// repeated marker\n")
	}
	for lineNo := 8; b.Len() < 3000; lineNo++ {
		b.WriteString("     " + itoa(lineNo) + "\tfmt.Println(\"x\")  \n")
	}
	return b.String()
}

func readEditRequest(payload string) map[string]any {
	return map[string]any{
		"tools": []any{map[string]any{"name": "Read"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "read the file"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read",
					"input": map[string]any{"file_path": "/tmp/main.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": payload},
			}},
			map[string]any{"role": "assistant", "content": "noted"},
			map[string]any{"role": "user", "content": "now edit line 3"},
		},
	}
}

func toolResultText(t *testing.T, req map[string]any, msgIdx int) string {
	t.Helper()
	msgs := req["messages"].([]any)
	block := msgs[msgIdx].(map[string]any)["content"].([]any)[0].(map[string]any)
	s, ok := block["content"].(string)
	if !ok {
		t.Fatalf("message %d tool_result content is not a string: %T", msgIdx, block["content"])
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func TestCCRStage_DemotesFrozenPrefixAboveMinSize(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewStage(store)

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

	reqCtx := &headroom.RequestContext{
		Request:           req,
		FrozenPrefixIndex: 0, // only message 0 is outside live window
	}

	cfg := &headroom.Config{
		Enabled: true,
		CCR: headroom.CCRConfig{
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
	stage := NewStage(store)

	smallPayload := "small output under min chunk size"
	req := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": smallPayload},
			}},
		},
	}
	reqCtx := &headroom.RequestContext{
		Request:           req,
		FrozenPrefixIndex: 0,
	}
	cfg := &headroom.Config{
		Enabled: true,
		CCR: headroom.CCRConfig{
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
	stage := NewStage(store)

	req := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	reqCtx := &headroom.RequestContext{Request: req}
	cfg := &headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: true},
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
	stage := NewStage(store)

	largePayload := strings.Repeat("x", 5000)
	req := map[string]any{
		"tools": []any{map[string]any{"name": "bash"}},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": largePayload},
			}},
		},
	}
	reqCtx := &headroom.RequestContext{
		Request:           req,
		FrozenPrefixIndex: 0,
	}
	cfg := &headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: false},
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

func TestCCRStage_SkipsVerbatimReadResult(t *testing.T) {
	store := NewCCRStore(1024 * 1024)
	stage := NewStage(store)

	payload := realisticReadPayload()
	req := readEditRequest(payload)

	reqCtx := &headroom.RequestContext{
		Request:           req,
		FrozenPrefixIndex: 2, // the tool_result message
		Verbatim:          headroom.NewToolInspector(req),
	}
	cfg := &headroom.Config{
		Enabled:               true,
		PreserveVerbatimReads: true,
		CCR:                   headroom.CCRConfig{Enabled: true, MinChunkBytes: 2048},
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
	stage := NewStage(store)

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
	reqCtx := &headroom.RequestContext{
		Request:           req,
		FrozenPrefixIndex: 1,
		Verbatim:          headroom.NewToolInspector(req),
	}
	cfg := &headroom.Config{
		Enabled:               true,
		PreserveVerbatimReads: true,
		CCR:                   headroom.CCRConfig{Enabled: true, MinChunkBytes: 1000},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx.ChunksStored != 1 {
		t.Errorf("non-verbatim payload must still demote, ChunksStored=%d", reqCtx.ChunksStored)
	}
}

func TestCCRStage_LogsChunkDemotion(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	bigPayload := strings.Repeat("hello world data\n", 500)
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "content": bigPayload},
				},
			},
			map[string]any{"role": "assistant", "content": "turn 1"},
			map[string]any{"role": "user", "content": "turn 2 live"},
		},
	}
	reqCtx := &headroom.RequestContext{Request: req, FrozenPrefixIndex: 0, Logger: logger}
	cfg := &headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: true, MinChunkBytes: 100},
	}

	if err := NewStage(NewCCRStoreFromMB(10)).Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx.ChunksStored == 0 {
		t.Fatal("expected a chunk to be stored")
	}
	for _, want := range []string{"ccr demoted chunk", "chunk_id", "chunk_bytes", "store_bytes"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("log missing %q; got: %s", want, buf.String())
		}
	}
}
