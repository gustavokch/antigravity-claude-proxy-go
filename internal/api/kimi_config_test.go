package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
)

type kimiMgmtTestBackend struct{}

func (m *kimiMgmtTestBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}
func (m *kimiMgmtTestBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}

func TestServer_HandleKimiConfigGet_RedactsAPIKey(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	// Seed config with a kimi block containing an api key
	seed := `{
		"enabled": true,
		"apiKey": "sk-secret",
		"baseUrl": "https://api.kimi.com/coding",
		"allowlist": [
			{"id": "kimi-k2-thinking", "alias": "k2", "enabled": true}
		]
	}`
	postReq := httptest.NewRequest(http.MethodPost, "/api/kimi/config", strings.NewReader(seed))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, body = %s", postRec.Code, postRec.Body.String())
	}

	// GET should redact
	req := httptest.NewRequest(http.MethodGet, "/api/kimi/config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status string         `json:"status"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, leaked := body.Config["apiKey"]; leaked {
		t.Fatalf("apiKey must be redacted, got %v", body.Config["apiKey"])
	}
	if body.Config["hasApiKey"] != true {
		t.Fatalf("hasApiKey = %v, want true", body.Config["hasApiKey"])
	}
	if body.Config["activeModelCount"] != float64(1) {
		t.Errorf("activeModelCount = %v, want 1", body.Config["activeModelCount"])
	}
}

func TestServer_HandleKimiConfigSave_StoresConfig(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	payload := `{
		"enabled": true,
		"apiKey": "sk-saved",
		"baseUrl": "https://api.kimi.com/coding",
		"allowlist": [
			{"id": "kimi-k2-thinking", "alias": "k2", "enabled": true}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/kimi/config", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := config.Get().Kimi
	if !got.Enabled {
		t.Fatal("Kimi not enabled after save")
	}
	if got.APIKey != "sk-saved" {
		t.Errorf("apiKey not stored, got %q", got.APIKey)
	}
	if len(got.Allowlist) != 1 || got.Allowlist[0].ID != "kimi-k2-thinking" {
		t.Errorf("allowlist not stored, got %+v", got.Allowlist)
	}
}

func TestServer_HandleKimiModelsFetch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "kimi-k2-thinking", "display_name": "Kimi K2", "context_length": 200000, "max_tokens": 8000},
			},
		})
	}))
	defer upstream.Close()

	// Save base URL via config (post the config first)
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	seed := `{
		"enabled": true,
		"apiKey": "sk-fetch-test",
		"baseUrl": "` + upstream.URL + `"
	}`
	postReq := httptest.NewRequest(http.MethodPost, "/api/kimi/config", strings.NewReader(seed))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, body = %s", postRec.Code, postRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/kimi/models/fetch", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Total  int    `json:"total"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 1 || len(body.Models) != 1 || body.Models[0].ID != "kimi-k2-thinking" {
		t.Errorf("unexpected fetch response: total=%d models=%+v", body.Total, body.Models)
	}
}
