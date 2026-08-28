package claudecode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_SendMessage(t *testing.T) {
	var capturedAuth, capturedBearer, capturedVersion, capturedBeta string
	var capturedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedAuth = r.Header.Get("x-api-key")
		capturedBearer = r.Header.Get("Authorization")
		capturedVersion = r.Header.Get("anthropic-version")
		capturedBeta = r.Header.Get("anthropic-beta")

		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)

		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, server.Client())

	customHeaders := make(http.Header)
	customHeaders.Set("anthropic-version", "2023-06-01")
	customHeaders.Set("anthropic-beta", "claude-code-20250219")

	resp, err := client.SendMessage(context.Background(), "sk-ant-test-key", []byte(`{"model":"claude-sonnet-5"}`), customHeaders)
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	defer resp.Body.Close()

	if capturedAuth != "sk-ant-test-key" {
		t.Errorf("expected auth 'sk-ant-test-key', got '%s'", capturedAuth)
	}
	if capturedBearer != "" {
		t.Errorf("expected empty Authorization for api key, got '%s'", capturedBearer)
	}
	if capturedVersion != "2023-06-01" {
		t.Errorf("expected version '2023-06-01', got '%s'", capturedVersion)
	}
	if capturedBeta != "claude-code-20250219" {
		t.Errorf("expected beta 'claude-code-20250219', got '%s'", capturedBeta)
	}
	if capturedBody != `{"model":"claude-sonnet-5"}` {
		t.Errorf("unexpected body: %s", capturedBody)
	}
}

func TestClient_SendMessage_OAuth(t *testing.T) {
	var capturedAuth, capturedBearer, capturedBeta string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("x-api-key")
		capturedBearer = r.Header.Get("Authorization")
		capturedBeta = r.Header.Get("anthropic-beta")

		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_123"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, server.Client())

	customHeaders := make(http.Header)
	customHeaders.Set("anthropic-beta", "claude-code-20250219")

	resp, err := client.SendMessage(context.Background(), "sk-ant-oat01-test-oauth-token", []byte(`{}`), customHeaders)
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	defer resp.Body.Close()

	if capturedAuth != "" {
		t.Errorf("expected empty x-api-key for oauth token, got '%s'", capturedAuth)
	}
	if capturedBearer != "Bearer sk-ant-oat01-test-oauth-token" {
		t.Errorf("expected Authorization 'Bearer sk-ant-oat01-test-oauth-token', got '%s'", capturedBearer)
	}
	if capturedBeta != "claude-code-20250219,oauth-2025-04-20" {
		t.Errorf("expected beta 'claude-code-20250219,oauth-2025-04-20', got '%s'", capturedBeta)
	}
}

func TestClient_ValidateAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") == "valid-token" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5"}]}`))
			return
		}
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-valid-oauth" && r.Header.Get("anthropic-beta") == "oauth-2025-04-20" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5"}]}`))
			return
		}

		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, server.Client())

	if err := client.ValidateAccount(context.Background(), "valid-token"); err != nil {
		t.Errorf("expected valid-token to pass validation, got: %v", err)
	}

	if err := client.ValidateAccount(context.Background(), "sk-ant-oat01-valid-oauth"); err != nil {
		t.Errorf("expected oauth token to pass validation, got: %v", err)
	}

	if err := client.ValidateAccount(context.Background(), "invalid-token"); err == nil {
		t.Errorf("expected invalid-token to fail validation")
	}
}

func TestIsOAuthToken(t *testing.T) {
	cases := []struct {
		token string
		want  bool
	}{
		{"sk-ant-oat01-abc", true},
		{"Bearer sk-ant-oat01-abc", true},
		{"bearer sk-ant-oat01-abc", true},
		{"BEARER sk-ant-oat01-abc", true},
		{"  sk-ant-oat01-abc  ", true},
		{"sk-ant-api01-abc", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsOAuthToken(c.token); got != c.want {
			t.Errorf("IsOAuthToken(%q) = %v, want %v", c.token, got, c.want)
		}
	}
}

func TestApplyAuthHeaders(t *testing.T) {
	cases := []struct {
		name           string
		token          string
		existing       map[string]string
		wantHeaders    map[string]string
		missingHeaders []string
	}{
		{
			name:  "oauth token sets bearer and beta",
			token: "sk-ant-oat01-token",
			wantHeaders: map[string]string{
				"Authorization":  "Bearer sk-ant-oat01-token",
				"anthropic-beta": OAuthBetaHeader,
			},
			missingHeaders: []string{"x-api-key"},
		},
		{
			name:  "lowercase bearer prefix treated as OAuth",
			token: "bearer oat-token-lower",
			wantHeaders: map[string]string{
				"Authorization":  "Bearer oat-token-lower",
				"anthropic-beta": OAuthBetaHeader,
			},
			missingHeaders: []string{"x-api-key"},
		},
		{
			name:  "bearer prefix stripped and normalized",
			token: "Bearer oat-token-123",
			wantHeaders: map[string]string{
				"Authorization": "Bearer oat-token-123",
			},
		},
		{
			name:  "api key uses x-api-key",
			token: "sk-ant-api01-key",
			wantHeaders: map[string]string{
				"x-api-key": "sk-ant-api01-key",
			},
			missingHeaders: []string{"Authorization", "anthropic-beta"},
		},
		{
			name:  "oauth beta merges with existing",
			token: "sk-ant-oat01-token",
			existing: map[string]string{
				"anthropic-beta": "claude-code-20250219",
			},
			wantHeaders: map[string]string{
				"anthropic-beta": "claude-code-20250219," + OAuthBetaHeader,
			},
		},
		{
			name:  "oauth beta not duplicated when already present",
			token: "sk-ant-oat01-token",
			existing: map[string]string{
				"anthropic-beta": "claude-code-20250219," + OAuthBetaHeader,
			},
			wantHeaders: map[string]string{
				"anthropic-beta": "claude-code-20250219," + OAuthBetaHeader,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", "https://example.com", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			for k, v := range tc.existing {
				req.Header.Set(k, v)
			}
			ApplyAuthHeaders(req, tc.token)
			for k, want := range tc.wantHeaders {
				if got := req.Header.Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}
			for _, k := range tc.missingHeaders {
				if got := req.Header.Get(k); got != "" {
					t.Errorf("header %s = %q, want empty", k, got)
				}
			}
		})
	}
}
