package format

import (
	"encoding/json"
	"errors"
)

// ErrOutputLimit ends a Cloud Code stream that reached the client's output
// token limit. The caller treats it as a finished response.
var ErrOutputLimit = errors.New("client output token limit reached")

// bytesPerToken converts streamed text to tokens where upstream reports no
// running count: about four bytes of English per token.
const bytesPerToken = 4

// OutputGuard enforces CloudCodeRequest.OutputLimit on a Cloud Code SSE
// stream. The request carries the catalog's maxOutputTokens, as agy sends, so
// upstream no longer stops at the client's limit; the guard stops where
// upstream would have. Google counts thought tokens against maxOutputTokens,
// so the guard counts them too. Gemini reports a running count on every
// event; Claude repeats its first count until the last event, so the text
// streamed since the reported count last changed is added as an estimate.
// A tool call's arguments count as streamed text. The event that crosses the
// limit is delivered whole.
type OutputGuard struct {
	limit    int
	reported int            // candidate + thought tokens upstream last reported
	pending  int            // bytes of text streamed since reported last changed
	usage    map[string]any // the last usageMetadata upstream sent
}

// NewOutputGuard returns nil, a guard that never stops, for limit <= 0.
func NewOutputGuard(limit int) *OutputGuard {
	if limit <= 0 {
		return nil
	}
	return &OutputGuard{limit: limit}
}

type guardedEvent struct {
	Response *struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Args json.RawMessage `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata map[string]any `json:"usageMetadata"`
	} `json:"response"`
}

// Observe reads one delivered event. When the response has reached the limit
// without finishing, it returns the event to deliver in place of the rest of
// the stream: finishReason MAX_TOKENS with the usage counted so far. The
// caller then stops the stream with ErrOutputLimit.
func (guard *OutputGuard) Observe(data []byte) ([]byte, bool) {
	if guard == nil {
		return nil, false
	}
	var event guardedEvent
	if json.Unmarshal(data, &event) != nil || event.Response == nil {
		return nil, false
	}
	text := 0
	for _, candidate := range event.Response.Candidates {
		if candidate.FinishReason != "" {
			return nil, false
		}
		for _, part := range candidate.Content.Parts {
			text += len(part.Text)
			if part.FunctionCall != nil {
				text += len(part.FunctionCall.Args)
			}
		}
	}
	usage := event.Response.UsageMetadata
	if reported := intValue(usage["candidatesTokenCount"], 0) + intValue(usage["thoughtsTokenCount"], 0); usage != nil && reported != guard.reported {
		// The new count already includes this event's text.
		guard.reported, guard.pending = reported, 0
	} else {
		guard.pending += text
	}
	if usage != nil {
		guard.usage = usage
	}
	used := guard.reported + (guard.pending+bytesPerToken-1)/bytesPerToken
	if used < guard.limit {
		return nil, false
	}
	// The finish carries the last usage seen, which an event without usage
	// would otherwise drop (prompt and thought counts).
	thoughts := intValue(guard.usage["thoughtsTokenCount"], 0)
	counted := make(map[string]any, len(guard.usage)+1)
	for key, value := range guard.usage {
		counted[key] = value
	}
	counted["candidatesTokenCount"] = used - thoughts
	final, err := json.Marshal(map[string]any{"response": map[string]any{
		"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{}}, "finishReason": "MAX_TOKENS"}},
		"usageMetadata": counted,
	}})
	if err != nil {
		return nil, false
	}
	return final, true
}
