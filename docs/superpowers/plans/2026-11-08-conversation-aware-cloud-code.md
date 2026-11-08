# Conversation-Aware Cloud Code Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep each Claude Code conversation on one Antigravity account, send the control-plane calls agy sends around a conversation, and answer `POST /v1/messages/count_tokens` instead of 501.

**Architecture:** Session pins live inside `accounts.Manager` under its existing mutex, so the pin check and the usability check are one atomic decision; strategies run only when a pin is created, rebound or overflowed. The Dispatcher sends agy's start burst and per-turn calls before generation, keyed by the same session. A new `internal/tokencount` package learns chars-per-token ratios from real usage and estimates counts for Cloud Code and Zen; other gateways forward count_tokens upstream, Claude Code with the identity captured from the real client.

**Tech Stack:** Go 1.27rc2 standard library only (`net/http`, `encoding/json`, `crypto/sha256`, `image/*`), existing packages `internal/accounts`, `internal/cloudcode`, `internal/api`, `internal/ccidentity`, `internal/claudecode`.

**Spec:** `docs/superpowers/specs/2026-11-07-conversation-aware-cloud-code-design.md` (read it before any task). Wire references: `.reference/client-drift/agy.jsonl`, `.reference/claude-code-count-tokens-20261107.jsonl` and `.meta.txt`.

## Global Constraints

- Cloud Code TLS is untouchable: empty `tls.Config{}`, standard `net/http` transport, HTTP/1.1, no new transport options (AGENTS.md, "The one rule that matters most").
- Every Cloud Code call goes to `cloudcode.DailyEndpoint` (`https://daily-cloudcode-pa.googleapis.com`) only.
- No new Go modules; standard library only.
- Clean cutover: when a signature changes, every caller and test changes in the same task; no aliases or deprecated shims.
- Pins: idle TTL `1 h`, cap `10,000`, wait bound `maxStickyWait = 2 * time.Minute` (existing const, `internal/accounts/manager.go:24`).
- Control-plane calls: `10 s` timeout each, failures logged at debug, never block or fail generation.
- Estimator: start ratio `4.0` chars/token, EMA `α = 0.2`, learn only from requests with `≥ 1,000` measured characters; memory TTL `1 h`, cap `10,000`.
- count_tokens never returns an error status for an upstream failure; unsupported gateways (404/405/501) are marked for `1 h`.
- Claude Code count_tokens identity is the captured one (`x-claude-code-request-class: auxiliary`, `anthropic-beta: claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01`); body forwarded unchanged.
- Per-package test command: `go test ./internal/<pkg> -run '<Name>' -count=1`; before the final commit `go test -race ./...` and `gofmt -l $(git ls-files '*.go' | grep -v '^gen/')` prints nothing.
- Commit after every task; push only to `fork` (`git push fork <branch>`), never `origin`.

---

## File Structure

| Unit | Files | Responsibility |
|---|---|---|
| Session pins | `internal/accounts/conversation.go` (new), `internal/accounts/manager.go` | Pin state, wait-vs-move classification, rebinding and overflow under `manager.mu`; exclusion-aware strategies; session key context helpers |
| Save ordering | `internal/accounts/manager.go` | `saveMu` orders `SaveToDisk` from snapshot to rename |
| Control-plane client | `internal/cloudcode/client.go` | The four new Cloud Code calls and the GET path, agy's header order |
| Control-plane sequencing | `internal/accounts/controlplane.go` (new), `internal/accounts/dispatcher.go`, `internal/accounts/outputlimit.go` | Start burst and per-turn calls before generation; response handling; no post-stream quota call |
| Token estimator | `internal/tokencount/` (new: `counter.go`, `measure.go`, `image.go`, `memory.go`) | Family ratios, character measurement, image pricing, conversation memory |
| count_tokens route | `internal/api/count_tokens.go` (new), `internal/api/dispatch.go`, `internal/api/server.go` | Route, shared gateway matching, estimate path, Observe feeding, forwards and unsupported marks |
| Claude Code count identity | `internal/ccidentity/profile.go`, `apply.go`, `defaults.go`, `internal/claudecode/client.go` | `CountTokensProfile`, `CountTokens` request on the pooled uTLS transport |
| Other count forwards | `internal/kimi/passthrough.go`, `internal/api/server.go`, `internal/api/count_tokens.go` | OpenRouter, Kimi and custom-endpoint counts with their existing identity headers |
| Drift tooling | `tools/clientprobe/main.go`, `scripts/client_drift.py` | Probe the new calls; report a changed agy call sequence; pin the count beta list |
| Docs | `README.md`, `CONTEXT.md`, `docs/adr/0004-claude-code-identity-normalization-exception.md` | Affinity, control plane, count_tokens |

Dependencies: Tasks 1→2→3 (affinity) come first; Task 4 before Task 7 (saves become frequent); Task 5→6→7 (control plane); Tasks 8→9 (estimator) before Task 10; Task 10→11→12 (count route and forwards); Task 13 needs Task 5; Task 14 last.

---

## Part A: Session affinity (spec §3, §4): Tasks 1–3

Line numbers below are from the base commit of branch `docs/conversation-aware-cloud-code-spec`. Each task moves lines for the next one, so find code by function name once an earlier task has landed.

---

### Task 1: Exclusion-aware strategies and the `Select(session, model)` signature change

**Files:**
- Create: `internal/accounts/exclusion_test.go`
- Modify: `internal/accounts/manager.go:372-387` (`Select`; `pickLocked` goes right after it)
- Modify: `internal/accounts/manager.go:845-931` (`selectStickyLocked`, `selectRoundRobinLocked`, `selectHybridLocked`)
- Modify: `internal/accounts/dispatcher.go:338` and `internal/accounts/dispatcher.go:429` (callers)
- Modify (existing tests that call the old signatures): `internal/accounts/manager_test.go:77,97,111,116,119,137,142,475,483,491,584,604,652,665,677,684,783,1084,1091,1098,1146,1177,1198`, `internal/accounts/select_bench_test.go:29`, `internal/api/management_test.go:1528,1739,1900`
- Test: `internal/accounts/exclusion_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `func (manager *Manager) Select(session, model string) Selection`. `session` is not used yet; Task 2 uses it. There is no alias for `Select(model string)`.
  - `func (manager *Manager) pickLocked(model string, exclude map[string]bool) Selection`. It calls the configured strategy. The caller holds `manager.mu`.
  - `func (manager *Manager) selectStickyLocked(model string, exclude map[string]bool) Selection`
  - `func (manager *Manager) selectRoundRobinLocked(model string, exclude map[string]bool) Selection`
  - `func (manager *Manager) selectHybridLocked(model string, exclude map[string]bool) Selection`
  - Exclusion rule: an excluded account counts as unusable, so it is never picked. Sticky also never returns a `Wait` for an excluded current account. A nil or empty set gives today's results.

- [ ] **Step 1: Write the failing test**

Create `internal/accounts/exclusion_test.go`:

```go
package accounts

import (
	"testing"
	"time"
)

// pickWith runs one strategy pick on a fresh three-account pool, so every
// call starts from the same rotation state and the picks are comparable.
func pickWith(t *testing.T, strategy string, exclude map[string]bool) Selection {
	t.Helper()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, err := New(Options{
		Accounts: []*Account{testAccount("a@example.com"), testAccount("b@example.com"), testAccount("c@example.com")},
		Strategy: strategy,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.pickLocked("claude-sonnet-4-6", exclude)
}

func TestStrategiesSkipExcludedAccounts(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{StrategySticky, StrategyRoundRobin, StrategyHybrid} {
		t.Run(strategy, func(t *testing.T) {
			t.Parallel()
			first := pickWith(t, strategy, nil).Account
			if first == nil {
				t.Fatal("no account picked from a healthy pool")
			}
			if got := pickWith(t, strategy, map[string]bool{}).Account; got == nil || got.Email != first.Email {
				t.Fatalf("empty exclusion set picked %v, want %s", got, first.Email)
			}
			second := pickWith(t, strategy, map[string]bool{first.Email: true}).Account
			if second == nil || second.Email == first.Email {
				t.Fatalf("excluding %s picked %v, want another account", first.Email, second)
			}
			onlyC := map[string]bool{"a@example.com": true, "b@example.com": true}
			if got := pickWith(t, strategy, onlyC).Account; got == nil || got.Email != "c@example.com" {
				t.Fatalf("with a and b excluded picked %v, want c@example.com", got)
			}
			all := map[string]bool{"a@example.com": true, "b@example.com": true, "c@example.com": true}
			if got := pickWith(t, strategy, all); got != (Selection{}) {
				t.Fatalf("with every account excluded got %+v, want no account and no wait", got)
			}
		})
	}
}

// Sticky waits out a short limit on its current account. An excluded
// current account must not be waited for: the caller asked for anyone else.
func TestStickyDoesNotWaitForAnExcludedCurrentAccount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	account := testAccount("sticky@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	manager.MarkRateLimited(account, "claude", 30*time.Second)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if got := manager.pickLocked("claude", nil); got != (Selection{Wait: 30 * time.Second}) {
		t.Fatalf("plain sticky pick = %+v, want a 30s wait on the current account", got)
	}
	if got := manager.pickLocked("claude", map[string]bool{account.Email: true}); got != (Selection{}) {
		t.Fatalf("sticky pick with the current account excluded = %+v, want no account and no wait", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts -run 'TestStrategiesSkipExcludedAccounts|TestStickyDoesNotWaitForAnExcludedCurrentAccount' -count=1`
Expected: FAIL. The build fails with `manager.pickLocked undefined (type *Manager has no field or method pickLocked)`.

- [ ] **Step 3: Write minimal implementation**

In `internal/accounts/manager.go`, replace `Select` (lines 372-387) with the code below. `pickLocked` goes directly after it:

```go
func (manager *Manager) Select(session, model string) Selection {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.clearExpiredLocked()
	if wait := manager.sharedThrottleWaitLocked(rateLimitModelKey(model)); wait > 0 {
		return Selection{Wait: wait}
	}
	return manager.pickLocked(model, nil)
}

// pickLocked runs the configured strategy. Accounts in exclude are treated as
// unusable; a nil or empty set is the plain strategy.
func (manager *Manager) pickLocked(model string, exclude map[string]bool) Selection {
	switch manager.strategy {
	case StrategySticky:
		return manager.selectStickyLocked(model, exclude)
	case StrategyRoundRobin:
		return manager.selectRoundRobinLocked(model, exclude)
	default:
		return manager.selectHybridLocked(model, exclude)
	}
}
```

Replace `selectStickyLocked`, `selectRoundRobinLocked` and `selectHybridLocked` (lines 845-931) with:

```go
func (manager *Manager) selectStickyLocked(model string, exclude map[string]bool) Selection {
	if len(manager.accounts) == 0 {
		return Selection{} // No accounts available
	}
	poolSupports := manager.poolSupportsModelLocked(model)
	if manager.currentIndex < 0 || manager.currentIndex >= len(manager.accounts) {
		manager.currentIndex = 0
	}
	current := manager.accounts[manager.currentIndex]
	if manager.usableLocked(current, model, poolSupports) && !exclude[current.Email] {
		current.LastUsedMS = manager.now().UnixMilli()
		return Selection{Account: current}
	}
	for offset := 1; offset <= len(manager.accounts); offset++ {
		index := (manager.currentIndex + offset) % len(manager.accounts)
		account := manager.accounts[index]
		if manager.usableLocked(account, model, poolSupports) && !exclude[account.Email] {
			manager.currentIndex = index
			account.LastUsedMS = manager.now().UnixMilli()
			return Selection{Account: account}
		}
	}
	// Only a current account that may be picked at all is worth waiting for.
	if limit := current.ModelRateLimits[rateLimitModelKey(model)]; current.Enabled && !current.IsInvalid && !exclude[current.Email] && limit != nil {
		wait := time.Duration(limit.ResetTimeMS-manager.now().UnixMilli()) * time.Millisecond
		if wait > 0 && wait <= maxStickyWait {
			return Selection{Wait: wait}
		}
	}
	return Selection{}
}

func (manager *Manager) selectRoundRobinLocked(model string, exclude map[string]bool) Selection {
	if len(manager.accounts) == 0 {
		return Selection{} // No accounts available
	}
	poolSupports := manager.poolSupportsModelLocked(model)
	if manager.cursor >= len(manager.accounts) {
		manager.cursor = 0
	}
	start := (manager.cursor + 1) % len(manager.accounts)
	for offset := range len(manager.accounts) {
		index := (start + offset) % len(manager.accounts)
		account := manager.accounts[index]
		if manager.usableLocked(account, model, poolSupports) && !exclude[account.Email] {
			manager.cursor = index
			manager.currentIndex = index
			account.LastUsedMS = manager.now().UnixMilli()
			return Selection{Account: account}
		}
	}
	return Selection{}
}

func (manager *Manager) selectHybridLocked(model string, exclude map[string]bool) Selection {
	type candidate struct {
		account *Account
		index   int
		score   float64
	}
	candidates := make([]candidate, 0)
	minUsable := mapFloat(manager.selectionConfig.HealthScore, "minUsable", 50)
	poolSupports := manager.poolSupportsModelLocked(model)
	for index, account := range manager.accounts {
		if !manager.usableLocked(account, model, poolSupports) || exclude[account.Email] || manager.healthScoreLocked(account.Email) < minUsable || manager.tokensLocked(account.Email) < 1 {
			continue
		}
		if manager.quotaCriticalLocked(account, model) {
			continue
		}
		candidates = append(candidates, candidate{account: account, index: index, score: manager.scoreLocked(account, model)})
	}
	if len(candidates) == 0 {
		for index, account := range manager.accounts {
			if manager.usableLocked(account, model, poolSupports) && !exclude[account.Email] {
				candidates = append(candidates, candidate{account: account, index: index, score: manager.scoreLocked(account, model)})
			}
		}
	}
	if len(candidates) == 0 {
		return Selection{}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	best := candidates[0]
	manager.consumeTokenLocked(best.account.Email)
	best.account.LastUsedMS = manager.now().UnixMilli()
	manager.currentIndex = best.index
	return Selection{Account: best.account}
}
```

`usableLocked` runs before `exclude[account.Email]` on purpose. `usableLocked` returns false for a nil account, so the short-circuit keeps a nil account from being dereferenced.

Update every caller. A one-argument `Select` call becomes a two-argument call with an empty session. The empty-pool tests of the three strategy functions now pass `nil`:

```bash
gofmt -r 'a.Select(b) -> a.Select("", b)' -w internal/accounts/dispatcher.go internal/accounts/manager_test.go internal/accounts/select_bench_test.go internal/api/management_test.go
gofmt -r 'a.selectStickyLocked(b) -> a.selectStickyLocked(b, nil)' -w internal/accounts/manager_test.go
gofmt -r 'a.selectRoundRobinLocked(b) -> a.selectRoundRobinLocked(b, nil)' -w internal/accounts/manager_test.go
gofmt -r 'a.selectHybridLocked(b) -> a.selectHybridLocked(b, nil)' -w internal/accounts/manager_test.go
```

Results to check by hand. These are the only one-argument `.Select(` calls in those files; `openrouter` router calls take three arguments and do not match the rewrite:
- `internal/accounts/dispatcher.go:338`: `selection := dispatcher.manager.Select("", "")`
- `internal/accounts/dispatcher.go:429`: `selection := dispatcher.manager.Select("", model)`. Task 3 passes the context's session key here.
- `internal/accounts/manager_test.go:77,97,111,116,119,137,142,584,604,652,665,677,684,783,1084,1091,1098,1146,1177,1198`: `manager.Select("", "<model>")` / `mgr.Select("", "<model>")`
- `internal/accounts/manager_test.go:475,483,491`: `manager.selectStickyLocked("model", nil)`, `manager.selectRoundRobinLocked("model", nil)`, `manager.selectHybridLocked("model", nil)`
- `internal/accounts/select_bench_test.go:29`: `_ = mgr.Select("", "claude-sonnet-5-5-high")`
- `internal/api/management_test.go:1528,1739,1900`: `sel := mgr.Select("", "gemini-3.8-flash-high")`

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/accounts -run 'TestStrategiesSkipExcludedAccounts|TestStickyDoesNotWaitForAnExcludedCurrentAccount' -count=1`
Expected: PASS.

Run: `go test ./internal/accounts ./internal/api -count=1`
Expected: PASS. All existing selection tests keep passing because a nil exclusion set is today's behaviour. Examples: `TestRoundRobinAndPerModelRateLimits`, `TestStickyWaitsOnlyForShortCurrentLimit`, the shared-throttle tests and `TestManager_SelectFiltersAccountsByModelSupport`.

Run: `gofmt -l internal/accounts internal/api`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/exclusion_test.go internal/accounts/dispatcher.go internal/accounts/manager_test.go internal/accounts/select_bench_test.go internal/api/management_test.go
git commit -m "refactor(accounts): exclusion sets for selection strategies; Select takes a session"
```

---

### Task 2: Session pins in the account manager (`conversation.go`)

**Files:**
- Create: `internal/accounts/conversation.go`
- Modify: `internal/accounts/manager.go:27-31` (`RateLimit` gains the unexported `failure` marker)
- Modify: `internal/accounts/manager.go:233-249` (`Manager` gains `pins` and `pinWait`)
- Modify: `internal/accounts/manager.go:317-331` (`New` sets `pinWait` to `maxStickyWait`)
- Modify: `internal/accounts/manager.go:372-387` (`Select` as rewritten in Task 1)
- Modify: `internal/accounts/manager.go:585-599` (`MarkFailure` tags its lock)
- Modify: `internal/accounts/manager.go:950-970` (`usableLocked`; the model-listing filter moves into `servesModel`)
- Test: `internal/accounts/conversation_test.go`

How the code tells a short rate limit from the other blockers (`manager.go`). Every per-model block is a `RateLimit` in `account.ModelRateLimits[rateLimitModelKey(model)]`, so the blocker type has to come from somewhere else:
- `MarkRateLimited` (`manager.go:551-573`) sets `ResetTimeMS = now+wait`. This covers a 429 and the 15 s capacity limit. It is the only block worth waiting for, and only when it lifts within `manager.pinWait`. `New` sets `pinWait` to `maxStickyWait` (2 m, `manager.go:24`). From Task 3 on, the Dispatcher lowers it to its own `maxWait` through `setPinWait`, because a pinned wait longer than `maxWait` would end in a `RateLimitError` (`dispatcher.go:448-454`) while another account could serve the request.
- `MarkFailure` (`manager.go:585-599`) adds a 1-minute lock after 3 consecutive failures. That lock looks exactly like a 60 s `MarkRateLimited`, so it needs a marker. This task adds the unexported `RateLimit.failure`. It is not persisted, and a reloaded lock reads as a plain rate limit.
- `MarkModelUnavailable` (`manager.go:608-638`) adds a 24 h lock, which is always longer than `maxStickyWait`.
- Quota exhaustion is set in the dispatcher by `recordQuotaExhaustion` → `MarkQuotaExhausted` (`manager.go:1762`), which writes `Quota.Models[key].ExhaustedUntilMS`. The dispatcher then calls `MarkRateLimited` with the upstream wait, which can be short. `ExhaustedUntilMS > now` therefore means "move", even when the paired limit is short.
- `CoolingDownUntilMS` makes `usableLocked` false (`manager.go:955`), but nothing sets it. It counts as a blocker.
- Model not listed: in a pool where some account lists the model, an account whose non-empty quota cache does not list it is unusable (`manager.go:961-968`). Waiting would not help, so it counts as a blocker.

`pinTargetLocked` puts these rules in one place. Its tests cover the boundary with a non-default `pinWait` of 30 s: exactly `pinWait` waits, and `pinWait + 1ms` overflows. A further case checks that the default bound is `maxStickyWait`.

Pins are swept when a pin is created, which happens once per new conversation, not on every request. A pin found idle for more than 1 h at lookup is treated as absent. `ActiveSessions` skips idle pins. This keeps `Select`'s cost flat with 10,000 pins.

**Interfaces:**
- Consumes (Task 1): `func (manager *Manager) pickLocked(model string, exclude map[string]bool) Selection`, `func (manager *Manager) Select(session, model string) Selection`.
- Produces:
  - `type pin struct { home string; overflow map[string]string; started map[string]bool; lastSeen time.Time }`
  - `Manager` field `pins map[string]*pin`, guarded by `manager.mu` and created lazily
  - `Manager` field `pinWait time.Duration`, guarded by `manager.mu`; `New` sets it to `maxStickyWait`
  - `func (manager *Manager) setPinWait(d time.Duration)`: takes `manager.mu` and stores `d` clamped to (0, `maxStickyWait`]; a `d` of zero or less, or above `maxStickyWait`, stores `maxStickyWait`
  - `const pinIdleTTL = time.Hour`, `const maxPins = 10000`
  - `func (manager *Manager) conversationStarted(session, email string) bool`: false when `session == ""` or the session has no pin
  - `func (manager *Manager) markConversationStarted(session, email string)`: does nothing when `session == ""` or the session has no pin, and never creates a pin
  - `func (manager *Manager) ActiveSessions() map[string]int`: email → number of live pins whose `home` or any `overflow` value is that email. Each pin is counted once per email.
  - `Select(session, model)` with a non-empty session follows spec §4.3. Pin changes are logged at info: `msg` is `session pinned`, `session rebound` or `session overflow`, with attributes `session` (first 8 chars), `model`, `from`, `to`, `reason`.
  - Internal, for this package only: `func (manager *Manager) pinTargetLocked(email, model string) pinTarget`, `func servesModel(account *Account, model string, poolSupports bool) bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/accounts/conversation_test.go`:

```go
package accounts

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"
)

const (
	pinModel   = "claude-sonnet-4-6"
	otherModel = "gemini-3.8-flash-high"
)

// pinTestManager builds a pool of testAccounts on a settable clock and
// returns the accounts in pool order.
func pinTestManager(t *testing.T, clock *time.Time, strategy string, emails ...string) (*Manager, []*Account) {
	t.Helper()
	pool := make([]*Account, 0, len(emails))
	for _, email := range emails {
		pool = append(pool, testAccount(email))
	}
	manager, err := New(Options{
		Accounts:             pool,
		Strategy:             strategy,
		SharedThrottleWindow: 10 * time.Second,
		Now:                  func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, pool
}

// selected returns the email Select picks, or "" when it picks no account.
func selected(manager *Manager, session, model string) string {
	if account := manager.Select(session, model).Account; account != nil {
		return account.Email
	}
	return ""
}

func TestPinTargetClassification(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		setup  func(t *testing.T, manager *Manager, a, b *Account)
		state  targetState
		wait   time.Duration
		reason string
	}{
		{name: "healthy", setup: func(*testing.T, *Manager, *Account, *Account) {}, state: targetUsable},
		{name: "limit on another model", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkRateLimited(a, otherModel, time.Hour)
		}, state: targetUsable},
		{name: "default bound is maxStickyWait", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkRateLimited(a, pinModel, maxStickyWait)
		}, state: targetWait, wait: maxStickyWait},
		{name: "rate limit of exactly pinWait", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.setPinWait(30 * time.Second)
			m.MarkRateLimited(a, pinModel, 30*time.Second)
		}, state: targetWait, wait: 30 * time.Second},
		{name: "rate limit 1ms past pinWait", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.setPinWait(30 * time.Second)
			m.MarkRateLimited(a, pinModel, 30*time.Second+time.Millisecond)
		}, state: targetBlocked, reason: "rate limited 30s"},
		{name: "failure lock lifting in a minute", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			for range 3 {
				m.MarkFailure(a, pinModel)
			}
		}, state: targetBlocked, reason: "failure lock"},
		{name: "404 lock", setup: func(t *testing.T, m *Manager, a, _ *Account) {
			if !m.MarkModelUnavailable(a, pinModel) {
				t.Fatal("MarkModelUnavailable = false with a spare account")
			}
		}, state: targetBlocked, reason: "rate limited 24h0m0s"},
		{name: "exhausted quota behind a short limit", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkQuotaExhausted(a.Email, pinModel, start.Add(90*time.Second).Format(time.RFC3339))
			m.MarkRateLimited(a, pinModel, 90*time.Second)
		}, state: targetBlocked, reason: "quota exhausted"},
		{name: "cooling down", setup: func(_ *testing.T, _ *Manager, a, _ *Account) {
			a.CoolingDownUntilMS = start.Add(time.Minute).UnixMilli()
		}, state: targetBlocked, reason: "cooling down"},
		{name: "short limit on a model the account does not list", setup: func(_ *testing.T, m *Manager, a, b *Account) {
			a.Quota.Models[otherModel] = ModelQuota{}
			b.Quota.Models[pinModel] = ModelQuota{}
			m.MarkRateLimited(a, pinModel, 30*time.Second)
		}, state: targetBlocked, reason: "model not listed"},
		{name: "invalid", setup: func(_ *testing.T, m *Manager, a, _ *Account) {
			m.MarkInvalid(a, "test", "")
		}, state: targetGone, reason: "invalid"},
		{name: "disabled", setup: func(_ *testing.T, _ *Manager, a, _ *Account) {
			a.Enabled = false
		}, state: targetGone, reason: "disabled"},
		{name: "removed", setup: func(t *testing.T, m *Manager, a, _ *Account) {
			if err := m.RemoveAccount(a.Email); err != nil {
				t.Fatal(err)
			}
		}, state: targetGone, reason: "removed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")
			test.setup(t, manager, pool[0], pool[1])
			manager.mu.Lock()
			got := manager.pinTargetLocked("a@example.com", pinModel)
			manager.mu.Unlock()
			if got.state != test.state || got.wait != test.wait || got.reason != test.reason {
				t.Fatalf("pinTargetLocked = state %d wait %s reason %q, want state %d wait %s reason %q",
					got.state, got.wait, got.reason, test.state, test.wait, test.reason)
			}
		})
	}
}

// The Dispatcher hands its maxWait to setPinWait. The pin bound never grows
// past maxStickyWait, and a missing value means maxStickyWait.
func TestPinWaitIsClampedToMaxStickyWait(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com")
	for _, test := range []struct {
		set, want time.Duration
	}{
		{set: 30 * time.Second, want: 30 * time.Second},
		{set: maxStickyWait, want: maxStickyWait},
		{set: maxStickyWait + time.Millisecond, want: maxStickyWait},
		{set: 10 * time.Minute, want: maxStickyWait},
		{set: time.Millisecond, want: time.Millisecond},
		{set: 0, want: maxStickyWait},
		{set: -time.Second, want: maxStickyWait},
	} {
		manager.setPinWait(test.set)
		if got := manager.pinWait; got != test.want {
			t.Errorf("setPinWait(%s) stored %s, want %s", test.set, got, test.want)
		}
	}
}

func TestPinKeepsASessionOnOneAccount(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")

	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("new pin = %q, want round-robin's next account b@example.com", got)
	}
	for turn := range 3 {
		if got := selected(manager, "s1", pinModel); got != "b@example.com" {
			t.Fatalf("turn %d = %q, want the pinned b@example.com", turn+2, got)
		}
	}
	if got := selected(manager, "s1", otherModel); got != "b@example.com" {
		t.Fatalf("another model in the same session = %q, want the pinned b@example.com", got)
	}
	if got := selected(manager, "s2", pinModel); got != "c@example.com" {
		t.Fatalf("second session = %q, want c@example.com: round-robin rotates per new pin", got)
	}
	if first, second := selected(manager, "", pinModel), selected(manager, "", pinModel); first != "a@example.com" || second != "b@example.com" {
		t.Fatalf("requests without a session = %q, %q; want per-request rotation a, b", first, second)
	}
}

func TestPinUnderStickyTakesTheGlobalCurrentAccount(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategySticky, "a@example.com", "b@example.com")

	if s1, s2 := selected(manager, "s1", pinModel), selected(manager, "s2", pinModel); s1 != "a@example.com" || s2 != "a@example.com" {
		t.Fatalf("new pins under sticky = %q, %q; want the current account a@example.com for both", s1, s2)
	}
	manager.MarkRateLimited(pool[0], pinModel, 10*time.Minute)
	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("overflow under sticky = %q, want b@example.com", got)
	}
	if got := selected(manager, "s3", pinModel); got != "b@example.com" {
		t.Fatalf("new pin after sticky moved = %q, want the new current account b@example.com", got)
	}
}

func TestPinWaitsOutAShortLimit(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")
	home := pool[1]

	if got := selected(manager, "s1", pinModel); got != home.Email {
		t.Fatalf("new pin = %q, want %s", got, home.Email)
	}
	manager.MarkRateLimited(home, pinModel, 30*time.Second)
	if got := manager.Select("s1", pinModel); got != (Selection{Wait: 30 * time.Second}) {
		t.Fatalf("pinned session under a 30s limit got %+v, want a 30s wait for its own account", got)
	}
	if got := selected(manager, "", pinModel); got != "a@example.com" {
		t.Fatalf("request without a session = %q, want a@example.com", got)
	}
	clock = clock.Add(30 * time.Second)
	if got := selected(manager, "s1", pinModel); got != home.Email {
		t.Fatalf("after the limit lifted the session got %q, want its home %s", got, home.Email)
	}
	if overflow := manager.pins["s1"].overflow; len(overflow) != 0 {
		t.Fatalf("a short limit must not create an overflow entry: %v", overflow)
	}
}

func TestPinOverflowsAndDoesNotFlipBack(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	blockers := []struct {
		name  string
		block func(t *testing.T, manager *Manager, home *Account)
	}{
		{name: "rate limit past maxStickyWait", block: func(_ *testing.T, m *Manager, home *Account) {
			m.MarkRateLimited(home, pinModel, maxStickyWait+time.Millisecond)
		}},
		{name: "exhausted quota", block: func(_ *testing.T, m *Manager, home *Account) {
			m.MarkQuotaExhausted(home.Email, pinModel, start.Add(90*time.Second).Format(time.RFC3339))
			m.MarkRateLimited(home, pinModel, 90*time.Second)
		}},
		{name: "failure lock", block: func(_ *testing.T, m *Manager, home *Account) {
			for range 3 {
				m.MarkFailure(home, pinModel)
			}
		}},
		{name: "404 lock", block: func(t *testing.T, m *Manager, home *Account) {
			if !m.MarkModelUnavailable(home, pinModel) {
				t.Fatal("MarkModelUnavailable = false with two spare accounts")
			}
		}},
	}
	for _, blocker := range blockers {
		t.Run(blocker.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
			home := pool[1]
			if got := selected(manager, "s1", pinModel); got != home.Email {
				t.Fatalf("new pin = %q, want %s", got, home.Email)
			}
			blocker.block(t, manager, home)

			if got := selected(manager, "s1", pinModel); got != "c@example.com" {
				t.Fatalf("blocked home: session moved to %q, want the overflow c@example.com", got)
			}
			entry := manager.pins["s1"]
			if entry.home != home.Email || entry.overflow[rateLimitModelKey(pinModel)] != "c@example.com" {
				t.Fatalf("pin = home %q overflow %v, want home %s and %s -> c@example.com", entry.home, entry.overflow, home.Email, pinModel)
			}
			if got := selected(manager, "s1", otherModel); got != home.Email {
				t.Fatalf("other model = %q, want the home %s: overflow is per model", got, home.Email)
			}
			manager.MarkSuccess(home, pinModel)
			if got := selected(manager, "s1", pinModel); got != "c@example.com" {
				t.Fatalf("after the home recovered the session moved to %q, want it to stay on c@example.com", got)
			}
		})
	}
}

func TestPinRebindsAGoneHomeAndKeepsOverflow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	gone := []struct {
		name   string
		remove func(t *testing.T, manager *Manager, home *Account)
	}{
		{name: "invalid", remove: func(_ *testing.T, m *Manager, home *Account) {
			m.MarkInvalid(home, "test", "")
		}},
		{name: "disabled", remove: func(t *testing.T, m *Manager, home *Account) {
			if err := m.SetAccountEnabled(home.Email, false); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "removed", remove: func(t *testing.T, m *Manager, home *Account) {
			if err := m.RemoveAccount(home.Email); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range gone {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
			home := pool[1]
			if got := selected(manager, "s1", pinModel); got != home.Email {
				t.Fatalf("new pin = %q, want %s", got, home.Email)
			}
			manager.MarkRateLimited(home, otherModel, 10*time.Minute)
			if got := selected(manager, "s1", otherModel); got != "c@example.com" {
				t.Fatalf("overflow for %s = %q, want c@example.com", otherModel, got)
			}

			test.remove(t, manager, home)
			rebound := selected(manager, "s1", pinModel)
			if rebound == "" || rebound == home.Email {
				t.Fatalf("session with a %s home got %q, want another account", test.name, rebound)
			}
			if got := manager.pins["s1"].home; got != rebound {
				t.Fatalf("pin home = %q, want the rebound %q", got, rebound)
			}
			if got := selected(manager, "s1", pinModel); got != rebound {
				t.Fatalf("next turn = %q, want the rebound home %q", got, rebound)
			}
			if got := manager.pins["s1"].overflow[rateLimitModelKey(otherModel)]; got != "c@example.com" {
				t.Fatalf("healthy overflow entry = %q, want it kept on c@example.com", got)
			}
			if got := selected(manager, "s1", otherModel); got != "c@example.com" {
				t.Fatalf("%s after the rebind = %q, want the kept overflow c@example.com", otherModel, got)
			}
		})
	}
}

func TestPinRepicksADeadOverflowAccount(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	deaths := []struct {
		name string
		kill func(manager *Manager, overflow *Account)
	}{
		{name: "invalid", kill: func(m *Manager, overflow *Account) { m.MarkInvalid(overflow, "test", "") }},
		{name: "long limit", kill: func(m *Manager, overflow *Account) { m.MarkRateLimited(overflow, pinModel, 10*time.Minute) }},
	}
	for _, death := range deaths {
		t.Run(death.name, func(t *testing.T) {
			t.Parallel()
			clock := start
			manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
			spare, home, overflow := pool[0], pool[1], pool[2]
			if got := selected(manager, "s1", pinModel); got != home.Email {
				t.Fatalf("new pin = %q, want %s", got, home.Email)
			}
			manager.MarkRateLimited(home, pinModel, 10*time.Minute)
			if got := selected(manager, "s1", pinModel); got != overflow.Email {
				t.Fatalf("first overflow = %q, want %s", got, overflow.Email)
			}
			// Two rejections inside one window would raise a pool-wide throttle.
			clock = clock.Add(11 * time.Second)
			death.kill(manager, overflow)

			if got := selected(manager, "s1", pinModel); got != spare.Email {
				t.Fatalf("after the overflow account died the session got %q, want %s", got, spare.Email)
			}
			entry := manager.pins["s1"]
			if entry.home != home.Email || entry.overflow[rateLimitModelKey(pinModel)] != spare.Email {
				t.Fatalf("pin = home %q overflow %v, want home %s and %s -> %s", entry.home, entry.overflow, home.Email, pinModel, spare.Email)
			}
		})
	}
}

func TestPinWithNoAlternativeGetsTodaysAnswer(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

	t.Run("round-robin, long limit", func(t *testing.T) {
		t.Parallel()
		clock := start
		manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "only@example.com")
		if got := selected(manager, "s1", pinModel); got != pool[0].Email {
			t.Fatalf("new pin = %q", got)
		}
		manager.MarkRateLimited(pool[0], pinModel, 10*time.Minute)
		got, today := manager.Select("s1", pinModel), manager.Select("", pinModel)
		if got != today || got != (Selection{}) {
			t.Fatalf("pinned answer %+v, answer without a session %+v; want both empty", got, today)
		}
		if entry := manager.pins["s1"]; entry.home != pool[0].Email || len(entry.overflow) != 0 {
			t.Fatalf("pin changed with nowhere to move: home %q overflow %v", entry.home, entry.overflow)
		}
	})

	t.Run("sticky, failure lock", func(t *testing.T) {
		t.Parallel()
		clock := start
		manager, pool := pinTestManager(t, &clock, StrategySticky, "only@example.com")
		if got := selected(manager, "s1", pinModel); got != pool[0].Email {
			t.Fatalf("new pin = %q", got)
		}
		for range 3 {
			manager.MarkFailure(pool[0], pinModel)
		}
		got, today := manager.Select("s1", pinModel), manager.Select("", pinModel)
		if got != today || got != (Selection{Wait: time.Minute}) {
			t.Fatalf("pinned answer %+v, answer without a session %+v; want both sticky's 1m wait", got, today)
		}
		if entry := manager.pins["s1"]; len(entry.overflow) != 0 {
			t.Fatalf("pin changed with nowhere to move: overflow %v", entry.overflow)
		}
	})
}

func TestPinExpiresAfterAnIdleHour(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")

	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("new pin = %q, want b@example.com", got)
	}
	clock = clock.Add(pinIdleTTL)
	if got := selected(manager, "s1", pinModel); got != "b@example.com" {
		t.Fatalf("after exactly an idle hour = %q, want the pin kept on b@example.com", got)
	}
	clock = clock.Add(pinIdleTTL + time.Millisecond)
	if got := selected(manager, "s1", pinModel); got != "a@example.com" {
		t.Fatalf("past an idle hour = %q, want a new pin on round-robin's next account a@example.com", got)
	}
}

func TestPinSweepDropsIdlePins(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")

	selected(manager, "old", pinModel)
	clock = clock.Add(pinIdleTTL + time.Millisecond)
	if active := manager.ActiveSessions(); len(active) != 0 {
		t.Fatalf("an idle pin counts as active: %v", active)
	}
	selected(manager, "new", pinModel)
	if _, kept := manager.pins["old"]; kept || len(manager.pins) != 1 {
		t.Fatalf("pins after creating one = %d (old kept: %v), want only the new pin", len(manager.pins), kept)
	}
}

func TestPinCapEvictsTheOldest(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	clock := start
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com")
	manager.pins = make(map[string]*pin, maxPins)
	for i := range maxPins {
		manager.pins[fmt.Sprintf("old-%05d", i)] = &pin{home: pool[0].Email, lastSeen: start.Add(-time.Duration(maxPins-i) * time.Millisecond)}
	}

	if got := selected(manager, "new", pinModel); got != pool[0].Email {
		t.Fatalf("new pin = %q, want %s", got, pool[0].Email)
	}
	if len(manager.pins) != maxPins {
		t.Fatalf("pins = %d, want the cap %d", len(manager.pins), maxPins)
	}
	if _, kept := manager.pins["old-00000"]; kept {
		t.Fatal("the least recently used pin survived the cap")
	}
	if _, kept := manager.pins["old-00001"]; !kept {
		t.Fatal("eviction dropped more than one pin")
	}
	if _, kept := manager.pins["new"]; !kept {
		t.Fatal("the new pin was not stored")
	}
}

func TestPinActiveSessionsCountsHomeAndOverflow(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com", "c@example.com")
	a, b, c := pool[0], pool[1], pool[2]

	if got := selected(manager, "s1", pinModel); got != b.Email {
		t.Fatalf("s1 = %q, want %s", got, b.Email)
	}
	manager.MarkRateLimited(b, pinModel, 10*time.Minute)
	if got := selected(manager, "s1", pinModel); got != c.Email {
		t.Fatalf("s1 overflow = %q, want %s", got, c.Email)
	}
	if got := selected(manager, "s2", pinModel); got != a.Email {
		t.Fatalf("s2 = %q, want %s", got, a.Email)
	}
	// A pin whose overflow points back at its home counts once for that account.
	manager.pins["s3"] = &pin{home: a.Email, overflow: map[string]string{rateLimitModelKey(otherModel): a.Email}, lastSeen: clock}

	want := map[string]int{a.Email: 2, b.Email: 1, c.Email: 1}
	if got := manager.ActiveSessions(); !maps.Equal(got, want) {
		t.Fatalf("ActiveSessions = %v, want %v", got, want)
	}
}

func TestPinConversationStarted(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, _ := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")

	if manager.conversationStarted("s1", "b@example.com") {
		t.Fatal("conversationStarted = true for a session with no pin")
	}
	manager.markConversationStarted("s1", "b@example.com")
	if len(manager.pins) != 0 {
		t.Fatal("markConversationStarted created a pin")
	}
	home := selected(manager, "s1", pinModel)
	if manager.conversationStarted("s1", home) {
		t.Fatal("conversationStarted = true before any start burst")
	}
	manager.markConversationStarted("s1", home)
	if !manager.conversationStarted("s1", home) {
		t.Fatalf("conversationStarted(s1, %s) = false after marking it", home)
	}
	if manager.conversationStarted("s1", "a@example.com") {
		t.Fatal("a start on one account marked another account started")
	}
	if manager.conversationStarted("", home) || manager.conversationStarted("s2", home) {
		t.Fatal("conversationStarted = true for an empty or unknown session")
	}
}

// Pin changes are logged at info so a smoke run can follow them. Only the
// first 8 characters of the session id appear in the log.
func TestPinChangesAreLogged(t *testing.T) {
	var logBuffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, pool := pinTestManager(t, &clock, StrategyRoundRobin, "a@example.com", "b@example.com")
	const session = "0123456789abcdef"
	selected(manager, session, pinModel)
	manager.MarkRateLimited(pool[1], pinModel, 10*time.Minute)
	selected(manager, session, pinModel)

	out := logBuffer.String()
	for _, want := range []string{
		`msg="session pinned" session=01234567 model=claude-sonnet-4-6 from="" to=b@example.com reason=new`,
		`msg="session overflow" session=01234567 model=claude-sonnet-4-6 from=b@example.com to=a@example.com reason="rate limited 10m0s"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, session) {
		t.Errorf("log carries the full session id:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts -run 'TestPin' -count=1`
Expected: FAIL. The build fails with errors such as `undefined: targetState`, `undefined: pin`, `manager.pins undefined (type *Manager has no field or method pins)`, `manager.setPinWait undefined (type *Manager has no field or method setPinWait)`, `undefined: pinIdleTTL`, `undefined: maxPins`.

- [ ] **Step 3: Write minimal implementation**

`internal/accounts/manager.go`, replace `RateLimit` (lines 27-31):

```go
type RateLimit struct {
	IsRateLimited bool  `json:"isRateLimited"`
	ResetTimeMS   int64 `json:"-"`
	ActualResetMS int64 `json:"actualResetMs,omitempty"`
	// failure marks MarkFailure's lock: a session pin moves the model to
	// another account instead of waiting it out. Not persisted, so a reloaded
	// lock reads as a plain rate limit.
	failure bool
}
```

`internal/accounts/manager.go`, `Manager` struct (lines 233-249): add after `modelThrottles       map[string]*sharedThrottle`:

```go
	// pins maps a session id to the account its conversation is pinned to;
	// see conversation.go. Created on first use.
	pins map[string]*pin
	// pinWait is the longest rate limit a pinned session waits out before
	// its model moves to another account; see setPinWait.
	pinWait time.Duration
```

`internal/accounts/manager.go`, `New` (lines 317-331): the returned literal becomes

```go
	return &Manager{
		configPath:           options.ConfigPath,
		accounts:             options.Accounts,
		settings:             options.Settings,
		strategy:             strategy,
		selectionConfig:      selectionConfig,
		globalQuotaThreshold: globalQuotaThreshold,
		currentIndex:         options.ActiveIndex,
		now:                  options.Now,
		health:               make(map[string]healthRecord),
		buckets:              make(map[string]tokenBucket),
		projects:             make(map[string]string),
		sharedThrottleWindow: sharedThrottleWindow,
		modelThrottles:       make(map[string]*sharedThrottle),
		pinWait:              maxStickyWait,
	}, nil
```

`internal/accounts/manager.go`, replace `Select` (Task 1's version):

```go
// Select picks an account for model. A request with a session stays on the
// session's pinned account (selectPinnedLocked); one without keeps the
// per-request strategy choice.
func (manager *Manager) Select(session, model string) Selection {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.clearExpiredLocked()
	if wait := manager.sharedThrottleWaitLocked(rateLimitModelKey(model)); wait > 0 {
		return Selection{Wait: wait}
	}
	if session == "" {
		return manager.pickLocked(model, nil)
	}
	return manager.selectPinnedLocked(session, model)
}
```

`internal/accounts/manager.go`, `MarkFailure` (lines 594-598): the lock literal becomes:

```go
	if key != "" && account.ConsecutiveFailure >= 3 {
		account.ModelRateLimits[key] = &RateLimit{
			IsRateLimited: true, ResetTimeMS: manager.now().Add(time.Minute).UnixMilli(), ActualResetMS: time.Minute.Milliseconds(),
			failure: true,
		}
	}
```

`internal/accounts/manager.go`, replace `usableLocked` (lines 950-970) and add `servesModel` after it:

```go
func (manager *Manager) usableLocked(account *Account, model string, poolSupports bool) bool {
	if account == nil || !account.Enabled || account.IsInvalid {
		return false
	}
	now := manager.now().UnixMilli()
	if account.CoolingDownUntilMS > now {
		return false
	}
	if limit := account.ModelRateLimits[rateLimitModelKey(model)]; limit != nil && limit.IsRateLimited && limit.ResetTimeMS > now {
		return false
	}
	return servesModel(account, model, poolSupports)
}

// servesModel applies the heterogeneous-pool filter: when some account in the
// pool lists model, an account whose non-empty quota cache does not is
// skipped. Accounts with uninitialized quota (len == 0) stay usable to allow
// initial discovery.
func servesModel(account *Account, model string, poolSupports bool) bool {
	return !poolSupports || len(account.Quota.Models) == 0 || accountSupportsQuotaModel(account, model)
}
```

Create `internal/accounts/conversation.go`:

```go
package accounts

import (
	"log/slog"
	"slices"
	"time"
)

const (
	pinIdleTTL = time.Hour
	maxPins    = 10000
)

// pin keeps one conversation on one account. It holds emails, not *Account,
// so a reload leaves it valid: an email no longer in the pool reads as a
// removed account.
type pin struct {
	home     string            // account email
	overflow map[string]string // rateLimitModelKey(model) -> account email
	started  map[string]bool   // account emails whose start burst ran for this session
	lastSeen time.Time
}

// targetState is what a pinned account can do for one model right now.
type targetState int

const (
	targetUsable targetState = iota
	// targetWait: a plain rate limit that lifts within manager.pinWait.
	targetWait
	// targetGone: removed, disabled or invalid. The pin rebinds.
	targetGone
	// targetBlocked: anything longer. The model moves to an overflow account.
	targetBlocked
)

type pinTarget struct {
	account *Account
	state   targetState
	wait    time.Duration
	reason  string
}

// selectPinnedLocked keeps a session on its pinned account. The strategy
// chooses only when the pin is created, when its account is gone, and when a
// model has to move to an overflow account. An overflow entry stays after the
// home account recovers: moving back would start another cascade.
func (manager *Manager) selectPinnedLocked(session, model string) Selection {
	now := manager.now()
	entry := manager.pins[session]
	if entry != nil && now.Sub(entry.lastSeen) > pinIdleTTL {
		delete(manager.pins, session)
		entry = nil
	}
	if entry == nil {
		selection := manager.pickLocked(model, nil)
		if selection.Account != nil {
			manager.addPinLocked(session, &pin{home: selection.Account.Email, lastSeen: now})
			logPinChange("session pinned", session, model, "", selection.Account.Email, "new")
		}
		return selection
	}
	entry.lastSeen = now
	key := rateLimitModelKey(model)
	email, overflowed := entry.overflow[key]
	if !overflowed {
		email = entry.home
	}
	target := manager.pinTargetLocked(email, model)
	switch target.state {
	case targetUsable:
		target.account.LastUsedMS = now.UnixMilli()
		return Selection{Account: target.account}
	case targetWait:
		return Selection{Wait: target.wait}
	case targetBlocked:
		selection := manager.pickLocked(model, map[string]bool{email: true})
		if selection.Account == nil {
			// Nothing else serves the model: the pin stays as it is and the
			// answer is the one a request without a session gets.
			return manager.pickLocked(model, nil)
		}
		if entry.overflow == nil {
			entry.overflow = make(map[string]string)
		}
		entry.overflow[key] = selection.Account.Email
		logPinChange("session overflow", session, model, email, selection.Account.Email, target.reason)
		return selection
	}
	selection := manager.pickLocked(model, nil)
	if selection.Account == nil {
		return selection
	}
	if overflowed {
		entry.overflow[key] = selection.Account.Email
		logPinChange("session overflow", session, model, email, selection.Account.Email, target.reason)
	} else {
		entry.home = selection.Account.Email
		logPinChange("session rebound", session, model, email, selection.Account.Email, target.reason)
	}
	return selection
}

// pinTargetLocked classifies the pinned account for model. Only a plain rate
// limit (MarkRateLimited) that lifts within manager.pinWait is worth waiting
// for. A failure lock, an exhausted quota, the 404 lock, a cooldown, or a
// model the account does not list moves the model to another account.
func (manager *Manager) pinTargetLocked(email, model string) pinTarget {
	account := manager.accountLocked(email)
	switch {
	case account == nil:
		return pinTarget{state: targetGone, reason: "removed"}
	case !account.Enabled:
		return pinTarget{account: account, state: targetGone, reason: "disabled"}
	case account.IsInvalid:
		return pinTarget{account: account, state: targetGone, reason: "invalid"}
	}
	poolSupports := manager.poolSupportsModelLocked(model)
	if manager.usableLocked(account, model, poolSupports) {
		return pinTarget{account: account, state: targetUsable}
	}
	nowMS := manager.now().UnixMilli()
	blocked := pinTarget{account: account, state: targetBlocked}
	limit := account.ModelRateLimits[rateLimitModelKey(model)]
	quota, _ := modelQuotaFor(account, model)
	switch {
	case account.CoolingDownUntilMS > nowMS:
		blocked.reason = "cooling down"
	case !servesModel(account, model, poolSupports):
		blocked.reason = "model not listed"
	case quota.ExhaustedUntilMS > nowMS:
		blocked.reason = "quota exhausted"
	case limit == nil:
		blocked.reason = "unusable"
	case limit.failure:
		blocked.reason = "failure lock"
	default:
		wait := time.Duration(limit.ResetTimeMS-nowMS) * time.Millisecond
		if wait <= manager.pinWait {
			return pinTarget{account: account, state: targetWait, wait: max(wait, time.Millisecond)}
		}
		blocked.reason = "rate limited " + wait.Round(time.Second).String()
	}
	return blocked
}

// setPinWait sets the longest rate limit a pinned session waits out. The
// Dispatcher passes its maxWait: it answers a longer wait with a
// RateLimitError, even when another account could serve the request. The
// bound never exceeds maxStickyWait, and a value of zero or less means
// maxStickyWait.
func (manager *Manager) setPinWait(d time.Duration) {
	if d <= 0 || d > maxStickyWait {
		d = maxStickyWait
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pinWait = d
}

// addPinLocked stores a new pin. Idle pins are swept here, once per new
// conversation rather than on every request. At the cap the least recently
// used pins are evicted.
func (manager *Manager) addPinLocked(session string, entry *pin) {
	if manager.pins == nil {
		manager.pins = make(map[string]*pin)
	}
	for key, existing := range manager.pins {
		if entry.lastSeen.Sub(existing.lastSeen) > pinIdleTTL {
			delete(manager.pins, key)
		}
	}
	if excess := len(manager.pins) + 1 - maxPins; excess > 0 {
		keys := make([]string, 0, len(manager.pins))
		for key := range manager.pins {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b string) int {
			return manager.pins[a].lastSeen.Compare(manager.pins[b].lastSeen)
		})
		for _, key := range keys[:excess] {
			delete(manager.pins, key)
		}
	}
	manager.pins[session] = entry
}

func (manager *Manager) accountLocked(email string) *Account {
	for _, account := range manager.accounts {
		if account.Email == email {
			return account
		}
	}
	return nil
}

// conversationStarted reports whether the start burst already ran for session
// on the account email.
func (manager *Manager) conversationStarted(session, email string) bool {
	if session == "" {
		return false
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	entry := manager.pins[session]
	return entry != nil && entry.started[email]
}

// markConversationStarted records a completed start burst. A session without
// a pin has nothing to record on.
func (manager *Manager) markConversationStarted(session, email string) {
	if session == "" {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry := manager.pins[session]
	if entry == nil {
		return
	}
	if entry.started == nil {
		entry.started = make(map[string]bool)
	}
	entry.started[email] = true
}

// ActiveSessions returns, per account email, how many live session pins use
// that account as home or as an overflow account.
func (manager *Manager) ActiveSessions() map[string]int {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	now := manager.now()
	counts := make(map[string]int)
	var counted []string
	for _, entry := range manager.pins {
		if now.Sub(entry.lastSeen) > pinIdleTTL {
			continue
		}
		counted = append(counted[:0], entry.home)
		counts[entry.home]++
		for _, email := range entry.overflow {
			if !slices.Contains(counted, email) {
				counted = append(counted, email)
				counts[email]++
			}
		}
	}
	return counts
}

func logPinChange(message, session, model, from, to, reason string) {
	slog.Info(message, "session", sessionPrefix(session), "model", model, "from", from, "to", to, "reason", reason)
}

// sessionPrefix keeps logs correlatable without writing whole session ids.
func sessionPrefix(session string) string {
	if len(session) > 8 {
		return session[:8]
	}
	return session
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/accounts -run 'TestPin' -count=1`
Expected: PASS.

Run: `go test -race ./internal/accounts -count=1`
Expected: PASS. Requests without a session still go through `pickLocked(model, nil)`. `servesModel` keeps the filter from `usableLocked`, so `TestManager_SelectFiltersAccountsByModelSupport` and `TestManager_SelectKeepsUndiscoveredAccountWhenPoolListsModel` are unchanged. `TestMarkFailureDoesNotRateLimitEmptyModel` still sees an active lock.

Run: `gofmt -l internal/accounts`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/conversation.go internal/accounts/conversation_test.go internal/accounts/manager.go
git commit -m "feat(accounts): pin Cloud Code sessions to one account with per-model overflow"
```

---

### Task 3: Session key on the request path and `activeSessions` in `/api/accounts`

**Files:**
- Modify: `internal/accounts/conversation.go` (created in Task 2; adds the context key, `WithSessionKey`, `SessionKey`)
- Modify: `internal/accounts/dispatcher.go:173-174` (`NewDispatcher` hands `MaxWait` to the Manager; insert between these lines)
- Modify: `internal/accounts/dispatcher.go:209-211` (`UpdateConfig` hands the new `maxWait` to the Manager)
- Modify: `internal/accounts/dispatcher.go:426-429` (`StreamGenerateContent` passes the session key)
- Modify: `internal/api/server.go:886-890` (`messages()`; insert after line 890)
- Modify: `internal/api/management.go:1001-1011` (`handleAccountsList`)
- Test: `internal/accounts/dispatcher_test.go` (append after line 925)
- Test: `internal/api/session_key_test.go` (create)
- Test: `internal/api/management_test.go` (insert after line 575, after `TestManagement_AccountsCRUD`)

**Interfaces:**
- Consumes (Task 1): `func (manager *Manager) Select(session, model string) Selection`. (Task 2): `func (manager *Manager) ActiveSessions() map[string]int`, `func (manager *Manager) setPinWait(d time.Duration)`, plus the pin behaviour of `Select` for a non-empty session.
- Produces:
  - `func WithSessionKey(ctx context.Context, key string) context.Context`: an empty key returns `ctx` unchanged.
  - `func SessionKey(ctx context.Context) string`: returns `""` when no key is set. The key type is the unexported `sessionKeyContextKey struct{}`.
  - `server.messages()` wraps the request context with `accounts.WithSessionKey(request.Context(), ccExtractSessionID(request, anthropicRequest))` before any gateway or Cloud Code send. `unaryMessage`, `streamMessage` and the `send` closure read that context.
  - `(*Dispatcher).StreamGenerateContent` calls `dispatcher.manager.Select(SessionKey(ctx), model)`. The catalog fetch (`dispatcher.go:338`) still passes `""`.
  - `NewDispatcher` calls `options.Manager.setPinWait(options.MaxWait)` once defaults are applied. `UpdateConfig` calls `dispatcher.manager.setPinWait(dispatcher.maxWait)` after it changes `maxWait`. So a pinned wait is never longer than `min(maxWait, maxStickyWait)`, and the Dispatcher's `wait <= dispatcher.maxWait` sleep branch (`dispatcher.go:441`) always takes it.
  - `GET /api/accounts` sends `activeSessions` (an integer, 0 when the account has no pins) on every element of `accounts`.

- [ ] **Step 1: Write the failing test**

Append to `internal/accounts/dispatcher_test.go`:

```go
// A conversation stays on one account under round-robin: the session key on
// the context reaches Select, and a second session takes the next account.
// Requests without a key keep rotating per request.
func TestStreamGenerateContentKeepsASessionOnItsAccount(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	close(release)
	manager, err := New(Options{
		Accounts: []*Account{
			{Email: "acc1@example.com", Enabled: true},
			{Email: "acc2@example.com", Enabled: true},
		},
		Strategy: StrategyRoundRobin,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  accountTokenResolver{},
		NewClient: func(string) CloudClient { return &blockingModelsClient{release: release} },
		ProjectID: "test-proj-affinity",
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	t.Cleanup(dispatcher.Close)
	// A cached catalog keeps resolveModel from selecting an account for a
	// catalog fetch, so the round-robin cursor moves only for generation.
	dispatcher.cacheCatalog("acc1@example.com", []byte(testCatalogBody))

	served := func(session string) string {
		t.Helper()
		ctx, meta := cloudcode.WithExecutionMetadata(WithSessionKey(context.Background(), session))
		if _, err := dispatcher.StreamGenerateContent(ctx, map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil }); err != nil {
			t.Fatalf("StreamGenerateContent(session %q): %v", session, err)
		}
		return meta.Account
	}

	first := served("session-one")
	for turn := range 3 {
		if got := served("session-one"); got != first {
			t.Fatalf("turn %d of session-one served by %s, want %s", turn+2, got, first)
		}
	}
	if second := served("session-two"); second == first {
		t.Fatalf("session-two served by %s like session-one; round-robin must rotate per new session", second)
	}
	if a, b := served(""), served(""); a == b {
		t.Fatalf("two requests without a session were both served by %s; they must rotate per request", a)
	}
}

// A pinned session waits for its account only as long as the Dispatcher
// would. A limit longer than maxWait moves the session to another account
// instead of failing with a RateLimitError, and a maxWait lowered by
// UpdateConfig applies to pins at once.
func TestStreamGenerateContentMovesAPinnedSessionPastMaxWait(t *testing.T) {
	t.Parallel()
	errWaited := errors.New("dispatcher slept waiting for the pinned account")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T, maxWait time.Duration) (*Dispatcher, map[string]*Account) {
		t.Helper()
		pool := map[string]*Account{
			"acc1@example.com": {Email: "acc1@example.com", Enabled: true},
			"acc2@example.com": {Email: "acc2@example.com", Enabled: true},
		}
		manager, err := New(Options{
			Accounts: []*Account{pool["acc1@example.com"], pool["acc2@example.com"]},
			Strategy: StrategyRoundRobin,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("new manager: %v", err)
		}
		release := make(chan struct{})
		close(release)
		dispatcher, err := NewDispatcher(DispatcherOptions{
			Manager:   manager,
			Resolver:  accountTokenResolver{},
			NewClient: func(string) CloudClient { return &blockingModelsClient{release: release} },
			ProjectID: "test-proj-affinity",
			MaxWait:   maxWait,
			Now:       func() time.Time { return now },
			// The clock never moves, so a wait would never end: fail instead.
			Sleep: func(context.Context, time.Duration) error { return errWaited },
		})
		if err != nil {
			t.Fatalf("new dispatcher: %v", err)
		}
		t.Cleanup(dispatcher.Close)
		dispatcher.cacheCatalog("acc1@example.com", []byte(testCatalogBody))
		return dispatcher, pool
	}
	serve := func(t *testing.T, dispatcher *Dispatcher, session string) string {
		t.Helper()
		ctx, meta := cloudcode.WithExecutionMetadata(WithSessionKey(context.Background(), session))
		if _, err := dispatcher.StreamGenerateContent(ctx, map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil }); err != nil {
			t.Fatalf("StreamGenerateContent(session %q): %v", session, err)
		}
		return meta.Account
	}

	t.Run("MaxWait at construction", func(t *testing.T) {
		t.Parallel()
		dispatcher, pool := setup(t, 30*time.Second)
		home := serve(t, dispatcher, "s1")
		dispatcher.manager.MarkRateLimited(pool[home], "gemini-2.5-pro", time.Minute)
		if got := serve(t, dispatcher, "s1"); got == "" || got == home {
			t.Fatalf("home %s limited for 60s with MaxWait 30s: served by %q, want the other account", home, got)
		}
	})

	t.Run("maxWait lowered by UpdateConfig", func(t *testing.T) {
		t.Parallel()
		dispatcher, pool := setup(t, 0) // NewDispatcher's 2-minute default
		dispatcher.UpdateConfig(config.Config{MaxWaitBeforeErrorMs: 20000})
		home := serve(t, dispatcher, "s1")
		dispatcher.manager.MarkRateLimited(pool[home], "gemini-2.5-pro", 25*time.Second)
		if got := serve(t, dispatcher, "s1"); got == "" || got == home {
			t.Fatalf("home %s limited for 25s after maxWait dropped to 20s: served by %q, want the other account", home, got)
		}
	})
}
```

Create `internal/api/session_key_test.go`:

```go
package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

// sessionKeyBackend records the session key each Cloud Code send carries.
type sessionKeyBackend struct {
	mu   sync.Mutex
	keys []string
}

func (b *sessionKeyBackend) FetchAvailableModels(context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func (b *sessionKeyBackend) StreamGenerateContent(ctx context.Context, _ map[string]any, _ func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keys = append(b.keys, accounts.SessionKey(ctx))
	return cloudcode.Response{}, nil
}

// The account pool pins on the session key, so messages() must hand the
// backend the same id the request metrics use: the Claude Code header first,
// then the session inside metadata.user_id.
func TestMessagesCarriesTheSessionKeyToTheBackend(t *testing.T) {
	config.SetForTest(config.DefaultConfig())
	const headerSession = "8b1f7c62-3c1e-4a51-9a63-1f0c2f3e9d10"
	const bodySession = "5d2e0a4b-7f61-4c3a-8e92-6b0d1c4f2a77"
	tests := []struct {
		name   string
		header string
		body   string
		want   string
	}{
		{
			name:   "Claude Code header",
			header: headerSession,
			body:   `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`,
			want:   headerSession,
		},
		{
			name:   "streaming request",
			header: headerSession,
			body:   `{"model":"gemini-3.8-flash-high","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			want:   headerSession,
		},
		{
			name: "metadata user_id",
			body: `{"model":"gemini-3.8-flash-high","metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"` + bodySession + `\"}"},"messages":[{"role":"user","content":"hi"}]}`,
			want: bodySession,
		},
		{
			name: "no session",
			body: `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &sessionKeyBackend{}
			server := &Server{backend: backend, builder: proxyformat.NewBuilder(), logger: slog.Default(), now: time.Now}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				req.Header.Set("X-Claude-Code-Session-Id", test.header)
			}
			rec := httptest.NewRecorder()
			server.messages(rec, req)

			if len(backend.keys) != 1 || backend.keys[0] != test.want {
				t.Fatalf("backend saw session keys %q, want [%q]; status=%d body=%s", backend.keys, test.want, rec.Code, rec.Body.String())
			}
		})
	}
}
```

Insert into `internal/api/management_test.go` after line 575:

```go
// /api/accounts reports, per account, how many live session pins use it.
func TestAccountsListReportsActiveSessions(t *testing.T) {
	server, mgr, _ := newTestServerWithManager(t)
	handler := server.Handler()
	activeSessions := func() int {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/accounts = %d: %s", rec.Code, rec.Body.String())
		}
		var res struct {
			Accounts []struct {
				Email          string `json:"email"`
				ActiveSessions *int   `json:"activeSessions"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Accounts) != 1 || res.Accounts[0].ActiveSessions == nil {
			t.Fatalf("accounts carry no activeSessions: %s", rec.Body.String())
		}
		return *res.Accounts[0].ActiveSessions
	}

	if got := activeSessions(); got != 0 {
		t.Fatalf("activeSessions before any session = %d, want 0", got)
	}
	for _, session := range []string{"session-a", "session-b", "session-a"} {
		if mgr.Select(session, "gemini-3.8-flash-high").Account == nil {
			t.Fatalf("Select(%q) picked no account", session)
		}
	}
	if got := activeSessions(); got != 2 {
		t.Fatalf("activeSessions = %d, want 2: two sessions pinned to the only account", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts -run 'TestStreamGenerateContentKeepsASessionOnItsAccount|TestStreamGenerateContentMovesAPinnedSessionPastMaxWait' -count=1`
Expected: FAIL. The build fails with `undefined: WithSessionKey`.

Run: `go test ./internal/api -run 'TestMessagesCarriesTheSessionKeyToTheBackend|TestAccountsListReportsActiveSessions' -count=1`
Expected: FAIL. The build fails with `undefined: accounts.SessionKey`.

- [ ] **Step 3: Write minimal implementation**

`internal/accounts/conversation.go`: the import block becomes

```go
import (
	"context"
	"log/slog"
	"slices"
	"time"
)
```

and append:

```go
type sessionKeyContextKey struct{}

// WithSessionKey returns ctx carrying the conversation's session id, which
// Select pins to one account. An empty key leaves ctx unchanged.
func WithSessionKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKeyContextKey{}, key)
}

// SessionKey returns the session id stored by WithSessionKey, or "".
func SessionKey(ctx context.Context) string {
	key, _ := ctx.Value(sessionKeyContextKey{}).(string)
	return key
}
```

`internal/accounts/dispatcher.go`, lines 426-429 (Task 1 left line 429 as `Select("", model)`) become:

```go
	maxAttempts := max(dispatcher.maxRetries, dispatcher.manager.Count()+1)
	session := SessionKey(ctx)
	var lastError error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		selection := dispatcher.manager.Select(session, model)
```

`internal/accounts/dispatcher.go`, `NewDispatcher` (lines 140-200): insert after the `options.Random` default (line 173) and before `lifetime, endLifetime := context.WithCancel(context.Background())` (line 174). By then `options.MaxWait` holds its 2-minute default:

```go
	// A pinned session waits for its account only as long as this
	// Dispatcher waits before answering with a RateLimitError.
	options.Manager.setPinWait(options.MaxWait)
```

`internal/accounts/dispatcher.go`, `UpdateConfig` (lines 202-244): lines 209-211 become

```go
	if cfg.MaxWaitBeforeErrorMs > 0 {
		dispatcher.maxWait = time.Duration(cfg.MaxWaitBeforeErrorMs) * time.Millisecond
		dispatcher.manager.setPinWait(dispatcher.maxWait)
	}
```

`UpdateConfig` holds `dispatcher.mu` when `setPinWait` takes `manager.mu`. That lock order is safe because the Manager never calls into the Dispatcher.

`internal/api/server.go`, in `messages()`, insert after line 890. That is right after the `messages is required and must be an array` check, so every later path (gateways, classifier reroutes, the Cloud Code `send`) sees the key:

```go
	// The account pool keeps a conversation on one account, keyed by the
	// same session id the request metrics use.
	request = request.WithContext(accounts.WithSessionKey(request.Context(), ccExtractSessionID(request, anthropicRequest)))
```

`internal/api/server.go` already imports `antigravity-go-proxy/internal/accounts` (line 29).

`internal/api/management.go`, replace lines 1001-1011 of `handleAccountsList` with:

```go
	status := server.accountManager.GetStatus()
	sessions := server.accountManager.ActiveSessions()
	if list, ok := status["accounts"].([]map[string]any); ok {
		for _, item := range list {
			email, _ := item["email"].(string)
			item["activeSessions"] = sessions[email]
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":   "ok",
		"accounts": status["accounts"],
		"summary": map[string]any{
			"total":       status["total"],
			"available":   status["available"],
			"rateLimited": status["rateLimited"],
			"invalid":     status["invalid"],
		},
	})
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/accounts -run 'TestStreamGenerateContentKeepsASessionOnItsAccount|TestStreamGenerateContentMovesAPinnedSessionPastMaxWait' -count=1`
Expected: PASS.

Run: `go test ./internal/api -run 'TestMessagesCarriesTheSessionKeyToTheBackend|TestAccountsListReportsActiveSessions' -count=1`
Expected: PASS.

Run: `go test -race ./internal/accounts ./internal/api -count=1`
Expected: PASS. Existing api tests that send `x-session-id` (`TestAntigravityObservability_Unary`, `TestAntigravityObservability_Streaming`) use a single account, so the pin does not change which account they hit.

Run: `gofmt -l internal/accounts internal/api`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/conversation.go internal/accounts/dispatcher.go internal/accounts/dispatcher_test.go internal/api/server.go internal/api/management.go internal/api/session_key_test.go internal/api/management_test.go
git commit -m "feat: carry the /v1/messages session id into account selection; report activeSessions"
```

---

## Part B: Saves and control-plane parity (spec §5): Tasks 4–7

Line numbers are from base commit `f8f6813`. Tasks 1–4 and earlier tasks of this section move some of them by a few lines, so find each spot by the code quoted next to it.

The order agy uses comes from `.reference/client-drift/agy.jsonl`: 38 records, agy 1.5.2, all `HTTP/1.1` to `daily-cloudcode-pa.googleapis.com`, JA4 `t13d131100_f57a46bbacb6_f50d94e863eb`. Each record below is named by its RPC, or by `requestType` for `streamGenerateContent`. A run ends at its last `agent` call. Records 0–20 and 21–37 are the two generation runs:

```text
run 1: loadCodeAssist, fetchAdminControls, fetchUserInfo, retrieveUserQuotaSummary, fetchAvailableModels,
       loadCodeAssist, fetchAdminControls, retrieveUserQuotaSummary, fetchUserInfo, fetchAvailableModels,
       retrieveUserQuotaSummary, listExperiments, loadCodeAssist, loadCodeAssist, fetchUserInfo,
       retrieveUserQuotaSummary, writeTrajectoryAcls, loadCodeAssist, retrieveUserQuotaSummary, agent, agent
run 2: loadCodeAssist, fetchAdminControls, fetchUserInfo, retrieveUserQuotaSummary, fetchAvailableModels,
       retrieveUserQuotaSummary, listExperiments, loadCodeAssist, cascadeNuxes, writeTrajectoryAcls,
       loadCodeAssist, fetchUserInfo, retrieveUserQuotaSummary, checkpoint, loadCodeAssist, agent, agent
```

`fetchUserInfo` and `retrieveUserQuotaSummary` show up in both orders in run 1 (records 2–3 and 7–8), so they are sent together. In both runs the second `agent` call carries two more `contents` entries than the first. [INFERENCE] Those two entries are the tool call and its result, so the second call continues the turn with a tool. `writeTrajectoryAcls` and `checkpoint` do not sit at the same place in the two runs. The proxy already sends them beside the first model call (`startConversation`, `dispatcher.go:573`), and these tasks do not change that.

---

### Task 4: Serialize `accounts.json` saves

From Task 7 on, every new prompt runs `loadCodeAssist`, whose result goes through `UpdateSubscription` and `CacheProject`; each calls `SaveToDisk` (`internal/accounts/manager.go:788`, `:814`). `SaveToDisk` takes its snapshot under a read lock and then calls `Save`, which writes `path + ".tmp"` and renames it (`manager.go:1289-1293`). Two concurrent saves can interleave writes to the same temp file (a torn `accounts.json`), and an older snapshot can be renamed over a newer one. The OpenRouter router had the same bug and fixed it with a mutex that serializes the whole save (`internal/openrouter/router.go:93-96`, test `router_test.go:402`). This task applies that fix here, before the control-plane calls make saves frequent.

**Files:**
- Modify: `internal/accounts/manager.go:233-249` (`Manager` gains `saveMu`)
- Modify: `internal/accounts/manager.go:1343-1359` (`SaveToDisk`)
- Test: `internal/accounts/save_test.go` (create)

**Interfaces:**
- Consumes: nothing new.
- Produces: `SaveToDisk` is safe to call concurrently: the file always parses, and the last save to finish carries every change made before any save started after it. Signature unchanged: `func (manager *Manager) SaveToDisk() error`.

- [ ] **Step 1: Write the failing test**

Create `internal/accounts/save_test.go`:

```go
package accounts

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Every UpdateSubscription saves. Concurrent saves must neither tear the file
// nor let an older snapshot land after a newer one: once all of them return,
// the file holds the tier the manager holds.
func TestConcurrentSavesKeepTheNewestSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	manager, err := New(Options{Accounts: []*Account{testAccount("a@example.com")}, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := range 200 {
		group.Go(func() {
			if err := manager.UpdateSubscription("a@example.com", Subscription{Tier: fmt.Sprintf("tier-%03d", i)}); err != nil {
				t.Errorf("UpdateSubscription: %v", err)
			}
		})
	}
	group.Wait()

	file, err := Load(path)
	if err != nil {
		t.Fatalf("load after concurrent saves: %v", err)
	}
	if len(file.Accounts) != 1 {
		t.Fatalf("saved %d accounts, want 1", len(file.Accounts))
	}
	want := manager.GetAllAccounts()[0].Subscription.Tier
	if got := file.Accounts[0].Subscription.Tier; got != want {
		t.Fatalf("saved tier %q, in-memory tier %q: an older snapshot landed last", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/accounts -run 'TestConcurrentSavesKeepTheNewestSnapshot' -count=20`
Expected: FAIL in at least one of the 20 runs, with `load after concurrent saves: ...` (a torn file) or `saved tier "tier-NNN", in-memory tier "tier-MMM"`. The race is timing-dependent; if all 20 pass, run with `-count=100` before moving on.

- [ ] **Step 3: Write minimal implementation**

In `internal/accounts/manager.go`, add to the `Manager` struct (after `modelThrottles`):

```go
	// saveMu serializes SaveToDisk from snapshot to rename: saves run on
	// request goroutines, and interleaved writes to path+".tmp" can persist a
	// torn file or let an older snapshot replace a newer one.
	saveMu sync.Mutex
```

Replace `SaveToDisk` (lines 1343-1359):

```go
func (manager *Manager) SaveToDisk() error {
	manager.saveMu.Lock()
	defer manager.saveMu.Unlock()
	manager.mu.RLock()
	path := manager.configPath
	if path == "" {
		manager.mu.RUnlock()
		return nil
	}
	accounts := make([]*Account, len(manager.accounts))
	for i, acc := range manager.accounts {
		accounts[i] = cloneAccount(acc)
	}
	settings := manager.settings
	activeIndex := manager.currentIndex
	manager.mu.RUnlock()

	return Save(path, accounts, settings, activeIndex)
}
```

`saveMu` is taken before `mu` and never while `mu` is held, so it cannot deadlock with callers that save after releasing `mu` (`CacheProject`, `UpdateSubscription` and the other mutators call `SaveToDisk` after `Unlock`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/accounts -run 'TestConcurrentSavesKeepTheNewestSnapshot' -count=50 -race`
Expected: PASS.

Run: `go test ./internal/accounts -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/save_test.go
git commit -m "fix(accounts): serialize accounts.json saves so concurrent writers cannot tear the file"
```

---

### Task 5: Cloud Code client — agy's control-plane calls and a GET path

**Files:**
- Modify: `internal/cloudcode/client.go:25-32` (path consts), `:298-331` (project-only bodies and the four new methods), `:341-384` (`DoJSON` and `doJSONAttempt` become `DoJSON`, `DoGet`, `do` and `doAttempt`), `:393` (DoSSE's `newRequest` call), `:430-443` (`newRequest` takes the method)
- Test: `internal/cloudcode/client_test.go:261-348` (`TestRequestHeadsOnTheWireMatchAgy`, extended)

**Interfaces:**
- Consumes: nothing new.
- Produces (package `cloudcode`):
  - `const PathFetchAdminControls = "/v1internal:fetchAdminControls"`, `PathFetchUserInfo = "/v1internal:fetchUserInfo"`, `PathListExperiments = "/v1internal:listExperiments"`, `PathListCascadeNuxes = "/v1internal/cascadeNuxes?page_size=1000"`
  - `func (c *Client) FetchAdminControls(ctx context.Context, projectID string) (Response, error)`
  - `func (c *Client) FetchUserInfo(ctx context.Context, projectID string) (Response, error)`
  - `func (c *Client) ListExperiments(ctx context.Context, projectID string) (Response, error)`
  - `func (c *Client) ListCascadeNuxes(ctx context.Context) (Response, error)`
  - `func (c *Client) DoGet(ctx context.Context, endpoints []string, path string) (Response, error)`, which sends a GET with no body and no `Content-Type`
  - Unexported: `projectRequest(projectID string) map[string]string`, `(*Client).do(ctx, method string, endpoints []string, path string, body []byte, options RequestOptions)`, `(*Client).doAttempt(ctx, method, endpoint, path string, body []byte, options RequestOptions)` (replaces `doJSONAttempt`), `(*Client).newRequest(ctx, method, endpoint, path string, body []byte, options RequestOptions)` (a nil body means no body and no Content-Type)

Wire ground truth: the header names of every record in `.reference/client-drift/agy.jsonl`, grouped by request (`request_bytes` in brackets):

```text
POST /v1internal:fetchAdminControls        [30]  Host, User-Agent, Content-Length, Authorization, Content-Type, Accept-Encoding
POST /v1internal:fetchUserInfo             [30]  Host, User-Agent, Content-Length, Authorization, Content-Type, Accept-Encoding
POST /v1internal:listExperiments           [30]  Host, User-Agent, Content-Length, Authorization, Content-Type, Accept-Encoding
POST /v1internal:loadCodeAssist            [38]  Host, User-Agent, Content-Length, Authorization, Content-Type, Accept-Encoding
GET  /v1internal/cascadeNuxes?page_size=1000 [0] Host, User-Agent, Authorization, Accept-Encoding
POST /v1internal:streamGenerateContent?alt=sse   Host, User-Agent, Transfer-Encoding, Authorization, Content-Type, Accept-Encoding
```

Each of the three POSTs has the body shape `{"project": string}`. 30 bytes is `{"project":"` plus a 16-character id plus `"}`. The GET has no `request_body`.

- [ ] **Step 1: Write the failing test.** Replace `TestRequestHeadsOnTheWireMatchAgy` (`client_test.go:261-348`) with:

```go
// TestRequestHeadsOnTheWireMatchAgy reads the request head as the production
// transport writes it, before a server could reorder or canonicalise it, and
// compares it with agy's. A unary call carries Content-Length and a streaming
// call is chunked, and Accept-Encoding comes last because net/http, not the
// client, adds it (.reference/agy-1.4.10-wire-20261030.txt). The control-plane
// calls agy 1.5.2 sends around a conversation are unary POSTs with body
// {"project":...}, except cascadeNuxes: a GET with no body and no Content-Type
// (.reference/client-drift/agy.jsonl).
func TestRequestHeadsOnTheWireMatchAgy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// The server reports through the channel, never through t: its goroutine
	// can outlive the test.
	type captured struct {
		line string
		head []string
		body string
		err  error
	}
	requests := make(chan captured, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var raw bytes.Buffer
				request, err := http.ReadRequest(bufio.NewReader(io.TeeReader(conn, &raw)))
				if err != nil {
					requests <- captured{err: err}
					return
				}
				body, _ := io.ReadAll(request.Body)
				head, _, _ := strings.Cut(raw.String(), "\r\n\r\n")
				lines := strings.Split(head, "\r\n")
				requests <- captured{line: lines[0], head: lines[1:], body: string(body)}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
			}(conn)
		}
	}()

	receive := func() captured {
		got := <-requests
		if got.err != nil {
			t.Fatalf("read request: %v", got.err)
		}
		return got
	}
	names := func(head []string) []string {
		var out []string
		for _, line := range head {
			name, _, _ := strings.Cut(line, ":")
			out = append(out, name)
		}
		return out
	}

	ctx := context.Background()
	client := New(Options{AccessToken: "token"})
	client.provisioningEndpoints = []string{"http://" + listener.Addr().String()}
	client.generationEndpoints = client.provisioningEndpoints
	client.contentEndpoints = client.provisioningEndpoints
	if _, err := client.LoadCodeAssist(ctx); err != nil {
		t.Fatal(err)
	}
	unary := receive()
	if _, err := client.StreamGenerateContent(ctx, map[string]string{"x": "y"}, RequestOptions{}, func(SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stream := receive()

	unaryOrder := []string{"Host", "User-Agent", "Content-Length", "Authorization", "Content-Type", "Accept-Encoding"}
	if got := names(unary.head); !reflect.DeepEqual(got, unaryOrder) {
		t.Errorf("unary header order = %v, want %v", got, unaryOrder)
	}
	if got, want := names(stream.head), []string{"Host", "User-Agent", "Transfer-Encoding", "Authorization", "Content-Type", "Accept-Encoding"}; !reflect.DeepEqual(got, want) {
		t.Errorf("streaming header order = %v, want %v", got, want)
	} else if stream.head[2] != "Transfer-Encoding: chunked" || stream.head[5] != "Accept-Encoding: gzip" {
		t.Errorf("streaming head = %q", stream.head)
	}
	if stream.body != `{"x":"y"}` {
		t.Errorf("streaming body = %q", stream.body)
	}

	for _, call := range []struct {
		line string
		send func() (Response, error)
	}{
		{"POST /v1internal:fetchAdminControls HTTP/1.1", func() (Response, error) { return client.FetchAdminControls(ctx, "p") }},
		{"POST /v1internal:fetchUserInfo HTTP/1.1", func() (Response, error) { return client.FetchUserInfo(ctx, "p") }},
		{"POST /v1internal:listExperiments HTTP/1.1", func() (Response, error) { return client.ListExperiments(ctx, "p") }},
	} {
		if _, err := call.send(); err != nil {
			t.Fatalf("%s: %v", call.line, err)
		}
		got := receive()
		if got.line != call.line {
			t.Errorf("request line = %q, want %q", got.line, call.line)
		}
		if order := names(got.head); !reflect.DeepEqual(order, unaryOrder) {
			t.Errorf("%s header order = %v, want %v", call.line, order, unaryOrder)
		}
		if want := `{"project":"p"}`; got.body != want {
			t.Errorf("%s body = %q, want %q", call.line, got.body, want)
		}
	}

	if _, err := client.ListCascadeNuxes(ctx); err != nil {
		t.Fatal(err)
	}
	nuxes := receive()
	if want := "GET /v1internal/cascadeNuxes?page_size=1000 HTTP/1.1"; nuxes.line != want {
		t.Errorf("request line = %q, want %q", nuxes.line, want)
	}
	if got, want := names(nuxes.head), []string{"Host", "User-Agent", "Authorization", "Accept-Encoding"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cascadeNuxes header order = %v, want %v", got, want)
	}
	if nuxes.body != "" {
		t.Errorf("cascadeNuxes body = %q, want none", nuxes.body)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails.**

Run: `go test ./internal/cloudcode -run 'TestRequestHeadsOnTheWireMatchAgy' -count=1`
Expected: build failure:
```text
client_test.go:...: client.FetchAdminControls undefined (type *Client has no field or method FetchAdminControls)
client_test.go:...: client.FetchUserInfo undefined (type *Client has no field or method FetchUserInfo)
client_test.go:...: client.ListExperiments undefined (type *Client has no field or method ListExperiments)
client_test.go:...: client.ListCascadeNuxes undefined (type *Client has no field or method ListCascadeNuxes)
```

- [ ] **Step 3: Write the minimal implementation** in `internal/cloudcode/client.go`.

(a) Path consts. Insert after `PathWriteTrajectoryAcls` (line 32), inside the same `const` block. gofmt keeps the existing alignment:

```go
	PathWriteTrajectoryAcls      = "/v1internal:writeTrajectoryAcls"
	PathFetchAdminControls       = "/v1internal:fetchAdminControls"
	PathFetchUserInfo            = "/v1internal:fetchUserInfo"
	PathListExperiments          = "/v1internal:listExperiments"
	PathListCascadeNuxes         = "/v1internal/cascadeNuxes?page_size=1000"
```

(b) Replace lines 298-331, from `FetchAvailableModelsAt` through `RetrieveUserQuota`, with the code below. The project-only body that was written out three times becomes `projectRequest`. It sends the same bytes, so the existing tests keep covering it. `FetchAvailableModels` (294-296) and `WriteTrajectoryAcls` (318-322) stay as they are:

```go
// FetchAvailableModelsAt is FetchAvailableModels against an explicit endpoint list.
func (c *Client) FetchAvailableModelsAt(ctx context.Context, endpoints []string, projectID string) (Response, error) {
	return c.DoJSON(ctx, endpoints, PathFetchAvailableModels, projectRequest(projectID), RequestOptions{})
}

// RetrieveUserQuotaSummary fetches the live per-bucket quota summary
// (remaining fractions + reset times). Unlike fetchAvailableModels'
// static quotaInfo, this RPC is the authoritative consumption signal.
func (c *Client) RetrieveUserQuotaSummary(ctx context.Context, projectID string) (Response, error) {
	return c.DoJSON(ctx, c.contentEndpoints, PathRetrieveUserQuotaSummary, projectRequest(projectID), RequestOptions{})
}

// WriteTrajectoryAcls is the call agy makes for a new conversation's
// trajectory, alongside its first model call.
func (c *Client) WriteTrajectoryAcls(ctx context.Context, trajectoryID string) (Response, error) {
	return c.DoJSON(ctx, c.contentEndpoints, PathWriteTrajectoryAcls, map[string]string{"trajectoryId": trajectoryID}, RequestOptions{})
}

// RetrieveUserQuota fetches per-model token-bucket quotas (model_id keyed).
func (c *Client) RetrieveUserQuota(ctx context.Context, projectID string) (Response, error) {
	return c.DoJSON(ctx, c.contentEndpoints, PathRetrieveUserQuota, projectRequest(projectID), RequestOptions{})
}

// FetchAdminControls, FetchUserInfo and ListExperiments are calls agy 1.5.2
// sends around a conversation, each with body {"project":"<id>"}
// (.reference/client-drift/agy.jsonl). The proxy sends them for parity and
// reads only their status.
func (c *Client) FetchAdminControls(ctx context.Context, projectID string) (Response, error) {
	return c.DoJSON(ctx, c.contentEndpoints, PathFetchAdminControls, projectRequest(projectID), RequestOptions{})
}

func (c *Client) FetchUserInfo(ctx context.Context, projectID string) (Response, error) {
	return c.DoJSON(ctx, c.contentEndpoints, PathFetchUserInfo, projectRequest(projectID), RequestOptions{})
}

func (c *Client) ListExperiments(ctx context.Context, projectID string) (Response, error) {
	return c.DoJSON(ctx, c.contentEndpoints, PathListExperiments, projectRequest(projectID), RequestOptions{})
}

// ListCascadeNuxes is the one GET agy sends to Cloud Code, with no body
// (.reference/client-drift/agy.jsonl).
func (c *Client) ListCascadeNuxes(ctx context.Context) (Response, error) {
	return c.DoGet(ctx, c.contentEndpoints, PathListCascadeNuxes)
}

// projectRequest is the body of the calls that name only the project; it is
// {} before the project is known.
func projectRequest(projectID string) map[string]string {
	request := map[string]string{}
	if projectID != "" {
		request["project"] = projectID
	}
	return request
}
```

(c) Replace lines 341-384 (`DoJSON`, `requestCreationError` and `doJSONAttempt`) with:

```go
func (c *Client) DoJSON(ctx context.Context, endpoints []string, path string, payload any, options RequestOptions) (Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("encode Cloud Code request: %w", err)
	}
	return c.do(ctx, http.MethodPost, endpoints, path, body, options)
}

// DoGet sends a GET with no body. Without a body it carries no Content-Type,
// so its head is Host, User-Agent, Authorization, Accept-Encoding, as agy's
// GET is (.reference/client-drift/agy.jsonl).
func (c *Client) DoGet(ctx context.Context, endpoints []string, path string) (Response, error) {
	return c.do(ctx, http.MethodGet, endpoints, path, nil, RequestOptions{})
}

func (c *Client) do(ctx context.Context, method string, endpoints []string, path string, body []byte, options RequestOptions) (Response, error) {
	var failures []error
	for _, endpoint := range endpoints {
		result, err := c.doAttempt(ctx, method, endpoint, path, body, options)
		if err == nil {
			return result, nil
		}
		var reqErr *requestCreationError
		if errors.As(err, &reqErr) {
			return Response{}, reqErr.err
		}
		failures = append(failures, err)
	}
	return Response{}, errors.Join(failures...)
}

type requestCreationError struct {
	err error
}

func (e *requestCreationError) Error() string {
	return e.err.Error()
}

func (c *Client) doAttempt(ctx context.Context, method, endpoint, path string, body []byte, options RequestOptions) (Response, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	request, err := c.newRequest(ctx, method, endpoint, path, body, options)
	if err != nil {
		return Response{}, &requestCreationError{err: err}
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Response{}, fmt.Errorf("Cloud Code request to %s: %w", endpoint, err)
	}
	return readResponse(endpoint, response)
}
```

(d) In `DoSSE`, line 393 becomes:

```go
		request, err := c.newRequest(ctx, http.MethodPost, endpoint, path, body, options)
```

(e) Replace `newRequest` (lines 430-443) with the version below. A GET with a nil body has `ContentLength` 0, and net/http writes no `Content-Length` for a GET. Deleting `Content-Type` therefore leaves Host, User-Agent, Authorization, and then the transport's Accept-Encoding. Transport and TLS are untouched.

```go
// newRequest builds a request with agy's headers. A nil body sends no body and
// no Content-Type.
func (c *Client) newRequest(ctx context.Context, method, endpoint, path string, body []byte, options RequestOptions) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create Cloud Code request: %w", err)
	}
	request.Header = c.defaultHeader.Clone()
	if body == nil {
		request.Header.Del("Content-Type")
	}
	for name, values := range options.Headers {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return request, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass.**

Run: `go test ./internal/cloudcode -run 'TestRequestHeadsOnTheWireMatchAgy' -count=1`
Expected: PASS.
Then: `go test -race ./internal/cloudcode -count=1` should PASS: the bodies of FetchAvailableModels and both quota calls are unchanged, and `TestTransportKeepsTLSZeroValueAndHTTP2Disabled` is untouched.

- [ ] **Step 5: Commit.**

```bash
git add internal/cloudcode/client.go internal/cloudcode/client_test.go
git commit -m "feat(cloudcode): send agy's control-plane calls, with a bodiless GET for cascadeNuxes"
```

---

### Task 6: Dispatcher — start burst and per-turn calls (`controlplane.go`)

**Files:**
- Create: `internal/accounts/controlplane.go`
- Create: `internal/accounts/controlplane_test.go`
- Modify: `internal/accounts/dispatcher.go:126-127` (new field after `modelsFetch`), `:197` (initialise it), `:916-920` (`RefreshAccount` uses `applyCodeAssist`), `:1146-1150` (`refreshLiveQuota` uses `applyQuotaSummary`)

**Interfaces:**
- Consumes:
  - Task 5: the four `*cloudcode.Client` methods, checked at compile time against `controlPlaneClient`.
  - Tasks 1–3: `func (manager *Manager) Select(session, model string) Selection`, which creates the session's pin; `func (manager *Manager) conversationStarted(session, email string) bool`; `func (manager *Manager) markConversationStarted(session, email string)`.
  - Existing code: `quotaSummaryFetcher` (`dispatcher.go:38`), `metered`, `cacheCatalog`, `updateAccountQuota`, `mergeQuotaReading`, `ExtractSubscription`, `Manager.CacheProject`, `Manager.UpdateSubscription`.
  - Test helpers already in the package: `testAccount` (`manager_test.go:341`), `staticResolver` (`retry_test.go:70`), `agyEvent` (`agy_conversation_test.go:68`).
- Produces (package `accounts`):
  - `const controlPlaneCallTimeout = 10 * time.Second`
  - `type controlPlaneClient interface { FetchAdminControls(context.Context, string) (cloudcode.Response, error); FetchUserInfo(context.Context, string) (cloudcode.Response, error); ListExperiments(context.Context, string) (cloudcode.Response, error); ListCascadeNuxes(context.Context) (cloudcode.Response, error) }`
  - `func (dispatcher *Dispatcher) beginConversation(ctx context.Context, session string, account *Account, client CloudClient, project string)`
  - `func (dispatcher *Dispatcher) beginTurn(ctx context.Context, session string, account *Account, client CloudClient, project string, request map[string]any)`
  - `func isNewUserPrompt(request map[string]any) bool`
  - `func (dispatcher *Dispatcher) shareControlCall(ctx context.Context, key string, work func())`, and the Dispatcher field `controlCalls map[string]chan struct{}` (guarded by `dispatcher.mu`)
  - `func (dispatcher *Dispatcher) applyCodeAssist(account *Account, body []byte) Subscription`, `func (dispatcher *Dispatcher) applyQuotaSummary(email string, body []byte)`
  - Test helpers for Task 7 (in `controlplane_test.go`): `agyStartBurst`, `agyTurn [][]string`, `sequenceModelsBody`, `sequenceModel`, `controlPlaneNow`, `type agyCallRecorder` with `recorded() []string`, `deadlines() []time.Duration`, `setFailure(name string, err error)` and `reset()`, `newControlPlaneDispatcher(t, clients map[string]*agyCallRecorder, emails ...string) *Dispatcher`, `assertSteps(t, calls []string, steps [][]string)`, `toolList() []any`

Decisions in this task:
- **What counts as a new prompt.** Spec §5.3 says "a `text` or `image` block, not only `tool_result`". `isNewUserPrompt` applies the rule `ccidentity.promptOf` already uses (`internal/ccidentity/turn.go:168-219`, built from the Claude Code captures). An empty text block or one that starts with `<system-reminder>` does not count, and an image or a document block does. Claude Code appends `<system-reminder>` text to its tool-result messages, so the literal spec rule would send the per-turn calls on tool continuations, which §5.3 forbids. The rule reads the decoded map, so a continuation's tool result is never re-encoded.
- **When a burst completes.** A burst marks the account started for the session once every one of its calls got an HTTP answer, whatever the status. A call counts as answered when `err == nil`, or when `cloudcode.FindHTTPError(err) != nil`. `DoJSON` drops the `Response` on a non-2xx answer, so the debug log reads that status from the `HTTPError`. Only a call with no answer leaves the account unstarted: a transport error, the 10 s timeout, or a request context that has ended (after which no further call is sent). The session's next request then sends the burst again. Without this rule, a call that always answers 4xx for some account would make every request of that session resend all nine calls. A call without an answer does not stop the calls after it, so the sequence stays agy's. Only a 2xx answer's body is applied (`loadCodeAssist`, `retrieveUserQuotaSummary`, `fetchAvailableModels`).
- **Concurrent groups** use `sync.WaitGroup.Go`, already used in `internal/claudecode/ccusage`. Every call goes through `dispatcher.metered`, as every upstream call does today.
- **Which client the calls use.** The control-plane calls use the `client` and `project` that `StreamGenerateContent` already holds. The burst's `fetchAvailableModels` feeds `cacheCatalog(email, body)` and `updateAccountQuota`, the same steps `fetchAvailableModelsForAccount` takes on success (`dispatcher.go:406-407`). It does not call that function, which would re-resolve credentials and mark pool state on a control-plane failure.
- **Known cost, not changed here.** `applyCodeAssist` keeps `RefreshAccount`'s writes: `CacheProject` and `UpdateSubscription` each save `accounts.json`, and the per-turn `loadCodeAssist` now runs them on every new prompt. `Save` writes `<path>.tmp` and then renames it (`manager.go:1289-1293`), so two concurrent saves can interleave in the temp file. That hazard already exists, and this task makes it more frequent.

- [ ] **Step 1: Write the failing tests.** Create `internal/accounts/controlplane_test.go`:

```go
package accounts

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/modelcatalog"
)

// agyStartBurst and agyTurn are agy's control-plane calls, named as
// .reference/client-drift/agy.jsonl names them. Each step holds calls agy
// sends together, which may arrive in any order.
var (
	agyStartBurst = [][]string{
		{"loadCodeAssist"},
		{"fetchAdminControls", "fetchUserInfo", "retrieveUserQuotaSummary"},
		{"fetchAvailableModels"},
		{"retrieveUserQuotaSummary"},
		{"listExperiments"},
		{"loadCodeAssist"},
		{"cascadeNuxes"},
	}
	agyTurn = [][]string{
		{"loadCodeAssist"},
		{"fetchUserInfo", "retrieveUserQuotaSummary"},
	}
)

// sequenceModelsBody is a catalog without a flash-lite tier, so a first turn
// sends no title checkpoint and the calls a client records are the control
// plane's and the model call's only.
const sequenceModelsBody = `{
	"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-low"]}]}],
	"models":{"gemini-3.8-flash-low":{"displayName":"Gemini 3.8 Flash (Low)","supportsThinking":true,"thinkingLevel":1,
		"maxTokens":1048576,"maxOutputTokens":65536,"model":"MODEL_PLACEHOLDER_M320","apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}}
}`

const sequenceModel = "gemini-3.8-flash-low"

var controlPlaneNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// agyCallRecorder is a CloudClient with every capability agy's control plane
// uses. It records each call under its agy.jsonl name (a generation under its
// requestType) and the time each control-plane call had before its deadline.
type agyCallRecorder struct {
	modelsBody   []byte
	codeAssist   []byte
	quotaSummary []byte
	// hold, when set, keeps every loadCodeAssist waiting, after it is
	// recorded, until hold is closed.
	hold chan struct{}

	mu       sync.Mutex
	failures map[string]error
	calls    []string
	budgets  []time.Duration
}

func (client *agyCallRecorder) answer(ctx context.Context, name string, body []byte) (cloudcode.Response, error) {
	client.mu.Lock()
	client.calls = append(client.calls, name)
	if deadline, ok := ctx.Deadline(); ok {
		client.budgets = append(client.budgets, time.Until(deadline))
	}
	err := client.failures[name]
	client.mu.Unlock()
	if err != nil {
		// As cloudcode.Client.DoJSON: no Response beside the error.
		return cloudcode.Response{}, err
	}
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	return cloudcode.Response{StatusCode: http.StatusOK, Body: body}, nil
}

func (client *agyCallRecorder) LoadCodeAssist(ctx context.Context) (cloudcode.Response, error) {
	response, err := client.answer(ctx, "loadCodeAssist", client.codeAssist)
	if client.hold != nil {
		<-client.hold
	}
	return response, err
}

func (client *agyCallRecorder) FetchAvailableModels(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "fetchAvailableModels", client.modelsBody)
}

func (client *agyCallRecorder) RetrieveUserQuotaSummary(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "retrieveUserQuotaSummary", client.quotaSummary)
}

func (client *agyCallRecorder) FetchAdminControls(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "fetchAdminControls", nil)
}

func (client *agyCallRecorder) FetchUserInfo(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "fetchUserInfo", nil)
}

func (client *agyCallRecorder) ListExperiments(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "listExperiments", nil)
}

func (client *agyCallRecorder) ListCascadeNuxes(ctx context.Context) (cloudcode.Response, error) {
	return client.answer(ctx, "cascadeNuxes", nil)
}

func (client *agyCallRecorder) StreamGenerateContent(_ context.Context, payload any, _ cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	client.mu.Lock()
	client.calls = append(client.calls, payload.(proxyformat.CloudCodeRequest).RequestType)
	client.mu.Unlock()
	if err := consume(cloudcode.SSEEvent{Data: agyEvent(1, "STOP")}); err != nil {
		return cloudcode.Response{StatusCode: http.StatusOK}, err
	}
	return cloudcode.Response{Endpoint: cloudcode.DailyEndpoint, StatusCode: http.StatusOK}, nil
}

func (client *agyCallRecorder) recorded() []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	return slices.Clone(client.calls)
}

func (client *agyCallRecorder) deadlines() []time.Duration {
	client.mu.Lock()
	defer client.mu.Unlock()
	return slices.Clone(client.budgets)
}

// setFailure makes every later call named name return err; a nil err makes
// it answer again.
func (client *agyCallRecorder) setFailure(name string, err error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.failures == nil {
		client.failures = make(map[string]error)
	}
	if err == nil {
		delete(client.failures, name)
		return
	}
	client.failures[name] = err
}

func (client *agyCallRecorder) reset() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.calls, client.budgets = nil, nil
}

// newControlPlaneDispatcher builds a sticky pool of emails, in that order,
// each served by its recorder, with the catalog already cached so that no
// request fetches it.
func newControlPlaneDispatcher(t *testing.T, clients map[string]*agyCallRecorder, emails ...string) *Dispatcher {
	t.Helper()
	pool := make([]*Account, 0, len(emails))
	tokens := make(map[string]string, len(emails))
	for _, email := range emails {
		pool = append(pool, testAccount(email))
		tokens[email] = "token-" + email
	}
	manager, err := New(Options{Accounts: pool, Strategy: StrategySticky, Now: func() time.Time { return controlPlaneNow }})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager: manager, Resolver: &staticResolver{tokens: tokens},
		Now:    func() time.Time { return controlPlaneNow },
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Random: func() float64 { return 0 },
		NewClient: func(token string) CloudClient {
			client := clients[strings.TrimPrefix(token, "token-")]
			if client == nil {
				t.Fatalf("unexpected token %q", token)
			}
			return client
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dispatcher.Close)
	catalog, err := modelcatalog.Parse([]byte(sequenceModelsBody))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.storeCatalog("", catalog)
	return dispatcher
}

// assertSteps checks calls against steps in order; the calls of one step may
// come in any order.
func assertSteps(t *testing.T, calls []string, steps [][]string) {
	t.Helper()
	rest := calls
	for index, step := range steps {
		if len(rest) < len(step) {
			t.Fatalf("calls %v end before step %d %v", calls, index, step)
		}
		got, want := slices.Clone(rest[:len(step)]), slices.Clone(step)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("step %d = %v, want %v (calls %v)", index, rest[:len(step)], step, calls)
		}
		rest = rest[len(step):]
	}
	if len(rest) > 0 {
		t.Fatalf("calls %v go on past the steps with %v", calls, rest)
	}
}

func toolList() []any {
	return []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}}
}

func TestIsNewUserPrompt(t *testing.T) {
	t.Parallel()
	user := func(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
	block := func(kind, text string) map[string]any { return map[string]any{"type": kind, "text": text} }
	toolResult := map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package login"}
	reminder := block("text", "<system-reminder>\nlogin.go is short.\n</system-reminder>")
	for _, test := range []struct {
		name     string
		tools    []any
		messages []any
		want     bool
	}{
		{"typed string", toolList(), []any{user("Fix the login bug")}, true},
		{"text block", toolList(), []any{user([]any{block("text", "Fix the login bug")})}, true},
		{"image block", toolList(), []any{user([]any{map[string]any{"type": "image", "source": map[string]any{}}})}, true},
		{"tool result with typed text", toolList(), []any{user([]any{toolResult, block("text", "stop, use the other file")})}, true},
		{"tool result", toolList(), []any{user([]any{toolResult})}, false},
		{"tool result with reminder", toolList(), []any{user([]any{toolResult, reminder})}, false},
		{"blank string", toolList(), []any{user("  ")}, false},
		{"no tools", nil, []any{user("Write a five-word title")}, false},
		{"assistant last", toolList(), []any{user("hi"), map[string]any{"role": "assistant", "content": "hello"}}, false},
		{"no messages", toolList(), nil, false},
	} {
		request := map[string]any{"messages": test.messages}
		if test.tools != nil {
			request["tools"] = test.tools
		}
		if got := isNewUserPrompt(request); got != test.want {
			t.Errorf("%s: isNewUserPrompt = %v, want %v", test.name, got, test.want)
		}
	}
}

// Concurrent requests of one session on one account share one start burst,
// sent in agy's order with a 10 s deadline on each call; the session's later
// requests send none, and another session gets its own.
func TestBeginConversationSendsTheStartBurstOncePerSessionAndAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const email = "a@example.com"
		client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody), hold: make(chan struct{})}
		dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
		account := dispatcher.manager.accounts[0]
		dispatcher.manager.Select("session-1", sequenceModel)

		var requests sync.WaitGroup
		for range 8 {
			requests.Go(func() {
				dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
			})
		}
		// Every request is blocked: one in the burst's first loadCodeAssist,
		// the others waiting for that burst.
		synctest.Wait()
		close(client.hold)
		requests.Wait()

		assertSteps(t, client.recorded(), agyStartBurst)
		for index, budget := range client.deadlines() {
			if budget != controlPlaneCallTimeout {
				t.Errorf("call %d had %s before its deadline, want %s", index, budget, controlPlaneCallTimeout)
			}
		}
		if !dispatcher.manager.conversationStarted("session-1", email) {
			t.Fatal("a burst whose calls all answered 2xx left the account unstarted")
		}

		client.reset()
		dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
		if calls := client.recorded(); len(calls) != 0 {
			t.Fatalf("a started session sent %v", calls)
		}
		dispatcher.manager.Select("session-2", sequenceModel)
		dispatcher.beginConversation(context.Background(), "session-2", account, client, "p")
		assertSteps(t, client.recorded(), agyStartBurst)
	})
}

// An error status is an answer: the burst completes and the account is
// started, so an account whose listExperiments always answers 403 does not
// resend all nine calls on each of the session's requests.
func TestBeginConversationCountsAnErrorStatusAsAnAnswer(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	client.setFailure("listExperiments", errors.Join(&cloudcode.HTTPError{
		StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`}))
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	dispatcher.manager.Select("session-1", sequenceModel)

	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	assertSteps(t, client.recorded(), agyStartBurst)
	if !dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("a burst whose calls all got an HTTP answer left the account unstarted")
	}

	client.reset()
	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("the session's next request sent %v", calls)
	}
}

// A call without an answer does not cut the burst short, but it leaves the
// account unstarted, so the session's next request sends the whole burst
// again.
func TestBeginConversationRetriesABurstWithAnUnansweredCall(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	client.setFailure("listExperiments", errors.New("Cloud Code request to https://daily-cloudcode-pa.googleapis.com: read: connection reset by peer"))
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	dispatcher.manager.Select("session-1", sequenceModel)

	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	assertSteps(t, client.recorded(), agyStartBurst)
	if dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("a burst with an unanswered call marked the account started")
	}

	client.reset()
	client.setFailure("listExperiments", nil)
	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	assertSteps(t, client.recorded(), agyStartBurst)
	if !dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("the resent burst got every answer and left the account unstarted")
	}
}

// The burst runs on the request's context: a client that has gone sends
// nothing, and the account stays unstarted.
func TestBeginConversationStopsWithTheClient(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	dispatcher.manager.Select("session-1", sequenceModel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dispatcher.beginConversation(ctx, "session-1", dispatcher.manager.accounts[0], client, "p")
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("a request whose client left sent %v", calls)
	}
	if dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("a burst that sent nothing marked the account started")
	}
}

// Only a new prompt in a session's main loop sends the turn calls: tool
// continuations, tool-less side calls and requests without a session send
// none.
func TestBeginTurnSendsTheTurnCallsBeforeANewPromptOnly(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	prompt := map[string]any{"tools": toolList(), "messages": []any{map[string]any{"role": "user", "content": "Fix the login bug"}}}
	continuation := map[string]any{"tools": toolList(), "messages": []any{
		map[string]any{"role": "user", "content": "Fix the login bug"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package login"}}},
	}}
	title := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Write a five-word title for: Fix the login bug"}}}

	for _, request := range []map[string]any{continuation, title} {
		dispatcher.beginTurn(context.Background(), "session-1", account, client, "p", request)
	}
	dispatcher.beginTurn(context.Background(), "", account, client, "p", prompt)
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("requests that are no new prompt in a session sent %v", calls)
	}
	dispatcher.beginTurn(context.Background(), "session-1", account, client, "p", prompt)
	assertSteps(t, client.recorded(), agyTurn)
}

// Identical prompts of one session that arrive together share one run of
// the turn calls.
func TestBeginTurnSharesOneRunAmongIdenticalPrompts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const email = "a@example.com"
		client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody), hold: make(chan struct{})}
		dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
		account := dispatcher.manager.accounts[0]
		prompt := map[string]any{"tools": toolList(), "messages": []any{map[string]any{"role": "user", "content": "Fix the login bug"}}}

		var requests sync.WaitGroup
		for range 4 {
			requests.Go(func() {
				dispatcher.beginTurn(context.Background(), "session-1", account, client, "p", prompt)
			})
		}
		synctest.Wait()
		close(client.hold)
		requests.Wait()
		assertSteps(t, client.recorded(), agyTurn)
	})
}

// loadCodeAssist answers set the account's tier and project, quota summaries
// its pools, and the burst's model list its catalog snapshot and model quota.
func TestControlPlaneAnswersUpdateTheAccount(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{
		modelsBody: []byte(sequenceModelsBody),
		codeAssist: []byte(`{"cloudaicompanionProject":"proj-live","currentTier":{"id":"standard-tier"},"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`),
		quotaSummary: []byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h",
			"displayName":"Five Hour Limit Remaining","remainingFraction":0.4,"resetTime":"2026-11-07T17:00:00Z","window":"5h"}]}]}`),
	}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	dispatcher.manager.Select("session-1", sequenceModel)

	dispatcher.beginConversation(context.Background(), "session-1", account, client, "project-a@example.com")

	if got := dispatcher.manager.Project(account); got != "proj-live" {
		t.Errorf("project = %q, want the one loadCodeAssist named", got)
	}
	if account.Subscription.Tier != "pro" {
		t.Errorf("tier = %q, want pro from the paid tier", account.Subscription.Tier)
	}
	if pool, ok := account.Quota.Pools["gemini-5h"]; !ok || pool.RemainingFraction == nil || *pool.RemainingFraction != 0.4 {
		t.Errorf("gemini-5h pool = %+v (present %v), want the summary's 0.4", pool, ok)
	}
	if _, ok := account.Quota.Models[sequenceModel]; !ok {
		t.Errorf("model quota = %+v, want the burst's model list", account.Quota.Models)
	}
	dispatcher.mu.RLock()
	_, snapshot := dispatcher.catalogs[email]
	dispatcher.mu.RUnlock()
	if !snapshot {
		t.Error("the burst's model list did not become the account's catalog snapshot")
	}
}
```

The two concurrency tests use `testing/synctest`, which is in the standard library since Go 1.25. `synctest.Wait` returns only once every request is durably blocked: the burst's leader on `hold`, the others on the shared call's `done` channel. Every interleaving is therefore exercised, with no sleep or poll. Inside the bubble the clock is fake, so each call's budget is exactly `controlPlaneCallTimeout`.

- [ ] **Step 2: Run the tests to verify they fail.**

Run: `go test ./internal/accounts -run 'TestIsNewUserPrompt|TestBeginConversation|TestBeginTurn|TestControlPlaneAnswers' -count=1`
Expected: build failure:
```text
controlplane_test.go:...: undefined: isNewUserPrompt
controlplane_test.go:...: dispatcher.beginConversation undefined (type *Dispatcher has no field or method beginConversation)
controlplane_test.go:...: undefined: controlPlaneCallTimeout
controlplane_test.go:...: dispatcher.beginTurn undefined (type *Dispatcher has no field or method beginTurn)
```

- [ ] **Step 3: Write the minimal implementation.**

(a) Create `internal/accounts/controlplane.go`:

```go
package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// controlPlaneCallTimeout bounds each control-plane call on its own: a slow
// call delays generation by at most this much, and a timeout only fails its
// run.
const controlPlaneCallTimeout = 10 * time.Second

// controlPlaneClient is the optional CloudClient capability for the calls agy
// 1.5.2 sends around a conversation whose answers the proxy does not read
// (.reference/client-drift/agy.jsonl). A client without it, such as a test
// fake, sends no control-plane calls.
type controlPlaneClient interface {
	FetchAdminControls(context.Context, string) (cloudcode.Response, error)
	FetchUserInfo(context.Context, string) (cloudcode.Response, error)
	ListExperiments(context.Context, string) (cloudcode.Response, error)
	ListCascadeNuxes(context.Context) (cloudcode.Response, error)
}

// The production client must have both capabilities: without one, the
// capability checks would skip those calls on every request, silently.
var (
	_ controlPlaneClient  = (*cloudcode.Client)(nil)
	_ quotaSummaryFetcher = (*cloudcode.Client)(nil)
)

// beginConversation sends agy's start burst the first time session uses
// account: loadCodeAssist; fetchAdminControls, fetchUserInfo and
// retrieveUserQuotaSummary at once; fetchAvailableModels;
// retrieveUserQuotaSummary; listExperiments; loadCodeAssist; cascadeNuxes
// (.reference/client-drift/agy.jsonl; cascadeNuxes is in its later run only).
// Concurrent requests of the session on the account share one burst. Once
// every call has an HTTP answer, whatever its status, the account is started
// for the session; a call without one (a transport error, a timeout, a client
// that has gone) leaves it unstarted, and the session's next request sends
// the burst again.
func (dispatcher *Dispatcher) beginConversation(ctx context.Context, session string, account *Account, client CloudClient, project string) {
	plane, capable := client.(controlPlaneClient)
	if session == "" || !capable || dispatcher.manager.conversationStarted(session, account.Email) {
		return
	}
	dispatcher.shareControlCall(ctx, "start\x00"+session+"\x00"+account.Email, func() {
		// A burst that ended between the check above and this run has
		// already marked the account.
		if dispatcher.manager.conversationStarted(session, account.Email) {
			return
		}
		run := &controlRun{dispatcher: dispatcher, ctx: ctx, account: account, client: client, project: project}
		run.loadCodeAssist()
		together(
			func() { run.withProject("fetchAdminControls", plane.FetchAdminControls) },
			func() { run.withProject("fetchUserInfo", plane.FetchUserInfo) },
			run.quotaSummary,
		)
		run.models()
		run.quotaSummary()
		run.withProject("listExperiments", plane.ListExperiments)
		run.loadCodeAssist()
		run.send("cascadeNuxes", plane.ListCascadeNuxes)
		if run.completed() {
			dispatcher.manager.markConversationStarted(session, account.Email)
		}
	})
}

// beginTurn sends what agy sends before each new prompt: loadCodeAssist, then
// fetchUserInfo and retrieveUserQuotaSummary at once
// (.reference/client-drift/agy.jsonl). Tool continuations and tool-less side
// calls (titles, topic checks) send nothing. Identical prompts of the session
// that arrive together share one run.
func (dispatcher *Dispatcher) beginTurn(ctx context.Context, session string, account *Account, client CloudClient, project string, request map[string]any) {
	plane, capable := client.(controlPlaneClient)
	if session == "" || !capable || !isNewUserPrompt(request) {
		return
	}
	messages, _ := request["messages"].([]any)
	// A decoded JSON body always encodes again.
	prompt, _ := json.Marshal(messages[len(messages)-1])
	digest := sha256.Sum256(prompt)
	dispatcher.shareControlCall(ctx, "turn\x00"+session+"\x00"+hex.EncodeToString(digest[:]), func() {
		run := &controlRun{dispatcher: dispatcher, ctx: ctx, account: account, client: client, project: project}
		run.loadCodeAssist()
		together(
			func() { run.withProject("fetchUserInfo", plane.FetchUserInfo) },
			run.quotaSummary,
		)
	})
}

// isNewUserPrompt reports whether request carries something a person sent to
// a main loop: it declares tools, which title and topic side calls do not,
// and its last message is a user message with more than tool results and
// injected <system-reminder> text. That is ccidentity's prompt rule (promptOf)
// read from the decoded body: an image or document block counts, an empty or
// reminder text block does not.
func isNewUserPrompt(request map[string]any) bool {
	if tools, _ := request["tools"].([]any); len(tools) == 0 {
		return false
	}
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		return false
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if role, _ := last["role"].(string); role != "user" {
		return false
	}
	switch content := last["content"].(type) {
	case string:
		return isPromptText(content)
	case []any:
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			switch kind, _ := block["type"].(string); kind {
			case "tool_result":
			case "text":
				if text, _ := block["text"].(string); isPromptText(text) {
					return true
				}
			default:
				return true
			}
		}
	}
	return false
}

func isPromptText(text string) bool {
	text = strings.TrimSpace(text)
	return text != "" && !strings.HasPrefix(text, "<system-reminder>")
}

// shareControlCall runs work once for key among the requests that ask for it
// at the same time; the others wait for it to end, or for their own context.
func (dispatcher *Dispatcher) shareControlCall(ctx context.Context, key string, work func()) {
	dispatcher.mu.Lock()
	if done, running := dispatcher.controlCalls[key]; running {
		dispatcher.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	dispatcher.controlCalls[key] = done
	dispatcher.mu.Unlock()
	defer func() {
		dispatcher.mu.Lock()
		delete(dispatcher.controlCalls, key)
		dispatcher.mu.Unlock()
		close(done)
	}()
	work()
}

// controlRun sends one burst's or one turn's calls for an account and notes
// whether any of them went without an answer.
type controlRun struct {
	dispatcher *Dispatcher
	ctx        context.Context
	account    *Account
	client     CloudClient
	project    string

	mu         sync.Mutex
	unanswered bool
}

// send makes one call under its own timeout, counted by the throttle meter,
// and returns the body of a 2xx answer. Any HTTP answer, an error status too,
// counts as answered: a call that always answers 4xx for an account must not
// make each of its requests resend the burst. A call without an answer (a
// transport error, a timeout, or a client that has gone, after which nothing
// is sent) marks the run unanswered. Each outcome is logged at debug.
func (run *controlRun) send(name string, call func(context.Context) (cloudcode.Response, error)) ([]byte, bool) {
	if run.ctx.Err() != nil {
		run.markUnanswered()
		return nil, false
	}
	ctx, cancel := context.WithTimeout(run.ctx, controlPlaneCallTimeout)
	defer cancel()
	response, err := run.dispatcher.metered(run.account.Email, func() (cloudcode.Response, error) {
		return call(ctx)
	})
	if err != nil {
		// DoJSON returns no Response with a non-2xx answer; its status is
		// on the HTTPError.
		if upstream := cloudcode.FindHTTPError(err); upstream != nil {
			slog.Debug("control-plane call", "call", name, "account", run.account.Email, "status", upstream.StatusCode)
			return nil, false
		}
		slog.Debug("control-plane call got no answer", "call", name, "account", run.account.Email, "error", err)
		run.markUnanswered()
		return nil, false
	}
	slog.Debug("control-plane call", "call", name, "account", run.account.Email, "status", response.StatusCode)
	return response.Body, response.StatusCode >= 200 && response.StatusCode < 300
}

// withProject sends a call that names the account's project.
func (run *controlRun) withProject(name string, call func(context.Context, string) (cloudcode.Response, error)) ([]byte, bool) {
	return run.send(name, func(ctx context.Context) (cloudcode.Response, error) {
		return call(ctx, run.project)
	})
}

func (run *controlRun) loadCodeAssist() {
	if body, ok := run.send("loadCodeAssist", run.client.LoadCodeAssist); ok {
		run.dispatcher.applyCodeAssist(run.account, body)
	}
}

func (run *controlRun) quotaSummary() {
	fetcher, ok := run.client.(quotaSummaryFetcher)
	if !ok {
		return
	}
	if body, ok := run.withProject("retrieveUserQuotaSummary", fetcher.RetrieveUserQuotaSummary); ok {
		run.dispatcher.applyQuotaSummary(run.account.Email, body)
	}
}

func (run *controlRun) models() {
	if body, ok := run.withProject("fetchAvailableModels", run.client.FetchAvailableModels); ok {
		run.dispatcher.cacheCatalog(run.account.Email, body)
		run.dispatcher.updateAccountQuota(run.account, body)
	}
}

func (run *controlRun) markUnanswered() {
	run.mu.Lock()
	run.unanswered = true
	run.mu.Unlock()
}

// completed reports whether every call of the run got an HTTP answer.
func (run *controlRun) completed() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return !run.unanswered
}

// together sends calls at once and waits for all of them: agy sends these
// calls concurrently, the capture holds them in both orders.
func together(calls ...func()) {
	var group sync.WaitGroup
	for _, call := range calls {
		group.Go(call)
	}
	group.Wait()
}

// applyCodeAssist records what a loadCodeAssist answer says about account:
// its tier and, when the answer names one, its Cloud Code project.
func (dispatcher *Dispatcher) applyCodeAssist(account *Account, body []byte) Subscription {
	sub := ExtractSubscription(body, dispatcher.now())
	if sub.ProjectID != "" {
		dispatcher.manager.CacheProject(account, sub.ProjectID)
	}
	_ = dispatcher.manager.UpdateSubscription(account.Email, sub)
	return sub
}

// applyQuotaSummary merges a retrieveUserQuotaSummary answer. Its buckets are
// shared pools (gemini-5h, 3p-weekly, ...), so they land in Quota.Pools and
// never surface as model rows.
func (dispatcher *Dispatcher) applyQuotaSummary(email string, body []byte) {
	for _, bucket := range cloudcode.ParseQuotaSummary(body) {
		dispatcher.mergeQuotaReading(email, strings.ToLower(strings.TrimSpace(bucket.ID)),
			bucket.RemainingFraction, bucket.RemainingAmount, bucket.ResetTime, true)
	}
}
```

(b) `internal/accounts/dispatcher.go`: add a Dispatcher field after `modelsFetch` (line 127). `liveQuotaRefreshMS` stays until Task 7:

```go
	// modelsFetch is the shared in-flight catalog fetch; guarded by mu.
	modelsFetch *modelFetchCall
	// controlCalls is the in-flight start burst or turn calls per key
	// (shareControlCall), guarded by mu; concurrent requests share one run.
	controlCalls map[string]chan struct{}
```

and initialise it in `NewDispatcher` after `clients:` (line 197):

```go
		clients:                  make(map[string]accountClient),
		controlCalls:             make(map[string]chan struct{}),
```

(c) `RefreshAccount`: lines 916-920

```go
	sub := ExtractSubscription(response.Body, dispatcher.now())
	if sub.ProjectID != "" {
		dispatcher.manager.CacheProject(targetAccount, sub.ProjectID)
	}
	_ = dispatcher.manager.UpdateSubscription(email, sub)
```

become

```go
	sub := dispatcher.applyCodeAssist(targetAccount, response.Body)
```

(d) `refreshLiveQuota`: lines 1146-1150

```go
			for _, bucket := range cloudcode.ParseQuotaSummary(response.Body) {
				dispatcher.mergeQuotaReading(account.Email,
					strings.ToLower(strings.TrimSpace(bucket.ID)),
					bucket.RemainingFraction, bucket.RemainingAmount, bucket.ResetTime, true)
			}
```

become

```go
			dispatcher.applyQuotaSummary(account.Email, response.Body)
```

- [ ] **Step 4: Run the tests to verify they pass.**

Run: `go test -race ./internal/accounts -run 'TestIsNewUserPrompt|TestBeginConversation|TestBeginTurn|TestControlPlaneAnswers' -count=3`
Expected: PASS, all 8 tests, 3 times each.
Then run `go test -race ./internal/accounts -count=1` and expect PASS. Nothing calls `beginConversation` or `beginTurn` yet. `TestRefreshLiveQuota_DisplayNamesDoNotDuplicateKeys` and `TestFetchAvailableModelsMergesLiveQuota` cover the `applyQuotaSummary` extraction.

- [ ] **Step 5: Commit.**

```bash
git add internal/accounts/controlplane.go internal/accounts/controlplane_test.go internal/accounts/dispatcher.go
git commit -m "feat(accounts): agy's start burst and per-turn calls, shared per session"
```

---

### Task 7: Send agy's sequence before generation and drop the post-stream quota call

**Files:**
- Create: `internal/accounts/agy_sequence_test.go`
- Modify: `internal/accounts/dispatcher.go`, base line numbers. Task 6 adds 3 lines after line 127 and 1 after line 197, and removes 4 each in `RefreshAccount` and `refreshLiveQuota`, so find each spot by its quoted code:
  - `:128-130`: delete the `liveQuotaRefreshMS` field
  - `:198`: delete its initialisation
  - `:376-377`: fix the comment on `fetchAvailableModelsForAccount`
  - `:470-476`: call the control plane after `resolveProject`
  - `:499`: change the `sendStream` call
  - `:1089-1127`: delete `liveQuotaRefreshInterval`, `recordLiveQuotaRefresh` and `refreshLiveQuotaThrottled`
  - `:1142`: delete the `recordLiveQuotaRefresh` call
- Modify: `internal/accounts/outputlimit.go:72-78` (sendStream doc and signature), `:91`, `:143` (settle calls), `:186-197` (`settle`)
- Modify: `internal/accounts/quota_merge_test.go:215-248`: `TestStreamGenerateContent_RefreshesLiveQuotaThrottled` pins the post-stream refresh and is replaced

**Interfaces:**
- Consumes:
  - Task 1: `func WithSessionKey(ctx context.Context, key string) context.Context` and `func SessionKey(ctx context.Context) string`. `server.messages()` puts the key on the request context; that change belongs to the affinity/api task.
  - Task 3: a pinned session whose target has a limit longer than `maxStickyWait` overflows to another account through the strategy.
  - Task 6: `beginConversation`, `beginTurn` and the test helpers `agyCallRecorder`, `newControlPlaneDispatcher`, `assertSteps`, `agyStartBurst`, `agyTurn`, `sequenceModel`, `sequenceModelsBody`, `toolList`.
- Produces:
  - `func (dispatcher *Dispatcher) settle(account *Account, credits []cloudcode.CreditBalance)`, without ctx, client or project.
  - `func (dispatcher *Dispatcher) sendStream(ctx context.Context, account *Account, client CloudClient, payload proxyformat.CloudCodeRequest, options cloudcode.RequestOptions, guard *proxyformat.OutputGuard, consume func(cloudcode.SSEEvent) error) (response cloudcode.Response, events int, err error)`, without project.
  - Deleted: `refreshLiveQuotaThrottled` (its only caller was `settle`, `outputlimit.go:196`); `recordLiveQuotaRefresh` (its only caller was `refreshLiveQuota`, `dispatcher.go:1142`); `liveQuotaRefreshInterval`; and the field `liveQuotaRefreshMS`, used only by those two. `refreshLiveQuota` stays for the catalog fetch path (`dispatcher.go:359`) and `RefreshAccount` (`:932`).

Notes:
- The sequence test reads `.reference/client-drift/agy.jsonl`, which is tracked in git. It splits the capture into the two runs quoted at the top of this section and checks that the spec's sequence appears in both. Each step's calls may come in any order, and agy's extra calls may sit between steps: repeated startup calls, `writeTrajectoryAcls`, the checkpoint, the second `loadCodeAssist` of run 2. `cascadeNuxes` must appear in run 2 only. The test then checks that the dispatcher sends exactly that sequence. If `client_drift.py` recaptures agy and the order has moved, the test fails.
- The test catalog has no flash-lite tier and the recorder has no `WriteTrajectoryAcls`, so `startConversation` sends nothing. Its background calls are covered by `TestDispatcherSendsAgysConversationStartOnce`.
- The calls run once per attempt of the `StreamGenerateContent` loop. After a failover, the new account's first request in the session gets its own burst and per-turn calls. That is spec §5.2's "first turn of a session on an account", and the overflow test checks it.

- [ ] **Step 1: Write the failing tests.**

(a) Create `internal/accounts/agy_sequence_test.go`:

```go
package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// agyGenerationRuns reads .reference/client-drift/agy.jsonl as call names,
// split into its generation runs: a run ends with its last agent call. A
// generation is named by its requestType, any other call by its RPC name.
// A recapture that changes agy's sequence fails the tests that read it.
func agyGenerationRuns(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".reference", "client-drift", "agy.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var runs [][]string
	var run []string
	for line := range strings.Lines(string(data)) {
		var record struct {
			Path        string `json:"path"`
			RequestBody struct {
				RequestType string `json:"requestType"`
			} `json:"request_body"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("agy.jsonl: %v", err)
		}
		name := record.RequestBody.RequestType
		if !strings.Contains(record.Path, "streamGenerateContent") {
			path, _, _ := strings.Cut(record.Path, "?")
			name = path[strings.LastIndexAny(path, ":/")+1:]
		}
		if len(run) > 0 && run[len(run)-1] == "agent" && name != "agent" {
			runs = append(runs, run)
			run = nil
		}
		run = append(run, name)
	}
	return append(runs, run)
}

// containsSteps reports whether run holds steps in order, the calls of each
// step in any order, with other calls of agy's between them.
func containsSteps(run []string, steps [][]string) bool {
	position := 0
	for _, step := range steps {
		end := position
		for _, call := range step {
			index := slices.Index(run[position:], call)
			if index < 0 {
				return false
			}
			end = max(end, position+index+1)
		}
		position = end
	}
	return true
}

func withoutCall(steps [][]string, name string) [][]string {
	var out [][]string
	for _, step := range steps {
		if kept := slices.DeleteFunc(slices.Clone(step), func(call string) bool { return call == name }); len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

// promptRequest is a first prompt of Claude Code's main loop: typed text and
// the tool list every main-loop request declares.
func promptRequest(text string) map[string]any {
	return map[string]any{
		"model": sequenceModel, "max_tokens": float64(1024), "tools": toolList(),
		"messages": []any{map[string]any{"role": "user", "content": text}},
	}
}

// toolContinuation is request answered with a tool call and followed by the
// tool's result, with the reminder block Claude Code appends to it.
func toolContinuation(request map[string]any) map[string]any {
	next := cloneRequest(request)
	next["messages"] = append(slices.Clone(request["messages"].([]any)),
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "login.go"}}}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package login"},
			map[string]any{"type": "text", "text": "<system-reminder>\nlogin.go is short.\n</system-reminder>"},
		}},
	)
	return next
}

// followUp is request answered with text and followed by a new prompt.
func followUp(request map[string]any, text string) map[string]any {
	next := cloneRequest(request)
	next["messages"] = append(slices.Clone(request["messages"].([]any)),
		map[string]any{"role": "assistant", "content": "Done."},
		map[string]any{"role": "user", "content": text},
	)
	return next
}

func sendTurn(t *testing.T, dispatcher *Dispatcher, session string, request map[string]any) {
	t.Helper()
	ctx := context.Background()
	if session != "" {
		ctx = WithSessionKey(ctx, session)
	}
	if _, err := dispatcher.StreamGenerateContent(ctx, request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// A session's first prompt sends agy's start burst, the turn calls and the
// model call, and the tool continuation after it only the model call: what
// both generation runs of agy.jsonl show, with agy's own extra calls between
// them (repeated startup calls, writeTrajectoryAcls, the title checkpoint).
// cascadeNuxes is in the later run only.
func TestDispatcherFollowsAgysCallSequence(t *testing.T) {
	t.Parallel()
	want := slices.Concat(agyStartBurst, agyTurn, [][]string{{"agent"}, {"agent"}})
	runs := agyGenerationRuns(t)
	if len(runs) != 2 {
		t.Fatalf("agy.jsonl holds %d generation runs, want 2: %v", len(runs), runs)
	}
	if !containsSteps(runs[0], withoutCall(want, "cascadeNuxes")) {
		t.Fatalf("agy's first run %v does not hold %v", runs[0], want)
	}
	if !containsSteps(runs[1], want) {
		t.Fatalf("agy's second run %v does not hold %v", runs[1], want)
	}

	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	sendTurn(t, dispatcher, "session-1", toolContinuation(first))
	assertSteps(t, client.recorded(), want)
}

// After the first turn, only a new prompt sends the turn calls: a tool-less
// side call, a tool continuation and a request without a session send only
// the model call.
func TestDispatcherSendsTurnCallsOnlyBeforeANewPrompt(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	client.reset()

	title := map[string]any{"model": sequenceModel, "max_tokens": float64(64),
		"messages": []any{map[string]any{"role": "user", "content": "Write a five-word title for: Fix the login bug"}}}
	second := followUp(toolContinuation(first), "Now add a test")
	sendTurn(t, dispatcher, "session-1", title)
	sendTurn(t, dispatcher, "session-1", toolContinuation(first))
	sendTurn(t, dispatcher, "session-1", second)
	sendTurn(t, dispatcher, "", followUp(second, "And a changelog entry"))

	assertSteps(t, client.recorded(), slices.Concat([][]string{{"agent"}, {"agent"}}, agyTurn, [][]string{{"agent"}, {"agent"}}))
}

// The start burst is per session and account: a second session on the same
// account gets its own, and so does the account a session overflows to.
func TestDispatcherSendsTheStartBurstPerSessionAndAccount(t *testing.T) {
	t.Parallel()
	home := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	overflow := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{"a@example.com": home, "b@example.com": overflow},
		"a@example.com", "b@example.com")
	firstTurn := slices.Concat(agyStartBurst, agyTurn, [][]string{{"agent"}})

	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	sendTurn(t, dispatcher, "session-2", promptRequest("Explain the build"))
	assertSteps(t, home.recorded(), slices.Concat(firstTurn, firstTurn))

	// A limit longer than maxStickyWait moves the session's model to the
	// other account.
	home.reset()
	dispatcher.manager.MarkRateLimited(dispatcher.manager.accounts[0], sequenceModel, time.Hour)
	sendTurn(t, dispatcher, "session-1", followUp(first, "Now add a test"))
	if calls := home.recorded(); len(calls) != 0 {
		t.Fatalf("the rate-limited home account got %v", calls)
	}
	assertSteps(t, overflow.recorded(), firstTurn)
}
```

(b) In `internal/accounts/quota_merge_test.go`, replace `TestStreamGenerateContent_RefreshesLiveQuotaThrottled` (lines 215-248). Keep `countingSummaryClient` (205-213):

```go
// The live quota reading is taken before a turn (beginTurn), where agy takes
// it; a stream that ends takes none.
func TestStreamGenerateContentTakesNoQuotaReadingAfterTheStream(t *testing.T) {
	client := &countingSummaryClient{}
	dispatcher := newDispatcherWithClient(t, client)

	// Warm the catalog without touching the quota path, so a summary fetch
	// below could only come from the end of a stream.
	response, err := client.FetchAvailableModels(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := modelcatalog.Parse(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.storeCatalog("", catalog)

	consume := func(cloudcode.SSEEvent) error { return nil }
	for range 2 {
		if _, err := dispatcher.StreamGenerateContent(context.Background(),
			map[string]any{"model": "gemini-3.8-flash-high"}, consume); err != nil {
			t.Fatal(err)
		}
	}
	if client.summaryCalls != 0 {
		t.Fatalf("finished streams fetched the quota summary %d times, want none", client.summaryCalls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail.**

Run: `go test ./internal/accounts -run 'TestDispatcherFollowsAgysCallSequence|TestDispatcherSendsTurnCallsOnlyBeforeANewPrompt|TestDispatcherSendsTheStartBurstPerSessionAndAccount|TestStreamGenerateContentTakesNoQuotaReadingAfterTheStream' -count=1`
Expected FAIL. The capture checks pass; the dispatcher checks fail:
```text
--- FAIL: TestStreamGenerateContentTakesNoQuotaReadingAfterTheStream
    quota_merge_test.go:...: finished streams fetched the quota summary 1 times, want none
--- FAIL: TestDispatcherSendsTheStartBurstPerSessionAndAccount
    agy_sequence_test.go:...: step 0 = [agent], want [loadCodeAssist] (calls [agent retrieveUserQuotaSummary agent])
--- FAIL: TestDispatcherSendsTurnCallsOnlyBeforeANewPrompt
    agy_sequence_test.go:...: step 2 = [agent], want [loadCodeAssist] (calls [agent agent agent agent])
--- FAIL: TestDispatcherFollowsAgysCallSequence
    agy_sequence_test.go:...: step 0 = [agent], want [loadCodeAssist] (calls [agent retrieveUserQuotaSummary agent])
```

- [ ] **Step 3: Write the minimal implementation.**

(a) `dispatcher.go`, `StreamGenerateContent`: call the control plane after the `resolveProject` error block (base lines 470-475) and before `BuildCloudCodeRequestWithModel`, which comes before `startConversation`:

```go
		client := dispatcher.client(account, credentials.AccessToken)
		project, err := dispatcher.resolveProject(ctx, account, client)
		if err != nil {
			dispatcher.manager.MarkFailure(account, model)
			lastError = err
			continue
		}
		dispatcher.beginConversation(ctx, SessionKey(ctx), account, client, project)
		dispatcher.beginTurn(ctx, SessionKey(ctx), account, client, project, request)
		payload := dispatcher.builder.BuildCloudCodeRequestWithModel(request, project, credentials.Email, proxyformat.ModelOptions{
```

and at the send (base 499-500):

```go
			response, eventCount, requestErr := dispatcher.sendStream(ctx, account, client,
				dispatcher.builder.Stamp(payload), options, guard, consume)
```

(b) `dispatcher.go`: delete the field (base 128-130)

```go
	// liveQuotaRefreshMS is the last live-quota refresh per account email,
	// guarded by mu; it throttles post-request quota RPCs.
	liveQuotaRefreshMS map[string]int64
```

and its initialisation in `NewDispatcher` (base 198):

```go
		liveQuotaRefreshMS:       make(map[string]int64),
```

(c) `dispatcher.go`: in the comment on `fetchAvailableModelsForAccount` (base 376-377), replace

```go
// failed, so recovery tries it last next time. No live-quota RPCs ride along:
// recovery sits on a request path, and the post-request refresh covers them.
```

with

```go
// failed, so recovery tries it last next time. No live-quota RPCs ride along:
// recovery sits on a request path, and the quota summary a new prompt sends
// before generation (beginTurn) covers them.
```

(d) `dispatcher.go`: delete base lines 1089-1127, which hold `liveQuotaRefreshInterval` with its comment, `recordLiveQuotaRefresh` with its comment and `refreshLiveQuotaThrottled` with its comment. Then, in `refreshLiveQuota`, delete line 1142:

```go
	dispatcher.recordLiveQuotaRefresh(account.Email)
```

(e) `outputlimit.go`: the end of the `sendStream` doc and its signature (base 72-78) become

```go
// read keeps the account's in-flight count until upstream ends, because
// upstream still serves the request, and records the credits it reports at
// the end. consume then runs on the reader's goroutine, never after
// sendStream returns.
func (dispatcher *Dispatcher) sendStream(ctx context.Context, account *Account, client CloudClient,
	payload proxyformat.CloudCodeRequest, options cloudcode.RequestOptions, guard *proxyformat.OutputGuard,
	consume func(cloudcode.SSEEvent) error) (response cloudcode.Response, events int, err error) {
```

Both settle calls (base 91 and 143) become

```go
			dispatcher.settle(account, credits)
```

and `settle` (base 186-197) becomes

```go
// settle records the remaining credits a stream that ended cleanly reported.
// The live quota reading is taken before a turn instead, where agy takes it
// (beginTurn).
func (dispatcher *Dispatcher) settle(account *Account, credits []cloudcode.CreditBalance) {
	if len(credits) == 0 {
		return
	}
	balances := make(map[string]int64, len(credits))
	for _, balance := range credits {
		balances[balance.CreditType] = balance.Amount
	}
	dispatcher.manager.UpdateAccountCredits(account.Email, balances)
}
```

- [ ] **Step 4: Run the tests to verify they pass.**

Run: `go test ./internal/accounts -run 'TestDispatcherFollowsAgysCallSequence|TestDispatcherSendsTurnCallsOnlyBeforeANewPrompt|TestDispatcherSendsTheStartBurstPerSessionAndAccount|TestStreamGenerateContentTakesNoQuotaReadingAfterTheStream' -count=1`
Expected: PASS.
Then: `go test -race ./internal/accounts ./internal/cloudcode ./internal/api -count=1`. Expected: PASS. The `internal/api` fakes have no `controlPlaneClient`, so their streams send no control-plane calls.
Then: `go vet ./internal/accounts`, and `gofmt -l internal/accounts` should print nothing.

- [ ] **Step 5: Commit.**

```bash
git add internal/accounts/dispatcher.go internal/accounts/outputlimit.go internal/accounts/quota_merge_test.go internal/accounts/agy_sequence_test.go
git commit -m "feat(accounts): send agy's control-plane sequence before generation, not a quota call after it"
```

---

## Part C: Token estimator (spec §6.3): Tasks 8–9

`internal/tokencount` is a new, self-contained package: no other package imports it until Task 10 wires it into `internal/api`. Every code block below was compiled and run (`go vet`, `go test -race`) in a scratch copy of the module before this plan was written; the expected numbers in the tests are the observed ones.

Choices these tasks make that the spec text does not spell out (the spec was updated to match on 2026-11-07):

- **Image rule (decided 2026-11-07; spec §6.3 updated):** Anthropic's published rule is now ⌈w/28⌉ × ⌈h/28⌉ visual tokens after downscaling to the model tier (standard: 1,568 px long edge / 1,568 tokens; Claude 4.7 and later: 2,576 px / 4,784 tokens), not ⌈w×h/750⌉ capped at 1,600. Sources: <https://platform.claude.com/docs/en/build-with-claude/vision#evaluate-image-size> ("Resolution and token cost", table) and <https://platform.claude.com/docs/en/build-with-claude/vision-coordinates#resize-your-image-before-uploading> (reference resize, round half to even). The tier comes from the Claude version in the model id. gpt-oss, Zen models and every other non-Gemini family use the standard tier. The Gemini rule is the spec's, confirmed at <https://ai.google.dev/gemini-api/docs/tokens> ("Images ≤384 pixels in both dimensions count as 258 tokens. Larger images are tiled into 768x768 pixel tiles, each counting as 258 tokens.").
- **Unreadable image source** (URL, file id, WebP, bad base64): the tier's visual-token budget for Claude-rule families (1,568 or 4,784, the most the tier can charge); a flat 1,600 for Gemini, which has no cap.
- **Digests ignore `cache_control` at any depth.** Claude Code moves its cache breakpoint to the newest message every turn, so a byte-exact digest of the earlier messages would never match the previous turn. A marker costs no tokens. The repo already strips markers for the same reason in `ccidentity.digestPrompt` (`internal/ccidentity/turn.go:89-111`).
- `Observe` takes `model`, not `family` (shared contract), and derives the family itself. `realInput <= 0` (a reply without usage) teaches neither the ratio nor the memory. `Estimate` reads the memory but does not extend its idle TTL; only `Observe` does.
- Characters are Unicode code points. Compact JSON is measured with HTML escaping off, so `<` counts 1, not 6.
- Limitation: Claude Code's own count bodies (`/context`) are per-component, with no `system` (spec §6.2), so they take the full-estimate path. The memory pays off for clients that count a whole conversation before sending it.

---

### Task 8: Token estimator: family ratio, character measurement, image pricing

**Files:**
- Create: `internal/tokencount/counter.go`
- Create: `internal/tokencount/measure.go`
- Create: `internal/tokencount/image.go`
- Test: `internal/tokencount/counter_test.go`, `internal/tokencount/measure_test.go`, `internal/tokencount/image_test.go`

**Interfaces:**
- Consumes: nothing; standard library only (`encoding/json`, `encoding/base64`, `image`, `image/gif`, `image/jpeg`, `image/png`, `math`, `strings`, `sync`, `time`, `unicode/utf8`).
- Produces (Task 9 and Task 10+ rely on these exact names):
  - `func Family(model string) string`: `"claude"` if the id contains `claude`, else `"gemini"` if it contains `gemini`, else `"gpt-oss"` if it contains `gpt-oss`, else the id itself.
  - `type Counter struct { ... }` and `func New(now func() time.Time) *Counter`. A nil `now` means `time.Now`. Pass the server's clock.
  - `func (c *Counter) Observe(session, model string, request map[string]any, realInput int)`. `model` is the model id the request was served with. `request` is the decoded Anthropic Messages body (`system`, `messages`, `tools`); it is read, never modified or retained. **`realInput` is computed by the caller** as `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` of the converted Anthropic usage (spec §6.3). Values ≤ 0 are ignored. In this task `session` is accepted and unused; Task 9 gives it meaning.
  - `func (c *Counter) Estimate(session, model string, request map[string]any) int`: ⌈measured chars ÷ family ratio⌉ + image tokens.
  - Unexported, used by Task 9: `type measurement struct { chars, imageTokens int }`, `func (m measurement) tokens(ratio float64) int`, `func measureRequest(pricing imagePricing, request map[string]any) measurement`, `func (m *measurement) addMessages(pricing imagePricing, messages []any)`, `type imagePricing`, `func pricingFor(model string) imagePricing`, `func (c *Counter) ratioLocked(family string) float64`, `func (c *Counter) learnLocked(family string, measured measurement, realInput int)`.
  - Test helpers Task 9 reuses (same package): `decodeRequest(t, body string) map[string]any`, `encodedImage(t, format string, width, height int) string`, `base64ImageBlock(mediaType, data string) map[string]any`, `textRequest(chars int) map[string]any`.

- [ ] **Step 1: Write the failing tests**

`internal/tokencount/measure_test.go`:

```go
package tokencount

import (
	"encoding/json"
	"testing"
)

func decodeRequest(t *testing.T, body string) map[string]any {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return request
}

func TestFamily(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-4-6-thinking": "claude",
		"claude-sonnet-5-5":        "claude",
		"gemini-3.8-flash-high":    "gemini",
		"gemini-3.1-pro":           "gemini",
		"gpt-oss-120b-medium":      "gpt-oss",
		"mimo-v2.6-flash-free":     "mimo-v2.6-flash-free",
		"kimi-k2.6":                "kimi-k2.6",
	} {
		if got := Family(model); got != want {
			t.Errorf("Family(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestMeasureRequestCountsEveryTextSource(t *testing.T) {
	request := decodeRequest(t, `{
		"model": "claude-sonnet-4-6",
		"system": [{"type": "text", "text": "abcd", "cache_control": {"type": "ephemeral"}}],
		"messages": [
			{"role": "user", "content": "héllo"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "think", "signature": "c2lnbmF0dXJl"},
				{"type": "redacted_thinking", "data": "b3BhcXVl"},
				{"type": "text", "text": "ok"},
				{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": {"path": "a<b"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "out"},
				{"type": "tool_result", "tool_use_id": "toolu_2", "content": [{"type": "text", "text": "xy"}]},
				{"type": "text", "text": "z", "cache_control": {"type": "ephemeral"}}
			]}
		],
		"tools": [{"name": "f", "input_schema": {"type": "object"}}]
	}`)

	got := measureRequest(pricingFor("claude-sonnet-4-6"), request)

	// system 4, "héllo" 5 runes (6 bytes), thinking 5, text 2,
	// tool_use input {"path":"a<b"} 14 (19 if < were HTML-escaped),
	// tool_result string 3, tool_result block 2, text 1,
	// tool {"input_schema":{"type":"object"},"name":"f"} 45.
	want := measurement{chars: 4 + 5 + 5 + 2 + 14 + 3 + 2 + 1 + 45}
	if got != want {
		t.Fatalf("measureRequest = %+v, want %+v", got, want)
	}
}

func TestMeasureRequestReadsAStringSystem(t *testing.T) {
	request := decodeRequest(t, `{"system": "abc", "messages": [{"role": "user", "content": "hi"}]}`)
	if got := measureRequest(pricingFor("claude-sonnet-4-6"), request); got != (measurement{chars: 5}) {
		t.Fatalf("measureRequest = %+v, want 5 chars", got)
	}
}
```

`internal/tokencount/image_test.go`:

```go
package tokencount

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

// The rows of the table at
// https://platform.claude.com/docs/en/build-with-claude/vision#evaluate-image-size,
// the A4 scan of
// https://platform.claude.com/docs/en/build-with-claude/vision-coordinates#how-claude-resizes-and-pads-images
// (resized to 924×1307 on the standard tier, 39 × 55 = 2,145 on high
// resolution), a portrait screenshot that must resize like its landscape
// twin, and a 2690×1793 image whose short edge lands on a tie: 1345 / (2690 /
// 1793) = 896.5 rounds to the even 896 as the live API does, giving 1345×896 =
// 49 × 32 = 1,568; rounding the tie up would settle on 1344×896 = 1,536.
func TestClaudeImageTokensFollowAnthropicsTable(t *testing.T) {
	standard := pricingFor("claude-opus-4-6-thinking")
	highRes := pricingFor("claude-opus-4-7")
	for _, tc := range []struct {
		width, height     int
		standard, highRes int
	}{
		{200, 200, 64, 64},
		{1000, 1000, 1296, 1296},
		{1092, 1092, 1521, 1521},
		{1920, 1080, 1560, 2691},
		{2000, 1500, 1564, 3888},
		{3840, 2160, 1560, 4784},
		{1075, 1520, 1551, 2145},
		{1080, 1920, 1560, 2691},
		{2690, 1793, 1568, 4760},
	} {
		if got := standard.tokens(tc.width, tc.height); got != tc.standard {
			t.Errorf("standard tier %dx%d = %d tokens, want %d", tc.width, tc.height, got, tc.standard)
		}
		if got := highRes.tokens(tc.width, tc.height); got != tc.highRes {
			t.Errorf("high-resolution tier %dx%d = %d tokens, want %d", tc.width, tc.height, got, tc.highRes)
		}
	}
}

func TestClaudeResolutionTierComesFromTheModelVersion(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-4-7":            true,
		"claude-sonnet-5-5":          true,
		"claude-sonnet-5":            true,
		"claude-sonnet-5.5-high":     true,
		"claude-fable-5-1":           true,
		"claude-opus-4-6-thinking":   false,
		"claude-haiku-4-5-20251001":  false,
		"claude-opus-4-20250514":     false,
		"claude-3-7-sonnet-20250219": false,
		"claude-custom-model":        false,
	} {
		if got := claudeHighResolution(model); got != want {
			t.Errorf("claudeHighResolution(%q) = %v, want %v", model, got, want)
		}
	}
	// Only Claude has a high-resolution tier; a version number in another
	// family's id does not reach it.
	if got := pricingFor("gpt-oss-120b-medium").tokens(1920, 1080); got != 1560 {
		t.Errorf("gpt-oss 1920x1080 = %d tokens, want the standard tier's 1560", got)
	}
}

// https://ai.google.dev/gemini-api/docs/tokens: images with both sides at
// most 384 px count 258 tokens; larger ones 258 per 768×768 tile.
func TestGeminiImageTokensCountTiles(t *testing.T) {
	pricing := pricingFor("gemini-3.1-pro-high")
	for _, tc := range []struct{ width, height, want int }{
		{384, 384, 258},
		{385, 100, 258},
		{768, 768, 258},
		{769, 768, 516},
		{1920, 1080, 1548},
		{100, 2000, 774},
	} {
		if got := pricing.tokens(tc.width, tc.height); got != tc.want {
			t.Errorf("gemini %dx%d = %d tokens, want %d", tc.width, tc.height, got, tc.want)
		}
	}
}

func encodedImage(t *testing.T, format string, width, height int) string {
	t.Helper()
	picture := image.NewGray(image.Rect(0, 0, width, height))
	var encoded bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&encoded, picture)
	case "jpeg":
		err = jpeg.Encode(&encoded, picture, nil)
	case "gif":
		err = gif.Encode(&encoded, picture, nil)
	}
	if err != nil {
		t.Fatalf("encode %s: %v", format, err)
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func base64ImageBlock(mediaType, data string) map[string]any {
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
	}
}

func TestImageBlocksArePricedFromTheirHeader(t *testing.T) {
	readable := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			base64ImageBlock("image/png", encodedImage(t, "png", 200, 200)),
			map[string]any{"type": "text", "text": "look"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
				base64ImageBlock("image/jpeg", encodedImage(t, "jpeg", 56, 28)),
				base64ImageBlock("image/gif", encodedImage(t, "gif", 29, 28)),
			}},
		}},
	}}
	unreadable := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			base64ImageBlock("image/webp", "UklGRiQAAABXRUJQVlA4IBgAAAAwAQCdASoBAAEAAwA0JaQAA3AA/vuUAAA="),
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
		}},
	}}

	for _, tc := range []struct {
		model            string
		readable, unread int
	}{
		// PNG 200×200 = 8×8 patches, JPEG 56×28 = 2×1, GIF 29×28 = 2×1.
		{"claude-sonnet-4-6", 64 + 2 + 2, 2 * 1568},
		{"claude-opus-4-7", 64 + 2 + 2, 2 * 4784},
		{"gpt-oss-120b-medium", 64 + 2 + 2, 2 * 1568},
		{"gemini-3.1-pro", 3 * 258, 2 * 1600},
	} {
		pricing := pricingFor(tc.model)
		if got := measureRequest(pricing, readable); got != (measurement{chars: 4, imageTokens: tc.readable}) {
			t.Errorf("%s readable images = %+v, want 4 chars and %d image tokens", tc.model, got, tc.readable)
		}
		if got := measureRequest(pricing, unreadable); got != (measurement{imageTokens: tc.unread}) {
			t.Errorf("%s unreadable images = %+v, want %d image tokens", tc.model, got, tc.unread)
		}
	}
}
```

`internal/tokencount/counter_test.go`:

```go
package tokencount

import (
	"strings"
	"testing"
)

func textRequest(chars int) map[string]any {
	return map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": strings.Repeat("a", chars)},
	}}
}

// withImage adds a 200×200 PNG, 64 tokens on Claude's standard tier, to the
// request's first message.
func withImage(t *testing.T, request map[string]any) map[string]any {
	t.Helper()
	message := request["messages"].([]any)[0].(map[string]any)
	message["content"] = []any{
		map[string]any{"type": "text", "text": message["content"]},
		base64ImageBlock("image/png", encodedImage(t, "png", 200, 200)),
	}
	return request
}

func TestEstimateStartsAtFourCharsPerToken(t *testing.T) {
	counter := New(nil)
	if got := counter.Estimate("", "claude-sonnet-4-6", textRequest(4001)); got != 1001 {
		t.Errorf("4001 chars = %d tokens, want 1001", got)
	}
	if got := counter.Estimate("", "claude-sonnet-4-6", withImage(t, textRequest(4000))); got != 1000+64 {
		t.Errorf("4000 chars and a 200x200 image = %d tokens, want 1064", got)
	}
}

func TestObserveLearnsTheRatioOfItsFamilyOnly(t *testing.T) {
	counter := New(nil)
	// 2,000 chars and a 64-token image answered with 1,064 tokens: the text
	// took 1,000, an observed ratio of 2.0, so the ratio moves from 4.0 to
	// 0.8×4.0 + 0.2×2.0 = 3.6. Had the image stayed in, it would be 3.576.
	counter.Observe("", "claude-sonnet-4-6", withImage(t, textRequest(2000)), 1064)

	if got := counter.Estimate("", "claude-opus-4-6", textRequest(3601)); got != 1001 {
		t.Errorf("claude after one observation: 3601 chars = %d tokens, want 1001", got)
	}
	if got := counter.Estimate("", "gemini-3.1-pro", textRequest(4000)); got != 1000 {
		t.Errorf("gemini: 4000 chars = %d tokens, want 1000 at the starting ratio", got)
	}

	counter.Observe("", "mimo-v2.6-flash-free", textRequest(2000), 1000)
	if got := counter.Estimate("", "kimi-k2.6", textRequest(4000)); got != 1000 {
		t.Errorf("kimi-k2.6 learned from another model: 4000 chars = %d tokens, want 1000", got)
	}
}

func TestObserveSkipsRequestsThatCannotTeachTheRatio(t *testing.T) {
	counter := New(nil)
	counter.Observe("", "claude-sonnet-4-6", textRequest(999), 100)
	counter.Observe("", "claude-sonnet-4-6", withImage(t, textRequest(2000)), 64)
	counter.Observe("", "claude-sonnet-4-6", textRequest(2000), 0)
	if got := counter.Estimate("", "claude-sonnet-4-6", textRequest(4000)); got != 1000 {
		t.Fatalf("after skipped observations: 4000 chars = %d tokens, want 1000", got)
	}

	// 1,000 measured chars is enough: 1,000 chars in 1,000 tokens moves the
	// ratio to 0.8×4.0 + 0.2×1.0 = 3.4.
	counter.Observe("", "claude-sonnet-4-6", textRequest(1000), 1000)
	if got := counter.Estimate("", "claude-sonnet-4-6", textRequest(3401)); got != 1001 {
		t.Fatalf("after a 1000-char observation: 3401 chars = %d tokens, want 1001", got)
	}
}

func TestRatioConvergesOnSyntheticObservations(t *testing.T) {
	counter := New(nil)
	for range 30 {
		counter.Observe("", "gemini-3.8-flash-high", textRequest(3000), 1000)
	}
	// 3 + 0.8^30 ≈ 3.00124 chars per token.
	if got := counter.Estimate("", "gemini-3.8-flash-high", textRequest(30000)); got != 9996 {
		t.Fatalf("30000 chars after 30 observations at 3.0 = %d tokens, want 9996", got)
	}
}
```

What each test pins: `TestMeasureRequestCountsEveryTextSource` checks that every spec'd source is measured, that ignored fields (signature, redacted thinking, `cache_control` on blocks) are not, that characters are runes, and that tool input JSON is not HTML-escaped. The image tests check both Claude tiers against Anthropic's published table, including the round-half-even tie, the version parse, Gemini tiles, decoding of PNG, JPEG and GIF headers (also inside `tool_result`), and the unreadable-source fallback per family. The counter tests check the starting ratio, the EMA arithmetic, that image tokens are subtracted before learning (3.6, not 3.576), family isolation, the 999/1,000-character boundary, ignoring a `realInput` that is ≤ 0 or does not exceed the image tokens, and convergence.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tokencount -count=1`
Expected: `FAIL antigravity-go-proxy/internal/tokencount [build failed]`, listing `counter_test.go:27:13: undefined: New`, `image_test.go:23:14: undefined: pricingFor` and `image_test.go:61:13: undefined: claudeHighResolution` before `too many errors`.

- [ ] **Step 3: Write minimal implementation**

`internal/tokencount/counter.go`:

```go
// Package tokencount estimates the input tokens of an Anthropic Messages
// request for gateways that have no count_tokens endpoint (Cloud Code, Zen).
// It learns a chars-per-token ratio per model family from the real input
// counts upstreams report, and prices images per image.
package tokencount

import (
	"strings"
	"sync"
	"time"
)

const (
	// startCharsPerToken is a family's ratio before its first observation.
	startCharsPerToken = 4.0
	// ratioAlpha is the weight of one observation in the moving average.
	ratioAlpha = 0.2
	// minLearnChars keeps short requests out of the ratio: their real count
	// is mostly overhead the proxy and the upstream add (system prompt,
	// tool framing), which says little about the text's own ratio.
	minLearnChars = 1000
)

// Family names the models that share one chars-per-token ratio. Every model
// outside the Claude, Gemini and gpt-oss families learns its own.
func Family(model string) string {
	switch {
	case strings.Contains(model, "claude"):
		return "claude"
	case strings.Contains(model, "gemini"):
		return "gemini"
	case strings.Contains(model, "gpt-oss"):
		return "gpt-oss"
	}
	return model
}

// Counter learns from requests an upstream answered and estimates requests
// that are never sent. It is safe for concurrent use.
type Counter struct {
	now func() time.Time

	mu     sync.Mutex
	ratios map[string]float64 // family -> chars per token
}

// New returns an empty Counter. now is its clock; nil means time.Now.
func New(now func() time.Time) *Counter {
	if now == nil {
		now = time.Now
	}
	return &Counter{now: now, ratios: make(map[string]float64)}
}

// Observe learns from a request an upstream answered. realInput is the input
// the upstream reported for it, cache reads and writes included. Image tokens
// are taken out first: they are priced per image, not by the ratio.
func (c *Counter) Observe(session, model string, request map[string]any, realInput int) {
	measured := measureRequest(pricingFor(model), request)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.learnLocked(Family(model), measured, realInput)
}

// Estimate returns the input tokens request would count for model.
func (c *Counter) Estimate(session, model string, request map[string]any) int {
	measured := measureRequest(pricingFor(model), request)
	c.mu.Lock()
	ratio := c.ratioLocked(Family(model))
	c.mu.Unlock()
	return measured.tokens(ratio)
}

func (c *Counter) ratioLocked(family string) float64 {
	if ratio, ok := c.ratios[family]; ok {
		return ratio
	}
	return startCharsPerToken
}

func (c *Counter) learnLocked(family string, measured measurement, realInput int) {
	textTokens := realInput - measured.imageTokens
	if measured.chars < minLearnChars || textTokens <= 0 {
		return
	}
	observed := float64(measured.chars) / float64(textTokens)
	c.ratios[family] = (1-ratioAlpha)*c.ratioLocked(family) + ratioAlpha*observed
}
```

`internal/tokencount/measure.go`:

```go
package tokencount

import (
	"encoding/json"
	"math"
	"unicode/utf8"
)

// measurement is what an estimate is made of: characters that the family's
// ratio turns into tokens, and image tokens that are priced per image.
type measurement struct {
	chars       int
	imageTokens int
}

// tokens converts a measurement at the given chars-per-token ratio.
func (m measurement) tokens(ratio float64) int {
	return int(math.Ceil(float64(m.chars)/ratio)) + m.imageTokens
}

// measureRequest measures a decoded Anthropic Messages body: the system text,
// every message, and each tool definition as compact JSON.
func measureRequest(pricing imagePricing, request map[string]any) measurement {
	var m measurement
	m.addSystem(request["system"])
	messages, _ := request["messages"].([]any)
	m.addMessages(pricing, messages)
	tools, _ := request["tools"].([]any)
	for _, tool := range tools {
		m.chars += jsonChars(tool)
	}
	return m
}

func (m *measurement) addSystem(system any) {
	switch system := system.(type) {
	case string:
		m.chars += utf8.RuneCountInString(system)
	case []any:
		for _, block := range system {
			if block, ok := block.(map[string]any); ok && block["type"] == "text" {
				m.addString(block["text"])
			}
		}
	}
}

func (m *measurement) addMessages(pricing imagePricing, messages []any) {
	for _, message := range messages {
		if message, ok := message.(map[string]any); ok {
			m.addContent(pricing, message["content"])
		}
	}
}

// addContent measures a message's content or a tool_result's content: a
// string, or a list of blocks. Thinking signatures, redacted thinking and
// cache_control markers are not text the model reads as tokens of the
// conversation, so they are left out.
func (m *measurement) addContent(pricing imagePricing, content any) {
	switch content := content.(type) {
	case string:
		m.chars += utf8.RuneCountInString(content)
	case []any:
		for _, block := range content {
			block, ok := block.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				m.addString(block["text"])
			case "thinking":
				m.addString(block["thinking"])
			case "tool_use":
				if input, ok := block["input"]; ok {
					m.chars += jsonChars(input)
				}
			case "tool_result":
				m.addContent(pricing, block["content"])
			case "image":
				m.imageTokens += pricing.sourceTokens(block["source"])
			}
		}
	}
}

func (m *measurement) addString(value any) {
	if text, ok := value.(string); ok {
		m.chars += utf8.RuneCountInString(text)
	}
}

// runeCounter is an io.Writer that only counts the runes written to it.
type runeCounter int

func (counter *runeCounter) Write(p []byte) (int, error) {
	*counter += runeCounter(utf8.RuneCount(p))
	return len(p), nil
}

// jsonChars is the length of value as compact JSON, counted without building
// the string. HTML escaping is off: with it, every < > & in a tool input full
// of code would count as six characters.
func jsonChars(value any) int {
	var counter runeCounter
	encoder := json.NewEncoder(&counter)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 0
	}
	return int(counter) - 1 // Encode ends the value with a newline
}
```

`internal/tokencount/image.go`:

```go
package tokencount

import (
	"encoding/base64"
	"image"
	// Registered for image.DecodeConfig.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"
)

// Image costs, from the providers' published rules:
//
//   - Claude, https://platform.claude.com/docs/en/build-with-claude/vision#evaluate-image-size
//     and the resize rule at
//     https://platform.claude.com/docs/en/build-with-claude/vision-coordinates#resize-your-image-before-uploading:
//     one token per 28×28 patch, ⌈w/28⌉ × ⌈h/28⌉, after the image is scaled down
//     to fit the model's tier (standard: 1,568 px long edge and 1,568 tokens;
//     Claude 4.7 and later: 2,576 px and 4,784 tokens).
//   - Gemini, https://ai.google.dev/gemini-api/docs/tokens: 258 tokens when
//     both sides are at most 384 px, otherwise 258 per 768×768 tile.
//
// Families with no published rule here (gpt-oss, Zen models) use Claude's
// standard tier.
const (
	claudePatch          = 28
	claudeStandardEdge   = 1568
	claudeStandardTokens = 1568
	claudeHighResEdge    = 2576
	claudeHighResTokens  = 4784

	geminiSmallEdge  = 384
	geminiTile       = 768
	geminiTileTokens = 258
	// geminiUnknownTokens prices a Gemini image whose size cannot be read:
	// Gemini has no cap to fall back on, so it counts a flat 1,600.
	geminiUnknownTokens = 1600
)

// imagePricing prices images for one model.
type imagePricing struct {
	gemini    bool
	maxEdge   int
	maxTokens int
}

func pricingFor(model string) imagePricing {
	family := Family(model)
	switch {
	case family == "gemini":
		return imagePricing{gemini: true}
	case family == "claude" && claudeHighResolution(model):
		return imagePricing{maxEdge: claudeHighResEdge, maxTokens: claudeHighResTokens}
	}
	return imagePricing{maxEdge: claudeStandardEdge, maxTokens: claudeStandardTokens}
}

// claudeHighResolution reports whether a Claude model id names Claude 4.7 or
// later. The version is the first number of one or two digits in the id and
// the one after it, so claude-opus-4-7, claude-sonnet-5.5-high and
// claude-3-7-sonnet-20250219 read 4.7, 5.5 and 3.7; a date is not a minor
// version, so claude-opus-4-20250514 reads 4.0.
func claudeHighResolution(model string) bool {
	fields := strings.FieldsFunc(model, func(r rune) bool {
		return r == '-' || r == '.' || r == '@' || r == '_'
	})
	for i, field := range fields {
		major, ok := shortNumber(field)
		if !ok {
			continue
		}
		minor := 0
		if i+1 < len(fields) {
			if value, ok := shortNumber(fields[i+1]); ok {
				minor = value
			}
		}
		return major > 4 || major == 4 && minor >= 7
	}
	return false
}

// shortNumber parses a field of one or two ASCII digits.
func shortNumber(field string) (int, bool) {
	if len(field) == 0 || len(field) > 2 {
		return 0, false
	}
	value := 0
	for i := range len(field) {
		if field[i] < '0' || field[i] > '9' {
			return 0, false
		}
		value = value*10 + int(field[i]-'0')
	}
	return value, true
}

// sourceTokens prices an image block's source. Only base64 PNG, JPEG and GIF
// data has a size that can be read; anything else (a URL, a file id, WebP,
// data that does not decode) costs the most an image of the tier can cost.
func (pricing imagePricing) sourceTokens(source any) int {
	width, height, ok := imageSize(source)
	if !ok {
		if pricing.gemini {
			return geminiUnknownTokens
		}
		return pricing.maxTokens
	}
	return pricing.tokens(width, height)
}

// imageSize reads the dimensions from the header of base64 image data. The
// decoder streams, so only the header is decoded, not the whole image.
func imageSize(source any) (width, height int, ok bool) {
	object, _ := source.(map[string]any)
	if object["type"] != "base64" {
		return 0, 0, false
	}
	data, _ := object["data"].(string)
	config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func (pricing imagePricing) tokens(width, height int) int {
	if pricing.gemini {
		if width <= geminiSmallEdge && height <= geminiSmallEdge {
			return geminiTileTokens
		}
		return ceilDiv(width, geminiTile) * ceilDiv(height, geminiTile) * geminiTileTokens
	}
	width, height = pricing.resized(width, height)
	return claudePatches(width, height)
}

func claudePatches(width, height int) int {
	return ceilDiv(width, claudePatch) * ceilDiv(height, claudePatch)
}

func (pricing imagePricing) fits(width, height int) bool {
	return ceilDiv(width, claudePatch)*claudePatch <= pricing.maxEdge &&
		ceilDiv(height, claudePatch)*claudePatch <= pricing.maxEdge &&
		claudePatches(width, height) <= pricing.maxTokens
}

// resized is Anthropic's reference resize: the largest aspect-preserving size
// that fits the tier, found by binary search on the long edge, with the short
// edge rounded half to even as the live API does.
func (pricing imagePricing) resized(width, height int) (int, int) {
	if pricing.fits(width, height) {
		return width, height
	}
	if height > width {
		resizedHeight, resizedWidth := pricing.resized(height, width)
		return resizedWidth, resizedHeight
	}
	aspect := float64(width) / float64(height)
	shortEdge := func(longEdge int) int {
		return max(int(math.RoundToEven(float64(longEdge)/aspect)), 1)
	}
	low, high := 1, width // low always fits, high never does
	for low+1 < high {
		middle := (low + high) / 2
		if pricing.fits(middle, shortEdge(middle)) {
			low = middle
		} else {
			high = middle
		}
	}
	return low, shortEdge(low)
}

func ceilDiv(value, divisor int) int {
	return (value + divisor - 1) / divisor
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tokencount -run 'TestFamily|TestMeasureRequest|TestClaudeImageTokens|TestClaudeResolutionTier|TestGeminiImageTokens|TestImageBlocks|TestEstimateStartsAtFourCharsPerToken|TestObserveLearns|TestObserveSkips|TestRatioConverges' -count=1 -v`
Expected: 11 `--- PASS` lines, then `ok  	antigravity-go-proxy/internal/tokencount`.

Run: `go vet ./internal/tokencount && gofmt -l internal/tokencount`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/tokencount/counter.go internal/tokencount/measure.go internal/tokencount/image.go internal/tokencount/counter_test.go internal/tokencount/measure_test.go internal/tokencount/image_test.go
git commit -m "feat(tokencount): estimate input tokens from a learned chars-per-token ratio"
```

---

### Task 9: Token estimator: conversation memory

**Files:**
- Create: `internal/tokencount/memory.go`
- Modify: `internal/tokencount/counter.go` (whole file as written in Task 8: imports `:7-11`, `Counter` `:38-45`, `New` `:47-53`, `Observe` and `Estimate` `:55-72`; the replacement file is below)
- Modify: `internal/tokencount/measure.go:33-35` (insert `measureMessages` between `measureRequest`, which ends at line 33, and `addSystem`, which starts at line 35)
- Test: `internal/tokencount/memory_test.go`

**Interfaces:**
- Consumes (Task 8): `measureRequest`, `measurement.tokens`, `(*measurement).addMessages`, `pricingFor`, `imagePricing`, `(*Counter).ratioLocked`, `(*Counter).learnLocked`, `Family`, and the test helpers `encodedImage`, `base64ImageBlock`, `textRequest`.
- Produces (Task 10+ rely on these; the exported signatures do not change from Task 8):
  - `func New(now func() time.Time) *Counter`: the clock now also drives the memory's idle TTL.
  - `func (c *Counter) Observe(session, model string, request map[string]any, realInput int)`. With `session != ""` and `realInput > 0` it stores the session's memory for `Family(model)`: `realInput`, the message count `n`, the SHA-256 of the first `n` messages and the SHA-256 of `system` + `tools`. Both digests are compact JSON with `cache_control` removed. `session == ""` updates the ratio only (spec §6.3, "Feeding").
  - `func (c *Counter) Estimate(session, model string, request map[string]any) int`: when the session's memory for the family has the same `system` + `tools` digest and the request's first `n` messages digest the same, the result is the stored count plus the ratio estimate (and image tokens) of `messages[n:]`. Otherwise it estimates the whole request. It does not extend the memory's TTL.
  - Memory limits: `conversationIdleTTL = time.Hour` (an entry idle for *more than* 1 h is dropped), `maxConversations = 10000` (the least recently observed entry is evicted first).
  - Concurrency: one `sync.Mutex`. Measuring and hashing run outside it, so a burst of concurrent `Estimate` calls (a `/context` sends about 34) only serializes on map lookups.
  - Key ordering: `encoding/json` writes map keys sorted, so a decoded body hashes the same whatever key order the client sent. `Observe` and `Estimate` both hash the decoded maps the same way, so their digests match.

- [ ] **Step 1: Write the failing tests**

`internal/tokencount/memory_test.go`:

```go
package tokencount

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	readTool  = map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}  // 48 chars
	writeTool = map[string]any{"name": "Write", "input_schema": map[string]any{"type": "object"}} // 49 chars
)

// chatRequest builds a decoded request whose messages alternate user and
// assistant text. Like Claude Code, it puts the cache breakpoint on the
// newest message only, so the next turn sends this turn's last message
// without it.
func chatRequest(system string, tools []any, texts ...string) map[string]any {
	messages := make([]any, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		block := map[string]any{"type": "text", "text": text}
		if i == len(texts)-1 {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		messages[i] = map[string]any{"role": role, "content": []any{block}}
	}
	return map[string]any{"system": system, "tools": tools, "messages": messages}
}

// firstTurn is 71 chars: "be brief" 8, "fix the bug" 11, "done" 4, Read 48.
// With no memory it estimates ⌈71/4⌉ = 18 tokens.
func firstTurn() map[string]any {
	return chatRequest("be brief", []any{readTool}, "fix the bug", "done")
}

func TestEstimateContinuesFromTheLastRealCount(t *testing.T) {
	counter := New(nil)
	observed := firstTurn()
	counter.Observe("session-a", "claude-sonnet-4-6", observed, 5000)

	if got := counter.Estimate("session-a", "claude-sonnet-4-6", firstTurn()); got != 5000 {
		t.Errorf("same conversation = %d tokens, want the real 5000", got)
	}

	next := chatRequest("be brief", []any{readTool}, "fix the bug", "done", strings.Repeat("b", 400))
	newest := next["messages"].([]any)[2].(map[string]any)
	newest["content"] = append(newest["content"].([]any), base64ImageBlock("image/png", encodedImage(t, "png", 200, 200)))
	// 400 new chars at 4 per token and a 64-token image, on top of the real
	// count; the earlier messages lost their breakpoint and still match.
	if got := counter.Estimate("session-a", "claude-opus-4-6", next); got != 5000+100+64 {
		t.Errorf("next turn = %d tokens, want 5164", got)
	}

	last := observed["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if _, ok := last["cache_control"]; !ok {
		t.Error("Observe removed cache_control from the caller's request")
	}
}

func TestEstimateRestartsWhenTheConversationChanged(t *testing.T) {
	counter := New(nil)
	counter.Observe("session-a", "claude-sonnet-4-6", firstTurn(), 5000)

	for _, tc := range []struct {
		name    string
		session string
		model   string
		request map[string]any
		want    int
	}{
		{"continued", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 5000 + 1},
		{"earlier message edited", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bugs", "done", "next"), 19}, // 76 chars
		{"compacted to fewer messages", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "summary"), 16}, // 63 chars
		{"tool added", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool, writeTool}, "fix the bug", "done", "next"), 31}, // 124 chars
		{"system changed", "session-a", "claude-sonnet-4-6",
			chatRequest("be terse", []any{readTool}, "fix the bug", "done", "next"), 19}, // 75 chars
		{"other session", "session-b", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 19},
		{"other family", "session-a", "gemini-3.1-pro",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 19},
		{"no session", "", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 19},
	} {
		if got := counter.Estimate(tc.session, tc.model, tc.request); got != tc.want {
			t.Errorf("%s: %d tokens, want %d", tc.name, got, tc.want)
		}
	}
}

func TestObserveRemembersOnlyWithASessionAndARealCount(t *testing.T) {
	counter := New(nil)
	// No session: the ratio learns (2,000 chars in 1,000 tokens, 4.0 → 3.6)
	// and nothing is remembered, so a session estimate is ⌈2000/3.6⌉ = 556,
	// not 1,000.
	counter.Observe("", "claude-sonnet-4-6", textRequest(2000), 1000)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", textRequest(2000)); got != 556 {
		t.Fatalf("after a sessionless observation = %d tokens, want 556", got)
	}
	// A reply without usage teaches neither.
	counter.Observe("session-a", "claude-sonnet-4-6", textRequest(2000), 0)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", textRequest(2000)); got != 556 {
		t.Fatalf("after an observation without a count = %d tokens, want 556", got)
	}
}

func TestConversationMemoryExpiresAfterAnIdleHour(t *testing.T) {
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	now := start
	counter := New(func() time.Time { return now })
	counter.Observe("session-a", "claude-sonnet-4-6", firstTurn(), 5000)

	now = start.Add(50 * time.Minute)
	counter.Observe("session-a", "claude-sonnet-4-6", firstTurn(), 6000)

	now = start.Add(50*time.Minute + time.Hour)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", firstTurn()); got != 6000 {
		t.Fatalf("one hour after the last observation = %d tokens, want the remembered 6000", got)
	}
	now = now.Add(time.Nanosecond)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", firstTurn()); got != 18 {
		t.Fatalf("past one idle hour = %d tokens, want the full estimate 18", got)
	}
}

func TestConversationMemoryEvictsTheOldestAboveTheCap(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	counter := New(func() time.Time { return now })
	observe := func(session string) {
		now = now.Add(time.Millisecond)
		counter.Observe(session, "claude-sonnet-4-6", firstTurn(), 5000)
	}
	for i := range maxConversations {
		observe(fmt.Sprintf("session-%d", i))
	}
	observe("session-0") // now the latest; session-1 is the oldest
	observe("session-new")

	for session, want := range map[string]int{
		"session-0":   5000,
		"session-1":   18,
		"session-2":   5000,
		"session-new": 5000,
	} {
		if got := counter.Estimate(session, "claude-sonnet-4-6", firstTurn()); got != want {
			t.Errorf("%s = %d tokens, want %d", session, got, want)
		}
	}
	if held := len(counter.conversations); held != maxConversations {
		t.Errorf("memory holds %d conversations, want the cap %d", held, maxConversations)
	}
}

func TestCounterIsSafeForConcurrentUse(t *testing.T) {
	counter := New(nil)
	var wg sync.WaitGroup
	results := make(chan int, 8*100)
	for g := range 8 {
		wg.Go(func() {
			session := fmt.Sprintf("session-%d", g%2)
			for range 100 {
				counter.Observe(session, "claude-sonnet-4-6", firstTurn(), 5000)
				results <- counter.Estimate(session, "claude-sonnet-4-6", firstTurn())
				counter.Observe("", "gemini-3.1-pro", textRequest(2000), 1000)
			}
		})
	}
	wg.Wait()
	close(results)

	for got := range results {
		if got != 5000 {
			t.Fatalf("estimate after its own observation = %d tokens, want 5000", got)
		}
	}
	// 800 observations at 2.0 chars per token leave the gemini ratio at 2.0.
	if got := counter.Estimate("", "gemini-3.1-pro", textRequest(4000)); got != 2000 {
		t.Fatalf("gemini 4000 chars = %d tokens, want 2000", got)
	}
}
```

What each test pins: `TestEstimateContinuesFromTheLastRealCount` checks that the stored count is reused exactly. It also checks that the tail is priced by ratio plus image tokens, that a moved `cache_control` breakpoint still matches, and that the caller's map is not mutated. `TestEstimateRestartsWhenTheConversationChanged` covers each mismatch that must fall back to a full estimate: edited prefix, fewer messages, changed tools, changed system, other session, other family, no session. `TestObserveRemembersOnlyWithASessionAndARealCount` checks the sessionless ratio-only path and the zero-usage guard. The TTL test checks that `Observe` refreshes the TTL, that an entry at exactly 1 h is kept, and that 1 h + 1 ns drops it. The cap test checks LRU eviction (a re-observed session moves to the front) and that the size stays at the cap. The concurrency test runs `Observe` and `Estimate` from 8 goroutines under `-race` and checks the final state.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tokencount -run 'TestEstimateContinues|TestEstimateRestarts|TestObserveRemembers|TestConversationMemory|TestCounterIsSafe' -count=1`
Expected: `FAIL antigravity-go-proxy/internal/tokencount [build failed]` with `undefined: maxConversations` and `counter.conversations undefined (type *Counter has no field or method conversations)`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/tokencount/memory.go`:

```go
package tokencount

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"hash"
	"maps"
	"slices"
	"time"
)

const (
	// conversationIdleTTL drops a conversation's memory an hour after the
	// last request observed in it, the lifetime of a session pin.
	conversationIdleTTL = time.Hour
	// maxConversations caps the memory; the least recently observed
	// conversation goes first.
	maxConversations = 10000
)

// conversation is what Estimate keeps of the last request observed in a
// session for one family: its real input count, how many messages it had,
// and digests that tell whether a later request continues it.
type conversation struct {
	key       string
	realInput int
	messages  int
	prefix    [sha256.Size]byte // digestMessages of all its messages
	context   [sha256.Size]byte // digestContext of its system and tools
	lastSeen  time.Time
}

func memoryKey(session, family string) string {
	return session + "\x00" + family
}

func newConversation(key string, request map[string]any, realInput int) conversation {
	messages, _ := request["messages"].([]any)
	return conversation{
		key:       key,
		realInput: realInput,
		messages:  len(messages),
		prefix:    digestMessages(messages),
		context:   digestContext(request),
	}
}

// continuedBy reports whether request has the same system and tools as the
// remembered one and starts with all of its messages.
func (memory conversation) continuedBy(request map[string]any, messages []any) bool {
	return len(messages) >= memory.messages &&
		digestContext(request) == memory.context &&
		digestMessages(messages[:memory.messages]) == memory.prefix
}

// rememberLocked stores memory as its session's latest conversation.
func (c *Counter) rememberLocked(memory conversation, now time.Time) {
	c.pruneLocked(now)
	memory.lastSeen = now
	if element, ok := c.conversations[memory.key]; ok {
		element.Value = memory
		c.order.MoveToFront(element)
		return
	}
	if c.order.Len() >= maxConversations {
		c.dropLocked(c.order.Back())
	}
	c.conversations[memory.key] = c.order.PushFront(memory)
}

func (c *Counter) recallLocked(key string, now time.Time) (conversation, bool) {
	c.pruneLocked(now)
	element, ok := c.conversations[key]
	if !ok {
		return conversation{}, false
	}
	return element.Value.(conversation), true
}

// pruneLocked drops the conversations idle for more than conversationIdleTTL.
// order runs from the latest observation to the oldest, so they are all at
// its back.
func (c *Counter) pruneLocked(now time.Time) {
	for element := c.order.Back(); element != nil; element = c.order.Back() {
		if now.Sub(element.Value.(conversation).lastSeen) <= conversationIdleTTL {
			return
		}
		c.dropLocked(element)
	}
}

func (c *Counter) dropLocked(element *list.Element) {
	delete(c.conversations, element.Value.(conversation).key)
	c.order.Remove(element)
}

// digestContext hashes what a request carries besides its messages: the
// system prompt and the tool list.
func digestContext(request map[string]any) [sha256.Size]byte {
	digest := sha256.New()
	writeDigest(digest, request["system"])
	writeDigest(digest, request["tools"])
	return sum(digest)
}

// digestMessages hashes messages as compact JSON, one encoding per message.
func digestMessages(messages []any) [sha256.Size]byte {
	digest := sha256.New()
	for _, message := range messages {
		writeDigest(digest, message)
	}
	return sum(digest)
}

// writeDigest writes value to digest as compact JSON. encoding/json writes map
// keys sorted, so two decodes of one body hash alike whatever key order the
// client sent. cache_control markers are left out: a client that moves its
// cache breakpoint to the newest message, as Claude Code does, sends the
// earlier messages without the marker they carried the turn before, and a
// marker costs no tokens. Encode writes nothing for a value that does not
// encode, which leaves it out on both sides alike.
func writeDigest(digest hash.Hash, value any) {
	stripped, _ := withoutCacheControl(value)
	_ = json.NewEncoder(digest).Encode(stripped)
}

func sum(digest hash.Hash) [sha256.Size]byte {
	var out [sha256.Size]byte
	digest.Sum(out[:0])
	return out
}

// withoutCacheControl returns value without its cache_control members, at
// any depth, and whether it removed any. It copies only the maps and slices
// on the way to a marker: the request still belongs to the caller, who sends
// it upstream.
func withoutCacheControl(value any) (any, bool) {
	switch value := value.(type) {
	case map[string]any:
		var out map[string]any
		for key, member := range value {
			if key == "cache_control" {
				if out == nil {
					out = maps.Clone(value)
				}
				delete(out, key)
				continue
			}
			if stripped, changed := withoutCacheControl(member); changed {
				if out == nil {
					out = maps.Clone(value)
				}
				out[key] = stripped
			}
		}
		if out == nil {
			return value, false
		}
		return out, true
	case []any:
		var out []any
		for i, element := range value {
			if stripped, changed := withoutCacheControl(element); changed {
				if out == nil {
					out = slices.Clone(value)
				}
				out[i] = stripped
			}
		}
		if out == nil {
			return value, false
		}
		return out, true
	}
	return value, false
}
```

In `internal/tokencount/measure.go`, insert after `measureRequest` (after line 33, before `func (m *measurement) addSystem` at line 35):

```go
// measureMessages measures messages alone, for the tail of a conversation
// whose earlier part has a real count.
func measureMessages(pricing imagePricing, messages []any) measurement {
	var m measurement
	m.addMessages(pricing, messages)
	return m
}
```

Replace `internal/tokencount/counter.go` with the following. The changes against Task 8: the `container/list` import, the `conversations` and `order` fields, `New` initializing them, and new `Observe` and `Estimate` bodies and doc comments. `Family`, `ratioLocked` and `learnLocked` are unchanged.

```go
// Package tokencount estimates the input tokens of an Anthropic Messages
// request for gateways that have no count_tokens endpoint (Cloud Code, Zen).
// It learns a chars-per-token ratio per model family from the real input
// counts upstreams report, and prices images per image.
package tokencount

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

const (
	// startCharsPerToken is a family's ratio before its first observation.
	startCharsPerToken = 4.0
	// ratioAlpha is the weight of one observation in the moving average.
	ratioAlpha = 0.2
	// minLearnChars keeps short requests out of the ratio: their real count
	// is mostly overhead the proxy and the upstream add (system prompt,
	// tool framing), which says little about the text's own ratio.
	minLearnChars = 1000
)

// Family names the models that share one chars-per-token ratio. Every model
// outside the Claude, Gemini and gpt-oss families learns its own.
func Family(model string) string {
	switch {
	case strings.Contains(model, "claude"):
		return "claude"
	case strings.Contains(model, "gemini"):
		return "gemini"
	case strings.Contains(model, "gpt-oss"):
		return "gpt-oss"
	}
	return model
}

// Counter learns from requests an upstream answered and estimates requests
// that are never sent. It is safe for concurrent use.
type Counter struct {
	now func() time.Time

	mu            sync.Mutex
	ratios        map[string]float64       // family -> chars per token
	conversations map[string]*list.Element // memoryKey -> element of order
	order         *list.List               // conversation values, latest observation first
}

// New returns an empty Counter. now is its clock; nil means time.Now.
func New(now func() time.Time) *Counter {
	if now == nil {
		now = time.Now
	}
	return &Counter{
		now:           now,
		ratios:        make(map[string]float64),
		conversations: make(map[string]*list.Element),
		order:         list.New(),
	}
}

// Observe learns from a request an upstream answered. realInput is the input
// the upstream reported for it, cache reads and writes included. Image tokens
// are taken out first: they are priced per image, not by the ratio. With a
// session, the request also becomes that conversation's memory for Estimate.
func (c *Counter) Observe(session, model string, request map[string]any, realInput int) {
	family := Family(model)
	measured := measureRequest(pricingFor(model), request)
	remember := session != "" && realInput > 0
	var memory conversation
	if remember {
		memory = newConversation(memoryKey(session, family), request, realInput)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.learnLocked(family, measured, realInput)
	if remember {
		c.rememberLocked(memory, c.now())
	}
}

// Estimate returns the input tokens request would count for model. When it
// continues the last request observed in its session, with the same system
// and tools, the estimate is that request's real count plus an estimate of
// the messages after it; otherwise the whole request is estimated. Estimate
// does not keep a conversation alive: only Observe does.
func (c *Counter) Estimate(session, model string, request map[string]any) int {
	family := Family(model)
	pricing := pricingFor(model)
	c.mu.Lock()
	ratio := c.ratioLocked(family)
	var memory conversation
	found := false
	if session != "" {
		memory, found = c.recallLocked(memoryKey(session, family), c.now())
	}
	c.mu.Unlock()
	messages, _ := request["messages"].([]any)
	if found && memory.continuedBy(request, messages) {
		return memory.realInput + measureMessages(pricing, messages[memory.messages:]).tokens(ratio)
	}
	return measureRequest(pricing, request).tokens(ratio)
}

func (c *Counter) ratioLocked(family string) float64 {
	if ratio, ok := c.ratios[family]; ok {
		return ratio
	}
	return startCharsPerToken
}

func (c *Counter) learnLocked(family string, measured measurement, realInput int) {
	textTokens := realInput - measured.imageTokens
	if measured.chars < minLearnChars || textTokens <= 0 {
		return
	}
	observed := float64(measured.chars) / float64(textTokens)
	c.ratios[family] = (1-ratioAlpha)*c.ratioLocked(family) + ratioAlpha*observed
}
```

`c.now()` is read under the lock so that the list order (latest first) matches `lastSeen` order even when two `Observe` calls race. Pruning from the back is O(expired entries) and eviction is O(1), so no call scans all 10,000 entries.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/tokencount -run 'TestEstimateContinues|TestEstimateRestarts|TestObserveRemembers|TestConversationMemory|TestCounterIsSafe' -count=1 -v`
Expected: 6 `--- PASS` lines, no `WARNING: DATA RACE`, then `ok  	antigravity-go-proxy/internal/tokencount`.

Run: `go test -race ./internal/tokencount -count=1 && go vet ./internal/tokencount && gofmt -l internal/tokencount`
Expected: `ok  	antigravity-go-proxy/internal/tokencount` (all 17 tests, Task 8's included), no vet or gofmt output.

- [ ] **Step 5: Commit**

```bash
git add internal/tokencount/memory.go internal/tokencount/memory_test.go internal/tokencount/counter.go internal/tokencount/measure.go
git commit -m "feat(tokencount): continue estimates from a conversation's last real count"
```

---

## Part D: count_tokens route, drift tooling and docs (spec §6.1, §6.2, §9, §11): Tasks 10–14

Line numbers are those of the base commit; locate each edit by the quoted code once an earlier task has shifted the file. The Claude Code forward (Task 11) comes before the OpenRouter, Kimi and custom forwards (Task 12): it builds the forward machinery (one-hour unsupported marks, estimate fallback) together with the count identity, so Task 12's normalized custom endpoints use `ccidentity.CountTokensProfile` from the start.

---

### Task 10: count_tokens route, shared gateway routing, estimate path and its feeding

**Files:**
- Create: `internal/api/count_tokens.go`
- Modify: `internal/api/dispatch.go:1-128` and `:220-258` (match/forward split; `claudeCodeGatewayBody`, `thinkingNeedsDisplay`, `rawThinking`, `withSummarizedDisplay` at `:130-218` stay)
- Modify: `internal/api/discovery.go:410-411` (comment names the renamed matcher)
- Modify: `internal/api/server.go:29-53` (import), `:157-160` (Server field), `:213-224` (New), `:523-524` (route), `:1812`, `:1825`, `:1848-1871` (Zen recorder), `:3104` (unaryMessage), `:3199` (streamMessage)
- Modify: `internal/api/upstream_body_test.go:92` (new `zenInstrumentResponse` signature)
- Modify: `internal/api/management_auth_test.go:402` (501 row)
- Modify: `internal/api/dispatch_test.go:3-17` (import `context`), `:480-497` (coverage test renamed)
- Test: `internal/api/count_tokens_test.go`, `internal/api/dispatch_test.go`

**Interfaces:**
- Consumes (the tokencount tasks, `internal/tokencount`, safe for concurrent use): `func New(now func() time.Time) *Counter`; `func (c *Counter) Observe(session, model string, request map[string]any, realInput int)`; `func (c *Counter) Estimate(session, model string, request map[string]any) int`. From the session-affinity task: `server.messages()` already wraps the request context with `accounts.WithSessionKey(ctx, ccExtractSessionID(request, body))`; this task does not edit that code. Observe is keyed by the `sessionID` that `unaryMessage`/`streamMessage` already compute with the same `ccExtractSessionID`, so the estimator's key and the pin key are the same string.
- Produces (used by the Claude Code count forward task and the gateway count forwards task):
  - `type gatewayRoute struct { id config.GatewayID; target string; kimi config.KimiModelConfig; zen config.ZenModelConfig; endpoint config.EndpointConfig }`
  - `type gateway struct { match func(server *Server, ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool); forward func(server *Server, g *gatewayRequest, route gatewayRoute) }` and `var gateways map[config.GatewayID]gateway` (replaces `gatewayHandlers`)
  - `func (server *Server) routeGateway(ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool)`
  - `func (server *Server) countTokens(writer http.ResponseWriter, request *http.Request)`
  - `func (server *Server) observeTokens(session, model string, request map[string]any, realInput int)`
  - `Server` field `tokens *tokencount.Counter`, built in `New` with `options.Now`
  - `func (server *Server) zenUsageRecorder(model, sessionID string, startTime time.Time, anthropicRequest map[string]any) func(in, out, cr, cw int)` and `func (server *Server) zenInstrumentResponse(resp *http.Response, model, sessionID string, startTime time.Time, anthropicRequest map[string]any)`
  - test helpers in `count_tokens_test.go`: `const countPath`, `func newCountServer(t *testing.T, cfg config.Config, now func() time.Time) *Server`, `func sendInSession(handler http.Handler, path, apiKey, session, body string, header http.Header) *httptest.ResponseRecorder`, `func countOf(t *testing.T, recorder *httptest.ResponseRecorder) int`

- [ ] **Step 1: Write the failing tests**

Create `internal/api/count_tokens_test.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

const countPath = "/v1/messages/count_tokens"

// newCountServer installs cfg and returns a server on the clock now. Its
// account-backed path is a recording backend; the Claude Code pool and the Zen
// keyless warning start fresh, and the config dir is the test's own.
func newCountServer(t *testing.T, cfg config.Config, now func() time.Time) *Server {
	t.Helper()
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	config.SetForTest(cfg)
	resetCCPoolForTest()
	t.Cleanup(resetCCPoolForTest)
	resetZenKeylessWarning()
	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &dispatchRecordingBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     now,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// sendInSession posts body to path with the proxy key apiKey, under the Claude
// Code session header when session is not empty, plus header. It takes no
// *testing.T so a burst can call it from many goroutines.
func sendInSession(handler http.Handler, path, apiKey, session, body string, header http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", apiKey)
	if session != "" {
		request.Header.Set("X-Claude-Code-Session-Id", session)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// countOf is the count a count_tokens answer carries. The answer must be a 200
// holding input_tokens and nothing else, as Anthropic's does.
func countOf(t *testing.T, recorder *httptest.ResponseRecorder) int {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("count_tokens body %q: %v", recorder.Body.String(), err)
	}
	if len(answer) != 1 {
		t.Fatalf("count_tokens body = %s, want only input_tokens", recorder.Body.String())
	}
	var tokens int
	if err := json.Unmarshal(answer["input_tokens"], &tokens); err != nil {
		t.Fatalf("input_tokens in %s: %v", recorder.Body.String(), err)
	}
	return tokens
}

// usageStream is one Cloud Code answer reporting promptTokens of input, cached
// of them read from the prompt cache. promptTokens 0 reports no usage at all.
func usageStream(promptTokens, cached int) [][]byte {
	if promptTokens == 0 {
		return [][]byte{[]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"FORMAT_OK"}]},"finishReason":"STOP"}]}}`)}
	}
	return [][]byte{[]byte(fmt.Sprintf(`{"response":{"candidates":[{"content":{"parts":[{"text":"FORMAT_OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":%d,"cachedContentTokenCount":%d,"candidatesTokenCount":2}}}`, promptTokens, cached))}
}

// The estimate builds on what the conversation's last turn really cost: 5,000
// tokens of input, 1,200 of them cache reads. A count of the next turn in the
// same session is that plus a few tokens for the two new messages; the same
// count in another session knows nothing of it. A later turn whose answer
// reported no usage does not erase what the session learned.
func TestCountTokens_CloudCodeEstimateBuildsOnTheConversationsRealCount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			upstream := &fakeUpstream{streamData: usageStream(5000, 1200)}
			handler := newTestHandler(t, upstream, "managed-project")
			turn := func(messages string) string {
				return fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":64,"stream":%v,"messages":%s}`, stream, messages)
			}
			if rec := sendInSession(handler, "/v1/messages", "local-key", "sess-count", turn(`[{"role":"user","content":"hello"}]`), nil); rec.Code != http.StatusOK {
				t.Fatalf("first turn: %d %s", rec.Code, rec.Body.String())
			}

			next := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"}]}`
			if got := countOf(t, sendInSession(handler, countPath, "local-key", "sess-count", next, nil)); got < 5000 || got >= 5200 {
				t.Errorf("count in the session = %d, want the real 5000 plus a few tokens for the new messages", got)
			}
			if got := countOf(t, sendInSession(handler, countPath, "local-key", "another-session", next, nil)); got <= 0 || got >= 200 {
				t.Errorf("count in another session = %d, want a small estimate of the text alone", got)
			}

			upstream.mu.Lock()
			upstream.streamData = usageStream(0, 0)
			upstream.mu.Unlock()
			if rec := sendInSession(handler, "/v1/messages", "local-key", "sess-count", turn(`[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"}]`), nil); rec.Code != http.StatusOK {
				t.Fatalf("second turn: %d %s", rec.Code, rec.Body.String())
			}
			later := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"more"}]}`
			if got := countOf(t, sendInSession(handler, countPath, "local-key", "sess-count", later, nil)); got < 5000 || got >= 5200 {
				t.Errorf("count after a turn with no usage = %d, want it still built on the real 5000", got)
			}
			if upstream.streamCalls != 2 {
				t.Errorf("upstream generations = %d, want 2: a count never generates", upstream.streamCalls)
			}
		})
	}
}

// Zen has no count endpoint either. Its real count is input plus cache reads
// plus cache writes of the Anthropic usage, here 3,000 + 1,500 + 500.
func TestCountTokens_ZenEstimateBuildsOnTheConversationsRealCount(t *testing.T) {
	var generations atomic.Int32
	zenUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			generations.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_zen","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"FORMAT_OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3000,"cache_read_input_tokens":1500,"cache_creation_input_tokens":500,"output_tokens":3}}`)
	}))
	t.Cleanup(zenUpstream.Close)
	cfg := config.DefaultConfig()
	cfg.Zen.Enabled = true
	cfg.Zen.BaseURL = zenUpstream.URL
	cfg.Zen.APIKey = "k-zen"
	cfg.Zen.Allowlist = []config.ZenModelConfig{{ID: dispatchModel, Enabled: true}}
	handler := newCountServer(t, cfg, time.Now).Handler()

	first := `{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
	if rec := sendInSession(handler, "/v1/messages", "test-proxy-key", "sess-zen", first, nil); rec.Code != http.StatusOK {
		t.Fatalf("Zen turn: %d %s", rec.Code, rec.Body.String())
	}
	next := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"}]}`
	if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "sess-zen", next, nil)); got < 5000 || got >= 5200 {
		t.Errorf("count in the session = %d, want the real 5000 plus a few tokens", got)
	}
	if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "another-session", next, nil)); got <= 0 || got >= 200 {
		t.Errorf("count in another session = %d, want a small estimate of the text alone", got)
	}
	if n := generations.Load(); n != 1 {
		t.Errorf("Zen messages requests = %d, want 1: Zen counts are estimated, never forwarded", n)
	}
}

func TestCountTokens_RequiresAMessagesArray(t *testing.T) {
	handler := newTestHandler(t, &fakeUpstream{}, "managed-project")
	for _, body := range []string{`{}`, `{"model":"claude-sonnet-4-6","messages":"hi"}`, `not json`} {
		rec := sendInSession(handler, countPath, "local-key", "", body, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request_error") {
			t.Errorf("body %s: status %d %s, want 400 invalid_request_error", body, rec.Code, rec.Body.String())
		}
	}
}

func TestCountTokens_AnthropicMirrorAnswers(t *testing.T) {
	upstream := &fakeUpstream{streamData: standardStream()}
	handler := newTestHandler(t, upstream, "managed-project")
	body := `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":"hello there"}]}`
	if got := countOf(t, sendInSession(handler, "/anthropic/v1/messages/count_tokens", "local-key", "", body, nil)); got <= 0 {
		t.Errorf("count = %d, want a positive estimate", got)
	}
	if upstream.streamCalls != 0 {
		t.Errorf("upstream generations = %d, want 0", upstream.streamCalls)
	}
}
```

Append to `internal/api/dispatch_test.go` (and add `"context"` to its import block at `:3-17`):

```go
// routeGateway answers which gateway /v1/messages would use without sending
// anything: count_tokens asks the same question.
func TestRouteGateway_NamesTheClaimingGatewayAndSendsNothing(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	server := newDispatchServer(t, cfg, backend)
	for _, tc := range []struct {
		name   string
		order  []config.GatewayID
		model  string
		wantOK bool
		wantID config.GatewayID
		target string
	}{
		{"default order puts kimi first", nil, dispatchModel, true, config.GatewayKimi, dispatchModel},
		{"kimi strips the 1m suffix", nil, dispatchModel + "[1m]", true, config.GatewayKimi, dispatchModel},
		{"openrouter hoisted", []config.GatewayID{"openrouter", "kimi"}, dispatchModel, true, config.GatewayOpenRouter, dispatchModel},
		{"zen sends its canonical spelling", []config.GatewayID{"zen", "kimi"}, "opencode/" + dispatchModel, true, config.GatewayZen, dispatchModel},
		{"custom keeps the requested model", []config.GatewayID{"custom", "kimi"}, dispatchModel, true, config.GatewayCustom, dispatchModel},
		{"cloudcode first ends the walk", []config.GatewayID{"cloudcode", "kimi"}, dispatchModel, false, "", ""},
		{"nothing claims an unknown model", nil, "gemini-3.5-flash", false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			if tc.order != nil {
				c.GatewayOrder.Order = tc.order
			}
			route, ok := server.routeGateway(context.Background(), c, tc.model)
			if ok != tc.wantOK || route.id != tc.wantID || route.target != tc.target {
				t.Fatalf("routeGateway(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.model, route.id, route.target, ok, tc.wantID, tc.target, tc.wantOK)
			}
			if tc.wantID == config.GatewayCustom && route.endpoint.URL != custom.URL {
				t.Errorf("custom route endpoint = %q, want %q", route.endpoint.URL, custom.URL)
			}
		})
	}
	if total := probe.total(); total != 0 {
		t.Errorf("gateway hits = %d, want 0: routing sends nothing", total)
	}
}
```

Replace `TestGatewayHandlers_CoverEveryKnownGateway` (`dispatch_test.go:480-497`) with:

```go
func TestGateways_CoverEveryKnownGateway(t *testing.T) {
	if _, ok := gateways[config.GatewayCloudCode]; ok {
		t.Error("cloudcode must have no gateway entry: it is the terminal account-backed path")
	}
	for _, id := range config.KnownGatewayIDs() {
		if id == config.GatewayCloudCode {
			continue
		}
		gw, ok := gateways[id]
		if !ok {
			t.Errorf("known gateway %q has no entry", id)
			continue
		}
		if gw.match == nil || gw.forward == nil {
			t.Errorf("gateway %q lacks a matcher or a forward", id)
		}
	}
	for id := range gateways {
		if !config.IsKnownGatewayID(id) {
			t.Errorf("entry for unknown gateway %q", id)
		}
	}
}
```

In `internal/api/management_auth_test.go`, replace line 402:

```go
		// count_tokens answers a body with no messages itself, with a 400.
		{http.MethodPost, "/v1/messages/count_tokens", http.StatusBadRequest, false},
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api -run 'TestCountTokens_|TestRouteGateway_|TestGateways_|TestGateScopeIsTheManagementSurface' -count=1`
Expected: FAIL to build: `server.routeGateway undefined (type *Server has no field or method routeGateway)` and `undefined: gateways`.

- [ ] **Step 3: Write the implementation**

Replace `internal/api/dispatch.go:1-128` with:

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strconv"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/config"
)

// gatewayRequest carries what one /v1/messages forward needs. The forward of
// the gateway routeGateway picked writes the response.
type gatewayRequest struct {
	writer  http.ResponseWriter
	request *http.Request
	cfg     config.Config
	body    map[string]any
	rawBody []byte
	mutated bool
	model   string
}

// gatewayRoute is the gateway that claims a model and what its matcher found:
// target is the model id that gateway is sent, and kimi, zen and endpoint hold
// the matched allowlist entry or endpoint its forward reads.
type gatewayRoute struct {
	id       config.GatewayID
	target   string
	kimi     config.KimiModelConfig
	zen      config.ZenModelConfig
	endpoint config.EndpointConfig
}

// gateway is one gateway this build can dispatch to. match decides whether the
// gateway claims a model and writes nothing, so count_tokens asks the same
// question /v1/messages does; forward sends a /v1/messages request to the
// gateway match claimed it for.
type gateway struct {
	match   func(server *Server, ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool)
	forward func(server *Server, g *gatewayRequest, route gatewayRoute)
}

// gateways maps every gateway this build can dispatch to onto its matcher and
// forward. cloudcode is deliberately absent: it is the terminal account-backed
// path, not a gateway. TestGateways_CoverEveryKnownGateway pins that this table
// covers exactly config.KnownGatewayIDs() minus cloudcode.
var gateways = map[config.GatewayID]gateway{
	config.GatewayKimi:       {(*Server).matchKimiGateway, (*Server).forwardKimiGateway},
	config.GatewayZen:        {(*Server).matchZenGateway, (*Server).forwardZenGateway},
	config.GatewayClaudeCode: {(*Server).matchClaudeCodeGateway, (*Server).forwardClaudeCodeGateway},
	config.GatewayOpenRouter: {(*Server).matchOpenRouterGateway, (*Server).forwardOpenRouterGateway},
	config.GatewayCustom:     {(*Server).matchCustomEndpointGateway, (*Server).forwardCustomEndpointGateway},
}

// routeGateway walks the configured precedence and returns the first gateway
// that claims model. ok is false when cloudcode is reached or every listed
// gateway declines: the request then belongs to the account-backed path.
// Nothing is sent or written.
//
// Each matcher re-checks its own enabled flag, its own allowlist and (for Zen)
// its own key resolution; the walk is responsible for order only. An operator
// who hoists `cloudcode` ahead of a gateway is choosing to have the
// classifier-fallback gate consulted first: the walk stops at cloudcode and the
// caller falls through to the account-backed path (and its gate).
func (server *Server) routeGateway(ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	for _, id := range cfg.GatewayOrder.Effective(model) {
		if id == config.GatewayCloudCode {
			return gatewayRoute{}, false
		}
		gw, ok := gateways[id]
		if !ok {
			continue
		}
		if route, ok := gw.match(server, ctx, cfg, model); ok {
			route.id = id
			return route, true
		}
	}
	return gatewayRoute{}, false
}

// dispatchAlternateBackend hands a /v1/messages request to the gateway
// routeGateway picks and returns true once that gateway wrote the response. It
// returns false, having written nothing, when the request belongs to the
// account-backed path.
func (server *Server) dispatchAlternateBackend(g *gatewayRequest) bool {
	route, ok := server.routeGateway(g.request.Context(), g.cfg, g.model)
	if !ok {
		return false
	}
	gateways[route.id].forward(server, g, route)
	return true
}

func (server *Server) matchKimiGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.Kimi.Enabled {
		return gatewayRoute{}, false
	}
	entry, ok := matchKimiModelEntry(cfg.Kimi, model)
	if !ok {
		return gatewayRoute{}, false
	}
	return gatewayRoute{target: stripKimi1mSuffix(entry.ID), kimi: entry}, true
}

func (server *Server) forwardKimiGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal Kimi request: "+err.Error())
		return
	}
	reqBody = applyMaxTokensPolicy(reqBody, g.body, route.kimi.MaxOutputTokens, 0)
	server.forwardToKimi(g.writer, g.request, g.cfg.Kimi, reqBody, route.target)
}

func (server *Server) matchZenGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.Zen.Enabled {
		return gatewayRoute{}, false
	}
	entry, ok := matchZenModelEntry(cfg.Zen, model)
	if !ok {
		return gatewayRoute{}, false
	}
	return gatewayRoute{target: zenTargetModel(entry), zen: entry}, true
}

func (server *Server) forwardZenGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error",
			"Failed to marshal Zen request: "+err.Error())
		return
	}
	server.forwardToZen(g.writer, g.request, g.cfg.Zen, reqBody, g.body, route.target, route.zen)
}

func (server *Server) matchClaudeCodeGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.ClaudeCode.Enabled {
		return gatewayRoute{}, false
	}
	target := matchClaudeCodeModel(cfg.ClaudeCode, model)
	return gatewayRoute{target: target}, target != ""
}

func (server *Server) forwardClaudeCodeGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	// The Anthropic gateway requires max_tokens; derive it from the
	// allowlist entry (defaults carry each model's max output) when
	// the client omitted it. Client values are clamped down, never up.
	ccBody, err := claudeCodeGatewayBody(g, route.target, claudeCodeEntryMaxOutput(g.cfg.ClaudeCode, route.target))
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal ClaudeCode request: "+err.Error())
		return
	}
	server.forwardToClaudeCode(g.writer, g.request, g.cfg.ClaudeCode, ccBody, route.target)
}
```

Replace `internal/api/dispatch.go:220-258` (`tryOpenRouterGateway`, `tryCustomEndpointGateway`) with:

```go
func (server *Server) matchOpenRouterGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.OpenRouter.Enabled {
		return gatewayRoute{}, false
	}
	for _, item := range cfg.OpenRouter.Allowlist {
		if item.Enabled && (item.ID == model || (item.Alias != "" && item.Alias == model)) {
			return gatewayRoute{target: item.ID}, true
		}
	}
	return gatewayRoute{}, false
}

func (server *Server) forwardOpenRouterGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
		return
	}
	server.forwardToOpenRouter(g.writer, g.request, g.cfg.OpenRouter, reqBody, g.body)
}

func (server *Server) matchCustomEndpointGateway(ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	endpoint, ok := server.resolveCustomEndpoint(ctx, cfg, model)
	if !ok {
		return gatewayRoute{}, false
	}
	return gatewayRoute{target: model, endpoint: endpoint}, true
}

func (server *Server) forwardCustomEndpointGateway(g *gatewayRequest, route gatewayRoute) {
	// Nothing rewrote the request: forward the client's exact bytes.
	reqBody := g.rawBody
	if g.mutated {
		var err error
		if reqBody, err = json.Marshal(g.body); err != nil {
			writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
			return
		}
	}
	server.forwardToCustomEndpoint(g.writer, g.request, route.endpoint, g.model, reqBody)
}
```

In `internal/api/discovery.go:410-411`, change `the same condition tryCustomEndpointGateway uses to route` to `the same condition matchCustomEndpointGateway uses to route`.

Create `internal/api/count_tokens.go`:

```go
package api

import (
	"encoding/json"
	"io"
	"net/http"

	"antigravity-go-proxy/internal/config"
)

// countTokens answers POST /v1/messages/count_tokens with {"input_tokens": N}.
//
// A count takes the /v1/messages route: model mapping, then the gateway
// precedence, which decides the model id the count is for. Headroom and the
// classifier do not run: a count measures the request as the client wrote it.
// The count is estimated from what the conversation really cost so far
// (internal/tokencount). Only a body the proxy cannot read is refused.
func (server *Server) countTokens(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	rawBody, err := io.ReadAll(request.Body)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
		return
	}
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}
	if _, ok := body["messages"].([]any); !ok {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "messages is required and must be an array")
		return
	}
	model := server.resolveModelMapping(stringFrom(body["model"]))
	session := ccExtractSessionID(request, body)
	if route, routed := server.routeGateway(request.Context(), config.Get(), model); routed {
		model = route.target
	}
	tokens := server.tokens.Estimate(session, model, body)
	server.logger.Debug("count_tokens estimate", "session_id", session, "model", model, "input_tokens", tokens)
	writeJSON(writer, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// observeTokens gives the estimator the real input count of a completed
// request: its uncached input plus cache reads and writes. An answer that
// reported no usage teaches nothing, and a bare test Server has no estimator.
func (server *Server) observeTokens(session, model string, request map[string]any, realInput int) {
	if server.tokens == nil || request == nil || realInput <= 0 {
		return
	}
	server.tokens.Observe(session, model, request, realInput)
}
```

In `internal/api/server.go`:

1. Import block (`:29-53`): add `"antigravity-go-proxy/internal/tokencount"` between the `stats` and `transport` imports.
2. `Server` struct, after `ccReplies *ccidentity.ReplyLog` (`:160`):

```go

	// tokens learns, from completed Cloud Code and Zen requests, each model
	// family's chars-per-token ratio and each conversation's last real input
	// count; count_tokens estimates from it.
	tokens *tokencount.Counter
```

3. `New` (`:213-224`): add `tokens: tokencount.New(options.Now),` after `ccReplies: ccidentity.NewReplyLog(),` (gofmt aligns it).
4. `serveHTTP` (`:523-524`), replace the 501 case with:

```go
		case path == "/v1/messages/count_tokens" && request.Method == http.MethodPost:
			server.countTokens(writer, request)
```

5. Replace `zenUsageRecorder` and the first line of `zenInstrumentResponse` (`:1848-1871`):

```go
// zenUsageRecorder returns the usage callback shared by every Zen path:
// observability log, tracker accounting, and the count_tokens estimator.
func (server *Server) zenUsageRecorder(model, sessionID string, startTime time.Time, anthropicRequest map[string]any) func(in, out, cr, cw int) {
	return func(in, out, cr, cw int) {
		latency := server.nowTime().Sub(startTime)
		metrics := zen.RequestMetrics{
			Model:               model,
			SessionID:           sessionID,
			InputTokens:         in,
			OutputTokens:        out,
			CacheReadTokens:     cr,
			CacheCreationTokens: cw,
			Latency:             latency,
		}
		metrics.ComputeFinalMetrics()
		zen.LogObservability(server.logger, metrics)
		if server.tracker != nil {
			server.tracker.TrackRequest(model, latency, in, out, cr)
		}
		server.observeTokens(sessionID, model, anthropicRequest, in+cr+cw)
	}
}

func (server *Server) zenInstrumentResponse(resp *http.Response, model, sessionID string, startTime time.Time, anthropicRequest map[string]any) {
	onComplete := server.zenUsageRecorder(model, sessionID, startTime, anthropicRequest)
```

(the rest of `zenInstrumentResponse` is unchanged).

6. In `forwardToZen`, both calls (`:1812` and `:1825`) become `server.zenInstrumentResponse(resp, model, sessionKey, startTime, anthropicRequest)`.
7. In `unaryMessage` (`:3104`) and `streamMessage` (`:3199`), replace `sessionID := ccExtractSessionID(request, anthropicRequest)` with:

```go
	sessionID := ccExtractSessionID(request, anthropicRequest)
	server.observeTokens(sessionID, model, anthropicRequest, totalInput+totalCacheRead)
```

(`totalInput` is the uncached input — `InputTokens()` subtracts `cachedContentTokenCount` — so adding `totalCacheRead` gives the real count; Cloud Code reports no cache writes.)

In `internal/api/upstream_body_test.go:92`, the Zen case becomes:

```go
		{"zen", "zen", func(s *Server, r *http.Response) { s.zenInstrumentResponse(r, "m", "s", time.Now(), nil) }},
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api -run 'TestCountTokens_|TestRouteGateway_|TestGateways_|TestGateScopeIsTheManagementSurface|TestDispatch_|TestInstrumentedUpstreamBodyFailureIsNotAShort200|TestLoggingMiddleware' -count=1`
Expected: PASS (the `TestDispatch_*` tests guard the match/forward split).
Then: `go test ./internal/api -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/count_tokens.go internal/api/count_tokens_test.go internal/api/dispatch.go internal/api/dispatch_test.go internal/api/discovery.go internal/api/server.go internal/api/upstream_body_test.go internal/api/management_auth_test.go
git commit -m "feat(api): answer count_tokens from the gateway route and the token estimator"
```

---

### Task 11: Claude Code count_tokens identity and forward, with the forward machinery

**Files:**
- Modify: `internal/ccidentity/profile.go:101-126` (`Profile` gains `Static`)
- Modify: `internal/ccidentity/apply.go:43` (`ApplyHeaders` reads `p.Static`)
- Modify: `internal/ccidentity/defaults.go:1-6` (import `slices`), `:305-333` (profile values, count profile; `StaticFor` removed)
- Modify: `internal/ccidentity/defaults_test.go:12-13` (comment names `Profile.Static`)
- Modify: `internal/claudecode/client.go:339-485` (count request type and method; non-normalized headers shared)
- Modify: `internal/api/count_tokens.go` (forward machinery, Claude Code forward)
- Modify: `internal/api/server.go` (`Server` field `countMarks`, after the `tokens` field the count_tokens route task added)
- Modify: `scripts/client_drift.py:670-674` (new pin after `claude.betas`)
- Modify: `scripts/test_client_drift.py:37-47`
- Test: `internal/claudecode/count_tokens_test.go`, `internal/api/count_tokens_test.go`, `scripts/test_client_drift.py`

**Interfaces:**
- Consumes: from the count_tokens route task, `gatewayRoute`, `routeGateway`, `countTokens`, `Server.tokens`, and the test helpers `newCountServer`, `sendInSession`, `countOf`, `countPath`. Existing: `ccPoolConfig(baseURL string, accounts ...claudecode.AccountConfig) claudecode.Config` (`ccidentity_wire_test.go:334`), `resetCCPoolForTest()`, `(*Server).getOrCreateCCPool`, `poolReleasingBody`, `(*claudecode.AccountPool).SelectAccount/Acquire/Release/RecordRateLimit`, `claudecode.ExtractRateLimits`, `(*Server).noteClaudeCodeRateLimits`.
- Produces:
  - `ccidentity`: `Profile.Static func(Identity) []Header` (replaces `StaticFor`, removed); `const CountTokensPath = "/v1/messages/count_tokens?beta=true"`; `var CountTokensBetas []string`; `func CountTokensProfile() Profile`. Header application is the existing `ApplyHeaders(h, CountTokensProfile(), id, Turn{})`; there is no body applier for counts.
  - `claudecode`: `type CountTokensRequest struct { Token string; Body []byte; ClientHeaders http.Header; Normalize bool; Identity ccidentity.Identity }`; `func (r CountTokensRequest) Normalizes() bool`; `func (c *Client) CountTokens(ctx context.Context, req CountTokensRequest) (*http.Response, error)`.
  - `api`: `type countCall struct { request *http.Request; cfg config.Config; route gatewayRoute; body []byte; session string }`; `var countForwards map[config.GatewayID]func(*Server, countCall) (*http.Response, error)`; `func (server *Server) forwardCount(request *http.Request, cfg config.Config, route gatewayRoute, session string, rawBody []byte) (int, bool)`; `type countMarks struct{...}` with `unsupported(key string, now time.Time) bool` and `mark(key string, now time.Time)`; `func countMarkKey(route gatewayRoute) string`; `func countBody(rawBody []byte, model string) ([]byte, error)`; `const countUnsupportedFor = time.Hour`; `Server` field `countMarks countMarks`.
  - test helpers: `type testClock`, `func estimateOf(t *testing.T, server *Server, session, model, body string) int`, `type countSeen`, `func newCountUpstream(t *testing.T, status int, answer string, header http.Header) *countUpstream`, `const ccCountBody`.
  - `scripts/client_drift.py`: pin `claude.count_tokens_betas`.

- [ ] **Step 1: Write the failing tests**

Create `internal/claudecode/count_tokens_test.go`. The first test is driven by the committed capture: it reads the first `count_tokens` record of `.reference/claude-code-count-tokens-20261107.jsonl` and compares the request the client writes on a raw socket with it.

```go
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
)

// countCapture is Claude Code 2.1.311's own count_tokens traffic, recorded
// from /context.
var countCapture = filepath.Join("..", "..", ".reference", "claude-code-count-tokens-20261107.jsonl")

// capturedCount is the first count_tokens request in countCapture: its path,
// its header names in wire order, and the value of every header the capture
// kept in clear (credentials are recorded redacted, as objects).
func capturedCount(t *testing.T) (path string, names []string, values map[string]string) {
	t.Helper()
	data, err := os.ReadFile(countCapture)
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Method  string              `json:"method"`
			Path    string              `json:"path"`
			Headers [][]json.RawMessage `json:"headers"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("capture record: %v", err)
		}
		if record.Method != http.MethodPost || !strings.HasPrefix(record.Path, "/v1/messages/count_tokens") {
			continue
		}
		values = make(map[string]string)
		for _, pair := range record.Headers {
			var name, value string
			if len(pair) != 2 || json.Unmarshal(pair[0], &name) != nil {
				t.Fatalf("capture header %s is not a [name, value] pair", pair)
			}
			names = append(names, name)
			if json.Unmarshal(pair[1], &value) == nil {
				values[name] = value
			}
		}
		return record.Path, names, values
	}
	t.Fatalf("%s holds no count_tokens request", countCapture)
	return "", nil, nil
}

// wireRequest is one request as a raw HTTP/1.1 listener read it.
type wireRequest struct {
	target string
	names  []string
	values map[string]string
	body   string
}

// rawCountServer answers one count with {"input_tokens":123} and hands back the
// request as the wire carried it: header order and name case are the subject,
// and net/http's server keeps neither.
func rawCountServer(t *testing.T) (string, <-chan wireRequest) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan wireRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		request, err := readWireRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		got <- request
		const answer = `{"input_tokens":123}`
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(answer), answer)
	}()
	return "http://" + ln.Addr().String(), got
}

func readWireRequest(br *bufio.Reader) (wireRequest, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return wireRequest{}, err
	}
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return wireRequest{}, fmt.Errorf("request line %q", line)
	}
	request := wireRequest{target: fields[1], values: make(map[string]string)}
	length := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return wireRequest{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, _ := strings.Cut(line, ": ")
		request.names = append(request.names, name)
		request.values[name] = value
		if strings.EqualFold(name, "Content-Length") {
			length, _ = strconv.Atoi(value)
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return wireRequest{}, err
	}
	request.body = string(body)
	return request, nil
}

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// A normalized count is held to the committed capture: the same header names
// in the same order, and the same value for every header that does not vary
// by request, machine or release. The body goes out as given.
func TestClient_CountTokensSendsTheCapturedIdentity(t *testing.T) {
	path, names, values := capturedCount(t)
	url, sent := rawCountServer(t)
	body := `{"messages":[{"role":"user","content":"hi"}],"model":"claude-sonnet-5","tools":[]}`
	identity, normalize := IdentityConfig{}.WireIdentity("", "session-count")
	resp, err := NewClient(url, nil).CountTokens(context.Background(), CountTokensRequest{
		Token: "sk-ant-oat01-count", Body: []byte(body), Normalize: normalize, Identity: identity,
		// A foreign caller's headers: none of them may reach the wire.
		ClientHeaders: http.Header{
			"User-Agent":          {"foreign/1.0"},
			"X-Stainless-Timeout": {"600"},
			"Anthropic-Beta":      {ccidentity.AdvisorToolBeta},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var got wireRequest
	select {
	case got = <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("no count_tokens request reached the listener")
	}
	if got.target != path {
		t.Errorf("request target = %q, want captured %q", got.target, path)
	}
	if !slices.Equal(got.names, names) {
		t.Errorf("header names in wire order:\n got %q\nwant %q", got.names, names)
	}
	// Values that vary per request (credential, ids, body length), per machine
	// (Host, X-Stainless-OS and -Arch) or per Claude Code release, which the
	// client-drift pins move from the messages capture (User-Agent,
	// X-Stainless-Package-Version, X-Stainless-Runtime-Version).
	varying := map[string]bool{
		"Authorization": true, "Host": true, "Content-Length": true,
		"X-Claude-Code-Session-Id": true, "x-client-request-id": true,
		"X-Stainless-OS": true, "X-Stainless-Arch": true,
		"User-Agent": true, "X-Stainless-Package-Version": true, "X-Stainless-Runtime-Version": true,
	}
	for _, name := range names {
		if varying[name] {
			continue
		}
		if got.values[name] != values[name] {
			t.Errorf("%s = %q, want captured %q", name, got.values[name], values[name])
		}
	}
	if got.values["Authorization"] != "Bearer sk-ant-oat01-count" {
		t.Errorf("Authorization = %q, want the account's bearer token", got.values["Authorization"])
	}
	for _, name := range []string{"X-Claude-Code-Session-Id", "x-client-request-id"} {
		if !uuidShape.MatchString(got.values[name]) {
			t.Errorf("%s = %q, want a UUID", name, got.values[name])
		}
	}
	if got.values["User-Agent"] != ccidentity.MessagesUserAgent {
		t.Errorf("User-Agent = %q, want the messages path's %q", got.values["User-Agent"], ccidentity.MessagesUserAgent)
	}
	if got.values["Content-Length"] != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got.values["Content-Length"], len(body))
	}
	if got.body != body {
		t.Errorf("body = %s, want the caller's bytes %s", got.body, body)
	}
}

// An API key never claims the OAuth identity, on counts as on messages: the
// request goes to the plain path with the caller's betas.
func TestClient_CountTokensWithAnAPIKeyClaimsNoIdentity(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotApp, gotClass, gotBeta, gotVersion, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotKey = r.Header.Get("x-api-key")
		gotApp = r.Header.Get("x-app")
		gotClass = r.Header.Get("x-claude-code-request-class")
		gotBeta = r.Header.Get("anthropic-beta")
		gotVersion = r.Header.Get("anthropic-version")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":5}`)
	}))
	defer server.Close()

	body := `{"model":"claude-sonnet-5","messages":[]}`
	identity, _ := IdentityConfig{}.WireIdentity("", "session-count")
	resp, err := NewClient(server.URL, nil).CountTokens(context.Background(), CountTokensRequest{
		Token: "sk-ant-api03-key", Body: []byte(body), Normalize: true, Identity: identity,
		ClientHeaders: http.Header{"Anthropic-Beta": {"token-counting-2024-11-01"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotPath != "/v1/messages/count_tokens" || gotQuery != "" {
		t.Errorf("path = %q?%q, want /v1/messages/count_tokens with no query", gotPath, gotQuery)
	}
	if gotKey != "sk-ant-api03-key" {
		t.Errorf("x-api-key = %q, want the API key", gotKey)
	}
	if gotApp != "" || gotClass != "" {
		t.Errorf("x-app %q, request class %q: an API-key count claimed the Claude Code identity", gotApp, gotClass)
	}
	if gotBeta != "token-counting-2024-11-01" || gotVersion != DefaultAnthropicVersion {
		t.Errorf("anthropic-beta %q, anthropic-version %q: want the caller's beta and the default version", gotBeta, gotVersion)
	}
	if gotBody != body {
		t.Errorf("body = %s, want %s", gotBody, body)
	}
}
```

Add to `internal/api/count_tokens_test.go` (the import block becomes the one below; the count_tokens route task's tests are unchanged):

```go
import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)
```

```go
// ccCountBody is a count body as Claude Code's /context sends it (messages,
// model, tools) plus a system prompt and spacing the forward must keep: the
// body goes upstream as the client wrote it.
const ccCountBody = `{"model":"claude-sonnet-5", "system":"be brief <ok>","messages":[{"role":"user","content":"hi & bye"}],"tools":[]}`

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// estimateOf is what server's estimator answers for body: the count a request
// that is not forwarded gets.
func estimateOf(t *testing.T, server *Server, session, model, body string) int {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	return server.tokens.Estimate(session, model, decoded)
}

// countSeen is one count_tokens request an upstream received.
type countSeen struct {
	path, query string
	header      http.Header
	body        string
}

// countUpstream answers every count_tokens request with one status, body and
// header set, and records what it was sent. Other paths (OpenRouter's catalog
// warmup) get 404 and are not recorded.
type countUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen []countSeen
}

func newCountUpstream(t *testing.T, status int, answer string, header http.Header) *countUpstream {
	t.Helper()
	upstream := &countUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/count_tokens") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.seen = append(upstream.seen, countSeen{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: string(body)})
		upstream.mu.Unlock()
		for name, values := range header {
			w.Header()[name] = values
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *countUpstream) requests() []countSeen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]countSeen(nil), u.seen...)
}

// ccCountAccount is the OAuth pool account of the Claude Code count tests.
var ccCountAccount = claudecode.AccountConfig{ID: "acc1", Token: "sk-ant-oat01-count", Enabled: true}

func newCCCountServer(t *testing.T, upstreamURL string, now func() time.Time) (*Server, config.Config) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.ClaudeCode = ccPoolConfig(upstreamURL, ccCountAccount)
	return newCountServer(t, cfg, now), cfg
}

// ccCountAccountState is the pool's view of ccCountAccount.
func ccCountAccountState(t *testing.T, server *Server, cfg config.Config) claudecode.AccountSnapshot {
	t.Helper()
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	acc, ok := pool.GetAccount(ccCountAccount.ID)
	if !ok {
		t.Fatalf("pool has no account %q", ccCountAccount.ID)
	}
	return acc.Snapshot()
}

// The Claude Code count goes out with the count identity and the client's own
// bytes, and its answer is the client's answer. Its account is released, and
// the pool's rate-limit state is not read from a count, even one that carried
// unified headers.
func TestCountTokens_ClaudeCodeForwardsTheClientsBody(t *testing.T) {
	upstream := newCountUpstream(t, http.StatusOK, `{"input_tokens":4242}`, http.Header{
		"Anthropic-Ratelimit-Unified-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":  {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
	})
	server, cfg := newCCCountServer(t, upstream.URL, time.Now)

	got := countOf(t, sendInSession(server.Handler(), countPath, "test-proxy-key", "sess-cc", ccCountBody,
		http.Header{"User-Agent": {"foreign/1.0"}, "Anthropic-Beta": {"foreign-beta"}}))
	if got != 4242 {
		t.Errorf("count = %d, want the upstream's 4242", got)
	}
	seen := upstream.requests()
	if len(seen) != 1 {
		t.Fatalf("upstream got %d count requests, want 1", len(seen))
	}
	r := seen[0]
	if r.path != "/v1/messages/count_tokens" || r.query != "beta=true" {
		t.Errorf("upstream target = %s?%s, want /v1/messages/count_tokens?beta=true", r.path, r.query)
	}
	if r.body != ccCountBody {
		t.Errorf("forwarded body:\n%s\nwant the client's bytes:\n%s", r.body, ccCountBody)
	}
	for name, want := range map[string]string{
		"Authorization":               "Bearer sk-ant-oat01-count",
		"x-api-key":                   "",
		"x-claude-code-request-class": "auxiliary",
		"anthropic-beta":              strings.Join(ccidentity.CountTokensBetas, ","),
		"User-Agent":                  ccidentity.MessagesUserAgent,
		"X-Stainless-Timeout":         "",
		"x-claude-code-prompt-id":     "",
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	state := ccCountAccountState(t, server, cfg)
	if state.Status != "healthy" || state.RateLimits.Unified != nil {
		t.Errorf("account status %q, unified %+v: a count must not set rate-limit state", state.Status, state.RateLimits.Unified)
	}
	if state.InFlight != 0 {
		t.Errorf("account in flight = %d, want 0: the count did not release it", state.InFlight)
	}
}

// /context sends about 34 counts at once on one session, so on one account.
// The upstream answers none until all 34 have arrived: a forward that
// serialized them would leave the first waiting, and it gives up after 5 s so
// a failure costs one wait, not 34.
func TestCountTokens_ClaudeCodeBurstIsNotSerialized(t *testing.T) {
	const burst = 34
	var arrived atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == burst {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			releaseOnce.Do(func() { close(release) })
			http.Error(w, "the burst arrived serialized", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":777}`)
	}))
	t.Cleanup(upstream.Close)
	server, cfg := newCCCountServer(t, upstream.URL, time.Now)
	handler := server.Handler()

	recorders := make([]*httptest.ResponseRecorder, burst)
	var wg sync.WaitGroup
	for i := range burst {
		wg.Go(func() {
			recorders[i] = sendInSession(handler, countPath, "test-proxy-key", "sess-burst", ccCountBody, nil)
		})
	}
	wg.Wait()
	for i, recorder := range recorders {
		if got := countOf(t, recorder); got != 777 {
			t.Errorf("count %d = %d, want the upstream's 777: the burst was serialized", i, got)
		}
	}
	if n := arrived.Load(); n != burst {
		t.Errorf("upstream saw %d counts, want %d", n, burst)
	}
	if state := ccCountAccountState(t, server, cfg); state.InFlight != 0 {
		t.Errorf("account in flight = %d after the burst, want 0", state.InFlight)
	}
}

// A 429 on a count cools the account down as one on /v1/messages does; the
// count itself is estimated, and so is the next, without asking again.
func TestCountTokens_ClaudeCode429CoolsTheAccountDown(t *testing.T) {
	upstream := newCountUpstream(t, http.StatusTooManyRequests,
		`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, http.Header{"Retry-After": {"120"}})
	server, cfg := newCCCountServer(t, upstream.URL, time.Now)
	handler := server.Handler()
	want := estimateOf(t, server, "sess-cc", "claude-sonnet-5", ccCountBody)

	for i := range 2 {
		if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "sess-cc", ccCountBody, nil)); got != want {
			t.Errorf("count %d = %d, want the estimate %d", i, got, want)
		}
	}
	if n := len(upstream.requests()); n != 1 {
		t.Errorf("upstream asked %d times, want 1: a cooled-down account was asked again", n)
	}
	state := ccCountAccountState(t, server, cfg)
	if state.Status != "cooldown" {
		t.Errorf("account status = %q, want cooldown", state.Status)
	}
	if left := time.Until(state.CooldownUntil); left < 100*time.Second {
		t.Errorf("cooldown ends in %v, want the 120 s Retry-After", left)
	}
}

// 404, 405 and 501 say the upstream does not count tokens: the count is
// estimated, and the gateway is not asked again for an hour.
func TestCountTokens_UnsupportedUpstreamIsEstimatedForAnHour(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			upstream := newCountUpstream(t, status, `{"type":"error"}`, nil)
			clock := &testClock{now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
			server, _ := newCCCountServer(t, upstream.URL, clock.Now)
			handler := server.Handler()
			want := estimateOf(t, server, "", "claude-sonnet-5", ccCountBody)
			for i, step := range []struct {
				advance time.Duration
				asked   int
			}{
				{0, 1},                // asked, answered unsupported, marked
				{59 * time.Minute, 1}, // still marked: estimated without a call
				{time.Minute, 2},      // the hour is over: asked again
			} {
				clock.Advance(step.advance)
				if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "", ccCountBody, nil)); got != want {
					t.Errorf("step %d: count = %d, want the estimate %d", i, got, want)
				}
				if n := len(upstream.requests()); n != step.asked {
					t.Errorf("step %d: upstream asked %d times, want %d", i, n, step.asked)
				}
			}
		})
	}
}

// Any other failure is estimated for that request only: the next count asks
// the upstream again.
func TestCountTokens_FailedForwardEstimatesThatRequestOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		answer string
	}{
		{"server error", http.StatusInternalServerError, `{"type":"error"}`},
		{"bad gateway", http.StatusBadGateway, ``},
		{"unauthorized", http.StatusUnauthorized, `{"type":"error"}`},
		{"bad request", http.StatusBadRequest, `{"type":"error"}`},
		{"no count in a 200", http.StatusOK, `{"tokens":12}`},
		{"a 200 that is not JSON", http.StatusOK, `nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCountUpstream(t, tc.status, tc.answer, nil)
			server, _ := newCCCountServer(t, upstream.URL, time.Now)
			handler := server.Handler()
			want := estimateOf(t, server, "", "claude-sonnet-5", ccCountBody)
			for i := range 2 {
				if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "", ccCountBody, nil)); got != want {
					t.Errorf("count %d = %d, want the estimate %d", i, got, want)
				}
			}
			if n := len(upstream.requests()); n != 2 {
				t.Errorf("upstream asked %d times, want 2: this failure must not mark the gateway", n)
			}
		})
	}
}
```

In `scripts/test_client_drift.py`, replace `test_beta_list_survives_a_write_into_the_go_source` (`:37-47`) with:

```python
    def test_beta_lists_survive_a_write_into_the_go_source(self):
        source = "internal/ccidentity/defaults.go"
        lists = ("claude.betas", "claude.count_tokens_betas")
        for name in lists:
            with self.subTest(pin=name):
                tree = Path(tempfile.mkdtemp())
                self.addCleanup(shutil.rmtree, tree)
                (tree / source).parent.mkdir(parents=True)
                shutil.copy(ROOT / source, tree / source)
                pin = drift.PIN[name]
                site_only = drift.Pin(pin.name, pin.client, pin.pattern, pin.sites, decode=pin.decode, encode=pin.encode)
                betas = drift.read_pin(tree, site_only) + ["new-beta-2026-10-29"]
                drift.write_pin(tree, site_only, betas)
                self.assertEqual(drift.read_pin(tree, site_only), betas)
                # The two lists sit in one file; writing one must not touch the other.
                other = drift.PIN[lists[1] if name == lists[0] else lists[0]]
                self.assertEqual(drift.read_pin(tree, other), drift.read_pin(ROOT, other))
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/claudecode -run 'TestClient_CountTokens' -count=1`
Expected: FAIL to build: `undefined: CountTokensRequest` and `NewClient(...).CountTokens undefined`.
Run: `go test ./internal/api -run 'TestCountTokens_ClaudeCode|TestCountTokens_UnsupportedUpstream|TestCountTokens_FailedForward' -count=1`
Expected: FAIL to build: `undefined: ccidentity.CountTokensBetas`.
Run: `python3 -m unittest discover -s scripts -p 'test_client_drift.py' -k beta_lists`
Expected: FAIL with `KeyError: 'claude.count_tokens_betas'`.

- [ ] **Step 3: Write the implementation**

`internal/ccidentity/profile.go`, replace `:101-126` (the `Profile` doc and type):

```go
// Profile is a captured Claude Code request identity.
//
// The shape is a list of headers plus an explicit omit list rather than named
// fields per header, because the capture decides what exists: a named-field
// struct would need editing every time the client grows or drops a header.
//
// The constant header set is a function of the identity, not a list: three of
// its captured values depend on the identity or on operator overrides
// (User-Agent, X-Stainless-OS, X-Stainless-Runtime-Version). The metadata key
// order is not a field: it is fixed by the declaration order of the struct
// that encodes it.
type Profile struct {
	// Static returns the constant header set for one identity.
	Static func(Identity) []Header
	// Dynamic headers are regenerated on every request.
	Dynamic []DynamicHeader
	// Omit lists header names quiet Claude Code never sends. The applier deletes
	// them, so a foreign client's value cannot reach the upstream. This is the
	// leak guard and it is not optional: without it, a Cursor or Codex request
	// carries its own User-Agent and x-stainless family straight through.
	Omit []string
	// Path is the request path, including the query string the capture shows.
	Path string
	// Betas is the captured anthropic-beta list in captured order. It replaces
	// whatever the client sent rather than being appended to: the captured order
	// puts claude-code-20250219 ahead of oauth-2025-04-20, which append-only
	// cannot produce. The one client beta kept is added by WithClientBetas.
	Betas []string
}
```

`internal/ccidentity/apply.go:43`: `static := StaticFor(id)` becomes `static := p.Static(id)`.

`internal/ccidentity/defaults.go`: the import block (`:3-6`) gains `"slices"`. Replace `:305-333` (`defaultProfile`, `DefaultProfile`, `StaticFor`) with:

```go
// defaultProfile is the captured profile. Nothing in it varies per call:
// Static and the dynamicHeaders closures take the identity as an argument
// rather than closing over one, and Omit, Path and Betas are constants. It is
// therefore built once instead of on every request.
//
// Omit and Betas are shared with every caller, as they already were when this
// was a constructor. Neither is mutated anywhere; defaults_test.go pins the
// sharing, so a caller that started to mutate one would be caught.
var defaultProfile = Profile{
	Static:  staticHeaders,
	Dynamic: dynamicHeaders(),
	Omit:    omittedHeaders,
	Path:    MessagesPath,
	Betas:   Betas,
}

// DefaultProfile returns the captured identity.
func DefaultProfile() Profile {
	return defaultProfile
}

// CountTokensPath is the count_tokens path with the query string the capture
// shows (.reference/claude-code-count-tokens-20261107.jsonl).
const CountTokensPath = "/v1/messages/count_tokens?beta=true"

// CountTokensBetas is the anthropic-beta list Claude Code sends on
// POST /v1/messages/count_tokens, in captured order. It is its own list, not a
// rule over Betas: the count capture carries five betas, the last of them
// token-counting, which the messages list does not have.
var CountTokensBetas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"interleaved-thinking-2025-05-14",
	"context-management-2025-06-27",
	"token-counting-2024-11-01",
}

// countTokensStaticHeaders is the constant set as the count capture differs
// from the messages one: no X-Stainless-Timeout, and request class auxiliary.
// Every other name and value is the messages one, sent by the same SDK.
// staticHeaders returns a fresh slice, so it is filtered in place.
func countTokensStaticHeaders(id Identity) []Header {
	static := staticHeaders(id)
	out := static[:0]
	for _, header := range static {
		switch header.Name {
		case "X-Stainless-Timeout":
			continue
		case "x-claude-code-request-class":
			header.Value = "auxiliary"
		}
		out = append(out, header)
	}
	return out
}

// countTokensDynamicHeaders is the messages dynamic set without
// x-claude-code-prompt-id: a count belongs to /context, not to a prompt, and
// the capture carries none.
func countTokensDynamicHeaders() []DynamicHeader {
	return slices.DeleteFunc(dynamicHeaders(), func(header DynamicHeader) bool {
		return header.Name == "x-claude-code-prompt-id"
	})
}

// countTokensProfile is the captured count_tokens identity, built once like
// defaultProfile.
var countTokensProfile = Profile{
	Static:  countTokensStaticHeaders,
	Dynamic: countTokensDynamicHeaders(),
	Omit:    omittedHeaders,
	Path:    CountTokensPath,
	Betas:   CountTokensBetas,
}

// CountTokensProfile returns the identity Claude Code sends on
// POST /v1/messages/count_tokens. It is applied with ApplyHeaders; a count
// body is never rewritten, so it has no ApplyBody counterpart.
func CountTokensProfile() Profile {
	return countTokensProfile
}
```

`internal/ccidentity/defaults_test.go:12-13`: the last sentence of the comment becomes `The values that do vary with the identity come from Profile.Static, which is a function.`

`internal/claudecode/client.go`: in `SendMessage`, replace the whole `} else {` branch of `if normalize` (`:440-476`) with:

```go
	} else {
		setForwardedHeaders(httpReq.Header, req.ClientHeaders)
	}
```

and add after `SendMessage` (after `:485`):

```go
// setForwardedHeaders sets what a request that is not normalized carries:
// JSON content, the captured User-Agent, and the caller's anthropic-version
// (or the default), anthropic-beta and Accept.
//
// The User-Agent is set here too. Go's transport writes Go-http-client/1.1
// when none is set, which announces the standard library — the fingerprint
// this package exists to remove, and a worse one than the captured value. It
// does not make the claim the normalized branch makes: a version string
// carries no session, and the headers that do assert an OAuth Claude Code
// identity (x-app, the cc_* system block, the captured beta order) are not set.
func setForwardedHeaders(h, client http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", ccidentity.MessagesUserAgent)
	version := DefaultAnthropicVersion
	if v := client.Get("anthropic-version"); v != "" {
		version = v
	}
	h.Set("anthropic-version", version)
	for _, beta := range client.Values("anthropic-beta") {
		h.Add("anthropic-beta", beta)
	}
	if accept := client.Get("Accept"); accept != "" {
		h.Set("Accept", accept)
	}
}

// CountTokensRequest carries one /v1/messages/count_tokens send.
type CountTokensRequest struct {
	// Token authenticates the request, as on MessageRequest.
	Token string
	// Body is sent as given. Claude Code's count body is messages, model and
	// tools, with no billing block or metadata, so normalization adds nothing.
	Body []byte
	// ClientHeaders are the inbound headers. A request that does not normalize
	// copies anthropic-version, anthropic-beta and Accept from them.
	ClientHeaders http.Header
	// Normalize and Identity are MessageRequest's.
	Normalize bool
	Identity  ccidentity.Identity
}

// Normalizes is MessageRequest.Normalizes for a count: an API key never
// claims the captured OAuth identity.
func (r CountTokensRequest) Normalizes() bool {
	return r.Normalize && IsOAuthToken(r.Token)
}

// CountTokens sends a count_tokens request. When req.Normalizes() it carries
// the identity Claude Code sends on counts (ccidentity.CountTokensProfile),
// which is not the messages one. It goes out on the messages transport: the
// capture shows counts on the same TLS stack as /v1/messages.
func (c *Client) CountTokens(ctx context.Context, req CountTokensRequest) (*http.Response, error) {
	normalize := req.Normalizes()
	profile := ccidentity.CountTokensProfile()
	path := "/v1/messages/count_tokens"
	if normalize {
		path = profile.Path
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("failed to create count_tokens request: %w", err)
	}
	if normalize {
		ccidentity.ApplyHeaders(httpReq.Header, profile, req.Identity, ccidentity.Turn{})
	} else {
		setForwardedHeaders(httpReq.Header, req.ClientHeaders)
	}
	ApplyAuthHeaders(httpReq, req.Token)
	return c.httpClient.Do(httpReq)
}
```

`internal/api/server.go`, `Server` struct, after the `tokens` field:

```go

	// countMarks holds the gateways that answered count_tokens 404, 405 or
	// 501, until they are asked again. The zero value is ready.
	countMarks countMarks
```

`internal/api/count_tokens.go` becomes:

```go
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

// countUnsupportedFor is how long a gateway that answered count_tokens with
// 404, 405 or 501 is estimated without being asked.
const countUnsupportedFor = time.Hour

// maxCountResponse caps a count answer; Claude Code's is about 20 bytes.
const maxCountResponse = 64 << 10

// countTokens answers POST /v1/messages/count_tokens with {"input_tokens": N}.
//
// A count takes the /v1/messages route: model mapping, then the gateway
// precedence. Headroom and the classifier do not run: a count measures the
// request as the client wrote it. A gateway with a count endpoint is asked
// (forwardCount). Zen and Cloud Code have none, and a gateway that cannot
// answer is no error for the client, so those counts are estimated from what
// the conversation really cost so far (internal/tokencount). Only a body the
// proxy cannot read is refused.
func (server *Server) countTokens(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	rawBody, err := io.ReadAll(request.Body)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
		return
	}
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}
	if _, ok := body["messages"].([]any); !ok {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "messages is required and must be an array")
		return
	}
	model := server.resolveModelMapping(stringFrom(body["model"]))
	session := ccExtractSessionID(request, body)
	cfg := config.Get()
	if route, routed := server.routeGateway(request.Context(), cfg, model); routed {
		model = route.target
		if tokens, ok := server.forwardCount(request, cfg, route, session, rawBody); ok {
			writeJSON(writer, http.StatusOK, map[string]any{"input_tokens": tokens})
			return
		}
	}
	tokens := server.tokens.Estimate(session, model, body)
	server.logger.Debug("count_tokens estimate", "session_id", session, "model", model, "input_tokens", tokens)
	writeJSON(writer, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// observeTokens gives the estimator the real input count of a completed
// request: its uncached input plus cache reads and writes. An answer that
// reported no usage teaches nothing, and a bare test Server has no estimator.
func (server *Server) observeTokens(session, model string, request map[string]any, realInput int) {
	if server.tokens == nil || request == nil || realInput <= 0 {
		return
	}
	server.tokens.Observe(session, model, request, realInput)
}

// countCall is one count_tokens forward: the client's request, the gateway
// routeGateway chose, the body to send (the client's bytes with the gateway's
// model id written in) and the client's session.
type countCall struct {
	request *http.Request
	cfg     config.Config
	route   gatewayRoute
	body    []byte
	session string
}

// countForwards maps the gateways that count tokens themselves onto the call
// that asks them. Zen has no count endpoint and the account-backed path is not
// a gateway: both are estimated.
var countForwards = map[config.GatewayID]func(*Server, countCall) (*http.Response, error){
	config.GatewayClaudeCode: (*Server).countClaudeCode,
}

// forwardCount asks the gateway route names for the count. ok is false when
// the gateway does not count tokens, is marked as not counting, or the call
// failed in any way; the caller then estimates, so a count never fails the
// client for an upstream's sake. A 404, 405 or 501 marks the gateway (a custom
// endpoint: that endpoint) for countUnsupportedFor.
func (server *Server) forwardCount(request *http.Request, cfg config.Config, route gatewayRoute, session string, rawBody []byte) (int, bool) {
	send, ok := countForwards[route.id]
	if !ok {
		return 0, false
	}
	key := countMarkKey(route)
	if server.countMarks.unsupported(key, server.nowTime()) {
		return 0, false
	}
	body, err := countBody(rawBody, route.target)
	if err != nil {
		server.logger.Debug("count_tokens body not forwardable; estimating", "gateway", key, "error", err)
		return 0, false
	}
	resp, err := send(server, countCall{request: request, cfg: cfg, route: route, body: body, session: session})
	if err != nil {
		server.logger.Debug("count_tokens forward failed; estimating", "gateway", key, "error", err)
		return 0, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		server.countMarks.mark(key, server.nowTime())
		server.logger.Info("count_tokens not served upstream; estimating for an hour", "gateway", key, "status", resp.StatusCode)
		return 0, false
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		server.logger.Debug("count_tokens upstream error; estimating", "gateway", key, "status", resp.StatusCode)
		return 0, false
	}
	var counted struct {
		InputTokens *int `json:"input_tokens"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCountResponse)).Decode(&counted); err != nil || counted.InputTokens == nil || *counted.InputTokens < 0 {
		server.logger.Debug("count_tokens upstream answered no count; estimating", "gateway", key)
		return 0, false
	}
	return *counted.InputTokens, true
}

// countBody is the client's count body with the gateway's model id written
// into the client's own bytes; every other byte goes upstream as the client
// sent it.
func countBody(rawBody []byte, model string) ([]byte, error) {
	quoted, err := ccidentity.MarshalJS(model)
	if err != nil {
		return nil, fmt.Errorf("encode count model: %w", err)
	}
	return ccidentity.SetObjectMembers(rawBody, ccidentity.ObjectMember{Name: "model", Value: quoted})
}

// countMarkKey names what a mark covers: a custom endpoint by its URL, since
// one endpoint can count and another not; any other gateway by its id.
func countMarkKey(route gatewayRoute) string {
	if route.id == config.GatewayCustom {
		return string(config.GatewayCustom) + ":" + route.endpoint.URL
	}
	return string(route.id)
}

// countMarks remembers the gateways that do not count tokens, and until when.
// Keys come from the configuration, so the map stays small. The zero value is
// ready.
type countMarks struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// unsupported reports whether key is marked at now; an expired mark is dropped.
func (marks *countMarks) unsupported(key string, now time.Time) bool {
	marks.mu.Lock()
	defer marks.mu.Unlock()
	until, ok := marks.until[key]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(marks.until, key)
	return false
}

func (marks *countMarks) mark(key string, now time.Time) {
	marks.mu.Lock()
	defer marks.mu.Unlock()
	if marks.until == nil {
		marks.until = make(map[string]time.Time)
	}
	marks.until[key] = now.Add(countUnsupportedFor)
}

// countClaudeCode sends a count through the Claude Code pool with the count
// identity (claudecode.Client.CountTokens).
//
// Counts arrive in bursts of about 34 from one /context, so nothing is held
// across the call: SelectAccount and Acquire take the pool's locks briefly,
// and the token is not refreshed here (a refresh writes config.json under its
// lock; an expired token answers 401 and the count is estimated). A count
// answer carries no unified rate-limit headers, so only a 429 changes pool
// state, as it would on /v1/messages. The account is released when
// forwardCount closes the body.
func (server *Server) countClaudeCode(call countCall) (*http.Response, error) {
	pool, client := server.getOrCreateCCPool(call.cfg.ClaudeCode)
	acc, err := pool.SelectAccount(call.session, nil)
	if err != nil {
		return nil, fmt.Errorf("select Claude Code account: %w", err)
	}
	pool.Acquire(acc.ID)
	identity, normalize := call.cfg.ClaudeCode.Identity.WireIdentity(acc.AccountUUID, call.session)
	resp, err := client.CountTokens(call.request.Context(), claudecode.CountTokensRequest{
		Token:         acc.Token,
		Body:          call.body,
		ClientHeaders: call.request.Header,
		Normalize:     normalize,
		Identity:      identity,
	})
	if err != nil {
		pool.Release(acc.ID)
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		rl := claudecode.ExtractRateLimits(resp.Header)
		pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
		server.noteClaudeCodeRateLimits(acc.ID, rl)
	}
	resp.Body = &poolReleasingBody{ReadCloser: resp.Body, releaseFn: func() { pool.Release(acc.ID) }}
	return resp, nil
}
```

`scripts/client_drift.py`, insert after the `claude.betas` pin (after `:674`):

```python
    # Claude Code sends count_tokens only from /context, which the --print
    # capture never reaches, so no observe: a /context recapture moves it
    # (.reference/claude-code-count-tokens-*.jsonl).
    Pin("claude.count_tokens_betas", "claude", BETA_LIST, (
        Site("internal/ccidentity/defaults.go", "var CountTokensBetas = []string{\n<<V>>\n}"),
    ), decode=lambda text: re.findall(r'"([^"]+)"', text),
        encode=lambda values: "\n".join(f'\t"{value}",' for value in values)),
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/ccidentity ./internal/claudecode -count=1`
Expected: PASS (existing `TestApplyHeadersSendsTheCapturedSet` still sees request class `main` and `X-Stainless-Timeout` on messages).
Run: `go test ./internal/api -run 'TestCountTokens_' -count=1`
Expected: PASS.
Run: `python3 -m unittest discover -s scripts -p 'test_client_drift.py'`
Expected: `OK` (the registry test reads the new pin from this tree).

- [ ] **Step 5: Commit**

```bash
git add internal/ccidentity/profile.go internal/ccidentity/apply.go internal/ccidentity/defaults.go internal/ccidentity/defaults_test.go internal/claudecode/client.go internal/claudecode/count_tokens_test.go internal/api/count_tokens.go internal/api/count_tokens_test.go internal/api/server.go scripts/client_drift.py scripts/test_client_drift.py
git commit -m "feat(claudecode): forward count_tokens with Claude Code's captured count identity"
```

---

### Task 12: count_tokens forwards for OpenRouter, Kimi and custom endpoints

**Files:**
- Modify: `internal/kimi/passthrough.go:3-13` (imports `context`, `fmt`), `:74-92` (Director uses the shared header helper)
- Modify: `internal/api/server.go:2252-2267` (spoof defaults removed: `openrouter.ApplySpoofHeaders` already defaults empty values), `:2382-2411` (headers via helper), new `setOpenRouterHeaders` after `openRouterUpstreamClient` (`:2652-2658`)
- Modify: `internal/api/count_tokens.go` (`countForwards` entries, three count calls, `customCountURL`)
- Test: `internal/kimi/passthrough_test.go`, `internal/api/count_tokens_test.go`

**Interfaces:**
- Consumes: from the Claude Code count forward task, `countCall`, `countForwards`, `forwardCount`, marks, `ccidentity.CountTokensProfile()`, and the test helpers `newCountServer`, `sendInSession`, `countOf`, `estimateOf`, `newCountUpstream`, `countPath`. Existing: `customEndpointIdentity(endpoint config.EndpointConfig, sessionKey string) (ccidentity.Identity, bool)`, `withCapturedBetaQuery(rawQuery string) string`, `resolveCustomEndpointURL(endpointURL, requestPath string) (*url.URL, error)`, `customEndpointTransport`, `customEndpointIdentityTransport`, `(*Server).resolveKimiCredential`, `(*Server).kimiIdentityHeaders`, `openRouterUpstreamClient()`.
- Produces:
  - `kimi`: `func CountTokens(ctx context.Context, baseURL, apiKey string, body []byte, client, identity http.Header) (*http.Response, error)`
  - `api`: `func (server *Server) setOpenRouterHeaders(upReq *http.Request, client http.Header, cfg config.OpenRouterConfig) bool`; `func (server *Server) countOpenRouter(call countCall) (*http.Response, error)`; `func (server *Server) countKimi(call countCall) (*http.Response, error)`; `func (server *Server) countCustomEndpoint(call countCall) (*http.Response, error)`; `func customCountURL(endpointURL string) (*url.URL, bool)`; `var errNoCountRoute error`

- [ ] **Step 1: Write the failing tests**

Append to `internal/kimi/passthrough_test.go` (imports gain `context`, `strings`):

```go
// A count carries what a Kimi messages forward carries: the Bearer
// credential, the identity headers, and the caller's Anthropic headers, and
// never the x-api-key the caller sent the proxy.
func TestCountTokens_SendsTheForwardsHeaders(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeader = r.URL.Path, r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":9}`)
	}))
	defer upstream.Close()

	client := http.Header{}
	client.Set("anthropic-version", "2023-06-01")
	client.Set("anthropic-beta", "token-counting-2024-11-01")
	client.Set("x-api-key", "proxy-key")
	identity := http.Header{}
	identity.Set("User-Agent", "KimiCLI/"+CLIVersion)
	identity.Set("X-Msh-Platform", "kimi_cli")
	body := []byte(`{"model":"kimi-k2","messages":[]}`)

	resp, err := CountTokens(context.Background(), upstream.URL+"/coding/v1", "sk-kimi", body, client, identity)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if gotPath != "/coding/v1/messages/count_tokens" {
		t.Errorf("path = %q, want /coding/v1/messages/count_tokens", gotPath)
	}
	for name, want := range map[string]string{
		"Authorization":     "Bearer sk-kimi",
		"X-Api-Key":         "",
		"User-Agent":        "KimiCLI/" + CLIVersion,
		"X-Msh-Platform":    "kimi_cli",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    "token-counting-2024-11-01",
		"Content-Type":      "application/json",
	} {
		if got := gotHeader.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body = %s, want %s", gotBody, body)
	}
	if !strings.Contains(string(answer), `"input_tokens":9`) {
		t.Errorf("answer = %s, want the upstream's", answer)
	}
}
```

Append to `internal/api/count_tokens_test.go` (imports gain `"antigravity-go-proxy/internal/kimi"` and `"antigravity-go-proxy/internal/openrouter"`):

```go
// OpenRouter counts with the key and attribution its messages forward sends,
// for the model the route chose after the mapping.
func TestCountTokens_ForwardsToOpenRouter(t *testing.T) {
	upstream := newCountUpstream(t, http.StatusOK, `{"input_tokens":4321}`, nil)
	cfg := config.DefaultConfig()
	cfg.OpenRouter = config.OpenRouterConfig{
		Enabled: true, BaseURL: upstream.URL + "/api/v1", APIKey: "k-or",
		Allowlist: []config.OpenRouterModelConfig{{ID: "vendor/model-x", Alias: "model-x", Enabled: true}},
	}
	cfg.ModelMapping = map[string]any{"my-alias": "model-x"}
	server := newCountServer(t, cfg, time.Now)
	server.appSpoofActivated = true

	body := `{"model":"my-alias","messages":[{"role":"user","content":"a < b"}]}`
	got := countOf(t, sendInSession(server.Handler(), countPath, "test-proxy-key", "", body,
		http.Header{"Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"token-counting-2024-11-01"}}))
	if got != 4321 {
		t.Errorf("count = %d, want the upstream's 4321", got)
	}
	seen := upstream.requests()
	if len(seen) != 1 {
		t.Fatalf("upstream got %d count requests, want 1", len(seen))
	}
	r := seen[0]
	if r.path != "/api/v1/messages/count_tokens" {
		t.Errorf("path = %q, want /api/v1/messages/count_tokens", r.path)
	}
	for name, want := range map[string]string{
		"Authorization":                     "Bearer k-or",
		"X-Api-Key":                         "k-or",
		"Anthropic-Version":                 "2023-06-01",
		"Anthropic-Beta":                    "token-counting-2024-11-01",
		openrouter.SpoofAppTitleHeader:      openrouter.DefaultSpoofAppTitle,
		openrouter.SpoofAppRefererHeader:    openrouter.DefaultSpoofAppReferer,
		openrouter.SpoofAppCategoriesHeader: openrouter.DefaultSpoofAppCategories,
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if want := `{"model":"vendor/model-x","messages":[{"role":"user","content":"a < b"}]}`; r.body != want {
		t.Errorf("body = %s, want %s", r.body, want)
	}
}

func TestCountTokens_ForwardsToKimi(t *testing.T) {
	far := time.Now().Add(24 * time.Hour)
	for _, tc := range []struct {
		name               string
		oauth              bool
		wantPath, wantAuth string
		wantUA, wantMsh    string
	}{
		{"api key", false, "/anthropic/v1/messages/count_tokens", "Bearer k-kimi", "", ""},
		{"kimi code login", true, "/coding/v1/messages/count_tokens", "Bearer kimi-oauth-token", "KimiCLI/" + kimi.CLIVersion, "kimi_cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCountUpstream(t, http.StatusOK, `{"input_tokens":1111}`, nil)
			cfg := config.DefaultConfig()
			cfg.Kimi = config.KimiConfig{
				Enabled: true, BaseURL: upstream.URL + "/anthropic", APIKey: "k-kimi",
				Allowlist: []config.KimiModelConfig{{ID: "kimi-k2", Alias: "k2", Enabled: true}},
			}
			if tc.oauth {
				cfg.Kimi.OAuth = &config.KimiOAuthConfig{Token: "kimi-oauth-token", ExpiresAt: &far, BaseURL: upstream.URL + "/coding/v1"}
			}
			server := newCountServer(t, cfg, time.Now)

			body := `{"model":"k2","messages":[{"role":"user","content":"hi"}]}`
			got := countOf(t, sendInSession(server.Handler(), countPath, "test-proxy-key", "", body,
				http.Header{"Anthropic-Version": {"2023-06-01"}}))
			if got != 1111 {
				t.Errorf("count = %d, want the upstream's 1111", got)
			}
			seen := upstream.requests()
			if len(seen) != 1 {
				t.Fatalf("upstream got %d count requests, want 1", len(seen))
			}
			r := seen[0]
			if r.path != tc.wantPath {
				t.Errorf("path = %q, want %q", r.path, tc.wantPath)
			}
			if got := r.header.Get("Authorization"); got != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAuth)
			}
			if got := r.header.Get("X-Api-Key"); got != "" {
				t.Errorf("x-api-key = %q: the proxy key reached Kimi", got)
			}
			if tc.wantUA != "" && r.header.Get("User-Agent") != tc.wantUA {
				t.Errorf("User-Agent = %q, want %q", r.header.Get("User-Agent"), tc.wantUA)
			}
			if got := r.header.Get("X-Msh-Platform"); got != tc.wantMsh {
				t.Errorf("X-Msh-Platform = %q, want %q", got, tc.wantMsh)
			}
			if got := r.header.Get("Anthropic-Version"); got != "2023-06-01" {
				t.Errorf("anthropic-version = %q, want the client's", got)
			}
			if want := `{"model":"kimi-k2","messages":[{"role":"user","content":"hi"}]}`; r.body != want {
				t.Errorf("body = %s, want %s", r.body, want)
			}
		})
	}
}

// A custom endpoint is asked at its messages URL with /messages replaced by
// /messages/count_tokens, with its key and the client's bytes. An endpoint
// whose URL names no messages route has nothing to ask and is estimated.
func TestCountTokens_ForwardsToCustomEndpoints(t *testing.T) {
	const body = `{"model":"relay-model",  "messages":[{"role":"user","content":"a < b"}]}`
	for _, tc := range []struct {
		name     string
		suffix   string // appended to the upstream URL to make the endpoint URL
		wantPath string // "" when the endpoint has no count route
	}{
		{"messages URL", "/v1/messages", "/v1/messages/count_tokens"},
		{"prefixed messages URL with a slash", "/anthropic/v1/messages/", "/anthropic/v1/messages/count_tokens"},
		{"bare host", "", "/v1/messages/count_tokens"},
		{"v1 base", "/v1", "/v1/messages/count_tokens"},
		{"OpenAI-format URL", "/v1/chat/completions", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCountUpstream(t, http.StatusOK, `{"input_tokens":2468}`, nil)
			cfg := config.DefaultConfig()
			cfg.CustomEndpoints = map[string]config.EndpointConfig{
				"relay-model": {URL: upstream.URL + tc.suffix, APIKey: "ck"},
			}
			server := newCountServer(t, cfg, time.Now)

			got := countOf(t, sendInSession(server.Handler(), countPath, "test-proxy-key", "", body, nil))
			seen := upstream.requests()
			if tc.wantPath == "" {
				if want := estimateOf(t, server, "", "relay-model", body); got != want {
					t.Errorf("count = %d, want the estimate %d", got, want)
				}
				if len(seen) != 0 {
					t.Errorf("upstream got %d count requests, want none", len(seen))
				}
				return
			}
			if got != 2468 {
				t.Errorf("count = %d, want the upstream's 2468", got)
			}
			if len(seen) != 1 {
				t.Fatalf("upstream got %d count requests, want 1", len(seen))
			}
			r := seen[0]
			if r.path != tc.wantPath || r.query != "" {
				t.Errorf("target = %s?%s, want %s", r.path, r.query, tc.wantPath)
			}
			for name, want := range map[string]string{
				"Authorization":     "Bearer ck",
				"X-Api-Key":         "ck",
				"Anthropic-Version": "2023-06-01",
			} {
				if got := r.header.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if r.body != body {
				t.Errorf("body = %s, want the client's bytes %s", r.body, body)
			}
		})
	}
}

// A keyless Anthropic-shaped endpoint is one the messages forward normalizes;
// its count carries the count identity Claude Code sends, not the caller's.
func TestCountTokens_NormalizedCustomEndpointSendsTheCountIdentity(t *testing.T) {
	upstream := newCountUpstream(t, http.StatusOK, `{"input_tokens":1357}`, nil)
	cfg := config.DefaultConfig()
	cfg.CustomEndpoints = map[string]config.EndpointConfig{"relay-model": {URL: upstream.URL + "/v1/messages"}}
	server := newCountServer(t, cfg, time.Now)

	body := `{"model":"relay-model","messages":[{"role":"user","content":"hi"}],"tools":[]}`
	got := countOf(t, sendInSession(server.Handler(), countPath, "test-proxy-key", "sess-relay", body,
		http.Header{"User-Agent": {"foreign/1.0"}, "X-Stainless-Timeout": {"600"}}))
	if got != 1357 {
		t.Errorf("count = %d, want the upstream's 1357", got)
	}
	seen := upstream.requests()
	if len(seen) != 1 {
		t.Fatalf("upstream got %d count requests, want 1", len(seen))
	}
	r := seen[0]
	if r.path != "/v1/messages/count_tokens" || r.query != "beta=true" {
		t.Errorf("target = %s?%s, want /v1/messages/count_tokens?beta=true", r.path, r.query)
	}
	for name, want := range map[string]string{
		"x-claude-code-request-class": "auxiliary",
		"anthropic-beta":              strings.Join(ccidentity.CountTokensBetas, ","),
		"User-Agent":                  ccidentity.MessagesUserAgent,
		"x-app":                       "cli",
		"X-Stainless-Timeout":         "",
		"x-claude-code-prompt-id":     "",
		"Authorization":               "",
		"x-api-key":                   "",
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if r.body != body {
		t.Errorf("body = %s, want the client's bytes %s", r.body, body)
	}
}

// One custom endpoint's 404 marks that endpoint, not every custom endpoint.
func TestCountTokens_UnsupportedMarkCoversOneCustomEndpoint(t *testing.T) {
	unsupported := newCountUpstream(t, http.StatusNotFound, `{}`, nil)
	counting := newCountUpstream(t, http.StatusOK, `{"input_tokens":4321}`, nil)
	cfg := config.DefaultConfig()
	cfg.CustomEndpoints = map[string]config.EndpointConfig{
		"model-a": {URL: unsupported.URL + "/v1/messages", APIKey: "ka"},
		"model-b": {URL: counting.URL + "/v1/messages", APIKey: "kb"},
	}
	server := newCountServer(t, cfg, time.Now)
	handler := server.Handler()
	bodyA := `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`
	bodyB := `{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`
	wantA := estimateOf(t, server, "", "model-a", bodyA)

	for i := range 2 {
		if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "", bodyA, nil)); got != wantA {
			t.Errorf("round %d: model-a count = %d, want the estimate %d", i, got, wantA)
		}
		if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "", bodyB, nil)); got != 4321 {
			t.Errorf("round %d: model-b count = %d, want the upstream's 4321", i, got)
		}
	}
	if n := len(unsupported.requests()); n != 1 {
		t.Errorf("unsupported endpoint asked %d times, want 1: its 404 marks it", n)
	}
	if n := len(counting.requests()); n != 2 {
		t.Errorf("counting endpoint asked %d times, want 2: another endpoint's mark must not cover it", n)
	}
}

// A gateway that cannot be reached is estimated; the client still gets a 200.
func TestCountTokens_UnreachableUpstreamIsEstimated(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	for _, tc := range []struct {
		name      string
		configure func(*config.Config)
		model     string
	}{
		{"openrouter", func(c *config.Config) {
			c.OpenRouter = config.OpenRouterConfig{Enabled: true, BaseURL: deadURL, APIKey: "k",
				Allowlist: []config.OpenRouterModelConfig{{ID: "vendor/model-x", Enabled: true}}}
		}, "vendor/model-x"},
		{"kimi", func(c *config.Config) {
			c.Kimi = config.KimiConfig{Enabled: true, BaseURL: deadURL, APIKey: "k",
				Allowlist: []config.KimiModelConfig{{ID: "kimi-k2", Enabled: true}}}
		}, "kimi-k2"},
		{"custom", func(c *config.Config) {
			c.CustomEndpoints = map[string]config.EndpointConfig{"relay-model": {URL: deadURL + "/v1/messages", APIKey: "k"}}
		}, "relay-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			tc.configure(&cfg)
			server := newCountServer(t, cfg, time.Now)
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, tc.model)
			want := estimateOf(t, server, "", tc.model, body)
			if got := countOf(t, sendInSession(server.Handler(), countPath, "test-proxy-key", "", body, nil)); got != want {
				t.Errorf("count = %d, want the estimate %d", got, want)
			}
		})
	}
}

func TestCustomCountURL(t *testing.T) {
	for _, tc := range []struct{ endpoint, want string }{
		{"https://relay.example/v1/messages", "https://relay.example/v1/messages/count_tokens"},
		{"https://relay.example/anthropic/v1/messages/", "https://relay.example/anthropic/v1/messages/count_tokens"},
		{"https://relay.example/v1/messages?region=eu", "https://relay.example/v1/messages/count_tokens?region=eu"},
		{"https://relay.example/v1/messages/count_tokens", "https://relay.example/v1/messages/count_tokens"},
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages/count_tokens"},
		{"https://relay.example/v1", "https://relay.example/v1/messages/count_tokens"},
		{"https://relay.example/v1/chat/completions", ""},
		{"https://relay.example/openai", ""},
		{"://no-scheme", ""},
	} {
		got, ok := customCountURL(tc.endpoint)
		if tc.want == "" {
			if ok {
				t.Errorf("customCountURL(%q) = %q, want no count route", tc.endpoint, got)
			}
			continue
		}
		if !ok || got.String() != tc.want {
			t.Errorf("customCountURL(%q) = %v, %v; want %q", tc.endpoint, got, ok, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/kimi -run 'TestCountTokens_' -count=1`
Expected: FAIL to build: `undefined: CountTokens`.
Run: `go test ./internal/api -run 'TestCountTokens_Forwards|TestCountTokens_Normalized|TestCountTokens_UnsupportedMark|TestCountTokens_Unreachable|TestCustomCountURL' -count=1`
Expected: FAIL to build: `undefined: customCountURL`.

- [ ] **Step 3: Write the implementation**

`internal/kimi/passthrough.go`: the import block gains `"context"` and `"fmt"`. In `ForwardMessagesWithModify`'s Director, replace `:74-92` (from `// Always set Bearer` through the `anthropic-beta` block) with:

```go
			// Always set Bearer; clients that sent x-api-key to the proxy are
			// also covered because we strip any prior auth header.
			setUpstreamHeaders(req.Header, apiKey, r.Header, identity)
```

and add after `ForwardMessagesWithModify`:

```go
// setUpstreamHeaders sets what every Kimi request carries over whatever the
// header already holds: the Bearer credential and no x-api-key, the identity
// headers (KimiCLI's for a Kimi Code login), which override the client's, and
// the client's anthropic-version and anthropic-beta.
func setUpstreamHeaders(h http.Header, apiKey string, client, identity http.Header) {
	h.Set("Authorization", "Bearer "+apiKey)
	h.Del("x-api-key")
	for k, vs := range identity {
		if len(vs) > 0 {
			h.Set(k, vs[0])
		}
	}
	if av := client.Get("anthropic-version"); av != "" {
		h.Set("anthropic-version", av)
	}
	if ab := client.Get("anthropic-beta"); ab != "" {
		h.Set("anthropic-beta", ab)
	}
}

// countClient carries count_tokens calls on the forwards' shared transport.
var countClient = &http.Client{Transport: upstreamTransport}

// CountTokens posts an Anthropic count_tokens body to
// <base>/v1/messages/count_tokens with the headers a messages forward sends
// (setUpstreamHeaders). The caller reads and closes the response.
func CountTokens(ctx context.Context, baseURL, apiKey string, body []byte, client, identity http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, NormalizeBaseURL(baseURL)+"/v1/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Kimi count_tokens request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setUpstreamHeaders(req.Header, apiKey, client, identity)
	resp, err := countClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Kimi count_tokens: %w", err)
	}
	return resp, nil
}
```

`internal/api/server.go`, in `forwardToOpenRouter`:

1. Delete `:2252-2267`, the block from `// App identity for harness-gated models: OpenRouter 403s models restricted` through the `spoofReferer` default and its trailing blank line (`openrouter.ApplySpoofHeaders` trims and defaults each value itself).
2. Replace `:2382-2411` (from `upReq.Header.Set("Content-Type", "application/json")` through the closing `}` of `if appSpoofed { ... }`) with:

```go
		appSpoofed := server.setOpenRouterHeaders(upReq, request.Header, openRouterCfg)
```

(the harness-gate retry at `:2562-2573` keeps reading `appSpoofed`).

3. Add after `openRouterUpstreamClient` (`:2652-2658`):

```go
// setOpenRouterHeaders sets what every OpenRouter request carries: JSON in and
// out, the key (as Bearer and as x-api-key), the client's Anthropic version and
// betas, the client's own app attribution, and the spoofed attribution once a
// harness gate has been seen. OpenRouter 403s models restricted to agentic
// harnesses when a request carries no app attribution. It reports whether the
// spoof went on: a 403 harness gate is judged against that value, not against
// a fresh read another request may have flipped since.
func (server *Server) setOpenRouterHeaders(upReq *http.Request, client http.Header, cfg config.OpenRouterConfig) bool {
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	if cfg.APIKey != "" {
		apiKey := strings.TrimSpace(cfg.APIKey)
		upReq.Header.Set("Authorization", "Bearer "+apiKey)
		upReq.Header.Set("x-api-key", apiKey)
	}
	if av := client.Get("anthropic-version"); av != "" {
		upReq.Header.Set("anthropic-version", av)
	}
	if ab := client.Get("anthropic-beta"); ab != "" {
		upReq.Header.Set("anthropic-beta", ab)
	}
	for _, h := range []string{
		openrouter.SpoofAppRefererHeader,
		openrouter.SpoofAppRefererLegacyHeader,
		openrouter.SpoofAppTitleHeader,
		openrouter.SpoofAppTitleLegacyHeader,
		openrouter.SpoofAppCategoriesHeader,
	} {
		if v := client.Get(h); v != "" {
			upReq.Header.Set(h, v)
		}
	}
	server.mu.Lock()
	spoofed := server.appSpoofActivated
	server.mu.Unlock()
	if spoofed {
		openrouter.ApplySpoofHeaders(upReq, cfg.AppSpoof.Title, cfg.AppSpoof.Categories, cfg.AppSpoof.Referer)
	}
	return spoofed
}
```

`internal/api/count_tokens.go`: the import block becomes

```go
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/kimi"
	"antigravity-go-proxy/internal/openrouter"
)
```

`countForwards` becomes:

```go
var countForwards = map[config.GatewayID]func(*Server, countCall) (*http.Response, error){
	config.GatewayClaudeCode: (*Server).countClaudeCode,
	config.GatewayOpenRouter: (*Server).countOpenRouter,
	config.GatewayKimi:       (*Server).countKimi,
	config.GatewayCustom:     (*Server).countCustomEndpoint,
}
```

and append:

```go
// countOpenRouter asks OpenRouter's Anthropic surface for the count, with the
// headers its messages forward sends (setOpenRouterHeaders).
func (server *Server) countOpenRouter(call countCall) (*http.Response, error) {
	target := openrouter.NormalizeBaseURL(call.cfg.OpenRouter.BaseURL) + "/v1/messages/count_tokens"
	upReq, err := http.NewRequestWithContext(call.request.Context(), http.MethodPost, target, bytes.NewReader(call.body))
	if err != nil {
		return nil, fmt.Errorf("build OpenRouter count_tokens request: %w", err)
	}
	server.setOpenRouterHeaders(upReq, call.request.Header, call.cfg.OpenRouter)
	return openRouterUpstreamClient().Do(upReq)
}

// countKimi asks the Kimi gateway for the count with the credential, and for a
// Kimi Code login the KimiCLI identity headers, its messages forward sends.
func (server *Server) countKimi(call countCall) (*http.Response, error) {
	cred, err := server.resolveKimiCredential(call.request.Context(), call.cfg.Kimi)
	if err != nil {
		return nil, err
	}
	var identity http.Header
	if cred.oauth {
		identity = server.kimiIdentityHeaders()
	}
	return kimi.CountTokens(call.request.Context(), cred.baseURL, cred.token, call.body, call.request.Header, identity)
}

// errNoCountRoute is a custom endpoint whose URL names no Anthropic messages
// route, so it has no count_tokens route to ask.
var errNoCountRoute = errors.New("custom endpoint URL names no Anthropic messages route")

var (
	// customCountClient and customIdentityCountClient carry custom-endpoint
	// counts on the transports the messages forward uses for that endpoint.
	customCountClient         = &http.Client{Transport: customEndpointTransport}
	customIdentityCountClient = &http.Client{Transport: customEndpointIdentityTransport}
)

// countCustomEndpoint asks an Anthropic-format custom endpoint for the count.
// An endpoint the messages forward normalizes (customEndpointIdentity) gets
// the identity Claude Code sends on counts, over the same HTTP/1.1 transport;
// any other gets its key and the client's Anthropic version and betas, as its
// messages forward does.
func (server *Server) countCustomEndpoint(call countCall) (*http.Response, error) {
	endpoint := call.route.endpoint
	target, ok := customCountURL(endpoint.URL)
	if !ok {
		return nil, errNoCountRoute
	}
	identity, normalize := customEndpointIdentity(endpoint, call.session)
	if normalize {
		target.RawQuery = withCapturedBetaQuery(target.RawQuery)
	}
	upReq, err := http.NewRequestWithContext(call.request.Context(), http.MethodPost, target.String(), bytes.NewReader(call.body))
	if err != nil {
		return nil, fmt.Errorf("build custom endpoint count_tokens request: %w", err)
	}
	if normalize {
		ccidentity.ApplyHeaders(upReq.Header, ccidentity.CountTokensProfile(), identity, ccidentity.Turn{})
		return customIdentityCountClient.Do(upReq)
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	if endpoint.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
		upReq.Header.Set("x-api-key", endpoint.APIKey)
	}
	version := call.request.Header.Get("anthropic-version")
	if version == "" {
		version = "2023-06-01"
	}
	upReq.Header.Set("anthropic-version", version)
	if beta := call.request.Header.Get("anthropic-beta"); beta != "" {
		upReq.Header.Set("anthropic-beta", beta)
	}
	return customCountClient.Do(upReq)
}

// customCountURL is the count_tokens URL of an Anthropic-format endpoint: its
// messages URL with /messages replaced by /messages/count_tokens. A URL that
// leaves the path to the request (no path, or one ending in /v1) gets
// /v1/messages/count_tokens, as resolveCustomEndpointURL builds a messages URL.
// ok is false for any other URL: it names no messages route.
func customCountURL(endpointURL string) (*url.URL, bool) {
	target, err := resolveCustomEndpointURL(endpointURL, "/v1/messages/count_tokens")
	if err != nil {
		return nil, false
	}
	path := strings.TrimRight(target.Path, "/")
	switch {
	case strings.HasSuffix(path, "/messages/count_tokens"):
		target.Path = path
	case strings.HasSuffix(path, "/messages"):
		target.Path = path + "/count_tokens"
	default:
		return nil, false
	}
	return target, true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/kimi -count=1`
Expected: PASS (`TestForwardMessages_DirectorBehavior` guards the Director change).
Run: `go test ./internal/api -run 'TestCountTokens_|TestCustomCountURL|TestOpenRouter|AppSpoof' -count=1`
Expected: PASS (the OpenRouter app-spoof tests guard the header extraction).
Run: `go test ./internal/api -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/kimi/passthrough.go internal/kimi/passthrough_test.go internal/api/server.go internal/api/count_tokens.go internal/api/count_tokens_test.go
git commit -m "feat(api): forward count_tokens to OpenRouter, Kimi and custom endpoints"
```

---

### Task 13: Drift tooling probes the new calls and reports a changed agy call sequence

Spec §9 wants the drift run to notice when agy changes the order of its control-plane calls. Two gaps stand in the way:
- `tools/clientprobe cloudcode` sends only `loadCodeAssist`, `fetchAvailableModels`, `writeTrajectoryAcls`, the checkpoint and the agent call (`tools/clientprobe/main.go:109-123`). The four new calls and `retrieveUserQuotaSummary` are never compared with agy's.
- `analyze_agy` compares requests by key (`compare_captures`, `scripts/client_drift.py:868-883`), so a reordered sequence is not a finding. The baseline `.reference/client-drift/agy.jsonl` is rewritten only when a pin or hello changes and nothing drifts (`analyze_generic`, `:1070`). A reorder alone would leave the baseline, and so Task 7's sequence test, on the old order.

**Files:**
- Modify: `tools/clientprobe/main.go:4-12` (doc comment), `:109-123` (probe table)
- Modify: `scripts/client_drift.py:797-803` (new `AGY_CONCURRENT_CALLS` and `agy_call_sequence` after `request_key`), `:958-987` (`analyze_agy` compares the sequence before `analyze_generic`)
- Test: `scripts/test_client_drift.py` (two tests in `CompareTest`, after `test_a_request_only_this_build_sends_needs_a_human` at `:168-183`)

**Interfaces:**
- Consumes: Task 5's `(*cloudcode.Client).FetchAdminControls`, `FetchUserInfo`, `ListExperiments`, `ListCascadeNuxes`; existing `RetrieveUserQuotaSummary`.
- Produces: `client_drift.AGY_CONCURRENT_CALLS: frozenset[str]`, `client_drift.agy_call_sequence(records: list[dict]) -> list[str]`; a manual item starting `agy's Cloud Code call sequence changed` when the fresh capture's sequence differs from the baseline's.

- [ ] **Step 1: Write the failing tests**

In `scripts/test_client_drift.py`, add to `class CompareTest` after `test_a_request_only_this_build_sends_needs_a_human`:

```python
    def test_agy_sequence_ignores_order_inside_a_concurrent_group(self):
        # agy sends fetchAdminControls, fetchUserInfo and retrieveUserQuotaSummary
        # together; agy.jsonl shows them in both orders within one run.
        first = [record(f"/v1internal:{name}", []) for name in (
            "loadCodeAssist", "fetchAdminControls", "fetchUserInfo", "retrieveUserQuotaSummary", "fetchAvailableModels")]
        second = [record(f"/v1internal:{name}", []) for name in (
            "loadCodeAssist", "retrieveUserQuotaSummary", "fetchAdminControls", "fetchUserInfo", "fetchAvailableModels")]
        self.assertEqual(drift.agy_call_sequence(first), drift.agy_call_sequence(second))
        self.assertEqual(drift.agy_call_sequence(first), [
            "loadCodeAssist", "fetchAdminControls", "fetchUserInfo", "retrieveUserQuotaSummary", "fetchAvailableModels"])

    def test_a_changed_agy_sequence_needs_a_human(self):
        # The baseline is rewritten only when a pin changes, so a reorder alone
        # would leave the start-burst sequence test on agy's old order.
        def calls(*names):
            out = []
            for name in names:
                if name == "cascadeNuxes":
                    out.append(dict(record("/v1internal/cascadeNuxes?page_size=1000", []), method="GET"))
                else:
                    out.append(record(f"/v1internal:{name}", []))
            return out
        baseline = calls("loadCodeAssist", "fetchUserInfo", "listExperiments", "cascadeNuxes")
        moved = calls("loadCodeAssist", "listExperiments", "fetchUserInfo", "cascadeNuxes")
        path = Path(tempfile.mkdtemp()) / "agy.jsonl"
        self.addCleanup(shutil.rmtree, path.parent)
        path.write_text("".join(json.dumps(item) + "\n" for item in baseline), encoding="utf-8")
        for observed, changed in ((baseline, False), (moved, True)):
            result = drift.Result("agy", "1.5.2", drift.Capture(observed, {}))
            with mock.patch.object(drift, "analyze_generic"), mock.patch.object(drift, "baseline_path", return_value=path):
                drift.analyze_agy(ROOT, result, drift.Capture(observed, {}), None, None, [])
            found = [item for item in result.manual if item.startswith("agy's Cloud Code call sequence changed")]
            self.assertEqual(bool(found), changed, result.manual)
        self.assertIn("baseline loadCodeAssist → fetchUserInfo → listExperiments → cascadeNuxes", found[0])
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd scripts && python3 -m unittest test_client_drift.CompareTest -v`
Expected: FAIL: `AttributeError: module 'client_drift' has no attribute 'agy_call_sequence'` and the sequence test finds no manual item.

- [ ] **Step 3: Write minimal implementation**

In `scripts/client_drift.py`, after `request_key`:

```python
# agy sends these together, so a capture shows them in either order.
AGY_CONCURRENT_CALLS = frozenset({"fetchAdminControls", "fetchUserInfo", "retrieveUserQuotaSummary"})


def agy_call_sequence(records: list[dict]) -> list[str]:
    """agy's Cloud Code calls in capture order, named without the /v1internal
    prefix. A run of calls agy sends together is sorted, so the order they
    happened to arrive in is not a change."""
    sequence: list[str] = []
    group: list[str] = []
    for record in records:
        _, path = request_key(record)
        name = path.removeprefix("/v1internal:").removeprefix("/v1internal/")
        if name in AGY_CONCURRENT_CALLS:
            group.append(name)
            continue
        sequence.extend(sorted(group))
        group = []
        sequence.append(name)
    sequence.extend(sorted(group))
    return sequence
```

In `analyze_agy`, insert before the `analyze_generic(root, result, capture, pairs)` call:

```python
    baseline_file = baseline_path(root, "agy")
    if baseline_file.exists():
        before = agy_call_sequence(load_capture_file(baseline_file))
        now = agy_call_sequence(capture.records)
        if before != now:
            result.manual.append(
                "agy's Cloud Code call sequence changed; update the start burst and per-turn calls "
                "(internal/accounts/controlplane.go) and their sequence test, then the baseline: "
                f"baseline {' → '.join(before)}; now {' → '.join(now)}")
```

In `tools/clientprobe/main.go`, replace the `cloudcode` paragraph of the doc comment (lines 4-12) with:

```go
//	clientprobe cloudcode -proxy http://127.0.0.1:PORT
//	    Sends loadCodeAssist, fetchAvailableModels, the control-plane calls
//	    agy sends around a conversation (fetchAdminControls, fetchUserInfo,
//	    retrieveUserQuotaSummary, listExperiments, GET cascadeNuxes), and the
//	    conversation start (writeTrajectoryAcls, the title checkpoint, the
//	    agent streamGenerateContent), all built by the production code,
//	    through the given HTTP proxy
//	    with the proxy's own Cloud Code client and transport, using a token
//	    Google rejects. The interceptor records the ClientHello, header order
//	    and body this build sends, so the hook can diff them against what agy
//	    sent through the same interceptor. No account is used.
```

and add these entries to the probe table (`map[string]func(context.Context) (cloudcode.Response, error){...}`, lines 109-123), after `"fetchAvailableModels"`:

```go
		"fetchAdminControls": func(ctx context.Context) (cloudcode.Response, error) {
			return client.FetchAdminControls(ctx, "clientprobe-project")
		},
		"fetchUserInfo": func(ctx context.Context) (cloudcode.Response, error) {
			return client.FetchUserInfo(ctx, "clientprobe-project")
		},
		"retrieveUserQuotaSummary": func(ctx context.Context) (cloudcode.Response, error) {
			return client.RetrieveUserQuotaSummary(ctx, "clientprobe-project")
		},
		"listExperiments": func(ctx context.Context) (cloudcode.Response, error) {
			return client.ListExperiments(ctx, "clientprobe-project")
		},
		"cascadeNuxes": func(ctx context.Context) (cloudcode.Response, error) { return client.ListCascadeNuxes(ctx) },
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd scripts && python3 -m unittest test_client_drift -v`
Expected: PASS (all tests, including `RegistryTest`).

Run: `go vet ./tools/clientprobe && go run ./tools/clientprobe cloudcode -proxy http://127.0.0.1:9 -timeout 2s | python3 -c 'import json,sys; print(sorted(json.load(sys.stdin)))'`
Expected: exit 0; the printed list contains `cascadeNuxes`, `fetchAdminControls`, `fetchUserInfo`, `listExperiments` and `retrieveUserQuotaSummary` (each value is a connection error to port 9: only the request set matters here).

Run: `cd scripts && python3 -c 'import client_drift as d; print(d.agy_call_sequence(d.load_capture_file(d.Path("../.reference/client-drift/agy.jsonl"))))'`
Expected: the committed baseline reads as the two runs quoted at the top of Part B, with each concurrent group sorted.

- [ ] **Step 5: Commit**

```bash
git add tools/clientprobe/main.go scripts/client_drift.py scripts/test_client_drift.py
git commit -m "feat(drift): probe agy's control-plane calls and report a changed call sequence"
```

---

### Task 14: Affinity and count_tokens docs, final verification

**Files:**
- Modify: `README.md:16-20` (Key Features → Multi-Account Pool & Selection Strategies), `:423-428` (Selection Strategies), `:438-441` (Claude Code gateway features), `:883-890` (API table) and a new subsection after the table (`:929`)
- Modify: `README.md:129` (control-plane paragraph after the `tls.Config{}` paragraph in "What “Matching agy” Means")
- Modify: `CONTEXT.md:49` (four terms after **Wire Identity**: **Start Burst**, **Per-Turn Calls**, **Session Pin**, **Overflow Pin**)
- Modify: `docs/adr/0004-claude-code-identity-normalization-exception.md:32-35` (item 7 after item 6: the count identity is a normalization the ADR scopes)

**Interfaces:**
- Consumes: the behaviour named in the spec: pins (`pinIdleTTL = time.Hour`, `maxPins = 10000`, `maxStickyWait`) and `activeSessions` in `/api/accounts` from the session-affinity task; the start burst and per-turn calls from the control-plane task (this task adds their README paragraph and CONTEXT terms, Step 1); `ccidentity.CountTokensBetas` and the `count_tokens estimate` debug line from the Claude Code count forward task.
- Produces: documentation only.

README edits keep every `scripts/client_drift.py` site single: do not write another `JA4 fingerprint (\``, `- JA4 \``, `JA3 \`` or `OpenCode <version> header set` line; Step 4 runs the registry test that catches a duplicate.

- [ ] **Step 1: Control-plane docs**

The README paragraph and CONTEXT terms for the start burst and per-turn calls (Tasks 5–7). Insert these first; Step 2 and Step 3 refer to them. Line numbers are from the base commit; find each spot by the text named next to it.

`README.md`: after line 129 (the `tls.Config{}` paragraph in "What “Matching agy” Means") and before the `---`, insert a blank line and:

```markdown
Around a conversation the proxy also sends the control-plane calls agy 1.5.2 sends ([agy capture](.reference/client-drift/agy.jsonl)), over the same transport to the same host. The first request of a Claude Code session on an account sends agy's start burst: `loadCodeAssist`; `fetchAdminControls`, `fetchUserInfo` and `retrieveUserQuotaSummary` together; `fetchAvailableModels`; `retrieveUserQuotaSummary`; `listExperiments`; `loadCodeAssist`; `GET /v1internal/cascadeNuxes`. Each new prompt then sends `loadCodeAssist`, followed by `fetchUserInfo` and `retrieveUserQuotaSummary`, before generation. Tool continuations, tool-less side calls (titles) and requests without a session id send none of these. The `loadCodeAssist` and `retrieveUserQuotaSummary` answers update the account's tier, project and quota; the other calls are sent for parity, and only their status is logged at debug. Each call has a 10 s timeout and never fails the request. A start burst in which a call got no HTTP answer (a network error or a timeout) is sent again on the session's next request; an error status counts as an answer.
```

`CONTEXT.md`: insert after line 49 (the `_Avoid_` line of **Wire Identity**, in "Identity & Account Management"):

```markdown

**Start Burst**:
The control-plane calls sent, in agy's order, the first time a session uses an Account: `loadCodeAssist`; `fetchAdminControls`, `fetchUserInfo` and `retrieveUserQuotaSummary` together; `fetchAvailableModels`; `retrieveUserQuotaSummary`; `listExperiments`; `loadCodeAssist`; `GET cascadeNuxes`. Sent again for each Account the session overflows to, and on the session's next request after a burst in which a call got no HTTP answer.
_Avoid_: Warmup, handshake, init calls.

**Per-Turn Calls**:
The calls sent before each new prompt of a session: `loadCodeAssist`, then `fetchUserInfo` and `retrieveUserQuotaSummary` together. Tool continuations and tool-less side calls send none. Their answers keep the Account's tier, project and quota pools current; no quota call follows a stream.
_Avoid_: Pre-flight, quota poll, post-request refresh.
```

- [ ] **Step 2: README**

In the Key Features bullet **Multi-Account Pool & Selection Strategies** (`README.md:16-20`), add after the `Selection strategies:` line:

```markdown
  - Conversation affinity: each Claude Code session stays on one account for all of its turns and models; a long limit moves only the affected model to an overflow account.
```

After the `### Selection Strategies (\`ACCOUNT_STRATEGY\`)` paragraph that ends at `:428`, add:

```markdown
#### Conversation affinity

A request that carries a session id (`X-Claude-Code-Session-Id` or another session header, else `metadata.session_id` or the session inside `metadata.user_id`) is pinned to one account for all of its models, so a conversation keeps its cascade and its prompt cache. The strategy chooses only when a pin is made, rebound or overflowed: `sticky` gives a new pin the current account, `round-robin` rotates per new pin or overflow, `hybrid` scores. A request without a session id is still chosen per request.

- A model rate limit on the pinned account that resets within 2 minutes makes the request wait for that account (or within `maxWaitBeforeErrorMs`, when that is shorter).
- A longer limit, exhausted quota, a failure lock or the 404 model lock moves only that model to an **overflow** account, chosen once by the strategy and kept for the rest of the session; the model does not move back when the pinned account recovers.
- A pinned account that is removed, disabled or invalid is replaced through the strategy; overflow entries on healthy accounts stay.

Pins live in memory: a pin is dropped after an hour without a request, at most 10,000 are kept (oldest out first), and a restart loses them. Each pin change is logged at info with the session's first 8 characters, the model, the old and new account and the reason. `GET /api/accounts` reports `activeSessions` per account.
```

In `## Claude Code Gateway & Multi-Account OAuth Pool` → **Direct Anthropic Skin Gateway** (`:438-441`), add a sub-bullet:

```markdown
  - Answers `POST /v1/messages/count_tokens` (Claude Code's `/context`) through the pool with the identity the real client sends on counts; a burst of counts runs concurrently, and a `429` on a count cools the account down as one on `/v1/messages` does.
```

In the API table, replace the row at `:890` with:

```markdown
| `/v1/messages/count_tokens` | `POST` | Input token count `{"input_tokens": N}`: forwarded to Claude Code, OpenRouter, Kimi and Anthropic-format custom endpoints, estimated for Cloud Code and Zen (see below) |
```

After the table (after `:929`, before `---`), add:

```markdown
### Token counting (`/v1/messages/count_tokens`)

A count takes the `/v1/messages` route: model mapping, then the gateway precedence. Headroom and the classifier do not run.

| Gateway | Count |
|---|---|
| Claude Code | Forwarded to `/v1/messages/count_tokens?beta=true` through the pool, with the identity Claude Code sends on its own counts (`x-claude-code-request-class: auxiliary` and a five-entry `anthropic-beta` list ending in `token-counting-2024-11-01`). The body is forwarded as the client sent it, with the gateway's model id written in. |
| OpenRouter | Forwarded to `<baseUrl>/v1/messages/count_tokens` with the key and attribution headers of its messages forward. |
| Kimi | Forwarded to `<base>/v1/messages/count_tokens` with the Kimi credential, plus the KimiCLI identity for a Kimi Code login. |
| Custom endpoint | Forwarded to the endpoint URL with `/messages` replaced by `/messages/count_tokens` (`/v1/messages/count_tokens` when the URL leaves the path to the request). A normalized endpoint gets the Claude Code count identity. A URL that names no messages route is estimated. |
| Cloud Code, Zen | Estimated. |

An upstream that answers `404`, `405` or `501` is estimated for an hour without being asked (custom endpoints per URL). Any other failure is estimated for that request. A count never answers an error status for an upstream's sake; only a body without a `messages` array is refused, with `400`.

The estimate is the conversation's last real input count (input plus cache reads and writes of the latest completed Cloud Code or Zen request with the same session id) plus a chars-per-token estimate of the messages added since. The ratio is learned per model family (`claude`, `gemini`, `gpt-oss`, otherwise the model id) from requests of at least 1,000 characters, starting at 4.0. Images count by their dimensions: `width × height / 750`, at most 1,600 tokens, or 258 per 768 × 768 tile for Gemini. Without a session id, or when the conversation's earlier messages, system prompt or tools changed, the whole request is estimated. The memory is in process, kept an hour after its last use, for at most 10,000 conversations. With debug logging, each estimate is logged as `count_tokens estimate` with its `session_id`, next to the `input_tokens` and `cache_read_tokens` of that session's next request.
```

- [ ] **Step 3: CONTEXT.md**

After the **Per-Turn Calls** term (Step 1 inserts **Start Burst** and **Per-Turn Calls** after **Wire Identity**, `CONTEXT.md:47-49`), add:

```markdown
**Session Pin**:
The binding of one client session to the Account that serves all of its models, so a conversation keeps one cascade and one prompt cache. Made by the Selection Strategy on the session's first request; dropped after an hour without a request.
_Avoid_: Sticky session (collides with the `sticky` strategy), affinity entry, session lock.

**Overflow Pin**:
A per-model exception to a Session Pin: the Account one model of the session moved to when its pinned Account hit a long rate limit, exhausted quota or a failure lock. It stays for the rest of the session, even after the pinned Account recovers.
_Avoid_: Fallback account, spillover, failover pin.
```

- [ ] **Step 4: ADR-0004**

After item 6 (ends at `docs/adr/0004-claude-code-identity-normalization-exception.md:35`), add:

```markdown
7. **count_tokens**: `POST /v1/messages/count_tokens` is normalized on the same two paths and under the same scope (§1, §3, §5), with the identity Claude Code sends on its own counts (`.reference/claude-code-count-tokens-20261107.jsonl`, 2.1.311 `/context`), not the messages one: `x-claude-code-request-class: auxiliary`, no `X-Stainless-Timeout`, no `x-claude-code-prompt-id`, and the five-entry list `ccidentity.CountTokensBetas`. That list is pinned in `scripts/client_drift.py` as `claude.count_tokens_betas`; the drift run never reaches `/context`, so it moves only with a recapture. The advisor beta exception (§6) does not apply: the count capture is the specification and carries none. The body is not normalized: the real count body carries `messages`, `model` and `tools` only, with no billing block and no `metadata`, so there is nothing to add and no `cch` to sign; the gateway writes only its model id into the client's bytes. Count answers carry no `anthropic-ratelimit-unified-*` headers, so the pool's rate-limit state is not read from them; a `429` still cools the account down.
```

- [ ] **Step 5: Check the docs against the pin registry**

Run: `python3 -m unittest discover -s scripts -p 'test_client_drift.py'`
Expected: `OK` (`test_every_pin_reads_one_value_from_this_tree` fails if a README edit duplicated a pinned literal).

- [ ] **Step 6: Commit**

```bash
git add README.md CONTEXT.md docs/adr/0004-claude-code-identity-normalization-exception.md
git commit -m "docs: conversation affinity and count_tokens"
```

- [ ] **Step 7: Final verification (spec §9), all unchecked until run**

- [ ] Full suite: `go test -race ./...` passes.
- [ ] Format: `gofmt -l $(git ls-files '*.go' | grep -v '^gen/')` prints nothing.
- [ ] Python: `python3 -m unittest discover -s scripts -p 'test_client_drift.py'` prints `OK`.
- [ ] Smoke run with real accounts and a separate config dir (set only `ANTIGRAVITY_CONFIG_DIR`, never `HOME`):

```bash
go build -o bin/antigravity-proxy ./cmd/proxy
SMOKE=$(mktemp -d)
cp ~/.config/antigravity-proxy/config.json ~/.config/antigravity-proxy/accounts.json "$SMOKE/"
ANTIGRAVITY_CONFIG_DIR="$SMOKE" DEBUG=1 bin/antigravity-proxy --port 18090 2>"$SMOKE/proxy.log" &
```

  In two terminals run interactive Claude Code sessions against `http://127.0.0.1:18090` (same `ANTHROPIC_BASE_URL`/token setup as `README.md` "Claude Code Integration") on a Cloud Code model, each with a tool-using prompt and a second turn; run `/context` in one of them.
- [ ] Pins: `grep -iE 'pin|overflow|rebind' "$SMOKE/proxy.log"` shows one pin per session (first 8 characters of the session id, model, account, reason); `curl -s http://127.0.0.1:18090/api/accounts | jq '.accounts[] | {email, activeSessions}'` (add `-H "x-webui-password: …"` when the copied config sets one) shows both sessions counted.
- [ ] Overflow and rebind: force the home account of one session out with `curl -s -X PATCH http://127.0.0.1:18090/api/accounts/<home email> -H 'Content-Type: application/json' -d '{"quotaThreshold":0.99}'` (the quota branch of the overflow rule), send a turn, and check the log names an overflow for that model only; then `curl -s -X POST http://127.0.0.1:18090/api/accounts/<home email>/toggle`, send a turn, and check the log names a rebind of the home account while the overflow entry stays. Restore with the same `toggle` and `{"quotaThreshold":null}`.
- [ ] Control-plane sequence: `grep -E 'loadCodeAssist|fetchAdminControls|fetchUserInfo|retrieveUserQuotaSummary|fetchAvailableModels|listExperiments|cascadeNuxes' "$SMOKE/proxy.log"` shows, for each session's first turn on an account, the start burst in the order of `.reference/client-drift/agy.jsonl` followed by the per-turn calls, and only the per-turn calls before later prompts; no `retrieveUserQuotaSummary` after a stream.
- [ ] Interceptor capture against agy: `python3 scripts/client_drift.py check --clients agy` (captures agy and this build's `tools/clientprobe` requests through the same mitmdump `Interceptor`); the report has no `this build differs from agy` line.
- [ ] JA4 gate (`AGENTS.md`), including the new GET: start `sudo tcpdump -i pktap,all -P -w /tmp/proxy-cc.pcap host daily-cloudcode-pa.googleapis.com` (Linux: `-i any`), start a new Claude Code session against the smoke proxy so its first turn sends the start burst with `GET cascadeNuxes`, stop tcpdump, then `tshark -r /tmp/proxy-cc.pcap -Y 'tls.handshake.type==1' -T fields -e tls.handshake.ja4 | sort | uniq -c` prints one JA4, equal to the one in `.reference/agy-current-baseline.txt`.
- [ ] count_tokens answers: `/context` in the Claude Code session shows token numbers, not an error. `curl -s -X POST http://127.0.0.1:18090/v1/messages/count_tokens -H 'Content-Type: application/json' -H "x-api-key: $KEY" -H 'X-Claude-Code-Session-Id: <the session id from the log>' -d '{"model":"<the session model>","messages":[{"role":"user","content":"hi"}]}'` answers `{"input_tokens":N}`.
- [ ] Estimate accuracy: `grep -E 'count_tokens estimate|\[Antigravity\]' "$SMOKE/proxy.log" | grep 'session_id=<id>'` pairs each estimate with the next request's `input_tokens` + `cache_read_tokens`; record the differences in the PR description.
- [ ] Claude Code count identity on the wire, with a stub account behind mitmdump (the setup of `scripts/verify-claude-code-identity.sh`; the stub is rejected upstream, the recorded request is the subject):

```bash
CC=$(mktemp -d); mkdir -p "$CC/config" "$CC/mitm"
cat >"$CC/config/config.json" <<'JSON'
{"gatewayOrder":{"order":["claudecode"]},
 "claudecode":{"enabled":true,"baseUrl":"https://api.anthropic.com","accounts":[{"id":"stub","type":"oauth","source":"oauth","enabled":true,"priority":1,"token":"sk-ant-oat01-stub-not-valid","expiresAt":"2099-01-01T00:00:00Z"}]}}
JSON
MITM_DUMP_HOSTS=api.anthropic.com MITM_DUMP_REQUEST_BODY=1 MITM_DUMP_OUT="$CC/observed.jsonl" \
  mitmdump --listen-host 127.0.0.1 --listen-port 18081 --allow-hosts '^api\.anthropic\.com(:[0-9]+)?$' \
  --set confdir="$CC/mitm" -s scripts/mitm_header_dump.py >"$CC/mitm.log" 2>&1 &
until [ -f "$CC/mitm/mitmproxy-ca-cert.pem" ]; do sleep 0.2; done
ANTIGRAVITY_CONFIG_DIR="$CC/config" HTTPS_PROXY=http://127.0.0.1:18081 https_proxy=http://127.0.0.1:18081 \
  SSL_CERT_FILE="$CC/mitm/mitmproxy-ca-cert.pem" bin/antigravity-proxy --port 18099 >"$CC/proxy.log" 2>&1 &
sleep 2
curl -s -X POST http://127.0.0.1:18099/v1/messages/count_tokens -H 'Content-Type: application/json' \
  -H 'X-Claude-Code-Session-Id: 8794f349-ad99-41b4-b0ec-d83c7c6a880a' \
  -d '{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"ok"}],"tools":[]}'
names() { jq -c 'select(.path|startswith("/v1/messages/count_tokens")) | [.headers[][0]]' "$1" | head -1; }
diff <(names .reference/claude-code-count-tokens-20261107.jsonl) <(names "$CC/observed.jsonl") && echo "header names and order match"
jq -r 'select(.path|startswith("/v1/messages/count_tokens")) | .tls.ja4' "$CC/observed.jsonl" | head -1   # t13d1713h1_5b57614c22b0_7f527383e249
```

  Expected: `header names and order match`, the captured fetch-stack JA4, `x-claude-code-request-class` `auxiliary`, and the five-entry `anthropic-beta` in the observed record.

---

## Decisions recorded while planning

Behaviour the spec does not settle, with the decision taken. The spec was updated on 2026-11-07/08 where a decision changes its text.

**Affinity (Tasks 1–3)**
1. *Resolved:* a pinned short wait is bounded by `min(maxStickyWait, maxWaitBeforeErrorMs)` (`Manager.pinWait`, set by `NewDispatcher` and `UpdateConfig`), so a pinned wait never becomes a `RateLimitError` while another account is free. Spec §2 and §4.3 updated.
2. *Accepted, matches today's `sticky`:* transient credential errors (`handleCredentialError` → `MarkFailure(account, "")`) never lock, so retries of a pinned request stay on its account until `maxAttempts`.
3. *Accepted, matches today's `sticky`:* the first two 5xx or network failures retry the pinned account; the third sets the failure lock and the model overflows.
4. *Accepted:* a pinned wait still logs `[Server] All accounts rate-limited for <model>` (`dispatcher.go:439`).
5. `MarkFailure`'s one-minute lock gains an unexported `RateLimit.failure` marker, the only way to tell it from a 60 s rate limit; it is not persisted, so after a reload such a lock reads as a plain rate limit.

**Control plane (Tasks 4–7)**
6. A new prompt is detected with `ccidentity.promptOf`'s rule (non-empty text that is not `<system-reminder>`, or an image or document block), because Claude Code appends `<system-reminder>` text to tool results. Spec §5.3 updated.
7. A start burst counts as done once every call got an HTTP answer, any status; only transport errors, timeouts or an ended request leave it to be retried. Spec §5.5 updated.
8. The burst's concurrent group runs with `sync.WaitGroup.Go`; every call goes through `dispatcher.metered`; the calls use the request's client and project.
9. Per-prompt `loadCodeAssist` makes `accounts.json` saves frequent, which exposed the shared-temp-file race; Task 4 fixes it with the pattern of `internal/openrouter/router.go:93-96`.

**Estimator (Tasks 8–9)**: see the choices listed at the top of Part C (image rule, unreadable images, `cache_control` ignored in digests). Spec §6.3 updated.

**count_tokens (Tasks 10–14)**
10. `dispatch.go` splits into per-gateway matchers and forwards (`routeGateway`, `gateways`), so `/v1/messages` and count_tokens choose the gateway with the same code; existing `TestDispatch_*` tests pin `/v1/messages` routing.
11. The count identity drops `X-Stainless-Timeout` and `x-claude-code-prompt-id`, which real count requests do not carry. The header test skips per-release and per-request values (User-Agent, package and runtime versions, OS, arch, ids, auth) so a Claude Code update does not break it before a recapture.
12. count_tokens stays out of request logging (`shouldSkipLogging`); `TestLoggingMiddleware` needs no change.
13. ADR-0004 gains item 7 (the count identity is a normalization the ADR scopes).
14. The pin `claude.count_tokens_betas` has no automatic observation, because the drift run never reaches `/context`; it moves only with a recapture.

**Out of scope, found while planning**
15. `refreshLiveQuota` (`dispatcher.go:1138`), used by the catalog fetch and `RefreshAccount`, also calls `retrieveUserQuota`, which none of agy's 38 captured requests sends (`.reference/client-drift/agy.jsonl`). It feeds per-model live quota (`Quota.Models`). This plan leaves it unchanged; removing it needs a decision on per-model quota display.
