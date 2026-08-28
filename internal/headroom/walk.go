package headroom

// walkToolResultText visits every text payload inside every tool_result block
// of every message at index >= from, in document order. Per invariant I3 it
// never visits user prompt text, assistant text, thinking blocks, signatures,
// tool_use inputs, or images.
//
// idx is the message index. ord is the payload's position in the full document
// order — counted from message 0 regardless of from, so it is stable across
// stages and usable as a ToolInspector key. get returns the current text; set
// replaces it. Both are closures over the underlying map so callers do not
// need to know which content shape they are in.
func walkToolResultText(req map[string]any, from int, fn func(idx, ord int, get func() string, set func(string))) {
	messages, ok := req["messages"].([]any)
	if !ok {
		return
	}
	if from < 0 {
		from = 0
	}
	ord := 0
	for i := 0; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				continue
			}
			switch payload := block["content"].(type) {
			case string:
				b := block
				o := ord
				ord++
				if i >= from {
					fn(i, o, func() string { return b["content"].(string) },
						func(s string) { b["content"] = s })
				}
			case []any:
				for _, innerRaw := range payload {
					inner, ok := innerRaw.(map[string]any)
					if !ok || inner["type"] != "text" {
						continue
					}
					if _, ok := inner["text"].(string); !ok {
						continue
					}
					b := inner
					o := ord
					ord++
					if i >= from {
						fn(i, o, func() string { return b["text"].(string) },
							func(s string) { b["text"] = s })
					}
				}
			}
		}
	}
}

// walkToolUseBlocks visits every tool_use block in every message, in document
// order. Unlike walkToolResultText it is read-only.
func walkToolUseBlocks(req map[string]any, fn func(idx int, block map[string]any)) {
	messages, ok := req["messages"].([]any)
	if !ok {
		return
	}
	for i := 0; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			fn(i, block)
		}
	}
}
