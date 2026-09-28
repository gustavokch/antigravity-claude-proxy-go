package config

import (
	"encoding/json"
	"testing"
)

func TestDefaultMitmConfigIsDisabledAndValid(t *testing.T) {
	cfg := DefaultConfig().Mitm
	if cfg.Enabled {
		t.Fatal("mitm must be off by default")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8092" || cfg.RegistryMax != 1000 || cfg.RegistryTTLMinutes != 1440 {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestMitmConfigValidate(t *testing.T) {
	ok := DefaultMitmConfig()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*MitmConfig){
		"non-loopback": func(c *MitmConfig) { c.Listen = "0.0.0.0:8092" },
		"no port":      func(c *MitmConfig) { c.Listen = "127.0.0.1" },
		"zero max":     func(c *MitmConfig) { c.RegistryMax = 0 },
		"huge max":     func(c *MitmConfig) { c.RegistryMax = 100001 },
		"zero ttl":     func(c *MitmConfig) { c.RegistryTTLMinutes = 0 },
		"huge ttl":     func(c *MitmConfig) { c.RegistryTTLMinutes = 10081 },
	}
	for name, mutate := range cases {
		cfg := DefaultMitmConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// A saved partial block must merge over the defaults, not zero them.
func TestMitmConfigPartialJSONKeepsDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(`{"mitm":{"enabled":true}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Mitm.Enabled || cfg.Mitm.Listen != "127.0.0.1:8092" {
		t.Fatalf("partial block lost defaults: %+v", cfg.Mitm)
	}
	if err := cfg.Mitm.Validate(); err != nil {
		t.Fatal(err)
	}
}
