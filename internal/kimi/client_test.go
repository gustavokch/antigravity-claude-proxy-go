package kimi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClient_FetchModels_ParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing Bearer auth, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "kimi-k2-thinking", "display_name": "Kimi K2 Thinking", "context_length": 200000, "max_tokens": 8000},
				{"id": "kimi-k2-0905-preview", "display_name": "Kimi K2 0905", "context_length": 200000, "max_tokens": 8000},
			},
		})
	}))
	defer srv.Close()

	c := &Client{}
	got, err := c.FetchModels(context.Background(), "test-key", srv.URL)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2", len(got))
	}
	if got[0].ID != "kimi-k2-thinking" || got[0].ContextLen != 200000 {
		t.Errorf("first model wrong: %+v", got[0])
	}
}

func TestClient_GetCachedModels_EmptyByDefault(t *testing.T) {
	c := &Client{}
	if got := c.GetCachedModels(); len(got) != 0 {
		t.Fatalf("expected empty cache, got %d models", len(got))
	}
}

func TestClient_GetCachedModels_AfterFetch(t *testing.T) {
	c := &Client{}
	c.cached = []ModelItem{{ID: "kimi-k2-thinking"}}
	if got := c.GetCachedModels(); len(got) != 1 || got[0].ID != "kimi-k2-thinking" {
		t.Fatalf("GetCachedModels returned %+v", got)
	}
}

func TestClient_IsCacheValid(t *testing.T) {
	c := NewClient(5*time.Second, 100*time.Millisecond)
	if c.IsCacheValid() {
		t.Fatal("empty cache should not be valid")
	}

	c.cached = []ModelItem{{ID: "kimi-k2-thinking"}}
	c.fetched = time.Now()
	if !c.IsCacheValid() {
		t.Fatal("fresh cache should be valid")
	}

	c.fetched = time.Now().Add(-200 * time.Millisecond)
	if c.IsCacheValid() {
		t.Fatal("expired cache should not be valid")
	}
}
