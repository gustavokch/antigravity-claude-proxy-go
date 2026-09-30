// Package zen implements the OpenCode Zen gateway in front of
// https://opencode.ai/zen. Zen serves each catalog id over one of three wires,
// and the catalog carries no wire field, so the ids are static lists (see
// AnthropicWireIDs, ChatWireIDs, ResponsesWireIDs):
//
//   - Anthropic wire: /v1/messages, forwarded transparently. The proxy
//     rewrites the Authorization header and preserves the Anthropic
//     version/beta headers the client sent.
//   - Chat Completions wire (chatwire.go) and Responses wire
//     (responseswire.go, responsesstream.go): the Anthropic request is
//     translated to /v1/chat/completions or /v1/responses and the answer
//     translated back, so the caller still sees an Anthropic-shaped reply.
//
// ForwardSystemOne additionally passes a Jev systemone body through unchanged,
// because that contract has no Anthropic Messages mapping.
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
