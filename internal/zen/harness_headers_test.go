package zen

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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

func TestForwardMessages_PreservesOpenCodeIdentity(t *testing.T) {
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
	req.Header.Set("User-Agent", "opencode/1.18.30")
	req.Header.Set(HeaderSession, "ses_original")
	ForwardMessages(httptest.NewRecorder(), req, upstream.URL, "sk-test", body)
	if got.Get(HeaderSession) != "ses_original" || got.Get(HeaderUA) != "opencode/1.18.30" {
		t.Errorf("opencode identity not preserved: UA=%q session=%q", got.Get(HeaderUA), got.Get(HeaderSession))
	}

	req = httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.0.0")
	ForwardMessages(httptest.NewRecorder(), req, upstream.URL, "sk-test", body)
	requireHarnessHeaders(t, got, "opencode/"+DefaultVersion)
}

func TestSendChatWithHeaders_PreservesOpenCodeSession(t *testing.T) {
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

	clientHeaders := http.Header{
		HeaderUA:      {"opencode/1.18.30"},
		HeaderSession: {"ses_original"},
	}
	body := []byte(`{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := SendChatWithHeaders(context.Background(), http.DefaultClient, upstream.URL, "sk-zen", body, clientHeaders)
	if err != nil {
		t.Fatalf("SendChatWithHeaders: %v", err)
	}
	defer resp.Body.Close()

	if got.Get(HeaderSession) != "ses_original" {
		t.Errorf("x-opencode-session = %q, want ses_original", got.Get(HeaderSession))
	}
	if got.Get(HeaderUA) != "opencode/1.18.30" {
		t.Errorf("User-Agent = %q, want opencode/1.18.30", got.Get(HeaderUA))
	}
}

func TestSendResponsesWithHeaders_PreservesOpenCodeSession(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: true})

	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.output_item.added","item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
			`data: {"type":"response.content_part.added","part":{"type":"output_text","text":"hi"}}`,
			`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":4,"output_tokens":2}}}`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	clientHeaders := http.Header{
		HeaderUA:      {"opencode/1.18.30"},
		HeaderSession: {"ses_original"},
	}
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"ping"}],"max_tokens":64,"stream":false}`)
	resp, err := SendResponsesWithHeaders(context.Background(), upstream.Client(), upstream.URL, "sk-zen-test", body, clientHeaders)
	if err != nil {
		t.Fatalf("SendResponsesWithHeaders: %v", err)
	}
	defer resp.Body.Close()

	if got.Get(HeaderSession) != "ses_original" {
		t.Errorf("x-opencode-session = %q, want ses_original", got.Get(HeaderSession))
	}
	if got.Get(HeaderUA) != "opencode/1.18.30" {
		t.Errorf("User-Agent = %q, want opencode/1.18.30", got.Get(HeaderUA))
	}
}
