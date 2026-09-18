package classifier

import (
	"bytes"
	"encoding/json"
)

// encodeCompactJSON encodes value as JSON with HTML escaping disabled and trailing whitespace trimmed.
func encodeCompactJSON(v any) ([]byte, error) {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// anthropicEnvelope builds the standard Anthropic Messages API message envelope.
func anthropicEnvelope(id, model, text, stopReason string, inputTokens, outputTokens int) map[string]any {
	return map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]any{{"type": "text", "text": text}},
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
}
