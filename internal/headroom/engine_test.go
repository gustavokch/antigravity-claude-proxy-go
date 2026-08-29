package headroom

import (
	"bytes"
	"context"
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
