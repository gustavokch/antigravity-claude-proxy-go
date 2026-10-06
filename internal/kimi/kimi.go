// Package kimi implements the Kimi gateway: a thin transparent forwarder to an
// Anthropic-compatible /v1/messages endpoint. Two vendor products share it and
// they are different APIs with different model IDs and effort defaults: the
// Moonshot Open Platform (https://api.moonshot.ai/anthropic, the config
// default, API key) and Kimi Code (https://api.kimi.ai/coding, subscription,
// OAuth). The proxy rewrites the Authorization header and preserves the
// Anthropic version/beta headers the client sent; it never alters reasoning
// fields, because the gateway is a transparent forwarder (ADR-0001): Kimi Code
// maps Claude Code's effort levels itself, while the Open Platform publishes
// only low|high|max and answers other values with its own error, which the
// proxy does not hide. See docs/reasoning-parameters.md.
package kimi

import "strings"

// NormalizeBaseURL returns the base URL with whitespace trimmed, empty values
// defaulted to https://api.moonshot.ai/anthropic, trailing slashes trimmed, and any
// trailing "/v1" suffix removed (since endpoint paths like "/v1/messages" are
// appended by caller).
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "https://api.moonshot.ai/anthropic"
	}
	raw = strings.TrimRight(raw, "/")
	raw = strings.TrimSuffix(raw, "/v1")
	return strings.TrimRight(raw, "/")
}
