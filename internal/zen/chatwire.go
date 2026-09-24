package zen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SendChat serves an Anthropic Messages request against a Zen
// Chat-Completions-wire model: the Anthropic body is translated to an OpenAI
// /v1/chat/completions body, posted to Zen, and the upstream response is
// returned rewritten into the Anthropic shape (JSON body, SSE event stream, or
// Anthropic error envelope). The returned response is therefore
// indistinguishable from a /v1/messages answer, so callers reuse their
// Anthropic handling (CCR hydration, usage interception) unchanged.
func SendChat(ctx context.Context, client *http.Client, baseURL, apiKey string, anthropicBody []byte) (*http.Response, error) {
	var req map[string]any
	if err := json.Unmarshal(anthropicBody, &req); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	chatReq, err := AnthropicToChatRequest(req)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, NormalizeBaseURL(baseURL)+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	stream, _ := req["stream"].(bool)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	model, _ := chatReq["model"].(string)
	return translateChatResponse(resp, model), nil
}

// ForwardChat is the non-CCR entry point: SendChat, then copy the translated
// response to w (flushing per write so SSE stays incremental). modify runs on
// the translated response before any byte is written, mirroring
// ForwardMessagesWithModify.
func ForwardChat(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, modify func(*http.Response) error) {
	resp, err := SendChat(r.Context(), http.DefaultClient, baseURL, apiKey, body)
	if err != nil {
		slog.Default().Error("zen chat upstream error", "error", err)
		writeAPIError(w, http.StatusBadGateway, "api_error", "Zen upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if modify != nil {
		if err := modify(resp); err != nil {
			writeAPIError(w, http.StatusBadGateway, "api_error", "Zen response handling error: "+err.Error())
			return
		}
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// --- Request translation ---

// AnthropicToChatRequest converts an Anthropic Messages request body into an
// OpenAI Chat Completions request body.
func AnthropicToChatRequest(req map[string]any) (map[string]any, error) {
	model, _ := req["model"].(string)
	out := map[string]any{"model": StripOpencodePrefix(model)}

	messages := make([]any, 0)
	if sys := systemText(req["system"]); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	rawMsgs, _ := req["messages"].([]any)
	for _, raw := range rawMsgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			messages = append(messages, assistantToChat(msg["content"]))
		default:
			messages = append(messages, userToChat(msg["content"])...)
		}
	}
	out["messages"] = messages

	if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if stops, ok := req["stop_sequences"].([]any); ok && len(stops) > 0 {
		out["stop"] = stops
	}
	if stream, _ := req["stream"].(bool); stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if tools := toolsToChat(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if tc := toolChoiceToChat(req["tool_choice"]); tc != nil {
			out["tool_choice"] = tc
		}
	}
	return out, nil
}

func systemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		parts := make([]string, 0, len(s))
		for _, b := range s {
			if block, ok := b.(map[string]any); ok {
				if t, _ := block["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// userToChat maps one Anthropic user turn to Chat messages. tool_result
// blocks become role:"tool" messages, emitted first so they directly follow
// the assistant turn that issued the calls, as Chat Completions requires.
func userToChat(content any) []any {
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": "user", "content": s}}
	}
	blocks, _ := content.([]any)
	var toolMsgs []any
	parts := make([]any, 0, len(blocks))
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block["text"]})
		case "image":
			if u := imageURL(block["source"]); u != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			text := toolResultText(block["content"])
			if isErr, _ := block["is_error"].(bool); isErr && text != "" {
				text = "Error: " + text
			}
			toolMsgs = append(toolMsgs, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
		}
	}
	out := toolMsgs
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": collapseParts(parts)})
	}
	return out
}

// collapseParts sends a plain string when every part is text: several
// Chat-wire backends reject array content outright.
func collapseParts(parts []any) any {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		m := p.(map[string]any)
		if m["type"] != "text" {
			return parts
		}
		t, _ := m["text"].(string)
		texts = append(texts, t)
	}
	return strings.Join(texts, "\n\n")
}

func imageURL(src any) string {
	s, ok := src.(map[string]any)
	if !ok {
		return ""
	}
	switch s["type"] {
	case "base64":
		mt, _ := s["media_type"].(string)
		data, _ := s["data"].(string)
		return "data:" + mt + ";base64," + data
	case "url":
		u, _ := s["url"].(string)
		return u
	}
	return ""
}

func toolResultText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, b := range c {
			if block, ok := b.(map[string]any); ok && block["type"] == "text" {
				t, _ := block["text"].(string)
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func assistantToChat(content any) map[string]any {
	msg := map[string]any{"role": "assistant"}
	if s, ok := content.(string); ok {
		msg["content"] = s
		return msg
	}
	blocks, _ := content.([]any)
	var text, reasoning strings.Builder
	var calls []any
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			t, _ := block["text"].(string)
			text.WriteString(t)
		case "thinking":
			t, _ := block["thinking"].(string)
			reasoning.WriteString(t)
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			args, err := json.Marshal(block["input"])
			if err != nil || string(args) == "null" {
				args = []byte("{}")
			}
			calls = append(calls, map[string]any{
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": string(args)},
			})
		}
	}
	msg["content"] = text.String()
	// Reasoning backends (DeepSeek, Kimi, GLM) require prior reasoning to be
	// echoed back on tool-call turns; others ignore the field.
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return msg
}

func toolsToChat(v any) []any {
	tools, _ := v.([]any)
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		// Anthropic server tools (web_search_*, bash_*, …) carry no
		// input_schema and have no Chat Completions equivalent.
		schema, ok := tool["input_schema"]
		if !ok {
			continue
		}
		fn := map[string]any{"name": tool["name"], "parameters": schema}
		if d, ok := tool["description"].(string); ok && d != "" {
			fn["description"] = d
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func toolChoiceToChat(v any) any {
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
		return map[string]any{"type": "function", "function": map[string]any{"name": tc["name"]}}
	}
	return nil
}

// --- Response translation ---

func translateChatResponse(resp *http.Response, model string) *http.Response {
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return rebody(resp, "application/json", chatErrorToAnthropic(resp.StatusCode, raw))
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		pr, pw := io.Pipe()
		upstream := resp.Body
		go func() {
			defer upstream.Close()
			pw.CloseWithError(streamChatToAnthropic(upstream, pw, model))
		}()
		resp.Body = pr
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.Header.Del("Content-Encoding")
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return rebody(resp, "application/json", anthropicError(http.StatusBadGateway, "api_error", "Zen response read error: "+err.Error()))
	}
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		return rebody(resp, "application/json", anthropicError(http.StatusBadGateway, "api_error", "Zen returned non-JSON chat response"))
	}
	out, _ := json.Marshal(ChatResponseToAnthropic(chat, model))
	return rebody(resp, "application/json", out)
}

func rebody(resp *http.Response, contentType string, body []byte) *http.Response {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Type", contentType)
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp
}

func anthropicError(status int, kind, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
	return b
}

func chatErrorToAnthropic(status int, raw []byte) []byte {
	msg := strings.TrimSpace(string(raw))
	var env map[string]any
	if json.Unmarshal(raw, &env) == nil {
		if e, ok := env["error"].(map[string]any); ok {
			if m, _ := e["message"].(string); m != "" {
				msg = m
			}
		} else if m, _ := env["message"].(string); m != "" {
			msg = m
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	kind := "api_error"
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		kind = "invalid_request_error"
	case status == http.StatusUnauthorized:
		kind = "authentication_error"
	case status == http.StatusForbidden:
		kind = "permission_error"
	case status == http.StatusNotFound:
		kind = "not_found_error"
	case status == http.StatusTooManyRequests:
		kind = "rate_limit_error"
	case status == 529:
		kind = "overloaded_error"
	}
	return anthropicError(status, kind, "Zen: "+msg)
}

func mapFinishReason(r string) string {
	switch r {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// anthropicUsage splits OpenAI prompt_tokens into uncached input and cache
// reads, the Anthropic accounting convention.
func anthropicUsage(u map[string]any) map[string]any {
	prompt := toInt(u["prompt_tokens"])
	completion := toInt(u["completion_tokens"])
	cached := 0
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = toInt(d["cached_tokens"])
	}
	if c := toInt(u["prompt_cache_hit_tokens"]); c > cached { // DeepSeek spelling
		cached = c
	}
	if cached > prompt {
		cached = prompt
	}
	return map[string]any{
		"input_tokens":                prompt - cached,
		"output_tokens":               completion,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
	}
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

func messageID(chatID any) string {
	if s, _ := chatID.(string); s != "" {
		return "msg_" + s
	}
	return "msg_zen_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// ChatResponseToAnthropic converts a non-streaming Chat Completions response
// into an Anthropic Messages response.
func ChatResponseToAnthropic(chat map[string]any, model string) map[string]any {
	content := make([]any, 0, 2)
	stop := "end_turn"
	if choices, _ := chat["choices"].([]any); len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		if r := reasoningOf(msg); r != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": r, "signature": ""})
		}
		if t, _ := msg["content"].(string); t != "" {
			content = append(content, map[string]any{"type": "text", "text": t})
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, c := range calls {
			call, _ := c.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			args, _ := fn["arguments"].(string)
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    call["id"],
				"name":  fn["name"],
				"input": parseArgs(args),
			})
		}
		fr, _ := choice["finish_reason"].(string)
		stop = mapFinishReason(fr)
		if len(calls) > 0 {
			stop = "tool_use"
		}
	}
	usage, _ := chat["usage"].(map[string]any)
	return map[string]any{
		"id":            messageID(chat["id"]),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         anthropicUsage(usage),
	}
}

// reasoningOf reads the reasoning text under either spelling Chat-wire
// backends use.
func reasoningOf(m map[string]any) string {
	if r, _ := m["reasoning_content"].(string); r != "" {
		return r
	}
	r, _ := m["reasoning"].(string)
	return r
}

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

// streamChatToAnthropic reads OpenAI chat.completion.chunk SSE from r and
// writes the equivalent Anthropic Messages SSE event sequence to w.
func streamChatToAnthropic(r io.Reader, w io.Writer, model string) error {
	s := &chatStream{w: w, model: model, current: -1, toolBlocks: map[int]int{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if err := s.handle(chunk); err != nil {
			return err
		}
		if s.failed {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		s.emitError("api_error", "Zen stream read error: "+err.Error())
		return nil
	}
	return s.finish()
}

type chatStream struct {
	w          io.Writer
	model      string
	started    bool
	failed     bool
	nextIndex  int
	current    int    // open Anthropic block index, -1 when none
	kind       string // "thinking" | "text" | "tool"
	toolBlocks map[int]int
	stop       string
	usage      map[string]any
}

func (s *chatStream) emit(event string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

func (s *chatStream) emitError(kind, msg string) {
	s.failed = true
	_ = s.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
}

func (s *chatStream) start(id any) error {
	if s.started {
		return nil
	}
	s.started = true
	return s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": messageID(id), "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *chatStream) closeBlock() error {
	if s.current < 0 {
		return nil
	}
	idx := s.current
	s.current, s.kind = -1, ""
	return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (s *chatStream) openBlock(kind string, block map[string]any) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.current, s.kind = s.nextIndex, kind
	s.nextIndex++
	return s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": s.current, "content_block": block})
}

func (s *chatStream) delta(delta map[string]any) error {
	return s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.current, "delta": delta})
}

func (s *chatStream) handle(chunk map[string]any) error {
	if e, ok := chunk["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		if msg == "" {
			msg = "upstream stream error"
		}
		if err := s.start(chunk["id"]); err != nil {
			return err
		}
		s.emitError("api_error", "Zen: "+msg)
		return nil
	}
	if err := s.start(chunk["id"]); err != nil {
		return err
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		s.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	d, _ := choice["delta"].(map[string]any)
	if r := reasoningOf(d); r != "" {
		if s.kind != "thinking" {
			if err := s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""}); err != nil {
				return err
			}
		}
		if err := s.delta(map[string]any{"type": "thinking_delta", "thinking": r}); err != nil {
			return err
		}
	}
	if t, _ := d["content"].(string); t != "" {
		if s.kind != "text" {
			if err := s.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
				return err
			}
		}
		if err := s.delta(map[string]any{"type": "text_delta", "text": t}); err != nil {
			return err
		}
	}
	calls, _ := d["tool_calls"].([]any)
	for i, c := range calls {
		call, _ := c.(map[string]any)
		callIdx := i
		if _, present := call["index"]; present {
			callIdx = toInt(call["index"])
		}
		fn, _ := call["function"].(map[string]any)
		blockIdx, known := s.toolBlocks[callIdx]
		if !known {
			id, _ := call["id"].(string)
			if id == "" {
				id = "call_" + strconv.Itoa(callIdx)
			}
			if err := s.openBlock("tool", map[string]any{"type": "tool_use", "id": id, "name": fn["name"], "input": map[string]any{}}); err != nil {
				return err
			}
			s.toolBlocks[callIdx] = s.current
			blockIdx = s.current
		}
		if args, _ := fn["arguments"].(string); args != "" {
			if blockIdx != s.current {
				// Interleaved argument fragments for an earlier call cannot
				// be reopened in Anthropic SSE; drop rather than corrupt.
				continue
			}
			if err := s.delta(map[string]any{"type": "input_json_delta", "partial_json": args}); err != nil {
				return err
			}
		}
	}
	if fr, _ := choice["finish_reason"].(string); fr != "" {
		s.stop = mapFinishReason(fr)
	}
	return nil
}

func (s *chatStream) finish() error {
	if err := s.start(nil); err != nil {
		return err
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	stop := s.stop
	if stop == "" {
		stop = "end_turn"
	}
	if len(s.toolBlocks) > 0 && stop == "end_turn" {
		stop = "tool_use"
	}
	if err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": anthropicUsage(s.usage),
	}); err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}
