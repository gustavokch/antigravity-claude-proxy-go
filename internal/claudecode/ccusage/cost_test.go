package ccusage

import (
	"math"
	"testing"
)

func approx(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}

func ptr(v float64) *float64 { return &v }

// testPrice mirrors the "test-model" entry of ccusage's cost tests.
var testPrice = ModelPrice{
	Input:                1.0,
	Output:               10.0,
	CacheCreate:          1.25,
	CacheRead:            0.1,
	InputAbove200k:       ptr(2.0),
	CacheCreateAbove200k: ptr(1.5),
	FastMultiplier:       1,
}

func testPricer(price ModelPrice) Pricer {
	return PricerFunc(func(model string) (ModelPrice, bool) {
		return price, model == "test-model"
	})
}

func TestPriceTokens_CacheBreakdown(t *testing.T) {
	// From ccusage: 10 5m writes, 20 1h writes and 30 reads.
	got := PriceTokens(TokenUsage{CacheCreate5m: 10, CacheCreate1h: 20, CacheRead: 30}, testPrice)
	if !approx(got, 55.5) {
		t.Errorf("cost = %v, want 55.5", got)
	}
	// Without a breakdown every write is a 5-minute write.
	got = PriceTokens(TokenUsage{CacheCreate5m: 10}, testPrice)
	if !approx(got, 12.5) {
		t.Errorf("cost = %v, want 12.5", got)
	}
}

func TestPriceTokens_TierEdges(t *testing.T) {
	tests := []struct {
		name string
		u    TokenUsage
		want float64
	}{
		{"input at 200k", TokenUsage{Input: 200_000}, 200_000},
		{"input at 200k+1", TokenUsage{Input: 200_001}, 200_000 + 2},
		// Output has no above-200k rate, so it stays flat.
		{"output at 200k+1", TokenUsage{Output: 200_001}, 200_001 * 10},
		{"5m writes at 200k", TokenUsage{CacheCreate5m: 200_000}, 200_000 * 1.25},
		{"5m writes at 200k+1", TokenUsage{CacheCreate5m: 200_001}, 200_000*1.25 + 1.5},
		// 1h writes tier on input*2 and input_above*2.
		{"1h writes at 200k", TokenUsage{CacheCreate1h: 200_000}, 200_000 * 2},
		{"1h writes at 200k+1", TokenUsage{CacheCreate1h: 200_001}, 200_000*2 + 4},
		{"reads at 200k+1", TokenUsage{CacheRead: 200_001}, 200_001 * 0.1},
		// Buckets tier independently: two buckets of 150k stay below 200k.
		{"buckets are separate", TokenUsage{Input: 150_000, CacheCreate5m: 150_000}, 150_000 + 150_000*1.25},
		{"negative counts are ignored", TokenUsage{Input: -5}, 0},
	}
	for _, tt := range tests {
		if got := PriceTokens(tt.u, testPrice); !approx(got, tt.want) {
			t.Errorf("%s: cost = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestTokenCost_FastMultiplier(t *testing.T) {
	price := testPrice
	price.FastMultiplier = 6
	p := testPricer(price)
	u := TokenUsage{Input: 100, Output: 10}

	if got := TokenCost("test-model", u, p); !approx(got, 200) {
		t.Errorf("standard cost = %v, want 200", got)
	}
	u.Fast = true
	if got := TokenCost("test-model", u, p); !approx(got, 1200) {
		t.Errorf("fast cost = %v, want 1200", got)
	}
	// An injected price that leaves FastMultiplier unset is not zeroed.
	price.FastMultiplier = 0
	if got := TokenCost("test-model", u, testPricer(price)); !approx(got, 200) {
		t.Errorf("fast cost with unset multiplier = %v, want 200", got)
	}

	if got := TokenCost("other-model", u, p); got != 0 {
		t.Errorf("unknown model cost = %v, want 0", got)
	}
	if got := TokenCost("", u, p); got != 0 {
		t.Errorf("empty model cost = %v, want 0", got)
	}
	if got := TokenCost("test-model", u, nil); got != 0 {
		t.Errorf("nil pricer cost = %v, want 0", got)
	}
}

func TestEntryCost_Modes(t *testing.T) {
	p := testPricer(testPrice)
	logged := Entry{Model: "test-model", Input: 100, CostUSD: ptr(42)}
	unlogged := Entry{Model: "test-model", Input: 100}

	tests := []struct {
		name string
		e    Entry
		mode CostMode
		want float64
	}{
		{"auto uses the logged cost", logged, CostModeAuto, 42},
		{"auto calculates without one", unlogged, CostModeAuto, 100},
		{"calculate ignores the logged cost", logged, CostModeCalculate, 100},
		{"calculate", unlogged, CostModeCalculate, 100},
		{"display uses the logged cost", logged, CostModeDisplay, 42},
		{"display counts 0 without one", unlogged, CostModeDisplay, 0},
		{"a zero logged cost is kept", Entry{Model: "test-model", Input: 100, CostUSD: ptr(0)}, CostModeAuto, 0},
	}
	for _, tt := range tests {
		if got := EntryCost(tt.e, tt.mode, p); !approx(got, tt.want) {
			t.Errorf("%s: cost = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestEntryCost_FromParsedEntry(t *testing.T) {
	line := `{"timestamp":"2026-09-01T10:00:00Z","sessionId":"s1","requestId":"req_1",` +
		`"message":{"id":"msg_1","model":"test-model","usage":{"input_tokens":100,"output_tokens":10,` +
		`"cache_creation_input_tokens":300,"cache_read_input_tokens":1000,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}}}`
	entries := ParseLine([]byte(line))
	if len(entries) != 1 {
		t.Fatalf("ParseLine returned %d entries", len(entries))
	}
	e := entries[0]
	if e.CacheCreate1h != 200 || e.CacheCreate5m != 100 {
		t.Fatalf("cache split = %d/%d, want 100/200", e.CacheCreate5m, e.CacheCreate1h)
	}
	// 100*1 + 10*10 + 100*1.25 + 200*2 + 1000*0.1
	want := 100.0 + 100 + 125 + 400 + 100
	if got := EntryCost(e, CostModeAuto, testPricer(testPrice)); !approx(got, want) {
		t.Errorf("cost = %v, want %v", got, want)
	}

	e.Speed = "fast"
	price := testPrice
	price.FastMultiplier = 2
	if got := EntryCost(e, CostModeCalculate, testPricer(price)); !approx(got, 2*want) {
		t.Errorf("fast cost = %v, want %v", got, 2*want)
	}
}

func TestParseCostMode(t *testing.T) {
	for in, want := range map[string]CostMode{"": CostModeAuto, "auto": CostModeAuto, "calculate": CostModeCalculate, "display": CostModeDisplay} {
		if got, err := ParseCostMode(in); err != nil || got != want {
			t.Errorf("ParseCostMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseCostMode("Auto"); err == nil {
		t.Error("ParseCostMode accepted an unknown mode")
	}
}
