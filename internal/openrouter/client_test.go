package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "https://openrouter.ai/api"},
		{"https://openrouter.ai/api", "https://openrouter.ai/api"},
		{"https://openrouter.ai/api/", "https://openrouter.ai/api"},
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api"},
		{"https://openrouter.ai/api/v1/", "https://openrouter.ai/api"},
		{"http://custom-openrouter.internal", "http://custom-openrouter.internal"},
		{"http://custom-openrouter.internal/v1", "http://custom-openrouter.internal"},
	}

	for _, tt := range tests {
		got := NormalizeBaseURL(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeBaseURL(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestFetchAvailableModels_Success(t *testing.T) {
	maxTokensVal := 128000
	mockModels := []ModelItem{
		{
			ID:            "anthropic/claude-3.7-sonnet",
			Name:          "Anthropic: Claude 3.7 Sonnet",
			Description:   "Claude 3.7 Sonnet with hybrid reasoning",
			ContextLength: 200000,
			TopProvider:   &TopProvider{MaxCompletionTokens: &maxTokensVal},
		},
		{
			ID:                  "openai/gpt-4o",
			Name:                "OpenAI: GPT-4o",
			Description:         "Omni model by OpenAI",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("expected path /v1/models, got %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			t.Errorf("expected Bearer test-api-key, got %s", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ModelsResponse{Data: mockModels})
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)
	models, err := client.FetchAvailableModels(context.Background(), "test-api-key", server.URL)
	if err != nil {
		t.Fatalf("FetchAvailableModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "anthropic/claude-3.7-sonnet" {
		t.Errorf("expected model ID anthropic/claude-3.7-sonnet, got %s", models[0].ID)
	}
	if models[0].GetMaxOutputTokens() != 128000 {
		t.Errorf("expected max output tokens 128000, got %d", models[0].GetMaxOutputTokens())
	}
	if models[1].GetMaxOutputTokens() != 16384 {
		t.Errorf("expected max output tokens 16384, got %d", models[1].GetMaxOutputTokens())
	}

	// Verify cached
	cached := client.GetCachedModels()
	if len(cached) != 2 {
		t.Fatalf("expected 2 cached models, got %d", len(cached))
	}
	if !client.IsCacheValid() {
		t.Errorf("expected cache to be valid")
	}
}

func TestFetchAvailableModels_AuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"message": "Invalid API Key"}}`))
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)
	_, err := client.FetchAvailableModels(context.Background(), "invalid-key", server.URL)
	if err == nil {
		t.Fatalf("expected error on unauthorized response, got nil")
	}
}

func TestCaching(t *testing.T) {
	client := NewClient(5*time.Second, 50*time.Millisecond)
	if client.IsCacheValid() {
		t.Errorf("empty cache should not be valid")
	}

	items := []ModelItem{
		{ID: "meta-llama/llama-3.3-70b-instruct", Name: "Llama 3.3 70B"},
	}
	client.SaveCache(items)

	if !client.IsCacheValid() {
		t.Errorf("cache should be valid immediately after SaveCache")
	}

	cached := client.GetCachedModels()
	if len(cached) != 1 || cached[0].ID != "meta-llama/llama-3.3-70b-instruct" {
		t.Errorf("unexpected cached items: %+v", cached)
	}

	// Wait for TTL expiration
	time.Sleep(60 * time.Millisecond)
	if client.IsCacheValid() {
		t.Errorf("cache should be invalid after TTL expired")
	}
}

func TestResolveModelPricing_ColdStartAndCacheHit(t *testing.T) {
	fetchCount := 0
	mockModels := []ModelItem{
		{
			ID:            "qwen/qwen3.8-max",
			CanonicalSlug: "qwen/qwen3.8-max-20260803",
			Name:          "Qwen 3.8 Max",
			Pricing: &Pricing{
				Prompt:          0.000002,
				Completion:      0.000006,
				InputCacheRead:  0.00000025,
				InputCacheWrite: 0.0000025,
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ModelsResponse{Data: mockModels})
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)

	// 1. Cold start: Resolve pricing for model ID
	pricing, ok := client.ResolveModelPricing(context.Background(), "qwen/qwen3.8-max", "test-key", server.URL)
	if !ok {
		t.Fatalf("expected pricing to be resolved on cold start")
	}
	if pricing.Prompt != 0.000002 || pricing.Completion != 0.000006 {
		t.Errorf("unexpected pricing values: %+v", pricing)
	}
	if fetchCount != 1 {
		t.Errorf("expected 1 fetch, got %d", fetchCount)
	}

	// 2. Cache hit: Resolve pricing again (should not trigger new fetch)
	pricing2, ok2 := client.ResolveModelPricing(context.Background(), "qwen/qwen3.8-max", "test-key", server.URL)
	if !ok2 {
		t.Fatalf("expected pricing to be resolved from cache")
	}
	if pricing2.Prompt != 0.000002 {
		t.Errorf("unexpected pricing2: %+v", pricing2)
	}
	if fetchCount != 1 {
		t.Errorf("expected still 1 fetch on cache hit, got %d", fetchCount)
	}

	// 3. Resolve by canonical slug
	pricing3, ok3 := client.ResolveModelPricing(context.Background(), "qwen/qwen3.8-max-20260803", "test-key", server.URL)
	if !ok3 {
		t.Fatalf("expected pricing to be resolved by canonical slug")
	}
	if pricing3.Completion != 0.000006 {
		t.Errorf("unexpected pricing3: %+v", pricing3)
	}
	if fetchCount != 1 {
		t.Errorf("expected still 1 fetch on slug match, got %d", fetchCount)
	}
}

func TestResolveModelPricing_SingleflightConcurrency(t *testing.T) {
	fetchCount := 0
	mockModels := []ModelItem{
		{
			ID: "anthropic/claude-3.7-sonnet",
			Pricing: &Pricing{
				Prompt:     0.000003,
				Completion: 0.000015,
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond) // simulate latency
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ModelsResponse{Data: mockModels})
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)

	// Spawn 10 concurrent requests on cold cache
	concurrency := 10
	errChan := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			p, ok := client.ResolveModelPricing(context.Background(), "anthropic/claude-3.7-sonnet", "key", server.URL)
			if !ok {
				errChan <- errors.New("pricing resolution returned not-ok")
				return
			}
			if p.Prompt != 0.000003 {
				errChan <- fmt.Errorf("prompt price = %v, want 0.000003", p.Prompt)
				return
			}
			errChan <- nil
		}()
	}

	for i := 0; i < concurrency; i++ {
		if err := <-errChan; err != nil {
			t.Errorf("concurrent pricing resolution failed")
		}
	}

	if fetchCount != 1 {
		t.Errorf("expected singleflight to ensure exactly 1 fetch, got %d", fetchCount)
	}
}

func TestResolveModelPricing_CallerContextCancellation(t *testing.T) {
	fetchCount := 0
	mockModels := []ModelItem{
		{
			ID: "anthropic/claude-3.7-sonnet",
			Pricing: &Pricing{
				Prompt:     0.000003,
				Completion: 0.000015,
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // simulate upstream network latency
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ModelsResponse{Data: mockModels})
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)

	// Caller 1 starts fetch with a context that is cancelled quickly
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel1()

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = client.ResolveModelPricing(ctx1, "anthropic/claude-3.7-sonnet", "key", server.URL)
	}()

	<-started
	// Allow small time for caller 1 to establish singleflight inFlight entry
	time.Sleep(10 * time.Millisecond)

	// Caller 2 joins the in-flight fetch with a valid context
	p2, ok2 := client.ResolveModelPricing(context.Background(), "anthropic/claude-3.7-sonnet", "key", server.URL)
	if !ok2 {
		t.Fatalf("expected caller 2 to resolve pricing despite caller 1 context cancellation")
	}
	if p2.Prompt != 0.000003 {
		t.Errorf("unexpected pricing for caller 2: %+v", p2)
	}
	if fetchCount != 1 {
		t.Errorf("expected exactly 1 fetch, got %d", fetchCount)
	}
}

// TestGetModelLimits covers both returns and the miss path. The context length
// is read by /v1/models discovery, the max output tokens by the max_tokens
// policy, and ok distinguishes "model not in the catalog" from "catalog knows
// the model but states no limits" — the callers branch differently on each.
func TestGetModelLimits(t *testing.T) {
	maxCompletion := 65536
	client := NewClient(time.Second, time.Hour)
	client.SaveCache([]ModelItem{
		{
			ID:            "vendor/model-a",
			CanonicalSlug: "vendor/model-a-2026",
			ContextLength: 1048576,
			TopProvider:   &TopProvider{MaxCompletionTokens: &maxCompletion},
		},
		{ID: "vendor/model-b", ContextLength: 200000},
	})

	// Same tolerant matching as GetModelPricing: exact ID, canonical slug,
	// "openrouter/" prefix, and any casing all resolve to one entry.
	for _, requested := range []string{
		"vendor/model-a",
		"vendor/model-a-2026",
		"openrouter/vendor/model-a",
		"Vendor/Model-A",
	} {
		contextLen, maxOut, ok := client.GetModelLimits(requested)
		if !ok {
			t.Errorf("GetModelLimits(%q): ok = false, expected a cache hit", requested)
			continue
		}
		if contextLen != 1048576 {
			t.Errorf("GetModelLimits(%q): contextLength = %d, expected 1048576", requested, contextLen)
		}
		if maxOut != maxCompletion {
			t.Errorf("GetModelLimits(%q): maxOutputTokens = %d, expected %d", requested, maxOut, maxCompletion)
		}
	}

	// A matched entry with no stated output cap reports 0 with ok=true, so
	// callers can tell it apart from an unknown model.
	contextLen, maxOut, ok := client.GetModelLimits("vendor/model-b")
	if !ok || contextLen != 200000 || maxOut != 0 {
		t.Errorf("GetModelLimits(vendor/model-b) = (%d, %d, %v), expected (200000, 0, true)", contextLen, maxOut, ok)
	}

	// A model absent from the catalog reports the zero value with ok=false.
	if contextLen, maxOut, ok := client.GetModelLimits("vendor/absent"); ok || contextLen != 0 || maxOut != 0 {
		t.Errorf("GetModelLimits(vendor/absent) = (%d, %d, %v), expected (0, 0, false)", contextLen, maxOut, ok)
	}

	// An empty model ID never matches, even against a populated cache.
	if _, _, ok := client.GetModelLimits(""); ok {
		t.Error("GetModelLimits(\"\"): ok = true, expected no match for an empty model ID")
	}
}

func TestFetchCredits_Success(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/credits" {
			t.Errorf("expected path /v1/credits, got %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"total_credits": 100.5, "total_usage": 25.75}}`))
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)
	info, err := client.FetchCredits(context.Background(), "test-api-key", server.URL+"/v1")
	if err != nil {
		t.Fatalf("FetchCredits failed: %v", err)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Errorf("Authorization header = %q, expected %q", gotAuth, "Bearer test-api-key")
	}
	if info.TotalCredits != 100.5 || info.TotalUsage != 25.75 {
		t.Errorf("unexpected totals: %+v", info)
	}
	if info.Balance != 74.75 {
		t.Errorf("Balance = %v, expected 74.75", info.Balance)
	}
	if info.FetchedAt.IsZero() {
		t.Error("FetchedAt not set")
	}

	// Fetch stores the result in the credits cache.
	if cached := client.getCachedCredits("test-api-key", server.URL+"/v1"); cached == nil || cached.Balance != 74.75 {
		t.Errorf("expected cached credits with balance 74.75, got %+v", cached)
	}
}

func TestFetchCredits_403ManagementKeyRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": {"message": "Management key required"}}`))
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)
	_, err := client.FetchCredits(context.Background(), "standard-key", server.URL)
	if !errors.Is(err, ErrManagementKeyRequired) {
		t.Fatalf("expected ErrManagementKeyRequired, got %v", err)
	}
	// A rejected fetch must not populate the cache.
	if cached := client.getCachedCredits("standard-key", server.URL); cached != nil {
		t.Errorf("expected empty credits cache after 403, got %+v", cached)
	}
}

func TestResolveCredits_CacheTTLAndForce(t *testing.T) {
	fetchCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"total_credits": 50, "total_usage": 10}}`))
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)
	client.creditsCacheTTL = 50 * time.Millisecond

	if _, err := client.ResolveCredits(context.Background(), "key", server.URL, false); err != nil {
		t.Fatalf("first ResolveCredits failed: %v", err)
	}
	// Warm cache: second call must not hit the network.
	if _, err := client.ResolveCredits(context.Background(), "key", server.URL, false); err != nil {
		t.Fatalf("cached ResolveCredits failed: %v", err)
	}
	if fetchCount != 1 {
		t.Fatalf("expected 1 fetch with warm cache, got %d", fetchCount)
	}

	// force=true bypasses the cache.
	if _, err := client.ResolveCredits(context.Background(), "key", server.URL, true); err != nil {
		t.Fatalf("forced ResolveCredits failed: %v", err)
	}
	if fetchCount != 2 {
		t.Fatalf("expected 2 fetches after force, got %d", fetchCount)
	}

	// Expired TTL allows a refresh.
	time.Sleep(60 * time.Millisecond)
	if _, err := client.ResolveCredits(context.Background(), "key", server.URL, false); err != nil {
		t.Fatalf("post-TTL ResolveCredits failed: %v", err)
	}
	if fetchCount != 3 {
		t.Fatalf("expected 3 fetches after TTL expiry, got %d", fetchCount)
	}
}

func TestResolveCredits_SingleflightConcurrency(t *testing.T) {
	fetchCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond) // simulate latency
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"total_credits": 80, "total_usage": 30}}`))
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)
	concurrency := 10
	errChan := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			info, err := client.ResolveCredits(context.Background(), "key", server.URL, false)
			if err != nil {
				errChan <- err
				return
			}
			if info.Balance != 50 {
				errChan <- fmt.Errorf("balance = %v, want 50", info.Balance)
				return
			}
			errChan <- nil
		}()
	}
	for i := 0; i < concurrency; i++ {
		if err := <-errChan; err != nil {
			t.Errorf("concurrent credits resolution failed: %v", err)
		}
	}
	if fetchCount != 1 {
		t.Errorf("expected singleflight to ensure exactly 1 fetch, got %d", fetchCount)
	}
}

func TestResolveCredits_APIKeyIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch auth {
		case "Bearer key-a":
			_, _ = w.Write([]byte(`{"data": {"total_credits": 100, "total_usage": 10}}`))
		case "Bearer key-b":
			_, _ = w.Write([]byte(`{"data": {"total_credits": 50, "total_usage": 5}}`))
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	client := NewClient(5*time.Second, 10*time.Minute)

	infoA, err := client.ResolveCredits(context.Background(), "key-a", server.URL, false)
	if err != nil {
		t.Fatalf("ResolveCredits key-a failed: %v", err)
	}
	if infoA.Balance != 90 {
		t.Errorf("key-a balance = %v, expected 90", infoA.Balance)
	}

	// key-b must not hit key-a's cached credits even with same base URL
	infoB, err := client.ResolveCredits(context.Background(), "key-b", server.URL, false)
	if err != nil {
		t.Fatalf("ResolveCredits key-b failed: %v", err)
	}
	if infoB.Balance != 45 {
		t.Errorf("key-b balance = %v, expected 45 (got cached value from key-a)", infoB.Balance)
	}
}
