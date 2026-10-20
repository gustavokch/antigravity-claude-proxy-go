package httpwire

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// fetchLike and nodeLike mirror the two stacks the proxy imitates, so the tests
// check the layout rules without importing the package that defines them.
func fetchLike() Stack {
	return Stack{
		Order:          func(n []string) { sortStrings(n) },
		Tail:           []string{Connection, Host, AcceptEncoding, ContentLength},
		AcceptEncoding: "gzip, deflate, br, zstd",
		KeepAlive:      true,
	}
}

func nodeLike() Stack {
	return Stack{
		Order:          func(n []string) { sortStrings(n) },
		Tail:           []string{ContentLength, AcceptEncoding, Host, Connection},
		AcceptEncoding: "gzip, compress, deflate, br",
		KeepAlive:      false,
	}
}

func sortStrings(n []string) {
	for i := 1; i < len(n); i++ {
		for j := i; j > 0 && n[j] < n[j-1]; j-- {
			n[j], n[j-1] = n[j-1], n[j]
		}
	}
}

// server is a raw TCP HTTP/1.1 server: it records the exact bytes of every
// request and answers with whatever respond returns.
type server struct {
	ln      net.Listener
	mu      sync.Mutex
	conns   int
	reqs    []string
	respond func(index int, raw string) []byte
	// closeAfter closes each connection after its first response.
	closeAfter bool
}

func newServer(t *testing.T, respond func(index int, raw string) []byte) *server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{ln: ln, respond: respond}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.serve(c)
		}
	}()
	return s
}

func (s *server) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		raw, err := readRequest(br)
		if err != nil {
			return
		}
		s.mu.Lock()
		index := len(s.reqs)
		s.reqs = append(s.reqs, raw)
		s.mu.Unlock()
		if _, err := c.Write(s.respond(index, raw)); err != nil {
			return
		}
		if s.closeAfter {
			return
		}
	}
}

// readRequest returns one request's bytes: the head, and the body its
// Content-Length announces.
func readRequest(br *bufio.Reader) (string, error) {
	var head strings.Builder
	length := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		head.WriteString(line)
		if line == "\r\n" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "content-length") {
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return "", err
	}
	return head.String() + string(body), nil
}

func (s *server) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func (s *server) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *server) url(path string) string { return "http://" + s.ln.Addr().String() + path }

func ok(body string) []byte {
	return []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
}

func get(t *testing.T, tr *Transport, url string, header http.Header) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// The request must be written the way the stack lays it out: the caller's
// headers in the stack's order, with their names as given, then the tail.
func TestWritesTheFetchLayout(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return ok("fine") })
	tr := &Transport{Stack: fetchLike()}
	req, _ := http.NewRequest(http.MethodPost, s.url("/v1/messages?beta=true"), strings.NewReader(`{"a":1}`))
	// Assigned into the map so the names keep their case, as ApplyHeaders does.
	for _, kv := range [][2]string{
		{"x-app", "cli"}, {"User-Agent", "claude-cli/x"}, {"Accept", "application/json"},
		{"anthropic-version", "2023-06-01"}, {"X-Stainless-OS", "Linux"}, {"Content-Type", "application/json"},
	} {
		req.Header[kv[0]] = []string{kv[1]}
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	want := "POST /v1/messages?beta=true HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"Content-Type: application/json\r\n" +
		"User-Agent: claude-cli/x\r\n" +
		"X-Stainless-OS: Linux\r\n" +
		"anthropic-version: 2023-06-01\r\n" +
		"x-app: cli\r\n" +
		"Connection: keep-alive\r\n" +
		"Host: " + s.ln.Addr().String() + "\r\n" +
		"Accept-Encoding: gzip, deflate, br, zstd\r\n" +
		"Content-Length: 7\r\n" +
		"\r\n" + `{"a":1}`
	if got := s.requests()[0]; got != want {
		t.Errorf("request bytes:\n%q\nwant:\n%q", got, want)
	}
}

func TestWritesTheNodeLayout(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return ok("fine") })
	tr := &Transport{Stack: nodeLike()}
	get(t, tr, s.url("/api/claude_code/settings"), http.Header{"Accept": {"*/*"}, "User-Agent": {"claude-code/x"}})

	want := "GET /api/claude_code/settings HTTP/1.1\r\n" +
		"Accept: */*\r\n" +
		"User-Agent: claude-code/x\r\n" +
		"Accept-Encoding: gzip, compress, deflate, br\r\n" +
		"Host: " + s.ln.Addr().String() + "\r\n" +
		"Connection: close\r\n\r\n"
	if got := s.requests()[0]; got != want {
		t.Errorf("request bytes:\n%q\nwant:\n%q", got, want)
	}
}

// Framing, host and encoding belong to the transport. A caller's own values
// would otherwise put two Host lines on the wire or advertise an encoding the
// transport cannot decode.
func TestCallerCannotSetTheTransportsHeaders(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return ok("fine") })
	tr := &Transport{Stack: fetchLike()}
	get(t, tr, s.url("/"), http.Header{
		"Host": {"evil.example"}, "Accept-Encoding": {"identity"}, "Connection": {"upgrade"},
		"Content-Length": {"999"}, "Transfer-Encoding": {"chunked"}, "Accept": {"x"},
	})
	got := s.requests()[0]
	for _, bad := range []string{"evil.example", "identity", "upgrade", "999", "chunked"} {
		if strings.Contains(got, bad) {
			t.Errorf("a caller-supplied value %q reached the wire:\n%s", bad, got)
		}
	}
	if strings.Count(got, "Host:") != 1 || strings.Count(got, "Accept-Encoding:") != 1 {
		t.Errorf("duplicated transport headers:\n%s", got)
	}
}

// A CR or LF in a header would split the request.
func TestHeaderInjectionIsRefused(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return ok("fine") })
	tr := &Transport{Stack: fetchLike()}
	for name, header := range map[string]http.Header{
		"value": {"X-A": {"a\r\nX-Evil: 1"}},
		"name":  {"X-A\r\nX-Evil": {"1"}},
	} {
		req, _ := http.NewRequest(http.MethodGet, s.url("/"), nil)
		for k, v := range header {
			req.Header[k] = v
		}
		if resp, err := tr.RoundTrip(req); err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: injected header accepted", name)
		}
	}
	if len(s.requests()) != 0 {
		t.Error("a refused request still reached the server")
	}
}

func TestKeepAliveReusesTheConnection(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return ok("fine") })
	tr := &Transport{Stack: fetchLike()}
	for range 3 {
		get(t, tr, s.url("/"), nil)
	}
	if got := s.connections(); got != 1 {
		t.Errorf("3 sequential requests used %d connections, want 1", got)
	}

	closing := newServer(t, func(int, string) []byte { return ok("fine") })
	node := &Transport{Stack: nodeLike()}
	for range 3 {
		get(t, node, closing.url("/"), nil)
	}
	if got := closing.connections(); got != 3 {
		t.Errorf("Connection: close stack used %d connections for 3 requests, want 3", got)
	}
}

// A connection the server closed while it idled is not an error the caller
// sees: the request goes out again on a new one.
func TestStaleIdleConnectionIsRetried(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return ok("fine") })
	s.closeAfter = true // answers, advertises nothing, then hangs up
	tr := &Transport{Stack: fetchLike()}
	for range 3 {
		if body := get(t, tr, s.url("/"), nil); body != "fine" {
			t.Fatalf("body = %q", body)
		}
	}
	if got := s.connections(); got != 3 {
		t.Errorf("connections = %d, want one per request once each was found closed", got)
	}
}

func compress(t *testing.T, encoding, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch encoding {
	case "gzip":
		w := gzip.NewWriter(&buf)
		w.Write([]byte(text))
		w.Close()
	case "deflate":
		w := zlib.NewWriter(&buf)
		w.Write([]byte(text))
		w.Close()
	case "deflate-raw":
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		w.Write([]byte(text))
		w.Close()
	case "br":
		w := brotli.NewWriter(&buf)
		w.Write([]byte(text))
		w.Close()
	case "zstd":
		w, _ := zstd.NewWriter(&buf)
		w.Write([]byte(text))
		w.Close()
	}
	return buf.Bytes()
}

// The stack advertises gzip, deflate, br and zstd, so the transport must decode
// all four, in a Content-Length body and in a chunked one, and present the body
// the way net/http does when it decompresses.
func TestDecodesEveryAdvertisedEncoding(t *testing.T) {
	const text = "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, encoding := range []string{"gzip", "deflate", "deflate-raw", "br", "zstd"} {
		for _, chunked := range []bool{false, true} {
			name := encoding
			if chunked {
				name += "/chunked"
			}
			t.Run(name, func(t *testing.T) {
				payload := compress(t, encoding, text)
				wire := strings.TrimSuffix(encoding, "-raw")
				if encoding == "deflate-raw" {
					wire = "deflate"
				}
				s := newServer(t, func(int, string) []byte {
					head := "HTTP/1.1 200 OK\r\nContent-Encoding: " + wire + "\r\n"
					if !chunked {
						return append([]byte(head+fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))), payload...)
					}
					var b bytes.Buffer
					b.WriteString(head + "Transfer-Encoding: chunked\r\n\r\n")
					half := len(payload) / 2
					for _, part := range [][]byte{payload[:half], payload[half:]} {
						fmt.Fprintf(&b, "%x\r\n%s\r\n", len(part), part)
					}
					b.WriteString("0\r\n\r\n")
					return b.Bytes()
				})
				tr := &Transport{Stack: fetchLike()}
				req, _ := http.NewRequest(http.MethodGet, s.url("/"), nil)
				resp, err := (&http.Client{Transport: tr}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				got, err := io.ReadAll(resp.Body)
				if err != nil || string(got) != text {
					t.Fatalf("body = %q, %v; want %q", got, err, text)
				}
				if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "" || !resp.Uncompressed {
					t.Errorf("the response still claims its encoding: %v uncompressed=%v", resp.Header, resp.Uncompressed)
				}
				// The decoded body leaves the connection reusable.
				if body := get(t, tr, s.url("/"), nil); body != text {
					t.Errorf("second request body = %q", body)
				}
				if got := s.connections(); got != 1 {
					t.Errorf("connections = %d, want the first reused", got)
				}
			})
		}
	}
}

func TestUnknownEncodingIsLeftToTheCaller(t *testing.T) {
	s := newServer(t, func(int, string) []byte {
		return []byte("HTTP/1.1 200 OK\r\nContent-Encoding: compress\r\nContent-Length: 3\r\n\r\nabc")
	})
	req, _ := http.NewRequest(http.MethodGet, s.url("/"), nil)
	resp, err := (&http.Client{Transport: &Transport{Stack: fetchLike()}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "abc" || resp.Header.Get("Content-Encoding") != "compress" || resp.Uncompressed {
		t.Errorf("body %q header %v: an encoding the transport cannot decode was altered", body, resp.Header)
	}
}

// A stack that sends no Accept-Encoding gets what it asked for.
func TestNoAcceptEncodingMeansNoDecoding(t *testing.T) {
	payload := compress(t, "gzip", "hello")
	s := newServer(t, func(int, string) []byte {
		return append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n", len(payload))), payload...)
	})
	stack := fetchLike()
	stack.AcceptEncoding = ""
	req, _ := http.NewRequest(http.MethodGet, s.url("/"), nil)
	resp, err := (&http.Client{Transport: &Transport{Stack: stack}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, payload) {
		t.Error("the body was decoded although no encoding was advertised")
	}
	if strings.Contains(s.requests()[0], "Accept-Encoding") {
		t.Error("an empty AcceptEncoding still wrote the header")
	}
}

// Cancelling the request must end a body that is still streaming, promptly, and
// must not return the half-read connection to the pool.
func TestCancelEndsAStreamingBody(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
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
		_, _ = readRequest(bufio.NewReader(c))
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n"))
		<-release
	}()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	tr := &Transport{Stack: fetchLike()}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "hello" {
		t.Fatalf("first chunk = %q, %v", first, err)
	}

	done := make(chan error, 1)
	go func() { _, err := resp.Body.Read(make([]byte, 1)); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled stream kept reading")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not end the blocked read")
	}
	tr.mu.Lock()
	idle := len(tr.idle)
	tr.mu.Unlock()
	if idle != 0 {
		t.Error("a half-read connection went back to the pool")
	}
}

func TestResponseHeaderTimeout(t *testing.T) {
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
			defer c.Close() // accepts and stays silent
		}
	}()
	tr := &Transport{Stack: fetchLike(), ResponseHeaderTimeout: 100 * time.Millisecond}
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	if resp, err := tr.RoundTrip(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a silent server produced a response")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v to give up on a 100ms bound", elapsed)
	}
}

// Closing a body that was not read to its end cannot reuse the connection: the
// rest of the body is still on it.
func TestAbandonedBodyDoesNotPoisonThePool(t *testing.T) {
	s := newServer(t, func(i int, _ string) []byte {
		if i == 0 {
			return []byte("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n0123456789")
		}
		return ok("second")
	})
	tr := &Transport{Stack: fetchLike()}
	req, _ := http.NewRequest(http.MethodGet, s.url("/"), nil)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = resp.Body.Read(make([]byte, 3))
	_ = resp.Body.Close()
	if body := get(t, tr, s.url("/"), nil); body != "second" {
		t.Errorf("second response = %q: leftover bytes of the first leaked into it", body)
	}
	if got := s.connections(); got != 2 {
		t.Errorf("connections = %d, want the abandoned one replaced", got)
	}
}

func TestNoBodyResponseKeepsTheConnection(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return []byte("HTTP/1.1 204 No Content\r\n\r\n") })
	tr := &Transport{Stack: fetchLike()}
	for range 2 {
		req, _ := http.NewRequest(http.MethodGet, s.url("/"), nil)
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if got := s.connections(); got != 1 {
		t.Errorf("connections = %d, want 1", got)
	}
}

func TestStripDefaultPort(t *testing.T) {
	for _, tc := range []struct{ host, scheme, want string }{
		{"api.anthropic.com:443", "https", "api.anthropic.com"},
		{"api.anthropic.com", "https", "api.anthropic.com"},
		{"example.com:8443", "https", "example.com:8443"},
		{"example.com:443", "http", "example.com:443"},
		{"example.com:80", "http", "example.com"},
		{"[::1]:443", "https", "[::1]"},
	} {
		if got := stripDefaultPort(tc.host, tc.scheme); got != tc.want {
			t.Errorf("stripDefaultPort(%q, %q) = %q, want %q", tc.host, tc.scheme, got, tc.want)
		}
	}
}

// The header bound stops at the headers: an upstream that flushes them at once
// and then keeps writing for longer than the bound is read to completion.
func TestOpenStreamSurvivesTheHeaderBound(t *testing.T) {
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
		_, _ = readRequest(bufio.NewReader(c))
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"))
		for range 6 {
			_, _ = c.Write([]byte("2\r\nok\r\n"))
			time.Sleep(60 * time.Millisecond)
		}
		_, _ = c.Write([]byte("0\r\n\r\n"))
	}()

	tr := &Transport{Stack: fetchLike(), ResponseHeaderTimeout: 100 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != strings.Repeat("ok", 6) {
		t.Errorf("body = %q, %v; the bound must not apply to the body", body, err)
	}
}

// send issues one request with tr and returns the final response with its body
// read. It fails the test on a transport error.
func send(t *testing.T, tr *Transport, method, url, reqBody string, header http.Header) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if reqBody != "" {
		rd = strings.NewReader(reqBody)
	}
	req, _ := http.NewRequest(method, url, rd)
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(got)
}

// An interim response (100 Continue, 103 Early Hints) precedes the final one on
// the same connection, and RFC 9110 has a client accept it unasked. Returned as
// the answer, it would leave the real response unread on a connection that goes
// back to the pool, and the next request would be handed that response.
func TestInterimResponsesAreSkipped(t *testing.T) {
	for name, interim := range map[string]string{
		"103 Early Hints": "HTTP/1.1 103 Early Hints\r\nLink: </app.css>; rel=preload\r\n\r\n",
		"100 Continue":    "HTTP/1.1 100 Continue\r\n\r\n",
		"two in a row":    "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </a>; rel=preload\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, func(i int, _ string) []byte {
				if i == 0 {
					return append([]byte(interim), ok("first")...)
				}
				return ok("second")
			})
			tr := &Transport{Stack: fetchLike()}
			for i, want := range []string{"first", "second"} {
				resp, body := send(t, tr, http.MethodPost, s.url("/v1/messages"), "{}", nil)
				if resp.StatusCode != http.StatusOK || body != want {
					t.Fatalf("request %d: status %d body %q, want 200 %q", i, resp.StatusCode, body, want)
				}
			}
			if got := s.connections(); got != 1 {
				t.Errorf("connections = %d, want the interim response to leave the connection reusable", got)
			}
		})
	}
}

// The interim response's own headers are not the final response's.
func TestInterimResponseHeadersAreNotTheFinalOnes(t *testing.T) {
	s := newServer(t, func(int, string) []byte {
		return append([]byte("HTTP/1.1 103 Early Hints\r\nLink: </app.css>; rel=preload\r\n\r\n"), ok("fine")...)
	})
	resp, _ := send(t, &Transport{Stack: fetchLike()}, http.MethodGet, s.url("/"), "", nil)
	if link := resp.Header.Get("Link"); link != "" {
		t.Errorf("the final response carries the interim Link %q", link)
	}
	if resp.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("Content-Type = %q, want the final response's", resp.Header.Get("Content-Type"))
	}
}

// A 101 ends HTTP on the connection, and the transport has no way to hand the
// stream on, so it is returned as it came and the connection is not reused.
func TestSwitchingProtocolsIsReturnedAndNotPooled(t *testing.T) {
	s := newServer(t, func(i int, _ string) []byte {
		if i == 0 {
			return []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n")
		}
		return ok("second")
	})
	tr := &Transport{Stack: fetchLike()}
	if resp, _ := send(t, tr, http.MethodGet, s.url("/"), "", nil); resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want the 101 returned", resp.StatusCode)
	}
	if _, body := send(t, tr, http.MethodGet, s.url("/"), "", nil); body != "second" {
		t.Errorf("second body = %q", body)
	}
	if got := s.connections(); got != 2 {
		t.Errorf("connections = %d, want the 101's connection dropped", got)
	}
}

// The transport sends the body at once and never waits for a 100 Continue, and
// neither real client sends the header, so a caller's Expect does not go out.
func TestExpectIsNeverSent(t *testing.T) {
	s := newServer(t, func(int, string) []byte { return ok("fine") })
	tr := &Transport{Stack: fetchLike()}
	send(t, tr, http.MethodPost, s.url("/v1/messages"), "{}", http.Header{"Expect": {"100-continue"}})
	if raw := s.requests()[0]; strings.Contains(strings.ToLower(raw), "expect") {
		t.Errorf("Expect reached the wire:\n%s", raw)
	}
}
