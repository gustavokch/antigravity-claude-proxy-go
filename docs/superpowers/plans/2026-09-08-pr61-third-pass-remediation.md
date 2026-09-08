# PR #61 Third Pass Review Remediation — OpenRouter tool-capability routing

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/61
**Branch:** `feat/openrouter-tool-capability-routing`
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/61#issuecomment-5589347383

## Goal

Resolve the three third-pass findings without changing the feature contract: a tool-carrying
request must never be pinned to an endpoint OpenRouter rejects with `404 No endpoints found`,
and a request with no tool requirements routes exactly as before this PR.

## Architecture

Two seams:

1. `FilterCapable`'s all-incapable fallback — in `custom` mode it must walk the operator's
   `order.Order`, not the rank table, so the allowlist's precedence survives the substitution.
2. The rank-refresh call site in `server.go` — endpoints and their fill timestamp must be read
   in one lock acquisition so a concurrent cache refill cannot pair old endpoints with a new
   timestamp.

## Tech stack

Go 1.x, standard library only. Tests in `internal/openrouter` and `internal/api`.

## Verification gate

`gofmt -l` clean on touched files, `go vet ./...`, `go test ./... -race` all green.

---

## Task 1 — custom-mode fallback preserves operator precedence

**Finding:** `internal/openrouter/capability.go:L186` — the fallback loop iterates `ranks`
(rank order). In `custom` mode the fallback set is bounded by `order.Order`, but the sequence
comes from rank order: when the fallback fires (e.g. rank refresh between `SelectChain` and the
filter), the operator's configured precedence is lost.

- **Modify:** `internal/openrouter/capability.go`
- **Test:** `internal/openrouter/capability_test.go`
- **Consumes:** `ranks`, `order.Order`, `isCapable`, `providerHealthyUnderThresholdLocked`
- **Produces:** fallback sequence = operator order when `order.Mode == "custom"`

### Step 1 — failing test

```go
func TestProviderRouter_FilterCapableCustomFallbackKeepsOperatorOrder(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", []ProviderEndpoint{
		// Rank order: novita outranks parasail.
		{ProviderName: "gmicloud", ContextLength: 1000000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens"}},
		{ProviderName: "novita", ContextLength: 900000, UptimeLast5m: 0.98, UptimeLast30m: 0.98, UptimeLast1d: 0.98,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{Auto: true, Required: true}},
		{ProviderName: "parasail", ContextLength: 800000, UptimeLast5m: 0.97, UptimeLast30m: 0.97, UptimeLast1d: 0.97,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{None: true, Auto: true, Required: true, Function: true}},
	})

	// Operator allowlist puts parasail first, opposite of rank order. The only
	// candidate is incapable, so the fallback substitutes — it must hand back
	// the allowlist sequence, not the rank sequence.
	order := ProviderOrder{Mode: "custom", Order: []string{"parasail", "novita"}}
	got := r.FilterCapable("m1", []string{"gmicloud"},
		ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, order)
	want := []string{"parasail", "novita"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback must follow operator order, got %v, want %v", got, want)
		}
	}
}
```

### Step 2 — confirm failure

```
go test ./internal/openrouter -run FilterCapableCustomFallbackKeepsOperatorOrder -v
```

### Step 3 — implementation

In `FilterCapable`, extract the fallback body into a provider-name walk and choose the source
sequence by mode: `order.Order` when `order.Mode == "custom"`, else `ranks`. The allowlist
membership check becomes redundant in custom mode (walking the allowlist is the filter) and
stays for nothing else — drop `allowed` and gate only on `isCapable` + health.

### Step 4 — confirm pass

```
go test ./internal/openrouter -run FilterCapable -v
```

### Step 5 — commit

```
git add internal/openrouter/capability.go internal/openrouter/capability_test.go
git commit -m "fix(openrouter): custom-mode capability fallback keeps operator order"
```

---

## Task 2 — read endpoints and fill time atomically

**Finding:** `internal/api/server.go:L1197` — `GetCachedEndpoints` and `CachedEndpointsAt` each
take the cache lock separately. A refill between the two calls pairs endpoints from one fetch
with the timestamp of another; the staleness verdict can be wrong in either direction.

- **Modify:** `internal/openrouter/endpoints.go`, `internal/api/server.go`
- **Test:** `internal/openrouter/capability_test.go`

### Step 1 — failing test / guard

```go
func TestEndpointsClient_GetCachedEndpointsWithTime(t *testing.T) {
	c := NewEndpointsClient(time.Second, time.Hour)
	if _, _, ok := c.GetCachedEndpointsWithTime("author/model", "https://openrouter.ai/api"); ok {
		t.Fatal("empty cache must report no entry")
	}
	before := time.Now()
	c.SaveEndpoints("author/model", "https://openrouter.ai/api", toolCapableEndpoints())
	eps, at, ok := c.GetCachedEndpointsWithTime("author/model", "https://openrouter.ai/api")
	if !ok || len(eps) != len(toolCapableEndpoints()) || at.Before(before) {
		t.Fatalf("entry must return endpoints and fill time together, ok=%v at=%v", ok, at)
	}
	c.SaveEndpoints("author/model", "https://openrouter.ai/api", nil)
	if _, _, ok := c.GetCachedEndpointsWithTime("author/model", "https://openrouter.ai/api"); ok {
		t.Fatal("empty endpoint list must report no entry")
	}
}
```

Existing `CachedEndpointsAt`/`GetCachedEndpoints` tests guard the TTL and empty-entry semantics
the new accessor must share (same return-false conditions as `GetCachedEndpoints`).

### Step 2 — confirm failure

```
go test ./internal/openrouter -run GetCachedEndpointsWithTime -v
```

### Step 3 — implementation

Add `GetCachedEndpointsWithTime(modelID, baseURL string) ([]ProviderEndpoint, time.Time, bool)`
to `EndpointsClient`: one `RLock`, same miss conditions as `GetCachedEndpoints`, returns the
copy, `entry.cachedAt`, ok. Re-express `GetCachedEndpoints` and `CachedEndpointsAt` on top of
the locked body so the semantics stay in one place. In `server.go`, replace the two call sites
with the single accessor.

### Step 4 — confirm pass

```
go test ./internal/openrouter ./internal/api -v
```

### Step 5 — commit

```
git add internal/openrouter/endpoints.go internal/api/server.go internal/openrouter/capability_test.go
git commit -m "refactor(openrouter): fetch cached endpoints and fill time atomically"
```

---

## Final gate

```
gofmt -l internal/api/server.go internal/openrouter/capability.go internal/openrouter/endpoints.go internal/openrouter/capability_test.go
go vet ./...
go test ./... -race
git push origin feat/openrouter-tool-capability-routing
```
