package zen

import (
	"io"
	"maps"
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

func TestParseFallbackProxy(t *testing.T) {
	for _, ok := range []string{"", "socks5h://100.72.157.22:1080", "socks5://h:1", "http://h:3128"} {
		if _, err := ParseFallbackProxy(ok); err != nil {
			t.Errorf("ParseFallbackProxy(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"100.72.157.22:1080", "socks5h://host", "ftp://h:21", "socks5h://h:1/path",
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
