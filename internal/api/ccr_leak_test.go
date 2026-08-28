package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCCRLeak_Stream_NoLeakAndGaplessIndexes(t *testing.T) {
	var callCount int32

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Iteration 1:
			// Block 0: text
			// Block 1: headroom_retrieve (should be suppressed)
			// Block 2: Read tool_use (should be emitted as index 1)
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input_tokens\":50,\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Checking file...\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			// Block 1: headroom_retrieve
			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_ccr_1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_123\\\"}\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")

			// Block 2: Read
			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_read_1\",\"name\":\"Read\",\"input\":{}}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file_path\\\":\\\"foo.go\\\"}\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":2}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":20}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		} else {
			// Iteration 2:
			// Block 0: final text response (should be emitted as index 2)
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input_tokens\":100,\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Done!\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		}
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			return "chunk content", false
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, "POST", server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return server.Client().Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rawOutput := rec.Body.String()

	// 1. Assert NO headroom_retrieve in output
	if strings.Contains(rawOutput, "headroom_retrieve") {
		t.Fatalf("leak detected: downstream output contains headroom_retrieve:\n%s", rawOutput)
	}
	if strings.Contains(rawOutput, "call_ccr_1") {
		t.Fatalf("leak detected: downstream output contains call_ccr_1:\n%s", rawOutput)
	}

	// 2. Assert Read tool_use is present
	if !strings.Contains(rawOutput, "call_read_1") || !strings.Contains(rawOutput, "Read") {
		t.Fatalf("expected Read tool_use in downstream output:\n%s", rawOutput)
	}

	// 3. Verify sequence of content_block_start events
	lines := strings.Split(rawOutput, "\n")
	var startIndexes []int
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var ev map[string]any
			if err := json.Unmarshal([]byte(dataStr), &ev); err == nil {
				if ev["type"] == "content_block_start" {
					idx := int(ev["index"].(float64))
					startIndexes = append(startIndexes, idx)
				}
			}
		}
	}

	// Expected startIndexes: 0 (text), 1 (Read), 2 (final text)
	expected := []int{0, 1, 2}
	if len(startIndexes) != len(expected) {
		t.Fatalf("expected start indexes %v, got %v", expected, startIndexes)
	}
	for i := range expected {
		if startIndexes[i] != expected[i] {
			t.Fatalf("start index mismatch at pos %d: expected %d, got %d", i, expected[i], startIndexes[i])
		}
	}
}

func TestCCRLeak_Unary_TerminalStrip(t *testing.T) {
	// Upstream returns a response with headroom_retrieve and normal tool_use
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":   "msg_1",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Result"},
				map[string]any{"type": "tool_use", "id": "call_ccr_1", "name": "headroom_retrieve", "input": map[string]any{"chunk_id": "c1"}},
				map[string]any{"type": "tool_use", "id": "call_bash_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 20,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test",
		"messages": []any{
			map[string]any{"role": "user", "content": "run ls"},
		},
	}

	// MaxHydrations: 0 (do not hydrate, terminal on iter 0)
	opts := CCRProxyOptions{
		IsCCREnabled:  func() bool { return true },
		MaxHydrations: 0,
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, "POST", server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return server.Client().Do(req)
		},
	}

	err := ProxyAnthropicJSONWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	content := resp["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks (text + Bash); got %d", len(content))
	}
	if strings.Contains(rec.Body.String(), "headroom_retrieve") {
		t.Fatalf("leak detected in unary response: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Bash") {
		t.Fatalf("expected Bash tool_use in unary response: %s", rec.Body.String())
	}
}

// stopReasonFromSSE returns the stop_reason carried by the message_delta event
// in an SSE body, or "" when there is none.
func stopReasonFromSSE(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			continue
		}
		if ev["type"] != "message_delta" {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		reason, _ := delta["stop_reason"].(string)
		return reason
	}
	return ""
}

// retrieveOnlyStreamHandler emits a turn whose only tool_use is the internal
// headroom_retrieve call, ending in stop_reason "tool_use".
func retrieveOnlyStreamHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n")
		fmt.Fprintf(w, "event: content_block_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_ccr_1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"c1\\\"}\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprintf(w, "event: message_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n")
		fmt.Fprintf(w, "event: message_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	}
}

func senderTo(url string) func(context.Context, []byte) (*http.Response, error) {
	return func(ctx context.Context, body []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(req)
	}
}

// A turn whose only tool_use was the suppressed retrieve call must not reach the
// client as stop_reason "tool_use": the client would block forever waiting to
// answer a tool call it never received.
func TestCCRLeak_Stream_StopReasonReconciledAtCap(t *testing.T) {
	server := httptest.NewServer(retrieveOnlyStreamHandler())
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{"model": "test", "messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}

	// MaxHydrations 0: terminal on the first iteration with the retrieve call
	// still outstanding.
	opts := CCRProxyOptions{
		IsCCREnabled:  func() bool { return true },
		MaxHydrations: 0,
		Sender:        senderTo(server.URL),
	}
	if err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := rec.Body.String()
	if strings.Contains(body, "headroom_retrieve") {
		t.Fatalf("leak detected:\n%s", body)
	}
	if got := stopReasonFromSSE(t, body); got != "end_turn" {
		t.Fatalf("stop_reason = %q; want \"end_turn\" (no tool_use block survived)\n%s", got, body)
	}
}

// A surviving real tool_use must keep stop_reason "tool_use" untouched.
func TestCCRLeak_Stream_StopReasonPreservedWithVisibleToolUse(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n")
		fmt.Fprintf(w, "event: content_block_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_read_1\",\"name\":\"Read\",\"input\":{}}}\n\n")
		fmt.Fprintf(w, "event: content_block_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprintf(w, "event: message_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n")
		fmt.Fprintf(w, "event: message_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	})
	server := httptest.NewServer(upstream)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{"model": "test", "messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}
	opts := CCRProxyOptions{
		IsCCREnabled:  func() bool { return true },
		MaxHydrations: 0,
		Sender:        senderTo(server.URL),
	}
	if err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stopReasonFromSSE(t, rec.Body.String()); got != "tool_use" {
		t.Fatalf("stop_reason = %q; want \"tool_use\" (Read block survived)", got)
	}
}

func TestCCRLeak_Unary_StopReasonReconciledAtCap(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id": "msg_1", "role": "assistant", "stop_reason": "tool_use",
			"content": []any{
				map[string]any{"type": "tool_use", "id": "call_ccr_1", "name": "headroom_retrieve", "input": map[string]any{"chunk_id": "c1"}},
			},
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 4},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	server := httptest.NewServer(upstream)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{"model": "test", "messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}
	opts := CCRProxyOptions{
		IsCCREnabled:  func() bool { return true },
		MaxHydrations: 0,
		Sender:        senderTo(server.URL),
	}
	if err := ProxyAnthropicJSONWithCCR(context.Background(), rec, reqMap, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := resp["stop_reason"].(string); got != "end_turn" {
		t.Fatalf("stop_reason = %q; want \"end_turn\": %s", got, rec.Body.String())
	}
}

// stripRetrieveBlocks must repair stop_reason on the map it filters.
func TestStripRetrieveBlocks_ReconcilesStopReason(t *testing.T) {
	resp := map[string]any{
		"stop_reason": "tool_use",
		"content": []any{
			map[string]any{"type": "text", "text": "hi"},
			map[string]any{"type": "tool_use", "id": "c1", "name": "headroom_retrieve"},
		},
	}
	stripRetrieveBlocks(resp)
	if got, _ := resp["stop_reason"].(string); got != "end_turn" {
		t.Fatalf("stop_reason = %q; want \"end_turn\"", got)
	}

	kept := map[string]any{
		"stop_reason": "tool_use",
		"content": []any{
			map[string]any{"type": "tool_use", "id": "b1", "name": "Bash"},
			map[string]any{"type": "tool_use", "id": "c1", "name": "headroom_retrieve"},
		},
	}
	stripRetrieveBlocks(kept)
	if got, _ := kept["stop_reason"].(string); got != "tool_use" {
		t.Fatalf("stop_reason = %q; want \"tool_use\" (Bash survived)", got)
	}
}
