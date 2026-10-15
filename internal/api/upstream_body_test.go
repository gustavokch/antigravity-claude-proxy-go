package api

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
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
		if resp.Header.Get("Content-Length") != strconv.Itoa(len(got)) || resp.ContentLength != int64(len(got)) {
			t.Errorf("Content-Length %q / %d disagrees with body length %d",
				resp.Header.Get("Content-Length"), resp.ContentLength, len(got))
		}
		if !strings.Contains(buf.String(), "capped") {
			t.Errorf("no cap warning logged: %s", buf.String())
		}
	})
}
