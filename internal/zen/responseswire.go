package zen

import "encoding/json"

// --- Request translation ---

// anthropicToResponsesRequest converts an Anthropic Messages request body into
// an OpenAI Responses request body for POST /v1/responses.
//
// The gate treatment mirrors anthropicToChatRequest: the upstream body always
// carries "stream": true (a non-streaming body is rejected by the Zen
// free-tier gate; non-streaming clients get the stream folded back in
// translateResponsesResponse) and the tools array always contains functions
// named exactly "bash" and "read" (client tools named "Bash"/"Read" are
// renamed; missing ones get the captured OpenCode definitions injected).
//
// Fields with no Responses equivalent are dropped rather than guessed:
// stop_sequences and thinking. max_tokens becomes max_output_tokens; sampling
// parameters pass through unchanged.
//
// The second result maps upstream tool names back to the client's names for
// response translation; nil when no rename occurred. The third result names
// the gate tool definitions that were injected because the client never
// declared them — a tool call for one has no client-side tool to resolve to
// and must be dropped from the response.
func anthropicToResponsesRequest(req map[string]any) (map[string]any, map[string]string, map[string]bool) {
	renames := buildToolRenames(req["tools"])
	model, _ := req["model"].(string)
	out := map[string]any{"model": StripOpencodePrefix(model)}

	// The Responses API takes a flat item list rather than a role-tagged
	// message array: system/assistant/user turns become messages, tool_use
	// becomes function_call, tool_result becomes function_call_output.
	input := make([]any, 0, len(anySlice(req["messages"]))+1)
	if sys := systemText(req["system"]); sys != "" {
		input = append(input, map[string]any{
			"role":    "system",
			"content": []any{map[string]any{"type": "input_text", "text": sys}},
		})
	}
	for _, raw := range anySlice(req["messages"]) {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "assistant" {
			input = append(input, assistantToResponses(msg["content"], renames)...)
		} else {
			input = append(input, userToResponses(msg["content"])...)
		}
	}
	out["input"] = input

	if v, ok := req["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := req[key]; ok {
			out[key] = v
		}
	}
	// stop_sequences and thinking have no Responses equivalent and are
	// deliberately dropped rather than approximated.
	out["stream"] = true

	// toolsToChat and ensureGateTools already produce the exact tool set the
	// gate demands; the mapping is a shape change only (Responses tools are
	// flat, Chat-Completions tools nest the definition under "function").
	chatTools, injected := ensureGateTools(toolsToChat(req["tools"], renames))
	tools := make([]any, 0, len(chatTools))
	for _, t := range chatTools {
		tools = append(tools, chatToolToResponses(t))
	}
	out["tools"] = tools
	if tc := toolChoiceToResponses(req["tool_choice"], renames); tc != nil {
		out["tool_choice"] = tc
	}

	rev := make(map[string]string, len(renames))
	for client, upstream := range renames {
		rev[upstream] = client
	}
	if len(rev) == 0 {
		rev = nil
	}
	return out, rev, injected
}

// anySlice returns v as a []any, or nil when it is any other type.
func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// chatToolToResponses reshapes one Chat-Completions tool
// ({"type":"function","function":{...}}) into the flat Responses shape
// ({"type":"function","name":...,"parameters":...}).
func chatToolToResponses(t any) any {
	tool, _ := t.(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	out := map[string]any{"type": "function", "name": fn["name"]}
	if d, ok := fn["description"].(string); ok && d != "" {
		out["description"] = d
	}
	if p, ok := fn["parameters"]; ok {
		out["parameters"] = p
	}
	return out
}

// toolChoiceToResponses maps an Anthropic tool_choice to the Responses one.
// "any" becomes "required" (Responses has no separate "any" spelling), and a
// forced tool names the renamed upstream function.
func toolChoiceToResponses(v any, renames map[string]string) any {
	tc, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch tc["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := tc["name"].(string)
		return map[string]any{"type": "function", "name": renameTool(renames, name)}
	}
	return nil
}

// userToResponses maps one Anthropic user turn to Responses input items. A
// tool_result block becomes a function_call_output item and is emitted before
// the user message so the wire order stays input → output_answer; the
// remaining text/image parts become one user message.
func userToResponses(content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": s}},
		}}
	}
	blocks := anySlice(content)
	parts := make([]any, 0, len(blocks))
	var outputs []any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": text})
			}
		case "image":
			if u := imageURL(block["source"]); u != "" {
				parts = append(parts, map[string]any{"type": "input_image", "image_url": u})
			}
		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			output := toolResultText(block["content"])
			if isErr, _ := block["is_error"].(bool); isErr && output != "" {
				output = "Error: " + output
			}
			outputs = append(outputs, map[string]any{
				"type":    "function_call_output",
				"call_id": id,
				"output":  output,
			})
		}
	}
	out := outputs
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": parts})
	}
	if len(out) == 0 {
		// Every block was dropped (e.g. an image source with no Responses
		// equivalent). Emit an empty user message so the turn boundary
		// survives.
		out = append(out, map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": ""}},
		})
	}
	return out
}

// assistantToResponses maps one Anthropic assistant turn to Responses input
// items: thinking becomes a reasoning item, text becomes an assistant message,
// and tool_use becomes a function_call whose arguments are the JSON-encoded
// tool input. Items are ordered reasoning → message → function_call, which is
// the order the Responses API replays a prior assistant turn in.
func assistantToResponses(content any, renames map[string]string) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": s}},
		}}
	}
	blocks := anySlice(content)
	var reasoning, parts, calls []any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "thinking":
			if text, _ := block["thinking"].(string); text != "" {
				reasoning = append(reasoning, map[string]any{
					"type":    "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": text}},
				})
			}
		case "text":
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, map[string]any{"type": "output_text", "text": text})
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			encoded, err := json.Marshal(block["input"])
			if err != nil {
				encoded = []byte("{}")
			}
			calls = append(calls, map[string]any{
				"type":      "function_call",
				"call_id":   id,
				"name":      renameTool(renames, name),
				"arguments": string(encoded),
			})
		}
	}
	out := make([]any, 0, len(reasoning)+len(parts)+len(calls))
	out = append(out, reasoning...)
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "assistant", "content": parts})
	}
	return append(out, calls...)
}
