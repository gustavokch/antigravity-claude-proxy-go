package openrouter

import (
	"encoding/json"
	"math"
	"testing"
)

func TestPricing_UnmarshalJSON(t *testing.T) {
	jsonData := `{
		"prompt": "0.000003",
		"completion": "0.000015",
		"request": "0.001",
		"image": "0.0048",
		"input_cache_read": "0.0000003",
		"input_cache_write": "0.00000375"
	}`

	var p Pricing
	if err := json.Unmarshal([]byte(jsonData), &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if math.Abs(p.Prompt-0.000003) > 1e-9 {
		t.Errorf("expected prompt 0.000003, got %f", p.Prompt)
	}
	if math.Abs(p.Completion-0.000015) > 1e-9 {
		t.Errorf("expected completion 0.000015, got %f", p.Completion)
	}
	if math.Abs(p.Request-0.001) > 1e-9 {
		t.Errorf("expected request 0.001, got %f", p.Request)
	}
	if math.Abs(p.InputCacheRead-0.0000003) > 1e-10 {
		t.Errorf("expected input_cache_read 0.0000003, got %f", p.InputCacheRead)
	}
	if math.Abs(p.InputCacheWrite-0.00000375) > 1e-10 {
		t.Errorf("expected input_cache_write 0.00000375, got %f", p.InputCacheWrite)
	}
}

func TestPricing_UnmarshalJSON_FloatsAndEmpty(t *testing.T) {
	jsonData := `{
		"prompt": 0.0000025,
		"completion": 0.00001,
		"request": "",
		"cache_read": "0.00000025",
		"cache_write": "0.000003"
	}`

	var p Pricing
	if err := json.Unmarshal([]byte(jsonData), &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if math.Abs(p.Prompt-0.0000025) > 1e-9 {
		t.Errorf("expected prompt 0.0000025, got %f", p.Prompt)
	}
	if math.Abs(p.Completion-0.00001) > 1e-9 {
		t.Errorf("expected completion 0.00001, got %f", p.Completion)
	}
	if p.Request != 0 {
		t.Errorf("expected request 0, got %f", p.Request)
	}
	if math.Abs(p.InputCacheRead-0.00000025) > 1e-10 {
		t.Errorf("expected cache_read 0.00000025, got %f", p.InputCacheRead)
	}
	if math.Abs(p.InputCacheWrite-0.000003) > 1e-10 {
		t.Errorf("expected cache_write 0.000003, got %f", p.InputCacheWrite)
	}
}

func TestCalculateCost(t *testing.T) {
	p := Pricing{
		Prompt:          0.000003, // $3/M
		Completion:      0.000015, // $15/M
		InputCacheRead:  0.0000003, // $0.30/M
		InputCacheWrite: 0.00000375, // $3.75/M
		Request:         0.001,
	}

	// 1000 input tokens (400 cached read), 200 output tokens, 100 cache write tokens
	// Uncached input = 1000 - 400 = 600 tokens
	// Prompt cost = 600 * 0.000003 = 0.0018
	// Cache read cost = 400 * 0.0000003 = 0.00012
	// Cache write cost = 100 * 0.00000375 = 0.000375
	// Completion cost = 200 * 0.000015 = 0.003
	// Request cost = 0.001
	// Total = 0.0018 + 0.00012 + 0.000375 + 0.003 + 0.001 = 0.006295

	cost := CalculateCost(p, 1000, 200, 400, 100)
	expected := 0.006295

	if math.Abs(cost-expected) > 1e-7 {
		t.Errorf("expected cost %f, got %f", expected, cost)
	}
}
