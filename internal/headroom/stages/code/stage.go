package code

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"antigravity-go-proxy/internal/headroom"
)

var multipleBlankLinesRegex = regexp.MustCompile(`\n{3,}`)

// repeatThreshold is the run length at which identical consecutive lines are
// collapsed. Below this a run is cheaper to keep than to annotate.
const repeatThreshold = 3

type CodeCompressorStage struct{}

func NewStage() *CodeCompressorStage {
	return &CodeCompressorStage{}
}

func (s *CodeCompressorStage) Name() string { return "code_compressor" }

// PruneText removes trailing whitespace, collapses blank-line runs, and folds
// runs of identical lines. Leading whitespace is preserved: it is the
// indentation. The function is pure and idempotent (invariant I1).
func PruneText(input string) string {
	lines := strings.Split(input, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}

	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		line := lines[i]
		run := 1
		for i+run < len(lines) && lines[i+run] == line {
			run++
		}
		out = append(out, line)
		if line != "" && run >= repeatThreshold {
			out = append(out, fmt.Sprintf("[... repeated %d times ...]", run-1))
		} else {
			for j := 1; j < run; j++ {
				out = append(out, line)
			}
		}
		i += run
	}

	return multipleBlankLinesRegex.ReplaceAllString(strings.Join(out, "\n"), "\n\n")
}

func (s *CodeCompressorStage) Execute(ctx context.Context, reqCtx *headroom.RequestContext, cfg *headroom.Config) error {
	if !cfg.CodeCompressor || reqCtx == nil || reqCtx.Request == nil {
		return nil
	}
	log := reqCtx.Log()
	dbg := log.Enabled(ctx, slog.LevelDebug)

	headroom.WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if headroom.SkipVerbatim(reqCtx, cfg, ord) {
			return
		}
		before := get()
		after := PruneText(before)
		if after == before {
			return
		}
		set(after)
		reqCtx.RecordRewrite(before, after)
		if dbg {
			log.Debug("code_compressor pruned tool output",
				"stage", s.Name(), "ordinal", ord,
				"bytes_before", len(before), "bytes_after", len(after))
		}
	})
	return nil
}
