package kimi

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.moonshot.ai/anthropic/":    "https://api.moonshot.ai/anthropic",
		"https://api.moonshot.ai/anthropic":     "https://api.moonshot.ai/anthropic",
		"https://api.moonshot.ai/anthropic/v1/": "https://api.moonshot.ai/anthropic",
		"https://api.moonshot.ai/anthropic/v1":  "https://api.moonshot.ai/anthropic",
		"  https://api.moonshot.ai/anthropic  ": "https://api.moonshot.ai/anthropic",
		"":                                     "https://api.moonshot.ai/anthropic",
		"   ":                                  "https://api.moonshot.ai/anthropic",
		"https://api.kimi.com/coding/":         "https://api.kimi.com/coding",
		"https://api.kimi.com/coding":          "https://api.kimi.com/coding",
	}
	for in, want := range cases {
		if got := NormalizeBaseURL(in); got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
