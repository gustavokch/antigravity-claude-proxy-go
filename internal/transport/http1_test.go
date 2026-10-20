package transport

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// h2Server is a TLS server that speaks both HTTP/2 and HTTP/1.1 and records what
// each client offered and got.
type h2Server struct {
	*httptest.Server
	mu         sync.Mutex
	alpn       []string
	protoMajor int
}

func newH2Server(t *testing.T) *h2Server {
	t.Helper()
	s := &h2Server{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.protoMajor = r.ProtoMajor
		s.mu.Unlock()
	}))
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			s.mu.Lock()
			s.alpn = append([]string(nil), hello.SupportedProtos...)
			s.mu.Unlock()
			return nil, nil
		},
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// get issues one GET with tr, trusting the server's self-signed certificate, and
// returns the HTTP major version the server saw and the ALPN list the client
// offered. tr is changed only by adding that trust.
func (s *h2Server) get(t *testing.T, tr *http.Transport) (protoMajor int, alpn []string) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.RootCAs = pool
	t.Cleanup(tr.CloseIdleConnections)

	resp, err := (&http.Client{Transport: tr}).Get(s.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protoMajor, s.alpn
}

// Against a server that would happily speak h2, the Claude Code transports speak
// HTTP/1.1 and offer the ALPN list of the stack they imitate. The control leg
// proves the server really would have given h2, so the assertion cannot pass by
// the server being unable to.
func TestClaudeCodeTransportsDeclineH2(t *testing.T) {
	control := newH2Server(t)
	if major, alpn := control.get(t, Upstream()); major != 2 {
		t.Fatalf("control: the default upstream transport got HTTP/%d (ALPN %v), want h2; this test would prove nothing", major, alpn)
	}

	for _, tc := range []struct {
		name     string
		build    func() *http.Transport
		wantALPN []string
	}{
		// fetch offers http/1.1; node:https offers nothing at all.
		{"messages", ClaudeCodeMessages, []string{"http/1.1"}},
		{"discovery", ClaudeCodeDiscovery, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newH2Server(t)
			major, alpn := server.get(t, tc.build())
			if major != 1 {
				t.Errorf("got HTTP/%d, want HTTP/1.1", major)
			}
			if !slices.Equal(alpn, tc.wantALPN) {
				t.Errorf("ClientHello offered ALPN %v, want %v", alpn, tc.wantALPN)
			}
		})
	}
}

// The constructors build on Upstream, so the header bound must survive, every
// call must own its pool, and the shared default transport must not pick up the
// HTTP/1.1 restriction.
func TestClaudeCodeTransportsKeepTheBoundAndLeaveTheDefaultAlone(t *testing.T) {
	for name, build := range map[string]func() *http.Transport{
		"messages": ClaudeCodeMessages, "discovery": ClaudeCodeDiscovery,
	} {
		if tr := build(); tr.ResponseHeaderTimeout != ResponseHeaderTimeout {
			t.Errorf("%s: ResponseHeaderTimeout = %v, want %v", name, tr.ResponseHeaderTimeout, ResponseHeaderTimeout)
		}
		if first, second := build(), build(); first == second {
			t.Errorf("%s: each call must return its own transport", name)
		}
	}

	server := newH2Server(t)
	if major, _ := server.get(t, Upstream()); major != 2 {
		t.Errorf("Upstream got HTTP/%d after the Claude Code transports were built, want h2: the restriction leaked", major)
	}
}

// helloShape is what a ClientHello says about its stack, minus the fields that
// differ on every connection: the cipher suites and the extension types in the
// order they were sent.
type helloShape struct {
	ciphers    []uint16
	extensions []uint16
}

func shapeOf(t *testing.T, record []byte) helloShape {
	t.Helper()
	// record header (5), handshake header (4), version (2), random (32).
	pos := 5 + 4 + 2 + 32
	pos += 1 + int(record[pos]) // session id
	cipherLen := int(binary.BigEndian.Uint16(record[pos:]))
	var shape helloShape
	for i := 0; i < cipherLen; i += 2 {
		shape.ciphers = append(shape.ciphers, binary.BigEndian.Uint16(record[pos+2+i:]))
	}
	pos += 2 + cipherLen
	pos += 1 + int(record[pos]) // compression methods
	end := pos + 2 + int(binary.BigEndian.Uint16(record[pos:]))
	for pos += 2; pos < end; {
		shape.extensions = append(shape.extensions, binary.BigEndian.Uint16(record[pos:]))
		pos += 4 + int(binary.BigEndian.Uint16(record[pos+2:]))
	}
	return shape
}

// sniffHello returns the first TLS record a client sends to a listener that
// never answers.
func sniffHello(t *testing.T, tr *http.Transport) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint16(header[3:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		got <- append(header, body...)
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	_, _ = client.Get("https://localhost:" + port + "/")
	select {
	case record := <-got:
		return record
	case <-time.After(10 * time.Second):
		t.Fatal("no ClientHello sniffed")
		return nil
	}
}

// The bytes on the wire must be the capture's: same cipher suites, same
// extensions in the same order. This is what the JA4 the server computes is made
// of, and Go's own handshake differs from it in both.
func TestClaudeCodeTransportsSendTheCapturedHello(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   func() *http.Transport
		capture []byte
	}{
		{"messages", ClaudeCodeMessages, claudeCodeFetchHello},
		{"discovery", ClaudeCodeDiscovery, claudeCodeNodeHello},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := shapeOf(t, tc.capture)
			sent := sniffHello(t, tc.build())
			got := shapeOf(t, sent)
			if !slices.Equal(got.ciphers, want.ciphers) {
				t.Errorf("cipher suites = %x, want the capture's %x", got.ciphers, want.ciphers)
			}
			if !slices.Equal(got.extensions, want.extensions) {
				t.Errorf("extensions = %x, want the capture's %x", got.extensions, want.extensions)
			}
			if bytes.Equal(sent, tc.capture) {
				t.Error("the hello is byte-identical to the capture; the random and key shares must be fresh")
			}
		})
	}
}

// The two stacks are different clients, so their hellos must differ.
func TestClaudeCodeStacksAreDistinct(t *testing.T) {
	fetch, node := shapeOf(t, claudeCodeFetchHello), shapeOf(t, claudeCodeNodeHello)
	if slices.Equal(fetch.ciphers, node.ciphers) && slices.Equal(fetch.extensions, node.extensions) {
		t.Error("the fetch and node captures have the same shape; one was recorded twice")
	}
}
