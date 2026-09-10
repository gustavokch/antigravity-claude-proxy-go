# PR #66 Review Remediation Plan

- **PR:** #66 (`feat(openrouter): add balance monitoring to dashboard`)
- **Date:** 2026-09-10
- **Goal:** Remediate security, correctness, resource, and UI defects identified during PR #66 code review.
- **Tech Stack:** Go (HTTP server, client, tests), JavaScript (Alpine.js, Tailwind CSS).

---

## Tasks

### Task 1: Key OpenRouter Credits Cache and Singleflight by API Key and Base URL

- **Target Files:**
  - Modify: `internal/openrouter/client.go`
  - Test: `internal/openrouter/client_test.go`
- **Interfaces:**
  - `Client.creditsCache`: `map[string]*creditsCacheEntry`
  - `Client.creditsFlightMap`: `map[string]*creditsCall`
  - Key helper: `creditsKey(cleanBase, apiKey string) string`
- **Step 1 (Red):** Add `TestResolveCredits_APIKeyIsolation` in `internal/openrouter/client_test.go` testing that different API keys querying the same base URL do not share cached balances or coalesced in-flight calls.
- **Step 2:** Run test to verify failure (`go test ./internal/openrouter -run TestResolveCredits_APIKeyIsolation`).
- **Step 3 (Green):** Update `Client` to store credits cache per `creditsKey` and singleflight calls per `creditsKey`.
- **Step 4:** Run tests to confirm pass (`go test ./internal/openrouter -run TestResolveCredits`).
- **Step 5:** Git commit:
  ```bash
  git add internal/openrouter/client.go internal/openrouter/client_test.go
  git commit -m "fix(openrouter): isolate credits cache and in-flight calls by api key"
  ```

---

### Task 2: Bound Credits Response Body Read to Prevent OOM

- **Target Files:**
  - Modify: `internal/openrouter/client.go`
  - Test: `internal/openrouter/client_test.go`
- **Interfaces:**
  - `io.LimitReader(resp.Body, 1<<20)` in `FetchCredits`
- **Step 1 (Red):** Add test `TestFetchCredits_OversizedBodyTruncated` in `internal/openrouter/client_test.go` ensuring body reads are bounded at 1MB.
- **Step 2:** Run test to verify failure (`go test ./internal/openrouter -run TestFetchCredits_OversizedBodyTruncated`).
- **Step 3 (Green):** Wrap `resp.Body` with `io.LimitReader(resp.Body, 1<<20)` in `FetchCredits`.
- **Step 4:** Run tests to confirm pass (`go test ./internal/openrouter -run TestFetchCredits`).
- **Step 5:** Git commit:
  ```bash
  git add internal/openrouter/client.go internal/openrouter/client_test.go
  git commit -m "fix(openrouter): bound credits response body read to 1MB"
  ```

---

### Task 3: Handle Upstream Error Responses and Enable Dashboard Retry

- **Target Files:**
  - Modify: `internal/webui/public/js/components/dashboard.js`
  - Modify: `internal/webui/public/js/data-store.js`
- **Interfaces:**
  - `dashboard.openrouterCreditsAvailable()`: return true when OpenRouter enabled and API key configured (allowing retry even when in error or management key prompt state).
  - `data-store.fetchOpenRouterCredits()`: populate error payload on non-200 HTTP responses.
- **Step 1 (Red/Verify):** Inspect current behavior where 502 Bad Gateway response keeps stale balance and hides refresh button.
- **Step 2 (Green):**
  - Update `data-store.js` to catch non-OK responses and update `openrouterCredits` with `{ status: 'error', error: ... }`.
  - Update `dashboard.js` so `openrouterBalanceText()` formats error states and `openrouterCreditsAvailable()` allows retry when configured.
- **Step 3:** Commit UI fixes:
  ```bash
  git add internal/webui/public/js/components/dashboard.js internal/webui/public/js/data-store.js
  git commit -m "fix(webui): record error state and enable retry on credits failure"
  ```

---

### Task 4: Full Verification and Push

- **Step 1:** Run `go test ./...` across entire project.
- **Step 2:** Push to `origin feat/openrouter-balance-dashboard`.
