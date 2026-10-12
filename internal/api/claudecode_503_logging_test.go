package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/claudecode"
)

// When account selection fails before any upstream call, the 503 carries the
// reason only in the response body — the operator-facing log must carry it
// too, with each pool account's state. Without it the webUI logs tab shows
// only the bare "[POST] /v1/messages 503" middleware line and the cause is
// invisible (observed 2026-10-12: a rate-limited account 503'd for minutes
// with zero detail in server.log or the webUI).
func TestForwardToClaudeCode_NoAccountAvailableLogsReason(t *testing.T) {
	// First call hits a 429, which puts the account into cooldown and
	// mirrors the 429 to the client. The second call then fails at
	// SelectAccount with no prior 429 in this call — the silent 503 path.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))
	}))
	defer upstream.Close()

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: upstream.URL,
		Mode:    "pool",
		Accounts: []claudecode.AccountConfig{
			{ID: "acc1", Token: "sk-ant-a", Enabled: true},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}
	ccResetPool()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}

	// Call 1: 429 → account cooldown.
	w1 := httptest.NewRecorder()
	cc503RequestInto(t, srv, cfg, w1)
	if w1.Code != http.StatusTooManyRequests {
		t.Fatalf("setup call: expected 429, got %d (body: %s)", w1.Code, w1.Body.String())
	}

	// Call 2: instant 503 from SelectAccount failure.
	buf.Reset()
	w2 := httptest.NewRecorder()
	cc503RequestInto(t, srv, cfg, w2)
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body: %s)", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "No Claude Code accounts available") {
		t.Errorf("expected reason in response body, got %s", w2.Body.String())
	}

	logs := buf.String()
	if !strings.Contains(logs, "no available Claude Code accounts") {
		t.Errorf("503 reason missing from log; must log the SelectAccount error.\nlog:\n%s", logs)
	}
	if !strings.Contains(logs, "acc1=cooldown") {
		t.Errorf("per-account state missing from log; must log id=status per pool account.\nlog:\n%s", logs)
	}
}

// When every attempt fails with a transport error and no 429 was recorded,
// the final 503 must name the excluded accounts instead of answering blind.
func TestForwardToClaudeCode_AllAttemptsFailedLogsReason(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := upstream.URL
	upstream.Close() // connection refused on every attempt

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: deadURL,
		Mode:    "pool",
		Accounts: []claudecode.AccountConfig{
			{ID: "acc-a", Token: "sk-ant-a", Enabled: true},
			{ID: "acc-b", Token: "sk-ant-b", Enabled: true},
			{ID: "acc-c", Token: "sk-ant-c", Enabled: true},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}
	ccResetPool()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}

	w := httptest.NewRecorder()
	cc503RequestInto(t, srv, cfg, w)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "All Claude Code accounts rate-limited or unavailable") {
		t.Errorf("expected exhaustion reason in body, got %s", w.Body.String())
	}

	logs := buf.String()
	if !strings.Contains(logs, "All Claude Code accounts rate-limited or unavailable") {
		t.Errorf("exhaustion reason missing from log.\nlog:\n%s", logs)
	}
	for _, id := range []string{"acc-a", "acc-b", "acc-c"} {
		if !strings.Contains(logs, id) {
			t.Errorf("excluded account %q missing from log.\nlog:\n%s", id, logs)
		}
	}
}

// cc503RequestInto is cc503Request split so tests can pre-fabricate the
// recorder and run the same request twice against a shared pool.
func cc503RequestInto(t *testing.T, srv *Server, cfg claudecode.Config, w *httptest.ResponseRecorder) {
	t.Helper()
	reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")
}
