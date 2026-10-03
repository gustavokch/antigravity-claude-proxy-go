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
		id              any
		usage           map[string]any
		trunc           map[string]any
		done            bool
		failed          string
		completedOutput []any
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
				if o := anySlice(resp["output"]); len(o) > 0 {
					completedOutput = o
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
	// Fallback: a stream with no deltas or item envelopes still carries the
	// answer in response.completed's output. Adopt only indices the live
	// events never saw; streamed state always wins.
	for i, raw := range completedOutput {
		if item, seen := items[i]; seen && len(item) > 0 {
			continue
		}
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		if _, seen := items[i]; !seen {
			order = append(order, i)
		}
		items[i] = item
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
				var parts []string
				for _, s := range anySlice(item["summary"]) {
					if summary, ok := s.(map[string]any); ok {
						if text, _ := summary["text"].(string); text != "" {
							parts = append(parts, text)
						}
					}
				}
				b.WriteString(strings.Join(parts, "\n\n"))
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
		itemBlocks: map[int]int{}, sent: map[sentKey]*strings.Builder{}, toolNames: toolNames, injected: injected, dropped: map[int]bool{},
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
	toolNames  map[string]string            // upstream → client tool names, may be nil
	injected   map[string]bool              // gate-only tool names, may be nil
	dropped    map[int]bool                 // output indexes skipped as gate-injected
	itemBlocks map[int]int                  // output_index → Anthropic block index
	sent       map[sentKey]*strings.Builder // delivered output per (kind, output_index)
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

// Kinds of output the stream has delivered to the client, tracked per output
// item so output_item.done can emit only what the deltas did not.
const (
	sentArgs     = "args"
	sentText     = "text"
	sentRefusal  = "refusal"
	sentThinking = "thinking"
)

type sentKey struct {
	kind string
	idx  int
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

// writeText streams text into the open text block, opening one first when a
// different kind of block is open. idx is the output item the text belongs to.
func (s *responsesStream) writeText(idx int, text string) error {
	if s.kind != "text" {
		if err := s.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
			return err
		}
		s.itemBlocks[idx] = s.current
	}
	return s.delta(map[string]any{"type": "text_delta", "text": text})
}

// writeThinking is writeText for a reasoning summary.
func (s *responsesStream) writeThinking(idx int, text string) error {
	if s.kind != "thinking" {
		if err := s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""}); err != nil {
			return err
		}
		s.itemBlocks[idx] = s.current
	}
	return s.delta(map[string]any{"type": "thinking_delta", "thinking": text})
}

// record notes text as delivered to the client for one (kind, idx) stream.
func (s *responsesStream) record(kind string, idx int, text string) {
	key := sentKey{kind, idx}
	b := s.sent[key]
	if b == nil {
		b = &strings.Builder{}
		s.sent[key] = b
	}
	b.WriteString(text)
}

// missing returns the part of full the client has not yet received for one
// (kind, idx) stream. ok is false when nothing is owed: full is empty, is
// already delivered, or does not extend what was streamed (appending it
// would corrupt the client's concatenation, so it is dropped).
func (s *responsesStream) missing(kind string, idx int, full string) (rest string, ok bool) {
	sent := ""
	if b := s.sent[sentKey{kind, idx}]; b != nil {
		sent = b.String()
	}
	if len(full) <= len(sent) || !strings.HasPrefix(full, sent) {
		return "", false
	}
	return full[len(sent):], true
}

// completeCall emits the argument suffix a completed function_call carries
// beyond its argument deltas.
func (s *responsesStream) completeCall(idx int, item map[string]any) error {
	if s.dropped[idx] {
		return nil
	}
	args, _ := item["arguments"].(string)
	rest, ok := s.missing(sentArgs, idx, args)
	if !ok {
		return nil
	}
	block, known := s.itemBlocks[idx]
	if !known || block != s.current || s.kind != "tool" {
		// Same rule as an interleaved fragment: a completed call whose
		// block is no longer open cannot be reopened in Anthropic SSE.
		slog.Debug("zen responses stream: dropping trailing function_call arguments", "outputIndex", idx, "openBlock", s.current)
		return nil
	}
	s.record(sentArgs, idx, rest)
	return s.delta(map[string]any{"type": "input_json_delta", "partial_json": rest})
}

// openCall opens the tool_use block for a function_call item. A call to a
// gate-injected tool the client never declared opens nothing and is marked
// dropped: a tool_use block would hand the client an unresolvable name.
func (s *responsesStream) openCall(idx int, item map[string]any) error {
	if s.dropped[idx] {
		return nil
	}
	name, _ := item["name"].(string)
	if s.injected[name] {
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
}

// completeItem emits whatever a completed output item carries beyond what its
// deltas already delivered.
func (s *responsesStream) completeItem(idx int, item map[string]any) error {
	switch item["type"] {
	case "function_call":
		// A call whose output_item.added never arrived has no block yet.
		if _, known := s.itemBlocks[idx]; !known {
			if err := s.openCall(idx, item); err != nil {
				return err
			}
		}
		return s.completeCall(idx, item)
	case "message":
		return s.completeMessage(idx, item)
	case "reasoning":
		return s.completeReasoning(idx, item)
	}
	return nil
}

// completeMessage emits the refusal and text a completed message item carries
// beyond its deltas. The refusal goes first, the order the aggregator and
// ResponsesResponseToAnthropic give it, and marks the turn stop_reason refusal.
func (s *responsesStream) completeMessage(idx int, item map[string]any) error {
	var text, refusal strings.Builder
	for _, raw := range anySlice(item["content"]) {
		part, _ := raw.(map[string]any)
		switch part["type"] {
		case "output_text":
			t, _ := part["text"].(string)
			text.WriteString(t)
		case "refusal":
			r, _ := part["refusal"].(string)
			refusal.WriteString(r)
		}
	}
	if rest, ok := s.missing(sentRefusal, idx, refusal.String()); ok {
		s.stop = "refusal"
		s.record(sentRefusal, idx, rest)
		if err := s.writeText(idx, rest); err != nil {
			return err
		}
	}
	if rest, ok := s.missing(sentText, idx, text.String()); ok {
		s.record(sentText, idx, rest)
		return s.writeText(idx, rest)
	}
	return nil
}

// completeReasoning emits the summary text a completed reasoning item carries
// beyond its summary deltas. With no streamed deltas the part boundaries
// survive and are joined readably; once deltas flowed they carry no part
// boundaries, so the restatement stays fused — joining there would break
// missing()'s prefix match and drop text the client has not yet received.
func (s *responsesStream) completeReasoning(idx int, item map[string]any) error {
	var parts []string
	for _, raw := range anySlice(item["summary"]) {
		part, _ := raw.(map[string]any)
		if t, _ := part["text"].(string); t != "" {
			parts = append(parts, t)
		}
	}
	if b := s.sent[sentKey{sentThinking, idx}]; b == nil || b.Len() == 0 {
		if len(parts) == 0 {
			return nil
		}
		joined := strings.Join(parts, "\n\n")
		s.record(sentThinking, idx, joined)
		return s.writeThinking(idx, joined)
	}
	fused := strings.Join(parts, "")
	rest, ok := s.missing(sentThinking, idx, fused)
	if !ok {
		return nil
	}
	s.record(sentThinking, idx, rest)
	return s.writeThinking(idx, rest)
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
		kind := sentText
		if typ == "response.refusal.delta" {
			s.stop = "refusal"
			kind = sentRefusal
		}
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		s.record(kind, idx, delta)
		return s.writeText(idx, delta)
	case "response.reasoning_summary_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		s.record(sentThinking, idx, delta)
		return s.writeThinking(idx, delta)
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item == nil || item["type"] != "function_call" {
			return nil
		}
		return s.openCall(idx, item)
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
		s.record(sentArgs, idx, delta)
		return s.delta(map[string]any{"type": "input_json_delta", "partial_json": delta})
	case "response.output_item.done":
		// The completed item restates everything the deltas carried. Deltas
		// are the normal path, but an upstream that delivers an item only
		// here (a call with no argument deltas, text or a refusal with no
		// text deltas) would otherwise hand the client an empty block or a
		// clean end_turn. Emit whatever the deltas did not already cover, so
		// the fragments and the completed value cannot concatenate into
		// malformed JSON or repeated text.
		item, _ := event["item"].(map[string]any)
		if item == nil {
			return nil
		}
		return s.completeItem(idx, item)
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
					// A refusal outranks the output cap, as in
					// responsesStopReason: the client must see the decline.
					if s.stop != "refusal" {
						s.stop = "max_tokens"
					}
				}
			}
			// Fallback: a stream with no deltas or item envelopes still
			// carries the answer in response.output. Each item goes through
			// the same completion as output_item.done; content the deltas
			// already delivered is a no-op via missing(). Items are matched to
			// streamed state by array position, the same output_index ==
			// position assumption ResponsesResponseToAnthropic makes when it
			// numbers calls.
			for i, raw := range anySlice(resp["output"]) {
				item, _ := raw.(map[string]any)
				if item == nil {
					continue
				}
				if err := s.completeItem(i, item); err != nil {
					return err
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
	// tool_use is claimed only while a tool_use block survived: a turn whose
	// calls were all dropped as gate-injected would otherwise make the client
	// wait for a block that never came.
	if s.toolItems > 0 && stop == "" {
		stop = "tool_use"
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
