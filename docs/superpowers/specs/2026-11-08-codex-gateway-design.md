# ChatGPT Codex gateway

## Goal

Serve Anthropic `/v1/messages` requests (Claude Code and any Anthropic client) from a pool of ChatGPT-subscription accounts through the Codex backend (`https://chatgpt.com/backend-api/codex/responses`). The gateway mirrors the Claude Code gateway: config section, account pool, OAuth login in the WebUI, gateway precedence entry, `/v1/models` entries, management API, WebUI tab and usage windows.

Reference implementation: CLIProxyAPI (MIT), `router-for-me/CLIProxyAPI` at `681ce41`. The wire identity copies it on purpose (decided 2026-11-08): Chrome uTLS ClientHello, `codex-tui` User-Agent and Originator. ADR-0005 records the exception to the "empty `tls.Config`" rule.

## Decisions

| Topic | Decision |
|---|---|
| Wire identity | CLIProxyAPI parity: uTLS `HelloChrome_Auto`, `User-Agent: codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)`, `Originator: codex-tui`. Overridable per config (`codex.identity`). Not pinned by `client_drift.py`. |
| Accounts | WebUI OAuth login only (PKCE). No import of `~/.codex/auth.json`: refresh tokens rotate and are single-use, so sharing one with the codex CLI signs one of them out. |
| Store | One store: `config.json` `codex.accounts[]`. No separate pool file. Quota and cooldown state are in memory and survive a pool rebuild, not a restart. |
| Pool ownership | The pool lives on `Server` (not a package singleton), so the token refresher is always installed. |
| Translation | Request builder ported from CLIProxyAPI into `internal/codex`. Response translation is Zen's Responses→Anthropic stream state machine, moved to a shared package `internal/responses` and extended. |
| Thinking across turns | Codex `reasoning.encrypted_content` travels in the Anthropic thinking block `signature` and comes back as a `reasoning` input item. No separate replay cache. |
| Extras in scope | Claude `web_search_*` server tool → Codex `web_search`, results → `server_tool_use` + `web_search_tool_result`. 5h/weekly usage windows from `x-codex-*` headers. |
| Out of scope | Websockets, SIWC, compaction, image generation, multi-agent rewrites, zstd request compression, device-code login, client-drift pinning, cache bump. |

## Package contracts

### `internal/responses` (new, shared by Zen and Codex)

Moved from `internal/zen/responsesstream.go` and the response half of `internal/zen/responseswire.go`. Zen keeps its request builder, gate and harness code and calls this package.

```go
package responses

// ErrStreamTruncated moves here from internal/zen; every reference is updated.
var ErrStreamTruncated error

type Options struct {
	Model      string              // echoed as message.model
	ToolNames  map[string]string   // upstream tool name -> client tool name; nil = identity
	Drop       map[string]bool     // upstream tool names whose calls are dropped (Zen gate tools); nil = none
	MessageID  func(id any) string // nil -> "msg_" + id, the current Zen behaviour
	ErrorLabel string              // prefix of stream error messages: "Zen", "Codex"
}

func Stream(r io.Reader, w io.Writer, opts Options) error      // Responses SSE -> Anthropic SSE
func Aggregate(r io.Reader) (map[string]any, error)              // Responses SSE -> terminal response object
func ToAnthropic(resp map[string]any, opts Options) map[string]any // response object -> Anthropic message
func Usage(u map[string]any) map[string]any                      // Responses usage -> Anthropic usage
```

New behaviour, on every wire (Zen tests are the regression gate; only tests whose expectation follows from these items change):

1. A reasoning item's `encrypted_content` becomes the thinking block `signature`. Stream: `signature_delta` right before that thinking block's `content_block_stop`. A reasoning item with `encrypted_content` and no summary text yields a thinking block with `"thinking":""` and the signature. Non-stream: the same in the block object. A reasoning item without `encrypted_content` keeps today's output.
2. Tool-use blocks are serialized: one open at a time. Argument fragments for a call whose block is not open are buffered and emitted when it opens, never dropped.
3. `web_search_call` output items (on `response.output_item.done`, and in the non-stream output) become a `server_tool_use` block `{"id","name":"web_search","input":{"query":…}}` followed by a `web_search_tool_result` block `{"tool_use_id","content":[{"type":"web_search_result","title","url","page_age":null}]}`. Query from `action.query`; results from `action.sources`.
4. Usage: `input_tokens = input_tokens - cached - cache_write` (floor 0), `cache_read_input_tokens = input_tokens_details.cached_tokens`, `cache_creation_input_tokens = input_tokens_details.cache_write_tokens` (fallback `cache_creation_tokens`, else 0).
5. `response.done` is treated as `response.completed`.

### `internal/codex` (new)

```go
const (
	DefaultBaseURL       = "https://chatgpt.com/backend-api/codex"
	DefaultUserAgent     = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	DefaultOriginator    = "codex-tui"
	DefaultClientVersion = "0.154.0" // models?client_version=
	RefreshLead          = 24 * time.Hour
)

type AccountConfig struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	Token        string `json:"token"`            // OAuth access token
	RefreshToken string `json:"refreshToken"`
	IDToken      string `json:"idToken,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"` // RFC3339
	Email        string `json:"email,omitempty"`
	AccountID    string `json:"accountId"`        // chatgpt_account_id claim
	PlanType     string `json:"planType,omitempty"`
	Priority     int    `json:"priority"`
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source,omitempty"` // "oauth"
}

type ModelConfig struct {
	ID              string   `json:"id"`
	Alias           string   `json:"alias,omitempty"`   // comma list
	Aliases         []string `json:"aliases,omitempty"`
	DisplayName     string   `json:"displayName,omitempty"`
	ContextLength   int      `json:"contextLength,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`       // supported reasoning levels, ascending
	DefaultEffort   string   `json:"defaultEffort,omitempty"`
	Enabled         bool     `json:"enabled"`
}

type IdentityConfig struct {
	UserAgent  string `json:"userAgent,omitempty"`
	Originator string `json:"originator,omitempty"`
}
func (IdentityConfig) Validate() error // rejects control characters

type Config struct {
	Enabled   bool            `json:"enabled"`
	BaseURL   string          `json:"baseUrl"`
	Accounts  []AccountConfig `json:"accounts"`
	Allowlist []ModelConfig   `json:"allowlist"`
	Identity  IdentityConfig  `json:"identity,omitzero"`
}

func DefaultAllowlist() []ModelConfig
func NormalizeBaseURL(string) string
func ResolveModel(allowlist []ModelConfig, requested string) (ModelConfig, bool) // exact id, alias, then case-insensitive
```

Default allowlist (CLIProxyAPI `codex_client_models.json`, entries with `visibility:"list"`): `gpt-6.1-sol` (500000 ctx), `gpt-6.1-terra` (500000), `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna` (272000 each), `gpt-5.5` (500000). Max output 128000. Efforts and default effort per entry from that file.

Pool:

```go
type TokenSet struct {
	AccessToken, RefreshToken, IDToken string
	ExpiresAt                          time.Time
	Email, AccountID, PlanType         string
}
type TokenRefresher func(ctx context.Context, refreshToken string) (TokenSet, error)
type TokenPersister func(accountID string, ts TokenSet) error // writes config.json; called before the new token is used

func NewAccountPool(cfgs []AccountConfig) *AccountPool
func (p *AccountPool) SetTokenRefresher(TokenRefresher)
func (p *AccountPool) SetTokenPersister(TokenPersister)
func (p *AccountPool) Inherit(old *AccountPool)                      // carries quota, cooldown, counters and sticky sessions for surviving ids
func (p *AccountPool) SelectAccount(sessionKey string, excluded map[string]bool) (Account, error) // sticky (LRU, 10,000 sessions), then priority, in-flight, failures
func (p *AccountPool) EnsureFresh(ctx context.Context, id string) (Account, error) // refresh when < RefreshLead left; single-flight per account
func (p *AccountPool) ForceRefresh(ctx context.Context, id string) (Account, error) // management API
func (p *AccountPool) RefreshRejected(ctx context.Context, id, rejected string) (Account, error) // after a 401; no refresh once the token changed
func (p *AccountPool) Acquire(id string)
func (p *AccountPool) Release(id string)
func (p *AccountPool) RecordSuccess(id string)
func (p *AccountPool) RecordFailure(id string, cooldown time.Duration, reason string) // "" counts; 3rd in a row → consecutive_failures
func (p *AccountPool) RecordCooldown(id string, until time.Time, reason string) // usage_limit_reached, Retry-After
func (p *AccountPool) CooldownEnd() (until time.Time, ok bool)        // earliest reset when every account is cooling down
func (p *AccountPool) UpdateQuota(id string, q Quota)
func (p *AccountPool) GetAccount(id string) (Account, bool)
func (p *AccountPool) Snapshots() []AccountSnapshot
var ErrNoAvailableAccounts, ErrAccountNotFound error
```

A failed refresh whose error says the grant is gone (`invalid_grant`, `refresh_token_reused`, `refresh_token_expired`, `refresh_token_invalidated`, `token_revoked`, HTTP 401) marks the account `needs_login` and excludes it from selection until it is logged in again.

`AccountSnapshot` JSON: `id, name, email, accountId, planType, enabled, priority, source, status ("healthy"|"cooldown"|"needs_login"|"disabled"), cooldownUntil, cooldownReason, expiresAt, inFlight, totalRequests, consecutiveFailures, lastError, quota`.

Quota:

```go
type Window struct {
	UsedPercent   float64   `json:"usedPercent"`
	WindowMinutes int       `json:"windowMinutes"`
	ResetAt       time.Time `json:"resetAt"`
}
type Quota struct {
	Primary    *Window   `json:"primary,omitempty"`
	Secondary  *Window   `json:"secondary,omitempty"`
	PlanType   string    `json:"planType,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}
func ParseQuota(h http.Header, now time.Time) (Quota, bool)
func ParseUsageLimit(body []byte, now time.Time) (until time.Time, ok bool)
```

Headers read: `x-codex-{primary,secondary}-used-percent`, `-window-minutes`, `-reset-after-seconds` (relative), `-reset-at` (unix seconds), `x-codex-plan-type`. A window needs used-percent, window-minutes and one reset field. `ParseUsageLimit` matches `error.type == "usage_limit_reached"` (also top-level `type`) and reads `resets_at` (unix seconds, used when in the future), else `resets_in_seconds`.

Request builder (port of CLIProxyAPI `translator/codex/claude/codex_claude_request.go` and the executor body rules):

```go
type BuildOptions struct {
	Model          string      // upstream model id
	Entry          ModelConfig // effort clamping
	PromptCacheKey string
}
func BuildRequest(anthropicBody []byte, opts BuildOptions) (body []byte, toolNames map[string]string /*upstream->client*/, err error)
func PromptCacheKey(model, sessionID, agentID string) string // UUIDv5(OID, "antigravity-proxy:codex:claude-code\x00"+model+"\x00"+sessionID+":agent:"+agentID); "" when sessionID == ""
```

Body: `model`, `instructions:""`, `input`, `stream:true`, `store:false`, `include:["reasoning.encrypted_content"]` (+ `"web_search_call.action.sources"` with a web-search tool), `reasoning:{effort, summary?}`, `parallel_tool_calls` (only with tools; false when `tool_choice.disable_parallel_tool_use`), `tools`, `tool_choice`, `max_output_tokens` (integer below 16 raised to 16), `text.format` from `output_config.format` json_schema, `service_tier:"priority"` for `service_tier` fast/priority or `speed:"fast"`, `prompt_cache_key`. Never sent: `temperature`, `top_p`, `top_k`, `stop_sequences`, `metadata`, `previous_response_id`.

- `system` (string or text blocks) → one `{"type":"message","role":"developer","content":[input_text…]}` item; a block starting `x-anthropic-billing-header:` is skipped.
- User text → `input_text`; assistant text → `output_text`; images → `input_image` data URL (media type fallback `application/octet-stream`; `url` sources only http/https); base64 PDF `document` → `input_file` with `file_data` data URL and `filename:"document.pdf"`.
- `tool_use` → `function_call {call_id, name, arguments}`, keeping block order (flush the open message before each call). `tool_result` → `function_call_output {call_id, output}`; output is an array of `input_text`/`input_image` when the result has images, else a string; `is_error` prefixes `"Error: "` (Zen's behaviour).
- Assistant `thinking` with a non-empty `signature` → `{"type":"reasoning","summary":[],"content":null,"encrypted_content":<signature>}`; thinking text is not sent. A signature that is not a Codex envelope (an Anthropic or Gemini signature from another backend) is dropped: accept only signatures that start with `gAAAA` (Fernet). `redacted_thinking`, `server_tool_use`, `web_search_tool_result` blocks are dropped.
- Tool names over 64 bytes and `call_id`s over 64 bytes are shortened (CLIProxyAPI rules: `mcp__` readable form, else truncate; `_1`, `_2` for uniqueness; call ids `id[:47]+"_"+hex(sha256(id)[:8])`); `toolNames` maps back.
- Function tool schemas: `normalizeToolParameters` + `stripDialectKeywordsFromSchema` + `NormalizeCodexToolSchemas` (drop unsupported `\p{…}` patterns, out-of-int64 numeric bounds, constant `oneOf`/`anyOf` with ≥8 branches → `enum`). `strict:false`.
- Tools `web_search_20250305` / `web_search_20260209` → `{"type":"web_search"}` with `filters.allowed_domains` and `user_location`. Other tools without `input_schema` → function tool with an empty object schema.
- `tool_choice`: auto/none map through, `any` → `"required"`, `tool` → `{"type":"function","name":…}` (or `{"type":"web_search"}`).
- Effort: `output_config.effort` (low, medium, high, xhigh, max) passes through; else `thinking.budget_tokens` maps by CLIProxyAPI thresholds (≤512 minimal→low on models without minimal, ≤1024 low, ≤8192 medium, ≤24576 high, else xhigh); `thinking.type:"disabled"` → omit `reasoning.effort`; nothing → the entry's `DefaultEffort`, else `medium`. The level is clamped to the highest entry `Efforts` level at or below it (CONTEXT.md, Effort Level).
- `reasoning.summary:"auto"` when thinking is enabled or adaptive and `thinking.display` is not `"omitted"`.

Client and transport:

```go
type SendRequest struct {
	Token, AccountID string
	SessionID        string // Session-Id header (the prompt cache key)
	Model            string // X-Codex-Routing-Hint
	ServiceTier      string
	Body             []byte
	Identity         IdentityConfig
}
func NewClient(baseURL string, rt http.RoundTripper) *Client // rt nil -> transport.Codex()
func (c *Client) Send(ctx context.Context, r SendRequest) (*http.Response, error) // raw upstream response, status untouched
func (c *Client) FetchModels(ctx context.Context, token, accountID string, id IdentityConfig) ([]DiscoveredModel, http.Header, error)
```

`Send` headers: `Content-Type: application/json`, `Authorization: Bearer <token>`, `Accept: text/event-stream`, `User-Agent`, `Originator`, `Chatgpt-Account-Id`, `Session-Id` (when set), `X-Codex-Routing-Hint: model=<model>[;tier=<tier>]`. POST `{base}/responses`. `FetchModels`: GET `{base}/models?client_version=<DefaultClientVersion>` with `Accept: application/json` and the same identity headers.

`internal/transport/codex.go`: `func Codex() http.RoundTripper` — dials TLS with uTLS `HelloChrome_Auto`, honours `HTTPS_PROXY`/`ALL_PROXY` through `internal/proxydial`, pools HTTP/2 connections when ALPN selects `h2` and falls back to HTTP/1.1 otherwise. Response-header timeout as `transport.Upstream()`.

### `internal/auth/codex_oauth.go` (new)

```go
const (
	CodexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	CodexTokenURL     = "https://auth.openai.com/oauth/token"
	CodexRedirectURI  = "http://localhost:1455/auth/callback"
	CodexScopes       = "openid email profile offline_access"
)
type CodexAccountResult struct {
	AccessToken, RefreshToken, IDToken string
	ExpiresAt                          time.Time
	Email, AccountID, PlanType         string
}
func NewCodexOAuthManager() *CodexOAuthManager
func (m *CodexOAuthManager) SetEndpoints(authorizeURL, tokenURL string) // tests
func (m *CodexOAuthManager) StartAuthSession(mode string) (*CodexAuthSession, error) // "loopback" (default) | "manual"
func (m *CodexOAuthManager) GetSession(id string) (CodexAuthSessionSnapshot, bool)
func (m *CodexOAuthManager) CompleteManualAuth(sessionID, input string) (*CodexAccountResult, error) // full redirect URL, or bare code
func (m *CodexOAuthManager) CancelSession(id string) error
func (m *CodexOAuthManager) PruneExpiredSessions(maxAge time.Duration)
func (m *CodexOAuthManager) OnComplete(func(*CodexAccountResult)) // loopback success hook used by the API to register the account
func (m *CodexOAuthManager) RefreshToken(ctx context.Context, refreshToken string) (*CodexAccountResult, error)
func ParseCodexIDToken(idToken string) (email, accountID, planType string)
```

Authorize params: `client_id`, `response_type=code`, `redirect_uri`, `scope`, `state` (16 random bytes hex), `code_challenge` (S256 of a 96-byte base64url verifier), `code_challenge_method=S256`, `prompt=login`, `id_token_add_organizations=true`, `codex_cli_simplified_flow=true`. Loopback mode listens on `127.0.0.1:1455` path `/auth/callback`; a busy port falls back to manual mode. Manual mode: the user pastes the URL the browser was redirected to (it fails to load on a remote machine); state is checked in both modes. Token exchange: form POST `grant_type=authorization_code, client_id, code, redirect_uri, code_verifier`. Refresh: form POST `client_id, grant_type=refresh_token, refresh_token, scope="openid profile email"`, no redirect following; requires `access_token`, `refresh_token`, `expires_in > 0`. Expiry is the access token JWT `exp` when present, else now + `expires_in`. Claims from the id_token `https://api.openai.com/auth`: `chatgpt_account_id`, `chatgpt_plan_type` (default `free`); top-level `email`. Session snapshot JSON: `id, status ("pending"|"completed"|"failed"|"expired"|"cancelled"), mode, auth_url, error, email, created_at`. Sessions expire after 5 minutes. HTTP client: `transport.Upstream()`.

## API surface (`internal/api`)

- Config root field `Codex codex.Config` (JSON `codex`), defaults `{Enabled:false, BaseURL:DefaultBaseURL, Accounts:[], Allowlist:DefaultAllowlist()}`, `Save` merge preserving accounts, `GetPublicConfig` redaction (`token` → `hasToken`+`maskedToken`, `refreshToken` → `hasRefreshToken`, `idToken` removed).
- Gateway id `codex` (`config.GatewayCodex`), default order Kimi, Zen, ClaudeCode, Codex, OpenRouter, Custom, CloudCode. `matchCodexGateway` / `forwardCodexGateway` in the `gateways` table of `dispatch.go`; `appendCodexDiscovery` in `discovery.go` (`owned_by:"openai"`). Codex has no count endpoint, so `count_tokens` estimates its models.
- `forwardToCodex`: session id via `ccExtractSessionID`, agent from `X-Claude-Code-Agent-Id` (default `main`), prompt cache key, up to 3 attempts. 401 → `RefreshRejected` and one retry, else `RecordFailure(unauthorized)` and exclude. 429 with `usage_limit_reached` → `RecordCooldown(resets)`, other 429 → `Retry-After` or 60 s; exclude and try the next account; when none is left, return the last 429 as an Anthropic `rate_limit_error`. When every account is cooling down before any attempt, answer 429 `rate_limit_error` with `Retry-After` from `CooldownEnd`. 5xx → `RecordFailure(server_error)`, transport → `RecordFailure("")`. A 2xx stream is held back by `codex.Peek` (up to `PeekLimit`, 1 MiB) until its first output event; a failure before it (`response.failed`, `response.error`, `error`) is handled like the status `codex.FailureStatus` gives it, so a spent window fails over. A 400 `invalid_encrypted_content` is resent once on the same account, rebuilt with `BuildOptions.DropReasoning`. 2xx → `ParseQuota` → `UpdateQuota`, then `responses.Stream` (streaming client) or `Aggregate` + `ToAnthropic`. Upstream 4xx bodies become Anthropic error envelopes (400 → `invalid_request_error`, 401/403 → `authentication_error`/`permission_error`, 404 → `not_found_error`, 413 → `request_too_large`).
- Routes:

| Method | Path | Body / query | Response |
|---|---|---|---|
| GET | `/api/codex/config` | | public `codex` section |
| POST | `/api/codex/config` | `{enabled, baseUrl, allowlist, identity}` (accounts ignored) | `{status:"ok"}` |
| GET | `/api/codex/accounts` | | `{status:"ok", accounts:[AccountSnapshot]}` |
| POST | `/api/codex/accounts` | `{id, enabled?, priority?, name?}` | `{status:"ok"}` |
| DELETE | `/api/codex/accounts/{id}` | | `{status:"ok"}` |
| POST | `/api/codex/accounts/{id}/refresh` | | `{status:"ok", expiresAt}` |
| POST | `/api/codex/accounts/{id}/test` | | `{status:"ok", models:n}` or error |
| POST | `/api/codex/auth/start` | `{mode}` | `{status:"ok", session_id, auth_url, mode}` |
| GET | `/api/codex/auth/status?session_id=` | | `{status:"ok", session:{…}}` |
| POST | `/api/codex/auth/complete` | `{session_id, redirect_url}` | `{status:"ok", account:{id,email,planType}}` |
| POST | `/api/codex/auth/cancel` | `{session_id}` | `{status:"ok"}` |
| POST | `/api/codex/models/fetch` | | `{status:"ok", models:[{id,displayName,contextLength,efforts,defaultEffort}]}` |

  Errors: `{status:"error", error:"…"}` with a 4xx/5xx code, as the Claude Code routes do.
- Login registers the account: matched by `accountId`, else new id `codex-<email>` (`codex-<accountId>` without email; `-<accountId[:8]>` appended on an id collision), `source:"oauth"`, `enabled:true`, `priority:1`, and turns the gateway on.
- `/v1/usage` adds windows `{label:"Codex 5h (<name>)"|"Codex weekly (<name>)", remaining_fraction, used_percent, reset_at, model_ids, provider:"codex", account_id}`; the label comes from `windowMinutes` (300 → 5h, 10080 → weekly, else `<n>m`).

## WebUI

Codex accounts tab (list, enable toggle, priority, refresh, test, delete, 5h/weekly bars, cooldown and needs-login state), "ChatGPT Codex" add-account provider (loopback popup with status polling, always-visible paste-URL fallback), Codex Gateway settings section (enabled, base URL, identity overrides), Codex allowlist editor with discovery and default import, route diagram state, gateway label `gatewayCodex`, en/pt strings.

## Docs

README Codex section and config example, CONTEXT.md terms, ADR-0005 (Chrome uTLS exception, identity headers, `store:false`, encrypted reasoning in `signature`), AGENTS.md TLS-rule paragraph.

## Verification

`go build ./...`; `go test ./internal/responses/... ./internal/zen/... ./internal/codex/... ./internal/auth/... ./internal/api/... ./internal/config/...`; `node --test internal/webui/tests/*.test.mjs`; live: WebUI login with a ChatGPT account and one streamed and one non-streamed `/v1/messages` request on a Codex model through the running proxy.
