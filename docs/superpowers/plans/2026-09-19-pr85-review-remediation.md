# PR #85 Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve all eleven code review findings on PR #85, covering quota freshness semantics, model key normalization, rate limit encapsulation, reset timestamp fallback, and WebUI reactivity.

**Architecture:** Separate the two questions `isQuotaActiveLocked` currently conflates — "is this quota record fresh enough to trust?" and "is the exhaustion window still open?" — so that a stale record cannot masquerade as live capacity; encapsulate Claude Code rate-limit calculations directly within `claudecode.RateLimits` to eliminate feature envy; extract `ModelKeyCandidates` in `internal/accounts` to unify model name normalization across rate limit, threshold, and quota lookups, and route the existing `rateLimitModelKey` through it so one rule has one spelling; propagate that normalization into the `/account-limits` display path, which ADR 0002 requires to resolve individual aliases; optimize Alpine.js WebUI rendering with reactive getter properties.

**Tech Stack:** Go 1.27rc2, Alpine.js 3, HTML5/TailwindCSS.

**Spec:** The code review posted on PR #85 —
https://github.com/gustavokch/antigravity-claude-proxy-go/pull/85#issuecomment-5743709689
Findings 1, 2 and 5 block merge; the rest are safe as follow-ups. Section "Findings coverage" at the end of this plan maps every finding to the task that closes it.

## Global Constraints
- Zero external dependencies added.
- All Go tests must pass race detection (`go test -race`).
- `strings.ToLower` candidate matching in `internal/accounts` is retained, but expressed once in `ModelKeyCandidates` rather than re-spelled per call site.
- The null-or-undefined guard in `account-manager.js` is retained; only its spelling is at issue (finding 11), and that change is optional.
- WebUI reactivity must be preserved using Alpine.js reactive getters (`get quota()`).
- **Behaviour invariant:** a quota record that is both stale (outside the `quotaFresh` window) *and* reporting spare capacity must never be treated as authoritative. Only an *exhausted* record may outlive the freshness window, and only until its `ResetTime`.

---

### Task 1: Quota Freshness Semantics and Model Key Candidates in Accounts Manager

**Closes findings:** 1 (blocking), 5 (blocking), 6, 7, 8, and the manager half of 4.

**Files:**
- Modify: `internal/accounts/manager.go:414-426` (stale comment), `:423-430` (`rateLimitModelKey`), `:975-1045` (helpers and quota evaluation)
- Test: `internal/accounts/manager_test.go`

**Interfaces:**
- Consumes: `modelcatalog.Strip1mSuffix(string) string`
- Produces:
  - `accounts.ModelKeyCandidates(model string) []string`
  - `accounts.parseQuotaResetTime(string) (time.Time, bool)`
  - `accounts.quotaWindowOpen(ModelQuota, time.Time) (open bool, known bool)`
  - `accounts.modelQuotaFor(*Account, string) (ModelQuota, bool)` (free function, was a `*Manager` method)
  - `accounts.modelThresholdFor(*Account, string) (float64, bool)` (free function, was a `*Manager` method)

**Design note — why `isQuotaActiveLocked` is being replaced, not patched**

The shipped helper answers two different questions through one return value:

```go
if quota.ResetTime != "" {
    if parsed, err := time.Parse(time.RFC3339Nano, quota.ResetTime); err == nil {
        return manager.now().Before(parsed)   // LastChecked never read
    }
    ...
}
return quotaFresh(lastChecked, manager.now())
```

Whenever `ResetTime` parses, `LastChecked` is never consulted, so the field becomes authoritative in **both** directions. The PR intended only to widen trust for *exhaustion* ("exhaustion remains active past the default 5-minute `quotaFresh` cutoff"). It got two failures instead, mirror images of each other:

**1 — a future `ResetTime` grants trust that was not earned.** A quota fetched three hours ago reporting `RemainingFraction: 1.0` with `ResetTime: +4h` is treated as authoritative: `scoreLocked` skips the `*= .9` staleness penalty and the account scores a full 100, so the scheduler prefers phantom capacity and earns a 429. Confirmed against the real manager:

```
quotaFresh(LastChecked)=false   isQuotaActiveLocked=true
```

**1b — an elapsed `ResetTime` revokes trust that was earned.** The past branch returns `false` early rather than falling through to `quotaFresh`, so a quota read one second ago at 2% remaining, carrying an already-elapsed `resetTime`, stops being critical and the account gets selected. Also confirmed:

```
quotaFresh=true  quotaCriticalLocked=false   (2% remaining, read 1s ago)
```

The two share one cause and one fix. `quotaFresh` decides whether a reading may be trusted; `ResetTime` may only ever *extend* that trust for an exhausted record, never revoke it. The replacement, `quotaUsableLocked`, takes the threshold as an argument so it can ask whether the record is *exhausted* before granting the extension. `quotaCriticalLocked` must therefore compute its threshold **before** calling it — a reordering relative to the shipped code.

- [ ] **Step 1: Write the failing tests**

In `internal/accounts/manager_test.go`, add unit tests verifying:
1. `ModelKeyCandidates` generates deduplicated candidate keys in order `[raw, stripped, lower]`.
2. A stale record reporting spare capacity is **not** usable even when `ResetTime` is in the future (finding 1 — this is the regression test that must fail first).
3. A stale record reporting exhaustion **is** still usable until `ResetTime` (the behaviour the PR intended to add).
4. A **fresh** record reporting exhaustion stays critical even when `ResetTime` has elapsed (finding 1b — the mirror regression, also red before the fix).
5. `scoreLocked` does not penalize fresh quota when `ResetTime` is in the past.
6. `quotaCriticalLocked` returns `false` for a **stale** record whose `ResetTime` has passed.
7. `modelThresholdFor` resolves a `[1m]`-suffixed and a mixed-case model (finding 4 — the `ModelThreshold` path is currently untested).

```go
func TestModelKeyCandidates(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{
			input: "gemini-3.8-flash-high[1m]",
			want:  []string{"gemini-3.8-flash-high[1m]", "gemini-3.8-flash-high"},
		},
		{
			input: "Gemini-3.8-Flash-High",
			want:  []string{"Gemini-3.8-Flash-High", "gemini-3.8-flash-high"},
		},
		{
			input: "gemini-3.8-flash-high",
			want:  []string{"gemini-3.8-flash-high"},
		},
		{
			input: "Claude-Opus-4-6[1m]",
			want:  []string{"Claude-Opus-4-6[1m]", "Claude-Opus-4-6", "claude-opus-4-6"},
		},
	}

	for _, tc := range cases {
		got := ModelKeyCandidates(tc.input)
		if len(got) != len(tc.want) {
			t.Fatalf("ModelKeyCandidates(%q) len = %d, want %d (%v vs %v)", tc.input, len(got), len(tc.want), got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ModelKeyCandidates(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

// Finding 1. A three-hour-old record claiming full capacity must not escape the
// freshness guard just because its ResetTime has not arrived yet.
func TestManager_StaleFullQuota_IsNotUsable(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	full := 1.0

	acc := &Account{
		Email:   "stale-full@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &full,
					ResetTime:         clock.Add(4 * time.Hour).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-3 * time.Hour).UnixMilli()),
		},
	}

	manager, err := New(Options{Accounts: []*Account{acc}, Strategy: StrategyHybrid, Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	usable := manager.quotaUsableLocked(acc.Quota.Models["gemini-3.8-flash-high"], acc.Quota.LastChecked, 0.05)
	manager.mu.Unlock()

	if usable {
		t.Errorf("stale full-capacity quota reported usable; the 5 minute freshness guard was bypassed")
	}

	// And the staleness penalty must still land in scoring.
	manager.mu.Lock()
	score := manager.scoreLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()
	wQuota := mapFloat(manager.selectionConfig.Weights, "quota", 3)
	penalized := 1.0 * 100 * 0.9 * wQuota
	if score > penalized+1e-9 {
		t.Errorf("scoreLocked = %f, want <= %f (stale penalty not applied)", score, penalized)
	}
}

// The behaviour the PR did intend: exhaustion outlives the freshness window.
func TestManager_StaleExhaustedQuota_RemainsCritical(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	zero := 0.0

	acc := &Account{
		Email:   "stale-exhausted@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &zero,
					ResetTime:         clock.Add(30 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-3 * time.Hour).UnixMilli()),
		},
	}

	manager, err := New(Options{Accounts: []*Account{acc}, Strategy: StrategyHybrid, Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	critical := manager.quotaCriticalLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()

	if !critical {
		t.Errorf("exhausted quota with a future ResetTime should stay critical past the freshness window")
	}
}

func TestManager_QuotaResetTimeInPast_FallsBackToQuotaFresh(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	one := 1.0

	// Account checked 1 minute ago (fresh), but ResetTime was 10 minutes ago.
	acc := &Account{
		Email:   "past-reset@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &one,
					ResetTime:         clock.Add(-10 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-1 * time.Minute).UnixMilli()),
		},
	}

	manager, err := New(Options{
		Accounts: []*Account{acc},
		Strategy: StrategyHybrid,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Score should not have the 10% stale quota penalty applied.
	manager.mu.Lock()
	score := manager.scoreLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()
	// Expected quotaScore = 100 * wQuota (3) = 300. If stale penalty was applied, quotaScore = 90 * 3 = 270.
	wQuota := mapFloat(manager.selectionConfig.Weights, "quota", 3)
	expectedQuotaComponent := 1.0 * 100 * wQuota
	if score < expectedQuotaComponent {
		t.Errorf("scoreLocked = %f, expected at least quota component %f (penalty was incorrectly applied)", score, expectedQuotaComponent)
	}
}

func TestManager_QuotaCritical_ResetTimeInPast(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	zero := 0.0

	// Account exhausted at 11:50 with reset scheduled for 11:55. Now is 12:00.
	acc := &Account{
		Email:   "exhausted-past-reset@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &zero,
					ResetTime:         clock.Add(-5 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-10 * time.Minute).UnixMilli()),
		},
	}

	manager, err := New(Options{
		Accounts: []*Account{acc},
		Strategy: StrategyHybrid,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	critical := manager.quotaCriticalLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()
	if critical {
		t.Errorf("expected quotaCriticalLocked to return false when ResetTime is in the past")
	}
}

// Finding 1b. The mirror failure: an elapsed ResetTime must not revoke a
// reading that quotaFresh accepts. 2% remaining, measured one second ago.
func TestManager_FreshLowQuota_ElapsedResetTime_StaysCritical(t *testing.T) {
	clock := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	low := 0.02

	acc := &Account{
		Email:   "fresh-low@example.com",
		Enabled: true,
		Quota: Quota{
			Models: map[string]ModelQuota{
				"gemini-3.8-flash-high": {
					RemainingFraction: &low,
					ResetTime:         clock.Add(-1 * time.Minute).Format(time.RFC3339),
				},
			},
			LastChecked: float64(clock.Add(-1 * time.Second).UnixMilli()),
		},
	}

	manager, err := New(Options{Accounts: []*Account{acc}, Strategy: StrategyHybrid, Now: now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager.mu.Lock()
	critical := manager.quotaCriticalLocked(acc, "gemini-3.8-flash-high")
	manager.mu.Unlock()

	if !critical {
		t.Errorf("a one-second-old 2%%-remaining quota must stay critical; an elapsed ResetTime overrode the measured fraction")
	}
}

// Finding 4. The ModelThreshold normalization path shipped untested.func TestModelThresholdFor_NormalizesSuffixAndCase(t *testing.T) {
	acc := &Account{
		Email: "threshold@example.com",
		ModelThreshold: map[string]float64{
			"gemini-3.8-flash-high": 0.25,
		},
	}

	for _, query := range []string{
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-high[1m]",
		"Gemini-3.8-Flash-High",
		"Gemini-3.8-Flash-High[1m]",
	} {
		got, ok := modelThresholdFor(acc, query)
		if !ok || got != 0.25 {
			t.Errorf("modelThresholdFor(%q) = (%v, %v), want (0.25, true)", query, got, ok)
		}
	}

	if _, ok := modelThresholdFor(acc, "claude-opus-4-6"); ok {
		t.Errorf("modelThresholdFor should not resolve an unrelated model")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -v ./internal/accounts -run "TestModelKeyCandidates|TestManager_StaleFullQuota_IsNotUsable|TestManager_StaleExhaustedQuota_RemainsCritical|TestManager_QuotaResetTimeInPast|TestManager_QuotaCritical_ResetTimeInPast|TestModelThresholdFor"`

Expected: FAIL. `ModelKeyCandidates`, `quotaUsableLocked` and `modelThresholdFor` are undefined; once they compile, `TestManager_StaleFullQuota_IsNotUsable` is the finding-1 regression and must be red before the fix.

- [ ] **Step 3: Unify normalization and route `rateLimitModelKey` through it**

In `internal/accounts/manager.go`, `ModelKeyCandidates` becomes the single spelling of the normalization rule, and the pre-existing `rateLimitModelKey` is re-expressed in terms of it so the two cannot drift (finding 6):

```go
// ModelKeyCandidates returns the ordered candidate keys to search for quota or thresholds:
// 1. Exact model string
// 2. Model with [1m] suffix stripped
// 3. Lowercased stripped model — the canonical key, identical to rateLimitModelKey
// Duplicate entries and empty strings are omitted.
func ModelKeyCandidates(model string) []string {
	stripped := modelcatalog.Strip1mSuffix(model)
	lower := strings.ToLower(stripped)
	seen := make(map[string]struct{}, 3)
	candidates := make([]string, 0, 3)
	for _, k := range []string{model, stripped, lower} {
		if k == "" {
			continue
		}
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			candidates = append(candidates, k)
		}
	}
	return candidates
}

// rateLimitModelKey returns the canonical key: the last, most-normalized candidate.
func rateLimitModelKey(model string) string {
	candidates := ModelKeyCandidates(model)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[len(candidates)-1]
}
```

Verify before committing that the rewritten `rateLimitModelKey` is byte-for-byte equivalent to the original on the existing test corpus — the original applies `TrimSpace` via `Strip1mSuffix`, and `ModelKeyCandidates` must preserve that. Run `go test ./internal/accounts -run RateLimitModelKey -v` and keep every existing assertion green.

- [ ] **Step 4: Replace `isQuotaActiveLocked` with exhaustion-aware `quotaUsableLocked`**

Both new lookups drop the `*Manager` receiver they never used (finding 8), which also makes them directly unit-testable without constructing a `Manager`:

```go
func modelQuotaFor(account *Account, model string) (ModelQuota, bool) {
	if account == nil || account.Quota.Models == nil {
		return ModelQuota{}, false
	}
	for _, candidate := range ModelKeyCandidates(model) {
		if q, exists := account.Quota.Models[candidate]; exists {
			return q, true
		}
	}
	return ModelQuota{}, false
}

func modelThresholdFor(account *Account, model string) (float64, bool) {
	if account == nil || account.ModelThreshold == nil {
		return 0, false
	}
	for _, candidate := range ModelKeyCandidates(model) {
		if v, exists := account.ModelThreshold[candidate]; exists && v > 0 {
			return v, true
		}
	}
	return 0, false
}

func parseQuotaResetTime(resetTime string) (time.Time, bool) {
	if resetTime == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, resetTime); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// quotaWindowOpen reports whether the record's reset time is still ahead of now,
// and whether a reset time was known at all.
func quotaWindowOpen(quota ModelQuota, now time.Time) (open bool, known bool) {
	parsed, ok := parseQuotaResetTime(quota.ResetTime)
	if !ok {
		return false, false
	}
	return now.Before(parsed), true
}

// quotaUsableLocked reports whether this quota record may be trusted.
//
// An EXHAUSTED record stays authoritative until its window resets, even once it
// falls outside the quotaFresh window — that is the point of tracking ResetTime.
// Every other record, including one reporting spare capacity, must still be
// fresh: otherwise a hours-old "100% remaining" snapshot reads as live capacity
// and the scheduler routes traffic into a 429.
func (manager *Manager) quotaUsableLocked(quota ModelQuota, lastChecked any, threshold float64) bool {
	now := manager.now()
	if open, known := quotaWindowOpen(quota, now); known && open &&
		quota.RemainingFraction != nil && *quota.RemainingFraction <= threshold {
		return true
	}
	return quotaFresh(lastChecked, now)
}
```

Note the threshold is now computed **before** the usability check, reversing the order in the shipped code. Note also what is *absent*: there is no "`ResetTime` has passed, therefore not critical" early return. `ResetTime` may only ever *extend* trust in an exhausted record; it must never *revoke* a reading that `quotaFresh` accepts. An elapsed `ResetTime` alongside a one-second-old `RemainingFraction: 0.02` means the timestamp is stale relative to the fraction, not that the fraction is wrong — and the fraction is what was actually measured.

```go
func (manager *Manager) quotaCriticalLocked(account *Account, model string) bool {
	quota, exists := modelQuotaFor(account, model)
	if !exists || quota.RemainingFraction == nil {
		return false
	}

	threshold := 0.05
	if manager.globalQuotaThreshold > 0 {
		threshold = manager.globalQuotaThreshold
	} else if qCfg := manager.selectionConfig.Quota; qCfg != nil {
		threshold = mapFloat(qCfg, "criticalThreshold", 0.05)
	}
	if value, ok := modelThresholdFor(account, model); ok {
		threshold = value
	} else if account.QuotaThreshold != nil && *account.QuotaThreshold > 0 {
		threshold = *account.QuotaThreshold
	}

	if !manager.quotaUsableLocked(quota, account.Quota.LastChecked, threshold) {
		return false
	}
	return *quota.RemainingFraction <= threshold
}
```

`TestManager_QuotaCritical_ResetTimeInPast` still passes under this shape: its record is stale (`LastChecked` 10 minutes old), so `quotaFresh` rejects it and `quotaUsableLocked` returns `false` on its own. The early return was never load-bearing for that case — only harmful for the fresh one.

`scoreLocked` has no threshold in scope and only ever asks about staleness, so it calls `quotaFresh` directly rather than borrowing the exhaustion bypass:

```go
	if quota, exists := modelQuotaFor(account, model); exists && quota.RemainingFraction != nil {
		quotaScore = *quota.RemainingFraction * 100
		if !quotaFresh(account.Quota.LastChecked, manager.now()) {
			quotaScore *= .9
		}
	}
```

- [ ] **Step 5: Correct the now-false doc comment (finding 5, blocking)**

The comment above `rateLimitModelKey` at `internal/accounts/manager.go:419-422` currently reads:

> Only ModelRateLimits is normalized. Quota.Models and ModelThreshold [...] are still indexed by the raw argument and remain catalog-ID-only.

PR #85 is exactly the change that made that false. Replace it with a description of the unified scheme — one normalization rule (`ModelKeyCandidates`), applied to `ModelRateLimits`, `Quota.Models` and `ModelThreshold` alike, with `rateLimitModelKey` naming the canonical write key.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -v -race ./internal/accounts`

Expected: PASS, including the pre-existing `TestManager_QuotaCritical_Normalizes1mSuffix`.

- [ ] **Step 7: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/manager_test.go
git commit -m "fix(accounts): require freshness for non-exhausted quota and unify model key normalization"
```


---

### Task 2: Encapsulate Claude Code Rate Limits and Reset Timestamp Logic

**Closes findings:** 12 — the four-branch `resetTime` cascade is written verbatim twice in `handleAccountLimits` (`management.go:571-609`), and `{Limit, Remaining, Reset}` now travels as four parallel triples wanting one type with a `fraction()` method. Moving that arithmetic onto `RateLimits` absorbs both copies and is what lets Task 3's Claude Code branch shrink to two calls. Land it before Task 3.

**Files:**
- Modify: `internal/claudecode/ratelimit.go`
- Test: `internal/claudecode/ratelimit_test.go`

**Interfaces:**
- Consumes: `claudecode.RateLimits` fields
- Produces:
  - `(rl RateLimits) MinRemainingFraction() (float64, bool)`
  - `(rl RateLimits) ResetTime(now time.Time) time.Time`

- [ ] **Step 1: Write the failing tests**

In `internal/claudecode/ratelimit_test.go`, add unit tests:

```go
func TestRateLimits_MinRemainingFraction(t *testing.T) {
	// No limits set
	empty := RateLimits{}
	if frac, ok := empty.MinRemainingFraction(); ok || frac != 1.0 {
		t.Errorf("expected (1.0, false), got (%f, %v)", frac, ok)
	}

	// Requests limit lower
	rl1 := RateLimits{
		RequestsLimit:     100,
		RequestsRemaining: 20, // 0.2
		TokensLimit:       1000,
		TokensRemaining:   500, // 0.5
	}
	if frac, ok := rl1.MinRemainingFraction(); !ok || frac != 0.2 {
		t.Errorf("expected (0.2, true), got (%f, %v)", frac, ok)
	}

	// Granular input token limit lowest
	rl2 := RateLimits{
		RequestsLimit:        100,
		RequestsRemaining:    90, // 0.9
		InputTokensLimit:     1000,
		InputTokensRemaining: 50, // 0.05
		OutputTokensLimit:    500,
		OutputTokensRemaining: 400, // 0.8
	}
	if frac, ok := rl2.MinRemainingFraction(); !ok || frac != 0.05 {
		t.Errorf("expected (0.05, true), got (%f, %v)", frac, ok)
	}
}

func TestRateLimits_ResetTime(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	// Case 1: Active RetryAfter takes precedence
	rlRetry := RateLimits{
		RetryAfter:  30,
		LastUpdated: now,
		RequestsReset: now.Add(10 * time.Second),
	}
	expectedRetry := now.Add(30 * time.Second)
	if got := rlRetry.ResetTime(now); !got.Equal(expectedRetry) {
		t.Errorf("expected ResetTime %v from RetryAfter, got %v", expectedRetry, got)
	}

	// Case 2: Exhausted granular dimension (Remaining == 0) selected over non-exhausted dimension
	inputReset := now.Add(45 * time.Second)
	reqReset := now.Add(10 * time.Second)
	rlExhausted := RateLimits{
		RequestsLimit:        100,
		RequestsRemaining:    50,
		RequestsReset:        reqReset,
		InputTokensLimit:     1000,
		InputTokensRemaining: 0,
		InputTokensReset:     inputReset,
	}
	if got := rlExhausted.ResetTime(now); !got.Equal(inputReset) {
		t.Errorf("expected exhausted InputTokensReset %v, got %v", inputReset, got)
	}

	// Case 3: Multiple active dimensions - fallback chain selects latest active reset
	outReset := now.Add(25 * time.Second)
	rlActive := RateLimits{
		TokensLimit:       500,
		TokensRemaining:   200,
		TokensReset:       reqReset,
		OutputTokensLimit: 100,
		OutputTokensRemaining: 80,
		OutputTokensReset: outReset,
	}
	if got := rlActive.ResetTime(now); !got.Equal(outReset) {
		t.Errorf("expected latest active reset %v, got %v", outReset, got)
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -v ./internal/claudecode -run "TestRateLimits_MinRemainingFraction|TestRateLimits_ResetTime"`
Expected: FAIL due to undefined methods.

- [ ] **Step 3: Implement `MinRemainingFraction` and `ResetTime`**

In `internal/claudecode/ratelimit.go`:
```go
// MinRemainingFraction computes the minimum remaining fraction (0.0 to 1.0) across all configured
// rate limit dimensions (requests, unified tokens, input tokens, output tokens).
// Returns (1.0, false) if no limits are configured.
func (rl RateLimits) MinRemainingFraction() (float64, bool) {
	hasLimit := false
	minFrac := 1.0

	check := func(rem, lim int64) {
		if lim > 0 {
			hasLimit = true
			frac := float64(rem) / float64(lim)
			if frac < minFrac {
				minFrac = frac
			}
		}
	}

	check(rl.RequestsRemaining, rl.RequestsLimit)
	check(rl.TokensRemaining, rl.TokensLimit)
	check(rl.InputTokensRemaining, rl.InputTokensLimit)
	check(rl.OutputTokensRemaining, rl.OutputTokensLimit)

	if !hasLimit {
		return 1.0, false
	}
	if minFrac < 0 {
		minFrac = 0
	}
	if minFrac > 1.0 {
		minFrac = 1.0
	}
	return minFrac, true
}

// ResetTime determines the most relevant reset timestamp:
// 1. Active RetryAfter duration (relative to LastUpdated or now).
// 2. If any dimension is exhausted (Remaining == 0 with Limit > 0), the furthest reset time among exhausted dimensions.
// 3. Furthest reset time among all active dimensions with a future reset timestamp.
// 4. Fallback chain through configured reset timestamps.
func (rl RateLimits) ResetTime(now time.Time) time.Time {
	if rl.RetryAfter > 0 {
		ref := rl.LastUpdated
		if ref.IsZero() {
			ref = now
		}
		retryExpiry := ref.Add(time.Duration(rl.RetryAfter) * time.Second)
		if retryExpiry.After(now) {
			return retryExpiry
		}
	}

	// Check exhausted dimensions first
	var exhaustedReset time.Time
	checkExhausted := func(rem, lim int64, reset time.Time) {
		if lim > 0 && rem == 0 && reset.After(now) {
			if reset.After(exhaustedReset) {
				exhaustedReset = reset
			}
		}
	}
	checkExhausted(rl.RequestsRemaining, rl.RequestsLimit, rl.RequestsReset)
	checkExhausted(rl.TokensRemaining, rl.TokensLimit, rl.TokensReset)
	checkExhausted(rl.InputTokensRemaining, rl.InputTokensLimit, rl.InputTokensReset)
	checkExhausted(rl.OutputTokensRemaining, rl.OutputTokensLimit, rl.OutputTokensReset)

	if !exhaustedReset.IsZero() {
		return exhaustedReset
	}

	// Check any future reset timestamp among active dimensions
	var latestReset time.Time
	checkActive := func(lim int64, reset time.Time) {
		if lim > 0 && reset.After(now) {
			if reset.After(latestReset) {
				latestReset = reset
			}
		}
	}
	checkActive(rl.RequestsLimit, rl.RequestsReset)
	checkActive(rl.TokensLimit, rl.TokensReset)
	checkActive(rl.InputTokensLimit, rl.InputTokensReset)
	checkActive(rl.OutputTokensLimit, rl.OutputTokensReset)

	if !latestReset.IsZero() {
		return latestReset
	}

	// Fallback to any non-zero reset timestamp
	for _, t := range []time.Time{rl.InputTokensReset, rl.OutputTokensReset, rl.TokensReset, rl.RequestsReset} {
		if !t.IsZero() {
			return t
		}
	}

	return time.Time{}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v -race ./internal/claudecode -run "TestRateLimits_"`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/claudecode/ratelimit.go internal/claudecode/ratelimit_test.go
git commit -m "feat(claudecode): encapsulate MinRemainingFraction and ResetTime on RateLimits"
```

---

### Task 3: Refactor Management API Limits Handler

**Closes findings:** 2 (blocking), 3, 10, and the API half of 4.

**Files:**
- Modify: `internal/api/management.go:466-510, 543-625`
- Test: `internal/api/management_test.go`

**Interfaces:**
- Consumes: `accounts.ModelKeyCandidates`, `rl.MinRemainingFraction()`, `rl.ResetTime()`
- Produces: Normalized model key matching in `/account-limits`, plus a `rate_limited` status for Google Cloud Code accounts

**Design note — finding 2, why this blocks merge**

`rateLimits` is keyed by the keys of `acc.ModelRateLimits`, which `MarkRateLimited` writes through `rateLimitModelKey` — lowercased and `[1m]`-stripped. The lookup at `management.go:481` is an exact hit against `modelId`, which comes from `sortedModels`, built from allowlist IDs and `ExpandAliases()` output. ADR 0002 states that `ExpandAliases` "deduplicates case-insensitively **while preserving original casing**", and names `handleAccountLimits` as a site where individual alias resolution must work (`docs/adr/0002-claude-code-alias-dual-representation.md:14`).

So an account rate-limited on `gemini-3.8-flash-high` still displays stale catalog quota for an alias spelled `Gemini-3.8-Flash-High` or `gemini-3.8-flash-high[1m]`. This is the same normalization bug Task 1 fixes in the selection path, left unfixed in the display path one function away — which is why both must land together.

**Decision required before Step 3 — finding 3**

The Google branch computes only `ok` / `disabled` / `invalid`; there is no `rate_limited` case, even when every model in `ModelRateLimits` is actively 429'd. The Claude Code branch does set it (`management.go:551-552`). The WebUI therefore shows a green "ok" account whose models all read 0%.

Pick one and record the choice in the commit message:
- **(a) Close the asymmetry** — set `status = "rate_limited"` when `len(rateLimits) > 0` and it covers every allowlisted model for that account. This is the recommended option; it makes the two providers behave alike and matches what the WebUI status pill implies.
- **(b) Keep the asymmetry deliberately** — Google rate limits are per-model, not per-account, so an account with one exhausted model is still usable. If this is chosen, add a comment at the Google `status` block saying so, and leave the code unchanged.

- [ ] **Step 1: Write the failing tests**

In `internal/api/management_test.go`, add tests for:
1. Google Cloud Code active 429 matched when `modelId` has a `[1m]` suffix or case variance.
2. Claude Code rate limits using `RetryAfter` as `resetTime`.
3. Claude Code rate limits using `InputTokensReset` when input tokens are exhausted, asserting the exact timestamp (finding 4 — the shipped test checks `remaining` but never `resetTime`).
4. Google account `status`, per the decision above.

```go
func TestManagement_AccountLimits_GoogleActive429Normalizes1m(t *testing.T) {
	server, mgr, _ := newTestServerWithManager(t)
	handler := server.Handler()

	acc := mgr.GetAllAccounts()[0]
	// Mark active 429 under standard catalog ID
	mgr.MarkRateLimited(acc, "gemini-3.8-flash-high", 30*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	accountsList, ok := res["accounts"].([]any)
	if !ok || len(accountsList) == 0 {
		t.Fatalf("expected accounts in response")
	}
	firstAcc := accountsList[0].(map[string]any)
	limits := firstAcc["limits"].(map[string]any)

	// Both the standard ID and the [1m] variant must show 0% with an active resetTime.
	// Absence is a failure, not a skip — the shipped assertion silently passed
	// when the key was missing, which is exactly the bug under test.
	for _, m := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-high[1m]"} {
		l, exists := limits[m].(map[string]any)
		if !exists || l == nil {
			t.Errorf("model %q: expected a limits entry, got %v", m, limits[m])
			continue
		}
		if l["remaining"] != "0%" {
			t.Errorf("model %q: expected remaining 0%%, got %v", m, l["remaining"])
		}
		if l["resetTime"] == nil {
			t.Errorf("model %q: expected non-nil resetTime for active 429", m)
		}
	}
}

// Finding 4. The shipped granular-token test never asserted resetTime.
func TestManagement_AccountLimits_GranularResetTimeIsInputTokensReset(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	handler := server.Handler()

	nowTime := server.now()
	reset := nowTime.Add(90 * time.Second)

	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{ID: "cc-reset-1", Name: "CC Reset", Email: "cc-reset@example.com", Token: "sk-ant-test", Type: "oauth", Priority: 1, Enabled: true, Source: "oauth"},
	}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{{ID: "claude-3-7-sonnet-20250219"}}
	config.SetForTest(cfg)

	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	pool.UpdateAccountRateLimits("cc-reset-1", claudecode.RateLimits{
		InputTokensLimit:     1000,
		InputTokensRemaining: 0,
		InputTokensReset:     reset,
	})

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	want := reset.UTC().Format(time.RFC3339)
	for _, a := range res["accounts"].([]any) {
		am := a.(map[string]any)
		if am["email"] != "cc-reset@example.com" {
			continue
		}
		l := am["limits"].(map[string]any)["claude-3-7-sonnet-20250219"].(map[string]any)
		if l["resetTime"] != want {
			t.Errorf("resetTime = %v, want %v", l["resetTime"], want)
		}
		return
	}
	t.Fatalf("cc-reset@example.com not found")
}

func TestManagement_AccountLimits_ClaudeCodeRetryAfter(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	cfg, _ := config.Load()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{ID: "cc-1", Name: "CC Account", Enabled: true, Priority: 1},
	}
	_ = config.Save(cfg)

	nowTime := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return nowTime }

	pool := claudecode.NewPool(cfg.ClaudeCode, now)
	poolAcc, ok := pool.GetAccount("cc-1")
	if !ok {
		t.Fatalf("expected cc-1 in pool")
	}
	// Record rate limit with RetryAfter
	poolAcc.UpdateRateLimits(claudecode.RateLimits{
		RetryAfter:  45,
		LastUpdated: nowTime,
	})

	server, err := New(Options{
		APIKey:         "test-api-key",
		ClaudeCodePool: pool,
		Now:            now,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	accountsList := res["accounts"].([]any)
	var ccFound map[string]any
	for _, a := range accountsList {
		am := a.(map[string]any)
		if am["provider"] == "claudecode" {
			ccFound = am
			break
		}
	}
	if ccFound == nil {
		t.Fatalf("claudecode account not found")
	}
	limits := ccFound["limits"].(map[string]any)
	expectedReset := nowTime.Add(45 * time.Second).UTC().Format(time.RFC3339)
	for _, m := range []string{"claude-sonnet-4-6", "claude-opus-4-6"} {
		if l, ok := limits[m].(map[string]any); ok && l != nil {
			if l["resetTime"] != expectedReset {
				t.Errorf("model %q: expected resetTime %q, got %q", m, expectedReset, l["resetTime"])
			}
		}
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test -v ./internal/api -run "TestManagement_AccountLimits_GoogleActive429Normalizes1m|TestManagement_AccountLimits_GranularResetTimeIsInputTokensReset|TestManagement_AccountLimits_ClaudeCodeRetryAfter"`

Expected: FAIL — `gemini-3.8-flash-high[1m]` does not match the active 429, `resetTime` is unasserted or wrong, and/or `RetryAfter` is ignored.

- [ ] **Step 3: Implement the fixes in `internal/api/management.go`**

1. For the Google Cloud Code active-429 lookup and quota lookup. Note the reset timestamp is now computed once in the first loop from `rl.ResetTimeMS` directly, rather than packed into a `map[string]any` and type-asserted back out as an `int64` (finding 10). The old round-trip degraded to a silent `nil` if the literal's type ever changed; a `time.Time` cannot.

```go
		// 1. Google Cloud Code accounts
		for _, acc := range accountsList {
			rateLimits := make(map[string]any)
			// activeResets is keyed canonically so alias and [1m] spellings resolve.
			activeResets := make(map[string]time.Time)
			for model, rl := range acc.ModelRateLimits {
				if rl != nil && rl.IsRateLimited && rl.ResetTimeMS > now {
					rateLimits[model] = map[string]any{
						"isRateLimited": true,
						"waitMs":        rl.ResetTimeMS - now,
						"actualResetMs": rl.ActualResetMS,
					}
					activeResets[model] = time.UnixMilli(rl.ResetTimeMS).UTC()
				}
			}

			limits := make(map[string]any, len(sortedModels))
			for _, modelId := range sortedModels {
				candidates := accounts.ModelKeyCandidates(modelId)

				var matchedReset time.Time
				var matched bool
				for _, candidate := range candidates {
					if reset, ok := activeResets[candidate]; ok {
						matchedReset = reset
						matched = true
						break
					}
				}

				if matched {
					limits[modelId] = map[string]any{
						"remaining":         "0%",
						"remainingFraction": 0.0,
						"resetTime":         matchedReset.Format(time.RFC3339),
					}
					continue
				}

				var q accounts.ModelQuota
				var exists bool
				for _, candidate := range candidates {
					if mq, ok := acc.Quota.Models[candidate]; ok {
						q = mq
						exists = true
						break
					}
				}

				if !exists {
					limits[modelId] = nil
					continue
				}
				remStr := "N/A"
				if q.RemainingFraction != nil {
					remStr = fmt.Sprintf("%d%%", int(*q.RemainingFraction*100))
				}
				limits[modelId] = map[string]any{
					"remaining":         remStr,
					"remainingFraction": q.RemainingFraction,
					"resetTime":         q.ResetTime,
				}
			}
```

The `rateLimits` map itself is still emitted verbatim in the response payload (`management.go:533`), so its shape must not change — only the internal lookup moves to `activeResets`.

2. Google account `status`, per the decision recorded above. If option (a):

```go
		} else if len(activeResets) > 0 && allAllowlistedModelsRateLimited(sortedModels, activeResets) {
			status = "rate_limited"
		}
```

3. For Claude Code accounts limit calculation:

```go
			computedFrac, hasLimits := rl.MinRemainingFraction()
			computedReset := rl.ResetTime(server.now())

			for _, modelId := range sortedModels {
				if !isClaudeModel(modelId, cfg.ClaudeCode.Allowlist) {
					limits[modelId] = nil
					continue
				}

				var frac float64 = 1.0
				var resetTime any = nil

				if status == "disabled" {
					frac = 0.0
				} else if status == "cooldown" {
					frac = 0.0
					resetTime = ccAcc.CooldownUntil.UTC().Format(time.RFC3339)
				} else if status == "rate_limited" {
					frac = 0.0
					if !computedReset.IsZero() {
						resetTime = computedReset.UTC().Format(time.RFC3339)
					}
				} else if hasLimits {
					frac = computedFrac
					if !computedReset.IsZero() {
						resetTime = computedReset.UTC().Format(time.RFC3339)
					}
				}

				limits[modelId] = map[string]any{
					"remaining":         fmt.Sprintf("%d%%", int(frac*100)),
					"remainingFraction": frac,
					"resetTime":         resetTime,
				}
			}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v -race ./internal/api -run "TestManagement_AccountLimits_"`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/management.go internal/api/management_test.go
git commit -m "fix(api): resolve model aliases in account-limits and derive reset time from ResetTimeMS"
```


---

### Task 4: Alpine.js Reactive Quota Getter in WebUI

**Closes findings:** 9, and optionally 11.

**Files:**
- Modify: `internal/webui/public/views/accounts.html:245-265, 440-460`
- Modify (optional): `internal/webui/public/js/components/account-manager.js:404`

**Interfaces:**
- Consumes: `getMainModelQuota(acc)`
- Produces: `quota` reactive getter in Alpine component scope

- [ ] **Step 1: Inspect lines 245-265 and 440-460 in `accounts.html`**

Review both Google Cloud Code and Claude Code quota table cells. Currently, `getMainModelQuota(acc)` is called up to 6 times per cell in template expressions (`x-if`, `:class`, `:style`, `x-text`).

- [ ] **Step 2: Update quota cells to use `x-data="{ get quota() { return getMainModelQuota(acc); } }"`**

In `internal/webui/public/views/accounts.html`:

Lines 245-263 (Google Cloud Code accounts):
```html
                        <td class="py-4 cursor-pointer" @click="openQuotaModal(acc)">
                            <div x-data="{ get quota() { return getMainModelQuota(acc); } }">
                                <template x-if="quota.percent !== null">
                                    <div class="flex items-center gap-2">
                                        <div class="w-16 bg-gray-700 rounded-full h-2 overflow-hidden">
                                            <div class="h-full rounded-full transition-all"
                                                 :class="{
                                                     'bg-emerald-500': quota.percent > 50,
                                                     'bg-yellow-500': quota.percent > 20 && quota.percent <= 50,
                                                     'bg-red-500': quota.percent <= 20
                                                 }"
                                                 :style="`width: ${quota.percent}%`">
                                            </div>
                                        </div>
                                        <span class="text-xs font-mono text-gray-400" x-text="quota.percent + '%'"></span>
                                    </div>
                                </template>
                                <template x-if="quota.percent === null">
                                    <span class="text-xs text-gray-600">-</span>
                                </template>
                                <div x-show="acc.quotaThreshold !== undefined"
                                     class="text-[10px] font-mono text-amber-400/80 mt-0.5"
                                     x-text="'min: ' + getEffectiveThreshold(acc)"></div>
                            </div>
                        </td>
```

Lines 440-460 (Claude Code accounts):
```html
                            <!-- Quota -->
                            <td class="py-4 cursor-pointer" @click="openQuotaModal(acc)">
                                <div x-data="{ get quota() { return getMainModelQuota(acc); } }">
                                    <template x-if="quota.percent !== null">
                                        <div class="flex items-center gap-2">
                                            <div class="w-16 bg-gray-700 rounded-full h-2 overflow-hidden">
                                                <div class="h-full rounded-full transition-all"
                                                     :class="{
                                                         'bg-purple-400': quota.percent > 50,
                                                         'bg-yellow-500': quota.percent > 20 && quota.percent <= 50,
                                                         'bg-red-500': quota.percent <= 20
                                                     }"
                                                     :style="`width: ${quota.percent}%`">
                                                </div>
                                            </div>
                                            <span class="text-xs font-mono text-gray-400" x-text="quota.percent + '%'"></span>
                                        </div>
                                    </template>
                                    <template x-if="quota.percent === null">
                                        <span class="text-xs text-gray-600">-</span>
                                    </template>
                                </div>
                            </td>
```

- [ ] **Step 3: (Optional, finding 11) Simplify the null-or-undefined guard**

In `internal/webui/public/js/components/account-manager.js:404`:

```js
if (l.remainingFraction != null) return l.remainingFraction;
```

`!= null` is precisely the null-or-undefined check the two explicit comparisons spell out. Behaviour is identical; skip this step if the explicit form is preferred for readability.

- [ ] **Step 4: Verify HTML syntax and build**

Run: `go build ./...`
Expected: PASS

Then load `/accounts` in a browser with at least one account whose quota is refreshed by the store poll, and confirm the bar re-renders without a page reload. This is the behaviour the original `x-data` snapshot broke, and no Go test covers it.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/public/views/accounts.html internal/webui/public/js/components/account-manager.js
git commit -m "perf(webui): use Alpine reactive getter for account quota cells"
```

---

### Task 5: Full Suite Verification

**Files:**
- Test: All repository tests

- [ ] **Step 1: Run complete test suite with race detector**

Run: `go test -race ./...`
Expected: PASS with 0 race warnings.

- [ ] **Step 2: Run `go vet`**

Run: `go vet ./...`
Expected: Clean output.

- [ ] **Step 3: Verify git status**

Run: `git status`
Expected: Working tree clean (all changes committed in tasks 1-4).

- [ ] **Step 4: Confirm the blocking findings are actually closed**

Run the three regression tests by name and read the output rather than the exit code:

```bash
go test -v ./internal/accounts -run "TestManager_StaleFullQuota_IsNotUsable|TestManager_StaleExhaustedQuota_RemainsCritical"
go test -v ./internal/api -run "TestManagement_AccountLimits_GoogleActive429Normalizes1m"
git diff main -- internal/accounts/manager.go | grep -A4 'rateLimitModelKey'
```

Expected: the first two commands PASS, and the comment above `rateLimitModelKey` no longer claims `Quota.Models` and `ModelThreshold` are un-normalized.

- [ ] **Step 5: Push and report**

Push to the PR branch and reply on the review comment thread with the verification output — the test names and their PASS lines, not a summary claim.

---

## Findings coverage

Maps every finding in the PR #85 review to the task that closes it. Severity as posted: 🔴 blocking, 🟡 risk, 🔵 nit.

| # | Severity | Finding | Task | Closed by |
|---|---|---|---|---|
| 1 | 🔴 | Stale full-capacity quota bypasses the freshness guard (`manager.go` `isQuotaActiveLocked`) | 1 | `quotaUsableLocked` gates the `ResetTime` extension on exhaustion; `scoreLocked` calls `quotaFresh` directly |
| 1b | 🔴 | Elapsed `ResetTime` revokes a fresh exhausted reading, so a 2%-remaining account is selected | 1 | Same fix — no "reset has passed" early return; `quotaFresh` alone decides trust |
| 2 | 🔴 | `/account-limits` active-429 lookup misses `[1m]` and mixed-case aliases (`management.go:481`, ADR 0002) | 3 | `activeResets` keyed canonically, looked up through `ModelKeyCandidates` |
| 5 | 🔴 | `manager.go:419-422` comment asserts the opposite of the code | 1 | Step 5 rewrites the comment for the unified scheme |
| 3 | 🟡 | Google account `status` never becomes `rate_limited` | 3 | Decision recorded before Step 3; option (a) recommended |
| 6 | 🟡 | Normalization cascade written three times | 1 | `ModelKeyCandidates` is the single spelling; `rateLimitModelKey` delegates to it |
| 7 | 🟡 | `isQuotaActiveLocked` is a Mysterious Name; `lastChecked any` | 1 | Replaced by `quotaWindowOpen` + `quotaUsableLocked`, each answering one question |
| 12 | 🟡 | The four-branch `resetTime` cascade appears verbatim twice in one function; `{Limit, Remaining, Reset}` travels as four parallel triples (`management.go:571-609`) | 2 | `MinRemainingFraction()` / `ResetTime()` on `RateLimits` absorb both copies |
| 4 | 🔵 | Test coverage thinner than the PR's claims | 1, 3 | `TestModelThresholdFor…`, `TestManager_StaleFullQuota…`, `TestManager_FreshLowQuota…`, `TestManagement_…GranularResetTimeIsInputTokensReset`, direct `quotaCriticalLocked`/`scoreLocked` assertions |
| 8 | 🟡 | Helpers are `*Manager` methods that never use the receiver | 1 | `modelQuotaFor` / `modelThresholdFor` become free functions |
| 9 | 🔵 | 12 `getMainModelQuota(acc)` calls per row | 4 | Alpine `get quota()` getter |
| 10 | 🔵 | `waitMs` round-trips through `map[string]any` | 3 | Reset time computed once from `rl.ResetTimeMS` as a `time.Time` |
| 11 | 🔵 | `account-manager.js:404` verbose null check | 4 | Optional Step 3 |

**Undeclared scope, accepted as-is.** Two changes in PR #85 go beyond its four stated claims. Neither is wrong, and neither is being reverted — but both should be named in the PR description so the next reader is not surprised:

- `internal/claudecode/types.go:184` and `management.go:551` delegate to `RateLimits.IsRateLimited(now)`, which also honours `RetryAfter` (`ratelimit.go:99`). The inline check it replaced did not. A `RetryAfter`-only account now reports `rate_limited` where it previously reported `ok`. This is an improvement; it is simply broader than "granular `input_tokens` and `output_tokens` … alongside unified token limits."
- `account-manager.js:403` adds an `undefined` guard; the PR's WebUI claim covers only the `x-data` removal.

**Deliberately left open:** the two near-identical quota-bar blocks in `accounts.html` (lines 245-265 and 440-460) differ only in `bg-emerald-500` vs `bg-purple-400`. Extracting one shared `x-template` was raised in finding 9 but is pre-existing duplication, not introduced by PR #85. Track it separately rather than growing this remediation.
