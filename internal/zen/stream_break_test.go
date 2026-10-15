package zen

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// zenHalfReader hands out its payload, then reports the failure an upstream
// that lets go mid-stream produces.
type zenHalfReader struct {
	payload string
	err     error
}

func (r *zenHalfReader) Read(p []byte) (int, error) {
	if r.payload != "" {
		n := copy(p, r.payload)
		r.payload = r.payload[n:]
		return n, nil
	}
	return 0, r.err
}

func sseResponse(body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(body),
	}
}

// TestForwardTranslatedReportsTheTerminalError keeps the copy responsible for
// saying whether the stream ended on its own, for a body it copies verbatim.
func TestForwardTranslatedReportsTheTerminalError(t *testing.T) {
	reset := errors.New("upstream reset")
	cases := []struct {
		name    string
		body    io.Reader
		wantErr error
		wantOut string
	}{
		{"clean end of stream", strings.NewReader("data: x\n\n"), nil, "data: x\n\n"},
		{"upstream lets go mid-stream", &zenHalfReader{payload: "data: x\n\n", err: reset}, reset, "data: x\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := forwardTranslated(rec, "chat", nil, func() (*http.Response, error) {
				return sseResponse(tc.body), nil
			})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("forwardTranslated error = %v, want %v", err, tc.wantErr)
			}
			if got := rec.Body.String(); got != tc.wantOut {
				t.Errorf("forwarded bytes changed:\n got: %q\nwant: %q", got, tc.wantOut)
			}
		})
	}
}

// TestTranslatedStreamTruncationIsReported is the case that matters: the
// translators tell the client in-band and used to return nil, so the copy
// loop saw a clean EOF. The client bytes must stay as they were; only the
// return value changes.
func TestTranslatedStreamTruncationIsReported(t *testing.T) {
	chat := "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" // no [DONE], no finish_reason
	cases := []struct {
		name string
		resp func() *http.Response
	}{
		{"chat wire, clean EOF", func() *http.Response {
			return translateChatResponse(sseResponse(strings.NewReader(chat)), "m", true, nil, nil)
		}},
		{"chat wire, read error", func() *http.Response {
			return translateChatResponse(sseResponse(&zenHalfReader{payload: chat, err: io.ErrUnexpectedEOF}), "m", true, nil, nil)
		}},
		{"responses wire, clean EOF", func() *http.Response {
			return translateResponsesResponse(sseResponse(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")), "m", true, nil, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := forwardTranslated(rec, "chat", nil, func() (*http.Response, error) { return tc.resp(), nil })
			if !errors.Is(err, ErrStreamTruncated) {
				t.Fatalf("error = %v, want ErrStreamTruncated", err)
			}
			if !strings.Contains(rec.Body.String(), "Zen stream ended before completion") &&
				!strings.Contains(rec.Body.String(), "Zen stream read error") {
				t.Errorf("the in-band error event to the client changed:\n%s", rec.Body.String())
			}
		})
	}

	t.Run("a stream that finishes is not reported", func(t *testing.T) {
		done := chat + "data: {\"id\":\"c\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		rec := httptest.NewRecorder()
		err := forwardTranslated(rec, "chat", nil, func() (*http.Response, error) {
			return translateChatResponse(sseResponse(strings.NewReader(done)), "m", true, nil, nil), nil
		})
		if err != nil {
			t.Errorf("error = %v, want nil", err)
		}
	})
}
