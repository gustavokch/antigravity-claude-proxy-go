package api

import (
	"encoding/json"
	"testing"
)

func TestCCRStreamState_Basic(t *testing.T) {
	state := newCCRStreamState(0)

	// Block 0: text
	down, emit := state.StartBlock(0, map[string]any{"type": "text", "text": ""})
	if !emit || down != 0 {
		t.Fatalf("expected emit=true, down=0; got emit=%v, down=%d", emit, down)
	}

	state.AppendText(0, "Hello ")
	state.AppendText(0, "World")

	down, emit = state.MapIndex(0)
	if !emit || down != 0 {
		t.Fatalf("expected delta emit=true, down=0; got emit=%v, down=%d", emit, down)
	}

	// Block 1: headroom_retrieve (suppressed)
	down, emit = state.StartBlock(1, map[string]any{"type": "tool_use", "id": "call_1", "name": "headroom_retrieve"})
	if emit {
		t.Fatalf("expected headroom_retrieve to be suppressed; got emit=true, down=%d", down)
	}

	state.AppendJSON(1, `{"chunk_id":`)
	state.AppendJSON(1, `"chk_abc123"}`)

	down, emit = state.MapIndex(1)
	if emit {
		t.Fatalf("expected delta for suppressed block to be suppressed; got emit=true, down=%d", down)
	}

	// Block 2: regular tool_use (e.g. Read)
	down, emit = state.StartBlock(2, map[string]any{"type": "tool_use", "id": "call_2", "name": "Read"})
	if !emit || down != 1 {
		t.Fatalf("expected Read tool_use emit=true, down=1; got emit=%v, down=%d", emit, down)
	}

	state.AppendJSON(2, `{"file_path":"main.go"}`)
	down, emit = state.MapIndex(2)
	if !emit || down != 1 {
		t.Fatalf("expected Read delta emit=true, down=1; got emit=%v, down=%d", emit, down)
	}

	// Visible count should be 2 (blocks 0 and 2 mapped to 0 and 1)
	if visible := state.VisibleCount(); visible != 2 {
		t.Fatalf("expected visible count 2; got %d", visible)
	}

	// Finalize should return retrieve calls
	retrieves := state.Finalize()
	if len(retrieves) != 1 {
		t.Fatalf("expected 1 retrieve call; got %d", len(retrieves))
	}
	input, ok := retrieves[0]["input"].(map[string]any)
	if !ok || input["chunk_id"] != "chk_abc123" {
		t.Fatalf("expected chunk_id chk_abc123; got %v", input)
	}

	// Assistant blocks should have 3 blocks (text with appended text, retrieve with parsed input, Read with parsed input)
	assistantBlocks := state.AssistantBlocks()
	if len(assistantBlocks) != 3 {
		t.Fatalf("expected 3 assistant blocks; got %d", len(assistantBlocks))
	}
	b0 := assistantBlocks[0].(map[string]any)
	if b0["text"] != "Hello World" {
		t.Fatalf("expected text 'Hello World'; got %v", b0["text"])
	}
}

func TestCCRStreamState_MultiIteration(t *testing.T) {
	// Iteration 0: produces 1 text block and 1 retrieve block
	state0 := newCCRStreamState(0)
	d0, e0 := state0.StartBlock(0, map[string]any{"type": "text", "text": "Thought"})
	if !e0 || d0 != 0 {
		t.Fatalf("iter0 block 0: e=%v, d=%d", e0, d0)
	}
	_, e1 := state0.StartBlock(1, map[string]any{"type": "tool_use", "name": "headroom_retrieve", "id": "t1"})
	if e1 {
		t.Fatalf("iter0 block 1 should be suppressed")
	}
	_ = state0.Finalize()

	base1 := state0.VisibleCount()
	if base1 != 1 {
		t.Fatalf("expected base1=1; got %d", base1)
	}

	// Iteration 1: produces 1 text block
	state1 := newCCRStreamState(base1)
	d1_0, e1_0 := state1.StartBlock(0, map[string]any{"type": "text", "text": "Final answer"})
	if !e1_0 || d1_0 != 1 {
		t.Fatalf("iter1 block 0: expected e=true, d=1; got e=%v, d=%d", e1_0, d1_0)
	}
	if visible := state1.VisibleCount(); visible != 1 {
		t.Fatalf("iter1 visible count: expected 1; got %d", visible)
	}
}

func TestCCRStreamState_OrphanEvents(t *testing.T) {
	state := newCCRStreamState(0)

	// Delta/stop for index 5 which never had a StartBlock
	_, emit := state.MapIndex(5)
	if emit {
		t.Fatalf("expected orphan delta to be dropped")
	}
	if state.orphanEvents != 1 {
		t.Fatalf("expected orphanEvents=1; got %d", state.orphanEvents)
	}
}

func TestStripRetrieveBlocks(t *testing.T) {
	resp := map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "Hello"},
			map[string]any{"type": "tool_use", "name": "headroom_retrieve", "id": "call_1"},
			map[string]any{"type": "tool_use", "name": "Read", "id": "call_2"},
		},
	}

	stripRetrieveBlocks(resp)

	content := resp["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks after strip; got %d", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("expected block 0 to be text")
	}
	if content[1].(map[string]any)["name"] != "Read" {
		t.Fatalf("expected block 1 to be Read tool_use")
	}
}

func TestStripRetrieveBlocksJSON(t *testing.T) {
	raw := `{"role":"assistant","content":[{"type":"text","text":"Hi"},{"type":"tool_use","name":"headroom_retrieve","id":"c1","input":{"chunk_id":"123"}}]}`
	stripped := stripRetrieveBlocksJSON([]byte(raw))

	var parsed map[string]any
	if err := json.Unmarshal(stripped, &parsed); err != nil {
		t.Fatalf("unmarshal stripped error: %v", err)
	}
	content := parsed["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block; got %d", len(content))
	}
	if content[0].(map[string]any)["text"] != "Hi" {
		t.Fatalf("expected block to be text 'Hi'")
	}
}
