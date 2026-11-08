package responses

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

// Stream converts a Responses SSE stream into the Anthropic SSE event sequence
// on w: message_start, a content block per output item (thinking, text,
// tool_use, or a server_tool_use and web_search_tool_result pair),
// message_delta with the derived stop_reason and Anthropic usage, then
// message_stop. Calls to tools in opts.Drop are dropped before any content
// block opens.
//
// A stream that ends before its terminal event is reported to the client as an
// Anthropic error event and returned as ErrStreamTruncated. An upstream
// failure event is reported the same way and returns nil: the upstream said it
// failed, and the client was told.
func Stream(r io.Reader, w io.Writer, opts Options) error {
	s := &stream{
		w: w, opts: opts, current: -1,
		sent: map[sentKey]*strings.Builder{}, summaryPart: map[int]int{},
		thoughts: map[int]int{}, signed: map[int]bool{}, pendingSig: map[int]string{},
		searched: map[int]bool{}, calls: map[int]*call{},
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
		s.emitError("api_error", opts.errorMessage(" ", "stream read error: "+err.Error()))
		return fmt.Errorf("%w: %v", ErrStreamTruncated, err)
	}
	// A clean EOF without the terminal event is a truncated stream (dropped
	// connection, proxy timeout); finishing it as end_turn would pass a partial
	// answer off as complete.
	if !done && !s.settled {
		s.emitError("api_error", opts.errorMessage(" ", "stream ended before completion"))
		return ErrStreamTruncated
	}
	return s.finish()
}

// stream is the Anthropic SSE emitter for the Responses wire. The Responses
// stream has no per-chunk usage, so the usage that arrives with
// response.completed is carried until finish.
//
// Anthropic SSE has one open content block at a time and a closed block cannot
// be reopened, while Responses events for different output items may
// interleave. Function calls are therefore serialized: a call opens its
// tool_use block only once the call before it is done, its argument fragments
// wait in its buffer until then, and every other event is held back while a
// tool_use block is open (see holds), so nothing cuts a call's arguments short.
type stream struct {
	w           io.Writer
	opts        Options
	sent        map[sentKey]*strings.Builder // delivered output per (kind, output_index)
	summaryPart map[int]int                  // output_index → summary_index of the last reasoning delta
	thoughts    map[int]int                  // reasoning output_index → Anthropic block holding its summary
	signed      map[int]bool                 // reasoning items whose signature was delivered
	pendingSig  map[int]string               // encrypted_content seen on output_item.added
	searched    map[int]bool                 // web_search_call items delivered
	calls       map[int]*call                // function_call items by output_index
	queue       []*call                      // calls waiting for their tool_use block, in arrival order
	active      *call                        // call whose tool_use block is open
	deferred    []map[string]any             // events held while a tool_use block is open
	started     bool
	failed      bool
	settled     bool
	toolItems   int // tool_use blocks opened (surviving dropped calls)
	nextIndex   int
	current     int    // open Anthropic block index, -1 when none
	kind        string // "thinking" | "text" | "tool" | "server_tool_use" | "web_search_tool_result"
	stop        string
	usage       map[string]any
}

// call is one function_call output item on its way to a tool_use block.
type call struct {
	idx      int
	id, name string
	args     strings.Builder // every argument fragment received
	emitted  int             // bytes of args already sent as input_json_delta
	done     bool            // the upstream finished the item, so its block can close
	closed   bool            // its tool_use block opened and closed
	dropped  bool            // a call to a tool in Options.Drop; never opens
}

// Kinds of output the stream has delivered to the client, tracked per output
// item so output_item.done can emit only what the deltas did not.
const (
	sentText     = "text"
	sentRefusal  = "refusal"
	sentThinking = "thinking"
)

type sentKey struct {
	kind string
	idx  int
}

func (s *stream) emit(event string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

func (s *stream) emitError(kind, msg string) {
	s.failed = true
	_ = s.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
}

func (s *stream) start(id any) error {
	if s.started {
		return nil
	}
	s.started = true
	return s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.opts.messageID(id), "type": "message", "role": "assistant", "model": s.opts.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *stream) closeBlock() error {
	if s.current < 0 {
		return nil
	}
	idx := s.current
	s.current, s.kind = -1, ""
	return s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (s *stream) openBlock(kind string, block map[string]any) error {
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.current, s.kind = s.nextIndex, kind
	s.nextIndex++
	return s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": s.current, "content_block": block})
}

func (s *stream) delta(delta map[string]any) error {
	return s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.current, "delta": delta})
}

// writeText streams text into the open text block, opening one first when a
// different kind of block is open.
func (s *stream) writeText(text string) error {
	if s.kind != "text" {
		if err := s.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
			return err
		}
	}
	return s.delta(map[string]any{"type": "text_delta", "text": text})
}

// writeThinking is writeText for a reasoning summary. It records the block the
// item's summary is in, so the item's signature can close that block.
func (s *stream) writeThinking(idx int, text string) error {
	if s.kind != "thinking" {
		if err := s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""}); err != nil {
			return err
		}
	}
	s.thoughts[idx] = s.current
	return s.delta(map[string]any{"type": "thinking_delta", "thinking": text})
}

// sign delivers a reasoning item's encrypted_content as the signature of its
// thinking block: a signature_delta right before that block's
// content_block_stop. An item that streamed no summary text, or whose block
// another block already closed, gets a thinking block of its own with empty
// text, so the signature is never lost. The next turn hands it back as an
// encrypted reasoning item.
func (s *stream) sign(idx int, signature string) error {
	if signature == "" || s.signed[idx] {
		return nil
	}
	s.signed[idx] = true
	if block, ok := s.thoughts[idx]; !ok || block != s.current || s.kind != "thinking" {
		if err := s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""}); err != nil {
			return err
		}
		s.thoughts[idx] = s.current
	}
	if err := s.delta(map[string]any{"type": "signature_delta", "signature": signature}); err != nil {
		return err
	}
	return s.closeBlock()
}

// hasSent reports whether any text was delivered for one (kind, idx) stream.
func (s *stream) hasSent(kind string, idx int) bool {
	b := s.sent[sentKey{kind, idx}]
	return b != nil && b.Len() > 0
}

// summaryBoundary reports whether a reasoning_summary_text.delta opens a new
// summary part of its item, and records the part it belongs to. The OpenAI
// Responses schema tags every such delta with summary_index; an untagged delta
// never reports a boundary, so an upstream that omits the field keeps its
// deltas fused.
func summaryBoundary(event map[string]any, idx int, last map[int]int) bool {
	raw, tagged := event["summary_index"]
	if !tagged {
		return false
	}
	part := toInt(raw)
	prev, seen := last[idx]
	last[idx] = part
	return seen && part != prev
}

// record notes text as delivered to the client for one (kind, idx) stream.
func (s *stream) record(kind string, idx int, text string) {
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
func (s *stream) missing(kind string, idx int, full string) (rest string, ok bool) {
	sent := ""
	if b := s.sent[sentKey{kind, idx}]; b != nil {
		sent = b.String()
	}
	if len(full) <= len(sent) || !strings.HasPrefix(full, sent) {
		return "", false
	}
	return full[len(sent):], true
}

// callAt returns the function call at output index idx, queueing it the first
// time any of its events arrives: calls open in the order the upstream
// started them.
func (s *stream) callAt(idx int) *call {
	c := s.calls[idx]
	if c == nil {
		c = &call{idx: idx}
		s.calls[idx] = c
		s.queue = append(s.queue, c)
	}
	return c
}

// identify records the call id and name an item envelope carries. A call to a
// tool in Options.Drop is marked so it never opens: a tool_use block would
// hand the client a name it cannot resolve.
func (s *stream) identify(c *call, item map[string]any) {
	if id, _ := item["call_id"].(string); id != "" && c.id == "" {
		c.id = id
	}
	if name, _ := item["name"].(string); name != "" && c.name == "" {
		c.name = name
		if s.opts.Drop[name] {
			c.dropped = true
			slog.Debug("responses stream: dropping call to a dropped tool", "outputIndex", c.idx, "tool", name)
		}
	}
}

// completeArgs adopts the complete argument string a finished item carries
// when it extends the fragments received; the client concatenates
// partial_json, so a value that diverges from what streamed cannot be
// appended without corrupting the input.
func (c *call) completeArgs(full string) {
	sent := c.args.String()
	if len(full) > len(sent) && strings.HasPrefix(full, sent) {
		c.args.WriteString(full[len(sent):])
	}
}

// flushArgs sends the open call the argument bytes the client does not have.
func (s *stream) flushArgs(c *call) error {
	args := c.args.String()
	if c.emitted >= len(args) {
		return nil
	}
	rest := args[c.emitted:]
	c.emitted = len(args)
	return s.delta(map[string]any{"type": "input_json_delta", "partial_json": rest})
}

// pump advances the tool_use blocks: it flushes the open call's buffered
// arguments, closes its block once the upstream finished the call, and opens
// the next queued call that has a name. When no tool_use block is left open,
// the events held back meanwhile are replayed.
func (s *stream) pump() error {
	for {
		if c := s.active; c != nil {
			if err := s.flushArgs(c); err != nil {
				return err
			}
			if !c.done {
				return nil
			}
			c.closed = true
			s.active = nil
			if err := s.closeBlock(); err != nil {
				return err
			}
		}
		for len(s.queue) > 0 && s.queue[0].dropped {
			s.queue = s.queue[1:]
		}
		// A call whose fragments arrived before its envelope waits for the
		// name; the calls behind it wait too, to keep the upstream's order.
		if len(s.queue) == 0 || s.queue[0].name == "" {
			break
		}
		c := s.queue[0]
		s.queue = s.queue[1:]
		id := c.id
		if id == "" {
			id = "call_" + strconv.Itoa(c.idx)
		}
		if err := s.openBlock("tool", map[string]any{
			"type": "tool_use", "id": id,
			"name": s.opts.toolName(c.name), "input": map[string]any{},
		}); err != nil {
			return err
		}
		s.active = c
		s.toolItems++
	}
	for s.active == nil && len(s.deferred) > 0 {
		event := s.deferred[0]
		s.deferred = s.deferred[1:]
		if err := s.handle(event); err != nil {
			return err
		}
	}
	return nil
}

// holds reports whether an event must wait while a tool_use block is open.
// Call events go through (fragments of a queued call are buffered on it), and
// so do terminal events; anything else would open a block of its own and cut
// the open call's arguments short.
func holds(typ string, event map[string]any) bool {
	switch typ {
	case "response.function_call_arguments.delta",
		"response.completed", "response.incomplete", "response.done",
		"response.failed", "response.error", "error":
		return false
	case "response.output_item.added", "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		return item["type"] != "function_call"
	}
	return true
}

// completeItem emits whatever a completed output item carries beyond what its
// deltas already delivered.
func (s *stream) completeItem(idx int, item map[string]any) error {
	switch item["type"] {
	case "function_call":
		c := s.callAt(idx)
		s.identify(c, item)
		if !c.closed {
			args, _ := item["arguments"].(string)
			c.completeArgs(args)
			c.done = true
		}
		return s.pump()
	case "message":
		return s.completeMessage(idx, item)
	case "reasoning":
		return s.completeReasoning(idx, item)
	case "web_search_call":
		return s.completeWebSearch(idx, item)
	}
	return nil
}

// completeMessage emits the refusal and text a completed message item carries
// beyond its deltas. The refusal goes first, the order Aggregate and
// ToAnthropic give it, and marks the turn stop_reason refusal.
func (s *stream) completeMessage(idx int, item map[string]any) error {
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
		if err := s.writeText(rest); err != nil {
			return err
		}
	}
	if rest, ok := s.missing(sentText, idx, text.String()); ok {
		s.record(sentText, idx, rest)
		return s.writeText(rest)
	}
	return nil
}

// completeReasoning emits the summary text a completed reasoning item carries
// beyond its summary deltas, then its signature. Parts are joined with a blank
// line, the same separator tagged live deltas (summary_index) were given, so
// the joined text extends what was streamed. Untagged deltas arrive fused, so
// the fused form is the fallback comparison; either way only the missing
// suffix is emitted. The final encrypted_content is the one on the completed
// item; the snapshot from output_item.added stands in when it has none.
func (s *stream) completeReasoning(idx int, item map[string]any) error {
	parts := summaryTexts(item)
	for _, full := range [2]string{strings.Join(parts, "\n\n"), strings.Join(parts, "")} {
		if rest, ok := s.missing(sentThinking, idx, full); ok {
			s.record(sentThinking, idx, rest)
			if err := s.writeThinking(idx, rest); err != nil {
				return err
			}
			break
		}
	}
	signature, _ := item["encrypted_content"].(string)
	if signature == "" {
		signature = s.pendingSig[idx]
	}
	return s.sign(idx, signature)
}

// completeWebSearch emits a finished web search as Anthropic's server tool
// pair: a server_tool_use block whose input streams as one input_json_delta,
// then the web_search_tool_result block with the pages found. The search runs
// upstream, so neither block asks the client for anything and the turn's
// stop_reason is unaffected.
func (s *stream) completeWebSearch(idx int, item map[string]any) error {
	if s.searched[idx] {
		return nil
	}
	search, ok := webSearchOf(idx, item)
	if !ok {
		return nil
	}
	s.searched[idx] = true
	if err := s.openBlock("server_tool_use", map[string]any{
		"type": "server_tool_use", "id": search.id, "name": "web_search", "input": map[string]any{},
	}); err != nil {
		return err
	}
	if search.query != "" {
		input, err := json.Marshal(search.input())
		if err != nil {
			return err
		}
		if err := s.delta(map[string]any{"type": "input_json_delta", "partial_json": string(input)}); err != nil {
			return err
		}
	}
	if err := s.openBlock("web_search_tool_result", search.result()); err != nil {
		return err
	}
	return s.closeBlock()
}

// handle consumes one Responses SSE event. Text, reasoning, and refusal deltas
// open their block on the first delta (an empty block would be noise); a
// function_call opens its tool_use block when pump reaches it.
func (s *stream) handle(event map[string]any) error {
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
	if s.active != nil && holds(typ, event) {
		s.deferred = append(s.deferred, event)
		return nil
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
		return s.writeText(delta)
	case "response.reasoning_summary_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		if summaryBoundary(event, idx, s.summaryPart) && s.hasSent(sentThinking, idx) {
			delta = "\n\n" + delta
		}
		s.record(sentThinking, idx, delta)
		return s.writeThinking(idx, delta)
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		switch item["type"] {
		case "function_call":
			s.identify(s.callAt(idx), item)
			return s.pump()
		case "reasoning":
			if signature, _ := item["encrypted_content"].(string); signature != "" {
				s.pendingSig[idx] = signature
			}
		}
		return nil
	case "response.function_call_arguments.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		c := s.callAt(idx)
		if c.dropped {
			return nil
		}
		if c.closed {
			// The call's block is already closed and Anthropic SSE cannot
			// reopen it; appending elsewhere would corrupt another input.
			slog.Debug("responses stream: dropping arguments for a finished call", "outputIndex", idx)
			return nil
		}
		c.args.WriteString(delta)
		if c == s.active {
			return s.flushArgs(c)
		}
		return nil
	case "response.output_item.done":
		// The completed item restates everything the deltas carried. Deltas
		// are the normal path, but an upstream that delivers an item only
		// here (a call with no argument deltas, text or a refusal with no
		// text deltas, a reasoning item's signature, a web search) would
		// otherwise hand the client an empty block or a clean end_turn. Emit
		// whatever the deltas did not already cover, so the fragments and the
		// completed value cannot concatenate into malformed JSON or repeated
		// text.
		item, _ := event["item"].(map[string]any)
		if item == nil {
			return nil
		}
		return s.completeItem(idx, item)
	case "response.completed", "response.incomplete", "response.done":
		// response.done is the terminal event's other spelling; it carries
		// the same response object as response.completed.
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
					// A refusal outranks the output cap, as in stopReason:
					// the client must see the decline.
					if s.stop != "refusal" {
						s.stop = "max_tokens"
					}
				}
			}
			// Fallback: a stream with no deltas or item envelopes still
			// carries the answer in response.output. Each item goes through
			// the same completion as output_item.done (held back like one
			// while a tool_use block is open); content the deltas already
			// delivered is a no-op via missing(). Items are matched to
			// streamed state by array position, the same output_index ==
			// position assumption ToAnthropic makes when it numbers calls.
			for i, raw := range anySlice(resp["output"]) {
				item, _ := raw.(map[string]any)
				if item == nil {
					continue
				}
				if err := s.handle(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item}); err != nil {
					return err
				}
			}
		}
		return nil
	case "response.failed", "response.error", "error":
		s.settled = true
		s.emitError("api_error", s.opts.errorMessage(": ", failureText(event)))
		return nil
	}
	return nil
}

// drain settles the function calls once the stream is over: a call the
// upstream never finished is as complete as it will get, a call that never
// got a name has nothing to open and is discarded, and the remaining calls
// open and close in order. The events held back behind them replay as the
// last block closes.
func (s *stream) drain() error {
	for _, c := range s.calls {
		c.done = true
	}
	named := s.queue[:0]
	for _, c := range s.queue {
		if c.name != "" {
			named = append(named, c)
		}
	}
	s.queue = named
	return s.pump()
}

func (s *stream) finish() error {
	if err := s.start(nil); err != nil {
		return err
	}
	if err := s.drain(); err != nil {
		return err
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	stop := s.stop
	// tool_use is claimed only while a tool_use block survived: a turn whose
	// calls were all dropped would otherwise make the client wait for a block
	// that never came.
	if s.toolItems > 0 && stop == "" {
		stop = "tool_use"
	}
	if stop == "" {
		stop = "end_turn"
	}
	if err := s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": Usage(s.usage),
	}); err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}
