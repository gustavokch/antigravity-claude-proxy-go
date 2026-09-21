package zen

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
			"object": "list",
			"data": []map[string]any{
				{"id": "claude-sonnet-4-6", "object": "model"},
				{"id": "gpt-5.5", "object": "model"},
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
	if got[0].ID != "claude-sonnet-4-6" {
		t.Errorf("first model wrong: %+v", got[0])
	}
}

func TestClient_FetchModels_EmptyKeyWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "claude-sonnet-4-6", "object": "model"},
			},
		})
	}))
	defer srv.Close()

	c := &Client{}
	got, err := c.FetchModels(context.Background(), "", srv.URL)
	if err != nil {
		t.Fatalf("FetchModels with empty key: %v", err)
	}
	if len(got) != 1 || got[0].ID != "claude-sonnet-4-6" {
		t.Errorf("unexpected models: %+v", got)
	}
}

func TestClient_FetchModels_BaseURLVariations(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "claude-sonnet-4-6"},
			},
		})
	}))
	defer srv.Close()

	variations := []string{
		srv.URL,
		srv.URL + "/",
		srv.URL + "/v1",
		srv.URL + "/v1/",
	}

	c := &Client{}
	for _, base := range variations {
		requestedPath = ""
		got, err := c.FetchModels(context.Background(), "", base)
		if err != nil {
			t.Fatalf("FetchModels(%q): %v", base, err)
		}
		if requestedPath != "/v1/models" {
			t.Errorf("base %q led to path %q, want /v1/models", base, requestedPath)
		}
		if len(got) != 1 || got[0].ID != "claude-sonnet-4-6" {
			t.Errorf("base %q failed to parse models: %+v", base, got)
		}
	}
}

func TestClient_GetCachedModels_EmptyByDefault(t *testing.T) {
	c := &Client{}
	if got := c.GetCachedModels(); len(got) != 0 {
		t.Fatalf("expected empty cache, got %d models", len(got))
	}
}

func TestClient_IsCacheValid(t *testing.T) {
	c := NewClient(5*time.Second, 100*time.Millisecond)
	if c.IsCacheValid() {
		t.Fatal("empty cache should not be valid")
	}

	c.cached = []ModelItem{{ID: "claude-sonnet-4-6"}}
	c.fetched = time.Now()
	if !c.IsCacheValid() {
		t.Fatal("fresh cache should be valid")
	}

	c.fetched = time.Now().Add(-200 * time.Millisecond)
	if c.IsCacheValid() {
		t.Fatal("expired cache should not be valid")
	}
}
