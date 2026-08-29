package headroom

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type mockStage struct {
	name     string
	executed bool
	rewrite  bool
}

func (m *mockStage) Name() string { return m.name }
func (m *mockStage) Execute(_ context.Context, reqCtx *RequestContext, _ *Config) error {
	m.executed = true
	if m.rewrite {
		reqCtx.RecordRewrite("aaaaaaaaaa", "a")
	}
	return nil
}

func TestEngine_ProcessRunsInjectedStagesAndLogsSummary(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stage := &mockStage{name: "mock", rewrite: true}
	engine := NewEngine(Config{Enabled: true}, logger, stage)

	reqCtx, err := engine.Process(context.Background(), map[string]any{"messages": []any{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stage.executed {
		t.Fatal("injected stage did not execute")
	}
	if reqCtx.RewritesCount != 1 {
		t.Fatalf("expected 1 rewrite, got %d", reqCtx.RewritesCount)
	}

	out := buf.String()
	for _, want := range []string{"headroom compression summary", "request_id", "saved_bytes", "duration_ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary log missing %q; got: %s", want, out)
		}
	}
}

func TestEngine_ProcessAssignsDistinctRequestIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine := NewEngine(Config{Enabled: true}, logger, &mockStage{name: "mock", rewrite: true})

	_, _ = engine.Process(context.Background(), map[string]any{"messages": []any{}})
	_, _ = engine.Process(context.Background(), map[string]any{"messages": []any{}})

	if strings.Count(buf.String(), "request_id=1") != 1 || strings.Count(buf.String(), "request_id=2") != 1 {
		t.Fatalf("expected request_id 1 and 2 exactly once each; got: %s", buf.String())
	}
}

func TestEngine_ProcessNilLoggerFallsBackToDefault(t *testing.T) {
	engine := NewEngine(Config{Enabled: true}, nil, &mockStage{name: "mock"})
	if _, err := engine.Process(context.Background(), map[string]any{"messages": []any{}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestEngine_SummaryLogAllocatesNothingWhenDebugDisabled pins invariant I6 at
// the engine level: with DEBUG off, the summary must not build its varargs
// slice or box its int/float values, so a rewriting request must allocate no
// more than a non-rewriting one.
func TestEngine_SummaryLogAllocatesNothingWhenDebugDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	req := map[string]any{"messages": []any{}}

	quiet := NewEngine(Config{Enabled: true}, logger, &mockStage{name: "mock"})
	loud := NewEngine(Config{Enabled: true}, logger, &mockStage{name: "mock", rewrite: true})

	base := testing.AllocsPerRun(200, func() { _, _ = quiet.Process(ctx, req) })
	with := testing.AllocsPerRun(200, func() { _, _ = loud.Process(ctx, req) })

	if with > base {
		t.Fatalf("summary log allocates when debug is disabled: %.0f allocs vs %.0f baseline", with, base)
	}
}

// TestEngine_ProcessZeroLoggerAllocsWhenDebugDisabled pins that Process no
// longer clones a child logger per request via Logger.With("request_id", ...).
// The two allocations left (RequestContext, and the Config copy whose address
// is passed into Pipeline.Run) are unrelated to logging and pre-date this fix.
func TestEngine_ProcessZeroLoggerAllocsWhenDebugDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	req := map[string]any{"messages": []any{}}

	engine := NewEngine(Config{Enabled: true}, logger, &mockStage{name: "mock"})

	allocs := testing.AllocsPerRun(200, func() {
		_, _ = engine.Process(ctx, req)
	})

	if allocs > 2.0 {
		t.Fatalf("expected <= 2 allocs (RequestContext + Config copy, no logger clone), got %.0f allocs", allocs)
	}
}
