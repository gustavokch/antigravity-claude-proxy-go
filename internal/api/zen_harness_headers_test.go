package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/zen"
)

type harnessHeaderCapture struct {
	mu      sync.Mutex
	headers []http.Header
}

func (c *harnessHeaderCapture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.headers = append(c.headers, r.Header.Clone())
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6",`+
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":1,"output_tokens":1}}`)
	}
}

func (c *harnessHeaderCapture) first(t *testing.T) http.Header {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		t.Fatal("upstream received no requests")
	}
	return c.headers[0]
}

func (c *harnessHeaderCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.headers)
}

func withHarnessDefaults(t *testing.T) {
	t.Helper()
	prev := zen.GetHarnessConfig()
	zen.SetHarnessConfig(zen.HarnessConfig{
		Enabled: true, Version: "1.18.31", Client: "cli", Project: "global",
	})
	t.Cleanup(func() { zen.SetHarnessConfig(prev) })
}

func requireHarnessHeaders(t *testing.T, hdr http.Header) {
	t.Helper()
	if got := hdr.Get("User-Agent"); got != "opencode/latest/1.18.31/cli" {
		t.Errorf("User-Agent = %q, want %q", got, "opencode/latest/1.18.31/cli")
	}
	for name, want := range map[string]string{
		"x-opencode-client":  "cli",
		"x-opencode-project": "global",
	} {
		if got := hdr.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	session := hdr.Get("x-opencode-session")
	if session == "" {
		t.Error("x-opencode-session empty")
	}
	for _, name := range []string{"x-session-affinity", "X-Session-Id"} {
		if got := hdr.Get(name); got != session {
			t.Errorf("%s = %q, want the x-opencode-session value %q", name, got, session)
		}
	}
	if got := hdr.Get("x-opencode-request"); got != "" {
		t.Errorf("x-opencode-request = %q, want absent (OpenCode 2.x does not send it)", got)
	}
}

// TestSendZenBump_SendsHarnessHeaders covers the cachebump replay path:
// sendZenBump builds its own request through postBumpRequest.
func TestSendZenBump_SendsHarnessHeaders(t *testing.T) {
	withHarnessDefaults(t)

	capture := &harnessHeaderCapture{}
	upstream := httptest.NewServer(capture.handler())
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.Zen = config.ZenConfig{Enabled: true, APIKey: "sk-zen-test", BaseURL: upstream.URL}
	config.SetForTest(cfg)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })

	server := &Server{}
	rec := cachebump.Record{
		Body: []byte(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
	}
	if _, err := server.sendZenBump(context.Background(), rec); err != nil {
		t.Fatalf("sendZenBump: %v", err)
	}
	if n := capture.count(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
	requireHarnessHeaders(t, capture.first(t))
}

func TestApplyZenHarnessConfig_TLSWiring(t *testing.T) {
	t.Cleanup(func() { zen.SetTLSConfig(zen.ZenTLSConfig{}) })

	enabled := true
	applyZenHarnessConfig(config.ZenConfig{Harness: &config.ZenHarnessConfig{
		Enabled: &enabled, TLS: true,
	}})
	if !zen.GetTLSConfig().Enabled {
		t.Error("harness.tls=true must enable the utls transport")
	}

	applyZenHarnessConfig(config.ZenConfig{})
	if zen.GetTLSConfig().Enabled {
		t.Error("missing harness section must fall back to TLS off")
	}
}

// A hand-written partial section ({"tls":true} with no "enabled" key) must
// not silently disable the header disguise: an absent key defaults to true,
// an explicit enabled=false still turns it off.
func TestApplyZenHarnessConfig_PartialSectionDefaultsEnabled(t *testing.T) {
	prev := zen.GetHarnessConfig()
	t.Cleanup(func() {
		zen.SetHarnessConfig(prev)
		zen.SetTLSConfig(zen.ZenTLSConfig{})
	})

	applyZenHarnessConfig(config.ZenConfig{Harness: &config.ZenHarnessConfig{TLS: true}})
	if !zen.GetHarnessConfig().Enabled {
		t.Error("absent harness.enabled must default to true")
	}
	if !zen.GetTLSConfig().Enabled {
		t.Error("harness.tls=true must enable the utls transport")
	}

	off := false
	applyZenHarnessConfig(config.ZenConfig{Harness: &config.ZenHarnessConfig{Enabled: &off}})
	if zen.GetHarnessConfig().Enabled {
		t.Error("explicit enabled=false must disable the harness")
	}
}

// The cache-bump replay posts the raw recorded Anthropic body — no
// stream:true, no gate tools — so free-tier Zen rejects it with 403
// FreeTierError. The gate must be observed: warning logged, bump result
// carrying the failure, instead of a silent bump error.
func TestSendZenBump_ObservesFreeTierGate(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError",`+
			`"message":"OpenCode's free tier can only be used from within OpenCode"}}`)
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.Zen = config.ZenConfig{Enabled: true, APIKey: "sk-zen-test", BaseURL: upstream.URL}
	config.SetForTest(cfg)
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })

	server := &Server{}
	rec := cachebump.Record{
		Model: "mimo-v2.6-flash-free",
		Body:  []byte(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"q"}]}`),
	}
	_, err := server.sendZenBump(context.Background(), rec)
	var upErr *cachebump.UpstreamError
	if !errors.As(err, &upErr) || upErr.Status != http.StatusForbidden {
		t.Fatalf("bump err = %v, want 403 upstream error", err)
	}
	if !strings.Contains(logBuf.String(), "zen free-tier gate rejected request") ||
		!strings.Contains(logBuf.String(), "model=mimo-v2.6-flash-free") {
		t.Fatalf("gate warning missing:\n%s", logBuf.String())
	}
}

// Zen routes x-session-affinity on the OpenCode session id, and the genuine
// client keeps one id per session. Two turns of one Claude Code conversation
// must therefore reach Zen with the same id on every wire, although the
// second turn moves cache_control off the first prompt; the cache-bump replay
// of that conversation must reuse it; another session gets its own.
func TestForwardToZen_SessionIDPerConversation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")
	withHarnessDefaults(t)

	var mu sync.Mutex
	var sessions []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get("x-opencode-session"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/chat/completions" {
			_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"yo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6",`+
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
			{"id": "glm-5.3", "enabled": true},
		},
	})
	server := newZenTestServer(t)

	turn1 := `{"model":%q,"max_tokens":16,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"fix the bug","cache_control":{"type":"ephemeral"}}]}]}`
	turn2 := `{"model":%q,"max_tokens":16,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"fix the bug"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"done"}]},` +
		`{"role":"user","content":[{"type":"text","text":"thanks","cache_control":{"type":"ephemeral"}}]}]}`
	sessionOf := func(session, body string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")
		req.Header.Set("X-Claude-Code-Session-Id", session)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		mu.Lock()
		defer mu.Unlock()
		return sessions[len(sessions)-1]
	}

	for _, model := range []string{"claude-sonnet-4-6", "glm-5.3"} {
		session := "session-a-" + model
		first := sessionOf(session, fmt.Sprintf(turn1, model))
		if first == "" {
			t.Fatalf("%s: no x-opencode-session sent", model)
		}
		if next := sessionOf(session, fmt.Sprintf(turn2, model)); next != first {
			t.Errorf("%s: second turn sent session %q, want the first turn's %q", model, next, first)
		}
		if other := sessionOf("session-b-"+model, fmt.Sprintf(turn1, model)); other == first {
			t.Errorf("%s: another session reused %q", model, first)
		}
		if model != "claude-sonnet-4-6" {
			continue
		}
		bump := cachebump.Record{SessionID: session, Body: []byte(fmt.Sprintf(turn2, model))}
		if _, err := (&Server{}).sendZenBump(context.Background(), bump); err != nil {
			t.Fatalf("sendZenBump: %v", err)
		}
		mu.Lock()
		replayed := sessions[len(sessions)-1]
		mu.Unlock()
		if replayed != first {
			t.Errorf("cache-bump replay sent session %q, want the conversation's %q", replayed, first)
		}
	}
}
