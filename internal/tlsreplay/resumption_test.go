package tlsreplay

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// recordingListener keeps the first TLS record each accepted connection sends,
// which is the ClientHello as it crossed the wire.
type recordingListener struct {
	net.Listener
	mu     sync.Mutex
	hellos [][]byte
}

func (l *recordingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &recordingConn{Conn: conn, l: l}, nil
}

type recordingConn struct {
	net.Conn
	l    *recordingListener
	buf  []byte
	done bool
}

func (c *recordingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if !c.done && n > 0 {
		c.buf = append(c.buf, b[:n]...)
		if len(c.buf) >= 5 && len(c.buf) >= 5+int(binary.BigEndian.Uint16(c.buf[3:5])) {
			c.done = true
			c.l.mu.Lock()
			c.l.hellos = append(c.l.hellos, slices.Clone(c.buf))
			c.l.mu.Unlock()
		}
	}
	return n, err
}

// extensionTypes lists the extensions of a ClientHello record in wire order.
func extensionTypes(record []byte) []uint16 {
	pos := 5 + 4 + 2 + 32
	pos += 1 + int(record[pos])
	pos += 2 + int(binary.BigEndian.Uint16(record[pos:]))
	pos += 1 + int(record[pos])
	end := pos + 2 + int(binary.BigEndian.Uint16(record[pos:]))
	var types []uint16
	for pos += 2; pos < end; {
		types = append(types, binary.BigEndian.Uint16(record[pos:]))
		pos += 4 + int(binary.BigEndian.Uint16(record[pos+2:]))
	}
	return types
}

// connect makes one request over a transport of its own, so nothing but the
// Hello's session cache can carry state from one connection to the next, and
// reports whether the server resumed the session.
func connect(t *testing.T, hello *Hello, pool *x509.CertPool, url string) {
	t.Helper()
	tr := hello.Transport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}})
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
}

func resumptionServer(t *testing.T) (*httptest.Server, *recordingListener, *[]bool, *x509.CertPool) {
	t.Helper()
	var (
		mu      sync.Mutex
		resumed []bool
	)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		resumed = append(resumed, r.TLS.DidResume)
		mu.Unlock()
	}))
	listener := &recordingListener{Listener: server.Listener}
	server.Listener = listener
	server.StartTLS()
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	return server, listener, &resumed, pool
}

// Bun resumes: against a server that issues tickets, the first connection sends
// the full hello and every later one the same hello with pre_shared_key last.
// The wire is checked, not just the server's DidResume, because the claim is
// about which hello the server was shown.
func TestWithResumptionSendsPSKAfterTheFirstConnection(t *testing.T) {
	server, listener, resumed, pool := resumptionServer(t)
	hello := MustNew(readCapture(t), []string{"http/1.1"}).WithResumption()

	for range 3 {
		connect(t, hello, pool, server.URL)
	}

	if !slices.Equal(*resumed, []bool{false, true, true}) {
		t.Fatalf("DidResume per connection = %v, want [false true true]", *resumed)
	}
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if len(listener.hellos) != 3 {
		t.Fatalf("recorded %d hellos, want 3", len(listener.hellos))
	}
	full := extensionTypes(listener.hellos[0])
	if slices.Contains(full, 0x29) {
		t.Errorf("the first hello already carries pre_shared_key: %x", full)
	}
	want := append(slices.Clone(full), 0x29)
	for i, record := range listener.hellos[1:] {
		if got := extensionTypes(record); !slices.Equal(got, want) {
			t.Errorf("resumed hello %d extensions = %x, want the full hello's plus pre_shared_key last: %x", i+1, got, want)
		}
	}
}

// Resumption is opt-in: a Hello that was not asked for it sends the full hello
// every time, as Zen's replay always has.
func TestWithoutResumptionEveryConnectionIsFull(t *testing.T) {
	server, listener, resumed, pool := resumptionServer(t)
	hello := MustNew(readCapture(t), []string{"http/1.1"})

	for range 2 {
		connect(t, hello, pool, server.URL)
	}

	if !slices.Equal(*resumed, []bool{false, false}) {
		t.Errorf("DidResume per connection = %v, want [false false]", *resumed)
	}
	listener.mu.Lock()
	defer listener.mu.Unlock()
	for i, record := range listener.hellos {
		if slices.Contains(extensionTypes(record), 0x29) {
			t.Errorf("hello %d carries pre_shared_key without resumption enabled", i)
		}
	}
}

// Tickets are the Hello's, not a transport's: two Hellos built from one capture
// are two stacks, and one's tickets must not resume the other's connections.
func TestResumptionCachesAreNotShared(t *testing.T) {
	server, _, resumed, pool := resumptionServer(t)
	capture := readCapture(t)

	connect(t, MustNew(capture, []string{"http/1.1"}).WithResumption(), pool, server.URL)
	connect(t, MustNew(capture, []string{"http/1.1"}).WithResumption(), pool, server.URL)

	if !slices.Equal(*resumed, []bool{false, false}) {
		t.Errorf("DidResume per connection = %v, want [false false]", *resumed)
	}
}
