package config

import proxyformat "antigravity-go-proxy/internal/format"

// AntigravityConfig shapes Cloud Code (Antigravity) generation requests.
type AntigravityConfig struct {
	// SensitiveWords are masked in the client's system text before it
	// reaches Cloud Code (format.SystemMask). Absent or null keeps the
	// built-in format.DefaultSensitiveWords, [] disables masking, and a list
	// replaces the defaults. No omitempty: a saved [] must stay [].
	SensitiveWords []string `json:"sensitiveWords"`
}

// SystemMask compiles the effective sensitive-word mask.
func (c AntigravityConfig) SystemMask() *proxyformat.SystemMask {
	if c.SensitiveWords == nil {
		return proxyformat.NewSystemMask(proxyformat.DefaultSensitiveWords)
	}
	return proxyformat.NewSystemMask(c.SensitiveWords)
}
