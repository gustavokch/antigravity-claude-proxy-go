package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/proxydial/proxytest"
	"antigravity-go-proxy/internal/zen"
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

// The WebUI route diagram reads Zen's credential state from /api/config, so
// that payload must count the OPENCODE_API_KEY fallback the way routing does.
func TestServer_HandleConfigGet_ZenKeySourceEnv(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "sk-zen-env-only")
	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-zen-env-only") {
		t.Fatalf("/api/config must never echo the env key")
	}
	var body struct {
		Config struct {
			Zen map[string]any `json:"zen"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Config.Zen["keySource"] != "env" {
		t.Errorf("zen.keySource = %v, want env", body.Config.Zen["keySource"])
	}
	if body.Config.Zen["hasApiKey"] != true {
		t.Errorf("zen.hasApiKey = %v, want true (env fallback)", body.Config.Zen["hasApiKey"])
	}
}

// With no key in the config file and no OPENCODE_API_KEY, the route
// diagram must see Zen as keyless.
func TestServer_HandleConfigGet_ZenKeySourceNone(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Config struct {
			Zen map[string]any `json:"zen"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Config.Zen["keySource"] != "none" {
		t.Errorf("zen.keySource = %v, want none", body.Config.Zen["keySource"])
	}
	if body.Config.Zen["hasApiKey"] == true {
		t.Error("zen.hasApiKey = true with no key in config or env")
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

// A Zen panel save must take effect without a restart: the fallback egress it
// sets is used by the very next forward that hits the free usage limit.
func TestServer_HandleZenConfigSave_AppliesFallbackEgress(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	exhausted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Zen: Free usage limit reached."}}`))
	}))
	defer exhausted.Close()
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer fresh.Close()
	socks := proxytest.NewSOCKS5(t, false, strings.TrimPrefix(fresh.URL, "http://"))

	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() {
		config.SetForTest(config.DefaultConfig())
		applyRouterConfig(config.DefaultConfig())
	})
	handler := server.Handler()

	payload := `{"enabled": true, "apiKey": "public", "baseUrl": "` + exhausted.URL + `",
		"fallbackProxy": "socks5h://` + socks.Addr + `",
		"allowlist": [{"id": "claude-sonnet-4-6", "alias": "sonnet", "enabled": true}]}`
	saveRec := httptest.NewRecorder()
	handler.ServeHTTP(saveRec, httptest.NewRequest(http.MethodPost, "/api/zen/config", strings.NewReader(payload)))
	if saveRec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", saveRec.Code, saveRec.Body.String())
	}

	msgReq := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"sonnet","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgReq.Header.Set("x-api-key", "test-api-key")
	msgRec := httptest.NewRecorder()
	handler.ServeHTTP(msgRec, msgReq)
	if msgRec.Code != http.StatusOK {
		t.Fatalf("forward status = %d, want the fallback's 200; body = %s", msgRec.Code, msgRec.Body.String())
	}
}

// Zen keeps a daily free budget per egress IP and model bucket, so one
// model's 429 must not move other models off the direct egress.
func TestZenFreeUsageLimitOfOneModelKeepsOthersDirect(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	const spent, fresh = "claude-opus-4-6", "claude-opus-4-5"
	var mu sync.Mutex
	var directModels, fallbackModels []string
	modelOf := func(r *http.Request) string {
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&probe)
		return probe.Model
	}
	answer := func(w http.ResponseWriter, model string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"` + model + `","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := modelOf(r)
		mu.Lock()
		directModels = append(directModels, model)
		mu.Unlock()
		if model == spent {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Zen: Free usage limit reached."}}`))
			return
		}
		answer(w, model)
	}))
	defer direct.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := modelOf(r)
		mu.Lock()
		fallbackModels = append(fallbackModels, model)
		mu.Unlock()
		answer(w, model)
	}))
	defer fallback.Close()
	socks := proxytest.NewSOCKS5(t, false, strings.TrimPrefix(fallback.URL, "http://"))

	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() {
		config.SetForTest(config.DefaultConfig())
		applyRouterConfig(config.DefaultConfig())
	})
	handler := server.Handler()
	payload := `{"enabled": true, "apiKey": "public", "baseUrl": "` + direct.URL + `",
		"fallbackProxy": "socks5h://` + socks.Addr + `",
		"allowlist": [{"id": "` + spent + `", "enabled": true}, {"id": "` + fresh + `", "enabled": true}]}`
	saveRec := httptest.NewRecorder()
	handler.ServeHTTP(saveRec, httptest.NewRequest(http.MethodPost, "/api/zen/config", strings.NewReader(payload)))
	if saveRec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", saveRec.Code, saveRec.Body.String())
	}

	for _, model := range []string{spent, fresh} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-api-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", model, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(directModels, fresh) || slices.Contains(fallbackModels, fresh) {
		t.Fatalf("%s went direct=%v fallback=%v; want the direct egress, whose budget for it is unspent",
			fresh, directModels, fallbackModels)
	}
}

// The Zen panel toggles save through /api/zen/config. The disguise and the
// TLS fingerprint they switch must take effect on that save, not at the next
// restart.
func TestServer_HandleZenConfigSave_AppliesHarnessLive(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() {
		config.SetForTest(config.DefaultConfig())
		applyRouterConfig(config.DefaultConfig())
	})
	applyRouterConfig(config.Get())

	payload := `{"enabled": true, "harness": {"enabled": false, "tls": true}}`
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/zen/config", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if zen.GetHarnessConfig().Enabled {
		t.Error("harness still enabled after the panel saved enabled:false")
	}
	if !zen.GetTLSConfig().Enabled {
		t.Error("TLS fingerprint still off after the panel saved tls:true")
	}
}

// The harness identity strings become User-Agent and x-opencode-* values. A
// control character in one would fail every Zen request, so both save routes
// refuse it and keep the stored identity.
func TestServer_ZenHarnessControlCharacterRefused(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	t.Cleanup(func() {
		config.SetForTest(config.DefaultConfig())
		applyRouterConfig(config.DefaultConfig())
	})
	handler := server.Handler()

	for _, tc := range []struct{ name, path, body string }{
		{"zen panel", "/api/zen/config", `{"enabled": true, "harness": {"version": "2.0.20\nX-Injected: 1"}}`},
		{"full config", "/api/config", `{"zen": {"harness": {"client": "cli\u0000"}}}`},
		{"non-string", "/api/zen/config", `{"harness": {"project": 7}}`},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body = %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	h := config.Get().Zen.Harness
	if h == nil || h.Version != zen.DefaultVersion || h.Client != zen.DefaultHarnessClient || h.Project != zen.DefaultProject {
		t.Errorf("stored harness = %+v, want the defaults untouched", h)
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
				{"id": "glm-5.3", "object": "model"},
				{"id": "gpt-5.5", "object": "model"},
				{"id": "gemini-3.1-pro", "object": "model"},
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
		Chat      int    `json:"chat"`
		Responses int    `json:"responses"`
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
	if body.Total != 4 || body.Anthropic != 1 || body.Chat != 1 || body.Responses != 1 {
		t.Errorf("total=%d anthropic=%d chat=%d responses=%d, want 4/1/1/1", body.Total, body.Anthropic, body.Chat, body.Responses)
	}
	usable := map[string]bool{}
	for _, m := range body.Models {
		usable[m.ID] = true
	}
	if len(body.Models) != 3 || !usable["claude-sonnet-4-6"] || !usable["glm-5.3"] || !usable["gpt-5.5"] {
		t.Errorf("forwardable bucket = %+v, want claude-sonnet-4-6, glm-5.3 and gpt-5.5", body.Models)
	}
	if len(body.Other) != 1 || body.Other[0].ID != "gemini-3.1-pro" {
		t.Errorf("other bucket wrong: %+v", body.Other)
	}
}
