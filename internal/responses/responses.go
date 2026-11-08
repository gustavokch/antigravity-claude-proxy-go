// Package responses translates the OpenAI Responses wire back into the
// Anthropic Messages shape. Zen's Responses-wire models and the ChatGPT Codex
// backend both answer with a Responses event stream, so the streaming emitter
// (Stream), the non-streaming fold (Aggregate) and the response-object
// translation (ToAnthropic) live here, and each gateway adapts them through
// Options instead of carrying its own copy.
package responses

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// ErrStreamTruncated marks a translated stream that ended before its terminal
// event. The client already received an in-band error event; the error exists
// so the caller can log the break.
var ErrStreamTruncated = errors.New("stream ended before its terminal event")

// Options adapts the translation to one upstream.
type Options struct {
	Model      string              // echoed as message.model
	ToolNames  map[string]string   // upstream tool name -> client tool name; nil = identity
	Drop       map[string]bool     // upstream tool names whose calls are dropped (Zen gate tools); nil = none
	MessageID  func(id any) string // nil -> "msg_" + id, the current Zen behaviour
	ErrorLabel string              // prefix of stream error messages: "Zen", "Codex"
}

// messageID derives the Anthropic message id from the upstream response id.
// A response without an id still needs a unique one, so the clock stands in.
func (o Options) messageID(id any) string {
	if o.MessageID != nil {
		return o.MessageID(id)
	}
	if s, _ := id.(string); s != "" {
		return "msg_" + s
	}
	return "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// toolName maps an upstream tool name back to the name the client declared.
func (o Options) toolName(name string) string {
	if client, ok := o.ToolNames[name]; ok {
		return client
	}
	return name
}

// errorMessage names the upstream in an error the client sees, e.g. "Zen
// stream ended before completion" (sep " ") or "Codex: overloaded" (sep ": ").
func (o Options) errorMessage(sep, msg string) string {
	if o.ErrorLabel == "" {
		return msg
	}
	return o.ErrorLabel + sep + msg
}

// ToAnthropic converts a Responses response object into an Anthropic Messages
// response. reasoning items become thinking blocks carrying their
// encrypted_content as the signature, message items become text blocks,
// web_search_call items become a server_tool_use and web_search_tool_result
// pair, and function_call items become tool_use blocks with their arguments
// decoded. A call to a tool in opts.Drop is left out, because the client has
// no way to resolve it.
func ToAnthropic(resp map[string]any, opts Options) map[string]any {
	content := make([]any, 0, 2)
	toolCalls, refusals := 0, 0
	for i, raw := range anySlice(resp["output"]) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "reasoning":
			// The signature is what lets the next turn hand the reasoning
			// back as an encrypted reasoning item, so a reasoning item with
			// no summary text still becomes a thinking block when it has one.
			text := strings.Join(summaryTexts(item), "\n\n")
			signature, _ := item["encrypted_content"].(string)
			if text != "" || signature != "" {
				content = append(content, map[string]any{"type": "thinking", "thinking": text, "signature": signature})
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
					}
				case "refusal":
					// A decline rides as a refusal part. Anthropic has no
					// refusal block, so the model's explanation becomes the
					// text and stopReason marks the turn refusal — dropping
					// the part would hand back an empty turn that reads as a
					// successful answer.
					if text, _ := part["refusal"].(string); text != "" {
						content = append(content, map[string]any{"type": "text", "text": text})
					}
					refusals++
				}
			}
		case "web_search_call":
			if search, ok := webSearchOf(i, item); ok {
				content = append(content, search.toolUse(), search.result())
			}
		case "function_call":
			name, _ := item["name"].(string)
			if opts.Drop[name] {
				slog.Debug("responses: dropping call to a dropped tool", "tool", name)
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
				"name":  opts.toolName(name),
				"input": parseArgs(args),
			})
			toolCalls++
		}
	}
	usage, _ := resp["usage"].(map[string]any)
	return map[string]any{
		"id":            opts.messageID(resp["id"]),
		"type":          "message",
		"role":          "assistant",
		"model":         opts.Model,
		"content":       content,
		"stop_sequence": nil,
		"stop_reason":   stopReason(resp, toolCalls, refusals),
		"usage":         Usage(usage),
	}
}

// stopReason derives the Anthropic stop_reason. The Responses wire carries no
// finish_reason, so the output itself decides: a refusal part or
// incomplete_details.reason "content_filter" means the model declined,
// incomplete_details.reason "max_output_tokens" means the output cap stopped
// it, a surviving function call in the output means the model asked for tools,
// and anything else is a normal end of turn.
//
// A refusal outranks the rest: Anthropic tells clients to discard the output of
// a declined turn, so reporting max_tokens or tool_use over a filter would hide
// the decline behind a stop the client would resume from.
func stopReason(resp map[string]any, toolCalls, refusals int) string {
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

// Usage converts Responses usage into the Anthropic accounting convention:
// input_tokens counts only the uncached input, the cached prefix is reported
// as cache reads and the tokens written to the cache as cache creation. The
// Responses input_tokens includes both, so both are taken out of it.
func Usage(u map[string]any) map[string]any {
	input := toInt(u["input_tokens"])
	output := toInt(u["output_tokens"])
	cached, written := 0, 0
	if details, ok := u["input_tokens_details"].(map[string]any); ok {
		cached = max(toInt(details["cached_tokens"]), 0)
		written = max(toInt(details["cache_write_tokens"]), 0)
		if written == 0 {
			written = max(toInt(details["cache_creation_tokens"]), 0)
		}
	}
	return map[string]any{
		"input_tokens":                max(input-cached-written, 0),
		"output_tokens":               output,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": written,
	}
}

// webSearch is what a web_search_call item tells the client: the query the
// model ran and the pages it got back.
type webSearch struct {
	id, query string
	results   []any
}

// webSearchOf reads a web_search_call item. The query comes from action.query
// and the results from action.sources (requested with the
// web_search_call.action.sources include). An item with neither (an
// open_page or find action) has nothing to show and reports false. idx stands
// in for a missing item id, so two searches never share one.
func webSearchOf(idx int, item map[string]any) (webSearch, bool) {
	action, _ := item["action"].(map[string]any)
	search := webSearch{results: []any{}}
	search.query, _ = action["query"].(string)
	search.query = strings.TrimSpace(search.query)
	for _, raw := range anySlice(action["sources"]) {
		source, _ := raw.(map[string]any)
		url, _ := source["url"].(string)
		if url = strings.TrimSpace(url); url == "" {
			continue
		}
		title, _ := source["title"].(string)
		if title = strings.TrimSpace(title); title == "" {
			title = url
		}
		search.results = append(search.results, map[string]any{
			"type": "web_search_result", "title": title, "url": url, "page_age": nil,
		})
	}
	if search.query == "" && len(search.results) == 0 {
		return webSearch{}, false
	}
	search.id, _ = item["id"].(string)
	if search.id == "" {
		search.id = "web_search_" + strconv.Itoa(idx)
	}
	return search, true
}

// input is the server_tool_use input: the query, or an empty object for a
// search that reported only results.
func (w webSearch) input() map[string]any {
	if w.query == "" {
		return map[string]any{}
	}
	return map[string]any{"query": w.query}
}

func (w webSearch) toolUse() map[string]any {
	return map[string]any{"type": "server_tool_use", "id": w.id, "name": "web_search", "input": w.input()}
}

func (w webSearch) result() map[string]any {
	return map[string]any{"type": "web_search_tool_result", "tool_use_id": w.id, "content": w.results}
}

// summaryTexts returns the non-empty summary parts of a reasoning item.
func summaryTexts(item map[string]any) []string {
	var parts []string
	for _, raw := range anySlice(item["summary"]) {
		if part, ok := raw.(map[string]any); ok {
			if text, _ := part["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return parts
}

// failureText extracts the human-readable reason from a response.failed /
// response.error / error event.
func failureText(event map[string]any) string {
	if resp, ok := event["response"].(map[string]any); ok {
		return failureText(resp)
	}
	if e, ok := event["error"].(map[string]any); ok {
		if msg, _ := e["message"].(string); msg != "" {
			return msg
		}
	}
	if msg, _ := event["message"].(string); msg != "" {
		return msg
	}
	return "upstream stream error"
}

// anySlice returns v as a []any, or nil when it is any other type.
func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// parseArgs decodes a function call's argument string into the tool_use
// input. Anthropic requires an object, so anything else becomes {}.
func parseArgs(args string) any {
	var v any
	if strings.TrimSpace(args) == "" || json.Unmarshal([]byte(args), &v) != nil {
		return map[string]any{}
	}
	if _, ok := v.(map[string]any); !ok {
		return map[string]any{}
	}
	return v
}
