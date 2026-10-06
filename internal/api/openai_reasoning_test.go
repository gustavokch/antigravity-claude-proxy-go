package api

import (
	"reflect"
	"testing"
)

func TestTranslateOpenAIRequest_ReasoningEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		extra        map[string]any
		wantEffort   string // output_config.effort, "" for none
		wantThinking map[string]any
	}{
		{"low", map[string]any{"reasoning_effort": "low"}, "low", nil},
		{"medium", map[string]any{"reasoning_effort": "medium"}, "medium", nil},
		{"high", map[string]any{"reasoning_effort": "high"}, "high", nil},
		{"xhigh is a real Anthropic level", map[string]any{"reasoning_effort": "xhigh"}, "xhigh", nil},
		{"max", map[string]any{"reasoning_effort": "max"}, "max", nil},
		{"minimal rides the low level", map[string]any{"reasoning_effort": "minimal"}, "low", nil},
		{"Responses object form", map[string]any{"reasoning": map[string]any{"effort": "high"}}, "high", nil},
		{"none turns thinking off", map[string]any{"reasoning_effort": "none"}, "", map[string]any{"type": "disabled"}},
		{"unknown spelling is dropped", map[string]any{"reasoning_effort": "turbo"}, "", nil},
		{"absent", map[string]any{}, "", nil},
	}
	for _, tc := range tests {
		request := map[string]any{
			"model":    "gemini-3.8-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		for k, v := range tc.extra {
			request[k] = v
		}
		got, err := translateOpenAIRequest(request)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, leaked := got.Anthropic["reasoning_effort"]; leaked {
			t.Errorf("%s: the proxy-only reasoning_effort field must not be forwarded: %#v", tc.name, got.Anthropic)
		}
		if _, leaked := got.Anthropic["reasoning"]; leaked {
			t.Errorf("%s: the OpenAI reasoning object must not be forwarded: %#v", tc.name, got.Anthropic)
		}
		var effort string
		if config, ok := got.Anthropic["output_config"].(map[string]any); ok {
			effort, _ = config["effort"].(string)
		}
		if effort != tc.wantEffort {
			t.Errorf("%s: output_config.effort = %q, want %q", tc.name, effort, tc.wantEffort)
		}
		thinking, _ := got.Anthropic["thinking"].(map[string]any)
		if !reflect.DeepEqual(thinking, tc.wantThinking) {
			t.Errorf("%s: thinking = %#v, want %#v", tc.name, thinking, tc.wantThinking)
		}
	}
}
