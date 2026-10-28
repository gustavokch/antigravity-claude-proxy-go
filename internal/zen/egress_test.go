package zen

import (
	"context"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/proxydial/proxytest"
)

const freeUsageBody = `{"type":"error","error":{"type":"rate_limit_error","message":"Zen: Free usage limit reached. Free models work in OpenCode"}}`

// zenBackend answers every request with status and body, counting hits and
// recording the last request body.
type zenBackend struct {
	srv      *httptest.Server
	hits     atomic.Int32
	lastBody atomic.Value
}

func newZenBackend(t *testing.T, status int, body string, header http.Header) *zenBackend {
	t.Helper()
	b := &zenBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		got, _ := io.ReadAll(r.Body)
		b.lastBody.Store(string(got))
		maps.Copy(w.Header(), header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// useEgress configures a fallback proxy with fresh exhaustion state and
// restores the defaults afterwards.
func useEgress(t *testing.T, proxy string) {
	t.Helper()
	reset := func() {
		SetTLSConfig(ZenTLSConfig{})
		if err := SetEgressConfig(EgressConfig{}); err != nil {
			t.Fatal(err)
		}
		exhaustMu.Lock()
		clear(exhaustedUntil)
		exhaustMu.Unlock()
		now = time.Now
	}
	reset()
	t.Cleanup(reset)
	if err := SetEgressConfig(EgressConfig{FallbackProxy: proxy}); err != nil {
		t.Fatal(err)
	}
}

func forwardMessages(t *testing.T, baseURL, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	ForwardMessages(rec, req, baseURL, "public", []byte(body))
	return rec
}

func TestFreeUsageLimitFailsOverToTheFallbackEgress(t *testing.T) {
	direct := newZenBackend(t, http.StatusTooManyRequests, freeUsageBody, nil)
	fresh := newZenBackend(t, http.StatusOK, `{"type":"message"}`, nil)
	socks := proxytest.NewSOCKS5(t, false, strings.TrimPrefix(fresh.srv.URL, "http://"))
	useEgress(t, "socks5h://"+socks.Addr)

	const body = `{"model":"glm-5.3-flash-free","messages":[]}`
	rec := forwardMessages(t, direct.srv.URL, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want the fallback's 200", rec.Code, rec.Body)
	}
	if got := fresh.lastBody.Load(); got != body {
		t.Fatalf("fallback received body %q, want the original %q", got, body)
	}

	// The direct egress stays exhausted: the next request goes straight to
	// the fallback instead of spending another 429.
	rec = forwardMessages(t, direct.srv.URL, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", rec.Code)
	}
	if n := direct.hits.Load(); n != 1 {
		t.Errorf("direct egress hits = %d, want 1", n)
	}
	if n := fresh.hits.Load(); n != 2 {
		t.Errorf("fallback hits = %d, want 2", n)
	}
}

func TestDirectEgressIsRetriedAfterRetryAfter(t *testing.T) {
	direct := newZenBackend(t, http.StatusTooManyRequests, freeUsageBody, http.Header{"Retry-After": {"1800"}})
	fresh := newZenBackend(t, http.StatusOK, `{"type":"message"}`, nil)
	socks := proxytest.NewSOCKS5(t, false, strings.TrimPrefix(fresh.srv.URL, "http://"))
	useEgress(t, "socks5h://"+socks.Addr)

	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return start }
	forwardMessages(t, direct.srv.URL, `{}`)
	now = func() time.Time { return start.Add(29 * time.Minute) }
	forwardMessages(t, direct.srv.URL, `{}`)
	if n := direct.hits.Load(); n != 1 {
		t.Fatalf("direct hits inside Retry-After = %d, want 1", n)
	}
	now = func() time.Time { return start.Add(31 * time.Minute) }
	forwardMessages(t, direct.srv.URL, `{}`)
	if n := direct.hits.Load(); n != 2 {
		t.Fatalf("direct hits after Retry-After = %d, want 2", n)
	}
}

func TestOtherRateLimitsDoNotFailOver(t *testing.T) {
	const other = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	direct := newZenBackend(t, http.StatusTooManyRequests, other, nil)
	fresh := newZenBackend(t, http.StatusOK, `{"type":"message"}`, nil)
	socks := proxytest.NewSOCKS5(t, false, strings.TrimPrefix(fresh.srv.URL, "http://"))
	useEgress(t, "socks5h://"+socks.Addr)

	rec := forwardMessages(t, direct.srv.URL, `{}`)
	if rec.Code != http.StatusTooManyRequests || rec.Body.String() != other {
		t.Fatalf("got %d %q, want the upstream 429 untouched", rec.Code, rec.Body)
	}
	if n := fresh.hits.Load(); n != 0 {
		t.Errorf("fallback hits = %d, want 0", n)
	}
}

func TestBothEgressExhaustedReturnsTheLimit(t *testing.T) {
	direct := newZenBackend(t, http.StatusTooManyRequests, freeUsageBody, nil)
	alsoSpent := newZenBackend(t, http.StatusTooManyRequests, freeUsageBody, nil)
	socks := proxytest.NewSOCKS5(t, false, strings.TrimPrefix(alsoSpent.srv.URL, "http://"))
	useEgress(t, "socks5h://"+socks.Addr)

	for i := range 2 {
		rec := forwardMessages(t, direct.srv.URL, `{}`)
		if rec.Code != http.StatusTooManyRequests || rec.Body.String() != freeUsageBody {
			t.Fatalf("request %d: got %d %q, want Zen's 429 body", i, rec.Code, rec.Body)
		}
	}
	// Once both are marked, each request costs one upstream call, not two.
	if d, f := direct.hits.Load(), alsoSpent.hits.Load(); d+f != 3 {
		t.Errorf("upstream hits = %d direct + %d fallback, want 3 in total", d, f)
	}
}

// With the disguise on, the fallback leg must present the captured Bun hello
// inside the proxy tunnel: the free tier rejects Go's own handshake.
func TestFallbackEgressSendsTheCapturedHello(t *testing.T) {
	helloCh := make(chan []byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if record, err := readTLSRecord(conn); err == nil {
			helloCh <- record
		}
		// Never answer: the handshake fails once the hello is out.
	}()
	socks := proxytest.NewSOCKS5(t, false, ln.Addr().String())
	useEgress(t, "socks5h://"+socks.Addr)
	SetTLSConfig(ZenTLSConfig{Enabled: true})
	exhaustMu.Lock()
	exhaustedUntil[exhaustKey{routeDirect, ""}] = time.Now().Add(time.Hour)
	exhaustMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://zen.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := GenerationClient().Do(req); err == nil {
		resp.Body.Close()
	}

	var sent []byte
	select {
	case sent = <-helloCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no ClientHello reached the fallback proxy's target")
	}
	if got, want := ja3OfHello(t, sent), ja3OfHello(t, opencodeClientHello); got != want {
		t.Errorf("fallback hello JA3 = %s, want the capture's %s", got, want)
	}
	if sni := sniOfHello(t, sent); sni != "zen.test" {
		t.Errorf("fallback hello SNI = %q, want the Zen host", sni)
	}
}

func TestParseFallbackProxy(t *testing.T) {
	for _, ok := range []string{"", "socks5h://192.0.2.10:1080", "socks5://h:1", "http://h:3128"} {
		if _, err := ParseFallbackProxy(ok); err != nil {
			t.Errorf("ParseFallbackProxy(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"192.0.2.10:1080", "socks5h://host", "ftp://h:21", "socks5h://h:1/path",
		"ftp://u:s3cret@h:21", "socks5h://u:s3cret@h", "socks5h://u:s3cret@h:1/path", "socks5h://u:s3cret@h:1/%zz", "socks5h://u:s3cret@h:abc"} {
		_, err := ParseFallbackProxy(bad)
		if err == nil {
			t.Errorf("ParseFallbackProxy(%q) accepted", bad)
			continue
		}
		// The error reaches the panel and the startup log.
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("ParseFallbackProxy(%q) error shows the password: %v", bad, err)
		}
	}
}
