# Claude Code Gateway Remediation Plan

Goal: Remediate 7 code review findings identified in PR #29 (Claude Code Official API Gateway).
Architecture: Go HTTP proxy with multi-account pool, sticky sessions, rate-limit parsing, pricing matrix, and session tracking.
Tech Stack: Go 1.22+, standard library (`net/http`, `sync`, `time`, `strconv`, `strings`).
Spec Reference: `docs/superpowers/specs/2026-08-27-claude-code-gateway-design.md`

---

## Tasks

### Task 1: Clean Repository Hygiene (Accidental Transcript File)
- **Target files**: Delete `2026-08-26-163702-command-messagesuperpowerswriting-planscomma.txt`
- **Step 1**: Verify file existence and remove via git.
- **Step 2**: Commit fix.
  - `git rm 2026-08-26-163702-command-messagesuperpowerswriting-planscomma.txt`
  - `git commit -m "fix(claudecode): remove accidental transcript dump from repo root"`

---

### Task 2: Deterministic Router Prefix Matching
- **Target files**: `internal/claudecode/router.go`, `internal/claudecode/router_test.go`
- **Step 1**: Add failing test for non-deterministic generic prefix match rejection (`claude`, `c`).
- **Step 2**: Run `go test -v ./internal/claudecode -run TestRouter_GenericPrefixRejection` (Confirm failure).
- **Step 3**: Fix `router.go` to only match prefix if `strings.HasPrefix(req, id)` or `strings.HasPrefix(req, alias)`, removing reverse `strings.HasPrefix(id, req)`.
- **Step 4**: Run `go test -v ./internal/claudecode -run TestRouter` (Confirm pass).
- **Step 5**: Commit fix.
  - `git commit -m "fix(claudecode): prevent non-deterministic prefix matching on generic model names"`

---

### Task 3: Bound Sticky Session Memory in AccountPool
- **Target files**: `internal/claudecode/pool.go`, `internal/claudecode/pool_test.go`
- **Step 1**: Add failing test for sticky session cap and LRU/oldest eviction at `maxStickyEntries`.
- **Step 2**: Run `go test -v ./internal/claudecode -run TestAccountPool_StickySessionCapacity` (Confirm failure).
- **Step 3**: Implement `maxStickyEntries` cap and oldest-entry eviction in `pool.go`.
- **Step 4**: Run `go test -v ./internal/claudecode -run TestAccountPool` (Confirm pass).
- **Step 5**: Commit fix.
  - `git commit -m "fix(claudecode): bound sticky session map capacity with eviction in AccountPool"`

---

### Task 4: Bound SessionTracker Sessions Map
- **Target files**: `internal/claudecode/observability.go`, `internal/claudecode/observability_test.go`
- **Step 1**: Add failing test for `SessionTracker` memory capacity cap and eviction.
- **Step 2**: Run `go test -v ./internal/claudecode -run TestSessionTracker_CapacityEviction` (Confirm failure).
- **Step 3**: Implement `maxSessionEntries` cap and LRU/oldest eviction in `observability.go`.
- **Step 4**: Run `go test -v ./internal/claudecode -run TestSessionTracker` (Confirm pass).
- **Step 5**: Commit fix.
  - `git commit -m "fix(claudecode): bound session tracker capacity to prevent unbounded memory growth"`

---

### Task 5: Model Family Prefix Matching in Pricing Calculation
- **Target files**: `internal/claudecode/pricing.go`, `internal/claudecode/pricing_test.go`
- **Step 1**: Add failing test for non-timestamped model aliases (`claude-3-5-haiku`, `claude-3-opus`, `claude-3-7-sonnet`).
- **Step 2**: Run `go test -v ./internal/claudecode -run TestCalculateCost_ModelFamilyAliases` (Confirm failure).
- **Step 3**: Implement family prefix resolution (`haiku`, `opus`, `sonnet`) before falling back to default in `pricing.go`.
- **Step 4**: Run `go test -v ./internal/claudecode -run TestCalculateCost` (Confirm pass).
- **Step 5**: Commit fix.
  - `git commit -m "fix(claudecode): resolve model family pricing for non-timestamped aliases"`

---

### Task 6: Proxy Status Code Guard for Pool Success/Failure Recording
- **Target files**: `internal/api/claudecode_proxy.go`, `internal/api/claudecode_proxy_test.go`
- **Step 1**: Add failing test verifying that non-2xx status codes (4xx, 5xx) record failures on the account instead of calling `RecordSuccess`.
- **Step 2**: Run `go test -v ./internal/api -run TestClaudeCodeProxy_Non2xxStatusRecording` (Confirm failure).
- **Step 3**: Guard `pool.RecordSuccess` with `resp.StatusCode < 400` check, and call `pool.RecordFailure` for 4xx/5xx in `claudecode_proxy.go`.
- **Step 4**: Run `go test -v ./internal/api -run TestClaudeCode` (Confirm pass).
- **Step 5**: Commit fix.
  - `git commit -m "fix(claudecode): guard pool success recording on upstream response status code"`

---

### Task 7: Support Floating-Point Seconds in Rate Limit Headers
- **Target files**: `internal/claudecode/ratelimit.go`, `internal/claudecode/ratelimit_test.go`
- **Step 1**: Add failing test for floating-point seconds in rate limit reset headers (`"1.5"`, `"0.25"`).
- **Step 2**: Run `go test -v ./internal/claudecode -run TestParseRateLimitHeaders_FloatingPointSeconds` (Confirm failure).
- **Step 3**: Update `parseTimeOrDuration` in `ratelimit.go` to use `strconv.ParseFloat`.
- **Step 4**: Run `go test -v ./internal/claudecode -run TestParseRateLimitHeaders` (Confirm pass).
- **Step 5**: Commit fix.
  - `git commit -m "fix(claudecode): support floating-point seconds in rate limit duration parsing"`
