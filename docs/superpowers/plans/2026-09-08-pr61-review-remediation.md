# PR #61 Review Remediation — OpenRouter tool-capability routing

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/61
**Branch:** `feat/openrouter-tool-capability-routing`
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/61#issuecomment-5588757757

## Goal

Resolve the seven findings raised in review of the tool-capability provider filter, without
changing the feature's contract: a tool-carrying request must never be pinned to an endpoint
that OpenRouter will reject with `404 No endpoints found`, and a request with no tool
requirements must route exactly as it did before this PR.

## Architecture

`forwardToOpenRouter` builds a failover chain with `ProviderRouter.SelectChain`, then narrows it
with `ProviderRouter.FilterCapable` when the request body carries tools. Capability data lives on
`ProviderEndpoint` (`supported_parameters`, `supports_tool_choice`) and reaches the router through
the rank table refreshed from the endpoints cache. The remediation touches three seams:

1. Provider-name aggregation inside `FilterCapable` (rank entries are per endpoint variant, the
   routing key is the provider name).
2. Mode awareness in the all-incapable fallback (`custom` mode carries a user allowlist).
3. Rank freshness at the call site in `server.go` (ranks must track the endpoints cache).

## Tech stack

Go 1.x, standard library only. Tests are table-driven `go test` in
`internal/openrouter` and `internal/api`.

## Verification gate

`gofmt -l` clean on touched files, `go vet ./...`, `go test ./...` all green.

---

## Task 1 — OR-merge capability across a provider's endpoint variants

**Finding:** `internal/openrouter/capability.go:L142` — `known[providerName] = endpoint` is
last-write-wins. OpenRouter returns one rank entry per (provider, variant/tag). A provider whose
top-ranked variant serves tools is dropped when a lesser variant does not, and the outcome depends
on rank order. `provider.order` names only the provider, and OpenRouter picks a capable variant
within it, so the correct predicate is "any variant satisfies the requirements".

- **Modify:** `internal/openrouter/capability.go`
- **Test:** `internal/openrouter/capability_test.go`
- **Consumes:** `[]ranked` rank entries, `ToolRequirements`
- **Produces:** `capable(provider) bool` that ORs over all endpoints of that provider

### Step 1 — failing test

```go
func TestProviderRouter_FilterCapableMergesProviderVariants(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", []ProviderEndpoint{
		// Same provider, two variants: the higher-scored one serves tools,
		// the lower-scored one does not. The provider must survive.
		{ProviderName: "deepinfra", Tag: "fp8", ContextLength: 900000,
			UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &ToolChoiceSupport{Auto: true, Required: true}},
		{ProviderName: "deepinfra", Tag: "bf16", ContextLength: 100000,
			UptimeLast5m: 0.90, UptimeLast30m: 0.90, UptimeLast1d: 0.90,
			SupportedParameters: []string{"max_tokens"}},
	})

	got := r.FilterCapable("m1", []string{"deepinfra"},
		ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, ProviderOrder{Mode: "auto"})
	if len(got) != 1 || got[0] != "deepinfra" {
		t.Errorf("a provider with one tool-capable variant must survive, got %v", got)
	}
}
```

### Step 2 — confirm failure

```
go test ./internal/openrouter -run FilterCapableMergesProviderVariants -v
```

### Step 3 — implementation

Replace the `known map[string]ProviderEndpoint` with `known map[string][]ProviderEndpoint`
appending every rank entry, and make `capable` return true when any endpoint of the provider
satisfies `need` (unknown provider still passes).

### Step 4 — confirm pass

```
go test ./internal/openrouter -run FilterCapable -v
```

### Step 5 — commit

```
git add internal/openrouter/capability.go internal/openrouter/capability_test.go
git commit -m "fix(openrouter): OR capability across a provider's endpoint variants"
```

---

## Task 2 — respect `custom` mode's allowlist in the fallback

**Finding:** `internal/openrouter/capability.go:L163` — when every candidate is incapable the
fallback substitutes every ranked capable provider regardless of routing mode. In `custom` mode
that discards the operator's configured allowlist and can route to a provider they deliberately
excluded. `pinned` and `auto` keep the open substitution (that is the bug this PR fixes).

- **Modify:** `internal/openrouter/capability.go`, `internal/api/server.go`
- **Test:** `internal/openrouter/capability_test.go`
- **Consumes:** `ProviderOrder` (new fourth parameter on `FilterCapable`)
- **Produces:** fallback restricted to `order.Order` when `order.Mode == "custom"`

### Step 1 — failing test

```go
func TestProviderRouter_FilterCapableCustomModeKeepsAllowlist(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	r.RefreshRanks("m1", toolCapableEndpoints())

	order := ProviderOrder{Mode: "custom", Order: []string{"gmicloud", "novita"}}
	chain := r.SelectChain("s1", "m1", order)

	got := r.FilterCapable("m1", chain, ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}, order)
	for _, p := range got {
		if p == "parasail" {
			t.Fatalf("custom mode must not route outside its allowlist, got %v", got)
		}
	}
}
```

### Step 2 — confirm failure

```
go test ./internal/openrouter -run FilterCapableCustomModeKeepsAllowlist -v
```

### Step 3 — implementation

Add `order ProviderOrder` to `FilterCapable`. In the fallback loop skip any provider absent from
`order.Order` when `order.Mode == "custom"`. Update the call site in `server.go` and the existing
tests to the new signature.

### Step 4 — confirm pass

```
go test ./internal/openrouter ./internal/api -run 'FilterCapable|OpenRouterRouting' -v
```

### Step 5 — commit

```
git add internal/openrouter/capability.go internal/openrouter/capability_test.go internal/api/server.go
git commit -m "fix(openrouter): keep custom-mode allowlist in capability fallback"
```

---

## Task 3 — read-lock the capability filter

**Finding:** `internal/openrouter/capability.go:L133` — `FilterCapable` is read-only but takes the
write half of `sync.RWMutex`, serializing every tool-carrying request against `SelectChain` and
`RecordResult`. `providerHealthyUnderThresholdLocked` only reads `ranks`, `stats`, and `cfg`.

- **Modify:** `internal/openrouter/capability.go`
- **Test:** `internal/openrouter/capability_test.go` (concurrency test under `-race`)

### Step 1 — failing test

A `-race` test that runs `FilterCapable` concurrently with `GetRanks` and `RecordResult`; it
passes before and after, and exists to prove the lock downgrade is safe rather than to fail first.
Documented as a regression guard, not a red-green step.

### Step 2 — run

```
go test ./internal/openrouter -race -run FilterCapableConcurrent -v
```

### Step 3 — implementation

`r.mu.Lock()` → `r.mu.RLock()`, `defer r.mu.Unlock()` → `defer r.mu.RUnlock()`.

### Step 4 — confirm pass

```
go test ./internal/openrouter -race -v
```

### Step 5 — commit

```
git add internal/openrouter/capability.go internal/openrouter/capability_test.go
git commit -m "perf(openrouter): read-lock the tool-capability filter"
```

---

## Task 4 — refresh ranks when the endpoints cache is newer

**Finding:** `internal/api/server.go:L1197` — ranks refresh only when `len(ranks) == 0`, so a later
endpoints re-fetch carrying new or changed `supports_tool_choice` never reaches the rank table.
Capability decisions can run on the first payload for the process lifetime.

- **Modify:** `internal/openrouter/router.go`, `internal/openrouter/endpoints.go`,
  `internal/api/server.go`
- **Test:** `internal/openrouter/capability_test.go`
- **Consumes:** `EndpointsClient` cache timestamp, `ProviderRouter.rankedAt`
- **Produces:** `RankedAt(model) time.Time`, `CachedEndpointsAt(model, baseURL) (time.Time, bool)`

### Step 1 — failing test

```go
func TestProviderRouter_RankedAtTracksRefresh(t *testing.T) {
	r := NewProviderRouter(DefaultRoutingConfig())
	if !r.RankedAt("m1").IsZero() {
		t.Fatal("unranked model must report a zero rank time")
	}
	r.RefreshRanks("m1", toolCapableEndpoints())
	if r.RankedAt("m1").IsZero() {
		t.Fatal("RefreshRanks must record the rank time")
	}
}
```

### Step 2 — confirm failure

```
go test ./internal/openrouter -run RankedAtTracksRefresh -v
```

### Step 3 — implementation

Add `RankedAt` to the router and `CachedEndpointsAt` to the endpoints client. In `server.go`
refresh when ranks are empty **or** the cache entry is newer than `rankedAt`.

### Step 4 — confirm pass

```
go test ./internal/openrouter ./internal/api -v
```

### Step 5 — commit

```
git add internal/openrouter/router.go internal/openrouter/endpoints.go internal/api/server.go internal/openrouter/capability_test.go
git commit -m "fix(openrouter): re-rank when the endpoints cache is newer than the ranks"
```

---

## Task 5 — call-site and truncation cleanups

**Findings:**
- `internal/api/server.go:L1215` — `sameProviderChain` only gates the log; the assignment is
  unconditional in effect.
- `internal/api/server.go:L2024` — `truncate` cuts bytes, not runes; at 2048 an upstream body can
  split mid-rune and put invalid UTF-8 in the client-facing error.
- `internal/openrouter/capability.go:L98` — the empty-`supported_parameters` fail-open is untested.

- **Modify:** `internal/api/server.go`
- **Test:** `internal/api/server_test.go` (or the nearest existing helper test file),
  `internal/openrouter/capability_test.go`

### Step 1 — failing test

```go
func TestTruncateCutsOnRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 20 bytes
	got := truncate(s, 5)        // byte 5 lands mid-rune
	if !utf8.ValidString(strings.TrimSuffix(got, "…")) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
}
```

```go
func TestProviderEndpoint_NoAdvertisedParametersFailsOpen(t *testing.T) {
	ep := ProviderEndpoint{ProviderName: "silent"}
	if !ep.SupportsRequirements(ToolRequirements{Tools: true, ToolChoice: ToolChoiceRequired}) {
		t.Error("an endpoint advertising no parameters must fail open")
	}
}
```

### Step 2 — confirm failure

```
go test ./internal/api -run TruncateCutsOnRuneBoundary -v
```

### Step 3 — implementation

Make `truncate` back off to the previous rune boundary. Assign `candidates = filtered`
unconditionally and keep the comparison inside the log guard.

### Step 4 — confirm pass

```
go test ./internal/api ./internal/openrouter -v
```

### Step 5 — commit

```
git add internal/api/server.go internal/openrouter/capability_test.go
git commit -m "refactor(openrouter): tidy capability call site and rune-safe truncation"
```

---

## Final gate

```
gofmt -l internal/api/server.go internal/openrouter/capability.go internal/openrouter/router.go internal/openrouter/endpoints.go
go vet ./...
go test ./... -race
git push origin feat/openrouter-tool-capability-routing
```
