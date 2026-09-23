// Package ccidentity reproduces vanilla Claude Code's HTTP request identity.
//
// The values it sends are not invented. Every one is transcribed from a live
// capture of Claude Code's own traffic, recorded in
// .reference/claude-code-headers-20260923.txt. Read that file before changing
// anything here: it records which values are constants, which are generated per
// request, and which are unresolved.
//
// Two limits are deliberate and documented rather than worked around:
//
//   - Header ORDER cannot be reproduced. net/http sorts header keys before
//     writing them (sortedKeyValues in net/http/header.go), and the captured
//     wire order is not sorted. A caller that needs the captured order needs a
//     transport that writes the request itself, not http.Transport.
//   - Header NAMES keep their exact case, because the appliers assign into the
//     http.Header map directly. Header.Set would canonicalise, turning the
//     captured X-Stainless-OS into X-Stainless-Os, which reaches the wire over
//     the HTTP/1.1 transport the capture used.
package ccidentity

// Header is one request header with the exact name and value to send. The name
// is used as a map key verbatim, so case is preserved.
type Header struct {
	Name  string
	Value string
}

// DynamicHeader is a header whose value cannot be a constant: request ids, and
// anything else the capture shows varying between two requests from one session.
type DynamicHeader struct {
	Name  string
	Value func(Identity, Turn) string
}

// Identity is the per-account, per-session context a spoofed request carries.
// It is intentionally separate from Turn: identity is stable across a
// conversation, turn is not.
type Identity struct {
	// AccountUUID is the OAuth account's UUID. The capture recorded it as an
	// empty string in every request, so an empty value is a faithful
	// reproduction rather than a missing field.
	AccountUUID string
	// DeviceID seeds the metadata.user_id device hash. Empty means derive it
	// from the session key.
	DeviceID string
	// SessionKey is the client's session identifier — the same value the pool
	// uses for account stickiness, so identity and routing agree.
	SessionKey string
	// ClientVersion is the Claude Code version to claim, e.g. "2.1.280".
	ClientVersion string
	// Entrypoint is the cc_entrypoint value, e.g. "sdk-cli" or "cli".
	Entrypoint string
	// TurnOrigin is the cc_turn_origin value. The capture recorded "sdk" for
	// every --print request; the interactive value is unverified.
	TurnOrigin string
	// UserAgent overrides the messages-path User-Agent. Empty uses the default.
	UserAgent string
	// StainlessOS overrides X-Stainless-OS. Empty uses the captured value.
	StainlessOS string
	// StainlessRuntimeVersion overrides X-Stainless-Runtime-Version. Empty uses
	// the captured value.
	StainlessRuntimeVersion string
}

// Turn carries the per-request pieces of the identity.
type Turn struct {
	// PrevRequestID is the upstream request id of the previous turn, sent as
	// cc_prev_req. Claude Code only includes it on a follow-up turn.
	PrevRequestID string
}

// Profile is a captured Claude Code request identity.
//
// The shape is a list of headers plus an explicit omit list rather than named
// fields per header, because the capture decides what exists: a named-field
// struct would need editing every time the client grows or drops a header.
//
// The constant header set is deliberately NOT a field here. Three of its values
// depend on the identity or on operator overrides, so it is a function of the
// identity — see StaticFor. The metadata key order is not a field either: it is
// fixed by the declaration order of the struct that encodes it.
type Profile struct {
	// Dynamic headers are regenerated on every request.
	Dynamic []DynamicHeader
	// Omit lists header names quiet Claude Code never sends. The applier deletes
	// them, so a foreign client's value cannot reach the upstream. This is the
	// leak guard and it is not optional: without it, a Cursor or Codex request
	// carries its own User-Agent and x-stainless family straight through.
	Omit []string
	// Path is the request path, including the query string the capture shows.
	Path string
	// Betas is the captured anthropic-beta list in captured order. It replaces
	// whatever the client sent rather than being appended to: the captured order
	// puts claude-code-20250219 ahead of oauth-2025-04-20, which append-only
	// cannot produce.
	Betas []string
}
