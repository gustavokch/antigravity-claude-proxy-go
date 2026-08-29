package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
)

// ccCaptureLog returns a logger writing JSON records into buf.
func ccCaptureLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ccObservabilityRecords returns every slog record emitted by
// claudecode.LogObservability, in emission order.
func ccObservabilityRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, ok := rec["msg"].(string); ok && strings.Contains(msg, "[ClaudeCode]") {
			out = append(out, rec)
		}
	}
	return out
}

// ccFindObservabilityRecord returns the parsed slog record emitted by
// claudecode.LogObservability, or nil when none was emitted.
func ccFindObservabilityRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	recs := ccObservabilityRecords(t, buf)
	if len(recs) == 0 {
		return nil
	}
	return recs[0]
}

func ccTestConfig(baseURL string) claudecode.Config {
	return claudecode.Config{
		Enabled: true,
		BaseURL: baseURL,
		Mode:    "pool",
		Accounts: []claudecode.AccountConfig{
			{ID: "acc1", Name: "primary", Token: "sk-ant-test", Enabled: true},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}
}

func ccResetPool() {
	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()
}

func ccNum(t *testing.T, rec map[string]any, key string) float64 {
	t.Helper()
	v, ok := rec[key].(float64)
	if !ok {
		t.Fatalf("record missing numeric field %q: %v", key, rec)
	}
	return v
}

// TestClaudeCodeObservability_UnaryUsage proves the gateway must read token
// usage from the upstream RESPONSE body, like the OpenRouter gateway does.
func TestClaudeCodeObservability_UnaryUsage(t *testing.T) {
	const respBody = `{"id":"msg_01","type":"message","model":"claude-opus-5",` +
		`"content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":1200,"output_tokens":345,` +
		`"cache_read_input_tokens":800,"cache_creation_input_tokens":100}}`

	upstream := fakeMsgServer(t, http.StatusOK, respBody)
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],` +
		`"metadata":{"user_id":"user_abc_session_xyz"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if w.Body.String() != respBody {
		t.Fatalf("response body altered:\n got: %s\nwant: %s", w.Body.String(), respBody)
	}

	rec := ccFindObservabilityRecord(t, &buf)
	if rec == nil {
		t.Fatalf("no [ClaudeCode] observability record emitted; log:\n%s", buf.String())
	}
	t.Logf("emitted: %v", rec["msg"])

	if got := ccNum(t, rec, "input_tokens"); got != 1200 {
		t.Errorf("input_tokens = %v, want 1200", got)
	}
	if got := ccNum(t, rec, "output_tokens"); got != 345 {
		t.Errorf("output_tokens = %v, want 345", got)
	}
	if got := ccNum(t, rec, "cache_read_tokens"); got != 800 {
		t.Errorf("cache_read_tokens = %v, want 800", got)
	}
	if got := ccNum(t, rec, "cache_creation_tokens"); got != 100 {
		t.Errorf("cache_creation_tokens = %v, want 100", got)
	}
	if got := ccNum(t, rec, "call_cost_usd"); got <= 0 {
		t.Errorf("call_cost_usd = %v, want > 0", got)
	}
	if got, _ := rec["account_id"].(string); got != "acc1" {
		t.Errorf("account_id = %q, want \"acc1\"", got)
	}
	if got, _ := rec["session_id"].(string); got != "user_abc_session_xyz" {
		t.Errorf("session_id = %q, want \"user_abc_session_xyz\"", got)
	}
	if got, _ := rec["gateway"].(string); got != "claudecode" {
		t.Errorf("gateway = %q, want \"claudecode\"", got)
	}
}

// TestClaudeCodeObservability_StreamUsage proves streaming responses must be
// intercepted for SSE usage events, like proxyStreamResponse does for OpenRouter.
func TestClaudeCodeObservability_StreamUsage(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_02","usage":{"input_tokens":900,"cache_read_input_tokens":400,"cache_creation_input_tokens":50}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":210}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", "sess-stream-1")
	w := httptest.NewRecorder()

	var buf bytes.Buffer
	srv := &Server{logger: ccCaptureLog(&buf)}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != sse {
		t.Fatalf("stream body altered:\n got: %q\nwant: %q", w.Body.String(), sse)
	}

	rec := ccFindObservabilityRecord(t, &buf)
	if rec == nil {
		t.Fatalf("no [ClaudeCode] observability record emitted; log:\n%s", buf.String())
	}
	t.Logf("emitted: %v", rec["msg"])
	if got := ccNum(t, rec, "input_tokens"); got != 900 {
		t.Errorf("input_tokens = %v, want 900", got)
	}
	if got := ccNum(t, rec, "output_tokens"); got != 210 {
		t.Errorf("output_tokens = %v, want 210", got)
	}
	if got := ccNum(t, rec, "cache_read_tokens"); got != 400 {
		t.Errorf("cache_read_tokens = %v, want 400", got)
	}
	if got := ccNum(t, rec, "tps"); got <= 0 {
		t.Errorf("tps = %v, want > 0", got)
	}
	if got, _ := rec["session_id"].(string); got != "sess-stream-1" {
		t.Errorf("session_id = %q, want \"sess-stream-1\"", got)
	}
}

// ccCCRServer builds a headroom engine with CCR enabled plus a stored chunk,
// so forwardToClaudeCode takes the CCR hydration path.
func ccCCRServer(t *testing.T, buf *bytes.Buffer, payload string) (*Server, string) {
	t.Helper()
	store := ccr.NewCCRStore(1024 * 1024)
	engine := headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: true},
	}, nil, ccr.NewStage(store))
	chunkID, ok := store.Put(payload)
	if !ok {
		t.Fatalf("failed to store CCR chunk")
	}
	srv := &Server{headroom: engine, ccrStore: store, logger: ccCaptureLog(buf)}
	if !srv.isCCREnabled() {
		t.Fatalf("CCR path not enabled; test would exercise the direct path")
	}
	return srv, chunkID
}

// TestClaudeCodeObservability_CCRStreamUsage covers the CCR hydration path:
// each upstream call must produce its own observability record with the usage
// reported by that call, captured below the CCR rewriter.
func TestClaudeCodeObservability_CCRStreamUsage(t *testing.T) {
	var chunkID string
	var callCount int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Turn 1: model asks for the stored chunk.
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_r1\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":500,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":20}}}\n\n")
			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_r1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"%s\\\"}\"}}\n\n", chunkID)
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")
			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
			return
		}

		// Turn 2: hydrated answer.
		fmt.Fprintf(w, "event: message_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_r2\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":700,\"cache_read_input_tokens\":200,\"cache_creation_input_tokens\":0}}}\n\n")
		fmt.Fprintf(w, "event: content_block_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hydrated answer\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprintf(w, "event: message_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":15}}\n\n")
		fmt.Fprintf(w, "event: message_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	var buf bytes.Buffer
	srv, id := ccCCRServer(t, &buf, "secret payload")
	chunkID = id

	reqBody := `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"fetch chunk"}],` +
		`"metadata":{"user_id":"user_ccr_session_1"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&callCount); got != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", got)
	}
	if body := w.Body.String(); !strings.Contains(body, "hydrated answer") {
		t.Fatalf("hydrated text missing from stream: %s", body)
	}

	recs := ccObservabilityRecords(t, &buf)
	if len(recs) != 2 {
		t.Fatalf("expected 1 observability record per upstream call, got %d; log:\n%s", len(recs), buf.String())
	}
	for i, rec := range recs {
		t.Logf("emitted[%d]: %v", i, rec["msg"])
	}

	// Turn 1 usage, captured below the CCR rewriter.
	if got := ccNum(t, recs[0], "input_tokens"); got != 500 {
		t.Errorf("call 1 input_tokens = %v, want 500", got)
	}
	if got := ccNum(t, recs[0], "output_tokens"); got != 10 {
		t.Errorf("call 1 output_tokens = %v, want 10", got)
	}
	if got := ccNum(t, recs[0], "cache_read_tokens"); got != 100 {
		t.Errorf("call 1 cache_read_tokens = %v, want 100", got)
	}
	if got := ccNum(t, recs[0], "cache_creation_tokens"); got != 20 {
		t.Errorf("call 1 cache_creation_tokens = %v, want 20", got)
	}

	// Turn 2 usage.
	if got := ccNum(t, recs[1], "input_tokens"); got != 700 {
		t.Errorf("call 2 input_tokens = %v, want 700", got)
	}
	if got := ccNum(t, recs[1], "output_tokens"); got != 15 {
		t.Errorf("call 2 output_tokens = %v, want 15", got)
	}
	if got := ccNum(t, recs[1], "cache_read_tokens"); got != 200 {
		t.Errorf("call 2 cache_read_tokens = %v, want 200", got)
	}

	for i, rec := range recs {
		if got, _ := rec["account_id"].(string); got != "acc1" {
			t.Errorf("record %d account_id = %q, want \"acc1\"", i, got)
		}
		if got, _ := rec["session_id"].(string); got != "user_ccr_session_1" {
			t.Errorf("record %d session_id = %q, want \"user_ccr_session_1\"", i, got)
		}
		if got := ccNum(t, rec, "call_cost_usd"); got <= 0 {
			t.Errorf("record %d call_cost_usd = %v, want > 0", i, got)
		}
	}

	// Session cost accumulates across the hydration turns.
	if first, second := ccNum(t, recs[0], "session_cost_usd"), ccNum(t, recs[1], "session_cost_usd"); second <= first {
		t.Errorf("session_cost_usd did not accumulate: %v then %v", first, second)
	}
}

// TestClaudeCodeObservability_CCRUnaryUsage covers the non-streaming CCR path,
// where each upstream JSON response must be buffered, parsed and reported.
func TestClaudeCodeObservability_CCRUnaryUsage(t *testing.T) {
	var chunkID string
	var callCount int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_u1", "type": "message", "role": "assistant", "model": "claude-sonnet-5",
				"content": []any{map[string]any{
					"type": "tool_use", "id": "toolu_u1", "name": "headroom_retrieve",
					"input": map[string]any{"chunk_id": chunkID},
				}},
				"stop_reason": "tool_use",
				"usage": map[string]any{
					"input_tokens": 600, "output_tokens": 10,
					"cache_read_input_tokens": 150, "cache_creation_input_tokens": 30,
				},
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_u2", "type": "message", "role": "assistant", "model": "claude-sonnet-5",
			"content":     []any{map[string]any{"type": "text", "text": "unary hydrated answer"}},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens": 800, "output_tokens": 14,
				"cache_read_input_tokens": 250, "cache_creation_input_tokens": 0,
			},
		})
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	getOrCreateCCPool(cfg)

	var buf bytes.Buffer
	srv, id := ccCCRServer(t, &buf, "secret unary payload")
	chunkID = id

	reqBody := `{"model":"claude-opus-5","stream":false,"messages":[{"role":"user","content":"fetch chunk"}],` +
		`"metadata":{"user_id":"user_ccr_session_2"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-opus-5")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&callCount); got != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", got)
	}
	if body := w.Body.String(); !strings.Contains(body, "unary hydrated answer") {
		t.Fatalf("hydrated text missing from response: %s", body)
	}

	recs := ccObservabilityRecords(t, &buf)
	if len(recs) != 2 {
		t.Fatalf("expected 1 observability record per upstream call, got %d; log:\n%s", len(recs), buf.String())
	}
	for i, rec := range recs {
		t.Logf("emitted[%d]: %v", i, rec["msg"])
	}

	if got := ccNum(t, recs[0], "input_tokens"); got != 600 {
		t.Errorf("call 1 input_tokens = %v, want 600", got)
	}
	if got := ccNum(t, recs[0], "cache_read_tokens"); got != 150 {
		t.Errorf("call 1 cache_read_tokens = %v, want 150", got)
	}
	if got := ccNum(t, recs[0], "cache_creation_tokens"); got != 30 {
		t.Errorf("call 1 cache_creation_tokens = %v, want 30", got)
	}
	if got := ccNum(t, recs[1], "input_tokens"); got != 800 {
		t.Errorf("call 2 input_tokens = %v, want 800", got)
	}
	if got := ccNum(t, recs[1], "output_tokens"); got != 14 {
		t.Errorf("call 2 output_tokens = %v, want 14", got)
	}
	if got := ccNum(t, recs[1], "cache_read_tokens"); got != 250 {
		t.Errorf("call 2 cache_read_tokens = %v, want 250", got)
	}

	for i, rec := range recs {
		if got, _ := rec["session_id"].(string); got != "user_ccr_session_2" {
			t.Errorf("record %d session_id = %q, want \"user_ccr_session_2\"", i, got)
		}
		if got, _ := rec["gateway"].(string); got != "claudecode" {
			t.Errorf("record %d gateway = %q, want \"claudecode\"", i, got)
		}
	}
}

func TestGetOrCreateCCPool_BaseURLUpdate(t *testing.T) {
	ccResetPool()
	cfg1 := ccTestConfig("https://api.anthropic.com")
	_, client1 := getOrCreateCCPool(cfg1)
	if client1 == nil {
		t.Fatal("expected non-nil client")
	}

	cfg2 := ccTestConfig("https://custom.anthropic.internal")
	_, client2 := getOrCreateCCPool(cfg2)
	if client2 == nil {
		t.Fatal("expected non-nil client")
	}
	if client1 == client2 {
		t.Errorf("expected new client instance when BaseURL changes")
	}
}

func TestExtractSessionID_TopLevelUserID(t *testing.T) {
	tests := []struct {
		name   string
		header string
		body   map[string]any
		want   string
	}{
		{
			name:   "header takes precedence",
			header: "sess-header",
			body:   map[string]any{"user_id": "usr-body", "metadata": map[string]any{"user_id": "meta-usr"}},
			want:   "sess-header",
		},
		{
			name: "metadata session_id",
			body: map[string]any{"metadata": map[string]any{"session_id": "meta-sess", "user_id": "meta-usr"}},
			want: "meta-sess",
		},
		{
			name: "metadata user_id",
			body: map[string]any{"metadata": map[string]any{"user_id": "meta-usr"}},
			want: "meta-usr",
		},
		{
			name: "top-level session_id",
			body: map[string]any{"session_id": "top-sess", "user_id": "top-usr"},
			want: "top-sess",
		},
		{
			name: "top-level user_id",
			body: map[string]any{"user_id": "top-usr"},
			want: "top-usr",
		},
		{
			name: "empty body and header",
			body: map[string]any{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tc.header != "" {
				req.Header.Set("x-session-id", tc.header)
			}
			got := ccExtractSessionID(req, tc.body)
			if got != tc.want {
				t.Errorf("ccExtractSessionID() = %q, want %q", got, tc.want)
			}
		})
	}
}
