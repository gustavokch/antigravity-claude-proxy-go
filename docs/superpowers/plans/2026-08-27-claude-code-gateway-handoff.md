# Handoff: Claude Code Official API Gateway

## Goal
Implement the new Claude Code Official API Gateway for `antigravity-claude-proxy-go`, providing native Anthropic API forwarding (`https://api.anthropic.com/v1/messages`), multi-account rotation, single-account exponential backoff, rate-limit header parsing, pricing matrix calculations, credential auto-import, and WebUI management.

## Key Artifacts
- **Design Spec**: `docs/superpowers/specs/2026-08-27-claude-code-gateway-design.md`
- **Plan File**: `/Users/gus/.claude/plans/lets-plan-a-new-hidden-otter.md` (and `docs/superpowers/plans/2026-08-27-claude-code-gateway.md`)

## Key Decisions & Context
1. **Auth Header**: Anthropic uses `x-api-key: <token>` for both API keys and OAuth/setup tokens.
2. **Account Modes**:
   - Multi-account pool with sticky session affinity and immediate failover on 429 / exhaustion.
   - Single-account mode with exponential jitter backoff on 429 (`retry-after` / `backoffBaseMs`).
3. **Live Rate Limits**: Extracted from `anthropic-ratelimit-*` headers (`requests-remaining`, `tokens-remaining`, reset timestamps) and exposed to WebUI.
4. **Pricing Matrix**: Built-in pricing for Claude Fable 5 ($10/$50), Opus 5 ($5/$25), Sonnet 5 ($2/$10), Haiku 4.5 ($1/$5), and prompt caching multipliers (1.25x write, 0.1x read).
5. **Precedence**: `Kimi` -> `ClaudeCode` -> `OpenRouter` -> `CustomEndpoints` -> `CloudCode`.
6. **Execution Method**: Execute inline without using subagents.

## Implementation Tasks Summary
1. `internal/claudecode/types.go` & `ratelimit.go` (with tests)
2. `internal/claudecode/pricing.go` & `router.go` (with tests)
3. `internal/claudecode/pool.go` & `client.go` (with tests)
4. `internal/claudecode/discovery.go` & `observability.go` (with tests)
5. `internal/config/config.go` integration
6. `internal/api/claudecode_proxy.go` & `internal/api/server.go` routing integration
7. `internal/api/management.go` endpoints & WebUI updates
8. Verification via `go test ./...` and manual curl tests

## Suggested Skills for Next Agent
- `superpowers:executing-plans` (execute plan inline without subagents)
- `superpowers:test-driven-development`
- `superpowers:verification-before-completion`
