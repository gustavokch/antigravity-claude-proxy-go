# Plan: Go port of ccusage's Claude Code usage/limit tracking

_Revision 2 (2026-09-27): rewritten after an adversarial review. The "Review
changes" section says what changed from revision 1 and why._

## Context
The WebUI and API show Google / Cloud Code quota as per-account, per-model data: `limits:{modelId:{remaining, remainingFraction, resetTime}}` and `quota.pools` (`gemini-5h`, `gemini-weekly`, `3p-5h`, `3p-weekly`). This data is polled from `GET /account-limits` and summarised by `GET /v1/usage`.

Claude Code accounts (`provider:"claudecode"`) only get a fraction from the classic `anthropic-ratelimit-{requests,tokens,…}-*` headers (`internal/claudecode/ratelimit.go`).
- Subscription (OAuth) accounts never receive those headers. The captures in `.reference/claude-code-headers-20260923*.jsonl` carry only `anthropic-ratelimit-unified-*`.
- So for the accounts that matter, `limits` is always `null` today. `TestManagement_AccountLimits_ClaudeCodeUnknownQuotaIsNull` pins that behaviour.
- There is no 5h/weekly window, no cost history and no burn rate.

ccusage (https://github.com/ccusage/ccusage) computes all of this from Claude Code JSONL logs: dedupe, LiteLLM pricing, 5-hour billing blocks, burn rate and projection, and daily/weekly/monthly/session reports.
- Its current implementation is Rust (HEAD `bbbb9a1`). The last TypeScript release is v18.0.11.
- This plan ports that logic to Go and presents it through the interface the Google provider already uses.

Decisions confirmed with the user:
- **Data source: both, merged.** A ccusage-compatible ledger written by the proxy, plus local Claude Code logs. The review below adds a filter the local logs must pass.
- **Limit basis: headers first, then ccusage.** The unified headers are the source of truth. ccusage math is the fallback and provides the projection.

## Review changes (revision 1 → 2)
Each point was verified against the code or the reference captures.

1. **The unified headers are already captured, so there is no need to "capture first".** `.reference/claude-code-headers-20260923{,-run2,-acct2}.jsonl` has the real names and formats:
   - `-unified-{status,reset,representative-claim,fallback-percentage}`
   - `-unified-5h-{status,reset,utilization}` and `-unified-7d-{status,reset,utilization}`
   - `-unified-overage-{status,disabled-reason}`

   Utilization is a **0–1 fraction with 2-decimal resolution** (for example `"0.04"`). Resets are **epoch seconds**, not the RFC 3339 format the classic headers use.
2. **ccusage's UTC-hour flooring does not match the real window.**
   - The captured 5h resets are 12:10 and 22:20 UTC, so real windows start at :10 and :20, not on the hour.
   - The weekly reset is a fixed weekly anchor (09:00 UTC, moving 09-23 → 09-30), not a rolling 7 days.
   - When headers exist, the window is anchored on them. The ccusage floor is used only when no reset has ever been seen.
3. **`remainingFraction` must describe the present, not the projection.** Revision 1 used `1 - projected/limit`. With two entries a minute apart, the burn rate extrapolates to a huge projection and reads 0% at the start of a block. Google's field means "remaining now", so the projection is shown separately.
4. **API-key accounts have no subscription windows.** Synthesising `claude-5h`/`claude-weekly` pools for `type=="api_key"` would invent limits. Pools apply only to `oauth`/`setup_token` accounts, or to accounts where the user configured explicit limits.
5. **Local logs contain every gateway's traffic.** Claude Code is this proxy's client, so `~/.claude/projects` also records turns served by Cloud Code, Kimi, Zen and OpenRouter.
   - Counting those turns would charge Google-served work against Anthropic windows.
   - The Cloud Code path never emits `request-id`. Only `ccCopyResponseHeaders` (`claudecode_proxy.go:609`) forwards it.
   - Translated responses use proxy-made IDs: `msg_`+32 hex (`internal/format/response.go:8`, `stream.go:28`), `msg_zen_`, `msg_clf_`, `msg_stub_`. These make a reliable filter (see "Local-log admission").
6. **A local-only "local" account row would break the WebUI.** Toggle, refresh and delete would act on a nonexistent ID. Unattributed local usage goes to reports only, never to account rows or pools.
7. **The usage capture point is too thin.** `recordClaudeCodeMetrics` (`claudecode_proxy.go:223`) gets `(in, out, cr, cw int)` from the shared `openrouter.NewSSEInterceptor` / `ParseUsageFromJSON`. Six other call sites use them (`server.go:1585,1603,1783,1801,2706,3002`), and they carry no message ID, served model, 5m/1h cache split, speed or iterations.
   - Add a separate Anthropic-specific detailed capture.
   - Do not change the shared signature.
8. **Cache-bump traffic is missing.** The bumps in `cachebump_server.go` (`:247`, `:331`) use up the subscription window ("a bump burns the same window a real turn does", `:227`). They never pass through `recordClaudeCodeMetrics` and never appear in local logs, so they must be written to the ledger too.
9. **429 cooldown on subscription accounts is broken today.**
   - `RecordRateLimit` (`pool.go:553`) only understands `retry-after` and the classic resets. Otherwise it falls back to the 10s default.
   - So an OAuth account whose 5h window is exhausted is retried every 10s for hours.
   - Fix: when unified status is `rejected`, cool down until the binding window's reset.
10. **Unified limits must survive a restart.** `RateLimits` is not written by `storage.go`, so after a restart the pools stay blank until the next request. Persist the unified snapshot with `observedAt`. Once its reset time has passed, treat that window as unknown.
11. **The ledger write must not happen on the stream path.** The interceptor's `onComplete` runs synchronously inside `Read`/`Close` (`openrouter/observability.go:469`). Use a buffered channel and a single writer goroutine.
12. **The ledger layout must be readable by ccusage.** ccusage only globs `<dir>/projects/**/*.jsonl`, so the cross-check needs that layout. Rust ccusage rejects a non-semver `version`, so omit it.
13. **Costs must come from one source.** Pool `TotalCost` (`GetModelPricing` via `ComputeFinalMetrics`) and ccusage's LiteLLM cost would show two different numbers on the same card.
    - Use one pricer.
    - Consult the curated in-repo table first, because it has the current Claude 5 models. LiteLLM is second.
    - Label costs as "API-equivalent", because subscription accounts don't pay per token.
14. **The fallback basis should be cost, not tokens.** Cache reads dominate token counts and cost little against limits. The fallback limit is calibrated from headers: `implied = windowCost / utilization`, only when utilization ≥ 0.20, because 2-decimal resolution is too coarse below that. The ccusage `max` block is the last resort.
15. **Revision 1 got the `/v1/usage` shape wrong.**
    - Windows are `{label, remaining_fraction, used_percent, reset_at, model_ids}`. There is no `id`.
    - `usage()` (`server.go:490`) returns an error when the Cloud Code catalog fetch fails.
    - Claude windows must not depend on that fetch, and must name the account and provider they belong to.
16. **`quotaPoolsForModel` must not be reused.** It maps `claude-*` to Google's `3p-*` pools (`accounts/manager.go:1565`), which are Claude-on-Cloud-Code. The Claude Code gateway needs its own mapping.
17. **Parity with `npx ccusage@latest` is nondeterministic.** Pin the version, use fixtures dated in the past so no block is active, and keep the test behind a build tag.
18. **Memory and file handling.** Keep only 8 days of entries plus a per-account summary in memory, and stream older reports from disk. The incremental reader must handle files that shrink or are rewritten (`size < offset` means reread) and a partial last line.
19. **Split into four PRs.** The header work alone delivers most of the value and is small.

## Reference port map (ccusage → Go)
| ccusage source | Go target |
|---|---|
| `rust/adapters/claude/src/paths.rs` (`claude_paths`, `usage_files`) | `ccusage/paths.go` |
| `rust/crates/ccusage-core/src/types.rs` `UsageEntry` | `ccusage/entry.go` |
| `rust/adapters/claude/src/lib.rs` `push_deduped_entry`, `should_replace_deduped_entry` | `ccusage/dedupe.go` |
| `ccusage-core/src/pricing.rs` (`PricingMap::find`, `pricing_key_matches`), `cost.rs` (`tiered_cost`) | `ccusage/pricing.go`, `ccusage/cost.go` |
| `rust/crates/ccusage/src/blocks.rs` (TS `_session-blocks.ts` for readability) | `ccusage/blocks.go` |
| `summary.rs` (`week_start`), daily/monthly/session loaders | `ccusage/reports.go` |
| v17 `_live-monitor.ts` (incremental reload of recently changed files) | `ccusage/engine.go` |

Port Rust HEAD semantics consistently, including `maxTokensFromAll` computed after filters, so the pinned Rust release is the single target for the parity test. Keep ccusage's MIT notice in the copied fixtures.

## PR 1: Unified subscription limits (headers first)
This PR has no ccusage code yet. It delivers Google-shaped pools for OAuth accounts.

- **`internal/claudecode/types.go` / `ratelimit.go`**
  - Add `Unified{Status, Reset time.Time, RepresentativeClaim, FallbackPct, OverageStatus, OverageDisabledReason, FiveHour, SevenDay UnifiedWindow, ObservedAt}` with `UnifiedWindow{Utilization *float64, Reset time.Time, Status}`.
  - `ExtractRateLimits` parses these headers. Resets are epoch seconds. Parse leniently and ignore unknown `-unified-*` headers.
  - `HasLimits()` is true when unified data is present.
  - `MinRemainingFraction` returns `1 - max(util5h, util7d)` over windows whose reset is still in the future, and prefers them over the classic dimensions.
  - `IsRateLimited` returns true when unified status is `rejected` and the binding reset is in the future.
- **`pool.go` `RecordRateLimit`:** when unified status is `rejected`, set the cooldown to the reset of the representative claim's window, falling back to `unified-reset`. `retry-after`, if present, still wins when it is longer.
- **Persistence:** store the `Unified` snapshot per account in the claudecode account store (`storage.go`). Reload it on start and drop windows whose reset has passed.
- **`/account-limits` Claude Code rows** (`management.go:691`):
  - `quota: {models:{}, pools:{"claude-5h", "claude-weekly"}, lastChecked}`, using `accounts.ModelQuota` so the shape is identical to Google's.
  - `limits[modelId]` = the minimum over the Claude pools. Every Claude Code model maps to both pools through a local helper; do not use `quotaPoolsForModel`.
  - `api_key` accounts keep today's classic-header behaviour and get no subscription pools.
  - Update `…UnknownQuotaIsNull` to cover API-key and no-data accounts. Add `…UnifiedPools`, which uses real header values copied from `.reference`.
- **Header forwarding (optional, flag-gated, off by default):** add the unified headers to `ccCopyResponseHeaders`, so a Claude Code client behind the proxy keeps its native limit warnings. This is off by default because it changes what the client sees. Confirm with the user before enabling it.

## PR 2: `internal/claudecode/ccusage/` (pure library)
This package imports no proxy code. Pricing is injected through an interface to avoid an import cycle with `claudecode`.

1. **`entry.go`**
   - `Entry{Timestamp, SessionID, RequestID, MessageID, Model, Speed, Input, Output, CacheCreate5m, CacheCreate1h, CacheRead, CostUSD *float64, IsSidechain, IsAPIError, UsageLimitResetAt, AccountID, Project, Source}`.
   - `ParseLine` does the following:
     - fast-rejects lines without `"usage":{`;
     - unmarshals a minimal schema;
     - when `cache_creation` is present, uses `ephemeral_5m + ephemeral_1h`;
     - expands `advisor_message` iterations into `{msgId}:advisor:{i}`;
     - parses the `Claude AI usage limit reached|<epoch>` reset time;
     - applies the `-fast` model suffix and excludes `<synthetic>` from model lists.
2. **`paths.go`**
   - `CLAUDE_CONFIG_DIR` is comma-separated. Expand `~` and accept a path that already ends in `projects`.
   - Otherwise check `$XDG_CONFIG_HOME/claude` and then `~/.claude`.
   - An empty result is not an error.
   - Session parts support the `{proj}/{sid}.jsonl` and `{sid}/subagents/*.jsonl` layouts.
3. **`dedupe.go`:** Rust rules.
   - Keys:
     - with a `requestId`: `msgId:reqId`;
     - without one: `msgId|sessionId|tsMs`;
     - sidechain replays: `sidechain-replay|msgId|sessionId`.
   - The winner is a non-sidechain entry, then more tokens, then an entry with `speed` set.
   - **Deliberate deviation:** a ledger entry always beats a local entry with the same key. The ledger holds the final usage and the account, while Claude Code writes several lines per message with partial output counts.
4. **`pricing.go` + `cost.go`**
   - `Pricer` interface.
   - The default chain is:
     1. the curated `claudecode.GetModelPricing` table, injected;
     2. an embedded Claude-only LiteLLM snapshot (`go generate` filters `claude-`, `anthropic.claude-`, `anthropic/claude-`);
     3. an optional online refresh, off by default, using the standard `http.Client`. It never shares the Cloud Code transport.
   - Fuzzy matching is boundary-aware: normalise `.`/`@` to `-`, require non-alphanumeric boundaries, reject a trailing `-<digits>` unless it is an 8-digit date, and prefer the longest key. Results are memoised.
   - Tiered pricing uses 200k per bucket. 1h cache writes cost `input*2`. The fast multiplier applies. Cost modes are `auto`, `calculate` and `display`.
5. **`blocks.go`**
   - `IdentifyBlocks(entries, dur, now, anchors)`.
   - `anchors` is a list of known window starts (`5h-reset − 5h`) taken from the unified headers.
     - An entry inside an anchored window joins that window's block, and the block's start is the anchor.
     - Without anchors, use the exact ccusage behaviour: UTC-hour floor, split when `ts-start > D || ts-last > D`, and create gap blocks.
   - `BurnRate` returns nil for fewer than 2 entries or 0 duration. It includes the indicator TPM (input+output).
   - `Project` returns projected tokens and cost and the remaining minutes.
   - `MaxFromHistory` over non-gap, non-active blocks. `Status` returns ok, warning (above 0.8) or exceeds.
6. **`reports.go`:** daily, weekly (start of week configurable, plain date arithmetic), monthly, session and blocks. Models are broken down by cost, descending. A timezone setting controls the date keys. Accounts are optional.

Tests:
- Table-driven, with fixtures in `testdata/`.
- Cases:
  - block edges at exactly 5h and at 5h+1ms;
  - anchored versus floored blocks;
  - a gap block;
  - a single-entry burn rate returning nil;
  - tiered pricing at 200k ± 1;
  - `claude-opus-5` must not match `claude-opus-5-5`;
  - sidechain replay;
  - advisor iterations.
- Parity test behind `//go:build ccusage_parity`:
  - `npx ccusage@<pinned> {daily,blocks} --json --offline`, with a fixed `TZ` and fixtures dated in the past;
  - skipped when `npx` is absent (same model as `internal/webui/kimi_poll_race_test.go`).

## PR 3: Ledger, engine, and proxy wiring
- **Detailed capture:**
  - Add a new detailed SSE/JSON capture in `internal/claudecode`, called for example `usagecapture.go`. It records:
    - `message.id` and served `message.model` from `message_start`;
    - usage from `message_start` and `message_delta`, including `cache_creation.ephemeral_{5m,1h}_input_tokens`, `speed` and `iterations`.
  - `ccInstrumentResponse` (`claudecode_proxy.go:263`) uses it instead of the OpenRouter interceptor. The other six callers are untouched.
  - `request-id` comes from `resp.Header`.
  - `recordClaudeCodeMetrics` takes a `claudecode.Usage` struct instead of 4 ints. `metrics.CallCost` and the ledger `costUSD` both come from the shared `Pricer`.
- **Cache bump:** `cachebump_server.go` records each bump to the ledger with `source:"cachebump"`.
- **`ledger.go`:**
  - Files live at `<configDir>/claudecode-usage/projects/<accountId>/<YYYY-MM-DD>.jsonl`.
  - Each line uses Claude Code's schema (`timestamp`, `sessionId`, `requestId`, `message{id, model, usage}`, `costUSD`) plus `accountId` and `source`. No `version` is written.
  - One writer goroutine is fed by a buffered channel (e.g. 4096). On overflow the entry is dropped and a counter is exposed in the stats.
  - Files are fsynced when they rotate and at shutdown. Retention defaults to 60 days.
- **Local-log admission.** A local entry counts only if:
  - its `requestId` starts with `req_`;
  - its message ID does not match the proxy-generated patterns (`^msg_[0-9a-f]{32}$`, `^msg_(zen|clf|stub)_`);
  - its model starts with `claude-`.

  Entries that dedupe against the ledger inherit the ledger's account. The remaining entries are direct-to-Anthropic usage:
  - They are attributed to `localAccountId` if it is configured. Otherwise, if exactly one auto-imported account (`Source=="auto_import"`) exists, they are attributed to it and the UI shows this as inferred.
  - Otherwise they are unattributed and go to reports only.
- **`engine.go`:**
  - At startup, load the last 8 days of the ledger and of local logs that pass the filter.
  - Tail changed files by offset, handling truncation and partial lines.
  - Keep a per-account summary (max block, calibrated limit) and persist it next to the ledger.
  - `Snapshot(accountID, now)` returns the window blocks, burn rate, projection, the 5h/7d cost and tokens, today's cost and the per-model split. It is cached for 5s.
  - Construct it in `api.New`. `Options.CCUsage` may be nil in tests.
- **Config:** `claudecode.usage` in `claudecode.Config` (`types.go:175`) holds `{enabled, ledgerDir, scanLocalLogs, localAccountId, sessionHours:5, costMode:"auto", timezone, onlinePricing:false, retentionDays:60}`. `AccountConfig` holds per-account `usageLimits{costUsd5h, costUsd7d, tokens5h, tokens7d}`.
  - `scanLocalLogs` defaults to true when a Claude config dir exists, matching the "both, merged" decision. Revisit this default if local-log privacy matters for server deployments.

## PR 4: Fallback math, APIs, WebUI
- **Pool values**, in order of precedence:
  1. **Unified headers**, when they are fresh (the reset is in the future): `remainingFraction = 1 - utilization`, `resetTime` = the header reset, source `headers`.
  2. **Calibrated fallback**, for OAuth accounts whose headers are stale or missing:
     - `remaining = 1 - windowCost/impliedLimit`, clamped to [0,1].
     - The window uses the last known anchor, stepping forward in whole 5h or 7d periods. If there is no anchor, use the ccusage floored block for 5h and a rolling 7d window with no `resetTime`.
     - Source `calibrated`.
  3. **Configured limits** (`usageLimits`): source `config`.
  4. **ccusage `max` block**, for 5h only: source `max`.
  5. Otherwise `null`.
- **Projection:** `projectedUtilization = utilization × projectedCost/currentCost` for the active window. It is exposed in `usage` and never in `remainingFraction`.
- **`/account-limits`** gains `usage: {window5h{start,end,costUSD,tokens,utilization,projectedUtilization,status,source}, window7d{…}, burnRate{tokensPerMinute,tokensPerMinuteForIndicator,costPerHour}, todayCostUSD, costBasis:"api-equivalent"}`. `totalCost` comes from the same pricer.
- **`/v1/usage`:**
  - Build Claude windows independently of the Cloud Code catalog fetch, so a Cloud Code error does not block them. Only an error from both sides fails the call.
  - Append windows `{label:"Claude 5h (<account>)", remaining_fraction, used_percent, reset_at, model_ids, provider:"claudecode", account_id}`. The new fields are additive and existing fields are unchanged.
- **New `GET /api/claudecode/usage`** (WebUI password):
  - `report=daily|weekly|monthly|session|blocks`, plus `since`, `until`, `account`, `timezone`, `recent`, `active`.
  - Includes a synthetic `unattributed` account key in reports only.
  - `POST /api/claudecode/usage/reload`.
- **WebUI** (`internal/webui/public/`):
  - A shared pool-bar renderer for `quota.pools`, used by Google and Claude alike. This also fixes the fact that Google pools are never rendered today. Put it in the `views/accounts.html` card and in the `quota_modal` (`:685`).
  - A Claude window panel: utilization now, a projected marker (red above 100%, yellow above 80%), a countdown via `utils.formatTimeUntil`, a burn indicator (above 1000 tokens/min HIGH, above 500 MODERATE), a source badge (headers / calibrated / config / max), and a cost labelled "API-equivalent".
  - A history table from `/api/claudecode/usage` in `js/components/account-manager.js`.
  - `en`/`pt` translation keys (parity is enforced by `translations_test.go`).
  - Update the placeholder data (`data-store.js:560-690`).

## Verification
- On every PR: `gofmt`, `go build -o bin/proxy ./cmd/proxy`, and `make test` (`-race`).
- PR 1:
  - A unit test feeds the header set from `.reference/claude-code-headers-20260923.jsonl` into `ExtractRateLimits` and expects 5h utilization 0.04 with reset `2026-09-23T12:10:00Z`, and 7d utilization 0.22.
  - A pool test simulates a `rejected` 429 and expects the cooldown to equal the 5h reset, not 10s.
  - Restart persistence test.
- PR 2: unit tests plus `go test -tags ccusage_parity ./internal/claudecode/ccusage/` with the pinned ccusage.
- PR 3:
  - A ledger cross-check: `CLAUDE_CONFIG_DIR=<configDir>/claudecode-usage npx ccusage@<pinned> daily --json` totals must equal `/api/claudecode/usage?report=daily`.
  - A local-log filter test with fixtures that mix Anthropic `req_…`/`msg_01…` lines with proxy-generated `msg_<32hex>` lines that have no `requestId`.
- Live run:
  - Start the proxy on :8091 with an OAuth Claude Code account and send a few `/v1/messages` requests.
  - Run `curl -H 'x-webui-password: …' localhost:8091/account-limits | jq '.accounts[]|select(.provider=="claudecode")|{quota,usage,limits}'`. Pools should show `source:"headers"` and match the `anthropic-ratelimit-unified-*` values the proxy logged.
  - Then check the WebUI Accounts and Models views.
- Fingerprint gate: the tshark JA4 check in AGENTS.md must still pass. There are no Cloud Code transport changes, and online pricing is off by default and uses its own client.
- The graft index is not installed in this environment. Run `graft build` where it is available.
