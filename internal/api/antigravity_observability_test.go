package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
	"antigravity-go-proxy/internal/logger"
	"antigravity-go-proxy/internal/stats"
)

type agyFakeResolver struct{}

func (r *agyFakeResolver) Resolve(_ context.Context, account *accounts.Account) (auth.Credentials, error) {
	return auth.Credentials{AccessToken: "token-1", Email: account.Email}, nil
}

func (r *agyFakeResolver) Invalidate(string) {}

func agyCaptureLog(buf *bytes.Buffer, broadcaster *logger.Broadcaster) *slog.Logger {
	jsonH := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	if broadcaster != nil {
		return slog.New(logger.NewStreamHandler(jsonH, broadcaster))
	}
	return slog.New(jsonH)
}

func agyObservabilityRecords(buf *bytes.Buffer, prefix string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, ok := rec["msg"].(string); ok && strings.Contains(msg, prefix) {
			out = append(out, rec)
		}
	}
	return out
}

func fakeCloudCodeResponse(text string, in, out, cr, thinking int) []byte {
	payload := map[string]any{
		"response": map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{"thought": true, "text": "pondering...", "thoughtSignature": "sig12345678901234567890"},
							map[string]any{"text": text},
						},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount":        in + cr,
				"candidatesTokenCount":    out,
				"cachedContentTokenCount": cr,
				"thoughtsTokenCount":      thinking,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

type fakeAgyClient struct {
	responseBytes []byte
}

func (c *fakeAgyClient) LoadCodeAssist(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: http.StatusOK, Body: []byte(`{"cloudaicompanionProject":"proj-agy-1"}`)}, nil
}

func (c *fakeAgyClient) FetchAvailableModels(context.Context, string) (cloudcode.Response, error) {
	catalogJSON := `{
		"models": {
			"claude-sonnet-4-6": {"displayName": "Claude Sonnet 4.6"},
			"claude-opus-4-6-thinking": {"displayName": "Claude Opus 4.6 (Thinking)"}
		},
		"agentModelSorts": [
			{"groups": [{"modelIds": ["claude-sonnet-4-6", "claude-opus-4-6-thinking"]}]}
		]
	}`
	return cloudcode.Response{StatusCode: http.StatusOK, Body: []byte(catalogJSON)}, nil
}

func (c *fakeAgyClient) StreamGenerateContent(_ context.Context, _ any, _ cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	err := consume(cloudcode.SSEEvent{Data: c.responseBytes})
	return cloudcode.Response{StatusCode: http.StatusOK}, err
}

func TestAntigravityObservability_Unary(t *testing.T) {
	var logBuf bytes.Buffer
	broadcaster := logger.NewBroadcaster(50)
	log := agyCaptureLog(&logBuf, broadcaster)

	cloudResp := fakeCloudCodeResponse("Hello from Claude Sonnet!", 1000, 250, 400, 80)
	client := &fakeAgyClient{responseBytes: cloudResp}

	acc := &accounts.Account{
		Email:   "dev@example.com",
		APIKey:  "key-1",
		Enabled: true,
	}
	mgr, err := accounts.New(accounts.Options{
		Accounts: []*accounts.Account{acc},
		Strategy: accounts.StrategyHybrid,
	})
	if err != nil {
		t.Fatal(err)
	}

	disp, err := accounts.NewDispatcher(accounts.DispatcherOptions{
		Manager:  mgr,
		Resolver: &agyFakeResolver{},
		NewClient: func(string) accounts.CloudClient {
			return client
		},
		ProjectID: "proj-test-123",
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		backend:     disp,
		builder:     proxyformat.NewBuilder(),
		logger:      log,
		broadcaster: broadcaster,
		now:         time.Now,
	}

	reqBody := `{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-unary-1")
	w := httptest.NewRecorder()

	srv.messages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify structured slog record
	recs := agyObservabilityRecords(&logBuf, "[Antigravity]")
	if len(recs) == 0 {
		t.Fatalf("expected [Antigravity] log record, got:\n%s", logBuf.String())
	}
	rec := recs[0]

	if rec["gateway"] != "antigravity" {
		t.Errorf("gateway = %v, want antigravity", rec["gateway"])
	}
	if rec["model"] != "claude-sonnet-4-6" {
		t.Errorf("model = %v, want claude-sonnet-4-6", rec["model"])
	}
	if rec["account"] != "dev@example.com" {
		t.Errorf("account = %v, want dev@example.com", rec["account"])
	}
	if rec["session_id"] != "sess-unary-1" {
		t.Errorf("session_id = %v, want sess-unary-1", rec["session_id"])
	}
	if rec["input_tokens"] != float64(1000) {
		t.Errorf("input_tokens = %v, want 1000", rec["input_tokens"])
	}
	if rec["output_tokens"] != float64(250) {
		t.Errorf("output_tokens = %v, want 250", rec["output_tokens"])
	}
	if rec["cache_read_tokens"] != float64(400) {
		t.Errorf("cache_read_tokens = %v, want 400", rec["cache_read_tokens"])
	}
	if rec["thinking_tokens"] != float64(80) {
		t.Errorf("thinking_tokens = %v, want 80", rec["thinking_tokens"])
	}
	if rec["level_tag"] != "SUCCESS" {
		t.Errorf("level_tag = %v, want SUCCESS", rec["level_tag"])
	}
	if cost, ok := rec["retail_cost_usd"].(float64); !ok || cost <= 0 {
		t.Errorf("retail_cost_usd = %v, want > 0", rec["retail_cost_usd"])
	}

	msg, _ := rec["msg"].(string)
	if !strings.Contains(msg, "[Antigravity] claude-sonnet-4-6 (dev@example.com)") {
		t.Errorf("expected model and account in message, got: %s", msg)
	}
	if !strings.Contains(msg, "80 thinking") {
		t.Errorf("expected thinking tokens in message, got: %s", msg)
	}
	if !strings.Contains(msg, "saved") {
		t.Errorf("expected 'saved' keyword in message, got: %s", msg)
	}

	// Verify WebUI Broadcaster received entry with Level == "SUCCESS"
	history := broadcaster.GetHistory()
	foundSuccess := false
	for _, entry := range history {
		if entry.Level == "SUCCESS" && strings.Contains(entry.Message, "[Antigravity]") {
			foundSuccess = true
			break
		}
	}
	if !foundSuccess {
		t.Errorf("WebUI Broadcaster did not receive SUCCESS log entry, history: %+v", history)
	}
}

func TestAntigravityObservability_Streaming(t *testing.T) {
	var logBuf bytes.Buffer
	broadcaster := logger.NewBroadcaster(50)
	log := agyCaptureLog(&logBuf, broadcaster)

	cloudResp := fakeCloudCodeResponse("Streamed token reply", 1200, 300, 600, 120)
	client := &fakeAgyClient{responseBytes: cloudResp}

	acc := &accounts.Account{
		Email:   "streamer@example.com",
		APIKey:  "key-stream",
		Enabled: true,
	}
	mgr, err := accounts.New(accounts.Options{
		Accounts: []*accounts.Account{acc},
		Strategy: accounts.StrategyHybrid,
	})
	if err != nil {
		t.Fatal(err)
	}

	disp, err := accounts.NewDispatcher(accounts.DispatcherOptions{
		Manager:  mgr,
		Resolver: &agyFakeResolver{},
		NewClient: func(string) accounts.CloudClient {
			return client
		},
		ProjectID: "proj-stream-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		backend:     disp,
		builder:     proxyformat.NewBuilder(),
		logger:      log,
		broadcaster: broadcaster,
		now:         time.Now,
	}

	reqBody := `{"model":"claude-opus-4-6-thinking","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-stream-42")
	w := httptest.NewRecorder()

	srv.messages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	recs := agyObservabilityRecords(&logBuf, "[Antigravity]")
	if len(recs) == 0 {
		t.Fatalf("expected [Antigravity] log record, got:\n%s", logBuf.String())
	}
	rec := recs[0]

	if rec["gateway"] != "antigravity" {
		t.Errorf("gateway = %v, want antigravity", rec["gateway"])
	}
	if rec["model"] != "claude-opus-4-6-thinking" {
		t.Errorf("model = %v, want claude-opus-4-6-thinking", rec["model"])
	}
	if rec["account"] != "streamer@example.com" {
		t.Errorf("account = %v, want streamer@example.com", rec["account"])
	}
	if rec["session_id"] != "sess-stream-42" {
		t.Errorf("session_id = %v, want sess-stream-42", rec["session_id"])
	}
	if rec["input_tokens"] != float64(1200) {
		t.Errorf("input_tokens = %v, want 1200", rec["input_tokens"])
	}
	if rec["output_tokens"] != float64(300) {
		t.Errorf("output_tokens = %v, want 300", rec["output_tokens"])
	}
	if rec["cache_read_tokens"] != float64(600) {
		t.Errorf("cache_read_tokens = %v, want 600", rec["cache_read_tokens"])
	}
	if rec["thinking_tokens"] != float64(120) {
		t.Errorf("thinking_tokens = %v, want 120", rec["thinking_tokens"])
	}
	if rec["level_tag"] != "SUCCESS" {
		t.Errorf("level_tag = %v, want SUCCESS", rec["level_tag"])
	}

	// Verify WebUI Broadcaster received entry with Level == "SUCCESS"
	history := broadcaster.GetHistory()
	foundSuccess := false
	for _, entry := range history {
		if entry.Level == "SUCCESS" && strings.Contains(entry.Message, "[Antigravity]") {
			foundSuccess = true
			break
		}
	}
	if !foundSuccess {
		t.Errorf("WebUI Broadcaster did not receive SUCCESS log entry, history: %+v", history)
	}
}

func TestKimiObservability_Unary(t *testing.T) {
	const respBody = `{"id":"msg_kimi_1","type":"message","model":"moonshot-v1-8k",` +
		`"content":[{"type":"text","text":"hello from kimi"}],` +
		`"usage":{"input_tokens":500,"output_tokens":120,"cache_read_input_tokens":200,"cache_creation_input_tokens":50}}`

	kimiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer kimiServer.Close()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker, _ := stats.NewTracker("")

	srv := &Server{
		logger:  log,
		tracker: tracker,
		now:     time.Now,
	}

	kimiCfg := config.KimiConfig{
		Enabled: true,
		BaseURL: kimiServer.URL,
		APIKey:  "kimi-key-test",
	}

	reqBody := `{"model":"moonshot-v1-8k","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-kimi-unary")
	w := httptest.NewRecorder()

	srv.forwardToKimi(w, req, kimiCfg, []byte(reqBody), "moonshot-v1-8k")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recs := agyObservabilityRecords(&logBuf, "[Kimi]")
	if len(recs) == 0 {
		t.Fatalf("expected [Kimi] log record, got:\n%s", logBuf.String())
	}
	rec := recs[0]

	if rec["gateway"] != "kimi" {
		t.Errorf("gateway = %v, want kimi", rec["gateway"])
	}
	if rec["model"] != "moonshot-v1-8k" {
		t.Errorf("model = %v, want moonshot-v1-8k", rec["model"])
	}
	if rec["session_id"] != "sess-kimi-unary" {
		t.Errorf("session_id = %v, want sess-kimi-unary", rec["session_id"])
	}
	if rec["input_tokens"] != float64(500) {
		t.Errorf("input_tokens = %v, want 500", rec["input_tokens"])
	}
	if rec["output_tokens"] != float64(120) {
		t.Errorf("output_tokens = %v, want 120", rec["output_tokens"])
	}
	if rec["cache_read_tokens"] != float64(200) {
		t.Errorf("cache_read_tokens = %v, want 200", rec["cache_read_tokens"])
	}
	if rec["level_tag"] != "SUCCESS" {
		t.Errorf("level_tag = %v, want SUCCESS", rec["level_tag"])
	}

	if history := tracker.GetHistory(); len(history) == 0 {
		t.Errorf("expected request recorded in stats tracker")
	}
}

func TestKimiObservability_Streaming(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_kimi_s","usage":{"input_tokens":600,"cache_read_input_tokens":150}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":85}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	kimiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer kimiServer.Close()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker, _ := stats.NewTracker("")

	srv := &Server{
		logger:  log,
		tracker: tracker,
		now:     time.Now,
	}

	kimiCfg := config.KimiConfig{
		Enabled: true,
		BaseURL: kimiServer.URL,
		APIKey:  "kimi-key-stream",
	}

	reqBody := `{"model":"moonshot-v1-32k","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-kimi-stream")
	w := httptest.NewRecorder()

	srv.forwardToKimi(w, req, kimiCfg, []byte(reqBody), "moonshot-v1-32k")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recs := agyObservabilityRecords(&logBuf, "[Kimi]")
	if len(recs) == 0 {
		t.Fatalf("expected [Kimi] log record, got:\n%s", logBuf.String())
	}
	rec := recs[0]

	if rec["gateway"] != "kimi" {
		t.Errorf("gateway = %v, want kimi", rec["gateway"])
	}
	if rec["model"] != "moonshot-v1-32k" {
		t.Errorf("model = %v, want moonshot-v1-32k", rec["model"])
	}
	if rec["session_id"] != "sess-kimi-stream" {
		t.Errorf("session_id = %v, want sess-kimi-stream", rec["session_id"])
	}
	if rec["input_tokens"] != float64(600) {
		t.Errorf("input_tokens = %v, want 600", rec["input_tokens"])
	}
	if rec["output_tokens"] != float64(85) {
		t.Errorf("output_tokens = %v, want 85", rec["output_tokens"])
	}
	if rec["cache_read_tokens"] != float64(150) {
		t.Errorf("cache_read_tokens = %v, want 150", rec["cache_read_tokens"])
	}
	if rec["level_tag"] != "SUCCESS" {
		t.Errorf("level_tag = %v, want SUCCESS", rec["level_tag"])
	}

	if history := tracker.GetHistory(); len(history) == 0 {
		t.Errorf("expected request recorded in stats tracker")
	}
}

func TestKimiObservability_CCRStreaming(t *testing.T) {
	var callCount int32
	kimiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		fmt.Fprintf(w, "event: message_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ccr\",\"usage\":{\"input_tokens\":400,\"cache_read_input_tokens\":100}}}\n\n")
		fmt.Fprintf(w, "event: content_block_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hello ccr\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprintf(w, "event: message_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":50}}\n\n")
		fmt.Fprintf(w, "event: message_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer kimiServer.Close()

	store := ccr.NewCCRStore(1024 * 1024)
	engine := headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: true},
	}, nil, ccr.NewStage(store))

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker, _ := stats.NewTracker("")

	srv := &Server{
		headroom: engine,
		ccrStore: store,
		logger:   log,
		tracker:  tracker,
		now:      time.Now,
	}

	kimiCfg := config.KimiConfig{
		Enabled: true,
		BaseURL: kimiServer.URL,
		APIKey:  "kimi-key-ccr",
	}

	reqBody := `{"model":"moonshot-v1-8k","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-kimi-ccr")
	w := httptest.NewRecorder()

	srv.forwardToKimi(w, req, kimiCfg, []byte(reqBody), "moonshot-v1-8k")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recs := agyObservabilityRecords(&logBuf, "[Kimi]")
	if len(recs) == 0 {
		t.Fatalf("expected [Kimi] log record, got:\n%s", logBuf.String())
	}
	rec := recs[0]

	if rec["gateway"] != "kimi" {
		t.Errorf("gateway = %v, want kimi", rec["gateway"])
	}
	if rec["level_tag"] != "SUCCESS" {
		t.Errorf("level_tag = %v, want SUCCESS", rec["level_tag"])
	}
	if rec["input_tokens"] != float64(400) {
		t.Errorf("input_tokens = %v, want 400", rec["input_tokens"])
	}
	if rec["output_tokens"] != float64(50) {
		t.Errorf("output_tokens = %v, want 50", rec["output_tokens"])
	}

	if history := tracker.GetHistory(); len(history) == 0 {
		t.Errorf("expected request recorded in stats tracker")
	}
}

func TestKimiObservability_GzipResponse(t *testing.T) {
	const jsonBody = `{"id":"msg_kimi_gz","type":"message","model":"moonshot-v1-8k",` +
		`"content":[{"type":"text","text":"gzipped response"}],` +
		`"usage":{"input_tokens":550,"output_tokens":130,"cache_read_input_tokens":220,"cache_creation_input_tokens":60}}`

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte(jsonBody))
	_ = gw.Close()

	kimiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzBuf.Bytes())
	}))
	defer kimiServer.Close()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := &Server{
		logger: log,
		now:    time.Now,
	}

	kimiCfg := config.KimiConfig{
		Enabled: true,
		BaseURL: kimiServer.URL,
		APIKey:  "kimi-key-gzip",
	}

	reqBody := `{"model":"moonshot-v1-8k","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("x-session-id", "sess-kimi-gzip")
	w := httptest.NewRecorder()

	srv.forwardToKimi(w, req, kimiCfg, []byte(reqBody), "moonshot-v1-8k")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify Content-Length header is set and matches gzipped body size
	if cl := w.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", gzBuf.Len()) {
		t.Errorf("Content-Length = %q, want %d", cl, gzBuf.Len())
	}

	recs := agyObservabilityRecords(&logBuf, "[Kimi]")
	if len(recs) == 0 {
		t.Fatalf("expected [Kimi] log record, got:\n%s", logBuf.String())
	}
	rec := recs[0]

	if rec["input_tokens"] != float64(550) {
		t.Errorf("input_tokens = %v, want 550", rec["input_tokens"])
	}
	if rec["output_tokens"] != float64(130) {
		t.Errorf("output_tokens = %v, want 130", rec["output_tokens"])
	}
	if rec["cache_read_tokens"] != float64(220) {
		t.Errorf("cache_read_tokens = %v, want 220", rec["cache_read_tokens"])
	}
}
