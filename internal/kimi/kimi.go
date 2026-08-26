// Package kimi implements the Kimi Code gateway: a thin transparent forwarder
// to https://api.kimi.com/coding, which exposes an Anthropic-compatible
// /v1/messages endpoint. The proxy rewrites the Authorization header and
// preserves the Anthropic version/beta headers the client sent.
package kimi

import "strings"

// NormalizeBaseURL returns the base URL with a single trailing slash trimmed.
// Kimi's coding endpoint is configured without a trailing slash in
// ANTHROPIC_BASE_URL; the proxy appends "/v1/messages" itself.
func NormalizeBaseURL(raw string) string {
	return strings.TrimRight(raw, "/")
}
