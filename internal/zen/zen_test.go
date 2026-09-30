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
	// Membership of AnthropicWireIDs is covered by
	// TestAnthropicWireIDsCoverLiveClaudeModels / ...HaveNoStaleEntries, which
	// diff the list against a catalog snapshot. What is pinned here is the
	// normalization: case, the opencode/ prefix, and mixed-case spellings all
	// fold to the same wire. claude-sonnet-5-5 is the one membership pin kept
	// by name: its omission was the original bug, and a snapshot edit must not
	// be able to hide it.
	for _, id := range []string{
		"claude-sonnet-5-5",
		"opencode/claude-sonnet-4-6",
		"OPencode/Claude-Sonnet-4-6",
		"Claude-Opus-4-5",
	} {
		if !IsAnthropicWire(id) {
			t.Errorf("IsAnthropicWire(%q) = false, want true", id)
		}
	}

	// Near-miss guards: superset/subset names and non-Claude prefixes must not
	// claim the Anthropic wire. These are not catalog assertions.
	for _, id := range []string{
		"", "gpt-5.5", "opencode/gpt-5.5", "gemini-2.5-pro", "grok-4",
		"claude-opus-4-5-1", "claude-sonnet-4-6-free",
		"deepseek-v4", "glm-4.6", "minimax-m2", "kimi-k2-thinking",
		"muse-spark", "jev-test", "big-pickle",
		"qwen3.7-max", "qwen3.7-plus",
	} {
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
		{"gpt-5", "gpt-5", WireResponses}, // Responses wire
		{"gemini-3.1-pro", "", WireNone},  // Gemini-native
		{"jev-1.13", "", WireNone},        // systemone
		{"muse-spark-1.3", "muse-spark-1.3", WireResponses},
	}
	for _, c := range cases {
		got, w := WireFor(c.in)
		if got != c.canonical || w != c.wire {
			t.Errorf("WireFor(%q) = %q,%v; want %q,%v", c.in, got, w, c.canonical, c.wire)
		}
	}
}
