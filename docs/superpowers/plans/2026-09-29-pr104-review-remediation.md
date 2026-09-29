# PR #104 Review Remediation

**Goal:** Resolve the verified findings from the PR #104 review (native Claude Code Cloud integration): one blocking bug in the Cloud toggle, the forward-proxy robustness and privacy gaps, the Cloud tab polling and toggle problems, and the docs and probe-script claims that no longer match the code.

**Architecture:** Narrow fixes at the layer that owns each defect. `internal/api` validates the `mitm` patch over defaults instead of in-memory state. `internal/mitm` gets a resilient accept loop, one observation per exchange (recorded when the response head is forwarded for routes whose body is not parsed), a closed route vocabulary, bounded caches and a clean TLS close. The Cloud tab component gains tab-aware, single-flight polling. Docs and scripts are corrected to match what the code and the acceptance run showed. No TLS client configuration changes: the outbound handshake stays an empty `tls.Config{}`, and `internal/cloudcode` is untouched.

**Tech Stack:** Go 1.27rc2 (`go test`), Node 22 (`node` harness tests under `internal/webui/tests`), Python 3 (`unittest`), Alpine.js.

**Spec:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/104#issuecomment-5882333263

**Test commands:** Go: `go test ./internal/<pkg> -run <Name> -count=1`. WebUI harness: `go test ./internal/webui -run CloudSessions -count=1` (wraps `node internal/webui/tests/cloud-sessions.test.mjs`). Python: `python3 -m unittest discover -s scripts -p 'test_*.py' -v`.

**Commit trailer** (every commit): `Co-Authored-By: Claude <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_012fcHg5SNjARNuJjC8XcKNC`.

## Not addressed here

- `config.Save` refreshes the in-memory config by unmarshalling into a zero `Config{}` instead of `DefaultConfig()`. This is pre-existing and affects other sections; Task 1 stops the Cloud toggle depending on it. Follow-up.
- `/api/*` has no Origin/Host check and answers with `Access-Control-Allow-Origin: *`. Pre-existing management API behaviour.
- `/api/sessions/cloud` has no `?limit=`. The registry is capped at `registryMax` (default 1000).
- Registry still creates a session for a non-2xx exchange on a session-shaped path (nit).
- `LoadOrCreateCA` still regenerates on any load error (documented in Task 10).
- Twelve unrelated plan docs (PR #94-#102 remediation and Kimi) came in with `fb36bf2`; moving them is the author's call.
- `.reference/mitm-upstream-fingerprint-20260928.txt` has no pcap hashes or commit; the pcaps are not in the repo, so nothing can be added honestly.

## Task 1: Cloud toggle survives an unrelated config save

**Files:** Modify `internal/api/management.go` (the `mitm` hunk of `handleConfigSave`). Test `internal/api/mitm_management_test.go`.
**Consumes:** `config.DefaultMitmConfig()`, `MitmConfig.Validate()`. **Produces:** `POST /api/config {"mitm":{...}}` validated independently of `config.Get()`.

- [x] **Step 1: Write the failing test.** Add `fmt` and `os` to the imports, then:

```go
func postMitmConfig(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
	return rec
}

// config.Save refreshes the in-memory config from the merged file, so on an
// install whose config.json predates the mitm block config.Get().Mitm loses its
// defaults after any unrelated Save. The Cloud toggle must not depend on it.
func TestConfigSaveMitmToggleAfterUnrelatedSave(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"logLevel":"info"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Save(map[string]any{"debug": true}); err != nil {
		t.Fatal(err)
	}

	for _, enabled := range []bool{true, false} {
		rec := postMitmConfig(t, srv, fmt.Sprintf(`{"mitm":{"enabled":%t}}`, enabled))
		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%t: %d %s", enabled, rec.Code, rec.Body.String())
		}
		if got := config.Get().Mitm.Enabled; got != enabled {
			t.Fatalf("after enabled=%t, config.Get().Mitm.Enabled = %t", enabled, got)
		}
	}
}

func TestConfigSaveValidatesMitmBounds(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	for _, c := range []struct{ name, body, want string }{
		{"non-loopback listen", `{"mitm":{"listen":"0.0.0.0:8092"}}`, "loopback"},
		{"registryMax below range", `{"mitm":{"registryMax":0}}`, "registryMax"},
		{"registryMax above range", `{"mitm":{"registryMax":100001}}`, "registryMax"},
		{"ttl below range", `{"mitm":{"registryTtlMinutes":0}}`, "registryTtlMinutes"},
		{"ttl above range", `{"mitm":{"registryTtlMinutes":10081}}`, "registryTtlMinutes"},
		{"not an object", `{"mitm":"yes"}`, "Invalid mitm configuration format"},
	} {
		rec := postMitmConfig(t, srv, c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: code=%d body=%s, want 400 mentioning %q", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	for _, body := range []string{
		`{"mitm":{"registryMax":1,"registryTtlMinutes":1}}`,
		`{"mitm":{"registryMax":100000,"registryTtlMinutes":10080}}`,
	} {
		if rec := postMitmConfig(t, srv, body); rec.Code != http.StatusOK {
			t.Errorf("%s rejected: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/api -run 'TestConfigSaveMitm' -count=1` fails on the toggle test with `400 mitm listen "" must be host:port`; the bounds test already passes.
- [x] **Step 3: Implement.** In `handleConfigSave`, replace `merged := config.Get().Mitm` with `merged := config.DefaultMitmConfig()` and add a comment: the patch is validated over the defaults (the base `Load` uses) because `Save` refreshes the in-memory config from a file that may have no mitm block.
- [x] **Step 4: Confirm the pass.** `go test ./internal/api -run 'Mitm' -count=1`.
- [x] **Step 5: Commit.**

```sh
git add internal/api/management.go internal/api/mitm_management_test.go
git commit -m "fix(api): validate the mitm config patch over defaults, not in-memory state"
```

## Task 2: Accept loop retries temporary errors

**Files:** Modify `internal/mitm/server.go` (`Serve`). Test `internal/mitm/server_test.go`.
**Produces:** `Serve` keeps running through `EMFILE`-style errors, backing off 5 ms to 1 s like `net/http`.

- [x] **Step 1: Write the failing test.**

```go
type tempAcceptError struct{}

func (tempAcceptError) Error() string   { return "accept: too many open files" }
func (tempAcceptError) Timeout() bool   { return false }
func (tempAcceptError) Temporary() bool { return true }

// failOnceListener returns one temporary error from Accept, then behaves normally.
type failOnceListener struct {
	net.Listener
	failed atomic.Bool
}

func (l *failOnceListener) Accept() (net.Conn, error) {
	if l.failed.CompareAndSwap(false, true) {
		return nil, tempAcceptError{}
	}
	return l.Listener.Accept()
}

func TestServeRetriesTemporaryAcceptError(t *testing.T) {
	srv, err := New(Options{CA: newTestCA(t)})
	if err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(&failOnceListener{Listener: base}) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	raw, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	io.WriteString(raw, "GET http://example.org/ HTTP/1.1\r\nHost: example.org\r\n\r\n")
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(raw).ReadString('\n')
	if !strings.Contains(line, "405") {
		t.Fatalf("status line = %q: the accept loop did not survive the temporary error", line)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve returned: %v", err)
	default:
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/mitm -run TestServeRetriesTemporaryAcceptError -count=1` fails (`Serve` returned the temporary error, nothing answers).
- [x] **Step 3: Implement.** In `Serve`, keep a `delay time.Duration`; on an `Accept` error that is not `closing`, if `retryableAccept(err)` set `delay = min(max(2*delay, 5*time.Millisecond), time.Second)`, log a warning with the error and delay, `time.Sleep(delay)` and `continue`; otherwise `return err`. Reset `delay = 0` after a successful accept. Add:

```go
// retryableAccept reports whether an Accept error is transient: a timeout, or a
// temporary condition such as running out of file descriptors.
func retryableAccept(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var temp interface{ Temporary() bool }
	return errors.As(err, &temp) && temp.Temporary()
}
```

- [x] **Step 4: Confirm the pass.** `go test ./internal/mitm -run 'TestServe' -count=1`.
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/server.go internal/mitm/server_test.go
git commit -m "fix(mitm): keep accepting after a temporary accept error"
```

## Task 3: Route names never echo client text

**Files:** Modify `internal/mitm/observe.go` (`classifyRoute`). Test `internal/mitm/observe_test.go`.
**Produces:** route names drawn from a closed vocabulary; any non-standard method maps to `other`.

- [x] **Step 1: Write the failing test.**

```go
func TestClassifyRouteDoesNotEchoUnknownMethods(t *testing.T) {
	const id = "session_01ABCDEFGHJK"
	cases := []struct{ method, target, route string }{
		{"<svg/onload=alert(1)>", "/v1/code/sessions/" + id, "code.session.other"},
		{"BREW", "/v1/code/sessions/" + id + "/events", "code.session.events.other"},
		{"Get", "/v1/sessions/" + id, "sessions.other"},
	}
	for _, c := range cases {
		route, gotID := classifyRoute(c.method, c.target)
		if route != c.route || gotID != id {
			t.Errorf("%q %s = (%q,%q), want (%q,%q)", c.method, c.target, route, gotID, c.route, id)
		}
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/mitm -run TestClassifyRouteDoesNotEchoUnknownMethods -count=1` fails (`code.session.<svg/onload=alert(1)>`).
- [x] **Step 3: Implement.** Replace `lower := strings.ToLower(method)` with `verb := routeVerb(method)` and use `verb` in the three route names that used `lower`:

```go
// routeVerb lowercases a standard HTTP method for use in a route name. Any
// other token comes from the client's request line and is free text, so it
// maps to "other" instead of reaching the registry and the API.
func routeVerb(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return strings.ToLower(method)
	}
	return "other"
}
```

- [x] **Step 4: Confirm the pass.** `go test ./internal/mitm -run 'TestClassifyRoute' -count=1`.
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/observe.go internal/mitm/observe_test.go
git commit -m "fix(mitm): keep client-supplied method text out of route names"
```

## Task 4: Observe streams while open and when aborted

**Files:** Modify `internal/mitm/server.go` (`exchange`). Test `internal/mitm/server_test.go`.
**Produces:** exactly one observation per routed exchange. For routes whose body is not parsed it is recorded as soon as the response head is forwarded. For body-parsing routes it is recorded after the body, or with route and status only if the copy fails.

- [x] **Step 1: Write the failing test.**

```go
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamIsObservedWhileOpenAndOnceWhenAborted(t *testing.T) {
	release := make(chan struct{})
	up, roots := startFakeUpstream(t, func(_ string, _ []byte, conn net.Conn) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		io.WriteString(conn, "a\r\nevent: a\n\n\r\n")
		<-release
		io.WriteString(conn, "a\r\nevent: b\n\n\r\n0\r\n\r\n")
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /v1/code/sessions/session_01ABCDEFGHJK/events/stream HTTP/1.1\r\nHost: a\r\n\r\n")
	if _, err := readHead(br); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadString('\n'); err != nil { // first chunk header: the stream is live
		t.Fatal(err)
	}

	waitFor(t, "the open stream to be registered", func() bool { return len(h.registry.List()) == 1 })

	conn.Close() // the client aborts mid-stream
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil { // waits for the relay to finish
		t.Fatalf("shutdown: %v", err)
	}
	list := h.registry.List()
	if len(list) != 1 || list[0].Requests != 1 || list[0].LastRoute != "code.session.events.stream" {
		t.Fatalf("sessions = %+v, want one session observed once", list)
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/mitm -run TestStreamIsObservedWhileOpenAndOnceWhenAborted -count=1` times out waiting for the open stream to be registered.
- [x] **Step 3: Implement.** In `exchange`, build `obs := Observation{Route: route, RawID: rawID, Status: resp.Status}` once, right after the sink is chosen. If `route != "" && captured == nil`, call `s.observe(obs)` before `copyBody`. If `copyBody` fails, log as before, and when `route != "" && captured != nil` call `s.observe(obs)` (route and status survive, body fields do not) before returning false. After a successful copy, when `route != "" && captured != nil`, fill `obs.RawID`/`obs.Fields` from `summarizeBody` exactly as today (only when `captured.Bytes() != nil`) and call `s.observe(obs)`.
- [x] **Step 4: Confirm the pass.** `go test ./internal/mitm -count=1` (the existing `TestObserverRegistersCloudSession` must still pass).
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/server.go internal/mitm/server_test.go
git commit -m "fix(mitm): register streamed and aborted exchanges without waiting for the body"
```

## Task 5: Bound the leaf cache and the warned-host set

**Files:** Modify `internal/mitm/ca.go` (`LeafFor`), `internal/mitm/server.go` (`warnedHosts`, `terminate`). Test `internal/mitm/ca_test.go`, `internal/mitm/server_test.go`.
**Produces:** `const maxCachedLeaves = 256`, `const maxWarnedHosts = 64`, `(*CA).pruneLeavesLocked()`, `(*Server).firstFailureFor(host) bool`.

- [x] **Step 1: Write the failing tests.** In `ca_test.go` (add `fmt` to the imports):

```go
func TestLeafCacheIsBounded(t *testing.T) {
	ca := newTestCA(t)
	for i := 0; i < maxCachedLeaves+50; i++ {
		if _, err := ca.LeafFor(fmt.Sprintf("host%d.anthropic.com", i)); err != nil {
			t.Fatal(err)
		}
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if len(ca.leaves) > maxCachedLeaves {
		t.Fatalf("leaf cache holds %d entries, cap is %d", len(ca.leaves), maxCachedLeaves)
	}
	if _, ok := ca.leaves[fmt.Sprintf("host%d.anthropic.com", maxCachedLeaves+49)]; !ok {
		t.Fatal("the newest leaf must stay cached")
	}
}

func TestLeafCachePrunesExpiredLeavesFirst(t *testing.T) {
	ca := newTestCA(t)
	now := time.Now()
	ca.now = func() time.Time { return now }
	for i := 0; i < maxCachedLeaves; i++ {
		if _, err := ca.LeafFor(fmt.Sprintf("old%d.claude.ai", i)); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(leafLifetime + time.Hour)
	if _, err := ca.LeafFor("fresh.claude.ai"); err != nil {
		t.Fatal(err)
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if len(ca.leaves) != 1 {
		t.Fatalf("leaf cache holds %d entries, want only the fresh one", len(ca.leaves))
	}
}
```

In `server_test.go`:

```go
func TestHandshakeWarningsAreOncePerHostAndBounded(t *testing.T) {
	srv, err := New(Options{CA: newTestCA(t)})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.firstFailureFor("api.anthropic.com") {
		t.Fatal("the first failure for a host must warn")
	}
	if srv.firstFailureFor("api.anthropic.com") {
		t.Fatal("a second failure for the same host must not warn")
	}
	warned := 1
	for i := 0; i < maxWarnedHosts+20; i++ {
		if srv.firstFailureFor(fmt.Sprintf("h%d.claude.ai", i)) {
			warned++
		}
	}
	if warned != maxWarnedHosts {
		t.Fatalf("warned for %d hosts, want the cap of %d", warned, maxWarnedHosts)
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/mitm -run 'TestLeafCache|TestHandshakeWarnings' -count=1` fails to compile (`maxCachedLeaves`, `firstFailureFor` undefined).
- [x] **Step 3: Implement.** `ca.go`: add `maxCachedLeaves = 256` to the const block; before `ca.leaves[host] = leaf`, `if len(ca.leaves) >= maxCachedLeaves { ca.pruneLeavesLocked() }`, with

```go
// pruneLeavesLocked makes room for one more cached leaf: expired leaves go
// first, then arbitrary ones, so a client cycling through hostnames cannot
// grow the cache without bound.
func (ca *CA) pruneLeavesLocked() {
	now := ca.now()
	for host, leaf := range ca.leaves {
		if leaf.Leaf == nil || !now.Add(time.Hour).Before(leaf.Leaf.NotAfter) {
			delete(ca.leaves, host)
		}
	}
	for host := range ca.leaves {
		if len(ca.leaves) < maxCachedLeaves {
			break
		}
		delete(ca.leaves, host)
	}
}
```

`server.go`: replace `warnedHosts sync.Map` with `warnedHosts map[string]struct{}` (guarded by `s.mu`, initialised in `New`), add `const maxWarnedHosts = 64` and

```go
// firstFailureFor reports whether the handshake-failure warning should be
// logged for host: once per host, and for at most maxWarnedHosts hosts so
// names chosen by a client cannot grow the set without bound.
func (s *Server) firstFailureFor(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.warnedHosts[host]; seen || len(s.warnedHosts) >= maxWarnedHosts {
		return false
	}
	s.warnedHosts[host] = struct{}{}
	return true
}
```

and use `if s.firstFailureFor(host) { s.logger.Warn(...) }` in `terminate`.

- [x] **Step 4: Confirm the pass.** `go test ./internal/mitm -count=1 -race`.
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/ca.go internal/mitm/ca_test.go internal/mitm/server.go internal/mitm/server_test.go
git commit -m "fix(mitm): bound the leaf certificate cache and the warned-host set"
```

## Task 6: Send TLS close_notify when a terminated connection ends

**Files:** Modify `internal/mitm/server.go` (`terminate`). Test `internal/mitm/server_test.go`.
**Produces:** RFC 8446 section 6.1 compliance. Node 22 tolerates the missing alert (checked), so this is protocol hygiene.

- [x] **Step 1: Write the failing test.** TLS 1.2 keeps the record content type in the clear, so an alert is visible on the wire:

```go
// recordingConn records every byte the TLS client reads from the raw conn.
type recordingConn struct {
	net.Conn
	r   *bufio.Reader
	mu  sync.Mutex
	buf []byte
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	c.buf = append(c.buf, p[:n]...)
	c.mu.Unlock()
	return n, err
}

// lastRecordType returns the content type of the last complete TLS record.
func lastRecordType(stream []byte) byte {
	var last byte
	for len(stream) >= 5 {
		n := int(stream[3])<<8 | int(stream[4])
		if len(stream) < 5+n {
			break
		}
		last = stream[0]
		stream = stream[5+n:]
	}
	return last
}

func TestTerminatedConnectionEndsWithCloseNotify(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{"ok":true}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	raw, br := h.connect(t, "api.anthropic.com:443")
	rec := &recordingConn{Conn: raw, r: br}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.ca.CertPEM())
	tc := tls.Client(rec, &tls.Config{RootCAs: pool, ServerName: "api.anthropic.com", MaxVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(tc, "GET /a HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n")
	if _, err := io.ReadAll(tc); err != nil {
		t.Fatalf("read to EOF: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := lastRecordType(rec.buf); got != 21 {
		t.Fatalf("last TLS record type = %d, want 21 (close_notify alert)", got)
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/mitm -run TestTerminatedConnectionEndsWithCloseNotify -count=1` reports `last TLS record type = 23`.
- [x] **Step 3: Implement.** In `terminate`, after the handshake succeeds: `defer tlsConn.Close()` with a comment that it sends `close_notify` instead of dropping the TCP connection.
- [x] **Step 4: Confirm the pass.** `go test ./internal/mitm -count=1`.
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/server.go internal/mitm/server_test.go
git commit -m "fix(mitm): close terminated TLS connections with close_notify"
```

## Task 7: Close the listener when Serve loses the race with Shutdown

**Files:** Modify `internal/mitm/server.go` (`Serve`). Test `internal/mitm/server_test.go` (add `errors` and `net/http` to the imports).

- [x] **Step 1: Write the failing test.**

```go
func TestServeAfterShutdownClosesTheListener(t *testing.T) {
	srv, err := New(Options{CA: newTestCA(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve = %v, want http.ErrServerClosed", err)
	}
	l.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the listener is still open after Serve refused it: %v", err)
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/mitm -run TestServeAfterShutdownClosesTheListener -count=1` reports the listener is still open (a deadline error, not `net.ErrClosed`).
- [x] **Step 3: Implement.** In `Serve`, when `s.closing` is already set, unlock, `l.Close()` and return `http.ErrServerClosed`.
- [x] **Step 4: Confirm the pass.** `go test ./internal/mitm -count=1`.
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/server.go internal/mitm/server_test.go
git commit -m "fix(mitm): close the listener when Serve is refused after Shutdown"
```

## Task 8: Serve only the CA certificate from ca.pem

**Files:** Modify `internal/mitm/ca.go` (`loadCA`). Test `internal/mitm/ca_test.go` (add `bytes` to the imports).

- [x] **Step 1: Write the failing test.**

```go
func TestLoadedCAServesOnlyItsCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mitm")
	first, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(append([]byte(nil), certPEM...), keyPEM...) // certificate, then private key
	if err := os.WriteFile(filepath.Join(dir, caCertFile), bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ca.Fingerprint() != first.Fingerprint() {
		t.Fatal("the CA was regenerated instead of loaded")
	}
	if bytes.Contains(ca.CertPEM(), []byte("PRIVATE KEY")) {
		t.Fatal("CertPEM served key material from a bundled ca.pem")
	}
	if !bytes.Equal(ca.CertPEM(), certPEM) {
		t.Fatalf("CertPEM = %q, want the single certificate block", ca.CertPEM())
	}
}
```

- [x] **Step 2: Confirm the failure.** `go test ./internal/mitm -run TestLoadedCAServesOnlyItsCertificate -count=1` fails with key material served.
- [x] **Step 3: Implement.** In `loadCA`, after the certificate parses, set `certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})` with a comment that the export is re-encoded from the parsed certificate so an extra PEM block can never be served.
- [x] **Step 4: Confirm the pass.** `go test ./internal/mitm -count=1`.
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/ca.go internal/mitm/ca_test.go
git commit -m "fix(mitm): re-encode the CA certificate instead of serving ca.pem verbatim"
```

## Task 9: Cloud tab component and view

**Files:** Modify `internal/webui/public/js/components/cloud-sessions.js`, `internal/webui/public/views/settings.html`, `internal/webui/public/js/translations/en.js`, `internal/webui/public/js/translations/pt.js`. Create `internal/webui/tests/cloud-sessions.harness.mjs`, `internal/webui/tests/cloud-sessions.test.mjs`, `internal/webui/cloud_sessions_test.go`.
**Produces:** no fetch before the tab opens; polling only while Settings > Cloud is the active tab and the page is visible; `refresh({withConfig})` single-flight; `/api/config` read only when the tab opens or on Refresh; polling stops on a 401; the enable toggle reverts when the POST fails and is shown in both states; the CA download revokes its object URL after the browser starts it; the env snippet includes `NO_PROXY=127.0.0.1,localhost`.

- [x] **Step 1: Write the failing tests.** `cloud-sessions.harness.mjs` loads the component in a `vm` sandbox with a fake Alpine store (`activeTab`, `settingsTab`), manual `setInterval`/`setTimeout`, a mutable `document.hidden`, recording `URL.revokeObjectURL`, `$watch` callbacks kept by expression, and a `window.utils.request` queue like `kimi-poll-race.harness.mjs`. `cloud-sessions.test.mjs` covers, in order:
  1. `init()` on another tab makes no request and starts no timer.
  2. `init()` while Settings > Cloud is already the active tab (hash navigation) activates at once.
  3. Opening the tab (settingsTab watcher) requests `/api/config`, `/api/mitm/status` and `/api/sessions/cloud` once and starts one timer.
  4. A timer tick requests only status and sessions.
  5. A tick while `document.hidden` requests nothing, and the next visible tick does.
  6. Leaving Settings (activeTab watcher) or switching to another Settings tab clears the timer.
  7. A tick while a refresh is in flight adds no requests, and a failed refresh does not leave the component stuck in flight.
  8. A 401 on any request stops polling and leaves `error = 'HTTP 401'`; `activate()` (the Refresh button) resumes polling.
  9. `setEnabled(input)` with a failed POST sets `input.checked = false` (reverting the click), keeps `configuredEnabled` and toasts the error.
  10. `setEnabled(input)` with a successful POST keeps the box, updates `configuredEnabled` and toasts success.
  11. `downloadCA()` appends, clicks and removes the link, does not revoke synchronously, and revokes the same URL once the timer fires.
  12. `envSnippet()` contains `NO_PROXY=127.0.0.1,localhost` and the listen address.

  `cloud_sessions_test.go` runs the file with `node` (skipped when `node` is missing), like `kimi_poll_race_test.go`.
- [x] **Step 2: Confirm the failure.** `go test ./internal/webui -run CloudSessions -count=1` fails (eager fetch at `init`, no revert, immediate revoke, no `NO_PROXY`).
- [x] **Step 3: Implement.**
  - Component: add `_inflight: false`; `onCloudTab()` (`activeTab === 'settings' && settingsTab === 'cloudsessions'`); `activate()` (`startPolling()` then `refresh({ withConfig: true })`); `init()` registers `$watch` on `$store.global.settingsTab` and `$store.global.activeTab` that call `activate()` when `onCloudTab()` else `stopPolling()`, and activates immediately only if already on the tab; `startPolling()` ticks call `refresh()` unless `document.hidden`; `refresh({ withConfig = false } = {})` returns early when `_inflight`, fetches status and sessions (plus `/api/config` when asked) and always clears `_inflight`; `getJSON` calls `stopPolling()` on a 401 before throwing; `setEnabled(input)` reads `input.checked` and restores `input.checked = !value` in the catch; `downloadCA()` appends the anchor, clicks, removes it and revokes in `setTimeout(..., 1000)`; `envSnippet()` adds `NO_PROXY=127.0.0.1,localhost`.
  - View: the Refresh button calls `activate()`; move the enable label and restart note out of the `!status.enabled` block so both states show them; `@change="setEnabled($event.target)"`.
  - Translations (`cloudSessionsRestartNote`, en and pt): say the listener binds loopback addresses only.
- [x] **Step 4: Confirm the pass.** `go test ./internal/webui -count=1` (includes the translation parity test).
- [x] **Step 5: Commit.**

```sh
git add internal/webui
git commit -m "fix(webui): poll the Cloud tab only while visible and keep the enable toggle honest"
```

## Task 10: User doc

**Files:** Modify `docs/claude-code-forward-proxy.md`.

- [x] **Step 1:** Qualify "inference still uses `ANTHROPIC_BASE_URL` as before" with the `NO_PROXY` requirement; add `NO_PROXY=127.0.0.1,localhost` to the step 3 command; add a troubleshooting bullet for `405 status code (no body)` (a plain `http://` gateway URL sent through the proxy as absolute-URI requests, cleared by `NO_PROXY` or by unsetting the proxy variables for that process); document the CA lifetime (365 days, regenerated with a new key within 7 days of expiry, so downloaded copies go stale and show up as "TLS trust failures") and that any unreadable CA file is regenerated.
- [x] **Step 2:** Check the doc against the code: `grep -n 'caLifetime\|caRenewWindow' internal/mitm/ca.go` matches the stated numbers.
- [x] **Step 3: Commit.**

```sh
git add docs/claude-code-forward-proxy.md
git commit -m "docs: document NO_PROXY for the forward proxy and the CA lifetime"
```

## Task 11: Stale research and design docs

**Files:** Modify `docs/superpowers/specs/2026-09-28-claude-code-cloud-feasibility.md`, `docs/superpowers/specs/2026-09-28-claude-code-forward-proxy-design.md`, `docs/superpowers/plans/2026-09-28-claude-code-cloud-open-questions-handoff.md`, `docs/superpowers/plans/2026-09-28-claude-code-forward-proxy.md`, `.reference/mitm-upstream-fingerprint-20260928.txt`.

- [x] **Step 1:** Feasibility report: mark §6/§7 (option (a) rejected, "No change upstream", "Mitigation: don't build (a)") as superseded by the design doc and correct the fingerprint row to say the proxy's upstream ClientHello differs from the CLI's (evidence file); fix "(both uncommitted)" and "Tested offline" for the two probe scripts.
- [x] **Step 2:** Design spec: status line (implemented in PR #104), dependency line (`github.com/andybalholm/brotli`), file list (`observe.go`, `registry.go`, `server.go`, `httpwire.go`, `runtime.go`, `ca.go`).
- [x] **Step 3:** Handoff: mark superseded (its target file was never created; feasibility §9 records the answers). Plan Step 5 result: scope "`code.session.events.post` never crosses the client machine" to the interactive create flow, since Probe C captured that POST.
- [x] **Step 4:** Evidence file: drop "GREASE" and add a dated correction. Verified first with a throwaway Go program that parses the raw ClientHello the Go client sends to a local listener: 13 cipher suites, 11 extensions, no GREASE value in any list.
- [x] **Step 5: Commit.**

```sh
git add docs .reference/mitm-upstream-fingerprint-20260928.txt
git commit -m "docs: bring the Claude Code cloud research and design docs in line with the implementation"
```

## Task 12: Probe scripts

**Files:** Modify `scripts/mitm_cloud_session_probe.py`, `scripts/probe-cloud-session-api.sh`, `scripts/probe-agy-session-id.sh`. Create `scripts/test_mitm_cloud_session_probe.py`.

- [x] **Step 1: Write the failing test** (`unittest`, importing `shape` and `mask_path`):

```python
class ShapeRedactionTests(unittest.TestCase):
    def test_free_text_keys_never_keep_their_value(self):
        for key in ("source", "reason", "code"):
            self.assertEqual(shape({key: "acme/private-repo"})[key], "string")

    def test_repo_slugs_and_host_ports_are_not_kept_under_enum_keys(self):
        for value in ("octo-org/secret.git", "acme/private-repo", "host:8080"):
            self.assertEqual(shape({"type": value})["type"], "string", value)

    def test_enum_values_are_still_kept(self):
        self.assertEqual(shape({"type": "user", "model": "claude-opus-5-5"}),
                         {"model": "string:claude-opus-5-5", "type": "string:user"})

    def test_ids_are_not_kept_even_under_enum_keys(self):
        self.assertEqual(shape({"type": "session_01ABCDEFGHJK"})["type"], "string")

    def test_ids_in_paths_are_masked(self):
        path, _ = mask_path("/v1/code/sessions/session_01ABCDEFGHJK/events")
        self.assertEqual(path, "/v1/code/sessions/session_{id}/events")
```

- [x] **Step 2: Confirm the failure.** `python3 -m unittest scripts.test_mitm_cloud_session_probe -v` (or the `discover` form) fails on the first two tests.
- [x] **Step 3: Implement.** Remove `source`, `reason` and `code` from `ENUM_KEYS`; make `_TOKEN` reject `/` and `:` (`^[A-Za-z0-9_.-]{1,40}$`). `probe-cloud-session-api.sh`: commit with `-c commit.gpgsign=false -c core.hooksPath=/dev/null` and drop the reference to a stage 4 fallback that does not exist. `probe-agy-session-id.sh`: say the summary holds field names, 8-character hashes and up to 40 characters of surrounding binary strings, and to review it before pasting. Check both shell scripts with `bash -n`.
- [x] **Step 4: Confirm the pass.** `python3 -m unittest discover -s scripts -p 'test_*.py' -v`.
- [x] **Step 5: Commit.**

```sh
git add scripts
git commit -m "fix(scripts): tighten the cloud session probe redaction and correct the wizard notes"
```

## Task 13: Test hardening

**Files:** Modify `internal/mitm/server_test.go`, `internal/api/mitm_management_test.go`, `internal/config/mitm_test.go`.

- [x] **Step 1: `TestSSEStreamsIncrementally`.** Replace the final `io.ReadAll` (which waits for the 2 s deadline and passes by matching "timeout") with a read of exactly the remaining chunks up to the `0\r\n\r\n` terminator, so the test no longer depends on a deadline.
- [x] **Step 2: API tests.** Type-check `sessions[0]` instead of panicking; check the by-id and status payloads for the raw session id; add a 404 for an unknown id while the proxy runs; make the password test restore `config` in `t.Cleanup`, cover `/api/sessions/cloud/{id}`, and add a positive `x-webui-password` case.
- [x] **Step 3: Config tests.** Assert which rule fired (error substrings), add the boundary values (1 and 100000, 1 and 10080), negatives and an empty `Listen`.
- [x] **Step 4: Confirm.** `go test ./internal/mitm ./internal/api ./internal/config -count=1` passes, and `go test ./internal/mitm -run TestSSEStreamsIncrementally -v` no longer takes about 2 s.
- [x] **Step 5: Commit.**

```sh
git add internal/mitm/server_test.go internal/api/mitm_management_test.go internal/config/mitm_test.go
git commit -m "test: tighten the mitm SSE, management and config assertions"
```

## Final gate

- `gofmt -l $(git diff --name-only origin/main...HEAD | grep '\.go$')` prints nothing.
- `go build ./... && go vet ./...`.
- `go test ./... -count=1` passes except `internal/auth`, which needs the `agy` binary that this sandbox lacks. Prove that by running `go test ./internal/auth` on `origin/main` and getting the same failure.
- `go test -race ./internal/mitm ./internal/config ./internal/api ./internal/webui -count=1`.
- `python3 -m unittest discover -s scripts -p 'test_*.py'`.
- `git diff --stat origin/feat/claude-code-cloud..HEAD` shows nothing under `internal/cloudcode`, and the outbound `tls.Config` in `dialUpstream` is still the empty default.

## Results

All thirteen tasks are done, each as its own commit on top of the PR head. Every Go and Python test was written first and seen to fail for the stated reason before the fix (Task 1: `400 mitm listen "" must be host:port`; Task 2: no response after the temporary accept error; Task 3: `code.session.<svg/onload=alert(1)>`; Task 4: timed out waiting for the open stream; Tasks 5 and 8: undefined symbols and key material served; Task 6: last TLS record type 23; Task 7: listener still open; Task 9: `init fetched before the tab was opened`; Task 12: two of five tests failed).

Final gate:
- `gofmt`, `go build ./...`, `go vet ./...` and `go mod tidy -diff` are clean.
- `go test ./... -count=1`: 28 packages pass, 2127 tests pass, 1 skipped. The only failures are four `internal/auth` tests (`TestGetAuthorizationURL` and `TestOAuthManager_HTTPHandler` with two subtests), which fail identically on `origin/main` because the `agy` binary is not installed in this sandbox (`agy executable not found; set AGY_BINARY_PATH`).
- `go test -race ./internal/mitm ./internal/config ./internal/api ./internal/webui -count=1` passes.
- `python3 -m unittest discover -s scripts -p 'test_*.py'`: 86 tests, the same two import errors as on `origin/main` (`test_check_laya`, `test_finetune_laya` need `pytest`, which is not installed here). The mitm-related modules pass.
- `internal/cloudcode` is untouched and the upstream handshake in `dialUpstream` is still an empty `tls.Config{}`.

Checked beyond the unit tests:
- Settings > Cloud in Chromium against a running proxy whose `config.json` had no `mitm` block: no Cloud request at page load, one status/sessions/config fetch on opening the tab, the enable toggle POST succeeds after an unrelated config save, one poll tick per 5 s while visible and none after leaving Settings, the toggle is present and reverts on a rejected save in the running state, and the env snippet carries `NO_PROXY`.
- A Node 22 TLS client against the proxy still reads complete close-delimited and `Connection: close` responses after the `close_notify` change.
- One pre-existing WebUI page error (`Unexpected identifier 's'`, raised inside Alpine) shows up identically with the `main` and PR-head views, so it is not part of this PR.
