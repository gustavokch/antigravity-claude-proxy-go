package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ccUsageDetailRecord returns the first "claudecode usage detail" debug record.
func ccUsageDetailRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "claudecode usage detail" {
			return rec
		}
	}
	return nil
}

// TestClaudeCodeUsageCapture_StreamDetail proves the Claude Code path captures
// the message ID, served model, upstream request-id and the 5m/1h cache write
// split from a streamed response.
func TestClaudeCodeUsageCapture_StreamDetail(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_01detail","model":"claude-opus-5-20260101","usage":{"input_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":90,"cache_creation":{"ephemeral_5m_input_tokens":30,"ephemeral_1h_input_tokens":60},"output_tokens":1}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":77}}`,
		``,
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req_011detail")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != sse {
		t.Fatalf("stream body altered")
	}

	rec := ccFindObservabilityRecord(t, &buf)
	if rec == nil {
		t.Fatalf("no [ClaudeCode] observability record; log:\n%s", buf.String())
	}
	if got := ccNum(t, rec, "output_tokens"); got != 77 {
		t.Errorf("output_tokens = %v, want 77", got)
	}
	if got := ccNum(t, rec, "cache_creation_tokens"); got != 90 {
		t.Errorf("cache_creation_tokens = %v, want 90", got)
	}

	d := ccUsageDetailRecord(t, &buf)
	if d == nil {
		t.Fatalf("no usage detail record; log:\n%s", buf.String())
	}
	for k, want := range map[string]any{
		"message_id":        "msg_01detail",
		"served_model":      "claude-opus-5-20260101",
		"request_id":        "req_011detail",
		"account":           "acc1",
		"cache_creation_5m": float64(30),
		"cache_creation_1h": float64(60),
	} {
		if d[k] != want {
			t.Errorf("%s = %v, want %v", k, d[k], want)
		}
	}
}

// TestClaudeCodeUsageCapture_UnaryDetail covers the buffered JSON path.
func TestClaudeCodeUsageCapture_UnaryDetail(t *testing.T) {
	const respBody = `{"id":"msg_01unary","type":"message","model":"claude-opus-5",` +
		`"content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":8}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_011unary")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")

	if w.Body.String() != respBody {
		t.Fatalf("response body altered")
	}
	d := ccUsageDetailRecord(t, &buf)
	if d == nil {
		t.Fatalf("no usage detail record; log:\n%s", buf.String())
	}
	for k, want := range map[string]any{
		"message_id":        "msg_01unary",
		"served_model":      "claude-opus-5",
		"request_id":        "req_011unary",
		"cache_creation_5m": float64(8),
		"cache_creation_1h": float64(0),
	} {
		if d[k] != want {
			t.Errorf("%s = %v, want %v", k, d[k], want)
		}
	}
}
