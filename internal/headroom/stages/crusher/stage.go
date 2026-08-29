package crusher

import (
	"context"
	"log/slog"

	"antigravity-go-proxy/internal/headroom"
)

type CommandCrusherStage struct{}

func NewStage() *CommandCrusherStage {
	return &CommandCrusherStage{}
}

func (s *CommandCrusherStage) Name() string { return "command_crusher" }

// CrushCommandOutput detects a known tool-output signature and compresses it.
// Pure and deterministic (invariant I1); input returned unchanged when no
// signature matches or the filter finds nothing to strip.
func CrushCommandOutput(text string) (string, bool) {
	switch detectSignature(text) {
	case sigPytest:
		return crushPytest(text)
	case sigUnittest:
		return crushUnittest(text)
	case sigRuff:
		return crushRuff(text)
	case sigJest:
		return crushJest(text)
	case sigMocha:
		return crushMocha(text)
	case sigTSC:
		return crushTSC(text)
	case sigESLint:
		return crushESLint(text)
	case sigGoTest:
		return crushGoTest(text)
	case sigGolangci:
		return crushGolangci(text)
	case sigCargoTest:
		return crushCargoTest(text)
	case sigCargoBuild:
		return crushCargoBuild(text)
	case sigGitStatus:
		return crushGitStatus(text)
	case sigGitLog:
		return crushGitLog(text)
	}
	return text, false
}

func (s *CommandCrusherStage) Execute(ctx context.Context, reqCtx *headroom.RequestContext, cfg *headroom.Config) error {
	if !cfg.CommandCrusher || reqCtx == nil || reqCtx.Request == nil {
		return nil
	}
	log := reqCtx.Log()
	dbg := log.Enabled(ctx, slog.LevelDebug)

	errOrds := headroom.ErrorOrdinals(reqCtx.Request)
	headroom.WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if errOrds[ord] {
			return // invariant I4
		}
		if headroom.SkipVerbatim(reqCtx, cfg, ord) {
			return // invariant I2
		}
		before := get()
		after, changed := CrushCommandOutput(before)
		if !changed {
			return
		}
		set(after)
		reqCtx.RecordRewrite(before, after)
		if dbg {
			log.Debug("command_crusher compressed tool output",
				"stage", s.Name(),
				"signature", detectSignature(before).String(),
				"ordinal", ord,
				"bytes_before", len(before),
				"bytes_after", len(after))
		}
	})
	return nil
}
