package zen

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

// requireHarnessHeaders asserts the outgoing header map carries the genuine
// OpenCode harness identity.
func requireHarnessHeaders(t *testing.T, hdr http.Header, wantUA string) {
	t.Helper()
	if got := hdr.Get(HeaderUA); got != wantUA {
		t.Errorf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := hdr.Get(HeaderClient); got != DefaultHarnessClient {
		t.Errorf("x-opencode-client = %q, want %q", got, DefaultHarnessClient)
	}
	if got := hdr.Get(HeaderProject); got != DefaultProject {
		t.Errorf("x-opencode-project = %q, want %q", got, DefaultProject)
	}
	if got := hdr.Get(HeaderSession); !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(got) {
		t.Errorf("x-opencode-session = %q, want ses_ + 26 base62 chars", got)
	}
	if got := hdr.Get(HeaderRequest); !regexp.MustCompile(`^msg_[0-9A-Za-z]{26}$`).MatchString(got) {
		t.Errorf("x-opencode-request = %q, want msg_ + 26 base62 chars", got)
	}
}

func TestForwardMessages_SendsHarnessHeaders(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: true})

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	body := []byte(`{"model":"claude-sonnet-4-6","messages":[]}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.0.0") // incoming client identity
	w := httptest.NewRecorder()

	ForwardMessages(w, req, upstream.URL, "sk-test", body)

	requireHarnessHeaders(t, got, "opencode/"+DefaultVersion)
}

func TestForwardMessages_HarnessDisabledSendsNoHeaders(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: false})

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	body := []byte(`{"model":"claude-sonnet-4-6","messages":[]}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.0.0")
	w := httptest.NewRecorder()

	ForwardMessages(w, req, upstream.URL, "sk-test", body)

	if got := got.Get(HeaderSession); got != "" {
		t.Errorf("harness disabled but x-opencode-session = %q", got)
	}
	if got := got.Get(HeaderUA); got != "claude-cli/2.0.0" {
		t.Errorf("User-Agent = %q, want the untouched client UA", got)
	}
}

func TestSendChat_SendsHarnessHeaders(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: true})

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"yo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	body := []byte(`{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := SendChat(context.Background(), http.DefaultClient, upstream.URL, "sk-zen", body)
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	defer resp.Body.Close()

	requireHarnessHeaders(t, got, "opencode/"+DefaultVersion)
}

func TestFetchModels_SendsHarnessHeaders(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: true})

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	c := NewClient(2*time.Second, defaultCatalogTTL)
	if _, err := c.FetchModels(context.Background(), "", upstream.URL); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	requireHarnessHeaders(t, got, "opencode/"+DefaultVersion)
}
