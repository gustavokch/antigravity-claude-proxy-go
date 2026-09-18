# PR #83 Review Remediation — Throttle Calibration

Date: 2026-09-18
PR: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/83
Branch: `feat/throttle-calibration`

## Goal

Resolve the nine findings from the review of PR #83 so the calibrator's output
is appliable, its sample is the right sample, and its estimators are not
decided by a single outlier.

## Architecture

Three layers, unchanged in shape:

- `internal/accounts` — records evidence while serving traffic (`throttleMeter`,
  `Forensics429Entry`, dispatcher call sites).
- `internal/calibrate` — reads the journal and derives settings. Pure; no
  network, no config writes.
- `cmd/calibrate` — CLI. Prints a report and a config fragment; optionally
  probes the daily endpoint.

## Tech stack

Go 1.27rc2, standard library only. Tests are `go test ./...`.

## Spec reference

`docs/superpowers/specs/2026-09-18-throttle-calibration-design.md`

---

## Task 1 — Emit a nested config fragment

Findings: 🔴 `derive.go:175`.

- Modify: `internal/calibrate/derive.go`, `cmd/calibrate/main.go` (if it needs it)
- Test: `internal/calibrate/fragment_test.go`

1. Rewrite `TestFragmentKeysMatchTheConfigStruct` to decode `Fragment()`
   directly, with no key lifting, under `DisallowUnknownFields`.
2. `go test ./internal/calibrate/ -run Fragment` — expect failure.
3. In `Fragment`, nest the bucket under an `accountSelection` map.
4. Re-run; expect pass.
5. `git commit -m "fix(calibrate): nest the token bucket under accountSelection"`

## Task 2 — Derive only from rate-limit rejections

Findings: 🔴 `derive.go:63-76`.

- Modify: `internal/accounts/forensics.go`, `internal/accounts/dispatcher.go`,
  `internal/calibrate/journal.go`, `internal/calibrate/derive.go`
- Test: `internal/calibrate/derive_test.go`, `internal/accounts/forensics_test.go`

1. Add a test asserting that a reject whose body carries daily-quota language
   is excluded from the concurrency and RPM samples.
2. Run; expect failure.
3. Add `Reason` to `Forensics429Entry`, populate it from the `reason` the
   dispatcher already classifies, carry `reason` and `body` on
   `calibrate.Entry`, and filter `Derive` to rate-limit rejections — falling
   back to `accounts.ClassifyError(body, status)` for lines written before the
   field existed.
4. Re-run; expect pass.
5. `git commit -m "fix(calibrate): derive only from rate-limit rejections"`

## Task 3 — Replace the minimum with a low percentile

Findings: 🟡 `derive.go:86-100`.

- Modify: `internal/calibrate/derive.go`
- Test: `internal/calibrate/derive_test.go`

1. Add a test where one outlier sits far below a tight cluster and assert the
   ceiling follows the cluster, not the outlier.
2. Run; expect failure.
3. Replace `oneBelowMinimum` with a 10th-percentile estimator, still stepping
   one below and still flooring at 1.
4. Re-run; expect pass.
5. `git commit -m "fix(calibrate): use a low percentile, not the minimum"`

## Task 4 — Count every request the account makes

Findings: 🟡 `dispatcher.go:376`.

- Modify: `internal/accounts/dispatcher.go`
- Test: `internal/accounts/throttlemeter_test.go` or a dispatcher test

1. Add a test asserting the meter counts a `LoadCodeAssist` /
   `FetchAvailableModels` call against the account's minute window.
2. Run; expect failure.
3. Wrap those call sites in `Begin`/`End`.
4. Re-run; expect pass.
5. `git commit -m "fix(accounts): meter every upstream call, not just streams"`

## Task 5 — Pair recoveries without the Failures proxy

Findings: 🟡 `derive.go:123`.

- Modify: `internal/calibrate/derive.go`
- Test: `internal/calibrate/derive_test.go`

1. Add a test asserting a first-ever rejection (`failures: 0`) followed by a
   recovery still contributes a gap.
2. Run; expect failure.
3. Drop the `Failures > 0` filter and bound gaps by a maximum plausible window
   instead, so an account left idle for a day does not enter the median.
4. Re-run; expect pass.
5. `git commit -m "fix(calibrate): pair recoveries by gap, not by failure count"`

## Task 6 — Observe before the in-flight count is released

Findings: 🟡 `dispatcher.go:647`.

- Modify: `internal/accounts/dispatcher.go`, `internal/accounts/throttlemeter.go`
- Test: `internal/accounts/throttlemeter_test.go`

1. Add a test asserting `Observe` on an account with nothing in flight reports
   at least the one request being rejected.
2. Run; expect failure.
3. Have the rejection path report a floor of 1 in flight.
4. Re-run; expect pass.
5. `git commit -m "fix(accounts): count the rejected request itself as in flight"`

## Task 7 — Keep the backoff ladder monotonic

Findings: 🔵 `derive.go:180-184`.

- Modify: `internal/calibrate/derive.go`
- Test: `internal/calibrate/derive_test.go`

1. Add a test with a short window asserting ascending tiers.
2. Run; expect failure.
3. Clamp the first tier to the window when the window is shorter.
4. Re-run; expect pass.
5. `git commit -m "fix(calibrate): keep the backoff ladder ascending"`

## Task 8 — Stop the probe on an unexpected status

Findings: 🔵 `cmd/calibrate/main.go:157`.

- Modify: `cmd/calibrate/main.go`
- Test: manual — the probe loop is not unit-testable without a seam.

1. Return immediately on any status that is neither 429 nor the handled 401.
2. `go build ./...`
3. `git commit -m "fix(calibrate): stop the probe on a non-throttle status"`

## Task 9 — Release the in-flight count on a panic

Findings: 🔵 `dispatcher.go:381`.

- Modify: `internal/accounts/dispatcher.go`
- Test: covered by the existing dispatcher tests.

1. Move `meter.End` into a `defer` scoped to the attempt.
2. `go test ./internal/accounts/`
3. `git commit -m "fix(accounts): release the in-flight count on a panic"`

---

## Verification

- `go build ./...`
- `go vet ./...`
- `go test ./...`
- `go test -race ./internal/accounts/ ./internal/calibrate/`
- `git push fork feat/throttle-calibration`
