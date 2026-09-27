# Plan: Go port of ccusage's Claude Code usage/limit tracking

## Context
The WebUI and API show Google / Cloud Code quota as per-account, per-model data: `limits:{modelId:{remaining, remainingFraction, resetTime}}` and `quota.pools` (`gemini-5h`, `gemini-weekly`, …), polled from `GET /account-limits` and summarised by `GET /v1/usage`.

Claude Code accounts (`provider:"claudecode"`) only get a fraction taken from the classic `anthropic-ratelimit-*` headers (`internal/claudecode/ratelimit.go`). There is no 5h/weekly window tracking, no cost history and no burn rate.

ccusage (https://github.com/ccusage/ccusage) solves this from Claude Code JSONL logs: dedupe, LiteLLM pricing, 5-hour billing blocks, burn rate and projection, and daily/weekly/monthly/session reports. I researched its current Rust implementation (HEAD `bbbb9a1`) and the last TypeScript release (v18.0.11). The plan ports that logic natively in Go and presents it through the same interface the Google provider uses.

Decisions confirmed with the user:
- **Data source: both, merged.**
  - A proxy-written, ccusage-compatible ledger tagged with the account ID.
  - Plus local `~/.claude/projects` logs when they exist.
  - The two are deduplicated together.
- **Limit basis: headers first, then ccusage.**
  - Use the real `anthropic-ratelimit-unified-5h/7d-*` headers when present.
  - Otherwise use the ccusage projection against a configured per-account limit, or against `max` (the largest past block).

## Reference port map (ccusage → Go)
| ccusage source | Go target |
|---|---|
| `rust/adapters/claude/src/paths.rs` (`claude_paths`, `usage_files`) | `ccusage/paths.go` |
| `rust/crates/ccusage-core/src/types.rs` `UsageEntry` / TS `usageDataSchema` | `ccusage/entry.go` |
| `rust/adapters/claude/src/lib.rs` `push_deduped_entry`, `should_replace_deduped_entry` | `ccusage/dedupe.go` |
| `ccusage-core/src/pricing.rs` (`PricingMap::find`, `pricing_key_matches`), `cost.rs` (`tiered_cost`) | `ccusage/pricing.go`, `ccusage/cost.go` |
| TS `_session-blocks.ts` / Rust `blocks.rs` (`identifySessionBlocks`, `calculateBurnRate`, `projectBlockUsage`) | `ccusage/blocks.go` |
| `summary.rs` (`week_start`), TS `data-loader.ts` daily/monthly/session loaders | `ccusage/reports.go` |
| v17 `_live-monitor.ts` (incremental reload of files active in the last 24h) | `ccusage/engine.go` (poller) |

For every algorithm, use Rust HEAD behaviour where it differs from TS:
- the stricter dedupe;
- 1h cache writes priced at `input*2.0`;
- boundary-aware fuzzy model matching;
- `maxTokensFromAll` calculated over all non-gap, non-active blocks.

## New package: `internal/claudecode/ccusage/`
It is pure Go with no imports from the proxy. It is kept separate so it can be tested on its own.

1. **`entry.go`**
   - `Entry{Timestamp, SessionID, RequestID, MessageID, Model, Speed, Input, Output, CacheCreate5m, CacheCreate1h, CacheRead, CostUSD *float64, IsSidechain, IsAPIError, UsageLimitResetAt *time.Time, AccountID, Project, Source}`.
   - `ParseLine([]byte) (Entry, bool)` does the following:
     - Fast-reject lines that don't contain `"usage":{`.
     - Unmarshal only the fields ccusage reads.
     - Default the cache fields to 0.
     - When `cache_creation` is present, use `ephemeral_5m + ephemeral_1h`.
     - Expand `iterations[type=advisor_message]` into extra entries with ID `{msgId}:advisor:{i}`.
     - Parse the reset time from `"Claude AI usage limit reached|<epoch>"`.
     - Apply the `-fast` model suffix and exclude `<synthetic>` from model lists.

2. **`paths.go`**
   - `ClaudePaths()`:
     - If `CLAUDE_CONFIG_DIR` is set, split it on commas, trim, expand `~`, accept a path ending in `projects`, and require a `projects/` directory. Use only those paths.
     - Otherwise check `$XDG_CONFIG_HOME/claude` and then `~/.claude`.
     - Return an empty list if nothing is found. The proxy must not error on a headless host.
   - `UsageFiles()` walks `projects/**/*.jsonl`.
   - `sessionParts(path)` handles the `{proj}/{sid}.jsonl` and `{sid}/subagents/x.jsonl` layouts.

3. **`dedupe.go`**
   - Exact key:
     - with a `requestId`: `msgId:reqId`;
     - without one: `msgId|sessionId|tsMs`.
   - Sidechain-replay key: `sidechain-replay|msgId|sessionId`.
   - Which duplicate is kept:
     1. a non-sidechain entry beats a sidechain one;
     2. otherwise the entry with more total tokens wins;
     3. otherwise an entry with `speed` set wins.
   - Ledger entries and local-log entries for the same request share `msgId:reqId`, so a request is counted once. The ledger entry wins so its `AccountID` is kept.

4. **`pricing.go`**
   - `PricingMap` loaded from a `//go:embed` Claude-only snapshot of LiteLLM `model_prices_and_context_window.json`.
     - A `go generate` script fetches it and filters keys with the prefixes `claude-`, `anthropic.claude-` and `anthropic/claude-`.
   - Optional online refresh from `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json`:
     - off by default, via a config flag;
     - refreshed every 24h;
     - uses the standard `http.Client`. This is a different host from Cloud Code, so the TLS rule in AGENTS.md doesn't apply, but we still use the default transport.
   - Fields: input, output, cache_creation and cache_read, each with an `_above_200k_tokens` version, plus `max_input_tokens` and the fast multiplier.
     - Missing cache prices default to `input*1.25` for writes and `input*0.1` for reads.
     - The fast multiplier falls back to a small override table.
   - `Find(model)` tries, in order:
     1. an exact key;
     2. alias prefixes;
     3. a boundary-aware fuzzy match, normalising `.` and `@` to `-`, with the longest key winning.

     Results are memoised, including misses.
   - Fall back to the existing `claudecode.GetModelPricing` table (`internal/claudecode/pricing.go`) when LiteLLM has no entry, so current Claude 5 models keep a price.

5. **`cost.go`**
   - `CostMode` is `auto`, `calculate` or `display`.
   - `tiered(n, base, above, 200_000)` is applied to each token bucket.
   - 1h cache writes are priced at `input*2`.
   - The fast multiplier applies when `speed=="fast"`.

6. **`blocks.go`**
   - `IdentifyBlocks(entries, dur=5h, now)` is an exact port:
     - Floor the block start to the UTC hour.
     - Split when `ts-start > D` or `ts-last > D`.
     - Create gap blocks (`isGap`, id `gap-<iso>`).
     - A block is active when `now-lastEntry < D && now < end`.
   - `BurnRate(block)` computes tokens per minute, tokens per minute for the indicator (input+output only), and cost per hour, measured from the first entry to the last.
   - `Project(block, now)` returns the projected tokens and cost and the remaining minutes.
   - `MaxTokensFromHistory(blocks)`.
   - `Status(projected, limit)` returns `ok`, `warning` (above 0.8) or `exceeds`.

7. **`reports.go`**
   - Daily, monthly, weekly (configurable start of week, plain date arithmetic) and session aggregations.
   - Each row has per-model breakdowns sorted by cost, descending. Accounts are optional.
   - A timezone setting controls the date keys.

8. **`ledger.go`**
   - `Ledger` is an append-only JSONL writer at `<configDir>/claudecode-usage/YYYY-MM.jsonl`.
   - Each line uses the same schema Claude Code writes: `timestamp`, `sessionId`, `requestId`, `message.{id, model, usage{…}}`, `costUSD`, plus an extra `accountId` field.
     - The real `ccusage` CLI can therefore read these files directly by pointing `CLAUDE_CONFIG_DIR` at them. This is a cheap way to cross-validate.
   - Writes are serialised by a mutex and fsynced on rotation.
   - Retention defaults to 60 days.

9. **`engine.go`**
   - `Engine` owns the merged entry set.
   - Startup does a full load:
     - all ledger files;
     - local logs for 8 days, enough for the weekly window.
   - It then reloads incrementally, using the v17 live-monitor approach: re-read only files whose mtime or size changed, and only read bytes past the saved offset.
   - It keeps a sorted, deduplicated slice guarded by an `RWMutex`.
   - `Record(Entry)` is the hot path from the proxy. It appends to the ledger and inserts into memory.
   - `Snapshot(accountID, now) AccountUsage` returns:
     - the active block and its burn rate and projection;
     - the 5h and 7d token and cost sums;
     - the `max` limit taken from history;
     - today's cost;
     - the per-model split.
   - The snapshot is cached for 5s.
   - Local-log entries have no account. They are attributed to a configurable `localAccountId` if one is set; otherwise they appear under a synthetic "local" row.

## Unified-header limits (headers first)
- Extend `internal/claudecode/ratelimit.go` `ExtractRateLimits` and `RateLimits` (`types.go:187`) with:
  - `anthropic-ratelimit-unified-status`
  - `-unified-5h-utilization` / `-5h-reset` / `-5h-status`
  - `-unified-7d-utilization` / `-7d-reset` / `-7d-status`
  - `-unified-representative-claim`
  - `-unified-fallback-percentage`
- Store the parsed values as new JSON fields `unified5h{utilization, resetAt, status}` and `unified7d{…}`.
- `MinRemainingFraction` (`:134`) should prefer the unified windows when they are present.
- The exact header names must be confirmed against real responses captured through the proxy before the parser is finalised.
  - The capture happens through the existing `claudecode_proxy.go` header logging, not by guessing.
  - Parse leniently and ignore headers we don't recognise.

## Wiring into the proxy
- **Hot path:** `recordClaudeCodeMetrics` (`internal/api/claudecode_proxy.go:223`) is already the single place usage is finalised.
  - Extend `ccAttempt` and the signature to carry `requestID` (from the upstream `request-id` response header), the SSE `message_start.message.id` / JSON `id`, and the 5m and 1h cache split when the upstream sends `cache_creation`.
  - Then call `server.ccusage.Record(entry)` next to `pool.RecordSuccess`.
  - Cover the call sites at `:393`/`:526` (the stream and non-stream paths) and `internal/api/cachebump_server.go:229`.
- **Construction:**
  - The engine is built in `api.New`. `Options` gains `CCUsage *ccusage.Engine`, which tests can set to nil.
  - Config lives under `claudecode.usage` in `claudecode.Config` (`internal/claudecode/types.go:175`): `{enabled, ledgerDir, scanLocalLogs, localAccountId, sessionHours:5, tokenLimit5h per-account|"max", weeklyTokenLimit, costMode:"auto", timezone, onlinePricing:false}`.
  - Per-account overrides go in `AccountConfig` (`types.go:16`) as `usageLimits{tokens5h, tokens7d, costUsd5h}`.

## Presenting it through the Google-shaped interface
1. **`/account-limits`** (`internal/api/management.go:691`, the Claude Code rows). Each row gains:
   - `quota: {models:{…}, pools:{"claude-5h":ModelQuota, "claude-weekly":ModelQuota}, lastChecked}`, reusing `accounts.ModelQuota{remainingFraction, resetTime}` (`internal/accounts/manager.go:53`) so the shape is identical to Google's `gemini-5h`/`gemini-weekly` pools.
     - `claude-5h`:
       - with unified headers: fraction = `1 - utilization`, reset = the 5h reset time;
       - otherwise: fraction = `1 - projectedTokens/limit`, with the limit being the configured one or `max`; reset = `activeBlock.end`;
       - `null` when there is no active block and no limit.
     - `claude-weekly`:
       - with unified headers: the 7d utilization;
       - otherwise: the rolling 7-day token sum against the configured weekly limit. `null` if no limit is configured, because ccusage has no weekly limit to infer from.
   - `limits[modelId]`: each model's `remainingFraction` is the minimum across the pools that apply to it, the same way `quotaPoolsForModel` maps models to pools (`manager.go:1559`). If no pool data exists, fall back to today's header fraction.
   - `usage: {activeBlock{start,end,tokens,costUSD,models,burnRate{tokensPerMinute,tokensPerMinuteForIndicator,costPerHour},projection{tokens,costUSD,remainingMinutes},status,limit,limitSource:"headers"|"config"|"max"}, window5h, window7d, todayCostUSD, source:"ccusage-go"}`.
   - `totalTokens` and `totalCost` stay unchanged.
2. **`GET /v1/usage`** (`server.go:490`): when the Claude Code gateway is enabled, add `claude` entries to `windows` (`groupQuotaWindows` `:521`) in the existing `{id, remaining_fraction, used_percent, reset_at}` form.
3. **New `GET /api/claudecode/usage`**, registered in `claudecode_management.go:335-385`, protected by the same WebUI password:
   - `?report=daily|weekly|monthly|session|blocks`, plus `since`, `until`, `account`, `timezone`, `recent`, `active`.
   - Returns the ccusage JSON report shapes for drill-down views.
   - `POST /api/claudecode/usage/reload` forces a rescan.
4. **WebUI** (`internal/webui/public/`):
   - `js/data-store.js` `computeQuotaRows` (`:337`) needs no change for per-model fractions. It already reads `acc.limits`.
   - Add a pools renderer, which also fixes the existing gap where Google `quota.pools` is never rendered:
     - a shared `views/partials`-style block in `views/accounts.html`, in both the `quota_modal` (`:685`) and the account card;
     - it shows each pool as a bar with a reset countdown via `utils.formatTimeUntil`.
   - On Claude Code account cards and in the quota modal, add an active-block panel: tokens and cost, burn-rate indicator (thresholds 1000/500 tokens per minute as in v17 live mode), a projection bar coloured red above 100% and yellow above 80%, the time left, and the limit source badge.
   - Add a small usage-history view (daily/weekly table) fed by `/api/claudecode/usage`, in `js/components/account-manager.js`.
   - Add translation keys to `js/translations/en.js` and `pt.js`. `translations_test.go` enforces parity.
   - Update the placeholder data (`data-store.js:560-690`) with `quota.pools` and `usage` for Claude Code.

## Tests
Tests follow the existing patterns: table-driven `*_test.go` files next to the code, with the `newTestServerWithManager` helper in `internal/api/management_test.go:25`.
- `ccusage/*_test.go`:
  - golden fixtures in `testdata/` — JSONL copied from ccusage's own test cases (dedupe, sidechain replay, advisor iterations, gaps);
  - block splitting at exactly 5h and 5h + 1ms;
  - UTC-hour flooring;
  - burn rate with a single entry returning nil;
  - tiered pricing just above and below 200k;
  - fuzzy match boundaries (`claude-opus-4` must not match `claude-opus-4-5`);
  - weekly start-of-week.
- **Parity test** (`go test -tags ccusage_parity`):
  - Runs `npx ccusage@latest blocks --json --offline` and `daily --json` on the fixture directory and diffs them against the Go output.
  - It is skipped when `npx` is absent, following the same model as the node-gated `kimi_poll_race_test.go`.
- `ratelimit_test.go`: unified header parsing.
- `management_test.go`:
  - `TestManagement_AccountLimits_ClaudeCodeUsagePools`, covering the headers source, the max-history source and the null case;
  - a test for the `/api/claudecode/usage` report.
- WebUI: extend `translations_test.go`, and add a node harness test for the pool rendering helper if it contains logic.

## Suggested implementation order
1. `ccusage` package: entry, dedupe, pricing, cost, blocks, reports, with fixtures and parity.
2. Ledger, engine, and the `recordClaudeCodeMetrics` hook with IDs.
3. Unified header parsing.
4. `/account-limits`, `/v1/usage`, and the `/api/claudecode/usage` API.
5. WebUI.
6. `graft build`, gofmt, and `make test`.

## Verification
- `go build -o bin/proxy ./cmd/proxy && make test`, with `-race`.
- Parity: `CLAUDE_CONFIG_DIR=<fixture> npx ccusage@latest blocks --json --offline` should match the Go `/api/claudecode/usage?report=blocks` output for the same fixture.
- Ledger cross-check: point `CLAUDE_CONFIG_DIR` at the ledger directory and confirm the real `ccusage daily` totals equal the proxy's report.
- Live run:
  1. Start the proxy on :8091 with a Claude Code account.
  2. Send a few `/v1/messages` requests.
  3. `curl -H 'x-webui-password: …' localhost:8091/account-limits | jq '.accounts[]|select(.provider=="claudecode")|{quota,usage,limits}'`.
  4. Open the WebUI Accounts and Models views and confirm the `claude-5h`/`claude-weekly` bars, the countdowns and the burn-rate panel.
- Fingerprint gate: the tshark JA4 check in AGENTS.md is unchanged. The work adds no Cloud Code transport changes, and the online pricing fetch is off by default and never shares the Cloud Code client.
