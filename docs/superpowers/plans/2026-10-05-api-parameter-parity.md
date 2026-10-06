# API Parameter Parity (Effort, Thinking, Output Limits) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Verify every gap (G1–G14) and recommendation (R1–R11) in the 2026-10-05 research report against primary sources and the code, then close each one: fix what is real, document what is by design, and drop what the evidence shows was wrong.

**Architecture:** One new dependency-free package, `internal/reasoning`, parses every reasoning signal a request can carry (`reasoning_effort`, `reasoning`, `thinking.*`, `thinking_budget`, `output_config.effort`) into a single `Params` value. The model catalog (tier routing) and the Cloud Code converter (budget emission) both read it, so they can no longer disagree. Wire-shape questions that only the upstream can answer are settled by a live probe CLI and an agy capture *before* the code that depends on them is written; each such change sits behind a recorded decision gate.

**Tech Stack:** Go 1.27 (`internal/reasoning` new; `internal/modelcatalog`, `internal/format`, `internal/api`, `internal/claudecode`, `internal/accounts`), a live probe CLI (`cmd/paramprobe`, new), a mitmproxy addon (`scripts/mitm_header_dump.py`, Python 3), Markdown docs.

**Spec:** `docs/research/2026-10-05-api-parameters-reasoning-effort-matrix.md` — the report this plan verifies. Its claims were produced by a subagent and were unverified when this plan was written; §0 below is the verdict on each.

## Global Constraints

- **TLS is off limits.** `tls.Config{}` stays empty; nothing in this plan touches `internal/cloudcode` transport (AGENTS.md, "The one rule that matters most").
- **Never invent proto fields.** The only thinking fields on the wire are `GenerationConfig.ThinkingConfig.include_thoughts` (1), `thinking_budget` (3), `thinking_level` (4) and enum `ThinkingLevel {UNSPECIFIED 0, LOW 1, MEDIUM 2, HIGH 3, MINIMAL 4}` (`gen/google/cloud/aiplatform/master/content.pb.go:530-538, 6113-6121`).
- **Gateways stay transparent (ADR-0001).** No body rewriting on the Kimi, Claude Code, OpenRouter or custom-endpoint routes unless a new ADR scopes the exception (Task 10b is the only candidate).
- **A wire-shape change on the Cloud Code path needs recorded evidence** under `.reference/` (probe JSONL or agy capture). With no evidence the existing shape is kept and the decision is written down.
- **Evidence never holds credentials or prompt text.** The capture addon records by name allowlist; probe prompts are fixed one-liners.
- **Stage named files only** (`git add <paths>`, never `-A`): the working tree carries many unrelated untracked files.
- **Before every commit that touches Go:** `gofmt -l <touched dirs>` prints nothing, `go vet ./...` is clean, `go test -race ./...` passes.
- **Conventional Commits** (`fix(format): …`, `test(api): …`, `docs: …`), matching `git log`.
- **Do not push or open a PR** unless the user asks; Task 14 ends by asking.

---

## 0. What was verified while writing this plan

Verdicts below were produced by reading code, fetching the vendors' own docs on 2026-10-05, reading the committed captures, and running throwaway tests against the unmodified tree. "Red test" means a test that fails on the current code and is carried into the plan.

| Gap | Verdict | Evidence | Closed by |
|---|---|---|---|
| **G1** `output_config.effort` ignored on Cloud Code path | **Confirmed** | `grep output_config` over `internal/` and `cmd/` finds only a comment (`internal/ccidentity/apply.go:119`). Probe: `thinking:adaptive` + `output_config.effort=max` → `thinking_budget:1024`, identical to no effort. Real Claude Code sends `output_config:{effort}` and `thinking:{type,display}` on every request (`.reference/claude-code-headers-20260923.txt:94-102`). | Tasks 4, 5, 6 |
| **G2** `xhigh`/`max` collapsed to `high` | **Confirmed, mis-framed** | Collapse is *correct* for the Gemini flash families (three published tiers, none above `high`) and for `xhigh` on Claude 4.6 — Claude Code's own docs: "`xhigh` runs as `high` on Opus 4.6"; Opus/Sonnet 4.6 take `low, medium, high, max` only (code.claude.com model-config, "Adjust effort level"). Only `max` on 4.6 is a real loss. | Task 4 (downward-fallback rule), Task 12d (gated) |
| **G3** `minimal`→`low`; `MINIMAL` unreachable | **Confirmed, no reachable target** | `MINIMAL = 4` exists in the proto. Google's table marks `minimal` "Not supported (error)" on some families and supported on others (ai.google.dev generate-content/thinking, "Thinking levels"). The catalog carries no per-model level list (`ModelDetails.thinking_level` is one int, `model_configs.pb.go:185`), so nothing can say where emitting it is safe. | Task 4 keeps `LevelMinimal`; probe GF records evidence; emission is a documented non-goal (§Decisions D6) |
| **G4** `adaptive` flattened to a fixed budget | **Partial** | With a live catalog entry `adaptive` gets the catalog's own default (1024 for the Opus 4.6 fixture), not 32000; 32000/16000 apply only without a catalog (`options == nil`). The real defect was G1: effort could not move the budget. | Task 6; Task 12b (gated) |
| **G5** `thinkingConfig` casing differs between branches | **Confirmed; the correct side is unknown** | Claude branch snake_case, others camelCase. Both the JS reference and three existing assertions pin snake_case for Claude (`format_test.go:283`, the parity fixture `testdata/google-request.json:31`, `accounts/retry_test.go:223`). **No capture in `.reference/` contains a `thinkingConfig` at all** (grep over every file: zero hits). | Tasks 2–3 (capture + probe GA); Task 12a (gated) |
| **G6** Gemini `maxOutputTokens` clamped to 16384 | **Confirmed** | Introduced by the initial port, commit `45cd493` ("phase 3: port format conversion and streaming"), as a JS constant. Probe: `max_tokens:65536` on a Gemini tier → `16384`. README advertises 65,536 (`README.md:704-711`). | Task 7 (gated by GE) |
| **G7** no `temperature`/`top_p`/`top_k` coercion under thinking | **Premise unverified, probably moot** | Real Claude Code sends none of `temperature, top_p, top_k, stop_sequences` (`claude-code-headers-20260923.txt:99-102`). The rule is Anthropic's; whether Cloud Code enforces it is unknown. The parity fixture pins sampling passing through with thinking on. | Probe GC; Task 12c (gated, default: no change) |
| **G8** `display` ignored; `redacted_thinking` "vanishes" | **Refuted as a bug** | A red-test attempt passed on current code: a `redacted_thinking` block counts as an *unsigned* thinking part (`thinking.go:23,58`), is stripped, and the tool-loop recovery path runs; nothing leaks upstream. `display` is Anthropic's concept; Cloud Code returns thought text and the client collapses it. | Regression test in Task 6; documented in Task 13 |
| **G9** two divergent effort normalizers | **Confirmed, worse than reported** | Red tests: on the legacy path (`options == nil`, `internal/api/server.go:1129`) `reasoning_effort:low` → budget **16000** and a top-level `thinking_budget` is ignored, because `clampGeminiThinkingBudget(model, thinking["budget_tokens"])` overwrote them (`request.go:236-238`). `thinking_budget:0` emitted the default budget while the catalog treated it as "off". `reasoning:{effort:…}` (object form) was unsupported. An unknown spelling was kept as junk and suppressed budget back-mapping. | Tasks 4, 5, 6 |
| **G10** 2.5-Pro budget ceiling cut to 24576 | **Confirmed, production-dead** | Reachable only when `options == nil`; `cmd/proxy/main.go:261` always sets `Backend`, so only `server.backend == nil` (tests, `cmd/formatcheck`) reaches it. Google documents 2.5 Pro `128–32768`, Flash/Flash-Lite up to `24576`. | Task 6 |
| **G11** discovery output fallback = 200000 | **Confirmed** | Red test: a context-less allowlist entry advertises `max_output_tokens = 200000` against `context_window = 200000`. The two existing guards use 1M-context fixtures and pass whatever the constant is. | Task 8 |
| **G12** `max_output_tokens` alias / missing `max_tokens` | **Refuted as a gap (by design)** | `/v1/messages` has no `max_output_tokens`; the OpenAI endpoint already maps `max_tokens`/`max_completion_tokens` (`openai_request.go:144-152`). Anthropic and Kimi both *require* `max_tokens` (Kimi OpenAPI: `minimum: 1`, required), so forwarding without it is the upstream behavior; `TestServer_ForwardToKimi_OmitsMaxTokensWhenNothingKnown` pins it. | Documented (Task 13); no code |
| **G13** budget can end up ≥ `maxOutputTokens` | **Confirmed (latent)** | Red test: `budget_tokens:70000, max_tokens:80000`, cap 64000 → `maxOutputTokens=64000, budget=70000`; `budget 10000, cap 8192` → `8192` vs `10000`. The raise (`+8192`) runs *before* the catalog cap. | Task 7 (gated by GD control) |
| **G14** intentional divergences | n/a | | Task 13 |

### Findings the report missed

| # | Finding | Evidence | Closed by |
|---|---|---|---|
| **N1** | **Kimi Code is not the Kimi Open Platform.** The repo's OAuth gateway talks to Kimi *Code* (`https://api.kimi.ai/coding/…`): model IDs `k3`, `k3-256k`, `kimi-for-coding`, `kimi-for-coding-highspeed`; contexts 1,048,576 / 262,144; efforts `low|high|max` with defaults `high` (k3) / `max` (kimi-for-coding); **the server itself maps Claude Code levels** (`medium→high`, `xhigh→max`, `ultra/max→max`, `minimum/light→low`, `none→thinking disabled`, anything else → HTTP 400) (kimi.com/code/docs/en/kimi-code/models.html). The report's "three-way mismatch" claim is wrong for Kimi Code. The Open Platform (`api.moonshot.ai/anthropic`, the config default) publishes `low|high|max`, default `max`, fixed `temperature 1.0`/`top_p 0.95`, K2.7 thinking forced on, and **discontinued** `kimi-k2.5`/`moonshot-v1*`/`kimi-k2*` (platform.kimi.ai/docs/models, api/models-overview). | fetched 2026-10-05 | Task 10, Task 13 |
| **N2** | Claude Code gateway default for **Haiku 4.5** is `MaxOutputTokens: 8192`; Anthropic publishes **64K** (200K context). `applyMaxTokensPolicy` therefore clamps a 64K client request to 8K. | `internal/claudecode/router.go:58`; Models overview | Task 9 |
| **N3** | The default allowlist **deliberately omits** `claude-opus-5-5` / `claude-sonnet-5-5` (the current flagships and what Claude Code's `opus`/`sonnet` aliases resolve to): `TestMatchClaudeCodeModel_AllowlistAndAlias` pins "default allowlist must not claim claude-sonnet-5-5" so a new model is never silently rewritten. | `claudecode_proxy_test.go:323-333` | Kept; documented (Task 13) |
| **N4** | The OpenAI `/v1/chat/completions` translation **drops `reasoning_effort`** entirely, so the proxy's `reasoning_effort` extension is unreachable from OpenAI-shaped clients. | `openai_request.go` copies only `max_tokens`, `temperature`, `top_p`, `stop`, `stream` | Task 11 (**opt-in**) |
| **N5** | The headroom OutputShaper runs before every gateway; Anthropic documents that changing top-level `output_config.effort` between requests invalidates the prompt cache (Effort doc, "Hold top-level effort constant"), and Kimi says the same. Extending the shaper to clamp it would break caching on those routes. | fetched 2026-10-05 | Guard test, Task 5 |
| **N6** | `ThinkingConfig`'s three fields are independent proto3 `optional` presence fields (synthetic oneofs `_include_thoughts`, `_thinking_budget`, `_thinking_level`, descriptor `content.pb.go:7584-7586`), **not one oneof**. "Only one of budget/level may be set" is a server rule that no vendor doc I could fetch states; the proxy never sends both. | descriptor bytes | Probe GX records it; Task 13 errata |
| **N7** | Kimi's `/v1/models` documents `context_length` and `supports_*` flags but **no `max_tokens`**; `kimi.ModelItem.MaxOutputTokens` (`json:"max_tokens"`) has no upstream source, so Kimi output limits can only be operator-set. | platform.kimi.ai/docs/api/list-models | Documented (Task 13) |
| **N8** | `thinking_budget: -1` is read as "off" (non-positive and not `enabled`), while Gemini's convention for that name is "dynamic". Behavior is unchanged by this plan (parity-tested). | `internal/reasoning` parity run, 5,280 legacy inputs, 0 mismatches | Noted (Task 13) |
| **N9** | **Policy risk, no task:** Kimi Code's docs say tampering with the client User-Agent "may result in suspension of membership benefits"; `internal/kimi/identity.go` presents `KimiCLI/1.0.0`. Out of this plan's scope; flagged so the owner can decide. | kimi.com/code/docs/en/ | none |
| **N10** | **The real `agy` CLI has `--effort low\|medium\|high\|xhigh\|max` and `--model`** (`agy --help` on this host, 2026-10-05). That is first-party proof the Antigravity route's effort vocabulary is Anthropic's five levels, and it makes the ground truth capturable: run agy per (model, effort) through the existing MITM harness and record the `generationConfig` it really sends. That answers G2 (does agy distinguish `max`?), G4, G5 (casing), G6 (agy's `maxOutputTokens`) and gives the effort→budget numbers directly instead of inventing them. | `agy --help` | Tasks 2–3, gate GM, Task 12e |

---

## Decisions (override before executing; each is the conservative default)

- **D1 — Precedence.** `output_config.effort` is *ambient* (Claude Code sends it on every request) and therefore the **lowest** precedence signal. For tier routing: `reasoning_effort` > bare thinking budget > a tier named in the model ID > `output_config.effort` > catalog default. For budget emission: `reasoning_effort` > explicit budget > `output_config.effort` > catalog default. Rationale: with the opposite order, any Claude Code user who picked `gemini-3.8-flash-low` would be silently re-routed to `-high` by their default `high` effort.
- **D2 — Level fallback.** Adopt Claude Code's documented rule: a level a model does not publish runs as *the highest published level at or below it*. Gemini flash tiers and budget-style Gemini/GPT-OSS publish `low, medium, high`; Claude 4.6 publishes `low, medium, high, max`. `max` keeps its own meaning on Claude only if gate GD passes (Task 12d); otherwise it equals `high` and the doc says why.
- **D3 — Effort→budget table applies to ambient effort. This is a behavior change with a quota cost: please sign off.** Claude Code defaults to effort `high` on Opus/Sonnet 4.6, so every Claude 4.6 request through Cloud Code moves from the catalog default budget (1024 in the Opus fixture, which is also what agy itself sends) to **32000**. The alternative "treat the model's default level as unset" was rejected: it makes `medium` (8000) think more than `high` (1024). If you decline D3 for Claude, change the third `case` of `thinkingBudget` (Task 6) to `case params.Level != reasoning.LevelUnset && family != FamilyClaude:` and edit the Claude ambient-effort rows of `TestOutputConfigEffortSetsBudgetOnBudgetStyleModels` plus the budget assertion in `TestClaudeCodeCapturedBodyShapeConvertsToACleanGenerationConfig` to expect the catalog default (1024); Gemini budget-style models and tier routing are unaffected. The probe sweep (GH, Task 3) measures what the change actually costs, and gate GM may settle it outright: if agy's capture shows the numbers it sends per level, Task 12e adopts them and the question becomes "does the proxy match agy", which is the project's rule.
- **D4 — Claude Code gateway defaults.** Fix Haiku 4.5's limit only (Task 9). Do **not** add `claude-opus-5-5`/`claude-sonnet-5-5`, and do not add the 4.x IDs: the first would reverse a pinned design decision (N3), the second would let the Anthropic OAuth gateway claim requests the free Cloud Code route serves today.
- **D5 — Kimi stays transparent.** No effort normalization unless gate GG shows the Open Platform rejects Claude Code's levels (Task 10b, with an ADR).
- **D6 — Closed without code:** G3 emission, G7 coercion (unless GC says Cloud Code rejects it), G8, G12, `thinking.display` handling, Zen-wire reasoning (the wires drop `thinking` on purpose and no per-model evidence exists), the `claude-opus-5-5` defaults (D4).
- **D7 — Opt-in:** Task 11 (OpenAI `reasoning_effort` bridge) is outside "/v1/messages". It is written out in full; skip it if you want the plan scoped to the Anthropic surface.

## Decision gates

Task 3 records one verdict per gate. Each gated change in Tasks 7 and 12 states both branches in full.

| Gate | Question | Probe case(s) | If accepted | If rejected / unverified |
|---|---|---|---|---|
| **GM** | What `generationConfig` does **agy itself** send for each (model, `--effort`)? | agy capture (Task 3 step 4) | **Beats every provisional constant in this plan**: Task 12e rewrites the effort→budget table, the per-family level sets, the key casing and the output cap to match what agy sends | Keep the provisional table (D3); no capture means no change to casing |
| **GA** | May the Claude route use camelCase `thinkingConfig` keys? | `GA-claude-snake`, `GA-claude-camel`, **and** GM's Claude `thinkingConfig` spelling | Task 12a (flip two constants, update the three pinned assertions and the two this plan adds) — only if GM also shows camelCase; if GM shows snake_case the pin comment in Task 6 is the whole fix | Keep snake_case; add the pin comment (already in Task 6) |
| **GB** | Does the Claude route accept `thinkingConfig` with no budget, and still think? | `GB-claude-no-budget` (status 200 **and** `thinkingBlocks > 0`) | Task 12b | Keep the catalog default for `adaptive` |
| **GC** | Does Cloud Code reject non-default sampling while Claude thinks? | `GC-claude-temperature`, `GC-claude-top-k` (HTTP 400) | Task 12c | No change; document that Cloud Code is more lenient than Anthropic |
| **GD** | (control) Is `budget == maxOutputTokens` rejected? (max) Is a budget of `cap − 8192` accepted? | `GD-claude-budget-equals-max` → 400; `GD-claude-budget-55808` → 200 | Task 7 keeps the budget-shrink branch; Task 12d | Control accepted → skip the shrink branch in Task 7 (keep only the raise). Max rejected → skip 12d |
| **GE** | Is `maxOutputTokens` at the advertised 65536 accepted on a Gemini route? | `GE-tier-65536` | Task 7 default (live limit is authoritative) | Task 7 alternative: keep 16384 and advertise 16384 in `/v1/models` |
| **GF** | Is `thinkingLevel: MINIMAL` accepted on a served tier? | `GF-tier-minimal` | Record only | Record only (D6) |
| **GX** | Is `thinkingLevel` + `thinkingBudget` together rejected? | `GX-tier-level-and-budget` | Record only (the proxy never sends both) | Record only |
| **GH** | Does the budget move real thinking? | `GH-claude-effort-{1024,8000,32000}` (`-heavy`) | Attach the numbers to D3's sign-off | If thinking tokens are flat across the three, tell the owner D3 buys nothing on this route |
| **GG** | Does the Moonshot Open Platform reject `output_config.effort` of `medium`/`xhigh`? | Task 3 step 5 (manual, needs a Moonshot key) | Task 10b + ADR-0005 | No change (D5) |

## File structure

**Create**
- `cmd/paramprobe/cases.go`, `main.go`, `cases_test.go` — live GenerationConfig probe (Task 1).
- `internal/reasoning/reasoning.go`, `reasoning_test.go` — the one parser (Task 4).
- `internal/format/thinkingconfig.go`, `thinkingconfig_test.go` — effort→budget policy, ceilings, budget/max reconciliation (Tasks 6–7).
- `internal/api/openai_reasoning.go`, `openai_reasoning_test.go` — opt-in OpenAI bridge (Task 11).
- `docs/reasoning-parameters.md` — the parity reference (Task 13).
- `docs/adr/0005-kimi-open-platform-effort-normalization.md` — only if gate GG says so (Task 10b).
- `.reference/cloudcode-params-probe-20261005.jsonl`, `.reference/agy-generationconfig-mitm-20261005.jsonl` (one line per agy run: `label` = `<model>__<effort>`, the model, `requestType`, and the allowlisted `generation_config`), `.reference/params-verdicts-20261005.txt` — evidence (Task 3).

**Modify**
- `scripts/mitm_header_dump.py`, `scripts/test_mitm_header_dump.py` — record agy's `generationConfig` by allowlist (Task 2).
- `internal/modelcatalog/catalog.go`, `catalog_test.go` — tier routing on `reasoning.Params`; delete `ExtractReasoningParams` (Task 5).
- `internal/headroom/stages/shaper/shaper_test.go` — guard (Task 5).
- `internal/format/request.go`, `model.go` — converter on `reasoning.Params`; delete `clampGeminiThinkingBudget` (Tasks 6–7).
- `internal/accounts/retry_test.go` — dispatcher-level integration test (Task 6).
- `internal/api/server.go`, `models_discovery_test.go` — discovery fallback (Task 8).
- `internal/claudecode/router.go`, `router_test.go` — Haiku 4.5 limit (Task 9).
- `internal/api/kimi_proxy_test.go` — transparency guard (Task 10).
- `internal/api/openai_request.go` — one call (Task 11).
- `README.md`, `CONTEXT.md`, `docs/research/2026-10-05-api-parameters-reasoning-effort-matrix.md` — docs (Task 13).

---

## Phase A — Verify against the upstream

### Task 1: Live GenerationConfig probe (`cmd/paramprobe`)

**Files:**
- Create: `cmd/paramprobe/cases_test.go`
- Create: `cmd/paramprobe/cases.go`
- Create: `cmd/paramprobe/main.go`

**Interfaces:**
- Consumes (all already used by `cmd/probe429` / `cmd/formatcheck`): `accounts.DefaultConfigPath() (string, error)`, `accounts.Load(path)`, `accounts.NewCredentialResolver(auth.Manager{}, nil).Resolve(ctx, account)`, `cloudcode.New(cloudcode.Options{AccessToken, Timeout})`, `(*cloudcode.Client).StreamGenerateContent(ctx, payload, cloudcode.RequestOptions, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)`, `*cloudcode.HTTPError{StatusCode, Body}`, `proxyformat.NewBuilder().BuildCloudCodeRequestWithModel(request, project, email, proxyformat.ModelOptions{})`, `proxyformat.NewThinkingAccumulator()` (`Consume`, `Response`, `ThinkingTokens`, `OutputTokens`), `proxyformat.GetModelFamily`.
- Produces: `Models{Claude, Tier string}`, `Case`, `Result` with `Outcome() string` (`accepted` = 200, `rejected` = 400, else `inconclusive`), `buildCases(Models) []Case`, `buildPayload(c Case, project, email string) map[string]any`, `requestOptions(Case) cloudcode.RequestOptions`, `Summary([]Result) []string`. CLI flags `-claude -tier -heavy -gate -out -list`. Task 3 runs it.

The probe sends one tiny request per case with a *hand-built* `generationConfig` swapped into the production Cloud Code envelope, so it measures what upstream accepts, not what the converter emits.

- [ ] **Step 1: Write the failing tests**

Create `cmd/paramprobe/cases_test.go`:

```go
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

var testModels = Models{Claude: "claude-opus-4-6-thinking", Tier: "gemini-3.8-flash-high"}

func TestBuildCasesAreWellFormed(t *testing.T) {
	t.Parallel()
	gates := map[string]bool{
		GateCasing: true, GateAdaptive: true, GateSampling: true, GateMaxBudget: true,
		GateOutputCap: true, GateMinimal: true, GateBoth: true, GateEffort: true,
	}
	seen := map[string]bool{}
	covered := map[string]bool{}
	for _, c := range buildCases(testModels) {
		if seen[c.ID] {
			t.Errorf("duplicate case ID %q", c.ID)
		}
		seen[c.ID] = true
		covered[c.Gate] = true
		if !gates[c.Gate] {
			t.Errorf("%s: unknown gate %q", c.ID, c.Gate)
		}
		if !strings.HasPrefix(c.ID, c.Gate+"-") {
			t.Errorf("%s: ID must start with its gate %q", c.ID, c.Gate)
		}
		if c.Model == "" || c.Prompt == "" || c.Question == "" || len(c.Config) == 0 {
			t.Errorf("%s: incomplete case %+v", c.ID, c)
		}
		if _, err := json.Marshal(c.Config); err != nil {
			t.Errorf("%s: config is not JSON-encodable: %v", c.ID, err)
		}
		if c.Heavy != (c.Gate == GateEffort) {
			t.Errorf("%s: only the effort sweep may be heavy (heavy=%v)", c.ID, c.Heavy)
		}
	}
	for gate := range gates {
		if !covered[gate] {
			t.Errorf("gate %s has no probe", gate)
		}
	}
}

func TestCasesTargetTheRightModelFamily(t *testing.T) {
	t.Parallel()
	for _, c := range buildCases(testModels) {
		_, snakeKeys := c.Config["thinkingConfig"].(map[string]any)["include_thoughts"]
		wantClaude := c.Model == testModels.Claude
		if c.Gate == GateOutputCap || c.Gate == GateMinimal || c.Gate == GateBoth {
			if wantClaude {
				t.Errorf("%s: Gemini gate probes must not run against the Claude route", c.ID)
			}
		}
		if snakeKeys && !wantClaude {
			t.Errorf("%s: snake_case thinkingConfig belongs to the Claude route", c.ID)
		}
	}
}

func TestOutcomeOnlyCountsAnUpstream400AsRejection(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]string{200: "accepted", 400: "rejected", 429: "inconclusive", 500: "inconclusive", 0: "inconclusive"} {
		if got := (Result{Status: status}).Outcome(); got != want {
			t.Errorf("status %d: outcome = %q, want %q", status, got, want)
		}
	}
}

func TestSummaryGroupsByGateInFirstSeenOrder(t *testing.T) {
	t.Parallel()
	got := Summary([]Result{
		{ID: "GA-a", Gate: "GA", Status: 200},
		{ID: "GB-a", Gate: "GB", Status: 400},
		{ID: "GA-b", Gate: "GA", Status: 429},
	})
	want := []string{"GA: GA-a=accepted, GA-b=inconclusive", "GB: GB-a=rejected"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
}

// The probe is only meaningful if the request differs from production in
// exactly one place: the generationConfig under test.
func TestBuildPayloadKeepsTheProductionEnvelopeAndSwapsOnlyTheConfig(t *testing.T) {
	t.Parallel()
	c := buildCases(testModels)[1] // GA-claude-camel
	payload := buildPayload(c, "proj-1", "user@example.com")
	if payload["project"] != "proj-1" || payload["model"] != c.Model ||
		payload["userAgent"] != "antigravity" || payload["requestType"] != "agent" {
		t.Fatalf("envelope = %#v", payload)
	}
	inner, _ := payload["request"].(map[string]any)
	generation, _ := inner["generationConfig"].(map[string]any)
	encoded, _ := json.Marshal(generation)
	want, _ := json.Marshal(c.Config)
	if string(encoded) != string(want) {
		t.Fatalf("generationConfig = %s, want exactly the case config %s", encoded, want)
	}
	if inner["systemInstruction"] == nil || inner["contents"] == nil {
		t.Fatalf("inner request lost the production system instruction or contents: %#v", inner)
	}
}

func TestClaudeCasesCarryTheInterleavedThinkingBeta(t *testing.T) {
	t.Parallel()
	for _, c := range buildCases(testModels) {
		got := requestOptions(c).Headers.Get("anthropic-beta")
		if c.Model == testModels.Claude && got != "interleaved-thinking-2025-05-14" {
			t.Errorf("%s: anthropic-beta = %q", c.ID, got)
		}
		if c.Model != testModels.Claude && got != "" {
			t.Errorf("%s: a non-Claude probe must not send anthropic-beta, got %q", c.ID, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/paramprobe -count=1`
Expected: FAIL — build error `undefined: Models` (and `buildCases`, `Result`, …).

- [ ] **Step 3: Write the matrix (pure part)**

Create `cmd/paramprobe/cases.go`:

```go
package main

import (
	"fmt"
	"strings"
	"time"

	proxyformat "antigravity-go-proxy/internal/format"
)

// Gate names tie a probe to the decision it unlocks in
// docs/superpowers/plans/2026-10-05-api-parameter-parity.md ("Decision gates").
const (
	GateCasing    = "GA" // Claude thinkingConfig key casing
	GateAdaptive  = "GB" // Claude thinkingConfig without a budget
	GateSampling  = "GC" // sampling parameters under thinking
	GateMaxBudget = "GD" // large Claude thinking budget; budget == maxOutputTokens
	GateOutputCap = "GE" // Gemini maxOutputTokens at the advertised limit
	GateMinimal   = "GF" // thinkingLevel MINIMAL
	GateBoth      = "GX" // thinkingLevel and thinkingBudget in one request
	GateEffort    = "GH" // whether the budget moves real thinking
)

const (
	okPrompt     = "Reply with exactly: OK"
	reasonPrompt = "What is the sum of the first 40 prime numbers? Reply with only the number."
)

// Models are the upstream routing IDs the matrix runs against. Both must be
// IDs the account's fetchAvailableModels catalog publishes.
type Models struct {
	Claude string // a Claude thinking route, e.g. claude-opus-4-6-thinking
	Tier   string // a thinkingLevel-style Gemini route, e.g. gemini-3.8-flash-high
}

// Case is one upstream call. Config replaces the request's generationConfig
// verbatim: the probe asks what Cloud Code accepts, not what the converter
// would emit.
type Case struct {
	ID       string
	Gate     string
	Question string
	Model    string
	Prompt   string
	Heavy    bool // spends real thinking tokens; needs -heavy
	Config   map[string]any
}

func claudeCase(m Models, id, gate, question string, config map[string]any) Case {
	return Case{ID: id, Gate: gate, Question: question, Model: m.Claude, Prompt: okPrompt, Config: config}
}

func tierCase(m Models, id, gate, question string, config map[string]any) Case {
	return Case{ID: id, Gate: gate, Question: question, Model: m.Tier, Prompt: okPrompt, Config: config}
}

func snake(budget any) map[string]any {
	config := map[string]any{"include_thoughts": true}
	if budget != nil {
		config["thinking_budget"] = budget
	}
	return config
}

// buildCases returns the fixed probe matrix. It does no I/O.
func buildCases(m Models) []Case {
	cases := []Case{
		claudeCase(m, "GA-claude-snake", GateCasing,
			"baseline: the snake_case thinkingConfig the proxy sends to Claude today",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": snake(1024)}),
		claudeCase(m, "GA-claude-camel", GateCasing,
			"does the Claude route also accept the proto JSON (camelCase) spelling?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingBudget": 1024}}),
		claudeCase(m, "GB-claude-no-budget", GateAdaptive,
			"does the Claude route accept thinkingConfig without a budget?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": snake(nil)}),
		claudeCase(m, "GC-claude-temperature", GateSampling,
			"does Cloud Code reject a non-default temperature while Claude thinks?",
			map[string]any{"maxOutputTokens": 512, "temperature": 0.2, "thinkingConfig": snake(1024)}),
		claudeCase(m, "GC-claude-top-k", GateSampling,
			"does Cloud Code reject topK while Claude thinks?",
			map[string]any{"maxOutputTokens": 512, "topK": 10, "thinkingConfig": snake(1024)}),
		claudeCase(m, "GD-claude-budget-55808", GateMaxBudget,
			"is a budget of maxOutputTokens-8192 accepted (the largest sensible Claude budget)?",
			map[string]any{"maxOutputTokens": 64000, "thinkingConfig": snake(55808)}),
		claudeCase(m, "GD-claude-budget-equals-max", GateMaxBudget,
			"negative control: is a budget equal to maxOutputTokens rejected?",
			map[string]any{"maxOutputTokens": 4096, "thinkingConfig": snake(4096)}),
		tierCase(m, "GE-tier-65536", GateOutputCap,
			"is maxOutputTokens at the advertised 65536 accepted on a Gemini route?",
			map[string]any{"maxOutputTokens": 65536, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": "LOW"}}),
		tierCase(m, "GF-tier-minimal", GateMinimal,
			"is thinkingLevel MINIMAL accepted on the served Gemini tier?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": "MINIMAL"}}),
		tierCase(m, "GX-tier-level-and-budget", GateBoth,
			"is thinkingLevel together with thinkingBudget rejected?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": "LOW", "thinkingBudget": 1024}}),
	}
	for _, budget := range []int{1024, 8000, 32000} {
		cases = append(cases, Case{
			ID: fmt.Sprintf("GH-claude-effort-%d", budget), Gate: GateEffort,
			Question: "does the budget the effort table emits change how much Claude thinks?",
			Model:    m.Claude, Prompt: reasonPrompt, Heavy: true,
			Config: map[string]any{"maxOutputTokens": 40000, "thinkingConfig": snake(budget)},
		})
	}
	return cases
}

// Result is one probe's outcome. Status 0 means the call failed before a
// response; Error carries the detail.
type Result struct {
	ID             string    `json:"id"`
	Gate           string    `json:"gate"`
	Question       string    `json:"question"`
	Model          string    `json:"model"`
	Status         int       `json:"status"`
	Error          string    `json:"error,omitempty"`
	StopReason     string    `json:"stopReason,omitempty"`
	ThinkingBlocks int       `json:"thinkingBlocks"`
	ThinkingTokens int       `json:"thinkingTokens"`
	OutputTokens   int       `json:"outputTokens"`
	LatencyMS      int64     `json:"latencyMs"`
	At             time.Time `json:"at"`
}

// Outcome classifies a result for the decision gates. Only an upstream 400
// counts as a rejection: a 429 or a transport error says nothing about the
// request shape.
func (result Result) Outcome() string {
	switch result.Status {
	case 200:
		return "accepted"
	case 400:
		return "rejected"
	}
	return "inconclusive"
}

// Summary groups outcomes by gate: "GA: GA-claude-snake=accepted, ...".
func Summary(results []Result) []string {
	order := []string{}
	byGate := map[string][]string{}
	for _, result := range results {
		if _, seen := byGate[result.Gate]; !seen {
			order = append(order, result.Gate)
		}
		byGate[result.Gate] = append(byGate[result.Gate], result.ID+"="+result.Outcome())
	}
	lines := make([]string, 0, len(order))
	for _, gate := range order {
		lines = append(lines, gate+": "+strings.Join(byGate[gate], ", "))
	}
	return lines
}

// buildPayload wraps the case in the production Cloud Code envelope, then
// swaps in the case's generationConfig.
func buildPayload(c Case, project, email string) map[string]any {
	request := map[string]any{
		"model":    c.Model,
		"messages": []any{map[string]any{"role": "user", "content": c.Prompt}},
	}
	payload := proxyformat.NewBuilder().BuildCloudCodeRequestWithModel(request, project, email, proxyformat.ModelOptions{})
	inner, _ := payload["request"].(map[string]any)
	inner["generationConfig"] = c.Config
	return payload
}
```

- [ ] **Step 4: Write the live runner**

Create `cmd/paramprobe/main.go`:

```go
// Command paramprobe measures which GenerationConfig shapes Cloud Code accepts
// for the models the proxy serves. Each probe sends one tiny request with a
// hand-built generationConfig and records the upstream status, so a proxy change
// that alters the wire shape can cite evidence instead of a doc reading.
//
// It talks to the user's own accounts with ordinary client requests, on the same
// generation host the proxy uses. The -heavy cases spend real thinking tokens;
// leave them off unless the effort sweep is wanted.
//
// Not part of the proxy. See
// docs/superpowers/plans/2026-10-05-api-parameter-parity.md (Task 1).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
)

func main() {
	claude := flag.String("claude", "claude-opus-4-6-thinking", "upstream Claude thinking model ID")
	tier := flag.String("tier", "gemini-3.8-flash-high", "upstream thinkingLevel-style Gemini model ID")
	heavy := flag.Bool("heavy", false, "also run the effort sweep (spends real thinking tokens)")
	only := flag.String("gate", "", "run only the probes of this gate (GA, GB, ...)")
	out := flag.String("out", "", "append results as JSONL to this path")
	list := flag.Bool("list", false, "print the probe matrix and exit without calling upstream")
	flag.Parse()

	cases := buildCases(Models{Claude: *claude, Tier: *tier})
	if *list {
		for _, c := range cases {
			fmt.Printf("%-28s %s heavy=%-5v %s\n", c.ID, c.Gate, c.Heavy, c.Question)
		}
		return
	}

	account, err := loadAccount()
	if err != nil {
		fmt.Println("load account:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	var results []Result
	for _, c := range cases {
		if (c.Heavy && !*heavy) || (*only != "" && !strings.EqualFold(*only, c.Gate)) {
			continue
		}
		fmt.Printf("=== %s (%s)\n", c.ID, c.Model)
		result := run(ctx, account, c)
		fmt.Printf("    status=%d %s thinkingBlocks=%d thinkingTokens=%d %s\n",
			result.Status, result.Outcome(), result.ThinkingBlocks, result.ThinkingTokens, result.Error)
		results = append(results, result)
	}

	fmt.Println()
	for _, line := range Summary(results) {
		fmt.Println(line)
	}
	if *out != "" {
		if err := writeJSONL(*out, results); err != nil {
			fmt.Println("write results:", err)
			os.Exit(1)
		}
		fmt.Println("results written to", *out)
	}
}

type poolAccount struct {
	Email   string
	Project string
	Client  *cloudcode.Client
}

// loadAccount resolves the first enabled pool account's credentials.
func loadAccount() (poolAccount, error) {
	path, err := accounts.DefaultConfigPath()
	if err != nil {
		return poolAccount{}, err
	}
	file, err := accounts.Load(path)
	if err != nil {
		return poolAccount{}, err
	}
	resolver := accounts.NewCredentialResolver(auth.Manager{}, nil)
	for _, account := range file.Accounts {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		credentials, err := resolver.Resolve(ctx, account)
		cancel()
		if err != nil {
			fmt.Printf("  skip %s: resolve: %v\n", account.Email, err)
			continue
		}
		return poolAccount{
			Email:   account.Email,
			Project: account.ProjectID,
			Client:  cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: 120 * time.Second}),
		}, nil
	}
	return poolAccount{}, errors.New("no usable account in the pool")
}

// requestOptions adds the beta header the proxy sends for Claude thinking
// routes (accounts.Dispatcher.StreamGenerateContent does the same).
func requestOptions(c Case) cloudcode.RequestOptions {
	if proxyformat.GetModelFamily(c.Model) != proxyformat.FamilyClaude {
		return cloudcode.RequestOptions{}
	}
	headers := make(http.Header)
	headers.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	return cloudcode.RequestOptions{Headers: headers}
}

func run(ctx context.Context, account poolAccount, c Case) Result {
	result := Result{ID: c.ID, Gate: c.Gate, Question: c.Question, Model: c.Model, At: time.Now()}
	accumulator := proxyformat.NewThinkingAccumulator()
	started := time.Now()
	_, err := account.Client.StreamGenerateContent(ctx, buildPayload(c, account.Project, account.Email), requestOptions(c),
		func(event cloudcode.SSEEvent) error { return accumulator.Consume(event.Data) })
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		var upstream *cloudcode.HTTPError
		if errors.As(err, &upstream) {
			result.Status = upstream.StatusCode
			result.Error = truncate(strings.Join(strings.Fields(upstream.Body), " "), 300)
			return result
		}
		result.Error = err.Error()
		return result
	}
	result.Status = http.StatusOK
	response := accumulator.Response(c.Model, proxyformat.NewSignatureCache(), "msg_paramprobe")
	result.StopReason, _ = response["stop_reason"].(string)
	blocks, _ := response["content"].([]any)
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok && block["type"] == "thinking" {
			result.ThinkingBlocks++
		}
	}
	result.ThinkingTokens = accumulator.ThinkingTokens()
	result.OutputTokens = accumulator.OutputTokens()
	return result
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

func writeJSONL(path string, results []Result) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, result := range results {
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `gofmt -l cmd/paramprobe && go vet ./cmd/paramprobe && go test ./cmd/paramprobe -count=1 && go run ./cmd/paramprobe -list`
Expected: `gofmt` prints nothing; `ok  antigravity-go-proxy/cmd/paramprobe`; `-list` prints 13 rows (`GA-claude-snake` … `GH-claude-effort-32000`, the last three `heavy=true`) without needing credentials.

- [ ] **Step 6: Commit**

```bash
git add cmd/paramprobe
git commit -m "chore(tools): add paramprobe, a live Cloud Code GenerationConfig probe"
```

---

### Task 2: Record agy's `generationConfig` in the MITM capture addon

**Files:**
- Modify: `scripts/mitm_header_dump.py` (docstring `:22-27`, constants `:86-88`, `body_fingerprint` `:174-198`)
- Test: `scripts/test_mitm_header_dump.py`

**Interfaces:**
- Consumes: the addon's existing `body_fingerprint(content: bytes) -> dict | None`, `IDENTITY_SCALARS`.
- Produces: `generation_config_fingerprint(body: dict) -> dict | None`; `body_fingerprint` gains `fingerprint["generation_config"]` for Cloud Code bodies; `requestType` and `userAgent` join `IDENTITY_SCALARS`. Task 3 reads `.request_body.generation_config` with `jq`.

Why an allowlist: the existing guarantee is that caller text stays out of the capture "by construction, not by review". `stopSequences` and `responseSchema` can carry user text, so `generationConfig` is recorded by key allowlist (sampling and thinking scalars only), and key spelling is kept verbatim so the capture settles the casing question (G5).

- [ ] **Step 1: Write the failing tests**

In `scripts/test_mitm_header_dump.py`, add `generation_config_fingerprint,` to the `from mitm_header_dump import (…)` list (after `build_record,`), then add this class above the `if __name__ == "__main__":` line:

```python
class GenerationConfigFingerprintTest(unittest.TestCase):
    def _cloud_code_body(self, generation_config):
        return json.dumps({
            "project": "p",
            "model": "claude-opus-4-6-thinking",
            "requestType": "agent",
            "userAgent": "antigravity",
            "request": {
                "contents": [{"role": "user", "parts": [{"text": "SECRET_PROMPT_TEXT"}]}],
                "generationConfig": generation_config,
            },
        }).encode()

    def test_thinking_and_sampling_scalars_are_kept_verbatim(self):
        fingerprint = body_fingerprint(self._cloud_code_body({
            "temperature": 0.2,
            "maxOutputTokens": 64000,
            "thinkingConfig": {"include_thoughts": True, "thinking_budget": 1024},
        }))
        self.assertEqual(fingerprint["generation_config"], {
            "temperature": 0.2,
            "maxOutputTokens": 64000,
            "thinkingConfig": {"include_thoughts": True, "thinking_budget": 1024},
        })

    def test_key_casing_is_the_casing_on_the_wire(self):
        snake = generation_config_fingerprint(
            {"request": {"generationConfig": {"thinkingConfig": {"thinking_budget": 1}}}})
        camel = generation_config_fingerprint(
            {"request": {"generationConfig": {"thinkingConfig": {"thinkingBudget": 1}}}})
        self.assertEqual(snake, {"thinkingConfig": {"thinking_budget": 1}})
        self.assertEqual(camel, {"thinkingConfig": {"thinkingBudget": 1}})

    def test_enum_level_is_kept(self):
        got = generation_config_fingerprint(
            {"request": {"generationConfig": {"thinkingConfig": {"thinkingLevel": "HIGH", "includeThoughts": True}}}})
        self.assertEqual(got, {"thinkingConfig": {"thinkingLevel": "HIGH", "includeThoughts": True}})

    def test_prompt_bearing_fields_never_land_in_the_capture(self):
        fingerprint = body_fingerprint(self._cloud_code_body({
            "stopSequences": ["SECRET_STOP"],
            "responseSchema": {"description": "SECRET_SCHEMA"},
            "responseMimeType": "application/json",
            "maxOutputTokens": 1,
            "thinkingConfig": {"thinkingLevel": "x" * 500, "extra": "SECRET_EXTRA"},
        }))
        self.assertEqual(fingerprint["generation_config"], {"maxOutputTokens": 1})
        for secret in ("SECRET_PROMPT_TEXT", "SECRET_STOP", "SECRET_SCHEMA", "SECRET_EXTRA"):
            self.assertNotIn(secret, repr(fingerprint))

    def test_wire_constants_of_the_envelope_are_identity_scalars(self):
        fingerprint = body_fingerprint(self._cloud_code_body({"maxOutputTokens": 1}))
        self.assertEqual(fingerprint["requestType"], "agent")
        self.assertEqual(fingerprint["userAgent"], "antigravity")
        self.assertEqual(fingerprint["model"], "claude-opus-4-6-thinking")

    def test_bodies_without_a_generation_config_add_nothing(self):
        self.assertNotIn("generation_config", body_fingerprint(b'{"model":"m"}'))
        self.assertNotIn("generation_config", body_fingerprint(b'{"request":{"contents":[]}}'))
        self.assertNotIn("generation_config", body_fingerprint(b'{"request":"text"}'))
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd scripts && python3 -m unittest test_mitm_header_dump 2>&1 | tail -5`
Expected: FAIL — `ImportError: cannot import name 'generation_config_fingerprint' from 'mitm_header_dump'`.

- [ ] **Step 3: Implement**

In `scripts/mitm_header_dump.py`, replace line 88 (`IDENTITY_SCALARS = ("model", "max_tokens", "stream")`) with:

```python
IDENTITY_SCALARS = ("model", "max_tokens", "stream", "requestType", "userAgent")

# generationConfig knobs describe HOW the model runs, never WHAT the caller
# typed. They are kept by name allowlist so a prompt-bearing field
# (stopSequences, responseSchema, ...) cannot reach the capture file by
# construction, and both key spellings are listed because the capture exists to
# settle which one a client sends. Keys are kept verbatim, so the casing in the
# file is the casing on the wire.
GENERATION_CONFIG_KEYS = frozenset({
    "temperature", "topP", "topK", "maxOutputTokens", "candidateCount", "seed",
})
THINKING_CONFIG_KEYS = frozenset({
    "includeThoughts", "include_thoughts",
    "thinkingBudget", "thinking_budget",
    "thinkingLevel", "thinking_level",
})
MAX_CONFIG_STRING = 32


def _config_scalar(value):
    """The value if it is a short scalar, else None (drops strings that could be text)."""
    if isinstance(value, (bool, int, float)):
        return value
    if isinstance(value, str) and len(value) <= MAX_CONFIG_STRING:
        return value
    return None


def generation_config_fingerprint(body: dict) -> dict | None:
    """Allowlisted request.generationConfig scalars of a Cloud Code body, or None."""
    inner = body.get("request")
    config = inner.get("generationConfig") if isinstance(inner, dict) else None
    if not isinstance(config, dict):
        return None
    kept = {}
    for key, value in config.items():
        if key in GENERATION_CONFIG_KEYS and _config_scalar(value) is not None:
            kept[key] = value
    thinking = config.get("thinkingConfig", config.get("thinking_config"))
    if isinstance(thinking, dict):
        kept_thinking = {
            key: value for key, value in thinking.items()
            if key in THINKING_CONFIG_KEYS and _config_scalar(value) is not None
        }
        if kept_thinking:
            kept["thinkingConfig" if "thinkingConfig" in config else "thinking_config"] = kept_thinking
    return kept
```

Then, at the end of `body_fingerprint` (the loop over `IDENTITY_SCALARS` and `return fingerprint`), make it:

```python
    for name in IDENTITY_SCALARS:
        if name in body and not isinstance(body[name], (dict, list)):
            fingerprint[name] = body[name]
    generation = generation_config_fingerprint(body)
    if generation is not None:
        fingerprint["generation_config"] = generation
    return fingerprint
```

Finally extend the module docstring (lines 22-27). Replace

```
records a body *fingerprint* instead of the body: the key tree with value
types, the redacted metadata map, and a short prefix of the first system block
(the client identity marker). Everything the caller typed — prompts, file
```

with

```
records a body *fingerprint* instead of the body: the key tree with value
types, the redacted metadata map, a short prefix of the first system block
(the client identity marker) and, for Cloud Code bodies, the allowlisted
request.generationConfig scalars (sampling and thinking knobs, key spelling
preserved). Everything the caller typed — prompts, file
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd scripts && python3 -m unittest test_mitm_header_dump 2>&1 | tail -4`
Expected: `Ran 35 tests` … `OK` (the 29 existing tests plus 6 new).

- [ ] **Step 5: Commit**

```bash
git add scripts/mitm_header_dump.py scripts/test_mitm_header_dump.py
git commit -m "feat(fingerprint): record agy generationConfig scalars in the MITM capture"
```

---

### Task 3: Run the probes and the agy capture; record the verdicts (operator-run)

**Files:**
- Create: `.reference/cloudcode-params-probe-20261005.jsonl`
- Create: `.reference/agy-generationconfig-mitm-20261005.jsonl`
- Create: `.reference/params-verdicts-20261005.txt`

**Interfaces:**
- Consumes: Task 1's `paramprobe`, Task 2's addon, the existing `scripts/capture-agy-headers.sh`, a configured account pool, a logged-in `agy`, `mitmdump`, `jq`.
- Produces: one verdict per gate (GA–GH, GM, GG) in `.reference/params-verdicts-20261005.txt`; Tasks 7, 10b, 12a–12e read it.

This task touches the owner's own Google accounts and trusts a local CA. It sends a handful of one-line requests; the `-heavy` sweep spends real thinking tokens. **If any prerequisite is missing, record the gate as `UNVERIFIED` and take that gate's "rejected / unverified" branch — never guess.**

- [ ] **Step 1: Preflight (read-only)**

```bash
git switch -c fix/api-parameter-parity
go build ./... && go run ./cmd/paramprobe -list | head -3
command -v mitmdump agy jq
agy --help | grep -E -- '--(effort|model)'
```
Expected: the build succeeds; `-list` prints probe rows; the three binaries are found; `agy --help` shows `--effort … (low|medium|high|xhigh|max)` and `--model`. If `agy` lacks `--effort`, skip Step 4's effort loop and capture each model once without it.

- [ ] **Step 2: Standard probe run (a few tiny requests)**

```bash
go run ./cmd/paramprobe -out .reference/cloudcode-params-probe-20261005.jsonl
```
Expected: ten cases print `status=… accepted|rejected|inconclusive`, then one `Gx: id=outcome, …` summary line per gate. A case that prints `inconclusive` with a 429 says nothing about the shape: wait 60 s and re-run just that gate (`-gate GC`), appending to the same file. If the Claude or tier model ID is not in your catalog, pass `-claude` / `-tier` with IDs from `agy models`.

- [ ] **Step 3: Effort sweep (spends real tokens; run once)**

```bash
go run ./cmd/paramprobe -heavy -gate GH -out .reference/cloudcode-params-probe-20261005.jsonl
jq -r 'select(.gate=="GH") | [.id, .status, .thinkingBlocks, .thinkingTokens, .outputTokens] | @tsv' .reference/cloudcode-params-probe-20261005.jsonl
```
Expected: three rows. Record the three `thinkingTokens` values in the verdict file; they are what D3's sign-off looks at.

- [ ] **Step 4: agy ground truth — what agy itself sends per (model, effort)**

Trust the mitmproxy CA for the duration (same procedure as `docs/superpowers/plans/2026-09-03-agy-mitm-header-capture.md` Task 2, "Probe B"; if agy then fails TLS, agy pins the certificate — STOP and record GM as `UNVERIFIED`):

```bash
mitmdump --set confdir="$HOME/.mitmproxy" & pid=$!; sleep 3; kill $pid      # creates the CA once
sudo security add-trusted-cert -d -r trustRoot -k "$HOME/Library/Keychains/login.keychain-db" "$HOME/.mitmproxy/mitmproxy-ca-cert.pem"
shasum -a 256 "$HOME/.mitmproxy/mitmproxy-ca-cert.pem"            # keep this for the cleanup below
```

(The `jq` expressions below avoid indexing `null`, so they work with jq 1.6+ and with `jaq`, which is what `jq` resolves to on this host.) Run agy once per cell. Each run is one tiny generation on your own agy account. Budget-style and Claude models get the full effort sweep; the three tiered flash models get their default and `max` (their level is part of the model ID):

```bash
mkdir -p /tmp/agy-gc
run() { # run <model> <effort|->
  local model="$1" effort="$2" args=(--model "$1")
  [ "$effort" != "-" ] && args+=(--effort "$effort")
  MITM_DUMP_REQUEST_BODY=1 MITM_DUMP_OUT="/tmp/agy-gc/${model}__${effort}.jsonl" \
    scripts/capture-agy-headers.sh env HTTPS_PROXY=http://127.0.0.1:18080 HTTP_PROXY=http://127.0.0.1:18080 \
    agy "${args[@]}" --print 'Reply with exactly: OK' >/dev/null
}
for model in claude-opus-4-6-thinking claude-sonnet-4-6 gemini-3.1-pro-high gpt-oss-120b-medium; do
  for effort in low medium high xhigh max; do run "$model" "$effort"; done
done
for model in gemini-3.8-flash-high gemini-3.8-flash-medium gemini-3.8-flash-low; do
  run "$model" -; run "$model" max
done
for f in /tmp/agy-gc/*.jsonl; do
  jq -c --arg label "$(basename "$f" .jsonl)" \
    'select((.request_body // {}).generation_config != null)
     | {label: $label, model: .request_body.model, requestType: .request_body.requestType, generation_config: .request_body.generation_config}' "$f"
done > .reference/agy-generationconfig-mitm-20261005.jsonl
wc -l .reference/agy-generationconfig-mitm-20261005.jsonl
grep -c 'SECRET\|Reply with exactly' .reference/agy-generationconfig-mitm-20261005.jsonl   # must print 0
```

Always revoke the CA, even if a step above failed:

```bash
sudo security delete-certificate -Z <SHA256-from-above> "$HOME/Library/Keychains/login.keychain-db"
pgrep mitmdump || echo "no mitmdump running"
```
Expected: roughly 26 lines (one `generation_config` per run; some runs may add none if agy rejected the level client-side — that absence is itself the answer for that cell). Tabulate:

```bash
jq -r '[.label, (.generation_config|tojson)] | @tsv' .reference/agy-generationconfig-mitm-20261005.jsonl | column -t -s$'\t'
```

- [ ] **Step 5: Kimi Open Platform effort enum (optional; needs a Moonshot key)**

```bash
for effort in low medium high xhigh max; do
  printf '%s -> ' "$effort"
  curl -sS -o /dev/null -w '%{http_code}\n' https://api.moonshot.ai/anthropic/v1/messages \
    -H "Authorization: Bearer $MOONSHOT_API_KEY" -H 'content-type: application/json' \
    -d "{\"model\":\"kimi-k3\",\"max_tokens\":16,\"output_config\":{\"effort\":\"$effort\"},\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
done
```
Expected: `low`, `high`, `max` → 200. GG = **rejected** if `medium` or `xhigh` returns 400. No key → GG = `UNVERIFIED`.

- [ ] **Step 6: Write the verdicts**

Create `.reference/params-verdicts-20261005.txt` with one line per gate in this exact shape (replace the words after `=` with what you observed; copy the numbers, do not summarize):

```
Recorded 2026-10-05. Sources: cloudcode-params-probe-20261005.jsonl, agy-generationconfig-mitm-20261005.jsonl.
GA casing:     snake=<accepted|rejected|inconclusive> camel=<…>  agy-claude-spelling=<snake_case|camelCase|unverified>
GB no-budget:  status=<…> thinkingBlocks=<n>
GC sampling:   temperature=<…> top-k=<…>
GD budgets:    equals-max-control=<…> budget-55808=<…>
GE output cap: 65536=<…>
GF minimal:    <…>
GX both:       <…>
GH sweep:      thinkingTokens 1024=<n> 8000=<n> 32000=<n>
GM agy sends:  claude-opus-4-6-thinking low=<generation_config> … max=<…>   (one row per capture label; paste from the jq table)
GG kimi:       medium=<code> xhigh=<code> | UNVERIFIED
```

Then apply the "Decision gates" table: for every gate write the branch taken as a final line `TAKEN GA=<12a|keep> GB=<12b|keep> GC=<12c|keep> GD=<shrink+12d|shrink only|raise only|none> GE=<default|alternative> GM=<12e|keep> GG=<10b|keep>`.

- [ ] **Step 7: Commit the evidence**

```bash
git add .reference/cloudcode-params-probe-20261005.jsonl .reference/agy-generationconfig-mitm-20261005.jsonl .reference/params-verdicts-20261005.txt
git commit -m "docs(reference): record Cloud Code GenerationConfig probe and agy capture for 2026-10-05"
```

---

## Phase B — Fix what the evidence confirms

### Task 4: `internal/reasoning` — one parser for every effort signal

**Files:**
- Create: `internal/reasoning/reasoning_test.go`
- Create: `internal/reasoning/reasoning.go`

**Interfaces:**
- Consumes: nothing (standard library only; it must stay importable from `format`, `modelcatalog` and `api` without a cycle).
- Produces (Tasks 5, 6, 11, 12 rely on these exact names):
  - `type Level string` with `LevelUnset ""`, `LevelMinimal`, `LevelLow`, `LevelMedium`, `LevelHigh`, `LevelXHigh`, `LevelMax`.
  - `func (Level) Supported(supported ...Level) Level` — highest level in `supported` at or below the receiver; a level below everything takes the lowest supported; unset stays unset.
  - `type Source int` with `SourceNone`, `SourceOutputConfig`, `SourceBudget`, `SourceExplicit`.
  - `type Params struct { Level Level; Source Source; Budget int; HasBudget, Disabled, Adaptive bool }`.
  - `func Parse(request map[string]any) Params`.

Behavior contract (D1/D2): `Parse` accepts the proxy's historical spellings (`xhigh`, `extra-high`, `very-high`, `max`, `maximum`, `extreme`, `minimal`, `none|disabled|off|false|0`), keeps `xhigh` and `max` distinct (consumers fold them with `Supported`), accepts `reasoning` as a string or `{"effort": …}`, ignores an unknown spelling instead of keeping it as junk, and reads `output_config.effort` last. During planning it was run against the legacy normalizer over 5,280 input combinations and agreed on effort tier, `disabled`, `hasBudget` and budget value in every case except the unknown-spelling one (intentional).

- [ ] **Step 1: Write the failing tests**

Create `internal/reasoning/reasoning_test.go`:

```go
package reasoning

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	obj := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	tests := []struct {
		name    string
		request map[string]any
		want    Params
	}{
		{"nil request", nil, Params{}},
		{"nothing reasoning-related", obj("model", "m"), Params{}},

		// The explicit proxy extension.
		{"reasoning_effort low", obj("reasoning_effort", "low"), Params{Level: LevelLow, Source: SourceExplicit}},
		{"spelling is case-insensitive", obj("reasoning_effort", "HIGH"), Params{Level: LevelHigh, Source: SourceExplicit}},
		{"reasoning is the fallback key", obj("reasoning", "medium"), Params{Level: LevelMedium, Source: SourceExplicit}},
		{"empty reasoning_effort falls through to reasoning", obj("reasoning_effort", "", "reasoning", "low"), Params{Level: LevelLow, Source: SourceExplicit}},
		{"reasoning object form", obj("reasoning", obj("effort", "high")), Params{Level: LevelHigh, Source: SourceExplicit}},
		{"xhigh stays distinct", obj("reasoning_effort", "xhigh"), Params{Level: LevelXHigh, Source: SourceExplicit}},
		{"extra-high is xhigh", obj("reasoning_effort", "extra-high"), Params{Level: LevelXHigh, Source: SourceExplicit}},
		{"very-high is xhigh", obj("reasoning_effort", "very-high"), Params{Level: LevelXHigh, Source: SourceExplicit}},
		{"max stays distinct", obj("reasoning_effort", "max"), Params{Level: LevelMax, Source: SourceExplicit}},
		{"maximum is max", obj("reasoning_effort", "maximum"), Params{Level: LevelMax, Source: SourceExplicit}},
		{"extreme is max", obj("reasoning_effort", "extreme"), Params{Level: LevelMax, Source: SourceExplicit}},
		{"minimal stays distinct", obj("reasoning_effort", "minimal"), Params{Level: LevelMinimal, Source: SourceExplicit}},
		{"unknown spelling is ignored", obj("reasoning_effort", "turbo"), Params{}},
		{"none turns thinking off", obj("reasoning_effort", "none"), Params{Disabled: true}},
		{"off turns thinking off", obj("reasoning_effort", "off"), Params{Disabled: true}},
		{"numeric zero turns thinking off", obj("reasoning_effort", float64(0)), Params{Disabled: true}},
		{"boolean false turns thinking off", obj("reasoning_effort", false), Params{Disabled: true}},

		// thinking.type
		{"thinking disabled", obj("thinking", obj("type", "disabled")), Params{Disabled: true}},
		{"thinking adaptive", obj("thinking", obj("type", "adaptive")), Params{Adaptive: true}},
		{"thinking type is case-insensitive", obj("thinking", obj("type", "DISABLED")), Params{Disabled: true}},

		// Budgets.
		{"budget_tokens derives a level", obj("thinking", obj("type", "enabled", "budget_tokens", float64(1024))),
			Params{Level: LevelLow, Source: SourceBudget, Budget: 1024, HasBudget: true}},
		{"2048 is still low", obj("thinking", obj("budget_tokens", float64(2048))),
			Params{Level: LevelLow, Source: SourceBudget, Budget: 2048, HasBudget: true}},
		{"2049 is medium", obj("thinking", obj("budget_tokens", float64(2049))),
			Params{Level: LevelMedium, Source: SourceBudget, Budget: 2049, HasBudget: true}},
		{"11999 is medium", obj("thinking", obj("budget_tokens", float64(11999))),
			Params{Level: LevelMedium, Source: SourceBudget, Budget: 11999, HasBudget: true}},
		{"12000 is high", obj("thinking", obj("budget_tokens", float64(12000))),
			Params{Level: LevelHigh, Source: SourceBudget, Budget: 12000, HasBudget: true}},
		{"top-level thinking_budget wins over nested budget_tokens",
			obj("thinking", obj("budget_tokens", float64(1024)), "thinking_budget", float64(40000)),
			Params{Level: LevelHigh, Source: SourceBudget, Budget: 40000, HasBudget: true}},
		{"zero budget switches thinking off", obj("thinking_budget", float64(0)), Params{Budget: 0, HasBudget: true, Disabled: true}},
		{"zero budget under type enabled does not", obj("thinking", obj("type", "enabled", "budget_tokens", float64(0))),
			Params{Budget: 0, HasBudget: true}},
		{"explicit effort keeps its level over a budget",
			obj("reasoning_effort", "low", "thinking_budget", float64(40000)),
			Params{Level: LevelLow, Source: SourceExplicit, Budget: 40000, HasBudget: true}},

		// output_config.effort is ambient and lowest precedence.
		{"output_config effort", obj("output_config", obj("effort", "max")), Params{Level: LevelMax, Source: SourceOutputConfig}},
		{"output_config effort with adaptive thinking",
			obj("thinking", obj("type", "adaptive"), "output_config", obj("effort", "xhigh")),
			Params{Level: LevelXHigh, Source: SourceOutputConfig, Adaptive: true}},
		{"explicit effort beats output_config",
			obj("reasoning_effort", "low", "output_config", obj("effort", "high")),
			Params{Level: LevelLow, Source: SourceExplicit}},
		{"a derived budget level beats output_config",
			obj("thinking", obj("type", "enabled", "budget_tokens", float64(40000)), "output_config", obj("effort", "low")),
			Params{Level: LevelHigh, Source: SourceBudget, Budget: 40000, HasBudget: true}},
		{"disabled wins over output_config", obj("thinking", obj("type", "disabled"), "output_config", obj("effort", "high")),
			Params{Level: LevelHigh, Source: SourceOutputConfig, Disabled: true}},
		{"output_config without effort", obj("output_config", obj("format", obj("type", "json_schema"))), Params{}},
		{"output_config none is not a level", obj("output_config", obj("effort", "none")), Params{}},
		{"unknown output_config spelling is ignored", obj("output_config", obj("effort", "turbo")), Params{}},
		{"output_config of the wrong type is ignored", obj("output_config", "high"), Params{}},
	}
	for _, tc := range tests {
		got := Parse(tc.request)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", tc.name, got, tc.want)
		}
	}
}

func TestSupported(t *testing.T) {
	t.Parallel()
	threeTier := []Level{LevelLow, LevelMedium, LevelHigh}
	opus46 := []Level{LevelLow, LevelMedium, LevelHigh, LevelMax}
	tests := []struct {
		name      string
		level     Level
		supported []Level
		want      Level
	}{
		{"exact match", LevelMedium, threeTier, LevelMedium},
		{"max falls back to high", LevelMax, threeTier, LevelHigh},
		{"xhigh falls back to high", LevelXHigh, threeTier, LevelHigh},
		{"xhigh runs as high on a model with max but not xhigh", LevelXHigh, opus46, LevelHigh},
		{"max is kept where supported", LevelMax, opus46, LevelMax},
		{"minimal rides the lowest supported level", LevelMinimal, threeTier, LevelLow},
		{"an unset level stays unset", LevelUnset, threeTier, LevelUnset},
		{"nothing supported yields unset", LevelHigh, nil, LevelUnset},
		{"order of the supported list does not matter", LevelXHigh, []Level{LevelMax, LevelLow, LevelHigh}, LevelHigh},
		{"an unknown supported entry is skipped", LevelHigh, []Level{"turbo", LevelMedium}, LevelMedium},
	}
	for _, tc := range tests {
		if got := tc.level.Supported(tc.supported...); got != tc.want {
			t.Errorf("%s: %q.Supported(%v) = %q, want %q", tc.name, tc.level, tc.supported, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/reasoning -count=1`
Expected: FAIL — build error `undefined: Params` / `undefined: Parse` / `undefined: Level`.

- [ ] **Step 3: Implement**

Create `internal/reasoning/reasoning.go`:

```go
// Package reasoning normalizes the reasoning-effort signals a client can put
// on an Anthropic /v1/messages request into one value that both the model
// catalog (tier routing) and the Cloud Code request builder (budget emission)
// read, so the two can never disagree about the same request.
//
// The level names are Anthropic's output_config.effort vocabulary (Effort
// doc: low, medium, high, xhigh, max). Minimal exists only because
// OpenAI-shaped clients send it.
package reasoning

import (
	"fmt"
	"strings"
)

// Level is a normalized reasoning-effort level.
type Level string

const (
	LevelUnset   Level = ""
	LevelMinimal Level = "minimal"
	LevelLow     Level = "low"
	LevelMedium  Level = "medium"
	LevelHigh    Level = "high"
	LevelXHigh   Level = "xhigh"
	LevelMax     Level = "max"
)

// order is ascending strength; Supported relies on it.
var order = [...]Level{LevelMinimal, LevelLow, LevelMedium, LevelHigh, LevelXHigh, LevelMax}

func rank(level Level) int {
	for i, candidate := range order {
		if candidate == level {
			return i
		}
	}
	return -1
}

// Supported returns the highest level in supported that is at or below level.
// That is the fallback Claude Code documents for a level a model does not
// accept ("xhigh runs as high on Opus 4.6"). A level below everything the
// model supports takes the lowest supported level; an unset level stays unset.
func (level Level) Supported(supported ...Level) Level {
	want := rank(level)
	if want < 0 || len(supported) == 0 {
		return LevelUnset
	}
	best, bestRank := LevelUnset, -1
	lowest, lowestRank := LevelUnset, len(order)
	for _, candidate := range supported {
		r := rank(candidate)
		if r < 0 {
			continue
		}
		if r <= want && r > bestRank {
			best, bestRank = candidate, r
		}
		if r < lowestRank {
			lowest, lowestRank = candidate, r
		}
	}
	if bestRank >= 0 {
		return best
	}
	return lowest
}

// Source records which request field produced Params.Level. Precedence
// between the fields differs by consumer, so it travels with the value.
type Source int

const (
	SourceNone Source = iota
	// SourceOutputConfig is output_config.effort. Claude Code sends it on
	// every request, so it is ambient: it must not override a tier the client
	// already chose by model name.
	SourceOutputConfig
	// SourceBudget is a level derived from a bare thinking budget.
	SourceBudget
	// SourceExplicit is reasoning_effort or reasoning, the proxy's own
	// request extension; a client sets it on purpose.
	SourceExplicit
)

// Params is the normalized reasoning intent of one request.
type Params struct {
	// Level is the requested effort, LevelUnset when the client expressed none.
	Level  Level
	Source Source
	// Budget is the explicit token budget (thinking.budget_tokens or
	// thinking_budget). It is meaningful only when HasBudget is set.
	Budget    int
	HasBudget bool
	// Disabled means the client turned thinking off.
	Disabled bool
	// Adaptive means thinking.type was "adaptive".
	Adaptive bool
}

// Parse reads every reasoning signal a request can carry. Precedence for the
// level, highest first: reasoning_effort / reasoning, a level derived from an
// explicit budget, output_config.effort.
func Parse(request map[string]any) Params {
	var p Params
	if request == nil {
		return p
	}

	effort := textOf(request["reasoning_effort"])
	if strings.TrimSpace(effort) == "" {
		effort = textOf(request["reasoning"])
	}
	level, off := normalize(effort)
	if level != LevelUnset {
		p.Level, p.Source = level, SourceExplicit
	}
	p.Disabled = off

	thinking, _ := request["thinking"].(map[string]any)
	switch strings.ToLower(textOf(thinking["type"])) {
	case "disabled":
		p.Disabled = true
	case "adaptive":
		p.Adaptive = true
	}
	if budget, ok := intOf(thinking["budget_tokens"]); ok {
		p.Budget, p.HasBudget = budget, true
	}
	if budget, ok := intOf(request["thinking_budget"]); ok {
		p.Budget, p.HasBudget = budget, true
	}

	// A non-positive budget switches thinking off unless the client said
	// "enabled" outright.
	if p.HasBudget && p.Budget <= 0 && !p.Disabled &&
		strings.ToLower(textOf(thinking["type"])) != "enabled" {
		p.Disabled = true
	}

	if p.Level == LevelUnset && !p.Disabled && p.HasBudget && p.Budget > 0 {
		p.Level, p.Source = levelForBudget(p.Budget), SourceBudget
	}

	if p.Level == LevelUnset {
		if config, ok := request["output_config"].(map[string]any); ok {
			if ambient, _ := normalize(textOf(config["effort"])); ambient != LevelUnset {
				p.Level, p.Source = ambient, SourceOutputConfig
			}
		}
	}
	return p
}

// levelForBudget back-maps a bare budget onto a level: at most 2048 tokens is
// low, under 12000 is medium, anything larger is high.
func levelForBudget(budget int) Level {
	switch {
	case budget <= 2048:
		return LevelLow
	case budget < 12000:
		return LevelMedium
	}
	return LevelHigh
}

// normalize maps a client spelling to a Level. off reports an explicit
// "thinking off" spelling. An unknown spelling is ignored, not an error.
func normalize(raw string) (level Level, off bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "disabled", "off", "false", "0":
		return LevelUnset, true
	case "minimal":
		return LevelMinimal, false
	case "low":
		return LevelLow, false
	case "medium":
		return LevelMedium, false
	case "high":
		return LevelHigh, false
	case "xhigh", "extra-high", "very-high":
		return LevelXHigh, false
	case "max", "maximum", "extreme":
		return LevelMax, false
	}
	return LevelUnset, false
}

// textOf renders a scalar request value, or the effort field of the OpenAI
// Responses object form {"effort": "..."}.
func textOf(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case map[string]any:
		return textOf(typed["effort"])
	}
	return fmt.Sprint(value)
}

func intOf(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	}
	return 0, false
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l internal/reasoning && go vet ./internal/reasoning && go test ./internal/reasoning -count=1`
Expected: `gofmt` prints nothing; `ok  antigravity-go-proxy/internal/reasoning`.

- [ ] **Step 5: Commit**

```bash
git add internal/reasoning
git commit -m "feat(reasoning): add one parser for every reasoning-effort signal"
```

---

### Task 5: Tier routing reads `reasoning.Params` (G1 tiers, G2 tiers, G9)

**Files:**
- Modify: `internal/modelcatalog/catalog.go` (imports `:3-9`; delete `ExtractReasoningParams` `:275-347`; edit `ResolveWithRequest` `:349-420`)
- Test: `internal/modelcatalog/catalog_test.go` (append)
- Test: `internal/headroom/stages/shaper/shaper_test.go` (append a guard)

**Interfaces:**
- Consumes: Task 4's `reasoning.Parse`, `reasoning.Params`, `reasoning.SourceOutputConfig`, `(Level).Supported`.
- Produces: `ResolveWithRequest` routes a *bare* flash family ID by `output_config.effort`; a tier named in the model ID wins over ambient effort; `reasoning_effort` and a bare budget still override the name; `ExtractReasoningParams` no longer exists (its only caller was `ResolveWithRequest`; no test referenced it). Task 6 relies on the catalog having chosen the tier before the converter runs.

- [ ] **Step 1: Write the failing test and the guard**

Append to `internal/modelcatalog/catalog_test.go`:

```go
// output_config.effort is ambient: Claude Code sends it on every request, so
// it may choose a tier for a bare family ID but must never override a tier the
// client already chose by model name. reasoning_effort and a bare thinking
// budget are deliberate and still win over the name.
func TestResolveWithRequest_OutputConfigEffortRouting(t *testing.T) {
	t.Parallel()
	catalog, err := Parse([]byte(`{
		"defaultAgentModelId":"gemini-3.8-flash-high",
		"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":[
			"gemini-3.8-flash-high","gemini-3.8-flash-medium","gemini-3.8-flash-low","claude-opus-4-6-thinking"
		]}]}],
		"models":{
			"gemini-3.8-flash-high":{"displayName":"Gemini 3.8 Flash (High)","supportsThinking":true,"thinkingBudget":16000,"maxTokens":1048576,"maxOutputTokens":65536},
			"gemini-3.8-flash-medium":{"displayName":"Gemini 3.8 Flash (Medium)","supportsThinking":true,"thinkingBudget":8000,"maxTokens":1048576,"maxOutputTokens":65536},
			"gemini-3.8-flash-low":{"displayName":"Gemini 3.8 Flash (Low)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":1048576,"maxOutputTokens":65536},
			"claude-opus-4-6-thinking":{"displayName":"Claude Opus 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	effort := func(level string) map[string]any {
		return map[string]any{"output_config": map[string]any{"effort": level}}
	}
	with := func(base map[string]any, key string, value any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		out[key] = value
		return out
	}

	cases := []struct {
		name      string
		requested string
		request   map[string]any
		want      string
	}{
		{"bare family follows ambient low", "gemini-3.8-flash", effort("low"), "gemini-3.8-flash-low"},
		{"bare family follows ambient medium", "gemini-3.8-flash", effort("medium"), "gemini-3.8-flash-medium"},
		{"xhigh has no tier above high", "gemini-3.8-flash", effort("xhigh"), "gemini-3.8-flash-high"},
		{"max has no tier above high", "gemini-3.8-flash", effort("max"), "gemini-3.8-flash-high"},
		{"legacy 3.5 id repoints then follows ambient", "gemini-3.5-flash", effort("low"), "gemini-3.8-flash-low"},
		{"named tier beats ambient effort", "gemini-3.8-flash-low", effort("high"), "gemini-3.8-flash-low"},
		{"named high tier beats ambient low", "gemini-3.8-flash-high", effort("low"), "gemini-3.8-flash-high"},
		{"reasoning_effort overrides the named tier", "gemini-3.8-flash-high", with(effort("high"), "reasoning_effort", "low"), "gemini-3.8-flash-low"},
		{"bare budget overrides the named tier", "gemini-3.8-flash-low", with(effort("low"), "thinking_budget", float64(40000)), "gemini-3.8-flash-high"},
		{"thinking disabled beats ambient effort", "gemini-3.8-flash", with(effort("high"), "thinking", map[string]any{"type": "disabled"}), "gemini-3.8-flash-low"},
		{"unknown ambient spelling is ignored", "gemini-3.8-flash", effort("turbo"), "gemini-3.8-flash-high"},
		{"non-flash model is never retiered", "claude-opus-4-6-thinking", effort("low"), "claude-opus-4-6-thinking"},
	}
	for _, tc := range cases {
		got, err := catalog.ResolveWithRequest(tc.requested, tc.request)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.ID != tc.want {
			t.Errorf("%s: ResolveWithRequest(%q) = %q, want %q", tc.name, tc.requested, got.ID, tc.want)
		}
	}
}
```

Append to `internal/headroom/stages/shaper/shaper_test.go` (a guard, N5):

```go
// Anthropic documents that changing the top-level output_config.effort between
// requests invalidates the prompt cache, and Kimi says the same of its effort
// level. The shaper runs before every gateway, so it must leave the field alone
// even on a mechanical continuation where reasoning_effort would be lowered.
func TestOutputShaper_LeavesOutputConfigEffortAlone(t *testing.T) {
	req := map[string]any{
		"max_tokens":    float64(8192),
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "high"},
		"messages":      []any{toolContinuation(false)},
	}
	reqCtx := &headroom.RequestContext{Request: req}
	if err := (&OutputShaperStage{}).Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req["output_config"].(map[string]any)["effort"]; got != "high" {
		t.Errorf("output_config.effort = %v, want it left at high", got)
	}
	if reqCtx.EffortClamped {
		t.Error("EffortClamped must stay false: nothing in this request is clampable")
	}
}
```

- [ ] **Step 2: Run to verify**

Run: `go test ./internal/modelcatalog -run TestResolveWithRequest_OutputConfigEffortRouting -count=1`
Expected: FAIL with three lines:
`bare family follows ambient low: ResolveWithRequest("gemini-3.8-flash") = "gemini-3.8-flash-high", want "gemini-3.8-flash-low"`, the same for `medium`, and `legacy 3.5 id repoints then follows ambient: … = "gemini-3.8-flash-high", want "gemini-3.8-flash-low"` (the named-tier rows pass today because ambient effort is ignored — they are the guard for the new rule).

Run: `go test ./internal/headroom/stages/shaper -run TestOutputShaper_LeavesOutputConfigEffortAlone -count=1`
Expected: PASS — this is a regression guard and must stay green.

- [ ] **Step 3: Implement**

In `internal/modelcatalog/catalog.go`:

1. Extend the import block to
```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"antigravity-go-proxy/internal/reasoning"
)
```
2. Delete the whole `ExtractReasoningParams` function (`:275-347`) and put this in its place:
```go
// flashTiers are the tiers the Gemini flash families publish. A stronger
// request (xhigh, max) falls back to high; minimal rides the low tier.
var flashTiers = [...]reasoning.Level{reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh}

// effortTier returns the tier suffix ("low", "medium" or "high") the request
// asks the flash families to route to, or "" when the model the client named
// stands. output_config.effort is ambient (Claude Code sends it on every
// request), so it only picks a tier for a family ID that does not already
// name one. reasoning_effort and a bare thinking budget are deliberate, so
// they override the name.
func effortTier(requested string, params reasoning.Params) string {
	if params.Level == reasoning.LevelUnset {
		return ""
	}
	if params.Source == reasoning.SourceOutputConfig && hasTierSuffix(requested) {
		return ""
	}
	return string(params.Level.Supported(flashTiers[:]...))
}

// tierSuffixes end the per-tier flash IDs (gemini-3.8-flash-low, ...).
var tierSuffixes = [...]string{"-high", "-medium", "-low", "-extra-low"}

func hasTierSuffix(requested string) bool {
	id := strings.ToLower(Strip1mSuffix(requested))
	for _, suffix := range tierSuffixes {
		if strings.HasSuffix(id, suffix) {
			return true
		}
	}
	return false
}
```
3. Replace the whole `ResolveWithRequest` function with (the disabled-thinking branch and the 3.5 repoint are unchanged; only the effort source differs):
```go
func (catalog *Catalog) ResolveWithRequest(requested string, request map[string]any) (Model, error) {
	model, err := catalog.Resolve(requested)
	if err != nil {
		return Model{}, err
	}
	if request == nil {
		return model, nil
	}

	params := reasoning.Parse(request)

	if params.Disabled {
		if isGemini37Flash(model.ID) || isGemini37Flash(requested) {
			if variant, err := catalog.Resolve("gemini-3.7-flash-low"); err == nil {
				return variant, nil
			}
			model.ThinkingLevel = "LOW"
			model.SupportsThinking = true
			return model, nil
		}
		if isGemini38Flash(model.ID) || isGemini38Flash(requested) {
			if variant, err := catalog.Resolve("gemini-3.8-flash-low"); err == nil {
				return variant, nil
			}
			model.ThinkingLevel = "LOW"
			model.SupportsThinking = true
			return model, nil
		}
		model.SupportsThinking = false
		model.ThinkingBudget = 0
		return model, nil
	}

	if tier := effortTier(requested, params); tier != "" {
		targetID := ""
		lowerReq := strings.ToLower(strings.TrimSpace(requested))
		switch {
		case strings.HasPrefix(lowerReq, "gemini-3.8-flash"):
			targetID = "gemini-3.8-flash-" + tier
		case strings.HasPrefix(lowerReq, "gemini-3.7-flash"):
			targetID = "gemini-3.7-flash-" + tier
		case strings.HasPrefix(lowerReq, "gemini-3.6-flash"):
			targetID = "gemini-3.6-flash-" + tier
		case strings.HasPrefix(lowerReq, "gemini-3.5-flash"):
			// Legacy 3.5 IDs repoint to the 3.8 family (user decision
			// 2026-09-03); fall back to a real 3.5 tier only when this
			// account has no 3.8 at all.
			targetID = "gemini-3.8-flash-" + tier
		}
		if targetID != "" {
			if variant, err := catalog.Resolve(targetID); err == nil {
				if params.HasBudget && params.Budget > 0 {
					variant.ThinkingBudget = params.Budget
				}
				return variant, nil
			}
			if strings.HasPrefix(lowerReq, "gemini-3.5-flash") {
				if variant, err := catalog.Resolve("gemini-3.5-flash-" + tier); err == nil {
					if params.HasBudget && params.Budget > 0 {
						variant.ThinkingBudget = params.Budget
					}
					return variant, nil
				}
			}
		}
	}

	if params.HasBudget && params.Budget > 0 {
		model.ThinkingBudget = params.Budget
	}
	return model, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l internal/modelcatalog && go vet ./internal/modelcatalog && go test ./internal/modelcatalog ./internal/reasoning ./internal/headroom/... -count=1`
Expected: `ok` for all. The pre-existing tests (`TestSynthetic37AndReasoningResolution` with `xhigh`/`max` → `-high`, the 3.8/3.5 repointing tests) pass unchanged: legacy behavior is preserved.

- [ ] **Step 5: Commit**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go internal/headroom/stages/shaper/shaper_test.go
git commit -m "fix(modelcatalog): route bare flash IDs by output_config.effort without overriding a named tier"
```

---

### Task 6: Converter budget policy on `reasoning.Params` (G1 budgets, G2, G4, G9, G10, G8 guard)

**Files:**
- Create: `internal/format/thinkingconfig_test.go`
- Create: `internal/format/thinkingconfig.go`
- Modify: `internal/format/request.go` (imports `:3-6`; thinking block `:155-249`)
- Modify: `internal/format/model.go` (delete `clampGeminiThinkingBudget` `:99-112`)
- Test: `internal/accounts/retry_test.go` (append the dispatcher-level test)

**Interfaces:**
- Consumes: Task 4's `reasoning.Params`/`Supported`; Task 5 (the catalog has already chosen the tier/variant and `ModelOptions`).
- Produces in `package format`: constants `effortBudgetLow=1024`, `effortBudgetMedium=8000`, `effortBudgetHighClaude=32000`, `effortBudgetHighGemini=16000`, `claudeKeyIncludeThoughts`, `claudeKeyThinkingBudget`; `budgetLevels`; `effortBudget(family ModelFamily, level reasoning.Level) int`; `thinkingBudget(params reasoning.Params, family ModelFamily, fallback, minimum int) int`; `geminiBudgetCeiling(model string) int`. Task 7 adds `reconcileClaudeBudget`; Tasks 12a/12b/12d edit these names.

> The 1024/8000/32000/16000 numbers are the proxy's *historical* `reasoning_effort` table, unchanged. Gate GM (Task 12e) replaces them with agy's own numbers if the capture shows any. **D3 needs your sign-off before this task merges** (see Decisions).

- [ ] **Step 1: Write the failing tests**

Create `internal/format/thinkingconfig_test.go`:

```go
package format

import (
	"strings"
	"testing"
)

// claudeOpts mirrors a live Cloud Code entry for Claude Opus 4.6 (Thinking):
// catalog default budget 1024, output cap 64000.
var claudeOpts = ModelOptions{SupportsThinking: true, ThinkingBudget: 1024, MaxOutputTokens: 64000}

func convertWith(request map[string]any, options *ModelOptions) map[string]any {
	request["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
	if options == nil {
		return asMap(ConvertAnthropicToGoogle(request, NewSignatureCache())["generationConfig"])
	}
	return asMap(ConvertAnthropicToGoogleWithModel(request, NewSignatureCache(), *options)["generationConfig"])
}

func claudeRequestFor(model string, extra map[string]any) map[string]any {
	request := map[string]any{"model": model, "max_tokens": float64(64000)}
	for k, v := range extra {
		request[k] = v
	}
	return request
}

func claudeRequest(extra map[string]any) map[string]any {
	return claudeRequestFor("claude-opus-4-6-thinking", extra)
}

func budgetOf(t *testing.T, generation map[string]any) int {
	t.Helper()
	config := asMap(generation["thinkingConfig"])
	if config == nil {
		t.Fatalf("no thinkingConfig in %#v", generation)
	}
	for _, key := range []string{"thinking_budget", "thinkingBudget"} {
		if value, ok := config[key]; ok {
			return intValue(value, -1)
		}
	}
	t.Fatalf("thinkingConfig carries no budget: %#v", config)
	return 0
}

func effortConfig(level string) map[string]any { return map[string]any{"effort": level} }

// Claude Code sends output_config.effort on every request; on a budget-style
// model it is the only dial the client has, so it must move the budget.
func TestOutputConfigEffortSetsBudgetOnBudgetStyleModels(t *testing.T) {
	t.Parallel()
	adaptive := map[string]any{"type": "adaptive"}
	tests := []struct {
		name    string
		model   string
		options ModelOptions
		extra   map[string]any
		want    int
	}{
		{"claude: low", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("low")}, 1024},
		{"claude: medium", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("medium")}, 8000},
		{"claude: high", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("high")}, 32000},
		{"claude: xhigh falls back to high", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("xhigh")}, 32000},
		{"claude: max falls back to high", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("max")}, 32000},
		{"claude: no effort keeps the catalog default", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive}, 1024},
		{"claude: explicit budget beats ambient effort", "claude-opus-4-6-thinking", claudeOpts,
			map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(20000)}, "output_config": effortConfig("low")}, 20000},
		{"claude: reasoning_effort beats an explicit budget", "claude-opus-4-6-thinking", claudeOpts,
			map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(20000)}, "reasoning_effort": "low"}, 1024},
		{"claude: top-level thinking_budget is honored", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking_budget": float64(5000)}, 5000},
		{"gemini budget model: medium", "gemini-3.1-pro-high", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("medium")}, 8000},
		{"gemini budget model: max falls back to high", "gemini-3.1-pro-high", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("max")}, 16000},
		{"gpt-oss: low", "gpt-oss-120b-medium", ModelOptions{SupportsThinking: true, ThinkingBudget: 8192, MaxOutputTokens: 32768},
			map[string]any{"output_config": effortConfig("low")}, 1024},
	}
	for _, tc := range tests {
		options := tc.options
		generation := convertWith(claudeRequestFor(tc.model, tc.extra), &options)
		if got := budgetOf(t, generation); got != tc.want {
			t.Errorf("%s: thinking budget = %d, want %d (%#v)", tc.name, got, tc.want, generation)
		}
	}
}

// A tiered model carries its level in the catalog; effort selects the tier in
// the catalog, so the converter must not second-guess it from the request.
func TestOutputConfigEffortDoesNotChangeTieredLevel(t *testing.T) {
	t.Parallel()
	options := ModelOptions{SupportsThinking: true, ThinkingLevel: "LOW", MaxOutputTokens: 65536}
	generation := convertWith(map[string]any{
		"model": "gemini-3.8-flash-low", "max_tokens": float64(1024), "output_config": effortConfig("max"),
	}, &options)
	config := asMap(generation["thinkingConfig"])
	if config["thinkingLevel"] != "LOW" {
		t.Fatalf("thinkingConfig = %#v", config)
	}
	if _, hasBudget := config["thinkingBudget"]; hasBudget {
		t.Fatalf("a thinkingLevel config must not also carry a budget: %#v", config)
	}
}

// The legacy single-credential path has no catalog entry. It used to overwrite
// every effort-derived budget with the clamped budget_tokens, so effort and a
// top-level thinking_budget were silently ignored for Gemini.
func TestLegacyGeminiPathHonorsEffortAndTopLevelBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		extra map[string]any
		want  int
	}{
		{"reasoning_effort low", map[string]any{"reasoning_effort": "low"}, 1024},
		{"output_config medium", map[string]any{"output_config": effortConfig("medium")}, 8000},
		{"top-level thinking_budget", map[string]any{"thinking_budget": float64(5000)}, 5000},
		{"no signal uses the Gemini default", map[string]any{}, DefaultGeminiThinkBudget},
	}
	for _, tc := range tests {
		generation := convertWith(claudeRequestFor("gemini-3.1-pro-high", tc.extra), nil)
		if got := budgetOf(t, generation); got != tc.want {
			t.Errorf("%s: budget = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Google documents different thinkingBudget ceilings per 2.5 model, and none
// for the later families.
func TestGeminiBudgetCeilingByModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model  string
		budget float64
		want   int
	}{
		{"gemini-2.5-pro-thinking", 30000, 30000},
		{"gemini-2.5-pro-thinking", 40000, 32768},
		{"gemini-2.5-flash-thinking", 20000, 20000},
		{"gemini-2.5-flash-thinking", 30000, 24576},
		{"gemini-3.1-pro-high", 100000, 100000},
		{"gemini-3.1-pro-high", 200000, 128000},
	}
	for _, tc := range tests {
		generation := convertWith(claudeRequestFor(tc.model, map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": tc.budget},
		}), nil)
		if got := budgetOf(t, generation); got != tc.want {
			t.Errorf("%s budget_tokens=%v: budget = %d, want %d", tc.model, tc.budget, got, tc.want)
		}
	}
}

// A non-positive bare budget turns thinking off in the catalog's tier routing;
// the converter used to disagree and emit the default budget anyway.
func TestZeroThinkingBudgetTurnsThinkingOffOnBudgetStyleModels(t *testing.T) {
	t.Parallel()
	options := claudeOpts
	generation := convertWith(claudeRequest(map[string]any{"thinking_budget": float64(0)}), &options)
	if _, has := generation["thinkingConfig"]; has {
		t.Fatalf("thinkingConfig = %#v, want none", generation["thinkingConfig"])
	}
}

// Real Claude Code requests carry thinking.display and, after a model switch,
// redacted_thinking blocks from an Anthropic-native backend. Neither may leak
// upstream: Cloud Code cannot replay another backend's encrypted payload.
func TestRedactedThinkingFromAnotherBackendDoesNotReachCloudCode(t *testing.T) {
	t.Parallel()
	request := map[string]any{
		"model": "claude-opus-4-6-thinking", "max_tokens": float64(4096),
		"thinking": map[string]any{"type": "adaptive", "display": "summarized"},
		"messages": []any{
			map[string]any{"role": "user", "content": "run it"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "redacted_thinking", "data": strings.Repeat("R", 200)},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			}},
		},
	}
	options := claudeOpts
	converted := ConvertAnthropicToGoogleWithModel(request, NewSignatureCache(), options)
	encoded := stringValue(converted)
	for _, forbidden := range []string{"redacted_thinking", strings.Repeat("R", 50)} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("converted request leaks %q: %s", forbidden, encoded)
		}
	}
	if asMap(asMap(converted["generationConfig"])["thinkingConfig"]) == nil {
		t.Fatalf("thinking must stay enabled: %#v", converted["generationConfig"])
	}
}

// The body fields real Claude Code sends (.reference/claude-code-headers-20260923.txt:
// context_management, diagnostics, max_tokens, messages, metadata, model,
// output_config, stream, system, thinking, tools; thinking carries type+display,
// output_config carries effort; no sampling fields) must convert to a
// generationConfig that holds only what Cloud Code understands.
func TestClaudeCodeCapturedBodyShapeConvertsToACleanGenerationConfig(t *testing.T) {
	t.Parallel()
	request := map[string]any{
		"model":              "claude-opus-4-6-thinking",
		"max_tokens":         float64(64000),
		"stream":             true,
		"thinking":           map[string]any{"type": "adaptive", "display": "summarized"},
		"output_config":      map[string]any{"effort": "high"},
		"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all"}}},
		"diagnostics":        map[string]any{"client": "claude-code"},
		"metadata":           map[string]any{"user_id": "{}"},
		"messages":           []any{map[string]any{"role": "user", "content": "hi"}},
	}
	options := claudeOpts
	converted := ConvertAnthropicToGoogleWithModel(request, NewSignatureCache(), options)
	generation := asMap(converted["generationConfig"])
	if len(generation) != 2 || intValue(generation["maxOutputTokens"], 0) != 64000 || budgetOf(t, generation) != 32000 {
		t.Fatalf("generationConfig = %#v, want only maxOutputTokens 64000 and thinking_budget 32000", generation)
	}
	encoded := stringValue(converted)
	for _, leaked := range []string{"output_config", "context_management", "diagnostics", "display", "clear_thinking"} {
		if strings.Contains(encoded, leaked) {
			t.Errorf("Cloud Code request leaks the client-only field %q: %s", leaked, encoded)
		}
	}
}
```

Also append the dispatcher-level integration test to `internal/accounts/retry_test.go`; it uses the helpers its neighbor `TestDispatcherUsesAgyAgentRouteAndLiveOutputLimit` already uses (`testAccount`, `scriptedClient`, `newTestDispatcher`, `staticResolver`, `testRequest`):

```go
// Claude Code sends output_config.effort on every request. Through the real
// dispatcher it must pick the flash tier for a bare family ID and set the
// budget on the Claude route, while leaving a tier named in the model ID alone.
func TestDispatcherAppliesOutputConfigEffort(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC)
	account := testAccount("effort@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	modelsBody := []byte(`{
		"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-high","gemini-3.8-flash-medium","gemini-3.8-flash-low","claude-opus-4-6-thinking"]}]}],
		"models":{
			"gemini-3.8-flash-high":{"displayName":"Gemini 3.8 Flash (High)","supportsThinking":true,"thinkingBudget":16000,"maxTokens":1048576,"maxOutputTokens":65536},
			"gemini-3.8-flash-medium":{"displayName":"Gemini 3.8 Flash (Medium)","supportsThinking":true,"thinkingBudget":8000,"maxTokens":1048576,"maxOutputTokens":65536},
			"gemini-3.8-flash-low":{"displayName":"Gemini 3.8 Flash (Low)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":1048576,"maxOutputTokens":65536},
			"claude-opus-4-6-thinking":{"displayName":"Claude Opus 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}
		}
	}`)
	events := [][]byte{[]byte(`{}`)}
	client := &scriptedClient{modelsBody: modelsBody, results: []scriptedResult{{events: events}, {events: events}, {events: events}, {events: events}}}
	dispatcher := newTestDispatcher(t, manager, &staticResolver{tokens: map[string]string{account.Email: "token"}}, map[string]*scriptedClient{"token": client}, now, func(context.Context, time.Duration) error { return nil })

	send := func(model, effort string, thinking map[string]any) map[string]any {
		t.Helper()
		request := testRequest()
		request["model"] = model
		request["max_tokens"] = float64(64000)
		request["output_config"] = map[string]any{"effort": effort}
		if thinking != nil {
			request["thinking"] = thinking
		}
		if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		return client.payload
	}

	if got := send("gemini-3.8-flash", "low", nil)["model"]; got != "gemini-3.8-flash-low" {
		t.Errorf("bare family with ambient low routed to %v, want gemini-3.8-flash-low", got)
	}
	if got := send("gemini-3.8-flash-high", "low", nil)["model"]; got != "gemini-3.8-flash-high" {
		t.Errorf("a named tier must beat ambient effort, routed to %v", got)
	}

	payload := send("claude-opus-4-6-thinking", "high", map[string]any{"type": "adaptive", "display": "summarized"})
	thinking := payload["request"].(map[string]any)["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if thinking["thinking_budget"] != 32000 {
		t.Errorf("claude route with ambient high: thinkingConfig=%#v, want thinking_budget 32000", thinking)
	}
	payload = send("claude-opus-4-6-thinking", "low", map[string]any{"type": "adaptive"})
	thinking = payload["request"].(map[string]any)["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if thinking["thinking_budget"] != 1024 {
		t.Errorf("claude route with ambient low: thinkingConfig=%#v, want thinking_budget 1024", thinking)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/format ./internal/accounts -count=1`
Expected: FAIL in six tests (everything compiles against the current code): `TestOutputConfigEffortSetsBudgetOnBudgetStyleModels` (e.g. `claude: high: thinking budget = 1024, want 32000`, `gemini budget model: medium: … = 10001, want 8000`, `gpt-oss: low: … = 8192, want 1024`), `TestLegacyGeminiPathHonorsEffortAndTopLevelBudget` (`reasoning_effort low: budget = 16000, want 1024`), `TestGeminiBudgetCeilingByModel` (`gemini-2.5-pro-thinking budget_tokens=30000: budget = 24576, want 30000`), `TestZeroThinkingBudgetTurnsThinkingOffOnBudgetStyleModels` (`thinkingConfig = …thinking_budget:1024…, want none`) and `TestClaudeCodeCapturedBodyShapeConvertsToACleanGenerationConfig`, and in `./internal/accounts` `TestDispatcherAppliesOutputConfigEffort` (`claude route with ambient high: thinkingConfig=…thinking_budget:1024…, want thinking_budget 32000`; its tier assertions already pass because Task 5 is in). `TestOutputConfigEffortDoesNotChangeTieredLevel` and `TestRedactedThinkingFromAnotherBackendDoesNotReachCloudCode` already pass: they are guards (G8 is not a bug).

- [ ] **Step 3: Write the policy file**

Create `internal/format/thinkingconfig.go`:

```go
package format

import (
	"strings"

	"antigravity-go-proxy/internal/reasoning"
)

// The effort-to-budget table is the proxy's own translation choice. Cloud
// Code's ThinkingConfig carries a token budget or a tier and never an effort
// level, so an Anthropic output_config.effort has to become a number here.
const (
	effortBudgetLow        = 1024
	effortBudgetMedium     = 8000
	effortBudgetHighClaude = 32000
	effortBudgetHighGemini = 16000
)

// Spelling of the Claude route's thinkingConfig keys. The proxy has always sent
// snake_case here while every other branch sends the proto JSON (camelCase)
// names. Whether the Claude route may use camelCase too is decided by probe GA
// and the agy capture (plan Task 12a); keep the two keys together.
const (
	claudeKeyIncludeThoughts = "include_thoughts"
	claudeKeyThinkingBudget  = "thinking_budget"
)

// budgetLevels are the effort levels the budget table distinguishes. A
// stronger request falls back to the highest level listed, the rule Claude
// Code documents for a level a model does not accept.
var budgetLevels = [...]reasoning.Level{reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh}

// effortBudget returns the thinking budget for an effort level, 0 for none.
func effortBudget(family ModelFamily, level reasoning.Level) int {
	switch level.Supported(budgetLevels[:]...) {
	case reasoning.LevelLow:
		return effortBudgetLow
	case reasoning.LevelMedium:
		return effortBudgetMedium
	case reasoning.LevelHigh:
		if family == FamilyClaude {
			return effortBudgetHighClaude
		}
		return effortBudgetHighGemini
	}
	return 0
}

// thinkingBudget picks the token budget for a budget-style model. Precedence:
// an explicit reasoning_effort, then an explicit budget, then the ambient
// output_config.effort, then fallback. The result honors the catalog minimum.
func thinkingBudget(params reasoning.Params, family ModelFamily, fallback, minimum int) int {
	budget := 0
	switch {
	case params.Source == reasoning.SourceExplicit:
		budget = effortBudget(family, params.Level)
	case params.HasBudget:
		budget = params.Budget
	case params.Level != reasoning.LevelUnset:
		budget = effortBudget(family, params.Level)
	}
	if budget <= 0 {
		budget = fallback
	}
	if minimum > 0 && budget < minimum {
		budget = minimum
	}
	return budget
}

// geminiBudgetCeiling is the largest thinkingBudget Google documents for the
// Gemini 2.5 series (2.5 Pro 128-32768, 2.5 Flash and Flash-Lite up to 24576).
// Other families have no documented budget range, so they keep the proxy's
// historical 128000 ceiling. It applies only when no live catalog entry
// describes the model.
func geminiBudgetCeiling(model string) int {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "gemini-2.5-pro"):
		return 32768
	case strings.Contains(lower, "gemini-2.5"):
		return 24576
	}
	return 128000
}
```

- [ ] **Step 4: Rewire the converter**

In `internal/format/request.go`:

1. Replace the import block (`:3-6`) with
```go
import (
	"regexp"

	"antigravity-go-proxy/internal/reasoning"
)
```
(`strings` was used only by the normalization block being removed.)

2. Replace lines `:155-249` (from `thinking := asMap(request["thinking"])` through the closing `}` of the `else if isThinking {` branch) with:
```go
	params := reasoning.Parse(request)

	thinkingLevel := ""
	if options != nil {
		thinkingLevel = options.ThinkingLevel
	}
	if thinkingLevel != "" {
		if params.Disabled {
			thinkingLevel = "LOW"
		}
		generation["thinkingConfig"] = map[string]any{
			"includeThoughts": true,
			"thinkingLevel":   thinkingLevel,
		}
	} else if params.Disabled {
		delete(generation, "thinkingConfig")
	} else if isThinking && family == FamilyClaude {
		if defaultThinkingBudget <= 0 {
			defaultThinkingBudget = DefaultClaudeThinkBudget
		}
		budget := thinkingBudget(params, family, defaultThinkingBudget, minThinkingBudget)
		generation["thinkingConfig"] = map[string]any{claudeKeyIncludeThoughts: true, claudeKeyThinkingBudget: budget}
		maximum := intValue(generation["maxOutputTokens"], 0)
		if maximum > 0 && maximum <= budget {
			generation["maxOutputTokens"] = budget + 8192
		}
	} else if isThinking {
		fallback := defaultThinkingBudget
		if fallback <= 0 {
			fallback = DefaultGeminiThinkBudget
		}
		budget := thinkingBudget(params, family, fallback, minThinkingBudget)
		if family == FamilyGemini && options == nil {
			// No live catalog entry describes the model, so cap the budget at
			// the ceiling Google documents for its series.
			budget = min(budget, geminiBudgetCeiling(model))
		}
		generation["thinkingConfig"] = map[string]any{
			"includeThoughts": true,
			"thinkingBudget":  budget,
		}
	}
```

In `internal/format/model.go`, delete `clampGeminiThinkingBudget` (`:99-112`, the whole function). `strings` stays imported (used by `getModelFamilyInfo`).

- [ ] **Step 5: Run to verify it passes**

Run: `gofmt -l internal/format internal/accounts && go vet ./internal/format ./internal/accounts && go test ./internal/format ./internal/accounts ./internal/modelcatalog ./internal/reasoning -count=1`
Expected: `ok` ×4, including every pre-existing format and dispatcher test (`TestRequestConversionMatchesParityFixture`, `TestLiveModelOptionsCapOutputAndApplyDefaultThinkingBudget`, …).

- [ ] **Step 6: Full gate and commit**

Run: `go vet ./... && go test -race ./... 2>&1 | tail -15`
Expected: every package `ok`.

```bash
git add internal/format/thinkingconfig.go internal/format/thinkingconfig_test.go internal/format/request.go internal/format/model.go internal/accounts/retry_test.go
git commit -m "fix(format): honor output_config.effort and one effort policy on every Cloud Code path"
```

---

### Task 7: Output ceilings and the budget < max invariant (G6, G13)

**Files:**
- Modify: `internal/format/thinkingconfig.go` (append `thinkingResponseHeadroom`, `reconcileClaudeBudget`)
- Modify: `internal/format/request.go` (the Claude branch, and the two ceiling `if` statements just before `return result`)
- Test: `internal/format/thinkingconfig_test.go` (append)

**Interfaces:**
- Consumes: Task 6's `claudeKeyThinkingBudget`, `thinkingBudget`, `asMap`, `intValue`; the converter's local `maxOutputTokens` (the live catalog cap).
- Produces: `thinkingResponseHeadroom = 8192`; `reconcileClaudeBudget(generation map[string]any, budget, limit int)`; the Gemini fixed ceiling (`GeminiMaxOutputTokens`) applies only when no live limit is known. Task 12d relies on `reconcileClaudeBudget` keeping any large budget under the cap.

Gates: **GE** (the 65536 probe) and **GM** (what agy itself sends as `maxOutputTokens`) select between the default and the alternative at Step 3 — the project's rule is to match agy, so an upstream that merely *accepts* more than agy sends is not enough; **GD's control** (`budget == max` rejected?) decides whether the shrink branch exists.

- [ ] **Step 1: Write the failing tests**

Append to `internal/format/thinkingconfig_test.go`:

```go
// Anthropic rejects budget_tokens >= max_tokens, and Google counts thought
// tokens against maxOutputTokens, so the budget must stay below the final
// maxOutputTokens however the model cap and the client's value interact.
func TestThinkingBudgetStaysBelowMaxOutputTokens(t *testing.T) {
	t.Parallel()
	enabled := func(budget float64) map[string]any {
		return map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	tests := []struct {
		name       string
		maxTokens  float64
		budget     float64
		cap        int
		wantMax    int
		wantBudget int
	}{
		{"max_tokens above budget is untouched", 64000, 20000, 64000, 64000, 20000},
		{"max_tokens at the budget is raised by the headroom", 16000, 16000, 64000, 24192, 16000},
		{"max_tokens below the budget is raised by the headroom", 16000, 20000, 64000, 28192, 20000},
		{"the cap stops the raise and the budget shrinks", 80000, 70000, 64000, 64000, 55808},
		{"a small cap halves the room instead", 5000, 10000, 8192, 8192, 4096},
		{"no cap: only the raise applies", 16000, 20000, 0, 28192, 20000},
	}
	for _, tc := range tests {
		options := ModelOptions{SupportsThinking: true, ThinkingBudget: 1024, MaxOutputTokens: tc.cap}
		generation := convertWith(map[string]any{
			"model": "claude-opus-4-6-thinking", "max_tokens": tc.maxTokens, "thinking": enabled(tc.budget),
		}, &options)
		gotMax, gotBudget := intValue(generation["maxOutputTokens"], 0), budgetOf(t, generation)
		if gotMax != tc.wantMax || gotBudget != tc.wantBudget {
			t.Errorf("%s: maxOutputTokens=%d budget=%d, want %d and %d", tc.name, gotMax, gotBudget, tc.wantMax, tc.wantBudget)
		}
		if gotBudget >= gotMax {
			t.Errorf("%s: budget %d is not below maxOutputTokens %d", tc.name, gotBudget, gotMax)
		}
	}
}

// /v1/models advertises the catalog's max output, so a client that sends
// exactly that must not be cut to a smaller proxy-side ceiling.
func TestGeminiOutputIsCappedByTheLiveLimitNotAFixedCeiling(t *testing.T) {
	t.Parallel()
	options := ModelOptions{SupportsThinking: true, ThinkingLevel: "HIGH", MaxOutputTokens: 65536}
	tests := []struct {
		name      string
		maxTokens float64
		options   *ModelOptions
		want      int
	}{
		{"the advertised limit goes through", 65536, &options, 65536},
		{"above the limit is cut to the limit", 100000, &options, 65536},
		{"below the limit is untouched", 4096, &options, 4096},
		{"no live limit falls back to the fixed ceiling", 65536, nil, GeminiMaxOutputTokens},
		{"an entry without a limit falls back to the fixed ceiling", 65536, &ModelOptions{SupportsThinking: true, ThinkingLevel: "HIGH"}, GeminiMaxOutputTokens},
	}
	for _, tc := range tests {
		generation := convertWith(map[string]any{"model": "gemini-3.8-flash-high", "max_tokens": tc.maxTokens}, tc.options)
		if got := intValue(generation["maxOutputTokens"], 0); got != tc.want {
			t.Errorf("%s: maxOutputTokens = %d, want %d", tc.name, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/format -run 'TestThinkingBudgetStaysBelowMaxOutputTokens|TestGeminiOutputIsCapped' -count=1`
Expected: FAIL —
`the advertised limit goes through: maxOutputTokens = 16384, want 65536`;
`the cap stops the raise and the budget shrinks: maxOutputTokens=64000 budget=70000, want 64000 and 55808`;
`a small cap halves the room instead: maxOutputTokens=8192 budget=10000, want 8192 and 4096`
(plus the matching `budget … is not below maxOutputTokens …` lines). The first is G6, the other two are G13.

- [ ] **Step 3: Implement**

Append to `internal/format/thinkingconfig.go`:

```go
// thinkingResponseHeadroom is the room kept for the answer above a thinking
// budget: it is added when max_tokens is too small for the budget, and kept
// when the model cap forces the budget down.
const thinkingResponseHeadroom = 8192

// reconcileClaudeBudget keeps thinking_budget strictly below maxOutputTokens,
// the rule Anthropic enforces as budget_tokens < max_tokens. It runs after the
// model cap is applied so the cap cannot undo it. A maxOutputTokens at or
// below the budget is raised to budget plus headroom first; when the cap
// forbids that, the budget shrinks instead so the answer keeps its room.
func reconcileClaudeBudget(generation map[string]any, budget, limit int) {
	config := asMap(generation["thinkingConfig"])
	maximum := intValue(generation["maxOutputTokens"], 0)
	if config == nil || maximum <= 0 || maximum > budget {
		return
	}
	maximum = budget + thinkingResponseHeadroom
	if limit > 0 && maximum > limit {
		maximum = limit
	}
	generation["maxOutputTokens"] = maximum
	if maximum <= budget {
		config[claudeKeyThinkingBudget] = maximum - min(thinkingResponseHeadroom, maximum/2)
	}
}
```

In `internal/format/request.go`:

1. Declare `claudeBudget := 0` on the line before `if thinkingLevel != "" {`.
2. In the Claude branch replace
```go
		budget := thinkingBudget(params, family, defaultThinkingBudget, minThinkingBudget)
		generation["thinkingConfig"] = map[string]any{claudeKeyIncludeThoughts: true, claudeKeyThinkingBudget: budget}
		maximum := intValue(generation["maxOutputTokens"], 0)
		if maximum > 0 && maximum <= budget {
			generation["maxOutputTokens"] = budget + 8192
		}
```
with
```go
		claudeBudget = thinkingBudget(params, family, defaultThinkingBudget, minThinkingBudget)
		generation["thinkingConfig"] = map[string]any{claudeKeyIncludeThoughts: true, claudeKeyThinkingBudget: claudeBudget}
```
3. Replace the ceilings block (the two `if` statements before `return result`) with:
```go
	// A live catalog limit is authoritative for the model it describes. The
	// fixed Gemini ceiling is only the fallback when none is known.
	if family == FamilyGemini && maxOutputTokens <= 0 && intValue(generation["maxOutputTokens"], 0) > GeminiMaxOutputTokens {
		generation["maxOutputTokens"] = GeminiMaxOutputTokens
	}
	if maxOutputTokens > 0 && intValue(generation["maxOutputTokens"], 0) > maxOutputTokens {
		generation["maxOutputTokens"] = maxOutputTokens
	}
	if claudeBudget > 0 {
		reconcileClaudeBudget(generation, claudeBudget, maxOutputTokens)
	}
```

**Gate branches (apply before Step 4):**
- **GD control accepted** (Cloud Code does *not* reject `budget == maxOutputTokens`): the shrink is changing client intent for no upstream benefit. Delete the final `if maximum <= budget { … }` block from `reconcileClaudeBudget`, delete the two shrink rows ("the cap stops the raise…", "a small cap halves…") and the `budget … is not below` assertion from the test, and keep the raise.
- **GE rejected, or GM shows agy sends a fixed ceiling below the catalog limit for Gemini routes** (Cloud Code refuses 65536, or agy never asks for more than e.g. 16384): keep the original unconditional Gemini clamp (`if family == FamilyGemini && intValue(…) > GeminiMaxOutputTokens`), delete the first test table's first two rows, and make discovery advertise what will be sent — in `internal/api/server.go` `models()`, replace `"max_output_tokens": details.MaxOutputTokens` with a local that caps Gemini-family entries:
```go
		maxOutput := details.MaxOutputTokens
		if proxyformat.GetModelFamily(details.ID) == proxyformat.FamilyGemini && maxOutput > proxyformat.GeminiMaxOutputTokens {
			// The converter caps Gemini output at this ceiling; advertise what will be sent.
			maxOutput = proxyformat.GeminiMaxOutputTokens
		}
```
  and use `maxOutput` in the entry map, with a discovery test asserting a Gemini entry never advertises more than `GeminiMaxOutputTokens`.

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l internal/format && go vet ./internal/format && go test ./internal/format ./internal/accounts ./internal/api -count=1`
Expected: `ok` ×3. The pre-existing `TestLiveModelOptionsCapOutputAndApplyDefaultThinkingBudget` still passes (cap 64000, budget 1024).

- [ ] **Step 5: Commit**

```bash
git add internal/format/thinkingconfig.go internal/format/thinkingconfig_test.go internal/format/request.go
git commit -m "fix(format): let the live catalog limit cap Gemini output and keep thinking budget below max tokens"
```

---

### Task 8: Discovery output fallback below the context window (G11)

**Files:**
- Modify: `internal/api/server.go` (`defaultDiscoveryMaxOutputTokens`, `:555-559`)
- Test: `internal/api/models_discovery_test.go` (append)

**Interfaces:**
- Consumes: the three existing fallback sites in `internal/api/discovery.go` (`maxOutput = contextLen; if maxOutput > defaultDiscoveryMaxOutputTokens { … }`) — they read the constant by name and need no edit.
- Produces: `defaultDiscoveryMaxOutputTokens = 32768`. 32768 is the `max_tokens` default Kimi documents for K2.x (`kimi-k2-7-code-quickstart`, "Parameters Differences") and the value `zen.DefaultMaxOutputTokens` already fills in, so every gateway fallback advertises one conservative number.

The two existing guards (`TestOpenRouterModels_…`, `TestKimiModels_MaxOutputFallbackDoesNotEqualContextWindow`) use 1M-context fixtures, which pass for any constant below 1M. The shape that actually advertised `max_output_tokens == context_window` is an entry with *no* limits (both fall back to 200000).

- [ ] **Step 1: Write the failing test**

Append to `internal/api/models_discovery_test.go`:

```go
// The two guards above use 1M-context fixtures, which pass whatever the
// fallback is as long as it sits under 1M. An entry with no limits at all is
// the shape that used to advertise max_output_tokens == context_window (both
// 200000).
func TestKimiModels_EntryWithoutLimitsAdvertisesAnOutputBelowItsContext(t *testing.T) {
	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	testCfg := origCfg
	testCfg.Kimi.Enabled = true
	testCfg.Kimi.Allowlist = []config.KimiModelConfig{{ID: "kimi/no-limits", Enabled: true}}
	config.SetForTest(testCfg)

	server := &Server{backend: &discoveryTestBackend{}, logger: slog.Default(), now: time.Now}
	rec := httptest.NewRecorder()
	server.models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	for _, m := range resp.Data {
		if m["id"] != "kimi/no-limits" {
			continue
		}
		contextWindow, _ := m["context_window"].(float64)
		maxOutput, _ := m["max_output_tokens"].(float64)
		if contextWindow != float64(defaultDiscoveryContextWindow) {
			t.Errorf("context_window = %v, want the discovery default %d", contextWindow, defaultDiscoveryContextWindow)
		}
		if maxOutput >= contextWindow {
			t.Errorf("max_output_tokens = %v is not below context_window = %v", maxOutput, contextWindow)
		}
		if maxOutput != 32768 {
			t.Errorf("max_output_tokens = %v, want 32768", maxOutput)
		}
		return
	}
	t.Fatal("allowlist model missing from discovery response")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api -run TestKimiModels_EntryWithoutLimitsAdvertisesAnOutputBelowItsContext -count=1`
Expected: FAIL — `max_output_tokens = 200000 is not below context_window = 200000` and `max_output_tokens = 200000, want 32768`.

- [ ] **Step 3: Implement**

In `internal/api/server.go` replace the constant and its comment with:

```go
// defaultDiscoveryMaxOutputTokens caps the max_output_tokens that /v1/models
// advertises when only the context window is known. A model's output cap is
// always far below its context window, so reporting the context window as the
// output cap invites clients to send a max_tokens the provider rejects. 32768
// is the default max_tokens Kimi documents for its K2.x models and the value
// zen.DefaultMaxOutputTokens already fills in, so every gateway fallback
// advertises the same, conservative number.
const defaultDiscoveryMaxOutputTokens = 32768
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l internal/api && go test ./internal/api -count=1`
Expected: `ok` (the OpenRouter and Kimi guards reference the constant by name and keep passing).

- [ ] **Step 5: Commit**

```bash
git add internal/api/server.go internal/api/models_discovery_test.go
git commit -m "fix(api): stop advertising a context-sized max output for gateway entries without limits"
```

---

### Task 9: Claude Code gateway default for Haiku 4.5 matches Anthropic (N2)

**Files:**
- Modify: `internal/claudecode/router.go` (`claude-haiku-4-5-20251001` entry, `:53-61`)
- Test: `internal/claudecode/router_test.go` (append)

**Interfaces:**
- Consumes: `DefaultAllowlist() []ModelConfig`, `ModelConfig{ID, ContextLen, MaxOutputTokens}`.
- Produces: Haiku 4.5 `MaxOutputTokens: 64000`. `claudeCodeEntryMaxOutput` (`internal/api/server.go:2188`) feeds `applyMaxTokensPolicy`, so a client's `max_tokens: 64000` stops being clamped to 8192.

Source: Anthropic Models overview (platform.claude.com/docs/en/models/overview, fetched 2026-10-05): Haiku 4.5 — 200K context, 64K max output. Deliberately **not** changed (D4, N3): the 3.x entries (retired upstream; not re-verified), and no `claude-opus-5-5` / `claude-sonnet-5-5` / 4.x additions.

- [ ] **Step 1: Write the failing test**

Append to `internal/claudecode/router_test.go`:

```go
// Limits below are Anthropic's published numbers, not the proxy's choice:
// Models overview (platform.claude.com/docs/en/models/overview, checked
// 2026-10-05) lists Claude Haiku 4.5 at a 200K context window and 64K max
// output; the clamp in applyMaxTokensPolicy would otherwise cut a client's
// 64K request to the stale value.
func TestDefaultAllowlist_Haiku45MatchesAnthropicLimits(t *testing.T) {
	for _, m := range DefaultAllowlist() {
		if m.ID != "claude-haiku-4-5-20251001" {
			continue
		}
		if m.ContextLen != 200000 {
			t.Errorf("ContextLen = %d, want 200000", m.ContextLen)
		}
		if m.MaxOutputTokens != 64000 {
			t.Errorf("MaxOutputTokens = %d, want 64000", m.MaxOutputTokens)
		}
		return
	}
	t.Fatal("claude-haiku-4-5-20251001 missing from DefaultAllowlist")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/claudecode -run TestDefaultAllowlist_Haiku45MatchesAnthropicLimits -count=1`
Expected: FAIL — `MaxOutputTokens = 8192, want 64000`.

- [ ] **Step 3: Implement**

In `internal/claudecode/router.go`, change the Haiku 4.5 entry to:

```go
		{
			ID:          "claude-haiku-4-5-20251001",
			Alias:       "haiku-4-5",
			Aliases:     []string{"haiku-4-5", "claude-haiku-4-5", "claude-haiku-4.5", "haiku-4.5"},
			DisplayName: "Claude Haiku 4.5",
			// 200K context / 64K output: Anthropic Models overview, checked 2026-10-05.
			ContextLen:      200000,
			MaxOutputTokens: 64000,
			Thinking:        true,
			Enabled:         true,
		},
```
(`gofmt` realigns the field columns; run it.)

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -w internal/claudecode/router.go && go test ./internal/claudecode ./internal/api -count=1`
Expected: `ok` ×2.

- [ ] **Step 5: Commit**

```bash
git add internal/claudecode/router.go internal/claudecode/router_test.go
git commit -m "fix(claudecode): default Haiku 4.5 max output to Anthropic's published 64K"
```

---

### Task 10: Kimi — pin transparency and correct the package comment (N1); conditional normalization (GG)

**Files:**
- Test: `internal/api/kimi_proxy_test.go` (append)
- Modify: `internal/kimi/kimi.go` (package comment, `:1-4`)
- Conditional (10b): Create `internal/api/kimi_effort.go`, `internal/api/kimi_effort_test.go`, `docs/adr/0005-kimi-open-platform-effort-normalization.md`; Modify `internal/api/dispatch.go` (`tryKimiGateway`)

**Interfaces:**
- Consumes: the existing Kimi test harness in `kimi_proxy_test.go` (`newKimiTestServer`, `config.Save`).
- Produces: a guard that every reasoning field reaches Kimi byte-for-byte, and (10b only) `normalizeKimiEffort(baseURL string, body map[string]any) bool`.

Why a guard: Kimi Code documents its own server-side mapping (`medium→high`, `xhigh→max`, unknown → HTTP 400), so rewriting on this route would hide the vendor's behavior and change which level the vendor runs. The report claimed the opposite ("three-way mismatch"); the vendor docs refute it for Kimi Code.

- [ ] **Step 1: Write the guard**

Append to `internal/api/kimi_proxy_test.go`:

```go
// Kimi Code maps the Claude Code effort levels itself (medium->high, xhigh->max,
// docs: kimi.com/code/docs/en/kimi-code/models.html "Effort mapping in
// third-party tools"), and rejects unknown spellings with a 400. The gateway is
// a transparent forwarder (ADR-0001), so every reasoning field must reach
// Kimi exactly as the client sent it: normalizing here would hide the
// vendor's own mapping and change which level the vendor actually runs.
func TestServer_ForwardToKimi_PassesReasoningFieldsThrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer upstream.Close()

	if _, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true, "apiKey": "sk-kimi-test", "baseUrl": upstream.URL,
			"allowlist": []map[string]any{{"id": "k3", "enabled": true}},
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)
	body := `{"model":"k3[1m]","max_tokens":4096,` +
		`"thinking":{"type":"enabled","budget_tokens":2048,"display":"summarized"},` +
		`"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("unmarshal upstream body %s: %v", gotBody, err)
	}
	if sent["model"] != "k3" {
		t.Errorf("model = %v, want k3 (the [1m] suffix is a Claude Code convention, not a Kimi id)", sent["model"])
	}
	if got := sent["output_config"]; fmt.Sprint(got) != "map[effort:xhigh]" {
		t.Errorf("output_config = %v, want it forwarded untouched", got)
	}
	if got := sent["thinking"]; fmt.Sprint(got) != "map[budget_tokens:2048 display:summarized type:enabled]" {
		t.Errorf("thinking = %v, want it forwarded untouched", got)
	}
	if sent["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", sent["max_tokens"])
	}
}
```

- [ ] **Step 2: Run to verify it passes (guard)**

Run: `go test ./internal/api -run TestServer_ForwardToKimi_PassesReasoningFieldsThrough -count=1`
Expected: PASS — it pins today's behavior and must stay green.

- [ ] **Step 3: Correct the package comment**

Replace the package comment at the top of `internal/kimi/kimi.go` (`:1-4`) with:

```go
// Package kimi implements the Kimi gateway: a thin transparent forwarder to an
// Anthropic-compatible /v1/messages endpoint. Two vendor products share it and
// they are different APIs with different model IDs and effort defaults: the
// Moonshot Open Platform (https://api.moonshot.ai/anthropic, the config
// default, API key) and Kimi Code (https://api.kimi.ai/coding, subscription,
// OAuth). The proxy rewrites the Authorization header and preserves the
// Anthropic version/beta headers the client sent; it never alters reasoning
// fields, because Kimi Code maps Claude Code's effort levels server-side. See
// docs/reasoning-parameters.md.
```
(keep `package kimi` and the imports below it unchanged).

- [ ] **Step 4: Commit**

```bash
git add internal/api/kimi_proxy_test.go internal/kimi/kimi.go
git commit -m "test(api): pin Kimi reasoning-field transparency; correct the kimi package comment"
```

- [ ] **Step 5 (10b — only if gate GG = rejected): normalize for the Open Platform**

Skip this step unless `.reference/params-verdicts-20261005.txt` records `GG kimi: medium=400` or `xhigh=400`. It narrows ADR-0001 for one host family, so it ships with an ADR.

Write the test first, `internal/api/kimi_effort_test.go`:

```go
package api

import "testing"

func TestNormalizeKimiEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		baseURL string
		effort  any
		want    any
		changed bool
	}{
		{"medium becomes high on the Open Platform", "https://api.moonshot.ai/anthropic", "medium", "high", true},
		{"xhigh becomes max", "https://api.moonshot.ai/anthropic", "xhigh", "max", true},
		{"the China host is covered", "https://api.moonshot.cn/anthropic", "medium", "high", true},
		{"a published level is untouched", "https://api.moonshot.ai/anthropic", "high", "high", false},
		{"spelling is matched case-insensitively", "https://api.moonshot.ai/anthropic", "XHigh", "max", true},
		{"an unknown spelling is left for the vendor to reject", "https://api.moonshot.ai/anthropic", "turbo", "turbo", false},
		{"Kimi Code maps levels itself", "https://api.kimi.ai/coding", "medium", "medium", false},
		{"Kimi Code China host", "https://api.kimi.com/coding/", "xhigh", "xhigh", false},
		{"a local test upstream is untouched", "http://127.0.0.1:9", "medium", "medium", false},
		{"a non-string effort is untouched", "https://api.moonshot.ai/anthropic", float64(3), float64(3), false},
	}
	for _, tc := range tests {
		body := map[string]any{"output_config": map[string]any{"effort": tc.effort}}
		if got := normalizeKimiEffort(tc.baseURL, body); got != tc.changed {
			t.Errorf("%s: changed = %v, want %v", tc.name, got, tc.changed)
		}
		if got := body["output_config"].(map[string]any)["effort"]; got != tc.want {
			t.Errorf("%s: effort = %v, want %v", tc.name, got, tc.want)
		}
	}
	if normalizeKimiEffort("https://api.moonshot.ai/anthropic", map[string]any{"model": "k3"}) {
		t.Error("a body without output_config must be left alone")
	}
}
```

Run `go test ./internal/api -run TestNormalizeKimiEffort -count=1` → FAIL (`undefined: normalizeKimiEffort`). Then create `internal/api/kimi_effort.go`:

```go
package api

import (
	"net/url"
	"strings"
)

// kimiOpenPlatformEffort maps Claude Code's effort levels onto the enum the
// Moonshot Open Platform publishes for output_config.effort (low, high, max),
// using the mapping Moonshot documents for its own Kimi Code endpoint
// ("Effort mapping in third-party tools": medium to high, xhigh to max).
var kimiOpenPlatformEffort = map[string]string{
	"low": "low", "medium": "high", "high": "high", "xhigh": "max", "max": "max",
}

// normalizeKimiEffort rewrites output_config.effort for the Moonshot Open
// Platform only. Kimi Code maps the levels server-side, and an effort spelling
// this table does not know is left for the vendor to reject: its own 400 is
// more honest than a guess. It reports whether it changed the body.
func normalizeKimiEffort(baseURL string, body map[string]any) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "api.moonshot.ai" && host != "api.moonshot.cn" {
		return false
	}
	config, ok := body["output_config"].(map[string]any)
	if !ok {
		return false
	}
	effort, ok := config["effort"].(string)
	if !ok {
		return false
	}
	mapped, known := kimiOpenPlatformEffort[strings.ToLower(strings.TrimSpace(effort))]
	if !known || mapped == effort {
		return false
	}
	config["effort"] = mapped
	return true
}
```

In `internal/api/dispatch.go` `tryKimiGateway`, add one line after `g.body["model"] = targetModel`:

```go
	normalizeKimiEffort(g.cfg.Kimi.BaseURL, g.body)
```

Create `docs/adr/0005-kimi-open-platform-effort-normalization.md`:

```markdown
# Kimi Open Platform Effort Normalization and its Exception to ADR-0001

## Context

ADR-0001 keeps gateway traffic transparent. Claude Code sends `output_config.effort` as one of `low|medium|high|xhigh|max`. The Moonshot Open Platform's Messages API publishes only `low|high|max` (platform.kimi.ai/docs/api/messages), and the probe recorded in `.reference/params-verdicts-20261005.txt` (gate GG) showed it rejects `medium` or `xhigh` with HTTP 400. Kimi Code, the subscription product, accepts all five and maps them server-side (kimi.com/code/docs/en/kimi-code/models.html).

## Decision

`normalizeKimiEffort` rewrites `output_config.effort` on requests whose configured Kimi base URL host is `api.moonshot.ai` or `api.moonshot.cn`, using the vendor's own published mapping (`medium→high`, `xhigh→max`). Every other host, every spelling the table does not know, and every other field is forwarded untouched.

## Consequences

- A Claude Code user who picks `/effort medium` against the Open Platform gets a working request instead of a 400.
- ADR-0001's guarantee is narrowed for one field on two hosts. Kimi Code is still byte-transparent.
- The table is Moonshot's own mapping for a sibling product; if the Open Platform later publishes `medium`, delete the entry rather than keep rewriting.
```

Run: `gofmt -l internal/api && go vet ./internal/api && go test ./internal/api -count=1` → `ok`. Commit:

```bash
git add internal/api/kimi_effort.go internal/api/kimi_effort_test.go internal/api/dispatch.go docs/adr/0005-kimi-open-platform-effort-normalization.md
git commit -m "fix(api): map Claude Code effort levels onto the Moonshot Open Platform enum"
```

---

### Task 11 [OPT-IN, D7]: OpenAI `reasoning_effort` reaches the pipeline (N4)

Skip this task if you want the plan scoped to `/v1/messages`.

**Files:**
- Test: `internal/api/openai_reasoning_test.go` (create)
- Create: `internal/api/openai_reasoning.go`
- Modify: `internal/api/openai_request.go` (`translateOpenAIRequest`, after the `stream` copy, `:172-174`)

**Interfaces:**
- Consumes: Task 4's `reasoning.Parse`, `Level.Supported`, `reasoning.LevelLow…LevelMax`.
- Produces: `applyOpenAIReasoning(anthropic, openaiRequest map[string]any)`. OpenAI's `reasoning_effort` (Chat Completions) and `reasoning.effort` (Responses shape) become `output_config.effort`; `minimal` rides `low` (Anthropic has none below it); `none` becomes `thinking.type: "disabled"`; the proxy-only `reasoning_effort` field is never forwarded (an Anthropic-shaped upstream rejects unknown top-level fields); unknown spellings are dropped. The effort is ambient from the pipeline's point of view (D1): a tier named in the model ID still wins.

- [ ] **Step 1: Write the failing test**

Create `internal/api/openai_reasoning_test.go`:

```go
package api

import (
	"reflect"
	"testing"
)

func TestTranslateOpenAIRequest_ReasoningEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		extra        map[string]any
		wantEffort   string // output_config.effort, "" for none
		wantThinking map[string]any
	}{
		{"low", map[string]any{"reasoning_effort": "low"}, "low", nil},
		{"medium", map[string]any{"reasoning_effort": "medium"}, "medium", nil},
		{"high", map[string]any{"reasoning_effort": "high"}, "high", nil},
		{"xhigh is a real Anthropic level", map[string]any{"reasoning_effort": "xhigh"}, "xhigh", nil},
		{"max", map[string]any{"reasoning_effort": "max"}, "max", nil},
		{"minimal rides the low level", map[string]any{"reasoning_effort": "minimal"}, "low", nil},
		{"Responses object form", map[string]any{"reasoning": map[string]any{"effort": "high"}}, "high", nil},
		{"none turns thinking off", map[string]any{"reasoning_effort": "none"}, "", map[string]any{"type": "disabled"}},
		{"unknown spelling is dropped", map[string]any{"reasoning_effort": "turbo"}, "", nil},
		{"absent", map[string]any{}, "", nil},
	}
	for _, tc := range tests {
		request := map[string]any{
			"model":    "gemini-3.8-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		for k, v := range tc.extra {
			request[k] = v
		}
		got, err := translateOpenAIRequest(request)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, leaked := got.Anthropic["reasoning_effort"]; leaked {
			t.Errorf("%s: the proxy-only reasoning_effort field must not be forwarded: %#v", tc.name, got.Anthropic)
		}
		if _, leaked := got.Anthropic["reasoning"]; leaked {
			t.Errorf("%s: the OpenAI reasoning object must not be forwarded: %#v", tc.name, got.Anthropic)
		}
		var effort string
		if config, ok := got.Anthropic["output_config"].(map[string]any); ok {
			effort, _ = config["effort"].(string)
		}
		if effort != tc.wantEffort {
			t.Errorf("%s: output_config.effort = %q, want %q", tc.name, effort, tc.wantEffort)
		}
		thinking, _ := got.Anthropic["thinking"].(map[string]any)
		if !reflect.DeepEqual(thinking, tc.wantThinking) {
			t.Errorf("%s: thinking = %#v, want %#v", tc.name, thinking, tc.wantThinking)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api -run TestTranslateOpenAIRequest_ReasoningEffort -count=1`
Expected: FAIL — `low: output_config.effort = "", want "low"` (and the same for every level row); the `none` row fails on `thinking = map[string]interface {}(nil), want map[type:disabled]`.

- [ ] **Step 3: Implement**

Create `internal/api/openai_reasoning.go`:

```go
package api

import "antigravity-go-proxy/internal/reasoning"

// anthropicEffortLevels are the output_config.effort values Anthropic defines.
var anthropicEffortLevels = [...]reasoning.Level{
	reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh, reasoning.LevelXHigh, reasoning.LevelMax,
}

// applyOpenAIReasoning carries an OpenAI reasoning request onto the Anthropic
// fields the rest of the pipeline reads. OpenAI's reasoning_effort (Chat
// Completions) and reasoning.effort (Responses shape) become
// output_config.effort, with minimal riding the low level because Anthropic
// has none below it; "none" becomes thinking.type "disabled". The proxy-only
// reasoning_effort field is never forwarded: an Anthropic-shaped upstream
// rejects unknown top-level fields. An unrecognized spelling is dropped.
func applyOpenAIReasoning(anthropic, openaiRequest map[string]any) {
	params := reasoning.Parse(openaiRequest)
	switch {
	case params.Disabled:
		anthropic["thinking"] = map[string]any{"type": "disabled"}
	case params.Level != reasoning.LevelUnset:
		anthropic["output_config"] = map[string]any{
			"effort": string(params.Level.Supported(anthropicEffortLevels[:]...)),
		}
	}
}
```

In `internal/api/openai_request.go` `translateOpenAIRequest`, add the call right after the `stream` copy:

```go
	if stream, exists := openaiRequest["stream"]; exists {
		anthropic["stream"] = stream
	}
	applyOpenAIReasoning(anthropic, openaiRequest)
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l internal/api && go vet ./internal/api && go test ./internal/api -count=1`
Expected: `ok`; every pre-existing OpenAI translation test passes unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/api/openai_reasoning.go internal/api/openai_reasoning_test.go internal/api/openai_request.go
git commit -m "feat(api): carry OpenAI reasoning_effort onto output_config.effort"
```

---

### Task 12: Gate-conditional wire changes (apply only what Task 3's `TAKEN` line selects)

Read `.reference/params-verdicts-20261005.txt`. Apply the sub-tasks its `TAKEN` line names, in this order when several apply: **12e → 12a → 12b → 12c → 12d**. A sub-task whose gate says "keep" is skipped and costs nothing. Each is an independent commit with its own test. 12a–12d were prototyped against the Task 4–7 code while writing this plan and the tests below passed there (12a: flipping the constants breaks exactly the three pinned assertions listed, nothing else); 12e is data-driven and has no code of its own beyond the constants it edits. Where a sub-task breaks an existing pinned test, the edit is listed.

#### 12e — adopt agy's own effort mapping (gate GM)

The project's rule is to match agy, so where the capture shows what agy sends, it replaces every provisional number in Tasks 6–7.

- [ ] **Step 1: Tabulate what agy sent**

```bash
jq -r '[.label, (.generation_config.thinkingConfig // {} | tojson), (.generation_config.maxOutputTokens // "-")] | @tsv' \
  .reference/agy-generationconfig-mitm-20261005.jsonl | sort | column -t -s$'\t'
```

- [ ] **Step 2: Read the table and edit `internal/format/thinkingconfig.go` accordingly**

| What the capture shows | Edit |
|---|---|
| Claude rows (`claude-opus-4-6-thinking__<effort>`, `claude-sonnet-4-6__<effort>`) carry a budget that differs per effort | Set `effortBudgetLow`, `effortBudgetMedium`, `effortBudgetHighClaude` to the captured `low`, `medium`, `high` values. |
| `__max` equals `__high` | Nothing more (the fallback already makes `max` run as `high`); skip 12d. |
| `__max` differs from `__high` | Apply 12d, but make its `LevelMax` case return the captured constant instead of `cap − 8192`. |
| `__xhigh` equals `__max` rather than `__high` | Add `reasoning.LevelXHigh` to `claudeBudgetLevels` (12d's per-family list). |
| Claude rows carry **no** budget key at all | agy lets the model decide: apply 12b unconditionally (drop its `GB` precondition in the commit message). |
| `gemini-3.1-pro-high__*` / `gpt-oss-120b-medium__*` carry budgets | Set `effortBudgetHighGemini` (and Low/Medium if they differ) to the captured values. |
| Those rows carry `thinkingLevel` and no budget | They are tier-style routes. That is a catalog change (`ModelDetails.thinking_level`, tag 35), outside this plan: record it in the verdict file and stop. |
| Claude rows use `thinkingBudget`/`includeThoughts` | Apply 12a. |
| Gemini rows carry a fixed `maxOutputTokens` below the catalog limit | Take Task 7's alternative branch (keep the ceiling and advertise it). |

- [ ] **Step 3: Make the tests say where the numbers came from**

For every constant changed in Step 2, edit the matching row of `TestOutputConfigEffortSetsBudgetOnBudgetStyleModels` and add a comment naming the capture label it came from (for example `// agy-generationconfig-mitm-20261005.jsonl: claude-opus-4-6-thinking__medium`).

- [ ] **Step 4: Verify and commit**

Run: `gofmt -l internal/format && go test ./internal/format ./internal/accounts -count=1`
Expected: `ok`. Commit:

```bash
git add internal/format .reference/params-verdicts-20261005.txt
git commit -m "fix(format): match the effort budgets agy itself sends"
```

#### 12a — Claude route uses camelCase `thinkingConfig` keys (gate GA)

Only if `GA` shows `camel=accepted` **and** GM shows agy sending camelCase for Claude. If agy sends snake_case the existing spelling is correct and the Task 6 comment is the whole fix.

- [ ] **Step 1: Flip the spelling.** In `internal/format/thinkingconfig.go` change the two constants and drop the "decided by" sentence from their comment:

```go
// Spelling of the Claude route's thinkingConfig keys: the proto JSON names, as
// agy sends them (agy-generationconfig-mitm-20261005.jsonl) and as every other
// branch does. Keep the two keys together.
const (
	claudeKeyIncludeThoughts = "includeThoughts"
	claudeKeyThinkingBudget  = "thinkingBudget"
)
```

- [ ] **Step 2: Update the pins that assert the old spelling** (found with `grep -rn 'thinking_budget\|include_thoughts' internal cmd`):
  - `internal/format/format_test.go:283` — `thinking["thinking_budget"] != 1024 || thinking["include_thoughts"] != true` → `thinking["thinkingBudget"] != 1024 || thinking["includeThoughts"] != true`
  - `internal/format/testdata/google-request.json:31` — `"thinkingConfig": {"include_thoughts": true, "thinking_budget": 1000}` → `"thinkingConfig": {"includeThoughts": true, "thinkingBudget": 1000}` (this is the parity fixture, so this edit is the visible record that the spelling changed on evidence).
  - `internal/accounts/retry_test.go:223` — `thinking["thinking_budget"]` → `thinking["thinkingBudget"]`, and the two `thinking["thinking_budget"]` assertions in `TestDispatcherAppliesOutputConfigEffort`.
  - `internal/format/thinkingconfig_test.go`: `budgetOf` already accepts both spellings.

- [ ] **Step 3: Verify and commit**

Run: `go test ./internal/format ./internal/accounts ./internal/api -count=1` → `ok`.

```bash
git add internal/format internal/accounts
git commit -m "fix(format): spell the Claude thinkingConfig keys the way agy does"
```

#### 12b — adaptive thinking leaves the budget to the model (gate GB)

Only if `GB` shows status 200 **and** `thinkingBlocks > 0` (the route accepts a budget-less `thinkingConfig` and still thinks).

- [ ] **Step 1: Write the failing tests**

Append to `internal/format/thinkingconfig_test.go`:

```go
// A model that advertises adaptive thinking decides its own depth. When the
// client asks for adaptive thinking and states no effort or budget, the
// converter must not pin a budget on it.
func TestAdaptiveThinkingLeavesTheBudgetToTheModelThatSupportsIt(t *testing.T) {
	t.Parallel()
	adaptive := map[string]any{"type": "adaptive"}
	supports := claudeOpts
	supports.SupportsAdaptiveThinking = true
	tests := []struct {
		name       string
		options    ModelOptions
		extra      map[string]any
		wantBudget bool
	}{
		{"adaptive on a supporting model", supports, map[string]any{"thinking": adaptive}, false},
		{"adaptive on a model that does not advertise it", claudeOpts, map[string]any{"thinking": adaptive}, true},
		{"an effort still sets a budget", supports, map[string]any{"thinking": adaptive, "output_config": effortConfig("low")}, true},
		{"an explicit budget is kept", supports, map[string]any{"thinking": map[string]any{"type": "adaptive", "budget_tokens": float64(4000)}}, true},
		{"no thinking field at all keeps the catalog default", supports, map[string]any{}, true},
	}
	for _, tc := range tests {
		options := tc.options
		generation := convertWith(claudeRequest(tc.extra), &options)
		config := asMap(generation["thinkingConfig"])
		if config == nil || config["include_thoughts"] != true {
			t.Errorf("%s: thinking must stay on: %#v", tc.name, generation)
			continue
		}
		_, hasBudget := config["thinking_budget"]
		if hasBudget != tc.wantBudget {
			t.Errorf("%s: carries a budget = %v, want %v (%#v)", tc.name, hasBudget, tc.wantBudget, config)
		}
	}
}
```

Append to `internal/accounts/retry_test.go` (the catalog's `supportsAdaptiveThinking` must actually reach the converter):

```go
// The catalog's supportsAdaptiveThinking must reach the converter: a Claude
// route that advertises it gets thinkingConfig without a pinned budget when the
// client asks for adaptive thinking and states no effort.
func TestDispatcherLeavesTheBudgetToAdaptiveClaudeRoutes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC)
	account := testAccount("adaptive@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	modelsBody := []byte(`{
		"agentModelSorts":[{"groups":[{"modelIds":["claude-opus-4-6-thinking"]}]}],
		"models":{"claude-opus-4-6-thinking":{"displayName":"Claude Opus 4.6 (Thinking)","supportsThinking":true,"supportsAdaptiveThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}}
	}`)
	client := &scriptedClient{modelsBody: modelsBody, results: []scriptedResult{{events: [][]byte{[]byte(`{}`)}}}}
	dispatcher := newTestDispatcher(t, manager, &staticResolver{tokens: map[string]string{account.Email: "token"}}, map[string]*scriptedClient{"token": client}, now, func(context.Context, time.Duration) error { return nil })

	request := testRequest()
	request["model"] = "claude-opus-4-6-thinking"
	request["max_tokens"] = float64(64000)
	request["thinking"] = map[string]any{"type": "adaptive"}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	thinking := client.payload["request"].(map[string]any)["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if thinking["include_thoughts"] != true {
		t.Fatalf("thinking must stay on: %#v", thinking)
	}
	if _, pinned := thinking["thinking_budget"]; pinned {
		t.Fatalf("an adaptive-capable route must not get a pinned budget: %#v", thinking)
	}
}
```
(If 12a was applied, use `includeThoughts` / `thinkingBudget` in both tests.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/format ./internal/accounts -count=1`
Expected: FAIL — build error `unknown field SupportsAdaptiveThinking in struct literal of type ModelOptions` (format); once that compiles, `an adaptive-capable route must not get a pinned budget: …thinking_budget:1024…` (accounts).

- [ ] **Step 3: Implement**

`internal/format/model.go` — add the field to `ModelOptions`:

```go
type ModelOptions struct {
	SupportsThinking         bool
	SupportsAdaptiveThinking bool
	ThinkingBudget           int
	MinThinkingBudget        int
	ThinkingLevel            string
	MaxOutputTokens          int
}
```

`internal/accounts/dispatcher.go` — forward it where `ModelOptions` is built (`BuildCloudCodeRequestWithModel`, ~`:393`): add `SupportsAdaptiveThinking: modelDetails.SupportsAdaptiveThinking,` to the literal and run `gofmt -w internal/accounts/dispatcher.go`.

`internal/format/thinkingconfig.go` — append:

```go
// letModelDecide reports whether an adaptive-thinking request carries no budget
// and no effort, so the budget should be left off and the model left to decide
// how much to think. Only a model whose catalog entry advertises adaptive
// thinking qualifies; every other request keeps a concrete budget.
func letModelDecide(params reasoning.Params, options *ModelOptions) bool {
	return params.Adaptive && options != nil && options.SupportsAdaptiveThinking &&
		!params.HasBudget && params.Level == reasoning.LevelUnset
}
```

`internal/format/request.go` — in the Claude branch replace

```go
		claudeBudget = thinkingBudget(params, family, defaultThinkingBudget, minThinkingBudget)
		generation["thinkingConfig"] = map[string]any{claudeKeyIncludeThoughts: true, claudeKeyThinkingBudget: claudeBudget}
```
with
```go
		if letModelDecide(params, options) {
			generation["thinkingConfig"] = map[string]any{claudeKeyIncludeThoughts: true}
		} else {
			claudeBudget = thinkingBudget(params, family, defaultThinkingBudget, minThinkingBudget)
			generation["thinkingConfig"] = map[string]any{claudeKeyIncludeThoughts: true, claudeKeyThinkingBudget: claudeBudget}
		}
```
(`claudeBudget` stays `0` on the budget-less path, so `reconcileClaudeBudget` is skipped.)

- [ ] **Step 4: Run to verify they pass, then commit**

Run: `gofmt -l internal && go vet ./internal/format ./internal/accounts && go test ./internal/format ./internal/accounts -count=1` → `ok`.

```bash
git add internal/format/model.go internal/format/request.go internal/format/thinkingconfig.go internal/format/thinkingconfig_test.go internal/accounts/dispatcher.go internal/accounts/retry_test.go
git commit -m "fix(format): leave the thinking budget to Claude routes that advertise adaptive thinking"
```

#### 12c — drop sampling overrides while Claude thinks (gate GC)

Only if `GC` shows `GC-claude-temperature` or `GC-claude-top-k` **rejected (400)**. Otherwise Cloud Code is more lenient than Anthropic, which is fine.

- [ ] **Step 1: Write the failing test.** Append to `internal/format/thinkingconfig_test.go`:

```go
func TestSamplingOverridesAreDroppedWhileClaudeThinks(t *testing.T) {
	t.Parallel()
	sampling := map[string]any{"temperature": 0.2, "top_p": 0.5, "top_k": float64(10)}
	withSampling := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range sampling {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	options := claudeOpts
	thinking := convertWith(claudeRequest(withSampling(nil)), &options)
	for _, key := range []string{"temperature", "topP", "topK"} {
		if _, kept := thinking[key]; kept {
			t.Errorf("%s survived while Claude thinks: %#v", key, thinking)
		}
	}

	off := convertWith(claudeRequest(withSampling(map[string]any{"thinking": map[string]any{"type": "disabled"}})), &options)
	if off["temperature"] != 0.2 || off["topP"] != 0.5 || off["topK"] != float64(10) {
		t.Errorf("sampling must pass through when thinking is off: %#v", off)
	}

	gemini := ModelOptions{SupportsThinking: true, ThinkingLevel: "HIGH", MaxOutputTokens: 65536}
	kept := convertWith(map[string]any{"model": "gemini-3.8-flash-high", "max_tokens": float64(1024), "temperature": 0.2}, &gemini)
	if kept["temperature"] != 0.2 {
		t.Errorf("Gemini keeps its sampling overrides: %#v", kept)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/format -run TestSamplingOverridesAreDroppedWhileClaudeThinks -count=1`
Expected: FAIL — `temperature survived while Claude thinks`, `topP …`, `topK …`.

- [ ] **Step 3: Implement.** Append to `internal/format/thinkingconfig.go`:

```go
// dropSamplingOverrides removes temperature, topP and topK. Anthropic rejects
// them while Claude thinks (Thinking doc, "Sampling parameters"); probe GC
// showed Cloud Code enforces the same rule, so forwarding them only turns a
// working request into a 400.
func dropSamplingOverrides(generation map[string]any) {
	delete(generation, "temperature")
	delete(generation, "topP")
	delete(generation, "topK")
}
```
In `internal/format/request.go`, immediately before `if len(tools) > 0 {` (the tool-declaration block) add:
```go
	if family == FamilyClaude && generation["thinkingConfig"] != nil {
		dropSamplingOverrides(generation)
	}
```
The parity fixture sends sampling alongside thinking, so edit `internal/format/testdata/google-request.json`: remove `"temperature": 0,`, `"topP": 0.9,` and `"topK": 0,` from `generationConfig` (keep `maxOutputTokens`, `stopSequences`, `thinkingConfig`). `anthropic-request.json` stays as is: it is the input that proves the drop.

- [ ] **Step 4: Run to verify, then commit**

Run: `gofmt -l internal/format && go test ./internal/format ./internal/accounts ./internal/api -count=1` → `ok`.

```bash
git add internal/format/thinkingconfig.go internal/format/thinkingconfig_test.go internal/format/request.go internal/format/testdata/google-request.json
git commit -m "fix(format): drop sampling overrides while Claude thinks, as Cloud Code rejects them"
```

#### 12d — `max` effort spends the cap on Claude (gate GD)

Only if `GD-claude-budget-55808` was **accepted**. (If `GM` showed a different captured `max` budget, return that constant in the `LevelMax` case instead.)

- [ ] **Step 1: Update the tests first.** In `TestOutputConfigEffortSetsBudgetOnBudgetStyleModels` replace the row `claude: max falls back to high` with:

```go
		{"claude: max spends the cap minus the answer headroom", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("max")}, 55808},
		{"claude: max never drops below high on a small cap", "claude-opus-4-6-thinking", ModelOptions{SupportsThinking: true, ThinkingBudget: 1024, MaxOutputTokens: 40000},
			map[string]any{"thinking": adaptive, "output_config": effortConfig("max")}, 32000},
		{"claude: max without a known cap is high", "claude-opus-4-6-thinking", ModelOptions{SupportsThinking: true, ThinkingBudget: 1024},
			map[string]any{"thinking": adaptive, "output_config": effortConfig("max")}, 32000},
```
(The Gemini `max falls back to high → 16000` row stays: Gemini publishes three tiers.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/format -run TestOutputConfigEffortSetsBudgetOnBudgetStyleModels -count=1`
Expected: FAIL — `claude: max spends the cap minus the answer headroom: thinking budget = 32000, want 55808`.

- [ ] **Step 3: Implement.** In `internal/format/thinkingconfig.go` replace `budgetLevels` and `effortBudget`, and add the `maxOutput` parameter to `thinkingBudget`:

```go
// budgetLevels are the effort levels the budget table distinguishes, per
// family. A stronger request falls back to the highest level listed, the rule
// Claude Code documents for a level a model does not accept. Claude 4.6 accepts
// max (Effort doc: max is available on Opus 4.6 and Sonnet 4.6) but not xhigh,
// so xhigh falls back to high there; the Gemini families publish three tiers.
var (
	claudeBudgetLevels = [...]reasoning.Level{reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh, reasoning.LevelMax}
	geminiBudgetLevels = [...]reasoning.Level{reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh}
)

// effortBudget returns the thinking budget for an effort level, 0 for none.
// maxOutput is the model's output cap (0 when unknown); max effort spends all
// of it except the headroom the answer needs, and never less than high.
func effortBudget(family ModelFamily, level reasoning.Level, maxOutput int) int {
	levels := geminiBudgetLevels[:]
	if family == FamilyClaude {
		levels = claudeBudgetLevels[:]
	}
	switch level.Supported(levels...) {
	case reasoning.LevelLow:
		return effortBudgetLow
	case reasoning.LevelMedium:
		return effortBudgetMedium
	case reasoning.LevelHigh:
		if family == FamilyClaude {
			return effortBudgetHighClaude
		}
		return effortBudgetHighGemini
	case reasoning.LevelMax:
		return max(effortBudgetHighClaude, maxOutput-thinkingResponseHeadroom)
	}
	return 0
}
```
and change `thinkingBudget`'s signature to `func thinkingBudget(params reasoning.Params, family ModelFamily, fallback, minimum, maxOutput int) int`, passing `maxOutput` to both `effortBudget(family, params.Level, maxOutput)` calls. In `internal/format/request.go` pass the converter's local `maxOutputTokens` as the new last argument at both call sites (`thinkingBudget(params, family, defaultThinkingBudget, minThinkingBudget, maxOutputTokens)` and `thinkingBudget(params, family, fallback, minThinkingBudget, maxOutputTokens)`; if 12b is applied, the first call sits in its `else` branch).

- [ ] **Step 4: Run to verify, then commit**

Run: `gofmt -l internal/format && go vet ./internal/format && go test ./internal/format ./internal/accounts ./internal/api -count=1` → `ok`.

```bash
git add internal/format/thinkingconfig.go internal/format/thinkingconfig_test.go internal/format/request.go
git commit -m "feat(format): let max effort spend the Claude output cap minus the answer headroom"
```

---

## Phase C — Record it

### Task 13: Reference doc, README, glossary, research errata

**Files:**
- Create: `docs/reasoning-parameters.md`
- Modify: `README.md` (new subsection before the `---` that precedes `## Client Integrations`)
- Modify: `CONTEXT.md` (new `### Reasoning` section before `### Cache Management`)
- Modify: `docs/research/2026-10-05-api-parameters-reasoning-effort-matrix.md` (append an errata section)

**Interfaces:** none (documentation). The reference doc is the answer to "is the proxy's behavior identical to upstream, and where not, why": every row cites its owner.

- [ ] **Step 1: Create `docs/reasoning-parameters.md`**

Fill §9 from the verdict file (`cat .reference/params-verdicts-20261005.txt` into the fenced block) and adjust §2/§5 if Task 12 changed a constant; everything else below is true of the default branch.

````markdown
# Reasoning, Thinking and Output-Limit Parameters

How the proxy treats the parameters that control how much a model thinks and how much it may write, per route, and where it deliberately differs from the vendor. Last verified 2026-10-05. Every row names the document that owns the convention; evidence the proxy produced itself lives in `.reference/`.

## 1. Request fields the proxy reads

| Field | Convention (owner) | Proxy behavior |
|---|---|---|
| `max_tokens` | Anthropic Messages API: required, `minimum: 0` (`0` = cache pre-warm). Kimi Messages API: required, `minimum: 1`. | Cloud Code: sent as `maxOutputTokens`; absent stays absent. Gateways: `applyMaxTokensPolicy` — clamp down to the entry's limit, floor 16, never raise above the client's value, omit when no limit is known. |
| `thinking.type` | Anthropic Thinking doc: `enabled`+`budget_tokens` (deprecated on 4.6, rejected on 4.7+), `adaptive`, `disabled`. | `disabled` → thinking off (a tiered Gemini 3.7/3.8 route drops to its lowest tier: Google does not allow full off). `adaptive` → the catalog's default depth. `enabled` → `budget_tokens` is honored. |
| `thinking.budget_tokens` | Anthropic: ≥ 1024 and < `max_tokens`. | Honored on budget-style routes; on Claude it is kept below the final `maxOutputTokens`. |
| `thinking.display` | Anthropic: `summarized` / `omitted`. | Not honored, by design: Cloud Code returns the thought text and the client collapses it. Signatures are always returned and must be replayed. |
| `output_config.effort` | Anthropic Effort doc: `low`, `medium`, `high`, `xhigh`, `max`; Claude Code sends it on every request; `agy --effort` accepts the same five. | Read, at the lowest precedence (§3). Forwarded untouched by every gateway. |
| `output_config.format` | Anthropic structured outputs. | Not translated on the Cloud Code route. |
| `reasoning_effort`, `reasoning` (string or `{"effort"}`), `thinking_budget` | Proxy extensions (OpenAI / Gemini spellings). | Highest precedence. `none`/`off`/`false`/`0` turn thinking off; an unknown spelling is ignored. |
| `temperature`, `top_p`, `top_k`, `stop_sequences` | Anthropic accepts them; real Claude Code sends none of them (`.reference/claude-code-headers-20260923.txt`). | Cloud Code route: copied (see §9 for the thinking-time rule). Gateways: untouched. |

## 2. Levels and the fallback rule

Levels: `low < medium < high < xhigh < max` (plus `minimal`, which only OpenAI-shaped clients send). A level a model does not publish runs as **the highest published level at or below it**; a level below everything published takes the lowest. This is Claude Code's own rule ("`xhigh` runs as `high` on Opus 4.6").

| Route | Published levels | Consequence |
|---|---|---|
| Gemini flash tiers (3.8 / 3.7 / 3.6, legacy 3.5 repointed) | `low`, `medium`, `high` | `xhigh` and `max` route to the `-high` tier; `minimal` to `-low`. |
| Budget-style Gemini (3.1 Pro), GPT-OSS | `low`, `medium`, `high` | same fallback, then the budget in §5. |
| Claude 4.6 (Opus, Sonnet) | `low`, `medium`, `high`, `max` (Effort doc: no `xhigh` on 4.6) | `xhigh` → `high`; `max` → `high` unless gate GD/12d is applied. |

## 3. Precedence

Tier routing (catalog), highest first: `reasoning_effort` / `reasoning` → a bare thinking budget (back-mapped: ≤ 2048 `low`, < 12000 `medium`, else `high`) → a tier named in the model ID (`…-low|-medium|-high|-extra-low`) → `output_config.effort` → the catalog default. Budget emission (converter), highest first: `reasoning_effort` → explicit `budget_tokens` / `thinking_budget` → `output_config.effort` → the catalog's default budget.

Why `output_config.effort` is last: Claude Code sends it on every request. If it outranked the model ID, anyone who picked `gemini-3.8-flash-low` would be rerouted to `-high` by their default `high` effort.

## 4. Per-route behavior

| Route | Reasoning fields | Limits |
|---|---|---|
| **Cloud Code** (Gemini, Claude 4.6, GPT-OSS via agy) | Translated (§§2–3, 5). Tiered flash routes get `thinkingLevel`; everything else gets a budget; never both. | The live catalog's `maxOutputTokens` is authoritative. The fixed 16384 Gemini ceiling applies only to the legacy single-credential path that has no catalog. |
| **Claude Code gateway** (OAuth → Anthropic) | Forwarded untouched (ADR-0001/0004). | Defaults below (§6); client `max_tokens` is clamped down to the entry's limit. |
| **Kimi gateway** | Forwarded untouched. | Operator-set per entry; discovery fallback 32768 (§6). |
| **OpenRouter / custom endpoints** | Forwarded untouched. | OpenRouter catalog or operator-set. |
| **Zen** (Chat / Responses wires) | `thinking`, `output_config`, `top_k`, `stop_sequences` are dropped on the Responses wire and `thinking` is not translated on the Chat wire: there is no per-model evidence for a safe mapping, and the free-tier gate pins the body shape. | `max_tokens` → `max_output_tokens`; fill default 32768. |

## 5. Effort → thinking budget (budget-style routes)

Cloud Code's `ThinkingConfig` carries a token budget (`thinking_budget`, tag 3) or a tier (`thinking_level`, tag 4) and no effort level, so the proxy has to turn a level into a number. The table is the proxy's own choice, not a vendor number:

| Level | Claude | Gemini / GPT-OSS |
|---|---|---|
| `low` / `minimal` | 1024 | 1024 |
| `medium` | 8000 | 8000 |
| `high` / `xhigh` / `max` | 32000 | 16000 |

The catalog's default budget (1024 for the Opus 4.6 fixture, which is also what agy sends by default) applies when the client states no effort and no budget. `max` on Claude may spend the output cap minus 8192 if gate GD/12d was applied. Anthropic's rule `budget_tokens < max_tokens` is enforced on the final `maxOutputTokens` after the model cap.

## 6. Output limits and context windows

| Surface | Model | Context | Max output | Source |
|---|---|---|---|---|
| Cloud Code | Gemini 3.x Flash | 1,048,576 | 65,536 | `.reference/agy-models-20260903.txt` (View B) |
| Cloud Code | Gemini 3.1 Pro | 1,048,576 | 65,535 | same |
| Cloud Code | Claude Sonnet / Opus 4.6 | 250,000 | 64,000 | same (Google's serving limits, not Anthropic's) |
| Cloud Code | GPT-OSS 120B | 131,072 | 32,768 | same |
| Claude Code gateway | Fable 5 / 5.1, Opus 5, Sonnet 5 | 1,000,000 | 128,000 | platform.claude.com/docs/en/models/overview; build-with-claude/context-windows |
| Claude Code gateway | Haiku 4.5 | 200,000 | 64,000 | Models overview |
| Kimi Open Platform | `kimi-k3` | 1M | not published | platform.kimi.ai/docs/models |
| Kimi Open Platform | `kimi-k2.7-code`, `kimi-k2.6` | 256K | default `max_tokens` 32768 | quickstart "Parameters Differences" |
| Kimi Code | `k3` | 1,048,576 (Pro+; 262,144 below) | not published | kimi.com/code/docs/en/kimi-code/models.html |
| Kimi Code | `k3-256k`, `kimi-for-coding-highspeed` | 262,144 | not published | same |
| Kimi Code | `kimi-for-coding` (K2.8 Preview) | 1,048,576 | not published | same |

The proxy advertises the limits of the upstream that serves the request, so Cloud Code rows are Google's, not Anthropic's. `/v1/models` fallback when a gateway entry has no limits: context 200000, output 32768.

## 7. Kimi: two products, one gateway

| | Moonshot Open Platform | Kimi Code (subscription) |
|---|---|---|
| Base URL | `https://api.moonshot.ai/anthropic` (config default) | `https://api.kimi.ai/coding/` overseas, `https://api.kimi.com/coding/` China |
| Auth | API key | OAuth device flow / membership key |
| Model IDs | `kimi-k3`, `kimi-k2.7-code`, `kimi-k2.7-code-highspeed`, `kimi-k2.6`; discontinued: `kimi-k2.5`, `moonshot-v1-*`, `kimi-k2-*` | `k3`, `k3-256k`, `kimi-for-coding`, `kimi-for-coding-highspeed` |
| Effort | `output_config.effort` ∈ `low|high|max`, default `max` | `low|high|max`; defaults `high` (k3, k3-256k), `max` (kimi-for-coding); **server maps** `medium→high`, `xhigh→max`, `ultra/max→max`, `minimum/light→low`, `none→thinking disabled`, anything else → 400 |
| Thinking | K2.7: forced on, only `{"type":"enabled","keep":"all"}`; K2.6: enabled/disabled | thinking off routes K3 and K2.8 requests to K2.8 Preview without thinking |
| Sampling | `temperature` fixed 1.0, `top_p` 0.95, other values → error | not published |
| `[1m]` suffix | not a Kimi ID | "only needed for Claude Code env vars"; the proxy strips it |

The proxy forwards reasoning fields untouched on both. (If Task 10b was applied, effort is mapped to the three-value enum for the Open Platform hosts only; see ADR-0005.) Kimi's `/v1/models` publishes `context_length` and `supports_*` flags but no output limit, so Kimi output limits are operator-set.

## 8. Claude Code settings that matter here

- Claude Code decides what to send from the model ID: effort and thinking capabilities are matched by ID pattern, and unknown IDs get none. To get `/effort` for a Gemini-named model, declare it: `ANTHROPIC_DEFAULT_<TIER>_MODEL_SUPPORTED_CAPABILITIES=effort,thinking` (add `xhigh_effort`, `max_effort` only for models that publish them).
- `opus` / `sonnet` resolve to Opus 5.5 / Sonnet 5.5 on the Anthropic API; the Claude Code gateway's default allowlist deliberately does **not** claim `claude-opus-5-5` or `claude-sonnet-5-5` (a distinct newer model must never be silently rewritten to an older one). To route them, add allowlist entries with context 1,000,000 and max output 128,000.
- Effort changes between requests invalidate the Anthropic prompt cache, so the OutputShaper never rewrites `output_config.effort`.

## 9. Intentional divergences

- Anthropic model spellings (`claude-3-5-sonnet`, `sonnet`, `opus`, `fable`) are unmapped on Cloud Code: hard-mapping them would silently change which model answers (PR #75).
- `reasoning_effort`, `reasoning`, `thinking_budget` are proxy extensions; `thinking_budget: -1` means "off" here, not Gemini's "dynamic".
- Generation is pinned to the Daily host because a thought signature is rejected by the other host.
- `max_output_tokens` is not a `/v1/messages` field and is not aliased; a missing `max_tokens` is tolerated on Cloud Code and forwarded as absent to gateways (Anthropic and Kimi will reject it themselves).
- `redacted_thinking` and foreign thinking signatures in history are stripped (they cannot be replayed to another backend); `thinking.display` is accepted and ignored.
- Sampling parameters: copied on Cloud Code (see the verdict below for whether Cloud Code enforces Anthropic's thinking-time rule).
- `ThinkingLevel.MINIMAL` is never emitted: no catalog field says which routes accept it and Google documents errors on some families.
- The Zen wires drop reasoning (§4); the Claude Code gateway defaults omit 5.5 and 4.x IDs (§8).

## 10. Probe verdicts (2026-10-05)

```
<paste .reference/params-verdicts-20261005.txt here>
```

## 11. Re-verify

```bash
go run ./cmd/paramprobe -list                                  # the probe matrix, offline
go run ./cmd/paramprobe -out .reference/cloudcode-params-probe-$(date +%Y%m%d).jsonl
agy --help | grep -- --effort                                   # agy's effort vocabulary
curl -s https://api.anthropic.com/v1/models -H "x-api-key: $ANTHROPIC_API_KEY" -H 'anthropic-version: 2023-06-01' | jq '.data[] | {id, max_input_tokens, max_tokens, effort: .capabilities.effort}'
```

## 12. Sources

Anthropic: [Effort](https://platform.claude.com/docs/en/build-with-claude/effort), [Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking), [Context windows](https://platform.claude.com/docs/en/build-with-claude/context-windows), [Models overview](https://platform.claude.com/docs/en/models/overview), [List models](https://platform.claude.com/docs/en/api/models/list). Claude Code: [Model configuration](https://code.claude.com/docs/en/model-config). Google: [Gemini thinking](https://ai.google.dev/gemini-api/docs/generate-content/thinking). Kimi: [Messages API](https://platform.kimi.ai/docs/api/messages), [Model parameter reference](https://platform.kimi.ai/docs/api/models-overview), [Model list](https://platform.kimi.ai/docs/models), [Kimi Code overview](https://www.kimi.com/code/docs/en/), [Kimi Code models](https://www.kimi.com/code/docs/en/kimi-code/models.html), [Kimi Code in Claude Code](https://www.kimi.com/code/docs/en/third-party-tools/claude-code.html). Proto: `gen/google/cloud/aiplatform/master/content.pb.go:530-538, 6113-6121`.
````

- [ ] **Step 2: README**

Insert this subsection immediately before the `---` that precedes `## Client Integrations` (after the "Server-Side Model Mapping" text):

```markdown
### Reasoning effort and token limits

Claude Code's `/effort` (`output_config.effort`) is honored on the Cloud Code route: it picks the tier for a bare `gemini-3.8-flash` ID and sets the thinking budget on the Claude, Gemini Pro and GPT-OSS routes, while a tier named in the model ID (`gemini-3.8-flash-low`) always wins. Gateways forward the field untouched. The full mapping, precedence, per-route limits, the Kimi Code vs Moonshot Open Platform differences and every intentional divergence from the upstream APIs are in [docs/reasoning-parameters.md](docs/reasoning-parameters.md).

Claude Code matches effort support by model ID, so a Gemini-named model gets no `/effort` unless you declare it, for example `ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES=effort,thinking`.
```

- [ ] **Step 3: Glossary**

In `CONTEXT.md`, add before `### Cache Management`:

```markdown
### Reasoning

**Effort Level**:
One of `low`, `medium`, `high`, `xhigh`, `max` — Anthropic's `output_config.effort` vocabulary, which `agy --effort` also accepts. A model that does not publish a level runs the highest published level at or below it.
_Avoid_: Reasoning mode, thinking intensity, think level.

**Ambient Effort**:
The `output_config.effort` Claude Code sends on every request. It has the lowest precedence: it picks a tier for a bare family ID but never overrides a tier named in the model ID, an explicit `reasoning_effort`, or an explicit thinking budget.
_Avoid_: Default effort, client effort.

**Budget-Style Model**:
A Cloud Code route whose thinking is set by a token budget (`thinking_budget`) rather than a tier (`thinking_level`): Claude 4.6, Gemini 3.1 Pro, GPT-OSS. The proxy turns an Effort Level into a budget; tiered Gemini flash routes get a tier from the model catalog instead.
_Avoid_: Legacy thinking, budget thinking.
```

- [ ] **Step 4: Errata on the research report**

Append to `docs/research/2026-10-05-api-parameters-reasoning-effort-matrix.md`:

```bash
cat >> docs/research/2026-10-05-api-parameters-reasoning-effort-matrix.md <<'EOF'

---

## Errata (verified 2026-10-05, see docs/superpowers/plans/2026-10-05-api-parameter-parity.md §0)

- **§3.3** `thinking_budget` and `thinking_level` are independent proto3 `optional` fields (separate synthetic oneofs in the descriptor, `content.pb.go:7584-7586`), not one oneof. "Only one may be set" is an unverified server rule; the proxy never sends both (probe GX records it).
- **§4 / G2 / Executive summary** The "three-way mismatch" holds only for the Moonshot Open Platform. Kimi Code (the OAuth gateway) uses model IDs `k3`, `k3-256k`, `kimi-for-coding`, `kimi-for-coding-highspeed`, and its server maps Claude Code's levels itself (`medium→high`, `xhigh→max`, unknown → 400).
- **G2** Collapsing `xhigh`/`max` to `high` is correct for the Gemini flash tiers and for `xhigh` on Claude 4.6 (Claude Code: "`xhigh` runs as `high` on Opus 4.6"). Only `max` on 4.6 is a real loss.
- **G4** With a live catalog entry, `adaptive` already gets the catalog's default budget (not 32000); the 32000/16000 defaults apply only without a catalog.
- **G7** Real Claude Code sends no `temperature`, `top_p`, `top_k` or `stop_sequences`; whether Cloud Code enforces Anthropic's thinking-time rule is a probe question (GC).
- **G8** `redacted_thinking` history is stripped and the tool-loop recovery runs; nothing leaks upstream. Not a bug.
- **G9** The legacy single-credential path ignored `reasoning_effort` and a top-level `thinking_budget` for Gemini (reproduced: `reasoning_effort:low` → budget 16000).
- **G10** Production-dead: only `options == nil` reaches it, and `cmd/proxy/main.go:261` always sets `Backend`.
- **G12** Not a gap: `/v1/messages` has no `max_output_tokens`, and Anthropic and Kimi both require `max_tokens`.
- **§5.2** The Claude "pass-through" rows (200K / 8192) described the Claude Code gateway defaults, which are 1M / 128K for the 5-series and, for Haiku 4.5, 200K / **8192 where Anthropic publishes 64K** (fixed in the plan, Task 9).
- **Missing from the report:** the agy CLI's own `--effort low|medium|high|xhigh|max`; the Claude Code gateway's deliberate omission of `claude-opus-5-5`/`claude-sonnet-5-5`; the OpenAI endpoint dropping `reasoning_effort`; the shaper/prompt-cache interaction.
EOF
```

- [ ] **Step 5: Verify and commit**

Run: `grep -n 'paste .reference' docs/reasoning-parameters.md` → no output (the verdict block was filled); `git diff --stat` shows only the four doc files.

```bash
git add docs/reasoning-parameters.md README.md CONTEXT.md docs/research/2026-10-05-api-parameters-reasoning-effort-matrix.md
git commit -m "docs: document reasoning, thinking and output-limit parity and correct the research report"
```

---

### Task 14: Final verification

**Files:** none.

- [ ] **Step 1: Static gates**

```bash
make fmt-check
go vet ./...
go test -race ./... 2>&1 | tail -25
(cd scripts && python3 -m unittest test_mitm_header_dump)
```
Expected: `fmt-check` clean, vet clean, every package `ok`, `Ran 35 tests … OK`. One known unrelated flake: `internal/claudecode/ccusage` `TestEngine_RecordWritesLedgerAndSnapshot` fails (`costs 5h 0.00019999999999999998 today 0, want 0.0002`) when run in the first minutes after 00:00 UTC — it reproduced on an untouched `main` at 2026-10-06T00:00:38Z. If it is the only failure, re-run after 00:15 UTC and confirm it passes on `main`; do not chase it from this branch.

- [ ] **Step 2: Fingerprint untouched**

Run: `git diff main --stat -- internal/cloudcode cmd/proxy` → empty. (This plan never edits the transport; no JA4 recheck is needed. If anything shows up here, stop and find out why.)

- [ ] **Step 3: Live smoke through the proxy (uses your own accounts; a few small requests)**

```bash
go build -o /tmp/agp ./cmd/proxy
/tmp/agp -listen 127.0.0.1:8099 -api-key plan-smoke 2>/tmp/agp.log & agp_pid=$!
sleep 3
# discovery: Gemini advertises the live limit; gateway entries never advertise a context-sized output
curl -sS -H 'x-api-key: plan-smoke' http://127.0.0.1:8099/v1/models \
  | jq -r '.data[] | select(.id|test("gemini|claude")) | [.id, .context_window, .max_output_tokens] | @tsv'
# effort sweep on the Claude route: thinking should grow with the level
for e in low medium high; do
  curl -sS http://127.0.0.1:8099/v1/messages -H 'x-api-key: plan-smoke' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
    -d "{\"model\":\"claude-opus-4-6-thinking\",\"max_tokens\":40000,\"thinking\":{\"type\":\"adaptive\"},\"output_config\":{\"effort\":\"$e\"},\"messages\":[{\"role\":\"user\",\"content\":\"What is the sum of the first 40 prime numbers? Reply with only the number.\"}]}" \
    | jq -c --arg e "$e" '{effort:$e, output_tokens:.usage.output_tokens, thinking_chars:([.content[]|select(.type=="thinking")|.thinking|length]|add)}'
done
# tier routing: a bare family ID follows effort, a named tier does not
for pair in "gemini-3.8-flash low" "gemini-3.8-flash-high low"; do
  set -- $pair
  curl -sS -o /dev/null http://127.0.0.1:8099/v1/messages -H 'x-api-key: plan-smoke' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
    -d "{\"model\":\"$1\",\"max_tokens\":64,\"output_config\":{\"effort\":\"$2\"},\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: OK\"}]}"
done
grep 'Mapping model' /tmp/agp.log
kill $agp_pid
```
Expected: the Gemini rows show `65536`/`65535` max output (or `16384` if Task 7 took the alternative branch); the three sweep rows show non-decreasing `thinking_chars`; the log shows `Mapping model gemini-3.8-flash -> gemini-3.8-flash-low` and **no** mapping line for `gemini-3.8-flash-high` (it is already a catalog ID and stays). If a request is rate-limited, record that and retry later; a 429 body still proves routing.

- [ ] **Step 4: Refresh the graph and ask about integration**

```bash
graft build
git status --short | grep -v '^??'
```
Expected: `graft build` succeeds; no tracked file is left modified.

Then ask the owner: push `fix/api-parameter-parity` to the `fork` remote (`git push -u fork fix/api-parameter-parity`) and open a PR against `main`? Do not push without an answer.

---

## Out-of-scope observations (no task; for the owner)

- `kimi.ModelItem.MaxOutputTokens` (`json:"max_tokens"`) has no upstream source (N7).
- `thinking_budget: -1` is treated as "off" here and as "dynamic" by Gemini (N8); left alone, parity-tested.
- `appendClaudeCodeDiscovery` still carries a bare `8192` fallback for allowlist entries with no limit (`internal/api/discovery.go:147`); every current Claude model is 64K–128K.
- The 3.x Claude Code gateway defaults (`claude-3-*`, retired upstream) were not re-verified.
- `internal/claudecode/ccusage` `TestEngine_RecordWritesLedgerAndSnapshot` is time-of-day sensitive (fails just after 00:00 UTC on an untouched `main`); worth a deterministic clock.
- **Policy risk (N9):** Kimi Code's docs say tampering with the client User-Agent may suspend membership benefits; `internal/kimi/identity.go` presents `KimiCLI/1.0.0`.

## Verification Summary

| Gap / finding | Proof the fix holds |
|---|---|
| G1, G2, G9 | `TestResolveWithRequest_OutputConfigEffortRouting`, `TestOutputConfigEffortSetsBudgetOnBudgetStyleModels`, `TestLegacyGeminiPathHonorsEffortAndTopLevelBudget`, `TestDispatcherAppliesOutputConfigEffort`, `TestParse`, `TestSupported` — all red on the old code, green after |
| G13, G6 | `TestThinkingBudgetStaysBelowMaxOutputTokens`, `TestGeminiOutputIsCappedByTheLiveLimitNotAFixedCeiling` |
| G10 | `TestGeminiBudgetCeilingByModel` |
| G11 | `TestKimiModels_EntryWithoutLimitsAdvertisesAnOutputBelowItsContext` |
| N2 | `TestDefaultAllowlist_Haiku45MatchesAnthropicLimits` |
| N1, N5 | `TestServer_ForwardToKimi_PassesReasoningFieldsThrough`, `TestOutputShaper_LeavesOutputConfigEffortAlone` (guards, green before and after) |
| G8 | `TestRedactedThinkingFromAnotherBackendDoesNotReachCloudCode` (guard) |
| Real client shape | `TestClaudeCodeCapturedBodyShapeConvertsToACleanGenerationConfig` |
| G3, G4, G5, G7, GM | probe + capture evidence in `.reference/`, branch recorded in `params-verdicts-20261005.txt`; code only where Task 12 says |
| Live behavior | Task 14 step 3 |

## Risks

1. **D3 is a quota change.** Claude 4.6 requests move from the catalog default budget to 32000 at Claude Code's default effort. Mitigated by the GH numbers, the sign-off, GM (agy's own numbers win), and the one-line decline in D3.
2. **Wire-shape changes without evidence.** Tasks 5–11 change no wire shape the Cloud Code upstream has not already seen from the old code (budgets and levels come from the same fields); every change that does alter a shape (12a–12d) sits behind a recorded verdict.
3. **Probe results are account-dependent** (catalogs fluctuate per account, `.reference/agy-models-20260903.txt`). A 429 is `inconclusive`, never `rejected`; re-run the gate.
4. **CA trust and agy capture** modify machine state; the revocation step in Task 3 is mandatory and its `delete-certificate` must be confirmed. If agy pins its certificate, GM is `UNVERIFIED` and nothing in Task 12 that depends on it runs.
5. **`internal/format` is a port of the JS reference.** The parity fixture is the safety net; any fixture edit in 12a/12c is a deliberate, committed record of a spelling or sampling change.
6. **Task 11 widens scope** to the OpenAI endpoint; skip it if you want `/v1/messages` only.
