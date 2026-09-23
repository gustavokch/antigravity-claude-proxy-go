package ccidentity

// Values transcribed from .reference/claude-code-headers-20260923.txt, a live
// capture of Claude Code 2.1.280's own traffic to api.anthropic.com. Each
// constant names the artifact it came from. Nothing here is inferred unless the
// comment says so and says why.
const (
	// ClientVersion is the version literal in cc_version, minus the build suffix
	// the capture shows varying per process (2.1.280.5c2 and 2.1.280.336 were
	// both observed from one binary).
	ClientVersion = "2.1.280"

	// MessagesUserAgent is the User-Agent on POST /v1/messages. The capture
	// records claude-cli/, lowercase, with a parenthesised mode.
	MessagesUserAgent = "claude-cli/2.1.280 (external, sdk-cli)"

	// DiscoveryUserAgent is the User-Agent on GET /api/claude_code/*. It is a
	// DIFFERENT family from the messages path: claude-code/ not claude-cli/, and
	// no parenthesised mode. Two spellings, two families.
	DiscoveryUserAgent = "claude-code/2.1.280"

	// EntrypointSDKCLI and EntrypointInteractive are the two cc_entrypoint values
	// known to exist. The capture only ever produced sdk-cli (because every run
	// used --print); cli comes from docs/classifier-fallback-notes.md, which
	// recorded it from an interactive session. The cli header set itself is
	// still uncaptured.
	EntrypointSDKCLI      = "sdk-cli"
	EntrypointInteractive = "cli"

	// TurnOriginSDK is the cc_turn_origin value observed alongside sdk-cli. The
	// interactive value is unverified.
	TurnOriginSDK = "sdk"

	// MessagesPath includes the query string the capture shows. A request to
	// /v1/messages without ?beta=true does not match the captured traffic.
	MessagesPath = "/v1/messages?beta=true"
)

// Betas is the captured anthropic-beta list, in captured order. Order is part of
// the value: claude-code-20250219 leads, oauth-2025-04-20 is second.
var Betas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	"advanced-tool-use-2025-11-20",
	"mid-conversation-system-clear-at-2026-08-21",
	"effort-2025-11-24",
	"thinking-binding-controls-2026-08-01",
	"extended-cache-ttl-2025-04-11",
	"cache-diagnosis-2026-04-07",
}

// staticHeaders are the captured constant header set, minus the ones net/http
// owns (Host, Content-Length) and the ones the transport manages (Connection).
//
// Accept-Encoding is deliberately absent: the operator chose to leave Go's
// transparent gzip handling in place rather than spoof the captured
// "gzip, deflate, br, zstd", which would mean inflating upstream responses in
// the proxy, on the SSE path, with br and zstd missing from the stdlib.
// Accept-Encoding is in the omit list instead, so Go's default applies.
func staticHeaders(id Identity) []Header {
	return []Header{
		{"Accept", "application/json"},
		{"Content-Type", "application/json"},
		{"User-Agent", messagesUserAgent(id)},
		{"X-Stainless-Arch", "arm64"},
		{"X-Stainless-Lang", "js"},
		{"X-Stainless-OS", stainlessOS(id)},
		{"X-Stainless-Package-Version", "0.112.1"},
		{"X-Stainless-Retry-Count", "0"},
		{"X-Stainless-Runtime", "node"},
		{"X-Stainless-Runtime-Version", stainlessRuntimeVersion(id)},
		{"X-Stainless-Timeout", "600"},
		{"anthropic-dangerous-direct-browser-access", "true"},
		{"anthropic-version", "2023-06-01"},
		{"x-app", "cli"},
		{"x-claude-code-request-class", "main"},
	}
}

// messagesUserAgent returns the captured literal, or a constructed one when the
// caller changed the entrypoint.
//
// The construction is INFERRED, not captured: the sample shows the entrypoint
// appears inside the parentheses, so substituting cli yields
// "claude-cli/2.1.280 (external, cli)". No capture has confirmed that exact
// string. It is used only when the operator explicitly sets a non-default
// entrypoint, and it is the reason a cli capture is still wanted.
func messagesUserAgent(id Identity) string {
	if id.UserAgent != "" {
		return id.UserAgent
	}
	entrypoint := id.Entrypoint
	if entrypoint == "" || entrypoint == EntrypointSDKCLI {
		return MessagesUserAgent
	}
	version := id.ClientVersion
	if version == "" {
		version = ClientVersion
	}
	return "claude-cli/" + version + " (external, " + entrypoint + ")"
}

// stainlessOS returns the captured OS value unless overridden. The capture ran in
// a Linux container, so "Linux" is what was observed; a macOS host would report
// something else. The operator chose to default to the captured value and allow
// an override rather than infer the host's.
func stainlessOS(id Identity) string {
	if id.StainlessOS != "" {
		return id.StainlessOS
	}
	return "Linux"
}

// stainlessRuntimeVersion is the captured runtime version, overridable for the
// same reason as stainlessOS.
func stainlessRuntimeVersion(id Identity) string {
	if id.StainlessRuntimeVersion != "" {
		return id.StainlessRuntimeVersion
	}
	return "v26.3.0"
}

// omittedHeaders are names quiet Claude Code never sends, plus the ones this
// implementation deliberately declines to spoof. Deleting them is what stops a
// foreign client's fingerprint from surviving the rewrite.
var omittedHeaders = []string{
	// Never sent by Claude Code. Every one of these leaks a non-Claude-Code
	// client or a Go default.
	"x-api-key",
	"Cookie",
	"Accept-Language",
	"X-Forwarded-For",
	"Forwarded",
	"x-session-id",
	"session-id",
	"anthropic-session-id",
	"x-conversation-id",

	// Left to net/http and the transport, per the fidelity decision. Go adds
	// gzip itself, which keeps transparent decompression working.
	"Accept-Encoding",
	"Connection",
	"Content-Length",
	"Host",
	"Transfer-Encoding",
	"Trailer",
	"Upgrade",
	"Proxy-Connection",
	"Keep-Alive",
	"TE",

	// The client's own app attribution, which must not reach an Anthropic
	// upstream. These belong to the OpenRouter spoof, not this one.
	"HTTP-Referer",
	"Referer",
	"X-Title",
	"X-OpenRouter-Title",
	"X-OpenRouter-Categories",
}

// dynamicHeaders are the headers regenerated per request and per session.
func dynamicHeaders() []DynamicHeader {
	return []DynamicHeader{
		// Stable for one process, so it is derived from the session rather than
		// generated per request.
		{"X-Claude-Code-Session-Id", func(id Identity, _ Turn) string {
			return sessionUUID(id)
		}},
		{"x-client-request-id", func(_ Identity, _ Turn) string {
			return newUUID()
		}},
	}
}

// defaultProfile is the captured profile. Nothing in it varies per call:
// dynamicHeaders holds two closures that take the identity as an argument
// rather than closing over one, and Omit, Path and Betas are constants. It is
// therefore built once instead of on every request.
//
// Omit and Betas are shared with every caller, as they already were when this
// was a constructor. Neither is mutated anywhere; defaults_test.go pins the
// sharing, so a caller that started to mutate one would be caught.
var defaultProfile = Profile{
	Dynamic: dynamicHeaders(),
	Omit:    omittedHeaders,
	Path:    MessagesPath,
	Betas:   Betas,
}

// DefaultProfile returns the captured identity.
func DefaultProfile() Profile {
	return defaultProfile
}

// StaticFor returns the captured constant header set for one identity.
//
// Separate from DefaultProfile because three captured values depend on the
// identity or on operator overrides (User-Agent, X-Stainless-OS,
// X-Stainless-Runtime-Version), so the static set is a function of the identity
// rather than a package-level value.
func StaticFor(id Identity) []Header {
	return staticHeaders(id)
}
