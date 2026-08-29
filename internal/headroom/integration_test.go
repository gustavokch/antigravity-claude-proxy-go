package headroom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
	"antigravity-go-proxy/internal/headroom/stages/code"
	"antigravity-go-proxy/internal/headroom/stages/crusher"
	"antigravity-go-proxy/internal/headroom/stages/shaper"
	"antigravity-go-proxy/internal/headroom/stages/smart"
)

// testStages returns the production stage list. Tasks 3-7 update this single
// helper as each stage moves into its own subpackage.
func testStages(store *ccr.CCRStore) []headroom.Stage {
	return []headroom.Stage{
		ccr.NewStage(store),
		crusher.NewStage(),
		smart.NewStage(),
		code.NewStage(),
		shaper.NewStage(),
	}
}

func newTestEngine(cfg headroom.Config) *headroom.Engine {
	store := ccr.NewCCRStoreFromMB(cfg.CCR.MaxStoreMB)
	return headroom.NewEngine(cfg, nil, testStages(store)...)
}

// fullConfig and toolResultMsg move verbatim from engine_test.go, gaining
// only the headroom. qualification.
func fullConfig() headroom.Config {
	return headroom.Config{
		Enabled: true, SmartCrusher: true, CodeCompressor: true, LiveTurns: 2,
		OutputShaper: headroom.OutputShaperConfig{
			Enabled: true, VerbositySteering: true, EffortRouting: true,
			MechanicalThinkingBudget: 1024,
		},
	}
}

func toolResultMsg(payload string) any {
	return map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_" + payload[:1], "content": payload},
	}}
}

func TestEngine_CommandCrusherRunsBeforeSmartCrusher(t *testing.T) {
	cfg := fullConfig()
	cfg.CommandCrusher = true
	engine := newTestEngine(cfg)
	// A payload that is BOTH pytest output and invalid for JSON compaction:
	// crusher must strip the progress line, smart crusher must leave it alone.
	payload := "collected 2 items\n\ntest_a.py .. [100%]\n\n=== 2 passed in 0.01s ==="
	req := map[string]any{"messages": []any{toolResultMsg(payload)}}

	reqCtx, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "[100%]") {
		t.Errorf("expected progress line stripped, got %q", got)
	}
	if !strings.Contains(got, "=== 2 passed in 0.01s ===") {
		t.Errorf("expected summary retained, got %q", got)
	}
	if reqCtx.BytesAfter >= reqCtx.BytesBefore {
		t.Errorf("expected savings, before=%d after=%d", reqCtx.BytesBefore, reqCtx.BytesAfter)
	}
}

func TestEngine_CommandCrusherDisabledByDefault(t *testing.T) {
	engine := newTestEngine(fullConfig()) // fullConfig does not set CommandCrusher
	payload := "collected 2 items\n\ntest_a.py .. [100%]\n\n=== 2 passed in 0.01s ==="
	req := map[string]any{"messages": []any{toolResultMsg(payload)}}

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("disabled stage must not modify payload, got %q", got)
	}
}

func TestEngine_CompressesToolResults(t *testing.T) {
	engine := newTestEngine(fullConfig())
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
	engine := newTestEngine(headroom.Config{Enabled: false, SmartCrusher: true, CodeCompressor: true})
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

	engine := newTestEngine(fullConfig())

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
	engine := newTestEngine(fullConfig())
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
	engine := newTestEngine(headroom.Config{Enabled: false})
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
	engine := newTestEngine(fullConfig())
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

// --- Verbatim pipeline integration tests -------------------------------------

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

func firstDiff(want, got string) string {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			lo := i - 40
			if lo < 0 {
				lo = 0
			}
			hi := i + 40
			if hi > n {
				hi = n
			}
			return "byte " + itoa(i) + ":\nwant: " + want[lo:hi] + "\ngot:  " + got[lo:hi]
		}
	}
	if len(want) != len(got) {
		return "length: want " + itoa(len(want)) + " got " + itoa(len(got))
	}
	return ""
}

func TestVerbatim_EditExactMatchSurvivesPipeline(t *testing.T) {
	payload := realisticReadPayload()

	engine := newTestEngine(headroom.Config{
		Enabled:               true,
		SmartCrusher:          true,
		TabularArrays:         true,
		CodeCompressor:        true,
		LiveTurns:             2,
		PreserveVerbatimReads: true,
		CCR: headroom.CCRConfig{
			Enabled:       true,
			MinChunkBytes: 2048,
		},
	})

	req := readEditRequest(payload)

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	got := toolResultText(t, req, 2)
	if got != payload {
		t.Errorf("Read payload mutated; Edit old_string drawn from it cannot match disk.\nfirst divergence:\n%s", firstDiff(payload, got))
	}
}

func TestVerbatim_DisabledByConfig(t *testing.T) {
	payload := realisticReadPayload()

	engine := newTestEngine(headroom.Config{
		Enabled:               true,
		SmartCrusher:          true,
		CodeCompressor:        true,
		LiveTurns:             2,
		PreserveVerbatimReads: false,
		CCR: headroom.CCRConfig{
			Enabled:       true,
			MinChunkBytes: 2048,
		},
	})

	req := readEditRequest(payload)
	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	got := toolResultText(t, req, 2)
	if got == payload {
		t.Error("with PreserveVerbatimReads=false the payload must be rewritten as before")
	}
}

// --- CCR pipeline integration tests ------------------------------------------

func TestEngine_CCRStoresOriginalBeforeCompression(t *testing.T) {
	prettyJSON := "{\n  \"field\": 1,\n  \"nested\": [\n    " + strings.Repeat("\"long_data_item\",\n    ", 150) + "\"end\"\n  ]\n}"
	cfg := headroom.Config{
		Enabled:        true,
		SmartCrusher:   true,
		CodeCompressor: true,
		LiveTurns:      1,
		CCR: headroom.CCRConfig{
			Enabled:       true,
			MinChunkBytes: 500,
		},
	}
	store := ccr.NewCCRStoreFromMB(cfg.CCR.MaxStoreMB)
	engine := headroom.NewEngine(cfg, nil, testStages(store)...)

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

	chunkID := ccr.ChunkID(prettyJSON)
	_, found := store.Get(chunkID)
	if !found {
		t.Fatalf("chunk not found in engine store")
	}
}

// --- Continuation pipeline integration tests ---------------------------------

func TestEngine_EffortRoutingKeepsInspector(t *testing.T) {
	newReq := func() map[string]any {
		return map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": 16000},
			"messages": []any{
				map[string]any{"role": "user", "content": "edit the file"},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "tool_use", "id": "tu_1", "name": "Edit",
						"input": map[string]any{"file_path": "/repo/main.go"}},
				}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tu_1",
						"content": "Applied 1 edit."},
				}},
			},
		}
	}

	cfg := headroom.Config{
		Enabled:               true,
		PreserveVerbatimReads: false,
		OutputShaper: headroom.OutputShaperConfig{
			Enabled:                  true,
			EffortRouting:            true,
			MechanicalThinkingBudget: 1024,
		},
	}

	engine := newTestEngine(cfg)
	reqCtx, err := engine.Process(context.Background(), newReq())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reqCtx.ContinuationKind != "coding" {
		t.Errorf("continuation = %q; want \"coding\" with PreserveVerbatimReads off", reqCtx.ContinuationKind)
	}
	if reqCtx.EffortClamped {
		t.Error("coding continuation must keep its thinking budget")
	}
}

// --- Benchmark integration tests ---------------------------------------------

func generateBenchmarkJSON(itemCount int) string {
	items := make([]string, itemCount)
	for i := 0; i < itemCount; i++ {
		items[i] = fmt.Sprintf("{\n  \"id\": %d,\n  \"name\": \"item_%d\",\n  \"value\": %f,\n  \"active\": true\n}", i, i, float64(i)*1.5)
	}
	return "[\n  " + strings.Join(items, ",\n  ") + "\n]"
}

func generateBenchmarkLog(lineCount int) string {
	lines := make([]string, lineCount)
	for i := 0; i < lineCount; i++ {
		lines[i] = fmt.Sprintf("2026-08-28T12:00:%02d.000Z INFO [service_%d] Processing request id=%d status=200\n\n", i%60, i%5, i)
	}
	return strings.Join(lines, "")
}

func generateBenchmarkTranscript(numMessages int) map[string]any {
	msgs := make([]any, 0, numMessages)
	for i := 0; i < numMessages; i += 2 {
		tuID := fmt.Sprintf("call_%d", i)
		toolName := "Read"
		if i%4 == 0 {
			toolName = "Glob"
		} else if i%6 == 0 {
			toolName = "Bash"
		}
		msgs = append(msgs, map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"id":   tuID,
					"name": toolName,
					"input": map[string]any{
						"file_path": fmt.Sprintf("/path/to/file_%d.go", i),
					},
				},
			},
		})

		var content string
		if toolName == "Read" {
			var sb strings.Builder
			for line := 1; line <= 100; line++ {
				sb.WriteString(fmt.Sprintf("  %d\tfunc line%d() { println(%d) }\n", line, line, line))
			}
			content = sb.String()
		} else if toolName == "Bash" {
			content = generateBenchmarkLog(100)
		} else {
			content = generateBenchmarkJSON(20)
		}

		msgs = append(msgs, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": tuID,
					"content":     content,
				},
			},
		})
	}
	return map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": float64(8192),
		"messages":   msgs,
	}
}

func BenchmarkEngine_Process(b *testing.B) {
	engine := newTestEngine(headroom.Config{
		Enabled:        true,
		SmartCrusher:   true,
		TabularArrays:  true,
		CodeCompressor: true,
		LiveTurns:      2,
		OutputShaper: headroom.OutputShaperConfig{
			Enabled:                  true,
			VerbositySteering:        true,
			EffortRouting:            true,
			MechanicalThinkingBudget: 1024,
		},
	})

	jsonData := generateBenchmarkJSON(50)
	logData := generateBenchmarkLog(200)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		req := map[string]any{
			"model":      "claude-3-5-sonnet",
			"max_tokens": float64(8192),
			"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
			"messages": []any{
				map[string]any{"role": "user", "content": "run task"},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "tool_use", "id": "tu_1", "name": "fetch"},
				}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": jsonData},
					map[string]any{"type": "tool_result", "tool_use_id": "tu_2", "content": logData},
				}},
			},
		}

		ctx := context.Background()
		reqCtx, err := engine.Process(ctx, req)
		if err != nil || reqCtx.BytesBefore <= reqCtx.BytesAfter {
			b.Fatalf("process failed or no compression: %v", err)
		}
	}
}

func BenchmarkEngineProcess_WithVerbatim(b *testing.B) {
	engine := newTestEngine(headroom.Config{
		Enabled:               true,
		SmartCrusher:          true,
		TabularArrays:         true,
		CodeCompressor:        true,
		PreserveVerbatimReads: true,
		LiveTurns:             2,
		OutputShaper: headroom.OutputShaperConfig{
			Enabled:                  true,
			VerbositySteering:        true,
			EffortRouting:            true,
			MechanicalThinkingBudget: 1024,
		},
	})

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		reqCopy := generateBenchmarkTranscript(40)
		ctx := context.Background()
		_, err := engine.Process(ctx, reqCopy)
		if err != nil {
			b.Fatalf("process failed: %v", err)
		}
	}
}
