# PR #108 Remediation Plan

**Goal:** Resolve the findings from the PR #108 review (`feat/zen-systemone-responses`) without changing the Responses-wire design.

**Architecture:** All fixes stay inside the existing Responses translator (`internal/zen/responseswire.go`, `internal/zen/responsesstream.go`) and its dispatch (`internal/api/server.go`). No new wire, no TLS change, no new public API.

**Tech Stack:** Go 1.27, stdlib `testing`, `httptest`.

**Spec reference:** review comment on PR #108 (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/108#issuecomment-5918823314); OpenAI Responses schema (`ReasoningItem.required = [id, summary, type]`; `FunctionTool.strict` default `true`).

**Gate:** `go test -count=1 ./...` green, `gofmt -l` clean.

**Not changed (design calls, noted in the review):** `POST /v1/systemone` does not consult `zen.allowlist`; no `store` field on the Responses body.

---

## Task 1: Drop replayed thinking blocks

**Finding:** `internal/zen/responseswire.go:L219` — 🔴 a `thinking` block replays as a `reasoning` item with no `id` (required by the Responses schema). This translator always emits `signature: ""`, so there is no id or `encrypted_content` to give back.

**Files:** Modify `internal/zen/responseswire.go` (`assistantToResponses`); Test `internal/zen/responseswire_test.go`.

**Consumes:** Anthropic assistant content blocks. **Produces:** Responses input items `message` / `function_call` only; never `reasoning`.

- [ ] **Step 1: failing test**

```go
// A replayed thinking block must not become a reasoning input item: the
// Responses schema requires reasoning items to carry the id the upstream
// issued, and this translator never has one to give back.
func TestAnthropicToResponsesRequest_DropsReplayedThinking(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model": "gpt-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "pondering", "signature": ""},
				map[string]any{"type": "text", "text": "hello"},
			}},
			map[string]any{"role": "user", "content": "again"},
		},
	})
	for _, raw := range responsesInputItems(t, out) {
		item, _ := raw.(map[string]any)
		if item["type"] == "reasoning" {
			t.Fatalf("reasoning item replayed without an id: %s", mustJSON(t, item))
		}
	}
	items := responsesInputItems(t, out)
	if len(items) != 3 {
		t.Fatalf("input has %d items, want 3 (user, assistant text, user): %s", len(items), mustJSON(t, items))
	}
}
```

- [ ] **Step 2: run, expect FAIL** — `go test -count=1 ./internal/zen -run DropsReplayedThinking`
- [ ] **Step 3: implement** — remove the `reasoning` accumulator and the `case "thinking"` branch from `assistantToResponses`; update its doc comment ("thinking is dropped: …").
- [ ] **Step 4: run, expect PASS** — `go test -count=1 ./internal/zen`
- [ ] **Step 5: commit** — `git add internal/zen && git commit -m "fix(zen): drop replayed thinking blocks on the Responses wire"`

## Task 2: Non-strict tools

**Finding:** `internal/zen/responseswire.go:L107` — 🟡 `strict` omitted; Responses attempts strict mode when it is, the Chat wire did not.

**Files:** Modify `internal/zen/responseswire.go` (`chatToolToResponses`); Test `internal/zen/responseswire_test.go`.

**Produces:** every entry of `out["tools"]` carries `"strict": false`.

- [ ] **Step 1: failing test**

```go
// Responses function tools default to strict mode when `strict` is omitted,
// which normalises the schema (optional params become required). The Chat
// wire this mirrors was non-strict, so every tool — declared or injected
// for the gate — must say so explicitly.
func TestAnthropicToResponsesRequest_ToolsAreNonStrict(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model":    "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"name": "Edit", "input_schema": map[string]any{"type": "object"}},
		},
	})
	tools, _ := out["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %s, want Edit plus the injected bash and read", mustJSON(t, tools))
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if strict, ok := tool["strict"].(bool); !ok || strict {
			t.Errorf("tool %v strict = %v, want an explicit false", tool["name"], tool["strict"])
		}
	}
}
```

- [ ] **Step 2: run, expect FAIL** — `go test -count=1 ./internal/zen -run ToolsAreNonStrict`
- [ ] **Step 3: implement** — add `"strict": false` to the map `chatToolToResponses` returns.
- [ ] **Step 4: run, expect PASS** — `go test -count=1 ./internal/zen`
- [ ] **Step 5: commit** — `git add internal/zen && git commit -m "fix(zen): send strict:false on Responses tools"`

## Task 3: Stream content carried only by `output_item.done`; linear argument bookkeeping

**Findings:** `internal/zen/responsesstream.go:L503` — 🟡 streaming ignores done-only text/refusal/reasoning (the aggregator recovers them); `:L501` — 🟡 `argSent[idx] += delta` is O(n²).

**Files:** Modify `internal/zen/responsesstream.go`; Test `internal/zen/responseswire_test.go`.

**Design:** `responsesStream.sent` becomes `map[sentKey]*strings.Builder` (`sentKey{kind, idx}`, kinds `args`/`text`/`refusal`/`thinking`). Every streamed delta appends to its builder (amortised O(1)). On `output_item.done`, the completed value for the item is compared with the builder: the undelivered suffix (when the completed value extends what was sent) is emitted through the same block helpers the deltas use. A refusal part sets `stop = "refusal"`.

- [ ] **Step 1: failing tests**

```go
// The aggregator recovers text, reasoning and refusals that only the
// completed item carries; the stream must too, or a streaming client gets an
// empty turn (or, for a refusal, a clean end_turn).
func TestStreamResponsesToAnthropic_EmitsContentCarriedOnlyByDoneItem(t *testing.T) {
	for _, tc := range []struct {
		name      string
		item      string
		wantDelta string
		wantStop  string
	}{
		{"text", `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello there"}]}`, `"text":"hello there"`, "end_turn"},
		{"refusal", `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no can do"}]}`, `"text":"no can do"`, "refusal"},
		{"reasoning", `{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"weighing it"}]}`, `"thinking":"weighing it"`, "end_turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sse := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_d"}}`,
				``,
				`data: {"type":"response.output_item.done","output_index":0,"item":` + tc.item + `}`,
				``,
				`data: {"type":"response.completed","response":{"id":"resp_d","usage":{"input_tokens":3,"output_tokens":1}}}`,
				``,
			}, "\n")
			var out bytes.Buffer
			if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
				t.Fatalf("streamResponsesToAnthropic: %v", err)
			}
			got := out.String()
			if !strings.Contains(got, tc.wantDelta) {
				t.Errorf("done-only content missing %s:\n%s", tc.wantDelta, got)
			}
			if !strings.Contains(got, `"stop_reason":"`+tc.wantStop+`"`) {
				t.Errorf("stop_reason want %s:\n%s", tc.wantStop, got)
			}
		})
	}
}

// Content already streamed as deltas must not be repeated when the completed
// item restates it, and a completed item that extends the deltas contributes
// only the missing suffix: the client concatenates text_delta frames.
func TestStreamResponsesToAnthropic_DoesNotRepeatDeliveredText(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_e"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hel"}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"lo"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_e","usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	var text strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"text_delta"`) {
			continue
		}
		var ev struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		text.WriteString(ev.Delta.Text)
	}
	if text.String() != "hello world" {
		t.Errorf("client text = %q, want %q", text.String(), "hello world")
	}
}
```

- [ ] **Step 2: run, expect FAIL** — `go test -count=1 ./internal/zen -run 'DoneItem|DoesNotRepeatDeliveredText'`
- [ ] **Step 3: implement** — per the design above; existing `DoesNotRepeatDeliveredArguments` and `EmitsArgumentsFromCompletedItem` keep pinning the `args` kind.
- [ ] **Step 4: run, expect PASS** — `go test -count=1 ./internal/zen`
- [ ] **Step 5: commit** — `git add internal/zen && git commit -m "fix(zen): stream content carried only by the completed item; keep delta bookkeeping linear"`

## Task 4: Remove the dead stop-reason branch

**Finding:** `internal/zen/responsesstream.go:L565` — 🔵 `s.toolItems == 0 && stop == "tool_use"` is unreachable (`stop` only becomes `tool_use` when `toolItems > 0`).

No new test: behaviour is unchanged; `DropsInjectedToolCalls` pins the end-state (injected-only calls report `end_turn`).

- [ ] **Step 1:** delete the branch and its comment in `finish`; keep the comment's intent on the `toolItems > 0` line.
- [ ] **Step 2:** `go test -count=1 ./internal/zen` — PASS
- [ ] **Step 3:** `git add internal/zen && git commit -m "refactor(zen): drop an unreachable stop-reason branch"`

## Task 5: Share the forward/copy path between the translated wires

**Findings:** `internal/api/server.go:L1783` and `internal/zen/responseswire.go:L488` — 🔵 verbatim copies of the Chat branch / `ForwardChat`.

**Files:** Modify `internal/zen/passthrough.go` (new `forwardTranslated`), `internal/zen/chatwire.go` (`ForwardChat`), `internal/zen/responseswire.go` (`ForwardResponses`), `internal/api/server.go`.

No new test: pure refactor; `TestForwardResponses_*`, the Chat-wire suite, and the `forwardToZen` Chat/Responses routing + CCR tests pin both paths.

- [ ] **Step 1:** `forwardTranslated(w, r, wire string, send func(context.Context) (*http.Response, error), modify)` — the body of today's `ForwardChat` (log `"zen "+wire+" upstream error"`).
- [ ] **Step 2:** `ForwardChat` / `ForwardResponses` become one-line delegations.
- [ ] **Step 3:** in `forwardToZen`, one `wire == WireChat || wire == WireResponses` branch picks `forward, send` once.
- [ ] **Step 4:** `go build ./... && go test -count=1 ./internal/zen ./internal/api` — PASS
- [ ] **Step 5:** `git add internal && git commit -m "refactor(zen): share the translated-wire forward path"`

## Final verification

- [ ] `gofmt -l internal` prints nothing
- [ ] `go vet ./internal/zen ./internal/api`
- [ ] `go test -count=1 ./...`
- [ ] `git push fork feat/zen-systemone-responses`
