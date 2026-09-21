package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/config"
)

func TestServer_HandleZenConfigGet_RedactsAPIKey(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })
	handler := server.Handler()

	seed := `{
		"enabled": true,
		"apiKey": "sk-secret",
		"baseUrl": "https://opencode.ai/zen",
		"allowlist": [
			{"id": "claude-sonnet-4-6", "alias": "sonnet", "enabled": true}
		]
	}`
	postReq := httptest.NewRequest(http.MethodPost, "/api/zen/config", strings.NewReader(seed))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, body = %s", postRec.Code, postRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/zen/config", nil)
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
	if body.Config["keySource"] != "config" {
		t.Errorf("keySource = %v, want config", body.Config["keySource"])
	}
	if body.Config["activeModelCount"] != float64(1) {
		t.Errorf("activeModelCount = %v, want 1", body.Config["activeModelCount"])
	}
}

func TestServer_HandleZenConfigGet_KeySourceEnv(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "sk-zen-env-only")
	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/zen/config", nil)
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
	if body.Config["keySource"] != "env" {
		t.Errorf("keySource = %v, want env", body.Config["keySource"])
	}
	if body.Config["hasApiKey"] != true {
		t.Errorf("hasApiKey = %v, want true (env fallback)", body.Config["hasApiKey"])
	}
	if _, leaked := body.Config["apiKey"]; leaked {
		t.Fatalf("apiKey must never echo the env value, got %v", body.Config["apiKey"])
	}
}

func TestServer_HandleZenConfigSave_RoundTripHonoredOnNextForward(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")

	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })
	handler := server.Handler()

	payload := `{
		"enabled": true,
		"apiKey": "sk-saved",
		"baseUrl": "` + upstream.URL + `",
		"allowlist": [
			{"id": "claude-sonnet-4-6", "alias": "sonnet", "enabled": true}
		]
	}`
	saveReq := httptest.NewRequest(http.MethodPost, "/api/zen/config", strings.NewReader(payload))
	saveReq.Header.Set("Content-Type", "application/json")
	saveRec := httptest.NewRecorder()
	handler.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", saveRec.Code, saveRec.Body.String())
	}

	msgReq := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"sonnet","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgReq.Header.Set("x-api-key", "test-api-key")
	msgRec := httptest.NewRecorder()
	handler.ServeHTTP(msgRec, msgReq)

	if msgRec.Code != 200 {
		t.Fatalf("forward status = %d, want 200; body = %s", msgRec.Code, msgRec.Body.String())
	}
	if gotAuth != "Bearer sk-saved" {
		t.Errorf("Authorization = %q, want Bearer sk-saved (saved round-trip)", gotAuth)
	}
}

func TestServer_HandleZenModelsFetch_SplitsBuckets(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header for empty key, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "claude-sonnet-4-6", "object": "model"},
				{"id": "gpt-5.5", "object": "model"},
			},
		})
	}))
	defer upstream.Close()

	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })
	handler := server.Handler()

	seed := `{"enabled": true, "baseUrl": "` + upstream.URL + `"}`
	postReq := httptest.NewRequest(http.MethodPost, "/api/zen/config", strings.NewReader(seed))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, body = %s", postRec.Code, postRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/zen/models/fetch", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status    string `json:"status"`
		Total     int    `json:"total"`
		Anthropic int    `json:"anthropic"`
		Models    []struct {
			ID string `json:"id"`
		} `json:"models"`
		Other []struct {
			ID string `json:"id"`
		} `json:"other"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 || body.Anthropic != 1 {
		t.Errorf("total=%d anthropic=%d, want 2/1", body.Total, body.Anthropic)
	}
	if len(body.Models) != 1 || body.Models[0].ID != "claude-sonnet-4-6" {
		t.Errorf("anthropic bucket wrong: %+v", body.Models)
	}
	if len(body.Other) != 1 || body.Other[0].ID != "gpt-5.5" {
		t.Errorf("other bucket wrong: %+v", body.Other)
	}
}
