package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGatewayOrder_Effective(t *testing.T) {
	full := []GatewayID{"kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"}
	tests := []struct {
		name  string
		cfg   GatewayOrderConfig
		model string
		want  []GatewayID
	}{
		{"zero value yields default", GatewayOrderConfig{}, "anything", full},
		{"empty model selects global order", GatewayOrderConfig{Order: []GatewayID{GatewayOpenRouter}}, "", []GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"}},
		{"partial order appends rest in default order", GatewayOrderConfig{Order: []GatewayID{GatewayKimi}}, "m", full},
		{"openrouter first appends rest", GatewayOrderConfig{Order: []GatewayID{GatewayOpenRouter}}, "m", []GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"}},
		{"duplicates deduped first-wins", GatewayOrderConfig{Order: []GatewayID{GatewayKimi, GatewayKimi, GatewayZen}}, "m", full},
		{"unknown IDs dropped", GatewayOrderConfig{Order: []GatewayID{"nope", GatewayKimi}}, "m", full},
		{"empty order yields default", GatewayOrderConfig{Order: []GatewayID{}}, "m", full},
		{
			"byModel beats order",
			GatewayOrderConfig{
				Order:   []GatewayID{GatewayKimi},
				ByModel: map[string][]GatewayID{"claude-sonnet-5": {GatewayOpenRouter}},
			},
			"claude-sonnet-5",
			[]GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"},
		},
		{
			"non-matching model falls back to order",
			GatewayOrderConfig{
				Order:   []GatewayID{GatewayOpenRouter},
				ByModel: map[string][]GatewayID{"claude-sonnet-5": {GatewayKimi}},
			},
			"other-model",
			[]GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"},
		},
		{
			"byModel key matches 1m request",
			GatewayOrderConfig{
				ByModel: map[string][]GatewayID{"claude-sonnet-5": {GatewayOpenRouter}},
			},
			"claude-sonnet-5[1m]",
			[]GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"},
		},
		{
			"hand-edited unnormalised key still matches",
			GatewayOrderConfig{
				ByModel: map[string][]GatewayID{"  Claude-Sonnet-5[1M] ": {GatewayOpenRouter}},
			},
			"claude-sonnet-5",
			[]GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"},
		},
		{
			"empty key matches nothing",
			GatewayOrderConfig{
				ByModel: map[string][]GatewayID{"   ": {GatewayOpenRouter}},
			},
			"claude-sonnet-5",
			full,
		},
		{
			"empty order with byModel entry",
			GatewayOrderConfig{
				Order:   []GatewayID{},
				ByModel: map[string][]GatewayID{"m": {GatewayCustom}},
			},
			"m",
			[]GatewayID{"custom", "kimi", "zen", "claudecode", "openrouter", "cloudcode"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Effective(tt.model); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Effective(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestNormalizeModelKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{" Claude-Sonnet-5[1m] ", "claude-sonnet-5"},
		{"[1M]", ""},
		{"gpt-4.1-mini", "gpt-4.1-mini"},
		{"", ""},
		{"  ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := NormalizeModelKey(tt.in); got != tt.want {
				t.Errorf("NormalizeModelKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGatewayOrder_Load(t *testing.T) {
	t.Run("no config file yields default", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := DefaultGatewayOrder()
		if !reflect.DeepEqual(cfg.GatewayOrder.Order, want) {
			t.Errorf("Order = %v, want %v", cfg.GatewayOrder.Order, want)
		}
	})

	t.Run("partial file keeps both keys", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		raw := `{"logLevel":"debug","gatewayOrder":{"order":["openrouter"]}}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !reflect.DeepEqual(cfg.GatewayOrder.Order, []GatewayID{"openrouter"}) {
			t.Errorf("Order = %v", cfg.GatewayOrder.Order)
		}
		if cfg.GatewayOrder.ByModel != nil {
			t.Errorf("ByModel = %v, want nil", cfg.GatewayOrder.ByModel)
		}
		if cfg.LogLevel != "debug" {
			t.Errorf("LogLevel = %q", cfg.LogLevel)
		}
	})

	t.Run("empty order yields default effective", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		raw := `{"gatewayOrder":{"order":[]}}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.GatewayOrder.Effective("m"); !reflect.DeepEqual(got, DefaultGatewayOrder()) {
			t.Errorf("Effective = %v", got)
		}
	})

	t.Run("byModel round-trips through JSON", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		raw := `{"gatewayOrder":{"order":["kimi"],"byModel":{"m":["openrouter"]}}}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		var _ = json.Marshal
		if got := cfg.GatewayOrder.Effective("m"); got[0] != GatewayOpenRouter {
			t.Errorf("Effective(m)[0] = %v", got[0])
		}
	})
}
