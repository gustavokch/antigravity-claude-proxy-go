package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"antigravity-go-proxy/internal/proxydial"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http/httpproxy"
)

// codexDialTimeout and codexHandshakeTimeout bound the two local steps of a
// Codex dial. With DialTLSContext set, net/http applies neither its dial nor its
// TLS handshake timeout, and a streamed request's context has no deadline.
const (
	codexDialTimeout      = 30 * time.Second
	codexHandshakeTimeout = 10 * time.Second
)

// Codex returns the transport for the ChatGPT Codex backend
// (chatgpt.com/backend-api/codex). It is the Codex-only exception to this
// repo's rule that outbound TLS uses an empty tls.Config, recorded in ADR-0005:
// chatgpt.com sits behind a browser fingerprint check, and the reference
// client the gateway copies (CLIProxyAPI) presents Chrome's ClientHello there,
// so every https connection here is dialed with utls's HelloChrome_Auto.
//
// The protocol follows ALPN, as it does for Chrome: a connection on which the
// server selects h2 speaks HTTP/2 and is pooled for concurrent requests, one
// that selects http/1.1 (or no ALPN, as some TLS-inspecting proxies leave it)
// speaks HTTP/1.1 with keep-alive. net/http makes that choice per connection
// from the ConnectionState the dialer reports.
//
// HTTPS_PROXY is honoured, and ALL_PROXY when HTTPS_PROXY is unset, with
// NO_PROXY applied to both; the dialer opens the tunnel through proxydial
// (http, https, socks5) and sends the hello inside it. The response-header
// timeout is the one Upstream sets. Every call returns a transport with its own
// connection pool.
func Codex() http.RoundTripper {
	tr := Upstream()
	proxy := codexProxy()
	// net/http would dial an https target behind a proxy itself and run
	// crypto/tls over the tunnel, which would put Go's hello on the wire; the
	// dialer below tunnels instead. A plain-http base URL has no hello to
	// keep and goes through the proxy as usual.
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme == "https" {
			return nil, nil
		}
		return proxy(req)
	}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	tr.Protocols.SetHTTP2(true)
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialChrome(ctx, network, addr, proxy, tr.TLSClientConfig)
	}
	return tr
}

// codexProxy selects the proxy for a Codex request from the environment. It is
// http.ProxyFromEnvironment's rule (HTTPS_PROXY, HTTP_PROXY, NO_PROXY, either
// case) with ALL_PROXY as the https proxy when HTTPS_PROXY is unset, the
// fallback curl and most CLI tools apply.
func codexProxy() func(*http.Request) (*url.URL, error) {
	cfg := httpproxy.FromEnvironment()
	if cfg.HTTPSProxy == "" {
		cfg.HTTPSProxy = getenvAny("ALL_PROXY", "all_proxy")
	}
	fn := cfg.ProxyFunc()
	return func(req *http.Request) (*url.URL, error) { return fn(req.URL) }
}

func getenvAny(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// dialChrome opens a TLS connection to addr presenting Chrome's ClientHello,
// through the proxy proxy selects for it. verify supplies RootCAs and
// InsecureSkipVerify (nil: the system roots); they apply to an https proxy as
// well as to the target.
func dialChrome(ctx context.Context, network, addr string, proxy func(*http.Request) (*url.URL, error), verify *tls.Config) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("codex transport: split %q: %w", addr, err)
	}
	proxyURL, err := proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil {
		return nil, fmt.Errorf("codex transport: proxy for %s: %w", addr, err)
	}
	dialer := &net.Dialer{Timeout: codexDialTimeout, KeepAlive: 30 * time.Second}
	raw, err := proxydial.Tunnel(ctx, dialer, network, addr, proxyURL, verify)
	if err != nil {
		return nil, err
	}
	config := &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if verify != nil {
		config.RootCAs = verify.RootCAs
		config.InsecureSkipVerify = verify.InsecureSkipVerify
	}
	conn := utls.UClient(raw, config, utls.HelloChrome_Auto)
	hsCtx, cancel := context.WithTimeout(ctx, codexHandshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hsCtx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("codex transport: TLS handshake: %w", err)
	}
	state := conn.ConnectionState()
	return &chromeConn{Conn: conn, state: tls.ConnectionState{
		Version:                    state.Version,
		HandshakeComplete:          state.HandshakeComplete,
		DidResume:                  state.DidResume,
		CipherSuite:                state.CipherSuite,
		NegotiatedProtocol:         state.NegotiatedProtocol,
		NegotiatedProtocolIsMutual: true,
		ServerName:                 state.ServerName,
		PeerCertificates:           state.PeerCertificates,
		VerifiedChains:             state.VerifiedChains,
	}}, nil
}

// chromeConn reports the utls handshake to net/http as a crypto/tls
// ConnectionState: net/http reads NegotiatedProtocol from it to choose HTTP/2
// or HTTP/1.1 for the connection, and utls's own state type does not satisfy
// that interface. Only net.Conn methods are promoted, so net/http does not try
// to run the handshake again.
type chromeConn struct {
	net.Conn
	state tls.ConnectionState
}

func (c *chromeConn) ConnectionState() tls.ConnectionState { return c.state }
