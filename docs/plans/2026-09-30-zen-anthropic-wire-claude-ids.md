# Plan: Add current Claude Code–only model ids to `AnthropicWireIDs`

## Goal

Make `internal/zen/client.go` `AnthropicWireIDs` contain every current
**Claude Code–only** model id that the live OpenCode Zen catalog serves, so a
Zen allowlist entry for such a model claims `POST /v1/messages` instead of
falling through to a backend that mishandles Anthropic tool definitions
(suspected cause of the oh-my-pi tool-definition failures; not established
by the evidence in this PR — see Task 5).

**Set definition (operator policy):** *anthropic models should only be
accessible through the Claude Code gateway, except for `claude-sonnet-4-6` and
`claude-opus-4-6` that route to antigravity/Cloud Code.* That is a routing
policy. The wire list is a capability gate, so it contains every live
`claude-*` id, the two exceptions included; routing keeps them off Zen.

**Net change (verified against the live catalog 2026-09-30):** add
`claude-sonnet-5-5`. It is the only live `claude-*` id missing from the list.

## Architecture

- `AnthropicWireIDs` / `ChatWireIDs` (`internal/zen/client.go:36-85`) are
  static wire-capability lists. `init()` folds them into `wireSet`;
  `IsForwardable(id)` / `WireFor(id)` read that set.
- The wire gate lives in `matchZenModelEntry` (`internal/api/server.go:2062`):
  a non-forwardable allowlist entry never claims the route, so the request
  falls through the configured `gatewayOrder` to Claude Code / OpenRouter /
  CloudCode. A stale list therefore fails closed — that is exactly the bug
  class being fixed.
- Routing itself is decided by `gatewayOrder` + each gateway's allowlist
  (`dispatchAlternateBackend`, `internal/api/dispatch.go:49`), **not** by the
  wire list. The walk stops at `cloudcode`. Claude Code–only ids are
  claimed by the claudecode gateway when it precedes `zen`;
  `claude-sonnet-4-6` / `claude-opus-4-6` (absent from the claudecode
  allowlist, never in the zen allowlist) fall through to `cloudcode` =
  antigravity/Cloud Code. Task 5 probes the real router for the actual
  order and matches.
- Discovery (`internal/api/discovery.go:45,329`) and the WebUI zen-allowlist
  validation filter on `zen.IsForwardable`, so the new id also becomes
  addable/advertised there.

## Tech Stack

Go 1.27rc2, stdlib `testing`, `gofmt`/`go vet`; `python3` for the read-only
evidence scripts (live catalog diff, config assertions).

## Spec

Sources, in precedence order:

1. Operator requirements (this session):
   - "Write a plan to add all current Claude Code only model ids to
     `internal/zen/client.go` `AnthropicWireIDs`."
   - "anthropic models should only be accessible through the Claude Code
     gateway, except for sonnet-4-6 and opus-4-6 that should route to
     antigravity/Cloud Code."
2. `docs/plans/2026-09-21-opencode-zen-gateway-plan.md` — original Zen plan;
   its 15-id list is historical and now stale (do not edit).
3. `internal/zen/client.go:32-35` doc comment — the list is maintained
   against the docs endpoint table **with the live catalog as authoritative**
   (precedent: `qwen3.7-max`/`qwen3.7-plus` are excluded because they are
   absent from the *live* catalog, not because the docs table omits them).
4. `README.md` Zen section (lines ~530-534) — documents the same list and
   must be updated with it.

Acceptance criteria:

- A. `AnthropicWireIDs` contains every live-catalog `claude-*` id; the
  Claude Code–only requirement is met for the current catalog
  (expected live set: `claude-fable-5`, `claude-fable-5-1`, `claude-opus-5-5`,
  `claude-opus-5`, `claude-opus-4-8`, `claude-opus-4-7`, `claude-opus-4-6`,
  `claude-opus-4-5`, `claude-sonnet-5-5`, `claude-sonnet-5`,
  `claude-sonnet-4-6`, `claude-sonnet-4-5`, `claude-sonnet-4`,
  `claude-haiku-4-5`).
- B. `IsAnthropicWire("claude-sonnet-5-5") == true`, pinned by name in
  `TestIsAnthropicWire` and by the snapshot drift tests (which cover
  `claude-opus-5-5` and every other live `claude-*` id).
- C. No id is added that is absent from the live Zen catalog.
- D. The routing policy statement holds in the operator config and is
  verified with evidence (Task 5).
- E. `go test ./...` green, `gofmt -l` empty, `go build ./...` clean.

## Global Constraints

- Never touch TLS/fingerprinting code (AGENTS.md one rule) — no changes to
  `internal/zen/tls.go`, `passthrough.go`, or anything under `.reference/`.
- `gofmt`-clean before commit (repo hook refuses dirty Go files;
  bypass exists but must not be needed).
- Only ids present in the live Zen catalog may enter either wire list.
- `claude-sonnet-4-6` and `claude-opus-4-6` stay in `AnthropicWireIDs`: they
  speak the Anthropic wire. The policy that keeps them off Zen is routing
  (see Non-goals 2), not list membership.
- No network inside default `go test ./...`. The live-catalog check is
  build-tagged (`zen_live`) and run on demand; the snapshot under
  `internal/zen/testdata/` is the offline reference.
- No new abstractions beyond the one `wireDrift` test helper.

---

## Task 0 — Evidence: live-catalog diff (read-only, no code changes)

Run from the repo root:

```bash
python3 - <<'PY'
import json, re, subprocess
# curl, not urllib: the Zen catalog 403s the stock python UA.
cat = json.loads(subprocess.run(
    ["curl", "-sSL", "https://opencode.ai/zen/v1/models"],
    capture_output=True, text=True, check=True).stdout)
live = sorted(m["id"] for m in cat["data"] if m["id"].startswith("claude-"))
src = open("internal/zen/client.go").read()
block = src.split("AnthropicWireIDs = []string{", 1)[1].split("}", 1)[0]
listed = sorted(re.findall(r'"([^"]+)"', block))
listed_claude = [i for i in listed if i.startswith("claude-")]
print("live claude ids:", len(live))
print("missing (must add):", [i for i in live if i not in listed_claude])
print("listed claude but not live (must be empty):",
      [i for i in listed_claude if i not in live])
PY
```

**Expected as of 2026-09-30** (verified):

```
live claude ids: 14
missing (must add): ['claude-sonnet-5-5']
listed claude but not live (must be empty): []
```

The `claude-` filter is applied to both sides: the list also holds three
`qwen*` Anthropic-wire ids, which the claude-only diff must ignore. The
qwen side of the wire lists is a separate check (Non-goal 1).

- If `missing` contains ids beyond `claude-sonnet-5-5`, the catalog drifted:
  add every missing `claude-*` id — including `claude-sonnet-4-6` /
  `claude-opus-4-6` if they ever appear — because the list is a capability
  gate and `TestAnthropicWireIDsCoverLiveClaudeModels` requires it. Keeping
  those two off Zen is routing (allowlists), not list membership. Refresh the
  snapshot and record the drift in the commit message.
- If `listed but not live` is non-empty, stop and report — a dead list entry
  is a separate defect, not part of this change.
- Baseline gate: `go build ./... && go vet ./... && go test ./...` must be
  green before any edit.

## Task 1 — Red: drift tests against a catalog snapshot

**Files:** `internal/zen/testdata/catalog-2026-09-30.json` (snapshot of the
live catalog ids), `internal/zen/catalog_drift_test.go`,
`internal/zen/zen_test.go`.

A hand-copied `allowed` slice in `TestIsAnthropicWire` can only assert the
copy equals itself, so it cannot see a missing id. The shipped tests diff
`AnthropicWireIDs` against the snapshot instead:

- `TestAnthropicWireIDsCoverLiveClaudeModels` — every snapshot `claude-*`
  id must satisfy `IsAnthropicWire` (this includes `claude-sonnet-4-6` /
  `claude-opus-4-6`).
- `TestAnthropicWireIDsHaveNoStaleEntries` — every `AnthropicWireIDs` entry
  must be in the snapshot.
- Both call the shared `wireDrift` helper, which the `zen_live`-tagged
  `TestAnthropicWireIDsLiveCatalog` also uses against today's catalog via
  `Client.FetchModels`.
- `TestIsAnthropicWire` keeps only the case/`opencode/`-prefix normalization
  pins, near-miss denials, and `claude-sonnet-5-5` pinned by name.

Run with the id removed from `client.go`:

```bash
go test ./internal/zen/ -run 'TestAnthropicWireIDs|TestIsAnthropicWire' -v
```

**Expected RED** — `live claude-* ids not in AnthropicWireIDs:
[claude-sonnet-5-5]` and `IsAnthropicWire("claude-sonnet-5-5") = false,
want true`.

## Task 2 — Green: insert the id into `AnthropicWireIDs`

**File:** `internal/zen/client.go` (list at lines 36-53).

Replace:

```go
	"claude-opus-4-6",
	"claude-opus-4-5",
	"claude-sonnet-5",
```

with:

```go
	"claude-opus-4-6",
	"claude-opus-4-5",
	"claude-sonnet-5-5",
	"claude-sonnet-5",
```

Placement keeps the existing family/generation ordering: opus block, then
sonnet block newest-first (`5-5` before `5`).

Run:

```bash
go test ./internal/zen/ -run 'TestIsAnthropicWire|TestWireFor' -v
```

**Expected:** both tests PASS (`TestWireFor`'s `claude-sonnet-4-6 →
WireAnthropic` case is untouched by this change).

## Task 3 — Docs: README list

**File:** `README.md` lines 530-534.

Replace:

````
`claude-fable-5-1`, `claude-fable-5`, `claude-opus-5-5`, `claude-opus-5`,
`claude-opus-4-8`, `claude-opus-4-7`, `claude-opus-4-6`, `claude-opus-4-5`,
`claude-sonnet-5`, `claude-sonnet-4-6`, `claude-sonnet-4-5`,
`claude-sonnet-4`, `claude-haiku-4-5`, `qwen3.8-flash`, `qwen3.6-plus`,
`qwen3.5-plus`.
````

with:

````
`claude-fable-5-1`, `claude-fable-5`, `claude-opus-5-5`, `claude-opus-5`,
`claude-opus-4-8`, `claude-opus-4-7`, `claude-opus-4-6`, `claude-opus-4-5`,
`claude-sonnet-5-5`, `claude-sonnet-5`, `claude-sonnet-4-6`,
`claude-sonnet-4-5`, `claude-sonnet-4`, `claude-haiku-4-5`,
`qwen3.8-flash`, `qwen3.6-plus`, `qwen3.5-plus`.
````

Nothing else in the README changes: the routing-order bullet and the
`claude-sonnet-4-6` example config remain accurate (the wire list is a
capability gate; routing stays gateway-order + allowlists).

## Task 4 — Full-suite gate

```bash
gofmt -l internal/zen
go vet ./internal/zen/
go build ./...
go test ./...
```

**Expected:** `gofmt -l` prints nothing; vet/build/test all green, including
`TestAnthropicWireIDsCoverLiveClaudeModels` and
`TestAnthropicWireIDsHaveNoStaleEntries`. A failing test outside
`internal/zen` means a fixture assumed the old list contents — inspect before
touching code. On-demand, network-gated:
`go test -tags zen_live ./internal/zen/ -run TestAnthropicWireIDsLiveCatalog -v`.

## Task 5 — Verify the routing policy against the real router (Spec D)

Routing is decided by `matchClaudeCodeModel`, not by id equality against an
allowlist: `Router.ResolveModel` tries exact id, alias, dot/hyphen
normalization, then longest prefix (a prefix hit that continues the version
number, e.g. `claude-sonnet-5-5` against `claude-sonnet-5`, is rejected), and
an empty configured allowlist falls back to `DefaultAllowlist()`.
Re-implementing that in a script drifts from the code, so probe the real
function with the real loaded config (prints no secrets):

```bash
cat > internal/api/zz_probe_test.go <<'GO'
package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/config"
)

func TestZZProbe(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("../zen/testdata/catalog-2026-09-30.json")
	var snap struct{ IDs []string `json:"ids"` }
	_ = json.Unmarshal(raw, &snap)
	t.Logf("claudecode.enabled=%v order=%v", cfg.ClaudeCode.Enabled, cfg.GatewayOrder.Effective(""))
	for _, id := range snap.IDs {
		if strings.HasPrefix(id, "claude-") {
			t.Logf("%-20s -> %q", id, matchClaudeCodeModel(cfg.ClaudeCode, id))
		}
	}
}
GO
go test ./internal/api/ -run TestZZProbe -v | grep -v '^=== '
rm -f internal/api/zz_probe_test.go
```

**Expected shape** (the ids claimed depend on the operator's live
`claudecode.allowlist`, which changes; re-run rather than trust a table):

- `claude-sonnet-4-6` and `claude-opus-4-6` resolve to `""` — they decline
  claudecode, are not in the zen allowlist, and reach `cloudcode`, the
  antigravity/Cloud Code terminal path (`dispatchAlternateBackend` returns
  at `cloudcode`, `internal/api/dispatch.go:51`, so gateways ordered after
  it are never consulted).
- `claude-sonnet-5-5` resolves to itself only when claudecode is enabled and
  allowlists that exact id (or an alias of it). With the default allowlist
  it resolves to `""`: claudecode declines and the request proceeds down the
  order — `zen` if the operator allowlists the id there (this is where the
  new `AnthropicWireIDs` entry decides whether `zen` can claim it), then
  `cloudcode`. It never resolves to `claude-sonnet-5`. Claudecode precedes
  `zen` in the order, so the Zen wire entry is **not** on the path while
  claudecode allowlists the id.

> The added entry is a **capability gate**: it makes the id forwardable over
> the Anthropic wire the moment it becomes reachable through `zen` — the
> case that previously fell through to a gateway that could not speak the
> wire. This PR does not establish that the oh-my-pi tool-definition
> failures were routed through Zen.

If the probe shows `claude-sonnet-4-6` / `claude-opus-4-6` claimed by
claudecode, stop: the config no longer expresses the policy — report the
mismatch instead of changing code.

## Task 6 — Commit

Shipped as separate commits on the PR branch: the list fix and README
(`fix(zen): add claude-sonnet-5-5 to AnthropicWireIDs`), this plan, the
catalog snapshot, the drift tests (`catalog_drift_test.go`,
`catalog_live_test.go`, `zen_test.go`), and the plan corrections. Run
`gofmt -l internal/zen` before each.

---

## Non-goals

1. **`ChatWireIDs` stale omissions** (`qwen3.8-max`,
   `longcat-2.5-preview-free`, `deepseek-v4-flash-free`) — chat-wire models,
   not Claude Code ids; none is in the current zen allowlist. Tracked in
   #107.
2. **Removing `claude-sonnet-4-6` / `claude-opus-4-6` from
   `AnthropicWireIDs`** — not done. The list is a *capability* gate (both ids
   genuinely speak Anthropic wire in the Zen catalog); routing is decided by
   `gatewayOrder` + allowlists, where the config already sends them to
   antigravity (verified in Task 5). Making them non-claimable would require
   migrating every Zen fixture that uses `claude-sonnet-4-6` as the
   claimable model — `internal/api/dispatch_test.go:21`,
   `zen_proxy_test.go`, `zen_config_test.go`, `zen_cachebump_test.go`,
   `zen_harness_headers_test.go`, plus the `TestWireFor` case — and would
   change README's documented "explicit Zen allowlist entry wins on ID
   collision" behavior. If the policy should become a hard gate, request that
   as a follow-up plan.
3. **Expanding `claudecode.allowlist` to the full current Claude Code
   catalogue** — operator/WebUI action (Settings → Claude Code → Discover
   Models), config not code. The effective list is the configured
   allowlist, or `claudecode.DefaultAllowlist()`
   (`internal/claudecode/router.go:11-122`) when it is empty. The defaults
   carry `claude-fable-5`, `claude-fable-5-1`, `claude-opus-5`,
   `claude-sonnet-5`, `claude-haiku-4-5-20251001`, and the `claude-3-*`
   family — no `claude-opus-4-*` / `claude-sonnet-4-*`, and no
   `claude-sonnet-5-5` / `claude-opus-5-5`, so under the defaults claudecode
   **declines** those two. Matching is not id equality: `Router.ResolveModel`
   also matches aliases and, last, longest prefix. That prefix step used to
   rewrite `claude-sonnet-5-5` to `claude-sonnet-5` (and `claude-opus-5-5`,
   `claude-sonnet-5.5`, `opus-4-6`, …) because nothing stopped a prefix hit
   from continuing the version number; `tryClaudeCodeGateway` then sent the
   rewritten `body.model` upstream, silently serving a different model. It is
   fixed in this PR (`continuesVersion`, `router.go`): a prefix hit whose
   tail is a digit, or `-`/`.` plus one to three digits, does not match;
   build stamps (`-20260101`), `[1m]`, and other suffixes still do. Setting an
   explicit allowlist replaces the defaults wholesale, so the operator must
   re-add every id they still want — and exclude `claude-sonnet-4-6` and
   `claude-opus-4-6` so those keep routing to antigravity/Cloud Code.
4. **Stale comment on `matchZenModelEntry`** (`internal/api/server.go:2057-2059`):
   says "the Anthropic-wire subset" but the gate is `zen.IsForwardable`, which
   also admits Chat-Completions ids — pre-existing, unrelated to this add.
