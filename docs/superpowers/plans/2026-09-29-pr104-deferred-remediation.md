# PR #104 Review Deferred Items Remediation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the 6 items deferred during the PR #104 review: prevent `config.Save` from zeroing unrelated config sections, enforce Origin/CORS validation on `/api/*`, support `?limit=` on the cloud sessions list, prevent non-2xx responses from generating phantom sessions, isolate the 12 unrelated plan docs to a dedicated branch/PR, and document pcap provenance and SHA-256 hashes in the fingerprint record.

**Architecture:**
- Config: initialize `updatedConfig := DefaultConfig()` in `config.Save` so unmarshaling a partial config payload preserves all defaults for unmentioned sections, mirroring `config.Load`.
- API Security: implement loopback and same-host Origin checks on `/api/*` and `/account-limits`, denying cross-origin browser requests and preflights with 403 Forbidden while leaving `/v1/*` inference endpoints open with `Access-Control-Allow-Origin: *`.
- Sessions API: parse optional `?limit=` query parameter (defaulting to 50) and return `{"enabled": true, "total": total, "sessions": [...]}` from `/api/sessions/cloud`.
- MITM Observer: in `Registry.Observe`, require successful response status (`o.Status == 0 || (o.Status >= 200 && o.Status < 300) || o.Status == http.StatusSwitchingProtocols`) before creating a new session entry, preventing 404s/4xx from creating phantom sessions.
- Git & Reference Hygiene: branch the 12 unrelated historical plan docs to `docs/pr94-pr102-remediation-plans` and remove them from the PR #104 branch; document the exact commit (`5e446bc`) and SHA-256 hashes for the 2026-09-28 pcap captures in `.reference/mitm-upstream-fingerprint-20260928.txt`.

**Tech Stack:** Go 1.27rc2, standard library `net/http`, `crypto/tls`, `crypto/sha256`, Git.

**Spec:** PR #104 review comments (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/104#issuecomment-5882333263) and `docs/superpowers/plans/2026-09-29-pr104-review-remediation.md:L724-L734`.

## Global Constraints

- Do NOT touch TLS internals in `internal/cloudcode` or client connections to Cloud Code (empty `tls.Config{}`).
- Follow existing patterns and code conventions in `internal/config`, `internal/api`, and `internal/mitm`.
- Backward compatibility: `/v1/*` inference endpoints MUST continue allowing cross-origin requests (`Access-Control-Allow-Origin: *`).
- The WebUI at `http://127.0.0.1:8091` and `http://localhost:8091` MUST continue functioning seamlessly without CORS or Origin blocks.
- Non-browser API clients (curl, scripts, CLI) without an `Origin` header MUST NOT be broken.
- No placeholders; each step must be executable and verified.

---

### Task 1: Fix `config.Save` In-Memory Zero-Value Refresh

**Files:**
- Modify: `internal/config/config.go:1051-1055`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `DefaultConfig()` from `internal/config/config.go`
- Produces: `Save(updates map[string]any) (Config, error)` preserving canonical defaults for all sections not present in the persisted JSON or update map.

- [ ] **Step 1: Write the failing test**

Add `TestSave_PreservesDefaultsForUnrelatedSections` to `internal/config/config_test.go`:

```go
func TestSave_PreservesDefaultsForUnrelatedSections(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	// Create an existing config.json that only defines port, simulating an older
	// install or minimal config before sections like mitm and routing were added.
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"port": 8091}`), 0644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	if _, err := Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Verify pre-save defaults are intact
	initial := Get()
	if initial.Mitm.Listen != "127.0.0.1:8092" || initial.Routing.FailureThreshold != 10 {
		t.Fatalf("initial config missing defaults: Mitm.Listen=%q, FailureThreshold=%d",
			initial.Mitm.Listen, initial.Routing.FailureThreshold)
	}

	// Save an unrelated setting (e.g. debug toggle)
	updated, err := Save(map[string]any{"debug": true})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// The returned Config and config.Get() must NOT have zeroed unmentioned sections
	if updated.Mitm.Listen != "127.0.0.1:8092" {
		t.Errorf("Save zeroed updated.Mitm.Listen: got %q, want %q", updated.Mitm.Listen, "127.0.0.1:8092")
	}
	if updated.Mitm.RegistryMax != 1000 {
		t.Errorf("Save zeroed updated.Mitm.RegistryMax: got %d, want 1000", updated.Mitm.RegistryMax)
	}
	if updated.Routing.FailureThreshold != 10 {
		t.Errorf("Save zeroed updated.Routing.FailureThreshold: got %d, want 10", updated.Routing.FailureThreshold)
	}

	inMemory := Get()
	if inMemory.Mitm.Listen != "127.0.0.1:8092" {
		t.Errorf("Save zeroed Get().Mitm.Listen: got %q, want %q", inMemory.Mitm.Listen, "127.0.0.1:8092")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/config -run TestSave_PreservesDefaultsForUnrelatedSections`
Expected: FAIL with `Save zeroed updated.Mitm.Listen: got "", want "127.0.0.1:8092"`.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, locate line 1051:
```go
	var updatedConfig Config
	if err := json.Unmarshal(encoded, &updatedConfig); err == nil {
		currentConfig = updatedConfig
	}
```
Replace it with:
```go
	updatedConfig := DefaultConfig()
	if err := json.Unmarshal(encoded, &updatedConfig); err == nil {
		currentConfig = updatedConfig
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/config -run TestSave_PreservesDefaultsForUnrelatedSections`
Expected: PASS
Run entire config test suite: `go test -v ./internal/config`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "fix(config): preserve defaults for unmentioned sections in Save"
```

---

### Task 2: Implement Origin Check for Management Routes (`/api/*` and `/account-limits`)

**Files:**
- Modify: `internal/api/server.go:370-380,3714-3718`
- Modify: `internal/api/management.go:42-65`
- Test: `internal/api/management_test.go`

**Interfaces:**
- Consumes: HTTP request `Origin` header and `request.Host`
- Produces: `isAllowedManagementOrigin(originHeader, host string) bool` returning `true` only for same-host or loopback origins.
- Behavior:
  - If `Origin` is missing (CLI, curl, same-origin GET): allow.
  - If `Origin` is present and matches `request.Host` or is loopback on the proxy's port: allow and set `Access-Control-Allow-Origin: <origin>` with `Vary: Origin`.
  - If `Origin` is untrusted (`https://evil.com`, `null`, unauthorized domain):
    - OPTIONS preflight: 403 Forbidden with no `Access-Control-Allow-Origin`.
    - Other methods: 403 Forbidden with `{"status":"error","error":"cross-origin access forbidden"}`.
  - Routes under `/v1/*` continue using `setCORS(writer)` (`Access-Control-Allow-Origin: *`).

- [ ] **Step 1: Write the failing tests**

Add `TestManagementAPI_OriginChecks` to `internal/api/management_test.go`:

```go
func TestManagementAPI_OriginChecks(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)

	testCases := []struct {
		name         string
		method       string
		path         string
		origin       string
		host         string
		wantCode     int
		wantCORSWild bool
	}{
		{
			name:     "no origin header allowed",
			method:   http.MethodGet,
			path:     "/api/accounts",
			origin:   "",
			host:     "127.0.0.1:8091",
			wantCode: http.StatusOK,
		},
		{
			name:     "matching host origin allowed",
			method:   http.MethodGet,
			path:     "/api/accounts",
			origin:   "http://127.0.0.1:8091",
			host:     "127.0.0.1:8091",
			wantCode: http.StatusOK,
		},
		{
			name:     "loopback localhost origin allowed",
			method:   http.MethodGet,
			path:     "/api/accounts",
			origin:   "http://localhost:8091",
			host:     "127.0.0.1:8091",
			wantCode: http.StatusOK,
		},
		{
			name:     "external untrusted origin rejected with 403",
			method:   http.MethodGet,
			path:     "/api/accounts",
			origin:   "https://evil.com",
			host:     "127.0.0.1:8091",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "null origin rejected with 403",
			method:   http.MethodGet,
			path:     "/api/accounts",
			origin:   "null",
			host:     "127.0.0.1:8091",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "untrusted origin options preflight rejected with 403",
			method:   http.MethodOptions,
			path:     "/api/accounts",
			origin:   "https://evil.com",
			host:     "127.0.0.1:8091",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "trusted origin options preflight allowed with 204",
			method:   http.MethodOptions,
			path:     "/api/accounts",
			origin:   "http://127.0.0.1:8091",
			host:     "127.0.0.1:8091",
			wantCode: http.StatusNoContent,
		},
		{
			name:         "v1 inference endpoint retains wildcard cors",
			method:       http.MethodOptions,
			path:         "/v1/messages",
			origin:       "https://external-client.com",
			host:         "127.0.0.1:8091",
			wantCode:     http.StatusNoContent,
			wantCORSWild: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Errorf("%s: got status %d, want %d", tc.name, rec.Code, tc.wantCode)
			}
			if tc.wantCORSWild {
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
					t.Errorf("%s: got allow-origin %q, want *", tc.name, got)
				}
			} else if tc.wantCode == http.StatusForbidden {
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
					t.Errorf("%s: forbidden request should not have Access-Control-Allow-Origin, got %q", tc.name, got)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/api -run TestManagementAPI_OriginChecks`
Expected: FAIL on `external untrusted origin rejected with 403` (currently returns 200 or 401 with `Access-Control-Allow-Origin: *`).

- [ ] **Step 3: Implement Origin validation**

1. In `internal/api/server.go`, implement helper functions:

```go
func isLoopbackHost(hostname string) bool {
	h := strings.ToLower(strings.TrimSpace(hostname))
	return h == "127.0.0.1" || h == "localhost" || h == "::1" || h == "[::1]"
}

func isAllowedManagementOrigin(originHeader, reqHost string) bool {
	if originHeader == "" {
		return true
	}
	u, err := url.Parse(originHeader)
	if err != nil || u.Host == "" {
		return false
	}
	originHost := u.Host
	if strings.EqualFold(originHost, reqHost) {
		return true
	}
	// Check loopback equivalence: both must be loopback and ports must match
	originHostname := u.Hostname()
	reqHostname, reqPort, err := net.SplitHostPort(reqHost)
	if err != nil {
		reqHostname = reqHost
		reqPort = ""
	}
	if isLoopbackHost(originHostname) && isLoopbackHost(reqHostname) {
		originPort := u.Port()
		if originPort == reqPort {
			return true
		}
	}
	return false
}
```

2. In `internal/api/server.go`, update `serveHTTP`:
Separate management requests (`/api/*` except `/api/event_logging/batch`, and `/account-limits`) from inference requests (`/v1/*`):

```go
func (server *Server) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if path == "/anthropic" {
		path = "/"
	} else if strings.HasPrefix(path, "/anthropic/") {
		path = strings.TrimPrefix(path, "/anthropic")
	}

	isManagement := (strings.HasPrefix(path, "/api/") && path != "/api/event_logging/batch") || path == "/account-limits"

	if isManagement {
		origin := request.Header.Get("Origin")
		if origin != "" {
			if !isAllowedManagementOrigin(origin, request.Host) {
				if request.Method == http.MethodOptions {
					writer.WriteHeader(http.StatusForbidden)
					return
				}
				writeJSON(writer, http.StatusForbidden, map[string]any{
					"status": "error",
					"error":  "cross-origin access forbidden",
				})
				return
			}
			writer.Header().Set("Access-Control-Allow-Origin", origin)
			writer.Header().Set("Access-Control-Allow-Headers", "authorization, content-type, x-api-key, anthropic-version, anthropic-beta")
			writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			writer.Header().Set("Vary", "Origin")
		}
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	} else {
		setCORS(writer)
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// First try management handlers (/health, /account-limits, /api/*)
	if server.handleManagement(writer, request, path) {
		return
	}
...
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/api -run TestManagementAPI_OriginChecks`
Expected: PASS
Run all tests in `internal/api`: `go test ./internal/api -run "TestManagement.*"`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/server.go internal/api/management_test.go
git commit -m "fix(api): block untrusted cross-origin requests to management endpoints"
```

---

### Task 3: Add `?limit=` Query Parameter and `total` to Cloud Sessions List

**Files:**
- Modify: `internal/api/mitm_management.go:39-46`
- Test: `internal/api/mitm_management_test.go`

**Interfaces:**
- Consumes: `GET /api/sessions/cloud?limit=<n>`
- Produces: JSON payload:
  ```json
  {
    "enabled": true,
    "total": 120,
    "sessions": [...]
  }
  ```
  where `total` is total live sessions in registry and `sessions` is capped by `limit` (default: 50, minimum: 1, maximum: 1000).

- [ ] **Step 1: Write the failing test**

Add `TestCloudSessionsList_LimitAndTotal` to `internal/api/mitm_management_test.go`:

```go
func TestCloudSessionsList_LimitAndTotal(t *testing.T) {
	srv, reg := newTestServerWithRegistry(t, 100)

	// Populate 10 fake sessions
	for i := 0; i < 10; i++ {
		reg.Observe(mitm.Observation{
			Route:  "sessions.create",
			RawID:  fmt.Sprintf("session_test_%02d", i),
			Status: 200,
			Fields: map[string]string{"model": "claude-opus-5-5"},
		})
	}

	// 1. Default limit test (should return total=10, all 10 since 10 < default 50)
	_, listDefault := getJSON(t, srv, "/api/sessions/cloud")
	if total, ok := listDefault["total"].(float64); !ok || int(total) != 10 {
		t.Fatalf("want total=10, got %v", listDefault["total"])
	}
	sessions, _ := listDefault["sessions"].([]any)
	if len(sessions) != 10 {
		t.Fatalf("want 10 sessions, got %d", len(sessions))
	}

	// 2. Explicit ?limit=3
	_, listLimit3 := getJSON(t, srv, "/api/sessions/cloud?limit=3")
	if total, ok := listLimit3["total"].(float64); !ok || int(total) != 10 {
		t.Fatalf("want total=10, got %v", listLimit3["total"])
	}
	sessions3, _ := listLimit3["sessions"].([]any)
	if len(sessions3) != 3 {
		t.Fatalf("want 3 sessions with ?limit=3, got %d", len(sessions3))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/api -run TestCloudSessionsList_LimitAndTotal`
Expected: FAIL (`want total=10, got <nil>`).

- [ ] **Step 3: Implement `?limit=` and `total`**

In `internal/api/mitm_management.go`:
```go
// handleCloudSessionsList lists observed Claude Code cloud sessions.
func (server *Server) handleCloudSessionsList(writer http.ResponseWriter, request *http.Request) {
	rt := server.mitm
	if rt == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"enabled": false, "total": 0, "sessions": []any{}})
		return
	}

	all := rt.Registry.List()
	total := len(all)
	limit := 50
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}

	sessions := all
	if limit < len(sessions) {
		sessions = sessions[:limit]
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"enabled":  true,
		"total":    total,
		"sessions": sessions,
	})
}
```

Make sure `strconv` is imported in `internal/api/mitm_management.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/api -run TestCloudSessionsList`
Expected: PASS
Run existing mitm management tests: `go test -v ./internal/api -run "TestMitm.*|TestCloud.*"`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/mitm_management.go internal/api/mitm_management_test.go
git commit -m "feat(api): add ?limit= parameter and total count to /api/sessions/cloud"
```

---

### Task 4: Prevent Non-2xx Responses from Creating Phantom Sessions in MITM Registry

**Files:**
- Modify: `internal/mitm/registry.go:57-71`
- Test: `internal/mitm/observe_test.go`
- Test: `internal/mitm/server_test.go`

**Interfaces:**
- Consumes: `Observation{Route, RawID, Status, Fields}`
- Produces: `Registry.Observe(o Observation)` creates a session ONLY if `ok` (already exists) OR `o.Status` is successful (`o.Status == 0 || (o.Status >= 200 && o.Status < 300) || o.Status == http.StatusSwitchingProtocols`).

- [ ] **Step 1: Write the failing tests**

1. In `internal/mitm/observe_test.go`, add `TestRegistry_DoesNotCreatePhantomSessionOnNon2xx`:

```go
func TestRegistry_DoesNotCreatePhantomSessionOnNon2xx(t *testing.T) {
	reg := NewRegistry(10, time.Hour, nil)

	// An observation for an unknown session ID with 404 Not Found
	reg.Observe(Observation{
		Route:  "sessions.get",
		RawID:  "session_nonexistent_404",
		Status: 404,
	})
	if len(reg.List()) != 0 {
		t.Fatalf("404 must not create a session, got: %v", reg.List())
	}

	// 403 Forbidden
	reg.Observe(Observation{
		Route:  "sessions.get",
		RawID:  "session_forbidden_403",
		Status: 403,
	})
	if len(reg.List()) != 0 {
		t.Fatalf("403 must not create a session, got: %v", reg.List())
	}

	// 500 Internal Server Error
	reg.Observe(Observation{
		Route:  "sessions.get",
		RawID:  "session_error_500",
		Status: 500,
	})
	if len(reg.List()) != 0 {
		t.Fatalf("500 must not create a session, got: %v", reg.List())
	}

	// 200 OK creates the session
	reg.Observe(Observation{
		Route:  "sessions.create",
		RawID:  "session_valid_200",
		Status: 200,
	})
	if len(reg.List()) != 1 {
		t.Fatalf("200 must create a session, got: %v", reg.List())
	}

	// Once the session is known, a subsequent non-2xx updates activity without creating a duplicate
	reg.Observe(Observation{
		Route:  "code.session.events.post",
		RawID:  "session_valid_200",
		Status: 500,
	})
	if len(reg.List()) != 1 {
		t.Fatalf("subsequent error must not duplicate session, got: %v", reg.List())
	}
}
```

2. In `internal/mitm/server_test.go`, add `TestObserverDoesNotRegisterSessionOn404`:

```go
func TestObserverDoesNotRegisterSessionOn404(t *testing.T) {
	up, roots := startFakeUpstream(t, func(head string, _ []byte, conn net.Conn) {
		io.WriteString(conn, "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: 26\r\n\r\n{\"error\":\"not_found\"}")
	})
	defer up.Close()

	ca := newTestCA(t)
	reg := NewRegistry(10, time.Hour, nil)
	srv, addr := startTestServer(t, ServerOptions{
		Listen:        "127.0.0.1:0",
		CA:            ca,
		Registry:      reg,
		ClientRoots:   roots,
		DisableDirect: true,
	})
	defer srv.Shutdown(context.Background())

	client := proxyClient(t, addr, ca)
	resp, err := client.Get("https://api.anthropic.com/v1/sessions/session_phantom404")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	if sessions := reg.List(); len(sessions) != 0 {
		t.Fatalf("registry should have 0 sessions, got %d: %+v", len(sessions), sessions)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v ./internal/mitm -run "TestRegistry_DoesNotCreatePhantomSessionOnNon2xx|TestObserverDoesNotRegisterSessionOn404"`
Expected: FAIL (`404 must not create a session, got: [...]`).

- [ ] **Step 3: Implement minimal fix**

In `internal/mitm/registry.go`:
```go
func isSuccessfulObservationStatus(status int) bool {
	return status == 0 || (status >= 200 && status < 300) || status == 101
}
```
In `Registry.Observe`:
```go
	s, ok := r.byID[id]
	if !ok {
		if !isSuccessfulObservationStatus(o.Status) {
			return
		}
		s = &Session{ID: id, CreatedAt: now, LastSeenAt: now}
		r.byID[id] = s
		r.evictLocked()
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/mitm -run "TestRegistry_DoesNotCreatePhantomSessionOnNon2xx|TestObserverDoesNotRegisterSessionOn404"`
Expected: PASS
Run full mitm suite: `go test -race ./internal/mitm`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/registry.go internal/mitm/observe_test.go internal/mitm/server_test.go
git commit -m "fix(mitm): do not create phantom sessions on non-2xx responses"
```

---

### Task 5: Isolate 12 Unrelated Plan Docs to Dedicated Branch and Remove from PR #104

**Files:**
- Move to new branch `docs/pr94-pr102-remediation-plans`:
  1. `docs/superpowers/plans/2026-09-24-pr94-review-remediation-round2.md`
  2. `docs/superpowers/plans/2026-09-24-pr96-review-remediation.md`
  3. `docs/superpowers/plans/2026-09-24-pr97-two-axis-remediation.md`
  4. `docs/superpowers/plans/2026-09-24-pr98-review-remediation.md`
  5. `docs/superpowers/plans/2026-09-25-kimi-code-login-accounts-page.md`
  6. `docs/superpowers/plans/2026-09-25-kimi-poll-race-followups.md`
  7. `docs/superpowers/plans/2026-09-25-pr100-kimi-modal-remediation.md`
  8. `docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation-final.md`
  9. `docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation-round2.md`
  10. `docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation.md`
  11. `docs/superpowers/plans/2026-09-25-pr98-review-remediation-refined.md`
  12. `docs/superpowers/plans/2026-09-25-pr99-review-remediation.md`
- Remove from current branch `feat/claude-code-cloud`.

- [ ] **Step 1: Create dedicated branch preserving the 12 plan docs**

```bash
git branch docs/pr94-pr102-remediation-plans HEAD
```
This ensures all 12 plan documents remain tracked and can be pushed or opened in an independent documentation PR.

- [ ] **Step 2: Remove the 12 files from `feat/claude-code-cloud`**

```bash
git rm \
  docs/superpowers/plans/2026-09-24-pr94-review-remediation-round2.md \
  docs/superpowers/plans/2026-09-24-pr96-review-remediation.md \
  docs/superpowers/plans/2026-09-24-pr97-two-axis-remediation.md \
  docs/superpowers/plans/2026-09-24-pr98-review-remediation.md \
  docs/superpowers/plans/2026-09-25-kimi-code-login-accounts-page.md \
  docs/superpowers/plans/2026-09-25-kimi-poll-race-followups.md \
  docs/superpowers/plans/2026-09-25-pr100-kimi-modal-remediation.md \
  docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation-final.md \
  docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation-round2.md \
  docs/superpowers/plans/2026-09-25-pr102-kimi-poll-race-remediation.md \
  docs/superpowers/plans/2026-09-25-pr98-review-remediation-refined.md \
  docs/superpowers/plans/2026-09-25-pr99-review-remediation.md
```

- [ ] **Step 3: Verify git status**

Run: `git status`
Expected: 12 deletions staged, no other staged changes.

- [ ] **Step 4: Commit removal**

```bash
git commit -m "docs: remove 12 unrelated PR #94-#102 plan docs from branch"
```

---

### Task 6: Document Provenance, Proxy Commit, and Capture SHA-256 Hashes in Fingerprint Record

**Files:**
- Modify: `.reference/mitm-upstream-fingerprint-20260928.txt`

**Context & Truth:**
The captures were taken on 2026-09-28 at 21:50-21:51 against proxy commit `5e446bc`.
The local capture files in `/tmp` have SHA-256:
- `/tmp/mitm-upstream-cli.pcap`: `f34b86b97194ebcee5cf73e3dd65cb518e30892836aa72e8977cd7171d4ebca5` (815,376 bytes)
- `/tmp/mitm-upstream-proxy.pcap`: `740be8369787e14ced7f61084d18452412254af26e2473b7e8e049f5719f8bf8` (3,152,464 bytes)
They are ephemeral local files, not committed into the git tree due to file size (~4 MB total) and because the observe-only forward proxy reaches `api.anthropic.com`, not Cloud Code / `daily-cloudcode-pa.googleapis.com` (which is the sole scope of the AGENTS.md JA4 gate).

- [ ] **Step 1: Update `.reference/mitm-upstream-fingerprint-20260928.txt`**

Update lines 4-18 of `.reference/mitm-upstream-fingerprint-20260928.txt` to include the commit hash, capture SHA-256 sums, and provenance note:

```text
Captured: 2026-09-28 21:49 and 21:53 America/Sao_Paulo (darwin/arm64)
Proxy commit: 5e446bc (feat/claude-code-cloud: fix(mitm): decode brotli response bodies in the observer)
Proxy build: ./bin/proxy (observe-only forward proxy)
  mitm listen 127.0.0.1:8092, WebUI 127.0.0.1:18091, scratch config dir
CLI: /Users/gus/.local/bin/claude 2.1.280 (Claude Code), CLAUDE_CONFIG_DIR=/Users/gus/.claude-container
Session: claude --cloud "Reply with exactly CLOUD_PROBE_OK. Do not modify any files."

Capture commands (both: sudo tcpdump -i pktap,all -P 'host api.anthropic.com and tcp port 443'):

  /tmp/mitm-upstream-proxy.pcap  — while the CLI ran with
      HTTPS_PROXY=http://127.0.0.1:8092 NODE_EXTRA_CA_CERTS=/tmp/mitm-ca.pem
      (records the PROXY's upstream connection to api.anthropic.com:443)
      Capture SHA-256: 740be8369787e14ced7f61084d18452412254af26e2473b7e8e049f5719f8bf8
  /tmp/mitm-upstream-cli.pcap    — while the same CLI ran with no proxy env
      (records the CLI's own connection to api.anthropic.com:443)
      Capture SHA-256: f34b86b97194ebcee5cf73e3dd65cb518e30892836aa72e8977cd7171d4ebca5

Note on capture retention:
  Raw pcaps are held locally in /tmp and not committed to git due to size (~4 MB)
  and because this forward proxy connects to api.anthropic.com (the AGENTS.md JA4
  gate strictly governs the official agy Cloud Code proxy to googleapis.com).
```

- [ ] **Step 2: Verify checksums match**

Run: `shasum -a 256 /tmp/mitm-upstream-*.pcap`
Verify the output matches the hashes recorded in the document:
`f34b86b97194ebcee5cf73e3dd65cb518e30892836aa72e8977cd7171d4ebca5  /tmp/mitm-upstream-cli.pcap`
`740be8369787e14ced7f61084d18452412254af26e2473b7e8e049f5719f8bf8  /tmp/mitm-upstream-proxy.pcap`

- [ ] **Step 3: Commit**

```bash
git add .reference/mitm-upstream-fingerprint-20260928.txt
git commit -m "docs: add commit hash, capture SHA-256 sums and retention note to fingerprint record"
```

---

### Task 7: Full Suite Verification and Build Check

**Files:**
- Test all modified packages

- [ ] **Step 1: Run full test suite across touched packages**

```bash
go test -race -v ./internal/config ./internal/api ./internal/mitm
```
Expected: All tests PASS.

- [ ] **Step 2: Run repository build and lint checks**

```bash
go build -o bin/proxy ./cmd/proxy
go vet ./...
gofmt -l internal/ cmd/
```
Expected:
- Build succeeds.
- Vet produces zero errors.
- `gofmt -l` produces no output (all files clean).

---
