package api

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCapUpstreamBody(t *testing.T) {
	newResp := func(body string) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(body))}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}
	var buf bytes.Buffer
	logger := ccCaptureLog(&buf)

	t.Run("under the cap passes through untouched", func(t *testing.T) {
		resp := newResp(`{"a":1}`)
		body, ok := capUpstreamBody(resp, 64, logger)
		if !ok || string(body) != `{"a":1}` || resp.StatusCode != http.StatusOK {
			t.Fatalf("ok=%v body=%q status=%d", ok, body, resp.StatusCode)
		}
	})
	t.Run("over the cap becomes a 502, never a short 200", func(t *testing.T) {
		resp := newResp(strings.Repeat("x", 100))
		_, ok := capUpstreamBody(resp, 16, logger, "gateway", "zen")
		if ok {
			t.Fatal("ok = true for a capped body")
		}
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
		got, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(got), "api_error") {
			t.Errorf("body = %q, want an api_error envelope", got)
		}
		if !strings.Contains(string(got), "exceeded the proxy's body limit") {
			t.Errorf("client message does not name the cap: %q", got)
		}
		if resp.Header.Get("Content-Length") != strconv.Itoa(len(got)) || resp.ContentLength != int64(len(got)) {
			t.Errorf("Content-Length %q / %d disagrees with body length %d",
				resp.Header.Get("Content-Length"), resp.ContentLength, len(got))
		}
		if !strings.Contains(buf.String(), "capped") {
			t.Errorf("no cap warning logged: %s", buf.String())
		}
	})
	t.Run("a body that fails mid-read is a 502 and says read failed", func(t *testing.T) {
		resp := newResp(`{"a":1}`)
		resp.Body = io.NopCloser(&ccHalfReader{payload: `{"a":1}`, err: io.ErrUnexpectedEOF})
		var log bytes.Buffer
		_, ok := capUpstreamBody(resp, 64, ccCaptureLog(&log), "gateway", "zen")
		if ok || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("ok=%v status=%d", ok, resp.StatusCode)
		}
		if !strings.Contains(log.String(), "read failed") || strings.Contains(log.String(), "capped") {
			t.Errorf("the log calls a short read a cap:\n%s", log.String())
		}
		got, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(got), "could not be read") || strings.Contains(string(got), "body limit") {
			t.Errorf("the client message calls a short read a cap: %q", got)
		}
	})
}

// TestInstrumentedUpstreamBodyFailureIsNotAShort200 pins the invariant where it
// is delivered, not only in the helper: a gateway that cannot read an upstream
// unary body to the end must answer 502, and the response the gateway then
// writes must carry the envelope with a Content-Length that matches it. The body
// fails mid-read instead of passing the 64 MiB cap, so the test needs neither an
// oversized payload nor a mutable cap.
func TestInstrumentedUpstreamBodyFailureIsNotAShort200(t *testing.T) {
	newResp := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}, "Content-Length": {"7"}},
			Body:       io.NopCloser(&ccHalfReader{payload: `{"a":1}`, err: io.ErrUnexpectedEOF}),
		}
	}
	cases := []struct {
		name    string
		gateway string
		run     func(*Server, *http.Response)
	}{
		{"zen", "zen", func(s *Server, r *http.Response) { s.zenInstrumentResponse(r, "m", "s", time.Now()) }},
		{"kimi", "kimi", func(s *Server, r *http.Response) { s.kimiInstrumentResponse(r, "m", "s", time.Now()) }},
		{"claudecode", "claudecode", func(s *Server, r *http.Response) { s.ccInstrumentResponse(r, ccAttempt{accountID: "acc1"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			s := &Server{logger: ccCaptureLog(&log)}
			resp := newResp()
			tc.run(s, resp)

			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: a truncated body must not reach the client as a 200", resp.StatusCode)
			}
			got, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(got), "api_error") {
				t.Errorf("body = %q, want an api_error envelope", got)
			}
			if !strings.Contains(string(got), "could not be read") {
				t.Errorf("client message does not name the cause: %q", got)
			}
			if resp.Header.Get("Content-Length") != strconv.Itoa(len(got)) || resp.ContentLength != int64(len(got)) {
				t.Errorf("Content-Length %q / %d disagrees with body length %d",
					resp.Header.Get("Content-Length"), resp.ContentLength, len(got))
			}
			if !strings.Contains(log.String(), `"gateway":"`+tc.gateway+`"`) || !strings.Contains(log.String(), "read failed") {
				t.Errorf("log does not name the gateway and the cause:\n%s", log.String())
			}
		})
	}
}
