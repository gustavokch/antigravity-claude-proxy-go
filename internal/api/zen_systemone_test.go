package api

import (
	"encoding/json"
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
	var answers map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answers); err != nil {
		t.Fatalf("response not JSON: %v; body = %s", err, rec.Body.String())
	}
	if answers["answers"] == nil {
		t.Errorf("response = %s, want the upstream answers passthrough", rec.Body.String())
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
