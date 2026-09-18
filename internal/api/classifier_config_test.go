package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/classifier"
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

func TestClassifierAuditStreamEmitsHistoryAndLiveEvents(t *testing.T) {
	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	srv.classifierAudit.Add(classifier.Event{RuleID: "historic", Status: "stubbed"})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/classifier/audit/stream?history=true", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleClassifierAuditStream(recorder, request)
		close(done)
	}()

	// Give the handler time to subscribe before the live event is pushed.
	time.Sleep(100 * time.Millisecond)
	srv.classifierAudit.Add(classifier.Event{RuleID: "live", Status: "rerouted"})
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return when the request context was cancelled")
	}

	body := recorder.Body.String()
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("expected an SSE content type, got %q", got)
	}
	for _, want := range []string{"historic", "live"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in stream:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event classifier.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("invalid frame %q: %v", line, err)
		}
	}
}
