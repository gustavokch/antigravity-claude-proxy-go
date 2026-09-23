package corpus

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// maxCapturedResponse bounds what the tap keeps. Classifier responses are
// small by construction (max_tokens of 64, 8192 and 2112 across the three
// variants), so this is a safety bound, not a routine path.
const maxCapturedResponse = 64 << 10

// ResponseTap wraps an http.ResponseWriter and copies what is written into a
// bounded buffer. Every write reaches the inner writer first, so the client
// sees byte-identical output whether or not capture is on.
type ResponseTap struct {
	inner     http.ResponseWriter
	buffer    bytes.Buffer
	status    int
	truncated bool
	start     time.Time
}

func NewResponseTap(inner http.ResponseWriter) *ResponseTap {
	return &ResponseTap{inner: inner, start: time.Now()}
}

func (tap *ResponseTap) Header() http.Header {
	return tap.inner.Header()
}

func (tap *ResponseTap) WriteHeader(status int) {
	if tap.status == 0 {
		tap.status = status
	}
	tap.inner.WriteHeader(status)
}

func (tap *ResponseTap) Write(payload []byte) (int, error) {
	written, err := tap.inner.Write(payload)
	if tap.status == 0 {
		tap.status = http.StatusOK
	}
	room := maxCapturedResponse - tap.buffer.Len()
	if room <= 0 {
		tap.truncated = true
		return written, err
	}
	if len(payload) > room {
		tap.buffer.Write(payload[:room])
		tap.truncated = true
		return written, err
	}
	tap.buffer.Write(payload)
	return written, err
}

// Flush delegates to the inner writer. writeClassifierResponse asserts
// http.Flusher before emitting SSE, so this method is load-bearing.
func (tap *ResponseTap) Flush() {
	if flusher, ok := tap.inner.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (tap *ResponseTap) Status() int {
	if tap.status == 0 {
		return http.StatusOK
	}
	return tap.status
}

func (tap *ResponseTap) Truncated() bool {
	return tap.truncated
}

func (tap *ResponseTap) ElapsedMs() int64 {
	return time.Since(tap.start).Milliseconds()
}

type anthropicEnvelope struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type sseEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Text string `json:"text"`
	} `json:"delta"`
}

// VerdictText reconstructs the answer text from whichever shape the response
// took. An unrecognized shape returns the captured bytes verbatim: an
// unreadable answer is a finding worth keeping, not a row worth dropping.
func (tap *ResponseTap) VerdictText() string {
	captured := tap.buffer.Bytes()
	if len(captured) == 0 {
		return ""
	}

	var envelope anthropicEnvelope
	if err := json.Unmarshal(captured, &envelope); err == nil && len(envelope.Content) > 0 {
		var builder strings.Builder
		for _, block := range envelope.Content {
			if block.Type == "text" {
				builder.WriteString(block.Text)
			}
		}
		if builder.Len() > 0 {
			return builder.String()
		}
	}

	var builder strings.Builder
	for _, line := range strings.Split(string(captured), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event sseEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if event.Type == "content_block_delta" {
			builder.WriteString(event.Delta.Text)
		}
	}
	if builder.Len() > 0 {
		return builder.String()
	}

	return string(captured)
}
