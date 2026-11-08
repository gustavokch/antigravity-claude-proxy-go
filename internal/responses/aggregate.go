package responses

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Aggregate folds a Responses SSE stream into the single response object a
// non-streaming call would have returned, ready for ToAnthropic. Output items
// are reconstructed from the item-added/delta events: text and reasoning
// deltas are concatenated per output_index, and a function call's arguments
// are concatenated and then overridden by the complete value the
// response.output_item.done event carries. A reasoning item keeps its
// encrypted_content and a web_search_call item is kept whole, so both reach
// the client.
//
// A stream that ends without its terminal event is reported as
// ErrStreamTruncated rather than a partial answer: a dropped connection must
// not pass off a truncated reply as complete. An upstream failure event is
// returned as an error carrying the upstream's reason.
func Aggregate(r io.Reader) (map[string]any, error) {
	items := map[int]map[string]any{}
	texts := map[int]*strings.Builder{}
	reasoning := map[int]*strings.Builder{}
	summaryPart := map[int]int{}
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
				b := builder(reasoning, idx)
				if summaryBoundary(event, idx, summaryPart) && b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(delta)
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
		case "response.completed", "response.incomplete", "response.done":
			// response.done is the terminal event's other spelling; it
			// carries the same response object as response.completed.
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
			failed = failureText(event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStreamTruncated, err)
	}
	if failed != "" {
		return nil, errors.New(failed)
	}
	if !done {
		return nil, ErrStreamTruncated
	}
	// Fallback: a stream with no deltas or item envelopes still carries the
	// answer in response.completed's output. Adopt the completed item only for
	// an index with no envelope (never seen, or seen only as a delta
	// placeholder); deltas and envelopes already streamed always win.
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
		if len(item) == 0 {
			// Only deltas were seen for this index (no item envelope).
			if b := reasoning[idx]; b != nil && b.Len() > 0 {
				output = append(output, reasoningItem(b.String(), ""))
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
			text := ""
			if src := reasoning[idx]; src != nil {
				text = src.String()
			}
			if text == "" {
				text = strings.Join(summaryTexts(item), "\n\n")
			}
			signature, _ := item["encrypted_content"].(string)
			if text == "" && signature == "" {
				continue
			}
			output = append(output, reasoningItem(text, signature))
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
		case "web_search_call":
			// Nothing streams for a search; the item envelope is the whole
			// record ToAnthropic turns into the server tool blocks.
			output = append(output, item)
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

// reasoningItem builds a folded reasoning output item: the summary text as one
// part, and the encrypted reasoning when the upstream sent it.
func reasoningItem(text, signature string) map[string]any {
	item := map[string]any{"type": "reasoning", "summary": []any{}}
	if text != "" {
		item["summary"] = []any{map[string]any{"type": "summary_text", "text": text}}
	}
	if signature != "" {
		item["encrypted_content"] = signature
	}
	return item
}

// messageItem wraps content parts as an assistant message output item.
func messageItem(parts []any) map[string]any {
	return map[string]any{"type": "message", "role": "assistant", "content": parts}
}
