# PR #153 Remediation Plan

> Execute inline, task by task. Use the existing PR worktree and commit only the files named below.

**Goal:** Synchronize the current-baseline documentation with PR #153's agy 1.5.6 identity update.

**Architecture:** Keep the captured version/CL pins and sanitized baseline unchanged. Update only the three README descriptions of the current agy target; historical evidence and fixtures retain their recorded versions.

**Tech Stack:** Go 1.27rc2, Python standard-library identity tooling, Markdown.

**Spec:** [PR #153 and its posted review](https://github.com/gustavokch/antigravity-claude-proxy-go/pull/153), reviewed at `c0ec5d5ee5907fba793f272b0dfd0e58be16542a` against `0400a64b20a890158f827797e295a445c318661f`.

## Global Constraints

- Leave Cloud Code's TLS configuration and HTTP transport untouched.
- Preserve all unrelated worktree files and historical captures/comments/fixtures.
- Push to `fork chore/client-identity-drift`, never `origin`.
- Documentation-only remediation: use a disposable red/green consistency check, not a permanent source-text test.

## Review Findings

- **Standards:** One P3 documentation nit at `README.md:14`, `:115`, and `:134`: the current target/capture is still described as agy 1.5.5 after the linked baseline and live pins move to 1.5.6. Worst: this documentation nit; no blocking findings.
- **Spec:** Zero actionable findings; worst: none. The two pins match the installed binary and all 42 real upstream records. The baseline exactly equals the sanitized capture. No unexplained wire or endpoint changes were found.

## Review Focus

Only the current-target descriptions change. Do not repin historical 1.5.5 fixtures or imply that a unit test proves live packet parity. The unrelated Claude drift listed in the PR body remains outside this agy-only update.

### Task 1: Correct the three current-target descriptions

**Files:**
- Modify: `README.md:14,115,134`.
- Create: this plan, as required by the review/remediation workflow.
- Test: disposable consistency check below; no permanent test file.

**Interfaces:**
- Consumes: the agy 1.5.6/CL 1018951551 capture and unchanged JA4 `t13d1312h2_f57a46bbacb6_f50d94e863eb`.
- Produces: README descriptions that identify the linked current baseline as agy 1.5.6.

- [x] **Step 1: Run the disposable documentation check before editing.** Execute this Python in the PR worktree:

```python
from pathlib import Path
lines = Path("README.md").read_text().splitlines()
references = {number: lines[number - 1] for number in (14, 115, 134)}
stale = [number for number, line in references.items() if "agy 1.5.6" not in line]
assert not stale, f"Current agy baseline descriptions are stale at README.md lines {stale}"
```

Expected: failure naming lines 14, 115, and 134.

- [x] **Step 2: Make the minimal edit.** Replace `agy 1.5.5` with `agy 1.5.6` on those three lines. Do not change fingerprints, links, transport instructions, or historical evidence.
- [x] **Step 3: Run the same disposable check.** Expected: pass.
- [x] **Step 4: Record verification evidence, stage the two documentation files, and run `trail check --base fork/main` against this worktree.** No additional production test is warranted for three prose replacements; the full project verification below already exercised the unchanged PR code.
**Step 5: Commit and push using the delivery commands below.**

```bash
git add README.md docs/superpowers/plans/2026-11-11-pr153-remediation.md
git commit -m "docs(identity): align agy baseline references with 1.5.6"
git push fork HEAD:chore/client-identity-drift
```

## Verification Evidence

Executed on the PR head before the documentation-only remediation:

- `go build ./...`: passed.
- `go test -json -race ./...`: 2,164 top-level tests and 1,124 subtests passed; 3 top-level tests skipped; 41 test packages passed; zero failures.
- `python3 -m unittest test_client_drift test_tls_hello test_cc_billing test_diff_claude_code_identity test_check_identity_policy test_mitm_header_dump` from `scripts/`: 203 tests passed.
- Focused Cloud Code zero-value TLS/HTTP2 and request identity tests: 2 passed.
- Recorded evidence from run `20261111-190049`: all 42 agy requests returned 200 over HTTP/2 to `daily-cloudcode-pa.googleapis.com`, with the same 1.5.6/1018951551 User-Agent. All 42 authorization fields are redacted, and per-run/response fields are excluded from the committed baseline.
- The committed baseline exactly matches the sanitized real capture. Baseline/proxy wire comparisons found no unexplained differences after accounting for the two pending pins; endpoint sets are unchanged.
- Stored agy and proxy ClientHello bytes both fingerprint as JA4 `t13d1312h2_f57a46bbacb6_f50d94e863eb`, JA3 `03117a8ed39ef02427ebbc39f121275c`, ALPN `h2`, `http/1.1`. The proxy capture predates the pin update. No fresh live packet capture was run; do not claim a new live packet gate pass.

## Execution Record

- The disposable check failed with stale lines `[14, 115, 134]`, then passed after the three version replacements.
- The staged learning gate exited successfully with no conflicts or overlapping branch changes. Its non-blocking lexical leads were unrelated to these version-only edits.
- No production code, TLS settings, historical fixtures, or captured evidence changed during remediation. No permanent test or throwaway script file was added.
- Delivery uses the atomic commit and `fork` push commands in Step 5; the commit and PR history record their result.
