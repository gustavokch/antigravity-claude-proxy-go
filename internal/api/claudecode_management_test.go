package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaudeCodeManagement_ModelsFetch(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()

	t.Run("returns default catalogue when token is empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/claudecode/models", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Status string `json:"status"`
			Models []struct {
				ID          string   `json:"id"`
				DisplayName string   `json:"display_name"`
				Family      string   `json:"family"`
				Aliases     []string `json:"aliases"`
			} `json:"models"`
			Total int `json:"total"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.Status != "ok" {
			t.Errorf("status = %q, want 'ok'", resp.Status)
		}
		if resp.Total == 0 || len(resp.Models) == 0 {
			t.Fatalf("expected non-empty models catalogue")
		}

		var hasFable, hasSonnet bool
		for _, m := range resp.Models {
			if m.ID == "claude-fable-5" {
				hasFable = true
			}
			if m.ID == "claude-sonnet-5" {
				hasSonnet = true
			}
		}
		if !hasFable || !hasSonnet {
			t.Errorf("expected claude-fable-5 and claude-sonnet-5 in discovered models")
		}
	})

	t.Run("fetches from mock upstream when token and baseUrl provided", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"id":           "claude-opus-5",
						"display_name": "Claude Opus 5",
						"created_at":   "2026-05-01T00:00:00Z",
					},
				},
			})
		}))
		defer upstream.Close()

		payload := map[string]string{
			"token":   "sk-ant-oat01-test",
			"baseUrl": upstream.URL,
		}
		bodyBytes, _ := json.Marshal(payload)

		req := httptest.NewRequest(http.MethodPost, "/api/claudecode/models/fetch", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Status string `json:"status"`
			Models []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				Family      string `json:"family"`
			} `json:"models"`
			Total int `json:"total"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.Total != 1 || len(resp.Models) != 1 {
			t.Fatalf("expected 1 model, got %d", resp.Total)
		}
		if resp.Models[0].ID != "claude-opus-5" || resp.Models[0].Family != "opus" {
			t.Errorf("unexpected model 0: %+v", resp.Models[0])
		}
	})
}
