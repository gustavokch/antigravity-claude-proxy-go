package ccidentity

import (
	"runtime"
	"strings"
)

// Values transcribed from .reference/claude-code-headers-20261020.txt, a live
// capture of Claude Code 2.1.304's own traffic to api.anthropic.com. Each
// constant names the artifact it came from. Nothing here is inferred unless the
// comment says so and says why.
const (
	// ClientVersion is the version literal in cc_version, minus the suffix that
	// versionSuffix derives from the conversation's first prompt.
	ClientVersion = "2.1.304"

	// MessagesUserAgent is the User-Agent on POST /v1/messages. The capture
	// records claude-cli/, lowercase, with a parenthesised mode.
	MessagesUserAgent = "claude-cli/2.1.304 (external, sdk-cli)"

	// DiscoveryUserAgent is the User-Agent on GET /api/claude_code/*. It is a
	// DIFFERENT family from the messages path: claude-code/ not claude-cli/, and
	// no parenthesised mode. Two spellings, two families.
	DiscoveryUserAgent = "claude-code/2.1.304"

	// EntrypointSDKCLI and EntrypointInteractive are the two cc_entrypoint values
	// known to exist. The capture only ever produced sdk-cli (because every run
	// used --print); cli comes from docs/classifier-fallback-notes.md, which
	// recorded it from an interactive session. The cli header set itself is
	// still uncaptured.
	EntrypointSDKCLI      = "sdk-cli"
	EntrypointInteractive = "cli"

	// TurnOriginSDK is the cc_turn_origin value observed alongside sdk-cli.
	// TurnOriginHuman is the value the 2.1.304 binary gives a prompt a person
	// typed into an interactive session (origin kind "human"); it is the only
	// origin that advances cc_prompt_index.
	TurnOriginSDK   = "sdk"
	TurnOriginHuman = "human"

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
	"per-turn-control-2026-07-01",
	"mid-conversation-tool-changes-2026-07-01",
	"advanced-tool-use-2025-11-20",
	"mid-conversation-system-clear-at-2026-08-21",
	"effort-2025-11-24",
	"thinking-binding-controls-2026-08-01",
	"extended-cache-ttl-2025-04-11",
	"cache-diagnosis-2026-04-07",
}

// staticHeaders are the captured constant header set, minus the ones the
// transport writes itself (Host, Content-Length, Connection and
// Accept-Encoding; see httpwire and transport.ClaudeCodeMessages).
func staticHeaders(id Identity) []Header {
	return []Header{
		{"Accept", "application/json"},
		{"Content-Type", "application/json"},
		{"User-Agent", messagesUserAgent(id)},
		{"X-Stainless-Arch", stainlessArch(id)},
		{"X-Stainless-Lang", "js"},
		{"X-Stainless-OS", stainlessOS(id)},
		{"X-Stainless-Package-Version", "0.128.0"},
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
// "claude-cli/2.1.304 (external, cli)". No capture has confirmed that exact
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

// stainlessOS returns the OS the proxy runs on, in the spelling the Stainless
// SDK reports it, unless overridden.
//
// The capture ran in a Linux container, so "Linux" is what was observed, but a
// proxy on another machine would then claim an OS its TCP/IP stack contradicts:
// the SYN's TTL, window and options say which OS sent them, whatever the headers
// claim. The real client reports the machine it runs on, so the default is the
// machine the proxy runs on. An operator who runs the proxy behind another
// host's network identity sets stainlessOs.
func stainlessOS(id Identity) string {
	if id.StainlessOS != "" {
		return id.StainlessOS
	}
	return HostOS(runtime.GOOS)
}

// stainlessArch is stainlessOS for X-Stainless-Arch.
func stainlessArch(id Identity) string {
	if id.StainlessArch != "" {
		return id.StainlessArch
	}
	return HostArch(runtime.GOARCH)
}

// HostOS is the X-Stainless-OS value the SDK reports on a machine whose Go
// GOOS is goos.
//
// The SDK never sees GOOS. It reads Node's process.platform, so GOOS is first
// renamed to what Node calls the same system and the SDK's normalizePlatform
// then runs over that name unchanged. Skipping the first step spells windows as
// "Other:windows" and solaris as "Other:solaris", where the SDK says "Windows"
// and "Other:sunos".
func HostOS(goos string) string {
	return normalizePlatform(nodePlatform(goos))
}

// HostArch is the X-Stainless-Arch value the SDK reports on a machine whose Go
// GOARCH is goarch: Node's process.arch name for it, run through the SDK's
// normalizeArch. The 32-bit x86 that Go calls 386 is "ia32" to Node, which
// normalizeArch does not recognize and spells "other:ia32"; Node never reports
// "x32".
func HostArch(goarch string) string {
	return normalizeArch(nodeArch(goarch))
}

// nodePlatform is Node's process.platform for a Go GOOS. The systems Node
// supports that Go also names are spelled the same in both.
func nodePlatform(goos string) string {
	switch goos {
	case "windows":
		return "win32"
	case "solaris", "illumos":
		return "sunos"
	}
	return goos
}

// nodeArch is Node's process.arch for a Go GOARCH. Node does not tell the byte
// orders of ppc64 apart, and spells the little-endian MIPS ones with an "el".
// arm, arm64, loong64, mips, mips64, ppc64, riscv64 and s390x are spelled the
// same in both.
func nodeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "ppc64le":
		return "ppc64"
	case "mipsle":
		return "mipsel"
	case "mips64le":
		return "mips64el"
	}
	return goarch
}

// normalizeArch is the Stainless SDK's function of the same name
// (anthropic-sdk-typescript, src/internal/detect-platform.ts), ported rule for
// rule. It is given the runtime's own architecture string, which on Node is
// process.arch.
func normalizeArch(arch string) string {
	switch arch {
	case "x32":
		return "x32"
	case "x86_64", "x64":
		return "x64"
	case "arm":
		return "arm"
	case "aarch64", "arm64":
		return "arm64"
	case "":
		return "unknown"
	}
	return "other:" + arch
}

// normalizePlatform is the Stainless SDK's function of the same name, ported
// rule for rule. It is given the runtime's platform string, which on Node is
// process.platform, and lowercases it before comparing.
func normalizePlatform(platform string) string {
	platform = strings.ToLower(platform)
	switch {
	case strings.Contains(platform, "ios"):
		return "iOS"
	case platform == "android":
		return "Android"
	case platform == "darwin":
		return "MacOS"
	case platform == "win32":
		return "Windows"
	case platform == "freebsd":
		return "FreeBSD"
	case platform == "openbsd":
		return "OpenBSD"
	case platform == "linux":
		return "Linux"
	case platform != "":
		return "Other:" + platform
	}
	return "Unknown"
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

	// Written by the transport, not copied from the caller: httpwire writes
	// Accept-Encoding, Connection, Host and Content-Length itself, in the
	// captured positions and with the captured values, and decodes br and zstd.
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
		// New in 2.1.304: the same UUID as cc_prompt_id in the billing header, in
		// all 7 captured messages requests (.reference/claude-code-headers-20261020*.jsonl).
		// It must stay equal to promptUUID, which BillingHeader writes, or the two
		// slots disagree in a way no real client produces.
		{"x-claude-code-prompt-id", func(id Identity, turn Turn) string {
			return promptUUID(id, turn)
		}},
	}
}

// defaultProfile is the captured profile. Nothing in it varies per call:
// dynamicHeaders holds three closures that take the identity as an argument
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
