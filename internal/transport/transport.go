// Package transport builds the outbound HTTP transports for the upstream paths
// that carry no TLS fingerprint requirement of their own: custom endpoints,
// Kimi, Zen, and Claude Code, which needs only HTTP/1.1 (UpstreamHTTP1). The
// Cloud Code path builds its own transport in internal/cloudcode, where the
// ClientHello is the product.
package transport

import (
	"crypto/tls"
	"net/http"
	"time"
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

// UpstreamHTTP1 returns Upstream with HTTP/2 switched off: the ClientHello
// offers http/1.1 as its only ALPN protocol and every request goes out as
// HTTP/1.1.
//
// This is the one deliberate departure from the default ClientHello in this
// package, and it is for the Claude Code wire identity. The real client reaches
// api.anthropic.com over HTTP/1.1 — every record in
// .reference/claude-code-headers-20261020*.jsonl says so, through an
// interceptor that negotiates h2 with any client that offers it — and a default
// Go transport offers h2 and gets it, which is a difference the server sees
// without looking at a single header.
//
// Only ALPN and the HTTP version change. The cipher suites, curves and
// extensions stay Go's own, so the TLS handshake still differs from the Claude
// Code CLI's (see docs/claude-code-forward-proxy.md).
func UpstreamHTTP1() *http.Transport {
	tr := Upstream()
	// Protocols is what decides: net/http reads it before ForceAttemptHTTP2 and
	// TLSNextProto, and with only HTTP1 set it never registers h2 or touches
	// the TLS config. ForceAttemptHTTP2 is cleared as well so no reader of the
	// field is misled.
	tr.ForceAttemptHTTP2 = false
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	// Offered explicitly: with h2 off Go would send no ALPN extension at all,
	// and the real client sends http/1.1.
	tr.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return tr
}
