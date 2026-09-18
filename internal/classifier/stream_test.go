package classifier

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteSyntheticStreamEmitsFullEventSequence(t *testing.T) {
	message := []byte(`{
		"id": "msg_clf_abc",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-5",
		"content": [{"type":"text","text":"<severity>0</severity>"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 100, "output_tokens": 10}
	}`)

	var out strings.Builder
	flushes := 0
	if err := WriteSyntheticStream(&out, func() { flushes++ }, message); err != nil {
		t.Fatalf("WriteSyntheticStream: %v", err)
	}

	body := out.String()
	wantOrder := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	position := 0
	for _, want := range wantOrder {
		index := strings.Index(body[position:], want)
		if index < 0 {
			t.Fatalf("missing or out-of-order event %q in:\n%s", want, body)
		}
		position += index + len(want)
	}
	if flushes != len(wantOrder) {
		t.Errorf("expected one flush per event (%d), got %d", len(wantOrder), flushes)
	}
	if !strings.Contains(body, "<severity>0</severity>") {
		t.Error("the verdict text never reached the delta frame")
	}

	// Every data: line must be valid JSON; a malformed frame would stall the
	// client just as surely as no frame at all.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatalf("invalid JSON frame %q: %v", line, err)
		}
		if payload["type"] == nil {
			t.Errorf("frame missing a type field: %q", line)
		}
	}
}

func TestWriteSyntheticStreamRejectsGarbage(t *testing.T) {
	var out strings.Builder
	if err := WriteSyntheticStream(&out, nil, []byte(`not json`)); err == nil {
		t.Fatal("expected an error for an undecodable message")
	}
}

func TestStubWithTextBuildsAnthropicEnvelope(t *testing.T) {
	stub, err := StubWithText("claude-sonnet-5", "<severity>0</severity>")
	if err != nil {
		t.Fatalf("StubWithText: %v", err)
	}
	var got struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(stub, &got); err != nil {
		t.Fatalf("unmarshal stub: %v", err)
	}
	if !strings.HasPrefix(got.ID, "msg_stub_") {
		t.Errorf("expected a msg_stub_ id, got %q", got.ID)
	}
	if got.Model != "claude-sonnet-5" {
		t.Errorf("expected the echoed model, got %q", got.Model)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "<severity>0</severity>" {
		t.Fatalf("unexpected content %+v", got.Content)
	}
}
