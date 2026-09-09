package config

import (
	"encoding/json"
	"testing"
)

func TestDefaultConfig_CacheBump(t *testing.T) {
	cfg := DefaultConfig()

	cb := cfg.CacheBump
	if cb.Enabled {
		t.Error("expected cache bump disabled by default")
	}
	if !cb.AllowHeaderOverride {
		t.Error("expected AllowHeaderOverride true by default")
	}
	if cb.LeadSeconds != 60 {
		t.Errorf("expected LeadSeconds 60, got %d", cb.LeadSeconds)
	}
	if cb.MaxBumpsPerSession != 48 {
		t.Errorf("expected MaxBumpsPerSession 48, got %d", cb.MaxBumpsPerSession)
	}
	if cb.MaxIdleMinutes != 240 {
		t.Errorf("expected MaxIdleMinutes 240, got %d", cb.MaxIdleMinutes)
	}
	if cb.MaxSessions != 200 {
		t.Errorf("expected MaxSessions 200, got %d", cb.MaxSessions)
	}
	if !cb.Routes.ClaudeCode {
		t.Error("expected claudecode route enabled by default")
	}
	if cb.Routes.Kimi {
		t.Error("expected kimi route disabled by default")
	}
	if cb.Routes.CustomEndpoints {
		t.Error("expected customEndpoints route disabled by default")
	}
}

func TestCacheBumpConfig_JSONRoundTrip(t *testing.T) {
	in := DefaultConfig()
	in.CacheBump.Enabled = true
	in.CacheBump.LeadSeconds = 90
	in.CacheBump.Routes.Kimi = true

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}

	got := out.CacheBump
	if !got.Enabled || got.LeadSeconds != 90 || !got.Routes.Kimi || !got.Routes.ClaudeCode {
		t.Errorf("round-trip lost fields: %+v", got)
	}
	if got.MaxSessions != 200 {
		t.Errorf("expected MaxSessions 200 preserved, got %d", got.MaxSessions)
	}
}

func TestCacheBumpConfig_EnabledFor(t *testing.T) {
	base := DefaultConfig()
	base.CacheBump.Enabled = true

	tests := []struct {
		name   string
		cfg    CacheBumpConfig
		route  string
		header string
		want   bool
	}{
		{"global on, route on", base.CacheBump, "claudecode", "", true},
		{"global on, route off", base.CacheBump, "kimi", "", false},
		{"global on, custom off", base.CacheBump, "custom", "", false},
		{"global off", DefaultConfig().CacheBump, "claudecode", "", false},
		{"header on overrides route off", base.CacheBump, "kimi", "on", true},
		{"header off overrides route on", base.CacheBump, "claudecode", "off", false},
		{"header ignored when not allowed", func() CacheBumpConfig {
			cb := base.CacheBump
			cb.AllowHeaderOverride = false
			return cb
		}(), "kimi", "on", false},
		{"unknown route", base.CacheBump, "openrouter", "", false},
		{"header junk falls through to route", base.CacheBump, "claudecode", "maybe", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.EnabledFor(tt.route, tt.header); got != tt.want {
				t.Errorf("EnabledFor(%q, %q) = %v, want %v", tt.route, tt.header, got, tt.want)
			}
		})
	}
}
