package claudecode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_SendMessage(t *testing.T) {
	var capturedAuth, capturedVersion, capturedBeta string
	var capturedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedAuth = r.Header.Get("x-api-key")
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

		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, server.Client())

	if err := client.ValidateAccount(context.Background(), "valid-token"); err != nil {
		t.Errorf("expected valid-token to pass validation, got: %v", err)
	}

	if err := client.ValidateAccount(context.Background(), "invalid-token"); err == nil {
		t.Errorf("expected invalid-token to fail validation")
	}
}
