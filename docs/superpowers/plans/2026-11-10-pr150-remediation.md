# PR #150 Remediation Implementation Plan

> Execute inline, task by task, using `executing-plans`. No subagents, as requested.

**Goal:** Preserve rejection escalation across imposed cooldowns while dropping stale streaks after ten quiet minutes.

**Architecture:** Keep the account-wide, in-memory streak and read-only expiry calculation. Start its quiet clock at the end of an imposed rate-limit cooldown, or at the rejection time when no cooldown is imposed. Success still clears the streak immediately. Do not change cooldown tiers, jitter, routing, or TLS.

**Tech Stack:** Go, the existing account manager, injected `Options.Now`, standard `testing`.

**Spec:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/150 and the Code Review comment on that PR. The PR requires both stale-streak decay and unchanged escalation during a sustained throttle.

## Global Constraints

- Execute inline without subagents; push only to `fork`.
- Preserve the ten-minute quiet window and every existing cooldown tier.
- Cloud Code retains the default Go HTTPS transport; do not change TLS internals.
- Preserve unrelated work in the main checkout and existing untracked files.

## Review Focus

- A retry at the end of the ten-minute top tier must retain its streak.
- A retry after the jittered 12m30s top tier must retain its streak.
- A longer server-supplied cooldown must not count as a quiet outage recovery.
- Short cooldowns must still expire after ten eligible quiet minutes, including the exact boundary.
- Clearing an expired rate-limit entry through selection must not erase the streak clock; success must still clear it.

## Task 1: Exclude imposed cooldown time from streak decay

**Files:**
- Modify: `internal/accounts/manager.go` (failure clock field, `failuresLocked`, `noteFailureLocked`, `MarkRateLimited`, `MarkSuccess`).
- Modify: `internal/accounts/conversation.go` (quiet-window comment only).
- Test: `internal/accounts/manager_test.go`.
- Modify after smoke proof: `README.md` (Cloud Code cooldown policy).

**Interfaces:**
- Consumes: `New(Options)`, `Manager.IncrementFailure(*Account) int`, `Manager.MarkRateLimited(*Account, string, time.Duration)`, `Manager.FailureCount(*Account) int`, `Manager.Select(string, string) Selection`, `SmartBackoff(ErrorReason, time.Duration, int) time.Duration`.
- Produces: unchanged manager methods and cooldown behavior, with a clock whose start is the end of the imposed wait. Rename `Account.LastFailureMS` to `Account.FailureDecayStartMS`; use LSP references/rename to migrate all references, without an alias.

- [x] **Step 1: Write a failing boundary regression.** Append this test; retain the existing success-reset test. Update the existing quiet-window test to measure quiet time after each imposed wait, not during it.

```go
func TestFailureStreakSurvivesImposedCooldown(t *testing.T) {
	t.Parallel()
	for _, wait := range []time.Duration{30 * time.Second, 10 * time.Minute, 12*time.Minute + 30*time.Second, time.Hour} {
		t.Run(wait.String(), func(t *testing.T) {
			now := time.Date(2026, 11, 10, 17, 21, 0, 0, time.UTC)
			account := testAccount("cooldown@example.com")
			manager, err := New(Options{Accounts: []*Account{account}, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				manager.IncrementFailure(account)
			}
			manager.MarkRateLimited(account, "gemini-3.8-flash-high", wait)
			for _, want := range []int{5, 6} {
				now = now.Add(wait)
				if selected := manager.Select("", "gemini-3.8-flash-high").Account; selected != account {
					t.Fatal("account did not become selectable after its cooldown")
				}
				if got := manager.FailureCount(account); got != want {
					t.Fatalf("streak at cooldown expiry = %d, want %d", got, want)
				}
				if got := SmartBackoff(ReasonRateLimit, 0, manager.FailureCount(account)); got != 10*time.Minute {
					t.Fatalf("repeated 429 wait = %s, want 10m", got)
				}
				if want == 5 {
					manager.MarkRateLimited(account, "gemini-3.8-flash-high", wait)
				}
			}
			now = now.Add(10*time.Minute - time.Millisecond)
			if got := manager.FailureCount(account); got != 6 {
				t.Fatalf("streak before quiet expiry = %d, want 6", got)
			}
			now = now.Add(time.Millisecond)
			if got := manager.FailureCount(account); got != 0 {
				t.Fatalf("streak at quiet expiry = %d, want 0", got)
			}
			if got := SmartBackoff(ReasonRateLimit, 0, manager.FailureCount(account)); got != 30*time.Second {
				t.Fatalf("fresh 429 wait = %s, want 30s", got)
			}
		})
	}
}
```

- [x] **Step 2: Run RED.** `go test ./internal/accounts -run '^TestFailureStreakSurvivesImposedCooldown$' -count=1`. Failed as expected in all four duration cases: long cooldowns lost escalation at retry time; the short case expired before ten quiet minutes.
- [x] **Step 3: Apply the minimal fix.** LSP renamed all four clock references. `MarkRateLimited` now starts the decay clock at the imposed cooldown's `ResetTimeMS`; failures without a real cooldown retain the rejection-time clock. Success still resets both fields. Updated the existing comments.
- [x] **Step 4: Run GREEN.** `go test ./internal/accounts -count=1`: 262 passed. Added the README policy explanation after this proof.
- [ ] **Step 5: Commit.** Stage only the four changed source/doc files and this requested plan; run `trail check --base fork/main`, then `git commit -m 'fix(accounts): preserve rejection streaks across cooldowns'`.

## Final Verification and Push

- [ ] Run the project suite once: `go test -json -race ./... -count=1`; count actual test results and retain the output.
- [x] Inspect the final change against the review finding inline. No subagents.
- [ ] A live JA4 capture needs the user's terminal under the standing verification decision; do not claim a new packet match from unit tests or an old capture. Supply the exact command and state the outstanding gate if it is not provided.
- [ ] Push `git push fork HEAD:fix/rate-limit-escalation` only after the test suite passes.
- [ ] Post test evidence and the commit to PR #150; report the resolved finding, test count, PR URL, and any unexercised packet gate.

## Execution Record

- Review: one P1 correctness finding; no actionable Standards findings. Posted to PR #150.
- Baseline: `go test ./internal/accounts` passed, 257 tests.
- Ruling: the ten-minute quiet window excludes an imposed cooldown, because no request can establish recovery while the proxy blocks retries. Tradeoff: stale streaks decay later by the cooldown duration; explicit success still resets them immediately.
- Ruling: use Go commands rather than the skill's Python examples, and `fork` rather than `origin`, following this repository's toolchain and remote rule.
- Ruling: review and implementation remain inline; the user's no-subagent instruction also excludes a final reviewer agent.
- Verification: `trail check --base fork/main` found no conflicts. Its advisory about a token-file read lock does not apply: this fix changes no token reads and all streak access remains protected by the manager's lock.
- Inline review: all four renamed field uses migrated; the cooldown clock is set only after the previous streak is evaluated, persists when selection removes expired limits, and is cleared on success. No cooldown tiers, jitter, request identity, or TLS code changed.
