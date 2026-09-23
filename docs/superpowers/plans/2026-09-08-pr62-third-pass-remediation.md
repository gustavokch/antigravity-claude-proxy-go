# PR #62 Third Pass Remediation Plan

## Goal
Remediate third-pass review findings on PR #62:
1. Default `input_schema` to `{"type":"object","properties":{}}` when a client tool omits `parameters` (OpenAI makes it optional; Anthropic requires it — omission currently 400s the whole request upstream).
2. Assert `provider.require_parameters` on the upstream body for the `response_format` synthetic-tool path.
3. Add an end-to-end SSE test: `response_format` request through the HTTP handler unwraps the synthetic tool into content deltas.

Accepted nits (no change): `structuredOutputEmulation` evaluated twice in `chatCompletions` — purity documented at the function; backend-agnostic emulation is a design note for the author, not a code change in this pass.

## Architecture
- `internal/api/openai_request.go`: OpenAI Chat Completions → Anthropic Messages request translation, `response_format` emulation, schema stripping.
- `internal/api/openai_proxy.go` + `internal/api/openai_response.go`: response translation, synthetic-tool unwrap (unary + SSE).
- Tests: `internal/api/openai_structured_test.go`.

## Tech Stack
- Go (1.23+), `net/http/httptest`, `encoding/json`, `testing`.

---

## Task 1: Default `input_schema` When Client Tool Omits `parameters`

### Target Files
- Modify: `internal/api/openai_request.go`
- Test: `internal/api/openai_structured_test.go`

### Step 1: Write Failing Test
Add `TestTranslateOpenAIRequest_ToolWithoutParameters`: translate a request whose only tool is
`{"type":"function","function":{"name":"ping"}}`. Assert the translated tool carries
`input_schema == {"type":"object","properties":{}}`.

### Step 2: Confirm Failure
```bash
go test -v -run TestTranslateOpenAIRequest_ToolWithoutParameters ./internal/api
```

### Step 3: Minimal Implementation
In `internal/api/openai_request.go` (~L92): when `parameters` is absent or nil, set
`anthropicTool["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}`;
otherwise keep the existing `stripUnenforcedSchemaKeywords(parameters)`.

### Step 4: Confirm Pass
```bash
go test -v -run TestTranslateOpenAIRequest_ToolWithoutParameters ./internal/api
```

### Step 5: Git Commit
```bash
git add internal/api/openai_request.go internal/api/openai_structured_test.go
git commit -m "fix(openai): default input_schema when client tool omits parameters"
```

---

## Task 2: Assert `require_parameters` on the `response_format` Path

### Target Files
- Test: `internal/api/openai_structured_test.go`

### Step 1: Write Failing Test
Extend `TestOpenAIChatCompletions_StructuredOutputUnary`: in addition to the upstream
`tool_choice` assertion, assert `receivedBody["provider"]["require_parameters"] == true`.
(The mock upstream body is already captured in `receivedBody`; `mockOR` needs the
`/v1/models` + `/endpoints` fixtures only if the capability filter requires catalog data —
the existing test passes without them, so extend assertions in place.)

### Step 2: Confirm Failure
```bash
go test -v -run TestOpenAIChatCompletions_StructuredOutputUnary ./internal/api
```

### Step 3: Minimal Implementation
None expected — `normalizeToolChoice("tool")` already maps the synthetic forced choice to
`ToolChoiceFunction`, so `ForcesTool()` injects `require_parameters`. If the assertion fails,
fix `ToolRequirementsFromAnthropic` until green.

### Step 4: Confirm Pass
```bash
go test -v -run TestOpenAIChatCompletions_StructuredOutputUnary ./internal/api
```

### Step 5: Git Commit
```bash
git add internal/api/openai_structured_test.go
git commit -m "test(openai): assert require_parameters on response_format path"
```

---

## Task 3: End-to-End SSE Test for `response_format`

### Target Files
- Test: `internal/api/openai_structured_test.go`

### Step 1: Write Failing Test
Add `TestOpenAIChatCompletions_StructuredOutputStreaming`: mock OpenRouter returns an
Anthropic SSE stream (`message_start`, `content_block_start` tool_use naming the synthetic
tool, `input_json_delta` fragments, `message_delta` stop_reason tool_use, `message_stop`),
mirroring the SSE fixtures in `internal/api/openrouter_observability_test.go`. Client sends
`stream: true` + `response_format json_schema`. Assert:
- response Content-Type is `text/event-stream`;
- concatenated `choices[0].delta.content` parses as JSON with the expected field;
- no chunk emits `tool_calls`;
- final `finish_reason` is `"stop"`;
- a `data: [DONE]` sentinel terminates the stream.

### Step 2: Confirm Failure
```bash
go test -v -run TestOpenAIChatCompletions_StructuredOutputStreaming ./internal/api
```

### Step 3: Minimal Implementation
None expected — this is a coverage gap, not a behavior gap. Fix only if the test exposes one.

### Step 4: Confirm Pass
```bash
go test -v -run TestOpenAIChatCompletions_StructuredOutputStreaming ./internal/api
```

### Step 5: Git Commit
```bash
git add internal/api/openai_structured_test.go
git commit -m "test(openai): end-to-end streaming coverage for response_format unwrap"
```

---

## Verification
```bash
go vet ./... && go test ./...
```
Gate: full suite green before push. Then `git push origin fix/openrouter-structured-output`.
