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
	WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if SkipVerbatim(reqCtx, cfg, ord) {
			return // CompactJSON is a no-op on cat -n output, but a raw JSON
			// file read starts with '{' and TryTabularConversion is lossy
		}
		before := get()
		if cfg.TabularArrays {
			if table, converted := TryTabularConversion(before, DefaultMinTabularSavings); converted {
				set(table)
				reqCtx.RecordRewrite(before, table)
				return
			}
		}
		if after, changed := CompactJSON(before); changed {
			set(after)
			reqCtx.RecordRewrite(before, after)
		}
	})
	return nil
}
