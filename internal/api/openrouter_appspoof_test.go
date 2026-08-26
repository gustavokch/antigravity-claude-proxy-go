package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/openrouter"
)

// harnessGateBody mirrors OpenRouter's 403 permission_error body for models
// restricted to attributed agentic harnesses.
const harnessGateBody = `{"type":"error","error":{"type":"permission_error","message":"thinkingmachines/inkling:free is only available on agentic harnesses. Try plugging it into a coding agent or productivity app listed on https://openrouter.ai/apps","error_type":"permission_denied"}}`

func newSpoofTestServer(t *testing.T, openrouterCfg map[string]any, messagesHandler http.HandlerFunc) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
			{ID: "thinkingmachines/inkling:free", Name: "Inkling"},
		}})
	})
	mux.HandleFunc("/endpoints", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"endpoints":[]}}`))
	})
	mux.HandleFunc("/v1/messages", messagesHandler)

	mockOR := httptest.NewServer(mux)
	t.Cleanup(mockOR.Close)

	openrouterCfg["enabled"] = true
	openrouterCfg["apiKey"] = "sk-or-v1-secret-123"
	openrouterCfg["baseUrl"] = mockOR.URL
	openrouterCfg["allowlist"] = []map[string]any{
		{"id": "thinkingmachines/inkling:free", "enabled": true},
	}

	if _, err := config.Save(map[string]any{"openrouter": openrouterCfg}); err != nil {
		t.Fatalf("config save error: %v", err)
	}
	return mockOR
}

func serveSpoofRequest(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"thinkingmachines/inkling:free","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestOpenRouterForwarding_HarnessGateRetriesWithSpoofedApp(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var spoofTitle, spoofTitleLegacy, spoofCategories, spoofReferer, spoofRefererLegacy string
	var calls int32

	newSpoofTestServer(t, map[string]any{}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		title := r.Header.Get("X-OpenRouter-Title")
		if title == "" {
			// Unattributed request — gate it.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(harnessGateBody))
			return
		}
		spoofTitle = title
		spoofTitleLegacy = r.Header.Get("X-Title")
		spoofCategories = r.Header.Get("X-OpenRouter-Categories")
		spoofReferer = r.Header.Get("HTTP-Referer")
		spoofRefererLegacy = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"thinkingmachines/inkling:free"}`))
	})

	rec := serveSpoofRequest(t)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after spoofed retry, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Errorf("expected 2 upstream attempts (original + spoofed retry), got %d", calls)
	}
	if spoofTitle != openrouter.DefaultSpoofAppTitle {
		t.Errorf("expected default spoof title %q, got %q", openrouter.DefaultSpoofAppTitle, spoofTitle)
	}
	if spoofTitleLegacy != openrouter.DefaultSpoofAppTitle {
		t.Errorf("expected default legacy spoof title %q, got %q", openrouter.DefaultSpoofAppTitle, spoofTitleLegacy)
	}
	if spoofCategories != openrouter.DefaultSpoofAppCategories {
		t.Errorf("expected default spoof categories %q, got %q", openrouter.DefaultSpoofAppCategories, spoofCategories)
	}
	if spoofReferer != openrouter.DefaultSpoofAppReferer {
		t.Errorf("expected default spoof referer %q, got %q", openrouter.DefaultSpoofAppReferer, spoofReferer)
	}
	if spoofRefererLegacy != openrouter.DefaultSpoofAppReferer {
		t.Errorf("expected default legacy spoof referer %q, got %q", openrouter.DefaultSpoofAppReferer, spoofRefererLegacy)
	}
}

func TestOpenRouterForwarding_HarnessGateCustomSpoofConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var spoofTitle, spoofTitleLegacy, spoofCategories, spoofReferer, spoofRefererLegacy string

	newSpoofTestServer(t, map[string]any{
		"appSpoof": map[string]any{
			"title":      "My Harness",
			"categories": "cli-agent,cloud-agent",
			"referer":    "https://myharness.ai",
		},
	}, func(w http.ResponseWriter, r *http.Request) {
		title := r.Header.Get("X-OpenRouter-Title")
		if title == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(harnessGateBody))
			return
		}
		spoofTitle = title
		spoofTitleLegacy = r.Header.Get("X-Title")
		spoofCategories = r.Header.Get("X-OpenRouter-Categories")
		spoofReferer = r.Header.Get("HTTP-Referer")
		spoofRefererLegacy = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"thinkingmachines/inkling:free"}`))
	})

	rec := serveSpoofRequest(t)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after spoofed retry, got %d: %s", rec.Code, rec.Body.String())
	}
	if spoofTitle != "My Harness" {
		t.Errorf("expected configured spoof title, got %q", spoofTitle)
	}
	if spoofTitleLegacy != "My Harness" {
		t.Errorf("expected configured legacy spoof title, got %q", spoofTitleLegacy)
	}
	if spoofCategories != "cli-agent,cloud-agent" {
		t.Errorf("expected configured spoof categories, got %q", spoofCategories)
	}
	if spoofReferer != "https://myharness.ai" {
		t.Errorf("expected configured spoof referer, got %q", spoofReferer)
	}
	if spoofRefererLegacy != "https://myharness.ai" {
		t.Errorf("expected configured legacy spoof referer, got %q", spoofRefererLegacy)
	}
}

func TestOpenRouterForwarding_HarnessGatePersistent403(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var calls int32

	newSpoofTestServer(t, map[string]any{}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(harnessGateBody))
	})

	rec := serveSpoofRequest(t)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 passthrough when gate persists, got %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Errorf("expected exactly 2 upstream attempts, got %d", calls)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "only available on agentic harnesses") {
		t.Errorf("expected upstream gate error surfaced to client, got %s", string(body))
	}
}

func TestOpenRouterForwarding_AttributionHeadersPassThrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedReferer, receivedTitle string

	newSpoofTestServer(t, map[string]any{}, func(w http.ResponseWriter, r *http.Request) {
		receivedReferer = r.Header.Get("HTTP-Referer")
		receivedTitle = r.Header.Get("X-OpenRouter-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"thinkingmachines/inkling:free"}`))
	})

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"thinkingmachines/inkling:free","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("HTTP-Referer", "https://client-app.com")
	req.Header.Set("X-OpenRouter-Title", "Client App")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedReferer != "https://client-app.com" {
		t.Errorf("expected passthrough HTTP-Referer 'https://client-app.com', got %q", receivedReferer)
	}
	if receivedTitle != "Client App" {
		t.Errorf("expected passthrough X-OpenRouter-Title 'Client App', got %q", receivedTitle)
	}
}
