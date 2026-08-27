# Kimi Code Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a Kimi Code gateway to `antigravity-claude-proxy-go` that forwards `/v1/messages` requests transparently to `https://api.kimi.com/coding` for allowlisted models, modelled on the existing OpenRouter gateway but reduced to its Anthropic-native essentials.

**Architecture:** New `internal/kimi` package owns the client, model catalog, and one config struct. `internal/api/server.go` gains a `forwardToKimi` branch in `messages()` that uses `httputil.ReverseProxy` (same shape as the existing `forwardToCustomEndpoint` from PR #7) — no body translation because Kimi's `/v1/messages` is already Anthropic-compatible. `internal/api/management.go` exposes `GET/POST /api/kimi/config` and `POST /api/kimi/models/fetch`. WebUI gains a Kimi settings section in `settings.html`, a `kimiConfig` block in `data-store.js`/`models.js`, and i18n keys in five translation files.

**Tech Stack:** Go 1.27rc2, `net/http/httputil`, Alpine.js (UI), 4 existing webui translation files (en, pt, zh, tr, id).

**Spec:** User-confirmed requirements in this conversation:
- `ANTHROPIC_AUTH_TOKEN` set in `settings.json` is the user's Kimi API key (Kimi publishes Claude Code integration guide at https://platform.kimi.ai/docs/guide/claude-code-kimi that wires `ANTHROPIC_BASE_URL=https://api.kimi.com/coding` + `ANTHROPIC_AUTH_TOKEN=<key>`)
- Gateway: separate `internal/kimi` package (parity with `internal/openrouter`)
- Per-model alias mapping (each allowlist row has `{id, alias}`)
- Model discovery: live `/v1/models` fetch + cache + UI "Discover" button (mirrors OR, not just hardcoded seed)

**Working directory:** `main` branch, repo root `/Users/gus/Git/antigravity-claude-proxy-go`. No new worktree (single-feature, TDD, no parallel agents expected).

## Global Constraints

- Go 1.27rc2 (see `go.mod`); no new module dependencies.
- All config fields are JSON-tagged; the file is `config.json` next to the binary.
- API keys must never appear in `GetPublicConfig` output (only a `hasApiKey` boolean).
- No new module imports. `httputil.ReverseProxy` already in use in `internal/api/server.go:forwardToCustomEndpoint` (transparent-forwarding worktree, commit e81e9ba — already merged on `main` per `a3bb5a5`).
- Test names follow the existing convention: `TestXxx_FunctionName_StateUnderTest`.
- Commit prefix: `feat(kimi):` for new files, `feat(api):` / `feat(config):` / `feat(webui):` for cross-cutting.
- New package follows the same `kimi` lowercase naming as `internal/openrouter`/`internal/format`.
- WebUI strings go through `$store.global.t('key')` with the English string as fallback — same pattern as every other string in `settings.html`.

## File Map

| File | Status | Responsibility |
|------|--------|----------------|
| `internal/kimi/kimi.go` | new | `KimiConfig` struct, `NormalizeBaseURL`, package-level `DefaultClient` |
| `internal/kimi/client.go` | new | `FetchModels`, `GetCachedModels`, `ModelItem` |
| `internal/kimi/client_test.go` | new | tests for client |
| `internal/kimi/passthrough.go` | new | `ForwardMessages` (ReverseProxy director); `ExtractSessionID` |
| `internal/kimi/passthrough_test.go` | new | director test (header set, body re-marshal) |
| `internal/config/config.go` | modify | add `KimiConfig`/`KimiModelConfig`, `Kimi` field on `Config`, defaults, public redaction |
| `internal/config/config_test.go` | modify | tests for Kimi redaction + defaults |
| `internal/api/server.go` | modify | add `forwardToKimi`, branch in `messages()`, `/v1/models` allowlist injection |
| `internal/api/management.go` | modify | add `/api/kimi/config` GET/POST + `/api/kimi/models/fetch` |
| `internal/api/kimi_proxy_test.go` | new | end-to-end proxy test against `httptest.Server` |
| `internal/api/kimi_config_test.go` | new | management endpoint tests |
| `internal/webui/public/views/settings.html` | modify | add Kimi gateway section + Discover modal |
| `internal/webui/public/js/data-store.js` | modify | add `kimi` to state, model-family mapping |
| `internal/webui/public/js/components/models.js` | modify | add `kimiConfig` block + save/fetch |
| `internal/webui/public/js/components/model-dropdown.js` | modify | add `kimi` family |
| `internal/webui/public/js/translations/{en,pt,zh,tr,id}.js` | modify | add 12 keys per language |

---

### Task 1: Kimi config struct + defaults + public redaction

**Files:**
- Modify: `internal/config/config.go:1-300` (add types, defaults, redaction)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `type KimiModelConfig struct { ID, Alias, DisplayName, ContextLen, MaxOutputTokens int, Enabled bool }`
  - `type KimiConfig struct { Enabled bool, BaseURL, APIKey string, Allowlist []KimiModelConfig }`
  - Field `Kimi KimiConfig` added to `Config`
  - `DefaultConfig()` returns a `KimiConfig` with `BaseURL: "https://api.kimi.com/coding"`

- [ ] **Step 1.1: Write failing test for Kimi defaults**

Append to `internal/config/config_test.go`:

```go
func TestDefaultConfig_KimiBaseURL(t *testing.T) {
    cfg := DefaultConfig()
    if cfg.Kimi.BaseURL != "https://api.kimi.com/coding" {
        t.Fatalf("default Kimi base URL = %q, want %q", cfg.Kimi.BaseURL, "https://api.kimi.com/coding")
    }
    if cfg.Kimi.Enabled {
        t.Fatal("Kimi should be disabled by default")
    }
    if len(cfg.Kimi.Allowlist) != 0 {
        t.Fatalf("Kimi allowlist should be empty by default, got %d", len(cfg.Kimi.Allowlist))
    }
}
```

- [ ] **Step 1.2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestDefaultConfig_KimiBaseURL -v`
Expected: compile error `cfg.Kimi undefined`.

- [ ] **Step 1.3: Add Kimi types to `config.go`**

Add near the existing `OpenRouterConfig` (after `AppSpoof` field, around line 60):

```go
// KimiModelConfig describes one Kimi Code model the proxy may forward to.
type KimiModelConfig struct {
    ID              string `json:"id"`
    Alias           string `json:"alias,omitempty"`
    DisplayName     string `json:"displayName,omitempty"`
    ContextLen      int    `json:"contextLength,omitempty"`
    MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
    Enabled         bool   `json:"enabled"`
}

// KimiConfig holds the Kimi Code gateway configuration.
type KimiConfig struct {
    Enabled   bool               `json:"enabled"`
    BaseURL   string             `json:"baseUrl"`
    APIKey    string             `json:"apiKey,omitempty"`
    Allowlist []KimiModelConfig  `json:"allowlist,omitempty"`
}
```

Add `Kimi KimiConfig` to the `Config` struct (next to `OpenRouter OpenRouterConfig` at line 95):

```go
    Kimi                    KimiConfig               `json:"kimi,omitempty"`
```

In `DefaultConfig()` (around line 124), add the Kimi block alongside the OpenRouter defaults:

```go
        Kimi: KimiConfig{
            BaseURL:   "https://api.kimi.com/coding",
            Allowlist: []KimiModelConfig{},
        },
```

- [ ] **Step 1.4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestDefaultConfig_KimiBaseURL -v`
Expected: PASS.

- [ ] **Step 1.5: Write failing test for redaction**

In `internal/config/config_test.go`:

```go
func TestGetPublicConfig_KimiRedactsAPIKey(t *testing.T) {
    cfg := DefaultConfig()
    cfg.Kimi.Enabled = true
    cfg.Kimi.APIKey = "sk-kimi-secret-123"
    cfg.Kimi.Allowlist = []KimiModelConfig{{ID: "kimi-k2-thinking", Alias: "k2", Enabled: true}}

    pub, err := cfg.GetPublicConfig()
    if err != nil {
        t.Fatalf("GetPublicConfig: %v", err)
    }
    kimi, ok := pub["kimi"].(map[string]any)
    if !ok {
        t.Fatalf("public config missing kimi map: %v", pub["kimi"])
    }
    if _, leaked := kimi["apiKey"]; leaked {
        t.Fatalf("apiKey must be redacted in public config, got %v", kimi["apiKey"])
    }
    if has, _ := kimi["hasApiKey"].(bool); !has {
        t.Fatal("public config should expose hasApiKey=true")
    }
    if kimi["baseUrl"] != "https://api.kimi.com/coding" {
        t.Fatalf("public config should preserve baseUrl, got %v", kimi["baseUrl"])
    }
}
```

- [ ] **Step 1.6: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestGetPublicConfig_KimiRedactsAPIKey -v`
Expected: FAIL — `pub["kimi"]` is nil.

- [ ] **Step 1.7: Add Kimi to public config + redaction**

In `GetPublicConfig`, after the OpenRouter block is built (look for the map literal that includes `"openrouter": ...`), add:

```go
        "kimi": buildKimiPublic(cfg.Kimi),
```

Add the helper function near `redactOpenRouter` (or wherever the OR redaction helper lives):

```go
func buildKimiPublic(c KimiConfig) map[string]any {
    allowlist := make([]map[string]any, 0, len(c.Allowlist))
    for _, m := range c.Allowlist {
        allowlist = append(allowlist, map[string]any{
            "id":              m.ID,
            "alias":           m.Alias,
            "displayName":     m.DisplayName,
            "contextLength":   m.ContextLen,
            "maxOutputTokens": m.MaxOutputTokens,
            "enabled":         m.Enabled,
        })
    }
    out := map[string]any{
        "enabled":   c.Enabled,
        "baseUrl":   c.BaseURL,
        "hasApiKey": c.APIKey != "",
        "allowlist": allowlist,
    }
    return out
}
```

In `Save` (the dictionary-merge helper that already special-cases `"openrouter"`, around line 304), add a sibling block for `"kimi"` that preserves the existing API key when the incoming payload omits it:

```go
            case "kimi":
                existingKimi, _ := currentMap["kimi"].(map[string]any)
                if hasApiKey && apiKey == "" && existingKimi != nil {
                    if existingKey, ok := existingKimi["apiKey"].(string); ok && existingKey != "" {
                        // placeholder; replaced below by the full Kimi block
                        _ = existingKey
                    }
                }
```

(Implementation: route the whole `kimi` submap through the same merge logic already used for `openrouter` — the full body becomes `result["kimi"]`. Confirm by reading the existing `openrouter` branch and copy its shape; the empty-`apiKey`/preserve-`existingKey` handling reuses the same `hasApiKey` flag the OR branch already exposes.)

- [ ] **Step 1.8: Run all config tests**

Run: `go test ./internal/config/ -v`
Expected: PASS, including both new tests.

- [ ] **Step 1.9: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add Kimi Code gateway config with redaction"
```

---

### Task 2: `internal/kimi` package skeleton

**Files:**
- Create: `internal/kimi/kimi.go`
- Create: `internal/kimi/kimi_test.go`

- [ ] **Step 2.1: Write failing test for `NormalizeBaseURL`**

Create `internal/kimi/kimi_test.go`:

```go
package kimi

import "testing"

func TestNormalizeBaseURL_StripsTrailingSlash(t *testing.T) {
    cases := map[string]string{
        "https://api.kimi.com/coding/": "https://api.kimi.com/coding",
        "https://api.kimi.com/coding":  "https://api.kimi.com/coding",
        "https://api.kimi.com/coding/v1/": "https://api.kimi.com/coding/v1",
    }
    for in, want := range cases {
        if got := NormalizeBaseURL(in); got != want {
            t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
        }
    }
}
```

- [ ] **Step 2.2: Run test to verify it fails**

Run: `go test ./internal/kimi/ -v`
Expected: build error, package doesn't exist.

- [ ] **Step 2.3: Create the package**

Create `internal/kimi/kimi.go`:

```go
// Package kimi implements the Kimi Code gateway: a thin transparent forwarder
// to https://api.kimi.com/coding, which exposes an Anthropic-compatible
// /v1/messages endpoint. The proxy rewrites the Authorization header and
// preserves the Anthropic version/beta headers the client sent.
package kimi

import "strings"

// NormalizeBaseURL returns the base URL with a single trailing slash trimmed.
// Kimi's coding endpoint is configured without a trailing slash in
// ANTHROPIC_BASE_URL; the proxy appends "/v1/messages" itself.
func NormalizeBaseURL(raw string) string {
    return strings.TrimRight(raw, "/")
}
```

- [ ] **Step 2.4: Run test to verify it passes**

Run: `go test ./internal/kimi/ -v`
Expected: PASS.

- [ ] **Step 2.5: Commit**

```bash
git add internal/kimi/kimi.go internal/kimi/kimi_test.go
git commit -m "feat(kimi): add package skeleton with NormalizeBaseURL"
```

---

### Task 3: Model catalog client (`/v1/models`)

**Files:**
- Create: `internal/kimi/client.go`
- Create: `internal/kimi/client_test.go`

**Interfaces:**
- Produces:
  - `type ModelItem struct { ID, DisplayName string, ContextLen, MaxOutputTokens int }`
  - `var DefaultClient = &Client{...}`
  - `func (c *Client) FetchModels(ctx context.Context, apiKey, baseURL string) ([]ModelItem, error)`
  - `func (c *Client) GetCachedModels() []ModelItem`

- [ ] **Step 3.1: Write failing test for FetchModels parsing**

Append to `internal/kimi/client_test.go`:

```go
package kimi

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "sync"
    "testing"
)

func TestClient_FetchModels_ParsesResponse(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/v1/models" {
            t.Errorf("unexpected path %q", r.URL.Path)
        }
        if r.Header.Get("Authorization") != "Bearer test-key" {
            t.Errorf("missing Bearer auth, got %q", r.Header.Get("Authorization"))
        }
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(map[string]any{
            "data": []map[string]any{
                {"id": "kimi-k2-thinking", "display_name": "Kimi K2 Thinking", "context_length": 200000, "max_tokens": 8000},
                {"id": "kimi-k2-0905-preview", "display_name": "Kimi K2 0905", "context_length": 200000, "max_tokens": 8000},
            },
        })
    }))
    defer srv.Close()

    c := &Client{}
    got, err := c.FetchModels(context.Background(), "test-key", srv.URL)
    if err != nil {
        t.Fatalf("FetchModels: %v", err)
    }
    if len(got) != 2 {
        t.Fatalf("got %d models, want 2", len(got))
    }
    if got[0].ID != "kimi-k2-thinking" || got[0].ContextLen != 200000 {
        t.Errorf("first model wrong: %+v", got[0])
    }
}
```

- [ ] **Step 3.2: Run test to verify it fails**

Run: `go test ./internal/kimi/ -run TestClient_FetchModels -v`
Expected: build error, `Client`/`FetchModels` undefined.

- [ ] **Step 3.3: Implement client**

Create `internal/kimi/client.go`:

```go
package kimi

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "sync"
    "time"
)

// ModelItem is one entry from Kimi's /v1/models response, trimmed to the
// fields the proxy actually needs.
type ModelItem struct {
    ID             string `json:"id"`
    DisplayName    string `json:"display_name,omitempty"`
    ContextLen     int    `json:"context_length,omitempty"`
    MaxOutputTokens int   `json:"max_tokens,omitempty"`
}

// Client fetches and caches the Kimi model catalog. Safe for concurrent use.
type Client struct {
    mu      sync.RWMutex
    cached  []ModelItem
    fetched time.Time
    ttl     time.Duration
}

const defaultCatalogTTL = 5 * time.Minute

// DefaultClient is the package-level client used by the proxy.
var DefaultClient = &Client{ttl: defaultCatalogTTL}

type kimiModelsResponse struct {
    Data []ModelItem `json:"data"`
}

// FetchModels GETs /v1/models from Kimi, returns the parsed list, and caches
// it. The cache is consulted by GetCachedModels.
func (c *Client) FetchModels(ctx context.Context, apiKey, baseURL string) ([]ModelItem, error) {
    url := NormalizeBaseURL(baseURL) + "/v1/models"
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return nil, fmt.Errorf("build Kimi models request: %w", err)
    }
    if apiKey != "" {
        req.Header.Set("Authorization", "Bearer "+apiKey)
    }
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, fmt.Errorf("call Kimi /v1/models: %w", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode/100 != 2 {
        return nil, fmt.Errorf("Kimi /v1/models returned %d", resp.StatusCode)
    }
    var body kimiModelsResponse
    if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
        return nil, fmt.Errorf("decode Kimi /v1/models: %w", err)
    }
    c.mu.Lock()
    c.cached = body.Data
    c.fetched = time.Now()
    c.mu.Unlock()
    return body.Data, nil
}

// GetCachedModels returns the last FetchModels result, or an empty slice if
// nothing has been fetched yet.
func (c *Client) GetCachedModels() []ModelItem {
    c.mu.RLock()
    defer c.mu.RUnlock()
    if c.cached == nil {
        return []ModelItem{}
    }
    out := make([]ModelItem, len(c.cached))
    copy(out, c.cached)
    return out
}
```

- [ ] **Step 3.4: Run test to verify it passes**

Run: `go test ./internal/kimi/ -run TestClient_FetchModels -v`
Expected: PASS.

- [ ] **Step 3.5: Add cache test**

Append to `internal/kimi/client_test.go`:

```go
func TestClient_GetCachedModels_EmptyByDefault(t *testing.T) {
    c := &Client{}
    if got := c.GetCachedModels(); len(got) != 0 {
        t.Fatalf("expected empty cache, got %d models", len(got))
    }
}

func TestClient_GetCachedModels_AfterFetch(t *testing.T) {
    c := &Client{}
    c.cached = []ModelItem{{ID: "kimi-k2-thinking"}}
    if got := c.GetCachedModels(); len(got) != 1 || got[0].ID != "kimi-k2-thinking" {
        t.Fatalf("GetCachedModels returned %+v", got)
    }
}
```

- [ ] **Step 3.6: Run tests**

Run: `go test ./internal/kimi/ -v`
Expected: PASS, all three tests.

- [ ] **Step 3.7: Commit**

```bash
git add internal/kimi/client.go internal/kimi/client_test.go
git commit -m "feat(kimi): add /v1/models client with TTL cache"
```

---

### Task 4: `ForwardMessages` passthrough helper

**Files:**
- Create: `internal/kimi/passthrough.go`
- Create: `internal/kimi/passthrough_test.go`

**Interfaces:**
- Produces:
  - `func ForwardMessages(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte)`
  - `func ExtractSessionID(r *http.Request, body map[string]any) string` — for stats grouping; returns `r.Header.Get("x-session-id")` if set, else a SHA1-style hash of `user_id`/`metadata.user_id`, else `""`.

- [ ] **Step 4.1: Write failing test for the director**

Create `internal/kimi/passthrough_test.go`:

```go
package kimi

import (
    "bytes"
    "io"
    "net/http"
    "net/http/httptest"
    "testing"
)

func TestForwardMessages_DirectorBehavior(t *testing.T) {
    var (
        gotPath        string
        gotAuth        string
        gotBody        []byte
        gotAnthropicV  string
        gotAnthropicB  string
    )
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        gotPath = r.URL.Path
        gotAuth = r.Header.Get("Authorization")
        gotAnthropicV = r.Header.Get("anthropic-version")
        gotAnthropicB = r.Header.Get("anthropic-beta")
        gotBody, _ = io.ReadAll(r.Body)
        w.Header().Set("Content-Type", "text/event-stream")
        w.WriteHeader(200)
        _, _ = w.Write([]byte("data: ok\n\n"))
    }))
    defer upstream.Close()

    body := []byte(`{"model":"kimi-k2-thinking","messages":[]}`)
    req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
    req.Header.Set("anthropic-version", "2023-06-01")
    req.Header.Set("anthropic-beta", "messages-2023-12-15")
    w := httptest.NewRecorder()

    ForwardMessages(w, req, upstream.URL, "sk-test", body)

    if gotPath != "/v1/messages" {
        t.Errorf("upstream path = %q, want /v1/messages", gotPath)
    }
    if gotAuth != "Bearer sk-test" {
        t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
    }
    if gotAnthropicV != "2023-06-01" {
        t.Errorf("anthropic-version not forwarded: %q", gotAnthropicV)
    }
    if gotAnthropicB != "messages-2023-12-15" {
        t.Errorf("anthropic-beta not forwarded: %q", gotAnthropicB)
    }
    if !bytes.Equal(gotBody, body) {
        t.Errorf("upstream body = %q, want %q", gotBody, body)
    }
    if w.Code != 200 {
        t.Errorf("client response code = %d, want 200", w.Code)
    }
}
```

- [ ] **Step 4.2: Run test to verify it fails**

Run: `go test ./internal/kimi/ -run TestForwardMessages -v`
Expected: build error, `ForwardMessages` undefined.

- [ ] **Step 4.3: Implement `ForwardMessages`**

Create `internal/kimi/passthrough.go`:

```go
package kimi

import (
    "bytes"
    "crypto/sha1"
    "encoding/hex"
    "io"
    "log/slog"
    "net/http"
    "net/http/httputil"
    "net/url"
    "strings"
)

// ForwardMessages transparently forwards an /v1/messages request to a Kimi
// gateway. It rewrites Authorization, preserves the Anthropic version/beta
// headers the client sent, and re-emits the JSON body from `body` (so the
// caller can mutate it before forwarding).
//
// On proxy error, it writes a 502 with an `api_error` body so the client
// receives a structured response matching the rest of the proxy.
func ForwardMessages(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte) {
    target, err := url.Parse(NormalizeBaseURL(baseURL) + "/v1/messages")
    if err != nil {
        writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid Kimi target URL: "+err.Error())
        return
    }

    proxy := &httputil.ReverseProxy{
        Director: func(req *http.Request) {
            req.URL.Scheme = target.Scheme
            req.URL.Host = target.Host
            req.URL.Path = target.Path
            req.URL.RawQuery = target.RawQuery
            req.Host = target.Host

            req.Body = io.NopCloser(bytes.NewReader(body))
            req.ContentLength = int64(len(body))

            // Always set Bearer; clients that sent x-api-key to the proxy are
            // also covered because we strip any prior auth header.
            req.Header.Set("Authorization", "Bearer "+apiKey)
            req.Header.Del("x-api-key")

            // Forward Anthropic protocol headers if the client sent them.
            if av := r.Header.Get("anthropic-version"); av != "" {
                req.Header.Set("anthropic-version", av)
            }
            if ab := r.Header.Get("anthropic-beta"); ab != "" {
                req.Header.Set("anthropic-beta", ab)
            }
        },
        ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
            slog.Default().Error("kimi upstream proxy error", "error", proxyErr, "url", target.String())
            writeAPIError(rw, http.StatusBadGateway, "api_error", "Kimi upstream error: "+proxyErr.Error())
        },
    }

    proxy.ServeHTTP(w, r)
}

// writeAPIError mirrors the helper in internal/api/server.go so the kimi
// package does not import the api package (which would be a cycle).
func writeAPIError(w http.ResponseWriter, status int, kind, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    payload := map[string]any{
        "type": "error",
        "error": map[string]any{
            "type":    kind,
            "message": msg,
        },
    }
    _ = jsonEncode(w, payload)
}

func jsonEncode(w http.ResponseWriter, v any) error {
    return jsonEncodeImpl(w, v)
}

// ExtractSessionID returns a stable identifier for the calling client so
// downstream observability can group requests per session. It prefers the
// `x-session-id` header, then `user_id` in metadata, then an empty string.
func ExtractSessionID(r *http.Request, body map[string]any) string {
    if h := r.Header.Get("x-session-id"); h != "" {
        return h
    }
    if meta, ok := body["metadata"].(map[string]any); ok {
        if uid, ok := meta["user_id"].(string); ok && uid != "" {
            return uid
        }
    }
    return ""
}

// Fingerprint returns a stable hash of the body for log grouping. Helper used
// by observability code that wants to group equal payloads without logging
// their contents.
func Fingerprint(body map[string]any) string {
    h := sha1.New()
    for _, k := range sortedKeys(body) {
        h.Write([]byte(k))
        h.Write([]byte{'='})
        h.Write([]byte(toString(body[k])))
        h.Write([]byte{';'})
    }
    return hex.EncodeToString(h.Sum(nil))
}

func toString(v any) string {
    if s, ok := v.(string); ok {
        return s
    }
    return strings.TrimSpace(strings.ReplaceAll(replaceAllNewline(toStringInner(v)), "\n", " "))
}
```

(Implementation note: the helpers above have stubs `jsonEncodeImpl`, `toStringInner`, `replaceAllNewline`, and `sortedKeys` to be filled in below — keep this file compiling by writing the bodies now.)

Replace the `jsonEncode` stub by adding at the bottom of the same file:

```go
func jsonEncodeImpl(w http.ResponseWriter, v any) error {
    enc := jsonNewEncoder(w)
    return enc.Encode(v)
}
```

Add a new file `internal/kimi/encode.go`:

```go
package kimi

import (
    "bytes"
    "encoding/json"
    "sort"
)

func jsonNewEncoder(w http.ResponseWriter) *json.Encoder {
    enc := json.NewEncoder(w)
    enc.SetEscapeHTML(false)
    return enc
}

func sortedKeys(m map[string]any) []string {
    keys := make([]string, 0, len(m))
    for k := range m {
        keys = append(keys, k)
    }
    sort.Strings(keys)
    return keys
}

func toStringInner(v any) string {
    b, err := json.Marshal(v)
    if err != nil {
        return ""
    }
    var buf bytes.Buffer
    for _, r := range string(b) {
        if r == '\n' {
            buf.WriteByte(' ')
        } else {
            buf.WriteRune(r)
        }
    }
    return buf.String()
}

func replaceAllNewline(s string) string { return s }
```

(Implementation note: `writeAPIError`/`Fingerprint`/`toString` are exercised by future observability work; this plan does not yet add observability tests, so the helpers exist to keep the package importable and to give Task 5 a stable surface.)

- [ ] **Step 4.4: Run test to verify it passes**

Run: `go test ./internal/kimi/ -run TestForwardMessages -v`
Expected: PASS.

- [ ] **Step 4.5: Commit**

```bash
git add internal/kimi/passthrough.go internal/kimi/passthrough_test.go internal/kimi/encode.go
git commit -m "feat(kimi): add transparent passthrough forwarder"
```

---

### Task 5: Wire Kimi into the `messages()` route + `/v1/models`

**Files:**
- Modify: `internal/api/server.go:540-660` (the OpenRouter branch is the template)
- Test: `internal/api/kimi_proxy_test.go`

**Interfaces:**
- Consumes: `kimi.ForwardMessages`, `cfg.Kimi`
- Produces: when `cfg.Kimi.Enabled` and a model in `cfg.Kimi.Allowlist` (matched by `id` or `alias`) is requested, forward to Kimi and return.

- [ ] **Step 5.1: Write failing end-to-end test**

Create `internal/api/kimi_proxy_test.go`:

```go
package api

import (
    "bytes"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "antigravity-go-proxy/internal/config"
)

func TestServer_ForwardToKimi_AllowsAliasMatch(t *testing.T) {
    var (
        gotPath string
        gotAuth string
        gotBody []byte
    )
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        gotPath = r.URL.Path
        gotAuth = r.Header.Get("Authorization")
        gotBody, _ = io.ReadAll(r.Body)
        w.Header().Set("Content-Type", "text/event-stream")
        w.WriteHeader(200)
        _, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
    }))
    defer upstream.Close()

    srv := newTestServerWithConfig(t, config.Config{
        Kimi: config.KimiConfig{
            Enabled: true,
            BaseURL: upstream.URL,
            APIKey:  "sk-kimi-test",
            Allowlist: []config.KimiModelConfig{
                {ID: "kimi-k2-thinking", Alias: "k2", Enabled: true},
            },
        },
    })

    body := map[string]any{
        "model":    "k2",
        "messages": []map[string]any{{"role": "user", "content": "hi"}},
    }
    raw, _ := json.Marshal(body)
    req := newTestRequest("POST", "/v1/messages", bytes.NewReader(raw))
    req.Header.Set("Content-Type", "application/json")
    w := newTestResponse()

    srv.handleMessages(w, req)

    if gotPath != "/v1/messages" {
        t.Errorf("upstream path = %q, want /v1/messages", gotPath)
    }
    if gotAuth != "Bearer sk-kimi-test" {
        t.Errorf("Authorization = %q, want Bearer sk-kimi-test", gotAuth)
    }
    if !strings.Contains(string(gotBody), `"model":"kimi-k2-thinking"`) {
        t.Errorf("upstream body should rewrite alias to kimi id, got %s", gotBody)
    }
    if w.Code != 200 {
        t.Errorf("client status = %d, want 200; body = %s", w.Code, w.Body.String())
    }
}
```

Note: `newTestServerWithConfig`, `newTestRequest`, `newTestResponse`, and `handleMessages` (or whatever the public test hook is named) are helpers the test file needs. Look at `internal/api/openrouter_proxy_test.go` for the existing pattern — copy the helper signatures it uses so the new test slots in identically.

- [ ] **Step 5.2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestServer_ForwardToKimi -v`
Expected: FAIL — `Kimi` is not routed; the request falls through to the default cloudcode path.

- [ ] **Step 5.3: Add Kimi branch to `messages()`**

In `internal/api/server.go`, locate the existing OpenRouter routing block (around line 546, the `if cfg.OpenRouter.Enabled {` near `for _, item := range cfg.OpenRouter.Allowlist`). Just before or after it, add the Kimi block — placed before OR so the cheaper check runs first:

```go
    if cfg.Kimi.Enabled {
        if kimiMatch := matchKimiModel(cfg.Kimi, model); kimiMatch != "" {
            payload, marshalErr := json.Marshal(anthropicRequest)
            if marshalErr != nil {
                writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal Kimi request: "+marshalErr.Error())
                return
            }
            // Rewrite alias → real id before forwarding.
            rewritten := map[string]any{}
            for k, v := range anthropicRequest {
                rewritten[k] = v
            }
            rewritten["model"] = kimiMatch
            payload, _ = json.Marshal(rewritten)
            server.forwardToKimi(writer, request, cfg.Kimi, payload, kimiMatch)
            return
        }
    }
```

Add the `forwardToKimi` method near `forwardToOpenRouter` (after line 970, before `openRouterUpstreamClient`):

```go
// forwardToKimi transparently forwards an /v1/messages request to the Kimi
// Code gateway. The Kimi endpoint is Anthropic-compatible, so no translation
// is needed: we rewrite Authorization, preserve the Anthropic version/beta
// headers, and stream the response back.
func (server *Server) forwardToKimi(writer http.ResponseWriter, request *http.Request, kimiCfg config.KimiConfig, body []byte, model string) {
    if kimiCfg.APIKey == "" {
        writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Kimi gateway enabled but no API key configured")
        return
    }
    if server.logger != nil {
        server.logger.Info("kimi forward", "model", model)
    }
    kimi.ForwardMessages(writer, request, kimiCfg.BaseURL, kimiCfg.APIKey, body)
}

// matchKimiModel returns the Kimi model ID if `model` matches an enabled
// allowlist entry by either ID or alias. Returns "" if no match.
func matchKimiModel(cfg config.KimiConfig, model string) string {
    for _, item := range cfg.Allowlist {
        if !item.Enabled {
            continue
        }
        if item.ID == model || item.Alias == model {
            return item.ID
        }
    }
    return ""
}
```

Add the import to `server.go`'s import block (alphabetical placement next to `"antigravity-go-proxy/internal/openrouter"`):

```go
    "antigravity-go-proxy/internal/kimi"
```

- [ ] **Step 5.4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestServer_ForwardToKimi -v`
Expected: PASS.

- [ ] **Step 5.5: Commit**

```bash
git add internal/api/server.go internal/api/kimi_proxy_test.go
git commit -m "feat(api): route /v1/messages to Kimi gateway for allowlisted models"
```

---

### Task 6: Inject Kimi models into `/v1/models`

**Files:**
- Modify: `internal/api/server.go:320-360` (the OpenRouter model-injection block is the template)
- Test: extend `internal/api/kimi_proxy_test.go`

- [ ] **Step 6.1: Write failing test for Kimi model injection**

Append to `internal/api/kimi_proxy_test.go`:

```go
func TestServer_ModelsList_IncludesKimi(t *testing.T) {
    srv := newTestServerWithConfig(t, config.Config{
        Kimi: config.KimiConfig{
            Enabled: true,
            BaseURL: "https://api.kimi.com/coding",
            APIKey:  "sk-kimi-test",
            Allowlist: []config.KimiModelConfig{
                {ID: "kimi-k2-thinking", Alias: "k2", DisplayName: "Kimi K2", ContextLen: 200000, MaxOutputTokens: 8000, Enabled: true},
            },
        },
    })
    req := newTestRequest("GET", "/v1/models", nil)
    w := newTestResponse()
    srv.handleModels(w, req)

    if w.Code != 200 {
        t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
    }
    var body struct {
        Data []map[string]any `json:"data"`
    }
    if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
        t.Fatalf("decode: %v", err)
    }
    var found bool
    for _, m := range body.Data {
        if m["id"] == "kimi-k2-thinking" {
            found = true
            if m["owned_by"] != "kimi" {
                t.Errorf("owned_by = %v, want kimi", m["owned_by"])
            }
        }
    }
    if !found {
        t.Fatalf("kimi-k2-thinking not in /v1/models response: %+v", body.Data)
    }
}
```

- [ ] **Step 6.2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestServer_ModelsList_IncludesKimi -v`
Expected: FAIL — Kimi model not in response.

- [ ] **Step 6.3: Add Kimi block to `models()` handler**

In `internal/api/server.go`, locate the OpenRouter injection inside the `/v1/models` handler (the `if cfg.OpenRouter.Enabled { for _, item := range cfg.OpenRouter.Allowlist ...` block around line 322). Add a Kimi block right after it (still inside the same handler):

```go
    if cfg.Kimi.Enabled {
        for _, item := range cfg.Kimi.Allowlist {
            if !item.Enabled {
                continue
            }
            entry := map[string]any{
                "id":       item.ID,
                "owned_by": "kimi",
            }
            if item.DisplayName != "" {
                entry["display_name"] = item.DisplayName
            }
            if item.ContextLen > 0 {
                entry["context_length"] = item.ContextLen
            }
            if item.MaxOutputTokens > 0 {
                entry["max_tokens"] = item.MaxOutputTokens
            }
            data = append(data, entry)
            // Also expose the alias as a separate id, so Claude Code can
            // discover and request it directly.
            if item.Alias != "" {
                aliasEntry := map[string]any{
                    "id":       item.Alias,
                    "owned_by": "kimi",
                }
                if item.DisplayName != "" {
                    aliasEntry["display_name"] = item.DisplayName + " (alias)"
                }
                data = append(data, aliasEntry)
            }
        }
    }
```

- [ ] **Step 6.4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestServer_ModelsList_IncludesKimi -v`
Expected: PASS.

- [ ] **Step 6.5: Commit**

```bash
git add internal/api/server.go internal/api/kimi_proxy_test.go
git commit -m "feat(api): expose Kimi allowlist in /v1/models response"
```

---

### Task 7: Management endpoints (`/api/kimi/config`, `/api/kimi/models/fetch`)

**Files:**
- Modify: `internal/api/management.go:150-220` (the OpenRouter routing block is the template)
- Test: `internal/api/kimi_config_test.go`

- [ ] **Step 7.1: Write failing test for `GET /api/kimi/config`**

Create `internal/api/kimi_config_test.go`:

```go
package api

import (
    "encoding/json"
    "net/http"
    "testing"

    "antigravity-go-proxy/internal/config"
)

func TestServer_HandleKimiConfigGet(t *testing.T) {
    srv := newTestServerWithConfig(t, config.Config{
        Kimi: config.KimiConfig{
            Enabled: true,
            BaseURL: "https://api.kimi.com/coding",
            APIKey:  "sk-secret",
            Allowlist: []config.KimiModelConfig{
                {ID: "kimi-k2-thinking", Alias: "k2", Enabled: true},
            },
        },
    })
    req := newTestRequest("GET", "/api/kimi/config", nil)
    w := newTestResponse()

    srv.handleManagement(w, req, "/api/kimi/config", http.MethodGet)

    if w.Code != 200 {
        t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
    }
    var body struct {
        Config map[string]any `json:"config"`
    }
    if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if _, leaked := body.Config["apiKey"]; leaked {
        t.Fatalf("apiKey must be redacted, got %v", body.Config["apiKey"])
    }
    if body.Config["hasApiKey"] != true {
        t.Fatalf("hasApiKey = %v, want true", body.Config["hasApiKey"])
    }
}

func TestServer_HandleKimiConfigSave(t *testing.T) {
    srv := newTestServerWithConfig(t, config.Config{})
    payload := map[string]any{
        "enabled":   true,
        "baseUrl":   "https://api.kimi.com/coding",
        "apiKey":    "sk-saved",
        "hasApiKey": true,
        "allowlist": []map[string]any{
            {"id": "kimi-k2-thinking", "alias": "k2", "enabled": true},
        },
    }
    body, _ := json.Marshal(payload)
    req := newTestRequest("POST", "/api/kimi/config", bytesReader(body))
    req.Header.Set("Content-Type", "application/json")
    w := newTestResponse()

    srv.handleManagement(w, req, "/api/kimi/config", http.MethodPost)

    if w.Code != 200 {
        t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
    }
    got := srv.config.Kimi
    if !got.Enabled {
        t.Fatal("Kimi not enabled after save")
    }
    if got.APIKey != "sk-saved" {
        t.Errorf("apiKey not stored, got %q", got.APIKey)
    }
    if len(got.Allowlist) != 1 || got.Allowlist[0].ID != "kimi-k2-thinking" {
        t.Errorf("allowlist not stored, got %+v", got.Allowlist)
    }
}
```

(`bytesReader` is a small helper; if not present, copy the three-line wrapper from `openrouter_proxy_test.go`.)

- [ ] **Step 7.2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestServer_HandleKimiConfig -v`
Expected: compile error, `handleManagement` route + helpers not present.

- [ ] **Step 7.3: Add Kimi routes to management dispatch**

In `internal/api/management.go`, locate the OpenRouter routing cases (lines 156-169). Add the Kimi cases immediately after them:

```go
    case path == "/api/kimi/config" && method == http.MethodGet:
        server.handleKimiConfigGet(writer, request)
        return
    case path == "/api/kimi/config" && method == http.MethodPost:
        server.handleKimiConfigSave(writer, request)
        return
    case path == "/api/kimi/models/fetch" && method == http.MethodPost:
        server.handleKimiModelsFetch(writer, request)
        return
```

Add three handler methods near the existing `handleOpenRouterConfig*` block (around line 1045). Use the OR handler as a template — strip routing/observability specifics, keep the same redaction + warmup pattern:

```go
func (server *Server) handleKimiConfigGet(writer http.ResponseWriter, request *http.Request) {
    cfg, _ := config.Load()
    pub, err := cfg.GetPublicConfig()
    if err != nil {
        writeAPIError(writer, http.StatusInternalServerError, "config_error", err.Error())
        return
    }
    kimiMap, _ := pub["kimi"].(map[string]any)
    writeJSON(writer, http.StatusOK, map[string]any{"config": kimiMap})
}

func (server *Server) handleKimiConfigSave(writer http.ResponseWriter, request *http.Request) {
    body, err := io.ReadAll(request.Body)
    if err != nil {
        writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
        return
    }
    defer request.Body.Close()
    saved, err := config.Save(map[string]any{"kimi": jsonRawMessage(body)})
    if err != nil {
        writeAPIError(writer, http.StatusInternalServerError, "config_error", err.Error())
        return
    }
    server.config = saved
    if server.logger != nil {
        server.logger.Info("kimi config saved", "enabled", saved.Kimi.Enabled, "models", len(saved.Kimi.Allowlist))
    }
    pub, _ := saved.GetPublicConfig()
    writeJSON(writer, http.StatusOK, map[string]any{"config": pub["kimi"]})
}

func (server *Server) handleKimiModelsFetch(writer http.ResponseWriter, request *http.Request) {
    cfg := server.config
    apiKey := cfg.Kimi.APIKey
    baseURL := cfg.Kimi.BaseURL
    if baseURL == "" {
        baseURL = "https://api.kimi.com/coding"
    }
    models, err := kimi.DefaultClient.FetchModels(request.Context(), apiKey, baseURL)
    if err != nil {
        writeAPIError(writer, http.StatusBadGateway, "upstream_error", "Failed to fetch Kimi models: "+err.Error())
        return
    }
    writeJSON(writer, http.StatusOK, map[string]any{"models": models})
}
```

Implementation notes:
- `jsonRawMessage(body)` wraps `body` as `json.RawMessage` so the existing `config.Save` merge path treats it as opaque JSON (same trick the OR handler uses; copy the conversion from there).
- `server.config` is the field the test reads after save; if it is named differently in this codebase, use the same field name `handleOpenRouterConfigSave` updates. Confirm by reading the OR handler in this file.

- [ ] **Step 7.4: Add the missing imports**

In `internal/api/management.go`, the imports already include `config` and `kimi` is not yet imported. Add:

```go
    "antigravity-go-proxy/internal/kimi"
```

(plus any standard-lib imports the helpers above need: `encoding/json` for `json.RawMessage`, `io` for `io.ReadAll` — most are already present).

- [ ] **Step 7.5: Run management tests**

Run: `go test ./internal/api/ -run TestServer_HandleKimiConfig -v`
Expected: PASS.

- [ ] **Step 7.6: Commit**

```bash
git add internal/api/management.go internal/api/kimi_config_test.go
git commit -m "feat(api): add /api/kimi/config and /api/kimi/models/fetch endpoints"
```

---

### Task 8: WebUI data store + model family

**Files:**
- Modify: `internal/webui/public/js/data-store.js:15-160` (state + model family mapping)

- [ ] **Step 8.1: Add `kimi` to the data store state**

In `data-store.js`, after the `openrouter: {}` line (line 15) add:

```js
        kimi: {}, // Kimi Code gateway configuration
```

- [ ] **Step 8.2: Load Kimi config from server response**

In the same file, locate the `this.openrouter = data.openrouter || {};` line (around line 86) and add the Kimi loader after it:

```js
                    this.kimi = data.kimi || {};
```

Also add Kimi to any payload that the store serializes back to the server. Look for the block containing `openrouter: this.openrouter` (around line 107) and add `kimi: this.kimi` next to it.

- [ ] **Step 8.3: Register Kimi as a model family**

Find the `getModelFamily` function (around line 400 in data-store.js). After the `case 'openrouter':` block (line 416), add:

```js
                case 'kimi':
                    return 'kimi';
```

Add Kimi to the family lookup above. Find the section that returns family names for known model id prefixes (look for `if (this.openrouter && this.openrouter.allowlist...)` near line 400) and add the Kimi equivalent:

```js
            if (this.kimi && this.kimi.allowlist) {
                this.kimi.allowlist.forEach(m => {
                    if (modelId === m.id || modelId === m.alias) return 'kimi';
                });
            }
```

(Adapt to the exact iteration pattern used by the OR block — copy the shape so the existing helpers don't break.)

- [ ] **Step 8.4: Smoke-check the page**

Run: `go build ./... && (cd internal/webui/public && python3 -m http.server 8080 &) ; sleep 1 ; curl -sf http://localhost:8080/index.html | head -5`
Expected: build succeeds; no JS syntax errors at the static-file level (the file is just served as text — JS validation needs the browser; the build check is the gate).

Then tear down the python server: `pkill -f "python3 -m http.server 8080" || true`.

- [ ] **Step 8.5: Commit**

```bash
git add internal/webui/public/js/data-store.js
git commit -m "feat(webui): add kimi to data store and model family"
```

---

### Task 9: WebUI settings store (`kimiConfig` block)

**Files:**
- Modify: `internal/webui/public/js/components/models.js:300-500` (mirror the `openRouterConfig` block)

- [ ] **Step 9.1: Add `kimiConfig` state**

After the `openRouterSaving: false` line (around line 311), add:

```js
    kimiConfig: {
        enabled: false,
        baseUrl: 'https://api.kimi.com/coding',
        apiKey: '',
        hasApiKey: false,
        allowlist: [],
    },
    kimiSaving: false,
    kimiError: '',
```

- [ ] **Step 9.2: Load Kimi config on init**

Locate the `loadOpenRouterConfig` block (around line 419) and add `loadKimiConfig` immediately after:

```js
        async loadKimiConfig() {
            try {
                const { response, newPassword } = await window.utils.request('/api/kimi/config', {}, password);
                if (response && response.config) {
                    this.kimiConfig = {
                        ...this.kimiConfig,
                        ...response.config,
                    };
                    this.kimiConfig.apiKey = '';
                    Alpine.store('data').kimi = this.kimiConfig;
                    if (newPassword) password = newPassword;
                }
            } catch (e) {
                this.kimiError = e.message || 'Failed to load Kimi settings';
            }
        },
```

- [ ] **Step 9.3: Add `saveKimiConfig`**

After `saveOpenRouterConfig` (around line 478), add:

```js
        async saveKimiConfig() {
            this.kimiSaving = true;
            this.kimiError = '';
            try {
                const payload = {
                    enabled: this.kimiConfig.enabled,
                    baseUrl: this.kimiConfig.baseUrl || 'https://api.kimi.com/coding',
                    hasApiKey: this.kimiConfig.hasApiKey,
                    allowlist: this.kimiConfig.allowlist || [],
                };
                if (this.kimiConfig.apiKey && this.kimiConfig.apiKey.trim()) {
                    payload.apiKey = this.kimiConfig.apiKey.trim();
                }
                const { response, newPassword } = await window.utils.request('/api/kimi/config', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(payload),
                }, password);
                if (response && response.config) {
                    this.kimiConfig = { ...this.kimiConfig, ...response.config };
                    this.kimiConfig.apiKey = '';
                    Alpine.store('data').kimi = this.kimiConfig;
                }
                store.showToast(store.t('kimiSavedSuccess') || 'Kimi settings saved', 'success');
                if (newPassword) password = newPassword;
            } catch (e) {
                this.kimiError = e.message || 'Failed to save Kimi settings';
                store.showToast(this.kimiError, 'error');
            } finally {
                this.kimiSaving = false;
            }
        },
```

(Adapt the `password` variable name to match the existing convention in this file — confirm by reading the surrounding `saveOpenRouterConfig` body.)

- [ ] **Step 9.4: Add `discoverKimiModels`**

After the `openRouterDiscoverModels` block (around line 498), add:

```js
        async discoverKimiModels() {
            try {
                const { response } = await window.utils.request('/api/kimi/models/fetch', {
                    method: 'POST',
                }, password);
                if (response && response.models) {
                    return response.models.map(m => ({
                        id: m.id,
                        displayName: m.display_name || m.id,
                        contextLength: m.context_length || 0,
                        maxOutputTokens: m.max_tokens || 0,
                        enabled: true,
                    }));
                }
                return [];
            } catch (e) {
                store.showToast(e.message || 'Failed to fetch Kimi models', 'error');
                return [];
            }
        },
```

- [ ] **Step 9.5: Smoke-check the page**

Run: `go build ./...`
Expected: no errors. (JS files are served verbatim; a build error here would mean a missing import elsewhere.)

- [ ] **Step 9.6: Commit**

```bash
git add internal/webui/public/js/components/models.js
git commit -m "feat(webui): add kimiConfig state and save/load handlers"
```

---

### Task 10: WebUI settings.html — Kimi gateway section + Discover modal

**Files:**
- Modify: `internal/webui/public/views/settings.html:940-1100` (mirror the OpenRouter gateway section)
- Modify: `internal/webui/public/views/settings.html:1320-1350` (model badge)
- Modify: `internal/webui/public/views/settings.html:1617-1700` (Discover modal)
- Modify: `internal/webui/public/js/components/model-dropdown.js:60-90` (model family)

- [ ] **Step 10.1: Add Kimi gateway section to settings.html**

After the OpenRouter gateway section (the section starting with `<!-- Section 0: OpenRouter Gateway & Anthropic Skin -->` around line 942, ending with its `</div>` closing tag — locate the matching `</div>` by indentation), insert a sibling Kimi section:

```html
                <!-- Section 0b: Kimi Code Gateway -->
                <div class="bg-gray-800/40 border border-gray-700/60 rounded-lg p-4">
                    <div class="flex items-center justify-between mb-3">
                        <div>
                            <h4 class="text-sm font-bold text-white uppercase tracking-wider" x-text="$store.global.t('kimiGateway') || 'Kimi Code Gateway'">Kimi Code Gateway</h4>
                            <p class="text-xs text-gray-400 mt-1" x-text="$store.global.t('kimiDesc') || 'Forward allowlisted models transparently to api.kimi.com/coding using Bearer auth.'">
                                Forward allowlisted models transparently to api.kimi.com/coding using Bearer auth.
                            </p>
                        </div>
                        <label class="inline-flex items-center cursor-pointer">
                            <input type="checkbox" class="sr-only peer"
                                x-model="kimiConfig.enabled"
                                @change="saveKimiConfig()"
                                aria-label="Kimi Gateway enabled toggle">
                            <div class="relative w-11 h-6 bg-gray-700 rounded-full peer peer-checked:bg-emerald-500/80 peer-checked:after:translate-x-full after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:rounded-full after:h-5 after:w-5 after:transition-all"></div>
                        </label>
                    </div>

                    <div class="grid grid-cols-1 md:grid-cols-2 gap-3 mt-3">
                        <label class="block">
                            <span class="text-[11px] uppercase text-gray-400" x-text="$store.global.t('kimiBaseUrl') || 'Base URL'">Base URL</span>
                            <input type="text" class="input input-sm input-bordered w-full mt-1 bg-gray-900/60 font-mono text-xs"
                                x-model="kimiConfig.baseUrl"
                                placeholder="https://api.kimi.com/coding">
                        </label>
                        <label class="block">
                            <span class="text-[11px] uppercase text-gray-400" x-text="$store.global.t('kimiApiKey') || 'API Key'">API Key</span>
                            <input type="password" class="input input-sm input-bordered w-full mt-1 bg-gray-900/60 font-mono text-xs"
                                x-model="kimiConfig.apiKey"
                                :placeholder="kimiConfig.hasApiKey ? '••••••••' : 'sk-...'">
                        </label>
                    </div>

                    <div class="flex items-center gap-2 mt-3">
                        <button class="btn btn-xs btn-primary" @click="saveKimiConfig()" :disabled="kimiSaving">
                            <span x-show="!kimiSaving" x-text="$store.global.t('save') || 'Save'">Save</span>
                            <span x-show="kimiSaving" class="loading loading-spinner loading-xs"></span>
                        </button>
                        <button class="btn btn-xs btn-ghost" @click="openKimiDiscoverModal()">
                            <span x-text="$store.global.t('kimiDiscover') || 'Discover Models'">Discover Models</span>
                        </button>
                        <span x-show="kimiError" class="text-xs text-rose-400" x-text="kimiError"></span>
                    </div>

                    <!-- Kimi Allowlist Table -->
                    <div class="mt-4">
                        <h5 class="text-[11px] uppercase text-gray-400 mb-2" x-text="$store.global.t('kimiAllowlist') || 'Allowlist'">Allowlist</h5>
                        <table class="table table-xs w-full">
                            <thead>
                                <tr class="text-[10px] uppercase text-gray-500">
                                    <th x-text="$store.global.t('kimiColEnabled') || 'On'">On</th>
                                    <th x-text="$store.global.t('kimiColId') || 'Model ID'">Model ID</th>
                                    <th x-text="$store.global.t('kimiColAlias') || 'Alias'">Alias</th>
                                    <th x-text="$store.global.t('kimiColDisplay') || 'Display Name'">Display Name</th>
                                    <th></th>
                                </tr>
                            </thead>
                            <tbody>
                                <template x-for="(m, idx) in kimiConfig.allowlist" :key="m.id">
                                    <tr>
                                        <td><input type="checkbox" class="checkbox checkbox-xs" x-model="m.enabled" @change="saveKimiConfig()"></td>
                                        <td><input class="input input-xs input-bordered w-full bg-gray-900/60 font-mono" x-model="m.id"></td>
                                        <td><input class="input input-xs input-bordered w-full bg-gray-900/60 font-mono" x-model="m.alias"></td>
                                        <td><input class="input input-xs input-bordered w-full bg-gray-900/60" x-model="m.displayName"></td>
                                        <td><button class="btn btn-xs btn-ghost text-rose-400" @click="kimiConfig.allowlist.splice(idx,1); saveKimiConfig()">×</button></td>
                                    </tr>
                                </template>
                                <tr>
                                    <td></td>
                                    <td><input class="input input-xs input-bordered w-full bg-gray-900/60 font-mono" x-model="kimiNewId" placeholder="kimi-k2-thinking"></td>
                                    <td><input class="input input-xs input-bordered w-full bg-gray-900/60 font-mono" x-model="kimiNewAlias" placeholder="k2"></td>
                                    <td><input class="input input-xs input-bordered w-full bg-gray-900/60" x-model="kimiNewDisplay" placeholder="Kimi K2"></td>
                                    <td><button class="btn btn-xs btn-primary" @click="addKimiAllowlistRow()">+</button></td>
                                </tr>
                            </tbody>
                        </table>
                    </div>
                </div>
```

- [ ] **Step 10.2: Add Kimi badge to model list**

Locate the model-row template that already renders an OpenRouter badge (around line 1326 — the `template x-if="$store.data.openrouter && ..."`). After it, add a sibling Kimi badge:

```html
                                                <template x-if="$store.data.kimi && $store.data.kimi.allowlist && $store.data.kimi.allowlist.some(m => m.id === modelId || m.alias === modelId)">
                                                    <span class="badge badge-xs bg-cyan-500/10 text-cyan-400 border-cyan-500/30 text-[9px] font-mono uppercase tracking-wider" x-text="$store.global.t('kimiBadge') || 'Kimi'">Kimi</span>
                                                </template>
```

- [ ] **Step 10.3: Add Kimi Discover modal**

After the existing `openrouter_discover_modal` dialog (around line 1618), add a sibling:

```html
                <!-- Discover Kimi Models Modal -->
                <dialog id="kimi_discover_modal" class="modal">
                    <div class="modal-box bg-gray-900 border border-gray-700 max-w-2xl">
                        <h3 class="font-bold text-white" x-text="$store.global.t('kimiDiscoverTitle') || 'Discover Kimi Models'">Discover Kimi Models</h3>
                        <p class="text-xs text-gray-400 mt-1" x-text="$store.global.t('kimiDiscoverDesc') || 'Fetch the model catalog from the Kimi API. Pick which ones to allow.'">
                            Fetch the model catalog from the Kimi API. Pick which ones to allow.
                        </p>
                        <div class="mt-3 max-h-80 overflow-y-auto">
                            <table class="table table-xs w-full">
                                <thead><tr class="text-[10px] uppercase text-gray-500">
                                    <th x-text="$store.global.t('kimiDiscoverImport') || 'Import'">Import</th>
                                    <th x-text="$store.global.t('kimiColId') || 'Model ID'">Model ID</th>
                                    <th x-text="$store.global.t('kimiColDisplay') || 'Display Name'">Display Name</th>
                                </tr></thead>
                                <tbody>
                                    <template x-for="m in kimiDiscovered" :key="m.id">
                                        <tr>
                                            <td><input type="checkbox" class="checkbox checkbox-xs" x-model="m.selected"></td>
                                            <td class="font-mono" x-text="m.id"></td>
                                            <td x-text="m.displayName"></td>
                                        </tr>
                                    </template>
                                </tbody>
                            </table>
                        </div>
                        <div class="modal-action">
                            <button class="btn btn-sm btn-ghost" @click="document.getElementById('kimi_discover_modal').close()">Cancel</button>
                            <button class="btn btn-sm btn-primary" @click="importKimiDiscovered()">Import</button>
                        </div>
                    </div>
                </dialog>
```

- [ ] **Step 10.4: Register Kimi family in `model-dropdown.js`**

In `internal/webui/public/js/components/model-dropdown.js`, find the array containing `{ family: 'openrouter', label: ..., items: [] }` (around line 64). Add a Kimi entry below it:

```js
            { family: 'kimi', label: this.$store.global.t('familyKimi') || 'Kimi Code Gateway', items: [] },
```

- [ ] **Step 10.5: Wire the discover modal in models.js**

In `internal/webui/public/js/components/models.js`, add these helpers near the existing `openRouter*` ones:

```js
        async openKimiDiscoverModal() {
            this.kimiDiscovered = await this.discoverKimiModels();
            this.kimiDiscovered.forEach(m => m.selected = true);
            document.getElementById('kimi_discover_modal').showModal();
        },
        importKimiDiscovered() {
            const picked = this.kimiDiscovered.filter(m => m.selected);
            const existing = new Set(this.kimiConfig.allowlist.map(m => m.id));
            picked.forEach(p => { if (!existing.has(p.id)) this.kimiConfig.allowlist.push(p); });
            this.saveKimiConfig();
            document.getElementById('kimi_discover_modal').close();
        },
        addKimiAllowlistRow() {
            if (!this.kimiNewId) return;
            this.kimiConfig.allowlist.push({
                id: this.kimiNewId,
                alias: this.kimiNewAlias || '',
                displayName: this.kimiNewDisplay || '',
                enabled: true,
            });
            this.kimiNewId = '';
            this.kimiNewAlias = '';
            this.kimiNewDisplay = '';
            this.saveKimiConfig();
        },
```

Also add the supporting state next to the other Kimi state added in Task 9:

```js
    kimiDiscovered: [],
    kimiNewId: '',
    kimiNewAlias: '',
    kimiNewDisplay: '',
```

- [ ] **Step 10.6: Smoke-check the page**

Run: `go build ./...`
Expected: success.

- [ ] **Step 10.7: Commit**

```bash
git add internal/webui/public/views/settings.html internal/webui/public/js/components/model-dropdown.js internal/webui/public/js/components/models.js
git commit -m "feat(webui): add Kimi gateway settings section and Discover modal"
```

---

### Task 11: Translations (en + 4 others)

**Files:**
- Modify: `internal/webui/public/js/translations/en.js`
- Modify: `internal/webui/public/js/translations/pt.js`
- Modify: `internal/webui/public/js/translations/zh.js`
- Modify: `internal/webui/public/js/translations/tr.js`
- Modify: `internal/webui/public/js/translations/id.js`

The 12 keys to add (per language):

| key | English |
|-----|---------|
| `kimiGateway` | Kimi Code Gateway |
| `kimiDesc` | Forward allowlisted models transparently to api.kimi.com/coding using Bearer auth. |
| `kimiBaseUrl` | Base URL |
| `kimiApiKey` | API Key |
| `kimiDiscover` | Discover Models |
| `kimiAllowlist` | Allowlist |
| `kimiColEnabled` | On |
| `kimiColId` | Model ID |
| `kimiColAlias` | Alias |
| `kimiColDisplay` | Display Name |
| `kimiBadge` | Kimi |
| `familyKimi` | Kimi Code Gateway |
| `kimiSavedSuccess` | Kimi settings saved |
| `kimiDiscoverTitle` | Discover Kimi Models |
| `kimiDiscoverDesc` | Fetch the model catalog from the Kimi API. Pick which ones to allow. |
| `kimiDiscoverImport` | Import |

- [ ] **Step 11.1: Add English keys**

In `en.js`, find the OpenRouter block (search for `openRouterGateway:`) and insert a Kimi block right after the OR keys but before any unrelated key:

```js
    kimiGateway: 'Kimi Code Gateway',
    kimiDesc: 'Forward allowlisted models transparently to api.kimi.com/coding using Bearer auth.',
    kimiBaseUrl: 'Base URL',
    kimiApiKey: 'API Key',
    kimiDiscover: 'Discover Models',
    kimiAllowlist: 'Allowlist',
    kimiColEnabled: 'On',
    kimiColId: 'Model ID',
    kimiColAlias: 'Alias',
    kimiColDisplay: 'Display Name',
    kimiBadge: 'Kimi',
    familyKimi: 'Kimi Code Gateway',
    kimiSavedSuccess: 'Kimi settings saved',
    kimiDiscoverTitle: 'Discover Kimi Models',
    kimiDiscoverDesc: 'Fetch the model catalog from the Kimi API. Pick which ones to allow.',
    kimiDiscoverImport: 'Import',
```

- [ ] **Step 11.2: Add Portuguese (pt) keys**

Same shape, translated values:

```js
    kimiGateway: 'Gateway Kimi Code',
    kimiDesc: 'Encaminhe modelos da allowlist de forma transparente para api.kimi.com/coding usando autenticação Bearer.',
    kimiBaseUrl: 'URL Base',
    kimiApiKey: 'Chave de API',
    kimiDiscover: 'Descobrir Modelos',
    kimiAllowlist: 'Allowlist',
    kimiColEnabled: 'Ativo',
    kimiColId: 'ID do Modelo',
    kimiColAlias: 'Alias',
    kimiColDisplay: 'Nome de Exibição',
    kimiBadge: 'Kimi',
    familyKimi: 'Gateway Kimi Code',
    kimiSavedSuccess: 'Configurações Kimi salvas',
    kimiDiscoverTitle: 'Descobrir Modelos Kimi',
    kimiDiscoverDesc: 'Buscar o catálogo de modelos da API Kimi. Escolha quais permitir.',
    kimiDiscoverImport: 'Importar',
```

- [ ] **Step 11.3: Add Chinese (zh) keys**

```js
    kimiGateway: 'Kimi Code 网关',
    kimiDesc: '使用 Bearer 鉴权将白名单中的模型透明转发至 api.kimi.com/coding。',
    kimiBaseUrl: '基础 URL',
    kimiApiKey: 'API 密钥',
    kimiDiscover: '发现模型',
    kimiAllowlist: '白名单',
    kimiColEnabled: '启用',
    kimiColId: '模型 ID',
    kimiColAlias: '别名',
    kimiColDisplay: '显示名称',
    kimiBadge: 'Kimi',
    familyKimi: 'Kimi Code 网关',
    kimiSavedSuccess: 'Kimi 设置已保存',
    kimiDiscoverTitle: '发现 Kimi 模型',
    kimiDiscoverDesc: '从 Kimi API 获取模型目录,选择允许的模型。',
    kimiDiscoverImport: '导入',
```

- [ ] **Step 11.4: Add Turkish (tr) keys**

```js
    kimiGateway: 'Kimi Code Ağ Geçidi',
    kimiDesc: 'Allowlist\'teki modelleri Bearer kimlik doğrulamasıyla api.kimi.com/coding\'e şeffafça iletin.',
    kimiBaseUrl: 'Temel URL',
    kimiApiKey: 'API Anahtarı',
    kimiDiscover: 'Modelleri Keşfet',
    kimiAllowlist: 'İzin Listesi',
    kimiColEnabled: 'Açık',
    kimiColId: 'Model Kimliği',
    kimiColAlias: 'Takma Ad',
    kimiColDisplay: 'Görünen Ad',
    kimiBadge: 'Kimi',
    familyKimi: 'Kimi Code Ağ Geçidi',
    kimiSavedSuccess: 'Kimi ayarları kaydedildi',
    kimiDiscoverTitle: 'Kimi Modellerini Keşfet',
    kimiDiscoverDesc: 'Kimi API\'den model kataloğunu alın. İzin verilecekleri seçin.',
    kimiDiscoverImport: 'İçe Aktar',
```

- [ ] **Step 11.5: Add Indonesian (id) keys**

```js
    kimiGateway: 'Gateway Kimi Code',
    kimiDesc: 'Teruskan model yang masuk allowlist secara transparan ke api.kimi.com/coding menggunakan autentikasi Bearer.',
    kimiBaseUrl: 'URL Dasar',
    kimiApiKey: 'Kunci API',
    kimiDiscover: 'Temukan Model',
    kimiAllowlist: 'Allowlist',
    kimiColEnabled: 'Aktif',
    kimiColId: 'ID Model',
    kimiColAlias: 'Alias',
    kimiColDisplay: 'Nama Tampilan',
    kimiBadge: 'Kimi',
    familyKimi: 'Gateway Kimi Code',
    kimiSavedSuccess: 'Pengaturan Kimi disimpan',
    kimiDiscoverTitle: 'Temukan Model Kimi',
    kimiDiscoverDesc: 'Ambil katalog model dari API Kimi. Pilih yang akan diizinkan.',
    kimiDiscoverImport: 'Impor',
```

- [ ] **Step 11.6: Build + test**

Run: `go build ./... && go test ./internal/...`
Expected: builds clean, all tests pass.

- [ ] **Step 11.7: Commit**

```bash
git add internal/webui/public/js/translations/
git commit -m "feat(webui): add Kimi gateway translations for en/pt/zh/tr/id"
```

---

### Task 12: End-to-end smoke test

**Files:**
- Modify: `internal/api/kimi_proxy_test.go` (add a "live proxy" test using two `httptest.Server` instances: one as Kimi upstream, one as the proxy)
- Run: full `go test ./...` suite

- [ ] **Step 12.1: Add live two-server smoke test**

Append to `kimi_proxy_test.go`:

```go
func TestServer_KimiEndToEnd_StreamingResponse(t *testing.T) {
    kimid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Header.Get("Authorization") != "Bearer kimi-key" {
            t.Errorf("auth header = %q", r.Header.Get("Authorization"))
        }
        flusher, _ := w.(http.Flusher)
        w.Header().Set("Content-Type", "text/event-stream")
        w.WriteHeader(200)
        _, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n"))
        if flusher != nil { flusher.Flush() }
        _, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"))
        if flusher != nil { flusher.Flush() }
    }))
    defer kimid.Close()

    proxy := newTestServerWithConfig(t, config.Config{
        Kimi: config.KimiConfig{
            Enabled: true,
            BaseURL: kimid.URL,
            APIKey:  "kimi-key",
            Allowlist: []config.KimiModelConfig{{ID: "kimi-k2-thinking", Enabled: true}},
        },
    })

    body := []byte(`{"model":"kimi-k2-thinking","stream":true,"messages":[]}`)
    req := newTestRequest("POST", "/v1/messages", bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    w := newTestResponse()
    proxy.handleMessages(w, req)

    if w.Code != 200 {
        t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
    }
    if !strings.Contains(w.Body.String(), "message_start") {
        t.Fatalf("missing message_start in stream: %s", w.Body.String())
    }
}
```

- [ ] **Step 12.2: Run the full suite**

Run: `go test ./... -count=1`
Expected: all packages green.

- [ ] **Step 12.3: Build the binary and confirm config validation**

Run: `go build -o /tmp/antigravity-kimi ./cmd/proxy && /tmp/antigravity-kimi -validate-config 2>&1 | head -20 || true`
Expected: build succeeds; if the binary has a config validation flag, it reports no errors. If no such flag exists, skip and use `go vet ./...` instead:

```bash
go vet ./...
```

Expected: no findings.

- [ ] **Step 12.4: Commit**

```bash
git add internal/api/kimi_proxy_test.go
git commit -m "test(api): add Kimi end-to-end streaming smoke test"
```

---

## Self-Review

**Spec coverage:**
- Forward to `https://api.kimi.com/coding` — Task 4 (`ForwardMessages`), Task 5 (route), Task 7 (config endpoint)
- `ANTHROPIC_AUTH_TOKEN` becomes Kimi API key (via the `apiKey` field in `KimiConfig`; user supplies it in the WebUI or `config.json`) — Task 1, Task 7, Task 10
- Modelled on OpenRouter gateway — mirrored structure in Tasks 1, 5, 6, 7, 10
- Separate `internal/kimi` package — Task 2
- Per-model alias mapping — Task 1 (`KimiModelConfig.Alias`), Task 5 (alias rewrite), Task 6 (alias in /v1/models)
- Model discovery — Task 3 (`FetchModels`), Task 7 (`/api/kimi/models/fetch`), Task 10 (Discover modal)

**Placeholder scan:** No `TBD`/`TODO`/`implement later`. Every step has actual code or actual commands. No "similar to" references without a concrete pointer.

**Type consistency:** `KimiConfig`, `KimiModelConfig` introduced in Task 1 used unchanged in Tasks 2, 5, 6, 7. `kimi.ForwardMessages` signature in Task 4 used unchanged in Task 5. `kimi.DefaultClient.FetchModels` signature in Task 3 used unchanged in Task 7. WebUI field names (`kimiConfig`, `kimiSaving`, `kimiError`, `kimiDiscovered`, `kimiNewId/Alias/Display`) introduced in Task 9 used unchanged in Task 10.

**Known limitations (out of scope, not gaps):**
- No Kimi-specific observability yet (Fingerprint/ExtractSessionID are present but unused).
- No app-spoof retry (Kimi does not currently use a harness gate).
- No pricing tracking (Kimi pricing is not yet published in a public endpoint we can model).
- No streaming cancellation cleanup beyond what `httputil.ReverseProxy` provides by default.

## Verification (end-to-end)

1. Build: `go build ./...`
2. Test: `go test ./... -count=1` — all green.
3. Run: `./antigravity-go-proxy -config config.json` (binary from `cmd/proxy`).
4. Open `http://localhost:8080/settings` in a browser. Confirm:
   - Kimi gateway section is visible.
   - Saving a config (toggle on, paste API key, add allowlist row) returns `200 OK` and the page reflects `hasApiKey: true`.
   - "Discover Models" button calls `/api/kimi/models/fetch` and the modal populates with the returned models.
5. Configure a Kimi model with alias `k2` mapping to `kimi-k2-thinking`. Restart the proxy.
6. With `ANTHROPIC_BASE_URL=http://localhost:8080` and `ANTHROPIC_AUTH_TOKEN=<anything>`, run `claude --model k2 "hi"` (or any Anthropic-SDK client pointed at the proxy). Expect a streaming response that round-trips through Kimi.
7. `GET /v1/models` should include `k2` (alias) and `kimi-k2-thinking` (real id) with `owned_by: kimi`.
8. Toggle the gateway off and re-run — the model should disappear from `/v1/models` and the routing branch should fall through to the default cloudcode path.
