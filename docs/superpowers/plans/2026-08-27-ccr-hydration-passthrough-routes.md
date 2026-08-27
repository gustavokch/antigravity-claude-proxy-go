# CCR Hydration for Kimi and Claude Code Routes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement CCR hydration for Kimi, Claude Code gateway, and Custom Endpoint routes to prevent leaking `headroom_retrieve` tool calls to clients.

**Architecture:** Shared proxy helper `proxyAnthropicStreamWithCCR` and `proxyAnthropicJSONWithCCR` in `internal/api/ccr_proxy.go`. Kimi, Claude Code, and Custom Endpoint route handlers delegate round-trips to this helper when CCR is enabled.

**Tech Stack:** Go (1.24+), standard net/http, bufio, json.

**Spec:** Described in Section 1 and Section 2 below.

## Global Constraints
- Target fork: gustavokch/antigravity-claude-proxy-go
- No new external dependencies.
- Zero client leakage of `headroom_retrieve` tool_use / tool_result events.
- Streaming flushes incrementally; no full-stream buffering on happy path.

---

## 1. Specification (Behavior Contract)

### 1.1 Goal
Enable Contextual Compression Retrieval (CCR) hydration on Kimi, Claude Code gateway, and Custom Endpoint routes. The proxy must intercept upstream `headroom_retrieve` tool calls, hydrate chunk data from the local CCR store, append conversation turns, and re-issue upstream requests without leaking CCR artifacts to downstream clients.

### 1.2 Invariants and Client Invisibility
1. **Tool Invisibility**: Downstream clients must never receive `tool_use` events or JSON blocks for `headroom_retrieve`.
2. **Result Invisibility**: Downstream clients must never receive `tool_result` events or messages for `headroom_retrieve`.
3. **Index Monotonicity**: Downstream content block indices (`index`) must be continuous and start from 0. Block indices for suppressed `headroom_retrieve` blocks must not cause gaps in client stream indices.
4. **Single Terminal Delivery**: Downstream clients must receive exactly one `message_stop` event (streaming) or one final message JSON (non-streaming) per client request.
5. **Usage Patching**: Final `message_delta` usage numbers must reflect total output and cache read tokens accumulated across all hydration iterations.

### 1.3 Hydration Loop Semantics
1. **Maximum Iterations**: `maxCCRHydrations = 3`. If the upstream model emits `headroom_retrieve` on iteration 3, the proxy must not hydrate again. It must finalize the response.
2. **Loop Flow**:
   - Parse upstream response (SSE events in streaming, JSON object in non-streaming).
   - Detect `headroom_retrieve` tool calls.
   - If found and `iter < maxCCRHydrations` and CCR enabled:
     - For each `headroom_retrieve` call: retrieve payload from `server.getCCRChunkPayload(chunkID)`.
     - Construct `assistant` message containing upstream tool calls.
     - Construct `user` message containing `tool_result` blocks (`is_error: true` on store miss).
     - Append both messages to `anthropicRequest["messages"]`.
     - Re-issue request to upstream via route-specific round-trip executor.
   - If not found or `iter >= maxCCRHydrations`:
     - Flush pending terminal events (`message_delta`, `message_stop`) with patched usage.

### 1.4 Streaming (`stream: true`) Event Protocol
- **`message_start`**: Forward to client only on `iter == 0`. Suppress on `iter > 0`.
- **`content_block_start`**:
  - If `content_block.name == "headroom_retrieve"`: mark block index as suppressed. Do not forward to client.
  - Else: rewrite `index = baseClientBlockIndex + idx`. Forward to client. Increment client block count.
- **`content_block_delta`**:
  - If block index is suppressed: buffer `partial_json` into memory for input extraction. Do not forward to client.
  - Else: rewrite `index = baseClientBlockIndex + idx`. Forward to client.
- **`content_block_stop`**:
  - If block index is suppressed: discard event. Do not forward to client.
  - Else: rewrite `index = baseClientBlockIndex + idx`. Forward to client.
- **`message_delta` and `message_stop`**:
  - Hold in memory (`pendingTerminalEvents`).
  - If hydration triggers: discard held terminal events.
  - If response finishes without hydration: patch cumulative token usage in `message_delta` and write all held terminal events to client.

### 1.5 Non-Streaming (`stream: false`) Protocol
1. Read full JSON body from upstream.
2. Inspect `content` array for `type == "tool_use"` and `name == "headroom_retrieve"`.
3. If detected and `iter < maxCCRHydrations`:
   - Hydrate chunks, append assistant and user messages to `anthropicRequest`.
   - Re-issue round-trip.
4. On terminal response:
   - Filter out any remaining `headroom_retrieve` blocks if max iterations were reached.
   - Aggregate usage numbers across iterations.
   - Write status code, forwarded headers, and JSON body to client.

### 1.6 Error Paths
- **Store Miss / Evicted Chunk**: `server.getCCRChunkPayload(chunkID)` returns `payload = "Error: Chunk <id> not found or evicted from CCR store"`, `is_error = true`. Send tool result with `is_error: true` back to model in next iteration. Do not fail client HTTP stream.
- **Upstream Network Error During Hydration**:
  - If stream headers not yet sent to client: write standard Anthropic API error JSON (`502 Bad Gateway`, `api_error`).
  - If stream headers already sent to client: emit SSE `error` event (`{"type":"error","error":{"type":"api_error","message":"..."}}`) and close connection.
- **Upstream Rate Limit (429/529) Mid-Hydration**:
  - Route round-trip handler applies route-specific retry/failover (e.g. Claude Code pool switches account; Kimi returns 429).

### 1.7 Custom Endpoints Decision
- **Decision**: Enable full CCR hydration on Custom Endpoints.
- **Rationale**: Custom endpoints forward Anthropic-format `/v1/messages`. CCR tool injection already applies to them. Adding hydration to `forwardToCustomEndpoint` prevents client tool errors on custom endpoints with zero added complexity via the shared helper.

---

## 2. Architecture & Design

### 2.1 Component Structure

```
                  ┌──────────────────────────────────────────────┐
                  │          handleMessages (server.go)          │
                  │  - Runs Headroom pipeline                    │
                  │  - Injects headroom_retrieve tool definition │
                  └──────────────────────┬───────────────────────┘
                                         │
        ┌────────────────────────────────┼──────────────────────────────┐
        ▼                                ▼                              ▼
┌───────────────┐              ┌───────────────────┐          ┌───────────────────┐
│ forwardToKimi │              │forwardToClaudeCode│          │forwardToCustomEnd │
└───────┬───────┘              └─────────┬─────────┘          └─────────┬─────────┘
        │                                │                              │
        │ doRoundTrip (Kimi Auth/URL)    │ doRoundTrip (Pool/Affinity)  │ doRoundTrip (Custom)
        ▼                                ▼                              ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           internal/api/ccr_proxy.go                             │
│                                                                                 │
│  - proxyAnthropicStreamWithCCR(w, r, reqMap, isCCREnabled, doRoundTrip)         │
│  - proxyAnthropicJSONWithCCR(w, r, reqMap, isCCREnabled, doRoundTrip)           │
│                                                                                 │
│  Capabilities:                                                                  │
│    * parseSSEStream event loop                                                  │
│    * Selective event suppression (headroom_retrieve blocks never reach client)  │
│    * Monotonic clientBlockIndex renumbering                                     │
│    * Terminal event buffering & usage aggregation                               │
│    * Assistant tool_use + User tool_result message reconstruction               │
│    * Re-issuing requests via doRoundTrip closure                                │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 2.2 Shared Interception Helper (`internal/api/ccr_proxy.go`)

Define round-trip closure type:
```go
type anthropicRoundTripFunc func(ctx context.Context, body []byte) (*http.Response, error)
```

Helper signatures:
```go
func (server *Server) proxyAnthropicStreamWithCCR(
    w http.ResponseWriter,
    r *http.Request,
    anthropicReq map[string]any,
    doRoundTrip anthropicRoundTripFunc,
) error

func (server *Server) proxyAnthropicJSONWithCCR(
    w http.ResponseWriter,
    r *http.Request,
    anthropicReq map[string]any,
    doRoundTrip anthropicRoundTripFunc,
) error
```

### 2.3 Route Integrations

#### A. Kimi Route (`internal/api/server.go` & `internal/kimi/passthrough.go`)
- In `internal/api/server.go:forwardToKimi`:
  - If CCR disabled (`!server.isCCREnabled()`): maintain fast-path forwarding (`kimi.ForwardMessages`).
  - If CCR enabled: construct `doRoundTrip` closure making HTTP POST to Kimi `/v1/messages` with Kimi headers (`Authorization: Bearer <key>`, stripped `x-api-key`, preserved `anthropic-version`/`anthropic-beta`). Call `proxyAnthropicStreamWithCCR` or `proxyAnthropicJSONWithCCR`.

#### B. Claude Code Route (`internal/api/claudecode_proxy.go`)
- In `internal/api/claudecode_proxy.go:forwardToClaudeCode`:
  - If CCR disabled (`!server.isCCREnabled()`): preserve current pool forwarding logic.
  - If CCR enabled:
    - Session affinity: keep selected `account` pinned across hydration iterations within the same request.
    - 429/529 retry: wrap `doRoundTrip` with account failover loop.
    - Observability & Metrics: compute latency and metrics only after hydration finishes. Do not record multiple success events per single client request.

#### C. Custom Endpoint Route (`internal/api/server.go:forwardToCustomEndpoint`)
- If CCR enabled: construct `doRoundTrip` closure targeting endpoint URL and call `proxyAnthropicStreamWithCCR` / `proxyAnthropicJSONWithCCR`.

---

## 3. Tasks

### Task 1: Create Shared CCR Proxy Helper (`internal/api/ccr_proxy.go`)

**Files:**
- Create: `internal/api/ccr_proxy.go`
- Test: `internal/api/ccr_proxy_test.go`

**Interfaces:**
- Consumes: `server.isCCREnabled()`, `server.getCCRChunkPayload(chunkID)` from `internal/api/server.go`.
- Produces:
  - `(server *Server) proxyAnthropicStreamWithCCR(w http.ResponseWriter, r *http.Request, anthropicReq map[string]any, doRoundTrip anthropicRoundTripFunc) error`
  - `(server *Server) proxyAnthropicJSONWithCCR(w http.ResponseWriter, r *http.Request, anthropicReq map[string]any, doRoundTrip anthropicRoundTripFunc) error`

- [ ] **Step 1: Write unit tests for shared CCR proxy helper in `internal/api/ccr_proxy_test.go`**

```go
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
	"testing"

	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/headroom"
)

func newTestServerWithCCR() (*Server, *headroom.Pipeline) {
	cfg := config.DefaultConfig()
	cfg.Headroom.Enabled = true
	cfg.Headroom.CCR.Enabled = true
	pipe := headroom.NewPipeline(cfg.Headroom)
	s := &Server{
		config:   cfg,
		headroom: pipe,
	}
	return s, pipe
}

func TestProxyAnthropicStreamWithCCR_Hydration(t *testing.T) {
	server, pipe := newTestServerWithCCR()
	pipe.CCRStore().Put("chunk_test123", "Hydrated full text content from chunk 123")

	roundTripCount := 0
	doRoundTrip := func(ctx context.Context, body []byte) (*http.Response, error) {
		roundTripCount++
		var respSSE string
		if roundTripCount == 1 {
			// First turn: model calls headroom_retrieve
			respSSE = strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"k3-256k","usage":{"input_tokens":50,"output_tokens":10}}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"headroom_retrieve","input":{}}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"chunk_id\":\"chunk_test123\"}"}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n")
		} else {
			// Second turn: model answers with actual text after hydration
			respSSE = strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"k3-256k","usage":{"input_tokens":80,"output_tokens":25}}}`,
				``,
				`event: content_block_start`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer is 42."}}`,
				``,
				`event: content_block_stop`,
				`data: {"type":"content_block_stop","index":0}`,
				``,
				`event: message_delta`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n")
		}

		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(respSSE)),
		}
		return resp, nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	anthropicReq := map[string]any{
		"model":    "k3-256k",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   true,
	}

	err := server.proxyAnthropicStreamWithCCR(rec, req, anthropicReq, doRoundTrip)
	if err != nil {
		t.Fatalf("proxyAnthropicStreamWithCCR failed: %v", err)
	}

	if roundTripCount != 2 {
		t.Errorf("expected 2 round trips, got %d", roundTripCount)
	}

	out := rec.Body.String()
	if strings.Contains(out, "headroom_retrieve") {
		t.Errorf("downstream output leaked headroom_retrieve: %s", out)
	}
	if strings.Contains(out, "toolu_1") {
		t.Errorf("downstream output leaked tool_use id: %s", out)
	}
	if !strings.Contains(out, "The answer is 42.") {
		t.Errorf("downstream output missing final text: %s", out)
	}
	// Verify continuous block indexing starting at index 0
	if !strings.Contains(out, `"index":0`) {
		t.Errorf("downstream output missing index 0 block: %s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api -run TestProxyAnthropicStreamWithCCR_Hydration -v`
Expected: FAIL (types / methods undefined in `internal/api`).

- [ ] **Step 3: Implement `internal/api/ccr_proxy.go`**

Implement `proxyAnthropicStreamWithCCR` and `proxyAnthropicJSONWithCCR` with complete event parsing, `headroom_retrieve` detection, index renumbering, chunk payload hydration, and iteration tracking.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api -run TestProxyAnthropicStreamWithCCR -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/ccr_proxy.go internal/api/ccr_proxy_test.go
git commit -m "feat(ccr): implement shared Anthropic CCR stream and JSON proxy helpers"
```

---

### Task 2: Integrate CCR Hydration into Kimi Route

**Files:**
- Modify: `internal/api/server.go:forwardToKimi`
- Test: `internal/api/kimi_proxy_test.go`

**Interfaces:**
- Consumes: `server.proxyAnthropicStreamWithCCR`, `server.proxyAnthropicJSONWithCCR`.
- Produces: Transparent Kimi CCR hydration on `/v1/messages`.

- [ ] **Step 1: Write failing test in `internal/api/kimi_proxy_test.go`**

Add `TestKimiForward_CCRHydration` with fake upstream Kimi HTTP server emitting `headroom_retrieve` SSE stream, asserting the proxy resolves it internally and never leaks `headroom_retrieve` to the downstream recorder.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api -run TestKimiForward_CCRHydration -v`
Expected: FAIL (leaks `headroom_retrieve` or fails with no tool available).

- [ ] **Step 3: Update `internal/api/server.go:forwardToKimi`**

When `server.isCCREnabled()` is true:
Construct `doRoundTrip` closure with Kimi HTTP target, Bearer token, and Anthropic protocol headers.
Dispatch via `server.proxyAnthropicStreamWithCCR` or `server.proxyAnthropicJSONWithCCR`.
When `server.isCCREnabled()` is false: retain existing `kimi.ForwardMessages` fast path.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api -run TestKimi -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/server.go internal/api/kimi_proxy_test.go
git commit -m "feat(kimi): enable CCR hydration on Kimi passthrough route"
```

---

### Task 3: Integrate CCR Hydration into Claude Code Gateway Route

**Files:**
- Modify: `internal/api/claudecode_proxy.go:forwardToClaudeCode`
- Test: `internal/api/claudecode_proxy_test.go`

**Interfaces:**
- Consumes: `server.proxyAnthropicStreamWithCCR`, `server.proxyAnthropicJSONWithCCR`, `claudecode.AccountPool`.
- Produces: Claude Code gateway CCR hydration with session affinity and 429 failover.

- [ ] **Step 1: Write failing test in `internal/api/claudecode_proxy_test.go`**

Add `TestClaudeCodeForward_CCRHydration` testing fake Claude Code upstream returning `headroom_retrieve`, ensuring sticky account reuse across hydration turns and successful text delivery to client.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api -run TestClaudeCodeForward_CCRHydration -v`
Expected: FAIL.

- [ ] **Step 3: Update `internal/api/claudecode_proxy.go`**

Refactor `forwardToClaudeCode` to use `proxyAnthropicStreamWithCCR` / `proxyAnthropicJSONWithCCR` when `server.isCCREnabled()` is true. Bind account selection to `doRoundTrip` with sticky session affinity and rate limit failover. Record metrics and pool success only once on request termination.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api -run TestClaudeCode -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/claudecode_proxy.go internal/api/claudecode_proxy_test.go
git commit -m "feat(claudecode): enable CCR hydration on Claude Code gateway route"
```

---

### Task 4: Integrate CCR Hydration into Custom Endpoints & Full Verification

**Files:**
- Modify: `internal/api/server.go:forwardToCustomEndpoint`
- Test: `internal/api/server_test.go`

- [ ] **Step 1: Write tests for Custom Endpoints with CCR**

Add test in `internal/api/server_test.go` verifying custom endpoint routes hydrate `headroom_retrieve` calls seamlessly.

- [ ] **Step 2: Update `forwardToCustomEndpoint` in `internal/api/server.go`**

Wire `forwardToCustomEndpoint` to use `proxyAnthropicStreamWithCCR` / `proxyAnthropicJSONWithCCR` when CCR is active.

- [ ] **Step 3: Run full repo test suite and verify no regressions**

Run: `go test -v -race ./...`
Expected: ALL PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "feat(endpoints): enable CCR hydration on custom endpoints and verify full suite"
```
