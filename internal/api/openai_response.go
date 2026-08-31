package api

import (
	"encoding/json"
	"time"
)

// --- Unary translation: Anthropic Messages response -> OpenAI chat.completion ---

// translateAnthropicMessageToOpenAI converts an Anthropic Messages response
// body into the OpenAI Chat Completions unary envelope.
func translateAnthropicMessageToOpenAI(message map[string]any, requestModel string, created int64) map[string]any {
	choiceMessage := map[string]any{"role": "assistant"}
	var textParts []string
	var reasoningParts []string
	var toolCalls []any
	for _, rawBlock := range message["content"].([]any) {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		switch stringFrom(block["type"]) {
		case "text":
			textParts = append(textParts, stringFrom(block["text"]))
		case "thinking":
			if thinking := stringFrom(block["thinking"]); thinking != "" {
				reasoningParts = append(reasoningParts, thinking)
			}
		case "tool_use":
			arguments, err := json.Marshal(block["input"])
			if err != nil {
				arguments = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block["id"],
				"type": "function",
				"function": map[string]any{
					"name":      block["name"],
					"arguments": string(arguments),
				},
			})
		}
	}
	if len(textParts) > 0 {
		choiceMessage["content"] = joinNonEmpty(textParts, "")
	}
	if len(reasoningParts) > 0 {
		choiceMessage["reasoning_content"] = joinNonEmpty(reasoningParts, "\n\n")
	}
	if len(toolCalls) > 0 {
		choiceMessage["tool_calls"] = toolCalls
	}
	if len(textParts) == 0 && len(toolCalls) == 0 && len(reasoningParts) == 0 {
		choiceMessage["content"] = ""
	}

	completion := map[string]any{
		"id":      "chatcmpl-" + stringFrom(message["id"]),
		"object":  "chat.completion",
		"created": created,
		"model":   requestModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       choiceMessage,
			"finish_reason": anthropicStopReasonToOpenAI(message["stop_reason"]),
		}},
	}
	if usage, ok := message["usage"].(map[string]any); ok {
		input := numberToInt(usage["input_tokens"])
		output := numberToInt(usage["output_tokens"])
		completion["usage"] = map[string]any{
			"prompt_tokens":     input,
			"completion_tokens": output,
			"total_tokens":      input + output,
		}
	}
	return completion
}

// anthropicStopReasonToOpenAI maps Anthropic stop_reason to the OpenAI
// finish_reason vocabulary.
func anthropicStopReasonToOpenAI(raw any) string {
	switch stringFrom(raw) {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// --- Streaming translation: Anthropic SSE events -> OpenAI chat.completion.chunk ---

// openAIStreamState converts a sequence of Anthropic SSE events into OpenAI
// chunk maps. One instance per streamed response.
type openAIStreamState struct {
	model            string
	id               string
	created          int64
	toolIndexByBk    map[int]int
	toolCount        int
	finishReason     string
	stopReasonSeen   bool
	done             bool
	promptTokens     int
	completionTokens int
}

func newOpenAIStreamState(model string) *openAIStreamState {
	return &openAIStreamState{
		model:          model,
		created:        time.Now().Unix(),
		toolIndexByBk:  map[int]int{},
		finishReason:   "stop",
		stopReasonSeen: false,
	}
}

// chunk builds one chat.completion.chunk with the given delta and finish
// reason.
func (s *openAIStreamState) chunk(delta map[string]any, finishReason any) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}
	return map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{choice},
	}
}

// HandleEvent consumes one Anthropic event and returns the OpenAI chunks to
// emit, in order. The final chunk (after message_stop) carries the
// finish_reason; the caller appends the data: [DONE] sentinel.
func (s *openAIStreamState) HandleEvent(eventType string, data map[string]any) []map[string]any {
	switch eventType {
	case "message_start":
		message, _ := data["message"].(map[string]any)
		s.id = "chatcmpl-" + stringFrom(message["id"])
		if usage, ok := message["usage"].(map[string]any); ok {
			s.promptTokens = numberToInt(usage["input_tokens"])
		}
		return []map[string]any{s.chunk(map[string]any{"role": "assistant", "content": ""}, nil)}

	case "content_block_start":
		index := numberToInt(data["index"])
		block, _ := data["content_block"].(map[string]any)
		if stringFrom(block["type"]) != "tool_use" {
			return nil
		}
		toolIndex := s.toolCount
		s.toolCount++
		s.toolIndexByBk[index] = toolIndex
		return []map[string]any{s.chunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    toolIndex,
				"id":       block["id"],
				"type":     "function",
				"function": map[string]any{"name": block["name"], "arguments": ""},
			}},
		}, nil)}

	case "content_block_delta":
		index := numberToInt(data["index"])
		delta, _ := data["delta"].(map[string]any)
		switch stringFrom(delta["type"]) {
		case "text_delta":
			text, _ := delta["text"].(string)
			return []map[string]any{s.chunk(map[string]any{"content": text}, nil)}
		case "thinking_delta":
			text, _ := delta["thinking"].(string)
			return []map[string]any{s.chunk(map[string]any{"reasoning_content": text}, nil)}
		case "input_json_delta":
			toolIndex, ok := s.toolIndexByBk[index]
			if !ok {
				return nil
			}
			partial, _ := delta["partial_json"].(string)
			return []map[string]any{s.chunk(map[string]any{
				"tool_calls": []any{map[string]any{"index": toolIndex, "arguments": partial}},
			}, nil)}
		}
		return nil

	case "message_delta":
		if delta, ok := data["delta"].(map[string]any); ok {
			if reason := stringFrom(delta["stop_reason"]); reason != "" {
				s.finishReason = anthropicStopReasonToOpenAI(reason)
				s.stopReasonSeen = true
			}
		}
		if usage, ok := data["usage"].(map[string]any); ok {
			s.completionTokens = numberToInt(usage["output_tokens"])
		}
		return nil

	case "message_stop":
		return []map[string]any{s.chunk(map[string]any{}, s.finishReason)}

	default:
		return nil
	}
}

// translateAnthropicErrorToOpenAI converts an Anthropic error envelope into
// the OpenAI error envelope.
func translateAnthropicErrorToOpenAI(anthropic map[string]any) map[string]any {
	errObj, _ := anthropic["error"].(map[string]any)
	kind := stringFrom(errObj["type"])
	message := stringFrom(errObj["message"])
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"code":    kind,
		},
	}
}
