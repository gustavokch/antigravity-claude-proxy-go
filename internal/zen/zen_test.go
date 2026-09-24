package zen

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://opencode.ai/zen/":    "https://opencode.ai/zen",
		"https://opencode.ai/zen":     "https://opencode.ai/zen",
		"https://opencode.ai/zen/v1/": "https://opencode.ai/zen",
		"https://opencode.ai/zen/v1":  "https://opencode.ai/zen",
		"  https://opencode.ai/zen  ": "https://opencode.ai/zen",
		"":                            DefaultBaseURL,
		"   ":                         DefaultBaseURL,
	}
	for in, want := range cases {
		if got := NormalizeBaseURL(in); got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsAnthropicWire(t *testing.T) {
	allowed := []string{
		"claude-fable-5-1", "claude-fable-5", "claude-opus-5",
		"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5",
		"claude-sonnet-5", "claude-sonnet-4-6", "claude-sonnet-4-5", "claude-sonnet-4",
		"claude-haiku-4-5",
		"qwen3.8-flash", "qwen3.6-plus", "qwen3.5-plus",
		"opencode/claude-sonnet-4-6",
		"OPencode/Claude-Sonnet-4-6",
		"Claude-Opus-4-5",
	}
	for _, id := range allowed {
		if !IsAnthropicWire(id) {
			t.Errorf("IsAnthropicWire(%q) = false, want true", id)
		}
	}
	denied := []string{
		"", "gpt-5.5", "opencode/gpt-5.5", "gemini-2.5-pro", "grok-4",
		"deepseek-v4", "glm-4.6", "minimax-m2", "kimi-k2-thinking",
		"muse-spark", "jev-test", "big-pickle", "claude-sonnet-4-6-free",
		"qwen3.7-max", "qwen3.7-plus",
	}
	for _, id := range denied {
		if IsAnthropicWire(id) {
			t.Errorf("IsAnthropicWire(%q) = true, want false", id)
		}
	}
}

func TestWireFor(t *testing.T) {
	cases := []struct {
		in        string
		canonical string
		wire      Wire
	}{
		{"opencode/Claude-Sonnet-4-6", "claude-sonnet-4-6", WireAnthropic},
		{"OPENCODE/GLM-5.3", "glm-5.3", WireChat},
		{"big-pickle", "big-pickle", WireChat},
		{"gpt-5", "", WireNone},          // Responses wire
		{"gemini-3.1-pro", "", WireNone}, // Gemini-native
		{"jev-1.13", "", WireNone},       // systemone
		{"muse-spark-1.3", "", WireNone}, // Responses wire
	}
	for _, c := range cases {
		got, w := WireFor(c.in)
		if got != c.canonical || w != c.wire {
			t.Errorf("WireFor(%q) = %q,%v; want %q,%v", c.in, got, w, c.canonical, c.wire)
		}
	}
}
