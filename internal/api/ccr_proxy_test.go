package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCCRProxyStream_SingleHydration(t *testing.T) {
	var callCount int32
	var capturedRequests [][]byte

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedRequests = append(capturedRequests, body)
		curr := atomic.AddInt32(&callCount, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Iteration 1: Text block + headroom_retrieve tool_use
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":50,\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Thinking... \"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_abc\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_999\\\"}\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":15}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		} else {
			// Iteration 2: Final response after hydration
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":150,\"output_tokens\":5}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Here is the retrieved answer.\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":25}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		}
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "What is in chunk 999?"},
		},
	}

	var recordedRetrievals int
	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			if chunkID == "chunk_999" {
				return "Secret data inside chunk 999", false
			}
			return "Not found", true
		},
		RecordHeadroom: func(count int) {
			recordedRetrievals = count
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	if atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("Expected 2 upstream calls, got %d", callCount)
	}

	if recordedRetrievals != 1 {
		t.Fatalf("Expected 1 recorded retrieval, got %d", recordedRetrievals)
	}

	// Verify request sent on second iteration contains assistant and tool_result turns
	if len(capturedRequests) < 2 {
		t.Fatalf("Expected 2 captured requests, got %d", len(capturedRequests))
	}
	var secondReq map[string]any
	if err := json.Unmarshal(capturedRequests[1], &secondReq); err != nil {
		t.Fatalf("Failed to parse second request: %v", err)
	}
	msgs, ok := secondReq["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("Expected 3 messages in second request (user, assistant, user tool_result), got %d", len(msgs))
	}
	userToolMsg, _ := msgs[2].(map[string]any)
	toolResults, _ := userToolMsg["content"].([]any)
	if len(toolResults) != 1 {
		t.Fatalf("Expected 1 tool result, got %d", len(toolResults))
	}
	firstResult, _ := toolResults[0].(map[string]any)
	if firstResult["content"] != "Secret data inside chunk 999" {
		t.Fatalf("Expected hydrated payload in tool_result, got: %v", firstResult["content"])
	}

	// Verify client received stream
	clientResp := rec.Body.String()
	if strings.Contains(clientResp, "headroom_retrieve") {
		t.Fatalf("Downstream client leaked headroom_retrieve event: %s", clientResp)
	}
	if strings.Contains(clientResp, "tool_result") {
		t.Fatalf("Downstream client leaked tool_result: %s", clientResp)
	}
	if !strings.Contains(clientResp, "Thinking... ") {
		t.Fatalf("Downstream client missing initial text delta: %s", clientResp)
	}
	if !strings.Contains(clientResp, "Here is the retrieved answer.") {
		t.Fatalf("Downstream client missing second text delta: %s", clientResp)
	}

	// Verify monotonic index for content_block_start on second text block
	// First text block had index 0, second text block should have index 1
	if !strings.Contains(clientResp, "{\"content_block\":{\"text\":\"\",\"type\":\"text\"},\"index\":1,\"type\":\"content_block_start\"}") {
		t.Fatalf("Downstream client did not receive re-indexed content_block_start index 1: %s", clientResp)
	}

	// Verify single message_stop
	stopCount := strings.Count(clientResp, "event: message_stop")
	if stopCount != 1 {
		t.Fatalf("Expected exactly 1 message_stop event, got %d", stopCount)
	}

	// Verify usage accumulation (15 + 25 = 40 output_tokens)
	if !strings.Contains(clientResp, "\"output_tokens\":40") {
		t.Fatalf("Expected accumulated output_tokens 40 in final message_delta: %s", clientResp)
	}
}

func TestCCRProxyStream_MultiHydration(t *testing.T) {
	var callCount int32

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		switch curr {
		case 1:
			// Turn 1: headroom_retrieve for chunk_1
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_m1\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":50,\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_1\\\"}\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")

		case 2:
			// Turn 2: headroom_retrieve for chunk_2
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_m2\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":150,\"output_tokens\":12}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_2\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_2\\\"}\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":12}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")

		default:
			// Turn 3: Final answer
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_m3\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":250,\"output_tokens\":20}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Both chunks hydrated successfully.\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		}
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Fetch chunk 1 and 2"},
		},
	}

	var recordedRetrievals int
	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			return "content of " + chunkID, false
		},
		RecordHeadroom: func(count int) {
			recordedRetrievals = count
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	if atomic.LoadInt32(&callCount) != 3 {
		t.Fatalf("Expected 3 calls for multi-hydration, got %d", callCount)
	}

	if recordedRetrievals != 2 {
		t.Fatalf("Expected 2 total recorded retrievals, got %d", recordedRetrievals)
	}

	clientResp := rec.Body.String()
	if strings.Contains(clientResp, "headroom_retrieve") {
		t.Fatalf("Leaked headroom_retrieve: %s", clientResp)
	}
	if !strings.Contains(clientResp, "Both chunks hydrated successfully.") {
		t.Fatalf("Missing final answer: %s", clientResp)
	}
	if !strings.Contains(clientResp, "\"output_tokens\":42") { // 10 + 12 + 20 = 42
		t.Fatalf("Expected output_tokens 42, got: %s", clientResp)
	}
}

func TestCCRProxyStream_MaxIterations(t *testing.T) {
	var callCount int32

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Model keeps calling headroom_retrieve every turn
		fmt.Fprintf(w, "event: message_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_loop\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":50,\"output_tokens\":5}}}\n\n")

		fmt.Fprintf(w, "event: content_block_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_loop\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")

		fmt.Fprintf(w, "event: content_block_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_x\\\"}\"}}\n\n")

		fmt.Fprintf(w, "event: content_block_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

		fmt.Fprintf(w, "event: message_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")

		fmt.Fprintf(w, "event: message_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Fetch chunk"},
		},
	}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			return "data", false
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
		MaxHydrations: 3,
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	// Max hydrations 3 means iter 0, 1, 2, 3 = 4 calls total
	if atomic.LoadInt32(&callCount) != 4 {
		t.Fatalf("Expected 4 calls before giving up at max hydrations, got %d", callCount)
	}

	clientResp := rec.Body.String()
	stopCount := strings.Count(clientResp, "event: message_stop")
	if stopCount != 1 {
		t.Fatalf("Expected exactly 1 message_stop event, got %d", stopCount)
	}
}

func TestCCRProxyStream_DisabledCCR(t *testing.T) {
	var callCount int32

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		fmt.Fprintf(w, "event: message_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"input_tokens\":50,\"output_tokens\":5}}}\n\n")

		fmt.Fprintf(w, "event: content_block_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

		fmt.Fprintf(w, "event: content_block_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"CCR is disabled.\"}}\n\n")

		fmt.Fprintf(w, "event: content_block_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

		fmt.Fprintf(w, "event: message_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n")

		fmt.Fprintf(w, "event: message_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{"model": "test-model"}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return false },
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	if atomic.LoadInt32(&callCount) != 1 {
		t.Fatalf("Expected 1 call when CCR disabled, got %d", callCount)
	}
}

func TestCCRProxyJSON_SingleHydration(t *testing.T) {
	var callCount int32

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			resp := map[string]any{
				"id":    "msg_unary_1",
				"type":  "message",
				"role":  "assistant",
				"model": "test-model",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_u1",
						"name":  "headroom_retrieve",
						"input": map[string]any{"chunk_id": "chunk_json_1"},
					},
				},
				"stop_reason": "tool_use",
				"usage": map[string]any{
					"input_tokens":  100,
					"output_tokens": 12,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		} else {
			resp := map[string]any{
				"id":    "msg_unary_2",
				"type":  "message",
				"role":  "assistant",
				"model": "test-model",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Answer from hydrated chunk JSON",
					},
				},
				"stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":  200,
					"output_tokens": 18,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Retrieve JSON chunk"},
		},
	}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			if chunkID == "chunk_json_1" {
				return "Hydrated chunk JSON payload", false
			}
			return "Not found", true
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicJSONWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicJSONWithCCR failed: %v", err)
	}

	if atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("Expected 2 calls, got %d", callCount)
	}

	var finalResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &finalResp); err != nil {
		t.Fatalf("Failed to unmarshal final response: %v", err)
	}

	content, ok := finalResp["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("Expected 1 content block in final response, got %v", content)
	}
	firstBlock, _ := content[0].(map[string]any)
	if firstBlock["text"] != "Answer from hydrated chunk JSON" {
		t.Fatalf("Expected final text in content, got: %v", firstBlock["text"])
	}

	usage, ok := finalResp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("Missing usage in final response")
	}
	if usage["output_tokens"].(float64) != 30 { // 12 + 18
		t.Fatalf("Expected output_tokens 30, got %v", usage["output_tokens"])
	}
}

func TestCCRProxy_UpstreamError(t *testing.T) {
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`))
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{"model": "test-model"}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Expected status 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid api key") {
		t.Fatalf("Expected error body forwarded, got: %s", rec.Body.String())
	}
}

func TestCCRProxyStream_MonotonicIndicesWithLeadingText(t *testing.T) {
	var callCount int32
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Turn 1: leading text block (index 0) + headroom_retrieve (index 1)
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Leading reasoning... \"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_chunk1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_100\\\"}\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":15}}\n\n")
			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		} else {
			// Turn 2: final text block (upstream index 0)
			body, _ := io.ReadAll(r.Body)
			var reqMap map[string]any
			_ = json.Unmarshal(body, &reqMap)
			msgs, _ := reqMap["messages"].([]any)
			if len(msgs) != 3 {
				t.Errorf("Expected 3 messages in turn 2, got %d", len(msgs))
			}
			// Verify assistant message contains leading text block
			asstMsg, _ := msgs[1].(map[string]any)
			asstContent, _ := asstMsg["content"].([]any)
			if len(asstContent) != 2 {
				t.Errorf("Expected 2 content blocks in assistant turn, got %d", len(asstContent))
			} else {
				block0, _ := asstContent[0].(map[string]any)
				if text, _ := block0["text"].(string); text != "Leading reasoning... " {
					t.Errorf("Expected assistant block 0 text 'Leading reasoning... ', got %q", text)
				}
			}

			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Answer based on chunk 100.\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		}
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Question"},
		},
	}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			return "Hydrated chunk 100 data", false
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	clientResp := rec.Body.String()
	// Check that block 0 has index 0
	if !strings.Contains(clientResp, "{\"content_block\":{\"text\":\"\",\"type\":\"text\"},\"index\":0,\"type\":\"content_block_start\"}") {
		t.Fatalf("Missing content_block_start index 0 in client response: %s", clientResp)
	}
	// Check that block 1 has index 1
	if !strings.Contains(clientResp, "{\"content_block\":{\"text\":\"\",\"type\":\"text\"},\"index\":1,\"type\":\"content_block_start\"}") {
		t.Fatalf("Missing content_block_start index 1 in client response: %s", clientResp)
	}
	if !strings.Contains(clientResp, "Leading reasoning... ") {
		t.Fatalf("Missing leading reasoning text in client response: %s", clientResp)
	}
	if !strings.Contains(clientResp, "Answer based on chunk 100.") {
		t.Fatalf("Missing second text in client response: %s", clientResp)
	}
	if strings.Contains(clientResp, "headroom_retrieve") {
		t.Fatalf("Client received leaked headroom_retrieve: %s", clientResp)
	}
}

func TestCCRProxyStream_PreservesNonEndTurnStopReason(t *testing.T) {
	var callCount int32
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Turn 1: tool_use headroom_retrieve
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_chunk1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_200\\\"}\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")
			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		} else {
			// Turn 2: regular tool_use (e.g. bash or read) with stop_reason: "tool_use"
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_client_tool\",\"name\":\"bash\",\"input\":{}}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"ls\\\"}\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":20}}\n\n")
			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		}
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Question"},
		},
	}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			return "Chunk 200 data", false
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	clientResp := rec.Body.String()
	if !strings.Contains(clientResp, "\"stop_reason\":\"tool_use\"") {
		t.Fatalf("Final message_delta did not preserve stop_reason: tool_use: %s", clientResp)
	}
	if !strings.Contains(clientResp, "bash") {
		t.Fatalf("Client response missing bash tool call: %s", clientResp)
	}
}

func TestCCRProxyStream_MidStreamUpstreamError(t *testing.T) {
	var callCount int32
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		if curr == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_chunk1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_300\\\"}\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")
			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"internal server error"}}`))
		}
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Question"},
		},
	}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			return "Chunk 300 data", false
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR returned error on mid-stream failure: %v", err)
	}

	clientResp := rec.Body.String()
	if !strings.Contains(clientResp, "event: error") {
		t.Fatalf("Expected client to receive SSE error event on mid-stream failure, got: %s", clientResp)
	}
	if !strings.Contains(clientResp, "internal server error") {
		t.Fatalf("Expected error message in SSE event, got: %s", clientResp)
	}
}

// A thinking block streamed before a headroom_retrieve call must be replayed
// upstream with its full thinking text and signature. The hydration loop
// rebuilds the assistant message from the accumulated stream state; dropping
// thinking_delta/signature_delta yields {"type":"thinking","thinking":""},
// which upstream rejects with 400 "each thinking block must contain thinking".
func TestCCRProxyStream_HydrationReplaysThinkingBlock(t *testing.T) {
	var callCount int32
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Turn 1: thinking block (index 0) + headroom_retrieve (index 1)
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"output_tokens\":10}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"I need the chunk first.\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-abc-123\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_ret1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")
			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"chunk_777\\\"}\"}}\n\n")
			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":15}}\n\n")
			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
			return
		}

		// Turn 2: verify replayed assistant message carries the thinking block.
		body, _ := io.ReadAll(r.Body)
		var reqMap map[string]any
		if err := json.Unmarshal(body, &reqMap); err != nil {
			t.Errorf("turn 2 request not valid JSON: %v", err)
		}
		msgs, _ := reqMap["messages"].([]any)
		if len(msgs) != 3 {
			t.Errorf("Expected 3 messages in turn 2, got %d", len(msgs))
		} else {
			asstMsg, _ := msgs[1].(map[string]any)
			asstContent, _ := asstMsg["content"].([]any)
			if len(asstContent) != 2 {
				t.Errorf("Expected 2 content blocks in assistant turn, got %d", len(asstContent))
			} else {
				block0, _ := asstContent[0].(map[string]any)
				if bType, _ := block0["type"].(string); bType != "thinking" {
					t.Errorf("Expected assistant block 0 type thinking, got %v", block0["type"])
				}
				if text, _ := block0["thinking"].(string); text != "I need the chunk first." {
					t.Errorf("Expected thinking text 'I need the chunk first.', got %q", text)
				}
				if sig, _ := block0["signature"].(string); sig != "sig-abc-123" {
					t.Errorf("Expected signature 'sig-abc-123', got %q", sig)
				}
			}
		}

		fmt.Fprintf(w, "event: message_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"role\":\"assistant\",\"model\":\"test-model\",\"usage\":{\"output_tokens\":10}}}\n\n")
		fmt.Fprintf(w, "event: content_block_start\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Answer with chunk 777.\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprintf(w, "event: message_delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
		fmt.Fprintf(w, "event: message_stop\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	})

	server := httptest.NewServer(upstreamHandler)
	defer server.Close()

	rec := httptest.NewRecorder()
	reqMap := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Question"},
		},
	}

	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		GetChunk: func(chunkID string) (string, bool) {
			return "Chunk 777 data", false
		},
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return http.DefaultClient.Do(req)
		},
	}

	err := ProxyAnthropicStreamWithCCR(context.Background(), rec, reqMap, opts)
	if err != nil {
		t.Fatalf("ProxyAnthropicStreamWithCCR failed: %v", err)
	}

	clientResp := rec.Body.String()
	if !strings.Contains(clientResp, "I need the chunk first.") {
		t.Fatalf("Client missing thinking_delta forward: %s", clientResp)
	}
}
