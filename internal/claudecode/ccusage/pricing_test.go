package ccusage

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func mustParsePricing(t *testing.T, doc string) *LiteLLMPricer {
	t.Helper()
	p, err := ParseLiteLLMPricing([]byte(doc))
	if err != nil {
		t.Fatalf("ParseLiteLLMPricing: %v", err)
	}
	return p
}

func TestPricingKeyMatches(t *testing.T) {
	tests := []struct {
		key, model string
		want       bool
	}{
		{"claude-opus-5", "claude-opus-5", true},
		{"claude-opus-5", "claude-opus-5-5", false},
		{"claude-opus-5-5", "claude-opus-5", false},
		{"claude-opus-5", "claude-opus-5-20260101", true},
		{"claude-opus-5", "claude-opus-5.20260101", true},
		{"claude-opus-5", "claude-opus-5-202601011", false},
		{"claude-opus-5", "claude-opus-5-20260101x", false},
		{"claude-opus-4", "claude-opus-4-20250514", true},
		{"claude-opus-4", "claude-opus-4-9", false},
		{"claude-opus-4", "claude-opus-4.70", false},
		{"claude-opus-4", "claude-opus-5", false},
		{"claude-opus-4-7", "claude-opus-4.70", false},
		{"claude-opus-4-7", "claude-opus-4.7", true},
		{"claude-opus-4-7", "claude-opus-4.7-20260416", true},
		{"anthropic.claude-haiku-4-5@20251001", "claude-haiku-4-5-20251001", true},
		{"claude-sonnet-5", "anthropic.claude-sonnet-5-v1:0", true},
		{"claude-sonnet-5", "xclaude-sonnet-5", false},
		{"claude-sonnet-5", "claude-sonnet-5x", false},
		{"claude-sonnet-4", "claude-sonnet-4-thinking", true},
	}
	for _, tt := range tests {
		got := pricingKeyMatches(tt.key, tt.model, normalizedPricingKey(tt.model))
		if got != tt.want {
			t.Errorf("pricingKeyMatches(%q, %q) = %v, want %v", tt.key, tt.model, got, tt.want)
		}
	}
}

const twoOpusDoc = `{
	"claude-opus-4": {"input_cost_per_token": 15e-6, "output_cost_per_token": 75e-6},
	"claude-opus-5": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6}
}`

func TestLiteLLMPricer_VersionBoundaries(t *testing.T) {
	p := mustParsePricing(t, twoOpusDoc)
	tests := []struct {
		model     string
		wantInput float64 // 0 means a miss
	}{
		{"claude-opus-5", 5e-6},
		{"claude-opus-5-20260101", 5e-6},
		{"claude-opus-5-5", 0},
		{"claude-opus-5.5", 0},
		{"claude-opus-4-20250514", 15e-6},
		{"claude-opus-4.8-20260528", 0},
		{"claude-opus-4-9", 0},
		{"claude-opus-4.70", 0},
		{"", 0},
	}
	for _, tt := range tests {
		got, ok := p.Find(tt.model)
		if ok != (tt.wantInput != 0) || got.Input != tt.wantInput {
			t.Errorf("Find(%q) = %v, %v; want input %v", tt.model, got.Input, ok, tt.wantInput)
		}
	}
}

func TestLiteLLMPricer_PrefersLongestKey(t *testing.T) {
	p := mustParsePricing(t, `{
		"claude-sonnet-4": {"input_cost_per_token": 1, "output_cost_per_token": 0},
		"claude-sonnet-4-20250514": {"input_cost_per_token": 2, "output_cost_per_token": 0}
	}`)
	got, ok := p.Find("claude-sonnet-4-20250514-via-bedrock")
	if !ok || got.Input != 2 {
		t.Errorf("Find = %v, %v; want the longer key's input 2", got.Input, ok)
	}
}

func TestLiteLLMPricer_MissingFieldDefaults(t *testing.T) {
	p := mustParsePricing(t, `{
		"claude-test-1": {"input_cost_per_token": 2e-6, "output_cost_per_token": 10e-6},
		"claude-test-2": {
			"input_cost_per_token": 3e-6,
			"output_cost_per_token": 15e-6,
			"cache_creation_input_token_cost": 4e-6,
			"cache_read_input_token_cost": 5e-7,
			"input_cost_per_token_above_200k_tokens": 6e-6,
			"max_input_tokens": 1000000,
			"provider_specific_entry": {"fast": 3.5}
		},
		"claude-no-output": {"input_cost_per_token": 1e-6},
		"claude-bad": {"input_cost_per_token": "cheap", "output_cost_per_token": 1e-6},
		"gpt-other": {"input_cost_per_token": 1e-6, "output_cost_per_token": 1e-6}
	}`)
	if p.Len() != 2 {
		t.Errorf("Len = %d, want 2", p.Len())
	}

	got, ok := p.Find("claude-test-1")
	if !ok {
		t.Fatal("claude-test-1 not found")
	}
	if !approx(got.CacheCreate, 2.5e-6) || !approx(got.CacheRead, 2e-7) {
		t.Errorf("cache rates = %v, %v; want input*1.25 and input*0.1", got.CacheCreate, got.CacheRead)
	}
	if got.FastMultiplier != 1 || got.MaxInputTokens != 0 || got.InputAbove200k != nil || got.OutputAbove200k != nil ||
		got.CacheCreateAbove200k != nil || got.CacheReadAbove200k != nil {
		t.Errorf("defaults = %+v", got)
	}

	got, _ = p.Find("claude-test-2")
	if got.CacheCreate != 4e-6 || got.CacheRead != 5e-7 || got.MaxInputTokens != 1000000 || got.FastMultiplier != 3.5 {
		t.Errorf("explicit fields = %+v", got)
	}
	if got.InputAbove200k == nil || *got.InputAbove200k != 6e-6 || got.OutputAbove200k != nil {
		t.Errorf("above-200k rates = %v, %v", got.InputAbove200k, got.OutputAbove200k)
	}

	for _, model := range []string{"claude-no-output", "claude-bad", "gpt-other"} {
		if _, ok := p.Find(model); ok {
			t.Errorf("Find(%q) hit; want the entry skipped", model)
		}
	}

	if _, err := ParseLiteLLMPricing([]byte(`{"gpt-other": {"input_cost_per_token": 1, "output_cost_per_token": 1}}`)); err == nil {
		t.Error("ParseLiteLLMPricing with no Claude entries succeeded")
	}
	if _, err := ParseLiteLLMPricing([]byte(`[`)); err == nil {
		t.Error("ParseLiteLLMPricing of malformed JSON succeeded")
	}
}

func TestLiteLLMPricer_FastMultiplierOverrides(t *testing.T) {
	p := mustParsePricing(t, `{
		"anthropic/claude-opus-4.7": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6},
		"claude-opus-4.7-20260416": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6},
		"claude-opus-4.8-20260528": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6},
		"claude-opus-4-70": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6},
		"claude-opus-4-6": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6,
			"provider_specific_entry": {"fast": 4}}
	}`)
	tests := []struct {
		model string
		want  float64
	}{
		{"anthropic/claude-opus-4.7", 6},
		{"claude-opus-4.7-20260416", 6},
		{"claude-opus-4.8-20260528", 2},
		{"claude-opus-4-70", 1},
		{"claude-opus-4-6", 4}, // a published multiplier beats the override
	}
	for _, tt := range tests {
		got, ok := p.Find(tt.model)
		if !ok || got.FastMultiplier != tt.want {
			t.Errorf("Find(%q).FastMultiplier = %v, %v; want %v", tt.model, got.FastMultiplier, ok, tt.want)
		}
	}
}

func TestLiteLLMPricer_Embedded(t *testing.T) {
	p := NewLiteLLMPricer()
	if p.Len() == 0 {
		t.Fatal("embedded snapshot is empty")
	}
	for key := range p.entries {
		if !isClaudeKey(key) {
			t.Errorf("embedded snapshot holds non-Claude key %q", key)
		}
	}

	opus5, ok := p.Find("claude-opus-5")
	if !ok {
		t.Fatal("claude-opus-5 not in the embedded snapshot")
	}
	if dated, ok := p.Find("claude-opus-5-20260101"); !ok || dated != opus5 {
		t.Errorf("date-suffixed lookup = %+v, %v; want claude-opus-5's price", dated, ok)
	}
	if next, ok := p.Find("claude-opus-5-5"); ok && next == opus5 {
		t.Error("claude-opus-5-5 resolved to claude-opus-5's entry")
	}

	for _, tt := range []struct {
		model string
		want  float64
	}{
		{"anthropic.claude-opus-4-6-v1", 6},
		{"anthropic.claude-opus-4-7", 6},
		{"anthropic.claude-opus-4-8", 2},
	} {
		got, ok := p.Find(tt.model)
		if !ok || got.FastMultiplier != tt.want {
			t.Errorf("Find(%q).FastMultiplier = %v, %v; want %v", tt.model, got.FastMultiplier, ok, tt.want)
		}
	}
}

type countingPricer struct {
	calls atomic.Int64
	price map[string]ModelPrice
}

func (c *countingPricer) Find(model string) (ModelPrice, bool) {
	c.calls.Add(1)
	p, ok := c.price[model]
	return p, ok
}

func TestChainPricer(t *testing.T) {
	first := &countingPricer{price: map[string]ModelPrice{"claude-a": {Input: 1}}}
	second := &countingPricer{price: map[string]ModelPrice{"claude-a": {Input: 2}, "claude-b": {Input: 3}}}
	chain := NewChainPricer(first, nil, second)

	if got, ok := chain.Find("claude-a"); !ok || got.Input != 1 {
		t.Errorf("Find(claude-a) = %v, %v; want the first pricer's 1", got.Input, ok)
	}
	if second.calls.Load() != 0 {
		t.Errorf("second pricer asked %d times after a first-pricer hit", second.calls.Load())
	}
	if got, ok := chain.Find("claude-b"); !ok || got.Input != 3 {
		t.Errorf("Find(claude-b) = %v, %v; want the second pricer's 3", got.Input, ok)
	}

	for range 3 {
		if _, ok := chain.Find("claude-missing"); ok {
			t.Error("Find(claude-missing) hit")
		}
	}
	if n := first.calls.Load(); n != 3 {
		t.Errorf("first pricer asked %d times, want 3 (a, b, one miss)", n)
	}
	if n := second.calls.Load(); n != 2 {
		t.Errorf("second pricer asked %d times, want 2 (b, one miss)", n)
	}

	first.price["claude-missing"] = ModelPrice{Input: 4}
	if _, ok := chain.Find("claude-missing"); ok {
		t.Error("memoised miss not served from the cache")
	}
	chain.Reset()
	if got, ok := chain.Find("claude-missing"); !ok || got.Input != 4 {
		t.Errorf("after Reset Find = %v, %v; want 4", got.Input, ok)
	}
}

func TestChainPricer_PricerFunc(t *testing.T) {
	curated := PricerFunc(func(model string) (ModelPrice, bool) {
		if model == "claude-opus-5" {
			return ModelPrice{Input: 42}, true
		}
		return ModelPrice{}, false
	})
	chain := NewChainPricer(curated, NewLiteLLMPricer())
	if got, _ := chain.Find("claude-opus-5"); got.Input != 42 {
		t.Errorf("curated price not preferred: %v", got.Input)
	}
	if _, ok := chain.Find("claude-sonnet-5"); !ok {
		t.Error("LiteLLM fallback missed claude-sonnet-5")
	}
}

func TestPricers_ConcurrentFind(t *testing.T) {
	lite := NewLiteLLMPricer()
	chain := NewChainPricer(lite)
	models := []string{"claude-opus-5", "claude-opus-5-20260101", "claude-sonnet-5", "claude-nope"}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			m := models[i%len(models)]
			lite.Find(m)
			chain.Find(m)
		})
	}
	wg.Wait()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubClient(status int, body string, gotURL *string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if gotURL != nil {
			*gotURL = r.URL.String()
		}
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
}

func TestLiteLLMPricer_Refresh(t *testing.T) {
	p := mustParsePricing(t, twoOpusDoc)

	if err := p.Refresh(context.Background(), nil); err == nil {
		t.Error("Refresh with a nil client succeeded")
	}
	if err := p.Refresh(context.Background(), stubClient(http.StatusBadGateway, "", nil)); err == nil {
		t.Error("Refresh on a 502 succeeded")
	}
	if err := p.Refresh(context.Background(), stubClient(http.StatusOK, `{"gpt-x": {}}`, nil)); err == nil {
		t.Error("Refresh without Claude entries succeeded")
	}
	if got, _ := p.Find("claude-opus-5"); got.Input != 5e-6 {
		t.Fatalf("failed refresh changed prices: %v", got.Input)
	}
	// Prime the cache with a miss that the refresh must clear.
	if _, ok := p.Find("claude-sonnet-5"); ok {
		t.Fatal("claude-sonnet-5 unexpectedly priced before refresh")
	}

	var url string
	doc := `{
		"claude-opus-5": {"input_cost_per_token": 7e-6, "output_cost_per_token": 30e-6},
		"claude-sonnet-5": {"input_cost_per_token": 2e-6, "output_cost_per_token": 10e-6}
	}`
	if err := p.Refresh(context.Background(), stubClient(http.StatusOK, doc, &url)); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if url != LiteLLMPricingURL {
		t.Errorf("Refresh fetched %q", url)
	}
	for model, want := range map[string]float64{"claude-opus-5": 7e-6, "claude-sonnet-5": 2e-6, "claude-opus-4": 15e-6} {
		if got, ok := p.Find(model); !ok || got.Input != want {
			t.Errorf("after refresh Find(%q) = %v, %v; want %v", model, got.Input, ok, want)
		}
	}
}
