# Cloud Code 429 Throttle — Evidence and Requirements

Companion spec for `2026-09-17-cloudcode-429-throttle-dimension.md`.
Date of the observed wave: 2026-09-17.

## 1. What we know

**Quota is not the limiter. A short-window throttle is.**

| # | Observation | Source |
|---|---|---|
| 1 | Google returned `429` with no `Retry-After` and no `quotaResetDelay`. The proxy logged `upstream 429 ... serverReset=0s`, classified `RATE_LIMIT_EXCEEDED`, and guessed 30 s. | `internal/accounts/retry.go:142`, proxy log |
| 2 | Rotation works as designed: account A 429 → marked 30 s → account B tried → B 429s ~1 s later → both marked → `All accounts rate-limited` → wait/retry. | `internal/accounts/dispatcher.go:375-398`, proxy log |
| 3 | The wave ran 16:57–17:16. The same model succeeded at 16:45 and 16:50, and again at 17:22 — verified by direct replay against upstream with both accounts' tokens, at small and 400 KB payloads. | `cmd/debug429` |
| 4 | Request rate during the wave was ~1/min, far below any plausible per-user RPM. | proxy log |
| 5 | `remainingFraction=1` in the status UI comes from `fetchAvailableModels` `quotaInfo` — that is *daily model quota*. Google throttles RPM/concurrency on a separate dimension that this field never reports. | `internal/accounts/dispatcher.go:620-673` |
| 6 | Both accounts resolve to the same Google-assigned project `aicode-consumers`, taken from `loadCodeAssist`, not from proxy config. Both 429 within the same second at ~1 req/min. | `internal/accounts/dispatcher.go:450-479` |

Conclusion so far: the rejection is **shared across accounts**, which rules out
a per-user counter. It is consistent with a project-scoped or a model-capacity
throttle. Which of the two, we do not yet know.

## 2. The open question

**What dimension is the throttle keyed on?** Candidates, and what each implies:

| Dimension | Test that distinguishes it | Implication for the proxy |
|-----------|---------------------------|---------------------------|
| Per-model capacity | While model M is 429, model N on the same account succeeds | Fail over to a sibling model, keep the account |
| Per-project (`aicode-consumers`) | While the assigned project is 429, an explicitly overridden project ID succeeds | Rotate projects, not accounts |
| Per-endpoint | `daily-cloudcode-pa` succeeds while `cloudcode-pa` is 429 | Endpoint failover |
| Per-account after all | One account recovers while the other is still rejected | Keep the current rotation, only fix backoff |
| Concurrency, not rate | A serialized probe succeeds where a burst is rejected | Add a concurrency gate |

Answering this needs the **429 response body and headers**, which the proxy has
never persisted. That is the first gap to close.

## 3. Defects that hold regardless of the answer

These are wrong today and stay wrong under every candidate dimension:

- **D1 — Flat backoff, no escalation.** `SmartBackoff` returns a constant 30 s
  for `ReasonRateLimit` (`internal/accounts/retry.go:142`). Across the 19-minute
  wave that is ~38 doomed upstream calls per account. On a sliding-window
  throttle each rejected call can extend the window.
- **D2 — No jitter.** Two accounts rejected in the same second are marked for
  the same 30 s and wake in lockstep, re-hitting the same bucket together.
- **D3 — Rotation burns accounts on a shared throttle.** When the bucket is
  shared, trying account B after A is guaranteed to fail; it costs a round trip,
  a failure count, and another rejected request against the window.
- **D4 — The client is told the wrong thing.** An upstream 429 is mapped to
  HTTP **400 `invalid_request_error`** (`internal/api/server.go:3161`), with no
  `Retry-After`. Claude Code treats 400 as permanent and does not retry. The
  pool-exhaustion path returns a plain error, which falls through to 500
  `api_error` — also without retry guidance.
- **D5 — No forensics.** The 429 body is the only place the dimension is named,
  and nothing persists it. (An uncommitted change logs a truncated body; that is
  a start, not a record.)

## 4. Requirements

**R1.** Every upstream 429 is persisted verbatim — status, endpoint, all
response headers, full body (capped), plus account, project, model, request
size, applied wait — to an append-only JSONL file, off by default, enabled by
config.

**R2.** No OAuth token, cookie, or user prompt text is ever written to that
file or to `.reference/`.

**R3.** The MITM harness captures **responses** (status, headers, and error
bodies), not only request headers, so a live `agy` run and a proxy run can be
compared on the same wave.

**R4.** A probe tool can hold every axis fixed but one — model, account,
project, endpoint, concurrency — and report a truth table naming the dimension.

**R5.** The guessed rate-limit cooldown escalates with consecutive failures and
carries additive jitter. Jitter never shortens a server-specified reset.

**R6.** When two or more accounts are rejected on the same model inside a short
window, the pool treats the throttle as shared: it stops rotating and waits,
instead of spending each remaining account on a certain rejection.

**R7.** When the pool cannot wait the throttle out within `maxWait`, the client
receives HTTP **429** with a `Retry-After` header, not 400 or 500.

**R8.** The shared-throttle state is visible in the account status output.

## 5. Non-goals

- Changing the account-selection strategy (sticky / round-robin / hybrid).
- Changing daily-quota accounting (`quotaInfo`), which is a separate dimension
  and is not the limiter here.
- Web UI work beyond the existing text status table.
