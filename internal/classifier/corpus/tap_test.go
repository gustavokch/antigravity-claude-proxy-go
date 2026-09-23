package corpus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseTapIsTransparent(t *testing.T) {
	recorder := httptest.NewRecorder()
	tap := NewResponseTap(recorder)

	tap.Header().Set("Content-Type", "application/json")
	tap.WriteHeader(http.StatusOK)
	payload := `{"content":[{"type":"text","text":"<severity>0</severity>"}]}`
	if _, err := tap.Write([]byte(payload)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if recorder.Body.String() != payload {
		t.Errorf("inner writer got %q, want the bytes written verbatim", recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "application/json" {
		t.Error("headers did not reach the inner writer")
	}
	if tap.Status() != http.StatusOK {
		t.Errorf("Status = %d, want 200", tap.Status())
	}
}

func TestResponseTapDefaultsStatusTo200(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	if _, err := tap.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if tap.Status() != http.StatusOK {
		t.Errorf("Status = %d, want 200 when WriteHeader was never called", tap.Status())
	}
}

func TestResponseTapImplementsFlusher(t *testing.T) {
	var writer http.ResponseWriter = NewResponseTap(httptest.NewRecorder())
	if _, ok := writer.(http.Flusher); !ok {
		t.Fatal("ResponseTap must implement http.Flusher; writeClassifierResponse asserts it")
	}
	// Flushing a writer that cannot flush must not panic.
	writer.(http.Flusher).Flush()
}

func TestResponseTapVerdictFromJSONEnvelope(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	body := `{"id":"msg_1","type":"message","content":[
		{"type":"thinking","thinking":"ignored"},
		{"type":"text","text":"<thinking>Routine.</thinking>"},
		{"type":"text","text":"<severity>4</severity>"}
	]}`
	if _, err := tap.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := "<thinking>Routine.</thinking><severity>4</severity>"
	if got := tap.VerdictText(); got != want {
		t.Errorf("VerdictText = %q, want %q", got, want)
	}
}

func TestResponseTapVerdictFromSSE(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	frames := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<severity>"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"7</severity>"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	if _, err := tap.Write([]byte(frames)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.VerdictText(); got != "<severity>7</severity>" {
		t.Errorf("VerdictText = %q, want the concatenated deltas", got)
	}
}

func TestResponseTapVerdictFallsBackToRawBytes(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	if _, err := tap.Write([]byte("<block>true</block>")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.VerdictText(); got != "<block>true</block>" {
		t.Errorf("VerdictText = %q, want the raw bytes when neither shape parses", got)
	}
}

func TestResponseTapCapsCapturedBytes(t *testing.T) {
	recorder := httptest.NewRecorder()
	tap := NewResponseTap(recorder)
	oversized := strings.Repeat("a", maxCapturedResponse+4096)
	written, err := tap.Write([]byte(oversized))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if written != len(oversized) {
		t.Errorf("Write returned %d, want %d: the client must receive every byte", written, len(oversized))
	}
	if recorder.Body.Len() != len(oversized) {
		t.Errorf("inner writer got %d bytes, want %d", recorder.Body.Len(), len(oversized))
	}
	if !tap.Truncated() {
		t.Error("Truncated = false, want true")
	}
	if len(tap.VerdictText()) > maxCapturedResponse {
		t.Errorf("captured %d bytes, want at most %d", len(tap.VerdictText()), maxCapturedResponse)
	}
}
