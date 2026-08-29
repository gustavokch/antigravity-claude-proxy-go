package smart

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"antigravity-go-proxy/internal/headroom"
)

type SmartCrusherStage struct{}

func NewStage() *SmartCrusherStage {
	return &SmartCrusherStage{}
}

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

func (s *SmartCrusherStage) transform(before string, cfg *headroom.Config) (string, string, bool) {
	if cfg.TabularArrays {
		if table, converted := TryTabularConversion(before, DefaultMinTabularSavings); converted {
			return table, "tabular", true
		}
	}
	if compacted, ok := CompactJSON(before); ok {
		return compacted, "compact", true
	}
	return before, "", false
}

func (s *SmartCrusherStage) Execute(ctx context.Context, reqCtx *headroom.RequestContext, cfg *headroom.Config) error {
	if !cfg.SmartCrusher || reqCtx == nil || reqCtx.Request == nil {
		return nil
	}
	log := reqCtx.Log()
	dbg := log.Enabled(ctx, slog.LevelDebug)

	headroom.WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if headroom.SkipVerbatim(reqCtx, cfg, ord) {
			return
		}
		before := get()
		after, mode, changed := s.transform(before, cfg) // mode is "compact" or "tabular"
		if !changed {
			return
		}
		set(after)
		reqCtx.RecordRewrite(before, after)
		if dbg {
			log.Debug("smart_crusher compacted tool output",
				"stage", s.Name(), "request_id", reqCtx.RequestID, "mode", mode, "ordinal", ord,
				"bytes_before", len(before), "bytes_after", len(after))
		}
	})
	return nil
}
