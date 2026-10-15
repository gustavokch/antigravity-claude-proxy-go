package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ccHalfReader hands out its payload, then reports the failure an upstream that
// lets go mid-stream produces.
type ccHalfReader struct {
	payload string
	err     error
}

func (r *ccHalfReader) Read(p []byte) (int, error) {
	if r.payload != "" {
		n := copy(p, r.payload)
		r.payload = r.payload[n:]
		return n, nil
	}
	return 0, r.err
}

// TestCCCopyStreamTerminalError keeps the split the copy is responsible for: a
// clean end is not a fault, anything else is.
func TestCCCopyStreamTerminalError(t *testing.T) {
	reset := errors.New("upstream reset")
	cases := []struct {
		name    string
		body    io.Reader
		wantErr error
		wantOut string
	}{
		{
			name:    "clean end of stream",
			body:    strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			wantErr: nil,
			wantOut: "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		{
			name:    "upstream lets go mid-stream",
			body:    &ccHalfReader{payload: "event: message_start\ndata: {}\n\n", err: reset},
			wantErr: reset,
			wantOut: "event: message_start\ndata: {}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := ccCopyStream(rec, tc.body)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ccCopyStream error = %v, want %v", err, tc.wantErr)
			}
			if got := rec.Body.String(); got != tc.wantOut {
				t.Errorf("forwarded bytes changed:\n got: %q\nwant: %q", got, tc.wantOut)
			}
		})
	}
}

// TestClaudeCodeStreamBreakIsLogged covers what the operator actually sees. The
// upstream flushes a real SSE body and then drops the connection without the
// terminating chunk, so the body read ends at ErrUnexpectedEOF. The client gets
// a short answer either way; the log is the only place the break appears.
func TestClaudeCodeStreamBreakIsLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		w.(http.Flusher).Flush()
		// Close the socket under the response instead of finishing it: no
		// terminating chunk reaches the client.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-break-1")
	w := httptest.NewRecorder()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")

	if !strings.Contains(w.Body.String(), "message_start") {
		t.Fatalf("partial stream was not forwarded to the client, got %q", w.Body.String())
	}

	logged := buf.String()
	t.Logf("emitted: %s", strings.TrimSpace(logged))
	if !strings.Contains(logged, "stream ended before its terminal event") {
		t.Fatalf("no stream-break warning emitted; log:\n%s", logged)
	}
	if !strings.Contains(logged, `"gateway":"claudecode"`) {
		t.Errorf("stream-break warning missing the gateway; log:\n%s", logged)
	}
	if !strings.Contains(logged, "sess-break-1") {
		t.Errorf("stream-break warning missing the session id; log:\n%s", logged)
	}
}

// ccEarlyCloseRun drives the Claude Code gateway against an upstream that
// answers with body and then ends the response cleanly: the handler returns, so
// the chunked body gets its terminating chunk and the client reads io.EOF, not a
// read error. It returns what the client received and what the server logged.
func ccEarlyCloseRun(t *testing.T, contentType, body, sessionID string) (client, logged string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", sessionID)
	w := httptest.NewRecorder()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")
	return w.Body.String(), buf.String()
}

// TestClaudeCodeGracefulEarlyCloseIsLogged: an upstream or an intermediary that
// closes an event stream gracefully, with no message_stop, leaves no read error
// for the copy loop to report. The client still gets a short answer.
func TestClaudeCodeGracefulEarlyCloseIsLogged(t *testing.T) {
	client, logged := ccEarlyCloseRun(t, "text/event-stream",
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n", "sess-early-1")

	if !strings.Contains(client, "message_start") {
		t.Fatalf("the partial stream was not forwarded, got %q", client)
	}
	for _, want := range []string{"stream ended before its terminal event", `"gateway":"claudecode"`, "sess-early-1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
}

// TestClaudeCodeFinishedStreamAndJSONAreNotLogged keeps the new check honest: a
// stream that carries message_stop, an in-band error event, and a non-stream
// JSON answer are all complete and must stay silent.
func TestClaudeCodeFinishedStreamAndJSONAreNotLogged(t *testing.T) {
	cases := []struct{ name, contentType, body string }{
		{"finished stream", "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"in-band error event", "text/event-stream",
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"},
		{"non-stream JSON answer", "application/json",
			`{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, logged := ccEarlyCloseRun(t, tc.contentType, tc.body, "sess-done-1")
			if strings.Contains(logged, "terminal event") {
				t.Errorf("a complete response was reported as a break:\n%s", logged)
			}
		})
	}
}
