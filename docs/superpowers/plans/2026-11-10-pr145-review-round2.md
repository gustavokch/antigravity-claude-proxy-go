# PR #145 round-two remediation plan

**Goal:** Remove false HTTP/2 drift findings and align the JA4 gate with the captured agy 1.5.5 target, without claiming unperformed live verification.

**Architecture:** Keep Go's empty `tls.Config{}` and existing HTTP/2 transport fix. The Python capture comparer ignores header ordering only for HTTP/2-to-HTTP/2 comparisons; every other comparison remains strict. Update the existing fingerprint registry sites together, retaining old packet artifacts as history.

**Tech Stack:** Go `net/http`, Python `unittest`, Bash/tshark.

**Spec reference:** PR #145 round-two Code Review, posted 2026-11-10; `.reference/claude-code-headers-20261110.jsonl`; agy 1.5.5 capture `20261110-184155/agy.jsonl`.

## Global constraints

- Execute inline; no implementation or review subagents.
- Do not customize TLS cipher suites, curves, protocol lists, or versions.
- Preserve unrelated files and prior commits. Push only to `fork`.
- Live mitmproxy/proxy verification scripts are run by the user. Missing live evidence is a blocker, not a passing gate.
- Claude Code wire captures must use an interactive session, never `--print`.

## Review focus

- HTTP/1.1 reordered headers must still report drift.
- A protocol transition must still report drift.
- HTTP/2 missing/extra headers and changed values must still report drift.
- HTTP/2 JSON body key order must still report drift.
- Historical no-ALPN packet artifacts must not be relabeled as new captures.

## Task 1: Compare HTTP/2 header semantics without incidental order

**Files:** Modify `scripts/client_drift.py:827-887`; test `scripts/test_client_drift.py:95-165`.

**Interfaces:** Consumes `compare_captures(baseline, observed, dynamic, pairs) -> (drifts, notes)` with recorded `http_version`. Produces the same interface; header order is ignored only when both versions equal `HTTP/2.0`.

- [x] Step 1: Add behavioral tests at the existing capture-comparison seam. Exact expectations:

```python
self.assertEqual(drift.compare_captures([base_h2], [reordered_h2], frozenset(), []), ([], []))
self.assertEqual(drifts, ["POST /v1:load: headers no longer sent: x-old",
                          "POST /v1:load: new headers: x-new",
                          "POST /v1:load: user-agent 'agy/1.5.5' -> 'other'"])
```

Keep the existing HTTP/1.1 order and JSON body-order tests, and add explicit protocol-transition and HTTP/2 body-order cases.

- [x] Step 2: Run `python3 -m unittest test_client_drift.CompareTest` from `scripts`; expect the reordered-HTTP/2 test to fail with a `header order` finding.
- [x] Step 3: Guard only the header-order comparison with both versions being `HTTP/2.0`. Header presence/values, HTTP version, queries and bodies remain checked.
- [x] Step 4: Run the same comparison tests; expect all to pass.
- [x] Step 5: Committed as `41d074e` (`fix(drift): ignore variable HTTP/2 header order`).

## Task 2: Move the JA4 gate and documentation to the agy 1.5.5 target

**Files:** Modify `scripts/verify-ja4.sh:8,19`; `README.md:14,115-128,136,1156`; `.reference/client-drift/agy.jsonl`; this plan records verification evidence.

**Interfaces:** Consumes the real agy capture's `tls.ja4`; produces the existing `agy.ja4` pin, consistently read by `client_drift.read_pin` across the gate and README.

- [x] Step 1: Run a throwaway assertion comparing `read_pin(ROOT, PIN['agy.ja4'])` with the independently recorded target `t13d1312h2_f57a46bbacb6_f50d94e863eb`; expect failure against the stale `t13d131100` pin. No permanent test of a copied literal.
- [x] Step 2: Update all four registry sites and the displayed tshark output. Describe the prior packet captures as historical; do not rewrite their capture dates or contents. Explain that `ForceAttemptHTTP2` lets net/http configure ALPN without a custom TLS configuration.
- [x] Refresh `.reference/client-drift/agy.jsonl` with `baseline_records` applied to the actual `20261110-184155/agy.jsonl` capture. The reviewed migration changes only version/build, protocol, h1-only header presence and header order. Do not relabel a proxy capture or synthesize any traffic.
- [x] Step 3: Repeat the throwaway assertion and run `bash -n scripts/verify-ja4.sh`; expect success. The full identity Python suite verifies registry consistency.
- [ ] Step 4: Request user-run `scripts/verify-ja4.sh`; inspect actual output before claiming packet parity. A skip is not a pass.
Commit: `git add scripts/verify-ja4.sh README.md .reference/client-drift/agy.jsonl docs/superpowers/plans/2026-11-10-pr145-review-round2.md && git commit -m 'fix(identity): align the JA4 gate with agy 1.5.5'`.

## Task 3: Interactive Claude capture and final verification

**Files:** Do not change Claude identity values without interactive evidence. Update a baseline only from actual redacted captured requests.

**Interfaces:** A user-run interactive Claude Code 2.1.322 capture provides headers/body fingerprints for messages and, separately, `/context` counts. A print capture cannot prove these paths.

- [ ] Obtain and inspect an interactive capture; compare the new `tool-provenance-2026-10-26` beta and `x-claude-code-sandy-plover: []` header. Record any unresolved evidence requirement explicitly.
- [x] Run `go build ./...` and `go test -json ./...` once for the final Go build/test gate, counting test outcomes from JSON.
- [x] Run all six identity script test modules listed in `IDENTITY_TESTS`.
- [x] Review the final changed ranges inline. Run `trail check` before committing/pushing; if the installed trail lacks that command, record the limitation rather than inventing a pass.
- [ ] Push `git push fork HEAD:chore/client-identity-drift` and post resolved findings plus any live-validation blockers to PR #145.

## Execution record

- Existing PR worktree: `~/.cache/antigravity-proxy/client-drift/worktree`, starting at `287180e`; tracked worktree clean. The main checkout's untracked files are untouched.
- Scope decision: leave historical `.reference/*current-baseline.txt` and pcaps unchanged. Their dates identify real older captures. Update the current gate target and clarify the README instead.
- Scope decision: do not broaden this remediation into a new Claude onboarding/terminal driver. The existing print capture is insufficient evidence; obtaining an approved interactive capture is a live prerequisite.
- Scope decision: refresh the reviewed agy 1.5.5 wire baseline now. Merely waiting for the next drift run cannot clear old protocol findings: unexplained baseline drift is manual and prevents automatic baseline refresh.
- Task 1 proof: comparison tests first failed on two false header-order findings, then passed 17/17. The stale JA4 throwaway assertion independently failed, then passed after all registry sites were updated.
- `trail check --base fork/main` ran and reported one lexical conflict on the prior beta-list test comment (`apply_test.go:623`, matched against a cache-control learning). No cache-control or conversation-state code changes in this PR; the commit message records why that warning is not a behavioral conflict.
- Final local verification: `go build ./...` passed; `go test -json ./...` passed 3,256 tests, skipped 3 tests, and failed 0 (40 tested packages; 30 packages without tests). The six identity script modules passed 203/203 tests. `bash -n scripts/verify-ja4.sh` and the JA4 registry assertion passed.
- Baseline proof: the 39 committed agy requests exactly equal the real capture after `baseline_records` removes per-run/response data; the refreshed baseline compares without drift. All 10 request classes preserve stable header values, body shapes and endpoints across the reviewed protocol migration.
- Final review: self-review inline, as explicitly requested; no reviewer subagent was used. No further code defect found in the changed ranges. The packet gate and interactive Claude capture remain unverified prerequisites, not a completed definition of done.
