package kimi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForwardMessages_DirectorBehavior(t *testing.T) {
	var (
		gotPath       string
		gotAuth       string
		gotBody       []byte
		gotAnthropicV string
		gotAnthropicB string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAnthropicV = r.Header.Get("anthropic-version")
		gotAnthropicB = r.Header.Get("anthropic-beta")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	defer upstream.Close()

	body := []byte(`{"model":"kimi-k2-thinking","messages":[]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "messages-2023-12-15")
	w := httptest.NewRecorder()

	ForwardMessages(w, req, upstream.URL, "sk-test", body)

	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotAnthropicV != "2023-06-01" {
		t.Errorf("anthropic-version not forwarded: %q", gotAnthropicV)
	}
	if gotAnthropicB != "messages-2023-12-15" {
		t.Errorf("anthropic-beta not forwarded: %q", gotAnthropicB)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("upstream body = %q, want %q", gotBody, body)
	}
	if w.Code != 200 {
		t.Errorf("client response code = %d, want 200", w.Code)
	}
}

func TestForwardMessagesWithHook(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantCalled bool
	}{
		{"success calls hook", 200, true},
		{"client error skips hook", 400, false},
		{"server error skips hook", 502, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"type":"message"}`))
			}))
			defer upstream.Close()

			called := 0
			hook := func(status int) { called++ }

			body := []byte(`{"model":"kimi-k2-thinking","messages":[]}`)
			req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
			w := httptest.NewRecorder()

			ForwardMessagesWithHook(w, req, upstream.URL, "sk-test", body, hook)

			if tt.wantCalled && called != 1 {
				t.Errorf("hook called %d times, want 1", called)
			}
			if !tt.wantCalled && called != 0 {
				t.Errorf("hook called %d times, want 0", called)
			}
			if w.Code != tt.status {
				t.Errorf("client code = %d, want %d", w.Code, tt.status)
			}
		})
	}
}

func TestForwardMessages_NilHookDelegates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	body := []byte(`{"model":"m","messages":[]}`)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()

	ForwardMessagesWithHook(w, req, upstream.URL, "sk-test", body, nil)

	if w.Code != 200 {
		t.Errorf("client code = %d, want 200", w.Code)
	}
}

// A count carries what a Kimi messages forward carries: the Bearer
// credential, the identity headers, and the caller's Anthropic headers, and
// never the x-api-key the caller sent the proxy.
func TestCountTokens_SendsTheForwardsHeaders(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeader = r.URL.Path, r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":9}`)
	}))
	defer upstream.Close()

	client := http.Header{}
	client.Set("anthropic-version", "2023-06-01")
	client.Set("anthropic-beta", "token-counting-2024-11-01")
	client.Set("x-api-key", "proxy-key")
	identity := http.Header{}
	identity.Set("User-Agent", "KimiCLI/"+CLIVersion)
	identity.Set("X-Msh-Platform", "kimi_cli")
	body := []byte(`{"model":"kimi-k2","messages":[]}`)

	resp, err := CountTokens(context.Background(), upstream.URL+"/coding/v1", "sk-kimi", body, client, identity)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if gotPath != "/coding/v1/messages/count_tokens" {
		t.Errorf("path = %q, want /coding/v1/messages/count_tokens", gotPath)
	}
	for name, want := range map[string]string{
		"Authorization":     "Bearer sk-kimi",
		"X-Api-Key":         "",
		"User-Agent":        "KimiCLI/" + CLIVersion,
		"X-Msh-Platform":    "kimi_cli",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    "token-counting-2024-11-01",
		"Content-Type":      "application/json",
	} {
		if got := gotHeader.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body = %s, want %s", gotBody, body)
	}
	if !strings.Contains(string(answer), `"input_tokens":9`) {
		t.Errorf("answer = %s, want the upstream's", answer)
	}
}
