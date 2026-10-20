// Package proxytest holds the in-process proxies and backends that tests of the
// proxy-aware dialers share: an HTTP or HTTPS proxy, a SOCKS5 proxy, a backend
// that echoes, and a server that never answers.
package proxytest

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

// EchoBackend serves TCP on a loopback port and answers each line with "echo: "
// and the line. It stands for the upstream a tunnel leads to.
func EchoBackend(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = io.WriteString(c, "echo: "+line)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// SilentServer accepts connections on a loopback port and never answers. It
// returns the address.
func SilentServer(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

// Trusting returns verification settings that accept the certificate of a TLS
// test server, and nil for a plain server. A dialer given them verifies the
// server as it would a real one; only the trust differs.
func Trusting(server *httptest.Server) *tls.Config {
	if server.Certificate() == nil {
		return nil
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	return &tls.Config{RootCAs: pool}
}

// Connect is one CONNECT an HTTPProxy received.
type Connect struct {
	Target string // the host:port asked for
	Auth   string // the Proxy-Authorization header
}

// Forwarded is one request addressed to an HTTPProxy as a proxy, not a CONNECT.
type Forwarded struct {
	Method string
	Target string // the request target as the client sent it: an absolute URL for a proxy request
	Header http.Header
}

// HTTPProxy is an HTTP or HTTPS proxy. A CONNECT is tunnelled to a fixed backend
// whatever its target. Any other request is recorded and answered with its own
// request target as the body; nothing is forwarded.
type HTTPProxy struct {
	*httptest.Server
	mu        sync.Mutex
	connects  []Connect
	forwarded []Forwarded
}

// NewHTTPProxy starts a proxy, over TLS when useTLS, that tunnels CONNECT to
// backend.
func NewHTTPProxy(t testing.TB, useTLS bool, backend string) *HTTPProxy {
	t.Helper()
	p := &HTTPProxy{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			p.mu.Lock()
			p.forwarded = append(p.forwarded, Forwarded{Method: r.Method, Target: r.RequestURI, Header: r.Header.Clone()})
			p.mu.Unlock()
			_, _ = io.WriteString(w, r.RequestURI)
			return
		}
		p.mu.Lock()
		p.connects = append(p.connects, Connect{Target: r.Host, Auth: r.Header.Get("Proxy-Authorization")})
		p.mu.Unlock()
		upstream, err := net.Dial("tcp", backend)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
		go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
	})
	if useTLS {
		p.Server = httptest.NewTLSServer(handler)
	} else {
		p.Server = httptest.NewServer(handler)
	}
	t.Cleanup(p.Close)
	return p
}

// ProxyURL returns the proxy's URL as an environment would name it, with
// credentials when user is not nil.
func (p *HTTPProxy) ProxyURL(user *url.Userinfo) *url.URL {
	u, err := url.Parse(p.Server.URL)
	if err != nil {
		panic(err)
	}
	u.User = user
	return u
}

// Connects returns the CONNECTs received so far.
func (p *HTTPProxy) Connects() []Connect {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Connect(nil), p.connects...)
}

// Forwarded returns the requests received so far that were not CONNECTs.
func (p *HTTPProxy) Forwarded() []Forwarded {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Forwarded(nil), p.forwarded...)
}

// SOCKS5Request is one request a SOCKS5 proxy received.
type SOCKS5Request struct {
	Target string // host:port as the client sent them: a name is not resolved by the client
	Cred   string // user:password, empty when none was offered
}

// SOCKS5 is a SOCKS5 proxy (RFC 1928, with RFC 1929 passwords) that connects
// every request to a fixed backend, whatever its target.
type SOCKS5 struct {
	Addr string

	requireAuth bool
	mu          sync.Mutex
	requests    []SOCKS5Request
}

// NewSOCKS5 starts a proxy that connects to backend. With requireAuth it accepts
// only the username/password method.
func NewSOCKS5(t testing.TB, requireAuth bool, backend string) *SOCKS5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &SOCKS5{Addr: ln.Addr().String(), requireAuth: requireAuth}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, backend)
		}
	}()
	return s
}

// Requests returns the requests received so far.
func (s *SOCKS5) Requests() []SOCKS5Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SOCKS5Request(nil), s.requests...)
}

func (s *SOCKS5) serve(c net.Conn, backend string) {
	defer c.Close()
	br := bufio.NewReader(c)
	readN := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			return nil
		}
		return b
	}
	greeting := readN(2) // version, method count
	if greeting == nil {
		return
	}
	methods := readN(int(greeting[1]))
	cred := ""
	if s.requireAuth {
		if !bytes.Contains(methods, []byte{0x02}) {
			_, _ = c.Write([]byte{5, 0xff})
			return
		}
		_, _ = c.Write([]byte{5, 2})
		head := readN(2) // version, user length
		if head == nil {
			return
		}
		user := readN(int(head[1]))
		plen := readN(1)
		if user == nil || plen == nil {
			return
		}
		password := readN(int(plen[0]))
		cred = string(user) + ":" + string(password)
		_, _ = c.Write([]byte{1, 0})
	} else {
		_, _ = c.Write([]byte{5, 0})
	}
	request := readN(4) // version, command, reserved, address type
	if request == nil {
		return
	}
	var host string
	switch request[3] {
	case 1:
		host = net.IP(readN(4)).String()
	case 3:
		host = string(readN(int(readN(1)[0])))
	case 4:
		host = net.IP(readN(16)).String()
	}
	port := readN(2)
	if port == nil {
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, SOCKS5Request{
		Target: net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1]))),
		Cred:   cred,
	})
	s.mu.Unlock()

	upstream, err := net.Dial("tcp", backend)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go func() { _, _ = io.Copy(upstream, br); _ = upstream.Close() }()
	_, _ = io.Copy(c, upstream)
}
