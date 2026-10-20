// Package transport builds the outbound HTTP transports for the upstream paths
// that carry no TLS fingerprint requirement of their own: custom endpoints,
// Kimi, Zen. Claude Code is the exception, and ClaudeCodeMessages and
// ClaudeCodeDiscovery replay the handshakes of the real client's two TLS
// stacks. The Cloud Code path builds its own transport in internal/cloudcode,
// where the ClientHello is the product.
package transport

import (
	_ "embed"
	"net/http"
	"time"

	"antigravity-go-proxy/internal/tlsreplay"
)

// ResponseHeaderTimeout bounds how long an upstream may take to send response
// headers. http.DefaultTransport bounds the dial and the TLS handshake but not
// this, so an upstream that accepts the connection and then stays silent holds
// the request, its goroutine, and its pooled connection open until the client
// itself disconnects.
//
// The value matches the default of -upstream-timeout, the flag that sets the same
// bound for Cloud Code. It is deliberately generous: a non-streaming completion
// sends its headers only when the first response byte is ready, so a shorter
// bound would cut off a long generation instead of a hung upstream. Open streams
// are not capped at all — the bound stops at the headers.
const ResponseHeaderTimeout = 5 * time.Minute

// Upstream returns a clone of http.DefaultTransport with ResponseHeaderTimeout
// set. The clone keeps the default dial, TLS handshake, idle, and HTTP/2
// settings, and its TLS config stays empty, so the ClientHello is unchanged.
// Callers cache the result: every call returns a separate transport with its own
// connection pool.
func Upstream() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr := base.Clone()
		tr.ResponseHeaderTimeout = ResponseHeaderTimeout
		return tr
	}
	// Only a replaced, non-Transport http.DefaultTransport reaches here.
	return &http.Transport{ResponseHeaderTimeout: ResponseHeaderTimeout}
}

// The two ClientHellos Claude Code 2.1.304 sends to api.anthropic.com, recorded
// off the wire by a listener that read the first TLS record of each connection
// (record header included). Claude Code runs on Bun, which has two TLS stacks:
//
//   - fetch, ALPN http/1.1: POST /v1/messages, /api/eval/*, and part of
//     /api/event_logging/v2/batch (JA4 t13d1713h1_5b57614c22b0_7f527383e249);
//   - node:https via axios, no ALPN: every GET under /api/claude_code/*,
//     /api/claude_cli/bootstrap, /mcp-registry/*, and the rest of the event
//     batches (JA4 t13d181000_5d04281c6031_25d8bb11352d).
//
// The request-to-stack map comes from scripts/mitm_header_dump.py, which
// records the JA4 of the hello that opened each connection. A new Claude Code
// release that upgrades Bun changes these bytes; recapture them then.
var (
	//go:embed claude-code-fetch-clienthello.bin
	claudeCodeFetchHello []byte
	//go:embed claude-code-node-clienthello.bin
	claudeCodeNodeHello []byte
)

// http1 is Upstream with HTTP/2 switched off, so every request goes out as
// HTTP/1.1. The real client reaches api.anthropic.com over HTTP/1.1 — every
// record in .reference/claude-code-headers-20261020*.jsonl says so, through an
// interceptor that negotiates h2 with any client that offers it — and a default
// Go transport offers h2 and gets it, which the server sees without looking at
// a single header. Protocols is what decides: net/http reads it before
// ForceAttemptHTTP2 and TLSNextProto.
func http1() *http.Transport {
	tr := Upstream()
	tr.ForceAttemptHTTP2 = false
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	return tr
}

// ClaudeCodeMessages returns an HTTP/1.1 transport whose TLS handshake is the
// one Claude Code's fetch stack sends, for POST /v1/messages. Every call
// returns a transport with its own connection pool.
func ClaudeCodeMessages() *http.Transport {
	return fetchHello.Transport(http1())
}

// ClaudeCodeDiscovery is ClaudeCodeMessages for the requests the real client
// makes through node:https: the account, settings and model GETs, and the OAuth
// token calls (which are an inference: only api.anthropic.com was captured).
func ClaudeCodeDiscovery() *http.Transport {
	return nodeHello.Transport(http1())
}

// Each stack resumes TLS sessions, as Bun does: after the first connection to a
// host the real client's hello carries pre_shared_key. The cache is per stack,
// so the messages transports share tickets and the discovery ones share theirs.
var (
	fetchHello = tlsreplay.MustNew(claudeCodeFetchHello, []string{"http/1.1"}).WithResumption()
	nodeHello  = tlsreplay.MustNew(claudeCodeNodeHello, nil).WithResumption()
)
