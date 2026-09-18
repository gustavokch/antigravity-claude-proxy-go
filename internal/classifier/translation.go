package classifier

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoChoices is returned when an OpenAI-format backend answers with an
// empty choices array. Callers treat it like any other backend failure and
// fall back to the built-in handling.
var ErrNoChoices = errors.New("classifier: openai response has no choices")

// TranslateAnthropicToOpenAI rewrites an Anthropic Messages request as an
// OpenAI Chat Completions request. The system prompt becomes a leading
// system message, and every content block is flattened to text: classifier
// calls are text-only, so nothing is lost. maxTokensOverride of 0 keeps the
// request's own cap. The result is always non-streaming — the caller
// re-emits SSE frames itself when the client asked for a stream.
func TranslateAnthropicToOpenAI(body []byte, targetModel string, maxTokensOverride int) ([]byte, error) {
	var req struct {
		MaxTokens   int             `json:"max_tokens"`
		Temperature *float64        `json:"temperature"`
		System      json.RawMessage `json:"system"`
		Messages    []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("classifier: decode anthropic request: %w", err)
	}

	messages := make([]map[string]any, 0, len(req.Messages)+1)
	if system := flattenText(req.System); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for _, message := range req.Messages {
		role := message.Role
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]any{"role": role, "content": flattenText(message.Content)})
	}

	out := map[string]any{
		"model":    targetModel,
		"messages": messages,
		"stream":   false,
	}
	maxTokens := req.MaxTokens
	if maxTokensOverride > 0 {
		maxTokens = maxTokensOverride
	}
	if maxTokens > 0 {
		out["max_tokens"] = maxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	return json.Marshal(out)
}

// TranslateOpenAIToAnthropic rewrites an OpenAI Chat Completions response as
// an Anthropic Messages response. echoModel is written into the model field
// so the client sees the model it requested rather than the backend's own id,
// which Claude Code would not recognize.
func TranslateOpenAIToAnthropic(body []byte, echoModel string) ([]byte, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("classifier: decode openai response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, ErrNoChoices
	}

	id, err := translatedMessageID()
	if err != nil {
		return nil, err
	}

	stopReason := "end_turn"
	if resp.Choices[0].FinishReason == "length" {
		stopReason = "max_tokens"
	}

	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         echoModel,
		"content":       []map[string]any{{"type": "text", "text": resp.Choices[0].Message.Content}},
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// translatedMessageID marks rerouted answers distinctly from canned stubs
// (msg_stub_), so a log reader can tell which path produced a verdict.
func translatedMessageID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "msg_clf_" + hex.EncodeToString(buf), nil
}
