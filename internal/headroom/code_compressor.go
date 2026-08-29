package headroom

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var multipleBlankLinesRegex = regexp.MustCompile(`\n{3,}`)

// repeatThreshold is the run length at which identical consecutive lines are
// collapsed. Below this a run is cheaper to keep than to annotate.
const repeatThreshold = 3

type CodeCompressorStage struct{}

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

func (s *CodeCompressorStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.CodeCompressor {
		return nil
	}
	WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if SkipVerbatim(reqCtx, cfg, ord) {
			return // pruning trailing whitespace or folding repeated lines breaks
			// the byte-exact old_string a later Edit draws from this payload
		}
		before := get()
		after := PruneText(before)
		if after != before {
			set(after)
			reqCtx.RecordRewrite(before, after)
		}
	})
	return nil
}
