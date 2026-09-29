package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream is a raw TLS server standing in for api.anthropic.com. It
// records every request head exactly as received and answers via respond.
type fakeUpstream struct {
	addr    string
	accepts atomic.Int64
	heads   chan string
	bodies  chan string
}

func startFakeUpstream(t *testing.T, respond func(head string, body []byte, conn net.Conn)) (*fakeUpstream, *x509.CertPool) {
	t.Helper()
	upstreamCA := newTestCA(t) // an unrelated CA that "signs" the fake Anthropic cert
	leaf, err := upstreamCA.LeafFor("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*leaf}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{addr: l.Addr().String(), heads: make(chan string, 32), bodies: make(chan string, 32)}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			f.accepts.Add(1)
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					raw, err := readHead(br)
					if err != nil {
						return
					}
					head, err := parseRequestHead(raw)
					if err != nil {
						return
					}
					var body bytes.Buffer
					var decoded bytes.Buffer
					if err := copyBody(&body, br, head.Framing, head.ContentLength, &decoded); err != nil {
						return
					}
					f.heads <- string(raw)
					f.bodies <- decoded.String()
					respond(string(raw), decoded.Bytes(), conn)
				}
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(upstreamCA.cert)
	return f, pool
}

func jsonResponse(body string) string {
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

func okJSON(body string) func(string, []byte, net.Conn) {
	return func(_ string, _ []byte, conn net.Conn) { io.WriteString(conn, jsonResponse(body)) }
}

type proxyHarness struct {
	srv      *Server
	ca       *CA
	registry *Registry
	addr     string
	logs     *bytes.Buffer
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// startProxy runs a Server on a loopback port. redirect maps a dialled
// "host:port" to a local address, so the tests never touch the network.
func startProxy(t *testing.T, redirect map[string]string, upstreamRoots *x509.CertPool) *proxyHarness {
	t.Helper()
	ca := newTestCA(t)
	registry := NewRegistry(100, time.Hour, nil)
	logs := &bytes.Buffer{}
	srv, err := New(Options{
		CA: ca, Registry: registry,
		Logger: slog.New(slog.NewTextHandler(&syncWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			if target, ok := redirect[addr]; ok {
				addr = target
			}
			return d.DialContext(ctx, network, addr)
		},
		UpstreamTLS: &tls.Config{RootCAs: upstreamRoots},
		IdleTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &proxyHarness{srv: srv, ca: ca, registry: registry, addr: l.Addr().String(), logs: logs}
}

func (h *proxyHarness) connect(t *testing.T, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	raw, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(raw)
	head, err := readHead(br)
	if err != nil || !strings.Contains(string(head), " 200 ") {
		t.Fatalf("CONNECT %s: head=%q err=%v", target, head, err)
	}
	return raw, br
}

// connectTLS does CONNECT host:443 and completes TLS as the CLI would,
// trusting only the proxy CA.
func (h *proxyHarness) connectTLS(t *testing.T, host string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	raw, br := h.connect(t, host+":443")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.ca.CertPEM())
	tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{RootCAs: pool, ServerName: host, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if tc.ConnectionState().NegotiatedProtocol != "http/1.1" {
		t.Fatalf("ALPN = %q, want http/1.1", tc.ConnectionState().NegotiatedProtocol)
	}
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	return tc, bufio.NewReader(tc)
}

// readResponse reads one full response (head and body) as received.
func readResponse(t *testing.T, br *bufio.Reader, method string) (*msgHead, string) {
	t.Helper()
	raw, err := readHead(br)
	if err != nil {
		t.Fatalf("read response head: %v", err)
	}
	head, err := parseResponseHead(raw, method)
	if err != nil {
		t.Fatal(err)
	}
	var wire, payload bytes.Buffer
	if err := copyBody(&wire, br, head.Framing, head.ContentLength, &payload); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return head, payload.String()
}

func TestTerminatedRequestPreservesHeaderCaseOrderAndBody(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{"ok":true}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")

	req := "POST /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nX-Stainless-OS: MacOS\r\nx-app: cli\r\nAccept: application/json\r\nContent-Length: 2\r\n\r\nhi"
	io.WriteString(conn, req)
	resp, body := readResponse(t, br, "POST")

	if resp.Status != 200 || body != `{"ok":true}` {
		t.Fatalf("status=%d body=%q", resp.Status, body)
	}
	if got := <-up.heads; got != strings.TrimSuffix(req, "hi") {
		t.Fatalf("upstream head not byte-exact:\n got %q\nwant %q", got, strings.TrimSuffix(req, "hi"))
	}
	if got := <-up.bodies; got != "hi" {
		t.Fatalf("upstream body = %q", got)
	}
}

func TestChunkedRequestBodyForwarded(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n")
	readResponse(t, br, "POST")
	<-up.heads
	if got := <-up.bodies; got != "hello world" {
		t.Fatalf("upstream body = %q", got)
	}
}

func TestSSEStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	up, roots := startFakeUpstream(t, func(_ string, _ []byte, conn net.Conn) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		io.WriteString(conn, "a\r\nevent: a\n\n\r\n")
		<-release
		io.WriteString(conn, "a\r\nevent: b\n\n\r\n0\r\n\r\n")
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /v1/code/sessions/session_01ABCDEFGHJK/events/stream HTTP/1.1\r\nHost: a\r\n\r\n")

	if _, err := readHead(br); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	size, err := br.ReadString('\n') // first chunk arrives while upstream is still blocked
	if err != nil || size != "a\r\n" {
		t.Fatalf("first chunk did not stream through: %q %v", size, err)
	}
	payload := make([]byte, 12)
	if _, err := io.ReadFull(br, payload); err != nil || !strings.HasPrefix(string(payload), "event: a") {
		t.Fatalf("payload = %q err=%v", payload, err)
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "event: b") || !strings.HasSuffix(string(rest), "0\r\n\r\n") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestKeepAliveReusesUpstreamConnection(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	for i := 0; i < 3; i++ {
		io.WriteString(conn, "GET /a HTTP/1.1\r\nHost: a\r\n\r\n")
		readResponse(t, br, "GET")
	}
	if n := up.accepts.Load(); n != 1 {
		t.Fatalf("upstream accepts = %d, want 1 (connection should be reused)", n)
	}
}

func TestStaleUpstreamConnectionIsRedialed(t *testing.T) {
	up, roots := startFakeUpstream(t, func(_ string, _ []byte, conn net.Conn) {
		io.WriteString(conn, jsonResponse(`{}`))
		time.AfterFunc(10*time.Millisecond, func() { conn.Close() }) // server drops the idle connection
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /a HTTP/1.1\r\nHost: a\r\n\r\n")
	readResponse(t, br, "GET")
	time.Sleep(150 * time.Millisecond)
	io.WriteString(conn, "GET /b HTTP/1.1\r\nHost: a\r\n\r\n")
	if resp, _ := readResponse(t, br, "GET"); resp.Status != 200 {
		t.Fatalf("second request status = %d", resp.Status)
	}
	if n := up.accepts.Load(); n != 2 {
		t.Fatalf("upstream accepts = %d, want 2 (redial after idle close)", n)
	}
}

func TestBlindTunnelForOtherHosts(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err == nil {
			io.Copy(conn, conn)
			conn.Close()
		}
	}()
	h := startProxy(t, map[string]string{"example.org:443": echo.Addr().String()}, nil)
	raw, br := h.connect(t, "example.org:443")
	io.WriteString(raw, "plain bytes, no TLS")
	got := make([]byte, len("plain bytes, no TLS"))
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "plain bytes, no TLS" {
		t.Fatalf("tunnel echo = %q err=%v", got, err)
	}
	if s := h.srv.Stats(); s.Tunnelled != 1 || s.Terminated != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestObserverRegistersCloudSession(t *testing.T) {
	up, roots := startFakeUpstream(t, func(head string, _ []byte, conn net.Conn) {
		if strings.HasPrefix(head, "POST /v1/sessions ") {
			io.WriteString(conn, jsonResponse(`{"id":"session_01ABCDEFGHJK","session_status":"running","environment_kind":"anthropic_cloud","configured_model":"claude-opus-5-5","title":"secret title"}`))
			return
		}
		io.WriteString(conn, jsonResponse(`{"results":[]}`))
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "POST /v1/sessions HTTP/1.1\r\nHost: a\r\nContent-Length: 2\r\n\r\n{}")
	readResponse(t, br, "POST")
	io.WriteString(conn, "POST /v1/code/sessions/session_01ABCDEFGHJK/events HTTP/1.1\r\nHost: a\r\nContent-Length: 2\r\n\r\n{}")
	readResponse(t, br, "POST")

	list := h.registry.List()
	if len(list) != 1 {
		t.Fatalf("sessions = %+v", list)
	}
	s := list[0]
	if s.ID != HashID("session_01ABCDEFGHJK") || s.Requests != 2 || s.Model != "claude-opus-5-5" ||
		s.EnvironmentKind != "anthropic_cloud" || s.SessionStatus != "running" || s.LastRoute != "code.session.events.post" {
		t.Fatalf("session = %+v", s)
	}
}

func TestLogsNeverContainCredentialsOrRawIDs(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{"id":"session_01ABCDEFGHJK"}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /v1/code/sessions/session_01ABCDEFGHJK HTTP/1.1\r\nHost: a\r\nAuthorization: Bearer SECRET-TOKEN-123\r\nCookie: k=SECRET-COOKIE\r\n\r\n")
	readResponse(t, br, "GET")
	logs := h.logs.String()
	if logs == "" {
		t.Fatal("expected debug logs")
	}
	for _, bad := range []string{"SECRET-TOKEN-123", "SECRET-COOKIE", "Authorization", "01ABCDEFGHJK"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("logs contain %q:\n%s", bad, logs)
		}
	}
}

func TestUpstreamDownReturns502(t *testing.T) {
	h := startProxy(t, map[string]string{"api.anthropic.com:443": "127.0.0.1:1"}, nil)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /a HTTP/1.1\r\nHost: a\r\n\r\n")
	resp, body := readResponse(t, br, "GET")
	if resp.Status != 502 || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("status=%d body=%q", resp.Status, body)
	}
	if s := h.srv.Stats(); s.UpstreamErrors != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestUntrustedClientHandshakeIsCounted(t *testing.T) {
	h := startProxy(t, nil, nil)
	raw, br := h.connect(t, "api.anthropic.com:443")
	tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "api.anthropic.com"})
	if err := tc.Handshake(); err == nil {
		t.Fatal("handshake should fail without the proxy CA")
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.srv.Stats().HandshakeFailures == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.srv.Stats().HandshakeFailures != 1 {
		t.Fatalf("stats = %+v", h.srv.Stats())
	}
}

func TestNonConnectRequestRejected(t *testing.T) {
	h := startProxy(t, nil, nil)
	raw, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	io.WriteString(raw, "GET http://example.org/ HTTP/1.1\r\nHost: example.org\r\n\r\n")
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(raw).ReadString('\n')
	if !strings.Contains(line, "405") {
		t.Fatalf("status line = %q", line)
	}
}

func TestExpectContinueRefused(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 1\r\n\r\n")
	if resp, _ := readResponse(t, br, "POST"); resp.Status != 417 {
		t.Fatalf("status = %d, want 417", resp.Status)
	}
}

func TestShutdownClosesLiveConnections(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, _ := h.connectTLS(t, "api.anthropic.com")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection should be closed after shutdown")
	}
}

type tempAcceptError struct{}

func (tempAcceptError) Error() string   { return "accept: too many open files" }
func (tempAcceptError) Timeout() bool   { return false }
func (tempAcceptError) Temporary() bool { return true }

// failOnceListener returns one temporary error from Accept, then behaves normally.
type failOnceListener struct {
	net.Listener
	failed atomic.Bool
}

func (l *failOnceListener) Accept() (net.Conn, error) {
	if l.failed.CompareAndSwap(false, true) {
		return nil, tempAcceptError{}
	}
	return l.Listener.Accept()
}

func TestServeRetriesTemporaryAcceptError(t *testing.T) {
	srv, err := New(Options{CA: newTestCA(t)})
	if err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(&failOnceListener{Listener: base}) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	raw, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	io.WriteString(raw, "GET http://example.org/ HTTP/1.1\r\nHost: example.org\r\n\r\n")
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(raw).ReadString('\n')
	if !strings.Contains(line, "405") {
		t.Fatalf("status line = %q: the accept loop did not survive the temporary error", line)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve returned: %v", err)
	default:
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamIsObservedWhileOpenAndOnceWhenAborted(t *testing.T) {
	release := make(chan struct{})
	up, roots := startFakeUpstream(t, func(_ string, _ []byte, conn net.Conn) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		io.WriteString(conn, "a\r\nevent: a\n\n\r\n")
		<-release
		io.WriteString(conn, "a\r\nevent: b\n\n\r\n0\r\n\r\n")
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /v1/code/sessions/session_01ABCDEFGHJK/events/stream HTTP/1.1\r\nHost: a\r\n\r\n")
	if _, err := readHead(br); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadString('\n'); err != nil { // first chunk header: the stream is live
		t.Fatal(err)
	}

	waitFor(t, "the open stream to be registered", func() bool { return len(h.registry.List()) == 1 })

	conn.Close() // the client aborts mid-stream
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil { // waits for the relay to finish
		t.Fatalf("shutdown: %v", err)
	}
	list := h.registry.List()
	if len(list) != 1 || list[0].Requests != 1 || list[0].LastRoute != "code.session.events.stream" {
		t.Fatalf("sessions = %+v, want one session observed once", list)
	}
}

func TestHandshakeWarningsAreOncePerHostAndBounded(t *testing.T) {
	srv, err := New(Options{CA: newTestCA(t)})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.firstFailureFor("api.anthropic.com") {
		t.Fatal("the first failure for a host must warn")
	}
	if srv.firstFailureFor("api.anthropic.com") {
		t.Fatal("a second failure for the same host must not warn")
	}
	warned := 1
	for i := 0; i < maxWarnedHosts+20; i++ {
		if srv.firstFailureFor(fmt.Sprintf("h%d.claude.ai", i)) {
			warned++
		}
	}
	if warned != maxWarnedHosts {
		t.Fatalf("warned for %d hosts, want the cap of %d", warned, maxWarnedHosts)
	}
}

// recordingConn records every byte the TLS client reads from the raw conn.
type recordingConn struct {
	net.Conn
	r   *bufio.Reader
	mu  sync.Mutex
	buf []byte
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	c.buf = append(c.buf, p[:n]...)
	c.mu.Unlock()
	return n, err
}

// lastRecordType returns the content type of the last complete TLS record.
func lastRecordType(stream []byte) byte {
	var last byte
	for len(stream) >= 5 {
		n := int(stream[3])<<8 | int(stream[4])
		if len(stream) < 5+n {
			break
		}
		last = stream[0]
		stream = stream[5+n:]
	}
	return last
}

func TestTerminatedConnectionEndsWithCloseNotify(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{"ok":true}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	raw, br := h.connect(t, "api.anthropic.com:443")
	rec := &recordingConn{Conn: raw, r: br}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.ca.CertPEM())
	// TLS 1.2 keeps the record content type in the clear, so an alert is visible.
	tc := tls.Client(rec, &tls.Config{RootCAs: pool, ServerName: "api.anthropic.com", MaxVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(tc, "GET /a HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n")
	if _, err := io.ReadAll(tc); err != nil {
		t.Fatalf("read to EOF: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := lastRecordType(rec.buf); got != 21 {
		t.Fatalf("last TLS record type = %d, want 21 (close_notify alert)", got)
	}
}

func TestServeAfterShutdownClosesTheListener(t *testing.T) {
	srv, err := New(Options{CA: newTestCA(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve = %v, want http.ErrServerClosed", err)
	}
	l.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the listener is still open after Serve refused it: %v", err)
	}
}
