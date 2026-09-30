package zen

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// --- Stream translation ---
//
// The two directions a Responses SSE stream has to be translated in are the
// mirror images of aggregateChatStream / streamChatToAnthropic for the
// Chat-Completions wire: folded into one JSON body for a non-streaming client
// (aggregateResponsesStream), or emitted as Anthropic SSE for a streaming one
// (streamResponsesToAnthropic). Both share the item/delta vocabulary of the
// Responses API — reasoning_summary_text.delta, output_text.delta,
// function_call_arguments.delta — rather than the chat wire's single chunk with
// a delta object.

// aggregateResponsesStream folds a Responses SSE stream into the single JSON
// body a non-streaming call would have returned. Output items are
// reconstructed from the item-added/delta events: text and reasoning deltas
// are concatenated per output_index, and a function call's arguments are
// concatenated and then overridden by the complete value the
// response.output_item.done event carries.
//
// A stream that ends without response.completed is reported as an error rather
// than a partial answer: a dropped connection must not pass off a truncated
// reply as complete.
func aggregateResponsesStream(r io.Reader) (map[string]any, error) {
	items := map[int]map[string]any{}
	texts := map[int]*strings.Builder{}
	reasoning := map[int]*strings.Builder{}
	refusals := map[int]*strings.Builder{}
	argText := map[int]*strings.Builder{}
	order := []int{}
	var (
		id     any
		usage  map[string]any
		trunc  map[string]any
		done   bool
		failed string
	)
	// mark registers an index that only ever produced deltas. It must not
	// clobber a real envelope: output_item.added carries a function call's
	// name and call_id, and a later argument delta would otherwise erase the
	// item the reconstruction below needs.
	mark := func(idx int) {
		if item, seen := items[idx]; !seen || len(item) == 0 {
			if !seen {
				order = append(order, idx)
			}
			items[idx] = map[string]any{}
		}
	}
	builder := func(m map[int]*strings.Builder, idx int) *strings.Builder {
		b := m[idx]
		if b == nil {
			b = &strings.Builder{}
			m[idx] = b
		}
		return b
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		typ, _ := event["type"].(string)
		idx := toInt(event["output_index"])
		switch typ {
		case "response.created", "response.in_progress":
			if resp, ok := event["response"].(map[string]any); ok {
				if v, ok := resp["id"]; ok {
					id = v
				}
			}
		case "response.output_item.added", "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item == nil {
				continue
			}
			if _, seen := items[idx]; !seen {
				order = append(order, idx)
			}
			items[idx] = item
		case "response.output_text.delta":
			mark(idx)
			if delta, _ := event["delta"].(string); delta != "" {
				builder(texts, idx).WriteString(delta)
			}
		case "response.reasoning_summary_text.delta":
			mark(idx)
			if delta, _ := event["delta"].(string); delta != "" {
				builder(reasoning, idx).WriteString(delta)
			}
		case "response.refusal.delta":
			mark(idx)
			if delta, _ := event["delta"].(string); delta != "" {
				builder(refusals, idx).WriteString(delta)
			}
		case "response.function_call_arguments.delta":
			mark(idx)
			if delta, _ := event["delta"].(string); delta != "" {
				builder(argText, idx).WriteString(delta)
			}
		case "response.completed", "response.incomplete":
			done = true
			if resp, ok := event["response"].(map[string]any); ok {
				if v, ok := resp["id"]; ok {
					id = v
				}
				if u, ok := resp["usage"].(map[string]any); ok {
					usage = u
				}
				if d, ok := resp["incomplete_details"].(map[string]any); ok {
					trunc = d
				}
			}
		case "response.failed", "response.error", "error":
			done = true
			failed = responsesFailureText(event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if failed != "" {
		return nil, errors.New(failed)
	}
	if !done {
		return nil, errors.New("Zen stream ended before completion")
	}
	sort.Ints(order)
	output := make([]any, 0, len(order))
	for _, idx := range order {
		item := items[idx]
		if item == nil || len(item) == 0 {
			// Only deltas were seen for this index (no item envelope).
			if b := reasoning[idx]; b != nil && b.Len() > 0 {
				output = append(output, map[string]any{
					"type":    "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": b.String()}},
				})
				continue
			}
			if b := refusals[idx]; b != nil && b.Len() > 0 {
				output = append(output, messageItem([]any{map[string]any{"type": "refusal", "refusal": b.String()}}))
				continue
			}
			if b := texts[idx]; b != nil && b.Len() > 0 {
				output = append(output, messageItem([]any{map[string]any{"type": "output_text", "text": b.String()}}))
			}
			continue
		}
		switch item["type"] {
		case "reasoning":
			var b strings.Builder
			if src := reasoning[idx]; src != nil {
				b.WriteString(src.String())
			}
			if b.Len() == 0 {
				for _, s := range anySlice(item["summary"]) {
					if summary, ok := s.(map[string]any); ok {
						if text, _ := summary["text"].(string); text != "" {
							b.WriteString(text)
						}
					}
				}
			}
			if b.Len() == 0 {
				continue
			}
			output = append(output, map[string]any{
				"type":    "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": b.String()}},
			})
		case "message":
			// A refusal and ordinary text are both content parts of one
			// message; both must survive the fold or the decline disappears
			// and the turn reads as a clean end_turn.
			parts := make([]any, 0, 2)
			refused := false
			if src := refusals[idx]; src != nil && src.Len() > 0 {
				parts = append(parts, map[string]any{"type": "refusal", "refusal": src.String()})
				refused = true
			}
			if !refused {
				// No refusal was streamed, but one may still sit in the item
				// envelope: output_item.done carries the complete part when
				// the refusal arrived without a delta to stream.
				for _, p := range anySlice(item["content"]) {
					if part, ok := p.(map[string]any); ok && part["type"] == "refusal" {
						if text, _ := part["refusal"].(string); text != "" {
							parts = append(parts, map[string]any{"type": "refusal", "refusal": text})
						}
						break
					}
				}
			}
			text := ""
			if src := texts[idx]; src != nil {
				text = src.String()
			}
			if text == "" {
				for _, p := range anySlice(item["content"]) {
					if part, ok := p.(map[string]any); ok && part["type"] == "output_text" {
						if t, _ := part["text"].(string); t != "" {
							text += t
						}
					}
				}
			}
			if text != "" {
				parts = append(parts, map[string]any{"type": "output_text", "text": text})
			}
			if len(parts) == 0 {
				continue
			}
			output = append(output, messageItem(parts))
		case "function_call":
			call := map[string]any{"type": "function_call", "name": item["name"]}
			if callID, _ := item["call_id"].(string); callID != "" {
				call["call_id"] = callID
			}
			args, _ := item["arguments"].(string)
			if args == "" {
				if b := argText[idx]; b != nil {
					args = b.String()
				}
			}
			call["arguments"] = args
			output = append(output, call)
		}
	}
	out := map[string]any{"output": output}
	if id != nil {
		out["id"] = id
	}
	if usage != nil {
		out["usage"] = usage
	}
	if trunc != nil {
		out["incomplete_details"] = trunc
	}
	return out, nil
}

// messageItem wraps content parts as an assistant message output item.
func messageItem(parts []any) map[string]any {
	return map[string]any{"type": "message", "role": "assistant", "content": parts}
}

// responsesFailureText extracts the human-readable reason from a
// response.failed / response.error / error event.
func responsesFailureText(event map[string]any) string {
	if resp, ok := event["response"].(map[string]any); ok {
		return responsesFailureText(resp)
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

// streamResponsesToAnthropic converts a Responses SSE stream into the Anthropic
// SSE event sequence on w: message_start, a content block per output item
// (thinking, text, or tool_use), message_delta with the derived stop_reason
// and Anthropic usage, then message_stop. Calls to gate-injected tools are
// dropped before any content block opens.
func streamResponsesToAnthropic(r io.Reader, w io.Writer, model string, toolNames map[string]string, injected map[string]bool) error {
	s := &responsesStream{
		w: w, model: model, current: -1,
		itemBlocks: map[int]int{}, toolNames: toolNames, injected: injected, dropped: map[int]bool{},
	}
	done := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if err := s.handle(event); err != nil {
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
	// A clean EOF without response.completed is a truncated stream (dropped
	// connection, proxy timeout); finishing it as end_turn would pass a partial
	// answer off as complete.
	if !done && !s.settled {
		s.emitError("api_error", "Zen stream ended before completion")
		return nil
	}
	return s.finish()
}

// responsesStream is the Anthropic SSE emitter for the Responses wire. The
// Responses stream has no per-chunk usage, so the usage that arrives with
// response.completed is carried until finish.
type responsesStream struct {
	w          io.Writer
	model      string
	toolNames  map[string]string // upstream → client tool names, may be nil
	injected   map[string]bool   // gate-only tool names, may be nil
	dropped    map[int]bool      // output indexes skipped as gate-injected
	itemBlocks map[int]int       // output_index → Anthropic block index
	started    bool
	failed     bool
	settled    bool
	toolItems  int // tool_use blocks opened (surviving gate-injected drops)
	nextIndex  int
	current    int    // open Anthropic block index, -1 when none
	kind       string // "thinking" | "text" | "tool"
	stop       string
	usage      map[string]any
}

func (s *responsesStream) emit(event string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

func (s *responsesStream) emitError(kind, msg string) {
	s.failed = true
	_ = s.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
}

func (s *responsesStream) start(id any) error {
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

func (s *responsesStream) closeBlock() error {
	if s.current < 0 {
		return nil
	}
	idx := s.current
	s.current, s.kind = -1, ""
	return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (s *responsesStream) openBlock(kind string, block map[string]any) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.current, s.kind = s.nextIndex, kind
	s.nextIndex++
	return s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": s.current, "content_block": block})
}

func (s *responsesStream) delta(delta map[string]any) error {
	return s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.current, "delta": delta})
}

// handle consumes one Responses SSE event. Text, reasoning, and refusal deltas
// open their block on the first delta (an empty block would be noise); a
// function_call opens a tool_use block on response.output_item.added, which
// carries the call id and the (renamed) function name.
func (s *responsesStream) handle(event map[string]any) error {
	typ, _ := event["type"].(string)
	idx := toInt(event["output_index"])
	// start is idempotent, so it is called for every event: a Responses stream
	// need not open with response.created, and message_start has to precede the
	// first content block for the client to attribute it to a message.
	var id any
	if resp, ok := event["response"].(map[string]any); ok {
		id = resp["id"]
	}
	if err := s.start(id); err != nil {
		return err
	}
	switch typ {
	case "response.output_text.delta", "response.refusal.delta":
		// A refusal rides the same text block: Anthropic has no refusal block,
		// so the explanation is streamed as text and the turn is marked
		// stop_reason refusal. Only the stop reason distinguishes it.
		if typ == "response.refusal.delta" {
			s.stop = "refusal"
		}
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		if s.kind != "text" {
			if err := s.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
				return err
			}
			s.itemBlocks[idx] = s.current
		}
		return s.delta(map[string]any{"type": "text_delta", "text": delta})
	case "response.reasoning_summary_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		if s.kind != "thinking" {
			if err := s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""}); err != nil {
				return err
			}
			s.itemBlocks[idx] = s.current
		}
		return s.delta(map[string]any{"type": "thinking_delta", "thinking": delta})
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			return nil
		}
		if s.dropped[idx] {
			return nil
		}
		name, _ := item["name"].(string)
		if s.injected[name] {
			// Gate-injected tool the client never declared: opening a
			// tool_use block would hand it an unresolvable name.
			s.dropped[idx] = true
			slog.Debug("zen responses stream: dropping call to gate-injected tool", "outputIndex", idx, "tool", name)
			return nil
		}
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID = "call_" + strconv.Itoa(idx)
		}
		if err := s.openBlock("tool", map[string]any{
			"type": "tool_use", "id": callID,
			"name": renameTool(s.toolNames, name), "input": map[string]any{},
		}); err != nil {
			return err
		}
		s.itemBlocks[idx] = s.current
		s.toolItems++
		return nil
	case "response.function_call_arguments.delta":
		if s.dropped[idx] {
			return nil
		}
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		block, known := s.itemBlocks[idx]
		if !known || block != s.current {
			// Interleaved argument fragments for an earlier call cannot be
			// reopened in Anthropic SSE; drop rather than corrupt.
			slog.Debug("zen responses stream: dropping interleaved function_call arguments", "outputIndex", idx, "openBlock", s.current)
			return nil
		}
		return s.delta(map[string]any{"type": "input_json_delta", "partial_json": delta})
	case "response.completed", "response.incomplete":
		s.settled = true
		if resp, ok := event["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok && len(u) > 0 {
				s.usage = u
			}
			if details, ok := resp["incomplete_details"].(map[string]any); ok {
				switch reason, _ := details["reason"].(string); reason {
				case "content_filter":
					s.stop = "refusal"
				case "max_output_tokens":
					s.stop = "max_tokens"
				}
			}
		}
		return nil
	case "response.failed", "response.error", "error":
		s.settled = true
		s.emitError("api_error", "Zen: "+responsesFailureText(event))
		return nil
	}
	return nil
}

func (s *responsesStream) finish() error {
	if err := s.start(nil); err != nil {
		return err
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	stop := s.stop
	if s.toolItems > 0 && stop == "" {
		stop = "tool_use"
	}
	// Every tool call was dropped as gate-injected: claiming tool_use with no
	// tool_use block would make the client wait for one.
	if s.toolItems == 0 && stop == "tool_use" {
		stop = "end_turn"
	}
	if stop == "" {
		stop = "end_turn"
	}
	if err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": responsesUsage(s.usage),
	}); err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}
