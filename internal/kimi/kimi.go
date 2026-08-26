// Package kimi implements the Kimi Code gateway: a thin transparent forwarder
// to https://api.kimi.com/coding, which exposes an Anthropic-compatible
// /v1/messages endpoint. The proxy rewrites the Authorization header and
// preserves the Anthropic version/beta headers the client sent.
package kimi

import "strings"

// NormalizeBaseURL returns the base URL with whitespace trimmed, empty values
// defaulted to https://api.kimi.com/coding, trailing slashes trimmed, and any
// trailing "/v1" suffix removed (since endpoint paths like "/v1/messages" are
// appended by caller).
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "https://api.kimi.com/coding"
	}
	raw = strings.TrimRight(raw, "/")
	raw = strings.TrimSuffix(raw, "/v1")
	return strings.TrimRight(raw, "/")
}
