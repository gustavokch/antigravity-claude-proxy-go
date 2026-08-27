# Claude Code Official API Gateway Implementation Plan

## Context
Provide native, resilient gateway for official Anthropic Claude API (`https://api.anthropic.com/v1/messages`). Support multi-account rotation, single-account exponential backoff, rate-limit header extraction, pricing matrix calculation, credential auto-import, WebUI visibility, and Headroom CCR hydration.

---

## Proposed Changes

### 1. `internal/claudecode` Subsystem
Create new package `internal/claudecode`:
- `types.go`: Structs for `Account`, `AccountConfig`, `ModelConfig`, `RoutingConfig`, `ClaudeCodeConfig`, `RateLimits`.
- `ratelimit.go` & `ratelimit_test.go`: Extract `anthropic-ratelimit-*` headers (requests, tokens, resets) and `retry-after`.
- `pricing.go` & `pricing_test.go`: Model pricing table (Fable 5, Opus 5, Sonnet 5, Haiku 4.5, 3.7/3.5 models) with prompt caching multipliers.
- `router.go` & `router_test.go`: Allowlist and alias matcher.
- `pool.go` & `pool_test.go`: Account pool, health tracker, sticky session affinity, concurrency counter, and cooldown manager.
- `client.go` & `client_test.go`: HTTP client sending `x-api-key: <token>`, `anthropic-version: 2023-06-01`, and preserving beta headers.
- `discovery.go` & `discovery_test.go`: Auto-import credentials from `~/.claude.json` / `~/.claude/settings.json`.
- `observability.go` & `observability_test.go`: Metrics tracking and broadcaster integration.

### 2. Configuration (`internal/config/config.go`)
- Add `ClaudeCode ClaudeCodeConfig` to root `Config` struct.
- Add default allowlist (Claude Fable 5, Opus 5, Sonnet 5, Haiku 4.5).
- Add defaults for backoff, 429 retries, and cooldown periods.

### 3. Proxy Handler & Routing (`internal/api/`)
- `internal/api/claudecode_proxy.go`: Streaming SSE and unary forwarder with account rotation on 429, retry backoff, and CCR hydration integration.
- `internal/api/server.go`: Integrate `forwardToClaudeCode` into `/v1/messages` request handler pipeline (precedence: `Kimi` -> `ClaudeCode` -> `OpenRouter` -> `CustomEndpoints` -> `CloudCode`). Add Claude Code models to `/v1/models`.
- `internal/api/management.go`: Management endpoints `/api/claudecode/config`, `/api/claudecode/accounts`, `/api/claudecode/accounts/:id/test`.
- `internal/api/claudecode_proxy_test.go`: End-to-end integration tests.

---

## Verification Plan

### Automated Tests
1. **Unit Tests**:
   - `go test -v -race ./internal/claudecode/...`
2. **Integration Tests**:
   - `go test -v -race ./internal/api/ -run "TestClaudeCode"`
3. **Full Suite**:
   - `go test ./...`

### Manual End-to-End Verification
1. Run local test server with Claude Code gateway enabled.
2. Send test request to `/v1/messages` targeting `claude-sonnet-5` with `x-api-key`.
3. Verify rate-limit headers extracted and visible in `/api/claudecode/accounts`.
4. Trigger simulated 429 response and verify account rotation / backoff.
