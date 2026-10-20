package zen

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/proxydial/proxytest"
	"antigravity-go-proxy/internal/transport"
)

// spyTransport records whether the client carrying it served a request.
type spyTransport struct {
	mu   sync.Mutex
	used bool
	base http.RoundTripper
}

func (s *spyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.used = true
	s.mu.Unlock()
	return s.base.RoundTrip(r)
}

func (s *spyTransport) markUnused() {
	s.mu.Lock()
	s.used = false
	s.mu.Unlock()
}

func (s *spyTransport) wasUsed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// TestBunSpecFromCapture: the embedded ClientHello must fingerprint cleanly
// into a utls spec with the shape of the genuine capture (17 ciphers,
// 13 extensions) and offer exactly http/1.1.
func TestBunSpecFromCapture(t *testing.T) {
	hello, err := bunReplay()
	if err != nil {
		t.Fatalf("fingerprint captured hello: %v", err)
	}
	spec := hello.Spec()
	if len(spec.CipherSuites) != 17 {
		t.Errorf("ciphers = %d, want 17", len(spec.CipherSuites))
	}
	if len(spec.Extensions) != 13 {
		t.Errorf("extensions = %d, want 13", len(spec.Extensions))
	}
	if got := alpnOfHello(t, opencodeClientHello); got != "http/1.1" {
		t.Errorf("captured ALPN = %q, want http/1.1", got)
	}
}

// TestTLSClientOffByDefault: no opt-in, no utls.
func TestTLSClientOffByDefault(t *testing.T) {
	SetTLSConfig(ZenTLSConfig{})
	t.Cleanup(func() { SetTLSConfig(ZenTLSConfig{}) })

	if TLSClient() != plainClient {
		t.Error("disabled TLSClient should be the shared plain client")
	}
	if Transport() != nil {
		t.Error("disabled Transport should be nil")
	}
	SetTLSConfig(ZenTLSConfig{Enabled: true})
	if TLSClient() == http.DefaultClient {
		t.Error("enabled TLSClient should not be http.DefaultClient")
	}
}

// TestTLSClientIsAlwaysBounded: the wire path must not fall back to
// http.DefaultClient, which puts no bound on the wait for response headers.
// The plain transport is the one the passthrough already uses.
func TestTLSClientIsAlwaysBounded(t *testing.T) {
	SetTLSConfig(ZenTLSConfig{})
	t.Cleanup(func() { SetTLSConfig(ZenTLSConfig{}) })

	c := TLSClient()
	if c == http.DefaultClient {
		t.Fatal("disabled-disguise TLSClient is http.DefaultClient: no bound on the wait for headers")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != transport.ResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, transport.ResponseHeaderTimeout)
	}
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v: a total timeout would cut open streams", c.Timeout)
	}
}

// TestSharedTransportCached: one default-shaped utls transport per
// configuration — consecutive calls return the same pointer, SetTLSConfig
// invalidates it, and the clone keeps the default transport's timeouts without
// ever mutating the shared default.
func TestSharedTransportCached(t *testing.T) {
	SetTLSConfig(ZenTLSConfig{})
	t.Cleanup(func() { SetTLSConfig(ZenTLSConfig{}) })

	if Transport() != nil {
		t.Fatal("disabled Transport should be nil")
	}

	SetTLSConfig(ZenTLSConfig{Enabled: true})
	first := Transport()
	if first == nil {
		t.Fatal("enabled Transport is nil")
	}
	if again := Transport(); again != first {
		t.Error("consecutive Transport() calls should return the same pointer")
	}
	if TLSClient().Transport != first {
		t.Error("TLSClient should carry the shared transport")
	}
	if first.IdleConnTimeout == 0 {
		t.Error("shared transport should keep the default IdleConnTimeout")
	}
	if first.DialTLSContext == nil {
		t.Error("shared transport should dial through the captured hello")
	}
	if def, ok := http.DefaultTransport.(*http.Transport); ok && def.DialTLSContext != nil {
		t.Error("http.DefaultTransport must never be mutated")
	}

	SetTLSConfig(ZenTLSConfig{Enabled: true})
	second := Transport()
	if second == nil {
		t.Fatal("Transport nil after SetTLSConfig")
	}
	if second == first {
		t.Error("SetTLSConfig should invalidate the cached transport")
	}

	SetTLSConfig(ZenTLSConfig{})
	if Transport() != nil {
		t.Error("disabled Transport should be nil after SetTLSConfig")
	}
}

// TestFetchModelsRoutesThroughTLSClient: with the disguise enabled the
// models fetch must ride TLSClient()'s transport, not the client's own
// timeout-bounded one; with the disguise off it must keep using the local
// client (the 15s timeout) and never touch the utls path.
func TestFetchModelsRoutesThroughTLSClient(t *testing.T) {
	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	defer upstream.Close()

	spy := &spyTransport{base: http.DefaultTransport}
	c := NewClient(time.Second, time.Minute)
	c.httpClient = &http.Client{Timeout: time.Second, Transport: spy}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	SetTLSConfig(ZenTLSConfig{})
	t.Cleanup(func() { SetTLSConfig(ZenTLSConfig{}) })
	if _, err := c.FetchModels(ctx, "", upstream.URL); err != nil {
		t.Fatalf("disabled FetchModels: %v", err)
	}
	if !spy.wasUsed() {
		t.Fatal("disabled FetchModels should use the client's own transport")
	}

	spy.markUnused()
	hit.Store(false)
	SetTLSConfig(ZenTLSConfig{Enabled: true})
	got, err := c.FetchModels(ctx, "", upstream.URL)
	if err != nil {
		t.Fatalf("enabled FetchModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("models = %+v, want one m1", got)
	}
	if !hit.Load() {
		t.Fatal("enabled FetchModels issued no request upstream")
	}
	if spy.wasUsed() {
		t.Fatal("enabled FetchModels used the plain client transport; want TLSClient()")
	}
	if TLSClient() == c.httpClient {
		t.Fatal("TLSClient() should differ from the local client")
	}
}

// TestUTLSHelloMatchesCaptureJA3 dials through the utls transport against a
// local listener that sniffs the raw ClientHello, then compares the JA3
// (Wireshark string format) against both the embedded capture and the
// committed baseline. The handshake itself is expected to fail — the sniffer
// never answers — only the bytes on the wire matter.
func TestUTLSHelloMatchesCaptureJA3(t *testing.T) {
	SetTLSConfig(ZenTLSConfig{Enabled: true})
	t.Cleanup(func() { SetTLSConfig(ZenTLSConfig{}) })

	helloCh := make(chan []byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		record, err := readTLSRecord(conn)
		if err != nil {
			return
		}
		select {
		case helloCh <- record:
		default:
		}
		// Never answer: the client handshake fails after the hello is out.
	}()

	hello, err := bunReplay()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Dial via "localhost" (not the IP) so the SNI assertion is meaningful:
	// utls strips IP literals from SNI.
	_, _ = hello.Dial(ctx, "tcp", net.JoinHostPort("localhost", port))

	var sent []byte
	select {
	case sent = <-helloCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no ClientHello sniffed")
	}

	want := ja3OfHello(t, opencodeClientHello)
	if want != "1523504b38f0fae0d881d4b6554aac1b" {
		t.Fatalf("capture JA3 = %s, want baseline 1523504b38f0fae0d881d4b6554aac1b", want)
	}
	if got := ja3OfHello(t, sent); got != want {
		t.Errorf("sent JA3 = %s, want capture JA3 %s", got, want)
	}
	if sni := sniOfHello(t, sent); sni != "localhost" {
		t.Errorf("SNI = %q, want dial target host", sni)
	}
	if alpn := alpnOfHello(t, sent); alpn != "http/1.1" {
		t.Errorf("ALPN = %q, want http/1.1", alpn)
	}
}

// readTLSRecord reads one complete TLS record (header + payload).
func readTLSRecord(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(hdr[3:5])
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return append(hdr[:], payload...), nil
}

func isGREASE(v uint16) bool {
	return v&0x0f0f == 0x0a0a && v>>8 == uint16(v&0xff)
}

// helloBody returns the handshake body (after record + handshake headers).
func helloBody(t *testing.T, record []byte) []byte {
	t.Helper()
	if len(record) < 9 || record[0] != 0x16 || record[5] != 0x01 {
		t.Fatalf("not a ClientHello record: %x", record[:min(9, len(record))])
	}
	hsLen := int(binary.BigEndian.Uint32([]byte{0, record[6], record[7], record[8]}))
	if len(record) < 9+hsLen {
		t.Fatalf("truncated handshake: have %d, want %d", len(record), 9+hsLen)
	}
	return record[9 : 9+hsLen]
}

// helloFields parses a ClientHello into the JA3 components, stripping GREASE
// the way every JA3 implementation does.
func helloFields(t *testing.T, record []byte) (version uint16, ciphers, exts, groups, formats []uint16) {
	t.Helper()
	p := helloBody(t, record)
	u16 := func() uint16 {
		v := binary.BigEndian.Uint16(p[:2])
		p = p[2:]
		return v
	}
	skip := func(n int) { p = p[n:] }

	version = u16()
	skip(32) // random
	sidLen := int(p[0])
	skip(1 + sidLen)
	csLen := int(u16())
	for i := 0; i < csLen; i += 2 {
		ciphers = append(ciphers, u16())
	}
	compLen := int(p[0])
	skip(1 + compLen)
	remaining := int(u16())
	for remaining > 0 {
		if remaining < 4 {
			t.Fatalf("truncated extension list (%d left)", remaining)
		}
		et := u16()
		el := int(u16())
		body := p[:el]
		skip(el)
		remaining -= 4 + el
		exts = append(exts, et)
		switch et {
		case 10: // supported_groups
			n := int(binary.BigEndian.Uint16(body[:2]))
			for i := 0; i < n; i += 2 {
				groups = append(groups, binary.BigEndian.Uint16(body[2+i:4+i]))
			}
		case 11: // ec_point_formats
			n := int(body[0])
			for i := 0; i < n; i++ {
				formats = append(formats, uint16(body[1+i]))
			}
		}
	}
	return version, ciphers, exts, groups, formats
}

// ja3OfHello computes the Wireshark JA3 hash: values inside a group are
// hyphen separated, groups comma separated, then md5.
func ja3OfHello(t *testing.T, record []byte) string {
	t.Helper()
	version, ciphers, exts, groups, formats := helloFields(t, record)
	join := func(vs []uint16) string {
		parts := make([]string, 0, len(vs))
		for _, v := range vs {
			if isGREASE(v) {
				continue
			}
			parts = append(parts, fmt.Sprintf("%d", v))
		}
		return strings.Join(parts, "-")
	}
	full := fmt.Sprintf("%d,%s,%s,%s,%s", version, join(ciphers), join(exts), join(groups), join(formats))
	sum := md5.Sum([]byte(full))
	return hex.EncodeToString(sum[:])
}

// extensionBody finds one extension's body in a ClientHello record.
func extensionBody(t *testing.T, record []byte, want uint16) []byte {
	t.Helper()
	p := helloBody(t, record)
	u16 := func() uint16 { v := binary.BigEndian.Uint16(p[:2]); p = p[2:]; return v }
	skip := func(n int) { p = p[n:] }
	u16() // version
	skip(32)
	sidLen := int(p[0])
	skip(1 + sidLen)
	csLen := int(u16())
	skip(csLen)
	compLen := int(p[0])
	skip(1 + compLen)
	remaining := int(u16())
	for remaining > 0 {
		et := u16()
		el := int(u16())
		body := p[:el]
		skip(el)
		remaining -= 4 + el
		if et == want {
			return body
		}
	}
	return nil
}

func sniOfHello(t *testing.T, record []byte) string {
	t.Helper()
	body := extensionBody(t, record, 0)
	if len(body) < 5 {
		return ""
	}
	nameLen := int(binary.BigEndian.Uint16(body[3:5]))
	if len(body) < 5+nameLen {
		return ""
	}
	return string(body[5 : 5+nameLen])
}

func alpnOfHello(t *testing.T, record []byte) string {
	t.Helper()
	body := extensionBody(t, record, 16)
	if len(body) < 3 {
		return ""
	}
	return string(body[3 : 3+int(body[2])])
}

// TestZenProxyHelperProcess is the child of TestPlainHTTPZenFollowsTheProxyEnvironment,
// not a test of its own. http.ProxyFromEnvironment reads the environment once
// per process, so a test that wants it set runs in a process of its own.
func TestZenProxyHelperProcess(t *testing.T) {
	if os.Getenv("ZEN_PROXY_HELPER") == "" {
		t.Skip("helper process of TestPlainHTTPZenFollowsTheProxyEnvironment")
	}
	SetTLSConfig(ZenTLSConfig{Enabled: true})
	client := TLSClient()
	if client.Transport == nil {
		fmt.Println("RESULT error no utls transport")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://zen.example.invalid/zen/v1/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("RESULT error %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("RESULT %d %s\n", resp.StatusCode, body)
}

// A Zen base URL on http:// is reached through the proxy a user's environment
// names whether or not the TLS disguise is on: the disguise changes how TLS is
// dialed, and a plain request has none.
func TestPlainHTTPZenFollowsTheProxyEnvironment(t *testing.T) {
	proxy := proxytest.NewHTTPProxy(t, false, "unused")
	cmd := exec.Command(os.Args[0], "-test.run=^TestZenProxyHelperProcess$")
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "REQUEST_METHOD":
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "ZEN_PROXY_HELPER=1", "HTTP_PROXY="+proxy.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	_, result, ok := strings.Cut(string(out), "RESULT ")
	if !ok || !strings.HasPrefix(result, "200 http://zen.example.invalid/zen/v1/models") {
		t.Fatalf("the request did not come back from the proxy:\n%s", out)
	}
	if got := proxy.Forwarded(); len(got) != 1 || got[0].Target != "http://zen.example.invalid/zen/v1/models" {
		t.Errorf("the proxy was sent %+v, want the request in absolute form", got)
	}
}
