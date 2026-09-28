package config

import (
	"fmt"

	"antigravity-go-proxy/internal/mitm"
)

// MitmConfig configures the observe-only Claude Code forward proxy
// (internal/mitm). It is off by default. Changes apply on restart.
type MitmConfig struct {
	Enabled            bool   `json:"enabled"`
	Listen             string `json:"listen,omitempty"`
	RegistryMax        int    `json:"registryMax,omitempty"`
	RegistryTTLMinutes int    `json:"registryTtlMinutes,omitempty"`
}

// DefaultMitmConfig is the disabled default.
func DefaultMitmConfig() MitmConfig {
	return MitmConfig{Enabled: false, Listen: "127.0.0.1:8092", RegistryMax: 1000, RegistryTTLMinutes: 1440}
}

// Validate rejects non-loopback listeners and out-of-range limits.
func (m MitmConfig) Validate() error {
	if err := mitm.ValidateListen(m.Listen); err != nil {
		return err
	}
	if m.RegistryMax < 1 || m.RegistryMax > 100000 {
		return fmt.Errorf("mitm registryMax must be between 1 and 100000")
	}
	if m.RegistryTTLMinutes < 1 || m.RegistryTTLMinutes > 10080 {
		return fmt.Errorf("mitm registryTtlMinutes must be between 1 and 10080")
	}
	return nil
}
