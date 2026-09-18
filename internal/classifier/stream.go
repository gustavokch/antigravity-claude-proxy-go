package classifier

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteSyntheticStream re-emits a completed Anthropic Messages response as
// the SSE event sequence a streaming client expects. A rerouted classifier
// call is answered non-streaming by its backend, but Claude Code may have
// asked for stream:true; without these frames the client waits forever.
//
// flush may be nil (tests, buffered writers). In a real handler it must be
// http.Flusher.Flush, or the client sees nothing until the handler returns.
func WriteSyntheticStream(w io.Writer, flush func(), message []byte) error {
	var msg struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(message, &msg); err != nil {
		return fmt.Errorf("classifier: decode message for streaming: %w", err)
	}

	text := ""
	for _, block := range msg.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	stopReason := msg.StopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	frames := []struct {
		event   string
		payload map[string]any
	}{
		{"message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            msg.ID,
				"type":          "message",
				"role":          "assistant",
				"model":         msg.Model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  msg.Usage.InputTokens,
					"output_tokens": 0,
				},
			},
		}},
		{"content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		}},
		{"content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}},
		{"content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		}},
		{"message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": msg.Usage.OutputTokens},
		}},
		{"message_stop", map[string]any{"type": "message_stop"}},
	}

	for _, frame := range frames {
		data, err := encodeCompactJSON(frame.payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.event, data); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
	}
	return nil
}
