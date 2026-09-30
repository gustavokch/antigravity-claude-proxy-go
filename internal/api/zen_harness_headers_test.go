package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
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
	zen.SetHarnessConfig(zen.HarnessConfig{
		Enabled: true, Version: "1.18.31", Client: "cli", Project: "global",
	})
	t.Cleanup(func() { zen.SetHarnessConfig(zen.HarnessConfig{}) })
}

func requireHarnessHeaders(t *testing.T, hdr http.Header) {
	t.Helper()
	if got := hdr.Get("User-Agent"); got != "opencode/1.18.31" {
		t.Errorf("User-Agent = %q, want %q", got, "opencode/1.18.31")
	}
	for name, want := range map[string]string{
		"x-opencode-client":  "cli",
		"x-opencode-project": "global",
	} {
		if got := hdr.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if hdr.Get("x-opencode-session") == "" {
		t.Error("x-opencode-session empty")
	}
	if hdr.Get("x-opencode-request") == "" {
		t.Error("x-opencode-request empty")
	}
}

// TestForwardToZen_CCRSender_SendsHarnessHeaders covers the CCR sender
// closure inside forwardToZen (anthropic-wire model, CCR enabled), the
// remaining header site for request-path traffic.
func TestForwardToZen_CCRSender_SendsHarnessHeaders(t *testing.T) {
	withHarnessDefaults(t)

	capture := &harnessHeaderCapture{}
	upstream := httptest.NewServer(capture.handler())
	defer upstream.Close()

	store := ccr.NewCCRStore(1024 * 1024)
	eng := headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: true},
	}, nil, ccr.NewStage(store))
	server := &Server{headroom: eng, ccrStore: store}

	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.forwardToZen(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		config.ZenConfig{Enabled: true, APIKey: "sk-zen-test", BaseURL: upstream.URL},
		body, reqMap, "claude-sonnet-4-6",
		config.ZenModelConfig{ID: "claude-sonnet-4-6", Enabled: true})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if n := capture.count(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
	requireHarnessHeaders(t, capture.first(t))
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
	t.Cleanup(func() {
		zen.SetHarnessConfig(zen.HarnessConfig{})
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
