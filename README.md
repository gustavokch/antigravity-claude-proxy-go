# Antigravity Claude Proxy (Go)

`antigravity-claude-proxy-go` exposes a local Anthropic Messages API for **Claude Code**, **Hermes Agent**, and other Anthropic-compatible clients. It provides two distinct routing paths:

1. **Translation Route**: Mirrors the official `agy` CLI upstream—native Go HTTPS transport, Cloud Code REST/SSE endpoints (`v1internal:streamGenerateContent`), exact client identity headers, and identical TLS ClientHello fingerprints with multi-account quota rotation.
2. **Transparent Forwarding**: Direct reverse-proxy path bypassing translation for configured models, streaming unmodified Anthropic requests and SSE events to **OpenRouter's Anthropic Skin** (`/api/v1/messages`) or user-configured **Custom Endpoints**.

The proxy listens by default on `127.0.0.1:8080` (configurable) and includes an embedded flat dark **Web UI dashboard**, multi-account pool management with Google OAuth 2.0 PKCE, real-time log streaming, unified model discovery, and server-side model mapping.

---

## Key Features

- **Exact `agy` Transport & Fingerprint Matching**: Matches the JA4 fingerprint (`t13d131100_f57a46bbacb6_f50d94e863eb`), cipher suite order, ALPN state, and header behavior of the official `agy` CLI.
- **Embedded Web UI Dashboard**: Manage accounts, monitor rate limits, inspect request volume history, stream live logs via SSE, configure server/Claude CLI presets, manage model mappings, discover OpenRouter models, and manage Claude Code OAuth credentials.
- **Multi-Account Pool & Selection Strategies**:
  - Auto-discovers local `agy` login credentials at `~/.gemini/antigravity-cli/antigravity-oauth-token`.
  - Built-in Google OAuth 2.0 PKCE sign-in flow for adding accounts (`antigravity-proxy accounts add` or Web UI).
  - Selection strategies: `hybrid` (health score, token bucket rate, quota remaining, and least-recently-used scoring), `sticky`, and `round-robin`.
  - Tracks subscription tiers (`PRO`, `FREE`, etc.), per-account rate limits, and model cooldowns in memory.
- **Claude Code Native OAuth Pool & Gateway**:
  - Direct reverse-proxy gateway to official Anthropic API (`api.anthropic.com`) using Claude Code OAuth tokens (`ant-oat-*`).
  - Native multi-account OAuth login flow in GUI and CLI (`antigravity-proxy accounts add --provider claudecode`) with localhost callback listener and manual URL extraction fallback.
  - Multi-account rotation pool for official Claude Code accounts with 0600 file permission token storage and session TTL management.
  - Automatic `Authorization: Bearer` handling and beta header injection.
- **Headroom Native Context Compression & Output Shaping**:
  - Provider-agnostic middleware pipeline (`internal/headroom`) executing across Cloud Code, OpenRouter, Kimi, Claude Code, and Custom Endpoints.
  - **CCR (Content-Conditioned Retrieval)**: Reversible chunk storage with SHA-256 hashing, LRU eviction, dynamic `headroom_retrieve` tool injection, and transparent hydration passthrough loop across all upstreams.
  - **SmartCrusher**: Exact byte-for-byte JSON minification (`CompactJSON`) and automatic Markdown/TSV table conversion (`TabularArray`) for homogeneous object arrays with 30%+ savings.
  - **CodeCompressor**: Prunes trailing whitespace, collapses multi-line blank gaps, strips full-line comments, and deduplicates recurring logs.
  - **Output Shaper**: Verbosity steering via system prompt injection to suppress conversational filler, plus effort routing (thinking budget clamping on mechanical tool continuations).
- **OpenRouter Gateway & Transparent Proxying**:
  - Direct reverse proxying to OpenRouter's Anthropic Skin (`https://openrouter.ai/api/v1/messages`) with zero payload translation overhead.
  - On-demand gateway model discovery (`GET /v1/models`) with local search, provider filtering, and one-click allowlisting.
  - Custom local model aliases and dynamic `max_tokens` calculation from OpenRouter provider metadata.
- **Transparent Forwarding to Custom Endpoints**:
  - Map specific model names directly to external Anthropic-compatible URLs with dedicated API keys and native SSE flushing.
- **Unified Model Catalog & Server-Side Mapping**:
  - Exposes Google Cloud Code models, allowlisted OpenRouter models, and custom aliases under `GET /v1/models`.
  - Supports `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` and `CLAUDE_CODE_SKIP_FAST_MODE_ORG_CHECK`.
  - Server-side model mapping (`/api/models/config`) to rewrite requested models to target models with multi-hop loop protection.
- **CLI & Daemon Management**: Integrated CLI commands (`start`, `stop`, `restart`, `status`, `web`, `accounts`) with background daemon support.
- **Client Integrations**: Seamless integration with **Claude Code** and **Hermes Agent** with custom quota usage reporting.

---

## Core Routing Architecture

```
[Claude Code / Hermes / Anthropic SDK Client]
                      │
                      ▼ POST /v1/messages { model: "<requested_model>", ... }
┌─────────────────────────────────────────────────────────────────────────────┐
│ Proxy Router (internal/api/server.go)                                       │
│                                                                             │
│ 0. Headroom Pipeline (CCR, SmartCrusher, CodeCompressor, OutputShaper)      │
│                                                                             │
│ 1. Model Mapping Resolution (Resolves aliases & chained mappings <= 5 hops) │
│                                                                             │
│ 2. Match Claude Code OAuth Token (ant-oat-*) or Claude Code Pool?           │
│    └─► Transparent ReverseProxy to https://api.anthropic.com/v1/messages   │
│    └─► Inject Bearer token, merge beta headers, handle CCR hydration loop   │
│                                                                             │
│ 3. Match OpenRouter Allowlist / Alias?                                      │
│    └─► Rewrite model to upstream OpenRouter ID                              │
│    └─► Transparent ReverseProxy to OpenRouter (/v1/messages)                │
│                                                                             │
│ 4. Match Custom Endpoints Map?                                              │
│    └─► Transparent ReverseProxy to Custom Endpoint URL                      │
│                                                                             │
│ 5. Match Google Cloud Code Catalog?                                         │
│    └─► Select Account via Strategy (hybrid / sticky / round-robin)          │
│    └─► Translate to Cloud Code format + Stream SSE + Adaptive Thinking      │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## What “Matching agy” Means

Fresh packet captures from `agy 1.1.2` were taken with both Gemini and Claude models. Both used:

- `POST https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse`
- SNI `daily-cloudcode-pa.googleapis.com`
- No ALPN extension
- JA4 `t13d131100_f57a46bbacb6_f50d94e863eb`
- Go-style `gl-go/...` and Antigravity client identity headers

The Go proxy matches the complete JA4, SNI, ALPN state, cipher list, and signature algorithms. Evidence is checked in at:

- [agy Gemini baseline](.reference/agy-current-baseline.txt)
- [agy Claude baseline](.reference/agy-claude-current-baseline.txt)
- [Go proxy fingerprint gate](.reference/go-current-baseline.txt)
- [live agy/proxy recheck](.reference/fingerprint-recheck-20260715.txt)
- [current model catalog](.reference/agy-current-models.txt)

TLS transport intentionally uses Go's standard library with an empty `tls.Config{}`. Never set custom cipher suites, curves, ALPN, or TLS versions, as doing so alters the fingerprint.

---

## Requirements

- **Go**: `1.24+` (or `go1.27rc2`)
- **OS**: Linux or macOS (systemd service support on Linux)
- **Upstream Auth**: A logged-in `agy` CLI, Google OAuth account credentials, or an OpenRouter API key
- **Tools**: `curl` for API testing; `tcpdump` / `tshark` for packet verification

---

## Build and Install

### Quick Install Script
```sh
make install
# Installs binary to ~/.local/bin/antigravity-proxy
```

### Manual Build
```sh
make build
# Binary created at ./bin/antigravity-proxy
```

### Run Tests & Verification
```sh
make test
go vet ./...
```

### Profile-Guided Optimization (PGO)
For maximum throughput:
1. Run proxy with `-pprof`.
2. Generate benchmark load.
3. Capture profile: `curl -o default.pgo http://localhost:6060/debug/pprof/profile?seconds=60`
4. Rebuild with PGO: `go build -pgo=default.pgo -ldflags="-s -w" -trimpath -o bin/antigravity-proxy ./cmd/proxy`

---

## Quick Start

1. Start the proxy server:
```sh
export ANTIGRAVITY_PROXY_API_KEY='choose-a-local-secret'
./bin/antigravity-proxy
```

2. Open the Web UI:
```sh
./bin/antigravity-proxy web
# Opens http://127.0.0.1:8080 in default browser
```

3. Test health check:
```sh
curl -sS http://127.0.0.1:8080/health | jq
```

---

## CLI Command Reference

The `antigravity-proxy` CLI provides built-in daemon and account management commands:

| Command | Description |
|---|---|
| `antigravity-proxy` / `antigravity-proxy start` | Start the proxy server in foreground |
| `antigravity-proxy start --daemon` | Run the proxy in background daemon mode |
| `antigravity-proxy stop` | Stop the running daemon process |
| `antigravity-proxy restart` | Stop and restart the proxy daemon |
| `antigravity-proxy status` | Show process status (PID, listen address, account pool status) |
| `antigravity-proxy web` | Launch Web UI dashboard in default browser |
| `antigravity-proxy accounts list` | Display configured accounts, sources, status, and subscription tiers |
| `antigravity-proxy accounts add` | Start interactive Google OAuth flow in browser to add a new account |
| `antigravity-proxy accounts add --provider claudecode` | Start interactive Claude Code OAuth flow to add an official Anthropic account |
| `antigravity-proxy accounts remove <email>` | Remove an account from pool |
| `antigravity-proxy accounts verify` | Test access token validity across all accounts |

---

## Configuration & Environment Variables

Proxy settings can be configured via flags, environment variables, or `~/.config/antigravity-proxy/config.json`.

### Flags and Environment Variables

| Flag | Environment Variable | Default | Purpose |
|---|---|---|---|
| `-listen` | `ANTIGRAVITY_PROXY_LISTEN` | `127.0.0.1:8080` | Local HTTP listen address |
| `-port` | - | `0` (disabled) | Override port in listen address |
| `-api-key` | `ANTIGRAVITY_PROXY_API_KEY` | `""` (none) | Required local proxy API key |
| `-accounts` | `ANTIGRAVITY_ACCOUNTS_FILE` | auto | Account-pool JSON file path |
| `-strategy` | `ACCOUNT_STRATEGY` | `hybrid` | Account selection strategy (`hybrid`, `sticky`, `round-robin`) |
| `-project` | `AGY_PROJECT_ID` | auto-detected | Global Cloud Code project override |
| `-upstream-timeout` | - | `5m` | Upstream Cloud Code request timeout |
| `-pprof` | - | `false` | Enable pprof server on `localhost:6060` |
| `-daemon` | - | `false` | Run proxy process in background |

Additional environment controls:
- `AGY_TOKEN_PATH`: Path to default `agy` token file.
- `AGY_BINARY_PATH`: Path to `agy` binary for OAuth secret extraction.
- `AGY_TOKEN_WRITEBACK=1`: Enable writing refreshed OAuth tokens back to disk.
- `DEBUG=true` / `ANTIGRAVITY_DEV_MODE=true`: Enable debug level logging.
- `ANTIGRAVITY_CONFIG_DIR`: Path to custom configuration directory (overrides `~/.config/antigravity-proxy`).

### Configuration File (`config.json`)

`~/.config/antigravity-proxy/config.json` holds proxy settings, custom endpoints, OpenRouter integration, Claude Code settings, and model mapping rules:

```json
{
  "apiKey": "choose-a-local-secret",
  "webuiPassword": "",
  "logLevel": "info",
  "maxRetries": 5,
  "defaultCooldownMs": 10000,
  "modelMapping": {
    "claude-3-5-sonnet-20241022": "claude-sonnet-4-6",
    "claude-3-opus-20240229": "claude-opus-4-6-thinking"
  },
  "customEndpoints": {
    "my-custom-model": {
      "url": "https://api.example.com/v1/messages",
      "apiKey": "sk-example-key"
    }
  },
  "claudeCode": {
    "enabled": true,
    "accountStrategy": "hybrid",
    "modelAliases": {
      "claude-3-7-sonnet-20250219": "claude-3-7-sonnet-20250219",
      "claude-3-5-sonnet-20241022": "claude-3-7-sonnet-20250219"
    }
  },
  "openrouter": {
    "enabled": true,
    "apiKey": "sk-or-v1-...",
    "baseUrl": "https://openrouter.ai/api",
    "allowlist": [
      {
        "id": "anthropic/claude-3.7-sonnet",
        "alias": "claude-3-7-openrouter",
        "displayName": "Claude 3.7 Sonnet (OpenRouter)",
        "contextLength": 200000,
        "maxOutputTokens": 64000,
        "enabled": true
      }
    ],
    "appSpoof": {
      "title": "Claude Code",
      "categories": "cli-agent",
      "referer": "https://claude.ai/code"
    }
  },
  "headroom": {
    "enabled": false,
    "smartCrusher": true,
    "tabularArrays": true,
    "codeCompressor": true,
    "liveTurns": 2,
    "ccr": {
      "enabled": false,
      "maxStoreMB": 64,
      "minChunkBytes": 2048
    },
    "outputShaper": {
      "enabled": false,
      "verbositySteering": true,
      "steeringText": "",
      "effortRouting": true,
      "mechanicalThinkingBudget": 1024
    }
  },
  "accountSelection": {
    "strategy": "hybrid"
  }
}
```

---

## Headroom Native Context Compression & Output Shaping

The proxy integrates native, provider-agnostic **Headroom** optimizations (`internal/headroom`) executed before provider dispatch across Cloud Code, OpenRouter, Kimi, Claude Code, and Custom Endpoints.

### Pipeline Architecture & Core Invariants

The Headroom Engine executes 4 stages in strict sequence, mutating the Anthropic request map in place:

```
Request ──► [ 1. CCR Stage ] ──► [ 2. SmartCrusher ] ──► [ 3. CodeCompressor ] ──► [ 4. OutputShaper ] ──► Provider Dispatch
```

- **Invariant I1 (Cache-Stable Determinism)**: Transformations applied to historical messages (beyond the live window) are 100% deterministic and position-independent. This preserves byte-identical prefixes across conversation turns, keeping upstream provider KV/prompt caches warm.
- **Invariant I3 (Target Isolation)**: Headroom only inspects and rewrites `tool_result` payload text blocks (via `walkToolResultText`). It **never** mutates user prompt text, assistant text, thinking blocks, signatures, tool use inputs, or images.
- **Live Turns Window (`liveTurns`)**: Retains the trailing $N$ messages (default `2`) inline and unmodified. Only messages outside the live window (`FrozenPrefixIndex`) are eligible for chunking/demotion by CCR.

---

### Headroom Features & Functionality

#### 1. Content-Conditioned Retrieval (CCR)
- **Prefix Demotion**: Scans historical `tool_result` blocks in the frozen prefix.
- **SHA-256 Chunk Storage**: Large payloads exceeding `minChunkBytes` (default 512B) are SHA-256 hashed (`chunk_<hex[:12]>`) and stored in a thread-safe, in-memory LRU `CCRStore` bounded by `maxStoreMB` (default 50MB).
- **Stub Replacement**: Replaces the full payload in context with a compact retrieval stub:
  ```text
  [HEADROOM_CHUNK id="chunk_abc123456789" lines=42 preview="<first lines>"]
  ```
- **Dynamic Tool Injection**: Automatically injects a `headroom_retrieve` tool definition into the request's `tools` array.
- **Transparent Dynamic Hydration Loop**: When the model calls `headroom_retrieve`, the proxy intercepts the call, fetches the raw chunk from `CCRStore`, updates context, and resumes execution transparently without client interruption. Supported across **all upstreams**: Google Cloud Code, OpenRouter, Kimi, Claude Code (`api.anthropic.com`), and Custom Endpoints.

#### 2. SmartCrusher
- **Exact JSON Minification (`CompactJSON`)**:
  - Strips insignificant whitespace, indentation, and newlines via `json.Compact`.
  - Byte-for-byte exact: preserves key order and numeric literals without unmarshal/marshal round-trips, ensuring prompt cache stability and zero corruption of large 64-bit integer IDs.
- **Tabular Array Transformation (`tabular.go`)**:
  - Automatically identifies JSON arrays of homogeneous objects or uniform 2D arrays.
  - Converts verbose repeated JSON key structures into compact Markdown / TSV tables (headers + pipe-delimited rows) when savings exceed the threshold (`DefaultMinTabularSavings` = 30%).
  - Yields 40–70% token reductions on database queries, log dumps, and REST array outputs.

#### 3. CodeCompressor
- **Whitespace Pruning**: Trims trailing spaces, tabs, and carriage returns per line.
- **Blank Line Collapsing**: Collapses multi-line empty gaps down to a single blank line.
- **Comment Stripping**: Removes full-line comments (`//`, `#`, `/* ... */`) while strictly preserving code syntax, indentation, and structure.
- **Log Folding**: Deduplicates consecutive repeated log lines into compact indicators (`[... repeated N times ...]`).

#### 4. Output Shaper (Detailed Description)
The Output Shaper optimizes downstream model responses to minimize output token consumption and round-trip latency:

- **Verbosity Steering**:
  - Injects behavioral steering instructions into the request's `system` prompt (supports both string and structured system message blocks).
  - Uses `DefaultVerbosityPrompt` or custom `steeringText`.
  - Forces the model to emit terse, direct technical answers, eliminating conversational fluff, greetings, repetitive confirmation clauses, and hedging.
- **Effort Routing (Mechanical Thinking Budget Clamping)**:
  - Analyzes user prompt intent and tool continuation messages to classify task complexity.
  - Detects mechanical actions (file reads, direct edits, path lookups, git status, routine CLI operations).
  - Automatically clamps `thinking.budget_tokens` down to `mechanicalThinkingBudget` (e.g. 1024 tokens) on mechanical turns to avoid deep, expensive reasoning chains on deterministic operations.
- **Telemetry & Stats**: Records `originalThinking`, `clampedThinking`, and sets `effortClamped` for observability.

---

### Headroom Telemetry & Management
- Track input bytes saved, compression ratios, requests compressed, clamped thinking tokens, and CCR dynamic retrievals in the Web UI or via:
  - `GET /api/headroom/stats`: Real-time compression telemetry and hit rates.
  - `POST /api/headroom/config`: Dynamic configuration updates without restarting the proxy.

---

## Multi-Account Management & Rotation

### Automatic Discovery
With no configuration file, the proxy automatically loads the active `agy` login from `~/.gemini/antigravity-cli/antigravity-oauth-token`.

### Pool Configuration (`accounts.json`)
To manage multiple accounts, create `~/.config/antigravity-proxy/accounts.json` (or add accounts via `antigravity-proxy accounts add` / Web UI):

```json
{
  "activeIndex": 0,
  "settings": {},
  "accounts": [
    {
      "email": "personal@example.com",
      "source": "agy",
      "agyTokenPath": "/home/me/.gemini/antigravity-cli/antigravity-oauth-token"
    },
    {
      "email": "work@example.com",
      "source": "oauth",
      "refreshToken": "1//04...",
      "projectId": "work-cloudcode-project"
    }
  ]
}
```

### Selection Strategies (`ACCOUNT_STRATEGY`)
- `hybrid` *(Default)*: Scores available accounts using health score, token bucket rate, quota remaining, and least-recently-used timestamp. Skips invalid or cooling-down accounts.
- `sticky`: Uses current active account until it encounters a cooldown or rate limit, then rotates to another account.
- `round-robin`: Cycles sequentially through usable accounts for every request.

On `429` rate limits, cooldowns are scoped to the specific model and account so other models on that account remain usable. Stream responses that have already begun emitting data are never replayed to prevent output duplication.

---

## Claude Code Gateway & Multi-Account OAuth Pool

The proxy provides native support for official Anthropic Claude Code accounts, including an interactive OAuth 2.0 PKCE sign-in flow, multi-account token management, secure file storage, and a direct gateway to `api.anthropic.com`.

### Features & Capabilities

- **Direct Anthropic Skin Gateway**:
  - Automatically identifies incoming Claude Code OAuth tokens (e.g. `ant-oat-*`) or routes configured Claude Code accounts.
  - Reverse-proxies requests directly to `https://api.anthropic.com/v1/messages` with native SSE streaming and low latency.
  - Automatically handles `Authorization: Bearer` formatting and injects required `anthropic-beta` headers.
- **Native Multi-Account OAuth Login**:
  - Add official Anthropic accounts directly via the Web UI (Claude tab) or CLI:
    ```bash
    antigravity-proxy accounts add --provider claudecode
    ```
  - Automatically launches a local loopback listener on `localhost:54545` (or fallback ports) to capture OAuth callbacks.
  - **Manual Code Submission Fallback**: If the loopback redirect is blocked (e.g., remote server, headless session, or firewall), users can paste the redirect URL or authorization code directly. The proxy extracts authorization codes from full URLs, query parameters, and hash fragments (`/callback#code=...` or `/callback?code=...`).
- **Secure Token Storage & Lifecycle**:
  - Claude Code credentials are saved to `~/.config/antigravity-proxy/claudecode_accounts.json` with strict `0600` file permissions.
  - Session TTL cleanup prevents unbounded memory growth from abandoned or expired OAuth login sessions.
  - Multi-account rotation with configurable selection strategies (`hybrid`, `sticky`, `round-robin`).
- **Claude Code Model Aliasing & Discovery**:
  - Exposes official Anthropic Claude models (`claude-3-7-sonnet-20250219`, `claude-3-5-sonnet-20241022`, `claude-3-opus-20240229`, etc.) through the catalog and allows custom model aliasing.
  - Includes multi-hop loop protection to prevent recursive model mapping chains.

---

## OpenRouter Gateway & Model Discovery

The OpenRouter Gateway allows querying OpenRouter's Anthropic-compatible messages API (`https://openrouter.ai/api/v1/messages`) transparently without changing client configurations.

### Features
1. **Anthropic Skin Proxying**: Passes requests directly through `httputil.ReverseProxy`, adding `Authorization: Bearer <key>` and `x-api-key: <key>`.
2. **Dynamic Discovery**: The Web UI includes a model discovery modal that queries `GET /v1/models` from OpenRouter with search, context window inspection, and provider filters.
3. **Allowlist & Aliasing**: Add models from OpenRouter to your allowlist and define convenient aliases (e.g. `claude-3-7-openrouter` -> `anthropic/claude-3.7-sonnet`).
4. **Metadata-Driven Token Limits**: Automatically calculates maximum output tokens based on provider metadata.
5. **Harness-Gate Spoofing**: Some free models (e.g. `thinkingmachines/inkling:free`) reject unattributed requests with `403 permission_error ... only available on agentic harnesses`. When OpenRouter returns that error, the proxy retries the same request once with `HTTP-Referer`, `X-OpenRouter-Title`, and `X-OpenRouter-Categories` attribution headers (defaults: `https://claude.ai/code`, `Claude Code`, `cli-agent`). Override via the `openrouter.appSpoof` block in `config.json` or Web UI.

---

## Transparent Forwarding to Custom Endpoints

Route non-Google models to external providers or local mock endpoints without payload conversion:

1. Configure endpoint in Web UI (Settings -> Models -> Custom Endpoints) or `config.json`.
2. Map model name (e.g. `my-custom-model`) to target URL (`https://api.example.com/v1/messages`) and optional API key.
3. Requests for `my-custom-model` stream directly to the target URL with SSE flushing and injected `x-api-key`.

---

## Selectable Models & Server-Side Mapping

`GET /v1/models` returns a unified catalog combining Google Cloud Code models and active allowlisted OpenRouter models.

| Selection ID | Display Name | Provider | Context Window | Max Output |
|---|---|---|---:|---:|
| `gemini-3.7-flash-high` | Gemini 3.7 Flash (High) | Google | 1,048,576 | 65,536 |
| `gemini-3.7-flash-medium` | Gemini 3.7 Flash (Medium) | Google | 1,048,576 | 65,536 |
| `gemini-3.7-flash-low` | Gemini 3.7 Flash (Low) | Google | 1,048,576 | 65,536 |
| `gemini-3.5-flash-low` | Gemini 3.5 Flash (Medium) | Google | 1,048,576 | 65,536 |
| `gemini-3-flash-agent` | Gemini 3.5 Flash (High) | Google | 1,048,576 | 65,536 |
| `gemini-3.5-flash-extra-low` | Gemini 3.5 Flash (Low) | Google | 1,048,576 | 65,536 |
| `gemini-3.1-pro-low` | Gemini 3.1 Pro (Low) | Google | 1,048,576 | 65,535 |
| `gemini-pro-agent` | Gemini 3.1 Pro (High) | Google | 1,048,576 | 65,535 |
| `claude-sonnet-4-6` | Claude Sonnet 4.6 (Thinking) | Anthropic | 250,000 | 64,000 |
| `claude-opus-4-6-thinking` | Claude Opus 4.6 (Thinking) | Anthropic | 250,000 | 64,000 |
| `gpt-oss-120b-medium` | GPT-OSS 120B (Medium) | OpenAI | 131,072 | 32,768 |

*Note: Allowlisted OpenRouter models and aliases are dynamically appended to this catalog when OpenRouter is enabled.*

### Server-Side Model Mapping
Map incoming requested model names to internal models, OpenRouter models, or custom endpoints in Web UI under Model Mapping or in `config.json`:

```json
{
  "modelMapping": {
    "claude-3-5-sonnet-20241022": "claude-sonnet-4-6",
    "claude-3-opus-20240229": "claude-opus-4-6-thinking",
    "claude-3-7-sonnet-latest": "claude-3-7-openrouter"
  }
}
```

The router supports chained mappings with automatic recursion and loop protection (up to 5 hops).

---

## Client Integrations

### Claude Code Integration

Configure environment variables or use the Web UI (Claude CLI tab) for one-click setup:

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080/anthropic
export ANTHROPIC_API_KEY='your-local-secret'
export ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-6
export ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-4-6-thinking
export ANTHROPIC_DEFAULT_HAIKU_MODEL=gemini-3.5-flash-low
export CLAUDE_CODE_SUBAGENT_MODEL=gemini-3.7-flash-high

# Optional: Enable dynamic model discovery and fast mode org check bypass
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=true
export CLAUDE_CODE_SKIP_FAST_MODE_ORG_CHECK=true

claude --bare -p --model sonnet 'Reply with OK'
```

### Hermes Agent Integration

Add custom provider to `~/.hermes/config.yaml`:

```yaml
custom_providers:
  - name: antigravity-proxy
    provider: anthropic
    api_mode: anthropic_messages
    base_url: http://127.0.0.1:8080/anthropic
    api_key: your-local-secret
    models:
      gemini-3.7-flash-high:
        context_length: 1048576
      gemini-3.5-flash-low:
        context_length: 1048576
      claude-sonnet-4-6:
        context_length: 250000
      claude-opus-4-6-thinking:
        context_length: 250000
```

---

## HTTP API & Management Routes

All `/v1/*` routes accept authentication via `x-api-key` or `Authorization: Bearer <key>`. Routes are mirrored under `/anthropic` prefix.

| Endpoint | Method | Purpose |
|---|---|---|
| `/health` | `GET` | Proxy status, account pool health summary (public) |
| `/account-limits` | `GET` | Real-time rate limits, account quota details, and model configurations (`?format=table` supported) |
| `/v1/models` | `GET` | Unified catalog of available models, context lengths, thinking support |
| `/v1/usage` | `GET` | Quota usage per model and grouped quota reset windows |
| `/v1/messages` | `POST` | Anthropic-compliant messages API (supports streaming SSE) |
| `/v1/messages/count_tokens` | `POST` | Token counting endpoint (`501 Not Implemented`) |
| `/api/accounts` | `GET` | List account pool with tiers, quotas, and health scores |
| `/api/accounts/{email}` | `DELETE`/`PATCH` | Remove account or update quota thresholds |
| `/api/accounts/{email}/refresh` | `POST` | Clear token caches and re-verify upstream access |
| `/api/accounts/{email}/toggle` | `POST` | Enable or disable account |
| `/api/accounts/reload` | `POST` | Reload accounts from disk |
| `/api/accounts/export` | `GET` | Export accounts payload |
| `/api/accounts/import` | `POST` | Import accounts payload |
| `/api/config` | `GET`/`POST` | Read (redacted) or save proxy runtime configuration |
| `/api/config/password` | `POST` | Set or update Web UI protection password |
| `/api/settings` | `GET` | Retrieve combined runtime configuration and active account pool stats |
| `/api/models/config` | `POST` | Save server-side model mappings and custom endpoints |
| `/api/strategy/health` | `GET` | Inspect account health scores and selection weights |
| `/api/stats/history` | `GET` | Historical request and token volume statistics |
| `/api/logs` | `GET` | Retrieve recent formatted log entries |
| `/api/logs/stream` | `GET` | SSE stream for real-time console logs |
| `/api/openrouter/config` | `GET`/`POST` | Read or save OpenRouter configuration and model allowlist |
| `/api/openrouter/models/fetch` | `POST` | Query live OpenRouter catalog and refresh in-memory cache |
| `/api/openrouter/models/cached` | `GET` | Retrieve cached OpenRouter model catalog |
| `/api/claude/config` | `GET`/`POST` | Read or update `~/.claude/settings.json` environment |
| `/api/claude/config/restore` | `POST` | Remove proxy environment variables from `~/.claude/settings.json` |
| `/api/claude/mode` | `GET`/`POST` | Toggle Claude CLI mode between `proxy` and `paid` |
| `/api/claude/presets` | `GET`/`POST` | Manage Claude CLI environment presets |
| `/api/claude/presets/{name}` | `DELETE` | Delete custom Claude CLI preset |
| `/api/server/presets` | `GET`/`POST` | Manage server configuration presets (Conservative, Balanced, High Throughput) |
| `/api/server/presets/{name}` | `PATCH`/`DELETE` | Update or delete server preset |
| `/api/auth/url` | `GET` | Generate Google OAuth PKCE sign-in URL |
| `/api/auth/complete` | `POST` | Complete Google OAuth callback with authorization code |
| `/api/claudecode/auth/url` | `GET` | Generate Claude Code OAuth sign-in URL and start callback listener |
| `/api/claudecode/auth/complete` | `POST` | Complete Claude Code OAuth flow with code or full callback URL |
| `/api/claudecode/auth/cancel` | `POST` | Cancel in-progress Claude Code OAuth login session |
| `/api/claudecode/accounts` | `GET` | List configured Claude Code accounts and status |
| `/api/claudecode/accounts/{email}` | `DELETE` | Remove a Claude Code account from pool |
| `/api/claudecode/status` | `GET` | Retrieve Claude Code gateway and pool health status |
| `/api/headroom/stats` | `GET` | Real-time Headroom compression and CCR dynamic retrieval statistics |
| `/api/headroom/config` | `GET`/`POST` | Read or dynamically update Headroom compression and Output Shaper settings |

---

## Systemd Service Installation (Linux)

Create systemd service and environment file:

```sh
install -m 0644 antigravity-go-proxy.service /etc/systemd/system/
install -m 0600 antigravity-go-proxy.env.example /etc/antigravity-go-proxy.env
editor /etc/antigravity-go-proxy.env
systemctl daemon-reload
systemctl enable --now antigravity-go-proxy.service
```

Check status and logs:
```sh
systemctl status antigravity-go-proxy.service
journalctl -u antigravity-go-proxy.service -f
```

---

## Fingerprint Re-verification Gate

To verify TLS ClientHello matches `agy`:

1. Start packet capture:
```sh
tcpdump -i any -w /tmp/antigravity-go.pcap 'host daily-cloudcode-pa.googleapis.com and tcp port 443'
```

2. Trigger request:
```sh
curl -sS -H "x-api-key: $ANTIGRAVITY_PROXY_API_KEY" http://127.0.0.1:8080/v1/usage >/dev/null
```

3. Verify JA4 with `tshark`:
```sh
tshark -r /tmp/antigravity-go.pcap \
  -Y 'tls.handshake.type==1 && tls.handshake.extensions_server_name contains "cloudcode"' \
  -T fields \
  -e tls.handshake.extensions_server_name \
  -e tls.handshake.extensions_alpn_str \
  -e tls.handshake.ja4
```

Expected row output:
```text
daily-cloudcode-pa.googleapis.com    t13d131100_f57a46bbacb6_f50d94e863eb
```

---

## Troubleshooting

- **`401 Unauthorized`**: Check that local `x-api-key` or Bearer token matches `ANTIGRAVITY_PROXY_API_KEY` or `config.json`.
- **`400 Bad Request`**: Verify requested model ID is valid in `/v1/models`, mapped via `modelMapping`, allowlisted under `openrouter`, or configured in `customEndpoints`.
- **`429 Rate Limit Exceeded`**: Handled via automatic account rotation and model cooldowns. Use `/v1/usage`, `/account-limits`, or Web UI to inspect cooldown status.
- **`403 Verification Required`**: Account requires manual verification or re-authentication.
- **JA4 Mismatch**: Ensure proxy is built with standard Go toolchain and no custom TLS configurations are applied.
