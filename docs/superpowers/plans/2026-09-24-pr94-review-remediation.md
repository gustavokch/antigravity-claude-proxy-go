# PR #94 review remediation — Zen chat-wire gateway hardening

**Branch:** `feat/zen-gateway`
**Base:** `main`
**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/94
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/94#issuecomment-5808576995

## Goal

PR #94 adds Anthropic↔Chat Completions translation for the Zen gateway. The
review found five fixable items: one wrong stop-reason mapping, one silently
vanishing user turn, one silently dropped stream fragment, one derived count
that will drift, and no handler-level test for the new chat-wire routing
branches. Each task below closes one, test-first.

Explicitly out of scope (documented in the review, no code change):

- Empty `signature` on translated thinking blocks — needs live-backend
  verification before changing; changing it blindly risks breaking the
  DeepSeek/GLM reasoning round-trip.
- Renaming the `zenNotAnthropicWire` translation key — cosmetic churn across
  both locales for no behavior change.

## Architecture

- `internal/zen/chatwire.go` — Anthropic→Chat request translation, Chat→Anthropic
  response/SSE translation.
- `internal/zen/chatwire_test.go` — translation tests.
- `internal/api/server.go` — `forwardToZen` wire dispatch (Anthropic passthrough
  vs. `zen.ForwardChat` / CCR sender).
- `internal/api/zen_proxy_test.go` — handler-level Zen tests (Anthropic wire only).
- `internal/api/management.go` — `handleZenModelsFetch` catalog bucketing.

## Tech stack

Go 1.27rc2 stdlib. Tests: `go test ./internal/zen/ ./internal/api/`.

---

## Task 1 — `content_filter` must map to `refusal`, not `end_turn`

**Target files**
- Modify: `internal/zen/chatwire.go` (`mapFinishReason`)
- Test: `internal/zen/chatwire_test.go`

### Why

A content-filtered Chat completion currently surfaces as a clean `end_turn`,
indistinguishable from normal termination. Anthropic defines `refusal` for
exactly this case; clients key retry/safety UX off it.

### Step 1 — failing test

```go
func TestMapFinishReasonContentFilter(t *testing.T) {
	if got := mapFinishReason("content_filter"); got != "refusal" {
		t.Fatalf("content_filter = %q, want refusal", got)
	}
}
```

### Step 2 — confirm failure

`go test ./internal/zen/ -run TestMapFinishReasonContentFilter -v` → FAIL (returns `end_turn`).

### Step 3 — implement

Add `case "content_filter": return "refusal"` to `mapFinishReason`.

### Step 4 — confirm pass

Same command → PASS; `go test ./internal/zen/` green.

### Step 5 — commit

`git add internal/zen/chatwire.go internal/zen/chatwire_test.go && git commit -m "fix(zen): map content_filter finish reason to refusal"`

---

## Task 2 — a fully-dropped user turn must not vanish

**Target files**
- Modify: `internal/zen/chatwire.go` (`userToChat`)
- Test: `internal/zen/chatwire_test.go`

### Why

A user turn whose every block is dropped (unsupported image source, empty
block list, no tool_result) currently emits zero Chat messages, collapsing
role alternation upstream — the next assistant turn is appended directly after
the previous one, which several Chat-wire backends reject with a confusing
400. Emit an empty-text user message so the turn boundary survives.

### Step 1 — failing test

```go
func TestUserToChatEmptyTurnPreserved(t *testing.T) {
	// document/file image sources have no Chat equivalent and are dropped.
	out := userToChat([]any{
		map[string]any{"type": "image", "source": map[string]any{"type": "file", "file_id": "f1"}},
	})
	if len(out) != 1 {
		t.Fatalf("dropped turn vanished: %v", out)
	}
	msg := out[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "" {
		t.Fatalf("fallback message = %v", msg)
	}
}
```

### Step 2 — confirm failure

`go test ./internal/zen/ -run TestUserToChatEmptyTurnPreserved -v` → FAIL (len 0).

### Step 3 — implement

In `userToChat`, when `out` is empty after processing, append
`map[string]any{"role": "user", "content": ""}`.

### Step 4 — confirm pass

Same command → PASS; `go test ./internal/zen/` green (existing role-order test must be unaffected).

### Step 5 — commit

`git add internal/zen/chatwire.go internal/zen/chatwire_test.go && git commit -m "fix(zen): preserve user turn when all its blocks are dropped"`

---

## Task 3 — log dropped interleaved tool-call argument fragments

**Target files**
- Modify: `internal/zen/chatwire.go` (`chatStream.handle`)
- Test: `internal/zen/chatwire_test.go`

### Why

When an upstream interleaves argument fragments for an earlier tool call after
a later block opened, the fragment is dropped with no trace — the emitted
`tool_use` input is silently truncated JSON. Keep the drop (Anthropic SSE
cannot reopen a closed block) but make it observable.

### Step 1 — failing test

```go
func TestStreamLogsDroppedInterleavedArgs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	in := "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"g\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	if err := streamChatToAnthropic(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "dropping interleaved tool_call arguments") {
		t.Fatalf("no drop logged:\n%s", buf.String())
	}
}
```

### Step 2 — confirm failure

`go test ./internal/zen/ -run TestStreamLogsDroppedInterleavedArgs -v` → FAIL (nothing logged).

### Step 3 — implement

In the `blockIdx != s.current` drop branch, `slog.Debug("zen chat stream: dropping interleaved tool_call arguments", "callIndex", callIdx, "openBlock", s.current)`.

### Step 4 — confirm pass

Same command → PASS; `go test ./internal/zen/` green.

### Step 5 — commit

`git add internal/zen/chatwire.go internal/zen/chatwire_test.go && git commit -m "fix(zen): log dropped interleaved tool_call argument fragments"`

---

## Task 4 — count `WireChat` directly in the models-fetch bucket

**Target files**
- Modify: `internal/api/management.go` (`handleZenModelsFetch`)

### Why

`"chat": len(usable) - anthropicCount` is derived arithmetic: adding a third
forwardable wire to `usable` later silently corrupts the chat count. Count in
the switch that already classifies each model.

### Step 1 — implement

Track `chatCount` alongside `anthropicCount` in the existing
`switch wire` (`case zen.WireChat: chatCount++`), report `"chat": chatCount`.

### Step 2 — verify

`go build ./... && go test ./internal/api/` green. (Handler bucketing has no
dedicated test; the arithmetic is now structurally impossible to drift.)

### Step 3 — commit

`git add internal/api/management.go && git commit -m "refactor(api): count Zen chat-wire models directly"`

---

## Task 5 — handler-level test for the chat-wire forward path

**Target files**
- Test: `internal/api/zen_proxy_test.go`

### Why

`zen.SendChat` is unit-tested, but nothing exercises the server wiring that
selects it: `matchZenModelEntry` claiming a chat-wire alias, `zenTargetModel`
canonicalizing the ID, and `forwardToZen` dispatching to `/v1/chat/completions`
with the response translated back to Anthropic shape. A regression in any of
those three is invisible to the current suite.

### Step 1 — failing test (fails pre-PR, passes now — characterization test)

```go
// Chat-wire entries must be forwarded to /v1/chat/completions with the
// response translated back into Anthropic shape.
func TestForwardToZenChatWire(t *testing.T) {
	var gotPath, gotAuth string
	var gotReq map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"yo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	server := &Server{}
	body := []byte(`{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	var reqMap map[string]any
	_ = json.Unmarshal(body, &reqMap)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	server.forwardToZen(rec, req,
		config.ZenConfig{Enabled: true, APIKey: "sk-zen-test", BaseURL: upstream.URL},
		body, reqMap, "glm-5.3",
		config.ZenModelConfig{ID: "glm-5.3", Enabled: true})

	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer sk-zen-test" {
		t.Fatalf("upstream call = %s auth=%q", gotPath, gotAuth)
	}
	if gotReq["model"] != "glm-5.3" {
		t.Fatalf("upstream request = %v", gotReq)
	}
	var msg map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if msg["type"] != "message" || msg["stop_reason"] != "end_turn" {
		t.Fatalf("response not Anthropic-shaped: %s", rec.Body.String())
	}
	content := msg["content"].([]any)
	if content[0].(map[string]any)["text"] != "yo" {
		t.Fatalf("content = %v", content)
	}
}
```

(Exact constructor details — `Server{}` fields, `config.ZenConfig` shape —
adjusted to match the existing tests in `zen_proxy_test.go`.)

### Step 2 — confirm pass on this branch, confirm it fails without the chat branch

Run `go test ./internal/api/ -run TestForwardToZenChatWire -v` → PASS. Sanity:
with the `wire == zen.WireChat` dispatch removed the test hits `/v1/messages`
and fails — proving it pins the routing, not just the translator.

### Step 3 — commit

`git add internal/api/zen_proxy_test.go && git commit -m "test(api): cover chat-wire forwardToZen routing"`

---

## Verification gate

`go build ./... && go vet ./... && go test ./...` must be 100% green, then
`git push origin feat/zen-gateway`.
