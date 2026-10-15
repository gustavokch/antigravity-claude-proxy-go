package transport

import (
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestUpstreamSetsOnlyTheHeaderBound checks the one promise Upstream makes: the
// clone carries a bound on time to response headers and changes nothing else that
// reaches the wire. The default transport itself must stay untouched, because
// other callers share it.
func TestUpstreamSetsOnlyTheHeaderBound(t *testing.T) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport is not a *http.Transport in this build")
	}
	if base.ResponseHeaderTimeout != 0 {
		t.Fatal("expected the untouched default transport to have no response-header bound")
	}

	tr := Upstream()
	if tr == base {
		t.Fatal("Upstream must not hand out the default transport itself")
	}
	if tr.ResponseHeaderTimeout != ResponseHeaderTimeout {
		t.Errorf("expected ResponseHeaderTimeout %v, got %v", ResponseHeaderTimeout, tr.ResponseHeaderTimeout)
	}
	// The fields that decide what the peer sees, and that a caller would not
	// expect a timeout helper to change.
	if !tr.ForceAttemptHTTP2 {
		t.Error("expected the clone to keep HTTP/2 enabled")
	}
	// Nothing that reaches the ClientHello may change: this repo's rule is that
	// the default crypto is the disguise.
	if tr.TLSClientConfig == nil || base.TLSClientConfig == nil {
		t.Fatalf("expected the clone to carry the default TLS config, got %+v", tr.TLSClientConfig)
	}
	if !slices.Equal(tr.TLSClientConfig.NextProtos, base.TLSClientConfig.NextProtos) {
		t.Errorf("expected NextProtos %v, got %v", base.TLSClientConfig.NextProtos, tr.TLSClientConfig.NextProtos)
	}
	if len(tr.TLSClientConfig.CipherSuites) != 0 || len(tr.TLSClientConfig.CurvePreferences) != 0 {
		t.Error("expected no explicit cipher suites or curve preferences")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify left false")
	}
	if tr.DialContext == nil {
		t.Error("expected the clone to keep the default dialer")
	}
	if base.ResponseHeaderTimeout != 0 {
		t.Error("Upstream mutated the shared default transport")
	}

	if first, second := Upstream(), Upstream(); first == second {
		t.Error("each call must return its own transport, so a caller can add its own DialTLSContext")
	}
}

// TestUpstreamHonoursTheHeaderBound proves the bound is live on the transport we
// hand out: a listener that accepts the connection and never writes a byte ends
// the round trip instead of holding it open. The timeout is shortened here so the
// test is fast; the shipped value is the same field.
func TestUpstreamHonoursTheHeaderBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accepted, then silent: no status line, no headers, and the
			// connection stays open so the client has nothing but the clock.
			_ = conn
		}
	}()

	tr := Upstream()
	tr.ResponseHeaderTimeout = 50 * time.Millisecond
	client := &http.Client{Transport: tr}

	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the silent listener to end the round trip with an error")
	}
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Errorf("expected a response-header timeout, got %v", err)
	}
}
