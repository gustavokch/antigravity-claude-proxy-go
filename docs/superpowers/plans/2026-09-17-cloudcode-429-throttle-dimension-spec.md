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

## 6. Findings (2026-09-17)

Recorded by the Task 4 probe matrix run (`go run ./cmd/probe429 -model
gemini-3.8-flash-high -alt-model gemini-2.5-pro -burst 8 -window 20m
-spacing 30s`), the live agy MITM capture attempt, and the Task 1 forensics
JSONL. No interpretation beyond what the data shows.

### 6.1 Verbatim 429 bodies

`cloudcode-pa.googleapis.com` (burst, baseline, model axis, account axis, and
window probes window-0..window-12), every rejection identical:

```json
{ "error": { "code": 429, "message": "Resource has been exhausted (e.g. check quota).", "status": "RESOURCE_EXHAUSTED" } }
```

No quota/rate/retry response headers were present on any recorded
`cloudcode-pa` rejection (the recorded header set contained no
quota/rate/retry fields).

`daily-cloudcode-pa.googleapis.com` (endpoint axis probe, 2026-09-17
21:40:48 -03:00):

```json
{ "error": { "code": 429, "message": "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 17m31s.", "status": "RESOURCE_EXHAUSTED", "details": [ { "@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED", "domain": "cloudcode-pa.googleapis.com", "metadata": { "uiMessage": "true", "model": "gemini-3.8-flash-high", "quotaResetDelay": "17m31.337247485s", "quotaResetTimeStamp": "2026-09-18T00:58:20Z" } }, { "@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "1051.337247485s" } ] } }
```

Quota metric phrase: none of the bodies names a per-project/per-user/per-minute
metric. The only quota identifiers present are the daily-endpoint
`ErrorInfo.reason` `QUOTA_EXHAUSTED` with `metadata.model`
`gemini-3.8-flash-high` and `quotaResetTimeStamp` `2026-09-18T00:58:20Z`, plus
`RetryInfo.retryDelay` `1051.337247485s`. A grep of all 123 entries in the
forensics JSONL for `quotaResetDelay`, `per project`, `per user`, `per model`,
and `metric` returned zero hits; the daily-endpoint rejection above was not
filed to the forensics JSONL (0 entries name `daily-cloudcode`).

### 6.2 Matrix VERDICT line (verbatim)

```
VERDICT: throttle is shared across every probed axis (model, account, project, endpoint): project-wide or model capacity. Re-run with -window to measure how long it holds
```

### 6.3 Measured throttle window

No recovery within 20m: the run printed `still throttled after 20m0s`.
Window probes window-0 through window-12 (the first ~6.5 minutes of the scan)
returned 429; from window-13 onward (2026-09-17 ~21:47 -03:00) every probe of
`ghlsem@gmail.com` returned **401 UNAUTHENTICATED** ("Request had invalid
authentication credentials") instead of 429, so the tail of the window could
not observe 429 recovery. The daily-endpoint body during the same run named a
`quotaResetTimeStamp` of `2026-09-18T00:58:20Z` (~17.5 minutes after the
endpoint-axis probe).

### 6.4 Live agy in the same window

agy was rejected before any request reached the server: `agy --print='say OK'`
through the MITM harness exited 1 with
`tls: failed to verify certificate: x509: "upload.video.google.com" certificate is not trusted`.
No content and no HTTP status were returned; whether the server would have
rejected agy in this window is not measurable from this run (capture harness
failure, recorded for escalation — no keychain changes were attempted).

### 6.5 Decision table

| Verdict | Task 5 ladder | Task 6 shared throttle | Extra follow-up |
|---------|---------------|------------------------|-----------------|
| model-scoped | cap the top tier at the measured window | keep: rejections still cluster per model | file a follow-up for sibling-model failover |
| project-scoped | as above | keep — it is exactly this case | file a follow-up for project rotation |
| endpoint-scoped | as above | keep | file a follow-up for endpoint failover |
| account-scoped | as above | **drop Task 6** — rotation is already correct | none |
| **shared across every axis** ← VERDICT | as above | keep — the primary fix | none |

Notes: the project axis was not probed (both accounts use the single project
`aicode-consumers`; no second project ID exists), and the window measurement
is a lower bound contaminated by the mid-scan 401s on `ghlsem@gmail.com`.
