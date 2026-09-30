package zen

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

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

// --- Response translation ---

// ResponsesResponseToAnthropic converts a Responses JSON body into an
// Anthropic Messages response. reasoning items become thinking blocks, message
// items become text blocks, and function_call items become tool_use blocks with
// their arguments decoded — a call to a gate-injected tool the client never
// declared is dropped, because the client has no way to resolve it.
//
// toolNames maps upstream tool names back to the client's names (the reverse
// map anthropicToResponsesRequest returns); injected names the gate tools
// ensureGateTools added, whose calls are dropped.
func ResponsesResponseToAnthropic(resp map[string]any, model string, toolNames map[string]string, injected map[string]bool) map[string]any {
	content := make([]any, 0, 2)
	toolCalls := 0
	for _, raw := range anySlice(resp["output"]) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "reasoning":
			var b strings.Builder
			for _, s := range anySlice(item["summary"]) {
				if summary, ok := s.(map[string]any); ok {
					if text, _ := summary["text"].(string); text != "" {
						b.WriteString(text)
					}
				}
			}
			if b.Len() > 0 {
				content = append(content, map[string]any{"type": "thinking", "thinking": b.String(), "signature": ""})
			}
		case "message":
			for _, raw := range anySlice(item["content"]) {
				part, ok := raw.(map[string]any)
				if !ok || part["type"] != "output_text" {
					continue
				}
				if text, _ := part["text"].(string); text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
			}
		case "function_call":
			name, _ := item["name"].(string)
			if injected[name] {
				slog.Debug("zen responses: dropping call to gate-injected tool", "tool", name)
				continue
			}
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID = "call_" + name
			}
			args, _ := item["arguments"].(string)
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    callID,
				"name":  renameTool(toolNames, name),
				"input": parseArgs(args),
			})
			toolCalls++
		}
	}
	usage, _ := resp["usage"].(map[string]any)
	return map[string]any{
		"id":            messageID(resp["id"]),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   responsesStopReason(resp, toolCalls),
		"stop_sequence": nil,
		"usage":         responsesUsage(usage),
	}
}

// responsesStopReason derives the Anthropic stop_reason. The Responses wire
// carries no finish_reason: a function call in the output means the model
// asked for tools, incomplete_details.reason == "max_output_tokens" means the
// output cap stopped it, and anything else is a normal end of turn.
func responsesStopReason(resp map[string]any, toolCalls int) string {
	if details, ok := resp["incomplete_details"].(map[string]any); ok {
		if reason, _ := details["reason"].(string); reason == "max_output_tokens" {
			return "max_tokens"
		}
	}
	if toolCalls > 0 {
		return "tool_use"
	}
	return "end_turn"
}

// responsesUsage converts Responses usage into the Anthropic accounting
// convention: input_tokens is reported uncached, with the cached prefix
// counted as cache reads.
func responsesUsage(u map[string]any) map[string]any {
	input := toInt(u["input_tokens"])
	output := toInt(u["output_tokens"])
	cached := 0
	if details, ok := u["input_tokens_details"].(map[string]any); ok {
		cached = toInt(details["cached_tokens"])
	}
	if cached > input {
		cached = input
	}
	return map[string]any{
		"input_tokens":                input - cached,
		"output_tokens":               output,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
	}
}

// translateResponsesResponse rewrites a Responses upstream response into the
// Anthropic shape: an error status into an Anthropic error envelope, and any
// other JSON body into a message translated by ResponsesResponseToAnthropic.
func translateResponsesResponse(resp *http.Response, model string, clientStream bool, toolNames map[string]string, injected map[string]bool) *http.Response {
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return rebody(resp, "application/json", chatErrorToAnthropic(resp.StatusCode, raw, model))
	}
	// The gate only accepts streaming upstream bodies (anthropicToResponsesRequest
	// always sets "stream": true), so folding an event-stream back into the
	// Anthropic shape — into Anthropic SSE for a streaming client, or into one
	// aggregated message for a non-streaming one — is the streaming translator's
	// job. It is not here yet, and reporting the stream as an untranslatable body
	// beats handing the client an event stream shaped like neither wire.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return failResponse(resp, "Zen returned a streaming responses body")
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return failResponse(resp, "Zen response read error: "+err.Error())
	}
	var rn map[string]any
	if json.Unmarshal(raw, &rn) != nil {
		return failResponse(resp, "Zen returned a non-JSON responses body")
	}
	out, _ := json.Marshal(ResponsesResponseToAnthropic(rn, model, toolNames, injected))
	return rebody(resp, "application/json", out)
}
