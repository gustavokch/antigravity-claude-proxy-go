package config

import (
	"encoding/json"
	"strings"
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
	cases := []struct {
		name   string
		mutate func(*MitmConfig)
		want   string // substring of the error
	}{
		{"non-loopback", func(c *MitmConfig) { c.Listen = "0.0.0.0:8092" }, "loopback"},
		{"no port", func(c *MitmConfig) { c.Listen = "127.0.0.1" }, "host:port"},
		{"empty listen", func(c *MitmConfig) { c.Listen = "" }, "host:port"},
		{"zero max", func(c *MitmConfig) { c.RegistryMax = 0 }, "registryMax"},
		{"negative max", func(c *MitmConfig) { c.RegistryMax = -1 }, "registryMax"},
		{"huge max", func(c *MitmConfig) { c.RegistryMax = 100001 }, "registryMax"},
		{"zero ttl", func(c *MitmConfig) { c.RegistryTTLMinutes = 0 }, "registryTtlMinutes"},
		{"negative ttl", func(c *MitmConfig) { c.RegistryTTLMinutes = -5 }, "registryTtlMinutes"},
		{"huge ttl", func(c *MitmConfig) { c.RegistryTTLMinutes = 10081 }, "registryTtlMinutes"},
	}
	for _, c := range cases {
		cfg := DefaultMitmConfig()
		c.mutate(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", c.name, err, c.want)
		}
	}
}

func TestMitmConfigValidateAcceptsTheBounds(t *testing.T) {
	for name, mutate := range map[string]func(*MitmConfig){
		"minimum limits": func(c *MitmConfig) { c.RegistryMax, c.RegistryTTLMinutes = 1, 1 },
		"maximum limits": func(c *MitmConfig) { c.RegistryMax, c.RegistryTTLMinutes = 100000, 10080 },
		"ipv6 loopback":  func(c *MitmConfig) { c.Listen = "[::1]:8092" },
		"localhost":      func(c *MitmConfig) { c.Listen = "localhost:8092" },
	} {
		cfg := DefaultMitmConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s rejected: %v", name, err)
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
