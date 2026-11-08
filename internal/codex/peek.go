package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// PeekLimit bounds how much of a reply is held back looking for an early
// failure. It is CLIProxyAPI's bootstrap byte budget: response.created echoes
// the request's instructions and tools, which alone can run past 64 KiB.
const PeekLimit = 1 << 20

// Peek reads the start of a Codex event stream up to its first output event,
// looking for a failure the backend reports before any output: a spent usage
// window or an overload arrives as a 200 whose stream fails at once.
//
// prefix holds every byte read, for the caller to replay ahead of the rest of
// r. Reading stops at the first event that is not part of the preamble
// (response.created, response.in_progress, response.queued and the metadata
// events), at [DONE], at the end of the stream, or once limit bytes are held.
// failure is the failure as an error body, {"error": {...}}, and nil when the
// stream got past the preamble without one. The end of the stream is not an
// error: the translator reports a truncation when it replays the prefix.
func Peek(r *bufio.Reader, limit int) (prefix, failure []byte, err error) {
	for len(prefix) < limit {
		line, readErr := r.ReadBytes('\n')
		prefix = append(prefix, line...)
		if done, f := peekLine(line); done {
			return prefix, f, nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return prefix, nil, nil
			}
			return prefix, nil, readErr
		}
	}
	return prefix, nil, nil
}

// peekLine reports whether a stream line ends the preamble, and the failure
// body when the line is a failure event. Lines that are not data lines, or do
// not parse, belong to the preamble.
func peekLine(line []byte) (done bool, failure []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return false, nil
	}
	data = bytes.TrimSpace(data)
	if string(data) == "[DONE]" {
		return true, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var event map[string]any
	if dec.Decode(&event) != nil || event == nil {
		return false, nil
	}
	switch typ, _ := event["type"].(string); typ {
	case "response.created", "response.in_progress", "response.queued",
		"response.metadata", "codex.response.metadata", "codex.rate_limits":
		return false, nil
	case "response.failed", "response.error":
		resp, _ := event["response"].(map[string]any)
		detail, _ := resp["error"].(map[string]any)
		if detail == nil {
			detail, _ = event["error"].(map[string]any)
		}
		return true, failureBody(detail)
	case "error":
		detail, _ := event["error"].(map[string]any)
		if detail == nil {
			detail = event
			delete(detail, "type")
			delete(detail, "sequence_number")
		}
		return true, failureBody(detail)
	default:
		return true, nil
	}
}

// failureBody wraps a stream failure's details as an error reply body.
func failureBody(detail map[string]any) []byte {
	if len(detail) == 0 {
		detail = map[string]any{"message": "upstream stream failed without error details"}
	}
	body, err := marshal(map[string]any{"error": detail})
	if err != nil {
		return []byte(`{"error":{"message":"upstream stream failed without error details"}}`)
	}
	return body
}

// FailureStatus is the HTTP status a stream failure body stands for, narrowed
// from CLIProxyAPI's codexTerminalFailureStatus to the statuses the gateway
// treats differently: a status the error names (status_code or status, 400 to
// 599), else 429 for a rate or usage limit, 400 for a request the backend
// refuses, and 502 for anything else.
func FailureStatus(failure []byte) int {
	var doc struct {
		Error map[string]any `json:"error"`
	}
	_ = json.Unmarshal(failure, &doc)
	for _, key := range []string{"status_code", "status"} {
		if n, ok := doc.Error[key].(float64); ok && n >= 400 && n < 600 {
			return int(n)
		}
	}
	typ := strings.ToLower(strings.TrimSpace(str(doc.Error["type"])))
	code := strings.ToLower(strings.TrimSpace(str(doc.Error["code"])))
	for _, v := range []string{typ, code} {
		switch v {
		case "usage_limit_reached", "rate_limit_error", "rate_limit_exceeded":
			return http.StatusTooManyRequests
		}
	}
	if typ == "invalid_request_error" || code == "context_length_exceeded" || code == "invalid_encrypted_content" {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}
