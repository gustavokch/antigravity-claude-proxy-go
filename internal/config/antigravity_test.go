package config

import (
	"encoding/json"
	"testing"
)

// TestAntigravitySensitiveWordsConfig pins the opt-out: an absent or null
// list keeps the built-in mask, [] disables it and survives a save, and a
// list replaces the defaults.
func TestAntigravitySensitiveWordsConfig(t *testing.T) {
	const probe = "RFC 2119 and Hermes Agent"
	cases := []struct {
		name, json string
		masked     bool
		custom     bool
	}{
		{"absent", `{}`, true, false},
		{"null", `{"antigravity":{"sensitiveWords":null}}`, true, false},
		{"disabled", `{"antigravity":{"sensitiveWords":[]}}`, false, false},
		{"custom", `{"antigravity":{"sensitiveWords":["Hermes Agent"]}}`, true, true},
	}
	for _, tc := range cases {
		cfg := DefaultConfig()
		if err := json.Unmarshal([]byte(tc.json), &cfg); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		saved, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		reloaded := DefaultConfig()
		if err := json.Unmarshal(saved, &reloaded); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, c := range []Config{cfg, reloaded} {
			got := c.Antigravity.SystemMask().Apply(probe)
			if (got != probe) != tc.masked {
				t.Errorf("%s: masked=%v, got %q", tc.name, got != probe, got)
			}
			if tc.custom && got != "RFC 2119 and H\u034Fermes A\u034Fgent" {
				t.Errorf("%s: custom list did not replace defaults: %q", tc.name, got)
			}
		}
	}
}
