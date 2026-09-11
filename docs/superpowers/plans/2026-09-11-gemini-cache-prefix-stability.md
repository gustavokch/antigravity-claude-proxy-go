# Gemini Prompt-Cache Prefix Stability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Google `contents` array a pure function of the client's message history so Gemini implicit context caching stops breaking on every tool-loop turn.

**Architecture:** Two prefix mutations are removed from `internal/format`. First, the synthetic `[Tool execution completed.]` / `[Continue]` pair appended for Gemini thinking models is dropped (kept behind an env kill switch), leaving the real tool turns in place; Gemini validates thought signatures only for the current turn and the proxy already stamps the documented `skip_thought_signature_validator` bypass on unsigned calls. Second, the `thoughtSignature` for a tool call is no longer filled from the process-local, 2-hour-TTL `SignatureCache`, so the same history converts identically before and after expiry or a restart.

**Tech Stack:** Go 1.27, standard `testing` package, no new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md`

## Global Constraints

- Module path is `antigravity-go-proxy`; the package under change is `internal/format`.
- Test command for the package: `go test ./internal/format/ -count=1`.
- Full gate before any push: `go build ./... && go test ./... -count=1`.
- `internal/format` has no logging and no config dependency. Keep it that way: the only outside input added by this plan is one `os.Getenv` read in a new file.
- Env var name: `ANTIGRAVITY_GEMINI_THINKING_RECOVERY`. Truthy values: `1`, `true`, `yes`, `on` (case-insensitive, trimmed). Default off.
- Bypass sentinel value is the existing constant `GeminiSkipSignature = "skip_thought_signature_validator"` (`internal/format/model.go:13`). Never inline the string literal in non-test code.
- `MinSignatureLength = 50` (`internal/format/model.go:11`).
- Do not change how `cache_read_input_tokens` or `input_tokens` are derived from `usageMetadata`. Spec §E6 explains why: the current mapping is already correct Anthropic semantics.
- Tests in this package use `t.Parallel()`. Any test that calls `t.Setenv` must NOT call `t.Parallel()` — the two are mutually exclusive in Go.

---

### Task 1: Prefix-stability regression tests (red)

Establishes invariant V1 as executable tests before any production change. Both tests must fail at the end of this task; that failure is the evidence the bug exists.

A reference copy of this file is parked at `/tmp/cacheprefix_test.go.repro`. Prefer the code below — it is authoritative.

**Files:**
- Create: `internal/format/cacheprefix_test.go`

**Interfaces:**
- Consumes: `ConvertAnthropicToGoogle(request map[string]any, cache *SignatureCache) map[string]any`, `NewSignatureCache() *SignatureCache`, `(*SignatureCache).CacheTool(toolID, signature string)`, helpers `asSlice(any) []any`, `MinSignatureLength`.
- Produces: test helpers `buildPiStyleHistory(turns int) []any`, `convertPiTurn(t *testing.T, turns int, cache *SignatureCache) []any`, `mustJSON(t *testing.T, value any) string` — Task 2 reuses `mustJSON`.

- [ ] **Step 1: Write the failing tests**

```go
package format

import (
	"encoding/json"
	"strings"
	"testing"
)

// buildPiStyleHistory reproduces what a strict Anthropic client (the Pi coding
// agent) sends after `turns` tool-loop rounds: assistant tool_use blocks with no
// thoughtSignature and no thinking blocks, each followed by a tool_result.
func buildPiStyleHistory(turns int) []any {
	messages := []any{
		map[string]any{"role": "user", "content": "start the loop"},
	}
	for index := 0; index < turns; index++ {
		toolID := "toolu_" + string(rune('a'+index))
		messages = append(messages,
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "tool_use", "id": toolID, "name": "read",
					"input": map[string]any{"path": "file.go"},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": toolID, "content": "file body",
				}},
			},
		)
	}
	return messages
}

func convertPiTurn(t *testing.T, turns int, cache *SignatureCache) []any {
	t.Helper()
	request := map[string]any{
		"model":    "gemini-3.0-flash-high",
		"messages": buildPiStyleHistory(turns),
	}
	converted := ConvertAnthropicToGoogle(request, cache)
	return asSlice(converted["contents"])
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// TestGeminiToolLoopPrefixIsStableAcrossTurns asserts the invariant implicit
// context caching depends on: the contents array produced for turn N must be an
// exact prefix of the contents array produced for turn N+1.
func TestGeminiToolLoopPrefixIsStableAcrossTurns(t *testing.T) {
	t.Parallel()
	cache := NewSignatureCache()
	previous := convertPiTurn(t, 3, cache)
	next := convertPiTurn(t, 4, cache)

	if len(next) < len(previous) {
		t.Fatalf("turn N+1 shrank: %d < %d", len(next), len(previous))
	}
	for index := range previous {
		want := mustJSON(t, previous[index])
		got := mustJSON(t, next[index])
		if want != got {
			t.Fatalf("prefix diverged at index %d\n turn N:   %s\n turn N+1: %s", index, want, got)
		}
	}
}

// TestGeminiToolSignatureIsIndependentOfSignatureCache asserts conversion does
// not depend on process-local ephemeral state: a warm and a cold signature cache
// must produce the same historical functionCall part.
func TestGeminiToolSignatureIsIndependentOfSignatureCache(t *testing.T) {
	t.Parallel()
	signature := strings.Repeat("g", MinSignatureLength)
	warm := NewSignatureCache()
	warm.CacheTool("toolu_a", signature)

	history := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_a", "name": "read", "input": map[string]any{},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_a", "content": "body",
		}}},
	}
	convert := func(cache *SignatureCache) string {
		converted := ConvertAnthropicToGoogle(map[string]any{
			"model": "gemini-3.0-flash-high", "messages": history,
		}, cache)
		return mustJSON(t, asSlice(converted["contents"])[1])
	}
	withCache := convert(warm)
	withoutCache := convert(NewSignatureCache())
	if withCache != withoutCache {
		t.Fatalf("history part changed when the signature cache expired\n warm: %s\n cold: %s", withCache, withoutCache)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/format/ -run 'TestGeminiToolLoopPrefixIsStableAcrossTurns|TestGeminiToolSignatureIsIndependentOfSignatureCache' -count=1 -v`

Expected: both FAIL.
- `TestGeminiToolLoopPrefixIsStableAcrossTurns` fails with `prefix diverged at index 7`, turn N showing `{"parts":[{"text":"[Tool execution completed.]"}],"role":"model"}` and turn N+1 showing a `functionCall` part.
- `TestGeminiToolSignatureIsIndependentOfSignatureCache` fails with `history part changed when the signature cache expired`, warm showing `"thoughtSignature":"gggg..."` and cold showing `"thoughtSignature":"skip_thought_signature_validator"`.

- [ ] **Step 3: Commit the red tests**

```bash
git add internal/format/cacheprefix_test.go docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md
git commit -m "test(format): add failing Gemini cache prefix stability tests

Both tests fail today and encode invariant V1 from the spec: the Google
contents array must be a pure function of the client's history."
```

Committing a red test is intentional here — the next two tasks turn each one green. Do not skip the commit; it is the record that the bug was reproduced before it was fixed.

---

### Task 2: Stop appending synthetic tool-loop turns for Gemini

Turns `TestGeminiToolLoopPrefixIsStableAcrossTurns` green. The legacy behavior stays reachable through one env var, because if the Cloud Code backend rejects the bypass sentinel on a current-turn `functionCall` (spec "Open question"), every Gemini thinking tool-loop request would fail and an operator needs an instant rollback that does not require a redeploy.

**Files:**
- Create: `internal/format/toggles.go`
- Modify: `internal/format/request.go:82-88`
- Test: `internal/format/format_test.go:247-278` (rewrite `TestGeminiToolLoopWithoutThinkingGetsRecoveryTurn`)

**Interfaces:**
- Consumes: `needsThinkingRecovery(messages []any) bool`, `closeToolLoopForThinking(messages []any, family ModelFamily, cache *SignatureCache) []any`, `stripInvalidThinkingBlocks(messages []any, family ModelFamily, cache *SignatureCache) []any` — all in `internal/format/thinking.go`.
- Produces: `geminiThinkingRecoveryEnabled() bool` and the constant `geminiThinkingRecoveryEnv = "ANTIGRAVITY_GEMINI_THINKING_RECOVERY"`, both package-private in `internal/format`.

- [ ] **Step 1: Write the failing tests**

Replace the whole of `TestGeminiToolLoopWithoutThinkingGetsRecoveryTurn` (currently `internal/format/format_test.go:247-278`) with the two tests below. The shared request builder avoids repeating the fixture.

```go
func geminiToolLoopRequest() map[string]any {
	return map[string]any{
		"model": "gemini-3.5-flash-low",
		"messages": []any{
			map[string]any{"role": "user", "content": "use a tool"},
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "tool_use", "id": "tool-1", "name": "read", "input": map[string]any{},
					"thoughtSignature": strings.Repeat("s", MinSignatureLength),
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "tool-1", "content": "done",
				}},
			},
		},
	}
}

// TestGeminiToolLoopKeepsRealToolTurns pins the new default: no synthetic turns
// are appended, so the tool call and its result stay at stable indices and the
// tool call keeps the signature the client sent.
func TestGeminiToolLoopKeepsRealToolTurns(t *testing.T) {
	t.Parallel()
	converted := ConvertAnthropicToGoogle(geminiToolLoopRequest(), NewSignatureCache())
	contents := asSlice(converted["contents"])
	if len(contents) != 3 {
		t.Fatalf("contents = %#v", contents)
	}
	call := asMap(asSlice(asMap(contents[1])["parts"])[0])
	if asMap(call["functionCall"])["name"] != "read" {
		t.Fatalf("tool turn = %#v", contents[1])
	}
	if call["thoughtSignature"] != strings.Repeat("s", MinSignatureLength) {
		t.Fatalf("thoughtSignature = %#v", call["thoughtSignature"])
	}
	response := asMap(asSlice(asMap(contents[2])["parts"])[0])
	if response["functionResponse"] == nil {
		t.Fatalf("tool result turn = %#v", contents[2])
	}
}

// TestGeminiToolLoopRecoveryKillSwitchRestoresSyntheticTurns proves the rollback
// path still works. No t.Parallel: t.Setenv forbids it.
func TestGeminiToolLoopRecoveryKillSwitchRestoresSyntheticTurns(t *testing.T) {
	t.Setenv(geminiThinkingRecoveryEnv, "1")
	converted := ConvertAnthropicToGoogle(geminiToolLoopRequest(), NewSignatureCache())
	contents := asSlice(converted["contents"])
	if len(contents) != 5 {
		t.Fatalf("contents = %#v", contents)
	}
	if asMap(asSlice(asMap(contents[3])["parts"])[0])["text"] != "[Tool execution completed.]" {
		t.Fatalf("synthetic assistant = %#v", contents[3])
	}
	if asMap(asSlice(asMap(contents[4])["parts"])[0])["text"] != "[Continue]" {
		t.Fatalf("synthetic user = %#v", contents[4])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/format/ -run 'TestGeminiToolLoop' -count=1 -v`

Expected: compile error `undefined: geminiThinkingRecoveryEnv`. That is the correct first failure — the constant arrives in Step 3.

- [ ] **Step 3: Add the toggle**

Create `internal/format/toggles.go`:

```go
package format

import (
	"os"
	"strings"
)

const geminiThinkingRecoveryEnv = "ANTIGRAVITY_GEMINI_THINKING_RECOVERY"

// geminiThinkingRecoveryEnabled reports whether the legacy synthetic tool-loop
// recovery turns are re-enabled for Gemini thinking models.
//
// Those turns ("[Tool execution completed.]" / "[Continue]") are never stored by
// the client, so the next request puts a real functionCall at the same index and
// Gemini implicit context caching misses every turn. They are off by default.
// Gemini validates thought signatures only for function calls in the current
// turn, and unsigned calls already carry GeminiSkipSignature, so the synthetic
// turns are not needed to pass validation. The switch exists only as an operator
// rollback if a backend rejects that bypass.
func geminiThinkingRecoveryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(geminiThinkingRecoveryEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
```

- [ ] **Step 4: Run the tests again**

Run: `go test ./internal/format/ -run 'TestGeminiToolLoop' -count=1 -v`

Expected: `TestGeminiToolLoopRecoveryKillSwitchRestoresSyntheticTurns` PASSES (default code path still appends). `TestGeminiToolLoopKeepsRealToolTurns` FAILS with `contents = ...` showing 5 entries. This is the real red state for this task.

- [ ] **Step 5: Branch the Gemini recovery path**

In `internal/format/request.go`, replace lines 82-88:

```go
	processedMessages := messages
	if family == FamilyGemini && isThinking && needsThinkingRecovery(messages) {
		processedMessages = closeToolLoopForThinking(messages, FamilyGemini, cache)
	}
	if family == FamilyClaude && isThinking && (hasGeminiHistory(messages) || hasUnsignedThinkingBlocks(messages)) && needsThinkingRecovery(messages) {
		processedMessages = closeToolLoopForThinking(messages, FamilyClaude, cache)
	}
```

with:

```go
	processedMessages := messages
	if family == FamilyGemini && isThinking && needsThinkingRecovery(messages) {
		if geminiThinkingRecoveryEnabled() {
			processedMessages = closeToolLoopForThinking(messages, FamilyGemini, cache)
		} else {
			// Strip thinking blocks the backend would reject, but leave the real
			// tool turns in place: appending synthetic turns the client cannot
			// echo back breaks Gemini implicit context caching every turn.
			processedMessages = stripInvalidThinkingBlocks(messages, FamilyGemini, cache)
		}
	}
	if family == FamilyClaude && isThinking && (hasGeminiHistory(messages) || hasUnsignedThinkingBlocks(messages)) && needsThinkingRecovery(messages) {
		processedMessages = closeToolLoopForThinking(messages, FamilyClaude, cache)
	}
```

The Claude branch is deliberately unchanged. Claude models are reached through a different validation path and there is no evidence for touching them.

- [ ] **Step 6: Run the package tests**

Run: `go test ./internal/format/ -count=1`

Expected: PASS, including `TestGeminiToolLoopKeepsRealToolTurns`, `TestGeminiToolLoopRecoveryKillSwitchRestoresSyntheticTurns` and `TestGeminiToolLoopPrefixIsStableAcrossTurns` from Task 1. `TestGeminiToolSignatureIsIndependentOfSignatureCache` still FAILS — Task 3 owns it. If any other test in the package fails, stop and report it rather than editing the test.

- [ ] **Step 7: Run the full gate**

Run: `go build ./... && go test ./... -count=1`

Expected: only `TestGeminiToolSignatureIsIndependentOfSignatureCache` fails.

- [ ] **Step 8: Commit**

```bash
git add internal/format/toggles.go internal/format/request.go internal/format/format_test.go
git commit -m "fix(format): stop appending synthetic Gemini tool-loop turns

The synthetic '[Tool execution completed.]' / '[Continue]' pair is never
stored by the client, so the next request replaces it with a real
functionCall at the same index and Gemini implicit caching misses every
turn. Gemini validates thought signatures only for current-turn calls and
unsigned calls already carry the documented bypass sentinel, so the pair
is unnecessary. ANTIGRAVITY_GEMINI_THINKING_RECOVERY=1 restores it."
```

---

### Task 3: Make the tool thoughtSignature independent of the signature cache

Turns `TestGeminiToolSignatureIsIndependentOfSignatureCache` green. Today a tool call with no client-supplied signature gets one from `SignatureCache`, which has a 2-hour TTL and dies with the process, so the same history converts two different ways and the prefix breaks at the *first* tool call rather than only at the tail.

Losing the real signature costs reasoning continuity on the current turn. That trade is deliberate: the cache only ever helps a client that strips signatures, and a client that strips them is exactly the case where its contents change under the proxy's feet.

**Files:**
- Modify: `internal/format/content.go:53-69`
- Test: `internal/format/format_test.go:183-200` (rewrite `TestGeminiToolSignatureRestorationAndBudgetClamp`)

**Interfaces:**
- Consumes: `GeminiSkipSignature` (`internal/format/model.go:13`), `stringValue(any) string`, `mapOrEmpty(any) any`.
- Produces: no new symbols. After this task `(*SignatureCache).Tool` has no non-test caller, which Task 4 cleans up.

- [ ] **Step 1: Rewrite the test that pins the old cache-restoration behavior**

Find `TestGeminiToolSignatureRestorationAndBudgetClamp` in `internal/format/format_test.go` (starts at line 183). It currently calls `cache.CacheTool("tool-1", signature)` and asserts the converted part carries that signature. Replace the whole test with:

```go
// TestGeminiToolSignatureFallsBackToSkipSentinel pins that a tool call whose
// signature the client stripped converts to the documented bypass sentinel and
// never to a value recovered from process-local state.
func TestGeminiToolSignatureFallsBackToSkipSentinel(t *testing.T) {
	t.Parallel()
	cache := NewSignatureCache()
	cache.CacheTool("tool-1", strings.Repeat("c", MinSignatureLength))
	request := map[string]any{
		"model":      "gemini-2.5-flash",
		"max_tokens": 100,
		"thinking":   map[string]any{"budget_tokens": 999999},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "tool-1", "name": "read", "input": map[string]any{},
			}}},
		},
	}
	converted := ConvertAnthropicToGoogle(request, cache)
	contents := asSlice(converted["contents"])
	part := asMap(asSlice(asMap(contents[len(contents)-1])["parts"])[0])
	if part["thoughtSignature"] != GeminiSkipSignature {
		t.Fatalf("thoughtSignature = %#v", part["thoughtSignature"])
	}
	generation := asMap(converted["generationConfig"])
	thinkingConfig := asMap(generation["thinkingConfig"])
	if intValue(thinkingConfig["thinkingBudget"], 0) != 24576 {
		t.Fatalf("thinkingBudget = %#v", thinkingConfig["thinkingBudget"])
	}
}
```

The budget assertion is carried over from the old test on purpose: `gemini-2.5` clamps to 24576 (`internal/format/model.go:99-112`) and that coverage must not be lost.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/format/ -run 'TestGeminiToolSignatureFallsBackToSkipSentinel|TestGeminiToolSignatureIsIndependentOfSignatureCache' -count=1 -v`

Expected: both FAIL. The first reports `thoughtSignature = "cccc..."` because the cache lookup still wins; the second reports `history part changed when the signature cache expired`.

- [ ] **Step 3: Remove the cache lookup**

In `internal/format/content.go`, replace the `case "tool_use":` body (lines 53-69):

```go
		case "tool_use":
			call := map[string]any{"name": block["name"], "args": mapOrEmpty(block["input"])}
			if family == FamilyClaude && stringValue(block["id"]) != "" {
				call["id"] = block["id"]
			}
			part := map[string]any{"functionCall": call}
			if family == FamilyGemini {
				signature := stringValue(block["thoughtSignature"])
				if signature == "" {
					signature = cache.Tool(stringValue(block["id"]))
				}
				if signature == "" {
					signature = GeminiSkipSignature
				}
				part["thoughtSignature"] = signature
			}
			parts = append(parts, part)
```

with:

```go
		case "tool_use":
			call := map[string]any{"name": block["name"], "args": mapOrEmpty(block["input"])}
			if family == FamilyClaude && stringValue(block["id"]) != "" {
				call["id"] = block["id"]
			}
			part := map[string]any{"functionCall": call}
			if family == FamilyGemini {
				// Only the client's own value is used. Recovering a signature from
				// process-local state makes the same history convert differently
				// after a TTL expiry or a restart, which breaks the Gemini implicit
				// cache prefix at the first tool call.
				signature := stringValue(block["thoughtSignature"])
				if signature == "" {
					signature = GeminiSkipSignature
				}
				part["thoughtSignature"] = signature
			}
			parts = append(parts, part)
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/format/ -count=1`

Expected: PASS, all tests including both Task 1 tests.

- [ ] **Step 5: Run the full gate**

Run: `go build ./... && go test ./... -count=1`

Expected: PASS with no failures.

- [ ] **Step 6: Commit**

```bash
git add internal/format/content.go internal/format/format_test.go
git commit -m "fix(format): derive Gemini tool signatures only from the request

Filling a missing thoughtSignature from the process-local SignatureCache
made the same client history convert differently once the 2h TTL expired
or the proxy restarted, breaking the implicit cache prefix at the first
tool call. Unsigned calls now always use the documented skip sentinel."
```

---

### Task 4: Delete the now-dead tool signature store

Pure cleanup, no behavior change. After Task 3 nothing reads `(*SignatureCache).Tool`, so the `tools` map and its two writers are dead weight. The thinking-family half of `SignatureCache` stays — `content.go:109` and `thinking.go:249` still use it.

**Files:**
- Modify: `internal/format/signature_cache.go:10-60`
- Modify: `internal/format/stream.go:115-125`
- Modify: `internal/format/response.go:48-56`
- Test: `internal/format/format_test.go:63`, `internal/format/format_test.go:215-225`

**Interfaces:**
- Consumes: nothing new.
- Produces: `SignatureCache` loses `Tool` and `CacheTool`; `signatureEntry` loses its `signature` field. No other package references them — verify with `grep -rn 'CacheTool\|\.Tool(' --include='*.go' .` before starting.

- [ ] **Step 1: Confirm the callers**

Run: `grep -rn 'CacheTool\|\.Tool(' --include='*.go' .`

Expected exactly five hits: `internal/format/stream.go`, `internal/format/response.go`, `internal/format/signature_cache.go`, and two in `internal/format/format_test.go`. If anything outside `internal/format` appears, stop and report — this task's premise is wrong.

- [ ] **Step 2: Update the tests first**

At `internal/format/format_test.go:63` a streaming parity test asserts `cache.Tool("tool-1")` returned the captured signature. Delete that `if` block and its body; the surrounding test keeps its other assertions.

Replace the expiry test (around `internal/format/format_test.go:215-225`, the one calling `cache.CacheTool("tool", "signature")`) with the thinking-signature equivalent:

```go
func TestSignatureCacheExpires(t *testing.T) {
	t.Parallel()
	cache := NewSignatureCache()
	now := time.Now()
	cache.now = func() time.Time { return now }
	signature := strings.Repeat("e", MinSignatureLength)
	cache.CacheThinking(signature, FamilyGemini)
	if got := cache.ThinkingFamily(signature); got != FamilyGemini {
		t.Fatalf("family before expiry = %#v", got)
	}
	now = now.Add(3 * time.Hour)
	if got := cache.ThinkingFamily(signature); got != FamilyUnknown {
		t.Fatalf("family after expiry = %#v", got)
	}
}
```

Keep the existing test name if it already is `TestSignatureCacheExpires`; if the current name differs, use the current name so no coverage appears to vanish from the suite listing.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/format/ -count=1`

Expected: PASS. This step is a guard, not a red state: the test edits above only drop assertions about an API that is about to go away, so the suite must stay green before the deletion. If it fails, fix the test edit before continuing.

- [ ] **Step 4: Delete the store**

In `internal/format/signature_cache.go`: remove the `signature string` field from `signatureEntry`, remove the `tools map[string]signatureEntry` field, remove its initialization in `NewSignatureCache`, remove the `CacheTool` and `Tool` methods, and remove `clear(cache.tools)` from `Clear`. Update the type comment to say the cache records the model family that produced a thinking signature.

In `internal/format/stream.go`, inside the `functionCall` branch, delete the line `converter.cache.CacheTool(toolID, signature)` and keep `block["thoughtSignature"] = signature`.

In `internal/format/response.go`, delete the corresponding `cache.CacheTool(toolID, signature)` line (line 54) and keep the surrounding signature assignment.

- [ ] **Step 5: Run the full gate**

Run: `go build ./... && go test ./... -count=1`

Expected: PASS. A build error naming `CacheTool` or `Tool` means a caller was missed — remove it, do not re-add the method.

- [ ] **Step 6: Commit**

```bash
git add internal/format/signature_cache.go internal/format/stream.go internal/format/response.go internal/format/format_test.go
git commit -m "refactor(format): drop the unused tool signature store

Nothing reads it since tool signatures come only from the request. The
thinking-family half of SignatureCache is still used and stays."
```

---

### Task 5: Clamp negative input_tokens in the two unguarded usage mappers

Small hardening, unrelated to the prefix but in the same blast radius. `ThinkingAccumulator.InputTokens` already clamps (`internal/format/response.go:166-173`); the non-streaming converter (`response.go:93`) and the streaming `message_start` (`stream.go:59`) do not. A `usageMetadata` frame where `cachedContentTokenCount > promptTokenCount` would emit a negative `input_tokens`, which every Anthropic client treats as corrupt.

**Files:**
- Modify: `internal/format/response.go:86-98`
- Modify: `internal/format/stream.go:52-64`
- Create: test cases inside `internal/format/cacheprefix_test.go`

**Interfaces:**
- Consumes: `intValue(any, int) int`.
- Produces: `nonNegative(value int) int` in `internal/format/response.go`, used by both mappers.

- [ ] **Step 1: Write the failing tests**

Append to `internal/format/cacheprefix_test.go`:

```go
// TestUsageMappersNeverReportNegativeInputTokens guards against a usageMetadata
// frame whose cached count exceeds the prompt count.
func TestUsageMappersNeverReportNegativeInputTokens(t *testing.T) {
	t.Parallel()
	response := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "hi"}}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":         100,
			"cachedContentTokenCount":  140,
			"candidatesTokenCount":     5,
		},
	}
	converted := ConvertGoogleToAnthropic(response, "gemini-3.0-flash-high", NewSignatureCache())
	usage := asMap(converted["usage"])
	if intValue(usage["input_tokens"], -1) != 0 {
		t.Fatalf("non-streaming input_tokens = %#v", usage["input_tokens"])
	}

	converter := NewStreamConverter("gemini-3.0-flash-high", NewSignatureCache(), "msg_test")
	frame := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":140,"candidatesTokenCount":5}}}`)
	events, err := converter.Consume(frame)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	for _, event := range events {
		if event["type"] != "message_start" {
			continue
		}
		streamUsage := asMap(asMap(event["message"])["usage"])
		if intValue(streamUsage["input_tokens"], -1) != 0 {
			t.Fatalf("streaming input_tokens = %#v", streamUsage["input_tokens"])
		}
	}
}
```

If `NewStreamConverter` has a different signature in this tree, check its definition at `internal/format/stream.go:24` and match it — do not change the constructor. As of this plan it is `NewStreamConverter(model string, cache *SignatureCache, messageID string) *StreamConverter`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/format/ -run TestUsageMappersNeverReportNegativeInputTokens -count=1 -v`

Expected: FAIL with `non-streaming input_tokens = -40`.

- [ ] **Step 3: Add the clamp**

In `internal/format/response.go`, add next to the other small helpers:

```go
func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
```

Change `response.go:93` from:

```go
			"input_tokens":                promptTokens - cachedTokens,
```

to:

```go
			"input_tokens":                nonNegative(promptTokens - cachedTokens),
```

Change `stream.go:59` from:

```go
					"input_tokens":                converter.inputTokens - converter.cacheReadTokens,
```

to:

```go
					"input_tokens":                nonNegative(converter.inputTokens - converter.cacheReadTokens),
```

- [ ] **Step 4: Run the full gate**

Run: `go build ./... && go test ./... -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/format/response.go internal/format/stream.go internal/format/cacheprefix_test.go
git commit -m "fix(format): clamp input_tokens at zero in both usage mappers"
```

---

### Task 6: Live verification against the real backend

The one claim the unit tests cannot settle: whether the Cloud Code / Antigravity backend accepts `skip_thought_signature_validator` on a **current-turn** `functionCall`. Public Gemini documentation says it does; some Vertex surfaces reportedly reject it. Until this passes, treat Tasks 2-5 as unverified in production.

This task needs the operator's own credentials and a running proxy, so a human runs it. Do not attempt to read or copy credential stores.

**Files:**
- Create: `scripts/verify-gemini-cache-prefix.sh`

**Interfaces:**
- Consumes: a running proxy on `$PROXY_URL` (default `http://127.0.0.1:8080`) exposing `/v1/messages`.
- Produces: nothing consumed by other tasks.

- [ ] **Step 1: Write the probe script**

Create `scripts/verify-gemini-cache-prefix.sh`:

```bash
#!/usr/bin/env bash
# Two-turn Gemini tool loop with no thought signatures, the shape a strict
# Anthropic client sends. Turn 1 must return 200. Turn 2 must return 200 and
# should report a non-zero cache_read_input_tokens once the prompt clears the
# ~32k implicit-cache floor.
set -euo pipefail

PROXY_URL="${PROXY_URL:-http://127.0.0.1:8080}"
MODEL="${MODEL:-gemini-3.0-flash-high}"
PAD="$(head -c 60000 /dev/urandom | base64 | tr -d '\n' | head -c 60000)"

turn() {
  local payload="$1" label="$2"
  local body status
  body="$(mktemp)"
  status="$(curl -sS -o "$body" -w '%{http_code}' \
    -X POST "$PROXY_URL/v1/messages" \
    -H 'content-type: application/json' \
    -H 'anthropic-version: 2023-06-01' \
    --data-binary "$payload")"
  echo "== $label: HTTP $status"
  if [ "$status" != "200" ]; then
    echo "-- error body:"
    head -c 2000 "$body"
    echo
    rm -f "$body"
    return 1
  fi
  grep -o '"usage":{[^}]*}' "$body" || echo "-- no usage block in response"
  rm -f "$body"
}

read -r -d '' TURN1 <<JSON || true
{"model":"$MODEL","max_tokens":256,"tools":[{"name":"read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"messages":[
 {"role":"user","content":"Context padding: $PAD\n\nCall the read tool on file.go, then summarise."},
 {"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read","input":{"path":"file.go"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"package main"}]}
]}
JSON

read -r -d '' TURN2 <<JSON || true
{"model":"$MODEL","max_tokens":256,"tools":[{"name":"read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"messages":[
 {"role":"user","content":"Context padding: $PAD\n\nCall the read tool on file.go, then summarise."},
 {"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read","input":{"path":"file.go"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"package main"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"toolu_b","name":"read","input":{"path":"other.go"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_b","content":"package other"}]}
]}
JSON

turn "$TURN1" "turn 1"
sleep 2
turn "$TURN2" "turn 2 (prefix of turn 1 must be reused)"
```

Then: `chmod +x scripts/verify-gemini-cache-prefix.sh`

- [ ] **Step 2: Ask the operator to run it**

Hand over exactly this, with the proxy already running and authenticated:

```bash
PROXY_URL=http://127.0.0.1:8080 ./scripts/verify-gemini-cache-prefix.sh
```

Interpretation:
- Both turns HTTP 200 and turn 2 reporting `cache_read_input_tokens` > 0 — the fix is verified; proceed.
- Either turn returning 400 with a message mentioning a thought signature — the backend rejects the bypass on current-turn calls. Set `ANTIGRAVITY_GEMINI_THINKING_RECOVERY=1` to restore the old behavior, then reopen the design: the next candidate is to keep the real tool turns but append nothing, relying on signatures the client does echo, and if that is impossible, accept the synthetic turns and instead make them *stable* by having the proxy also strip them from any history that already contains them.
- Turn 2 reporting `cache_read_input_tokens: 0` with HTTP 200 — inconclusive, not a failure: the prompt may sit under the ~32k implicit-cache floor, or the 5-minute TTL may have lapsed. Increase `PAD`, rerun within a minute, and re-read.

- [ ] **Step 3: Record the result**

Append a "Live verification" section to `docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md` with the date, the model used, both HTTP statuses and both `usage` blocks verbatim. The spec's "Open question" section is only allowed to be deleted once this section exists.

- [ ] **Step 4: Commit**

```bash
git add scripts/verify-gemini-cache-prefix.sh docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md
git commit -m "test(scripts): add live Gemini cache prefix probe"
```

---

## Self-Review

**Spec coverage:**
- E1/E2/E3 (synthetic turns break the prefix) → Task 1 test 1, Task 2.
- E4 (signature cache expiry rewrites history) → Task 1 test 2, Task 3; Task 4 removes the resulting dead code.
- E5 (Google's current-turn-only validation contract) → the rationale for Task 2, probed in Task 6.
- E6 (accounting is already correct) → no task changes the mapping, per Global Constraints; Task 5 only clamps a negative.
- E7 (5-minute TTL cold miss) → no task. Documented in the spec as not-a-defect; Task 6 step 2 names it as a source of inconclusive probe results.
- V1 (purity invariant) → Task 1 encodes it; Tasks 2 and 3 satisfy it.
- Open question → Task 6, with the rollback named in Task 2.

**Placeholders:** none. Every code step carries the literal code; every run step carries the command and the expected output.

**Type consistency:** `geminiThinkingRecoveryEnv` and `geminiThinkingRecoveryEnabled()` are defined in Task 2 and used only there. `mustJSON` is defined in Task 1 and reused in Task 5. `nonNegative` is defined in Task 5 and used twice in that same task. `GeminiSkipSignature` and `MinSignatureLength` are pre-existing constants, cited with file and line in Global Constraints. `(*SignatureCache).CacheTool` is used by Task 1's test and deleted in Task 4 — Task 4 Step 2 therefore also has to drop the `warm.CacheTool` line from `TestGeminiToolSignatureIsIndependentOfSignatureCache` and assert on two cold caches instead; the test still proves purity because after Task 3 no cache state can reach the output.

## Known ordering hazard

Task 4 deletes an API that Task 1's second test calls. Run the tasks in order 1 → 2 → 3 → 4 → 5 → 6. If Task 4 is reached and `go build ./...` fails on `warm.CacheTool` inside `cacheprefix_test.go`, rewrite that test to compare two freshly constructed caches rather than re-adding the method.
