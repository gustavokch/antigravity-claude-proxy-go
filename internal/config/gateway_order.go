package config

import (
	"strings"

	"antigravity-go-proxy/internal/modelcatalog"
)

// GatewayID identifies one dispatchable backend in gateway precedence order.
// "cloudcode" is the terminal account-backed path, not a gateway, but it
// participates in the order list so an operator can hoist it to prefer the
// account pool for a model.
type GatewayID string

const (
	GatewayKimi       GatewayID = "kimi"
	GatewayZen        GatewayID = "zen"
	GatewayClaudeCode GatewayID = "claudecode"
	GatewayOpenRouter GatewayID = "openrouter"
	GatewayCustom     GatewayID = "custom"
	GatewayCloudCode  GatewayID = "cloudcode"
)

// DefaultGatewayOrder returns the precedence used when config.json says
// nothing, and the order in which providers omitted from a configured list
// are appended. It reproduces the historical dispatch if-chain exactly
// (Kimi, Zen, ClaudeCode, OpenRouter, custom endpoints; Cloud Code terminal).
func DefaultGatewayOrder() []GatewayID {
	return []GatewayID{
		GatewayKimi,
		GatewayZen,
		GatewayClaudeCode,
		GatewayOpenRouter,
		GatewayCustom,
		GatewayCloudCode,
	}
}

// KnownGatewayIDs returns every gateway ID this build knows how to dispatch
// to, in default precedence order.
func KnownGatewayIDs() []GatewayID {
	return DefaultGatewayOrder()
}

// IsKnownGatewayID reports whether id is a gateway this build recognises.
func IsKnownGatewayID(id GatewayID) bool {
	switch normalizeGatewayID(id) {
	case GatewayKimi, GatewayZen, GatewayClaudeCode, GatewayOpenRouter, GatewayCustom, GatewayCloudCode:
		return true
	}
	return false
}

// GatewayOrderConfig holds the operator-configurable gateway precedence: a
// global order list plus per-model overrides. The per-model entry wins.
type GatewayOrderConfig struct {
	Order   []GatewayID            `json:"order,omitempty"`
	ByModel map[string][]GatewayID `json:"byModel,omitempty"`
}

// normalizeGatewayID trims whitespace and lowercases one gateway ID so a
// hand-edited config.json cannot misroute on spelling.
func normalizeGatewayID(id GatewayID) GatewayID {
	return GatewayID(strings.ToLower(strings.TrimSpace(string(id))))
}

// NormalizeModelKey trims, lowercases, and drops a trailing "[1m]" marker.
// It reuses modelcatalog.Strip1mSuffix so one override covers every spelling
// a client sends for the same model. A second implementation would be free to
// drift from the one discovery already uses.
func NormalizeModelKey(model string) string {
	return strings.ToLower(modelcatalog.Strip1mSuffix(model))
}

// Effective returns the precedence for one model: the matching ByModel entry
// when one exists, otherwise Order; then every known provider that the result
// omits, appended in DefaultGatewayOrder order. An empty list yields
// DefaultGatewayOrder. Unknown IDs are dropped, so a config.json written by a
// newer build cannot break routing. Duplicates keep their first occurrence.
// An empty model selects the global order only.
func (c GatewayOrderConfig) Effective(model string) []GatewayID {
	var base []GatewayID
	if key := NormalizeModelKey(model); key != "" && len(c.ByModel) > 0 {
		for rawKey, ids := range c.ByModel {
			if NormalizeModelKey(rawKey) == key {
				base = ids
				break
			}
		}
	}
	if base == nil {
		base = c.Order
	}
	seen := make(map[GatewayID]bool, len(base)+len(DefaultGatewayOrder()))
	out := make([]GatewayID, 0, len(DefaultGatewayOrder()))
	for _, id := range base {
		norm := normalizeGatewayID(id)
		if !IsKnownGatewayID(norm) || seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	for _, id := range DefaultGatewayOrder() {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
