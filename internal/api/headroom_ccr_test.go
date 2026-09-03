package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
	"antigravity-go-proxy/internal/openrouter"
	"antigravity-go-proxy/internal/stats"
)

type ccrMockBackend struct {
	mu        sync.Mutex
	calls     []map[string]any
	responses []func(func(cloudcode.SSEEvent) error) (cloudcode.Response, error)
}

func (b *ccrMockBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"models": []}`)}, nil
}

func (b *ccrMockBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	b.mu.Lock()
	callIndex := len(b.calls)
	raw, _ := json.Marshal(req)
	var copyReq map[string]any
	_ = json.Unmarshal(raw, &copyReq)
	b.calls = append(b.calls, copyReq)
	var respFn func(func(cloudcode.SSEEvent) error) (cloudcode.Response, error)
	if callIndex < len(b.responses) {
		respFn = b.responses[callIndex]
	}
	b.mu.Unlock()

	if respFn != nil {
		return respFn(cb)
	}
	return cloudcode.Response{}, nil
}

func (b *ccrMockBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func (b *ccrMockBackend) getCall(i int) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[i]
}

func makeSSE(part map[string]any, finishReason string) cloudcode.SSEEvent {
	payload := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{part},
				},
				"finishReason": finishReason,
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     100,
			"candidatesTokenCount": 20,
		},
	}
	raw, _ := json.Marshal(payload)
	return cloudcode.SSEEvent{Data: raw}
}

func TestHeadroomCCR_CloudCodeUnaryHydration(t *testing.T) {
	chunkPayload := "original long documentation content stored in CCR store"
	chunkID := ccr.ChunkID(chunkPayload)

	backend := &ccrMockBackend{}

	// Call 1 response: Model requests headroom_retrieve
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		callPart := map[string]any{
			"functionCall": map[string]any{
				"name": "headroom_retrieve",
				"id":   "toolu_ccr_1",
				"args": map[string]any{
					"chunk_id": chunkID,
				},
			},
		}
		_ = cb(makeSSE(callPart, "STOP"))
		return cloudcode.Response{}, nil
	})

	// Call 2 response: Model returns final text after getting chunk
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		textPart := map[string]any{
			"text": "Based on the retrieved documentation, here is the answer.",
		}
		_ = cb(makeSSE(textPart, "STOP"))
		return cloudcode.Response{}, nil
	})

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{
			"enabled": true,
			"ccr": map[string]any{
				"enabled":    true,
				"maxStoreMB": 64,
			},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: backend, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}

	// Pre-store chunk in server's CCR store
	srv.ccrStore.Put(chunkPayload)

	reqBody := map[string]any{
		"model": "gemini-3.5-flash-low",
		"tools": []any{
			map[string]any{"name": "test_tool"},
		},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": fmt.Sprintf("[HEADROOM_CHUNK id=%q lines=10 preview=\"doc\"]", chunkID),
			},
		},
	}

	rec := postMessages(t, srv, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if backend.callCount() != 2 {
		t.Fatalf("expected 2 backend calls, got %d", backend.callCount())
	}

	// Verify call 2 messages contain assistant tool_use and synthetic tool_result
	call2 := backend.getCall(1)
	msgs, ok := call2["messages"].([]any)
	if !ok || len(msgs) < 3 {
		t.Fatalf("call 2 messages malformed: %+v", call2)
	}

	// Check tool_result in call 2
	userTurn := msgs[len(msgs)-1].(map[string]any)
	if userTurn["role"] != "user" {
		t.Errorf("expected last turn in call 2 to be user, got %v", userTurn["role"])
	}
	blocks := userTurn["content"].([]any)
	toolRes := blocks[0].(map[string]any)
	if toolRes["type"] != "tool_result" || toolRes["tool_use_id"] != "toolu_ccr_1" || toolRes["content"] != chunkPayload {
		t.Errorf("tool_result in call 2 incorrect: %+v", toolRes)
	}

	// Verify final response to client is the text from call 2
	var finalResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &finalResp); err != nil {
		t.Fatalf("decode final response: %v", err)
	}
	contentBlocks := finalResp["content"].([]any)
	lastBlock := contentBlocks[len(contentBlocks)-1].(map[string]any)
	if lastBlock["text"] != "Based on the retrieved documentation, here is the answer." {
		t.Errorf("unexpected final text: %+v", lastBlock)
	}

	// Verify tracker recorded CCRRetrieval
	if srv.tracker.GetHeadroomStats().CCRRetrievals != 1 {
		t.Errorf("expected 1 CCRRetrieval in tracker, got %d", srv.tracker.GetHeadroomStats().CCRRetrievals)
	}
}

func TestHeadroomCCR_CloudCodeStreamHydration(t *testing.T) {
	chunkPayload := "streamed chunk data from store"
	chunkID := ccr.ChunkID(chunkPayload)

	backend := &ccrMockBackend{}

	// Call 1: tool use stream
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		callPart := map[string]any{
			"functionCall": map[string]any{
				"name": "headroom_retrieve",
				"id":   "toolu_stream_1",
				"args": map[string]any{
					"chunk_id": chunkID,
				},
			},
		}
		_ = cb(makeSSE(callPart, "STOP"))
		return cloudcode.Response{}, nil
	})

	// Call 2: text stream
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		textPart := map[string]any{
			"text": "Hydrated answer in stream.",
		}
		_ = cb(makeSSE(textPart, "STOP"))
		return cloudcode.Response{}, nil
	})

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{
			"enabled": true,
			"ccr": map[string]any{
				"enabled":    true,
				"maxStoreMB": 64,
			},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: backend, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	srv.ccrStore.Put(chunkPayload)

	reqBody := map[string]any{
		"model":  "gemini-3.5-flash-low",
		"stream": true,
		"tools": []any{
			map[string]any{"name": "search"},
		},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": fmt.Sprintf("[HEADROOM_CHUNK id=%q lines=5 preview=\"p\"]", chunkID),
			},
		},
	}

	rec := postMessages(t, srv, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if backend.callCount() != 2 {
		t.Fatalf("expected 2 backend calls, got %d", backend.callCount())
	}

	// Parse SSE stream events received by client
	lines := strings.Split(rec.Body.String(), "\n")
	var messageStarts int
	var messageStops int
	var blockStarts []int

	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var ev map[string]any
			if err := json.Unmarshal([]byte(data), &ev); err == nil {
				switch ev["type"] {
				case "message_start":
					messageStarts++
				case "message_stop":
					messageStops++
				case "content_block_start":
					if idx, ok := ev["index"].(float64); ok {
						blockStarts = append(blockStarts, int(idx))
					}
				}
			}
		}
	}

	// Must have exactly 1 message_start and 1 message_stop across the entire stitched stream
	if messageStarts != 1 {
		t.Errorf("expected exactly 1 message_start event, got %d", messageStarts)
	}
	if messageStops != 1 {
		t.Errorf("expected exactly 1 message_stop event, got %d", messageStops)
	}

	// Block indices should be sequential: [0] (headroom_retrieve in turn 1 is suppressed, 0 for text in turn 2)
	if len(blockStarts) != 1 || blockStarts[0] != 0 {
		t.Errorf("expected block start indices [0], got %v", blockStarts)
	}

	if srv.tracker.GetHeadroomStats().CCRRetrievals != 1 {
		t.Errorf("expected 1 CCRRetrieval in tracker, got %d", srv.tracker.GetHeadroomStats().CCRRetrievals)
	}
}

func TestHeadroomCCR_CacheMissReturnsIsError(t *testing.T) {
	backend := &ccrMockBackend{}

	// Call 1: requests nonexistent chunk
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		callPart := map[string]any{
			"functionCall": map[string]any{
				"name": "headroom_retrieve",
				"id":   "toolu_miss_1",
				"args": map[string]any{
					"chunk_id": "chunk_deadbeef0000",
				},
			},
		}
		_ = cb(makeSSE(callPart, "STOP"))
		return cloudcode.Response{}, nil
	})

	// Call 2: acknowledges cache miss error and proceeds
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		textPart := map[string]any{
			"text": "Chunk was missing, proceeding without it.",
		}
		_ = cb(makeSSE(textPart, "STOP"))
		return cloudcode.Response{}, nil
	})

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{
			"enabled": true,
			"ccr": map[string]any{
				"enabled": true,
			},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: backend, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}

	reqBody := map[string]any{
		"model": "gemini-3.5-flash-low",
		"tools": []any{map[string]any{"name": "t"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "retrieve it"},
		},
	}

	rec := postMessages(t, srv, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on cache miss recovery, got %d: %s", rec.Code, rec.Body.String())
	}

	if backend.callCount() != 2 {
		t.Fatalf("expected 2 calls, got %d", backend.callCount())
	}

	// Verify call 2 received is_error: true
	call2 := backend.getCall(1)
	msgs := call2["messages"].([]any)
	userTurn := msgs[len(msgs)-1].(map[string]any)
	toolRes := userTurn["content"].([]any)[0].(map[string]any)
	if isErr, ok := toolRes["is_error"].(bool); !ok || !isErr {
		t.Errorf("expected tool_result is_error=true on cache miss, got %+v", toolRes)
	}
}

func TestHeadroomCCR_IterationCapStopsLoop(t *testing.T) {
	backend := &ccrMockBackend{}

	// Return tool call on every single invocation
	for i := 0; i < 10; i++ {
		backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
			callPart := map[string]any{
				"functionCall": map[string]any{
					"name": "headroom_retrieve",
					"id":   "toolu_loop",
					"args": map[string]any{"chunk_id": "chunk_loop"},
				},
			}
			_ = cb(makeSSE(callPart, "STOP"))
			return cloudcode.Response{}, nil
		})
	}

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{
			"enabled": true,
			"ccr":     map[string]any{"enabled": true},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: backend, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}

	reqBody := map[string]any{
		"model": "gemini-3.5-flash-low",
		"tools": []any{map[string]any{"name": "t"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "loop test"},
		},
	}

	rec := postMessages(t, srv, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 1 initial call + 3 max hydrations = 4 total calls
	if backend.callCount() != 4 {
		t.Errorf("expected 4 calls (1 + max 3 hydrations), got %d", backend.callCount())
	}
}

func TestHeadroomCCR_OpenRouterUnaryHydration(t *testing.T) {
	chunkPayload := "openrouter chunk payload from store"
	chunkID := ccr.ChunkID(chunkPayload)

	var callsMu sync.Mutex
	var calls []map[string]any

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{
					ID:   "anthropic/claude-3.7-sonnet",
					Name: "Claude 3.7 Sonnet",
					Pricing: &openrouter.Pricing{
						Prompt:     0.000003,
						Completion: 0.000015,
					},
				},
			}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
			return
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			return
		}

		bodyBytes, _ := io.ReadAll(r.Body)
		var reqMap map[string]any
		_ = json.Unmarshal(bodyBytes, &reqMap)

		callsMu.Lock()
		callNum := len(calls)
		calls = append(calls, reqMap)
		callsMu.Unlock()

		if callNum == 0 {
			// First call: returns headroom_retrieve
			resp := map[string]any{
				"id":   "msg_or_1",
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "tool_use",
						"id":   "tu_or_ret",
						"name": "headroom_retrieve",
						"input": map[string]any{
							"chunk_id": chunkID,
						},
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     100,
					"completion_tokens": 20,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Second call: returns final answer
		resp := map[string]any{
			"id":   "msg_or_2",
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": "OpenRouter retrieved chunk answer.",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     150,
				"completion_tokens": 30,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockOR.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{
			"enabled": true,
			"ccr":     map[string]any{"enabled": true},
		},
		"openrouter": map[string]any{
			"enabled": true,
			"baseURL": mockOR.URL,
			"apiKey":  "sk-or-test",
			"allowlist": []any{
				map[string]any{
					"id":      "anthropic/claude-3.7-sonnet",
					"enabled": true,
				},
			},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: &mockCloudCodeBackend{}, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	srv.ccrStore.Put(chunkPayload)

	reqBody := map[string]any{
		"model": "anthropic/claude-3.7-sonnet", "max_tokens": 1024,
		"tools": []any{map[string]any{"name": "t"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "or retrieve test"},
		},
	}

	rec := postMessages(t, srv, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	callsMu.Lock()
	totalCalls := len(calls)
	callsMu.Unlock()

	if totalCalls != 2 {
		t.Fatalf("expected 2 OpenRouter calls, got %d", totalCalls)
	}

	// Verify client got final response
	var finalResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &finalResp); err != nil {
		t.Fatalf("decode final response: %v", err)
	}
	contentBlocks := finalResp["content"].([]any)
	if contentBlocks[0].(map[string]any)["text"] != "OpenRouter retrieved chunk answer." {
		t.Errorf("unexpected content: %+v", contentBlocks)
	}

	if srv.tracker.GetHeadroomStats().CCRRetrievals != 1 {
		t.Errorf("expected 1 CCRRetrieval in tracker, got %d", srv.tracker.GetHeadroomStats().CCRRetrievals)
	}
}

func TestHeadroomCCR_OpenRouterStreamHydration(t *testing.T) {
	chunkPayload := "openrouter streamed chunk payload from store"
	chunkID := ccr.ChunkID(chunkPayload)

	var callsMu sync.Mutex
	var calls []map[string]any

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{
					ID:   "anthropic/claude-3.7-sonnet",
					Name: "Claude 3.7 Sonnet",
					Pricing: &openrouter.Pricing{
						Prompt:     0.000003,
						Completion: 0.000015,
					},
				},
			}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
			return
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			return
		}

		bodyBytes, _ := io.ReadAll(r.Body)
		var reqMap map[string]any
		_ = json.Unmarshal(bodyBytes, &reqMap)

		callsMu.Lock()
		callNum := len(calls)
		calls = append(calls, reqMap)
		callsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, _ := w.(http.Flusher)

		writeSSE := func(eventType string, data map[string]any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
			if flusher != nil {
				flusher.Flush()
			}
		}

		if callNum == 0 {
			// First stream call: returns headroom_retrieve
			writeSSE("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":      "msg_ors_1",
					"type":    "message",
					"role":    "assistant",
					"content": []any{},
					"usage": map[string]any{
						"input_tokens":  100,
						"output_tokens": 5,
					},
				},
			})
			writeSSE("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    "tu_ors_ret",
					"name":  "headroom_retrieve",
					"input": map[string]any{},
				},
			})
			writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": fmt.Sprintf(`{"chunk_id":%q}`, chunkID),
				},
			})
			writeSSE("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": 0,
			})
			writeSSE("message_delta", map[string]any{
				"type": "message_delta",
				"delta": map[string]any{
					"stop_reason": "tool_use",
				},
				"usage": map[string]any{
					"output_tokens": 20,
				},
			})
			writeSSE("message_stop", map[string]any{
				"type": "message_stop",
			})
			return
		}

		// Second stream call: returns final text
		writeSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      "msg_ors_2",
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"usage": map[string]any{
					"input_tokens":  150,
					"output_tokens": 5,
				},
			},
		})
		writeSSE("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "text",
				"text": "Streamed OpenRouter retrieved answer.",
			},
		})
		writeSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		})
		writeSSE("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason": "end_turn",
			},
			"usage": map[string]any{
				"output_tokens": 30,
			},
		})
		writeSSE("message_stop", map[string]any{
			"type": "message_stop",
		})
	}))
	defer mockOR.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{
			"enabled": true,
			"ccr":     map[string]any{"enabled": true},
		},
		"openrouter": map[string]any{
			"enabled": true,
			"baseURL": mockOR.URL,
			"apiKey":  "sk-or-test",
			"allowlist": []any{
				map[string]any{
					"id":      "anthropic/claude-3.7-sonnet",
					"enabled": true,
				},
			},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: &mockCloudCodeBackend{}, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	srv.ccrStore.Put(chunkPayload)

	reqBody := map[string]any{
		"model":  "anthropic/claude-3.7-sonnet", "max_tokens": 1024,
		"stream": true,
		"tools":  []any{map[string]any{"name": "t"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "or stream retrieve test"},
		},
	}

	rec := postMessages(t, srv, reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	callsMu.Lock()
	totalCalls := len(calls)
	callsMu.Unlock()

	if totalCalls != 2 {
		t.Fatalf("expected 2 OpenRouter calls, got %d", totalCalls)
	}

	// Verify client received stitched stream
	lines := strings.Split(rec.Body.String(), "\n")
	var messageStarts, messageStops int
	var blockStarts []int
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var ev map[string]any
			if err := json.Unmarshal([]byte(data), &ev); err == nil {
				switch ev["type"] {
				case "message_start":
					messageStarts++
				case "message_stop":
					messageStops++
				case "content_block_start":
					if idx, ok := ev["index"].(float64); ok {
						blockStarts = append(blockStarts, int(idx))
					}
				}
			}
		}
	}

	if messageStarts != 1 {
		t.Errorf("expected 1 message_start event, got %d", messageStarts)
	}
	if messageStops != 1 {
		t.Errorf("expected 1 message_stop event, got %d", messageStops)
	}
	// The first iteration's only block was the headroom_retrieve call, which is
	// suppressed and consumes no downstream index. The client therefore sees a
	// single block: the final text from the second iteration, at index 0.
	if len(blockStarts) != 1 || blockStarts[0] != 0 {
		t.Errorf("expected block starts [0], got %v", blockStarts)
	}
	if strings.Contains(rec.Body.String(), "headroom_retrieve") {
		t.Errorf("leak detected: retrieve call reached the client:\n%s", rec.Body.String())
	}

	if srv.tracker.GetHeadroomStats().CCRRetrievals != 1 {
		t.Errorf("expected 1 CCRRetrieval in tracker, got %d", srv.tracker.GetHeadroomStats().CCRRetrievals)
	}
}


// The OpenRouter streaming path must suppress headroom_retrieve exactly as the
// shared CCR proxy does: the retrieve call is proxy-internal, and a client that
// sees it will try to answer a tool it does not implement.
func TestHeadroomCCR_OpenRouterStream_NoRetrieveLeak(t *testing.T) {
	chunkPayload := "openrouter leak-check chunk payload"
	chunkID := ccr.ChunkID(chunkPayload)

	var callsMu sync.Mutex
	callNum := 0

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{ID: "anthropic/claude-3.7-sonnet", Name: "Claude 3.7 Sonnet",
					Pricing: &openrouter.Pricing{Prompt: 0.000003, Completion: 0.000015}},
			}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
			return
		}

		callsMu.Lock()
		n := callNum
		callNum++
		callsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeSSE := func(eventType string, data map[string]any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeSSE("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": fmt.Sprintf("msg_%d", n), "type": "message", "role": "assistant",
			"content": []any{}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 5},
		}})

		if n == 0 {
			// Block 0 text, block 1 headroom_retrieve (suppressed), block 2 Read.
			writeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""}})
			writeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "Looking..."}})
			writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})

			writeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 1,
				"content_block": map[string]any{"type": "tool_use", "id": "tu_ret", "name": "headroom_retrieve", "input": map[string]any{}}})
			writeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": fmt.Sprintf(`{"chunk_id":%q}`, chunkID)}})
			writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})

			writeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 2,
				"content_block": map[string]any{"type": "tool_use", "id": "tu_read", "name": "Read", "input": map[string]any{}}})
			writeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 2,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"file_path":"foo.go"}`}})
			writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2})

			writeSSE("message_delta", map[string]any{"type": "message_delta",
				"delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 20}})
			writeSSE("message_stop", map[string]any{"type": "message_stop"})
			return
		}

		writeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": "Done."}})
		writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeSSE("message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 30}})
		writeSSE("message_stop", map[string]any{"type": "message_stop"})
	}))
	defer mockOR.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{"enabled": true, "ccr": map[string]any{"enabled": true}},
		"openrouter": map[string]any{
			"enabled": true, "baseURL": mockOR.URL, "apiKey": "sk-or-test",
			"allowlist": []any{map[string]any{"id": "anthropic/claude-3.7-sonnet", "enabled": true}},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: &mockCloudCodeBackend{}, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	srv.ccrStore.Put(chunkPayload)

	rec := postMessages(t, srv, map[string]any{
		"model": "anthropic/claude-3.7-sonnet", "stream": true, "max_tokens": 1024,
		"tools":    []any{map[string]any{"name": "t"}},
		"messages": []any{map[string]any{"role": "user", "content": "leak check"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "headroom_retrieve") {
		t.Fatalf("leak detected: OpenRouter stream emitted headroom_retrieve:\n%s", body)
	}
	if strings.Contains(body, "tu_ret") {
		t.Fatalf("leak detected: OpenRouter stream emitted the retrieve tool_use id:\n%s", body)
	}
	if !strings.Contains(body, "tu_read") {
		t.Fatalf("expected the Read tool_use to survive:\n%s", body)
	}

	var blockStarts []int
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if ev["type"] == "content_block_start" {
			if idx, ok := ev["index"].(float64); ok {
				blockStarts = append(blockStarts, int(idx))
			}
		}
	}
	// text(0), Read(1) from iteration 1; final text(2) from iteration 2.
	want := []int{0, 1, 2}
	if len(blockStarts) != len(want) {
		t.Fatalf("block start indexes = %v; want %v", blockStarts, want)
	}
	for i := range want {
		if blockStarts[i] != want[i] {
			t.Fatalf("block start indexes = %v; want %v", blockStarts, want)
		}
	}
}

// The OpenRouter CCR streaming path must accumulate thinking_delta and
// signature_delta into the replayed assistant message, same as the shared CCR
// proxy. Dropping them replays {"type":"thinking","thinking":""} which upstream
// rejects with 400 "each thinking block must contain thinking".
func TestHeadroomCCR_OpenRouterStreamHydration_ThinkingBlock(t *testing.T) {
	chunkPayload := "openrouter thinking-replay chunk payload"
	chunkID := ccr.ChunkID(chunkPayload)

	var callsMu sync.Mutex
	var calls []map[string]any

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{ID: "anthropic/claude-3.7-sonnet", Name: "Claude 3.7 Sonnet",
					Pricing: &openrouter.Pricing{Prompt: 0.000003, Completion: 0.000015}},
			}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
			return
		}

		bodyBytes, _ := io.ReadAll(r.Body)
		var reqMap map[string]any
		_ = json.Unmarshal(bodyBytes, &reqMap)

		callsMu.Lock()
		callNum := len(calls)
		calls = append(calls, reqMap)
		callsMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeSSE := func(eventType string, data map[string]any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
			if flusher != nil {
				flusher.Flush()
			}
		}

		if callNum == 0 {
			writeSSE("message_start", map[string]any{"type": "message_start", "message": map[string]any{
				"id": "msg_think_1", "type": "message", "role": "assistant",
				"content": []any{}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 5},
			}})
			writeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "thinking", "thinking": ""}})
			writeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "thinking_delta", "thinking": "Retrieve the chunk first."}})
			writeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "signature_delta", "signature": "sig-or-456"}})
			writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})

			writeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 1,
				"content_block": map[string]any{"type": "tool_use", "id": "tu_ret", "name": "headroom_retrieve", "input": map[string]any{}}})
			writeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": fmt.Sprintf(`{"chunk_id":%q}`, chunkID)}})
			writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})

			writeSSE("message_delta", map[string]any{"type": "message_delta",
				"delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 20}})
			writeSSE("message_stop", map[string]any{"type": "message_stop"})
			return
		}

		// Verify the replayed assistant message carries the thinking block.
		msgs, _ := reqMap["messages"].([]any)
		if len(msgs) != 3 {
			t.Errorf("expected 3 messages on second call, got %d", len(msgs))
		} else {
			asstMsg, _ := msgs[1].(map[string]any)
			asstContent, _ := asstMsg["content"].([]any)
			if len(asstContent) != 2 {
				t.Errorf("expected 2 assistant blocks, got %d", len(asstContent))
			} else {
				block0, _ := asstContent[0].(map[string]any)
				if bType, _ := block0["type"].(string); bType != "thinking" {
					t.Errorf("expected assistant block 0 type thinking, got %v", block0["type"])
				}
				if text, _ := block0["thinking"].(string); text != "Retrieve the chunk first." {
					t.Errorf("expected thinking text, got %q", text)
				}
				if sig, _ := block0["signature"].(string); sig != "sig-or-456" {
					t.Errorf("expected signature sig-or-456, got %q", sig)
				}
			}
		}

		writeSSE("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_think_2", "type": "message", "role": "assistant",
			"content": []any{}, "usage": map[string]any{"input_tokens": 150, "output_tokens": 5},
		}})
		writeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": "Done."}})
		writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeSSE("message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 30}})
		writeSSE("message_stop", map[string]any{"type": "message_stop"})
	}))
	defer mockOR.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{"enabled": true, "ccr": map[string]any{"enabled": true}},
		"openrouter": map[string]any{
			"enabled": true, "baseURL": mockOR.URL, "apiKey": "sk-or-test",
			"allowlist": []any{map[string]any{"id": "anthropic/claude-3.7-sonnet", "enabled": true}},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: &mockCloudCodeBackend{}, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	srv.ccrStore.Put(chunkPayload)

	rec := postMessages(t, srv, map[string]any{
		"model": "anthropic/claude-3.7-sonnet", "stream": true, "max_tokens": 1024,
		"tools":    []any{map[string]any{"name": "t"}},
		"messages": []any{map[string]any{"role": "user", "content": "thinking replay"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	callsMu.Lock()
	totalCalls := len(calls)
	callsMu.Unlock()
	if totalCalls != 2 {
		t.Fatalf("expected 2 OpenRouter calls, got %d", totalCalls)
	}
	if !strings.Contains(rec.Body.String(), "Retrieve the chunk first.") {
		t.Errorf("client missing thinking_delta forward: %s", rec.Body.String())
	}
}

// The CloudCode streaming path must accumulate thinking_delta and
// signature_delta like every other CCR path: the hydration loop replays
// state.AssistantBlocks() upstream, and an empty thinking block there fails
// conversion/validation with "each thinking block must contain thinking".
func TestHeadroomCCR_CloudCodeStreamHydration_ThinkingBlock(t *testing.T) {
	chunkPayload := "cloudcode thinking-replay chunk payload"
	chunkID := ccr.ChunkID(chunkPayload)
	const thinkSig = "sig-cloudcode-0123456789abcdef0123456789abcdef0123456789"
	if len(thinkSig) < 50 {
		t.Fatalf("test signature must exceed MinSignatureLength")
	}

	backend := &ccrMockBackend{}

	// Call 1: thought part + headroom_retrieve tool call.
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		_ = cb(makeSSE(map[string]any{
			"thought": true, "text": "Thinking about retrieval.",
			"thoughtSignature": thinkSig,
		}, ""))
		callPart := map[string]any{
			"functionCall": map[string]any{
				"name": "headroom_retrieve",
				"id":   "toolu_cc_think",
				"args": map[string]any{"chunk_id": chunkID},
			},
		}
		_ = cb(makeSSE(callPart, "STOP"))
		return cloudcode.Response{}, nil
	})

	// Call 2: verify the replayed messages carry the thinking block, then answer.
	backend.responses = append(backend.responses, func(cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		msgs, _ := backend.getCall(1)["messages"].([]any)
		found := false
		for _, rawMsg := range msgs {
			msg, _ := rawMsg.(map[string]any)
			if role, _ := msg["role"].(string); role != "assistant" {
				continue
			}
			for _, rawBlock := range msg["content"].([]any) {
				block, _ := rawBlock.(map[string]any)
				if block["type"] != "thinking" {
					continue
				}
				if text, _ := block["thinking"].(string); text == "Thinking about retrieval." {
					if sig, _ := block["signature"].(string); sig == thinkSig {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("replayed assistant message missing thinking text and signature:\n%s", mustJSON(backend.getCall(1)))
		}
		_ = cb(makeSSE(map[string]any{"text": "Hydrated answer with thought."}, "STOP"))
		return cloudcode.Response{}, nil
	})

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	_, _ = config.Load()
	_, _ = config.Save(map[string]any{
		"headroom": map[string]any{
			"enabled": true,
			"ccr":     map[string]any{"enabled": true, "maxStoreMB": 64},
		},
	})

	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{APIKey: "test-key", Backend: backend, Tracker: tracker})
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	srv.ccrStore.Put(chunkPayload)

	rec := postMessages(t, srv, map[string]any{
		"model":  "gemini-3.5-flash-low",
		"stream": true,
		"tools":  []any{map[string]any{"name": "search"}},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": fmt.Sprintf("[HEADROOM_CHUNK id=%q lines=5 preview=\"p\"]", chunkID),
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if backend.callCount() != 2 {
		t.Fatalf("expected 2 backend calls, got %d", backend.callCount())
	}
	if !strings.Contains(rec.Body.String(), "Thinking about retrieval.") {
		t.Errorf("client missing thinking_delta forward: %s", rec.Body.String())
	}
}

func mustJSON(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}
