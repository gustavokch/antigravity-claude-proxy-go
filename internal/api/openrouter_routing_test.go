package api

import (
	"context"
	"encoding/json"
	"fmt"
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

// routingTestEnv swaps the package-level router/endpoints globals for a clean
// state and restores them on cleanup.
type routingTestEnv struct {
	mockURL string
}

func setupRoutingTestEnv(t *testing.T, mockURL string, endpoints []openrouter.ProviderEndpoint) *routingTestEnv {
	t.Helper()

	prevRouter := openrouter.DefaultRouter
	prevEndpoints := openrouter.DefaultEndpointsClient
	openrouter.DefaultRouter = openrouter.NewProviderRouter(openrouter.DefaultRoutingConfig())
	openrouter.DefaultEndpointsClient = openrouter.NewEndpointsClient(2*time.Second, time.Hour)
	t.Cleanup(func() {
		openrouter.DefaultRouter = prevRouter
		openrouter.DefaultEndpointsClient = prevEndpoints
	})

	// Seed endpoints cache so the request path does not hit the mock for
	// /endpoints, and seed ranks from the same list.
	openrouter.DefaultEndpointsClient.SaveEndpoints("anthropic/claude-3.7-sonnet", mockURL, endpoints)
	openrouter.DefaultRouter.RefreshRanks("anthropic/claude-3.7-sonnet", endpoints)

	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	return &routingTestEnv{mockURL: mockURL}
}

func (e *routingTestEnv) saveConfig(t *testing.T, modelCfg map[string]any, routing map[string]any) {
	t.Helper()
	orCfg := map[string]any{
		"enabled": true,
		"apiKey":  "sk-or-v1-secret-123",
		"baseUrl": e.mockURL,
		"allowlist": []map[string]any{
			{
				"id":            "anthropic/claude-3.7-sonnet",
				"alias":         "claude-3-7-openrouter",
				"displayName":   "Claude 3.7 Sonnet (OpenRouter)",
				"contextLength": 200000,
				"enabled":       true,
			},
		},
		"routing": map[string]any{
			"failureThreshold": 3,
			"retry429Max":      2,
			"backoffBaseMs":    1,
			"backoffCapMs":     5,
			"requestBudgetMs":  10000,
		},
	}
	for k, v := range modelCfg {
		orCfg["allowlist"].([]map[string]any)[0][k] = v
	}
	for k, v := range routing {
		orCfg["routing"].(map[string]any)[k] = v
	}
	if _, err := config.Save(map[string]any{"openrouter": orCfg}); err != nil {
		t.Fatalf("config save error: %v", err)
	}
}

func (e *routingTestEnv) newServer(t *testing.T) *Server {
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
	return server
}

func (e *routingTestEnv) doRequest(t *testing.T, server *Server, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	return e.doRequestWithSession(t, server, ctx, "")
}

func (e *routingTestEnv) doRequestWithSession(t *testing.T, server *Server, ctx context.Context, session string) *httptest.ResponseRecorder {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	reqPayload := `{"model":"claude-3-7-openrouter","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	if session != "" {
		req.Header.Set("x-session-id", session)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestOpenRouterRouting_FailoverOn400(t *testing.T) {
	var bodies = make(chan map[string]any, 8)
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		bodies <- body
		prov, _ := body["provider"].(map[string]any)
		order, _ := prov["order"].([]any)
		if len(order) > 0 && order[0] == "p1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"p1 rejects"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","provider":"p2","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, nil)
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Two attempts: p1 (400) then p2 (200). Both must carry provider injection.
	first := <-bodies
	second := <-bodies
	providerOf := func(b map[string]any) string {
		prov, _ := b["provider"].(map[string]any)
		order, _ := prov["order"].([]any)
		if len(order) == 0 {
			return ""
		}
		s, _ := order[0].(string)
		return s
	}
	if got := providerOf(first); got != "p1" {
		t.Errorf("first attempt provider = %q, want p1", got)
	}
	if got := providerOf(second); got != "p2" {
		t.Errorf("second attempt provider = %q, want p2", got)
	}
	if fb, _ := first["provider"].(map[string]any)["allow_fallbacks"].(bool); fb {
		t.Errorf("allow_fallbacks must be false")
	}
}

func TestOpenRouterRouting_429BackoffSameProvider(t *testing.T) {
	var attempts int32
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&attempts, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","provider":"p1","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, nil)
	server := env.newServer(t)

	const sess = "sess-429"
	rec := env.doRequestWithSession(t, server, nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after 429 backoff, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected 3 attempts (2x429 + success), got %d", got)
	}
	// Sticky provider must still be p1 (429 retries same provider).
	if got, ok := openrouter.DefaultRouter.StickyProvider(sess, "anthropic/claude-3.7-sonnet"); !ok || got != "p1" {
		t.Errorf("sticky provider = (%q, %v), want (p1, true)", got, ok)
	}
}

func TestOpenRouterRouting_429ExhaustedFailsOver(t *testing.T) {
	var p1Attempts, p2Attempts int32
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		prov, _ := body["provider"].(map[string]any)
		order, _ := prov["order"].([]any)
		name, _ := order[0].(string)
		if name == "p1" {
			atomic.AddInt32(&p1Attempts, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		atomic.AddInt32(&p2Attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","provider":"p2","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, map[string]any{"retry429Max": 2})
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via failover, got %d: %s", rec.Code, rec.Body.String())
	}
	// p1: initial + retry429Max retries = 3 attempts; p2: 1 attempt.
	if got := atomic.LoadInt32(&p1Attempts); got != 3 {
		t.Errorf("expected 3 p1 attempts, got %d", got)
	}
	if got := atomic.LoadInt32(&p2Attempts); got != 1 {
		t.Errorf("expected 1 p2 attempt, got %d", got)
	}
}

func TestOpenRouterRouting_FailureThresholdMovesStickiness(t *testing.T) {
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, map[string]any{"failureThreshold": 2})
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup request failed: %d", rec.Code)
	}
	// Sticky on p1 (top ranked). Drive p1 over the failure threshold.
	openrouter.DefaultRouter.RecordResult("anthropic/claude-3.7-sonnet", "p1", false, 0, 0)
	openrouter.DefaultRouter.RecordResult("anthropic/claude-3.7-sonnet", "p1", false, 0, 0)

	// Next select for any session must skip p1.
	got := openrouter.DefaultRouter.Select("fresh-session", "anthropic/claude-3.7-sonnet", openrouter.ProviderOrder{Mode: "auto"})
	if got != "p2" {
		t.Errorf("expected p2 after threshold break, got %q", got)
	}
}

func TestOpenRouterRouting_PinnedProvider(t *testing.T) {
	var gotProvider string
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		prov, _ := body["provider"].(map[string]any)
		order, _ := prov["order"].([]any)
		gotProvider, _ = order[0].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	// Pin to p2 even though p1 ranks higher.
	env.saveConfig(t, map[string]any{"providerMode": "pinned", "pinnedProvider": "p2"}, nil)
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotProvider != "p2" {
		t.Errorf("expected pinned provider p2, got %q", gotProvider)
	}
}

func TestOpenRouterRouting_StreamingPassthroughWithProvider(t *testing.T) {
	sseBody := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"provider\":\"p1\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sseBody)
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, nil)
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != sseBody {
		t.Errorf("stream bytes must pass through unchanged\ngot:  %q\nwant: %q", rec.Body.String(), sseBody)
	}
	stats := openrouter.DefaultRouter.Stats("anthropic/claude-3.7-sonnet")
	if stats["p1"].SuccessCount != 1 {
		t.Errorf("expected 1 success recorded for p1, got %d", stats["p1"].SuccessCount)
	}
}

func TestOpenRouterRouting_ClientDisconnectAbortsRetry(t *testing.T) {
	var attempts int32
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	// Long backoff so the cancel lands before the second attempt.
	env.saveConfig(t, nil, map[string]any{"backoffBaseMs": 200})
	server := env.newServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.doRequest(t, server, ctx)
	}()
	// Wait for the first attempt, then disconnect.
	for atomic.LoadInt32(&attempts) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt after disconnect, got %d", got)
	}
}

func TestManagement_OpenRouterProvidersEndpoint(t *testing.T) {
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models/anthropic/claude-3.7-sonnet/endpoints" {
			_, _ = w.Write([]byte(`{"data":{"name":"anthropic/claude-3.7-sonnet","endpoints":[
				{"provider_name":"p1","context_length":200000,"uptime_last_5m":0.99,"uptime_last_30m":0.99,"uptime_last_1d":0.99,"pricing":{"prompt":"0.000003","completion":"0.000015"}},
				{"provider_name":"p2","context_length":100000,"uptime_last_5m":0.9,"uptime_last_30m":0.9,"uptime_last_1d":0.9}
			]}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockOR.Close()

	// No pre-seeded cache: handler must fetch from the mock itself.
	prevRouter := openrouter.DefaultRouter
	prevEndpoints := openrouter.DefaultEndpointsClient
	openrouter.DefaultRouter = openrouter.NewProviderRouter(openrouter.DefaultRoutingConfig())
	openrouter.DefaultEndpointsClient = openrouter.NewEndpointsClient(2*time.Second, time.Hour)
	t.Cleanup(func() {
		openrouter.DefaultRouter = prevRouter
		openrouter.DefaultEndpointsClient = prevEndpoints
	})

	env := &routingTestEnv{mockURL: mockOR.URL}
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	env.saveConfig(t, map[string]any{"providerMode": "custom", "providerOrder": []string{"p2", "p1"}}, nil)
	server := env.newServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/openrouter/providers?model=anthropic/claude-3.7-sonnet", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["mode"] != "custom" {
		t.Errorf("expected mode custom, got %v", res["mode"])
	}
	providers, ok := res["providers"].([]any)
	if !ok || len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %v", res["providers"])
	}
	first, _ := providers[0].(map[string]any)
	if first["provider"] != "p1" {
		t.Errorf("expected p1 ranked first, got %v", first["provider"])
	}
	if _, hasStats := first["stats"].(map[string]any); !hasStats {
		t.Errorf("expected stats object on provider entry")
	}

	// Missing model param → 400.
	req2 := httptest.NewRequest(http.MethodGet, "/api/openrouter/providers", nil)
	rec2 := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without model param, got %d", rec2.Code)
	}
}

func TestUpstreamClient_HasNoTotalTimeout(t *testing.T) {
	// A total Client.Timeout covers the full body read and kills SSE streams
	// mid-generation. The upstream client must rely on the request context.
	c := openRouterUpstreamClient()
	if c.Timeout != 0 {
		t.Errorf("total Timeout kills long SSE streams, got %v", c.Timeout)
	}
}

func TestRecordOpenRouterMetrics_ResolvesModelPricing(t *testing.T) {
	// Unary path passed raw attempt pricing; when the endpoint price is
	// unknown the model-catalog price must fill in, or cost is undercounted.
	prevClient := openrouter.DefaultClient
	openrouter.DefaultClient = openrouter.NewClient(2*time.Second, time.Hour)
	t.Cleanup(func() { openrouter.DefaultClient = prevClient })
	openrouter.DefaultClient.SaveCache([]openrouter.ModelItem{
		{ID: "anthropic/claude-3.7-sonnet", Pricing: &openrouter.Pricing{Prompt: 0.000003, Completion: 0.000015}},
	})

	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	env := &routingTestEnv{}
	server := env.newServer(t)

	m := server.recordOpenRouterMetrics("anthropic/claude-3.7-sonnet", "sess", openrouter.Pricing{}, server.now(), 1000, 500, 0, 0, "p1")
	want := 1000*0.000003 + 500*0.000015
	if m.CallCost != want {
		t.Errorf("CallCost = %v, want %v (model-catalog pricing must apply)", m.CallCost, want)
	}
}

func TestOpenRouterRouting_BudgetUsesServerClock(t *testing.T) {
	// Deadline checks must use server.now(), not time.Now(): a fake clock
	// advanced past the budget must stop the failover loop after one attempt.
	var attempts int32
	var fakeNow atomic.Value
	fakeNow.Store(time.Now())
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&attempts, 1)
		// Jump the server clock far beyond the budget after the first attempt.
		fakeNow.Store(time.Now().Add(2 * time.Minute))
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, map[string]any{"requestBudgetMs": 60000})
	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     func() time.Time { return fakeNow.Load().(time.Time) },
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	env.doRequest(t, server, nil)
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected loop to stop once budget exceeded (1 attempt), got %d", got)
	}
}

func TestOpenRouterRouting_DeadlineEnforcedOnSlowUpstream(t *testing.T) {
	// A very short budget must abort before a slow upstream response completes.
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_slow","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, map[string]any{"requestBudgetMs": 1})
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected budget to cut off before slow upstream responds (StatusBadGateway), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOpenRouterRouting_429BackoffAbortsOnDisconnect(t *testing.T) {
	// The 429 backoff sleep must unblock on client disconnect instead of
	// holding the handler for the full backoff duration.
	var attempts int32
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, map[string]any{
		"backoffBaseMs":   30000,
		"backoffCapMs":    60000,
		"retry429Max":     5,
		"requestBudgetMs": 120000,
	})
	server := env.newServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		env.doRequest(t, server, ctx)
	}()
	for atomic.LoadInt32(&attempts) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return promptly after disconnect during 429 backoff")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("handler blocked in backoff for %v after disconnect", elapsed)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestOpenRouterRouting_BudgetExemptsActiveStream(t *testing.T) {
	// The request budget bounds time-to-first-byte; once a stream starts it
	// must run to completion (client cancellation aside), or long generations
	// get truncated mid-flight.
	const events = 6
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		for i := 1; i <= events; i++ {
			time.Sleep(30 * time.Millisecond)
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"provider\":\"p1\",\"marker\":%d}\n\n", i)
			flusher.Flush()
		}
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, map[string]any{"requestBudgetMs": 50})
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for i := 1; i <= events; i++ {
		if !strings.Contains(body, fmt.Sprintf("\"marker\":%d", i)) {
			t.Errorf("stream truncated before event %d — budget must not cut active streams\ngot: %q", i, body)
		}
	}
	stats := openrouter.DefaultRouter.Stats("anthropic/claude-3.7-sonnet")
	if stats["p1"].SuccessCount != 1 {
		t.Errorf("expected completed stream to record one success, got %+v", stats["p1"])
	}
}

func TestEffectiveAttemptPricing_FollowsServedProvider(t *testing.T) {
	// Cost must use the SERVED provider's endpoint pricing, not the
	// requested one — OpenRouter may serve a different endpoint than ordered.
	prevRouter := openrouter.DefaultRouter
	openrouter.DefaultRouter = openrouter.NewProviderRouter(openrouter.DefaultRoutingConfig())
	t.Cleanup(func() { openrouter.DefaultRouter = prevRouter })
	openrouter.DefaultRouter.RefreshRanks("m", []openrouter.ProviderEndpoint{
		{ProviderName: "p1", UptimeLast5m: 0.99, Pricing: &openrouter.Pricing{Prompt: 0.000001, Completion: 0.000002}},
		{ProviderName: "p2", UptimeLast5m: 0.9, Pricing: &openrouter.Pricing{Prompt: 0.000003, Completion: 0.000015}},
	})

	requested := openrouter.Pricing{Prompt: 0.000001, Completion: 0.000002}

	got := effectiveAttemptPricing(requested, "m", "p2")
	if got.Prompt != 0.000003 || got.Completion != 0.000015 {
		t.Errorf("served p2 pricing = %+v, want p2 endpoint pricing", got)
	}
	if got := effectiveAttemptPricing(requested, "m", "p1"); got != requested {
		t.Errorf("served p1 pricing = %+v, want requested (identical)", got)
	}
	if got := effectiveAttemptPricing(requested, "m", ""); got != requested {
		t.Errorf("no served provider must fall back to requested pricing, got %+v", got)
	}
	if got := effectiveAttemptPricing(requested, "m", "unknown"); got != requested {
		t.Errorf("unknown served provider must fall back to requested pricing, got %+v", got)
	}
}

func TestCopyUpstreamHeaders_StripsHopByHop(t *testing.T) {
	// Hop-by-hop headers are connection-scoped and must not reach the client;
	// tokens named in Connection die with it. Everything else passes through.
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("Connection", "keep-alive, X-Drop-Token")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Upgrade", "h2c")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("X-Drop-Token", "bye")
	src.Set("X-Custom", "yes")

	dst := http.Header{}
	copyUpstreamHeaders(dst, src)

	for _, name := range []string{"Connection", "Keep-Alive", "Upgrade", "Transfer-Encoding", "X-Drop-Token"} {
		if got := dst.Values(name); len(got) != 0 {
			t.Errorf("%s must be stripped, got %v", name, got)
		}
	}
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("end-to-end headers must pass, got %q", dst.Get("Content-Type"))
	}
	if dst.Get("X-Custom") != "yes" {
		t.Errorf("custom headers must pass, got %q", dst.Get("X-Custom"))
	}
}

func TestOpenRouterRouting_FailoverMovesSticky(t *testing.T) {
	// A failover that succeeds on p2 must move the session's stickiness to
	// p2 — otherwise every later request retries dead p1 first and pays one
	// failed upstream call until the breaker trips.
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		prov, _ := body["provider"].(map[string]any)
		order, _ := prov["order"].([]any)
		if len(order) > 0 && order[0] == "p1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"p1 rejects"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","provider":"p2","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer mockOR.Close()

	endpoints := []openrouter.ProviderEndpoint{
		{ProviderName: "p1", ContextLength: 200000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99},
		{ProviderName: "p2", ContextLength: 100000, UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90},
	}
	env := setupRoutingTestEnv(t, mockOR.URL, endpoints)
	env.saveConfig(t, nil, nil)
	server := env.newServer(t)

	const sess = "sess-move"
	rec := env.doRequestWithSession(t, server, nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via failover, got %d: %s", rec.Code, rec.Body.String())
	}
	if got, ok := openrouter.DefaultRouter.StickyProvider(sess, "anthropic/claude-3.7-sonnet"); !ok || got != "p2" {
		t.Errorf("sticky after failover = (%q, %v), want (p2, true)", got, ok)
	}
}
