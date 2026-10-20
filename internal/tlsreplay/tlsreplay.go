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
package tlsreplay

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

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
// base.Proxy is moved into the dialer. net/http ignores DialTLSContext for an
// https target behind a proxy: it dials the proxy in plain text and runs
// crypto/tls over the tunnel itself, so the hello on the wire would be Go's
// own and nothing would say so. Here the dialer opens the CONNECT tunnel and
// replays the hello over it.
func (h *Hello) Transport(base *http.Transport) *http.Transport {
	proxy := base.Proxy
	base.Proxy = nil
	base.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return h.dial(ctx, network, addr, proxy, base.TLSClientConfig)
	}
	return base
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
	raw, err := dialTunnel(ctx, network, addr, proxy)
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

// dialTunnel returns a connection to addr: direct, or through the HTTP proxy
// that proxy selects for an https request to addr.
func dialTunnel(ctx context.Context, network, addr string, proxy func(*http.Request) (*url.URL, error)) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	var proxyURL *url.URL
	if proxy != nil {
		var err error
		proxyURL, err = proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
		if err != nil {
			return nil, fmt.Errorf("tlsreplay: proxy for %s: %w", addr, err)
		}
	}
	if proxyURL == nil {
		return dialer.DialContext(ctx, network, addr)
	}
	if proxyURL.Scheme != "http" {
		return nil, fmt.Errorf("tlsreplay: proxy scheme %q is not supported, only http", proxyURL.Scheme)
	}
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "80")
	}
	conn, err := dialer.DialContext(ctx, network, proxyAddr)
	if err != nil {
		return nil, err
	}
	// The tunnel handshake shares the dial's deadline, so a proxy that accepts
	// and then stays silent cannot hold the request.
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	connect := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if user := proxyURL.User; user != nil {
		password, _ := user.Password()
		connect.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	if err := connect.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tlsreplay: write CONNECT: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, connect)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tlsreplay: read CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("tlsreplay: proxy refused CONNECT %s: %s", addr, resp.Status)
	}
	// A TLS server never speaks first, so nothing may sit in the buffer.
	if reader.Buffered() != 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("tlsreplay: proxy sent data after the CONNECT response")
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
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
