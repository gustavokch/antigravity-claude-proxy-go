package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/logger"
	"antigravity-go-proxy/internal/stats"
)

func newTestServerWithManager(t *testing.T) (*Server, *accounts.Manager, *logger.Broadcaster) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	_, _ = config.Load()

	acc := &accounts.Account{
		Email:   "test@example.com",
		Source:  "manual",
		Enabled: true,
		APIKey:  "key-123",
	}
	now := func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }
	mgr, err := accounts.New(accounts.Options{
		Accounts:   []*accounts.Account{acc},
		ConfigPath: filepath.Join(tmpDir, "accounts.json"),
		Strategy:   accounts.StrategyHybrid,
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.SaveToDisk()

	broadcaster := logger.NewBroadcaster(100)
	tracker, _ := stats.NewTracker("")

	server, err := New(Options{
		APIKey:         "test-api-key",
		Credentials:    func(context.Context) (auth.Credentials, error) { return auth.Credentials{AccessToken: "token"}, nil },
		NewUpstream:    func(string) Upstream { return &mockUpstream{} },
		Now:            now,
		AccountManager: mgr,
		Broadcaster:    broadcaster,
		Tracker:        tracker,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, mgr, broadcaster
}

type mockUpstream struct{}

func (m *mockUpstream) LoadCodeAssist(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"cloudaicompanionProject":"proj-123"}`)}, nil
}
func (m *mockUpstream) FetchAvailableModels(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"models":[]}`)}, nil
}
func (m *mockUpstream) StreamGenerateContent(context.Context, any, cloudcode.RequestOptions, func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

type mockRefresherBackend struct {
	acc *accounts.Account
}

func (m *mockRefresherBackend) FetchAvailableModels(context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"models":[]}`)}, nil
}
func (m *mockRefresherBackend) StreamGenerateContent(context.Context, map[string]any, func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}
func (m *mockRefresherBackend) RefreshAccount(ctx context.Context, email string) (*accounts.Account, error) {
	m.acc.Subscription = accounts.Subscription{Tier: "pro", ProjectID: "proj-123"}
	return m.acc, nil
}

func TestManagement_HealthAndLimits(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	t.Run("GET /health", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["status"] != "ok" {
			t.Errorf("expected status ok")
		}
	})

	t.Run("GET /account-limits JSON", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["status"] != "ok" {
			t.Errorf("expected status ok")
		}
		if res["totalAccounts"].(float64) != 1 {
			t.Errorf("expected totalAccounts 1, got %v", res["totalAccounts"])
		}
		accountsList, ok := res["accounts"].([]any)
		if !ok || len(accountsList) == 0 {
			t.Fatal("expected accounts array in response")
		}
		firstAcc, ok := accountsList[0].(map[string]any)
		if !ok {
			t.Fatal("expected account map")
		}
		if _, hasLimits := firstAcc["limits"]; !hasLimits {
			t.Error("expected 'limits' key on account object")
		}
	})

	t.Run("GET /account-limits returns gemini-3.8 family limits", func(t *testing.T) {
		frac := 0.75
		server.accountManager.UpdateAccountQuota("test@example.com", accounts.Quota{
			Models: map[string]accounts.ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &frac,
					ResetTime:         "2026-09-05T12:00:00Z",
				},
				"gemini-3.8-flash-medium": {
					RemainingFraction: &frac,
					ResetTime:         "2026-09-05T12:00:00Z",
				},
				"gemini-3.8-flash-low": {
					RemainingFraction: &frac,
					ResetTime:         "2026-09-05T12:00:00Z",
				},
			},
		}, nil)

		req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res struct {
			Models   []string `json:"models"`
			Accounts []struct {
				Limits map[string]struct {
					Remaining         string   `json:"remaining"`
					RemainingFraction *float64 `json:"remainingFraction"`
					ResetTime         string   `json:"resetTime"`
				} `json:"limits"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Accounts) == 0 {
			t.Fatal("expected at least 1 account")
		}
		limits := res.Accounts[0].Limits
		for _, tier := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-medium", "gemini-3.8-flash-low"} {
			l, ok := limits[tier]
			if !ok {
				t.Fatalf("expected limit for %q, got: %v", tier, limits)
			}
			if l.RemainingFraction == nil || *l.RemainingFraction != 0.75 {
				t.Fatalf("expected 0.75 for %q, got %v", tier, l.RemainingFraction)
			}
			if l.Remaining != "75%" {
				t.Fatalf("expected '75%%' for %q, got %q", tier, l.Remaining)
			}
		}
	})

	t.Run("GET /account-limits table", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/account-limits?format=table", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "test@example.com") {
			t.Errorf("expected table to contain test email: %s", rec.Body.String())
		}
	})

	t.Run("POST /refresh-token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/refresh-token", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("GET /account-limits advertises modelContext", func(t *testing.T) {
		server.backend = &geminiDiscoveryTestBackend{}
		handler := server.Handler()

		req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res struct {
			ModelContext map[string]float64 `json:"modelContext"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.ModelContext == nil {
			t.Fatal("expected modelContext map in response")
		}
		if cw := res.ModelContext["gemini-3.8-flash-high"]; cw != 1048576 {
			t.Errorf("expected modelContext[gemini-3.8-flash-high] = 1048576, got %v", cw)
		}
		if cw := res.ModelContext["gemini-2.5-pro"]; cw != 1048576 {
			t.Errorf("expected modelContext[gemini-2.5-pro] = 1048576, got %v", cw)
		}
		if cw := res.ModelContext["gemini-3.8-flash"]; cw != 1048576 {
			t.Errorf("expected modelContext[gemini-3.8-flash] = 1048576, got %v", cw)
		}
		if cw := res.ModelContext["claude-opus-4-6"]; cw != 250000 {
			t.Errorf("expected modelContext[claude-opus-4-6] = 250000, got %v", cw)
		}
		// claude-opus-4-6-thinking fixture has maxTokens 250000 (positive), so
		// its entry must exist in the map.
		if _, exists := res.ModelContext["claude-opus-4-6-thinking"]; !exists {
			t.Errorf("expected modelContext entry for claude-opus-4-6-thinking: %v", res.ModelContext)
		}
	})

	t.Run("GET /account-limits omits empty quota keys", func(t *testing.T) {
		server.backend = &emptyIDDiscoveryTestBackend{}
		handler := server.Handler()
		server.accountManager.UpdateAccountQuota("test@example.com", accounts.Quota{
			Models: map[string]accounts.ModelQuota{
				"": {ResetTime: "2026-09-05T12:00:00Z"},
			},
		}, nil)

		req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res struct {
			Models []string `json:"models"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Models {
			if m == "" {
				t.Error("models list contains empty model ID from quota/ratelimit keys")
			}
		}
	})

	t.Run("GET /account-limits omits empty model IDs", func(t *testing.T) {
		server.backend = &emptyIDDiscoveryTestBackend{}
		handler := server.Handler()

		req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res struct {
			Models       []string           `json:"models"`
			ModelContext map[string]float64 `json:"modelContext"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Models {
			if m == "" {
				t.Error("models list contains empty model ID")
			}
		}
		if _, exists := res.ModelContext[""]; exists {
			t.Error("modelContext contains empty model ID")
		}
	})
}

func TestManagement_AccountsCRUD(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	t.Run("GET /api/accounts", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		summary, _ := res["summary"].(map[string]any)
		if summary["total"].(float64) != 1 {
			t.Errorf("expected 1 account, got %v", summary["total"])
		}
	})

	t.Run("POST /api/accounts/test@example.com/toggle", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/test@example.com/toggle", strings.NewReader(`{"enabled":false}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("POST /api/accounts/test@example.com/refresh", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/test@example.com/refresh", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("POST /api/accounts/test@example.com/refresh with refresher", func(t *testing.T) {
		acc := &accounts.Account{Email: "test@example.com", Enabled: true, Source: "manual"}
		mgr, _ := accounts.New(accounts.Options{Accounts: []*accounts.Account{acc}})
		refresherServer, _ := New(Options{
			APIKey:         "test-api-key",
			AccountManager: mgr,
			Backend:        &mockRefresherBackend{acc: acc},
		})

		req := httptest.NewRequest(http.MethodPost, "/api/accounts/test@example.com/refresh", nil)
		rec := httptest.NewRecorder()
		refresherServer.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["status"] != "ok" {
			t.Errorf("expected status ok, got %v", res["status"])
		}
	})

	t.Run("POST /api/accounts/import", func(t *testing.T) {
		body := `[{"email":"imported@example.com","refresh_token":"tok-123"}]`
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/import", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("GET /api/accounts/export", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/accounts/export", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})
}

func TestManagement_LogsAndStreaming(t *testing.T) {
	server, _, broadcaster := newTestServerWithManager(t)
	handler := server.Handler()

	broadcaster.Add(logger.LogEntry{
		Timestamp: "2026-08-14T12:00:00Z",
		Level:     "INFO",
		Message:   "hello log test",
	})

	t.Run("GET /api/logs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res struct {
			Status string            `json:"status"`
			Logs   []logger.LogEntry `json:"logs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Logs) != 1 || res.Logs[0].Message != "hello log test" {
			t.Errorf("unexpected logs response: %+v", res)
		}
	})

	t.Run("GET /api/logs/stream with history", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		req := httptest.NewRequest(http.MethodGet, "/api/logs/stream?history=true", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if !strings.Contains(rec.Body.String(), "hello log test") {
			t.Errorf("expected SSE stream to contain historical log")
		}
	})
}

func TestManagement_ConfigAndClaude(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	t.Run("GET /api/config", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("POST /api/event_logging/batch", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/event_logging/batch", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("GET /api/strategy/health", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/strategy/health", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})
}

func TestManagement_StatsHistory(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	_, _ = config.Load()

	tracker, err := stats.NewTracker("")
	if err != nil {
		t.Fatalf("NewTracker failed: %v", err)
	}
	tracker.Track("claude-opus-4-6")

	acc := &accounts.Account{
		Email:   "test@example.com",
		Source:  "manual",
		Enabled: true,
		APIKey:  "key-123",
	}
	now := func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }
	mgr, err := accounts.New(accounts.Options{
		Accounts:   []*accounts.Account{acc},
		ConfigPath: filepath.Join(tmpDir, "accounts.json"),
		Strategy:   accounts.StrategyHybrid,
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.SaveToDisk()

	server, err := New(Options{
		APIKey:         "test-api-key",
		Credentials:    func(context.Context) (auth.Credentials, error) { return auth.Credentials{AccessToken: "token"}, nil },
		NewUpstream:    func(string) Upstream { return &mockUpstream{} },
		Now:            now,
		AccountManager: mgr,
		Tracker:        tracker,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	t.Run("GET /account-limits?includeHistory=true", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/account-limits?includeHistory=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		history, ok := res["history"].(map[string]any)
		if !ok || len(history) == 0 {
			t.Fatalf("expected history in response, got %v", res["history"])
		}
	})

	t.Run("GET /api/stats/history", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/stats/history", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["status"] != "ok" {
			t.Errorf("expected status ok, got %v", res["status"])
		}
		history, ok := res["history"].(map[string]any)
		if !ok || len(history) == 0 {
			t.Fatalf("expected history in response, got %v", res["history"])
		}
	})
}

func TestManagement_CustomEndpointsAndModelMapping(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	t.Run("POST /api/config with customEndpoints", func(t *testing.T) {
		payload := `{"customEndpoints":{"claude-3-opus-20240229":{"url":"https://api.anthropic.com","apiKey":"secret-key"}}}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("GET /account-limits includes customEndpoints", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		ce, ok := res["customEndpoints"].(map[string]any)
		if !ok {
			t.Fatalf("expected customEndpoints in /account-limits response, got %v", res["customEndpoints"])
		}
		opusEp, exists := ce["claude-3-opus-20240229"].(map[string]any)
		if !exists {
			t.Fatalf("expected claude-3-opus-20240229 in customEndpoints, got %v", ce)
		}
		if opusEp["hasApiKey"] != true {
			t.Errorf("expected hasApiKey: true, got %v", opusEp["hasApiKey"])
		}
		if _, hasRawKey := opusEp["apiKey"]; hasRawKey {
			t.Errorf("apiKey secret should not be exposed in /account-limits")
		}
	})

	t.Run("POST /api/models/config and delete", func(t *testing.T) {
		// Add model alias
		payload := `{"modelId":"claude-opus-4-6","config":{"mapping":"claude-3-opus-20240229"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/models/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		// Delete model alias
		delPayload := `{"modelId":"claude-opus-4-6","config":{"delete":true}}`
		delReq := httptest.NewRequest(http.MethodPost, "/api/models/config", strings.NewReader(delPayload))
		delReq.Header.Set("Content-Type", "application/json")
		delRec := httptest.NewRecorder()
		handler.ServeHTTP(delRec, delReq)

		if delRec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", delRec.Code, delRec.Body.String())
		}
		var delRes map[string]any
		if err := json.Unmarshal(delRec.Body.Bytes(), &delRes); err != nil {
			t.Fatal(err)
		}
		if delRes["deleted"] != true {
			t.Errorf("expected deleted: true, got %v", delRes)
		}
	})

	t.Run("POST /api/config updates account selection config and global threshold", func(t *testing.T) {
		payload := `{"accountSelection":{"strategy":"round-robin","weights":{"health":10}},"globalQuotaThreshold":0.15}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		selCfg := server.accountManager.GetSelectionConfig()
		if selCfg.Strategy != "round-robin" {
			t.Errorf("expected strategy round-robin, got %q", selCfg.Strategy)
		}
		if server.accountManager.GlobalQuotaThreshold() != 0.15 {
			t.Errorf("expected globalQuotaThreshold 0.15, got %v", server.accountManager.GlobalQuotaThreshold())
		}
	})
}

func TestManagement_OpenRouterEndpoints(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":             "anthropic/claude-3.7-sonnet",
					"name":           "Claude 3.7 Sonnet",
					"context_length": 200000,
				},
			},
		})
	}))
	defer mockOR.Close()

	t.Run("POST /api/openrouter/config", func(t *testing.T) {
		payload := fmt.Sprintf(`{
			"enabled": true,
			"apiKey": "sk-or-test-key",
			"baseUrl": "%s",
			"appSpoof": {
				"title": "Custom App",
				"categories": "cli-agent",
				"referer": "https://custom.app.ai"
			},
			"responseCache": {
				"enabled": true,
				"ttlSeconds": 600,
				"allowClientOverride": false
			},
			"allowlist": [
				{
					"id": "anthropic/claude-3.7-sonnet",
					"alias": "claude-3-7-openrouter",
					"displayName": "Claude 3.7 Sonnet",
					"contextLength": 200000,
					"enabled": true,
					"responseCache": {
						"enabled": false,
						"ttlSeconds": 120
					}
				}
			]
		}`, mockOR.URL)

		req := httptest.NewRequest(http.MethodPost, "/api/openrouter/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("GET /api/openrouter/config", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/openrouter/config", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		cfg, ok := res["config"].(map[string]any)
		if !ok {
			t.Fatalf("expected config in response, got %v", res)
		}
		if cfg["hasApiKey"] != true {
			t.Errorf("expected hasApiKey = true")
		}
		if _, hasKey := cfg["apiKey"]; hasKey {
			t.Errorf("apiKey secret should be redacted")
		}
		if cfg["activeModelCount"] != float64(1) {
			t.Errorf("expected activeModelCount = 1, got %v", cfg["activeModelCount"])
		}
		appSpoof, ok := cfg["appSpoof"].(map[string]any)
		if !ok {
			t.Fatalf("expected appSpoof map in config, got %v", cfg["appSpoof"])
		}
		if appSpoof["title"] != "Custom App" {
			t.Errorf("expected appSpoof.title = 'Custom App', got %v", appSpoof["title"])
		}
		if appSpoof["categories"] != "cli-agent" {
			t.Errorf("expected appSpoof.categories = 'cli-agent', got %v", appSpoof["categories"])
		}
		if appSpoof["referer"] != "https://custom.app.ai" {
			t.Errorf("expected appSpoof.referer = 'https://custom.app.ai', got %v", appSpoof["referer"])
		}
		respCache, ok := cfg["responseCache"].(map[string]any)
		if !ok {
			t.Fatalf("expected responseCache map in config, got %v", cfg["responseCache"])
		}
		if respCache["enabled"] != true {
			t.Errorf("expected responseCache.enabled = true, got %v", respCache["enabled"])
		}
		if respCache["ttlSeconds"] != float64(600) {
			t.Errorf("expected responseCache.ttlSeconds = 600, got %v", respCache["ttlSeconds"])
		}
		if respCache["allowClientOverride"] != false {
			t.Errorf("expected responseCache.allowClientOverride = false, got %v", respCache["allowClientOverride"])
		}
		allowlist, ok := cfg["allowlist"].([]any)
		if !ok || len(allowlist) == 0 {
			t.Fatalf("expected allowlist in config, got %v", cfg["allowlist"])
		}
		m0, ok := allowlist[0].(map[string]any)
		if !ok {
			t.Fatalf("expected allowlist[0] map, got %v", allowlist[0])
		}
		m0Cache, ok := m0["responseCache"].(map[string]any)
		if !ok {
			t.Fatalf("expected model responseCache map, got %v", m0["responseCache"])
		}
		if m0Cache["enabled"] != false {
			t.Errorf("expected model responseCache.enabled = false, got %v", m0Cache["enabled"])
		}
		if m0Cache["ttlSeconds"] != float64(120) {
			t.Errorf("expected model responseCache.ttlSeconds = 120, got %v", m0Cache["ttlSeconds"])
		}
	})

	t.Run("POST /api/openrouter/models/fetch", func(t *testing.T) {
		payload := fmt.Sprintf(`{"baseUrl":"%s","apiKey":"sk-or-test-key"}`, mockOR.URL)
		req := httptest.NewRequest(http.MethodPost, "/api/openrouter/models/fetch", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res struct {
			Status string `json:"status"`
			Total  int    `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Total != 1 {
			t.Errorf("expected total 1, got %d", res.Total)
		}
	})

	t.Run("POST /api/openrouter/models/fetch without apiKey", func(t *testing.T) {
		payload := fmt.Sprintf(`{"baseUrl":"%s"}`, mockOR.URL)
		req := httptest.NewRequest(http.MethodPost, "/api/openrouter/models/fetch", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res struct {
			Status string `json:"status"`
			Total  int    `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Total != 1 {
			t.Errorf("expected total 1, got %d", res.Total)
		}
	})

	t.Run("GET /api/openrouter/models/cached", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/openrouter/models/cached", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res struct {
			Status string `json:"status"`
			Total  int    `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Total != 1 {
			t.Errorf("expected total 1 cached models, got %d", res.Total)
		}
	})
}

func TestManagement_SaveHeadroomConfig(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	body, _ := json.Marshal(map[string]any{"headroom": map[string]any{
		"enabled": true, "smartCrusher": true, "codeCompressor": true, "liveTurns": 3,
		"ccr":          map[string]any{"enabled": false, "maxStoreMB": 32, "minChunkBytes": 4096},
		"outputShaper": map[string]any{"enabled": true, "verbositySteering": true, "effortRouting": false, "mechanicalThinkingBudget": 2048},
	}})
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := config.Get().Headroom
	if !got.Enabled || got.LiveTurns != 3 || got.CCR.MaxStoreMB != 32 || got.OutputShaper.MechanicalThinkingBudget != 2048 {
		t.Errorf("headroom config not persisted: %+v", got)
	}
	if srv.headroom.GetConfig().LiveTurns != 3 {
		t.Error("live engine was not updated after config save")
	}
}

func TestManagement_ConfigGetExposesHeadroom(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var payload struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := payload.Config["headroom"]; !ok {
		t.Error("GET /api/config must expose the headroom subtree for the settings view")
	}
}

func TestClassifierConfigAPI(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	t.Run("POST /api/config with valid classifier config updates in-memory config and GET returns it", func(t *testing.T) {
		payload := `{
			"classifier": {
				"enabled": true,
				"action": "always_stub",
				"defaultModel": "custom-classifier-model",
				"defaultMaxTokens": 256,
				"defaultVerdict": "<custom>allowed</custom>",
				"defaultThinking": "all clear",
				"variants": {
					"stage1-severity": {
						"maxTokens": 128,
						"cannedVerdict": "<sev>0</sev>"
					}
				}
			}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		// Verify in-memory config updated
		cfg := config.Get()
		if !cfg.Classifier.Enabled {
			t.Errorf("expected classifier.enabled = true")
		}
		if cfg.Classifier.Action != config.ActionAlwaysStub {
			t.Errorf("expected action %q, got %q", config.ActionAlwaysStub, cfg.Classifier.Action)
		}
		if cfg.Classifier.DefaultModel != "custom-classifier-model" {
			t.Errorf("expected defaultModel custom-classifier-model, got %q", cfg.Classifier.DefaultModel)
		}
		if cfg.Classifier.DefaultMaxTokens != 256 {
			t.Errorf("expected defaultMaxTokens 256, got %d", cfg.Classifier.DefaultMaxTokens)
		}
		if v, ok := cfg.Classifier.Variants["stage1-severity"]; !ok || v.MaxTokens != 128 {
			t.Errorf("expected variant stage1-severity with maxTokens 128, got %+v", v)
		}

		// Verify GET /api/config returns it
		getReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		getRec := httptest.NewRecorder()
		handler.ServeHTTP(getRec, getReq)

		if getRec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", getRec.Code, getRec.Body.String())
		}

		var getResp struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
			t.Fatalf("unmarshal GET /api/config: %v", err)
		}
		classifierRaw, ok := getResp.Config["classifier"].(map[string]any)
		if !ok {
			t.Fatalf("expected classifier object in GET /api/config response, got %v", getResp.Config["classifier"])
		}
		if classifierRaw["action"] != "always_stub" {
			t.Errorf("expected action always_stub in GET response, got %v", classifierRaw["action"])
		}
		if classifierRaw["defaultModel"] != "custom-classifier-model" {
			t.Errorf("expected defaultModel custom-classifier-model, got %v", classifierRaw["defaultModel"])
		}
	})

	t.Run("POST /api/config with invalid action returns HTTP 400", func(t *testing.T) {
		payload := `{
			"classifier": {
				"action": "invalid_action"
			}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		errMsg, _ := errResp["error"].(string)
		if !strings.Contains(strings.ToLower(errMsg), "action") {
			t.Errorf("expected error message mentioning action, got: %s", errMsg)
		}
	})

	t.Run("POST /api/config with negative defaultMaxTokens returns HTTP 400", func(t *testing.T) {
		payload := `{
			"classifier": {
				"defaultMaxTokens": -1
			}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		errMsg, _ := errResp["error"].(string)
		if !strings.Contains(strings.ToLower(errMsg), "maxtokens") {
			t.Errorf("expected error message mentioning maxTokens, got: %s", errMsg)
		}
	})

	t.Run("POST /api/config with negative variant maxTokens returns HTTP 400", func(t *testing.T) {
		payload := `{
			"classifier": {
				"variants": {
					"stage1-severity": {
						"maxTokens": -5
					}
				}
			}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		errMsg, _ := errResp["error"].(string)
		if !strings.Contains(strings.ToLower(errMsg), "maxtokens") {
			t.Errorf("expected error message mentioning maxTokens, got: %s", errMsg)
		}
	})

	t.Run("POST /api/config with out of range temperature returns HTTP 400", func(t *testing.T) {
		payload := `{
			"classifier": {
				"defaultTemperature": 2.5
			}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		errMsg, _ := errResp["error"].(string)
		if !strings.Contains(strings.ToLower(errMsg), "temperature") {
			t.Errorf("expected error message mentioning temperature, got: %s", errMsg)
		}
	})
}

func TestManagement_HeadroomStatsEndpoint(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/headroom/stats", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"bytesBefore", "bytesAfter", "requestsCompressed"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("missing %q in headroom stats payload", key)
		}
	}
}

// TestManagement_OpenRouterResponseCacheReplaceSemantics pins the whole-object
// replace contract of POST /api/openrouter/config: a payload that omits
// responseCache erases the stored block. The WebUI panel therefore has to send
// the field back on every save (see saveOpenRouterConfig in models.js).
func TestManagement_OpenRouterResponseCacheReplaceSemantics(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	post := func(t *testing.T, payload string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/openrouter/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		cfg, ok := res["config"].(map[string]any)
		if !ok {
			t.Fatalf("expected config map, got %v", res["config"])
		}
		return cfg
	}

	cfg := post(t, `{
		"enabled": true,
		"apiKey": "sk-or-test-key",
		"responseCache": {"enabled": true, "ttlSeconds": 600, "allowClientOverride": false}
	}`)
	rc, ok := cfg["responseCache"].(map[string]any)
	if !ok {
		t.Fatalf("expected responseCache after save, got %v", cfg["responseCache"])
	}
	if rc["ttlSeconds"] != float64(600) {
		t.Errorf("expected ttlSeconds = 600, got %v", rc["ttlSeconds"])
	}

	// Re-sending the block preserves it.
	cfg = post(t, `{
		"enabled": true,
		"hasApiKey": true,
		"responseCache": {"enabled": true, "ttlSeconds": 600, "allowClientOverride": false}
	}`)
	if rc, ok := cfg["responseCache"].(map[string]any); !ok || rc["ttlSeconds"] != float64(600) {
		t.Errorf("expected responseCache to survive a save that re-sends it, got %v", cfg["responseCache"])
	}

	// Omitting it erases it — this is why the WebUI must ride the field through.
	cfg = post(t, `{"enabled": true, "hasApiKey": true}`)
	if raw, present := cfg["responseCache"]; present {
		if rc, ok := raw.(map[string]any); !ok || len(rc) != 0 {
			t.Errorf("expected responseCache to be cleared by an omitting save, got %v", raw)
		}
	}
}

func TestManagement_AccountLimits_WithClaudeCodeAccounts(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{
			ID:       "cc-test-1",
			Name:     "Claude Work",
			Email:    "claude-work@example.com",
			Token:    "sk-ant-test-123",
			Type:     "oauth",
			Priority: 1,
			Enabled:  true,
			Source:   "oauth",
		},
		{
			ID:       "cc-test-disabled",
			Name:     "Claude Disabled",
			Email:    "claude-disabled@example.com",
			Token:    "sk-ant-test-456",
			Type:     "api_key",
			Priority: 2,
			Enabled:  false,
			Source:   "manual",
		},
	}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{
		{
			ID:      "claude-3-7-sonnet-20250219",
			Aliases: []string{"claude-3-7-sonnet-custom"},
		},
	}
	config.SetForTest(cfg)

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	accounts, ok := res["accounts"].([]any)
	if !ok || len(accounts) < 3 {
		t.Fatalf("expected at least 3 accounts (1 google, 2 claudecode), got %d", len(accounts))
	}

	var foundWork, foundDisabled bool
	for _, accRaw := range accounts {
		acc := accRaw.(map[string]any)
		if acc["provider"] == "claudecode" {
			if acc["email"] == "claude-work@example.com" {
				foundWork = true
				if acc["status"] != "ok" {
					t.Errorf("expected status ok for work account, got %v", acc["status"])
				}
				limits, ok := acc["limits"].(map[string]any)
				if !ok {
					t.Fatalf("expected limits map on claudecode account")
				}
				if sonnet, hasSonnet := limits["claude-3-7-sonnet-20250219"].(map[string]any); !hasSonnet {
					t.Errorf("expected claude-3-7-sonnet-20250219 in claudecode account limits")
			} else {
				// No rate-limit headers received: quota is unknown (N/A),
				// not a false-full 100%.
				if sonnet["remaining"] != "N/A" {
					t.Errorf("expected N/A remaining for account with no quota signal, got %v", sonnet["remaining"])
				}
				if sonnet["remainingFraction"] != nil {
					t.Errorf("expected null remainingFraction, got %v", sonnet["remainingFraction"])
				}
			}
			if alias, hasAlias := limits["claude-3-7-sonnet-custom"].(map[string]any); !hasAlias {
				t.Errorf("expected claude-3-7-sonnet-custom alias in limits")
			} else {
				if alias["remaining"] != "N/A" {
					t.Errorf("expected N/A remaining for alias, got %v", alias["remaining"])
				}
			}
			} else if acc["email"] == "claude-disabled@example.com" {
				foundDisabled = true
				if acc["status"] != "disabled" {
					t.Errorf("expected status disabled, got %v", acc["status"])
				}
				limits, ok := acc["limits"].(map[string]any)
				if !ok {
					t.Fatalf("expected limits map on disabled account")
				}
				if sonnet, hasSonnet := limits["claude-3-7-sonnet-20250219"].(map[string]any); !hasSonnet {
					t.Errorf("expected claude-3-7-sonnet-20250219 in disabled account limits")
				} else {
					if sonnet["remaining"] != "0%" {
						t.Errorf("expected 0%% remaining for disabled account, got %v", sonnet["remaining"])
					}
				}
			}
		}
	}
	if !foundWork {
		t.Errorf("claude-work@example.com account not found in response")
	}
	if !foundDisabled {
		t.Errorf("claude-disabled@example.com account not found in response")
	}
}

func TestManagement_AccountLimits_ClaudeCodeGranularTokenRateLimit(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{
			ID:       "cc-granular-1",
			Name:     "Claude Granular",
			Email:    "claude-granular@example.com",
			Token:    "sk-ant-test-granular",
			Type:     "oauth",
			Priority: 1,
			Enabled:  true,
			Source:   "oauth",
		},
	}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{
		{ID: "claude-3-7-sonnet-20250219"},
	}
	config.SetForTest(cfg)

	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		t.Fatal("expected claude code pool to be created")
	}
	pool.UpdateAccountRateLimits("cc-granular-1", claudecode.RateLimits{
		InputTokensLimit:     1000,
		InputTokensRemaining: 0,
		InputTokensReset:     time.Now().Add(time.Minute),
	})

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	accountsRaw, ok := res["accounts"].([]any)
	if !ok {
		t.Fatalf("expected accounts array")
	}

	var found bool
	for _, accRaw := range accountsRaw {
		acc := accRaw.(map[string]any)
		if acc["provider"] == "claudecode" && acc["email"] == "claude-granular@example.com" {
			found = true
			if acc["status"] != "rate_limited" {
				t.Errorf("expected status rate_limited for exhausted input tokens, got %v", acc["status"])
			}
			limits, ok := acc["limits"].(map[string]any)
			if !ok {
				t.Fatalf("expected limits map")
			}
			sonnet, ok := limits["claude-3-7-sonnet-20250219"].(map[string]any)
			if !ok {
				t.Fatalf("expected sonnet model in limits")
			}
			if sonnet["remaining"] != "0%" {
				t.Errorf("expected 0%% remaining for exhausted input tokens, got %v", sonnet["remaining"])
			}
		}
	}
	if !found {
		t.Fatalf("claude-granular@example.com account not found")
	}
}

func TestManagement_AccountLimits_GoogleModelActive429NotStaleQuota(t *testing.T) {
	server, mgr, _ := newTestServerWithManager(t)
	handler := server.Handler()

	frac := 1.0
	mgr.UpdateAccountQuota("test@example.com", accounts.Quota{
		Models: map[string]accounts.ModelQuota{
			"gemini-3.8-flash-high": {
				RemainingFraction: &frac,
			},
		},
	}, nil)

	sel := mgr.Select("gemini-3.8-flash-high")
	if sel.Account == nil {
		t.Fatalf("expected an account to be selected")
	}
	mgr.MarkRateLimited(sel.Account, "gemini-3.8-flash-high", time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	accountsList, ok := res["accounts"].([]any)
	if !ok || len(accountsList) == 0 {
		t.Fatal("expected accounts array")
	}
	firstAcc := accountsList[0].(map[string]any)
	limits, ok := firstAcc["limits"].(map[string]any)
	if !ok {
		t.Fatalf("expected limits map")
	}
	model, ok := limits["gemini-3.8-flash-high"].(map[string]any)
	if !ok {
		t.Fatalf("expected gemini-3.8-flash-high in limits")
	}
	if model["remaining"] != "0%" {
		t.Errorf("expected 0%% remaining for actively rate-limited model, got %v", model["remaining"])
	}
	if model["resetTime"] == nil {
		t.Errorf("expected non-nil resetTime for actively rate-limited model")
	}
}

type emptyIDDiscoveryTestBackend struct{}

func (m *emptyIDDiscoveryTestBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{
		Body: []byte(`{
			"models":{
				"": {"displayName":"Empty","supportsThinking":false,"maxTokens":12345},
				"gemini-3.8-flash-high":{"displayName":"Gemini 3.8 Flash (High)","supportsThinking":true,"thinkingBudget":16000,"maxTokens":1048576,"maxOutputTokens":65536}
			},
			"agentModelSorts":[{"groups":[{"modelIds":["","gemini-3.8-flash-high"]}]}]
		}`),
	}, nil
}

func (m *emptyIDDiscoveryTestBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}

func TestIsClaudeModel_CommaSeparatedAlias(t *testing.T) {
	// Shape produced by the WebUI importCCDefaults button before the fix.
	allowlist := []claudecode.ModelConfig{{
		ID:    "custom-proxy-model",
		Alias: "my-alias, my-other-alias",
	}}
	if !isClaudeModel("my-alias", allowlist) {
		t.Error("expected first comma-separated alias entry to match")
	}
	if !isClaudeModel("my-other-alias", allowlist) {
		t.Error("expected second comma-separated alias entry to match")
	}
	if isClaudeModel("unrelated-model", allowlist) {
		t.Error("unexpected match for unrelated model")
	}
}

func TestManagement_ClearStatsRoutes(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	server.tracker.Track("claude-opus-4-6")
	server.tracker.Track("claude-opus-4-6")
	server.tracker.Track("gemini-2.5-flash")

	t.Run("DELETE /api/stats/history/{family}/{model}", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/stats/history/claude/opus-4-6", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		cleared, ok := res["cleared"].(map[string]any)
		if !ok {
			t.Fatalf("expected cleared object, got %v", res["cleared"])
		}
		if cleared["models"] != float64(2) || cleared["buckets"] != float64(1) {
			t.Errorf("expected models:2 buckets:1, got %v", cleared)
		}

		history := server.tracker.GetHistory()
		if len(history) == 0 {
			t.Fatal("expected history to survive a single-model clear, got 0 buckets")
		}
		geminiSeen := false
		for hourKey, v := range history {
			hourMap, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("bucket %s is not a map: %T", hourKey, v)
			}
			if _, exists := hourMap["claude"]; exists {
				t.Errorf("bucket %s: expected claude family cleared", hourKey)
			}
			if _, exists := hourMap["gemini"]; exists {
				geminiSeen = true
			}
		}
		if !geminiSeen {
			t.Error("expected gemini family to survive in some bucket")
		}
	})

	t.Run("DELETE /api/stats/history rejects malformed model path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/stats/history/claude", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("DELETE /api/stats/history clears all", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/stats/history", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if history := server.tracker.GetHistory(); len(history) != 0 {
			t.Errorf("expected empty history, got %d buckets", len(history))
		}
	})

	t.Run("DELETE /api/stats/history?headroom=true clears headroom", func(t *testing.T) {
		server.tracker.Track("claude-opus-4-6")
		server.tracker.RecordHeadroom(stats.HeadroomSample{BytesBefore: 100, BytesAfter: 60})

		req := httptest.NewRequest(http.MethodDelete, "/api/stats/history?headroom=true", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if got := server.tracker.GetHeadroomStats(); got.BytesBefore != 0 {
			t.Errorf("expected headroom cleared, got %+v", got)
		}
	})
}

// A pool-wide throttle looks identical to two independent per-account limits
// in the per-account table. Naming it is the whole point of the line.
func TestStatusTextShowsSharedThrottle(t *testing.T) {
	line := formatSharedThrottleLine(map[string]time.Duration{
		"gemini-3.8-flash-high": 252 * time.Second,
	})
	if !strings.Contains(line, "gemini-3.8-flash-high") {
		t.Fatalf("shared throttle line = %q, want the model name", line)
	}
	if !strings.Contains(line, "4m12s") {
		t.Fatalf("shared throttle line = %q, want the remaining wait", line)
	}
}

func TestStatusTextOmitsTheLineWhenNoThrottleIsActive(t *testing.T) {
	if got := formatSharedThrottleLine(map[string]time.Duration{}); got != "" {
		t.Fatalf("shared throttle line = %q, want empty when nothing is throttled", got)
	}
}

// Deterministic ordering keeps the output diffable between polls.
func TestStatusTextSortsThrottledModels(t *testing.T) {
	line := formatSharedThrottleLine(map[string]time.Duration{
		"zeta-model":  time.Minute,
		"alpha-model": time.Minute,
	})
	if strings.Index(line, "alpha-model") > strings.Index(line, "zeta-model") {
		t.Fatalf("shared throttle line = %q, want models in sorted order", line)
	}
}

func TestManagement_AccountLimits_GoogleActive429Normalizes1m(t *testing.T) {
	server, mgr, _ := newTestServerWithManager(t)
	handler := server.Handler()

	frac := 1.0
	mgr.UpdateAccountQuota("test@example.com", accounts.Quota{
		Models: map[string]accounts.ModelQuota{
			"gemini-3.8-flash-high": {
				RemainingFraction: &frac,
			},
			"gemini-3.8-flash-high[1m]": {
				RemainingFraction: &frac,
			},
		},
	}, nil)

	sel := mgr.Select("gemini-3.8-flash-high")
	if sel.Account == nil {
		t.Fatalf("expected an account to be selected")
	}
	// Mark active 429 under standard catalog ID
	mgr.MarkRateLimited(sel.Account, "gemini-3.8-flash-high", 30*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	accountsList, ok := res["accounts"].([]any)
	if !ok || len(accountsList) == 0 {
		t.Fatalf("expected accounts in response")
	}
	firstAcc := accountsList[0].(map[string]any)
	limits := firstAcc["limits"].(map[string]any)

	// Both the standard ID and the [1m] variant must show 0% with an active resetTime.
	// Absence is a failure, not a skip — the shipped assertion silently passed
	// when the key was missing, which is exactly the bug under test.
	for _, m := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-high[1m]"} {
		l, exists := limits[m].(map[string]any)
		if !exists || l == nil {
			t.Errorf("model %q: expected a limits entry, got %v", m, limits[m])
			continue
		}
		if l["remaining"] != "0%" {
			t.Errorf("model %q: expected remaining 0%%, got %v", m, l["remaining"])
		}
		if l["resetTime"] == nil {
			t.Errorf("model %q: expected non-nil resetTime for active 429", m)
		}
	}
}

// Finding 4. The shipped granular-token test never asserted resetTime.
func TestManagement_AccountLimits_GranularResetTimeIsInputTokensReset(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	nowTime := server.now()
	reset := nowTime.Add(90 * time.Second)

	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{ID: "cc-reset-1", Name: "CC Reset", Email: "cc-reset@example.com", Token: "sk-ant-test", Type: "oauth", Priority: 1, Enabled: true, Source: "oauth"},
	}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{{ID: "claude-3-7-sonnet-20250219"}}
	config.SetForTest(cfg)

	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	pool.UpdateAccountRateLimits("cc-reset-1", claudecode.RateLimits{
		InputTokensLimit:     1000,
		InputTokensRemaining: 0,
		InputTokensReset:     reset,
	})

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	want := reset.UTC().Format(time.RFC3339)
	for _, a := range res["accounts"].([]any) {
		am := a.(map[string]any)
		if am["email"] != "cc-reset@example.com" {
			continue
		}
		l := am["limits"].(map[string]any)["claude-3-7-sonnet-20250219"].(map[string]any)
		if l["resetTime"] != want {
			t.Errorf("resetTime = %v, want %v", l["resetTime"], want)
		}
		return
	}
	t.Fatalf("cc-reset@example.com not found")
}

func TestManagement_AccountLimits_ClaudeCodeRetryAfter(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{ID: "cc-1", Name: "CC Account", Enabled: true, Priority: 1},
	}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{
		{ID: "claude-sonnet-4-6"},
		{ID: "claude-opus-4-6"},
	}
	config.SetForTest(cfg)

	nowTime := server.now()
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		t.Fatalf("expected claude code pool to be created")
	}
	pool.UpdateAccountRateLimits("cc-1", claudecode.RateLimits{
		RetryAfter:  45,
		LastUpdated: nowTime,
	})

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	accountsList := res["accounts"].([]any)
	var ccFound map[string]any
	for _, a := range accountsList {
		am := a.(map[string]any)
		if am["provider"] == "claudecode" {
			ccFound = am
			break
		}
	}
	if ccFound == nil {
		t.Fatalf("claudecode account not found")
	}
	limits := ccFound["limits"].(map[string]any)
	expectedReset := nowTime.Add(45 * time.Second).UTC().Format(time.RFC3339)
	for _, m := range []string{"claude-sonnet-4-6", "claude-opus-4-6"} {
		if l, ok := limits[m].(map[string]any); ok && l != nil {
			if l["resetTime"] != expectedReset {
				t.Errorf("model %q: expected resetTime %q, got %q", m, expectedReset, l["resetTime"])
			}
		}
	}
}

func TestManagement_AccountLimits_GoogleAccountRateLimitedStatus(t *testing.T) {
	server, mgr, _ := newTestServerWithManager(t)
	handler := server.Handler()

	frac := 1.0
	mgr.UpdateAccountQuota("test@example.com", accounts.Quota{
		Models: map[string]accounts.ModelQuota{
			"gemini-3.8-flash-high": {
				RemainingFraction: &frac,
			},
		},
	}, nil)

	sel := mgr.Select("gemini-3.8-flash-high")
	if sel.Account == nil {
		t.Fatalf("expected an account to be selected")
	}
	mgr.MarkRateLimited(sel.Account, "gemini-3.8-flash-high", 30*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	accountsList, ok := res["accounts"].([]any)
	if !ok || len(accountsList) == 0 {
		t.Fatalf("expected accounts in response")
	}
	firstAcc := accountsList[0].(map[string]any)
	if firstAcc["status"] != "rate_limited" {
		t.Errorf("expected status rate_limited, got %v", firstAcc["status"])
	}
}

func TestManagement_AccountLimits_ClaudeCodeUnknownQuotaIsNull(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{ID: "cc-unknown-1", Name: "CC Unknown", Email: "cc-unknown@example.com", Token: "sk-ant-test", Type: "oauth", Priority: 1, Enabled: true, Source: "oauth"},
	}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{{ID: "claude-sonnet-4-6"}}
	config.SetForTest(cfg)

	// No rate-limit headers ever received: pool holds the zero value.
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		t.Fatalf("expected claude code pool to be created")
	}

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	for _, a := range res["accounts"].([]any) {
		am := a.(map[string]any)
		if am["email"] != "cc-unknown@example.com" {
			continue
		}
		l := am["limits"].(map[string]any)["claude-sonnet-4-6"].(map[string]any)
		if l["remainingFraction"] != nil {
			t.Errorf("unknown quota must be null, got %v", l["remainingFraction"])
		}
		if l["remaining"] != "N/A" {
			t.Errorf("unknown quota must render N/A, got %v", l["remaining"])
		}
		if _, ok := am["totalRequests"]; !ok {
			t.Errorf("expected totalRequests usage counter, got %v", am)
		}
		return
	}
	t.Fatalf("cc-unknown@example.com not found")
}
