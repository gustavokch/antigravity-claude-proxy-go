# Throttle Calibration from Production Evidence

Date: 2026-09-18

## 1. Problem

The proxy's pacing and cooldown settings are static heuristics:

| Key | Current value |
|---|---|
| `accountSelection.tokenBucket.tokensPerMinute` | 6 |
| `accountSelection.tokenBucket.maxTokens` | 50 |
| `requestDelayMs` | 200 |
| `sharedThrottleWindowMs` | 10000 |
| `capacityBackoffTiersMs` | `[5000, 10000, 20000, 30000, 60000]` |

None of them is derived from a measurement. Set too low, accounts are
under-used; set too high, the pool cascades into rotation failures. We want
these numbers to come from evidence.

## 2. Why active benchmarking is the wrong tool

An earlier draft proposed a stepped-ramp benchmark: ramp concurrency until
429, ramp RPM until 429, scale payload, then bisect the recovery window. The
forensics in `docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md`
§6 rule that design out.

- §6.3: after a deliberate burst, the account did not recover within twenty
  minutes — the run printed `still throttled after 20m0s`. A benchmark whose
  first phase ends on the first 429 leaves every later phase measuring an
  account that is already in a hole.
- §6.1: the daily endpoint reported `quotaResetDelay 17m31.337247485s` and
  `RetryInfo.retryDelay 1051.337247485s`. A seventeen-minute reset looks like
  a long-window budget, not a short sliding window. If it is a budget, each
  rung of a step ladder drains the allowance the next rung measures, and the
  resulting "sustained RPM" is an artifact of ladder order.
- A single ramp phase at the drafted tiers is roughly 270 upstream calls
  against a real account, and the cost of each trip is twenty-plus minutes of
  that account's availability.

So each measurement is expensive, slow, and contaminates its successor. The
cheap evidence is the traffic the proxy already serves.

## 3. Approach

Calibrate from the proxy's own 429 journal, which already exists, and keep
exactly one small active probe for the case where the journal cannot yet
answer.

`internal/accounts/forensics.go` already defines `Forensics429Recorder`,
appending one JSON line per upstream 429 to
`<configdir>/forensics/upstream-429.jsonl`. It records `Timestamp`,
`Account`, `Project`, `Model`, `Endpoint`, `Status`, `Headers`, `Body`
(capped at 4 KB), `AppliedWait`, and `Failures`. It carries no tokens, no
cookies, and no prompt text. The dispatcher writes to it at
`internal/accounts/dispatcher.go:423` and `:636`. This is the same file the
forensics in §6 were read from.

## 4. What the journal is missing

Three gaps block calibration.

**No concurrency evidence.** Nothing records how many requests were in flight
when a 429 landed, so a safe concurrency ceiling cannot be recovered.

**No rate evidence.** Nothing records how many requests the rejected account
sent in the preceding sixty seconds.

**No recovery events.** Only rejections are written. The recovery window is
the gap from a 429 to the next success on the same account, and successes are
not recorded at all.

The fix adds three fields to `Forensics429Entry`:

- `InFlight int` — requests in flight on that account at the moment of the
  record.
- `PriorMinuteRequests int` — requests that account sent in the preceding
  sixty seconds.
- `Outcome string` — `"reject"` or `"recover"`.

The dispatcher gains an in-flight gauge and a per-account sixty-second ring
counter, and writes one `"recover"` line at the first success on an account
that is currently marked throttled.

Every new field is a counter or a timestamp. The redaction rule the recorder
already enforces (response data only, no request headers, no tokens, no
prompt text) is unchanged.

## 5. Derivation

A new package `internal/calibrate` reads the journal and derives the config
values. Every output carries a confidence guard; a guard that fails prints
`insufficient data (n=X, need Y)` and omits that key from the suggestion.
No value is ever guessed.

| Output | Derivation | Guard |
|---|---|---|
| `C_safe` | `max(1, min(InFlight over reject records) - 1)` | ≥5 reject records |
| `RPM_safe` | `max(1, min(PriorMinuteRequests over reject records) - 1)` | ≥5 reject records |
| `T_recover` | median gap `reject → recover` per account, counting only gaps in which a retry was actually attempted | ≥3 recover records |
| `requestDelayMs` | `max(200, ceil(60000 / RPM_safe))` | inherits the `RPM_safe` guard |
| `accountSelection.tokenBucket.tokensPerMinute` | `RPM_safe` | inherits |
| `accountSelection.tokenBucket.maxTokens` | `min(20, C_safe * 3)` | inherits the `C_safe` guard |
| `capacityBackoffTiersMs` | `[10s, T_recover, T_recover * 2]`, each tier capped at 1800s | inherits the `T_recover` guard |
| `sharedThrottleWindowMs` | `T_recover` in milliseconds | inherits |

Two notes on the table.

The top backoff tier is capped at 1800s rather than the drafted 120s. The
measured window exceeded twenty minutes, so a 120s ceiling cannot cover it.

`sharedThrottleWindowMs` has no isolated-scope branch. §6.2 recorded the
verdict verbatim: the throttle is shared across every probed axis — model,
account, and endpoint. The per-account branch of the earlier formula was
unreachable and is dropped.

The `reject → recover` gap is measured per account and only across pairs
where a retry was actually attempted in between. Without that filter the gap
measures how long the proxy happened to leave the account alone, not how long
the throttle held.

## 6. The single active probe

`cmd/calibrate -probe-daily` sends minimal requests to
`daily-cloudcode-pa.googleapis.com` until it returns 429, then parses
`RetryInfo.retryDelay` and `ErrorInfo.metadata.quotaResetTimeStamp` out of
the body. Per §6.1 that endpoint is the only one observed to return those
fields; `cloudcode-pa.googleapis.com` returns a bare body and no
quota, rate, or retry response headers.

**Assumption to validate.** The daily endpoint's number may describe a
different bucket than the one `cloudcode-pa` enforces. Its value is therefore
labelled `T_recover_daily` and is used only when the journal holds too few
recovery records. When both are available the tool prints both and flags a
disagreement beyond 25%. Accumulating journal evidence is what settles this;
until then the probe is a fallback, not a source of truth.

Safety, scoped to what a ten-request probe needs:

- Abort with exit code 3 on `PERMISSION_DENIED` carrying `VALIDATION_REQUIRED`
  or `validation_url`, on `"has been disabled"`, on
  `"violation of terms of service"`, and on `invalid_grant` or
  `unauthorized_client`.
- Cap the probe at 10 requests total.
- On 401, re-resolve credentials through `accounts.CredentialResolver` and
  retry once; abort only if the re-resolve itself fails. §6.3 recorded a run
  where every probe from `window-13` onward returned 401 UNAUTHENTICATED and
  contaminated the measurement — that was token lifetime, not a ban, and
  treating repeated 401s as a permanent auth failure would abort a healthy
  run.
- `SIGINT` and `SIGTERM` cancel in-flight requests and print the partial
  result.

## 7. Output

The calibrator prints the derived values and a JSON fragment. It never writes
`config.json`. The operator applies the fragment by hand.

This keeps a thin or skewed sample from silently repacing the proxy. An
`-apply` mode is a possible follow-up once the guards have been exercised
against real journals, and is out of scope here.

## 8. Files

Modify:

1. `internal/accounts/forensics.go` — add `InFlight`, `PriorMinuteRequests`,
   and `Outcome` to `Forensics429Entry`.
2. `internal/accounts/dispatcher.go` — in-flight gauge, per-account
   sixty-second ring counter, recovery record at the first success on a
   throttled account.

Create:

3. `internal/calibrate/journal.go` — read and parse the JSONL, tolerating
   older lines that lack the new fields.
4. `internal/calibrate/derive.go` — the formulas and their guards.
5. `cmd/calibrate/main.go` — CLI, `-probe-daily`, `-dry-run`, suggestion
   rendering.

Unchanged: `cmd/probe429` keeps its role as qualitative dimension triage.

## 9. Non-goals

- No `internal/probing` package. The pacer, concurrency gate, circuit
  breaker, orchestrator, and the concurrency, RPM, and payload ramp phases
  are all dropped with the active-benchmark approach.
- No cross-account isolation phase. §6.2 already returned a verdict, and
  §6.5 records that the project axis is unprobeable: both accounts use the
  single project `aicode-consumers` and no second project ID exists.
- No payload or token-weight sensitivity measurement. If the journal later
  shows rejections clustering by payload size, that becomes its own spec.
- No change to the account-selection strategy or to daily-quota accounting.
- No automatic config writes.

## 10. Verification

1. Unit tests over synthetic journal lines covering each guard boundary:
   one below the threshold, one at it, one above.
2. A journal containing only pre-migration lines, to prove the parser
   tolerates absent fields and reports insufficient data rather than
   deriving from zero values.
3. `go test ./internal/calibrate/... ./internal/accounts/...`.
4. `go run ./cmd/calibrate -dry-run` against the existing journal: reads,
   derives, prints, and makes no network call.
5. The generated JSON fragment decoded into `config.Config` with
   `DisallowUnknownFields`, to prove every emitted key matches a real struct
   tag. `config.Load` cannot serve here — it takes no path and reads the
   operator's actual `config.json`.
