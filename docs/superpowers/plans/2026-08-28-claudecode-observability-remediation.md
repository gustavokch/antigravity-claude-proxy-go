# Claude Code Observability PR #37 Remediation Plan

- **Goal**: Remediate review findings for PR #37 (`fix/claudecode-observability-usage`): fix integer formatting overflow on `math.MinInt`, invalidate HTTP client on pool base URL change, check top-level `user_id` in session extraction, and add comprehensive unit test coverage.
- **Architecture**: Go HTTP proxy (`antigravity-go-proxy`), Claude Code gateway subpackage (`internal/claudecode`) and HTTP handler (`internal/api`).
- **Tech Stack**: Go 1.22+, `testing`, `log/slog`, `net/http/httptest`.

---

## Task 1: Safe Integer Formatting in `formatInt`

- **Target Files**:
  - Modify: `internal/claudecode/observability.go`
  - Test: `internal/claudecode/observability_test.go`
- **Consumes**: Integer `n`
- **Produces**: String with comma separators without risking overflow on `math.MinInt`

### Step 1: Write failing test
Add `TestFormatInt` in `internal/claudecode/observability_test.go` asserting `0`, `999`, `1000`, `1234567`, negative numbers, and `math.MinInt`.

### Step 2: Run test to confirm failure
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go && go test -v ./internal/claudecode -run TestFormatInt
```

### Step 3: Minimal implementation
Modify `formatInt` in `internal/claudecode/observability.go` to avoid `-n` overflow.

### Step 4: Run test to confirm pass
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go && go test -v ./internal/claudecode -run TestFormatInt
```

### Step 5: Git commit
```bash
git add internal/claudecode/observability.go internal/claudecode/observability_test.go
git commit -m "fix(claudecode): handle negative numbers and MinInt safely in formatInt"
```

---

## Task 2: Invalidate `ccHTTPClient` on `BaseURL` Change

- **Target Files**:
  - Modify: `internal/api/claudecode_proxy.go`
  - Test: `internal/api/claudecode_observability_test.go`
- **Consumes**: `claudecode.Config` with dynamic `BaseURL`
- **Produces**: Recreated `ccHTTPClient` whenever `ccPoolKey != key`

### Step 1: Write failing test
Add `TestGetOrCreateCCPool_BaseURLUpdate` in `internal/api/claudecode_observability_test.go`.

### Step 2: Run test to confirm failure
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go && go test -v ./internal/api -run TestGetOrCreateCCPool_BaseURLUpdate
```

### Step 3: Minimal implementation
In `internal/api/claudecode_proxy.go`, set `ccHTTPClient = claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.BaseURL), nil)` whenever `ccHTTPClient == nil || ccPoolKey != key`.

### Step 4: Run test to confirm pass
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go && go test -v ./internal/api -run TestGetOrCreateCCPool_BaseURLUpdate
```

### Step 5: Git commit
```bash
git add internal/api/claudecode_proxy.go internal/api/claudecode_observability_test.go
git commit -m "fix(claudecode): recreate HTTP client when pool BaseURL changes"
```

---

## Task 3: Support Top-Level `user_id` in `ccExtractSessionID`

- **Target Files**:
  - Modify: `internal/api/claudecode_proxy.go`
  - Test: `internal/api/claudecode_observability_test.go`
- **Consumes**: Request body map with top-level `user_id`
- **Produces**: Extracted session key

### Step 1: Write failing test
Add test case in `internal/api/claudecode_observability_test.go` verifying extraction of top-level `user_id`.

### Step 2: Run test to confirm failure
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go && go test -v ./internal/api -run TestExtractSessionID_TopLevelUserID
```

### Step 3: Minimal implementation
Update `ccExtractSessionID` in `internal/api/claudecode_proxy.go` to check `reqBody["user_id"]`.

### Step 4: Run test to confirm pass
```bash
cd /Users/gus/Git/antigravity-claude-proxy-go && go test -v ./internal/api -run TestExtractSessionID_TopLevelUserID
```

### Step 5: Git commit
```bash
git add internal/api/claudecode_proxy.go internal/api/claudecode_observability_test.go
git commit -m "fix(claudecode): extract top-level user_id as session key fallback"
```
