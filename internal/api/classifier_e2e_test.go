package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/config"
)

func TestEndToEndClassifierFlow(t *testing.T) {
	// 1. Stub backend
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"<severity>0</severity>"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	defer backend.Close()

	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	srv.applyClassifierConfig(config.ClassifierConfig{
		Enabled: true,
		Rules: []config.Rule{{
			ID:      "stage1-test",
			Name:    "Stage 1",
			Enabled: true,
			Conditions: config.RuleConditions{
				FooterPatterns: []config.MatchPattern{
					{Type: config.PatternSubstring, Pattern: "Grade HARM ONLY — do NOT reduce for user intent"},
				},
			},
			Action:        config.RuleActionReroute,
			TargetBackend: "local",
		}},
		Backends: map[string]config.TargetBackend{
			"local": {Name: "Local Stub", URL: backend.URL, Format: config.BackendFormatOpenAI, Model: "local-judge"},
		},
	})

	reqBody := `{"model":"claude-sonnet-5","max_tokens":64,
		"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],
		"messages":[{"role":"user","content":[{"type":"text","text":"<transcript></transcript>Grade HARM ONLY — do NOT reduce for user intent"}]}]}`

	// 2. Non-streaming reroute
	rec := httptest.NewRecorder()
	rule, target, matched := srv.classifierMatcher.Match([]byte(reqBody))
	if !matched {
		t.Fatal("expected match")
	}
	responded, skipDetect := srv.applyClassifierRule(
		rec,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody)),
		classifierRequest{
			rule:            rule,
			backend:         target,
			rawBody:         []byte(reqBody),
			model:           "claude-sonnet-5",
			streamRequested: false,
		},
	)
	if !responded || skipDetect || rec.Code != http.StatusOK {
		t.Fatalf("expected 200 responded, got %v, %v, %d", responded, skipDetect, rec.Code)
	}
	var msg struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(msg.ID, "msg_clf_") || msg.Model != "claude-sonnet-5" || len(msg.Content) != 1 || msg.Content[0].Text != "<severity>0</severity>" {
		t.Fatalf("unexpected message: %+v", msg)
	}

	// 3. Streaming reroute
	recStream := httptest.NewRecorder()
	respondedStream, _ := srv.applyClassifierRule(
		recStream,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody)),
		classifierRequest{
			rule:            rule,
			backend:         target,
			rawBody:         []byte(reqBody),
			model:           "claude-sonnet-5",
			streamRequested: true,
		},
	)
	if !respondedStream || recStream.Code != http.StatusOK {
		t.Fatalf("expected stream responded, got %v, %d", respondedStream, recStream.Code)
	}
	streamStr := recStream.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "<severity>0</severity>", "event: message_stop"} {
		if !strings.Contains(streamStr, want) {
			t.Errorf("stream missing %q", want)
		}
	}

	// 4. Audit events
	hist := srv.classifierAudit.History()
	if len(hist) != 2 || hist[0].Status != "rerouted" || hist[1].Status != "rerouted" {
		t.Fatalf("unexpected audit history: %+v", hist)
	}

	// 5. Fail-open when backend dies
	backend.Close()
	recFail := httptest.NewRecorder()
	respondedFail, skipDetectFail := srv.applyClassifierRule(
		recFail,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody)),
		classifierRequest{
			rule:            rule,
			backend:         target,
			rawBody:         []byte(reqBody),
			model:           "claude-sonnet-5",
			streamRequested: false,
		},
	)
	if respondedFail {
		t.Error("dead backend must not respond")
	}
	if skipDetectFail {
		t.Error("dead backend must not skip detect")
	}
	histAfter := srv.classifierAudit.History()
	if len(histAfter) != 3 || histAfter[2].Status != "error" {
		t.Fatalf("expected error audit entry, got %+v", histAfter)
	}
}
