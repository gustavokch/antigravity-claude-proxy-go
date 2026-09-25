# Remediation Plan: PR #97 Review Fixes

**Date**: 2026-09-24  
**PR**: #97 (`fix(cloudcode): resolve upstream 429 fallback and corrupted thought signature`)  
**Branch**: `fix/upstream-429-thought-signature`  

## Goal
Remediate findings identified in code review:
1. Prevent nil-pointer dereference panic when unwrapping typed-nil `*HTTPError` pointers in `FindHTTPError`.
2. Fix priority inversion by ensuring deterministic auth/permission errors (HTTP 401 and 403) take precedence over generic HTTP 5xx server errors in joined error sets.
3. Tighten loose test assertions in `internal/api/server_test.go` and update endpoint test naming in `internal/cloudcode/client_test.go`.

## Architecture & Tech Stack
- Go 1.27rc2 standard library (`net/http`, `errors`, `testing`).
- `internal/cloudcode`: HTTP client and error unwrapper.
- `internal/api`: Anthropic-compatible API server and `Retry-After` calculation.

---

## Tasks

### Task 1: Guard Against Typed-Nil `*HTTPError` in `FindHTTPError`

- **Target Files**:
  - `internal/cloudcode/client_test.go`
  - `internal/cloudcode/client.go`
- **Consumes / Produces**:
  - `cloudcode.FindHTTPError(err error) *HTTPError`
- **Step 1: Write failing test**:
  In `internal/cloudcode/client_test.go`, test `FindHTTPError` with a typed nil `(*HTTPError)(nil)` and a joined error containing a typed nil:
  ```go
  var nilHTTP *HTTPError
  var typedNilErr error = nilHTTP
  if got := FindHTTPError(typedNilErr); got != nil {
      t.Errorf("expected nil for typed nil HTTPError, got %v", got)
  }
  joinedNil := errors.Join(typedNilErr, err429)
  if got := FindHTTPError(joinedNil); got != err429 {
      t.Errorf("expected err429 for joined nil, got %v", got)
  }
  ```
- **Step 2: Run test to confirm failure**:
  `go test -v -run TestFindHTTPError ./internal/cloudcode`
- **Step 3: Minimal implementation**:
  In `internal/cloudcode/client.go`, update `collectHTTPErrors` to only append when `httpErr != nil`:
  ```go
  if httpErr, ok := err.(*HTTPError); ok {
      if httpErr != nil {
          *out = append(*out, httpErr)
      }
      return
  }
  ```
  And in `FindHTTPError`:
  ```go
  if errors.As(err, &upstreamError) && upstreamError != nil {
      return upstreamError
  }
  ```
- **Step 4: Run test to confirm pass**:
  `go test -v -run TestFindHTTPError ./internal/cloudcode`
- **Step 5: Git commit**:
  `git commit -m "fix(cloudcode): guard against typed nil HTTPError in FindHTTPError"`

---

### Task 2: Prioritize HTTP 401 and 403 over HTTP 5xx in `FindHTTPError`

- **Target Files**:
  - `internal/cloudcode/client_test.go`
  - `internal/cloudcode/client.go`
- **Consumes / Produces**:
  - `cloudcode.FindHTTPError(err error) *HTTPError`
- **Step 1: Write failing test**:
  In `internal/cloudcode/client_test.go`, add test cases where 500 is joined with 401 or 403:
  ```go
  err401 := &HTTPError{StatusCode: http.StatusUnauthorized, Status: "401", Body: "Unauthorized"}
  err403 := &HTTPError{StatusCode: http.StatusForbidden, Status: "403", Body: "Forbidden"}
  joined500And401 := errors.Join(err500, err401)
  if got := FindHTTPError(joined500And401); got != err401 {
      t.Errorf("got status %d, want 401", got.StatusCode)
  }
  joined500And403 := errors.Join(err500, err403)
  if got := FindHTTPError(joined500And403); got != err403 {
      t.Errorf("got status %d, want 403", got.StatusCode)
  }
  ```
- **Step 2: Run test to confirm failure**:
  `go test -v -run TestFindHTTPError ./internal/cloudcode`
- **Step 3: Minimal implementation**:
  In `internal/cloudcode/client.go:FindHTTPError`, reorder loops so 401/403 precedes 5xx:
  ```go
  for _, httpErr := range httpErrors {
      if httpErr.StatusCode == http.StatusTooManyRequests {
          return httpErr
      }
  }
  for _, httpErr := range httpErrors {
      if httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden {
          return httpErr
      }
  }
  for _, httpErr := range httpErrors {
      if httpErr.StatusCode >= 500 {
          return httpErr
      }
  }
  return httpErrors[0]
  ```
- **Step 4: Run test to confirm pass**:
  `go test -v -run TestFindHTTPError ./internal/cloudcode`
- **Step 5: Git commit**:
  `git commit -m "fix(cloudcode): prioritize 401 and 403 over 5xx in FindHTTPError"`

---

### Task 3: Tighten Assertions and Test Documentation

- **Target Files**:
  - `internal/api/server_test.go`
  - `internal/cloudcode/client_test.go`
- **Consumes / Produces**:
  - Test suite assertions.
- **Step 1: Write updated assertions**:
  In `internal/api/server_test.go`, assert exact fallback backoff `got != 30`:
  ```go
  if got != 30 {
      t.Fatalf("retryAfterSeconds should provide 30s fallback, got %d", got)
  }
  ```
  In `internal/cloudcode/client_test.go`, update test name/comment for `TestContentAndProvisioningEndpoints`.
- **Step 2: Run test suite**:
  `go test -v ./internal/api ./internal/cloudcode`
- **Step 3: Git commit**:
  `git commit -m "test(cloudcode,api): tighten backoff assertions and clarify endpoint tests"`
