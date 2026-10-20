// Package tlsreplay sends a captured TLS ClientHello through utls, so a Go
// client presents the handshake of the client that produced the capture.
//
// It exists for upstreams that identify a client by its handshake (Zen, which
// wants Bun's, and Claude Code's own API, whose client is Bun as well). Go's
// crypto/tls cannot produce a BoringSSL hello, and AGENTS.md forbids bending
// it into one: the Cloud Code path keeps the empty tls.Config and never comes
// here.
//
// The capture must include the TLS record header, as utls.Fingerprinter
// requires; that is what a recorder that reads the first record off the wire
// yields.
//
// Behind a proxy the connection to the target is tunnelled first, through
// proxydial (http, https, socks5 and socks5h proxies), and the hello is the first
// thing sent inside the tunnel.
package tlsreplay

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"antigravity-go-proxy/internal/proxydial"

	utls "github.com/refraction-networking/utls"
)

// dialTimeout and handshakeTimeout bound the two local steps of the dial. A
// zero net.Dialer Timeout waits for the OS, and with DialTLSContext set
// net/http does not apply its own TLSHandshakeTimeout, so the handshake would
// otherwise run on the caller's context, which for a streamed request has no
// deadline at all. Both values match http.DefaultTransport. They are local
// timers: the ClientHello bytes are unaffected.
const (
	dialTimeout      = 30 * time.Second
	handshakeTimeout = 10 * time.Second
)

// Hello is a captured ClientHello ready to be replayed. It keeps the raw
// capture and builds a utls spec from it for every connection: a spec is
// mutated by the handshake it is applied to, and one reused for a second
// connection fails that handshake.
type Hello struct {
	raw      []byte
	sessions utls.ClientSessionCache // nil unless WithResumption
}

// New fingerprints a captured ClientHello and checks that it offers exactly the
// ALPN protocols in wantALPN (none when wantALPN is empty).
//
// JA4 counts ALPN protocols instead of naming them, so a recapture that
// advertised h2 would pass a fingerprint gate while the custom-dial path of
// net/http has no HTTP/2 handler, and every request would hang. Rejecting the
// capture here leaves the failure visible instead.
func New(raw []byte, wantALPN []string) (*Hello, error) {
	hello := &Hello{raw: slices.Clone(raw)}
	spec, err := hello.fingerprint()
	if err != nil {
		return nil, err
	}
	if got := specALPN(spec); !slices.Equal(got, wantALPN) {
		return nil, fmt.Errorf("tlsreplay: captured ClientHello offers ALPN %v, want %v", got, wantALPN)
	}
	return hello, nil
}

// MustNew is New for a capture embedded in the binary: a failure is a
// programming error that the package's tests catch, not a runtime condition.
func MustNew(raw []byte, wantALPN []string) *Hello {
	hello, err := New(raw, wantALPN)
	if err != nil {
		panic(err)
	}
	return hello
}

// WithResumption returns a Hello that resumes TLS sessions the way the client
// it imitates does. Bun keeps a session cache per stack: the first connection to
// a host sends the full hello, and later ones send the same hello with
// pre_shared_key appended (Claude Code 2.1.304 against a server that issues
// tickets: every connection after the first was resumed). A transport that
// always sent the full hello would present a different JA4 on most of its
// connections than the client does.
//
// The cache belongs to the returned Hello, so every transport built from it
// shares tickets, as one stack's connections do. The receiver is unchanged.
func (h *Hello) WithResumption() *Hello {
	return &Hello{raw: h.raw, sessions: utls.NewLRUClientSessionCache(resumptionCacheSize)}
}

// resumptionCacheSize is the number of servers whose tickets are kept.
const resumptionCacheSize = 32

// Spec returns a fresh utls spec of the capture.
func (h *Hello) Spec() *utls.ClientHelloSpec {
	spec, _ := h.fingerprint() // New proved the capture fingerprints
	return spec
}

func (h *Hello) fingerprint() (*utls.ClientHelloSpec, error) {
	spec, err := (&utls.Fingerprinter{}).RawClientHello(h.raw)
	if err != nil {
		return nil, fmt.Errorf("tlsreplay: fingerprint ClientHello: %w", err)
	}
	return spec, nil
}

func specALPN(spec *utls.ClientHelloSpec) []string {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			return alpn.AlpnProtocols
		}
	}
	return nil
}

// Transport returns base with TLS dialed through the captured hello. base is
// consumed: the caller passes a fresh clone, and keeps its pool. Certificate
// verification follows base.TLSClientConfig (RootCAs, InsecureSkipVerify) when
// one is set; its NextProtos and cipher settings play no part, because the
// capture decides the hello.
//
// base.Proxy is taken over for https targets. net/http ignores DialTLSContext
// for an https target behind a proxy: it dials the proxy in plain text and runs
// crypto/tls over the tunnel itself, so the hello on the wire would be Go's
// own and nothing would say so. Here the dialer opens the tunnel itself, with
// CONNECT through an http or https proxy or a relay through a socks5 or socks5h
// one (see proxydial), and replays the hello over it. A plain-http target has no
// handshake to replay, so net/http keeps routing it through base.Proxy.
func (h *Hello) Transport(base *http.Transport) *http.Transport {
	proxy := base.Proxy
	if proxy != nil {
		base.Proxy = func(req *http.Request) (*url.URL, error) {
			if req.URL.Scheme == "https" {
				return nil, nil
			}
			return proxy(req)
		}
	}
	base.DialTLSContext = h.DialFunc(proxy, func() *tls.Config { return base.TLSClientConfig })
	return base
}

// DialFunc returns a dialer for clients that bring their own HTTP layer. proxy
// selects the proxy for a target (http, https, socks5 or socks5h), nil for none;
// verify supplies the certificate verification settings (RootCAs,
// InsecureSkipVerify) at dial time, nil for the system roots. They apply to an
// https proxy as well as to the target.
func (h *Hello) DialFunc(proxy func(*http.Request) (*url.URL, error), verify func() *tls.Config) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var config *tls.Config
		if verify != nil {
			config = verify()
		}
		return h.dial(ctx, network, addr, proxy, config)
	}
}

// Dial opens a connection to addr and completes a handshake that presents the
// captured hello. The SNI is addr's host, not the one the capture carried.
func (h *Hello) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return h.dial(ctx, network, addr, nil, nil)
}

func (h *Hello) dial(ctx context.Context, network, addr string, proxy func(*http.Request) (*url.URL, error), verify *tls.Config) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("tlsreplay: split %q: %w", addr, err)
	}
	raw, err := dialTunnel(ctx, network, addr, proxy, verify)
	if err != nil {
		return nil, err
	}
	config := &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if verify != nil {
		config.RootCAs = verify.RootCAs
		config.InsecureSkipVerify = verify.InsecureSkipVerify
	}
	// A spec carries per-handshake state (utls writes the key shares it
	// generates back into it), so each connection gets its own.
	spec, err := h.fingerprint()
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	if h.sessions != nil {
		// A resumed hello is the full one with pre_shared_key appended last. The
		// extension writes nothing until the cache holds a ticket for this
		// server, so a first connection is the full hello unchanged.
		config.ClientSessionCache = h.sessions
		config.OmitEmptyPsk = true
		spec.Extensions = append(spec.Extensions, &utls.UtlsPreSharedKeyExtension{})
	}
	uconn := utls.UClient(raw, config, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("tlsreplay: apply captured ClientHello: %w", err)
	}
	// The capture carries the original target's SNI; point it at the host
	// actually being dialed. An IP literal has no SNI, and utls drops the
	// name from the config when asked to set one, which leaves nothing to
	// verify the certificate against, so the config's own ServerName stands.
	if net.ParseIP(host) == nil {
		uconn.SetSNI(host)
	}
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := uconn.HandshakeContext(hsCtx); err != nil {
		_ = uconn.Close()
		return nil, fmt.Errorf("tlsreplay: handshake: %w", err)
	}
	state := uconn.ConnectionState()
	return &spoofedConn{Conn: uconn, state: tls.ConnectionState{
		Version:            state.Version,
		HandshakeComplete:  state.HandshakeComplete,
		DidResume:          state.DidResume,
		CipherSuite:        state.CipherSuite,
		NegotiatedProtocol: state.NegotiatedProtocol,
		ServerName:         state.ServerName,
	}}, nil
}

// dialTunnel returns a connection to addr: direct, or through the proxy that
// proxy selects for an https request to addr, whichever of http, https, socks5
// and socks5h it is. verify applies to an https proxy's certificate.
func dialTunnel(ctx context.Context, network, addr string, proxy func(*http.Request) (*url.URL, error), verify *tls.Config) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	var proxyURL *url.URL
	if proxy != nil {
		var err error
		proxyURL, err = proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
		if err != nil {
			return nil, fmt.Errorf("tlsreplay: proxy for %s: %w", addr, err)
		}
	}
	return proxydial.Tunnel(ctx, dialer, network, addr, proxyURL, verify)
}

// spoofedConn exposes the utls handshake result to net/http as a
// crypto/tls.ConnectionState (net/http type-asserts the dialed connection for
// it; utls carries its own copy of the type). Only net.Conn methods are
// promoted, so net/http does not try to re-run the handshake.
type spoofedConn struct {
	net.Conn
	state tls.ConnectionState
}

func (c *spoofedConn) ConnectionState() tls.ConnectionState { return c.state }
