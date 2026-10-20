// Package proxydial opens connections through the proxies a user's environment
// can name: HTTP and HTTPS proxies, and SOCKS5.
//
// net/http reaches a proxy itself, but only for its own dialer. A caller that
// supplies its own, to replay a ClientHello (tlsreplay) or to write requests
// byte by byte (httpwire), gets no proxy support from net/http and has to open
// the route to the target itself. http.ProxyFromEnvironment returns any of the
// four schemes, and a caller that handled one and refused the rest would break
// for users whose proxy it was.
//
// The hop to the proxy is an ordinary connection to the user's own proxy: an
// HTTPS proxy is reached with crypto/tls, which has no fingerprint to keep. The
// hello the upstream sees is the one the caller sends through the tunnel.
package proxydial

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// connectTimeout bounds the CONNECT exchange when the context has no earlier
// deadline, and handshakeTimeout bounds TLS with an HTTPS proxy. A proxy that
// accepts a connection and then says nothing would otherwise hold the request.
const (
	connectTimeout   = 10 * time.Second
	handshakeTimeout = 10 * time.Second
)

// Authorization returns the Proxy-Authorization value for proxy's credentials:
// Basic, as net/http sends it. It is empty when the URL carries no user info.
func Authorization(proxy *url.URL) string {
	if proxy.User == nil {
		return ""
	}
	password, _ := proxy.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password))
}

// proxyAddr returns the proxy's host:port with its scheme's default port filled
// in.
func proxyAddr(proxy *url.URL) string {
	port := proxy.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[proxy.Scheme]
	}
	return net.JoinHostPort(proxy.Hostname(), port)
}

// Tunnel returns a connection that reaches addr through proxy, or directly when
// proxy is nil. The caller speaks to addr over it as if it had dialed addr, so
// it suits a target whose protocol starts with the client (TLS).
//
//   - socks5 and socks5h relay through the proxy, which resolves the name;
//   - http and https open a CONNECT tunnel, over TCP and over TLS to the proxy
//     respectively. verify gives the RootCAs and InsecureSkipVerify for an https
//     proxy, as net/http applies a transport's TLSClientConfig to both hops; nil
//     verifies against the system roots.
//
// Any other scheme is an error. ctx bounds the whole of it, the dial, the TLS
// handshake with the proxy and the proxy's answer, and ends it when cancelled; the
// error is then ctx.Err().
func Tunnel(ctx context.Context, d *net.Dialer, network, addr string, proxy *url.URL, verify *tls.Config) (net.Conn, error) {
	if proxy == nil {
		return d.DialContext(ctx, network, addr)
	}
	var (
		conn net.Conn
		err  error
	)
	switch proxy.Scheme {
	case "socks5", "socks5h":
		conn, err = dialSOCKS5(ctx, d, network, addr, proxy)
	case "http", "https":
		conn, err = dialConnect(ctx, d, network, addr, proxy, verify)
	default:
		return nil, fmt.Errorf("proxydial: proxy scheme %q is not supported (http, https, socks5, socks5h)", proxy.Scheme)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return conn, nil
}

// DialProxy returns a connection to an HTTP or HTTPS proxy itself, for a request
// addressed to the proxy (an http:// target is sent to it in absolute form). The
// proxy's credentials are the caller's to send, see Authorization. verify is as
// for Tunnel.
func DialProxy(ctx context.Context, d *net.Dialer, network string, proxy *url.URL, verify *tls.Config) (net.Conn, error) {
	switch proxy.Scheme {
	case "http":
		return d.DialContext(ctx, network, proxyAddr(proxy))
	case "https":
		raw, err := d.DialContext(ctx, network, proxyAddr(proxy))
		if err != nil {
			return nil, err
		}
		config := &tls.Config{ServerName: proxy.Hostname()}
		if verify != nil {
			config.RootCAs = verify.RootCAs
			config.InsecureSkipVerify = verify.InsecureSkipVerify
		}
		hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
		defer cancel()
		conn := tls.Client(raw, config)
		if err := conn.HandshakeContext(hsCtx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("proxydial: TLS handshake with the proxy: %w", err)
		}
		return conn, nil
	default:
		return nil, fmt.Errorf("proxydial: %q is not an HTTP proxy scheme (http, https)", proxy.Scheme)
	}
}

func dialSOCKS5(ctx context.Context, d *net.Dialer, network, addr string, proxy *url.URL) (net.Conn, error) {
	var auth *xproxy.Auth
	if user := proxy.User; user != nil {
		password, _ := user.Password()
		auth = &xproxy.Auth{User: user.Username(), Password: password}
	}
	dialer, err := xproxy.SOCKS5(network, proxyAddr(proxy), auth, d)
	if err != nil {
		return nil, fmt.Errorf("proxydial: SOCKS5 proxy: %w", err)
	}
	contextual, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return nil, errors.New("proxydial: the SOCKS5 dialer cannot be bound by a context")
	}
	return contextual.DialContext(ctx, network, addr)
}

func dialConnect(ctx context.Context, d *net.Dialer, network, addr string, proxy *url.URL, verify *tls.Config) (net.Conn, error) {
	conn, err := DialProxy(ctx, d, network, proxy, verify)
	if err != nil {
		return nil, err
	}
	if err := connect(ctx, conn, addr, proxy); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// connect asks the proxy on conn for a tunnel to addr. The exchange runs under
// the earlier of the context's deadline and connectTimeout, and cancelling the
// context aborts a read or write that is blocked on the proxy.
func connect(ctx context.Context, conn net.Conn, addr string, proxy *url.URL) error {
	deadline := time.Now().Add(connectTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	abort := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	err := exchange(conn, addr, proxy)
	if !abort() {
		// The context ended while the exchange ran, or just after it: the
		// callback has spoiled the connection's deadline, so it is not usable
		// whatever the exchange made of it.
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return conn.SetDeadline(time.Time{})
}

func exchange(conn net.Conn, addr string, proxy *url.URL) error {
	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if auth := Authorization(proxy); auth != "" {
		request.Header.Set("Proxy-Authorization", auth)
	}
	if err := request.Write(conn); err != nil {
		return fmt.Errorf("proxydial: write CONNECT: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, request)
	if err != nil {
		return fmt.Errorf("proxydial: read CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxydial: proxy refused CONNECT %s: %s", addr, resp.Status)
	}
	// A TLS server never speaks first, so nothing may sit in the buffer.
	if reader.Buffered() != 0 {
		return errors.New("proxydial: proxy sent data after the CONNECT response")
	}
	return nil
}
