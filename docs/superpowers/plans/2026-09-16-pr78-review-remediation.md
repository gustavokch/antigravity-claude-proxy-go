# PR #78 Remediation Plan — rate-limit key namespace normalization

**Goal:** Close the gaps found in review of PR #78 (`fix/classifier-fallback-1m-model-key`): verify the shipped test is genuinely red-green, fix an empty-key write path in `MarkFailure`, add package-level coverage for the key symmetry, tighten the new API test's assertion, and reconcile a code comment that overclaims the blast radius of the fix.

**Architecture:** Go HTTP proxy. `internal/accounts/manager.go` owns the `Account.ModelRateLimits` map and is now the normalization point for its key namespace (`rateLimitModelKey`, `manager.go:409-411`). The dispatcher always passes catalog-resolved IDs (`dispatcher.go:277`, `model := modelDetails.ID`); the single foreign caller passing a raw client-facing string is the classifier fallback capacity check (`server.go:1002`, `Available(model)`).

**Tech Stack:** Go, `net/http/httptest`, standard `go test`.

**Base commit:** `de12f24` on `fix/classifier-fallback-1m-model-key`.

**Review reference:** findings below were gathered by reading the PR diff and tracing call sites; none were reproduced by running code. Task 1 exists to correct that.

---

## Review verdict

```
BLOCK:   0
HARDEN:  4 — Tasks 1-4 below
NOTE:    2 — see "Notes (no task)" at the end
gate:    GO for build, conditional on Task 1 confirming red-green.
         If Task 1 shows the shipped test passes without the fix, it is not
         evidence of anything and Task 1 escalates to BLOCK.
```

The PR's root-cause analysis holds. Traced and confirmed by reading: the dispatcher writes under the catalog ID, `server.go:1002` reads with the raw client string, and every `ModelRateLimits` read/write site in `manager.go` is now routed through `rateLimitModelKey`. Remaining references to the map (`manager.go:650, 770, 1017, 1336, 1377`; `management.go:287, 361, 444, 496`) are read-only iterations over keys, which normalization at the writers makes consistent. The findings below are hardening, not defects in the fix's direction.

---

## Task 1: Verify the shipped test is red-green

**Files:**
- Temporarily modify (then revert): `internal/accounts/manager.go`
- Exercise: `internal/api/classifier_fallback_test.go`

**Consumes:** the PR as committed at `de12f24`.
**Produces:** evidence that `TestMessages_ClassifierFallback_RateLimitedSuffixedModelStubs` fails without the normalization. No committed change.

This is a pre-flight gate, not a TDD cycle. Every other task assumes the fix works; this is the only step that checks it.

### Step 1 — Neutralize the normalization

In `internal/accounts/manager.go:409-411`, replace the body of `rateLimitModelKey` with a passthrough:

```go
func rateLimitModelKey(model string) string {
	_ = modelcatalog.Strip1mSuffix
	return model
}
```

The discard keeps the `modelcatalog` import live so the package still compiles.

### Step 2 — Confirm failure

```bash
go test ./internal/api/ -run TestMessages_ClassifierFallback -v
```

Expect `TestMessages_ClassifierFallback_RateLimitedSuffixedModelStubs` to FAIL with `backend was dispatched to`, and the other tests in the group to PASS. If it fails on the status assertion instead of `backend.hit`, that is a different failure mode — investigate before continuing, because it means the request never reached the dispatch decision.

Expected mechanics, for comparison against what actually happens: the test server's `backend` is `dispatchRecordingBackend` (`classifier_fallback_test.go:76-91`), so `server.messages` dispatches straight to it without going through `modelcatalog.Resolve`. Pre-fix, `Available("gemini-3.8-flash-medium[1m]")` misses the limit recorded under `gemini-3.8-flash-medium`, `noCapacity` is false, and the backend is hit.

### Step 3 — Revert

Restore `rateLimitModelKey` to its committed body and confirm a clean tree:

```bash
git diff --stat   # expect empty
```

### Step 4 — Confirm the suite is green at base

```bash
go test ./... -count=1
```

No commit for this task.

---

## Task 2: Normalize before the empty-model guard in `MarkFailure`

**Files:**
- Modify: `internal/accounts/manager.go` (`MarkFailure` L453-463, `MarkSuccess` L465-471)
- Test: `internal/accounts/manager_test.go`

**Consumes:** `rateLimitModelKey`.
**Produces:** a model string that normalizes to empty can no longer write `ModelRateLimits[""]`.

**The defect:** `MarkFailure` guards on the *raw* argument (`if model != "" && ...`, `manager.go:458`) but writes under the *normalized* key (`manager.go:459`). An input of `"[1m]"` or `"   "` passes the guard and produces key `""`. That collides with the empty-model namespace the model-listing path uses (`dispatcher.go:227` `Select("")`, `dispatcher.go:236` `MarkFailure(account, "")`), so the account would read as rate-limited for model listing. The existing invariant at `manager_test.go:156-157` asserts `ModelRateLimits[""]` is never created by an empty-model `MarkFailure`; this widens the set of inputs that should satisfy it.

**Reachability caveat:** no current caller can produce such an input — every `MarkFailure` call site in `dispatcher.go` passes either `modelDetails.ID` or a literal `""`. This is a latent inconsistency between the guard and the write, not a live bug. It is worth fixing precisely because the PR's stated intent is to make the manager safe for callers that pass raw client strings.

### Step 1 — Failing test

Add `TestMarkFailure_SuffixOnlyModelDoesNotCreateEmptyKey` to `internal/accounts/manager_test.go`, modeled on the existing empty-model case at L150-170:

- Build a manager with one account (reuse the fixture pattern at L290).
- Call `manager.MarkFailure(account, "[1m]")` three times, then again with `"   "` three times.
- Assert `account.ModelRateLimits[""] == nil`.

### Step 2 — Confirm failure

```bash
go test ./internal/accounts/ -run TestMarkFailure_SuffixOnlyModelDoesNotCreateEmptyKey -v
```

Expect FAIL: the empty key exists.

### Step 3 — Minimal implementation

In `MarkFailure`, hoist the key and guard on it:

```go
key := rateLimitModelKey(model)
if key != "" && account.ConsecutiveFailure >= 3 {
	account.ModelRateLimits[key] = &RateLimit{...}
}
```

Apply the same hoist in `MarkRateLimited` (`manager.go:436`) — it has no empty guard today, so add one rather than writing a `""` entry — and in `MarkSuccess` (`manager.go:469`), skip the `delete` when the key is empty so a suffix-only success cannot clear a legitimate empty-model entry.

Decide explicitly whether `MarkRateLimited` gaining an empty-key guard changes existing behavior: today `MarkRateLimited(acc, "", d)` writes `ModelRateLimits[""]`, and `dispatcher.go:227-251` uses the empty-model namespace deliberately. **Do not** add the guard to `MarkRateLimited` if any test or call path depends on that write — grep first:

```bash
grep -rn 'MarkRateLimited(.*, *"" *,' internal/
```

If there are no hits, guard it. If there are, leave `MarkRateLimited` alone and note why in a comment.

### Step 4 — Confirm pass

```bash
go test ./internal/accounts/ -count=1
```

### Step 5 — Commit

```bash
git add internal/accounts/manager.go internal/accounts/manager_test.go
git commit -m "fix(accounts): guard rate-limit writes on the normalized key, not the raw model"
```

---

## Task 3: Cover key symmetry in the accounts package

**Files:**
- Test: `internal/accounts/manager_test.go`

**Consumes:** `MarkRateLimited`, `MarkSuccess`, `Available`, `MinWait`.
**Produces:** direct coverage of the namespace contract, independent of the API layer.

**The gap:** the PR's only test for this behavior is an end-to-end API test. The contract it establishes — that the rate-limit map has one canonical key namespace — lives in `internal/accounts`, and nothing there asserts it. A refactor inside `manager.go` could break the contract and leave the API test as the only tripwire, which points at the wrong package.

### Step 1 — Failing test

Add `TestRateLimitKeyNamespaceIsSuffixAndCaseInsensitive`:

- One enabled account. `manager.MarkRateLimited(acc, "gemini-3.8-flash-medium", time.Hour)`.
- Assert `manager.Available("gemini-3.8-flash-medium[1m]") == 0`.
- Assert `manager.Available("GEMINI-3.8-Flash-Medium[1M]") == 0`.
- Assert `manager.MinWait("gemini-3.8-flash-medium[1m]") > 0`.
- Assert a *different* model is unaffected: `manager.Available("gemini-3.8-flash-low[1m]") == 1`.
- Then `manager.MarkSuccess(acc, "gemini-3.8-flash-medium[1m]")` and assert `manager.Available("gemini-3.8-flash-medium") == 1` — the reverse direction, clearing via a suffixed string.

Note `Strip1mSuffix` matches the suffix case-insensitively (`catalog.go:230-232` lowercases before `HasSuffix`), so the `[1M]` case above should pass; if it does not, that is a finding in `Strip1mSuffix`, not in this test.

### Step 2 — Confirm failure

Run against the neutralized `rateLimitModelKey` from Task 1 to prove the test has teeth, then revert:

```bash
go test ./internal/accounts/ -run TestRateLimitKeyNamespaceIsSuffixAndCaseInsensitive -v
```

### Step 3 — Confirm pass on the real implementation

```bash
go test ./internal/accounts/ -count=1
```

No implementation change expected. If one is needed, that is a new finding — record it before writing code.

### Step 4 — Commit

```bash
git add internal/accounts/manager_test.go
git commit -m "test(accounts): pin the rate-limit key namespace contract"
```

---

## Task 4: Tighten the new API test and narrow the overclaiming comment

**Files:**
- Modify: `internal/api/classifier_fallback_test.go` (the test added at L247-273)
- Modify: `internal/accounts/manager.go` (the `rateLimitModelKey` doc comment, L402-408)

**Consumes:** nothing new.
**Produces:** an assertion that the stub is the *right* stub, and a comment that matches what the code actually guarantees.

**Finding 4a — the new test under-asserts.** `TestMessages_ClassifierFallback_RateLimitedSuffixedModelStubs` checks `!backend.hit` and `rec.Code == 200`, but not the body. Its sibling `TestMessages_ClassifierFallback_StubsWhenNoCapacity` (L138-158) additionally asserts the body parses and contains exactly one block of `<severity>0</severity>`. A 200 with an empty or malformed body would pass the new test.

**Finding 4b — the comment overclaims.** `manager.go:402-408` and the `Strip1mSuffix` doc at `catalog.go:226-229` both state the normalization makes any caller passing a client-facing string safe. It does not. `Select` reaches `quotaCriticalLocked` (`manager.go:853-870`) and `scoreLocked` (`manager.go:872-896`), which index `account.Quota.Models[model]` (L854, L888) and `account.ModelThreshold[model]` (L864) with the **raw** argument. `Select("gemini-3.8-flash-medium[1m]")` would therefore normalize the rate-limit lookup but silently miss quota-critical detection and fall back to the default score of `50.0`. Only the rate-limit namespace was normalized.

**Recommendation: narrow the comment rather than normalize the quota maps.** `Quota.Models` is populated from upstream quota responses and its keys are joined against catalog IDs in the dashboard (`management.go:361, 456`); changing its key namespace is a wider change than this PR's scope and risks the dashboard join. The only foreign caller today is `Available` (`server.go:1002`), which does not touch quota. Scope the claim to what is true.

### Step 1 — Extend the test

Add to `TestMessages_ClassifierFallback_RateLimitedSuffixedModelStubs` the same body assertion used at L148-158: unmarshal into the `stub` struct and require a single content block equal to `<severity>0</severity>`.

### Step 2 — Confirm still passing

```bash
go test ./internal/api/ -run TestMessages_ClassifierFallback -v
```

If the body assertion fails, the fallback is returning a stub that differs from the no-capacity path for suffixed models — a real finding, stop and record it.

### Step 3 — Narrow the comments

Rewrite the second half of `manager.go:402-408` to claim only the rate-limit namespace, e.g.:

> Only `ModelRateLimits` is normalized. `Quota.Models` and `ModelThreshold` (see `quotaCriticalLocked`, `scoreLocked`) are still indexed by the raw argument and remain catalog-ID-only — a caller passing a client-facing string gets a correct rate-limit answer but a default quota score.

Apply the same narrowing to `catalog.go:226-229`, which currently implies the whole lookup path is safe.

### Step 4 — Commit

```bash
git add internal/api/classifier_fallback_test.go internal/accounts/manager.go internal/modelcatalog/catalog.go
git commit -m "test(api): assert stub content for suffixed models; docs: scope the normalization claim"
```

---

## Task 5: Retire or escalate the catalog-ID-case notes

**Files:**
- Investigate only: `internal/modelcatalog/catalog.go`, persisted account state.

**Produces:** a yes/no answer that either closes both notes below or turns them into tasks.

Catalog IDs originate from the upstream document's `Models` map keys (`catalog.go:184`) plus the synthetic Gemini 3.7/3.8 variant IDs (`catalog.go:556, 613`). The synthetic ones are lowercase literals. The upstream-supplied ones are not verified.

### Step 1 — Determine whether any catalog ID contains an uppercase character

Inspect a captured upstream catalog response, or add a temporary assertion in `catalog_test.go` over the fixture, checking `id == strings.ToLower(id)` for every entry in `catalog.byID`.

### Step 2 — If all IDs are lowercase

Both notes are moot. Record the finding in the PR thread and close. Optionally keep the assertion as a permanent regression test — it is cheap and it is the precondition the normalization relies on.

### Step 3 — If any ID has uppercase

Two consequences become real and need tasks:

- **Persisted state.** `ModelRateLimits` round-trips through JSON (`manager.go:88, 111, 156`). Entries written before this PR carry the un-lowercased catalog ID; after upgrade every read lowercases, so those entries go invisible until re-marked. A one-time key migration at load (`manager.go:156-163`) would be needed.
- **Dashboard duplicates.** `management.go:361` unions `ModelRateLimits` keys (now lowercase) into the same `modelSet` as catalog `m.ID` (mixed case), producing two rows for one model.

---

## Notes (no task)

- **Layering.** `internal/accounts` now imports `internal/modelcatalog` (`manager.go:17`). No cycle — it compiles — but a low-level account store now depends on the catalog package. Acceptable given the fix must live at the namespace owner; worth watching if the dependency grows.
- **Per-call normalization cost.** `usableLocked` (`manager.go:757`) computes the key per account per call, and is invoked in loops from `Available` (L379) and the scored/sticky selectors (L672-734). `MinWait` already hoists the key out of its loop (L418). The cost is a `TrimSpace` + `HasSuffix` + `ToLower`, and Go's `strings.ToLower` returns the input unchanged without allocating when there is nothing to fold, so for lowercase IDs this is effectively free. Hoisting in `Available` and `selectScored` would be tidier but is not worth a commit on its own; fold it in if those functions are touched for another reason.
