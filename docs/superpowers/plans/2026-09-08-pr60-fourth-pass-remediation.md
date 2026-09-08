# PR 60 Fourth Pass Review Remediation Plan

Findings from fourth pass ([review comment](https://github.com/gustavokch/antigravity-claude-proxy-go/pull/60#issuecomment-5577027420)):

1. `selectRoundRobinLocked` modulo-by-zero on empty pool (no `len==0` guard, unlike sticky/Available). Div-by-zero panic when strategy is round-robin and pool empty.
2. Quota/ratelimit map keys enter `models` list with no `""` filter in `handleAccountLimits`.
3. Merge into existing env map still mutates shared nested map from disk read (aliasing across callers holding `ReadClaudeConfig` result).
4. Bare `[1m]` guard returns unsanitized original with surrounding whitespace intact.

## Task 1: ~~Guard selectRoundRobinLocked~~ — DROPPED (false positive)

Verified via `git blame` + empty-pool probe: the `len==0` guard already exists at `manager.go:681-683` (added in `10a576c`). `Select` on an empty round-robin pool returns `Selection{}` with no panic. Correction posted to PR. No change needed.

## Task 2: Skip empty quota/ratelimit keys in handleAccountLimits models list

- **Files:**
  - Modify: `internal/api/management.go`
  - Test: `internal/api/management_test.go`
- **Step 1 (Red):** Extend `TestManagement_HealthAndLimits` subtest (or add new `empty quota keys` case) with backend returning quota map containing `""` key; assert `""` absent from `models`.
- **Step 2:** Run `go test -run TestManagement_HealthAndLimits -v ./internal/api` — expect failure.
- **Step 3 (Green):** Add `if m == "" { continue }` guards in both `acc.Quota.Models` and `acc.ModelRateLimits` loops.
- **Step 4:** Run `go test ./internal/api -v` — all pass.
- **Step 5:** Commit: `git commit -m "fix(api): skip empty quota keys in account-limits models list"`

## Task 3: ~~Copy nested env map~~ — DROPPED (false positive)

Verified: each `ReadClaudeConfig` call `json.Unmarshal`s a fresh map, so the merge mutates only its own disk read. Added `TestUpdateClaudeConfig_DoesNotAliasExistingEnv` as regression guard — passes on current code.

## Task 4: ~~Return trimmed value~~ — DROPPED (false positive)

Verified: `sanitizeModelValue` trims `s` on entry, so bare-suffix branch already returns trimmed value. Added `TestUpdateClaudeConfig_BareSuffixValueTrimmed` as regression guard — passes on current code.

## Task 5: Full suite verification & push

- **Step 1:** `go test ./...` — 100% green gate.
- **Step 2:** `git push` to PR branch.
