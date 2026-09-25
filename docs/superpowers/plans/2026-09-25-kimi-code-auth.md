# Kimi Code OAuth login (device flow, global kimi.ai only) for the Kimi gateway

## Context

The Kimi gateway (`internal/kimi`, config `kimi.*`) currently forwards `/v1/messages` to
`https://api.moonshot.ai/anthropic` using a manually entered API key. This change adds the
Kimi Code subscription login that oh-my-pi runs for its `kimi-code` provider: an RFC 8628
**device authorization flow**, pinned to the **global (overseas) deployment only**:
`https://auth.kimi.ai` for OAuth and `https://api.kimi.ai/coding/v1` for the API. There is
no mainland-China (`kimi.com`) support, no region concept, no region selector, and no
`KIMI_CODE_OAUTH_HOST` env override. The user is not in mainland China.
The finished feature stores one OAuth credential in config and uses it (Bearer plus KimiCLI
fingerprint headers) for gateway forwarding, cache-bump replays and model discovery.
Structural reference is the existing Claude Code OAuth code: manager in `internal/auth`,
handlers in `internal/api`, config redaction and merge in `internal/config`, UI in
`internal/webui`.

Decisions already made by the user (implementer must not change them): **single credential
slot** (`kimi.oauth` block, no accounts list). **The OAuth credential wins over `apiKey`**
when present. **Send KimiCLI fingerprint headers** on OAuth-flow requests and on
OAuth-authenticated upstream requests. **Global host only.**

### Wire facts

Sources: MoonshotAI/kimi-code `packages/oauth/src/{oauth.ts,constants.ts,region.ts,identity.ts,managed-userinfo.ts}`,
oh-my-pi `packages/ai/src/registry/oauth/kimi.ts` and `packages/catalog/src/compat/rules/auth/kimi-code.kdl`,
the Kimi Code docs overview page, and live probes run on 2026-09-25.

- Client ID `17e5f671-d194-4dfb-9706-5516cb48c098`. kimi-code shares it across regions.
  **Live-probed:** `auth.kimi.ai` accepts it. RFC 8628 needs no PKCE and no state, so do not add them.
- `POST https://auth.kimi.ai/api/oauth/device_authorization` with form `client_id` returns
  HTTP 200 with JSON `{device_code, user_code, verification_uri, verification_uri_complete, expires_in, interval}`.
  **Live-probed response:** `verification_uri` is `https://www.kimi.ai/code/authorize_device`,
  `verification_uri_complete` is the same URL plus `?user_code=XXXX-XXXX`, `expires_in` is `1800`,
  and `interval` is `5`.
- Polling is `POST https://auth.kimi.ai/api/oauth/token` with form `{client_id, device_code, grant_type=urn:ietf:params:oauth:grant-type:device_code}`.
  A pending authorization returns **HTTP 400** with
  `{"error":"authorization_pending","error_description":"Authorization is pending"}` (live-probed).
  The other outcomes: 200 plus `access_token` means success. `slow_down` means increase the interval by 5s.
  `expired_token` means expired. `access_denied` means denied. HTTP ≥500 is a terminal error.
- Refresh is `POST https://auth.kimi.ai/api/oauth/token` with form `{client_id, grant_type=refresh_token, refresh_token}`.
  401, 403 or `error=invalid_grant` means the user must log in again.
  Transport errors and 429/500/502/503/504 are retried, 3 attempts total, with 1s then 2s backoff.
- Token response is `{access_token, refresh_token, expires_in, scope, token_type}`. Require
  non-empty `access_token`, non-empty `refresh_token`, and a finite `expires_in` > 0. This mirrors
  kimi-code `tokenFromResponse`, including its error strings.
- Userinfo is `GET https://api.kimi.ai/coding/v1/me` with Bearer. It returns snake_case
  `{user_id, nickname, email, ...}`. `user_id` is required; the other fields are optional.
- API endpoints: the docs list the overseas Anthropic-compatible base as `https://api.kimi.ai/coding/`,
  which yields `/coding/v1/messages`. Models are at `/coding/v1/models`. **Live-probed:** both
  return 401 without auth, not 404. The existing `kimi.NormalizeBaseURL` strips a trailing `/v1`,
  and callers append `/v1/messages`. `kimi.Client.FetchModels` appends `/v1/models`.
  So `https://api.kimi.ai/coding/v1` round-trips correctly with no URL-logic changes.
- Fingerprint headers (oh-my-pi `getKimiCommonHeaders`):
  - `User-Agent: KimiCLI/<version>`
  - `X-Msh-Platform: kimi_cli`
  - `X-Msh-Version: <version>`
  - `X-Msh-Device-Name: <hostname>`
  - `X-Msh-Device-Model: "<label> <kernel release> <node-arch>"`
  - `X-Msh-Os-Version: <uname version string>` (Node `os.version()`)
  - `X-Msh-Device-Id: <32-hex persistent id>`

  Each value is stripped to printable ASCII `\x20-\x7E` and trimmed. An empty value becomes `"unknown"`.

## Approach

Steps are ordered so the tree builds and existing tests pass after each one. Steps 1–3 do not
depend on each other. Step 4 depends on 1–3. Step 5 depends on 2–4. Step 6 depends on 5.
None of these steps touch TLS: every client here uses the default `net/http` transport,
following the AGENTS.md rule.

### 0. Replace the repo plan document

Overwrite `docs/superpowers/plans/2026-09-25-kimi-code-auth.md` with the full contents of this
plan, from the `# Kimi Code OAuth login …` heading to the end, verbatim. This discards the
previous region/mainland version.

### 1. KimiCLI identity headers — `internal/kimi/identity.go` (new)

No equivalent exists: `internal/ccidentity` is Claude-specific.

- `const CLIVersion = "1.0.0"`. This is the proxy's own client-version token. oh-my-pi sends its
  own package version in the same slot.
- `func IdentityHeaders(deviceID string) http.Header` returns exactly these 7 headers, set with `Set`:
  - `User-Agent: KimiCLI/` + `CLIVersion`
  - `X-Msh-Platform: kimi_cli`
  - `X-Msh-Version: ` + `CLIVersion`
  - `X-Msh-Device-Name: os.Hostname()`. On error, use `""`.
  - `X-Msh-Device-Model: "<label> <release> <arch>"`, joining non-empty parts with a single space.
    - label: `macOS` for GOOS `darwin`, `Windows` for `windows`, `Linux` for `linux`, otherwise the raw `runtime.GOOS`.
    - arch: `runtime.GOARCH` mapped to Node names: `amd64`→`x64`, `386`→`ia32`, anything else unchanged.
  - `X-Msh-Os-Version: <version>`
  - `X-Msh-Device-Id: deviceID`

  Apply `sanitizeHeaderValue(v string) string` to every value: remove bytes outside `0x20–0x7E`,
  apply `strings.TrimSpace`, and turn an empty result into `"unknown"`.
- `func osInfo() (release, version string)` is split by build tag, mirroring
  `internal/auth/lock_unix.go` / `lock_windows.go`:
  - `identity_unix.go` with `//go:build !windows`: call `unix.Uname(&u)` from
    `golang.org/x/sys/unix` (already in go.mod v0.46.0). Return
    `unix.ByteSliceToString(u.Release[:])` and `unix.ByteSliceToString(u.Version[:])`.
    On error, return `"", ""`. Note that stdlib `syscall.Uname` does not exist on darwin.
  - `identity_windows.go` with `//go:build windows`: return `"", ""`. Both then become `"unknown"`.
- `func EnsureDeviceID(dir string) string`:
  1. Read `filepath.Join(dir, "kimi-device-id")` and trim it. If non-empty, return it.
  2. Otherwise generate 16 `crypto/rand` bytes as lowercase hex (32 chars).
  3. `os.MkdirAll(dir, 0o700)`, then write `id + "\n"` with mode `0o600`. This is best-effort.
  4. If the write fails, store the id in a package-level `map[string]string` keyed by `dir`,
     guarded by a `sync.Mutex`. Check that map first on later calls, so a persist failure still
     yields one stable id per dir for the process lifetime.

Tests (`internal/kimi/identity_test.go`):
- `IdentityHeaders("abc")` returns `X-Msh-Platform == "kimi_cli"`, `User-Agent == "KimiCLI/1.0.0"`,
  `X-Msh-Device-Id == "abc"`, and every value is non-empty printable ASCII.
- `sanitizeHeaderValue("h\x01ost\u00e9 ")` returns `"host"`, and `sanitizeHeaderValue("\x02")` returns `"unknown"`.
- `EnsureDeviceID(tmp)` returns 32 hex chars. A second call returns the same value, and the file exists.
- With `dir` set to a path under a regular file (so it is unwritable), two calls return the same id.

### 2. Device-flow OAuth manager — `internal/auth/kimi_oauth.go` (new)

Mirror the structure of `internal/auth/claudecode_oauth.go`: manager plus session map plus
`Snapshot`, `SetEndpoints` test hook, and `CancelSession` semantics. Take the device-flow
semantics from kimi-code `oauth.ts`. The auth package must not import `internal/kimi` or
`internal/config`; identity is injected as a func.

- Constants:
  ```go
  const (
      KimiCodeClientID  = "17e5f671-d194-4dfb-9706-5516cb48c098"
      KimiCodeOAuthHost = "https://auth.kimi.ai"
      KimiCodeBaseURL   = "https://api.kimi.ai/coding/v1"
  )
  var ErrKimiOAuthUnauthorized = errors.New("kimi oauth unauthorized")
  ```
- Types:
  - `KimiDeviceAuthorization{UserCode, DeviceCode, VerificationURI, VerificationURIComplete string; ExpiresIn, Interval int}`
  - `KimiTokenInfo{AccessToken, RefreshToken, Scope, TokenType string; ExpiresAt time.Time}`
  - `KimiUserInfo{UserID, Email, Nickname string}`
  - `KimiAuthSession`:
    - exported fields `ID, OAuthHost, BaseURL, Status, Error string`, `Device KimiDeviceAuthorization`,
      `Token *KimiTokenInfo`, `User *KimiUserInfo`, `CreatedAt time.Time`
    - unexported fields `registered bool`, `cancel context.CancelFunc`, `mu sync.Mutex`
    - `Status` is one of `pending|completed|expired|denied|cancelled|error`
  - `KimiAuthSessionSnapshot{ID, Status, Error, OAuthHost, BaseURL string; Device KimiDeviceAuthorization; Token *KimiTokenInfo; User *KimiUserInfo; CreatedAt time.Time}`.
    `func (s *KimiAuthSession) Snapshot() KimiAuthSessionSnapshot` copies the pointed-to Token and
    User values under `s.mu`, as the claudecode `Snapshot` does.
  - `func (s *KimiAuthSession) ClaimCompletion() bool`: under `s.mu`, if `Status == "completed" && Token != nil && !registered`,
    set `registered = true` and return true. Otherwise return false. This stops later status polls
    from re-saving a stale token over one that was refreshed since.
  - `KimiOAuthManager{mu sync.RWMutex; sessions map[string]*KimiAuthSession; httpClient *http.Client; identity func() http.Header; clientID, oauthHost, baseURL string; sleep func(context.Context, time.Duration) error}`.
- `func NewKimiOAuthManager(identity func() http.Header) *KimiOAuthManager`. Defaults:
  `httpClient = &http.Client{Timeout: 30 * time.Second}`, the three constants above, and
  `sleep = sleepCtx`. `sleepCtx` is new: a `time.NewTimer` select on `ctx.Done()` that returns `ctx.Err()`.
  A nil `identity` means no identity headers are added.
- Test hooks, modelled on `ClaudeCodeOAuthManager.SetEndpoints` (empty or nil arguments keep the current value):
  `func (m *KimiOAuthManager) SetEndpoints(oauthHost, baseURL string, httpClient *http.Client)` and
  `func (m *KimiOAuthManager) SetSleep(fn func(context.Context, time.Duration) error)`.
- Shared helpers (unexported):
  - `postForm(ctx, url string, form url.Values) (status int, data map[string]any, err error)`.
    Headers are the identity headers (if any) plus `Content-Type: application/x-www-form-urlencoded`
    and `Accept: application/json`. Read the body through `io.LimitReader(…, 1<<20)`. Non-JSON or
    non-object bodies give an empty map. A transport error returns
    `fmt.Errorf("OAuth request to %s failed: %w", url, err)`.
  - `kimiErrorDetail(data map[string]any) string` returns the first non-empty value among
    `error_description` (string), `message` (string), `error` (string), and `error.message`
    (when `error` is an object). If none is present it returns `"unknown"`.
  - `tokenFromResponse(data map[string]any, now time.Time) (*KimiTokenInfo, error)`.
    Missing or empty `access_token` gives `errors.New("OAuth response missing access_token")`.
    Missing or empty `refresh_token` gives `"OAuth response missing refresh_token"`.
    `expires_in` is accepted as float64 or as a numeric string via `strconv.ParseFloat`; if it is
    not finite or not > 0, return `"OAuth response missing or invalid expires_in"`.
    `ExpiresAt = now.Add(time.Duration(expiresIn * float64(time.Second)))`. `Scope` defaults to `""`
    and `TokenType` defaults to `"Bearer"`.
- `func (m *KimiOAuthManager) StartDeviceAuth(ctx context.Context) (*KimiAuthSession, error)`:
  1. Call `postForm(ctx, oauthHost+"/api/oauth/device_authorization", {client_id})`.
     If the status is not 200, return `fmt.Errorf("Device authorization failed (HTTP %d): %s", status, kimiErrorDetail(data))`.
  2. Require non-empty `user_code`. If missing, return `"Device authorization response missing user_code"`.
     Require non-empty `device_code`, with the analogous message. Require at least one of
     `verification_uri_complete` or `verification_uri`; if both are missing, return
     `"Device authorization response missing verification_uri_complete"`.
     If `interval` ≤ 0, use 5. If `expires_in` ≤ 0, use 900.
  3. Only after a valid response, build the session: `ID` is the hex of `generateRandomBytes(16)`
     (an existing helper in this package). `OAuthHost` and `BaseURL` come from the manager's current
     endpoints. `Status` is `"pending"` and `CreatedAt` is `time.Now()`. Register it under `m.mu`,
     first pruning sessions older than 1h: call their `cancel` and delete them.
  4. Start `go m.pollDeviceGrant(pollCtx, session)`, where `pollCtx, cancel :=
     context.WithCancel(context.Background())`. Do not use the request context, because the HTTP
     handler returns right away. Store `cancel` on the session. Return the session.
- `pollDeviceGrant(ctx, s)` holds `interval := time.Duration(Device.Interval) * time.Second` and
  `deadline := CreatedAt + ExpiresIn seconds`. Each loop iteration:
  1. If `time.Now().After(deadline)`, finish with `expired` and `"Device flow timed out"`.
  2. Call `m.sleep(ctx, interval)`. If it errors, finish with `cancelled` and `"authentication cancelled"`.
  3. Call `postForm(ctx, oauthHost+"/api/oauth/token", {client_id, device_code, grant_type})`.
  4. If there is a transport error: when `ctx.Err() != nil`, finish `cancelled`; otherwise finish
     with `error` and the error text.
  5. On `200` with a string `access_token`:
     - Run `tokenFromResponse`. On error, finish with `error` and its message.
     - Otherwise run `m.FetchUserInfo` with a 10s child context against `s.BaseURL`. This is
       best-effort; ignore any error.
     - Under `s.mu`, set `Token` and `User` and `Status = "completed"`. Return.
  6. On `status >= 500`, finish `error` with `fmt.Sprintf("Device token polling server error (HTTP %d): %s", status, detail)`.
  7. Otherwise switch on `data["error"]`:
     - `authorization_pending`: continue.
     - `slow_down`: `interval += 5*time.Second`, then continue.
     - `expired_token`: finish `expired` with `"device code expired"`.
     - `access_denied`: finish `denied`, with `error_description` or `"access denied"`.
     - Anything else: finish `error` with `fmt.Sprintf("Device token polling failed (HTTP %d): %s", status, detail)`.

  "Finish" means: under `s.mu`, set `Status`/`Error` only while the status is still `pending`, then return.
- `GetSession(id) (*KimiAuthSession, bool)` mirrors claudecode.
- `CancelSession(id)` mirrors claudecode `CancelSession`: delete the session from the map, call
  `cancel()`, and if it is still pending set `Status = "cancelled"`, `Error = "authentication cancelled"`.
- `func (m *KimiOAuthManager) RefreshToken(ctx context.Context, refreshToken, oauthHost string) (*KimiTokenInfo, error)`:
  - An empty `refreshToken` returns `fmt.Errorf("%w: missing refresh token", ErrKimiOAuthUnauthorized)`
    without any HTTP call.
  - If `oauthHost` is empty, use `m.oauthHost`.
  - Make up to 3 attempts of `postForm(ctx, oauthHost+"/api/oauth/token", {client_id, grant_type=refresh_token, refresh_token})`:
    - `200` with a string `access_token`: return `tokenFromResponse(data, time.Now())`.
    - `401`/`403` or `data["error"] == "invalid_grant"`: return `fmt.Errorf("%w: %s", ErrKimiOAuthUnauthorized, detail)`.
    - Transport error or status in {429,500,502,503,504}: remember the error. If attempts remain,
      call `m.sleep(ctx, backoff)` with backoff 1s after attempt 1 and 2s after attempt 2
      (a sleep error returns that error), then retry.
    - Any other status: return `fmt.Errorf("Token refresh failed (HTTP %d): %s", status, detail)` without retrying.
  - After the attempts run out, return the last error.
- `func (m *KimiOAuthManager) FetchUserInfo(ctx context.Context, baseURL, accessToken string) (*KimiUserInfo, error)`:
  - Send `GET strings.TrimRight(baseURL, "/") + "/me"` with identity headers,
    `Authorization: Bearer <token>`, and `Accept: application/json`.
  - A non-2xx status returns `fmt.Errorf("Kimi userinfo returned %d", status)`.
  - Decode `user_id`, `email`, `nickname`. An empty `user_id` returns `errors.New("malformed Kimi userinfo response")`.

Tests (`internal/auth/kimi_oauth_test.go`, `package auth`). Use an `httptest.Server` fake host
wired in through `SetEndpoints(srv.URL, srv.URL+"/coding/v1", srv.Client())`. Use a `SetSleep`
stub that appends `d` to a slice and returns `ctx.Err()`. Wait for terminal states by polling
`Snapshot()` with a 2s deadline.
- Two `authorization_pending` (HTTP 400) responses followed by success: status `completed`, 3 token
  POSTs, session carries `UserCode` and `VerificationURIComplete`, Token fields parsed,
  `/coding/v1/me` fetched and `User.Email` set, and the recorded intervals are `[5s,5s,5s]`.
- `slow_down` followed by success: the recorded intervals are `[5s,10s]`.
- `expired_token` gives `expired`. `access_denied` gives `denied`, with `Error` equal to the `error_description`.
- `CancelSession` while the token endpoint keeps returning pending gives a snapshot status of
  `cancelled`, `GetSession` returning false, and no further token POSTs after cancellation.
- A device response missing `user_code` makes `StartDeviceAuth` return an error and registers no session.
- Refresh with a 500 followed by a 200: success after exactly 2 POSTs, and one recorded sleep of 1s.
- Refresh with 400 `{"error":"invalid_grant"}`: `errors.Is(err, ErrKimiOAuthUnauthorized)`.
- Refresh with a 200 that lacks `refresh_token`: the error message is `"OAuth response missing refresh_token"`.
- `ClaimCompletion` returns true once and false after that.

### 3. Config: `kimi.oauth` slot, merge-on-save, redaction — `internal/config/config.go`

- Add the following after `KimiConfig`, and add `"time"` to the imports:
  ```go
  // KimiOAuthConfig is the Kimi Code subscription credential from the device flow
  // (global auth.kimi.ai / api.kimi.ai only).
  type KimiOAuthConfig struct {
      Token        string     `json:"token"`
      RefreshToken string     `json:"refreshToken,omitempty"`
      ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
      Email        string     `json:"email,omitempty"`
      UserID       string     `json:"userId,omitempty"`
      Nickname     string     `json:"nickname,omitempty"`
      OAuthHost    string     `json:"oauthHost,omitempty"` // issuing host; refresh goes here
      BaseURL      string     `json:"baseUrl,omitempty"`   // API base, …/coding/v1
  }
  ```
  Add `OAuth *KimiOAuthConfig \`json:"oauth,omitempty"\`` to `KimiConfig`. There is no region field.
  Defaults do not change, and `DefaultConfig` has no `oauth`.
- Replace the `if k == "kimi"` branch in `Save` (~L877-897) with merge-from-persisted, using the
  pattern of the `claudecode` branch (~L919-973):
  1. `existingKimi, _ := currentMap["kimi"].(map[string]any)`. Copy all of its keys into `kimiCopy`.
     Then overlay every posted key except `oauth`.
  2. Keep the existing `hasApiKey` preservation exactly as it is: if `hasApiKey` is true and the
     posted `apiKey` is empty, take `existingKimi["apiKey"]`. Then `delete(kimiCopy, "hasApiKey")`.
     Keys missing from the post now survive. That is safe because the UI (`saveKimiConfig`) always
     posts `hasApiKey` and has no path that clears a key.
  3. `oauth` handling, only when the key is present in the posted map:
     - `nil`: `delete(kimiCopy, "oauth")`. This is logout.
     - A `map[string]any` with a non-empty string `token`: store a copy with `hasToken`,
       `maskedToken` and `hasRefreshToken` deleted. This is login, or a refresh.
     - A map with an empty or missing `token`: leave the persisted value unchanged. This is a
       redacted echo, for example `Save(GetPublicConfig())`.
     - If `oauth` is absent from the post, the persisted value survives through step 1.
  4. A non-map `v` still assigns `currentMap[k] = v`.
- Extract `func maskToken(tok string) string` from the claudecode redaction (~L1143-1147):
  `len > 10` gives `tok[:6] + "..." + tok[len-4:]`, otherwise `"******"`. Use it in the claudecode
  branch, where behaviour stays identical, and in the kimi branch.
- In the `GetPublicConfig` kimi branch (~L1102-1114), when `kk == "oauth"` and `vv` is a
  `map[string]any`, emit a copy with these changes:
  - `token` (non-empty) is replaced by `hasToken: true` plus `maskedToken: maskToken(token)`.
  - `refreshToken` (non-empty) is replaced by `hasRefreshToken: true`.
  - Every other key is passed through (`expiresAt`, `email`, `userId`, `nickname`, `oauthHost`, `baseUrl`).

  This matters because `GET /api/config` is a public route (`management.go` ~L52).

Tests (extend `internal/config/config_test.go`, next to `TestGetPublicConfig_KimiRedactsAPIKey`):
- Save a kimi block with `apiKey` and `oauth{token:"tok-1234567890abcd", refreshToken:"rt", email:"a@b.c"}`.
  Then `Save({"kimi":{"enabled":true,"hasApiKey":true,"allowlist":[]}})`. Afterwards `Get().Kimi.OAuth.Token`
  is still `tok-1234567890abcd` and `APIKey` is kept.
- `Save({"kimi":{"oauth":nil}})` gives `OAuth == nil`, keeps `APIKey`, and keeps `BaseURL`.
- `GetPublicConfig()["kimi"]["oauth"]` has no `token` or `refreshToken`, has
  `hasToken == true`, has `maskedToken == "tok-12...abcd"`, and has `email` present.
- `Save(GetPublicConfig())` keeps `OAuth.Token` and `OAuth.RefreshToken` unchanged.

### 4. Credential resolution and upstream use — `internal/api`, `internal/kimi`

- `internal/kimi/passthrough.go`: change the signature to
  `ForwardMessagesWithModify(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, identity http.Header, modify func(*http.Response) error)`.
  In `Director`, after `req.Header.Del("x-api-key")`, loop `for k, vs := range identity { req.Header.Set(k, vs[0]) }`,
  skipping empty slices. This overrides the client's `User-Agent`. Update all 3 callers:
  `ForwardMessagesWithHook` (pass `nil`), and `server.go` ~L1309 and ~L1321.
  Leave `zen.ForwardMessagesWithModify` untouched; it is a separate package.
- `internal/kimi/client.go`: change the signature to
  `FetchModels(ctx context.Context, apiKey, baseURL string, headers http.Header) ([]ModelItem, error)`.
  `Set` each header before `Authorization`. Update the callers: `management.go` ~L2010, and
  `internal/kimi/client_test.go` L31 and L70 (pass `nil`).
- `internal/api/server.go`:
  - Add these `Server` fields:
    - `kimiOAuthMgr *auth.KimiOAuthManager`
    - `kimiIdentityOnce sync.Once`
    - `kimiIdentity http.Header`
    - `kimiRefreshMu sync.Mutex`

    Do not add an `Options` field. The claudecode `Options.ClaudeCodeOAuthMgr` has no callers
    outside `New`, and tests replace `server.kimiOAuthMgr` directly.
  - Add `func (server *Server) kimiIdentityHeaders() http.Header`. It is lazy: through
    `kimiIdentityOnce`, it computes `kimi.IdentityHeaders(kimi.EnsureDeviceID(config.GetConfigDir()))`
    and returns the cached header. Callers must only read it. Keeping it lazy means `New` never
    writes a file, so tests that do not set `ANTIGRAVITY_CONFIG_DIR` never touch the real config dir.
  - In `New` (~L161-169), after `srv := &Server{…}`, add `srv.kimiOAuthMgr = auth.NewKimiOAuthManager(srv.kimiIdentityHeaders)`.
  - Add these near `forwardToKimi`:
    ```go
    var (
        errKimiNoCredential = errors.New("Kimi gateway enabled but no credential configured (log in with Kimi Code or set an API key)")
        errKimiLoginExpired = errors.New("Kimi Code login expired — sign in again")
    )
    type kimiCredential struct {
        token, baseURL string
        oauth          bool
    }
    func (server *Server) resolveKimiCredential(ctx context.Context, cfg config.KimiConfig) (kimiCredential, error)
    ```
    1. If `cfg.OAuth != nil && cfg.OAuth.Token != ""`, set `o := cfg.OAuth`.
       - It is fresh when `o.ExpiresAt == nil || time.Until(*o.ExpiresAt) > 60*time.Second`.
       - If it is not fresh, lock `kimiRefreshMu`, then re-read `config.Get().Kimi.OAuth`.
         - If that value is nil or has an empty token: unlock and return `errKimiLoginExpired`.
           Someone logged out concurrently.
         - If it is fresh now: use it. Another request already refreshed.
         - Otherwise call `server.kimiOAuthMgr.RefreshToken(context.WithoutCancel(ctx), o.RefreshToken, o.OAuthHost)`.
           `WithoutCancel` means a client disconnect cannot strand a rotated refresh token half-persisted.
         - If `errors.Is(err, auth.ErrKimiOAuthUnauthorized)`, return `errKimiLoginExpired`.
           Keep the stored block; the next login replaces it.
         - Any other error returns `fmt.Errorf("Kimi OAuth token refresh failed: %w", err)`.
         - On success, persist through
           `config.Save(map[string]any{"kimi": map[string]any{"oauth": map[string]any{…}}})`. The map
           holds `token`, `refreshToken`, and `expiresAt` (formatted with `time.RFC3339`), plus the
           existing `email`, `userId`, `nickname`, `oauthHost` and `baseUrl`. The step-3 merge keeps
           every other kimi key. Push the result through `server.backend.(ConfigUpdater)` the same
           way `handleKimiConfigSave` does. If the Save fails, log
           `server.logger.Warn("kimi oauth refresh persist failed", "error", err)` and still use the new token.
       - `baseURL` is `o.BaseURL`, or `auth.KimiCodeBaseURL` if that is empty.
       - Return `{token, baseURL, true}`.
    2. Else, if `cfg.APIKey != ""`, return `{cfg.APIKey, cfg.BaseURL, false}`. This is the current behaviour.
    3. Else return `errKimiNoCredential`.
  - `forwardToKimi` (~L1289): replace the `kimiCfg.APIKey == ""` guard with a call to
    `resolveKimiCredential(request.Context(), kimiCfg)`. Map the error:
    - `errKimiNoCredential` → `writeAPIError(400, "invalid_request_error", err.Error())`
    - `errKimiLoginExpired` → `401` with `"authentication_error"`
    - anything else → `502` with `"api_error"`

    Then use `cred.baseURL` and `cred.token` in all three send paths. Set
    `var identity http.Header; if cred.oauth { identity = server.kimiIdentityHeaders() }`. Pass
    `identity` to both `ForwardMessagesWithModify` calls. In the manual CCR `sender` (~L1325-1346),
    `Set` the identity headers after `Authorization`.
- `internal/api/cachebump_server.go` `sendKimiBump` (~L252-260): replace the body with a call to
  `resolveKimiCredential(ctx, config.Get().Kimi)`.
  - If it errors, or `cred.baseURL == ""` (which preserves today's apiKey-path guard), return
    `cachebump.ErrAccountUnavailable`. That is the recoverable stop, matching `sendClaudeCodeBump`'s
    refresh-failure handling at ~L196-201.
  - Otherwise call `postBumpRequest(ctx, rec, kimi.NormalizeBaseURL(cred.baseURL)+"/v1/messages", …)`.
    Its header func sets `Authorization: Bearer cred.token` and, when `cred.oauth`, `Set`s the identity headers.
- `internal/api/management.go` `handleKimiModelsFetch` (~L1992-2026):
  - If `req.APIKey != ""`, keep the current explicit-key path. The base URL is `req.BaseURL`, then
    `cfg.Kimi.BaseURL`, then the moonshot default. Headers are `nil`.
  - Otherwise, call `server.resolveKimiCredential(request.Context(), cfg.Kimi)`. On error, write 502
    `{"status":"error","error":err.Error()}`, the same shape as the existing fetch failure.
    - For an OAuth credential, use `cred.token` and `cred.baseURL`, pass
      `server.kimiIdentityHeaders()`, and ignore `req.BaseURL`, since the token is valid only on api.kimi.ai.
    - For an apiKey credential, use `cred.token`, with the base URL being `req.BaseURL`, then
      `cred.baseURL`, then the moonshot default, and headers `nil`.

  Existing test `TestServer_HandleKimiModelsFetch` (apiKey path) must keep passing unchanged.

Out of scope: retrying an upstream 401 whose token is not yet expired. There is no forced refresh on 401.

Tests: extend `internal/api/kimi_proxy_test.go`, using its `newKimiTestServer` and temp-dir pattern.
Seed the config through `config.Save` with `enabled`, an allowlist entry, and
`oauth{token, refreshToken, expiresAt, oauthHost: fakeAuth.URL, baseUrl: upstream.URL+"/coding/v1"}`.
- With an OAuth token that is still valid, the upstream sees path `/coding/v1/messages`,
  `Authorization: Bearer <oauth token>`, `X-Msh-Platform: kimi_cli`, and a `User-Agent` starting with `KimiCLI/`.
- With `expiresAt` in the past, the fake auth host's `/api/oauth/token` is hit exactly once with
  `grant_type=refresh_token`. The upstream sees the new Bearer, and `config.Get().Kimi.OAuth.Token`
  equals the new token.
- When the refresh returns `invalid_grant`, the client gets 401 with `authentication_error` and the upstream is not called.
- When both `oauth` and `apiKey` are set, the OAuth token is used.
- With `apiKey` only, the upstream sees `Bearer <apiKey>` and no `X-Msh-Platform` header.
- With neither, the client gets 400 with a message containing `no credential configured`.
- In `cachebump_server` terms: calling `server.sendKimiBump` directly with OAuth-only config hits
  `/coding/v1/messages` with the OAuth Bearer and `X-Msh-Platform`.

### 5. HTTP handlers and routes — `internal/api/kimi_oauth_handlers.go` (new), `internal/api/management.go`

Mirror `claudecode_oauth_handlers.go`, including the nil-manager guard, which returns 500
`{"status":"error","error":"Kimi OAuth manager not initialized"}`. There are no request-body
parameters anywhere, because there is no region.

- `handleKimiAuthStartPost`, for `POST /api/kimi/auth/start`, ignores the body.
  - If `StartDeviceAuth(request.Context())` errors, return 502 `{"status":"error","error":err.Error()}`
    (the failure is always upstream).
  - On success, return 200 `{"status":"ok","session_id","user_code","verification_uri","verification_uri_complete","expires_in","interval"}`.
  - Log `slog.Info("Kimi Code auth session created via API", "session_id", id)`.
- `handleKimiAuthStatusGet`, for `GET /api/kimi/auth/status?session_id=`:
  - A missing param returns 400 `{"status":"error","error":"missing session_id parameter"}`.
  - An unknown session returns 404 `{"status":"expired","error":"session not found or expired"}`.
  - For `snap.Status == "completed"`:
    - If `session.ClaimCompletion()` is true, call `server.registerAuthenticatedKimiOAuth(snap)`.
      If that errors, return 500 `{"status":"error","error":"Failed to save Kimi login: "+err}`.
      The session stays claimed and the user must log in again.
    - Then return 200 `{"status":"completed","account":{"email","user_id","nickname","expires_at"}}`,
      with `expires_at` being `Token.ExpiresAt` in RFC3339. Empty strings are allowed when `User` is nil.
  - For `expired|denied|cancelled|error`, return 200 `{"status":<status>,"error":snap.Error}`.
  - Otherwise return `{"status":"pending"}`.
- `handleKimiAuthCancelPost`, for `POST /api/kimi/auth/cancel` with `{"session_id"}`: if the ID is
  non-empty, call `CancelSession`. Always return 200 `{"status":"ok"}`.
- `handleKimiAuthLogoutPost`, for `POST /api/kimi/auth/logout`: call
  `config.Save(map[string]any{"kimi": map[string]any{"oauth": nil}})`. On error, return 500
  `{"status":"error","error":"Failed to save config: "+err}`. On success, push through
  `ConfigUpdater` and return 200 `{"status":"ok","config":config.GetPublicConfig()["kimi"]}`.
- `func (server *Server) registerAuthenticatedKimiOAuth(snap auth.KimiAuthSessionSnapshot) error`:
  - If `snap.Token == nil`, do nothing and return nil.
  - Otherwise call `config.Save(map[string]any{"kimi": map[string]any{"enabled": true, "oauth": map[string]any{…}}})`.
    The oauth map holds `token`, `refreshToken`, `expiresAt` (RFC3339), `email`, `userId`,
    `nickname` (all from `snap.User` when non-nil), `oauthHost: snap.OAuthHost`, and `baseUrl: snap.BaseURL`.
    The step-3 merge keeps `baseUrl`, `apiKey` and `allowlist`.
  - Then push through `ConfigUpdater`.
- Routes: in the `management.go` switch, directly after the `/api/kimi/models/fetch` case (~L217-219),
  add cases with the same shape as `claudecode_management.go` ~L373-384:
  - `"/api/kimi/auth/start"` for POST
  - `"/api/kimi/auth/status"` for GET
  - `"/api/kimi/auth/cancel"` for POST
  - `"/api/kimi/auth/logout"` for POST

  They inherit the existing `x-webui-password` gate at ~L53-58.

Tests (`internal/api/kimi_oauth_handlers_test.go`, modelled on `claudecode_oauth_handlers_test.go`):
- Setup: `newTestServerWithManager`, then a fake host with:
  - `/api/oauth/device_authorization` returning `{device_code:"dc", user_code:"ABCD-1234", verification_uri_complete:"https://www.kimi.ai/code/authorize_device?user_code=ABCD-1234", expires_in:1800, interval:5}`
  - `/api/oauth/token` returning 400 `authorization_pending` until an `atomic.Bool approved` flips,
    then 200 `{access_token:"at-123456789012", refresh_token:"rt-1", expires_in:3600}`
  - `/coding/v1/me` returning `{user_id:"u1", email:"dev@example.com", nickname:"dev"}`

  Then `mgr := auth.NewKimiOAuthManager(nil)`, `mgr.SetEndpoints(fake.URL, fake.URL+"/coding/v1", fake.Client())`,
  `mgr.SetSleep` (10ms real sleep honouring ctx), and `server.kimiOAuthMgr = mgr`.
- Full flow:
  1. start returns 200 with `user_code == "ABCD-1234"`.
  2. status returns `pending`.
  3. Flip `approved`, then poll status (up to 2s) until it returns `completed` with `account.email == "dev@example.com"`.
  4. `config.Get().Kimi` now has `Enabled == true`, `OAuth.Token == "at-123456789012"`,
     `OAuth.RefreshToken == "rt-1"`, `OAuth.OAuthHost == fake.URL`, and `OAuth.BaseURL == fake.URL+"/coding/v1"`.
- Claim-once: pre-seed `apiKey` and `baseUrl` before login and confirm they survive login. Then
  overwrite `OAuth.Token` through `config.Save` and poll status again: it returns `completed`, but
  the config token stays at the overwritten value.
- An unknown `session_id` returns 404. Cancel followed by status returns 404.
- Logout: `OAuth == nil`, `APIKey` kept, and the response `config` has no `oauth` key.

### 6. Web UI — `internal/webui/public/views/settings.html`, `internal/webui/public/js/components/models.js`, translations

- `models.js`:
  - `kimiConfig` default (~L361-367): add `oauth: null`. In `fetchKimiConfig` (~L657-663), add `oauth: data.config.oauth || null`.
  - New state next to `kimiSaving`:
    `kimiOAuth: { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false }`.
  - Leave `saveKimiConfig` unchanged. Its payload lists fields explicitly and never includes
    `oauth`; the step-3 merge preserves the login.
  - `async startKimiOAuthLogin()`:
    1. Use `window.utils.request('/api/kimi/auth/start', {method:'POST', headers:{'Content-Type':'application/json'}, body:'{}'}, password)`,
       with the same `newPassword` handling as `fetchKimiConfig`.
    2. If the response is not ok or `status !== 'ok'`, show a toast with `data.error` and return.
    3. Otherwise store `sessionId`, `userCode`, and `verificationUri = data.verification_uri_complete || data.verification_uri`.
       Set `status='pending'`, `error=''`, `polling=true`. Open `kimi_oauth_modal` with `showModal()`
       and call `this._pollKimiOAuth()`.
  - `async _pollKimiOAuth()`: while `polling`, wait 2000ms with `setTimeout` and then
    `GET /api/kimi/auth/status?session_id=<id>`.
    - `completed`: set `status='completed'`, `polling=false`, close the modal, show the toast
      `kimiOAuthSuccess`, then `await this.fetchKimiConfig()`.
    - `expired|denied|cancelled|error`, or a non-ok HTTP response: set `status` and `error`
      (from `data.error`) and `polling=false`. The modal stays open to show the error.
    - `pending`: loop.
  - `async cancelKimiOAuthLogin()`: if `status === 'pending'`, POST `/api/kimi/auth/cancel` with
    `{session_id}` and set `status = 'cancelled'`. Then set `polling=false` and close the modal if it
    is open. Wire it to the modal's `@close` so ESC also cancels. It is idempotent: a completed
    session never posts a cancel.
  - `async kimiOAuthLogout()`: if `!confirm(t('kimiOAuthLogoutConfirm'))`, return. Otherwise POST
    `/api/kimi/auth/logout`, then `await this.fetchKimiConfig()`.
- `settings.html` Kimi card, inserted after the API-key row (~L1246-1253) and before the Save button row:
  - When `kimiConfig.oauth && kimiConfig.oauth.hasToken`: show the text `kimiOAuthLoggedInAs` plus
    `email || nickname || userId`, plus `expiresAt` when present, and a **Log out** button
    (`kimiOAuthLogout()`). Below it, a small gray note with `kimiOAuthPrecedence`.
  - Otherwise: a **Login with Kimi Code** button (`startKimiOAuthLogin()`). There is no region select.
- A new `<dialog id="kimi_oauth_modal" class="modal" @close="cancelKimiOAuthLogin()">`, placed
  after `kimi_discover_modal` (~L2577-2618) and copying its markup and classes. It contains:
  - title `kimiOAuthModalTitle`
  - text `kimiOAuthEnterCode`
  - `kimiOAuth.userCode` in large mono
  - `kimiOAuth.verificationUri` as an `<a target="_blank" rel="noopener">`
  - a spinner with `kimiOAuthWaiting` while `status==='pending'`
  - a red error line bound to `kimiOAuth.error`
  - a Cancel button (`cancelKimiOAuthLogin()`, label `t('cancel')`)
- `internal/webui/public/js/translations/en.js` and `pt.js`: add these keys after `kimiDiscoverImport`
  (en / pt). Use them as `$store.global.t('<key>') || '<en>'`, following the existing convention.

  | key | en | pt |
  |---|---|---|
  | `kimiOAuthLogin` | Login with Kimi Code | Entrar com Kimi Code |
  | `kimiOAuthLoggedInAs` | Logged in as | Conectado como |
  | `kimiOAuthLogout` | Log out | Sair |
  | `kimiOAuthLogoutConfirm` | Log out of Kimi Code? | Sair do Kimi Code? |
  | `kimiOAuthModalTitle` | Kimi Code Login | Login Kimi Code |
  | `kimiOAuthEnterCode` | Open the link and confirm this code: | Abra o link e confirme este código: |
  | `kimiOAuthWaiting` | Waiting for authorization… | Aguardando autorização… |
  | `kimiOAuthSuccess` | Kimi Code login complete | Login Kimi Code concluído |
  | `kimiOAuthPrecedence` | Kimi Code login is used for requests; Base URL and API key apply only when logged out. | O login Kimi Code é usado nas requisições; Base URL e chave de API só se aplicam sem login. |

## Critical files & anchors

1. `internal/auth/claudecode_oauth.go`: the shape to copy for the session, snapshot and manager
   (L82-165), `SetEndpoints` (L168-183), `CancelSession` (L375-392), and `generateRandomBytes` (L186) to reuse.
2. `internal/api/server.go`: the `Server` struct (L100-140), `New` (L142-193), and `forwardToKimi`
   (L1289-1373), where credential resolution and identity headers land.
3. `internal/config/config.go`: `KimiConfig` (L95-100), the `Save` kimi branch (L877-897) with the
   claudecode merge reference (L919-978), and the `GetPublicConfig` kimi redaction (L1102-1114)
   with the claudecode mask (L1141-1147).
4. `internal/api/cachebump_server.go`: `sendKimiBump` (L252-260), which today sends an empty
   Bearer when only OAuth is configured.
5. `internal/kimi/passthrough.go`: the `Director` auth rewrite (L53-75) that receives the identity loop.

## Verification

Run everything from the repo root `/Users/gus/Git/antigravity-claude-proxy-go`.

1. `go build ./... && GOOS=windows GOARCH=amd64 go build ./... && GOOS=linux GOARCH=amd64 go build ./...`.
   This proves that the `identity_unix.go`/`identity_windows.go` split compiles on every target.
2. `go test ./internal/kimi/ ./internal/auth/ ./internal/config/ ./internal/api/`. The new tests
   from steps 1–5 pass, and the existing Kimi forward, dispatch, config and claudecode OAuth suites
   still pass.
3. `go test ./...`: the full suite passes.
4. Live check against the real global host. No Kimi account is needed for this part.
   1. `go build -o bin/proxy ./cmd/proxy`
   2. `mkdir -p /tmp/kimi-e2e && ANTIGRAVITY_CONFIG_DIR=/tmp/kimi-e2e ./bin/proxy -port 18091 -api-key test-local`
      Run it as a background service. A fresh config dir has no web UI password.
      If startup fails for lack of Cloud Code credentials in the empty dir, `cp ~/.config/antigravity-proxy/accounts.json /tmp/kimi-e2e/` and retry.
   3. `curl -sS -X POST 127.0.0.1:18091/api/kimi/auth/start`. Expect 200 with a `user_code` of the
      form `XXXX-XXXX`, and `verification_uri_complete` starting with `https://www.kimi.ai/code/authorize_device?user_code=`.
   4. `curl -sS '127.0.0.1:18091/api/kimi/auth/status?session_id=<id>'` returns `{"status":"pending"}`.
      `cat /tmp/kimi-e2e/kimi-device-id` shows 32 hex chars.
5. Live login. **This needs the user** to approve in a browser with a Kimi Code subscription. If the
   user is not available, report this step as skipped; steps 2 and 4 still stand.
   1. Open the `verification_uri_complete` URL from 4.3 and approve.
   2. Poll status until it returns `completed` with `account.email`.
   3. `GET /api/kimi/config` shows `oauth.hasToken:true`, a `maskedToken`, and no `token`.
   4. `curl -sS -X POST 127.0.0.1:18091/api/kimi/models/fetch` lists the Kimi Code models
      (`k3`, `kimi-for-coding`, …). Add `kimi-for-coding` to the allowlist through
      `POST /api/kimi/config` with `{"enabled":true,"hasApiKey":false,"allowlist":[{"id":"kimi-for-coding","enabled":true}]}`.
   5. `curl -sS -X POST 127.0.0.1:18091/v1/messages -H 'x-api-key: test-local' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' -d '{"model":"kimi-for-coding","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'`
      returns 200 with an Anthropic-shaped body. That proves the chain dispatch → OAuth → `api.kimi.ai/coding/v1/messages`.
   6. `POST /api/kimi/auth/logout` makes `GET /api/kimi/config` show no `oauth`.
6. Web UI, using the browser tool against `http://127.0.0.1:18091/`, Settings page, Kimi card:
   1. Click **Login with Kimi Code**. The modal shows the code and a `www.kimi.ai` link, then ESC. `GET /api/kimi/auth/status` for that session returns 404.
   2. Seed a fake login: `curl -X POST 127.0.0.1:18091/api/kimi/config -H 'content-type: application/json' -d '{"enabled":true,"hasApiKey":false,"oauth":{"token":"tok-abcdefghijkl","refreshToken":"rt","email":"ui@example.com"}}'`.
      Reload. The card shows "Logged in as ui@example.com", **Log out**, and the precedence note.
   3. Click **Log out** and confirm. The login button returns.
   4. Take a screenshot as evidence for each state.
   5. Stop the service and `rm -rf /tmp/kimi-e2e`.

## Assumptions & contingencies

- Global only: all four endpoints are pinned to `auth.kimi.ai` / `api.kimi.ai/coding/v1`.
  Mainland `kimi.com` and region switching are not implemented because the user is not in mainland
  China. The persisted `oauthHost`/`baseUrl` exist so that refresh always reaches the issuing host
  (and so tests can point at fakes). They are not a region mechanism.
- `CLIVersion = "1.0.0"` is the proxy's own token in the KimiCLI identity that the user chose. Change
  it only if the user asks. Contingency: if step 5 returns 401/403 on `/coding/v1/messages` or
  `/models` while `/me` succeeds, the identity is being rejected. Stop and report the upstream error
  body. Do not guess other header values.
- If a live device-authorization response omits `verification_uri_complete`, the manager already
  falls back to `verification_uri` (step 2), and the UI shows that link. No code change is needed.
