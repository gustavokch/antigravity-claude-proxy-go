# PR #71 Remediation Plan: Antigravity & Kimi Observability Hardening

**Goal:** Remediate findings from code review on PR #71: add explicit Claude Haiku pricing to prevent Sonnet overbilling, provide deterministic timestamp tracking in `SessionTracker`, handle `Content-Encoding: gzip` and `Content-Length` synchronization in `kimiInstrumentResponse`, and record Kimi traffic in `server.tracker`.

**Architecture:**
- `internal/cloudcode`: Add `claude-haiku-4-5` to `basePricingTable` ($0.80 in, $4.00 out, $0.08 cache read). Prioritize `haiku` substring match in `lookupModelPricing` before generic `claude`. Add `RecordWithTime` to `SessionTracker` and update `ComputeFinalMetrics` to use caller timestamp.
- `internal/api`: In `kimiInstrumentResponse`, decompress gzipped body copies for usage parsing, update `resp.ContentLength` and `Content-Length` header. Wire `server.tracker.TrackRequest` in Kimi unary, streaming, and CCR paths.

**Tech Stack:** Go (1.27rc2), `log/slog`, `net/http/httputil`.

---

### Task 1: Claude Haiku Pricing Entry & Deterministic Timestamp Tracking in SessionTracker

- **Target files:**
  - Modify: `internal/cloudcode/observability.go`
  - Test: `internal/cloudcode/observability_test.go`
- **Interfaces:**
  - `CalculateRetailCost(model string, inputTokens, outputTokens, cacheReadTokens int, now time.Time) float64`
  - `SessionTracker.RecordWithTime(sessionID string, inTokens, outTokens, cacheRead int, cost float64, now time.Time) SessionStats`
  - `SessionTracker.Record(sessionID string, inTokens, outTokens, cacheRead int, cost float64) SessionStats`
  - `RequestMetrics.ComputeFinalMetrics(sessionTracker *SessionTracker, now time.Time)`

- **Step 1: Write failing tests**
  - Add test cases in `internal/cloudcode/observability_test.go` for `claude-3-5-haiku-20241022` and `claude-haiku-4-5` verifying costs match Haiku rates ($0.80/$4.00/$0.08).
  - Add test in `internal/cloudcode/observability_test.go` verifying `RecordWithTime` updates `LastActive` to specified time.

- **Step 2: Run tests to confirm failure**
  - Command: `cd /Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/antigravity-observability && go test -v ./internal/cloudcode -run "TestCalculateRetailCost_Haiku|TestSessionTracker_RecordWithTime"`

- **Step 3: Implementation**
  - In `internal/cloudcode/observability.go`:
    - Add `"claude-haiku-4-5"` pricing to `basePricingTable`.
    - Add `case strings.Contains(norm, "haiku"): return basePricingTable["claude-haiku-4-5"]` before `strings.Contains(norm, "claude")` in `lookupModelPricing`.
    - Add `RecordWithTime(sessionID string, inTokens, outTokens, cacheRead int, cost float64, now time.Time) SessionStats` to `SessionTracker`.
    - Make `Record(...)` call `RecordWithTime(..., time.Now())`.
    - Update `ComputeFinalMetrics` to call `sessionTracker.RecordWithTime(..., now)`.

- **Step 4: Run tests to confirm pass**
  - Command: `cd /Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/antigravity-observability && go test -v ./internal/cloudcode -run "TestCalculateRetailCost_|TestSessionTracker_"`

- **Step 5: Git commit**
  - `git commit -am "fix(cloudcode): add haiku pricing and deterministic session timestamp tracking"`

---

### Task 2: Kimi Content-Length Synchronization & Gzip Decompression for Token Parsing

- **Target files:**
  - Modify: `internal/api/server.go`
  - Test: `internal/api/antigravity_observability_test.go`
- **Interfaces:**
  - `(server *Server) kimiInstrumentResponse(resp *http.Response, model, sessionID string, startTime time.Time)`

- **Step 1: Write failing test**
  - Add `TestKimiObservability_GzipResponse` in `internal/api/antigravity_observability_test.go` verifying that gzipped responses have usage parsed and Content-Length header set.

- **Step 2: Run test to confirm failure**
  - Command: `cd /Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/antigravity-observability && go test -v ./internal/api -run "TestKimiObservability_GzipResponse"`

- **Step 3: Implementation**
  - In `internal/api/server.go`:
    - In `kimiInstrumentResponse`, decompress gzip if `strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip")` before calling `openrouter.ParseUsageFromJSON`.
    - Set `resp.ContentLength = int64(len(respBytes))` and `resp.Header.Set("Content-Length", strconv.Itoa(len(respBytes)))`.

- **Step 4: Run test to confirm pass**
  - Command: `cd /Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/antigravity-observability && go test -v ./internal/api -run "TestKimiObservability_GzipResponse"`

- **Step 5: Git commit**
  - `git commit -am "fix(api): handle gzip decompression and sync content length in kimi response"`

---

### Task 3: Kimi Gateway Request Tracking via server.tracker

- **Target files:**
  - Modify: `internal/api/server.go`
  - Test: `internal/api/antigravity_observability_test.go`
- **Interfaces:**
  - `(server *Server) forwardToKimi(...)`
  - `(server *Server) kimiInstrumentResponse(...)`

- **Step 1: Write failing test / update assertions**
  - In `TestKimiObservability_Unary`, `TestKimiObservability_Streaming`, and `TestKimiObservability_CCRStreaming`, attach a `stats.NewTracker()` to `srv.tracker` and assert that request metrics (model, tokens, count) are recorded.

- **Step 2: Run test to confirm failure**
  - Command: `cd /Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/antigravity-observability && go test -v ./internal/api -run "TestKimiObservability_"`

- **Step 3: Implementation**
  - In `internal/api/server.go`:
    - Call `if server.tracker != nil { server.tracker.TrackRequest(model, latency, in, out, cr) }` in `kimiInstrumentResponse.onComplete` and in `opts.OnUsage` in `forwardToKimi`.

- **Step 4: Run test to confirm pass**
  - Command: `cd /Users/gus/Git/antigravity-claude-proxy-go/.claude/worktrees/antigravity-observability && go test -v ./internal/api -run "TestKimiObservability_"`

- **Step 5: Git commit**
  - `git commit -am "fix(api): record kimi gateway traffic in stats tracker"`
