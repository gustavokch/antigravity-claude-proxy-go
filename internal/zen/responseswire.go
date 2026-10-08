package zen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
// Fields with no usable Responses counterpart are dropped rather than guessed:
// stop_sequences and top_k (no field), thinking (no equivalent), and
// temperature and top_p (OpenAI documents both as unsupported for reasoning
// models unless effort is none, and the reference OpenCode client sends
// neither for any Responses-wire id). max_tokens becomes max_output_tokens.
//
// The body always carries "store": false so the upstream does not retain the
// conversation; see the comment at the assignment for why that is safe.
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
	// temperature, top_p, stop_sequences and thinking are deliberately
	// dropped; see the function comment.
	out["stream"] = true
	// store is explicit because the Responses API retains every response
	// server-side by default. The replay is stateless (whole conversation each
	// turn, no previous_response_id, no item ids), so the stored copy is never
	// read, and the genuine OpenCode harness sends store:false for every model
	// on this wire. out is an allowlist, so no client field can switch it on.
	out["store"] = false

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
// ({"type":"function","name":...,"parameters":...,"strict":false}).
func chatToolToResponses(t any) any {
	tool, _ := t.(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	// strict is explicit because a Responses function tool with `strict`
	// omitted attempts strict mode, which rewrites optional parameters as
	// required; Chat-Completions tools (the source shape) are non-strict.
	out := map[string]any{"type": "function", "name": fn["name"], "strict": false}
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
// items: text becomes an assistant message and tool_use becomes a
// function_call whose arguments are the JSON-encoded tool input. Items are
// ordered message → function_call.
//
// thinking blocks are dropped rather than replayed: a Responses reasoning
// input item must carry the id (or encrypted_content) the upstream issued,
// and the thinking blocks this translator hands out have neither
// (signature is ""), so a reconstructed item would be rejected.
func assistantToResponses(content any, renames map[string]string) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": s}},
		}}
	}
	blocks := anySlice(content)
	var parts, calls []any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "thinking":
			// Dropped deliberately; see the function comment.
		case "text":
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, map[string]any{"type": "output_text", "text": text})
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			encoded, err := json.Marshal(block["input"])
			if err != nil || string(encoded) == "null" {
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
	out := make([]any, 0, 1+len(calls))
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
	toolCalls, refusals := 0, 0
	hasText := false
	for i, raw := range anySlice(resp["output"]) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "reasoning":
			var parts []string
			for _, s := range anySlice(item["summary"]) {
				if summary, ok := s.(map[string]any); ok {
					if text, _ := summary["text"].(string); text != "" {
						parts = append(parts, text)
					}
				}
			}
			if len(parts) > 0 {
				content = append(content, map[string]any{"type": "thinking", "thinking": strings.Join(parts, "\n\n"), "signature": ""})
			}
		case "message":
			for _, raw := range anySlice(item["content"]) {
				part, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				switch part["type"] {
				case "output_text":
					if text, _ := part["text"].(string); text != "" {
						content = append(content, map[string]any{"type": "text", "text": text})
						if strings.TrimSpace(text) != "" {
							hasText = true
						}
					}
				case "refusal":
					// A decline rides as a refusal part. Anthropic has no
					// refusal block, so the model's explanation becomes the
					// text and responsesStopReason marks the turn refusal —
					// dropping the part would hand back an empty turn that
					// reads as a successful answer.
					if text, _ := part["refusal"].(string); text != "" {
						content = append(content, map[string]any{"type": "text", "text": text})
					}
					refusals++
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
				// Numbered by output position, as the stream path numbers by
				// output_index: two calls to one tool must not share an id.
				callID = "call_" + strconv.Itoa(i)
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
	stop := responsesStopReason(resp, toolCalls, refusals)
	if !hasText && stop == "end_turn" {
		content = append(content, map[string]any{"type": "text", "text": emptyStopFallbackText})
	}
	usage, _ := resp["usage"].(map[string]any)
	return map[string]any{
		"id":            messageID(resp["id"]),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_sequence": nil,
		"stop_reason":   stop,
		"usage":         responsesUsage(usage),
	}
}

// responsesStopReason derives the Anthropic stop_reason. The Responses wire
// carries no finish_reason, so the output itself decides: a refusal part or
// incomplete_details.reason "content_filter" means the model declined,
// incomplete_details.reason "max_output_tokens" means the output cap stopped
// it, a surviving function call in the output means the model asked for tools,
// and anything else is a normal end of turn.
//
// A refusal outranks the rest: Anthropic tells clients to discard the output of
// a declined turn, so reporting max_tokens or tool_use over a filter would hide
// the decline behind a stop the client would resume from.
func responsesStopReason(resp map[string]any, toolCalls, refusals int) string {
	if refusals > 0 {
		return "refusal"
	}
	if details, ok := resp["incomplete_details"].(map[string]any); ok {
		switch reason, _ := details["reason"].(string); reason {
		case "content_filter":
			return "refusal"
		case "max_output_tokens":
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
// Anthropic shape: an error status into an Anthropic error envelope, an
// upstream event stream into Anthropic SSE (for a streaming client) or one
// aggregated message (for a non-streaming one), and any other body into JSON.
func translateResponsesResponse(resp *http.Response, model string, clientStream bool, toolNames map[string]string, injected map[string]bool) *http.Response {
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return rebody(resp, "application/json", chatErrorToAnthropic(resp.StatusCode, raw, model))
	}
	// The gate only accepts streaming upstream bodies (anthropicToResponsesRequest
	// always sets "stream": true), so an event stream arrives here for every
	// successful call and is folded back into whichever Anthropic shape the
	// client asked for.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		if clientStream {
			pr, pw := io.Pipe()
			upstream := resp.Body
			go func() {
				defer upstream.Close()
				// A translator error becomes a read error on the client side;
				// by then the emitter has already written the Anthropic error
				// event, so the client sees the reason either way.
				pw.CloseWithError(streamResponsesToAnthropic(upstream, pw, model, toolNames, injected))
			}()
			resp.Body = pr
			resp.ContentLength = -1
			resp.Header.Del("Content-Length")
			resp.Header.Del("Content-Encoding")
			resp.Header.Set("Content-Type", "text/event-stream")
			return resp
		}
		upstream := resp.Body
		aggregated, err := aggregateResponsesStream(upstream)
		// Closed before the rewrite: failResponse installs a fresh resp.Body,
		// so an upstream left open here would orphan the connection.
		_ = upstream.Close()
		if err != nil {
			return failResponse(resp, "Zen stream error: "+err.Error())
		}
		out, _ := json.Marshal(ResponsesResponseToAnthropic(aggregated, model, toolNames, injected))
		return rebody(resp, "application/json", out)
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

// SendResponses serves an Anthropic Messages request against a Zen
// Responses-wire model: the Anthropic body is translated to an OpenAI
// /v1/responses body, posted to Zen, and the upstream response is returned
// rewritten into the Anthropic shape (JSON body, SSE event stream, or
// Anthropic error envelope). The returned response is therefore
// indistinguishable from a /v1/messages answer, so callers reuse their
// Anthropic handling (usage interception) unchanged.
func SendResponses(ctx context.Context, client *http.Client, baseURL, apiKey string, anthropicBody []byte) (*http.Response, error) {
	return SendResponsesWithHeaders(ctx, client, baseURL, apiKey, anthropicBody, nil)
}

// SendResponsesWithHeaders is SendResponses with inbound client headers preserved
// when they originate from a genuine OpenCode client.
func SendResponsesWithHeaders(ctx context.Context, client *http.Client, baseURL, apiKey string, anthropicBody []byte, clientHeaders http.Header) (*http.Response, error) {
	var req map[string]any
	if err := json.Unmarshal(anthropicBody, &req); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	responsesReq, toolNames, injected := anthropicToResponsesRequest(req)
	payload, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, NormalizeBaseURL(baseURL)+"/v1/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	clientStream, _ := req["stream"].(bool)
	if clientStream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "*/*")
	}
	ApplyHarnessHeaderMapPreserving(httpReq.Header, clientHeaders)
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	model, _ := responsesReq["model"].(string)
	return translateResponsesResponse(resp, model, clientStream, toolNames, injected), nil
}

// ForwardResponses is the forwarding entry point: SendResponsesWithHeaders, then copy the
// translated response to w (flushing per write so SSE stays incremental).
// modify runs on the translated response before any byte is written, mirroring
// ForwardMessagesWithModify.
func ForwardResponses(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, modify func(*http.Response) error) {
	forwardTranslated(w, "responses", modify, func() (*http.Response, error) {
		return SendResponsesWithHeaders(r.Context(), TLSClient(), baseURL, apiKey, body, r.Header)
	})
}
