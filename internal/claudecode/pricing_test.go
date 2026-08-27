package claudecode

import (
	"math"
	"testing"
)

func TestGetModelPricing(t *testing.T) {
	tests := []struct {
		modelID        string
		expectedPrompt float64
		expectedComp   float64
	}{
		{"claude-fable-5", 10.0 / 1e6, 50.0 / 1e6},
		{"claude-opus-5", 5.0 / 1e6, 25.0 / 1e6},
		{"claude-sonnet-5", 2.0 / 1e6, 10.0 / 1e6},
		{"claude-haiku-4-5-20251001", 1.0 / 1e6, 5.0 / 1e6},
		{"claude-3-7-sonnet-20250219", 3.0 / 1e6, 15.0 / 1e6},
		{"claude-3-5-sonnet-20241022", 3.0 / 1e6, 15.0 / 1e6},
		{"claude-3-5-haiku-20241022", 0.8 / 1e6, 4.0 / 1e6},
		{"claude-3-opus-20240229", 15.0 / 1e6, 75.0 / 1e6},
		// Prefix/alias matching
		{"claude-3.7-sonnet", 3.0 / 1e6, 15.0 / 1e6},
		{"claude-sonnet-5-latest", 2.0 / 1e6, 10.0 / 1e6},
		{"claude-haiku", 1.0 / 1e6, 5.0 / 1e6},
		{"claude-3-haiku-20240307", 1.0 / 1e6, 5.0 / 1e6},
		{"claude-opus", 5.0 / 1e6, 25.0 / 1e6},
		{"unknown-model", 2.0 / 1e6, 10.0 / 1e6}, // default fallback
	}

	for _, tt := range tests {
		p := GetModelPricing(tt.modelID)
		if math.Abs(p.Prompt-tt.expectedPrompt) > 1e-9 {
			t.Errorf("model %s: expected prompt price %v, got %v", tt.modelID, tt.expectedPrompt, p.Prompt)
		}
		if math.Abs(p.Completion-tt.expectedComp) > 1e-9 {
			t.Errorf("model %s: expected completion price %v, got %v", tt.modelID, tt.expectedComp, p.Completion)
		}
	}
}

func TestCalculateCost(t *testing.T) {
	// For claude-sonnet-5:
	// Prompt: $2/MTok, Comp: $10/MTok, CacheWrite: $2.50/MTok, CacheRead: $0.20/MTok
	// 1,000 prompt tokens = $0.002
	// 500 completion tokens = $0.005
	// 2,000 cache write = $0.005
	// 10,000 cache read = $0.002
	// Total expected = 0.002 + 0.005 + 0.005 + 0.002 = $0.014
	cost := CalculateCost("claude-sonnet-5", 1000, 500, 2000, 10000)
	expected := 0.014
	if math.Abs(cost-expected) > 1e-6 {
		t.Errorf("expected cost %f, got %f", expected, cost)
	}
}
