// Package transport builds the outbound HTTP transports for the upstream paths
// that carry no fingerprint requirement of their own: custom endpoints, Kimi,
// Zen. The Cloud Code path builds its own transport in internal/cloudcode, where
// the ClientHello is the product.
package transport

import (
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
