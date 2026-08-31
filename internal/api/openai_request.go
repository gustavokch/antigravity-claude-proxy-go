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
		switch role {
		case "system", "developer":
			systemParts = append(systemParts, openAIContentToText(message["content"]))
		case "user":
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role":    "user",
				"content": openAIContentToBlocks(message["content"]),
			})
		case "assistant":
			blocks := openAIContentToBlocks(message["content"])
			for _, rawToolCall := range message["tool_calls"].([]any) {
				toolUse, err := openAIToolCallToToolUse(rawToolCall)
				if err != nil {
					return nil, err
				}
				if toolUse != nil {
					blocks = append(blocks, toolUse)
				}
			}
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role":    "assistant",
				"content": blocks,
			})
		case "tool":
			toolResult := openAIToolMessageToToolResult(message)
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role":    "user",
				"content": []any{toolResult},
			})
		}
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
	} else {
		anthropic["max_tokens"] = 4096
	}

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
