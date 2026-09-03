# PR #49 Remediation — OpenAI /v1/chat/completions Review Fixes

- **Goal:** Fix the three actionable findings from the PR #49 review (unbounded body read, consecutive user messages from parallel tool results, missing `stream_options.include_usage` support), plus the SSE framing dedup nit.
- **Architecture:** All changes stay inside the edge translation layer (`internal/api`): request translation (`openai_request.go`), response translation (`openai_response.go`), and the proxy wrapper (`openai_proxy.go`). The dispatch pipeline in `server.messages()` is untouched.
- **Tech Stack:** Go 1.x, stdlib `net/http`, existing table-test style in `internal/api/openai_proxy_test.go`.
- **Spec:** PR review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/49#issuecomment-5472138074
- **Branch:** `feat/openai-chat-completions`

---

## Task 1 — Cap request body size (🔴 bug)

**Target files:** Modify `internal/api/openai_proxy.go`, Test `internal/api/openai_proxy_test.go`

**Consumes:** `maxRequestBody` const (server.go:44), `http.MaxBytesReader`, `http.MaxBytesError`.
**Produces:** `/v1/chat/completions` rejects bodies over 50MB with HTTP 413 + OpenAI error envelope, without buffering them.

1. **Failing test** — append to `openai_proxy_test.go`:

```go
// TestOpenAIChatCompletions_OversizedBody caps the request body the same way
// /v1/messages does: bodies beyond maxRequestBody are rejected with 413 and an
// OpenAI error envelope instead of being buffered in full.
func TestOpenAIChatCompletions_OversizedBody(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	server, err := New(Options{APIKey: "test-proxy-key", Builder: proxyformat.NewBuilder(), Now: time.Now})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	oversized := strings.Repeat("a", maxRequestBody+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if _, ok := errBody["error"].(map[string]any); !ok {
		t.Errorf("expected OpenAI error envelope, got %v", errBody)
	}
}
```

2. **Run to confirm failure:** `go test ./internal/api/ -run TestOpenAIChatCompletions_OversizedBody` — currently returns 200-class/400 handling with full buffering, and the assert on 413 fails.
3. **Minimal implementation** in `chatCompletions` (before `io.ReadAll`):

```go
request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
```

   and in the read-error branch, distinguish the limit:

```go
var maxBytesErr *http.MaxBytesError
if errors.As(err, &maxBytesErr) {
	writeOpenAIError(writer, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large")
	return
}
```

4. **Run to confirm pass:** same command.
5. **Commit:** `git add internal/api/openai_proxy.go internal/api/openai_proxy_test.go && git commit -m "fix(api): cap /v1/chat/completions request body at 50MB"`

## Task 2 — Merge consecutive same-role messages (🔴 bug)

**Target files:** Modify `internal/api/openai_request.go`, Test `internal/api/openai_proxy_test.go`

**Consumes:** translated `anthropicMessages` slice.
**Produces:** parallel tool results collapse into one user message with N `tool_result` blocks; consecutive same-role messages never reach Anthropic-shaped upstreams.

1. **Failing test** — add two cases to the `TestTranslateOpenAIRequest` table:

```go
{
	name: "parallel tool results merge into one user message",
	input: `{"model":"m","messages":[
		{"role":"user","content":"weather and time?"},
		{"role":"assistant","tool_calls":[
			{"id":"call_1","function":{"name":"get_weather","arguments":"{}"}},
			{"id":"call_2","function":{"name":"get_time","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"},
		{"role":"tool","tool_call_id":"call_2","content":"noon"}
	]}`,
	want: `{
		"model": "m",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "weather and time?"}]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {}},
				{"type": "tool_use", "id": "call_2", "name": "get_time", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": [{"type": "text", "text": "sunny"}]},
				{"type": "tool_result", "tool_use_id": "call_2", "content": [{"type": "text", "text": "noon"}]}
			]}
		],
		"max_tokens": 4096
	}`,
},
{
	name: "consecutive same-role messages merge",
	input: `{"model":"m","messages":[
		{"role":"user","content":"a"},
		{"role":"user","content":"b"}
	]}`,
	want: `{
		"model": "m",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "a"},
			{"type": "text", "text": "b"}
		]}],
		"max_tokens": 4096
	}`,
},
```

   (Fix the `noon` vs `22C` mismatch when writing the real test — content is `"noon"` so expected block text is `"noon"`.)

2. **Run to confirm failure:** `go test ./internal/api/ -run TestTranslateOpenAIRequest` — both cases fail (currently 4 and 2 messages respectively).
3. **Implementation** in the message loop: before appending a new message, if the last kept message has the same role, append its blocks to that message's content instead:

```go
if len(anthropicMessages) > 0 {
	if last, ok := anthropicMessages[len(anthropicMessages)-1].(map[string]any); ok && stringFrom(last["role"]) == role {
		if lastBlocks, ok := last["content"].([]any); ok {
			last["content"] = append(lastBlocks, blocks...)
			continue // for user-with-blocks appends
		}
	}
}
```

   Applied for `user`, `assistant`, and `tool` (tool_result) cases uniformly after block construction.
4. **Run to confirm pass.**
5. **Commit:** `git commit -m "fix(openai): merge consecutive same-role messages for Anthropic alternation"`

## Task 3 — Support stream_options.include_usage (🟡 risk)

**Target files:** Modify `internal/api/openai_proxy.go`, `internal/api/openai_response.go`, Test `internal/api/openai_proxy_test.go`

**Consumes:** `message_start` usage.input_tokens, `message_delta` usage.output_tokens.
**Produces:** when the OpenAI request sets `stream_options.include_usage=true`, the client receives one final spec-shaped chunk `{"choices":[],"usage":{...}}` before `data: [DONE]`. Without the flag, behavior is unchanged.

1. **Failing tests:**

   a. Unit, in `openai_proxy_test.go` — stream state records usage:

```go
func TestOpenAIStreamState_UsageCapture(t *testing.T) {
	state := newOpenAIStreamState("m")
	var dataObj map[string]any
	_ = json.Unmarshal([]byte(`{"type":"message_start","message":{"id":"msg_u","usage":{"input_tokens":9}}}`), &dataObj)
	_ = state.HandleEvent("message_start", dataObj)
	_ = state.HandleEvent("message_delta", map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 7}})
	_ = state.HandleEvent("message_stop", map[string]any{"type": "message_stop"})
	if state.promptTokens != 9 || state.completionTokens != 7 {
		t.Errorf("usage = prompt %d completion %d, want 9/7", state.promptTokens, state.completionTokens)
	}
}
```

   b. Handler-level — extend `TestOpenAIChatCompletions_SSE` request payload with `"stream_options":{"include_usage":true}` and assert the last JSON chunk before `[DONE]` has `choices: []` and `usage` prompt 9 / completion 7.

2. **Run to confirm failure** (unknown fields `state.promptTokens` fail compile — write the test against the intended API, then implement).
3. **Implementation:**
   - `openAIStreamState` gains `promptTokens, completionTokens int` (captured in `message_start` / `message_delta`); replaces the dead `_ = numberToInt(...)` line.
   - `chatCompletions` reads `stream_options.include_usage` from the OpenAI body and passes it to `newOpenAIResponseWriter`.
   - `openAIResponseWriter` emits, once, right before `[DONE]` (both from `handleStreamEvent` and `finish()`): a chunk with `choices: []` and the usage object, only when requested.
4. **Run to confirm pass.**
5. **Commit:** `git commit -m "feat(openai): honor stream_options.include_usage in streaming responses"`

## Task 4 — Deduplicate SSE framing (🔵 nit)

**Target files:** Modify `internal/api/openai_proxy.go`

1. **No new test** — characterization locks in `TestOpenAIChatCompletions_SSE` / `_ToolCalls` already pin framing.
2. **Refactor:** extract `func (w *openAIResponseWriter) writeSSEFrame(payload []byte)` used by `writeSSEData`/`writeSSEChunk`/`handleStreamError` and the `[DONE]` sentinel.
3. **Run full suite:** `go test ./internal/api/`.
4. **Commit:** `git commit -m "refactor(openai): extract writeSSEFrame helper"`

## Documented, no code change

- Assistant `content:null` with no tool_calls → empty content array → upstream 400 surfaced as a translated OpenAI error envelope. Acceptable.
- `temperature` [0,2] passes through; Anthropic upstreams cap at 1.0. Backend-dependent; edge cannot know the route.

## Verification

- `go build ./...`
- `go test ./...` — 100% green
- `gofmt -l internal/api/` — empty
- Push: `git push origin feat/openai-chat-completions`
