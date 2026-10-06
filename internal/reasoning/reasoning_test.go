package reasoning

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	obj := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	tests := []struct {
		name    string
		request map[string]any
		want    Params
	}{
		{"nil request", nil, Params{}},
		{"nothing reasoning-related", obj("model", "m"), Params{}},

		// The explicit proxy extension.
		{"reasoning_effort low", obj("reasoning_effort", "low"), Params{Level: LevelLow, Source: SourceExplicit}},
		{"spelling is case-insensitive", obj("reasoning_effort", "HIGH"), Params{Level: LevelHigh, Source: SourceExplicit}},
		{"reasoning is the fallback key", obj("reasoning", "medium"), Params{Level: LevelMedium, Source: SourceExplicit}},
		{"empty reasoning_effort falls through to reasoning", obj("reasoning_effort", "", "reasoning", "low"), Params{Level: LevelLow, Source: SourceExplicit}},
		{"reasoning object form", obj("reasoning", obj("effort", "high")), Params{Level: LevelHigh, Source: SourceExplicit}},
		{"xhigh stays distinct", obj("reasoning_effort", "xhigh"), Params{Level: LevelXHigh, Source: SourceExplicit}},
		{"extra-high is xhigh", obj("reasoning_effort", "extra-high"), Params{Level: LevelXHigh, Source: SourceExplicit}},
		{"very-high is xhigh", obj("reasoning_effort", "very-high"), Params{Level: LevelXHigh, Source: SourceExplicit}},
		{"max stays distinct", obj("reasoning_effort", "max"), Params{Level: LevelMax, Source: SourceExplicit}},
		{"maximum is max", obj("reasoning_effort", "maximum"), Params{Level: LevelMax, Source: SourceExplicit}},
		{"extreme is max", obj("reasoning_effort", "extreme"), Params{Level: LevelMax, Source: SourceExplicit}},
		{"minimal stays distinct", obj("reasoning_effort", "minimal"), Params{Level: LevelMinimal, Source: SourceExplicit}},
		{"unknown spelling is ignored", obj("reasoning_effort", "turbo"), Params{}},
		{"none turns thinking off", obj("reasoning_effort", "none"), Params{Disabled: true}},
		{"off turns thinking off", obj("reasoning_effort", "off"), Params{Disabled: true}},
		{"numeric zero turns thinking off", obj("reasoning_effort", float64(0)), Params{Disabled: true}},
		{"boolean false turns thinking off", obj("reasoning_effort", false), Params{Disabled: true}},

		// thinking.type
		{"thinking disabled", obj("thinking", obj("type", "disabled")), Params{Disabled: true}},
		{"thinking adaptive", obj("thinking", obj("type", "adaptive")), Params{Adaptive: true}},
		{"thinking type is case-insensitive", obj("thinking", obj("type", "DISABLED")), Params{Disabled: true}},

		// Budgets.
		{"budget_tokens derives a level", obj("thinking", obj("type", "enabled", "budget_tokens", float64(1024))),
			Params{Level: LevelLow, Source: SourceBudget, Budget: 1024, HasBudget: true}},
		{"2048 is still low", obj("thinking", obj("budget_tokens", float64(2048))),
			Params{Level: LevelLow, Source: SourceBudget, Budget: 2048, HasBudget: true}},
		{"2049 is medium", obj("thinking", obj("budget_tokens", float64(2049))),
			Params{Level: LevelMedium, Source: SourceBudget, Budget: 2049, HasBudget: true}},
		{"11999 is medium", obj("thinking", obj("budget_tokens", float64(11999))),
			Params{Level: LevelMedium, Source: SourceBudget, Budget: 11999, HasBudget: true}},
		{"12000 is high", obj("thinking", obj("budget_tokens", float64(12000))),
			Params{Level: LevelHigh, Source: SourceBudget, Budget: 12000, HasBudget: true}},
		{"top-level thinking_budget wins over nested budget_tokens",
			obj("thinking", obj("budget_tokens", float64(1024)), "thinking_budget", float64(40000)),
			Params{Level: LevelHigh, Source: SourceBudget, Budget: 40000, HasBudget: true}},
		{"zero budget switches thinking off", obj("thinking_budget", float64(0)), Params{Budget: 0, HasBudget: true, Disabled: true}},
		{"zero budget under type enabled does not", obj("thinking", obj("type", "enabled", "budget_tokens", float64(0))),
			Params{Budget: 0, HasBudget: true}},
		{"explicit effort keeps its level over a budget",
			obj("reasoning_effort", "low", "thinking_budget", float64(40000)),
			Params{Level: LevelLow, Source: SourceExplicit, Budget: 40000, HasBudget: true}},

		{"json.Number budget", obj("thinking", obj("budget_tokens", json.Number("2048"))),
			Params{Level: LevelLow, Source: SourceBudget, Budget: 2048, HasBudget: true}},
		{"float32 budget", obj("thinking", obj("budget_tokens", float32(2048))),
			Params{Level: LevelLow, Source: SourceBudget, Budget: 2048, HasBudget: true}},
		// output_config.effort is ambient and lowest precedence.
		{"output_config effort", obj("output_config", obj("effort", "max")), Params{Level: LevelMax, Source: SourceOutputConfig}},
		{"output_config effort with adaptive thinking",
			obj("thinking", obj("type", "adaptive"), "output_config", obj("effort", "xhigh")),
			Params{Level: LevelXHigh, Source: SourceOutputConfig, Adaptive: true}},
		{"explicit effort beats output_config",
			obj("reasoning_effort", "low", "output_config", obj("effort", "high")),
			Params{Level: LevelLow, Source: SourceExplicit}},
		{"a derived budget level beats output_config",
			obj("thinking", obj("type", "enabled", "budget_tokens", float64(40000)), "output_config", obj("effort", "low")),
			Params{Level: LevelHigh, Source: SourceBudget, Budget: 40000, HasBudget: true}},
		{"disabled wins over output_config", obj("thinking", obj("type", "disabled"), "output_config", obj("effort", "high")),
			Params{Level: LevelHigh, Source: SourceOutputConfig, Disabled: true}},
		{"output_config without effort", obj("output_config", obj("format", obj("type", "json_schema"))), Params{}},
		{"output_config none is not a level", obj("output_config", obj("effort", "none")), Params{}},
		{"unknown output_config spelling is ignored", obj("output_config", obj("effort", "turbo")), Params{}},
		{"output_config of the wrong type is ignored", obj("output_config", "high"), Params{}},
	}
	for _, tc := range tests {
		got := Parse(tc.request)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", tc.name, got, tc.want)
		}
	}
}

func TestSupported(t *testing.T) {
	t.Parallel()
	threeTier := []Level{LevelLow, LevelMedium, LevelHigh}
	opus46 := []Level{LevelLow, LevelMedium, LevelHigh, LevelMax}
	tests := []struct {
		name      string
		level     Level
		supported []Level
		want      Level
	}{
		{"exact match", LevelMedium, threeTier, LevelMedium},
		{"max falls back to high", LevelMax, threeTier, LevelHigh},
		{"xhigh falls back to high", LevelXHigh, threeTier, LevelHigh},
		{"xhigh runs as high on a model with max but not xhigh", LevelXHigh, opus46, LevelHigh},
		{"max is kept where supported", LevelMax, opus46, LevelMax},
		{"minimal rides the lowest supported level", LevelMinimal, threeTier, LevelLow},
		{"an unset level stays unset", LevelUnset, threeTier, LevelUnset},
		{"nothing supported yields unset", LevelHigh, nil, LevelUnset},
		{"order of the supported list does not matter", LevelXHigh, []Level{LevelMax, LevelLow, LevelHigh}, LevelHigh},
		{"an unknown supported entry is skipped", LevelHigh, []Level{"turbo", LevelMedium}, LevelMedium},
	}
	for _, tc := range tests {
		if got := tc.level.Supported(tc.supported...); got != tc.want {
			t.Errorf("%s: %q.Supported(%v) = %q, want %q", tc.name, tc.level, tc.supported, got, tc.want)
		}
	}
}

func TestNamesTier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model string
		want  bool
	}{
		{"gemini-3.8-flash-low", true},
		{"gemini-3.8-flash-HIGH", true},
		{"gemini-3.8-flash-medium[1m]", true},
		{"gemini-3.5-flash-extra-low", true},
		{"gpt-oss-120b-medium", true},
		{"gemini-3.1-pro-high", true},
		{"gemini-3.8-flash", false},
		{"gemini-pro-agent", false},
		{"claude-opus-4-6-thinking", false},
		{"gemini-3.8-flash-tiered", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := NamesTier(tc.model); got != tc.want {
			t.Errorf("NamesTier(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}
