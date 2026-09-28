# PR #94 review remediation, round 2: Zen chat-wire correctness

**Branch:** `feat/zen-gateway` (head `ffda5d6`)
**Base:** `main`
**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/94
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/94#issuecomment-5808781277
**Prior round:** `2026-09-24-pr94-review-remediation.md`, fully landed (`d591a37`..`33d7885`).

## Goal

Round 2 found seven items in the Chat-Completions-wire path. Throwaway tests
against `ffda5d6` reproduced findings 1–4 before the review was posted:

| # | Sev | Finding | Repro observed |
|---|-----|---------|----------------|
| 1 | 🔴 | CCR streaming chat-wire drops input/cache usage | `OnUsage(in=0, out=7, cr=0)` for prompt 1000 / cached 400 |
| 2 | 🔴 | JSON `length` + tool_calls reported as `tool_use` with `input:{}` | `stop_reason=tool_use input=map[]` |
| 3 | 🟡 | Non-JSON 200 body becomes an error envelope with status 200 | `status=200 body={"type":"error",…}` |
| 4 | 🟡 | Clean EOF without `[DONE]`/`finish_reason` finalized as `end_turn` | `stop_reason:"end_turn"`, no error event |
| 5 | 🟡 | Chat-wire CCR branch of `forwardToZen` untested | n/a (coverage gap) |
| 6 | 🔵 | Images inside `tool_result` silently dropped | n/a |
| 7 | 🔵 | `AnthropicToChatRequest` exported with one caller and an always-nil error | n/a |

Each task below closes one item test-first. Tasks 5 and 7 are exceptions:
Task 5 is a characterization test and Task 7 is a pure refactor.

Each task's test code was dry-run against `ffda5d6` before this plan was
written, and each Step 2 result below is the observed outcome. Tasks 1–4 and
6 fail as predicted. The Task 1 double-count guard and the Task 5 test pass.
The Task 5 test fails when the CCR branch is bypassed.

Out of scope, same as round 1: the empty thinking `signature` needs a live
Zen smoke before anyone changes it.

## Architecture

- `internal/zen/chatwire.go`: `SendChat` → `AnthropicToChatRequest` (request),
  `translateChatResponse` → `ChatResponseToAnthropic` (JSON) /
  `streamChatToAnthropic` + `chatStream` (SSE). Chat-wire usage is only known
  at stream end, so `message_start` carries zero usage and `message_delta`
  carries the real totals.
- `internal/api/ccr_proxy.go`: `ProxyAnthropicStreamWithCCR` is the CCR
  hydration loop shared by every gateway. It reads input/cache usage from
  `message_start` only. That assumption breaks for translated upstreams
  (finding 1).
- `internal/api/server.go`: `forwardToZen` chat-wire branch (L1424-1451)
  dispatches to `zen.ForwardChat` (non-CCR) or the CCR loop with a
  `zen.SendChat` sender.

## Tech stack

Go 1.27rc2 stdlib only. Tests: `go test ./internal/zen/ ./internal/api/`.
No TLS changes: the Zen client path uses `http.DefaultClient` and is unaffected
by the agy fingerprint rule.

## Spec reference

- PR #94 body: chat-wire translation must give callers an Anthropic-shaped
  JSON, SSE, or error envelope.
- Anthropic Messages streaming: `message_delta.usage` may carry cumulative
  input/cache counts. `stop_reason` values are `end_turn | max_tokens |
  stop_sequence | tool_use | refusal`.
- OpenAI Chat Completions streaming: a well-formed stream ends with a
  `finish_reason` chunk and/or `data: [DONE]`.

---

## Task 1: CCR streaming must take usage from `message_delta` when `message_start` has none (🔴)

**Target files**
- Modify: `internal/api/ccr_proxy.go` (`ProxyAnthropicStreamWithCCR`, `message_start` case L140-157, `message_delta` case L203-209)
- Test: `internal/api/ccr_proxy_test.go`

**Consumes:** `zen.SendChat` (translated SSE: `message_start` usage = 0, `message_delta` usage = real).
**Produces:** `CCRProxyOptions.OnUsage` receives the real input/cache totals for translated upstreams. Accounting for upstreams that already report in `message_start` does not change.

### Step 1: failing test (+ double-count guard)

Add `"antigravity-go-proxy/internal/zen"` to the imports of `ccr_proxy_test.go`.

```go
// Translated upstreams (Zen Chat wire) learn prompt usage only at stream end:
// message_start reports zero and message_delta carries the real figures.
func TestCCRProxyStream_UsageFromMessageDelta(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
			`{"id":"c1","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":400}}}`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	var in, out, cacheRead int
	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			return zen.SendChat(ctx, upstream.Client(), upstream.URL, "k", body)
		},
		OnUsage: func(i, o, cr, _ int) { in, out, cacheRead = i, o, cr },
	}
	reqMap := map[string]any{"model": "glm-5.3", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "q"}}}
	if err := ProxyAnthropicStreamWithCCR(context.Background(), httptest.NewRecorder(), reqMap, opts); err != nil {
		t.Fatal(err)
	}
	if in != 600 || out != 7 || cacheRead != 400 {
		t.Fatalf("OnUsage in=%d out=%d cacheRead=%d, want 600/7/400", in, out, cacheRead)
	}
}

// Upstreams that report usage in both message_start and message_delta (the
// Anthropic API does, cumulatively) must not be double-counted.
func TestCCRProxyStream_UsageNotDoubleCounted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":50,\"cache_read_input_tokens\":30,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":50,\"cache_read_input_tokens\":30,\"output_tokens\":5}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	var in, cacheRead int
	opts := CCRProxyOptions{
		IsCCREnabled: func() bool { return true },
		Sender: func(ctx context.Context, body []byte) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			return http.DefaultClient.Do(req)
		},
		OnUsage: func(i, _, cr, _ int) { in, cacheRead = i, cr },
	}
	reqMap := map[string]any{"model": "m", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "q"}}}
	if err := ProxyAnthropicStreamWithCCR(context.Background(), httptest.NewRecorder(), reqMap, opts); err != nil {
		t.Fatal(err)
	}
	if in != 50 || cacheRead != 30 {
		t.Fatalf("OnUsage in=%d cacheRead=%d, want 50/30 (no double count)", in, cacheRead)
	}
}
```

### Step 2: confirm failure

`go test ./internal/api/ -run 'TestCCRProxyStream_Usage' -v`
→ `UsageFromMessageDelta` FAILS with `in=0 out=7 cacheRead=0`. `UsageNotDoubleCounted` passes (it guards the fix).

### Step 3: implement

Inside the `for iter` loop, before the scanner loop, declare the usage already
counted for this iteration:

```go
		// Usage already accounted for this iteration. Translated upstreams
		// (Zen Chat wire) report zero in message_start and the real figures in
		// message_delta; fall back to message_delta only for fields
		// message_start left at zero, so upstreams reporting in both events
		// are not double-counted.
		var iterInput, iterCacheRead, iterCacheCreation int
```

`message_start` case: after each existing accumulation, also record the
iteration value (`iterInput = int(inTok)`, `iterCacheRead = int(readTok)`,
`iterCacheCreation = int(crTok)`). The `iter == 0` gate on
`totalInputTokens` stays as it is.

`message_delta` case, after the `output_tokens` accumulation:

```go
					if inTok, ok := usage["input_tokens"].(float64); ok && inTok > 0 && iterInput == 0 {
						if iter == 0 {
							totalInputTokens = int(inTok)
						}
						iterInput = int(inTok)
					}
					if readTok, ok := usage["cache_read_input_tokens"].(float64); ok && readTok > 0 && iterCacheRead == 0 {
						totalCacheReadTokens += int(readTok)
						iterCacheRead = int(readTok)
					}
					if crTok, ok := usage["cache_creation_input_tokens"].(float64); ok && crTok > 0 && iterCacheCreation == 0 {
						totalCacheCreationTokens += int(crTok)
						iterCacheCreation = int(crTok)
					}
```

### Step 4: confirm pass

Same command → both PASS. Then `go test ./internal/api/ -run 'CCR' -v` → all existing CCR tests still pass (their `message_delta` events carry only `output_tokens`).

### Step 5: commit

`git add internal/api/ccr_proxy.go internal/api/ccr_proxy_test.go && git commit -m "fix(ccr): read usage from message_delta when message_start has none"`

---

## Task 2: JSON `length` + tool_calls must stay `max_tokens` (🔴)

**Target files**
- Modify: `internal/zen/chatwire.go` (`ChatResponseToAnthropic`, L512-516)
- Test: `internal/zen/chatwire_test.go`

**Consumes:** Chat JSON response `choices[0].finish_reason`, `message.tool_calls`.
**Produces:** `stop_reason` matches the SSE path (`chatStream.finish` L743 only promotes `end_turn`).

### Step 1: failing test

```go
// "length" with a tool call means the arguments were truncated: the client
// must see max_tokens, not a tool_use it would execute with {} input. Some
// backends report "stop" on tool-call turns; that must still promote.
func TestChatResponseToolCallStopReason(t *testing.T) {
	resp := func(finish string) map[string]any {
		return ChatResponseToAnthropic(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"tool_calls": []any{map[string]any{
				"id": "x", "function": map[string]any{"name": "Write", "arguments": `{"path":"/tmp/a","content":"trunc`},
			}}},
			"finish_reason": finish,
		}}}, "m")
	}
	if got := resp("length")["stop_reason"]; got != "max_tokens" {
		t.Errorf("length + tool_calls: stop_reason = %v, want max_tokens", got)
	}
	if got := resp("stop")["stop_reason"]; got != "tool_use" {
		t.Errorf("stop + tool_calls: stop_reason = %v, want tool_use", got)
	}
}
```

### Step 2: confirm failure

`go test ./internal/zen/ -run TestChatResponseToolCallStopReason -v` → FAIL: `length + tool_calls: stop_reason = tool_use`.

### Step 3: implement

```go
		fr, _ := choice["finish_reason"].(string)
		stop = mapFinishReason(fr)
		// Some backends report "stop" on tool-call turns; promote only a
		// normal end. "length" means the arguments were truncated.
		if len(calls) > 0 && stop == "end_turn" {
			stop = "tool_use"
		}
```

### Step 4: confirm pass

Same command → PASS. `go test ./internal/zen/` green (`TestSendChatJSONAndError` still sees `tool_use` for `finish_reason:"tool_calls"`).

### Step 5: commit

`git add internal/zen/chatwire.go internal/zen/chatwire_test.go && git commit -m "fix(zen): keep max_tokens when a tool call is truncated"`

---

## Task 3: unreadable / non-JSON 200 must surface as 502 (🟡)

**Target files**
- Modify: `internal/zen/chatwire.go` (`translateChatResponse` L370-378, `anthropicError` L392, `chatErrorToAnthropic` L427)
- Test: `internal/zen/chatwire_test.go`

**Consumes:** upstream 200 with a body that is not a Chat Completions JSON document (CDN/HTML interstitial, truncated read).
**Produces:** a response with status 502 and an Anthropic `api_error` envelope. `ForwardChat` then writes 502, and the CCR loop takes its `StatusCode != 200` error branch. Neither path parses the error as a message or tracks it as a successful request.

### Step 1: failing test

```go
func TestSendChatNonJSONSuccessIs502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>gateway</html>")
	}))
	defer srv.Close()

	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k", []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), `"type":"api_error"`) {
		t.Fatalf("got %d %s, want 502 api_error", resp.StatusCode, b)
	}
}
```

### Step 2: confirm failure

`go test ./internal/zen/ -run TestSendChatNonJSONSuccessIs502 -v` → FAIL: `got 200 {"error":{…"api_error"},"type":"error"}`.

### Step 3: implement

Drop the unused `status` parameter: `func anthropicError(kind, msg string) []byte`. Update the `chatErrorToAnthropic` call to `anthropicError(kind, "Zen: "+msg)`. Add:

```go
// failResponse rewrites resp into a 502 carrying an Anthropic api_error
// envelope, so callers branch on the status instead of parsing an error body
// as a successful message.
func failResponse(resp *http.Response, msg string) *http.Response {
	resp.StatusCode = http.StatusBadGateway
	resp.Status = strconv.Itoa(http.StatusBadGateway) + " " + http.StatusText(http.StatusBadGateway)
	return rebody(resp, "application/json", anthropicError("api_error", msg))
}
```

Replace L373 and L377 with `return failResponse(resp, "Zen response read error: "+err.Error())` and `return failResponse(resp, "Zen returned non-JSON chat response")`.

### Step 4: confirm pass

Same command → PASS; `go test ./internal/zen/` green.

### Step 5: commit

`git add internal/zen/chatwire.go internal/zen/chatwire_test.go && git commit -m "fix(zen): return 502 for unreadable chat responses"`

---

## Task 4: truncated chat stream must error, not `end_turn` (🟡)

**Target files**
- Modify: `internal/zen/chatwire.go` (`streamChatToAnthropic`, L554-583)
- Test: `internal/zen/chatwire_test.go`

**Consumes:** upstream Chat SSE ending in clean EOF.
**Produces:** an Anthropic `error` event when neither `[DONE]` nor any `finish_reason` was seen. Streams with either marker still finish normally.

### Step 1: failing test

```go
// A stream is complete only if it carried [DONE] or a finish_reason; a bare
// clean EOF is a dropped connection and must not pass as end_turn.
func TestStreamTerminationRequiresMarker(t *testing.T) {
	content := "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"The answer is\"}}]}\n\n"
	cases := []struct {
		name, in  string
		wantError bool
	}{
		{"clean EOF, no marker", content, true},
		{"finish_reason without [DONE]", content + "data: {\"id\":\"c\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", false},
		{"[DONE] without finish_reason", content + "data: [DONE]\n\n", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if err := streamChatToAnthropic(strings.NewReader(c.in), &out, "m"); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		s := out.String()
		gotError := strings.Contains(s, "event: error")
		gotStop := strings.Contains(s, "event: message_stop")
		if gotError != c.wantError || gotStop == c.wantError {
			t.Errorf("%s: error=%v message_stop=%v, want error=%v\n%s", c.name, gotError, gotStop, c.wantError, s)
		}
	}
}
```

### Step 2: confirm failure

`go test ./internal/zen/ -run TestStreamTerminationRequiresMarker -v` → FAIL on `clean EOF, no marker` (`error=false message_stop=true`). The other two cases pass and guard against overcorrecting.

### Step 3: implement

Track `done := false` before the scan loop and set it on `[DONE]` before `break`. After the existing `scanner.Err()` check, before `return s.finish()`:

```go
	// Clean EOF with neither [DONE] nor a finish_reason is a truncated
	// stream (dropped connection, proxy timeout); finishing it as end_turn
	// would pass a partial answer off as complete.
	if !done && s.stop == "" {
		s.emitError("api_error", "Zen stream ended before completion")
		return nil
	}
```

### Step 4: confirm pass

Same command → PASS; `go test ./internal/zen/` green (existing stream tests all end with `[DONE]`).

### Step 5: commit

`git add internal/zen/chatwire.go internal/zen/chatwire_test.go && git commit -m "fix(zen): error on chat streams that end without a terminator"`

---

## Task 5: handler-level test for the chat-wire CCR branch (🟡)

**Target files**
- Test: `internal/api/zen_proxy_test.go` (add imports `sync/atomic`, `antigravity-go-proxy/internal/headroom`, `antigravity-go-proxy/internal/headroom/stages/ccr`)

**Consumes:** `forwardToZen` chat-wire CCR branch (`server.go` L1436-1450), CCR hydration loop, and `zen.SendChat` request/response translation.
**Produces:** a regression pin proving that a `headroom_retrieve` call from a Chat-wire model is hydrated through the translator (`tool_result` → `role:"tool"`) and that the client sees clean Anthropic SSE. Removing the CCR branch would fail this test, which the round-1 test cannot do: the non-CCR path never hydrates.

### Step 1: test (characterization: expected green on arrival)

```go
// With CCR enabled, a Chat-wire model's headroom_retrieve call must be
// hydrated through the translator (tool_result → role:"tool") and the client
// must receive Anthropic SSE with the retrieve call suppressed.
func TestServer_ForwardToZen_ChatWireCCRHydrates(t *testing.T) {
	store := ccr.NewCCRStore(1024 * 1024)
	chunkID, ok := store.Put("secret chunk payload")
	if !ok {
		t.Fatal("store.Put rejected chunk")
	}
	engine := headroom.NewEngine(headroom.Config{Enabled: true, CCR: headroom.CCRConfig{Enabled: true}}, nil, ccr.NewStage(store))
	server := &Server{headroom: engine, ccrStore: store}

	var calls int32
	var second map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		n := atomic.AddInt32(&calls, 1)
		if n == 2 {
			_ = json.NewDecoder(r.Body).Decode(&second)
		}
		var chunk any
		if n == 1 {
			chunk = map[string]any{"id": "c1", "choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_r",
					"function": map[string]any{"name": "headroom_retrieve", "arguments": `{"chunk_id":"` + chunkID + `"}`},
				}}},
				"finish_reason": "tool_calls",
			}}}
		} else {
			chunk = map[string]any{"id": "c2", "choices": []any{map[string]any{
				"delta": map[string]any{"content": "answer"}, "finish_reason": "stop",
			}}}
		}
		b, _ := json.Marshal(chunk)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	body := []byte(`{"model":"glm-5.3","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"q"}],"tools":[{"name":"headroom_retrieve","input_schema":{"type":"object"}}]}`)
	var reqMap map[string]any
	_ = json.Unmarshal(body, &reqMap)
	rec := httptest.NewRecorder()
	server.forwardToZen(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		config.ZenConfig{Enabled: true, APIKey: "sk-zen-test", BaseURL: upstream.URL},
		body, reqMap, "glm-5.3", config.ZenModelConfig{ID: "glm-5.3", Enabled: true})

	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (retrieve + hydrated follow-up)", n)
	}
	msgs, _ := second["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "call_r" || last["content"] != "secret chunk payload" {
		t.Fatalf("hydrated follow-up tail = %v, want role:tool call_r with chunk payload", last)
	}
	out := rec.Body.String()
	if strings.Contains(out, "headroom_retrieve") {
		t.Fatalf("client stream leaked headroom_retrieve:\n%s", out)
	}
	for _, want := range []string{`"text":"answer"`, `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("client stream missing %s\n%s", want, out)
		}
	}
}
```

### Step 2: confirm it pins the branch

`go test ./internal/api/ -run TestServer_ForwardToZen_ChatWireCCRHydrates -v` → PASS.
Sanity (do not commit): temporarily change the chat-wire guard at `server.go` L1427 to `if true {`. The request then takes `zen.ForwardChat`, which makes one upstream call and leaks `headroom_retrieve`, so the test must FAIL. Revert.

### Step 3: commit

`git add internal/api/zen_proxy_test.go && git commit -m "test(api): cover chat-wire CCR hydration in forwardToZen"`

---

## Task 6: leave a placeholder for images inside `tool_result` (🔵)

**Target files**
- Modify: `internal/zen/chatwire.go` (`toolResultText`, L243)
- Test: `internal/zen/chatwire_test.go`

**Consumes:** Anthropic `tool_result.content` blocks (e.g. Claude Code `Read` on a PNG).
**Produces:** a text-only `role:"tool"` message that says an image was omitted instead of an empty result.

### Step 1: failing test

```go
func TestToolResultImageLeavesPlaceholder(t *testing.T) {
	out := userToChat([]any{map[string]any{
		"type": "tool_result", "tool_use_id": "t1",
		"content": []any{map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "AAAA",
		}}},
	}})
	tool := out[0].(map[string]any)
	if tool["role"] != "tool" || !strings.Contains(tool["content"].(string), "image omitted") {
		t.Fatalf("tool message = %v, want an image-omitted placeholder", tool)
	}
}
```

### Step 2: confirm failure

`go test ./internal/zen/ -run TestToolResultImageLeavesPlaceholder -v` → FAIL (content `""`).

### Step 3: implement

In the `[]any` branch of `toolResultText`, switch on block type:

```go
		for _, b := range c {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				t, _ := block["text"].(string)
				parts = append(parts, t)
			case "image":
				// Chat tool messages are text-only; tell the model something
				// was there rather than returning an empty result.
				parts = append(parts, "[image omitted: tool results are text-only on this model]")
			}
		}
```

### Step 4: confirm pass

Same command → PASS; `go test ./internal/zen/` green.

### Step 5: commit

`git add internal/zen/chatwire.go internal/zen/chatwire_test.go && git commit -m "fix(zen): mark images omitted from chat-wire tool results"`

---

## Task 7: unexport `AnthropicToChatRequest`, drop the dead error (🔵)

**Target files**
- Modify: `internal/zen/chatwire.go` (L29, L99-101, L146)

**Consumes / Produces:** no behavior change. `git grep` finds exactly one
caller (`SendChat`, L29) and no references outside `.go` files.

### Step 1: implement

Rename to `anthropicToChatRequest(req map[string]any) map[string]any`, update its doc
comment, and change `return out, nil` to `return out`. In `SendChat`, replace
the two-value call and its `if err != nil` block with
`chatReq := anthropicToChatRequest(req)`.

### Step 2: verify

`go build ./... && go vet ./internal/zen/ && go test ./internal/zen/` green.

### Step 3: commit

`git add internal/zen/chatwire.go && git commit -m "refactor(zen): unexport chat request translator"`

---

## Verification gate

1. `gofmt -l internal/` → empty.
2. `go build ./... && go vet ./... && go test ./...` → 100% green.
3. `git push fork feat/zen-gateway`. The PR head lives on remote `fork`; there
   is no `origin` remote.
4. Reply on the review comment and map each finding to its commit.
