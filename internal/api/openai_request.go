package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// translatedOpenAIRequest holds the translated Anthropic Messages payload and
// metadata extracted during request translation (such as synthetic tools
// injected for response_format emulation).
type translatedOpenAIRequest struct {
	Anthropic          map[string]any
	StructuredToolName string
}

// translateOpenAIRequest converts a decoded OpenAI Chat Completions request
// body into the Anthropic Messages shape the dispatch pipeline consumes. The
// pipeline in server.messages() takes over from there (model mapping,
// headroom, provider routing, retries, cost tracking).
//
// Structured output emulation is backend-agnostic: response_format schemas are
// injected as standard Anthropic tools and forced via tool_choice regardless of
// the backend route (Cloud Code, OpenRouter, Custom Endpoints), and unwrapped
// back into OpenAI content by openAIResponseWriter on the response side.
func translateOpenAIRequest(openaiRequest map[string]any) (translatedOpenAIRequest, error) {
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
			if rawToolCalls, exists := message["tool_calls"]; exists && rawToolCalls != nil {
				toolCalls, ok := rawToolCalls.([]any)
				if !ok {
					slog.Warn("openai translate: assistant tool_calls is not an array, skipping",
						"type", fmt.Sprintf("%T", rawToolCalls))
				} else {
					for _, rawToolCall := range toolCalls {
						toolUse, err := openAIToolCallToToolUse(rawToolCall)
						if err != nil {
							return translatedOpenAIRequest{}, err
						}
						if toolUse != nil {
							blocks = append(blocks, toolUse)
						}
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
	// response_format has no upstream counterpart and must be emulated here;
	// the instruction form appends to the system prompt, so it is resolved
	// before the system block is sealed.
	structuredToolName, structuredInstruction := structuredOutputEmulation(openaiRequest)
	if structuredInstruction != "" {
		systemParts = append(systemParts, structuredInstruction)
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
			// parameters is optional on the OpenAI shape (it defaults to an
			// empty object schema), but input_schema is required upstream.
			if parameters, ok := function["parameters"]; ok && parameters != nil {
				anthropicTool["input_schema"] = stripUnenforcedSchemaKeywords(parameters)
			} else {
				anthropicTool["input_schema"] = map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				}
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

	// The forced-tool emulation runs only when the client sent no tools of its
	// own, so it can claim both the tools array and tool_choice outright.
	if structuredToolName != "" {
		format, _ := openaiRequest["response_format"].(map[string]any)
		spec, _ := format["json_schema"].(map[string]any)
		syntheticTool := map[string]any{
			"name":         structuredToolName,
			"input_schema": stripUnenforcedSchemaKeywords(spec["schema"]),
		}
		description := stringFrom(spec["description"])
		if description == "" {
			description = "Return the final answer as structured data matching this schema. You must call this tool."
		}
		syntheticTool["description"] = description
		anthropic["tools"] = []any{syntheticTool}
		anthropic["tool_choice"] = map[string]any{"type": "tool", "name": structuredToolName}
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

	return translatedOpenAIRequest{
		Anthropic:          anthropic,
		StructuredToolName: structuredToolName,
	}, nil
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
			} else if partType == "image_url" || partType == "input_image" {
				image := openAIImageURLToImageBlock(part["image_url"])
				if image == nil {
					image = openAIImageURLToImageBlock(part)
				}
				if image != nil {
					blocks = append(blocks, image)
				}
			} else if partType == "image" {
				if source, ok := part["source"].(map[string]any); ok && len(source) > 0 {
					blocks = append(blocks, part)
				} else if image := openAIImageURLToImageBlock(part); image != nil {
					blocks = append(blocks, image)
				}
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

// openAIImageURLToImageBlock converts an OpenAI image_url part
// ({"url": ...}) into an Anthropic image block. Data URIs become base64
// sources; remote URLs become url sources. Returns nil when no usable URL.
func openAIImageURLToImageBlock(raw any) map[string]any {
	url := ""
	switch typed := raw.(type) {
	case string:
		url = typed
	case map[string]any:
		url = stringFrom(typed["url"])
		// Responses-style or flat nesting: image_url is an object or string.
		if url == "" {
			if nested, ok := typed["image_url"].(map[string]any); ok {
				url = stringFrom(nested["url"])
			} else if s := stringFrom(typed["image_url"]); s != "" {
				url = s
			}
		}
		if url == "" {
			if source, ok := typed["source"].(map[string]any); ok {
				url = stringFrom(source["url"])
			}
		}
	}
	if url == "" {
		return nil
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	if strings.HasPrefix(url, "{") {
		return nil
	}
	if rest, ok := cutPrefixFold(url, "data:"); ok {
		idx := strings.Index(strings.ToLower(rest), ";base64,")
		if idx < 0 {
			return nil
		}
		rawMime, rawData := rest[:idx], rest[idx+len(";base64,"):]
		data := strings.Join(strings.Fields(rawData), "")
		if data == "" {
			return nil
		}
		mimeType, _, _ := strings.Cut(rawMime, ";")
		mimeType = strings.ToLower(strings.TrimSpace(mimeType))
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": mimeType, "data": data,
			},
		}
	}
	if !isHTTPURL(url) {
		return nil
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": url},
	}
}

// cutPrefixFold cuts prefix case-insensitively, preserving the rest verbatim.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// isHTTPURL reports whether url uses http or https scheme.
func isHTTPURL(url string) bool {
	lower := strings.ToLower(url)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
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

// --- response_format emulation ---
//
// The OpenAI `response_format` field has no counterpart on the Anthropic
// Messages upstream this proxy forwards to (including OpenRouter's
// Anthropic-compatible /v1/messages endpoint), so it cannot be passed through:
// it has to be emulated at the edge or the constraint is simply lost.

// defaultStructuredToolName names the synthetic tool when the client supplied
// no usable json_schema name.
const defaultStructuredToolName = "structured_output"

// structuredOutputEmulation decides how one response_format request is
// emulated, and is the single source of truth for that decision: the request
// translator and the response translator both call it so they always agree on
// whether a synthetic tool is in play.
//
// json_schema with no client tools returns a toolName. The schema is injected
// as a synthetic tool and forced via tool_choice, so the model must emit it and
// the answer arrives as structured tool_use input, which the response side
// unwraps back into message.content. This is real enforcement wherever the
// provider honours forced tool choice.
//
// json_schema alongside the client's own tools returns an instruction instead.
// Forcing a synthetic tool there would make the client's tools unreachable and
// break an agentic loop, so the schema is described in the system prompt. Best
// effort, and weaker than the forced path.
//
// json_object carries no schema at all, so it is always an instruction.
//
// Both return values are empty when the request asks for no structured output.
func structuredOutputEmulation(openaiRequest map[string]any) (toolName string, instruction string) {
	format, ok := openaiRequest["response_format"].(map[string]any)
	if !ok {
		return "", ""
	}
	switch stringFrom(format["type"]) {
	case "json_object":
		return "", "Respond with a single valid JSON object. Output the object only: no prose, no code fences."
	case "json_schema":
		break
	default:
		return "", ""
	}

	spec, _ := format["json_schema"].(map[string]any)
	if spec == nil {
		return "", ""
	}
	schema, hasSchema := spec["schema"]
	if !hasSchema || schema == nil {
		return "", ""
	}
	name := sanitizeToolName(stringFrom(spec["name"]))

	if clientTools, ok := openaiRequest["tools"].([]any); ok && len(clientTools) > 0 {
		encoded, err := json.Marshal(schema)
		if err != nil {
			return "", ""
		}
		return "", "Respond with a single JSON object that validates against the \"" + name +
			"\" JSON Schema below. Output the object only: no prose, no code fences.\n\n" + string(encoded)
	}
	return name, ""
}

// sanitizeToolName coerces a json_schema name into the character set the
// Anthropic tools API accepts (letters, digits, underscore, hyphen; 1-64
// chars), falling back to the default when nothing usable survives.
func sanitizeToolName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return defaultStructuredToolName
	}
	return b.String()
}

// stripUnenforcedSchemaKeywords removes JSON Schema keywords that OpenAI's
// strict mode adds but that nothing downstream of this proxy enforces.
//
// `strict: true` lives on the OpenAI function object and is dropped by the
// translation above, because the Anthropic tool shape has no such flag and no
// grammar engine is engaged anywhere on this path. Its companion keyword
// `additionalProperties: false` would otherwise survive into input_schema,
// leaving the request paying the schema weight of a closed object with none of
// the enforcement — measured as a raised tool-bypass rate on small models. The
// Cloud Code path already strips it (format.SanitizeSchema); this does the same
// for the OpenRouter path, narrowly, leaving every other keyword intact.
func stripUnenforcedSchemaKeywords(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, raw := range typed {
			if key == "additionalProperties" {
				continue
			}
			cleaned[key] = stripUnenforcedSchemaKeywords(raw)
		}
		return cleaned
	case []any:
		cleaned := make([]any, 0, len(typed))
		for _, item := range typed {
			cleaned = append(cleaned, stripUnenforcedSchemaKeywords(item))
		}
		return cleaned
	default:
		return value
	}
}
