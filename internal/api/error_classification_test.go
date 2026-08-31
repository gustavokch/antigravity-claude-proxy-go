package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// 499 is nginx's "client closed request" status: the client disconnected
// before the response was written. The proxy classifies context
// cancellation with it instead of the misleading 500.
const wantClientClosedRequest = 499

type cancelingBackend struct{}

func (b *cancelingBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{}, context.Canceled
}

func (b *cancelingBackend) StreamGenerateContent(ctx context.Context, request map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func TestClassifyErrorContextCancellation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		status  int
		kind    string
		message string
	}{
		{
			name:    "bare canceled",
			err:     context.Canceled,
			status:  wantClientClosedRequest,
			kind:    "api_error",
			message: "Request canceled by the client.",
		},
		{
			name:    "canceled wrapped by dispatcher retry wrapper",
			err:     fmt.Errorf("model listing exhausted account retries: %w", context.Canceled),
			status:  wantClientClosedRequest,
			kind:    "api_error",
			message: "Request canceled by the client.",
		},
		{
			name:    "bare deadline",
			err:     context.DeadlineExceeded,
			status:  http.StatusGatewayTimeout,
			kind:    "api_error",
			message: "Request deadline exceeded while contacting the upstream.",
		},
		{
			name:    "deadline wrapped by model resolution",
			err:     fmt.Errorf("refresh selectable models: %w", context.DeadlineExceeded),
			status:  http.StatusGatewayTimeout,
			kind:    "api_error",
			message: "Request deadline exceeded while contacting the upstream.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, kind, message := classifyError(tc.err)
			if status != tc.status || kind != tc.kind || message != tc.message {
				t.Fatalf("classifyError(%v) = %d, %q, %q; want %d, %q, %q",
					tc.err, status, kind, message, tc.status, tc.kind, tc.message)
			}
		})
	}
}

func TestClassifyErrorStillClassifiesOtherErrors(t *testing.T) {
	t.Parallel()
	status, kind, _ := classifyError(errors.New("ordinary failure"))
	if status != http.StatusInternalServerError || kind != "api_error" {
		t.Fatalf("classifyError(ordinary) = %d, %q; want 500, %q", status, kind, "api_error")
	}
}

func TestModelsHandlerReportsClientDisconnect(t *testing.T) {
	t.Parallel()
	server := &Server{backend: &cancelingBackend{}, logger: slog.Default(), now: time.Now}
	recorder := httptest.NewRecorder()
	server.models(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != wantClientClosedRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
}

// levelRecordingHandler records the logger writes so tests can assert
// cancellation is not logged at error level.
type levelRecordingHandler struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (h *levelRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelRecordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	h.levels = append(h.levels, record.Level)
	h.mu.Unlock()
	return nil
}

func (h *levelRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *levelRecordingHandler) maxLevel() slog.Level {
	h.mu.Lock()
	defer h.mu.Unlock()
	max := slog.LevelDebug
	for _, level := range h.levels {
		if level > max {
			max = level
		}
	}
	return max
}

func TestWriteErrorLogsCancellationBelowErrorLevel(t *testing.T) {
	t.Parallel()
	records := &levelRecordingHandler{}
	server := &Server{logger: slog.New(records), now: time.Now}

	server.writeError(httptest.NewRecorder(), context.Canceled)
	if got := records.maxLevel(); got >= slog.LevelError {
		t.Fatalf("cancellation logged at %v; want below error level", got)
	}

	records.levels = nil
	server.writeError(httptest.NewRecorder(), errors.New("ordinary failure"))
	if got := records.maxLevel(); got < slog.LevelError {
		t.Fatalf("ordinary failure logged at %v; want error level", got)
	}
}
