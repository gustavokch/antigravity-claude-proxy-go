package openrouter

import (
	"sync"
	"testing"
	"time"
)

func toolCapableEndpoints() []ProviderEndpoint {
	return []ProviderEndpoint{
		// Highest score, but cannot serve tools at all.
		{ProviderName: "gmicloud", ContextLength: 1000000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens", "temperature", "top_p"}},
		// Tools, but no forced tool choice.
		{ProviderName: "novita", ContextLength: 900000, UptimeLast5m: 0.98, UptimeLast30m: 0.98, UptimeLast1d: 0.98,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{Auto: true}},
		// Full tool support.
		{ProviderName: "parasail", ContextLength: 800000, UptimeLast5m: 0.97, UptimeLast30m: 0.97, UptimeLast1d: 0.97,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{None: true, Auto: true, Required: true, Function: true}},
	}
}

func TestToolRequirementsFromAnthropic(t *testing.T) {
	cases := []struct {
		name string
		req  map[string]any
		want ToolRequirements
	}{
		{"no tools", map[string]any{"model": "m"}, ToolRequirements{}},
		{"tools only", map[string]any{"tools": []any{map[string]any{"name": "x"}}}, ToolRequirements{Tools: true}},
		{"empty tools list ignored", map[string]any{"tools": []any{}}, ToolRequirements{}},
		{"choice any", map[string]any{"tools": []any{map[string]any{"name": "x"}},
			"tool_choice": map[string]any{"type": "any"}}, ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}},
		{"choice tool", map[string]any{"tools": []any{map[string]any{"name": "x"}},
			"tool_choice": map[string]any{"type": "tool", "name": "x"}}, ToolRequirements{Tools: true, ToolChoice: ToolChoiceFunction}},
		{"choice auto", map[string]any{"tools": []any{map[string]any{"name": "x"}},
			"tool_choice": map[string]any{"type": "auto"}}, ToolRequirements{Tools: true, ToolChoice: ToolChoiceAuto}},
		{"choice none", map[string]any{"tools": []any{map[string]any{"name": "x"}},
			"tool_choice": map[string]any{"type": "none"}}, ToolRequirements{Tools: true, ToolChoice: ToolChoiceNone}},
		{"string choice (openai shape)", map[string]any{"tools": []any{map[string]any{"name": "x"}},
			"tool_choice": "required"}, ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolRequirementsFromAnthropic(tc.req); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProviderEndpoint_SupportsRequirements(t *testing.T) {
	eps := toolCapableEndpoints()
	gmi, novita, parasail := &eps[0], &eps[1], &eps[2]

	if !gmi.SupportsRequirements(ToolRequirements{}) {
		t.Error("no requirements must always pass")
	}
	if gmi.SupportsRequirements(ToolRequirements{Tools: true}) {
		t.Error("gmicloud has no tools parameter, must not pass")
	}
	if !novita.SupportsRequirements(ToolRequirements{Tools: true, ToolChoice: ToolChoiceAuto}) {
		t.Error("novita supports auto tool choice")
	}
	if novita.SupportsRequirements(ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}) {
		t.Error("novita does not support required tool choice")
	}
	if !parasail.SupportsRequirements(ToolRequirements{Tools: true, ToolChoice: ToolChoiceFunction}) {
		t.Error("parasail supports named function tool choice")
	}

	// Unknown capability metadata fails open: an endpoint with tools listed and
	// no supports_tool_choice object is assumed able to serve tool_choice.
	unknown := ProviderEndpoint{ProviderName: "x", SupportedParameters: []string{"tools", "tool_choice"}}
	if !unknown.SupportsRequirements(ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}) {
		t.Error("missing supports_tool_choice must fail open")
	}
}

func TestProviderRouter_FilterCapableDropsPinnedIncapableProvider(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", toolCapableEndpoints())

	chain := r.SelectChain("s1", "m1", ProviderOrder{Mode: "pinned", Pin: "gmicloud"})
	if len(chain) != 1 || chain[0] != "gmicloud" {
		t.Fatalf("pinned chain = %v", chain)
	}

	got := r.FilterCapable("m1", chain, ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, ProviderOrder{Mode: "pinned", Pin: "gmicloud"})
	if len(got) == 0 {
		t.Fatal("filter must fall back to capable providers, got empty chain")
	}
	if got[0] != "parasail" {
		t.Errorf("want parasail first, got %v", got)
	}
	for _, p := range got {
		if p == "gmicloud" || p == "novita" {
			t.Errorf("incapable provider %s in chain %v", p, got)
		}
	}
}

func TestProviderRouter_FilterCapableKeepsCapableCandidates(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", toolCapableEndpoints())

	chain := r.SelectChain("s1", "m1", ProviderOrder{Mode: "auto"})
	if chain[0] != "gmicloud" {
		t.Fatalf("expected gmicloud ranked first, got %v", chain)
	}

	got := r.FilterCapable("m1", chain, ToolRequirements{Tools: true}, ProviderOrder{Mode: "auto"})
	want := []string{"novita", "parasail"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestProviderRouter_FilterCapablePassthroughWithoutRequirements(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", toolCapableEndpoints())
	chain := []string{"gmicloud", "novita"}

	got := r.FilterCapable("m1", chain, ToolRequirements{}, ProviderOrder{Mode: "auto"})
	if len(got) != 2 || got[0] != "gmicloud" {
		t.Errorf("no requirements must pass the chain through unchanged, got %v", got)
	}
}

func TestProviderRouter_FilterCapableUnknownModelFailsOpen(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	chain := []string{"gmicloud"}

	got := r.FilterCapable("unranked", chain, ToolRequirements{Tools: true}, ProviderOrder{Mode: "auto"})
	if len(got) != 1 || got[0] != "gmicloud" {
		t.Errorf("unranked model has no capability data; want passthrough, got %v", got)
	}
}

func TestProviderRouter_FilterCapableMergesProviderVariants(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", []ProviderEndpoint{
		// Same provider, two variants: the higher-scored one serves tools, the
		// lower-scored one does not. provider.order names only the provider and
		// OpenRouter picks a capable variant within it, so the provider stays.
		{ProviderName: "deepinfra", Tag: "fp8", ContextLength: 900000,
			UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{Auto: true, Required: true}},
		{ProviderName: "deepinfra", Tag: "bf16", ContextLength: 100000,
			UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90,
			SupportedParameters: []string{"max_tokens"}},
	})

	got := r.FilterCapable("m1", []string{"deepinfra"},
		ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, ProviderOrder{Mode: "auto"})
	if len(got) != 1 || got[0] != "deepinfra" {
		t.Errorf("a provider with one tool-capable variant must survive, got %v", got)
	}
}

func TestProviderRouter_FilterCapableCustomModeKeepsAllowlist(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", toolCapableEndpoints())

	order := ProviderOrder{Mode: "custom", Order: []string{"gmicloud", "novita"}}
	chain := r.SelectChain("s1", "m1", order)

	got := r.FilterCapable("m1", chain, ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, order)
	for _, p := range got {
		if p == "parasail" {
			t.Fatalf("custom mode must not route outside its allowlist, got %v", got)
		}
	}
}

func TestProviderRouter_FilterCapableCustomFallbackKeepsOperatorOrder(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", []ProviderEndpoint{
		// Rank order: novita outranks parasail.
		{ProviderName: "gmicloud", ContextLength: 1000000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens"}},
		{ProviderName: "novita", ContextLength: 900000, UptimeLast5m: 0.98, UptimeLast30m: 0.98, UptimeLast1d: 0.98,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{Auto: true, Required: true}},
		{ProviderName: "parasail", ContextLength: 800000, UptimeLast5m: 0.97, UptimeLast30m: 0.97, UptimeLast1d: 0.97,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{None: true, Auto: true, Required: true, Function: true}},
	})

	// Operator allowlist puts parasail first, opposite of rank order. The only
	// candidate is incapable, so the fallback substitutes — it must hand back
	// the allowlist sequence, not the rank sequence.
	order := ProviderOrder{Mode: "custom", Order: []string{"parasail", "novita"}}
	got := r.FilterCapable("m1", []string{"gmicloud"},
		ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, order)
	want := []string{"parasail", "novita"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback must follow operator order, got %v, want %v", got, want)
		}
	}
}

func TestProviderRouter_FilterCapablePinnedModeSubstitutesFreely(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", toolCapableEndpoints())

	order := ProviderOrder{Mode: "pinned", Pin: "gmicloud"}
	chain := r.SelectChain("s1", "m1", order)

	got := r.FilterCapable("m1", chain, ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, order)
	if len(got) == 0 || got[0] != "parasail" {
		t.Errorf("pinned mode must substitute a capable provider, got %v", got)
	}
}

// TestProviderRouter_FilterCapableConcurrent guards the read-lock downgrade:
// FilterCapable must stay safe against concurrent rank refreshes and result
// recording. Run under -race.
func TestProviderRouter_FilterCapableConcurrent(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", toolCapableEndpoints())

	need := ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}
	order := ProviderOrder{Mode: "auto"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); r.FilterCapable("m1", []string{"gmicloud", "parasail"}, need, order) }()
		go func() { defer wg.Done(); r.RefreshRanks("m1", toolCapableEndpoints()) }()
		go func() { defer wg.Done(); r.RecordResult("m1", "parasail", true, time.Millisecond, 10) }()
	}
	wg.Wait()
}

func TestProviderRouter_RankedAtTracksRefresh(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	if !r.RankedAt("m1").IsZero() {
		t.Fatal("unranked model must report a zero rank time")
	}
	before := time.Now()
	r.RefreshRanks("m1", toolCapableEndpoints())
	if at := r.RankedAt("m1"); at.Before(before) {
		t.Fatalf("RefreshRanks must record the rank time, got %v", at)
	}
}

func TestEndpointsClient_GetCachedEndpointsWithTime(t *testing.T) {
	c := NewEndpointsClient(time.Second, time.Hour)
	if _, _, ok := c.GetCachedEndpointsWithTime("author/model", "https://openrouter.ai/api"); ok {
		t.Fatal("empty cache must report no entry")
	}
	before := time.Now()
	c.SaveEndpoints("author/model", "https://openrouter.ai/api", toolCapableEndpoints())
	eps, at, ok := c.GetCachedEndpointsWithTime("author/model", "https://openrouter.ai/api")
	if !ok || len(eps) != len(toolCapableEndpoints()) || at.Before(before) {
		t.Fatalf("entry must return endpoints and fill time together, ok=%v at=%v", ok, at)
	}
	c.SaveEndpoints("author/model", "https://openrouter.ai/api", nil)
	if _, _, ok := c.GetCachedEndpointsWithTime("author/model", "https://openrouter.ai/api"); ok {
		t.Fatal("empty endpoint list must report no entry")
	}
}

func TestProviderEndpoint_NoAdvertisedParametersFailsOpen(t *testing.T) {
	// An endpoint that advertises no parameters tells us nothing about its
	// capabilities. Excluding it would strand requests on an incomplete catalog,
	// so it must pass every requirement.
	ep := ProviderEndpoint{ProviderName: "silent"}
	if !ep.SupportsRequirements(ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}) {
		t.Error("an endpoint advertising no parameters must fail open")
	}
	var nilEndpoint *ProviderEndpoint
	if nilEndpoint.SupportsRequirements(ToolRequirements{Tools: true}) {
		t.Error("a nil endpoint must not satisfy a tool requirement")
	}
	if !nilEndpoint.SupportsRequirements(ToolRequirements{}) {
		t.Error("no requirements must pass even for a nil endpoint")
	}
}

func TestProviderRouter_SelectChainKeepsProviderWithHealthyVariant(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", []ProviderEndpoint{
		{ProviderName: "deepinfra", Tag: "fp8", ContextLength: 900000,
			UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{Auto: true, Required: true}},
		{ProviderName: "deepinfra", Tag: "broken", ContextLength: 100000,
			Status: 500, UptimeLast5m: 0, UptimeLast30m: 0, UptimeLast1d: 0},
	})

	chain := r.SelectChain("s1", "m1", ProviderOrder{Mode: "auto"})
	if len(chain) == 0 || chain[0] != "deepinfra" {
		t.Fatalf("provider with healthy primary variant must be selected in chain, got %v", chain)
	}
}
