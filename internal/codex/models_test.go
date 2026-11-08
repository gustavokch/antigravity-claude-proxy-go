package codex

import "testing"

func TestResolveModel(t *testing.T) {
	allowlist := []ModelConfig{
		{ID: "gpt-5.5", Alias: "codex, Fast", Enabled: true},
		{ID: "GPT-6-Sol", Aliases: []string{"sol"}, Enabled: true},
		{ID: "gpt-6-luna", Alias: "luna", Enabled: false},
		{ID: "sol", Enabled: true},
	}
	for _, tc := range []struct {
		requested string
		want      string
		ok        bool
	}{
		{"gpt-5.5", "gpt-5.5", true},
		{" gpt-5.5 ", "gpt-5.5", true},
		{"codex", "gpt-5.5", true},
		{"Fast", "gpt-5.5", true},
		{"fast", "gpt-5.5", true},
		{"gpt-6-sol", "GPT-6-Sol", true},
		{"sol", "sol", true}, // an exact id beats an alias of another entry
		{"SOL", "sol", true}, // an id in another case beats an alias in another case
		{"gpt-6-luna", "", false},
		{"luna", "", false},
		{"gpt-5", "", false},
		{"", "", false},
	} {
		got, ok := ResolveModel(allowlist, tc.requested)
		if ok != tc.ok || got.ID != tc.want {
			t.Errorf("ResolveModel(%q) = %q, %v; want %q, %v", tc.requested, got.ID, ok, tc.want, tc.ok)
		}
	}
}

func TestIdentityConfigValidate(t *testing.T) {
	if err := (IdentityConfig{UserAgent: "codex-tui/1.0 (Mac OS)", Originator: "codex-tui"}).Validate(); err != nil {
		t.Errorf("valid identity: %v", err)
	}
	if err := (IdentityConfig{Originator: "codex\r\nX-Injected: 1"}).Validate(); err == nil {
		t.Error("a CR/LF in the originator must be refused")
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                       DefaultBaseURL,
		"  ":                                     DefaultBaseURL,
		"https://chatgpt.com/backend-api/codex/": "https://chatgpt.com/backend-api/codex",
		"https://chatgpt.com/backend-api/codex/responses": "https://chatgpt.com/backend-api/codex",
		"http://127.0.0.1:9000":                           "http://127.0.0.1:9000",
	} {
		if got := NormalizeBaseURL(raw); got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
