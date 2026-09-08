# PR #62 Review Remediation Plan

## Goal
Fix Bash 3.2 compatibility in `scripts/structured-output-ab.sh` by removing `declare -A` associative array syntax, ensuring cross-platform support across macOS default `/bin/bash` (Bash 3.2.57) and newer Bash environments.

## Architecture
- `scripts/structured-output-ab.sh`: Shell harness for measuring tool-call and structured-output bypass rates.
- Aggregation mechanism: Accumulate miss reasons in a newline-delimited stream and count frequencies using standard pipeline (`sort | uniq -c | while read -r count reason; do ...; done`) compatible with POSIX and Bash 3.2+.

## Tech Stack
- Bash / Shell Scripting (Bash 3.2+)
- `curl`, `jq`, `sort`, `uniq`

---

## Task 1: Refactor `scripts/structured-output-ab.sh` Reason Aggregation for Bash 3.2 Compatibility

### Target Files
- Modify: `scripts/structured-output-ab.sh`
- Test: Execute `/bin/bash scripts/structured-output-ab.sh -n 1 -u http://127.0.0.1:9999 -a tools-plain`

### Step 1: Write Failing Test / Reproduction Command
Run script under `/bin/bash` (Bash 3.2) against an unreachable endpoint to verify execution and reason formatting:
```bash
/bin/bash scripts/structured-output-ab.sh -n 2 -u http://127.0.0.1:9999 -a tools-plain
```

### Step 2: Confirm Failure
Confirm current script fails with:
`declare: -A: invalid option`

### Step 3: Minimal Implementation
Update `scripts/structured-output-ab.sh`:
- Remove `declare -A reasons=()` and `unset reasons`.
- Accumulate miss reasons as newline-delimited strings in `misses_list`.
- Parse and display grouped reasons via `sort | uniq -c`.

### Step 4: Run Test to Confirm Pass
Execute under `/bin/bash`:
```bash
/bin/bash scripts/structured-output-ab.sh -n 2 -u http://127.0.0.1:9999 -a tools-plain
```
Expect output displaying trial dots/crosses, bypass percentage, and categorized failure reasons without `declare: -A` errors.

### Step 5: Git Commit
```bash
git add scripts/structured-output-ab.sh docs/superpowers/plans/2026-09-08-pr62-review-remediation.md
git commit -m "fix(openrouter): make structured-output-ab script compatible with bash 3.2"
```
