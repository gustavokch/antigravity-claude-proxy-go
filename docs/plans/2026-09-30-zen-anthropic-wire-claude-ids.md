# Plan: Add current Claude Code–only model ids to `AnthropicWireIDs`

## Goal

Make `internal/zen/client.go` `AnthropicWireIDs` contain every current
**Claude Code–only** model id that the live OpenCode Zen catalog serves, so a
Zen allowlist entry for such a model claims `POST /v1/messages` instead of
falling through to a backend that mishandles Anthropic tool definitions
(confirmed root cause of the oh-my-pi tool-definition failures).

**Set definition (operator policy):** *anthropic models should only be
accessible through the Claude Code gateway, except for `claude-sonnet-4-6` and
`claude-opus-4-6` that route to antigravity/Cloud Code.* The Claude
Code–only set is therefore all live-catalog `claude-*` ids; the two exceptions
are not part of the set.

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
  wire list. The live config orders `kimi → claudecode → zen → openrouter →
  cloudcode → custom`, so the Claude Code gateway claims Claude Code–only ids
  first, and `claude-sonnet-4-6` / `claude-opus-4-6` (absent from the
  claudecode allowlist, never in the zen allowlist) fall through to
  `cloudcode` = antigravity/Cloud Code. Task 5 verifies this with config
  evidence.
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
- B. `IsAnthropicWire("claude-sonnet-5-5") == true`, pinned by
  `TestIsAnthropicWire`; `claude-opus-5-5` also pinned (it was in the code
  list but unpinned).
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
- `claude-sonnet-4-6` and `claude-opus-4-6` are never re-added if a future
  diff flags them; they are not Claude Code–only (see Non-goals 2).
- No new fixtures, no network inside `go test` — live-catalog checks are
  shell evidence steps, not unit tests.
- No new abstractions: this is a static-list edit plus its pins.

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
  add every missing `claude-*` id except `claude-sonnet-4-6` /
  `claude-opus-4-6` (policy exceptions), and record the drift in the commit
  message. If an exception ever appears as missing, do **not** re-add it.
- If `listed but not live` is non-empty, stop and report — a dead list entry
  is a separate defect, not part of this change.
- Baseline gate: `go build ./... && go vet ./... && go test ./...` must be
  green before any edit.

## Task 1 — Red: pin the new ids in `TestIsAnthropicWire`

**File:** `internal/zen/zen_test.go` (test at line 22).

Replace the `allowed` slice:

```go
	allowed := []string{
		"claude-fable-5-1", "claude-fable-5", "claude-opus-5",
		"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5",
		"claude-sonnet-5", "claude-sonnet-4-6", "claude-sonnet-4-5", "claude-sonnet-4",
		"claude-haiku-4-5",
		"qwen3.8-flash", "qwen3.6-plus", "qwen3.5-plus",
		"opencode/claude-sonnet-4-6",
		"OPencode/Claude-Sonnet-4-6",
		"Claude-Opus-4-5",
	}
```

with:

```go
	allowed := []string{
		"claude-fable-5-1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5",
		"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5",
		"claude-sonnet-5-5", "claude-sonnet-5", "claude-sonnet-4-6",
		"claude-sonnet-4-5", "claude-sonnet-4",
		"claude-haiku-4-5",
		"qwen3.8-flash", "qwen3.6-plus", "qwen3.5-plus",
		"opencode/claude-sonnet-4-6",
		"OPencode/Claude-Sonnet-4-6",
		"Claude-Opus-4-5",
	}
```

(`claude-opus-5-5` was already in the code list but unpinned — contract
completeness. Case/prefix normalization stays pinned by the existing
`opencode/`/mixed-case entries; no new variants needed.)

Run:

```bash
go test ./internal/zen/ -run TestIsAnthropicWire -v
```

**Expected RED** — the failure message contains:

```
IsAnthropicWire("claude-sonnet-5-5") = false, want true
```

(`claude-opus-5-5` passes already; only `claude-sonnet-5-5` fails. The
`denied` slice is unchanged — it must still reject `qwen3.7-max`,
`claude-sonnet-4-6-free`, etc.)

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

**Expected:** `gofmt -l` prints nothing; vet/build/test all green. Any
failing test outside `internal/zen` means a fixture assumed the old list
contents — inspect before touching code, report if the blast radius exceeds
`internal/zen` (expected: none; no other test pins list membership).

## Task 5 — Verify the routing policy with config evidence (Spec D)

```bash
python3 - <<'PY'
import json, os, pathlib
base = pathlib.Path(os.environ.get("ANTIGRAVITY_CONFIG_DIR")
                    or os.environ.get("CONFIG_DIR")
                    or pathlib.Path.home() / ".config" / "antigravity-proxy")
cfg = json.loads((base / "config.json").read_text())
order = cfg["gatewayOrder"]["order"]
cc = [e["id"] for e in cfg.get("claudecode", {}).get("allowlist", [])]
zen = [e["id"] for e in cfg.get("zen", {}).get("allowlist", [])]
zen_claude = [m for m in zen if m.startswith("claude-")]
print("order:", order)
print("claudecode allowlist:", cc)
print("zen claude ids:", zen_claude)
assert cfg["claudecode"]["enabled"], "claudecode gateway disabled"
assert order.index("claudecode") < order.index("zen"), "claudecode must precede zen"
assert "claude-sonnet-5-5" in cc, "sonnet-5-5 must be claimed by the Claude Code gateway"
assert "claude-sonnet-4-6" not in cc and "claude-opus-4-6" not in cc, \
    "policy exceptions must not be claimed by the Claude Code gateway"
assert not zen_claude, "zen must not carry claude allowlist entries"
print("OK: anthropic ids claim at claudecode; sonnet-4-6/opus-4-6 fall "
      "through kimi->claudecode->zen->openrouter to cloudcode (antigravity)")
PY
```

**Expected:** every assertion passes; final `OK:` line printed.

Chain this proves: `claude-sonnet-4-6` / `claude-opus-4-6` decline kimi,
decline claudecode (not allowlisted), decline zen (not allowlisted — and
this plan keeps them out of the Claude Code–only set), decline openrouter,
and land on `cloudcode`, the antigravity/Cloud Code terminal path. The
Claude Code–only ids claim at `claudecode` first (`gatewayOrder` position 2),
so `AnthropicWireIDs` membership matters when claudecode is disabled, drops
an id, or an operator allowlists it in zen as a fallback — the case the
fall-through bug broke.

If any assertion fails, stop: the config no longer expresses the policy —
report the mismatch instead of changing code.

## Task 6 — Commit

```bash
gofmt -l internal/zen && git add internal/zen/client.go internal/zen/zen_test.go README.md
git commit -m "fix(zen): add claude-sonnet-5-5 to AnthropicWireIDs"
```

---

## Non-goals

1. **`ChatWireIDs` stale omissions** (`qwen3.8-max`,
   `longcat-2.5-preview-free`, `deepseek-v4-flash-free`) — chat-wire models,
   not Claude Code ids; none is in the current zen allowlist. Separate
   change.
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
   catalogue** (only `claude-sonnet-5-5`, `claude-opus-5-5`,
   `claude-fable-5-1` are allowlisted today) — operator/WebUI action
   (Settings → Claude Code → Discover Models), config not code. When
   importing, exclude `claude-sonnet-4-6` and `claude-opus-4-6` so they keep
   routing to antigravity/Cloud Code.
4. **Stale comment on `matchZenModelEntry`** (`internal/api/server.go:2057-2059`):
   says "the Anthropic-wire subset" but the gate is `zen.IsForwardable`, which
   also admits Chat-Completions ids — pre-existing, unrelated to this add.
