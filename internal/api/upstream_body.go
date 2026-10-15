package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"antigravity-go-proxy/internal/transport"
)

// capUpstreamBody buffers resp.Body up to limit. A body past the cap, or one
// that fails mid-read, cannot be forwarded: the bytes read are a prefix, and a
// prefix with a matching Content-Length is a wrong answer that looks right.
// resp is rewritten into a 502 api_error and ok is false.
func capUpstreamBody(resp *http.Response, limit int64, logger *slog.Logger, attrs ...any) (body []byte, ok bool) {
	body, truncated, err := transport.ReadCapped(resp.Body, limit)
	_ = resp.Body.Close()
	if err == nil && !truncated {
		return body, true
	}
	if logger != nil {
		// Two causes, one outcome. Say which one reached, so the log does not
		// call a short read a cap.
		reason := "upstream response body capped"
		if !truncated {
			reason = "upstream response body read failed"
		}
		logger.Warn(reason, append(attrs, "limit", limit, "truncated", truncated, "err", err)...)
	}
	msg, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{
		"type": "api_error", "message": "upstream response exceeded the proxy's body limit",
	}})
	resp.StatusCode, resp.Status = http.StatusBadGateway, "502 Bad Gateway"
	resp.Body = io.NopCloser(bytes.NewReader(msg))
	resp.ContentLength = int64(len(msg))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(msg)))
	resp.Header.Del("Content-Encoding")
	return nil, false
}
