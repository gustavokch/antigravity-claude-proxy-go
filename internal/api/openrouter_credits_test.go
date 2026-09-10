package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
)

// creditsResponse mirrors the JSON payload of GET /api/openrouter/credits.
type creditsResponse struct {
	Status                string `json:"status"`
	Enabled               bool   `json:"enabled"`
	HasApiKey             bool   `json:"hasApiKey"`
	RequiresManagementKey bool   `json:"requiresManagementKey"`
	Error                 string `json:"error"`
	Credits               *struct {
		TotalCredits float64   `json:"total_credits"`
		TotalUsage   float64   `json:"total_usage"`
		Balance      float64   `json:"balance"`
		FetchedAt    time.Time `json:"fetched_at"`
	} `json:"credits"`
}

func getOpenRouterCredits(t *testing.T, server *Server, force bool) (*httptest.ResponseRecorder, creditsResponse) {
	t.Helper()
	url := "/api/openrouter/credits"
	if force {
		url += "?force=true"
	}
	rec := httptest.NewRecorder()
	server.handleOpenRouterCredits(rec, httptest.NewRequest(http.MethodGet, url, nil))

	var resp creditsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response %q: %v", rec.Body.String(), err)
	}
	return rec, resp
}

func TestOpenRouterCredits_Disabled(t *testing.T) {
	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	testCfg := origCfg
	testCfg.OpenRouter.Enabled = false
	config.SetForTest(testCfg)

	server := &Server{logger: slog.Default()}
	rec, resp := getOpenRouterCredits(t, server, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}
	if resp.Status != "ok" || resp.Enabled {
		t.Errorf("expected status=ok enabled=false, got %+v", resp)
	}
	if resp.Credits != nil {
		t.Errorf("expected no credits payload, got %+v", resp.Credits)
	}
}

func TestOpenRouterCredits_MissingAPIKey(t *testing.T) {
	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	testCfg := origCfg
	testCfg.OpenRouter.Enabled = true
	testCfg.OpenRouter.APIKey = "   "
	config.SetForTest(testCfg)

	server := &Server{logger: slog.Default()}
	rec, resp := getOpenRouterCredits(t, server, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}
	if !resp.Enabled || resp.HasApiKey {
		t.Errorf("expected enabled=true hasApiKey=false, got %+v", resp)
	}
	if resp.Credits != nil {
		t.Errorf("expected no credits payload, got %+v", resp.Credits)
	}
}

func TestOpenRouterCredits_RequiresManagementKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/credits" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": {"message": "Management key required"}}`))
	}))
	t.Cleanup(upstream.Close)

	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	testCfg := origCfg
	testCfg.OpenRouter.Enabled = true
	testCfg.OpenRouter.APIKey = "standard-key"
	testCfg.OpenRouter.BaseURL = upstream.URL
	config.SetForTest(testCfg)

	server := &Server{logger: slog.Default()}
	rec, resp := getOpenRouterCredits(t, server, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200 (403 must surface as requiresManagementKey, not an error status)", rec.Code)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %q", resp.Status)
	}
	if !resp.RequiresManagementKey {
		t.Errorf("expected requiresManagementKey=true, got %+v", resp)
	}
	if resp.Credits != nil {
		t.Errorf("expected no credits payload, got %+v", resp.Credits)
	}
	if resp.Error == "" {
		t.Error("expected a human-readable error message")
	}
}

func TestOpenRouterCredits_SuccessAndCache(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/credits" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"total_credits": 100.5, "total_usage": 25.75}}`))
	}))
	t.Cleanup(upstream.Close)

	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	testCfg := origCfg
	testCfg.OpenRouter.Enabled = true
	testCfg.OpenRouter.APIKey = "management-key"
	testCfg.OpenRouter.BaseURL = upstream.URL
	config.SetForTest(testCfg)

	server := &Server{logger: slog.Default()}

	// force=true bypasses any cache left by earlier tests, so the first call
	// always exercises the upstream.
	rec, resp := getOpenRouterCredits(t, server, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}
	if resp.Credits == nil {
		t.Fatalf("expected credits payload, got %+v", resp)
	}
	if resp.Credits.Balance != 74.75 {
		t.Errorf("Balance = %v, expected 74.75", resp.Credits.Balance)
	}
	if resp.Credits.TotalCredits != 100.5 || resp.Credits.TotalUsage != 25.75 {
		t.Errorf("unexpected totals: %+v", resp.Credits)
	}
	if resp.Credits.FetchedAt.IsZero() {
		t.Error("expected fetched_at to be set")
	}

	// force=false now serves from the cache: no additional upstream hit.
	if _, resp := getOpenRouterCredits(t, server, false); resp.Credits == nil || resp.Credits.Balance != 74.75 {
		t.Errorf("expected cached credits payload, got %+v", resp.Credits)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 1 {
		t.Errorf("upstream hits = %d, expected 1 (second call must be served from cache)", got)
	}
}
