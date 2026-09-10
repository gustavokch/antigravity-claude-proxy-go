package cachebump

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// bumpReplayFieldOrder lists the top-level fields written first, in this
// order, so replay bodies are deterministic. Remaining fields follow in
// sorted order with their original bytes untouched.
var bumpReplayFieldOrder = []string{
	"model", "system", "tools", "messages", "metadata",
	"max_tokens", "stream",
}

// BuildReplayBody rewrites a recorded /v1/messages body into a minimal
// cache-warming replay:
//
//   - max_tokens is clamped to at least 1 (floor) — Anthropic rejects <1;
//     some gateways reject tiny values, so callers pass their own floor
//     (e.g. 16 for Kimi/custom endpoints).
//   - thinking is deleted: budget_tokens must stay below max_tokens, and a
//     thinking request with max_tokens 1 is a guaranteed 400.
//   - tool_choice is deleted: a forced tool call cannot complete in one token.
//   - stream is set to false so the result is one JSON object to parse.
//
// system, tools and messages are the cached prefix — their bytes are
// preserved exactly; any edit would invalidate the very entry being
// refreshed. All other top-level fields are also preserved byte-for-byte.
func BuildReplayBody(body []byte, floor int) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("cachebump: decode request body: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("cachebump: request body is not a JSON object")
	}

	if floor < 1 {
		floor = 1
	}

	out := make(map[string]json.RawMessage, len(fields))
	for key, raw := range fields {
		switch key {
		case "thinking", "tool_choice":
			continue
		case "max_tokens":
			out[key] = json.RawMessage(fmt.Sprintf("%d", floor))
		case "stream":
			out[key] = json.RawMessage("false")
		default:
			out[key] = raw
		}
	}
	// A recorded body always has max_tokens/stream, but be defensive.
	if _, ok := out["max_tokens"]; !ok {
		out["max_tokens"] = json.RawMessage(fmt.Sprintf("%d", floor))
	}
	out["stream"] = json.RawMessage("false")

	return marshalOrdered(out), nil
}

func marshalOrdered(fields map[string]json.RawMessage) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')

	used := make(map[string]bool, len(fields))
	writeField := func(key string) {
		raw, ok := fields[key]
		if !ok || used[key] {
			return
		}
		used[key] = true
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(raw)
	}

	for _, key := range bumpReplayFieldOrder {
		writeField(key)
	}
	rest := make([]string, 0, len(fields))
	for key := range fields {
		if !used[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		writeField(key)
	}

	buf.WriteByte('}')
	return buf.Bytes()
}
