// Package zen implements the OpenCode Zen gateway in front of
// https://opencode.ai/inference. Zen serves each catalog id over one of three
// wires, and the catalog carries no wire field, so the ids are static lists
// (see AnthropicWireIDs, ChatWireIDs, ResponsesWireIDs):
//
//   - Anthropic wire: MessagesPath, forwarded transparently. The proxy
//     rewrites the Authorization header and preserves the Anthropic
//     version/beta headers the client sent.
//   - Chat Completions wire (chatwire.go) and Responses wire
//     (responseswire.go, responsesstream.go): the Anthropic request is
//     translated to ChatPath or ResponsesPath and the answer translated back,
//     so the caller still sees an Anthropic-shaped reply.
//
// ForwardSystemOne additionally passes a Jev systemone body through unchanged,
// because that contract has no Anthropic Messages mapping.
package zen

import "strings"

const (
	// DefaultBaseURL is the Zen gateway base; the route paths below are
	// appended by the caller.
	DefaultBaseURL = "https://opencode.ai/inference"
	// DefaultMaxOutputTokens is the max_tokens fill used when neither the
	// client nor the allowlist entry states a cap. It is a floor every id
	// in the Anthropic-wire subset accepts — not a model capability — and
	// can be raised per model via the allowlist entry.
	DefaultMaxOutputTokens = 32768
)

// Zen's routes under the base URL, one prefix per upstream protocol family,
// as OpenCode 2.2.3 calls them: models.dev lists its opencode provider's
// anthropic models at /inference/anthropic/v1 and its openai and
// openai-compatible ones at /inference/openai/v1, and the client's Jev route
// is /inference/systemone/v1 plus /systemone. The catalog OpenCode never
// fetches (it reads models.dev) is served under the openai prefix. OpenCode
// 2.0.20 used /zen/v1/<route> for all of them.
const (
	MessagesPath  = "/anthropic/v1/messages"
	ChatPath      = "/openai/v1/chat/completions"
	ResponsesPath = "/openai/v1/responses"
	ModelsPath    = "/openai/v1/models"
	SystemOnePath = "/systemone/v1/systemone"
)

// NormalizeBaseURL returns the base URL with whitespace trimmed, empty values
// defaulted to DefaultBaseURL, and trailing slashes trimmed, ready for a route
// path to be appended.
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultBaseURL
	}
	return strings.TrimRight(raw, "/")
}
