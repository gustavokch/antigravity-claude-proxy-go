# PR #78 Second-Pass Remediation Plan — empty-key guards deleted the listing cooldown

**Goal:** Fix the regression found in the second review of PR #78: the `if key != ""` guards added in `8df166b` silently removed the model-listing rate-limit namespace (`""`), so a 429 during `fetchAvailableModels` no longer takes the account out of rotation. Also cover the untested listing-429 rotation path, and fix the catalog-lowercase test whose assertion does not match its comment's claim.

**Architecture:** Go HTTP proxy. `internal/accounts/manager.go` owns `Account.ModelRateLimits` and is the normalization point for its key namespace (`rateLimitModelKey`, `manager.go:414`). Two namespaces share the map: catalog-resolved model IDs, and `""` — the deliberate listing-namespace written and cleared by `fetchAvailableModels` via `rotateForError` (`dispatcher.go:257`) and `MarkSuccess` (`dispatcher.go:251`).

**Tech Stack:** Go, `net/http/httptest`, standard `go test`.

**Base commit:** `a6495b7` on `fix/classifier-fallback-1m-model-key`.

**Review reference:** posted as PR comment on #78. Finding 1 reproduced by a scratch probe (deleted after running): two accounts, `StrategySticky`, `MarkRateLimited(a, "", time.Hour)` → on this branch `Available("") == 2` and `Select("")` returns `a` again; on `main` it returns `b`. Tree is clean — no probe code remains.

---

## Review verdict

```
BLOCK:   2 — Tasks 1-2. Merge with the guard as written breaks listing-429
             rotation and strands pre-upgrade "" entries until expiry.
HARDEN:  2 — Tasks 3 (no test covers the broken path), 4 (test claims more
             than it pins).
DEFER:   3 — management.go:361 dashboard duplicate rows and server.go:1002
             display-name/alias capacity gap are both latent only; Task 4's
             outcome decides how they are recorded. See "Deferred".
gate:    NO-GO until Tasks 1-2 land and the suite is green.
```

The original `[1m]` fix stands and its tests pass. The regression is in the hardening commits layered on top: the empty-key guards treated all blank-normalizing inputs identically, but `""` and `"[1m]"` are not the same case — the former is a namespace in active use, the latter is a malformed model string.

---

## Task 1: Keep `""` writable in `MarkRateLimited`; only reject non-blank → blank

**Files:**
- Modify: `internal/accounts/manager.go` (`MarkRateLimited` L435-451)
- Test: `internal/accounts/manager_test.go`

**Consumes:** `rateLimitModelKey`.
**Produces:** `MarkRateLimited(acc, "", d)` writes `ModelRateLimits[""]` again (pre-PR behavior), while `MarkRateLimited(acc, "[1m]", d)` still writes nothing.

**The defect:** the guard `if key := rateLimitModelKey(model); key != ""` (manager.go:444) skips the write for `model == ""`, which is exactly the call `rotateForError` makes for the listing path on a 429 (`dispatcher.go:257` → `dispatcher.go:609`). With nothing written, `usableLocked(account, "")` returns true and the account keeps serving listing requests into a repeated 429. `CoolingDownUntilMS` is not a fallback — it is never set by this path.

**The distinction:** guard on the *raw* input, not the normalized key. Skip only when the caller passed a non-blank string that normalized to blank.

### Step 1 — Failing test

Add `TestMarkRateLimited_EmptyModelWritesListingNamespace` to `manager_test.go`:

```go
func TestMarkRateLimited_EmptyModelWritesListingNamespace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := testAccount("listing-429@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// "" is the deliberate model-listing namespace written by rotateForError
	// (dispatcher.go:257 -> MarkRateLimited(account, "", wait)). Dropping the
	// write leaves the account selectable after a listing 429.
	manager.MarkRateLimited(account, "", time.Hour)
	if limit := account.ModelRateLimits[""]; limit == nil || !limit.IsRateLimited {
		t.Fatalf("MarkRateLimited(account, \"\") must write the listing namespace: %#v", account.ModelRateLimits[""])
	}

	// A non-blank string that normalizes to blank is a malformed model, not
	// the listing namespace, and must still be rejected.
	manager.MarkRateLimited(account, "[1m]", time.Hour)
	manager.MarkRateLimited(account, "   ", time.Hour)
	for key := range account.ModelRateLimits {
		if key != "" {
			t.Fatalf("unexpected key %q written by a blank-normalizing model", key)
		}
	}
}
```

### Step 2 — Confirm failure

```bash
go test ./internal/accounts/ -run TestMarkRateLimited_EmptyModelWritesListingNamespace -v -count=1
```

Expect FAIL: `ModelRateLimits[""]` is nil after `MarkRateLimited(account, "", time.Hour)`.

### Step 3 — Minimal implementation

In `MarkRateLimited`, replace the guard:

```go
key := rateLimitModelKey(model)
if key == "" && strings.TrimSpace(model) != "" {
	// A non-blank model string that normalizes to blank (suffix only,
	// e.g. "[1m]") is malformed; do not write it into the empty-model
	// namespace model listing uses deliberately. An actual "" caller means
	// the listing namespace and must still be recorded.
	account.ConsecutiveFailure++
	manager.recordRateLimitLocked(account.Email)
	return
}
account.ModelRateLimits[key] = &RateLimit{
	IsRateLimited: true, ResetTimeMS: manager.now().Add(wait).UnixMilli(), ActualResetMS: wait.Milliseconds(),
}
account.ConsecutiveFailure++
manager.recordRateLimitLocked(account.Email)
```

(Or equivalently, guard the write with `if key != "" || strings.TrimSpace(model) == ""` — pick whichever reads cleaner; keep the failure-recording side effects unconditional, matching current code.)

### Step 4 — Confirm pass

```bash
go test ./internal/accounts/ -count=1
```

### Step 5 — Commit

```bash
git add internal/accounts/manager.go internal/accounts/manager_test.go
git commit -m "fix(accounts): keep the listing namespace writable in MarkRateLimited; reject only blank-normalizing non-blank models"
```

---

## Task 2: Restore the paired `delete` in `MarkSuccess`

**Files:**
- Modify: `internal/accounts/manager.go` (`MarkSuccess` L479-489)
- Test: `internal/accounts/manager_test.go`

**Consumes:** Task 1's distinction.
**Produces:** `MarkSuccess(acc, "")` clears `ModelRateLimits[""]` again; `MarkSuccess(acc, "[1m]")` clears nothing.

**The defect:** same guard shape, opposite direction. `dispatcher.go:251` calls `MarkSuccess(account, "")` after a successful listing; on `main` that deletes the `""` entry written by an earlier listing 429. On this branch it never deletes. Consequence beyond the live path: any `ModelRateLimits[""]` entry persisted by a pre-upgrade binary (the map round-trips through JSON, manager.go:111/156) can now only be removed by `clearExpiredLocked` at expiry — a pre-upgrade listing 429 with a long wait is stuck for its full duration.

### Step 1 — Failing test

Add `TestMarkSuccess_EmptyModelClearsListingNamespace`:

```go
func TestMarkSuccess_EmptyModelClearsListingNamespace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := testAccount("listing-clear@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	manager.MarkRateLimited(account, "", time.Hour)
	manager.MarkSuccess(account, "")
	if account.ModelRateLimits[""] != nil {
		t.Fatalf("MarkSuccess(account, \"\") must clear the listing namespace: %#v", account.ModelRateLimits[""])
	}

	// A blank-normalizing non-blank string must not touch the namespace.
	manager.MarkRateLimited(account, "", time.Hour)
	manager.MarkSuccess(account, "[1m]")
	if account.ModelRateLimits[""] == nil {
		t.Fatal("MarkSuccess(account, \"[1m]\") cleared the listing namespace")
	}
}
```

This test depends on Task 1 (the seed write must work to have something to clear).

### Step 2 — Confirm failure

```bash
go test ./internal/accounts/ -run TestMarkSuccess_EmptyModelClearsListingNamespace -v -count=1
```

Expect FAIL on the first assertion (entry survives the `""` success).

### Step 3 — Minimal implementation

Same raw-input distinction in `MarkSuccess`:

```go
key := rateLimitModelKey(model)
if key != "" || strings.TrimSpace(model) == "" {
	delete(account.ModelRateLimits, key)
}
```

### Step 4 — Confirm pass

```bash
go test ./internal/accounts/ -count=1
```

### Step 5 — Commit

```bash
git add internal/accounts/manager.go internal/accounts/manager_test.go
git commit -m "fix(accounts): let MarkSuccess clear the listing namespace again"
```

---

## Task 3: Cover listing-path 429 rotation end to end

**Files:**
- Test: `internal/accounts/dispatcher_test.go`

**Consumes:** `newDispatcherWithClient` (dispatcher_test.go:53), `cloudcode.HTTPError` (client.go:87), Tasks 1-2.
**Produces:** the regression Task 1 fixes can no longer land silently — `go test ./...` was green with it in place.

**The gap:** nothing exercises `rotateForError(account, "", err)` with a 429. Every existing dispatcher test either returns a success body or a non-HTTP error, and no manager test calls `MarkRateLimited` with `""`.

### Step 1 — Failing test

Add a client whose `FetchAvailableModels` returns `&cloudcode.HTTPError{StatusCode: http.StatusTooManyRequests, Body: ...}` on every call, and a dispatcher with a two-account manager:

```go
func TestFetchAvailableModels_RateLimitedAccountRotates(t *testing.T) {
	client := &alwaysRateLimitedModelsClient{} // returns 429 HTTPError, counts calls
	manager, err := New(Options{Accounts: []*Account{
		{Email: "first@example.com", Enabled: true},
		{Email: "second@example.com", Enabled: true},
	}, Strategy: StrategySticky})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return client },
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = dispatcher.FetchAvailableModels(context.Background())
	if err == nil {
		t.Fatal("want error after both accounts 429")
	}
	// The first account's 429 must mark it so the retry goes to the second;
	// pre-fix, the write is dropped and the loop retries the same account
	// (or, with maxRetries < Count()+1, never reaches the second).
	if got := client.calls.Load(); got < 2 {
		t.Fatalf("upstream calls = %d, want >= 2 (rotation across accounts)", got)
	}
	if limit := manager.accounts[0].ModelRateLimits[""]; limit == nil {
		t.Fatal("the 429 on the listing path must write the empty-model namespace")
	}
}
```

Note on the second account: sticky strategy's `selectStickyLocked` also reads `ModelRateLimits[rateLimitModelKey("")]` for the wait-cap path, which works once Task 1 lands. `manager.accounts` is unexported — the test lives in package `accounts`, so access is fine.

Run it on the pre-Task-1 tree to prove it fails, then on the fixed tree:

```bash
git stash && go test ./internal/accounts/ -run TestFetchAvailableModels_RateLimitedAccountRotates -v -count=1; git stash pop
```

### Step 2 — Confirm pass

```bash
go test ./internal/accounts/ -count=1
```

### Step 3 — Commit

```bash
git add internal/accounts/dispatcher_test.go
git commit -m "test(accounts): pin listing-path 429 rotation through rotateForError"
```

---

## Task 4: Make the catalog-lowercase test pin what its comment claims

**Files:**
- Modify: `internal/modelcatalog/catalog_test.go` (`TestCatalogByIDKeysAreLowercase` L651-678)
- Investigate only: whether any live upstream catalog ID is actually mixed-case.

**Consumes:** nothing new.
**Produces:** a test that asserts over `catalog.Selectable()[].ID` (the value that actually reaches `MarkRateLimited` via `modelDetails.ID`, dispatcher.go:278), and a recorded yes/no on whether the concern is live.

**The defect in the test:** `Parse` stores `Model{ID: id}` with the upstream case (catalog.go:175) and only lowercases the map key (catalog.go:197). The test iterates `catalog.byID` keys — guaranteed lowercase by construction — so it cannot fail. Its own fixture (`Gemini-3.1-Pro-High`) produces a mixed-case `Model.ID`, which is exactly the shape the comment says it guards against.

### Step 1 — Extend the assertion

After the existing loop, add:

```go
for _, m := range catalog.Selectable() {
	if m.ID != strings.ToLower(m.ID) {
		t.Fatalf("Selectable model ID %q is not lowercase; rate-limit keys are lowercased on read, so a limit recorded under this ID would be invisible", m.ID)
	}
}
```

Run it. It **fails on the current fixture** — that is the point: either the fixture is unrealistic (all real upstream IDs are lowercase) or the concern is live.

### Step 2 — Resolve the fork

- **If real upstream catalogs are all-lowercase:** change the fixture's `Gemini-3.1-Pro-High` entries to lowercase so the test passes, and note in the comment that mixed-case IDs would break the rate-limit join — the test now genuinely pins the precondition.
- **If mixed-case IDs occur upstream:** this escalates. `GetUpstreamID()` falls back to `m.ID` (catalog.go:31-36), so lowercasing at `Parse` changes the bytes sent upstream — out of scope for this PR. Record it as a known limitation in the PR thread and open a follow-up issue covering both the rate-limit join and the management.go:361 dashboard duplicate rows.

### Step 3 — Commit

```bash
git add internal/modelcatalog/catalog_test.go
git commit -m "test(modelcatalog): pin lowercase Selectable IDs, the value that keys rate limits"
```

---

## Deferred (no task this PR)

- **`management.go:361` dashboard duplicates** — latent unless Task 4 finds mixed-case upstream IDs; folded into the follow-up issue in that case.
- **`server.go:1002` capacity check with display-name/alias `TargetModel`** — `Available(model)` compares the classifier target string against exhaustion recorded under the post-`Resolve` ID; `Resolve` also matches via `byDisplay`/`routingAliases` (catalog.go:254-266), so a display-name target misses. Closing it needs a catalog resolve before the capacity check, and the server's dispatcher has no exported catalog accessor today (`resolveModel` is unexported and refreshes upstream when stale — wrong for a hot path). Reachable only via operator config, not client input. Open a follow-up issue; do not widen this PR.
- **`catalog_test.go:675` redundant byID lookup** — the final `byID["gemini-3.1-pro-high"]` assertion cannot fail independently of the loop above it. Trivial; fold into Task 4's edit if touching those lines anyway, otherwise leave.

---

## Execution order

```
Task 1 ──┐
         ├─> Task 3 (proves the rotation end to end)
Task 2 ──┘
Task 4 (independent; can run any time)
```

Full suite green (`go test ./... -count=1`) after Task 3, then push. Tasks 1+2 are one logical fix but commit separately so each half of the write/clear pair is revertible alone.
