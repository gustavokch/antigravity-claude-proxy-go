# PR #54 Remediation: max_tokens policy edge cases

## Goal

Fix three review findings on PR #54 (`maxtokens-derive-from-model-limits`):

1. Floor applied after limit clamp can send a value above a known limit.
2. `applyMaxTokensPolicy` mutates the caller's request map.
3. ClaudeCode and Kimi max_tokens policy paths have no test coverage.

## Architecture

`internal/api/server.go` — `applyMaxTokensPolicy` (L1027) is the single policy
chokepoint for the OpenRouter, Kimi, and ClaudeCode forwarding paths.

## Tech Stack

Go 1.27, stdlib `httptest`, table-driven tests.

---

## Task 1: Cap the floor by a known limit

**Files:** Modify `internal/api/server.go`, Test `internal/api/server_test.go`

**Problem:** L1054-1060 — client 4, limit 8: value stays 4, then floor raises
to 16, above the limit. The provider floor exists because providers reject
tiny values; an admin-set limit below the floor must still win.

**Step 1 — failing test** (`server_test.go`):

```go
func TestApplyMaxTokensPolicy_FloorNeverExceedsKnownLimit(t *testing.T) {
	body := []byte(`{"max_tokens":4}`)
	req := map[string]any{"max_tokens": float64(4)}
	out := applyMaxTokensPolicy(body, req, 8, 0)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["max_tokens"] != float64(8) {
		t.Errorf("expected clamp to limit 8, got %v", got["max_tokens"])
	}
}
```

**Step 2:** `go test ./internal/api -run TestApplyMaxTokensPolicy_FloorNeverExceedsKnownLimit -v` → fails (got 16).

**Step 3 — implementation:** effective floor = `min(minMaxTokensFloor, limit)` when `limit > 0`:

```go
floor := minMaxTokensFloor
if limit > 0 && limit < floor {
	floor = limit
}
```

Apply `floor` in both branches.

**Step 4:** test passes; full `./internal/api` suite green.

**Step 5:** `git commit -m "fix(api): cap max_tokens floor at known model limit"`

---

## Task 2: Stop mutating the caller's request map

**Files:** Modify `internal/api/server.go`, Test `internal/api/server_test.go`

**Problem:** `req["max_tokens"] = value` at L1041/L1064 mutates
`anthropicRequest`. On marshal failure the map and the returned body diverge,
and later readers of the map see the policy-applied value.

**Step 1 — failing test**:

```go
func TestApplyMaxTokensPolicy_DoesNotMutateCallerMap(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	req := map[string]any{"model": "m"}
	_ = applyMaxTokensPolicy(body, req, 1024, 0)
	if _, present := req["max_tokens"]; present {
		t.Errorf("caller map mutated: %v", req)
	}
}
```

**Step 2:** run → fails (map mutated).

**Step 3 — implementation:** clone before mutation:

```go
next := maps.Clone(req)
next["max_tokens"] = value
out, err := json.Marshal(next)
```

(`maps` stdlib, Go ≥1.21.) Restructure so both branches write into `next`
and marshal once.

**Step 4:** test passes; full suite green.

**Step 5:** `git commit -m "fix(api): clone request map in max_tokens policy"`

---

## Task 3: Cover ClaudeCode and Kimi policy paths

**Files:** Test `internal/api/claudecode_proxy_test.go`, `internal/api/kimi_proxy_test.go`

**Step 1 — failing/locking tests:**

- ClaudeCode: POST /v1/messages without max_tokens for a default-allowlist
  model → upstream body carries the entry's MaxOutputTokens.
- ClaudeCode: client max_tokens above entry limit → clamped down.
- Kimi: no client max_tokens, entry MaxOutputTokens 0 → upstream omits
  max_tokens.

**Step 2:** run; ClaudeCode injection test fails if behavior absent, others
lock current behavior.

**Step 3:** minimal implementation only if a test fails (expected: none —
coverage only).

**Step 4:** full suite green.

**Step 5:** `git commit -m "test(api): cover claudecode and kimi max_tokens policy paths"`

---

## Verification

`go build ./... && go vet ./... && go test ./...` — 100% green, then push.
