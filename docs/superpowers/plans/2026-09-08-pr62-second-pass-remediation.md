# PR #62 Second Pass Remediation Plan

## Goal
Remediate code review findings in PR #62:
1. Fix `unwrapStructuredOutput` to use an explicit boolean `found` flag rather than empty string checking, ensuring synthetic tool calls with empty arguments are correctly unwrapped.
2. Set `s.done = true` in `openAIStreamState.HandleEvent` on `message_stop` event to ensure immediate SSE stream completion and prompt usage chunk emission.
3. Preserve existing fields in `payload["provider"]` within `forwardToOpenRouter` when injecting `order`, `allow_fallbacks`, or `require_parameters`.
4. Guard `message["content"].([]any)` type assertion in `translateAnthropicMessageToOpenAI` to prevent runtime panic on malformed or empty responses.

## Architecture
- `internal/api/openai_response.go`: Unary and streaming response translation between Anthropic Messages and OpenAI Chat Completions envelopes.
- `internal/api/server.go`: OpenRouter routing and upstream request forwarding with provider parameter injection.
- Unit and integration tests in `internal/api/openai_structured_test.go` and `internal/api/openrouter_require_parameters_test.go`.

## Tech Stack
- Go (1.23+)
- Standard libraries: `net/http`, `encoding/json`, `testing`

---

## Task 1: Fix `unwrapStructuredOutput` Sentinel to Handle Empty Arguments

### Target Files
- Modify: `internal/api/openai_response.go`
- Test: `internal/api/openai_structured_test.go`

### Step 1: Write Failing Test
Add test case `TestUnwrapStructuredOutput_EmptyArguments` in `internal/api/openai_structured_test.go`:
Assert that when `function.arguments` is `""`, `unwrapStructuredOutput` still extracts the content, removes `tool_calls`, and updates `finish_reason` to `"stop"`.

### Step 2: Confirm Failure
Run test:
```bash
go test -v -run TestUnwrapStructuredOutput_EmptyArguments ./internal/api
```

### Step 3: Minimal Implementation
In `internal/api/openai_response.go`:
Replace `arguments == ""` sentinel with `var found bool` and check `!found`.

### Step 4: Confirm Pass
Run test:
```bash
go test -v -run TestUnwrapStructuredOutput_EmptyArguments ./internal/api
```

### Step 5: Git Commit
```bash
git add internal/api/openai_response.go internal/api/openai_structured_test.go
git commit -m "fix(openai): handle empty arguments in structured output unwrapping"
```

---

## Task 2: Set `s.done = true` on `message_stop` in `openAIStreamState.HandleEvent`

### Target Files
- Modify: `internal/api/openai_response.go`
- Test: `internal/api/openai_structured_test.go`

### Step 1: Write Failing Test
Add `TestOpenAIStreamState_MessageStopSetsDone` in `internal/api/openai_structured_test.go`. Verify `state.done` becomes `true` after `message_stop`.

### Step 2: Confirm Failure
Run test:
```bash
go test -v -run TestOpenAIStreamState_MessageStopSetsDone ./internal/api
```

### Step 3: Minimal Implementation
In `internal/api/openai_response.go`:
Under `case "message_stop":`, set `s.done = true`.

### Step 4: Confirm Pass
Run test:
```bash
go test -v -run TestOpenAIStreamState_MessageStopSetsDone ./internal/api
```

### Step 5: Git Commit
```bash
git add internal/api/openai_response.go internal/api/openai_structured_test.go
git commit -m "fix(openai): set stream state done on message_stop"
```

---

## Task 3: Preserve Existing Provider Options in `forwardToOpenRouter`

### Target Files
- Modify: `internal/api/server.go`
- Test: `internal/api/openrouter_require_parameters_test.go`

### Step 1: Write Failing Test
Add `TestOpenRouterForward_PreservesExistingProviderOptions` in `internal/api/openrouter_require_parameters_test.go`.
Send request with existing `provider: {"sort": "throughput", "ignore": ["openai"]}`.
Assert forwarded body retains `sort: throughput` alongside `require_parameters: true`.

### Step 2: Confirm Failure
Run test:
```bash
go test -v -run TestOpenRouterForward_PreservesExistingProviderOptions ./internal/api
```

### Step 3: Minimal Implementation
In `internal/api/server.go`:
Initialize `providerBlock` by copying existing `payload["provider"]` map entries before adding `order`, `allow_fallbacks`, or `require_parameters`.

### Step 4: Confirm Pass
Run test:
```bash
go test -v -run TestOpenRouterForward_PreservesExistingProviderOptions ./internal/api
```

### Step 5: Git Commit
```bash
git add internal/api/server.go internal/api/openrouter_require_parameters_test.go
git commit -m "fix(openrouter): preserve existing provider configuration during parameter injection"
```

---

## Task 4: Guard `message["content"]` Type Assertion in `translateAnthropicMessageToOpenAI`

### Target Files
- Modify: `internal/api/openai_response.go`
- Test: `internal/api/openai_structured_test.go`

### Step 1: Write Failing Test
Add `TestTranslateAnthropicMessageToOpenAI_MalformedContent` in `internal/api/openai_structured_test.go` passing a message with `content: nil` and `content: "not-a-list"`. Assert no panic occurs and an empty assistant message is returned.

### Step 2: Confirm Failure
Run test:
```bash
go test -v -run TestTranslateAnthropicMessageToOpenAI_MalformedContent ./internal/api
```

### Step 3: Minimal Implementation
In `internal/api/openai_response.go`:
Change `for _, rawBlock := range message["content"].([]any)` to check `if rawBlocks, ok := message["content"].([]any); ok { for _, rawBlock := range rawBlocks { ... } }`.

### Step 4: Confirm Pass
Run test:
```bash
go test -v -run TestTranslateAnthropicMessageToOpenAI_MalformedContent ./internal/api
```

### Step 5: Git Commit
```bash
git add internal/api/openai_response.go internal/api/openai_structured_test.go
git commit -m "fix(openai): guard content type assertion in message translation"
```
