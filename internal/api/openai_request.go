package api

import (
	"encoding/json"
	"fmt"
)

// translateOpenAIRequest converts a decoded OpenAI Chat Completions request
// body into the Anthropic Messages shape the dispatch pipeline consumes. The
// pipeline in server.messages() takes over from there (model mapping,
// headroom, provider routing, retries, cost tracking).
func translateOpenAIRequest(openaiRequest map[string]any) (map[string]any, error) {
	anthropic := map[string]any{}
	if model := stringFrom(openaiRequest["model"]); model != "" {
		anthropic["model"] = model
	}

	rawMessages, _ := openaiRequest["messages"].([]any)
	anthropicMessages := make([]any, 0, len(rawMessages))
	var systemParts []string
	for _, rawMessage := range rawMessages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		role := stringFrom(message["role"])
		var blocks []any
		switch role {
		case "system", "developer":
			systemParts = append(systemParts, openAIContentToText(message["content"]))
			continue
		case "user":
			blocks = openAIContentToBlocks(message["content"])
		case "assistant":
			blocks = openAIContentToBlocks(message["content"])
			// tool_calls is optional on assistant messages; an absent or null
			// value must not trip the type assertion.
			if toolCalls, ok := message["tool_calls"].([]any); ok {
				for _, rawToolCall := range toolCalls {
					toolUse, err := openAIToolCallToToolUse(rawToolCall)
					if err != nil {
						return nil, err
					}
					if toolUse != nil {
						blocks = append(blocks, toolUse)
					}
				}
			}
		case "tool":
			role = "user"
			blocks = []any{openAIToolMessageToToolResult(message)}
		default:
			continue
		}
		anthropicMessages = appendMessage(anthropicMessages, role, blocks)
	}
	if len(systemParts) > 0 {
		anthropic["system"] = joinNonEmpty(systemParts, "\n\n")
	}
	anthropic["messages"] = anthropicMessages

	if tools, ok := openaiRequest["tools"].([]any); ok && len(tools) > 0 {
		anthropicTools := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			function, _ := tool["function"].(map[string]any)
			if function == nil || stringFrom(tool["type"]) != "function" {
				continue
			}
			anthropicTool := map[string]any{"name": function["name"]}
			if description := stringFrom(function["description"]); description != "" {
				anthropicTool["description"] = description
			}
			if parameters, ok := function["parameters"]; ok && parameters != nil {
				anthropicTool["input_schema"] = parameters
			}
			anthropicTools = append(anthropicTools, anthropicTool)
		}
		if len(anthropicTools) > 0 {
			anthropic["tools"] = anthropicTools
		}
	}

	if toolChoice := translateOpenAIToolChoice(openaiRequest["tool_choice"]); toolChoice != nil {
		anthropic["tool_choice"] = toolChoice
	}

	if rawMaxTokens, exists := openaiRequest["max_tokens"]; exists {
		if maxTokens := numberToInt(rawMaxTokens); maxTokens > 0 {
			anthropic["max_tokens"] = maxTokens
		}
	} else if rawMaxCompletionTokens, exists := openaiRequest["max_completion_tokens"]; exists {
		if maxTokens := numberToInt(rawMaxCompletionTokens); maxTokens > 0 {
			anthropic["max_tokens"] = maxTokens
		}
	}
	// No default: when the client omits the limit, the messages handler
	// derives it from the model's own limits or omits it upstream.

	if temperature, exists := openaiRequest["temperature"]; exists {
		anthropic["temperature"] = temperature
	}
	if topP, exists := openaiRequest["top_p"]; exists {
		anthropic["top_p"] = topP
	}
	switch stop := openaiRequest["stop"].(type) {
	case string:
		if stop != "" {
			anthropic["stop_sequences"] = []any{stop}
		}
	case []any:
		if len(stop) > 0 {
			anthropic["stop_sequences"] = stop
		}
	}
	if stream, exists := openaiRequest["stream"]; exists {
		anthropic["stream"] = stream
	}

	return anthropic, nil
}

// appendMessage appends one translated message, merging into the previous
// kept message when the role matches. The Anthropic Messages API requires
// strictly alternating roles; OpenAI sequences that violate this (parallel
// role:"tool" results, back-to-back user messages) must collapse into one
// message with multiple content blocks.
func appendMessage(messages []any, role string, blocks []any) []any {
	if len(messages) > 0 {
		if last, ok := messages[len(messages)-1].(map[string]any); ok && stringFrom(last["role"]) == role {
			if lastBlocks, ok := last["content"].([]any); ok {
				last["content"] = append(lastBlocks, blocks...)
				return messages
			}
		}
	}
	return append(messages, map[string]any{"role": role, "content": blocks})
}

// openAIContentToBlocks converts OpenAI message content (string, null, or a
// parts array) into Anthropic content blocks. Unsupported part types are
// dropped rather than rejected.
func openAIContentToBlocks(content any) []any {
	blocks := []any{}
	switch typed := content.(type) {
	case string:
		blocks = append(blocks, map[string]any{"type": "text", "text": typed})
	case []any:
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if partType := stringFrom(part["type"]); partType == "text" {
				text, _ := part["text"].(string)
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		}
	}
	return blocks
}

// openAIContentToText flattens message content to plain text for system
// prompts.
func openAIContentToText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return joinNonEmpty(parts, "\n\n")
	default:
		return ""
	}
}

// openAIToolCallToToolUse converts one OpenAI tool_call (arguments is a JSON
// string) into an Anthropic tool_use block (input is an object).
func openAIToolCallToToolUse(rawToolCall any) (map[string]any, error) {
	toolCall, ok := rawToolCall.(map[string]any)
	if !ok {
		return nil, nil
	}
	function, _ := toolCall["function"].(map[string]any)
	if function == nil {
		return nil, nil
	}
	name := stringFrom(function["name"])
	var input any
	if raw := stringFrom(function["arguments"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return nil, fmt.Errorf("invalid_request_error: tool_call %q arguments is not valid JSON: %w", toolCall["id"], err)
		}
	}
	toolUse := map[string]any{"type": "tool_use", "id": toolCall["id"], "name": name}
	if inputMap, ok := input.(map[string]any); ok {
		toolUse["input"] = inputMap
	} else if input != nil {
		toolUse["input"] = map[string]any{"value": input}
	} else {
		toolUse["input"] = map[string]any{}
	}
	return toolUse, nil
}

// translateOpenAIToolChoice maps the OpenAI tool_choice forms to the
// Anthropic equivalents. Unrecognized values are omitted (backend default).
func translateOpenAIToolChoice(raw any) any {
	switch typed := raw.(type) {
	case string:
		switch typed {
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		if typed["type"] == "function" {
			if function, ok := typed["function"].(map[string]any); ok {
				if name := stringFrom(function["name"]); name != "" {
					return map[string]any{"type": "tool", "name": name}
				}
			}
		}
	}
	return nil
}

// openAIToolMessageToToolResult converts a role:"tool" message into a
// tool_result content block.
func openAIToolMessageToToolResult(message map[string]any) map[string]any {
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": stringFrom(message["tool_call_id"]),
		"content":     openAIContentToBlocks(message["content"]),
	}
}

// joinNonEmpty joins strings with sep, skipping empty entries.
func joinNonEmpty(parts []string, sep string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	out := ""
	for i, part := range kept {
		if i > 0 {
			out += sep
		}
		out += part
	}
	return out
}

// numberToInt coerces a decoded JSON number (float64) to int.
func numberToInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	default:
		return 0
	}
}
