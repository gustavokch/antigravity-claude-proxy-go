package openrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveModelSlug(t *testing.T) {
	tests := []struct {
		in          string
		author      string
		slug        string
		expectValid bool
	}{
		{"anthropic/claude-3.5-sonnet", "anthropic", "claude-3.5-sonnet", true},
		{"openrouter/anthropic/claude-3.5-sonnet", "anthropic", "claude-3.5-sonnet", true},
		{"anthropic/claude-3.5-sonnet:free", "anthropic", "claude-3.5-sonnet:free", true},
		{"claude-3.5-sonnet", "", "", false},
		{"anthropic/", "", "", false},
		{"", "", "", false},
		{"anthropic", "", "", false},
	}
	for _, tt := range tests {
		a, s, ok := ResolveModelSlug(tt.in)
		if ok != tt.expectValid {
			t.Errorf("ResolveModelSlug(%q) ok=%v want %v", tt.in, ok, tt.expectValid)
		}
		if ok && (a != tt.author || s != tt.slug) {
			t.Errorf("ResolveModelSlug(%q) = (%q,%q) want (%q,%q)", tt.in, a, s, tt.author, tt.slug)
		}
	}
}

func TestProviderEndpoint_Healthy(t *testing.T) {
	healthy := &ProviderEndpoint{UptimeLast5m: 0.99}
	if !healthy.Healthy() {
		t.Errorf("expected healthy when uptime is high")
	}

	dead := &ProviderEndpoint{UptimeLast5m: 0.1, UptimeLast30m: 0.1, UptimeLast1d: 0.1}
	if dead.Healthy() {
		t.Errorf("expected unhealthy when uptime is low")
	}

	noData := &ProviderEndpoint{}
	if !noData.Healthy() {
		t.Errorf("expected healthy (neutral) when no uptime data")
	}

	errStatus := &ProviderEndpoint{Status: 500}
	if errStatus.Healthy() {
		t.Errorf("expected unhealthy when explicit error status")
	}
}

func TestFlattenEndpointsResponse(t *testing.T) {
	t.Run("canonical", func(t *testing.T) {
		body := []byte(`{"data":{"name":"anthropic/claude","endpoints":[{"provider_name":"anthropic","tag":"beta","context_length":200000,"uptime_last_5m":0.99}]}}`)
		got, err := flattenEndpointsResponse(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].ProviderName != "anthropic" || got[0].ContextLength != 200000 {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("top-level-array", func(t *testing.T) {
		body := []byte(`[{"provider_name":"a"},{"provider_name":"b"}]`)
		got, err := flattenEndpointsResponse(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("expected 2 endpoints, got %d", len(got))
		}
	})

	t.Run("data-array", func(t *testing.T) {
		body := []byte(`{"data":[{"provider_name":"x"}]}`)
		got, err := flattenEndpointsResponse(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].ProviderName != "x" {
			t.Errorf("unexpected result: %+v", got)
		}
	})

	t.Run("canonical-empty-list", func(t *testing.T) {
		body := []byte(`{"data":{"name":"anthropic/claude","endpoints":[]}}`)
		got, err := flattenEndpointsResponse(body)
		if err != nil {
			t.Fatalf("empty catalog is valid, got error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected 0 endpoints, got %d", len(got))
		}
	})

	t.Run("garbage", func(t *testing.T) {
		_, err := flattenEndpointsResponse([]byte("not json"))
		if err == nil {
			t.Errorf("expected error for garbage")
		}
	})

	// Regression: OpenRouter now sends latency_last_30m / throughput_last_30m
	// as percentile objects {"p50":..,"p75":..,"p90":..,"p99":..} instead of
	// bare numbers. float64 fields made the whole-document unmarshal fail and
	// every fetch errored with "unrecognized shape" (live repro 2026-08-30).
	t.Run("canonical-percentile-objects", func(t *testing.T) {
		body := []byte(`{"data":{"name":"z-ai/glm-5.3","endpoints":[
			{"provider_name":"Io Net","tag":"io-net/fp8","context_length":262144,
			 "uptime_last_5m":100,"uptime_last_30m":99.33973589435774,"uptime_last_1d":92.09543750149271,
			 "latency_last_30m":{"p50":2229.5,"p75":4813.5,"p90":10117.7,"p99":26895.49},
			 "throughput_last_30m":{"p50":25,"p75":40,"p90":54,"p99":78}}]}}`)
		got, err := flattenEndpointsResponse(body)
		if err != nil {
			t.Fatalf("percentile-object shape must decode, got: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 endpoint, got %d", len(got))
		}
		if got[0].LatencyLast30mMs != 2229.5 {
			t.Errorf("latency p50 = %v, want 2229.5", got[0].LatencyLast30mMs)
		}
		if got[0].ThroughputLast30mTPS != 25 {
			t.Errorf("throughput p50 = %v, want 25", got[0].ThroughputLast30mTPS)
		}
	})

	t.Run("canonical-mixed-number-and-object", func(t *testing.T) {
		// Legacy bare-number latency must keep decoding (older snapshots).
		body := []byte(`{"data":{"name":"m","endpoints":[
			{"provider_name":"a","latency_last_30m":1500,"throughput_last_30m":42}]}}`)
		got, err := flattenEndpointsResponse(body)
		if err != nil {
			t.Fatalf("legacy numeric shape must decode, got: %v", err)
		}
		if got[0].LatencyLast30mMs != 1500 || got[0].ThroughputLast30mTPS != 42 {
			t.Errorf("unexpected legacy values: %+v", got[0])
		}
	})

	t.Run("canonical-null-percentile", func(t *testing.T) {
		body := []byte(`{"data":{"name":"m","endpoints":[
			{"provider_name":"a","latency_last_30m":null,"throughput_last_30m":null}]}}`)
		got, err := flattenEndpointsResponse(body)
		if err != nil {
			t.Fatalf("null percentile fields must decode, got: %v", err)
		}
		if got[0].LatencyLast30mMs != 0 || got[0].ThroughputLast30mTPS != 0 {
			t.Errorf("expected zero values for null, got %+v", got[0])
		}
	})
}

func TestEndpointsClient_FetchAndCache(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/v1/models/anthropic/claude-3.5-sonnet/endpoints" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"name": "anthropic/claude-3.5-sonnet",
				"endpoints": []map[string]any{
					{
						"provider_name":         "anthropic",
						"tag":                   "default",
						"context_length":        200000,
						"uptime_last_5m":        0.99,
						"uptime_last_30m":       0.98,
						"uptime_last_1d":        0.97,
						"max_completion_tokens": 8192,
					},
					{
						"provider_name":  "azure",
						"context_length": 180000,
						"uptime_last_5m": 0.92,
					},
				},
			},
		})
	}))
	defer server.Close()

	c := NewEndpointsClient(5*time.Second, 10*time.Minute)
	ctx := context.Background()

	eps, err := c.FetchModelEndpoints(ctx, "anthropic/claude-3.5-sonnet", "k", server.URL)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(eps))
	}
	if eps[0].ProviderName != "anthropic" || eps[0].ContextLength != 200000 {
		t.Errorf("first endpoint unexpected: %+v", eps[0])
	}
	if !eps[0].Healthy() || !eps[1].Healthy() {
		t.Errorf("expected both healthy")
	}

	// Cache + dedup
	c.SaveEndpoints("anthropic/claude-3.5-sonnet", server.URL, eps)
	cached, ok := c.GetCachedEndpoints("anthropic/claude-3.5-sonnet", server.URL)
	if !ok || len(cached) != 2 {
		t.Errorf("expected cached, got ok=%v len=%d", ok, len(cached))
	}

	// Resolve hits cache
	resolved, err := c.ResolveModelEndpoints(ctx, "anthropic/claude-3.5-sonnet", "k", server.URL)
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if len(resolved) != 2 {
		t.Errorf("expected 2 resolved, got %d", len(resolved))
	}

	c.Invalidate("anthropic/claude-3.5-sonnet", server.URL)
	if _, ok := c.GetCachedEndpoints("anthropic/claude-3.5-sonnet", server.URL); ok {
		t.Errorf("expected invalidated")
	}
}

func TestEndpointsClient_FetchError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	c := NewEndpointsClient(5*time.Second, time.Minute)
	_, err := c.FetchModelEndpoints(context.Background(), "anthropic/claude", "k", server.URL)
	if err == nil {
		t.Errorf("expected error on 500")
	}
}

func TestEndpointsClient_BadSlug(t *testing.T) {
	c := NewEndpointsClient(time.Second, time.Minute)
	_, err := c.FetchModelEndpoints(context.Background(), "no-slug", "k", "https://x")
	if err == nil {
		t.Errorf("expected error for bad slug")
	}
}

func TestResolveModelEndpoints_WaiterHonorsContext(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"endpoints":[{"provider_name":"p1"}]}}`))
	}))
	defer server.Close()

	c := NewEndpointsClient(5*time.Second, time.Minute)

	// Leader registers the flight and blocks in the upstream handler.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		if _, err := c.ResolveModelEndpoints(context.Background(), "anthropic/m", "k", server.URL); err != nil {
			t.Errorf("leader fetch failed: %v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond)

	// A follower whose own deadline passes must not block for the leader.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.ResolveModelEndpoints(ctx, "anthropic/m", "k", server.URL); err == nil {
		t.Errorf("expected context error while waiting on a blocked flight")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("waiter blocked %v past its own deadline", elapsed)
	}

	// Unblock the leader and let it complete successfully.
	close(release)
	<-leaderDone
}

func TestProviderEndpoint_BlendedUptime(t *testing.T) {
	// Single source for the blend weights shared with the WebUI.
	e := &ProviderEndpoint{UptimeLast5m: 1.0, UptimeLast30m: 1.0, UptimeLast1d: 1.0}
	if got := e.BlendedUptime(); got != 1.0 {
		t.Errorf("all-100%% uptime = %v, want 1", got)
	}
	if got := (&ProviderEndpoint{}).BlendedUptime(); got != 0 {
		t.Errorf("no uptime data = %v, want 0", got)
	}
	if got := (&ProviderEndpoint{UptimeLast5m: 0.4, UptimeLast30m: 0.4, UptimeLast1d: 0.4}).BlendedUptime(); got != 0.4 {
		t.Errorf("uniform 40%% = %v, want 0.4", got)
	}
}
