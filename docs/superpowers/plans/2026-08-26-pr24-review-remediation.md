# PR #24 Review Remediation Plan

## Goal
Remediate findings from code review of PR #24 (`feat(kimi): add Kimi Code gateway`):
1. Fix ReverseProxy streaming flush behavior with `FlushInterval: -1`.
2. Harden `NormalizeBaseURL` with whitespace trimming, default URL, and `/v1` suffix stripping.
3. Harden `kimi.Client` with configured HTTP timeout and `IsCacheValid` check.
4. Add empty string guard to `matchKimiModel`.

## Architecture & Interfaces
- `internal/kimi`:
  - `NormalizeBaseURL(raw string) string`
  - `ForwardMessages(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte)`
  - `Client`: `httpClient`, `FetchModels`, `GetCachedModels`, `IsCacheValid`, `NewClient`
- `internal/api`:
  - `matchKimiModel(cfg config.KimiConfig, model string) string`

---

## Task 1: Fix ReverseProxy FlushInterval in `internal/kimi/passthrough.go`
- **Files to modify**: `internal/kimi/passthrough.go`, `internal/kimi/passthrough_test.go`
- **TDD Steps**:
  1. Add test asserting `FlushInterval: -1` or verifying streaming behavior.
  2. Run `go test ./internal/kimi/... -count=1`.
  3. Add `FlushInterval: -1` in `ForwardMessages`.
  4. Run `go test ./internal/kimi/... -count=1` and verify pass.
  5. Commit `fix(kimi): set FlushInterval -1 on reverse proxy for immediate SSE flushing`.

---

## Task 2: Harden `NormalizeBaseURL` in `internal/kimi/kimi.go`
- **Files to modify**: `internal/kimi/kimi.go`, `internal/kimi/kimi_test.go`
- **TDD Steps**:
  1. Add test cases for whitespace, empty string, and trailing `/v1` / `/v1/`.
  2. Run `go test ./internal/kimi/... -count=1` to confirm failure on `/v1` stripping.
  3. Update `NormalizeBaseURL` to trim space, default empty string to `https://api.kimi.com/coding`, and strip `/v1`.
  4. Run `go test ./internal/kimi/... -count=1` and verify pass.
  5. Commit `fix(kimi): handle whitespace and strip /v1 in NormalizeBaseURL`.

---

## Task 3: Harden `kimi.Client` with HTTP Client Timeout & Cache Validity
- **Files to modify**: `internal/kimi/client.go`, `internal/kimi/client_test.go`
- **TDD Steps**:
  1. Add test for `IsCacheValid` and `NewClient` custom timeout/TTL.
  2. Run `go test ./internal/kimi/... -count=1` to confirm compilation/failure.
  3. Implement `NewClient`, `httpClient` field, and `IsCacheValid` in `internal/kimi/client.go`.
  4. Run `go test ./internal/kimi/... -count=1` and verify pass.
  5. Commit `fix(kimi): configure http client timeout and cache validation on Kimi client`.

---

## Task 4: Add Empty String Guard to `matchKimiModel`
- **Files to modify**: `internal/api/server.go`, `internal/api/kimi_proxy_test.go`
- **TDD Steps**:
  1. Add unit test asserting `matchKimiModel` returns `""` for empty model string and empty allowlist entry IDs.
  2. Run `go test ./internal/api/... -count=1`.
  3. Update `matchKimiModel` in `internal/api/server.go`.
  4. Run `go test ./internal/api/... -count=1` and verify pass.
  5. Commit `fix(api): guard matchKimiModel against empty model strings`.
