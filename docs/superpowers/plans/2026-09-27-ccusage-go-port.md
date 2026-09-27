# ccusage Go Port Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Track Claude Code subscription usage and limits natively in Go, porting ccusage's logic, and expose it through the same account/quota interface that Google / Cloud Code already uses in the Go API and the WebUI.

**Architecture:**
- Anthropic's `anthropic-ratelimit-unified-*` response headers are the source of truth for the 5h and weekly windows.
- A pure-Go port of ccusage (`internal/claudecode/ccusage`) supplies cost, blocks, burn rate, projection, reports, and a fallback limit estimate when headers are stale.
- Usage comes from two places, merged and deduplicated:
  - a ccusage-compatible JSONL ledger that the proxy writes for every Claude Code response and every cache bump;
  - the local Claude Code logs, filtered so that turns served by other gateways are excluded.
- Results appear as `quota.pools` (`claude-5h`, `claude-weekly`), `limits`, and a `usage` object on `/account-limits`, as Claude windows on `/v1/usage`, as a new `/api/claudecode/usage` report API, and in the WebUI.

**Tech Stack:** Go 1.27rc2 standard library, Alpine.js WebUI (no build step), Node for the optional parity test.

**Spec:** `docs/plans/2026-09-27-ccusage-go-port.md`, revision 2. It is the design, and it is binding. When a task below is terse, the spec section named in the task has the detail.

**ccusage reference source:** read-only clones, used only as a reference.
- Location: `/tmp/claude-0/-home-user-antigravity-claude-proxy-go/88d1769b-59c5-570d-aac7-8e217d393941/scratchpad/`
  - `ccusage/`: HEAD `bbbb9a1`, npm version 20.0.26, Rust implementation under `rust/`.
  - `ccusage-18/`: the last TypeScript release.
  - `ccusage-ts/`: v17, which has the live monitor.
- Port Rust HEAD semantics.

**User decisions (binding):**
- Data source: the proxy ledger plus local Claude Code logs, merged. Local-log scanning is on by default when a Claude config dir exists.
- Limit basis: unified headers first, then the ccusage fallback.
- Unified headers are forwarded to downstream clients by default, behind `claudecode.forwardUnifiedHeaders` (default `true`).

## Global Constraints

- **Branch and commits.** All work goes on branch `claude/youthful-thompson-6v88dy`.
  - Make one commit per task, and commit only the files that task touches.
  - Implementers commit but never push. The controller pushes.
  - Every commit message ends with these two trailer lines:
    ```
    Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
    Claude-Session: https://claude.ai/code/session_015TdxRSExihFWsEEFkydwch
    ```
  - Never put a model identifier in code, comments or docs.
- **AGENTS.md is binding.**
  - Do NOT touch TLS internals or the Cloud Code transport (`internal/cloudcode` client, dialer, and HTTP transport setup).
  - The optional online pricing fetch uses its own plain `http.Client` and is off by default.
- **Format, lint and test.** Code must be `gofmt`-clean and pass `go vet ./...`.
  - Run the changed packages' tests with `-race` before committing.
  - Run the full `go test ./...` at the end of each task. It takes about a minute.
- **Test style.** Use stdlib `testing` with `t.Errorf` / `t.Fatalf` and no assertion libraries. Table-driven tests where it fits.
  - Follow existing helpers: `newTestServerWithManager` in `internal/api/management_test.go`, and `newDispatcherWithClient` in `internal/accounts/dispatcher_test.go`.
- **Existing tests.** Do not edit existing test functions. The only exception is the one the spec names: `TestManagement_AccountLimits_ClaudeCodeUnknownQuotaIsNull`, which may be narrowed to API-key and no-data accounts. New test functions are fine.
- **Ground truth for unified headers:** `.reference/claude-code-headers-20260923.jsonl`, `-run2.jsonl` and `-acct2.jsonl`.
  - Utilization is a 0–1 decimal string (for example `"0.04"`).
  - Resets are Unix epoch seconds.
  - Header names:
    - `anthropic-ratelimit-unified-{status,reset,representative-claim,fallback-percentage,overage-status,overage-disabled-reason}`
    - `anthropic-ratelimit-unified-5h-{status,reset,utilization}`
    - `anthropic-ratelimit-unified-7d-{status,reset,utilization}`
- **Style.** Match the surrounding code: comment density, naming, error handling, `slog` logging. Comments and docs are plain English prose.
- **Hot path.** Capture, ledger and engine failures are logged and swallowed. They must never fail, delay or change a user request. Ledger disk I/O never runs on the response-stream goroutine.
- **Unrelated files.** The working tree may contain untracked files unrelated to this work. Never add them.

---

## PR 1: Unified subscription limits

### Task 1: Parse unified rate-limit headers
Spec: "PR 1", first two bullets; review items 1 and 2.

**Files:** `internal/claudecode/types.go`, `internal/claudecode/ratelimit.go`, `internal/claudecode/ratelimit_test.go`.

- [ ] Add `UnifiedWindow{Utilization *float64, Reset time.Time, Status string}`.
- [ ] Add `Unified{Status string, Reset time.Time, RepresentativeClaim string, FallbackPercentage *float64, OverageStatus, OverageDisabledReason string, FiveHour, SevenDay UnifiedWindow, ObservedAt time.Time}`.
- [ ] Add a pointer field `Unified *Unified` to `RateLimits` with JSON tag `unified,omitempty`, so the JSON of existing API-key accounts does not change.
- [ ] `ExtractRateLimits` parses these headers:
  - case-insensitively;
  - leniently, skipping malformed values without failing;
  - resets as epoch seconds.
- [ ] `HasLimits()` returns true when unified data is present.
- [ ] `MinRemainingFraction()` prefers unified data:
  - it uses windows whose reset is after `now`, or after `ObservedAt` when no `now` is available;
  - it returns `1 - max(util5h, util7d)`;
  - classic dimensions are used only when unified data is absent.
  - If the current signature has no `now`, add a `MinRemainingFractionAt(now)` variant and keep the old one delegating to `time.Now()`.
- [ ] `IsRateLimited(now)` returns true when unified `Status == "rejected"` and the binding window's reset is in the future.
  - The binding window is the one the representative claim names: `five_hour` means 5h, `seven_day` means 7d. Otherwise use `unified-reset`.
- [ ] Tests:
  - Feed the exact response-header sets from the first record of `.reference/claude-code-headers-20260923.jsonl` and of `-run2.jsonl`.
  - For the first record, expect 5h utilization 0.04 with reset `2026-09-23T12:10:00Z`, and 7d utilization 0.22 with reset `2026-09-23T09:00:00Z`.
  - Malformed values are ignored.
  - A rejected status makes `IsRateLimited` true before the reset and false after it.
  - Classic-only headers behave exactly as before.
- [ ] Commit: `claudecode: parse anthropic-ratelimit-unified headers`.

### Task 2: Cooldown until the unified reset on a rejected 429
Spec: "PR 1" `pool.go` bullet; review item 9.

**Files:** `internal/claudecode/pool.go`, `internal/claudecode/pool_test.go`.

- [ ] In `RecordRateLimit`, when `rl.Unified != nil && rl.Unified.Status == "rejected"`, set the cooldown to the binding window's reset minus now.
  - If `RetryAfter` is present and longer, it wins.
  - Otherwise keep the existing logic.
- [ ] Keep an unexpired unified snapshot when a later response has no unified headers. Only replace it when new unified data arrives. This keeps `UpdateAccountRateLimits` and `RecordSuccess` from wiping it.
- [ ] Tests:
  - A rejected 429 with a 5h reset 3h ahead gives `CooldownUntil` about 3h ahead, not 10s.
  - A longer `retry-after` wins.
  - A non-rejected 429 keeps today's behaviour.
- [ ] Commit: `claudecode: cool down until unified reset on subscription 429`.

### Task 3: Persist the unified snapshot across restarts
Spec: "PR 1" persistence bullet; review item 10.

**Files:** `internal/claudecode/storage.go`, `internal/claudecode/pool.go`, and tests.

- [ ] Store each account's `Unified` snapshot in the claudecode account store alongside its config. Stay backward compatible: old files load without error.
- [ ] Load it back on start into `Account.RateLimits.Unified`. Windows whose reset is in the past are dropped: a window with a past reset becomes a zero-value window. If both windows have expired, the whole snapshot is dropped.
- [ ] Save when unified data changes. Save at most once per 30s per account, and always save on a status change to or from `rejected`. Save through the existing store path.
- [ ] Tests: a round trip through `SaveStoredAccounts`/`LoadStoredAccounts`, and dropping expired windows on load.
- [ ] Commit: `claudecode: persist unified limit snapshot`.

### Task 4: Forward unified headers to clients
Spec: "PR 1" header-forwarding bullet.

**Files:** `internal/claudecode/types.go` (the `Config` field), `internal/api/claudecode_proxy.go`, `internal/config` (if defaults live there), and tests in `internal/api`.

- [ ] Add `ForwardUnifiedHeaders *bool` with JSON `forwardUnifiedHeaders,omitempty` to `claudecode.Config`. `nil` means true. Add a helper method that returns the effective value.
- [ ] In both `ccCopyResponseHeaders` and `writeCCUpstream429`, forward every header whose canonical name starts with `Anthropic-Ratelimit-Unified-` when forwarding is enabled.
  - Thread the flag in without breaking the existing callers. Adding a parameter or a server method is fine.
- [ ] Surface the setting wherever the other claudecode config fields are exposed (config GET and PATCH handlers in `claudecode_management.go`), following the existing pattern for `autoImport`.
- [ ] Tests:
  - A proxied success and a mirrored 429 carry the upstream unified headers.
  - With the switch false, neither does.
  - Use the existing Claude Code proxy test harness.
- [ ] Commit: `claudecode: forward unified rate-limit headers to clients`.

### Task 5: Claude pools and limits on /account-limits
Spec: "PR 1" `/account-limits` bullet; review items 4 and 16.

**Files:** `internal/api/management.go`, `internal/api/management_test.go`.

- [ ] For Claude Code rows whose account `Type` is `oauth` or `setup_token`, add `quota: {models:{}, pools:{"claude-5h":ModelQuota, "claude-weekly":ModelQuota}, lastChecked}`.
  - Use `accounts.ModelQuota` for the pool values.
  - Values come from fresh unified windows: `remainingFraction = 1 - utilization`, clamped to [0,1], and `resetTime` = the reset in RFC 3339.
  - A window whose reset has passed, or that is missing, is omitted from `pools`.
  - `lastChecked` is `ObservedAt` in milliseconds, or null.
- [ ] `limits[modelId]` for those rows is the minimum over the present pools. Every Claude Code model maps to both pools through a local helper; do not use `accounts.quotaPoolsForModel`.
  - With no pools, fall back to today's behaviour.
- [ ] `api_key` accounts are unchanged: no `quota` key or an empty one. Match whatever is least surprising for `data-store.js`.
- [ ] Narrow `TestManagement_AccountLimits_ClaudeCodeUnknownQuotaIsNull` to API-key and no-data accounts, if needed.
- [ ] Add `TestManagement_AccountLimits_ClaudeCodeUnifiedPools`, covering fresh windows, one expired window, and an API-key account.
- [ ] Commit: `api: expose Claude subscription windows as quota pools`.

---

## PR 2: ccusage library (`internal/claudecode/ccusage`)

This package must not import `internal/api`, `internal/accounts` or `internal/claudecode`. Pricing fallbacks are injected through interfaces.

### Task 6: Entries, paths and dedupe
Spec: "PR 2" items 1–3.

**Files:** `internal/claudecode/ccusage/{doc.go,entry.go,paths.go,dedupe.go}` with tests and `testdata/`.

- [ ] Port `ParseLine` from Rust `rust/crates/ccusage-core/src/types.rs` and `rust/adapters/claude/src/lib.rs`:
  - Fast-reject lines without `"usage":{`.
  - Require `timestamp` and usage `input_tokens`/`output_tokens`.
  - Reject null non-nullable fields, empty IDs and empty models.
  - If `version` is present it must look like semver, otherwise reject the line.
  - Cache creation = `ephemeral_5m + ephemeral_1h` when the `cache_creation` object is present, otherwise `cache_creation_input_tokens`. Keep the 5m and 1h split separately.
  - `iterations[type=advisor_message]` become extra entries with ID `{msgId}:advisor:{i}` and no `costUSD`.
  - A `speed=="fast"` model gets the display suffix `-fast`.
  - The model `<synthetic>` is flagged so reports exclude it from model lists.
  - Parse the usage-limit reset time from `isApiErrorMessage` text of the form `Claude AI usage limit reached|<epoch>`.
- [ ] `ClaudePaths()`:
  - If `CLAUDE_CONFIG_DIR` is set, it is comma-separated. Trim each part, expand `~`, and accept a path that already ends in `projects`. Keep only paths with a `projects/` directory and dedupe them.
  - Otherwise check `$XDG_CONFIG_HOME/claude` (default `~/.config/claude`) and then `~/.claude`.
  - An empty result is not an error.
- [ ] `UsageFiles(paths)` walks `projects/**/*.jsonl`. `SessionParts(path)` supports the `{proj}/{sid}.jsonl` and `{sid}/subagents/*.jsonl` layouts.
- [ ] Port the Rust dedupe (`push_deduped_entry`, `usage_dedupe_hash`, `should_replace_deduped_entry`):
  - Exact key: with a requestId, `msgId:reqId`; without one, `msgId|sessionId|tsMs`.
  - Sidechain-replay key.
  - Winner order: a non-sidechain entry, then more total tokens, then `speed` set.
  - Plus the plan's deliberate deviation: an entry whose `Source` is `ledger` always beats a `local` entry with the same key.
- [ ] Tests use fixtures copied from `ccusage/apps/ccusage/test/fixtures/claude` and from the Rust unit tests. Add an MIT attribution note in `testdata/README.md`.
- [ ] Commit: `ccusage: parse Claude Code usage entries, paths and dedupe`.

### Task 7: Pricing and cost
Spec: "PR 2" item 4; review item 13.

**Files:** `internal/claudecode/ccusage/{pricing.go,cost.go,litellm_claude.json,gen_pricing.go}` and tests.

- [ ] Add a `Pricer` interface: `Find(model string) (ModelPrice, bool)`.
  - `ModelPrice` holds input, output, cache-create, cache-read, the four `*Above200k` variants as pointers, `MaxInputTokens`, and `FastMultiplier`.
- [ ] `ChainPricer`: an ordered list of pricers. The first hit wins, and results are memoised, including misses.
- [ ] `LiteLLMPricer` loads the embedded `litellm_claude.json` via `//go:embed`.
  - Generate that file with a `go generate` program (`gen_pricing.go` behind `//go:build ignore`). It fetches `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json` and keeps only keys starting with `claude-`, `anthropic.claude-` or `anthropic/claude-`, and only the fields used.
  - Run it once and commit the generated JSON. Network access is available.
- [ ] Defaults:
  - When cache prices are missing: cache-create = `input*1.25` and cache-read = `input*0.1`.
  - Both input and output prices are required.
  - The fast multiplier falls back to Rust `fast-multiplier-overrides.json`.
- [ ] Boundary-aware fuzzy matching, ported from `pricing_key_matches`:
  - Normalise `.` and `@` to `-`.
  - Require non-alphanumeric boundaries.
  - A key ending in a digit must not be followed by `-<digits>` unless those are 8 date digits.
  - The longest matching key wins.
- [ ] Optional online refresh: `LiteLLMPricer.Refresh(ctx, *http.Client)`. It is not called by default.
- [ ] Cost:
  - `CostMode` is `auto`, `calculate` or `display`.
  - Tiered pricing per bucket with a 200k threshold.
  - 1h cache writes cost `input*2`; 5m writes cost the cache-create rate.
  - The fast multiplier applies to `speed=="fast"` entries.
- [ ] Tests:
  - tier edges at 200k and 200k+1;
  - `claude-opus-5` must not match `claude-opus-5-5`;
  - a date suffix is allowed;
  - missing-field defaults;
  - fast multiplier;
  - all three cost modes;
  - a chain in which the first pricer wins.
- [ ] Commit: `ccusage: LiteLLM pricing and tiered cost`.

### Task 8: Billing blocks
Spec: "PR 2" item 5; review item 2.

**Files:** `internal/claudecode/ccusage/blocks.go` and tests.

- [ ] Port Rust `rust/crates/ccusage/src/blocks.rs` (with TS `_session-blocks.ts` for readability): `IdentifyBlocks(entries []Entry, dur time.Duration, now time.Time, anchors []time.Time) []Block`.
  - Without anchors, the behaviour is exactly ccusage's:
    - floor the start to the UTC hour;
    - split when `ts-start > D || ts-last > D`;
    - create gap blocks (`IsGap`, ID `gap-<iso>`);
    - a block is active when `now-last < D && now < end`;
    - ID = start in ISO format.
    - Record token counts, cost, models in first-seen order, and `UsageLimitResetAt`.
  - With anchors (known window starts): an entry falling in `[anchor, anchor+D)` joins that window's block, whose start is the anchor. Entries outside every anchor window use the unanchored rule.
- [ ] `BurnRate(b)`:
  - returns nil for a gap block, for fewer than 2 entries, or for 0 duration;
  - `durMin` runs from the first entry to the last;
  - returns `TokensPerMinute`, `TokensPerMinuteForIndicator` (input+output only) and `CostPerHour`.
- [ ] `Project(b, now)` returns the projected tokens and cost and the remaining minutes, using ccusage's rounding.
- [ ] `MaxTokensFromHistory(blocks)` covers non-gap, non-active blocks. `LimitStatus(projected, limit)` returns ok, warning (above 0.8) or exceeds.
- [ ] Tests:
  - exactly 5h is not a split, and 5h+1ms is;
  - UTC-hour floor;
  - gap block;
  - active detection;
  - single-entry burn rate is nil;
  - projection;
  - anchored windows at `:10` offsets taken from the reference captures.
- [ ] Commit: `ccusage: session blocks, burn rate and projection`.

### Task 9: Reports
Spec: "PR 2" item 6.

**Files:** `internal/claudecode/ccusage/reports.go` and tests.

- [ ] `Daily`, `Weekly` (configurable start of week, plain date arithmetic as in Rust `summary.rs::week_start`), `Monthly` and `Session`.
  - Each takes entries, a `*time.Location`, a `Pricer`, a `CostMode` and optional since/until dates.
  - Rows carry input, output, cacheCreate, cacheRead, totalTokens, costUSD, `ModelsUsed` (excluding `<synthetic>`), and `ModelBreakdowns` sorted by cost descending.
  - An optional account key groups rows.
- [ ] JSON tags match ccusage's `--json` output field names, so the parity test can diff them. Check `ccusage/rust/crates/ccusage-core/src/output.rs` or the snapshots.
- [ ] Tests: timezone boundary, week start, month rollup, session grouping, `<synthetic>` exclusion.
- [ ] Commit: `ccusage: daily, weekly, monthly and session reports`.

### Task 10: Parity test against pinned ccusage
Spec: "PR 2" tests; review item 17.

**Files:** `internal/claudecode/ccusage/parity_test.go` (`//go:build ccusage_parity`) and fixtures.

- [ ] Using fixtures dated in the past, with `TZ=UTC`, run `npx --yes ccusage@20.0.26 daily --json --offline` and `blocks --json --offline` with `CLAUDE_CONFIG_DIR=<fixture>`.
  - Diff totals, per-day rows and block boundaries against the Go output.
  - Skip when `npx` is missing.
  - Document the command in the file comment.
- [ ] Run it once locally and report the result. If ccusage's JSON differs in known, justified ways (for example rounding), assert with a tolerance and explain why in a comment.
- [ ] Commit: `ccusage: pinned parity test against upstream ccusage`.

---

## PR 3: Ledger, engine, wiring

### Task 11: Detailed Anthropic usage capture
Spec: "PR 3" detailed-capture bullet; review items 7 and 13.

**Files:** `internal/claudecode/usagecapture.go` and tests; `internal/api/claudecode_proxy.go`; `internal/claudecode/pricing.go` / `observability.go` as needed.

- [ ] `claudecode.Usage{MessageID, Model, RequestID, Input, Output, CacheRead, CacheCreate, CacheCreate5m, CacheCreate1h, Speed string, Iterations []IterationUsage}`.
- [ ] An SSE interceptor (an `io.ReadCloser` wrapper with `onComplete(Usage)`, finalizing on EOF, error or Close, the same way the OpenRouter interceptor does) plus `ParseUsageJSON([]byte) Usage` for unary responses.
  - Read `message_start.message.{id,model,usage}` and `message_delta.usage`, using the same "fall back to message_delta only for fields message_start left zero" rule as `ccr_proxy.go:114`.
- [ ] `ccInstrumentResponse` switches to it. Do not touch the six OpenRouter/other callers of `openrouter.NewSSEInterceptor`.
  - `ccAttempt` carries `requestID` from `resp.Header.Get("request-id")`.
  - `recordClaudeCodeMetrics(a, u Usage)` keeps its existing behaviour (metrics, pool, tracker).
- [ ] Add a shared pricer hook so the Claude Code cost comes from one place. Consult `GetModelPricing` first; this task does not change prices.
- [ ] Tests:
  - An SSE stream with message_start and message_delta, including `cache_creation.ephemeral_*`.
  - A unary JSON response.
  - An aborted stream still reports the partial usage.
  - The existing claudecode proxy tests still pass.
- [ ] Commit: `claudecode: capture message id, model and cache split from responses`.

### Task 12: Usage ledger
Spec: "PR 3" ledger bullet; review items 11 and 12.

**Files:** `internal/claudecode/ccusage/ledger.go` and tests.

- [ ] `Ledger` writes to `<root>/projects/<accountId>/<YYYY-MM-DD>.jsonl`.
  - Account IDs are sanitised for the filesystem.
  - Directories are `0700` and files `0600`.
- [ ] Line schema: `timestamp` (RFC 3339 nanoseconds, UTC), `sessionId`, `requestId`, `message{id,model,usage{input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,cache_creation{ephemeral_5m_input_tokens,ephemeral_1h_input_tokens},speed}}`, `costUSD`, `accountId`, `source` (`proxy` or `cachebump`). No `version` field.
- [ ] `Append(Entry)` does a non-blocking send on a buffered channel (4096).
  - One writer goroutine handles all writes.
  - On overflow the entry is dropped and a counter incremented, exposed via `Stats()`.
  - Files are fsynced when the day rotates and on `Close()`.
  - `Close()` drains the queue. Retention pruning (default 60 days) runs at start and daily.
- [ ] Tests:
  - a round trip through `ParseLine` from Task 6;
  - the real ccusage directory layout (`projects/<acct>/…jsonl`);
  - drop-on-overflow;
  - Close drains;
  - pruning.
- [ ] Commit: `ccusage: append-only usage ledger`.

### Task 13: Engine, local-log admission, config, wiring
Spec: "PR 3" local-log admission, engine and config bullets; review items 5, 6, 8 and 18.

**Files:** `internal/claudecode/ccusage/engine.go` and tests; `internal/claudecode/types.go` (the `Usage` config and `AccountConfig.UsageLimits`); `internal/api/server.go` (`Options.CCUsage`, construction in `api.New` or in `cmd/proxy`, whichever matches how other subsystems are built); `internal/api/claudecode_proxy.go`; `internal/api/cachebump_server.go`.

- [ ] Config: `claudecode.usage{enabled *bool (default true), ledgerDir, scanLocalLogs *bool (default true when a Claude dir exists), localAccountId, sessionHours (5), costMode ("auto"), timezone, onlinePricing (false), retentionDays (60)}`. `AccountConfig.UsageLimits{CostUSD5h, CostUSD7d, Tokens5h, Tokens7d}`, all optional.
- [ ] Engine:
  - At startup, load the last 8 days of ledger files and of local logs.
  - Poll every 30s. Tail changed files from a stored offset. If the size shrank, reread the file. Keep a partial last line pending.
  - Keep the entries in memory, deduplicated and sorted, pruned to 8 days.
  - Keep a per-account summary (max-block tokens and cost, calibrated limits), persisted as JSON next to the ledger.
- [ ] Local-log admission: accept an entry only if:
  - `requestId` has the prefix `req_`;
  - the message ID does not match `^msg_[0-9a-f]{32}$` or `^msg_(zen|clf|stub)_`;
  - the model has the prefix `claude-`.
- [ ] Attribution:
  - An entry that dedupes against the ledger takes the ledger's account.
  - Otherwise use `localAccountId` when it is set.
  - Otherwise, if exactly one claudecode account has `Source=="auto_import"`, use that account and mark the entry inferred. The engine takes a callback that lists accounts.
  - Otherwise the entry is unattributed (account key `""`).
- [ ] `Record(Entry)` is the hot path: it calls the ledger `Append` and does an in-memory insert under a mutex. It never blocks on disk.
- [ ] `Snapshot(accountID, now, anchors)` returns the blocks and active block, burn rate and projection, the 5h/7d cost and tokens, today's cost, per-model totals and the summary. It is cached for 5s per account.
- [ ] Wiring:
  - `recordClaudeCodeMetrics` calls `engine.Record` with the Task 11 `Usage`, the `ccAttempt` session/account, and `costUSD` from the shared pricer.
  - The cache-bump paths (`cachebump_server.go` around `:247` and `:331`) record entries with `source:"cachebump"`. Extend the parse there to the detailed `Usage` if the IDs are needed; otherwise use a synthetic key `bump:<accountId>:<tsNano>`.
  - The engine is nil-safe: with `Options.CCUsage == nil`, every call is a no-op.
  - Stop the engine and close the ledger on server shutdown, if the server has a shutdown hook.
- [ ] Tests:
  - admission filter fixtures that mix `req_`/`msg_01…` lines with proxy-made `msg_<32hex>` lines that have no requestId;
  - ledger-vs-local dedupe;
  - auto-import attribution;
  - tailing with truncation;
  - a nil engine;
  - an end-to-end test where a proxied Claude Code response produces a ledger line.
- [ ] Commit: `claudecode: usage engine with ledger and local-log merge`.

---

## PR 4: Fallback math, APIs, WebUI

### Task 14: Pool precedence, calibration, projection, `usage` object
Spec: "PR 4" pool values, projection and `/account-limits` bullets; review items 3 and 14.

**Files:** `internal/api/management.go` (or a new `internal/api/claudecode_usage.go` helper), the engine (calibration), and tests.

- [ ] Pool value precedence for `oauth`/`setup_token` accounts:
  1. Fresh unified headers (source `headers`).
  2. Calibrated (source `calibrated`):
     - The implied limit is `windowCost/utilization`, recorded by the engine whenever utilization ≥ 0.20, per account and per window.
     - `remaining = 1 - windowCost/implied`, clamped to [0,1].
     - The window follows the last known anchor, stepping forward in whole periods. Without an anchor, use the floored ccusage block for 5h and a rolling 7d window with no reset.
  3. Configured `UsageLimits` (source `config`).
  4. The ccusage max block, 5h only (source `max`).
  5. Omitted.
- [ ] Add `source` to each pool object. It is an extra field next to `ModelQuota`; use a small wrapper struct if needed, keeping `remainingFraction` and `resetTime` identical.
- [ ] `projectedUtilization = utilization × projectedCost/currentCost` for the active window. It goes only in `usage`, never in `remainingFraction`.
- [ ] Add `usage` to Claude Code rows: `{window5h{start,end,costUSD,tokens,utilization,projectedUtilization,status,source}, window7d{…}, burnRate{tokensPerMinute,tokensPerMinuteForIndicator,costPerHour}, todayCostUSD, costBasis:"api-equivalent"}`.
- [ ] Tests cover each precedence level, clamping, and a projection that never lowers `remainingFraction`.
- [ ] Commit: `api: calibrated fallback and usage window details for Claude accounts`.

### Task 15: /v1/usage windows and the usage report API
Spec: "PR 4" `/v1/usage` and new-endpoint bullets; review item 15.

**Files:** `internal/api/server.go` (`usage`), `internal/api/claudecode_management.go`, and tests.

- [ ] `/v1/usage` builds the Claude windows independently of the Cloud Code catalog.
  - If the catalog fetch fails but Claude data exists, respond 200 with the Claude windows and an empty `models` list. Only an error from both sides fails the call.
  - Append `{label:"Claude 5h (<name>)" / "Claude weekly (<name>)", remaining_fraction, used_percent, reset_at, model_ids, provider:"claudecode", account_id}` for each enabled subscription account that has pool data.
  - Existing Google windows are unchanged.
- [ ] `GET /api/claudecode/usage` accepts `report=daily|weekly|monthly|session|blocks`, `since`, `until` (YYYYMMDD), `account`, `timezone`, `recent`, `active` and `startOfWeek`.
  - Unattributed usage appears under the account key `unattributed`.
  - `POST /api/claudecode/usage/reload` forces a rescan.
  - Both require the WebUI password, like the other `/api/claudecode/*` routes.
- [ ] Tests: `/v1/usage` with the catalog failing and Claude present, with both failing, and with both present; each report type; auth required.
- [ ] Commit: `api: Claude usage windows and ccusage report endpoints`.

### Task 16: WebUI
Spec: "PR 4" WebUI bullet.

**Files:** `internal/webui/public/views/accounts.html`, `internal/webui/public/js/components/account-manager.js`, `internal/webui/public/js/data-store.js`, `internal/webui/public/js/translations/{en,pt}.js`, and possibly `views/models.html`.

- [ ] Add a shared pool-bar renderer for `acc.quota.pools`, used for both Google and Claude accounts. It goes on the account card and in `quota_modal`, and shows a percentage bar, `utils.formatTimeUntil(resetTime)` and a source badge.
- [ ] Add a Claude window panel to Claude Code accounts:
  - utilization now plus a projected marker (red above 100%, yellow above 80%);
  - a burn indicator from `tokensPerMinuteForIndicator` (above 1000 HIGH, above 500 MODERATE);
  - cost labelled "API-equivalent".
- [ ] Add a usage-history table (daily or weekly toggle) fed by `/api/claudecode/usage`, loaded lazily when the panel opens.
- [ ] Add translation keys to both `en.js` and `pt.js`; `translations_test.go` must pass.
- [ ] Update the placeholder data in `data-store.js` (around lines 560–690) with `quota.pools` and `usage` for Claude Code accounts.
- [ ] Verify in a real browser: start the proxy with a temporary config dir and use Playwright (Chromium at `/opt/pw-browsers`) against the placeholder or demo data. Take screenshots of the accounts view and the quota modal, save them in the scratchpad, and report the paths.
- [ ] Commit: `webui: render quota pools and Claude usage windows`.

---

## Final
- [ ] A whole-branch review: correctness, the spec, AGENTS.md, and the hot path.
- [ ] `gofmt -l .` is empty, `go vet ./...` passes, and `go test -race ./...` passes.
- [ ] Push the branch.
