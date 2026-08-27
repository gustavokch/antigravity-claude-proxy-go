# Claude Code Gateway Design Specification

## 1. Overview & Goals

The Claude Code Gateway provides a high-performance, fault-tolerant proxy gateway for the official Anthropic Claude API (`https://api.anthropic.com/v1/messages`). It runs natively within `antigravity-claude-proxy-go`, modeled after the OpenRouter gateway and account dispatcher subsystems.

### Core Objectives:
1. **Official Anthropic API Support**: Transparently forward requests to `https://api.anthropic.com/v1/messages` using `x-api-key: <token>` (compatible with standard API keys and OAuth/setup tokens).
2. **Flexible Account Management**:
   - **Multi-Account Pool**: Automatic rotation across multiple credentials on 429/quota limits with sticky session affinity.
   - **Single Account Mode**: Configurable exponential jitter backoff and retry handling on rate limits.
3. **Live Rate-Limit Tracking**: Extract and parse `anthropic-ratelimit-*` headers to track live RPM/TPM and reset timestamps per account in memory.
4. **Accurate Cost Accounting**: Built-in pricing matrix supporting Claude Fable 5, Opus 5, Sonnet 5, Haiku 4.5, and prompt caching multipliers (1.25x cache write, 0.10x cache read).
5. **Credential Auto-Import & CLI Helper**: Optional auto-import from local `~/.claude.json` / `~/.claude/settings.json` (disabled by default) and interactive CLI helper.
6. **WebUI Integration**: Dedicated accounts panel showing rate limits, remaining tokens, cooldowns, and test triggers.
7. **Headroom & CCR Integration**: Full compatibility with Headroom context compression and transparent CCR hydration loop.

---

## 2. Architecture & Package Structure

The gateway is implemented in a dedicated package `internal/claudecode/`:

```
internal/claudecode/
├── types.go           # Account, config, model, and rate limit data structures
├── client.go          # HTTP client for Anthropic API (/v1/messages, /v1/models)
├── pool.go            # Account pool, health tracker, sticky sessions, cooldowns
├── router.go          # Model allowlist, alias resolution, and request validation
├── pricing.go         # Anthropic model pricing matrix and token cost calculation
├── ratelimit.go       # Anthropic rate-limit header parser (tokens, requests, resets)
├── discovery.go       # Local credential discovery (~/.claude.json, env) & login helper
└── observability.go   # Metrics collection, latency, throughput, and WebUI broadcasting
```

### Integration Points:
- `internal/config/config.go`: Stores `ClaudeCodeConfig` in the root proxy configuration.
- `internal/api/server.go`: Evaluates Claude Code allowlist after Kimi and before OpenRouter.
- `internal/api/claudecode_proxy.go`: Manages failover loop, streaming SSE events, and CCR hydration.
- `internal/api/management.go`: Management endpoints (`/api/claudecode/*`) for configuration and live account stats.

---

## 3. Configuration Model

```go
type ClaudeCodeAccountConfig struct {
    ID       string `json:"id"`
    Name     string `json:"name"`
    Token    string `json:"token"`
    Type     string `json:"type"`     // "setup_token" or "api_key"
    Priority int    `json:"priority"` // Lower number = higher priority
    Enabled  bool   `json:"enabled"`
    Source   string `json:"source,omitempty"` // "manual", "auto_import", "cli"
}

type ClaudeCodeModelConfig struct {
    ID              string `json:"id"`
    Alias           string `json:"alias,omitempty"`
    DisplayName     string `json:"displayName,omitempty"`
    ContextLen      int    `json:"contextLength,omitempty"`
    MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
    Enabled         bool   `json:"enabled"`
}

type ClaudeCodeRoutingConfig struct {
    Retry429Max      int `json:"retry429Max,omitempty"`      // Default: 5
    BackoffBaseMs    int `json:"backoffBaseMs,omitempty"`    // Default: 1000
    BackoffCapMs     int `json:"backoffCapMs,omitempty"`     // Default: 30000
    RequestBudgetMs  int `json:"requestBudgetMs,omitempty"`  // Default: 120000
    CooldownDuration int `json:"cooldownDuration,omitempty"` // Default: 30000 ms
}

type ClaudeCodeConfig struct {
    Enabled    bool                     `json:"enabled"`
    BaseURL    string                   `json:"baseUrl"`    // Default: "https://api.anthropic.com"
    Mode       string                   `json:"mode"`       // "pool" or "single"
    AutoImport bool                     `json:"autoImport"` // Default: false
    Accounts   []ClaudeCodeAccountConfig `json:"accounts,omitempty"`
    Allowlist  []ClaudeCodeModelConfig  `json:"allowlist,omitempty"`
    Routing    ClaudeCodeRoutingConfig  `json:"routing,omitempty"`
}
```

---

## 4. Authentication & Rate-Limit Tracking

### Authentication Header:
Anthropic API requires `x-api-key` for authentication even when using OAuth/setup tokens:
```http
POST /v1/messages HTTP/1.1
Host: api.anthropic.com
x-api-key: <token>
anthropic-version: 2023-06-01
content-type: application/json
```

### Rate-Limit Header Extraction:
Every upstream response is inspected for:
- `anthropic-ratelimit-requests-limit`
- `anthropic-ratelimit-requests-remaining`
- `anthropic-ratelimit-requests-reset` (RFC3339 timestamp)
- `anthropic-ratelimit-tokens-limit`
- `anthropic-ratelimit-tokens-remaining`
- `anthropic-ratelimit-tokens-reset` (RFC3339 timestamp)
- `retry-after`

Account state updates atomically in memory and emits stats updates to the WebUI.

---

## 5. Account Selection & Failover Engine

### Selection Strategy:
1. **Sticky Session**: Hash session identifier (`x-session-id`, `anthropic-session-id`, or prompt fingerprint) to select preferred account.
2. **Health Filter**: Skip accounts in `Cooldown` or with `TokensRemaining <= 0` / `RequestsRemaining <= 0` (if reset timestamp is in the future).
3. **Load Balancing**: Fall back to the healthy account with highest priority and lowest active in-flight requests.

### Error & 429 Handling:
- **429 Rate Limit (Multi-Account)**:
  - Account placed into `Cooldown` until `TokensReset` / `RequestsReset` (or default cooldown duration).
  - Immediately retry on next healthy account in the candidate chain.
- **429 Rate Limit (Single Account)**:
  - Sleep for duration indicated by `retry-after` or compute exponential jitter backoff (`backoffBaseMs` * 2^attempt, capped at `backoffCapMs`).
  - Retry up to `retry429Max` times.
- **5xx / Transport Failure**:
  - Track consecutive errors. On threshold (default 3), trigger cooldown and rotate to next account.

---

## 6. Pricing Matrix (`internal/claudecode/pricing.go`)

| Model ID | Aliases | Input ($/MTok) | Output ($/MTok) | Cache Write ($/MTok) | Cache Read ($/MTok) |
|---|---|---|---|---|---|
| `claude-fable-5` | `claude-fable-5` | $10.00 | $50.00 | $12.50 | $1.00 |
| `claude-opus-5` | `claude-opus-5` | $5.00 | $25.00 | $6.25 | $0.50 |
| `claude-sonnet-5` | `claude-sonnet-5` | $2.00 | $10.00 | $2.50 | $0.20 |
| `claude-haiku-4-5-20251001` | `claude-haiku-4-5` | $1.00 | $5.00 | $1.25 | $0.10 |
| `claude-3-7-sonnet-20250219` | `claude-3-7-sonnet` | $3.00 | $15.00 | $3.75 | $0.30 |
| `claude-3-5-sonnet-20241022` | `claude-3-5-sonnet` | $3.00 | $15.00 | $3.75 | $0.30 |
| `claude-3-5-haiku-20241022` | `claude-3-5-haiku` | $0.80 | $4.00 | $1.00 | $0.08 |
| `claude-3-opus-20240229` | `claude-3-opus` | $15.00 | $75.00 | $18.75 | $1.50 |

---

## 7. Credential Discovery & CLI Helper

### Discovery Engine (`internal/claudecode/discovery.go`):
When `autoImport: true`:
1. Check `~/.claude.json` and `~/.claude/settings.json`.
2. Extract credentials (`sessionKey`, `token`, `oauth_token`, or `apiKey`).
3. Create auto-managed account entry tagged with `source: "auto_import"`.

### CLI Login Helper:
Add proxy sub-command or helper to validate/store tokens interactively.

---

## 8. Management API & WebUI

### REST Endpoints:
- `GET /api/claudecode/config`: Retrieve gateway configuration.
- `POST /api/claudecode/config`: Update gateway configuration.
- `GET /api/claudecode/accounts`: List accounts, status, live RPM/TPM limits, and usage.
- `POST /api/claudecode/accounts`: Add/update account.
- `DELETE /api/claudecode/accounts/{id}`: Delete account.
- `POST /api/claudecode/accounts/{id}/test`: Test account connection against Anthropic `/v1/models`.

---

## 9. Verification & Test Strategy

1. **Unit Tests**:
   - `ratelimit_test.go`: Test header extraction for edge cases and malformed timestamps.
   - `pool_test.go`: Test account selection, sticky routing, concurrency accounting, and cooldown recovery.
   - `pricing_test.go`: Verify exact USD cost calculation with prompt caching.
   - `router_test.go`: Test model allowlist and alias resolution.
   - `discovery_test.go`: Test mock credential discovery from JSON config files.
2. **Integration Tests**:
   - `internal/api/claudecode_proxy_test.go`: Full end-to-end proxy test using `httptest.Server` verifying streaming SSE, 429 rotation, header propagation (`x-api-key`), and CCR hydration.
