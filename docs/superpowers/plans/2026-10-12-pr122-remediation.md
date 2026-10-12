# Remediation Plan: PR #122 Review Remediation

- **PR:** #122 (fix/claude-5-5-cloud-code-routing)
- **Goal:** Fix data races, state loss, and missing debouncing in model catalog recovery; harden Claude Code router tiered variant refusal; add rigorous test coverage.
- **Tech Stack:** Go 1.27rc2, standard library.

## Findings & Resolutions

1. **`internal/accounts/dispatcher.go:569`** (Data race)
   - *Problem:* Reads `dispatcher.catalog` without `dispatcher.mu`.
   - *Fix:* Replaced inline recovery with `recoverModelFromQuotaCache` which reads `CachedCatalog()`.

2. **`internal/accounts/dispatcher.go:577`** (Lost pool state on recovery)
   - *Problem:* Iterated over `GetAllAccounts()` which returns clones. `MarkSuccess`/`MarkFailure` mutated the clones, losing state.
   - *Fix:* Added `manager.AccountsWithQuotaModel(model)` returning live `*Account` pointers under read lock.

3. **`internal/accounts/dispatcher.go:583`** (No debounce on recovery)
   - *Problem:* Every request for an unpublishable model triggered synchronous upstream RPCs.
   - *Fix:* Added `claimRecoveryAttempt` debouncing per model by `missingModelFetchFloor` (5m).

4. **`internal/accounts/dispatcher.go:333`** (Unbounded timeout)
   - *Problem:* Upstream fetch ran on bare request context.
   - *Fix:* Added `context.WithTimeout(ctx, fetchModelsTimeout)`.

5. **`internal/accounts/dispatcher.go:348`** (Dropped upstream errors)
   - *Problem:* 401/403/429 errors from recovery fetch did not trigger `rotateForError`.
   - *Fix:* Call `rotateForError(account, "", err)` on fetch failure.

6. **`internal/accounts/dispatcher.go:594`** (Premature loop abort)
   - *Problem:* Stopped after first account even if its fetch lacked the model.
   - *Fix:* Try next account until one succeeds and resolves the model.

7. **`internal/claudecode/router.go:281`** (Out-of-bounds slice risk)
   - *Problem:* `isTierVariantOf` sliced without `HasPrefix`.
   - *Fix:* Used `strings.CutPrefix(requested, canonical)` and reused `isThinkingTier`.

8. **`internal/claudecode/router.go:227`** (Un-normalized argument)
   - *Problem:* Passed raw `req` instead of `normalized`.
   - *Fix:* Pass `normalized`.

9. **`internal/claudecode/router_test.go`** (Duplicate test entries)
   - *Problem:* Duplicated model checks.
   - *Fix:* Consolidated test cases.

10. **`internal/accounts/model_recovery_test.go`** (Missing test coverage)
    - *Fix:* Added comprehensive tests for poisoned catalog recovery, debouncing, and failure recording on live pool accounts.
