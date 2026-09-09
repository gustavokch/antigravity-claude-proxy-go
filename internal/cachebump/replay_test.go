package cachebump

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const replayFixture = `{
  "model": "claude-sonnet-5",
  "max_tokens": 64000,
  "stream": true,
  "temperature": 0.7,
  "thinking": {"type": "enabled", "budget_tokens": 8000},
  "tool_choice": {"type": "tool", "name": "search"},
  "metadata": {"user_id": "user_123"},
  "system": [
    {"type": "text", "text": "You are helpful.", "cache_control": {"type": "ephemeral"}}
  ],
  "tools": [
    {"name": "search", "input_schema": {"type": "object", "properties": {}}}
  ],
  "messages": [
    {"role": "user", "content": [
      {"type": "text", "text": "hello", "cache_control": {"type": "ephemeral"}}
    ]}
  ]
}`

func TestBuildReplayBody_PreservesCachePrefixByteForByte(t *testing.T) {
	got, err := BuildReplayBody([]byte(replayFixture), 1)
	if err != nil {
		t.Fatalf("BuildReplayBody: %v", err)
	}

	var orig, out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(replayFixture), &orig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"system", "tools", "messages"} {
		if !bytes.Equal(orig[field], out[field]) {
			t.Errorf("field %q not byte-identical:\norig: %s\ngot:  %s", field, orig[field], out[field])
		}
	}
}

func TestBuildReplayBody_SetsBumpFields(t *testing.T) {
	got, err := BuildReplayBody([]byte(replayFixture), 1)
	if err != nil {
		t.Fatalf("BuildReplayBody: %v", err)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}

	if string(out["max_tokens"]) != "1" {
		t.Errorf("expected max_tokens 1, got %s", out["max_tokens"])
	}
	if string(out["stream"]) != "false" {
		t.Errorf("expected stream false, got %s", out["stream"])
	}
	if _, ok := out["thinking"]; ok {
		t.Error("expected thinking dropped")
	}
	if _, ok := out["tool_choice"]; ok {
		t.Error("expected tool_choice dropped")
	}
}

func TestBuildReplayBody_PreservesOtherFields(t *testing.T) {
	got, err := BuildReplayBody([]byte(replayFixture), 1)
	if err != nil {
		t.Fatalf("BuildReplayBody: %v", err)
	}

	var orig, out map[string]json.RawMessage
	json.Unmarshal([]byte(replayFixture), &orig)
	json.Unmarshal(got, &out)

	for _, field := range []string{"model", "metadata", "temperature"} {
		if !bytes.Equal(orig[field], out[field]) {
			t.Errorf("field %q changed:\norig: %s\ngot:  %s", field, orig[field], out[field])
		}
	}
}

func TestBuildReplayBody_FloorClampedToOne(t *testing.T) {
	for _, floor := range []int{0, -5} {
		got, err := BuildReplayBody([]byte(replayFixture), floor)
		if err != nil {
			t.Fatalf("BuildReplayBody(floor=%d): %v", floor, err)
		}
		if !bytes.Contains(got, []byte(`"max_tokens":1`)) {
			t.Errorf("floor %d: expected max_tokens 1, got %s", floor, got)
		}
	}

	got, err := BuildReplayBody([]byte(replayFixture), 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"max_tokens":16`)) {
		t.Errorf("expected max_tokens 16, got %s", got)
	}
}

func TestBuildReplayBody_MissingOptionalFields(t *testing.T) {
	minimal := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	got, err := BuildReplayBody([]byte(minimal), 1)
	if err != nil {
		t.Fatalf("BuildReplayBody: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["stream"]; !ok {
		t.Error("expected stream key present")
	}
	if string(out["stream"]) != "false" {
		t.Errorf("expected stream false, got %s", out["stream"])
	}
	if !bytes.Contains(got, []byte(`"messages"`)) {
		t.Errorf("expected messages preserved, got %s", got)
	}
}

func TestBuildReplayBody_InvalidJSON(t *testing.T) {
	if _, err := BuildReplayBody([]byte("not json"), 1); err == nil {
		t.Error("expected error for invalid JSON")
	}
	if _, err := BuildReplayBody([]byte("[1,2,3]"), 1); err == nil {
		t.Error("expected error for non-object JSON")
	}
}

func TestBuildReplayBody_NestedPrefixUnchanged(t *testing.T) {
	// Deeply nested content inside messages must survive a full round-trip.
	nested := `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"z":1,"a":[2,{"k":"v"}]}}]}],"system":"plain string"}`
	got, err := BuildReplayBody([]byte(nested), 1)
	if err != nil {
		t.Fatal(err)
	}
	var orig, out map[string]json.RawMessage
	json.Unmarshal([]byte(nested), &orig)
	json.Unmarshal(got, &out)
	if !bytes.Equal(orig["messages"], out["messages"]) {
		t.Errorf("nested messages changed:\norig: %s\ngot:  %s", orig["messages"], out["messages"])
	}
	if !bytes.Equal(orig["system"], out["system"]) {
		t.Errorf("scalar system changed:\norig: %s\ngot:  %s", orig["system"], out["system"])
	}
}

func TestBuildReplayBody_LargeBody(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"`)
	for i := 0; i < 10000; i++ {
		b.WriteString("x")
	}
	b.WriteString(`","cache_control":{"type":"ephemeral"}}]}`)
	got, err := BuildReplayBody([]byte(b.String()), 1)
	if err != nil {
		t.Fatal(err)
	}
	var orig, out map[string]json.RawMessage
	json.Unmarshal([]byte(b.String()), &orig)
	json.Unmarshal(got, &out)
	if !bytes.Equal(orig["messages"], out["messages"]) {
		t.Error("large messages changed")
	}
}
