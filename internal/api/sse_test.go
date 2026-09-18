package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/logger"
)

func TestStreamSSEGenericHelperContract(t *testing.T) {
	type testItem struct {
		ID  string `json:"id"`
		Seq uint64 `json:"seq"`
	}

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "logs stream through streamSSE helper",
			run: func(t *testing.T) {
				srv := &Server{broadcaster: logger.NewBroadcaster(10)}
				srv.broadcaster.Add(logger.LogEntry{Timestamp: "2026-09-18T12:00:00Z", Level: "INFO", Message: "historic"})

				ctx, cancel := context.WithCancel(context.Background())
				request := httptest.NewRequest(http.MethodGet, "/api/logs/stream?history=true", nil).WithContext(ctx)
				recorder := &stagedFlushRecorder{rec: httptest.NewRecorder(), gates: []chan struct{}{make(chan struct{}), make(chan struct{})}}

				done := make(chan struct{})
				go func() {
					srv.handleLogsStream(recorder, request)
					close(done)
				}()

				waitForCondition(t, 2*time.Second, func() bool { return recorder.statusCode() == http.StatusOK })
				srv.broadcaster.Add(logger.LogEntry{Timestamp: "2026-09-18T12:00:01Z", Level: "INFO", Message: "overlap"})
				close(recorder.gates[0])
				waitForCondition(t, 2*time.Second, func() bool {
					return strings.Contains(recorder.bodyString(), "overlap")
				})
				srv.broadcaster.Add(logger.LogEntry{Timestamp: "2026-09-18T12:00:02Z", Level: "INFO", Message: "live"})
				close(recorder.gates[1])
				waitForCondition(t, 2*time.Second, func() bool {
					return strings.Contains(recorder.bodyString(), "live")
				})
				cancel()

				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("handler did not return when context cancelled")
				}

				for _, message := range []string{"historic", "overlap", "live"} {
					if got := strings.Count(recorder.bodyString(), message); got != 1 {
						t.Errorf("expected %q exactly once, got %d", message, got)
					}
				}
			},
		},
		{
			name: "audit stream through streamSSE helper",
			run: func(t *testing.T) {
				srv := &Server{classifierAudit: classifier.NewRecorder(10)}
				srv.classifierAudit.Add(classifier.Event{RuleID: "historic", Status: "stubbed"})

				ctx, cancel := context.WithCancel(context.Background())
				request := httptest.NewRequest(http.MethodGet, "/api/classifier/audit/stream?history=true", nil).WithContext(ctx)
				recorder := &stagedFlushRecorder{rec: httptest.NewRecorder(), gates: []chan struct{}{make(chan struct{}), make(chan struct{})}}

				done := make(chan struct{})
				go func() {
					srv.handleClassifierAuditStream(recorder, request)
					close(done)
				}()

				waitForCondition(t, 2*time.Second, func() bool { return recorder.statusCode() == http.StatusOK })
				srv.classifierAudit.Add(classifier.Event{RuleID: "overlap", Status: "stubbed"})
				close(recorder.gates[0])
				waitForCondition(t, 2*time.Second, func() bool {
					return strings.Contains(recorder.bodyString(), "overlap")
				})
				srv.classifierAudit.Add(classifier.Event{RuleID: "live", Status: "stubbed"})
				close(recorder.gates[1])
				waitForCondition(t, 2*time.Second, func() bool {
					return strings.Contains(recorder.bodyString(), "live")
				})
				cancel()

				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("handler did not return when context cancelled")
				}

				for _, id := range []string{"historic", "overlap", "live"} {
					if got := strings.Count(recorder.bodyString(), fmt.Sprintf(`"ruleId":%q`, id)); got != 1 {
						t.Errorf("expected ruleId %q exactly once, got %d", id, got)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
