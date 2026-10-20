package proxydial

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/proxydial/proxytest"
)

func dialer() *net.Dialer { return &net.Dialer{Timeout: 5 * time.Second} }

// echoes fails the test unless conn is a connection to proxytest.EchoBackend.
func echoes(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || got != "echo: ping\n" {
		t.Fatalf("read through the tunnel = %q, %v; want the backend's echo", got, err)
	}
}

// An HTTP proxy and an HTTPS proxy both carry the tunnel: the target is the one
// asked for, and the proxy's credentials travel as Basic.
func TestTunnelThroughAnHTTPOrHTTPSProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		useTLS   bool
		user     *url.Userinfo
		wantAuth string
	}{
		{"http with credentials", false, url.UserPassword("user", "pass"), "Basic dXNlcjpwYXNz"},
		{"https with credentials", true, url.UserPassword("user", "pass"), "Basic dXNlcjpwYXNz"},
		{"http without credentials", false, nil, ""},
		{"https without credentials", true, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := proxytest.NewHTTPProxy(t, tc.useTLS, proxytest.EchoBackend(t))
			conn, err := Tunnel(context.Background(), dialer(), "tcp", "upstream.invalid:443", proxy.ProxyURL(tc.user), proxytest.Trusting(proxy.Server))
			if err != nil {
				t.Fatalf("Tunnel: %v", err)
			}
			defer conn.Close()
			echoes(t, conn)

			connects := proxy.Connects()
			if len(connects) != 1 || connects[0].Target != "upstream.invalid:443" {
				t.Errorf("CONNECTs = %+v, want one for upstream.invalid:443", connects)
			}
			if connects[0].Auth != tc.wantAuth {
				t.Errorf("Proxy-Authorization = %q, want %q", connects[0].Auth, tc.wantAuth)
			}
		})
	}
}

// The hop to an HTTPS proxy is verified: against the system roots unless the
// caller passes its own, which is what lets the tests above trust a self-signed
// certificate without loosening anything else.
func TestHTTPSProxyCertificateIsVerified(t *testing.T) {
	proxy := proxytest.NewHTTPProxy(t, true, proxytest.EchoBackend(t))
	if conn, err := Tunnel(context.Background(), dialer(), "tcp", "upstream.invalid:443", proxy.ProxyURL(nil), nil); err == nil {
		_ = conn.Close()
		t.Fatal("a proxy with an untrusted certificate was accepted")
	}
	if got := proxy.Connects(); len(got) != 0 {
		t.Errorf("a CONNECT reached the proxy before its certificate was trusted: %v", got)
	}
}

// A SOCKS5 proxy relays the connection, and the name is the proxy's to resolve:
// the target reaches it as the host name, not as an address looked up here.
func TestTunnelThroughSOCKS5(t *testing.T) {
	for _, tc := range []struct {
		scheme string
		auth   bool
	}{
		{"socks5", false}, {"socks5h", false}, {"socks5", true}, {"socks5h", true},
	} {
		t.Run(fmt.Sprintf("%s auth=%v", tc.scheme, tc.auth), func(t *testing.T) {
			socks := proxytest.NewSOCKS5(t, tc.auth, proxytest.EchoBackend(t))
			proxyURL := &url.URL{Scheme: tc.scheme, Host: socks.Addr}
			wantCred := ""
			if tc.auth {
				proxyURL.User = url.UserPassword("user", "pass")
				wantCred = "user:pass"
			}
			conn, err := Tunnel(context.Background(), dialer(), "tcp", "upstream.invalid:443", proxyURL, nil)
			if err != nil {
				t.Fatalf("Tunnel: %v", err)
			}
			defer conn.Close()
			echoes(t, conn)

			requests := socks.Requests()
			if len(requests) != 1 || requests[0].Target != "upstream.invalid:443" {
				t.Errorf("SOCKS requests = %+v, want one for upstream.invalid:443", requests)
			}
			if requests[0].Cred != wantCred {
				t.Errorf("SOCKS credentials = %q, want %q", requests[0].Cred, wantCred)
			}
		})
	}
}

func TestTunnelWithoutAProxyDialsTheTarget(t *testing.T) {
	conn, err := Tunnel(context.Background(), dialer(), "tcp", proxytest.EchoBackend(t), nil, nil)
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}
	defer conn.Close()
	echoes(t, conn)
}

func TestAnUnknownProxySchemeIsAnError(t *testing.T) {
	proxyURL, _ := url.Parse("ftp://127.0.0.1:21")
	conn, err := Tunnel(context.Background(), dialer(), "tcp", "upstream.invalid:443", proxyURL, nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("an ftp proxy was accepted")
	}
	if !strings.Contains(err.Error(), "ftp") {
		t.Errorf("error %q does not name the scheme", err)
	}

	socksURL, _ := url.Parse("socks5://127.0.0.1:1080")
	if conn, err := DialProxy(context.Background(), dialer(), "tcp", socksURL, nil); err == nil {
		_ = conn.Close()
		t.Error("DialProxy accepted a SOCKS proxy: it reaches only HTTP and HTTPS proxies")
	}
}

// A proxy that refuses the tunnel is an error, not a fall back to a direct
// connection.
func TestProxyRefusalIsAnError(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(refuse.Close)
	proxyURL, _ := url.Parse(refuse.URL)
	if conn, err := Tunnel(context.Background(), dialer(), "tcp", "upstream.invalid:443", proxyURL, nil); err == nil {
		_ = conn.Close()
		t.Error("a refused CONNECT produced a connection")
	}
}

// What follows a CONNECT reply on the wire is the target's, and a TLS server
// never speaks first, so bytes there are not from one.
func TestBytesAfterTheConnectReplyAreAnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = http.ReadRequest(bufio.NewReader(c))
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\nunsolicited"))
		time.Sleep(200 * time.Millisecond)
	}()
	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	if conn, err := Tunnel(context.Background(), dialer(), "tcp", "upstream.invalid:443", proxyURL, nil); err == nil {
		_ = conn.Close()
		t.Error("data after the CONNECT reply was accepted")
	}
}

// A proxy that accepts and never answers must not hold a request past its
// context, whichever protocol the proxy speaks.
func TestContextCancelEndsAnUnansweredProxy(t *testing.T) {
	for _, scheme := range []string{"http", "https", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			proxyURL := &url.URL{Scheme: scheme, Host: proxytest.SilentServer(t)}
			ctx, cancel := context.WithCancel(context.Background())
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()

			start := time.Now()
			conn, err := Tunnel(ctx, dialer(), "tcp", "upstream.invalid:443", proxyURL, nil)
			if err == nil {
				_ = conn.Close()
				t.Fatal("a silent proxy produced a tunnel")
			}
			if elapsed := time.Since(start); elapsed > 350*time.Millisecond {
				t.Errorf("Tunnel took %v after a cancel at 50ms", elapsed)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want the context's", err)
			}
		})
	}
}

// A deadline on the context bounds the tunnel as well, well inside the cap that
// applies without one.
func TestContextDeadlineBoundsAnUnansweredProxy(t *testing.T) {
	proxyURL := &url.URL{Scheme: "http", Host: proxytest.SilentServer(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	conn, err := Tunnel(ctx, dialer(), "tcp", "upstream.invalid:443", proxyURL, nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a silent proxy produced a tunnel")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Tunnel took %v for a 200ms deadline", elapsed)
	}
}

// DialProxy is the connection for a request sent to the proxy itself, the form
// an http:// target takes: the request target arrives as an absolute URL.
func TestDialProxyReachesTheProxyItself(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", useTLS), func(t *testing.T) {
			proxy := proxytest.NewHTTPProxy(t, useTLS, proxytest.EchoBackend(t))
			conn, err := DialProxy(context.Background(), dialer(), "tcp", proxy.ProxyURL(nil), proxytest.Trusting(proxy.Server))
			if err != nil {
				t.Fatalf("DialProxy: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(conn, "GET http://upstream.invalid/v1/messages?x=1 HTTP/1.1\r\nHost: upstream.invalid\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != "http://upstream.invalid/v1/messages?x=1" {
				t.Errorf("the proxy saw the request target %q, want the absolute URL", body)
			}
		})
	}
}

func TestAuthorization(t *testing.T) {
	// The URLs are built rather than written out: a literal user:password URL is
	// what the repository's secret scan exists to catch.
	host := "proxy.example:3128"
	for _, tc := range []struct {
		user *url.Userinfo
		want string
	}{
		{nil, ""},
		{url.UserPassword("user", "pass"), "Basic dXNlcjpwYXNz"},
		{url.User("user"), "Basic dXNlcjo="},
		{url.UserPassword("us@er", "p:ss"), "Basic dXNAZXI6cDpzcw=="},
	} {
		u := &url.URL{Scheme: "http", User: tc.user, Host: host}
		if got := Authorization(u); got != tc.want {
			t.Errorf("Authorization(%v) = %q, want %q", tc.user, got, tc.want)
		}
	}
}
