# PR #54 Follow-up Remediation (2026-09-02)

Source: review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/54#issuecomment-5518840539 (2026-09-03T01:28Z).

## Findings

1. 🟡 **risk** — `server.go:1084-1169`: cold OpenRouter cache → `deriveOpenRouterMaxOutput` = 0 → omit `max_tokens` → upstream `/v1/messages` 400 (Anthropic schema requires `max_tokens`). First request to a new model always fails. Fix: when the client omitted `max_tokens` and no limit is known on the OpenRouter path, reject early with a clear 400 instead of forwarding a doomed request. (Kimi/OpenAI path keeps omitting — `max_tokens` optional there.)
2. 🔵 **nit** — `server.go:1017-1023`: docstring says "Never raised" but floor raises. Say floor may raise.
3. 🔵 **nit** — `openai_request.go:36-38`: malformed non-null `tool_calls` silently skipped. Add `slog.Warn`.
4. 🔵 **nit** — `openai_proxy_test.go`: add floor case (request 4 → forwarded 16).
5. 🔵 **nit** — `claudecode_proxy_test.go`: remove blank lines inside `TestClaudeCodeEntryMaxOutput`.
6. 🔵 **nit** — `models.js:1157`: pre-fill `maxOutputTokens` from `discoveredModel.top_provider.max_completion_tokens` when present.
7. 🔵 **nit** — `server.go:1058,1075`: `slog.Warn` on non-numeric `max_tokens` and on marshal failure.
8. 🔵 **nit** — `openrouter_proxy_test.go:575`: add sibling case 4096 → 16 when nothing known.

## Tasks (TDD per task)

### Task 1 — OpenRouter unknown-limit guard
- Test: update `TestOpenRouterForwarding_OmitsMaxTokensWhenNothingKnown` → `TestOpenRouterForwarding_RejectsWhenNothingKnown`; expect 400, upstream never called.
- Impl: in `forwardToOpenRouter` handler (server.go:1168-1170), when `!hadMaxTokens && derived == 0 && body` unchanged → `http.Error(w, "max_tokens required: no client value and model limit unknown", 400)`.

### Task 2 — docstring fix (server.go:1017)

### Task 3 — slog.Warn malformed tool_calls (openai_request.go)

### Task 4 — slog.Warn policy branches (server.go:1058,1075)

### Task 5 — openai test floor case

### Task 6 — openrouter test sibling case (4096 → 16, manualOverride = floor-1)

### Task 7 — claudecode test blank-line cleanup

### Task 8 — models.js pre-fill
