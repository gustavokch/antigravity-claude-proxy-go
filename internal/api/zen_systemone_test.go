package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func postZenSystemone(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

// The route exists only while the Zen gateway is enabled: a disabled gateway
// has no upstream to answer, so the endpoint is simply not there (same shape
// as the serveHTTP default 404 branch).
func TestServer_Systemone_ZenDisabledReturns404(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	saveZenTestConfig(t, map[string]any{"enabled": false, "apiKey": "sk-zen-test", "baseUrl": "https://example.invalid"})

	rec := postZenSystemone(t, newZenTestServer(t),
		`{"model":"jev-1.13","state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	// The status alone cannot tell this route's 404 from the serveHTTP default
	// branch's, which also answers 404. Only the handler names the reason.
	if !strings.Contains(rec.Body.String(), "Zen gateway is disabled") {
		t.Errorf("404 body = %s, want it to say the Zen gateway is disabled (a bare not_found would mean the route is missing)", rec.Body.String())
	}
}

// A keyless Zen config must not forward: the request would arrive at the
// gateway unauthenticated. 401, not 500, because the operator can fix it.
func TestServer_Systemone_KeylessZenConfigReturns401(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	saveZenTestConfig(t, map[string]any{"enabled": true, "baseUrl": "https://example.invalid"})

	rec := postZenSystemone(t, newZenTestServer(t),
		`{"model":"jev-1.13","state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "OPENCODE_API_KEY") {
		t.Errorf("401 body = %s, want it to name the missing-key sources", rec.Body.String())
	}
}

// Happy path: the body is forwarded byte-identically to /v1/systemone with the
// resolved key and the OpenCode harness identity, and the upstream JSON is
// returned unchanged. No translation happens on this route.
func TestServer_Systemone_ForwardsBodyToZenGateway(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var gotPath, gotAuth, gotUA string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotUA = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"is_urgent":{"answer":"yes"}},"usage":{"input_tokens":12,"output_tokens":4}}`))
	}))
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{"enabled": true, "apiKey": "sk-zen-test", "baseUrl": upstream.URL})

	body := `{"model":"jev-1.13","state":"Payments failing for 3 days","questions":{"is_urgent":{"type":"noul","instructions":"Does this need urgent attention?"}}}`
	rec := postZenSystemone(t, newZenTestServer(t), body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("upstream path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer sk-zen-test" {
		t.Errorf("Authorization = %q, want Bearer sk-zen-test", gotAuth)
	}
	if gotUA == "" || strings.Contains(gotUA, "claude-cli") {
		t.Errorf("User-Agent = %q, want the OpenCode harness identity", gotUA)
	}
	if string(gotBody) != body {
		t.Errorf("forwarded body = %q, want %q (byte-identical passthrough)", gotBody, body)
	}
	// Byte-identical response passthrough is this route's whole reason for
	// existing: any translation added here would break the systemone contract
	// the client already speaks, so the body must arrive exactly as written.
	if want := `{"answers":{"is_urgent":{"answer":"yes"}},"usage":{"input_tokens":12,"output_tokens":4}}`; rec.Body.String() != want {
		t.Errorf("response = %s, want the upstream body unchanged: %s", rec.Body.String(), want)
	}
}

// The body is capped before it is read, so an oversized systemone request is
// refused locally instead of being buffered in full and forwarded upstream.
func TestServer_Systemone_OversizedBodyReturns413(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var upstreamHit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{"enabled": true, "apiKey": "sk-zen-test", "baseUrl": upstream.URL})

	// maxRequestBody + 1 bytes of payload.
	oversized := `{"model":"jev-1.13","state":"` + strings.Repeat("x", maxRequestBody) + `"}`
	rec := postZenSystemone(t, newZenTestServer(t), oversized)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
	}
	if upstreamHit.Load() {
		t.Error("upstream was contacted for an oversized body, want the cap to refuse it locally")
	}
}

// An unreachable upstream must surface as a structured 502, not a panic or an
// empty 200.
func TestServer_Systemone_UnreachableUpstreamReturns502(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := upstream.URL
	upstream.Close()

	saveZenTestConfig(t, map[string]any{"enabled": true, "apiKey": "sk-zen-test", "baseUrl": deadURL})

	rec := postZenSystemone(t, newZenTestServer(t),
		`{"model":"jev-1.13","state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"api_error"`) {
		t.Errorf("502 body = %s, want an api_error envelope", rec.Body.String())
	}
}

// The route spends the operator's Zen key, so it must sit behind the same proxy
// key as every other /v1 route. An unauthenticated or wrongly keyed call is
// refused before the body is read, and the gateway is never contacted.
func TestServer_Systemone_RequiresProxyKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var upstreamHit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{"enabled": true, "apiKey": "sk-zen-test", "baseUrl": upstream.URL})
	server := newZenTestServer(t)

	for name, header := range map[string]map[string]string{
		"no key":          {},
		"wrong x-api-key": {"x-api-key": "not-the-proxy-key"},
		"wrong bearer":    {"Authorization": "Bearer not-the-proxy-key"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/systemone",
			strings.NewReader(`{"model":"jev-1.13","state":"s","questions":{}}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401; body = %s", name, rec.Code, rec.Body.String())
		}
	}
	if upstreamHit.Load() {
		t.Error("gateway was contacted for an unauthenticated systemone call, want it refused locally")
	}
}
