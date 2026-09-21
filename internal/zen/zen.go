// Package zen implements the OpenCode Zen gateway: a thin transparent
// forwarder to https://opencode.ai/zen, which exposes an
// Anthropic-compatible /v1/messages endpoint for the Claude and Qwen
// Anthropic-wire models. The proxy rewrites the Authorization header and
// preserves the Anthropic version/beta headers the client sent.
package zen

import "strings"

const (
	// DefaultBaseURL is the Zen gateway base; endpoint paths like
	// "/v1/messages" are appended by the caller.
	DefaultBaseURL = "https://opencode.ai/zen"
	// DefaultMaxOutputTokens is the max_tokens fill used when neither the
	// client nor the allowlist entry states a cap. It is a floor every id
	// in the Anthropic-wire subset accepts — not a model capability — and
	// can be raised per model via the allowlist entry.
	DefaultMaxOutputTokens = 32768
)

// NormalizeBaseURL returns the base URL with whitespace trimmed, empty values
// defaulted to DefaultBaseURL, trailing slashes trimmed, and any trailing
// "/v1" suffix removed (since endpoint paths like "/v1/messages" are appended
// by caller).
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultBaseURL
	}
	raw = strings.TrimRight(raw, "/")
	raw = strings.TrimSuffix(raw, "/v1")
	return strings.TrimRight(raw, "/")
}
