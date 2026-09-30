# Responses `store: false` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every request the proxy posts to Zen's `/v1/responses` carries an explicit `"store": false`, so OpenAI-side response retention is off, no client field can turn it back on, and the body matches what the genuine OpenCode harness sends for these models.

**Architecture:** One constant field added inside the existing request translator `anthropicToResponsesRequest` (`internal/zen/responseswire.go`). Both delivery paths (`ForwardResponses` and the CCR path through `SendResponses`) build their body with that function, so one edit covers both. No new wire, no new config, no TLS change.

**Tech Stack:** Go 1.27rc2, stdlib `testing`.

**Spec:** the open item in the PR #108 review (`Responses body has no store field`, https://github.com/gustavokch/antigravity-claude-proxy-go/pull/108#issuecomment-5918823314) and the evidence below.

## Evidence (why `false`, why unconditionally)

1. **Default retains.** OpenAI, *Migrate to the Responses API* (https://developers.openai.com/api/docs/guides/migrate-to-responses.md): "Responses are stored by default. Chat completions are stored by default for new accounts. To disable storage in either API, set `store: false`."
2. **The genuine harness already sends it for every Responses-wire id.** `packages/opencode/src/provider/transform.ts` (`options()`):
   ```ts
   // openai and providers using openai package should set store to false by default.
   if (input.model.providerID === "openai" || input.model.api.npm === "@ai-sdk/openai" || ...) {
     result["store"] = false
   }
   ```
   and the Zen docs endpoint table (https://opencode.ai/docs/zen/) lists **every** id in `zen.ResponsesWireIDs` (gpt-*, grok-*, muse-*) as `https://opencode.ai/zen/v1/responses` / `@ai-sdk/openai`. So `store: false` is part of the genuine body for exactly this wire; omitting it is a (small) deviation from the disguise, sending it removes the deviation.
3. **The translator is stateless, so nothing reads a stored copy.** `assistantToResponses` / `userToResponses` replay the whole conversation every turn; replayed items carry no `id` (only `call_id` on function calls/outputs) and the body never sets `previous_response_id`. With `store: false` there is therefore no "Item with id … not found" failure mode. Thinking blocks are already dropped on replay (`4b27bd8`), so the `include: ["reasoning.encrypted_content"]` companion that OpenCode adds for stateless reasoning replay is **not** needed.
4. **Client cannot override.** `anthropicToResponsesRequest` builds `out` from an allowlist (`model`, `input`, `max_output_tokens`, `temperature`, `top_p`, `stream`, `tools`, `tool_choice`); an Anthropic-body `store` key is never copied.

## Global Constraints

- Go toolchain 1.27rc2; module `antigravity-go-proxy`.
- **Do not touch TLS internals** (AGENTS.md). This change is body-only; the JA3/JA4 fingerprint is unaffected and needs no re-capture.
- `gofmt -l internal` must print nothing; `go vet ./internal/zen` clean; `go test -count=1 ./...` green.
- Reuse the existing test helpers in `internal/zen/responseswire_test.go` (`responsesInputItems`, `mustJSON`); no new convention.
- Work in the existing worktree `/private/var/folders/8g/72_1q1qx4wz2_2xb9xh9bw980000gp/T/opencode/wt-zen-wires` on branch `feat/zen-systemone-responses` (HEAD `98553c0`); push remote is `fork`, not `origin`.

## Not in this plan

- **Chat-Completions wire (`chatwire.go`) is deliberately left alone.** Its ids (DeepSeek, MiniMax, GLM, Kimi, …) are served through `@ai-sdk/openai-compatible`, for which the genuine harness sends **no** `store`; adding one would be a *new* deviation from the disguise, and those backends are not OpenAI, so OpenAI's retention default does not describe them. If you later want it anyway it is one line next to `out["stream_options"]` at `internal/zen/chatwire.go:164`, but it needs its own evidence.
- `include: ["reasoning.encrypted_content"]`, `promptCacheKey`, reasoning effort/summary: other fields the genuine harness sends; not part of the `store` item.
- Anthropic-wire passthrough and `/v1/systemone`: bodies are forwarded byte-for-byte; no `store` concept.

---

### Task 1: Send `store: false` on the Responses request

**Files:**
- Modify: `internal/zen/responseswire.go` (function doc comment, lines 16-34; `out["stream"] = true` at line 73)
- Modify: `README.md:560` (Responses-wire paragraph)
- Test: `internal/zen/responseswire_test.go` (append after `TestAnthropicToResponsesRequest_DropsUnsupportedFields`, which ends at line 240)

**Interfaces:**
- Consumes: `anthropicToResponsesRequest(req map[string]any) (map[string]any, map[string]string, map[string]bool)`; test helpers `responsesInputItems(t *testing.T, body map[string]any) []any` and `mustJSON(t *testing.T, v any) string`.
- Produces: the returned body map now always contains `"store": false` (Go `bool`, not a string). No signature change; callers (`SendResponses`) need no edit.

- [ ] **Step 1: Write the failing test**

Insert after line 240 of `internal/zen/responseswire_test.go` (blank line, then):

```go
// The Responses API retains every response server-side unless told otherwise,
// and the genuine OpenCode harness sends store:false for every model on this
// wire. store must be an explicit false, and because the body is built from an
// allowlist a client-supplied value must never reach the wire.
func TestAnthropicToResponsesRequest_DisablesStorage(t *testing.T) {
	cases := []struct {
		name string
		req  map[string]any
	}{
		{
			name: "field absent",
			req: map[string]any{
				"model":    "gpt-5",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			},
		},
		{
			name: "client asks for store true",
			req: map[string]any{
				"model":    "gpt-5",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"store":    true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, _ := anthropicToResponsesRequest(tc.req)
			v, present := out["store"]
			if !present {
				t.Fatalf("store missing from the upstream body (OpenAI stores by default): %s", mustJSON(t, out))
			}
			if v != false {
				t.Errorf("store = %v (%T), want the boolean false", v, v)
			}
		})
	}
}

// store:false is only safe because the replay is stateless: every turn resends
// the whole conversation, no replayed item references a stored id, and the
// body never chains on previous_response_id. An item carrying an id would make
// the upstream look it up and fail with "Item with id ... not found" once
// storage is off - an error that reads like a ban, not a bug.
func TestAnthropicToResponsesRequest_ReplayIsStateless(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model": "gpt-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "checking"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
			}},
		},
	})
	if _, ok := out["previous_response_id"]; ok {
		t.Error("previous_response_id must never be sent: the stored response it names does not exist with store:false")
	}
	for _, raw := range responsesInputItems(t, out) {
		item, _ := raw.(map[string]any)
		if _, ok := item["id"]; ok {
			t.Errorf("replayed input item carries an id the upstream never stored: %s", mustJSON(t, item))
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify the right one fails**

Run: `cd /private/var/folders/8g/72_1q1qx4wz2_2xb9xh9bw980000gp/T/opencode/wt-zen-wires && go test -count=1 ./internal/zen -run 'AnthropicToResponsesRequest_(DisablesStorage|ReplayIsStateless)' -v 2>&1 | tail -20`

Expected: `TestAnthropicToResponsesRequest_DisablesStorage/field_absent` and `/client_asks_for_store_true` both FAIL with `store missing from the upstream body`; `TestAnthropicToResponsesRequest_ReplayIsStateless` PASSES already (it pins the precondition that makes the fix safe, it does not drive the change).

- [ ] **Step 3: Write the minimal implementation**

In `internal/zen/responseswire.go`, replace the line at 73:

```go
	out["stream"] = true
```

with:

```go
	out["stream"] = true
	// store is explicit because the Responses API retains every response
	// server-side by default. The replay is stateless (whole conversation each
	// turn, no previous_response_id, no item ids), so the stored copy is never
	// read, and the genuine OpenCode harness sends store:false for every model
	// on this wire. out is an allowlist, so no client field can switch it on.
	out["store"] = false
```

Then extend the function doc comment: after line 28 (`// parameters pass through unchanged.`) insert:

```go
//
// The body always carries "store": false so the upstream does not retain the
// conversation; see the comment at the assignment for why that is safe.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd /private/var/folders/8g/72_1q1qx4wz2_2xb9xh9bw980000gp/T/opencode/wt-zen-wires && gofmt -l internal; go vet ./internal/zen && go test -count=1 ./internal/zen ./internal/api 2>&1 | tail -8`

Expected: no `gofmt` output; `ok  antigravity-go-proxy/internal/zen` and `ok  antigravity-go-proxy/internal/api`. No existing test asserts the exact key set of the Responses body, so none should need touching; if one fails, read it and fix the assertion only if it pinned the *absence* of `store`.

- [ ] **Step 5: Document it and commit**

In `README.md`, directly after the paragraph ending at line 561 (`rather than approximated; \`max_tokens\` becomes \`max_output_tokens\`.`), add a blank line and:

```markdown
Every translated Responses request carries `"store": false`: the Responses API
retains responses by default, the proxy replays the whole conversation each
turn so nothing reads the stored copy, and the genuine OpenCode client sends
the same flag for these models. A `store` field in the incoming Anthropic body
is ignored.
```

Then:

```bash
cd /private/var/folders/8g/72_1q1qx4wz2_2xb9xh9bw980000gp/T/opencode/wt-zen-wires
git add internal/zen/responseswire.go internal/zen/responseswire_test.go README.md docs/superpowers/plans/2026-09-30-responses-store-false.md
git commit -m "fix(zen): send store:false on the Responses wire"
```

---

### Task 2: Gate, push, and close the live-gateway gap

**Files:**
- No source edits. Verification only.

**Interfaces:**
- Consumes: Task 1's commit on `feat/zen-systemone-responses`.
- Produces: pushed branch; a PR comment recording what was and was not verified.

- [ ] **Step 1: Full gate**

Run: `cd /private/var/folders/8g/72_1q1qx4wz2_2xb9xh9bw980000gp/T/opencode/wt-zen-wires && gofmt -l internal; go vet ./internal/... && go test -count=1 ./... 2>&1 | grep -v 'no test files'`

Expected: no `gofmt` output; every line `ok …`; no `FAIL`. (`gofmt -l` on the whole repo also lists the pre-existing generated `gen/exa/codeium_common_pb/codeium_common.pb.go`; that file is not part of this change.)

- [ ] **Step 2: Push**

Run: `cd /private/var/folders/8g/72_1q1qx4wz2_2xb9xh9bw980000gp/T/opencode/wt-zen-wires && git push fork feat/zen-systemone-responses 2>&1 | tail -3`

Expected: `98553c0..<new-sha>  feat/zen-systemone-responses -> feat/zen-systemone-responses`.

- [ ] **Step 3: Live smoke (operator step — needs a real Zen key; not runnable in CI or without credentials)**

This is the only check that can show Zen accepts the flag. A unit test cannot: the risk is an upstream rejection such as `Unsupported parameter: 'store'`.

A. Through the proxy (proves acceptance). Start the proxy with Zen enabled and `OPENCODE_API_KEY` (or `zen.apiKey`) set, then send whatever proxy auth your instance needs:

```bash
curl -sS -m 60 -w '\nHTTP %{http_code}\n' http://127.0.0.1:8080/v1/messages \
  -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"gpt-5-nano","max_tokens":32,"messages":[{"role":"user","content":"reply with the word ok"}]}'
```

Expected: `HTTP 200` and an Anthropic `{"type":"message",…}` body with a text block. Failure signature: `HTTP 4xx` with an `api_error`/`invalid_request_error` body whose text mentions `store`. If that happens, stop: revert Task 1 and report it, because it would mean the gateway does not forward the field the genuine harness sends.

B. Direct echo (proves it is honoured). The Responses API echoes the stored flag on `response.created`:

```bash
curl -sN -m 60 https://opencode.ai/zen/v1/responses \
  -H "Authorization: Bearer $OPENCODE_API_KEY" -H 'content-type: application/json' \
  -d '{"model":"gpt-5-nano","stream":true,"store":false,"max_output_tokens":32,"input":[{"role":"user","content":[{"type":"input_text","text":"ok"}]}]}' \
  | grep -m1 '"type":"response.created"'
```

Expected: the JSON on that line contains `"store":false`. A `403 FreeTierError` body means the gate wants the harness headers; in that case rely on A only and say so in the PR comment.

- [ ] **Step 4: Record the result on the PR**

```bash
gh pr comment 108 --body "### store:false on the Responses wire (<new-sha>)

Adds \"store\": false to every translated /v1/responses body. Evidence: OpenAI docs (Responses are stored by default) and OpenCode's provider/transform.ts, which sets store=false for @ai-sdk/openai - the package the Zen docs list for every Responses-wire id. Replay is stateless (no item ids, no previous_response_id), pinned by a test. Chat wire intentionally unchanged (openai-compatible; the genuine client sends no store there).

go test -count=1 ./... passes. Live Zen smoke: <A: HTTP 200 / not run> <B: store:false echoed / not run>."
```

Fill the `<…>` with what actually happened; write "not run (no key)" rather than omitting the line.

---

## Self-review

- **Spec coverage:** explicit `store:false` on the wire → Task 1 Step 3; client cannot override → `DisablesStorage/client_asks_for_store_true`; safety precondition (stateless replay) → `ReplayIsStateless`; both delivery paths → covered by the single translator (Architecture); docs → doc comment + README (Task 1 Steps 3, 5); "can't check without a live gateway" → Task 2 Step 3 with an exact request, expected output, and failure signature, marked operator-only.
- **Placeholder scan:** the only angle-bracket fields are `<new-sha>` and the smoke results in the PR comment, which are values that do not exist until execution.
- **Type consistency:** `anthropicToResponsesRequest` signature, `responsesInputItems`, `mustJSON` match the current code; the test compares `v != false` against an `any` holding a Go `bool`, matching how `out["stream"] != true` is already asserted in `TestAnthropicToResponsesRequest_ConversationShape`.
