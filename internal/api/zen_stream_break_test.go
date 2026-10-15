package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	proxyformat "antigravity-go-proxy/internal/format"
)

// TestZenStreamBreakIsLogged covers the third hand-written copy loop. The Zen
// translators answer a truncated stream with an in-band error event, so the
// copy loop used to see a clean EOF and the operator saw nothing. The client
// bytes are unchanged; the log line is what this test pins.
func TestZenStreamBreakIsLogged(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	// A chat-wire SSE body that stops with no [DONE] and no finish_reason.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true, "apiKey": "sk-zen-test", "baseUrl": upstream.URL,
		"allowlist": []map[string]any{{"id": "glm-5.3", "alias": "glm", "enabled": true}},
	})

	var buf bytes.Buffer
	server, err := New(Options{
		APIKey: "test-proxy-key", Backend: &zenTestBackend{},
		Builder: proxyformat.NewBuilder(), Now: time.Now, Logger: ccCaptureLog(&buf),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":100}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("x-session-id", "sess-zen-break-1")
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)

	logged := buf.String()
	for _, want := range []string{"stream ended before its terminal event", `"gateway":"zen"`, "sess-zen-break-1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
}
