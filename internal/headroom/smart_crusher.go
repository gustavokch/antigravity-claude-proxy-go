package headroom

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

type SmartCrusherStage struct{}

func (s *SmartCrusherStage) Name() string { return "smart_crusher" }

// CompactJSON strips insignificant whitespace from a JSON payload. It uses
// json.Compact rather than an unmarshal/marshal round trip so key order and
// numeric literals survive byte-for-byte: both matter for prompt cache
// stability (invariant I1) and for not corrupting large integer IDs.
func CompactJSON(input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return input, false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		return input, false
	}
	compacted := buf.String()
	if len(compacted) < len(input) {
		return compacted, true
	}
	return input, false
}

func (s *SmartCrusherStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.SmartCrusher {
		return nil
	}
	// from=0: history included. Position independence is what keeps the
	// provider prompt cache warm across turns (invariant I1).
	walkToolResultText(reqCtx.Request, 0, func(_ int, get func() string, set func(string)) {
		before := get()
		if after, changed := CompactJSON(before); changed {
			set(after)
			reqCtx.RecordRewrite(before, after)
		}
	})
	return nil
}
