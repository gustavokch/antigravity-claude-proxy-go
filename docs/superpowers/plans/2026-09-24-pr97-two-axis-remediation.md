# PR #97 Two-Axis Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve every finding in the PR #97 two-axis review (9 Standards, 2 Spec). The code must keep its current behaviour, except for one documented `Retry-After` alignment in Task 2.

**Architecture:** Most fixes are local refactors in the packages the PR already touches. Two helpers are added to remove duplication: one 429-cooldown helper in `internal/accounts` and one family of tool-use predicates in `internal/api`. A dead code path is deleted, tests that only pin constants move to the contract they protect, the out-of-scope edits move to their own PR, and the docs and PR body are brought up to date.

**Tech Stack:** Go 1.27rc2, standard library only, `gh` CLI.

**Spec:** the review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/97#issuecomment-5825508726, plus the PR #97 body and its earlier review comments 1–4, which the review checked against.

**Supersedes:** nothing. `2026-09-24-pr97-final-pass-remediation.md` is complete on the branch (`d667087`, `c234df9`, `961d399`, `c441cba`).

## Global Constraints

- AGENTS.md: **do NOT touch TLS internals.** No task touches `internal/cloudcode` transport or `tls.Config`.
- AGENTS.md: generation host stays `DailyEndpoint` only (`internal/cloudcode/client.go:44`); nothing here changes the endpoint lists.
- Tests must catch consumer-visible bugs. Do not add tests that pin wording, wiring, forwarding or incidental defaults, and delete any such tests you touch.
- `gofmt -l .` must print nothing before every commit. The opt-in hook enforces this.
- Branch: `fix/upstream-429-thought-signature`, remote `fork`. Only Task 5 uses another branch. Never force-push: part of the history is already on the PR.
- Run commands from the repo root. If the default Go build cache is not writable, prefix `go` with `GOCACHE=$PWD/.gocache`.

## Finding → Task map

| # | Axis | Finding | Task |
|---|------|---------|------|
| S1 | Standards (hard) | `AGENTS.md:50` capture filter still aims at prod | 6 |
| S2 | Standards | the `retrieveToolName` check is copied four times; `HasVisibleToolUse` and `hasNonRetrieveToolUse` name the same idea two ways | 3 |
| S3 | Standards | the hydration rule is repeated at 6 sites | 3 |
| S4 | Standards | `retryAfterSeconds` recomputes the dispatcher's cooldown; `wait > 0` is always true | 2 |
| S5 | Standards | the `errors.As` fallback in `FindHTTPError` can never run | 1 |
| S6 | Standards | `got != 30` pins `rateLimitTiers[0]` | 2 |
| S7 | Standards | `TestEndpointDefaults` restates constants | 4 |
| S8 | Standards | the `ccr_leak_test` validator only runs after failure; magic `messages.66`; `t.Fatalf` in a handler | 4 |
| S9 | Standards | unrelated discovery-test fix; `accCopy` is not needed | 5 |
| P1 | Spec (a) | the CCR fix is missing from the PR body Summary and Verification | 7 |
| P2 | Spec (b) | `.gitignore /.omp/` and `discovery_test.go` are out of scope | 5 |

---

### Task 1: Delete the unreachable `errors.As` fallback in `FindHTTPError` (S5)

**Files:**
- Modify: `internal/cloudcode/client.go:132-144`
- Test: `internal/cloudcode/client_test.go` (existing `TestFindHTTPError`, no change)

**Interfaces:**
- Consumes: nothing.
- Produces: `func FindHTTPError(err error) *HTTPError` keeps the same signature and behaviour.

Why the fallback is dead: `collectHTTPErrors` (L108-126) walks `Unwrap() error` and `Unwrap() []error`, which is exactly the tree `errors.As` walks. The only extra thing `errors.As` does is call an `As(any) bool` method, and no type in the repo defines one (`grep -rn ') As(' internal cmd` finds nothing). So when `collectHTTPErrors` finds nothing, `errors.As` also finds nothing. Deleting the branch is safe because `TestFindHTTPError` already covers the nil, typed-nil, plain-error, wrapped and joined cases.

- [ ] **Step 1: Confirm nothing defines `As`**

Run: `grep -rn ') As(' internal cmd`
Expected: no output.

- [ ] **Step 2: Replace L136-144**

Replace:

```go
	var httpErrors []*HTTPError
	collectHTTPErrors(err, &httpErrors)
	if len(httpErrors) == 0 {
		var upstreamError *HTTPError
		if errors.As(err, &upstreamError) && upstreamError != nil {
			return upstreamError
		}
		return nil
	}
```

with:

```go
	var httpErrors []*HTTPError
	collectHTTPErrors(err, &httpErrors)
	if len(httpErrors) == 0 {
		return nil
	}
```

Keep the `errors` import: `errors.Join` still uses it at L302 and L347.

- [ ] **Step 3: Run the package tests**

Run: `go test ./internal/cloudcode/ -run 'TestFindHTTPError' -v`
Expected: PASS for every subtest.

- [ ] **Step 4: Commit**

```bash
gofmt -l internal/cloudcode
git add internal/cloudcode/client.go
git commit -m "refactor(cloudcode): drop unreachable errors.As fallback in FindHTTPError"
```

---

### Task 2: One 429 cooldown computation for the dispatcher and `Retry-After` (S4, S6)

**Files:**
- Modify: `internal/accounts/retry.go` (add `Cooldown`, `UpstreamCooldown` after `SmartBackoff`, L203-220; add import `antigravity-go-proxy/internal/cloudcode`)
- Modify: `internal/accounts/dispatcher.go:469-472` and `:762`
- Modify: `internal/api/server.go:3497-3505`
- Test: `internal/api/server_test.go:1859-1895` (replace two tests with one contract test; add import `strconv`)

**Interfaces:**
- Consumes: `cloudcode.HTTPError{StatusCode, Header, Body}`, `ClassifyError`, `ParseResetTime`, `SmartBackoff`, `Decorrelate` (all already in `internal/accounts`).
- Produces:

```go
type Cooldown struct {
	Reason ErrorReason
	Reset  time.Duration
	Wait   time.Duration
}
func UpstreamCooldown(upstreamError *cloudcode.HTTPError, failures int, now time.Time) Cooldown
```

Behaviour note, to write in the commit body: `Retry-After` used to be `ceil(reset)` whenever upstream supplied a reset. It becomes `ceil(max(reset, 2s))`, the same floor `SmartBackoff` applies to the account lock. This only differs for sub-2s resets. Because the header can no longer be shorter than the lock, Claude Code never retries into a locked account.

- [ ] **Step 1: Write the contract test (replaces `TestRetryAfterSecondsFallsBackToSmartBackoffWhenNoHeader` and `TestClassifyAndRetryAfterWithJoined429And400`)**

Delete `internal/api/server_test.go:1859-1895` (both functions) and add:

```go
// A bare RESOURCE_EXHAUSTED 429 carries no reset hint. Claude Code must still
// receive a Retry-After no shorter than the cooldown the dispatcher puts on the
// account, or it retries into a pool that is still locked. A 400 joined in
// from another endpoint must not hide the 429.
func TestWriteErrorRetryAfterCoversDispatcherCooldown(t *testing.T) {
	bare429 := &cloudcode.HTTPError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429",
		Body:       `{ "error": { "code": 429, "message": "Resource has been exhausted (e.g. check quota).", "status": "RESOURCE_EXHAUSTED" } }`,
	}
	corrupted400 := &cloudcode.HTTPError{
		StatusCode: http.StatusBadRequest,
		Status:     "400",
		Body:       `{ "error": { "code": 400, "message": "Corrupted thought signature.", "status": "INVALID_ARGUMENT" } }`,
	}
	floor := accounts.UpstreamCooldown(bare429, 0, time.Now()).Wait
	cases := map[string]error{
		"bare 429":             bare429,
		"429 joined after 400": fmt.Errorf("max retries exceeded: %w", errors.Join(corrupted400, bare429)),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			rec := httptest.NewRecorder()
			server.writeError(rec, err)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
				t.Fatalf("body = %s, want rate_limit_error", rec.Body.String())
			}
			seconds, convErr := strconv.Atoi(rec.Header().Get("Retry-After"))
			if convErr != nil {
				t.Fatalf("Retry-After = %q, want integer seconds", rec.Header().Get("Retry-After"))
			}
			if got := time.Duration(seconds) * time.Second; got < floor {
				t.Fatalf("Retry-After = %s, shorter than dispatcher cooldown %s", got, floor)
			}
		})
	}
}
```

Add `"strconv"` to the `server_test.go` import block, between `"net/http/httptest"` and `"strings"`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/api/ -run TestWriteErrorRetryAfterCoversDispatcherCooldown`
Expected: FAIL to compile with `undefined: accounts.UpstreamCooldown`.

- [ ] **Step 3: Add `Cooldown` / `UpstreamCooldown` to `internal/accounts/retry.go`, directly after `SmartBackoff`**

```go
// Cooldown is how long an account must rest after an upstream 429.
type Cooldown struct {
	Reason ErrorReason   // classification of the 429 body/status
	Reset  time.Duration // server-supplied reset; 0 when upstream sent none
	Wait   time.Duration // SmartBackoff(Reason, Reset, failures), before jitter
}

// UpstreamCooldown classifies a 429 once for every consumer. The dispatcher
// decorrelates Wait before locking the account; the API layer sends the
// failures=0 Wait as Retry-After so the client never retries before the
// account's first lock expires.
func UpstreamCooldown(upstreamError *cloudcode.HTTPError, failures int, now time.Time) Cooldown {
	reason := ClassifyError(upstreamError.Body, upstreamError.StatusCode)
	reset := ParseResetTime(upstreamError.Header, upstreamError.Body, now)
	return Cooldown{Reason: reason, Reset: reset, Wait: SmartBackoff(reason, reset, failures)}
}
```

Add `"antigravity-go-proxy/internal/cloudcode"` to the `retry.go` imports. There is no import cycle: `internal/cloudcode` imports nothing from `internal/accounts`, and `dispatcher.go` already imports `cloudcode`.

- [ ] **Step 4: Use it in the dispatcher**

`internal/accounts/dispatcher.go:469-472`, replace:

```go
				reason := ClassifyError(upstreamError.Body, upstreamError.StatusCode)
				reset := ParseResetTime(upstreamError.Header, upstreamError.Body, dispatcher.now())
				failures := dispatcher.manager.FailureCount(account)
				wait := Decorrelate(SmartBackoff(reason, reset, failures), dispatcher.random)
```

with:

```go
				failures := dispatcher.manager.FailureCount(account)
				cooldown := UpstreamCooldown(upstreamError, failures, dispatcher.now())
				reason, reset := cooldown.Reason, cooldown.Reset
				wait := Decorrelate(cooldown.Wait, dispatcher.random)
```

L473-493 keep using `reason`, `reset`, `wait` and `failures` unchanged.

`internal/accounts/dispatcher.go:762`, replace:

```go
		wait := Decorrelate(SmartBackoff(ClassifyError(body, upstreamError.StatusCode), ParseResetTime(upstreamError.Header, body, dispatcher.now()), dispatcher.manager.FailureCount(account)), dispatcher.random)
```

with:

```go
		wait := Decorrelate(UpstreamCooldown(upstreamError, dispatcher.manager.FailureCount(account), dispatcher.now()).Wait, dispatcher.random)
```

- [ ] **Step 5: Use it in `retryAfterSeconds`**

`internal/api/server.go:3497-3505`, replace:

```go
	if upstreamError := cloudcode.FindHTTPError(err); upstreamError != nil && upstreamError.StatusCode == http.StatusTooManyRequests {
		if wait := accounts.ParseResetTime(upstreamError.Header, upstreamError.Body, time.Now()); wait > 0 {
			return ceilSeconds(wait)
		}
		reason := accounts.ClassifyError(upstreamError.Body, upstreamError.StatusCode)
		if wait := accounts.SmartBackoff(reason, 0, 0); wait > 0 {
			return ceilSeconds(wait)
		}
	}
	return 0
```

with:

```go
	if upstreamError := cloudcode.FindHTTPError(err); upstreamError != nil && upstreamError.StatusCode == http.StatusTooManyRequests {
		return ceilSeconds(accounts.UpstreamCooldown(upstreamError, 0, time.Now()).Wait)
	}
	return 0
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/accounts/ ./internal/api/ -run 'TestWriteErrorRetryAfterCoversDispatcherCooldown|TestRetryAfterSeconds|TestSmartBackoff|429|RateLimit' -v`
Expected: PASS. `TestRetryAfterSecondsReadsUpstreamHeader` still returns 17, because 17s is above the 2s floor.

Run: `go test ./internal/accounts/ ./internal/api/`
Expected: `ok` for both.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/accounts internal/api
git add internal/accounts/retry.go internal/accounts/dispatcher.go internal/api/server.go internal/api/server_test.go
git commit -m "refactor(accounts,api): share one 429 cooldown between dispatcher and Retry-After" \
  -m "Retry-After now uses the same max(reset, 2s) floor as the account lock, so the client never retries before the lock expires. The test asserts that contract through writeError instead of pinning rateLimitTiers[0]."
```

---

### Task 3: One tool-use predicate family and one hydration rule for CCR (S2, S3)

**Files:**
- Modify: `internal/api/ccr_stream_state.go` (L57-65 `StartBlock`, L146-150 `Finalize`, L187-211 `stripRetrieveBlocks`, L249-266 `stripRetrieveBlocksJSON`, L275-298 `hasNonRetrieveToolUse`)
- Modify: `internal/api/server.go` (delete `findRetrieveToolUsesFromResponse`, L3024-3040; guard sites L2370, L2471, L3087, L3301)
- Modify: `internal/api/ccr_proxy.go` (guard sites L255, L417)
- Test: existing `internal/api/ccr_leak_test.go`, `ccr_stream_state_test.go`, `ccr_proxy_test.go`, `headroom_ccr_test.go` (no new tests; this is a behaviour-preserving refactor)

**Interfaces:**
- Consumes: const `retrieveToolName` (ccr_stream_state.go:10); `(*ccrStreamState).HasVisibleToolUse() bool` (unchanged).
- Produces (all in `ccr_stream_state.go`, package `api`):

```go
func isToolUse(block map[string]any) bool
func isRetrieveToolUse(block map[string]any) bool
func findRetrieveToolUsesFromResponse(resp map[string]any) []map[string]any // moved from server.go
func hasVisibleToolUse(resp map[string]any) bool                          // renamed from hasNonRetrieveToolUse
func hydratable(retrieveCalls []map[string]any, visibleToolUse bool) bool
```

- [ ] **Step 1: Record the green baseline**

Run: `go test ./internal/api/ -run 'CCR|Retrieve|Headroom|StreamState|Strip' -count=1`
Expected: `ok`.

- [ ] **Step 2: Add the predicates and the hydration rule at the end of `ccr_stream_state.go`**

```go
// isToolUse reports whether block is an Anthropic tool_use content block.
func isToolUse(block map[string]any) bool {
	bType, _ := block["type"].(string)
	return bType == "tool_use"
}

// isRetrieveToolUse reports whether block is a headroom_retrieve call: one the
// proxy answers itself and never shows the client.
func isRetrieveToolUse(block map[string]any) bool {
	name, _ := block["name"].(string)
	return isToolUse(block) && name == retrieveToolName
}

// findRetrieveToolUsesFromResponse returns the headroom_retrieve calls of a
// decoded Anthropic response, in content order.
func findRetrieveToolUsesFromResponse(resp map[string]any) []map[string]any {
	content, _ := resp["content"].([]any)
	var calls []map[string]any
	for _, raw := range content {
		if block, ok := raw.(map[string]any); ok && isRetrieveToolUse(block) {
			calls = append(calls, block)
		}
	}
	return calls
}

// hasVisibleToolUse is (*ccrStreamState).HasVisibleToolUse for a decoded unary
// response: true when any tool_use other than headroom_retrieve will reach the
// client.
func hasVisibleToolUse(resp map[string]any) bool {
	content, _ := resp["content"].([]any)
	for _, raw := range content {
		if block, ok := raw.(map[string]any); ok && isToolUse(block) && !isRetrieveToolUse(block) {
			return true
		}
	}
	return false
}

// hydratable reports whether the proxy alone can answer a turn's retrieve
// calls. A turn that also carries a client-visible tool_use cannot be
// hydrated: the replayed assistant message would hold a tool_use with no
// tool_result, which upstream rejects with 400.
func hydratable(retrieveCalls []map[string]any, visibleToolUse bool) bool {
	return len(retrieveCalls) > 0 && !visibleToolUse
}
```

Delete the old `hasNonRetrieveToolUse` (L275-298). Delete `findRetrieveToolUsesFromResponse` from `server.go` (L3024-3040). A nil `resp` map is safe here, because indexing a nil map returns the zero value.

- [ ] **Step 3: Switch the in-file duplicates to the predicates**

`StartBlock` (L57-65), replace:

```go
	bType, _ := block["type"].(string)
	if bType == "tool_use" {
		s.jsonBufs[upstreamIdx] = &bytes.Buffer{}
		if name, _ := block["name"].(string); name == retrieveToolName {
```

with:

```go
	if isToolUse(block) {
		s.jsonBufs[upstreamIdx] = &bytes.Buffer{}
		if isRetrieveToolUse(block) {
```

`Finalize` (L146-150), replace:

```go
		if bType, _ := block["type"].(string); bType == "tool_use" {
			if bName, _ := block["name"].(string); bName == retrieveToolName {
				retrieveCalls = append(retrieveCalls, block)
			}
		}
```

with:

```go
		if isRetrieveToolUse(block) {
			retrieveCalls = append(retrieveCalls, block)
		}
```

`stripRetrieveBlocks` (L188-210), replace the filter loop body and the `hasToolUse` loop:

```go
	for _, item := range content {
		if m, ok := item.(map[string]any); ok && isRetrieveToolUse(m) {
			continue
		}
		filtered = append(filtered, item)
	}
	resp["content"] = filtered

	hasToolUse := false
	for _, item := range filtered {
		if m, ok := item.(map[string]any); ok && isToolUse(m) {
			hasToolUse = true
			break
		}
	}
	reconcileStopReason(resp, hasToolUse)
```

`stripRetrieveBlocksJSON` (L249-266), replace everything from `content, ok := resp["content"].([]any)` through `if !hasRetrieve { return body }` with:

```go
	if len(findRetrieveToolUsesFromResponse(resp)) == 0 {
		return body
	}
```

- [ ] **Step 4: Route all six guard sites through `hydratable` / `hasVisibleToolUse`**

`internal/api/ccr_proxy.go:255`:

```go
		needsHydration := hydratable(retrieveCalls, state.HasVisibleToolUse()) && iter < maxHydrations && ccrEnabled
```

`internal/api/ccr_proxy.go:417`:

```go
		needsHydration := hydratable(retrieveCalls, hasVisibleToolUse(respMap)) && iter < maxHydrations && ccrEnabled
```

`internal/api/server.go:2370`:

```go
				if hydratable(retrieveCalls, state.HasVisibleToolUse()) && ccrHydrations < maxCCRHydrations {
```

`internal/api/server.go:2471`:

```go
					if hydratable(retrieveCalls, hasVisibleToolUse(respObj)) {
```

`internal/api/server.go:3087`:

```go
		if !hydratable(retrieveCalls, hasVisibleToolUse(response)) || iter == maxCCRHydrations || !server.isCCREnabled() {
```

`internal/api/server.go:3301`:

```go
		needsHydration := hydratable(retrieveCalls, state.HasVisibleToolUse()) && iter < maxCCRHydrations && server.isCCREnabled()
```

- [ ] **Step 5: Confirm no stragglers**

Run: `grep -rn 'hasNonRetrieveToolUse\|"headroom_retrieve"\|bName == retrieveToolName\|name == retrieveToolName' internal/api --include='*.go' | grep -v _test.go`
Expected: only `ccr_stream_state.go:10` (the `retrieveToolName` const) and the `name == retrieveToolName` line inside `isRetrieveToolUse`.

- [ ] **Step 6: Run the tests**

Run: `go vet ./internal/api/ && go test ./internal/api/ -count=1`
Expected: `ok`, with the same set of passing tests as Step 1.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/api
git add internal/api/ccr_stream_state.go internal/api/ccr_proxy.go internal/api/server.go
git commit -m "refactor(ccr): one retrieve-tool predicate and one hydration rule for all paths"
```

---

### Task 4: Test cleanup: endpoint constants and the CCR mock validator (S7, S8)

**Files:**
- Modify: `internal/cloudcode/client_test.go:208-222`
- Modify: `internal/api/ccr_leak_test.go:420-454` (stream handler) and `:511-515` (unary handler)

**Interfaces:**
- Consumes: `GenerationEndpoints`, `DailyEndpoint` (client.go:44).
- Produces: nothing.

- [ ] **Step 1: Keep only the host-parity assertion in `TestEndpointDefaults`**

Replace `internal/cloudcode/client_test.go:208-222` with:

```go
// Generation is pinned to agy's host with no cross-host fallback: agy 1.2.10
// sends streamGenerateContent to daily (SNI parity), and a thought signature
// issued by one host is rejected by the other. The no-fallback behaviour itself
// is covered by TestGenerationTargetsOnlyGenerationEndpoints.
func TestGenerationEndpointsMatchAgyHost(t *testing.T) {
	t.Parallel()
	if want := []string{DailyEndpoint}; !reflect.DeepEqual(GenerationEndpoints, want) {
		t.Errorf("GenerationEndpoints = %#v, want %#v", GenerationEndpoints, want)
	}
}
```

- [ ] **Step 2: Replace the stream mock's after-the-fact validator**

In `TestCCR_Stream_VisibleToolUseDoesNotHydrate`, replace L422-454 (from `upstream := http.HandlerFunc(...` through the closing `}` of `if curr > 1 { ... }`) with:

```go
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&callCount, 1); n > 1 {
			t.Errorf("unexpected upstream call %d: a turn with a client-visible tool_use must not be hydrated", n)
			http.Error(w, "unexpected hydration call", http.StatusInternalServerError)
			return
		}
```

The rest of the handler (the SSE writes from L456) and the test body are unchanged. The `callCount == 1` assertion at L490 stays; it is the real check.

- [ ] **Step 3: Stop calling `t.Fatalf` from the unary handler goroutine**

In `TestCCR_Unary_VisibleToolUseDoesNotHydrate`, replace L512-515:

```go
		curr := atomic.AddInt32(&callCount, 1)
		if curr > 1 {
			t.Fatalf("unexpected call %d: hydration must not be attempted when visible tool_use is present", curr)
		}
```

with:

```go
		if n := atomic.AddInt32(&callCount, 1); n > 1 {
			t.Errorf("unexpected upstream call %d: a turn with a client-visible tool_use must not be hydrated", n)
			http.Error(w, "unexpected hydration call", http.StatusInternalServerError)
			return
		}
```

- [ ] **Step 4: Prove the stream test still catches the bug it exists for**

Temporarily change `internal/api/ccr_proxy.go` (the L255 guard, now `hydratable(...)`) to `needsHydration := len(retrieveCalls) > 0 && iter < maxHydrations && ccrEnabled`.
Run: `go test ./internal/api/ -run 'TestCCR_(Stream|Unary)_VisibleToolUseDoesNotHydrate' -count=1`
Expected: `TestCCR_Stream_VisibleToolUseDoesNotHydrate` FAILs with `unexpected upstream call 2` and/or `expected exactly 1 call`.
Revert the temporary change: `git checkout internal/api/ccr_proxy.go`.

- [ ] **Step 5: Run the tests**

Run: `go vet ./internal/api/ ./internal/cloudcode/ && go test ./internal/api/ ./internal/cloudcode/ -count=1`
Expected: `ok` for both.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/api internal/cloudcode
git add internal/cloudcode/client_test.go internal/api/ccr_leak_test.go
git commit -m "test: drop constant-pinning endpoint asserts and dead CCR mock validator"
```

---

### Task 5: Move the out-of-scope `.gitignore` and discovery-test edits into their own PR (S9, P2)

**Files:**
- New branch `chore/omp-ignore-discovery-test` (from `main`), via worktree `../acp-chore`:
  - Modify: `.gitignore` (append `/.omp/` block)
  - Modify: `internal/claudecode/discovery_test.go:146-156`
- PR #97 branch:
  - Modify: `.gitignore`, `internal/claudecode/discovery_test.go` (restore `main`'s versions)
  - Local only: `.git/info/exclude`

**Interfaces:** none.

- [ ] **Step 1: Create the chore worktree from `main`**

```bash
git worktree add ../acp-chore -b chore/omp-ignore-discovery-test main
git -C ../acp-chore checkout fix/upstream-429-thought-signature -- .gitignore internal/claudecode/discovery_test.go
```

- [ ] **Step 2: Remove the redundant loop copy (Go ≥1.22 gives each iteration its own variable; go.mod is `go 1.27rc2`)**

In `../acp-chore/internal/claudecode/discovery_test.go`, replace:

```go
		if a.Token == "sk-ant-oat-token-milli" {
			accCopy := a
			acc = &accCopy
			break
		}
```

with:

```go
		if a.Token == "sk-ant-oat-token-milli" {
			acc = &a
			break
		}
```

- [ ] **Step 3: Record why the test fix exists**

Run on `main`'s version: `git -C ../acp-chore stash && (cd ../acp-chore && go test ./internal/claudecode/ -run TestDiscoverLocalCredentials_MillisecondTimestamp -count=1 -v); git -C ../acp-chore stash pop`
Expected: either FAIL (the first discovered account is not the temp `.claude.json` one; quote the failure line in the PR body) or PASS (then the PR body says the change aligns this test with `TestDiscoverLocalCredentials_OAuthWithRefreshToken`'s lookup-by-token pattern, `discovery_test.go:98-104`, and makes it independent of discovery order).

- [ ] **Step 4: Test, commit, push, open the PR**

```bash
cd ../acp-chore
gofmt -l internal/claudecode
go test ./internal/claudecode/ -count=1
git add .gitignore internal/claudecode/discovery_test.go
git commit -m "chore: ignore machine-local .omp/; find discovery test account by token"
git push fork chore/omp-ignore-discovery-test
gh pr create --base main --head chore/omp-ignore-discovery-test \
  --title "chore: ignore .omp/; make millisecond-timestamp discovery test order-independent" \
  --body "Split out of #97, where the two-axis review flagged these as out of scope. <Step 3 result>"
cd -
```

Expected: `go test` prints `ok`, and `gh pr create` prints the new PR URL. Call it `#N`.

- [ ] **Step 5: Restore `main`'s versions on the PR #97 branch**

```bash
git checkout main -- .gitignore internal/claudecode/discovery_test.go
printf '/.omp/\n' >> .git/info/exclude   # keep local OMP config untracked until #N merges
git add .gitignore internal/claudecode/discovery_test.go
git commit -m "revert: move .omp ignore and discovery test fix to #N"
```

- [ ] **Step 6: Confirm the PR diff no longer touches them**

Run: `git diff --stat main...HEAD -- .gitignore internal/claudecode/`
Expected: no output.

- [ ] **Step 7: Remove the worktree**

Run: `git worktree remove ../acp-chore`

---

### Task 6: Point the AGENTS.md capture filter at the generation host (S1)

**Files:**
- Modify: `AGENTS.md:50`

**Interfaces:** none.

- [ ] **Step 1: Replace L50**

Replace:

```markdown
- Fingerprint test: capture with `tcpdump -i any -w /tmp/go.pcap host cloudcode-pa.googleapis.com -c 30` while hitting the proxy, then run the tshark gate above.
```

with:

```markdown
- Fingerprint test: capture with `tcpdump -i any -w /tmp/go.pcap host daily-cloudcode-pa.googleapis.com -c 30` while hitting the proxy (generation only goes to daily; on macOS use `tcpdump -i pktap,all -P` as in `.reference/fingerprint-recheck-20260924.txt`), then run the tshark gate above.
```

- [ ] **Step 2: Check that the docs agree on the generation host**

Run: `grep -n 'tcpdump' AGENTS.md README.md`
Expected: every generation-gate capture names `daily-cloudcode-pa.googleapis.com`. README L116-117, L767 and L787 already do.

- [ ] **Step 3: Commit**

```bash
git add AGENTS.md
git commit -m "docs(agents): capture the fingerprint gate on the daily generation host"
```

---

### Task 7: PR body, full gate, push, reply (P1)

**Files:**
- PR #97 body (`gh pr edit`)

**Interfaces:** none.

- [ ] **Step 1: Full gate**

```bash
go build -o bin/proxy ./cmd/proxy
go vet ./...
gofmt -l .          # must print nothing
go test ./... -count=1
```

Expected: build succeeds, `vet` is silent, `gofmt` prints nothing, and every package is `ok`. If the only failure is `TestDiscoverLocalCredentials_MillisecondTimestamp` (Task 5 moved its fix to `#N`), merge `#N` first, then run `git fetch fork && git merge fork/main` (no force-push) and rerun this step.

- [ ] **Step 2: Pull the current body**

Run: `gh pr view 97 --json body --jq .body > /tmp/pr97-body.md`

- [ ] **Step 3: Edit `/tmp/pr97-body.md`**

Under "Root Causes & Fixes", after item 3, add:

```markdown
4. **CCR hydration with client-visible tool calls (`internal/api/ccr_proxy.go`, `internal/api/server.go`, `internal/api/ccr_stream_state.go`)**:
   - When one upstream turn held both `headroom_retrieve` and a client-visible tool call (e.g. `Read`), the proxy hydrated the turn: it replayed every assistant `tool_use` but supplied a `tool_result` only for `headroom_retrieve`. Upstream rejected the replay with HTTP 400 `tool_use ids were found without tool_result blocks immediately after`.
   - **Fix**: hydrate only when every tool call in the turn is `headroom_retrieve` (`hydratable`, used on all six stream/unary paths). Otherwise strip `headroom_retrieve`, keep `stop_reason: "tool_use"`, and return the turn so the client runs its tool.
```

In item 3, replace the `Retry-After` fix sentence with:

```markdown
   - **Fix**: `retryAfterSeconds` finds the 429 via `cloudcode.FindHTTPError` and sends `accounts.UpstreamCooldown(err, 0, now).Wait`, the same computation the dispatcher uses to lock the account (server reset floored at 2s, else the reason's first backoff tier).
```

Replace the Verification test list with:

```markdown
- Unit tests:
  - `internal/cloudcode/client_test.go`: `TestFindHTTPError` (table-driven: nil, typed nil, wrapped, joined; precedence 429 > 401/403 > 5xx > first found), `TestGenerationEndpointsMatchAgyHost`, `TestGenerationTargetsOnlyGenerationEndpoints`.
  - `internal/api/server_test.go`: `TestWriteErrorRetryAfterCoversDispatcherCooldown` (bare 429 and 429 joined after a 400).
  - `internal/api/ccr_leak_test.go`: `TestCCR_Stream_VisibleToolUseDoesNotHydrate`, `TestCCR_Unary_VisibleToolUseDoesNotHydrate`.
- `go build ./cmd/proxy`, `go vet ./...`, `gofmt -l .` and `go test ./...` are clean.
- Packet recheck: `.reference/fingerprint-recheck-20260924.txt` (agy 1.2.10 and the proxy both send SNI `daily-cloudcode-pa.googleapis.com` for generation, JA4 `t13d131100_f57a46bbacb6_f50d94e863eb`, no ALPN).
- Split out: `.gitignore /.omp/` and the discovery-test fix → #N.
```

- [ ] **Step 4: Apply and push**

```bash
gh pr edit 97 --body-file /tmp/pr97-body.md
git push fork fix/upstream-429-thought-signature
```

Expected: `gh` prints the PR URL, and the push fast-forwards (no `+` / forced update).

- [ ] **Step 5: Reply on the review comment**

```bash
gh pr comment 97 --body "Two-axis review (#issuecomment-5825508726) addressed: S1 <sha Task 6>, S2/S3 <sha Task 3>, S4/S6 <sha Task 2>, S5 <sha Task 1>, S7/S8 <sha Task 4>, S9/P2 → #N + <sha Task 5 revert>, P1 PR body updated. Full gate green."
```

Fill each `<sha …>` from `git log --oneline main..HEAD`.

- [ ] **Step 6: Clean up**

Run: `rm -f /tmp/pr97-body.md /tmp/pr97-review3.md /tmp/pr97.diff /tmp/pr97-spec.md`
