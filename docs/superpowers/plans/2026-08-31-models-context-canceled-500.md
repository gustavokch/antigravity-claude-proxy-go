# `/v1/models` `context canceled` Misclassification Fix — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `/v1/models` returning `500 api_error` when the real condition is a client disconnect or an upstream deadline; keep shared catalog fetch work alive across client disconnects.

**Architecture:** Two layers. (1) API layer: `classifyError` gains `context.Canceled` and `context.DeadlineExceeded` branches so canceled requests map to `499` (nginx "client closed request") and deadlines map to `504`; `writeError` logs context errors at Warn, not Error. (2) Dispatcher layer: `Dispatcher.FetchAvailableModels` runs the upstream catalog fetch on a background context bounded by a timeout (openrouter precedent, `internal/openrouter/client.go:229`), so a client disconnect cannot cancel shared work (OAuth refresh, catalog cache, quota update); concurrent callers share one fetch via a hand-rolled singleflight.

**Tech Stack:** Go stdlib only (`net/http`, `context`, `sync`, `log/slog`). No new dependencies.

**Spec:** Handoff `/private/tmp/claude-502/-Users-gus-Git-antigravity-claude-proxy-go/ca4f49ab-ee73-485d-8395-682f4a5c09bb/scratchpad/handoff-models-500.md` (prior session, systematic-debugging complete). Condensed spec:

- **Root cause (confirmed):** `classifyError` (`internal/api/server.go:2349`) has no branch for `context.Canceled` / `context.DeadlineExceeded`; both fall to the default `500, "api_error"` (`server.go:2372`). Every canceled `/v1/models` request therefore logs `[GET] /v1/models 500` and `ERROR API request failed error=context canceled`.
- **Call chain:** `server.models` (`server.go:387`) → `fetchModelCatalog(ctx)` (`server.go:680`) → `Dispatcher.FetchAvailableModels(ctx)` (`internal/accounts/dispatcher.go:163`) → `resolver.Resolve` (OAuth refresh on the request context, `internal/auth/token.go:253`) → upstream fetch. On cancel, `rotateForError` correctly refuses account rotation (`dispatcher.go:516`, `isCanceled` at `:603`), but the raw `context.Canceled` still propagates to the handler, which 500s it.
- **Two observed variants (log `~/.local/state/antigravity-proxy.log`):** fast (~550 ms, `/v1/models` + `/v1/messages` fired in parallel, first command's startup tears down the shared context) and slow (3–12.7 s, OAuth token refresh running on the client's context; client abandons). 18+ occurrences since 2026-08-30T21:10.
- **Precedent:** `internal/openrouter/client.go:229` decouples the shared catalog fetch from the caller context (`context.WithTimeout(context.Background(), 15*time.Second)`) and singleflights it (`client.go:207`). Commit `309f18f` made OpenRouter retry backoff interruptible on client disconnect.
- **Design deviation from the handoff (justified):** the handoff proposed that `writeError` skip writing a response when the client is gone. That would make `loggingMiddleware` (`server.go:277`) coerce `status == 0` to `200` (`server.go:289-291`) and log `[GET] /v1/models 200 (…)` — worse than the 500. Instead we write `499`, which the middleware logs honestly at Warn (`status >= 400`, `server.go:302`). Writing to a dead connection is harmless: `writeJSON` already ignores the encode error (`server.go:2382`).

## Global Constraints

- Module `antigravity-go-proxy`; tests live in the package under test (in-package `_test.go` files).
- No new third-party dependencies; the singleflight is hand-rolled, mirroring `openrouter.call` (`internal/openrouter/client.go:47-52`).
- Conventional commit messages (`fix(scope): …`, `perf(scope): …`), matching `git log` style.
- Gate for every task: `go build ./...` clean, `go test ./...` green, `gofmt -l internal/api internal/accounts` prints nothing. Concurrency tasks additionally run `go test -race ./internal/accounts/ ./internal/api/`.
- Do not restart or otherwise disturb the running proxies (pid 88948 on :8080, pid 8563 on :18080) without asking the user first. Live log verification is optional and user-gated.
- Existing tests `TestModelsAndHealthAliases` (`internal/api/server_test.go:147`) and `TestClaudeCodeModelDiscovery` (`internal/api/models_discovery_test.go:29`) must stay green; they pin the 200-path behavior of `/v1/models`.

---

### Task 1: Classify context errors in the API layer

**Files:**
- Modify: `internal/api/server.go:2343-2373` (`writeError`, `classifyError`)
- Test: `internal/api/error_classification_test.go` (new file)

**Interfaces:**
- Consumes: nothing new.
- Produces: `const statusClientClosedRequest = 499`; `classifyError(err) (int, string, string)` now returns `(499, "api_error", "Request canceled by the client.")` for `context.Canceled` (wrapped or bare) and `(504, "api_error", "Request deadline exceeded while contacting the upstream.")` for `context.DeadlineExceeded`; helper `isContextError(err error) bool`; `writeError` signature unchanged (`writeError(writer http.ResponseWriter, err error)`), logs context errors at Warn.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/error_classification_test.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// 499 is nginx's "client closed request" status: the client disconnected
// before the response was written. The proxy classifies context
// cancellation with it instead of the misleading 500.
const wantClientClosedRequest = 499

type cancelingBackend struct{}

func (b *cancelingBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{}, context.Canceled
}

func (b *cancelingBackend) StreamGenerateContent(ctx context.Context, request map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func TestClassifyErrorContextCancellation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		status  int
		kind    string
		message string
	}{
		{
			name:    "bare canceled",
			err:     context.Canceled,
			status:  wantClientClosedRequest,
			kind:    "api_error",
			message: "Request canceled by the client.",
		},
		{
			name:    "canceled wrapped by dispatcher retry wrapper",
			err:     fmt.Errorf("model listing exhausted account retries: %w", context.Canceled),
			status:  wantClientClosedRequest,
			kind:    "api_error",
			message: "Request canceled by the client.",
		},
		{
			name:    "bare deadline",
			err:     context.DeadlineExceeded,
			status:  http.StatusGatewayTimeout,
			kind:    "api_error",
			message: "Request deadline exceeded while contacting the upstream.",
		},
		{
			name:    "deadline wrapped by model resolution",
			err:     fmt.Errorf("refresh selectable models: %w", context.DeadlineExceeded),
			status:  http.StatusGatewayTimeout,
			kind:    "api_error",
			message: "Request deadline exceeded while contacting the upstream.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, kind, message := classifyError(tc.err)
			if status != tc.status || kind != tc.kind || message != tc.message {
				t.Fatalf("classifyError(%v) = %d, %q, %q; want %d, %q, %q",
					tc.err, status, kind, message, tc.status, tc.kind, tc.message)
			}
		})
	}
}

func TestClassifyErrorStillClassifiesOtherErrors(t *testing.T) {
	t.Parallel()
	status, kind, _ := classifyError(errors.New("ordinary failure"))
	if status != http.StatusInternalServerError || kind != "api_error" {
		t.Fatalf("classifyError(ordinary) = %d, %q; want 500, %q", status, kind, "api_error")
	}
}

func TestModelsHandlerReportsClientDisconnect(t *testing.T) {
	t.Parallel()
	server := &Server{backend: &cancelingBackend{}, logger: slog.Default(), now: time.Now}
	recorder := httptest.NewRecorder()
	server.models(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != wantClientClosedRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
}

// levelRecordingHandler records the level of every log record so tests can
// assert cancellation is not logged at error level.
type levelRecordingHandler struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (h *levelRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelRecordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	h.levels = append(h.levels, record.Level)
	h.mu.Unlock()
	return nil
}

func (h *levelRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *levelRecordingHandler) maxLevel() slog.Level {
	h.mu.Lock()
	defer h.mu.Unlock()
	max := slog.LevelDebug
	for _, level := range h.levels {
		if level > max {
			max = level
		}
	}
	return max
}

func TestWriteErrorLogsCancellationBelowErrorLevel(t *testing.T) {
	t.Parallel()
	records := &levelRecordingHandler{}
	server := &Server{logger: slog.New(records), now: time.Now}

	server.writeError(httptest.NewRecorder(), context.Canceled)
	if got := records.maxLevel(); got >= slog.LevelError {
		t.Fatalf("cancellation logged at %v; want below error level", got)
	}

	records.levels = nil
	server.writeError(httptest.NewRecorder(), errors.New("ordinary failure"))
	if got := records.maxLevel(); got < slog.LevelError {
		t.Fatalf("ordinary failure logged at %v; want error level", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run 'TestClassifyError|TestModelsHandlerReports|TestWriteErrorLogs' -v`
Expected: FAIL. `TestClassifyErrorContextCancellation` gets status `500` (default branch) where `499`/`504` wanted; `TestModelsHandlerReportsClientDisconnect` gets `500`; `TestWriteErrorLogsCancellationBelowErrorLevel` sees an Error-level record.

- [ ] **Step 3: Implement the classification**

In `internal/api/server.go`, add the constant just above `writeError` (line ~2343), add the context branches at the top of `classifyError`, and change `writeError` to log context errors at Warn:

```go
// statusClientClosedRequest is nginx's convention (499) for a client that
// disconnected before the response was written.
const statusClientClosedRequest = 499

func (server *Server) writeError(writer http.ResponseWriter, err error) {
	if isContextError(err) {
		// Cancellation and deadlines originate at the client or at a
		// bounded internal deadline, not at this server; keep them out of
		// the error log.
		server.logger.Warn("API request aborted", "error", err)
	} else {
		server.logger.Error("API request failed", "error", err)
	}
	status, kind, message := classifyError(err)
	writeAPIError(writer, status, kind, message)
}

// isContextError reports whether err is a context cancellation or deadline
// error, possibly wrapped by intermediate layers.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func classifyError(err error) (int, string, string) {
	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "api_error", "Request canceled by the client."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "api_error", "Request deadline exceeded while contacting the upstream."
	}
	var selectionError *modelcatalog.SelectionError
	if errors.As(err, &selectionError) {
		return http.StatusBadRequest, "invalid_request_error", selectionError.Error()
	}
	var upstreamError *cloudcode.HTTPError
	if errors.As(err, &upstreamError) {
		switch upstreamError.StatusCode {
		case http.StatusUnauthorized:
			return http.StatusUnauthorized, "authentication_error", "Authentication failed. Make sure Antigravity has a valid token."
		case http.StatusForbidden:
			return http.StatusForbidden, "permission_error", upstreamError.Error()
		case http.StatusTooManyRequests:
			return http.StatusBadRequest, "invalid_request_error", "RESOURCE_EXHAUSTED: capacity is exhausted for this model. Please wait for quota to reset."
		case http.StatusBadRequest, http.StatusNotFound:
			return http.StatusBadRequest, "invalid_request_error", upstreamError.Error()
		default:
			return http.StatusServiceUnavailable, "api_error", upstreamError.Error()
		}
	}
	if errors.Is(err, proxyformat.ErrEmptyResponse) {
		return http.StatusBadGateway, "api_error", err.Error()
	}
	return http.StatusInternalServerError, "api_error", err.Error()
}
```

(`classifyError` context checks go first: a `cloudcode.HTTPError` never wraps a context error, so ordering is safe, and the cheap `errors.Is` checks run before the `errors.As` walks.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run 'TestClassifyError|TestModelsHandlerReports|TestWriteErrorLogs' -v`
Expected: PASS.

Run: `go test ./internal/api/ -v`
Expected: PASS, including `TestModelsAndHealthAliases` and `TestClaudeCodeModelDiscovery` (unchanged 200 paths).

- [ ] **Step 5: gofmt and commit**

Run: `gofmt -l internal/api` — expect empty output.

```bash
git add internal/api/server.go internal/api/error_classification_test.go
git commit -m "fix(api): classify context cancellation as client disconnect, not 500"
```

---

### Task 2: Decouple the dispatcher catalog fetch from the caller context

**Files:**
- Modify: `internal/accounts/dispatcher.go:163-201` (split `FetchAvailableModels` into a public wrapper plus a private method)
- Test: `internal/accounts/dispatcher_test.go` (new file)

**Interfaces:**
- Consumes: nothing from Task 1 (independent layers; `Dispatcher` remains the `api.Backend` implementation, wired at `cmd/proxy/main.go:219`).
- Produces: public method `Dispatcher.FetchAvailableModels(ctx) (cloudcode.Response, error)` with new semantics — when `ctx` ends before the fetch, the caller receives `ctx.Err()` (e.g. `context.Canceled`), while the fetch itself continues on a background context and still performs its side effects (`MarkSuccess`, `cacheCatalog`, `updateAccountQuota`); private method `Dispatcher.fetchAvailableModels(ctx)` (the unchanged former body); `const fetchModelsTimeout = 30 * time.Second`.

**Critical detail:** the background `fetchCtx` must be cancelled inside the fetch goroutine (`defer cancel()` in the goroutine), NOT with `defer cancel()` in the wrapper. A wrapper-level `defer cancel()` fires when the abandoned caller returns and would kill the very fetch this task exists to protect.

- [ ] **Step 1: Write the failing test**

Create `internal/accounts/dispatcher_test.go`:

```go
package accounts

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
)

const testCatalogBody = `{"models":{"gemini-2.5-pro":{"displayName":"Gemini 2.5 Pro"}},"agentModelSorts":[{"groups":[{"modelIds":["gemini-2.5-pro"]}]}]}`

type stubResolver struct{}

func (stubResolver) Resolve(ctx context.Context, account *Account) (auth.Credentials, error) {
	return auth.Credentials{AccessToken: "token", Email: account.Email}, nil
}

func (stubResolver) Invalidate(string) {}

// blockingModelsClient blocks every FetchAvailableModels call on its release
// channel so tests control when the upstream fetch completes.
type blockingModelsClient struct {
	calls   atomic.Int32
	release chan struct{}
}

func (c *blockingModelsClient) LoadCodeAssist(ctx context.Context, project string) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"cloudaicompanionProject":{"id":"project"}}`)}, nil
}

func (c *blockingModelsClient) FetchAvailableModels(ctx context.Context, project string) (cloudcode.Response, error) {
	c.calls.Add(1)
	select {
	case <-c.release:
		return cloudcode.Response{Body: []byte(testCatalogBody)}, nil
	case <-ctx.Done():
		return cloudcode.Response{}, ctx.Err()
	}
}

func (c *blockingModelsClient) StreamGenerateContent(ctx context.Context, request any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func newTestDispatcher(t *testing.T, client CloudClient) *Dispatcher {
	t.Helper()
	manager, err := New(Options{Accounts: []*Account{{Email: "test@example.com", Enabled: true}}})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return client },
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	return dispatcher
}

func TestFetchAvailableModelsSurvivesCallerCancel(t *testing.T) {
	release := make(chan struct{})
	client := &blockingModelsClient{release: release}
	dispatcher := newTestDispatcher(t, client)

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()

	type outcome struct {
		response cloudcode.Response
		err     error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		response, err := dispatcher.FetchAvailableModels(callerCtx)
		resultCh <- outcome{response: response, err: err}
	}()

	// Cancel the caller while the upstream fetch is still blocked.
	cancelCaller()
	select {
	case result := <-resultCh:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("FetchAvailableModels error=%v; want context.Canceled", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FetchAvailableModels did not return after caller cancel")
	}

	// The decoupled fetch must keep running: release it and wait for the
	// shared catalog cache to be populated even though the caller left.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dispatcher.mu.RLock()
		cached := dispatcher.catalog
		dispatcher.mu.RUnlock()
		if cached != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background fetch never cached the catalog after caller cancel")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts/ -run TestFetchAvailableModelsSurvivesCallerCancel -v`
Expected: FAIL at the final `t.Fatal("background fetch never cached the catalog after caller cancel")` — today the fetch runs on the caller's context, so `cancelCaller()` aborts it and nothing populates the cache. (The `errors.Is` assertion already passes; the cache assertion is the behavioral change.)

- [ ] **Step 3: Implement the decoupled wrapper**

In `internal/accounts/dispatcher.go`, replace the `FetchAvailableModels` method (line 163) with the wrapper and rename the former body to a private method. New code above the old body:

```go
// fetchModelsTimeout bounds a decoupled catalog fetch, including any OAuth
// token refresh it triggers, so work abandoned by one disconnected client
// still completes and cannot run forever.
const fetchModelsTimeout = 30 * time.Second

// FetchAvailableModels returns the upstream model catalog. The fetch runs on
// a background context bounded by fetchModelsTimeout, so a single client
// disconnect cannot cancel shared work (OAuth token refresh, catalog cache,
// account quota update) — the same rationale as the OpenRouter catalog
// fetch (internal/openrouter/client.go). When the caller's context ends
// first, the caller receives its context error while the fetch continues
// in the background.
func (dispatcher *Dispatcher) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	fetchCtx, cancel := context.WithTimeout(context.Background(), fetchModelsTimeout)
	type fetchResult struct {
		response cloudcode.Response
		err      error
	}
	done := make(chan fetchResult, 1)
	go func() {
		// cancel() must live here, inside the goroutine: cancelling from
		// the wrapper would abort the fetch the moment an abandoned
		// caller returns.
		defer cancel()
		response, err := dispatcher.fetchAvailableModels(fetchCtx)
		done <- fetchResult{response: response, err: err}
	}()
	select {
	case result := <-done:
		return result.response, result.err
	case <-ctx.Done():
		return cloudcode.Response{}, ctx.Err()
	}
}
```

Then rename the former method (same file, immediately after):

```go
func (dispatcher *Dispatcher) fetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	// ... the complete, unchanged former FetchAvailableModels body:
	// the account loop with resolver.Resolve, throttling sleep, upstream
	// FetchAvailableModels, MarkSuccess, cacheCatalog, updateAccountQuota,
	// rotateForError, and the "model listing exhausted account retries"
	// wrapper error.
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -run TestFetchAvailableModelsSurvivesCallerCancel -v`
Expected: PASS.

Run: `go test -race ./internal/accounts/`
Expected: PASS (race detector must be clean — this task introduces the background goroutine).

- [ ] **Step 5: gofmt and commit**

Run: `gofmt -l internal/accounts` — expect empty output.

```bash
git add internal/accounts/dispatcher.go internal/accounts/dispatcher_test.go
git commit -m "fix(accounts): decouple catalog fetch from caller context"
```

---

### Task 3: Share one in-flight catalog fetch across concurrent callers

**Files:**
- Modify: `internal/accounts/dispatcher.go` (the Task 2 wrapper becomes a singleflight; new field on `Dispatcher`)
- Test: `internal/accounts/dispatcher_test.go` (append one test)

**Interfaces:**
- Consumes: `Dispatcher.fetchAvailableModels(ctx)` and `fetchModelsTimeout` from Task 2.
- Produces: `type modelFetchCall struct { done chan struct{}; response cloudcode.Response; err error }`; `func (dispatcher *Dispatcher) startModelFetch() *modelFetchCall`; new `Dispatcher` field `modelsFetch *modelFetchCall` (guarded by the existing `dispatcher.mu`, added next to `catalog`/`catalogAt` at `dispatcher.go:75-79`). `FetchAvailableModels` public semantics from Task 2 are unchanged.

- [ ] **Step 1: Write the failing test**

Append to `internal/accounts/dispatcher_test.go`:

```go
func TestConcurrentModelFetchesShareOneUpstreamCall(t *testing.T) {
	release := make(chan struct{})
	client := &blockingModelsClient{release: release}
	dispatcher := newTestDispatcher(t, client)

	// Leader: starts the shared fetch; the upstream call blocks on release.
	first := dispatcher.startModelFetch()

	// Joiner: arrives while the fetch is in flight and must attach to the
	// same call instead of starting a second upstream request.
	second := dispatcher.startModelFetch()
	if first != second {
		t.Fatal("second caller started a new upstream fetch instead of sharing the in-flight call")
	}

	// Let the shared fetch finish and check the result.
	close(release)
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("shared catalog fetch did not complete")
	}
	if first.err != nil {
		t.Fatalf("shared fetch error: %v", first.err)
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("upstream FetchAvailableModels calls=%d; want 1", got)
	}

	// Once complete, the slot frees so the next caller starts a fresh fetch.
	dispatcher.mu.RLock()
	pending := dispatcher.modelsFetch
	dispatcher.mu.RUnlock()
	if pending != nil {
		t.Fatal("modelsFetch still set after the shared fetch completed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts/ -run TestConcurrentModelFetchesShareOneUpstreamCall -v`
Expected: FAIL to compile — `undefined: dispatcher.startModelFetch`. That is the red state for this task.

- [ ] **Step 3: Implement the singleflight**

In `internal/accounts/dispatcher.go`, add the field to the `Dispatcher` struct (after `catalogAt`, `dispatcher.go:78`; the existing `mu sync.RWMutex` guards it):

```go
	mu        sync.RWMutex
	clients   map[string]accountClient
	catalog   *modelcatalog.Catalog
	catalogAt time.Time
	// modelsFetch is the shared in-flight catalog fetch; guarded by mu.
	modelsFetch *modelFetchCall
```

Replace the Task 2 `FetchAvailableModels` wrapper (the `fetchResult` type and per-caller goroutine are subsumed — delete them) with:

```go
// modelFetchCall is one shared catalog fetch. Concurrent FetchAvailableModels
// callers attach to the same call; done is closed once response and err are
// set, so any number of callers — or none, if they all disconnected — can
// observe the result.
type modelFetchCall struct {
	done     chan struct{}
	response cloudcode.Response
	err      error
}

// FetchAvailableModels returns the upstream model catalog. All concurrent
// callers share a single in-flight fetch running on a background context
// bounded by fetchModelsTimeout (see internal/openrouter/client.go for the
// same pattern). When the caller's context ends first, the caller receives
// its context error while the shared fetch continues and still refreshes
// the catalog cache and account quotas.
func (dispatcher *Dispatcher) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	call := dispatcher.startModelFetch()
	select {
	case <-call.done:
		return call.response, call.err
	case <-ctx.Done():
		return cloudcode.Response{}, ctx.Err()
	}
}

// startModelFetch returns the in-progress shared fetch, starting one when no
// fetch is running. Writing response/err before closing done makes the
// result visible to every waiter without extra locking.
func (dispatcher *Dispatcher) startModelFetch() *modelFetchCall {
	dispatcher.mu.Lock()
	if call := dispatcher.modelsFetch; call != nil {
		dispatcher.mu.Unlock()
		return call
	}
	call := &modelFetchCall{done: make(chan struct{})}
	dispatcher.modelsFetch = call
	dispatcher.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(context.Background(), fetchModelsTimeout)
	go func() {
		// cancel() lives inside the goroutine: cancelling from the caller
		// would abort the fetch the moment an abandoned caller returns.
		defer cancel()
		call.response, call.err = dispatcher.fetchAvailableModels(fetchCtx)
		close(call.done)
		dispatcher.mu.Lock()
		if dispatcher.modelsFetch == call {
			dispatcher.modelsFetch = nil
		}
		dispatcher.mu.Unlock()
	}()
	return call
}
```

`fetchAvailableModels` (Task 2) and `fetchModelsTimeout` are unchanged. Note the write-then-close ordering in the goroutine: `response`/`err` are written before `close(call.done)`, so every reader that observes `done` closed sees the final values (happens-before via channel close) — race-detector clean.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/accounts/ -run 'TestConcurrentModelFetchesShareOneUpstreamCall|TestFetchAvailableModelsSurvivesCallerCancel' -v`
Expected: PASS. The Task 2 test exercises the new path end-to-end: caller cancels → gets `context.Canceled`; the shared fetch later completes and populates the catalog.

Run: `go test -race ./internal/accounts/ ./internal/api/`
Expected: PASS.

- [ ] **Step 5: gofmt and commit**

Run: `gofmt -l internal/accounts` — expect empty output.

```bash
git add internal/accounts/dispatcher.go internal/accounts/dispatcher_test.go
git commit -m "perf(accounts): share one in-flight catalog fetch across concurrent callers"
```

---

### Task 4: Full verification gate

**Files:**
- No new files; verification only.

**Interfaces:**
- Consumes: all previous tasks.
- Produces: verified main branch state.

- [ ] **Step 1: Build and test everything**

```bash
go build ./...
go test ./...
go test -race ./internal/accounts/ ./internal/api/
gofmt -l internal/api internal/accounts
```

Expected: build clean; all tests green (including `TestModelsAndHealthAliases`, `TestClaudeCodeModelDiscovery`); race detector clean; gofmt prints nothing.

- [ ] **Step 2: Confirm the fix classifies both log variants**

Re-check against the handoff's two variants:
- Fast variant: client cancels → dispatcher returns `ctx.Err()` → `classifyError` → `499` → middleware logs `[GET] /v1/models 499 (…)` at Warn, `writeError` logs `WARN API request aborted error=context canceled`. No more 500, no more ERROR-level noise.
- Slow variant: OAuth refresh now runs on the background `fetchCtx`, so the client abandoning the request no longer aborts the refresh; the refresh completes and the next `/v1/models` call hits a warm dispatcher.

- [ ] **Step 3: Live verification (optional, user-gated)**

The proxy on `:8080` (pid 88948) is the user's running process. If the user wants live verification, ask first, then rebuild and restart per their preference, run `curl -s -w '%{http_code}' http://127.0.0.1:8080/v1/models`, and watch `~/.local/state/antigravity-proxy.log` for absence of `[GET] /v1/models 500` and `ERROR API request failed error=context canceled`. Otherwise skip — the test gate above is the completion criterion.

---

## Self-Review

**Spec coverage:**
- Handler-level classification (handoff fix option 1) → Task 1. Deviation from "skip writing": documented in the Architecture section; `499` write keeps the access log honest because `loggingMiddleware` coerces unwritten responses to 200.
- Decouple catalog fetch/refresh from the request context (handoff fix option 2, dispatcher path) → Task 2. `internal/auth/token.go` is deliberately untouched: with the wrapper, `resolver.Resolve` inside the background loop already receives the detached `fetchCtx`, and the streaming path's refresh-on-client-context behavior is out of scope.
- Singleflight/dedup (handoff fix option 3, "consider") → Task 3, justified by the log bursts (multiple 500s per minute from concurrent startup fetches) and by the existing openrouter precedent.
- Tests-first for `classifyError` cancel/deadline, models handler with canceled fetch, existing models tests staying green → Tasks 1–2 test steps + Task 4 gate.
- Verification gate (`go build`, `go test`, `gofmt`) → Task 4.

**Placeholder scan:** the only intentional elision is the Task 2 "unchanged former body" comment for `fetchAvailableModels` — it is a pure rename of existing code at `dispatcher.go:163-201`, not new content to invent. No TBDs, no untestable steps.

**Type consistency:** `startModelFetch() *modelFetchCall`, `modelFetchCall{done chan struct{}; response cloudcode.Response; err error}`, `fetchModelsTimeout = 30 * time.Second`, and `statusClientClosedRequest = 499` are used identically in Tasks 1–3 and in the tests. Task 3 deletes Task 2's `fetchResult` type explicitly.