package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/logger"
)

func postConfigRules(t *testing.T, srv *Server, classifierBlob string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"classifier":` + classifierBlob + `}`
	request := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	srv.handleConfigSave(recorder, request)
	return recorder
}

func TestConfigSaveRejectsMalformedRules(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want string
	}{
		{
			name: "unknown action",
			blob: `{"rules":[{"id":"r","enabled":true,"action":"explode","conditions":{"models":["m"]}}]}`,
			want: "action",
		},
		{
			name: "reroute naming an unknown backend",
			blob: `{"rules":[{"id":"r","enabled":true,"action":"reroute","targetBackend":"ghost","conditions":{"models":["m"]}}]}`,
			want: "backend",
		},
		{
			name: "stub with no verdict",
			blob: `{"rules":[{"id":"r","enabled":true,"action":"stub","conditions":{"models":["m"]}}]}`,
			want: "verdictTemplate",
		},
		{
			name: "rule with no conditions matches everything",
			blob: `{"rules":[{"id":"r","enabled":true,"action":"passthrough","conditions":{}}]}`,
			want: "condition",
		},
		{
			name: "uncompilable regex",
			blob: `{"rules":[{"id":"r","enabled":true,"action":"passthrough","conditions":{"footerPatterns":[{"type":"regex","pattern":"("}]}}]}`,
			want: "regex",
		},
		{
			name: "missing id",
			blob: `{"rules":[{"enabled":true,"action":"passthrough","conditions":{"models":["m"]}}]}`,
			want: "id",
		},
		{
			name: "inverted token bounds",
			blob: `{"rules":[{"id":"r","enabled":true,"action":"passthrough","conditions":{"maxTokensMin":500,"maxTokensMax":10}}]}`,
			want: "maxTokens",
		},
		{
			name: "backend with no url",
			blob: `{"backends":{"local":{"name":"Local","format":"openai","model":"m"}}}`,
			want: "url",
		},
		{
			name: "backend with a non-http url",
			blob: `{"backends":{"local":{"name":"Local","url":"ftp://x/y","format":"openai","model":"m"}}}`,
			want: "url",
		},
		{
			name: "unknown backend format",
			blob: `{"backends":{"local":{"name":"Local","url":"http://127.0.0.1:8000","format":"grpc","model":"m"}}}`,
			want: "format",
		},
	}

	srv := &Server{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := postConfigRules(t, srv, tc.blob)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(strings.ToLower(recorder.Body.String()), strings.ToLower(tc.want)) {
				t.Errorf("error should mention %q, got %s", tc.want, recorder.Body.String())
			}
		})
	}
}

// stagedFlushRecorder gates the first len(gates) Flush calls so a test can
// deterministically place Recorder.Add calls between handler stages.
type stagedFlushRecorder struct {
	rec   *httptest.ResponseRecorder
	gates []chan struct{}
	count atomic.Int32
	mu    sync.Mutex
}

func (g *stagedFlushRecorder) Header() http.Header {
	return g.rec.Header()
}

func (g *stagedFlushRecorder) WriteHeader(code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rec.WriteHeader(code)
}

func (g *stagedFlushRecorder) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rec.Write(p)
}

func (g *stagedFlushRecorder) Flush() {
	n := int(g.count.Add(1))
	if n <= len(g.gates) {
		<-g.gates[n-1]
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rec.Flush()
}

func (g *stagedFlushRecorder) statusCode() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rec.Code
}

func (g *stagedFlushRecorder) bodyString() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rec.Body.String()
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestClassifierAuditStreamEmitsHistoryAndLiveEvents(t *testing.T) {
	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	historyStamp := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	srv.classifierAudit.Add(classifier.Event{RuleID: "historic", Status: "stubbed", Timestamp: historyStamp})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/classifier/audit/stream?history=true", nil).WithContext(ctx)

	gateFlush := make(chan struct{})
	gateDrain := make(chan struct{})
	recorder := &stagedFlushRecorder{
		rec:   httptest.NewRecorder(),
		gates: []chan struct{}{gateFlush, gateDrain},
	}

	done := make(chan struct{})
	go func() {
		srv.handleClassifierAuditStream(recorder, request)
		close(done)
	}()

	// The status code is written after Subscribe, so seeing it means an Add
	// now lands in BOTH the ring buffer (history replay) and the live
	// channel; the stream must emit it exactly once.
	waitForCondition(t, 2*time.Second, func() bool { return recorder.statusCode() == http.StatusOK })
	srv.classifierAudit.Add(classifier.Event{RuleID: "overlap", Status: "rerouted", Timestamp: historyStamp.Add(time.Second)})
	close(gateFlush)

	// Once the history frames are written, the snapshot is fixed; an Add now
	// can only arrive via the live channel and must not be lost.
	waitForCondition(t, 2*time.Second, func() bool {
		return strings.Contains(recorder.bodyString(), `"ruleId":"overlap"`)
	})
	srv.classifierAudit.Add(classifier.Event{RuleID: "live", Status: "rerouted", Timestamp: historyStamp.Add(2 * time.Second)})
	close(gateDrain)

	waitForCondition(t, 2*time.Second, func() bool {
		return strings.Contains(recorder.bodyString(), `"ruleId":"live"`)
	})
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return when the request context was cancelled")
	}

	body := recorder.bodyString()
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("expected an SSE content type, got %q", got)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event classifier.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("invalid frame %q: %v", line, err)
		}
		counts[event.RuleID]++
	}
	for _, id := range []string{"historic", "overlap", "live"} {
		if counts[id] != 1 {
			t.Errorf("expected %q exactly once, got %d:\n%s", id, counts[id], body)
		}
	}
}

func TestClassifierAuditStreamEmitsSameTimestampSameRuleEvents(t *testing.T) {
	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	stamp := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	srv.classifierAudit.Add(classifier.Event{RuleID: "dup", Status: "stubbed", Timestamp: stamp})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/classifier/audit/stream?history=true", nil).WithContext(ctx)
	recorder := &stagedFlushRecorder{rec: httptest.NewRecorder(), gates: []chan struct{}{make(chan struct{}), make(chan struct{})}}

	done := make(chan struct{})
	go func() {
		srv.handleClassifierAuditStream(recorder, request)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, func() bool { return recorder.statusCode() == http.StatusOK })
	// Same Timestamp AND RuleID as the history event: the old
	// Timestamp+RuleID dedupe key would have swallowed this event.
	srv.classifierAudit.Add(classifier.Event{RuleID: "dup", Status: "rerouted", Timestamp: stamp})
	close(recorder.gates[0])
	close(recorder.gates[1])
	waitForCondition(t, 2*time.Second, func() bool {
		return strings.Count(recorder.bodyString(), `"ruleId":"dup"`) == 2
	})
	cancel()
	<-done
}

func TestLogsStreamEmitsHistoryAndLiveEntries(t *testing.T) {
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
		t.Fatal("handler did not return when the request context was cancelled")
	}

	for _, message := range []string{"historic", "overlap", "live"} {
		if got := strings.Count(recorder.bodyString(), message); got != 1 {
			t.Errorf("expected %q exactly once, got %d", message, got)
		}
	}
}

// TestConfigSaveAcceptsLayaBackend pins the laya case in the backend format
// allowlist. Without it the handler rejects every laya backend as an unknown
// format, so the wire format is configurable in the struct but unsaveable.
func TestConfigSaveAcceptsLayaBackend(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english"}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	saved, exists := config.Get().Classifier.Backends["local"]
	if !exists {
		t.Fatalf("backend %q missing from saved config: %+v", "local", config.Get().Classifier.Backends)
	}
	if saved.Format != config.BackendFormatLaya {
		t.Errorf("saved Format = %q, want laya", saved.Format)
	}
	if saved.URL != "http://127.0.0.1:8000/v1/systemone" {
		t.Errorf("saved URL = %q", saved.URL)
	}
}

func TestConfigSaveRejectsLayaCriteriaKeyMismatch(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	// Same key count, different key sets: criteria {X,Y} against severities
	// {X,Z}. The adapter cannot map a Z verdict to anything, so the save must
	// not land.
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english","layaCriteria":{"X":"one","Y":"two"},"layaSeverityMap":{"X":1,"Z":2}}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "layaSeverityMap") {
		t.Errorf("error should name layaSeverityMap, got %s", recorder.Body.String())
	}
	if _, exists := config.Get().Classifier.Backends["local"]; exists {
		t.Error("rejected backend was saved anyway")
	}
}

func TestConfigSaveRejectsInvalidLayaQuestionName(t *testing.T) {
	for _, name := range []string{"has space", "risk-level", strings.Repeat("q", 33), "rísk"} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := newTestServerWithManager(t)
			quoted, err := json.Marshal(name)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english","layaQuestionName":` + string(quoted) + `}}}`

			recorder := postConfigRules(t, srv, blob)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "layaQuestionName") {
				t.Errorf("error should name layaQuestionName, got %s", recorder.Body.String())
			}
			if _, exists := config.Get().Classifier.Backends["local"]; exists {
				t.Error("rejected backend was saved anyway")
			}
		})
	}
}

func TestConfigSaveAcceptsValidLayaQuestionName(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english","layaQuestionName":"risk_2"}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	if got := config.Get().Classifier.Backends["local"].LayaQuestionName; got != "risk_2" {
		t.Errorf("saved LayaQuestionName = %q, want risk_2", got)
	}
}

func TestConfigSaveRejectsBlankLayaInstructions(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english","layaInstructions":"   \n"}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "layaInstructions") {
		t.Errorf("error should name layaInstructions, got %s", recorder.Body.String())
	}
	if _, exists := config.Get().Classifier.Backends["local"]; exists {
		t.Error("rejected backend was saved anyway")
	}
}
