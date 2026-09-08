# PR #61 Second Pass Review Remediation — OpenRouter tool-capability routing

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/61
**Branch:** `feat/openrouter-tool-capability-routing`
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/61#issuecomment-5589033809

## Goal

Resolve the four second-pass review findings in the tool-capability routing and failover chain:
1. Ensure multi-variant providers stay healthy in `SelectChain` and `FilterCapable` when at least one variant is healthy.
2. Eliminate hot-path heap allocations from `GetRanks(model)` in `forwardToOpenRouter`.
3. Precompute provider capability in `FilterCapable` during rank grouping.
4. Add test coverage for multi-variant provider health and fallback survival.

## Architecture

1. `providerHealthyUnderThresholdLocked` aggregates endpoint health per provider name: `hasHealthy` must be true (at least one endpoint variant healthy) and the provider must exist in `ranks`.
2. `forwardToOpenRouter` uses `rankedAt.IsZero() || stale` to determine if ranks need refresh without allocating `[]RankedProvider`.
3. `FilterCapable` builds `capableProviders map[string]bool` in a single pass over `ranks`.

## Tech stack

Go 1.x, standard library only. Tests in `internal/openrouter` and `internal/api`.

## Verification gate

`gofmt -l` clean on touched files, `go vet ./...`, `go test ./...` and `go test -race ./...` all green.

---

## Task 1 — Multi-variant provider health aggregation in `providerHealthyUnderThresholdLocked`

**Finding:** `internal/openrouter/router.go:L487` — `providerHealthyUnderThresholdLocked` rejects whole provider when any endpoint variant is unhealthy. Provider with healthy primary variant and degraded secondary variant gets dropped from routing.

- **Modify:** `internal/openrouter/router.go`
- **Test:** `internal/openrouter/capability_test.go`
- **Consumes:** `r.ranks[model]` slice of `ranked` endpoints
- **Produces:** Provider health evaluation that requires `found && hasHealthy`

### Step 1 — failing test

```go
func TestProviderRouter_SelectChainKeepsProviderWithHealthyVariant(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", []ProviderEndpoint{
		{ProviderName: "deepinfra", Tag: "fp8", ContextLength: 900000,
			UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{Auto: true, Required: true}},
		{ProviderName: "deepinfra", Tag: "broken", ContextLength: 100000,
			Status: 500, UptimeLast5m: 0, UptimeLast30m: 0, UptimeLast1d: 0},
	})

	chain := r.SelectChain("s1", "m1", ProviderOrder{Mode: "auto"})
	if len(chain) == 0 || chain[0] != "deepinfra" {
		t.Fatalf("provider with healthy primary variant must be selected in chain, got %v", chain)
	}
}
```

### Step 2 — confirm failure

```
go test ./internal/openrouter -run SelectChainKeepsProviderWithHealthyVariant -v
```

### Step 3 — implementation

Update `providerHealthyUnderThresholdLocked` in `internal/openrouter/router.go`:
```go
	if ranks, ok := r.ranks[model]; ok {
		found := false
		hasHealthy := false
		for _, rk := range ranks {
			if rk.endpoint.ProviderName == provider {
				found = true
				if rk.endpoint.Healthy() {
					hasHealthy = true
					break
				}
			}
		}
		if !found || !hasHealthy {
			return false
		}
	}
```

### Step 4 — confirm pass

```
go test ./internal/openrouter -run SelectChainKeepsProviderWithHealthyVariant -v
```

### Step 5 — commit

```bash
git add internal/openrouter/router.go internal/openrouter/capability_test.go
git commit -m "fix(openrouter): consider provider healthy if any endpoint variant is healthy"
```

---

## Task 2 — Eliminate hot-path `GetRanks` allocation in `forwardToOpenRouter`

**Finding:** `internal/api/server.go:L1206` — `GetRanks(model)` allocates and copies `[]RankedProvider` on every request.

- **Modify:** `internal/api/server.go`
- **Test:** `internal/api/openrouter_capability_test.go`
- **Consumes:** `rankedAt` and `cachedAt`
- **Produces:** Zero-allocation freshness check

### Step 1 — failing test / check

Verify existing routing tests pass and test cold/stale refresh.

### Step 2 — implementation

In `internal/api/server.go`:
```go
		rankedAt := openrouter.DefaultRouter.RankedAt(model)
		cachedAt, haveCachedAt := openrouter.DefaultEndpointsClient.CachedEndpointsAt(model, baseURL)
		stale := haveCachedAt && rankedAt.Before(cachedAt)
		if rankedAt.IsZero() || stale {
			openrouter.DefaultRouter.RefreshRanks(model, endpoints)
		}
```

### Step 3 — confirm pass

```
go test ./internal/api -run TestOpenRouterRouting -v
```

### Step 4 — commit

```bash
git add internal/api/server.go
git commit -m "perf(openrouter): avoid GetRanks slice allocation on request path"
```

---

## Task 3 — Precompute provider capability in `FilterCapable`

**Finding:** `internal/openrouter/capability.go:L151` — `FilterCapable` loops through variant slices repeatedly in closure.

- **Modify:** `internal/openrouter/capability.go`
- **Test:** `internal/openrouter/capability_test.go`
- **Consumes:** `ranks` and `need`
- **Produces:** `capable map[string]bool` in single pass

### Step 1 — implementation

In `internal/openrouter/capability.go`:
```go
	capable := make(map[string]bool, len(ranks))
	for _, rk := range ranks {
		name := rk.endpoint.ProviderName
		if !capable[name] && rk.endpoint.SupportsRequirements(need) {
			capable[name] = true
		}
	}
	isCapable := func(p string) bool {
		if c, ok := capable[p]; ok {
			return c
		}
		return true // unknown provider: no basis to exclude it
	}
```

### Step 2 — confirm pass

```
go test ./internal/openrouter -run FilterCapable -v
```

### Step 3 — commit

```bash
git add internal/openrouter/capability.go
git commit -m "perf(openrouter): precompute provider capability in single pass"
```
