package headroom

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
)

type mockStage struct {
	name string
	runs int
	err  error
}

func (m *mockStage) Name() string { return m.name }
func (m *mockStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	m.runs++
	return m.err
}

func TestPipeline_RunsAllStagesInOrder(t *testing.T) {
	s1 := &mockStage{name: "stage1"}
	s2 := &mockStage{name: "stage2"}
	p := NewPipeline(s1, s2)

	reqCtx := &RequestContext{Request: map[string]any{"model": "claude-3-5-sonnet"}}

	if err := p.Run(context.Background(), reqCtx, &Config{Enabled: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s1.runs != 1 || s2.runs != 1 {
		t.Errorf("expected each stage to run once, got s1=%d s2=%d", s1.runs, s2.runs)
	}
}

func TestPipeline_SkipsEverythingWhenDisabled(t *testing.T) {
	s1 := &mockStage{name: "stage1"}
	p := NewPipeline(s1)

	if err := p.Run(context.Background(), &RequestContext{}, &Config{Enabled: false}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s1.runs != 0 {
		t.Errorf("expected no stages to run when disabled, got %d runs", s1.runs)
	}
}

func TestPipeline_StopsOnStageError(t *testing.T) {
	boom := errors.New("boom")
	s1 := &mockStage{name: "stage1", err: boom}
	s2 := &mockStage{name: "stage2"}
	p := NewPipeline(s1, s2)

	err := p.Run(context.Background(), &RequestContext{}, &Config{Enabled: true})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if s2.runs != 0 {
		t.Errorf("expected stage2 to be skipped after error")
	}
}

func TestRequestContext_LogIsNilSafe(t *testing.T) {
	reqCtx := &RequestContext{}
	if reqCtx.Log() == nil {
		t.Fatal("Log() must never return nil")
	}
}

func TestRequestContext_RecordRewriteCountsAndAccounts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reqCtx := &RequestContext{Logger: logger}

	reqCtx.RecordRewrite("hello world 12345", "hello")

	if reqCtx.BytesBefore != 17 || reqCtx.BytesAfter != 5 || reqCtx.RewritesCount != 1 {
		t.Fatalf("unexpected telemetry: before=%d after=%d rewrites=%d",
			reqCtx.BytesBefore, reqCtx.BytesAfter, reqCtx.RewritesCount)
	}
	if reqCtx.Log() != logger {
		t.Fatal("Log() must return the injected logger")
	}
}
