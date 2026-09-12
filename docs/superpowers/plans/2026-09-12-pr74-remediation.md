# PR #74 Remediation Plan: Kimi 1M Suffix Handling, Case-Insensitive Matching, Upstream Model Sanitization, and Robust FetchModels BaseURL

**Goal:** Remediate review findings on PR #74: prevent false matching of bare `[1m]` models, sanitize upstream forwarded model ID when allowlist item has `[1m]`, support case-insensitive matching in `matchKimiModelEntry`, and normalize `/anthropic` path case-insensitively in `FetchModels`.

**Architecture:**
- `internal/api/server.go`:
  - Update `stripKimi1mSuffix`: only strip suffix when `len(trimmed) > 4` and suffix is `[1m]`; bare `"[1m]"` remains `"[1m]"`.
  - Update `matchKimiModelEntry`: use `strings.EqualFold` for case-insensitive matching of ID and alias; ensure `cleanModel != ""` before matching against stripped item values.
  - In `server.go` route handler: assign `stripKimi1mSuffix(kimiEntry.ID)` to `anthropicRequest["model"]` and pass it to `server.forwardToKimi` so upstream Moonshot always receives a clean model ID.
  - Update `matchKimiModel`: return `stripKimi1mSuffix(item.ID)`.
- `internal/kimi/client.go`:
  - In `FetchModels`: trim `/anthropic` case-insensitively and trim any resulting trailing slashes.
- `internal/api/kimi_proxy_test.go` & `internal/kimi/client_test.go`:
  - Add comprehensive test cases covering bare `[1m]`, case insensitivity (`[1M]`, uppercase IDs), allowlist items with `[1m]`, upstream payload rewrite, and `/Anthropic` case variation in `FetchModels`.

**Tech Stack:** Go (1.27rc2/1.23+), HTTP reverse proxy, Moonshot/Kimi API.

---

### Task 1: Fix stripKimi1mSuffix, matchKimiModelEntry, and Upstream Model Sanitization

- **Target files:**
  - Modify: `internal/api/server.go`
  - Test: `internal/api/kimi_proxy_test.go`
- **Interfaces:**
  - `matchKimiModelEntry(cfg config.KimiConfig, model string) (config.KimiModelConfig, bool)`
  - `stripKimi1mSuffix(s string) string`
  - `matchKimiModel(cfg config.KimiConfig, model string) string`

- **Step 1: Write failing tests**
  - In `internal/api/kimi_proxy_test.go`:
    - Test `matchKimiModel(cfg, "[1m]") == ""` (bare `[1m]` must not match).
    - Test case-insensitive matching: `matchKimiModel(cfg, "kimi-k2-thinking[1M]") == "kimi-k2-thinking"`.
    - Test uppercase alias matching: `matchKimiModel(cfg, "K2[1M]") == "kimi-k2-thinking"` and `matchKimiModel(cfg, "K2") == "kimi-k2-thinking"`.
    - Test allowlist item with `[1m]` ID: allowlist item `{"id": "kimi-k2-thinking[1m]", "alias": "k2"}`; when matching `k2`, `matchKimiModel` returns `"kimi-k2-thinking"`.
    - Test `forwardToKimi` rewrites model in forwarded body to Moonshot without `[1m]`.

- **Step 2: Run tests to confirm failure**
  - Command: `go test -v ./internal/api -run "TestMatchKimiModel_EdgeCases|TestServer_ForwardToKimi_Strips1mInPayload"`

- **Step 3: Implementation**
  - In `internal/api/server.go`:
    - Update `stripKimi1mSuffix(s string) string`:
      ```go
      func stripKimi1mSuffix(s string) string {
          trimmed := strings.TrimSpace(s)
          lower := strings.ToLower(trimmed)
          if len(trimmed) > 4 && strings.HasSuffix(lower, "[1m]") {
              return strings.TrimSpace(trimmed[:len(trimmed)-4])
          }
          return trimmed
      }
      ```
    - Update `matchKimiModelEntry(cfg config.KimiConfig, model string) (config.KimiModelConfig, bool)`:
      Use `strings.EqualFold` and guard `cleanModel != ""`:
      ```go
      func matchKimiModelEntry(cfg config.KimiConfig, model string) (config.KimiModelConfig, bool) {
          cleanModel := stripKimi1mSuffix(model)
          if cleanModel == "" {
              return config.KimiModelConfig{}, false
          }
          for _, item := range cfg.Allowlist {
              if !item.Enabled {
                  continue
              }
              itemIDClean := stripKimi1mSuffix(item.ID)
              itemAliasClean := stripKimi1mSuffix(item.Alias)
              if (item.ID != "" && (strings.EqualFold(item.ID, model) || strings.EqualFold(item.ID, cleanModel) || strings.EqualFold(itemIDClean, cleanModel))) ||
                  (item.Alias != "" && (strings.EqualFold(item.Alias, model) || strings.EqualFold(item.Alias, cleanModel) || strings.EqualFold(itemAliasClean, cleanModel))) {
                  return item, true
              }
          }
          return config.KimiModelConfig{}, false
      }
      ```
    - In `matchKimiModel`: return `stripKimi1mSuffix(item.ID)`.
    - In `server.go` route handler: sanitize `targetModel := stripKimi1mSuffix(kimiEntry.ID)`, set `anthropicRequest["model"] = targetModel` and pass `targetModel` to `server.forwardToKimi`.

- **Step 4: Run tests to confirm pass**
  - Command: `go test -v ./internal/api -run "TestMatchKimiModel_|TestServer_ForwardToKimi_"`

- **Step 5: Git commit**
  - `git commit -am "fix(kimi): robust 1m suffix handling, case-insensitive match, and upstream model sanitization"`

---

### Task 2: Case-Insensitive BaseURL Trimming in FetchModels

- **Target files:**
  - Modify: `internal/kimi/client.go`
  - Test: `internal/kimi/client_test.go`
- **Interfaces:**
  - `(c *Client) FetchModels(ctx context.Context, apiKey, baseURL string) ([]ModelItem, error)`

- **Step 1: Write failing tests**
  - In `internal/kimi/client_test.go`:
    - Add test with case variation `srv.URL + "/Anthropic"`.
    - Add test with trailing slash `srv.URL + "/anthropic/"`.
    - Add test without `/anthropic` path `srv.URL`.

- **Step 2: Run tests to confirm failure**
  - Command: `go test -v ./internal/kimi -run "TestClient_FetchModels_BaseURLVariations"`

- **Step 3: Implementation**
  - In `internal/kimi/client.go`:
    ```go
    base := NormalizeBaseURL(baseURL)
    if strings.HasSuffix(strings.ToLower(base), "/anthropic") {
        base = base[:len(base)-len("/anthropic")]
    }
    base = strings.TrimRight(base, "/")
    url := base + "/v1/models"
    ```

- **Step 4: Run tests to confirm pass**
  - Command: `go test -v ./internal/kimi -run "TestClient_FetchModels_"`

- **Step 5: Git commit**
  - `git commit -am "fix(kimi): case-insensitive /anthropic trimming in FetchModels"`

---

### Task 3: Full Verification & PR Push

- Run `go test ./...` and `go test -race ./...`.
- Push commits to PR #74 branch `feat/kimi-gateway-update`.
