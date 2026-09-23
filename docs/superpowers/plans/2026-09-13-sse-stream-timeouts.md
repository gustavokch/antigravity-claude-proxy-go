# SSE Stream Timeout Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop SSE streams from dying mid-stream: remove total `http.Client.Timeout` from streaming upstream clients, add downstream keep-alive pings, and log mid-stream failures.

**Architecture:** Split `cloudcode.Client` into a unary client (keeps `Timeout`) and a streaming client (`Timeout: 0`) with time-to-first-byte (TTFB) bounded per attempt via `time.AfterFunc` + `context.CancelFunc`, mirroring the proven `openRouterSharedClient`/`headersCutoff` pattern in `internal/api/server.go:1806-1890`. Add a shared `sseKeepAlive` helper in the `api` package that flushes `: keep-alive\n\n` comments downstream after 15s of silence, wired into `streamMessage` and `ccCopyStream`. Log mid-stream upstream errors that currently vanish silently.

**Tech Stack:** Go, `net/http`, SSE, `httptest`, `slog`. Test gate: `make test` (= `go test -v -race ./...`).

**Spec:** `/tmp/handoff-sse-timeout-investigation.md` (investigation findings; this plan implements its Phase 2 + Phase 3).

## Root Cause Summary (from investigation)

1. **cloudcode** (`internal/cloudcode/client.go:130`): `http.Client{Timeout: options.Timeout}` (default 5m via `-upstream-timeout` flag, `cmd/proxy/main.go:89`). `http.Client.Timeout` spans the entire exchange including body read — any stream longer than 5m is canceled with `context deadline exceeded`.
2. **claudecode** (`internal/claudecode/client.go:278`): hardcoded `http.Client{Timeout: 5 * time.Minute}` — same semantics.
3. **No downstream heartbeats:** `ParseSSE` drops upstream comment lines (`internal/cloudcode/client.go:366-368`), and neither `streamMessage` (`internal/api/server.go:2837`) nor `ccCopyStream` (`internal/api/claudecode_proxy.go:277`) injects keep-alive comments. Silent thinking gaps can be dropped by intermediaries/NAT/clients.
4. **Observability gap:** mid-stream errors in `streamMessage` are sent to the client as an SSE `error` event but never logged server-side (`internal/api/server.go:2973-2981`), which is why proxy logs show no trace of these failures.

## Global Constraints

- Go module; no new dependencies (stdlib only).
- Every task ends green under `go build ./...` and `go test -race ./internal/<touched>/...`.
- Final gate: `make test` (full suite with `-race`).
- Do not change the `-upstream-timeout` flag name or default (5m); its meaning shifts to "unary timeout + stream TTFB bound".
- Mid-stream failures must NOT trigger endpoint/account failover after bytes were consumed downstream (duplicate-event risk). This is current behavior in `DoSSE` and must be preserved.
- SSE comment pings may only be injected at line boundaries when forwarding raw bytes (a partial `data:` line must never be split by a comment).

---

### Task 1: cloudcode — separate streaming client with TTFB-only bound

**Files:**
- Modify: `internal/cloudcode/client.go` (struct at 49-57, `New` at 121-153, `DoSSE` at 220-263)
- Test: `internal/cloudcode/client_test.go`

**Interfaces:**
- Consumes: existing `Options{Timeout}`, `RequestOptions`, `SSEEvent`, `ParseSSE`, `maybeGunzip`, `readResponse`.
- Produces:
  - `Client` gains fields `streamClient *http.Client`, `streamHeaderTimeout time.Duration`.
  - `DoSSE(ctx, endpoints, path, payload, options, consume) (Response, error)` — signature unchanged; semantics: unary `Timeout` no longer applies to the stream body; TTFB per endpoint bounded by `streamHeaderTimeout`.
  - `terminalStreamError` (unexported) — wraps errors raised after response-body consumption started; callers must not fail over on these.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cloudcode/client_test.go`:

```go
func TestDoSSEStreamSurvivesPastUnaryTimeout(t *testing.T) {
	// The unary Timeout must not cap the stream body read. Server drips
	// 3 events 150ms apart; Timeout is 200ms — pre-fix the client cancels
	// the exchange mid-stream with "context deadline exceeded".
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer server.Close()

	client := New(Options{Timeout: 200 * time.Millisecond})
	var events []SSEEvent
	_, err := client.DoSSE(context.Background(), []string{server.URL}, "/stream", map[string]any{}, RequestOptions{}, func(e SSEEvent) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
}

func TestDoSSEHeadersCutoffBoundsTTFB(t *testing.T) {
	// Server hangs 400ms before writing headers; TTFB bound (Options.Timeout)
	// is 200ms, so DoSSE must fail well before the headers arrive.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(Options{Timeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := client.DoSSE(context.Background(), []string{server.URL}, "/stream", map[string]any{}, RequestOptions{}, func(SSEEvent) error { return nil })
	if err == nil {
		t.Fatal("expected TTFB cutoff error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 350*time.Millisecond {
		t.Fatalf("TTFB cutoff not enforced, took %v", elapsed)
	}
}

func TestDoSSEConsumeErrorDoesNotFailOver(t *testing.T) {
	// A consume() error means bytes were already delivered downstream;
	// retrying another endpoint would duplicate events. Pin: no failover.
	var mu sync.Mutex
	var calls int
	mkServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {}\n\n")
		}))
	}
	s1, s2 := mkServer(), mkServer()
	defer s1.Close()
	defer s2.Close()

	client := New(Options{Timeout: time.Minute})
	boom := errors.New("boom")
	_, err := client.DoSSE(context.Background(), []string{s1.URL, s2.URL}, "/stream", map[string]any{}, RequestOptions{}, func(SSEEvent) error { return boom })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected consume error, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("failover after consume error: %d endpoints hit, want 1", calls)
	}
}
```

Add imports as needed: `sync`, `strings`, `errors`, `fmt`, `time`, `net/http`, `net/http/httptest`, `context`.

- [ ] **Step 2: Run tests to verify the first one fails**

Run: `go test ./internal/cloudcode/ -run 'TestDoSSE' -v`
Expected: `TestDoSSEStreamSurvivesPastUnaryTimeout` FAILS with "context deadline exceeded". The other two pass (they pin existing semantics).

- [ ] **Step 3: Implement the streaming client split**

In `internal/cloudcode/client.go`:

1. Add fields to `Client` struct (line 49-57):

```go
type Client struct {
	httpClient            *http.Client
	streamClient          *http.Client
	transport             *http.Transport
	streamHeaderTimeout   time.Duration
	accessToken           string
	userAgent             string
	contentEndpoints      []string
	provisioningEndpoints []string
	defaultHeader         http.Header
}
```

2. Add the terminal-error type above `DoSSE`:

```go
// terminalStreamError marks failures after the response body was partially
// consumed; retrying another endpoint would duplicate events downstream.
type terminalStreamError struct{ err error }

func (e *terminalStreamError) Error() string { return e.err.Error() }
func (e *terminalStreamError) Unwrap() error { return e.err }
```

3. In `New` (line 121-153), build the streaming client and default the TTFB bound:

```go
	var transport *http.Transport
	client := options.HTTPClient
	streamClient := options.HTTPClient
	if client == nil {
		transport = defaultTransport
		client = &http.Client{Transport: transport, Timeout: options.Timeout}
		// No total timeout on streams: http.Client.Timeout spans the whole
		// exchange including the body read and kills long generations.
		// Time-to-headers is bounded per attempt in DoSSE instead.
		streamClient = &http.Client{Transport: transport}
	}

	streamHeaderTimeout := options.Timeout
	if streamHeaderTimeout <= 0 {
		streamHeaderTimeout = 5 * time.Minute
	}
```

and in the returned struct literal add `streamClient: streamClient,` and `streamHeaderTimeout: streamHeaderTimeout,`.

4. Replace the body of `DoSSE` so each attempt runs through a new `streamEndpoint` helper, and failover only happens for pre-body errors:

```go
func (c *Client) DoSSE(ctx context.Context, endpoints []string, path string, payload any, options RequestOptions, consume func(SSEEvent) error) (Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("encode Cloud Code streaming request: %w", err)
	}
	var failures []error
	for _, endpoint := range endpoints {
		request, err := c.newRequest(ctx, endpoint, path, body, options)
		if err != nil {
			return Response{}, err
		}
		result, err := c.streamEndpoint(endpoint, request, consume)
		if err != nil {
			var terminal *terminalStreamError
			if errors.As(err, &terminal) {
				return result, err
			}
			failures = append(failures, err)
			continue
		}
		return result, nil
	}
	return Response{}, errors.Join(failures...)
}

// streamEndpoint runs one streaming attempt. Only time-to-headers is bounded
// (streamHeaderTimeout via a cancel-on-expiry timer); once headers arrive the
// body reads unbounded so long generations are not killed mid-stream. Mirrors
// the headersCutoff pattern in internal/api/server.go.
func (c *Client) streamEndpoint(endpoint string, request *http.Request, consume func(SSEEvent) error) (Response, error) {
	streamCtx, cancel := context.WithCancel(request.Context())
	defer cancel()
	headersCutoff := time.AfterFunc(c.streamHeaderTimeout, cancel)
	response, err := c.streamClient.Do(request.WithContext(streamCtx))
	headersCutoff.Stop()
	if err != nil {
		return Response{}, fmt.Errorf("Cloud Code stream to %s: %w", endpoint, err)
	}
	// The cutoff can fire in the window between header arrival and Stop();
	// the stream then holds a dead context and dies on the first read.
	// Treat it like a failed attempt and fail over.
	if streamCtx.Err() != nil {
		_ = response.Body.Close()
		return Response{}, fmt.Errorf("Cloud Code stream to %s: headers arrived at TTFB cutoff", endpoint)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result, responseErr := readResponse(endpoint, response)
		if responseErr != nil {
			return Response{}, responseErr
		}
		return Response{}, fmt.Errorf("unexpected streaming response: %#v", result)
	}
	result := Response{Endpoint: endpoint, StatusCode: response.StatusCode, Header: response.Header}
	streamReader, streamErr := maybeGunzip(response)
	if streamErr != nil {
		_ = response.Body.Close()
		return Response{}, fmt.Errorf("open Cloud Code stream from %s: %w", endpoint, streamErr)
	}
	err = ParseSSE(streamReader, consume)
	closeErr := response.Body.Close()
	if err != nil {
		return result, &terminalStreamError{err: fmt.Errorf("parse Cloud Code SSE stream: %w", err)}
	}
	if closeErr != nil {
		return result, &terminalStreamError{err: fmt.Errorf("close Cloud Code SSE stream: %w", closeErr)}
	}
	return result, nil
}
```

Note: the status-code and gunzip failures remain failover-able (pre-body), matching current behavior.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cloudcode/ -run 'TestDoSSE' -v -race`
Expected: all PASS.

- [ ] **Step 5: Run package tests**

Run: `go test ./internal/cloudcode/ -race`
Expected: PASS (existing gzip/fallback tests unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/cloudcode/client.go internal/cloudcode/client_test.go
git commit -m "fix(cloudcode): drop total client timeout on streams, bound TTFB instead"
```

---

### Task 2: claudecode — remove hardcoded 5m total timeout

**Files:**
- Modify: `internal/claudecode/client.go:275-285`
- Test: `internal/claudecode/client_test.go`

**Interfaces:**
- Consumes: `NewClient(baseURL string, httpClient *http.Client) *Client`.
- Produces: default client (when `httpClient == nil`) has `Timeout: 0`; exchange bounded by the caller's request context (`request.Context()` in `claudecode_proxy.go`, which cancels on downstream disconnect). Callers that inject their own client are unchanged.

- [ ] **Step 1: Write the failing test**

Append to `internal/claudecode/client_test.go`:

```go
func TestNewClientDefaultHasNoTotalTimeout(t *testing.T) {
	// A total http.Client.Timeout spans the SSE body read and kills
	// streams longer than the timeout. The default must be unbounded.
	c := NewClient("https://api.anthropic.com", nil)
	if c.httpClient.Timeout != 0 {
		t.Fatalf("default client Timeout = %v, want 0 (unbounded)", c.httpClient.Timeout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/claudecode/ -run TestNewClientDefaultHasNoTotalTimeout -v`
Expected: FAIL with "default client Timeout = 5m0s, want 0".

- [ ] **Step 3: Fix the default**

In `internal/claudecode/client.go:275-285`:

```go
// NewClient creates a new Anthropic Claude Code client.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		// No total timeout: it caps the whole exchange including the SSE
		// body read and kills long streams. The request context bounds the
		// exchange instead.
		httpClient = &http.Client{}
	}
	return &Client{
		baseURL:    NormalizeBaseURL(baseURL),
		httpClient: httpClient,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/claudecode/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/claudecode/client.go internal/claudecode/client_test.go
git commit -m "fix(claudecode): remove 5m total timeout killing long streams"
```

---

### Task 3: sseKeepAlive helper with unit tests

**Files:**
- Create: `internal/api/sse_keepalive.go`
- Test: `internal/api/sse_keepalive_test.go`

**Interfaces:**
- Consumes: `http.ResponseWriter`, `bufio.Writer`, `http.Flusher`.
- Produces:
  - `var sseKeepAliveInterval = 15 * time.Second` (package-level; tests override).
  - `newSSEKeepAlive(bw *bufio.Writer, writer http.ResponseWriter) *sseKeepAlive`
  - `(k *sseKeepAlive) activateLocked()` — caller holds `k.mu`; enables pings.
  - `(k *sseKeepAlive) markWriteLocked()` — caller holds `k.mu`; records activity.
  - `(k *sseKeepAlive) run(done <-chan struct{}, interval time.Duration)` — goroutine entry; pings on silence.
  - Field `k.pingAllowed func() bool` (optional, called with `k.mu` held) — gates pings for raw byte forwarding (line-boundary safety).

- [ ] **Step 1: Write the failing test**

Create `internal/api/sse_keepalive_test.go`:

```go
package api

import (
	"bufio"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEKeepAlivePingsAfterSilence(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := bufio.NewWriter(rec)
	k := newSSEKeepAlive(bw, rec)
	done := make(chan struct{})
	defer close(done)
	go k.run(done, 20*time.Millisecond)

	k.mu.Lock()
	k.activateLocked()
	k.mu.Unlock()

	time.Sleep(75 * time.Millisecond)
	if body := rec.Body.String(); !strings.Contains(body, ": keep-alive\n\n") {
		t.Fatalf("no ping after silence, got %q", body)
	}
}

func TestSSEKeepAliveSilentWhenInactive(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := bufio.NewWriter(rec)
	k := newSSEKeepAlive(bw, rec)
	done := make(chan struct{})
	defer close(done)
	go k.run(done, 20*time.Millisecond)

	time.Sleep(60 * time.Millisecond)
	if body := rec.Body.String(); body != "" {
		t.Fatalf("pinged before activate, got %q", body)
	}
}

func TestSSEKeepAliveSkipsWhenActive(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := bufio.NewWriter(rec)
	k := newSSEKeepAlive(bw, rec)
	done := make(chan struct{})
	defer close(done)
	go k.run(done, 20*time.Millisecond)

	k.mu.Lock()
	k.activateLocked()
	k.mu.Unlock()

	// Simulate regular downstream writes: no ping should fire.
	deadline := time.Now().Add(90 * time.Millisecond)
	for time.Now().Before(deadline) {
		k.mu.Lock()
		k.markWriteLocked()
		k.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if body := rec.Body.String(); strings.Contains(body, "keep-alive") {
		t.Fatalf("pinged despite activity, got %q", body)
	}
}

func TestSSEKeepAliveRespectsPingAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	bw := bufio.NewWriter(rec)
	k := newSSEKeepAlive(bw, rec)
	k.pingAllowed = func() bool { return false } // mid-line, never safe
	done := make(chan struct{})
	defer close(done)
	go k.run(done, 20*time.Millisecond)

	k.mu.Lock()
	k.activateLocked()
	k.mu.Unlock()

	time.Sleep(60 * time.Millisecond)
	if body := rec.Body.String(); body != "" {
		t.Fatalf("pinged despite pingAllowed=false, got %q", body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestSSEKeepAlive -v`
Expected: FAIL — `undefined: newSSEKeepAlive`.

- [ ] **Step 3: Implement the helper**

Create `internal/api/sse_keepalive.go`:

```go
package api

import (
	"bufio"
	"net/http"
	"sync"
	"time"
)

// sseKeepAliveInterval is the downstream silence budget before a comment
// ping is flushed. Package-level so tests can shorten it.
var sseKeepAliveInterval = 15 * time.Second

// sseKeepAlive writes SSE comment pings downstream whenever the stream has
// been silent for the configured interval, so upstream thinking gaps are not
// mistaken for dead connections by clients, NATs, or intermediary proxies.
//
// Locking: callers wrap downstream writes in k.mu and call markWriteLocked;
// run() takes the same mutex before pinging, so pings never interleave with
// a partial write.
type sseKeepAlive struct {
	mu          sync.Mutex
	bw          *bufio.Writer
	flusher     http.Flusher
	hasFlusher  bool
	active      bool
	lastWrite   time.Time
	pingAllowed func() bool // optional; called with mu held
}

func newSSEKeepAlive(bw *bufio.Writer, writer http.ResponseWriter) *sseKeepAlive {
	flusher, ok := writer.(http.Flusher)
	return &sseKeepAlive{
		bw:         bw,
		flusher:    flusher,
		hasFlusher: ok,
		lastWrite:  time.Now(),
	}
}

// activateLocked enables pings; call once response headers are written.
// Caller must hold k.mu.
func (k *sseKeepAlive) activateLocked() {
	k.active = true
	k.lastWrite = time.Now()
}

// markWriteLocked records downstream activity. Caller must hold k.mu.
func (k *sseKeepAlive) markWriteLocked() {
	k.lastWrite = time.Now()
}

// run pings after each interval of silence until done closes. Call in a
// goroutine; stop by closing done.
func (k *sseKeepAlive) run(done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			k.mu.Lock()
			if k.active && time.Since(k.lastWrite) >= interval &&
				(k.pingAllowed == nil || k.pingAllowed()) {
				if _, err := k.bw.WriteString(": keep-alive\n\n"); err != nil {
					k.active = false // dead connection; stop pinging
				} else {
					_ = k.bw.Flush()
					if k.hasFlusher {
						k.flusher.Flush()
					}
					k.lastWrite = time.Now()
				}
			}
			k.mu.Unlock()
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestSSEKeepAlive -v -race`
Expected: all 4 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/sse_keepalive.go internal/api/sse_keepalive_test.go
git commit -m "feat(api): add sseKeepAlive helper for downstream comment pings"
```

---

### Task 4: wire keep-alive into streamMessage (cloudcode path)

**Files:**
- Modify: `internal/api/server.go:2837-2881` (`streamMessage` prologue and `writeEvents`)
- Test: `internal/api/stream_message_keepalive_test.go` (create)

**Interfaces:**
- Consumes: `sseKeepAlive` from Task 3, `sseKeepAliveInterval`.
- Produces: `streamMessage` emits `: keep-alive\n\n` after 15s of downstream silence once headers are sent. No signature changes.

- [ ] **Step 1: Write the failing test**

Create `internal/api/stream_message_keepalive_test.go`:

```go
package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
)

func TestStreamMessageEmitsKeepAliveDuringUpstreamSilence(t *testing.T) {
	orig := sseKeepAliveInterval
	defer func() { sseKeepAliveInterval = orig }()
	sseKeepAliveInterval = 25 * time.Millisecond

	payload1 := `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"I should ","thoughtSignature":"claude-signature-0123456789012345678901234567890123456789"}]}}],"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":20}}}`
	payload2 := `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"inspect.","thoughtSignature":"claude-signature-0123456789012345678901234567890123456789"},{"text":"Done."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":9,"cachedContentTokenCount":20}}}`

	server, err := New(Options{
		APIKey: "test",
		Credentials: func(context.Context) (auth.Credentials, error) {
			return auth.Credentials{}, nil
		},
		Builder: proxyformat.NewBuilder(),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	w := httptest.NewRecorder()
	server.streamMessage(w, req, func(ctx context.Context, _ map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		if err := consume(cloudcode.SSEEvent{Event: "message", Data: []byte(payload1)}); err != nil {
			return cloudcode.Response{}, err
		}
		time.Sleep(90 * time.Millisecond) // upstream silence past the interval
		if err := consume(cloudcode.SSEEvent{Event: "message", Data: []byte(payload2)}); err != nil {
			return cloudcode.Response{}, err
		}
		return cloudcode.Response{StatusCode: 200}, nil
	}, map[string]any{"messages": []any{}}, "claude-sonnet-4-6-thinking")

	if body := w.Body.String(); !strings.Contains(body, ": keep-alive\n\n") {
		t.Fatalf("no keep-alive ping in stream, got %q", body)
	}
}
```

Module path is `antigravity-go-proxy` (see `go.mod`); imports above match `internal/api/bench_test.go`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestStreamMessageEmitsKeepAlive -v`
Expected: FAIL — "no keep-alive ping in stream".

- [ ] **Step 3: Wire the heartbeat**

In `streamMessage` (`internal/api/server.go:2837`), after the `bw := bufio.NewWriterSize(writer, 4096)` line, add:

```go
	keepAlive := newSSEKeepAlive(bw, writer)
	kaDone := make(chan struct{})
	defer close(kaDone)
	go keepAlive.run(kaDone, sseKeepAliveInterval)
```

Then wrap `writeEvents` so all downstream writes hold `keepAlive.mu`:

```go
	writeEvents := func(events []map[string]any) error {
		if len(events) == 0 {
			return nil
		}
		keepAlive.mu.Lock()
		defer keepAlive.mu.Unlock()
		if !started {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Set("Cache-Control", "no-cache")
			writer.Header().Set("Connection", "keep-alive")
			writer.Header().Set("X-Accel-Buffering", "no")
			writer.WriteHeader(http.StatusOK)
			started = true
			keepAlive.activateLocked()
		}
		for _, event := range events {
			buf := jsonBufferPool.Get().(*bytes.Buffer)
			buf.Reset()
			err := json.NewEncoder(buf).Encode(event)
			if err == nil {
				eventType, _ := event["type"].(string)
				bw.WriteString("event: ")
				bw.WriteString(eventType)
				bw.WriteString("\ndata: ")
				bw.Write(buf.Bytes())
				bw.WriteString("\n")
			}
			jsonBufferPool.Put(buf)
			if err != nil {
				return err
			}
		}
		bw.Flush()
		if hasFlusher {
			flusher.Flush()
		}
		keepAlive.markWriteLocked()
		return nil
	}
```

Note: pings stay disabled until headers are sent (`activateLocked`), so pre-stream errors can still return non-200 via `server.writeError`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestStreamMessageEmitsKeepAlive -v -race`
Expected: PASS.

- [ ] **Step 5: Run package tests**

Run: `go test ./internal/api/ -race`
Expected: PASS. Watch for data races on `sseKeepAliveInterval` in parallel tests — do not add `t.Parallel()` to the new test.

- [ ] **Step 6: Commit**

```bash
git add internal/api/server.go internal/api/stream_message_keepalive_test.go
git commit -m "feat(api): heartbeat keep-alive pings in streamMessage"
```

---

### Task 5: wire keep-alive into ccCopyStream (claudecode path), line-boundary safe

**Files:**
- Modify: `internal/api/claudecode_proxy.go:275-294` (`ccCopyStream`)
- Test: `internal/api/claudecode_proxy_test.go` (append; create if absent — check with `ls internal/api/`)

**Interfaces:**
- Consumes: `sseKeepAlive` from Task 3, `sseKeepAliveInterval`.
- Produces: `ccCopyStream(writer http.ResponseWriter, body io.Reader)` — signature unchanged; emits `: keep-alive\n\n` on silence, but only when the last byte written was `\n` or nothing has been written yet (never splits a partial SSE line).

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/claudecode_proxy_test.go`:

```go
func TestCCCopyStreamEmitsKeepAliveOnSilence(t *testing.T) {
	orig := sseKeepAliveInterval
	defer func() { sseKeepAliveInterval = orig }()
	sseKeepAliveInterval = 25 * time.Millisecond

	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		ccCopyStream(rec, pr)
		close(done)
	}()

	_, _ = pw.Write([]byte("data: first\n\n"))
	time.Sleep(90 * time.Millisecond) // silence past the interval
	_, _ = pw.Write([]byte("data: second\n\n"))
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, ": keep-alive\n\n") {
		t.Fatalf("no keep-alive ping in stream, got %q", body)
	}
	if !strings.Contains(body, "data: first\n\n") || !strings.Contains(body, "data: second\n\n") {
		t.Fatalf("upstream events mangled, got %q", body)
	}
}

func TestCCCopyStreamNeverSplitsPartialLine(t *testing.T) {
	orig := sseKeepAliveInterval
	defer func() { sseKeepAliveInterval = orig }()
	sseKeepAliveInterval = 25 * time.Millisecond

	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		ccCopyStream(rec, pr)
		close(done)
	}()

	_, _ = pw.Write([]byte("data: partial-no-newline")) // no trailing \n
	time.Sleep(90 * time.Millisecond)                  // silence, but mid-line
	_, _ = pw.Write([]byte("-rest\n\n"))
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if strings.Contains(body, "keep-alive") {
		t.Fatalf("ping injected mid-line, got %q", body)
	}
	if body != "data: partial-no-newline-rest\n\n" {
		t.Fatalf("stream corrupted, got %q", body)
	}
}
```

Add imports as needed: `io`, `strings`, `testing`, `time`, `net/http/httptest`.

- [ ] **Step 2: Run tests to verify the first fails**

Run: `go test ./internal/api/ -run TestCCCopyStream -v`
Expected: `TestCCCopyStreamEmitsKeepAliveOnSilence` FAILS ("no keep-alive ping"). `TestCCCopyStreamNeverSplitsPartialLine` PASSES pre-fix (pins the safety invariant the implementation must keep).

- [ ] **Step 3: Implement**

Replace `ccCopyStream` in `internal/api/claudecode_proxy.go:275-294`:

```go
// ccCopyStream forwards the upstream body, flushing per chunk so SSE events
// reach the client as they arrive instead of at end of stream. A keep-alive
// comment is flushed after each interval of silence — but only at a line
// boundary, never inside a partially-written SSE line.
func ccCopyStream(writer http.ResponseWriter, body io.Reader) {
	flusher, hasFlusher := writer.(http.Flusher)
	bw := bufio.NewWriterSize(writer, 16*1024)
	keepAlive := newSSEKeepAlive(bw, writer)
	done := make(chan struct{})
	defer close(done)
	go keepAlive.run(done, sseKeepAliveInterval)

	lastByte := byte(0)
	keepAlive.pingAllowed = func() bool { return lastByte == 0 || lastByte == '\n' }

	keepAlive.mu.Lock()
	keepAlive.activateLocked()
	keepAlive.mu.Unlock()

	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			keepAlive.mu.Lock()
			_, werr := bw.Write(buf[:n])
			if werr == nil {
				werr = bw.Flush()
			}
			if werr == nil && hasFlusher {
				flusher.Flush()
			}
			if werr != nil {
				keepAlive.mu.Unlock()
				return
			}
			lastByte = buf[n-1]
			keepAlive.markWriteLocked()
			keepAlive.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
```

Add `"bufio"` to the file's imports if absent.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestCCCopyStream -v -race`
Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/claudecode_proxy.go internal/api/claudecode_proxy_test.go
git commit -m "feat(api): line-boundary-safe keep-alive pings in ccCopyStream"
```

---

### Task 6: log mid-stream upstream errors

**Files:**
- Modify: `internal/api/server.go:2973-2981` (error branch of `streamMessage`)
- Test: `internal/api/stream_message_keepalive_test.go` (append)

**Interfaces:**
- Consumes: `server.logger` (`*slog.Logger`, injectable via `Options.Logger`).
- Produces: mid-stream upstream failures logged at WARN with `model`, `elapsed`, `err`. Client-visible behavior unchanged (SSE `error` event still written).

- [ ] **Step 1: Write the failing test**

Append to `internal/api/stream_message_keepalive_test.go`:

```go
func TestStreamMessageLogsMidStreamError(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	payload1 := `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"I should ","thoughtSignature":"claude-signature-0123456789012345678901234567890123456789"}]}}],"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":20}}}`

	server, err := New(Options{
		APIKey: "test",
		Credentials: func(context.Context) (auth.Credentials, error) {
			return auth.Credentials{}, nil
		},
		Builder: proxyformat.NewBuilder(),
		Logger:  logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("upstream exploded")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	w := httptest.NewRecorder()
	server.streamMessage(w, req, func(ctx context.Context, _ map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
		if err := consume(cloudcode.SSEEvent{Event: "message", Data: []byte(payload1)}); err != nil {
			return cloudcode.Response{}, err
		}
		return cloudcode.Response{StatusCode: 200}, boom
	}, map[string]any{"messages": []any{}}, "claude-sonnet-4-6-thinking")

	// Client still gets the in-band error event.
	if !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("no SSE error event, got %q", w.Body.String())
	}
	// And the failure is now visible server-side.
	if !strings.Contains(logBuf.String(), "upstream exploded") {
		t.Fatalf("mid-stream error not logged, got %q", logBuf.String())
	}
}
```

Add imports: `bytes`, `errors`, `log/slog`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestStreamMessageLogsMidStreamError -v`
Expected: FAIL — "mid-stream error not logged".

- [ ] **Step 3: Add the log line**

In `streamMessage` (`internal/api/server.go:2973-2981`), change the error branch:

```go
		if err != nil {
			if !started {
				server.writeError(writer, err)
				return
			}
			if server.logger != nil {
				server.logger.Warn("upstream stream failed mid-flight",
					"model", model,
					"elapsed", server.nowTime().Sub(startTime).String(),
					"err", err)
			}
			errorEvent := map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}}
			_ = writeEvents([]map[string]any{errorEvent})
			return
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestStreamMessageLogsMidStreamError -v -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/server.go internal/api/stream_message_keepalive_test.go
git commit -m "fix(api): log mid-stream upstream errors instead of swallowing them"
```

---

### Task 7: full gate + docs touch-up

**Files:**
- Modify: `cmd/proxy/main.go:89` (flag help text only)

- [ ] **Step 1: Update the flag help text**

`-upstream-timeout` no longer caps stream duration; update the description:

```go
	upstreamTimeout := fs.Duration("upstream-timeout", 5*time.Minute, "Cloud Code unary request timeout; for streams this bounds time-to-first-byte only")
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 3: Full test suite with race detector**

Run: `make test`
Expected: PASS, no data races.

- [ ] **Step 4: Commit**

```bash
git add cmd/proxy/main.go
git commit -m "docs(proxy): clarify -upstream-timeout semantics for streams"
```

---

## Out of Scope (documented non-goals)

- **Kimi passthrough** (`internal/kimi/passthrough.go`): uses `httputil.ReverseProxy` with `FlushInterval: -1` and no total timeout. Adding proxy-generated pings there requires a response-rewriting wrapper; upstream silence risk is accepted for now.
- **OpenRouter path** (`internal/api/server.go:1806+`): already `Timeout: 0` + `headersCutoff`, and forwards upstream `:comment` lines. No changes.
- **Downstream `http.Server` timeouts** (`cmd/proxy/main.go:238-244`): `WriteTimeout` is already 0 (correct for SSE).

## Self-Review Notes

- Spec coverage: handoff Phase 2 items → Tasks 1-2; Phase 3 heartbeats → Tasks 3-5; observability blind spot found during investigation → Task 6; Phase 1 log inspection done pre-plan (no evidence possible due to blind spot; duration distribution showed no 5m-mark cluster).
- Type consistency: `sseKeepAlive` methods `activateLocked`/`markWriteLocked` used identically in Tasks 4 and 5; `sseKeepAliveInterval` defined in Task 3, overridden in Tasks 4-6 tests; `terminalStreamError` produced/consumed only in Task 1.
- Race safety: all shared state (`started`, `lastByte`, `lastWrite`, `active`) accessed under `keepAlive.mu`; `run()` goroutine exits via `defer close(done)` before handler returns.
