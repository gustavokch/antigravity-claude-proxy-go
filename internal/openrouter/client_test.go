package openrouter

import (
	"context"
	"encoding/json"
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
			if !ok || p.Prompt != 0.000003 {
				errChan <- json.Unmarshal([]byte(""), nil)
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
