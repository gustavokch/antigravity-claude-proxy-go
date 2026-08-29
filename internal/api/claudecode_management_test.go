package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
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

func TestClaudeCodeManagement_Accounts_RoutingAndSerialization(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()

	t.Run("empty accounts returns JSON array not null", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/claudecode/accounts", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		if !bytes.Contains(rec.Body.Bytes(), []byte(`"accounts":[]`)) {
			t.Errorf("expected empty JSON array in accounts field, got: %s", rec.Body.String())
		}
	})

	t.Run("delete account with query param", func(t *testing.T) {
		// First add an account
		addBody := `{"id":"acc-query-del","name":"Query Del","token":"sk-ant-test","type":"apikey"}`
		addReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/accounts", bytes.NewReader([]byte(addBody)))
		addReq.Header.Set("Content-Type", "application/json")
		addRec := httptest.NewRecorder()
		handler.ServeHTTP(addRec, addReq)
		if addRec.Code != http.StatusOK {
			t.Fatalf("failed to add account: %s", addRec.Body.String())
		}

		// Delete via query param
		delReq := httptest.NewRequest(http.MethodDelete, "/api/claudecode/accounts?id=acc-query-del", nil)
		delRec := httptest.NewRecorder()
		handler.ServeHTTP(delRec, delReq)
		if delRec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on delete with query param, got %d: %s", delRec.Code, delRec.Body.String())
		}
	})

	t.Run("delete account with path param", func(t *testing.T) {
		// First add an account
		addBody := `{"id":"acc-path-del","name":"Path Del","token":"sk-ant-test","type":"apikey"}`
		addReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/accounts", bytes.NewReader([]byte(addBody)))
		addReq.Header.Set("Content-Type", "application/json")
		addRec := httptest.NewRecorder()
		handler.ServeHTTP(addRec, addReq)
		if addRec.Code != http.StatusOK {
			t.Fatalf("failed to add account: %s", addRec.Body.String())
		}

		// Delete via path param
		delReq := httptest.NewRequest(http.MethodDelete, "/api/claudecode/accounts/acc-path-del", nil)
		delRec := httptest.NewRecorder()
		handler.ServeHTTP(delRec, delReq)
		if delRec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on delete with path param, got %d: %s", delRec.Code, delRec.Body.String())
		}
	})
}

func TestClaudeCodeManagement_AccountRateLimitsAndRefresh(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-requests-limit", "100")
		w.Header().Set("anthropic-ratelimit-requests-remaining", "90")
		w.Header().Set("anthropic-ratelimit-input-tokens-limit", "50000")
		w.Header().Set("anthropic-ratelimit-input-tokens-remaining", "45000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-7-sonnet"}]}`))
	}))
	defer ts.Close()

	oauthMgr := auth.NewClaudeCodeOAuthManager()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-tok",
			"refresh_token": "new-ref",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()
	oauthMgr.SetEndpoints("", tokenSrv.URL, "", nil)

	srv, err := New(Options{
		APIKey: "test-key",
		Credentials: func(ctx context.Context) (auth.Credentials, error) {
			return auth.Credentials{AccessToken: "tok"}, nil
		},
		NewUpstream:        func(s string) Upstream { return nil },
		ClaudeCodeOAuthMgr: oauthMgr,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	config.SetForTest(config.Config{
		ClaudeCode: claudecode.Config{
			Enabled: true,
			BaseURL: ts.URL,
			Accounts: []claudecode.AccountConfig{
				{
					ID:           "acc-test-rl",
					Name:         "Test RL",
					Token:        "tok-test",
					RefreshToken: "ref-test",
					Type:         "oauth",
					Enabled:      true,
				},
			},
		},
	})

	// 1. POST /api/claudecode/accounts/acc-test-rl/ratelimits
	rlReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/accounts/acc-test-rl/ratelimits", nil)
	w := httptest.NewRecorder()
	srv.routeClaudeCodeManagement(w, rlReq, "/api/claudecode/accounts/acc-test-rl/ratelimits", http.MethodPost)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var rlResp struct {
		Status     string                `json:"status"`
		RateLimits claudecode.RateLimits `json:"rate_limits"`
	}
	if err := json.NewDecoder(w.Body).Decode(&rlResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if rlResp.RateLimits.RequestsLimit != 100 || rlResp.RateLimits.RequestsRemaining != 90 {
		t.Errorf("unexpected rate limits: %+v", rlResp.RateLimits)
	}

	// 2. POST /api/claudecode/accounts/acc-test-rl/refresh
	refReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/accounts/acc-test-rl/refresh", nil)
	w2 := httptest.NewRecorder()
	srv.routeClaudeCodeManagement(w2, refReq, "/api/claudecode/accounts/acc-test-rl/refresh", http.MethodPost)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w2.Code, w2.Body.String())
	}
}
