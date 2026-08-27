package headroom

import (
	"context"
	"errors"
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
