# Identity Gate Scope: Cover T1 and T4 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend `scripts/verify-claude-code-identity.sh` so its pass also proves the two things it silently does not cover today: that a custom endpoint authenticated by API key keeps its credential and is *not* normalized (T1), and that the `/v1/models` calls carry the captured discovery User-Agent (T4).

**Architecture:** The gate already boots the proxy behind `mitmdump` and writes one JSONL record per observed flow. It drives one request and diffs it against the committed capture. This plan keeps that phase byte-for-byte and adds three more, each isolated by snapshotting the observed file and truncating it between phases so every phase is diffed against its own records alone. The fallible comparison logic goes in a new Python checker with unit tests, mirroring how `diff_claude_code_identity.py` and its test file already split orchestration from judgement. The checker hardcodes no fingerprint strings: every expected value is read out of the committed baseline capture, so the capture stays the single source of truth.

**Tech Stack:** Bash 4+, `mitmdump` (mitmproxy), `jq`, `nc`, Go 1.x, Python 3 (stdlib `unittest`, run under `python3 -m pytest`).

**Spec:** `docs/superpowers/plans/2026-09-23-pr91-second-pass-remediation.md` (tasks T1 and T4), and the coverage gap reported against it: the gate narrows `gatewayOrder` to `["claudecode"]` and drives one request, so it proves only the pooled gateway's normalized `POST /v1/messages`.

## Global Constraints

- **The gate cannot be run from a Claude Code session.** The permission classifier refuses it as External System Writes, because it starts `mitmdump` plus a proxy listener. Every step below that runs `scripts/verify-claude-code-identity.sh` must be handed to the operator as `! ./scripts/verify-claude-code-identity.sh`. The Python steps have no such restriction and run normally.
- **Do not change the existing phase.** The first driven request, the `diff_claude_code_identity.py` invocation and the `<x2>` system-shape assertion must keep their current semantics exactly. Renumbering the `=== [n/N] ===` banners is the only edit permitted to them.
- **No fingerprint literals in new code.** Expected User-Agent values, request-class values and header names come from `.reference/claude-code-headers-20260923.jsonl` at runtime. The one exception is the gate's own stub credential, which is not a captured value.
- **Skip loudly, never silently.** A missing tool calls `skip`, which prints "this is NOT a pass" and exits 0. Do not add a code path that exits 0 without either a pass banner or that warning.
- Python: standard library only. Tests are `unittest.TestCase` classes in `scripts/test_*.py`, importing the module under test after `sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))`.
- Bash: the file runs under `set -euo pipefail`. A command whose non-zero exit is expected must end in `|| true`.

---

### Task 1: The wire-identity policy checker

The differ answers "does this request match the capture?". It cannot answer "should this request have matched the capture at all?" — handed a deliberately non-normalized request it would report twenty drifts, all of them correct and all of them wrong about the point. This task adds the second question as its own module.

**Files:**
- Create: `scripts/check_identity_policy.py`
- Create: `scripts/test_check_identity_policy.py`

**Interfaces:**
- Consumes: `diff_claude_code_identity.header_map(record) -> dict[str, tuple[str, object]]`, which lower-cases the lookup key and keeps the wire name.
- Produces, for Tasks 2 and 3:
  - `python3 scripts/check_identity_policy.py normalized --baseline B --observed O`
  - `python3 scripts/check_identity_policy.py passthrough --observed O --caller-user-agent UA --api-key-sha256 HEX`
  - `python3 scripts/check_identity_policy.py discovery --baseline B --observed O --min-records N`
  - Exit 0 when the policy holds, 1 with one `  - <problem>` line per violation on stderr otherwise.

- [ ] **Step 1: Write the failing tests**

Create `scripts/test_check_identity_policy.py`:

```python
#!/usr/bin/env python3
"""Tests for the wire-identity policy checker.

These run without mitmproxy, so the policy logic is testable everywhere the
drift gate itself has to skip. Each case is written to fail if the checker
stops catching a policy violation, not if the baseline moves alongside it.

Run: python3 -m pytest scripts/test_check_identity_policy.py -q
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import check_identity_policy as policy  # noqa: E402

CAPTURED_UA = "claude-cli/2.1.280 (external, sdk-cli)"
DISCOVERY_UA = "claude-code/2.1.280"
CALLER_UA = "foreign-harness/1.0"
# sha256 of "gate-key", as the mitm addon records an x-api-key value.
GATE_KEY = "gate-key"
GATE_KEY_SHA = "a4bb1c07d8d1f0a0e5dbd0f0a1f37f4ef3d7b7d3cfbcfb0e4bb8b4d4c3f1e0a2"


def baseline_post(ua=CAPTURED_UA):
    return {
        "method": "POST",
        "path": "/v1/messages?beta=true",
        "headers": [
            ["User-Agent", ua],
            ["x-app", "cli"],
            ["x-claude-code-request-class", "main"],
            ["X-Stainless-OS", "Linux"],
        ],
    }


def baseline_get(ua=DISCOVERY_UA, path="/api/claude_code/settings"):
    return {"method": "GET", "path": path, "headers": [["User-Agent", ua]]}


def observed_normalized(**overrides):
    record = {
        "method": "POST",
        "path": "/v1/messages?beta=true",
        "headers": [
            ["user-agent", CAPTURED_UA],
            ["x-claude-code-request-class", "main"],
            ["x-stainless-os", "Linux"],
        ],
    }
    record.update(overrides)
    return record


def observed_passthrough(**overrides):
    record = {
        "method": "POST",
        "path": "/v1/messages",
        "headers": [
            ["user-agent", CALLER_UA],
            ["x-api-key", {"redacted": True, "scheme": "", "sha256": GATE_KEY_SHA, "len": 8}],
        ],
    }
    record.update(overrides)
    return record


def write_jsonl(records):
    handle = tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False)
    for record in records:
        handle.write(json.dumps(record) + "\n")
    handle.close()
    return handle.name


class NormalizedTest(unittest.TestCase):
    def test_accepts_a_normalized_request(self):
        problems = policy.check_normalized(baseline_post(), observed_normalized())
        self.assertEqual(problems, [])

    def test_rejects_a_foreign_user_agent(self):
        observed = observed_normalized(
            headers=[["user-agent", CALLER_UA], ["x-claude-code-request-class", "main"]]
        )
        problems = policy.check_normalized(baseline_post(), observed)
        self.assertTrue(any("User-Agent" in p for p in problems), problems)

    def test_rejects_a_surviving_api_key(self):
        observed = observed_normalized(
            headers=[
                ["user-agent", CAPTURED_UA],
                ["x-claude-code-request-class", "main"],
                ["x-api-key", {"redacted": True, "sha256": GATE_KEY_SHA, "len": 8}],
            ]
        )
        problems = policy.check_normalized(baseline_post(), observed)
        self.assertTrue(any("x-api-key" in p for p in problems), problems)

    def test_rejects_a_missing_captured_query(self):
        observed = observed_normalized(path="/v1/messages")
        problems = policy.check_normalized(baseline_post(), observed)
        self.assertTrue(any("beta=true" in p for p in problems), problems)


class PassthroughTest(unittest.TestCase):
    def test_accepts_an_unnormalized_keyed_request(self):
        problems = policy.check_passthrough(observed_passthrough(), CALLER_UA, GATE_KEY_SHA)
        self.assertEqual(problems, [])

    def test_rejects_a_deleted_api_key(self):
        observed = observed_passthrough(headers=[["user-agent", CALLER_UA]])
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("x-api-key" in p for p in problems), problems)

    def test_rejects_a_different_api_key(self):
        observed = observed_passthrough(
            headers=[
                ["user-agent", CALLER_UA],
                ["x-api-key", {"redacted": True, "sha256": "0" * 64, "len": 8}],
            ]
        )
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("sha256" in p for p in problems), problems)

    def test_rejects_a_normalized_request(self):
        observed = observed_passthrough(
            headers=[
                ["user-agent", CAPTURED_UA],
                ["x-claude-code-request-class", "main"],
                ["x-api-key", {"redacted": True, "sha256": GATE_KEY_SHA, "len": 8}],
            ]
        )
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("User-Agent" in p for p in problems), problems)
        self.assertTrue(any("x-claude-code-request-class" in p for p in problems), problems)

    def test_rejects_the_captured_query_being_added(self):
        observed = observed_passthrough(path="/v1/messages?beta=true")
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("beta=true" in p for p in problems), problems)


class DiscoveryTest(unittest.TestCase):
    def test_accepts_the_captured_discovery_user_agent(self):
        observed = [
            {"method": "GET", "path": "/v1/models", "headers": [["user-agent", DISCOVERY_UA]]}
        ] * 3
        problems = policy.check_discovery([baseline_get()], observed, min_records=3)
        self.assertEqual(problems, [])

    def test_rejects_the_go_default(self):
        observed = [
            {
                "method": "GET",
                "path": "/v1/models",
                "headers": [["user-agent", "Go-http-client/1.1"]],
            }
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=1)
        self.assertTrue(any("Go-http-client" in p for p in problems), problems)

    def test_checks_a_non_get_discovery_request_too(self):
        observed = [
            {
                "method": "POST",
                "path": "/v1/oauth/token",
                "headers": [["user-agent", "Go-http-client/1.1"]],
            }
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=1)
        self.assertTrue(any("/v1/oauth/token" in p for p in problems), problems)

    def test_rejects_a_stale_version(self):
        observed = [
            {
                "method": "GET",
                "path": "/v1/models",
                "headers": [["user-agent", "Claude-Code/2.1.246"]],
            }
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=1)
        self.assertTrue(any("2.1.246" in p for p in problems), problems)

    def test_rejects_too_few_records(self):
        observed = [
            {"method": "GET", "path": "/v1/models", "headers": [["user-agent", DISCOVERY_UA]]}
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=3)
        self.assertTrue(any("3" in p for p in problems), problems)

    def test_rejects_a_baseline_without_a_get(self):
        problems = policy.check_discovery([], [], min_records=1)
        self.assertTrue(any("baseline" in p for p in problems), problems)


class LoaderTest(unittest.TestCase):
    def test_splits_records_by_method_and_path(self):
        path = write_jsonl(
            [
                observed_normalized(),
                {"method": "GET", "path": "/v1/models", "headers": []},
                {"method": "POST", "path": "/v1/oauth/token", "headers": []},
            ]
        )
        try:
            records = policy.load_records(path)
            self.assertEqual(len(policy.messages_records(records)), 1)
            self.assertEqual(len(policy.models_records(records)), 1)
            # Wider than models_records on purpose: the oauth POST is a
            # discovery-family request too.
            self.assertEqual(len(policy.discovery_records(records)), 2)
        finally:
            os.unlink(path)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Fix the fixture hash, then run the tests to verify they fail**

`GATE_KEY_SHA` above is a placeholder digest and must be replaced with the real one before the suite means anything:

```bash
printf '%s' 'gate-key' | shasum -a 256 | cut -d' ' -f1
```

Paste that value over `GATE_KEY_SHA` in the test file. Then:

Run: `python3 -m pytest scripts/test_check_identity_policy.py -q`
Expected: collection error, `ModuleNotFoundError: No module named 'check_identity_policy'`

- [ ] **Step 3: Write the checker**

Create `scripts/check_identity_policy.py`:

```python
#!/usr/bin/env python3
"""Assert WHICH requests get a Claude Code wire identity, not whether one matches.

diff_claude_code_identity.py answers "does this request match the capture?".
It cannot answer "should this request have matched the capture at all?" —
handed a deliberately unnormalized request it reports twenty drifts, every one
of them correct and every one of them missing the point.

Three policies, one per subcommand:

  normalized   the request carries the captured identity (pooled gateway, and
               a custom endpoint with no configured apiKey)
  passthrough  the request carries the CALLER's identity and its own API key,
               because internal/api/server.go refuses to normalize an endpoint
               whose credential normalization would delete
  discovery    every GET /v1/models carries the captured discovery User-Agent,
               not Go's default and not a stale version

No fingerprint value is written here. Each expected value is read out of the
committed baseline capture, so the capture stays the single source of truth.
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from diff_claude_code_identity import header_map  # noqa: E402


def load_records(path: str) -> list[dict]:
    """Every record in a capture file, in order."""
    records = []
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            records.append(json.loads(line))
    return records


def messages_records(records: list[dict]) -> list[dict]:
    return [
        r
        for r in records
        if r.get("method") == "POST" and str(r.get("path", "")).startswith("/v1/messages")
    ]


def models_records(records: list[dict]) -> list[dict]:
    return [
        r
        for r in records
        if r.get("method") == "GET" and str(r.get("path", "")).startswith("/v1/models")
    ]


def discovery_records(records: list[dict]) -> list[dict]:
    """Every request that is NOT the messages path.

    Deliberately wider than GET /v1/models. DiscoveryUserAgent's own comment
    calls it a different family from the messages path, so the rule is about
    the path, not about one caller: /v1/oauth/token and /api/oauth/profile in
    internal/auth belong to it too. Widening the selector costs nothing and
    means a future request that starts reaching api.anthropic.com is checked
    rather than ignored.
    """
    return [
        r
        for r in records
        if not (
            r.get("method") == "POST"
            and str(r.get("path", "")).startswith("/v1/messages")
        )
    ]


def get_records(records: list[dict]) -> list[dict]:
    return [r for r in records if r.get("method") == "GET"]


def user_agent(record: dict) -> object:
    entry = header_map(record).get("user-agent")
    return entry[1] if entry else None


def check_normalized(baseline: dict, observed: dict) -> list[str]:
    """The observed request must present the captured identity."""
    problems: list[str] = []
    headers = header_map(observed)

    expected_ua = user_agent(baseline)
    observed_ua = user_agent(observed)
    if observed_ua != expected_ua:
        problems.append(f"User-Agent is {observed_ua!r}, want the captured {expected_ua!r}")

    expected_class = header_map(baseline).get("x-claude-code-request-class")
    if expected_class is not None:
        entry = headers.get("x-claude-code-request-class")
        if entry is None:
            problems.append("x-claude-code-request-class is absent; the request was not normalized")
        elif entry[1] != expected_class[1]:
            problems.append(
                f"x-claude-code-request-class is {entry[1]!r}, want {expected_class[1]!r}"
            )

    if "x-api-key" in headers:
        problems.append("x-api-key reached the upstream; the captured OAuth request carries none")

    if "beta=true" not in str(observed.get("path", "")):
        problems.append(
            f"path is {observed.get('path')!r}; the captured request carries beta=true"
        )

    return problems


def check_passthrough(observed: dict, caller_user_agent: str, api_key_sha256: str) -> list[str]:
    """The observed request must present the CALLER's identity and its own key."""
    problems: list[str] = []
    headers = header_map(observed)

    entry = headers.get("x-api-key")
    if entry is None:
        problems.append(
            "x-api-key is absent; an endpoint configured with one cannot authenticate without it"
        )
    else:
        value = entry[1]
        digest = value.get("sha256") if isinstance(value, dict) else None
        if digest is None:
            problems.append(f"x-api-key was recorded as {value!r}, want a redacted sha256")
        elif digest != api_key_sha256:
            problems.append(
                f"x-api-key sha256 is {digest}, want {api_key_sha256}; a different key was sent"
            )

    observed_ua = user_agent(observed)
    if observed_ua != caller_user_agent:
        problems.append(
            f"User-Agent is {observed_ua!r}, want the caller's {caller_user_agent!r}; "
            "this endpoint must not be normalized"
        )

    for name in ("x-claude-code-request-class", "x-stainless-os"):
        if name in headers:
            problems.append(f"{name} was added; this endpoint must not be normalized")

    if "beta=true" in str(observed.get("path", "")):
        problems.append(
            f"path is {observed.get('path')!r}; the captured query must not be added here"
        )

    return problems


def check_discovery(
    baseline_gets: list[dict], observed_gets: list[dict], min_records: int
) -> list[str]:
    """Every observed non-messages request must carry the captured User-Agent."""
    problems: list[str] = []

    expected = {user_agent(r) for r in baseline_gets}
    expected.discard(None)
    if not expected:
        problems.append("the baseline holds no GET record to take a discovery User-Agent from")
        return problems
    if len(expected) > 1:
        problems.append(f"the baseline GETs disagree on User-Agent: {sorted(expected)}")
        return problems
    expected_ua = expected.pop()

    if len(observed_gets) < min_records:
        problems.append(
            f"only {len(observed_gets)} discovery request(s) reached the wire, want {min_records}"
        )

    for record in observed_gets:
        observed_ua = user_agent(record)
        if observed_ua != expected_ua:
            problems.append(
                f"{record.get('method')} {record.get('path')} sent User-Agent "
                f"{observed_ua!r}, want the captured {expected_ua!r}"
            )

    return problems


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="policy", required=True)

    p_norm = sub.add_parser("normalized")
    p_norm.add_argument("--baseline", required=True)
    p_norm.add_argument("--observed", required=True)

    p_pass = sub.add_parser("passthrough")
    p_pass.add_argument("--observed", required=True)
    p_pass.add_argument("--caller-user-agent", required=True)
    p_pass.add_argument("--api-key-sha256", required=True)

    p_disc = sub.add_parser("discovery")
    p_disc.add_argument("--baseline", required=True)
    p_disc.add_argument("--observed", required=True)
    p_disc.add_argument("--min-records", type=int, default=1)

    args = parser.parse_args()

    if args.policy == "discovery":
        baseline_gets = get_records(load_records(args.baseline))
        observed_gets = discovery_records(load_records(args.observed))
        problems = check_discovery(baseline_gets, observed_gets, args.min_records)
        subject = f"{len(observed_gets)} discovery request(s)"
    else:
        observed_posts = messages_records(load_records(args.observed))
        if not observed_posts:
            print(f"no POST /v1/messages record in {args.observed}", file=sys.stderr)
            return 1
        observed = observed_posts[0]
        if args.policy == "normalized":
            baseline_posts = messages_records(load_records(args.baseline))
            if not baseline_posts:
                print(f"no POST /v1/messages record in {args.baseline}", file=sys.stderr)
                return 1
            problems = check_normalized(baseline_posts[0], observed)
        else:
            problems = check_passthrough(
                observed, args.caller_user_agent, args.api_key_sha256
            )
        subject = f"POST {observed.get('path')}"

    if problems:
        print(f"{len(problems)} policy violation(s) in {subject}:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    print(f"policy {args.policy} holds for {subject}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `python3 -m pytest scripts/test_check_identity_policy.py -q`
Expected: PASS, 16 tests.

Then confirm nothing else broke:

Run: `python3 -m pytest scripts/test_*.py -q`
Expected: PASS, 70 tests (54 existing plus 16 new).

- [ ] **Step 5: Verify the checker agrees with the committed capture**

This proves the baseline really carries the values the checker reads out of it, which the unit tests cannot — they use synthetic fixtures.

Run:
```bash
python3 scripts/check_identity_policy.py normalized \
  --baseline .reference/claude-code-headers-20260923.jsonl \
  --observed .reference/claude-code-headers-20260923.jsonl
python3 scripts/check_identity_policy.py discovery \
  --baseline .reference/claude-code-headers-20260923.jsonl \
  --observed .reference/claude-code-headers-20260923.jsonl --min-records 0
```
Expected: exactly these two lines, exit 0 on both.

```
policy normalized holds for POST /v1/messages?beta=true
policy discovery holds for 4 discovery request(s)
```

The four are that capture's `/api/claude_code/settings` and `policy_limits` GETs, all carrying one User-Agent — the value the gate will hold `/v1/models` to. `--min-records 0` is right here and nowhere else: this file is the baseline, not a gate run.

- [ ] **Step 6: Commit**

```bash
git add scripts/check_identity_policy.py scripts/test_check_identity_policy.py
git commit -m "test(scripts): add the wire-identity policy checker

The differ answers whether a request matches the capture. It cannot answer
whether the request should have matched at all: handed a deliberately
unnormalized one it reports twenty correct drifts and misses the point.

Three policies, every expected value read out of the committed baseline so
no fingerprint string is duplicated here."
```

---

### Task 2: Gate the custom-endpoint credential rule (T1)

The gate narrows `gatewayOrder` to `["claudecode"]` and never builds a `customEndpoints` entry, so neither custom-endpoint path has ever been on the wire under it. T1 — an endpoint with a configured `apiKey` is sent unnormalized, because `x-api-key` is on the omit list and `ApplyHeaders` deletes it — is unit-tested only.

The pooled request is unaffected by these two: `tryCustomEndpointGateway` keys on the exact model name, and neither new model is in the Claude Code allowlist.

**Files:**
- Modify: `scripts/verify-claude-code-identity.sh`

**Interfaces:**
- Consumes: `check_identity_policy.py normalized|passthrough` from Task 1.
- Produces, for Task 3: the `snapshot_phase` helper and the `OBSERVED_POOLED` naming convention.

- [ ] **Step 1: Add the phase-isolation helper**

The differ reads `observed_records[0]`, so a second driven `POST /v1/messages` in the same file would change what the existing assertion examines. Each phase gets its own file instead. The mitm addon opens the output with `open(path, "a")` per record and closes it, so truncating between phases is safe.

Insert after the `cleanup()`/`trap` block (currently ending at line 82), before `echo "=== [1/6] ..."`:

```bash
# Each phase is diffed against its own records alone. The differ reads the
# FIRST POST /v1/messages record it finds, so a later phase's request would
# otherwise change what the earlier assertion examines. The mitm addon opens
# the output per record with mode "a" and closes it again, so truncating here
# cannot corrupt a write in flight.
snapshot_phase() {
  local destination="$1"
  local what="$2"
  for _ in $(seq 1 50); do
    [[ -s "${OBSERVED}" ]] && break
    sleep 0.2
  done
  if [[ ! -s "${OBSERVED}" ]]; then
    KEEP_WORK_DIR=1
    echo "mitmdump log:" >&2
    tail -20 "${MITM_LOG}" >&2 || true
    echo "proxy log:" >&2
    tail -20 "${PROXY_LOG}" >&2 || true
    fail "no flow reached mitmdump for ${what}. Work dir kept at ${WORK_DIR}"
  fi
  cp "${OBSERVED}" "${destination}"
  : > "${OBSERVED}"
}
```

- [ ] **Step 2: Declare the new phase files and the custom-endpoint credential**

First add `shasum` to the tool preflight, so a machine without it skips loudly instead of failing later with a digest mismatch. Change the `for tool in ...` line (currently line 60) to:

```bash
for tool in mitmdump jq go nc python3 shasum; do
```

Replace the `OBSERVED=` line (currently line 38) with:

```bash
OBSERVED="${WORK_DIR}/observed.jsonl"
OBSERVED_POOLED="${WORK_DIR}/observed-pooled.jsonl"
OBSERVED_CUSTOM_KEYLESS="${WORK_DIR}/observed-custom-keyless.jsonl"
OBSERVED_CUSTOM_KEYED="${WORK_DIR}/observed-custom-keyed.jsonl"
```

Then, after the `GATE_MODEL` declaration (currently line 44), add:

```bash
# Two custom endpoints, identical but for the credential. Both are
# Anthropic-shaped, so isAnthropicEndpoint matches and normalization is in
# scope for both; only the configured apiKey decides. Neither model name is in
# the Claude Code allowlist, so the pooled gateway declines them and this gate's
# first phase is untouched.
GATE_CUSTOM_KEYLESS_MODEL="gate-custom-keyless"
GATE_CUSTOM_KEYED_MODEL="gate-custom-keyed"
GATE_CUSTOM_API_KEY="gate-custom-endpoint-credential-not-valid-upstream"
# The mitm addon redacts any header whose name matches api[_-]?key, hashing the
# whole value. Comparing that digest proves the exact key survived without the
# key ever appearing in the capture.
GATE_CUSTOM_API_KEY_SHA256="$(printf '%s' "${GATE_CUSTOM_API_KEY}" | shasum -a 256 | cut -d' ' -f1)"
# The caller's own fingerprint, which a non-normalized endpoint must see
# unchanged. Kept in one place because both the driven request and the
# passthrough assertion need the same value.
GATE_CALLER_USER_AGENT="foreign-harness/1.0"
```

- [ ] **Step 3: Use the caller User-Agent variable in the existing request**

In the existing `curl` (currently line 216), replace the literal header with the variable so the driven value and the asserted value cannot drift:

```bash
  -H "User-Agent: ${GATE_CALLER_USER_AGENT}" \
```

- [ ] **Step 4: Add the two custom endpoints to the stub configuration**

In the `cat > "${CONFIG_DIR}/config.json"` heredoc, replace the `gatewayOrder` line and add a `customEndpoints` block. The full replacement for the first line of the JSON object and the addition after the `claudecode` block:

```json
  "gatewayOrder": { "byModel": {}, "order": ["claudecode", "custom"] },
  "customEndpoints": {
    "${GATE_CUSTOM_KEYLESS_MODEL}": {
      "url": "https://api.anthropic.com/v1/messages"
    },
    "${GATE_CUSTOM_KEYED_MODEL}": {
      "url": "https://api.anthropic.com/v1/messages",
      "apiKey": "${GATE_CUSTOM_API_KEY}"
    }
  },
```

The heredoc is unquoted (`<<JSON`), so these shell variables expand. Place `customEndpoints` immediately after the `gatewayOrder` line and leave the `claudecode` block exactly as it is.

- [ ] **Step 5: Renumber the banners and snapshot the existing phase**

The gate grows from 6 steps to 9. Change `[1/6]` through `[4/6]` to `[1/9]` through `[4/9]`, `[5/6]` to `[5/9]`, and `[6/6]` to `[6/9]`.

Then replace the existing readiness block that waits on `${OBSERVED}` (currently lines 222–234, from the `# Give mitmdump time to flush the record.` comment through the closing `fi` of the `no flow reached mitmdump` failure) with:

```bash
snapshot_phase "${OBSERVED_POOLED}" "the pooled gateway request"
```

And point the differ and the system-shape check at the snapshot instead of the live file: in the `[6/9]` block, change `--observed "${OBSERVED}"` to `--observed "${OBSERVED_POOLED}"`, change the two `Observed capture kept at ${OBSERVED}` messages to `${OBSERVED_POOLED}`, and change the `jq ... -s "${OBSERVED}"` to `-s "${OBSERVED_POOLED}"`.

- [ ] **Step 6: Add the keyless phase**

Append after the existing `[6/9]` block's final `fi`:

```bash
echo "=== [7/9] Driving a keyless custom endpoint ==="
# Anthropic-shaped and carrying no credential of its own, so normalization
# applies: this is the half of T1 that must keep working. Same hostile input as
# the pooled request, so a regression shows up as the caller's own values
# reaching the wire.
curl -sS -o /dev/null --max-time 60 \
  -X POST "http://127.0.0.1:${PROXY_PORT}/v1/messages" \
  -H 'Content-Type: application/json' \
  -H "User-Agent: ${GATE_CALLER_USER_AGENT}" \
  -H 'x-app: foreign-app' \
  -d "{\"model\":\"${GATE_CUSTOM_KEYLESS_MODEL}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"ok\"}]}" \
  >/dev/null 2>&1 || true

snapshot_phase "${OBSERVED_CUSTOM_KEYLESS}" "the keyless custom endpoint"

if ! python3 "${REPO_ROOT}/scripts/check_identity_policy.py" normalized \
  --baseline "${BASELINE}" \
  --observed "${OBSERVED_CUSTOM_KEYLESS}"; then
  KEEP_WORK_DIR=1
  echo "Observed capture kept at ${OBSERVED_CUSTOM_KEYLESS}" >&2
  fail "a keyless Anthropic-shaped custom endpoint was not normalized"
fi

echo "=== [8/9] Driving a custom endpoint authenticated by API key ==="
# The T1 regression, live. x-api-key is on the omit list and ApplyHeaders
# deletes every omitted name, so normalizing this endpoint would send it
# authenticated by nothing. internal/api/server.go refuses to normalize it at
# all; the wire must therefore show the caller's identity and the configured
# key, not the captured identity.
curl -sS -o /dev/null --max-time 60 \
  -X POST "http://127.0.0.1:${PROXY_PORT}/v1/messages" \
  -H 'Content-Type: application/json' \
  -H "User-Agent: ${GATE_CALLER_USER_AGENT}" \
  -H 'x-app: foreign-app' \
  -d "{\"model\":\"${GATE_CUSTOM_KEYED_MODEL}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"ok\"}]}" \
  >/dev/null 2>&1 || true

snapshot_phase "${OBSERVED_CUSTOM_KEYED}" "the keyed custom endpoint"

if ! python3 "${REPO_ROOT}/scripts/check_identity_policy.py" passthrough \
  --observed "${OBSERVED_CUSTOM_KEYED}" \
  --caller-user-agent "${GATE_CALLER_USER_AGENT}" \
  --api-key-sha256 "${GATE_CUSTOM_API_KEY_SHA256}"; then
  KEEP_WORK_DIR=1
  echo "ERROR: an endpoint configured with an apiKey must reach the upstream carrying it." >&2
  echo "ERROR: x-api-key is on the omit list, so normalizing such an endpoint deletes the" >&2
  echo "ERROR: credential and the endpoint authenticates by nothing." >&2
  echo "Observed capture kept at ${OBSERVED_CUSTOM_KEYED}" >&2
  fail "the custom-endpoint credential rule was broken"
fi
```

- [ ] **Step 7: Update the gate's header comment**

The comment at the top of the file describes one request through the pooled gateway. Replace the first paragraph of the description (currently lines 6–9) with:

```bash
# Boots the proxy behind mitmdump, drives four requests across the three paths
# that carry a wire identity, and checks each against the committed capture at
# .reference/claude-code-headers-20260923.jsonl:
#
#   pooled gateway, normalized      full header and body diff against the capture
#   custom endpoint, no apiKey      must present the captured identity
#   custom endpoint with an apiKey  must present the CALLER's identity and keep
#                                   the key, because x-api-key is on the omit list
#   GET /v1/models, three callers   must carry the captured discovery User-Agent
```

- [ ] **Step 8: Run the gate**

This step is the operator's — the classifier refuses it from a Claude session. Hand it over:

Run: `! ./scripts/verify-claude-code-identity.sh`
Expected: `=== [8/9] ===` reached and the pass banner printed. If `[7/9]` fails with a foreign User-Agent, the keyless endpoint is not being normalized, which means either the model name collided with the allowlist or `custom` is missing from `gatewayOrder`.

- [ ] **Step 9: Prove the new assertion bites**

A gate that cannot fail proves nothing. Falsify it deliberately, once:

```bash
# In scripts/verify-claude-code-identity.sh, temporarily delete the
# "apiKey": "${GATE_CUSTOM_API_KEY}" line from the keyed endpoint's config block.
```

Run: `! ./scripts/verify-claude-code-identity.sh`
Expected: FAIL at `[8/9]` with `x-api-key is absent` and `User-Agent is ... want the caller's ...`, because a keyless endpoint gets normalized.

Restore the line with `git checkout scripts/verify-claude-code-identity.sh` if it is not yet committed, or by re-adding it, and re-run to confirm the pass returns.

- [ ] **Step 10: Commit**

```bash
git add scripts/verify-claude-code-identity.sh
git commit -m "test(scripts): gate the custom-endpoint credential rule

The gate narrowed gatewayOrder to claudecode alone, so neither custom-endpoint
path had ever been on the wire under it and the apiKey refusal was unit-tested
only. Two endpoints now run, identical but for the credential: the keyless one
must present the captured identity, the keyed one must present the caller's and
keep its key.

Each phase is snapshotted to its own file, because the differ reads the first
POST /v1/messages record it finds and a second driven request would otherwise
change what the pooled assertion examines."
```

---

### Task 3: Gate the discovery User-Agent (T4)

Five sites sent `claude-code/2.1.246` while the captured value was `claude-code/2.1.280`, and two of them spelled it `Claude-Code`, a capitalisation no observed request uses. `ValidateAccount` sent no User-Agent at all, so Go's transport announced the standard library. All of that is now fixed and unit-tested, and none of it is on the gate: the gate drives one `POST /v1/messages` and never a `GET`.

Three management routes reach `/v1/models`, one per client method, and none of them goes through account selection — so the pooled stub's 401 cooldown does not block them.

**Not covered, and why.** T4 also fixed three sites in `internal/auth`, which post to `https://api.anthropic.com/v1/oauth/token` and get `https://api.anthropic.com/api/oauth/profile` — the same host the gate's mitm filter allows, so they would be recorded if they fired. They are left out because neither can be driven reliably:

- `POST /api/claudecode/accounts/{id}/refresh` reaches `RefreshToken` only when the pool carries a token refresher, and the refresher is attached in `getOrCreateCCPool` **only** when that function is reached through its server-bound method. The package-level `getOrCreateCCPool` at `internal/api/claudecode_proxy.go:28` passes a nil `*Server`, and whichever call creates the pool first wins for the process. Asserting a request that may or may not be made would make the gate flaky.
- `FetchProfile` runs inside OAuth login completion, which needs a real authorization code.

`discovery_records` is deliberately wider than `GET /v1/models`, so if either of those does reach the wire it is checked rather than ignored. What is not asserted is that they were sent at all.

**Files:**
- Modify: `scripts/verify-claude-code-identity.sh`

**Interfaces:**
- Consumes: `check_identity_policy.py discovery` from Task 1; `snapshot_phase` from Task 2.

- [ ] **Step 1: Declare the phase file**

Add to the block of `OBSERVED_*` declarations from Task 2:

```bash
OBSERVED_DISCOVERY="${WORK_DIR}/observed-discovery.jsonl"
```

- [ ] **Step 2: Add the discovery phase**

Append after Task 2's `[8/9]` block:

```bash
echo "=== [9/9] Driving the three discovery callers ==="
# Three of the five sites T4 corrected, one per client method:
#
#   .../test         ValidateAccount    (sent no User-Agent at all, so Go's
#                                        transport supplied Go-http-client/1.1)
#   .../ratelimits   FetchRateLimits    (sent Claude-Code/2.1.246)
#   models/fetch     FetchModels        (sent Claude-Code/2.1.246)
#
# The other two are in internal/auth and are not driven here: the refresh route
# reaches them only when the pool happens to have been created through the
# server-bound getOrCreateCCPool rather than the package-level nil-server one,
# and FetchProfile needs a real authorization code. The policy selector is wider
# than /v1/models, so either is still checked if it fires.
#
# No request body is needed: each handler resolves the stub token out of the
# configuration. The upstream rejects that token, which is irrelevant — the
# request mitmdump recorded on the way out is the whole subject.
#
# The management API is unauthenticated here because the stub configuration
# sets no webuiPassword.
GATE_ACCOUNT_ID="cc-identity-gate-stub"
for route in \
  "/api/claudecode/accounts/${GATE_ACCOUNT_ID}/test" \
  "/api/claudecode/accounts/${GATE_ACCOUNT_ID}/ratelimits" \
  "/api/claudecode/models/fetch"
do
  curl -sS -o /dev/null --max-time 60 \
    -X POST "http://127.0.0.1:${PROXY_PORT}${route}" \
    -H 'Content-Type: application/json' \
    -d '{}' \
    >/dev/null 2>&1 || true
done

snapshot_phase "${OBSERVED_DISCOVERY}" "the discovery requests"

if ! python3 "${REPO_ROOT}/scripts/check_identity_policy.py" discovery \
  --baseline "${BASELINE}" \
  --observed "${OBSERVED_DISCOVERY}" \
  --min-records 3; then
  KEEP_WORK_DIR=1
  echo "ERROR: the capture records claude-code/<version> on every GET it observed," >&2
  echo "ERROR: lowercase and with no parenthesised mode. Go's transport supplies" >&2
  echo "ERROR: Go-http-client/1.1 when no User-Agent is set at all." >&2
  echo "Observed capture kept at ${OBSERVED_DISCOVERY}" >&2
  fail "a discovery request did not carry the captured User-Agent"
fi
```

- [ ] **Step 3: Extend the pass banner**

Replace the two final `echo` lines with:

```bash
echo "Claude Code identity gate PASSED: the wire identity matches ${BASELINE##*/},"
echo "the caller's system prompt survived alongside the billing marker,"
echo "a keyed custom endpoint kept its credential and was not normalized,"
echo "and every discovery request carried the captured User-Agent."
```

- [ ] **Step 4: Run the gate**

The operator's step again:

Run: `! ./scripts/verify-claude-code-identity.sh`
Expected: `[9/9]` reached and all four pass lines printed. If it fails with `only N discovery request(s) reached the wire, want 3`, one of the three management handlers stopped calling upstream — check `${WORK_DIR}/proxy.log`, which the failure kept.

- [ ] **Step 5: Prove the new assertion bites**

```bash
# Temporarily revert one site: in internal/claudecode/client.go, change
# ValidateAccount's User-Agent line to set nothing, by deleting
#   req.Header.Set("User-Agent", ccidentity.DiscoveryUserAgent)
```

Run: `! ./scripts/verify-claude-code-identity.sh`
Expected: FAIL at `[9/9]` with `GET /v1/models sent User-Agent 'Go-http-client/1.1'`.

Restore with `git checkout internal/claudecode/client.go` and re-run to confirm the pass returns.

- [ ] **Step 6: Commit**

```bash
git add scripts/verify-claude-code-identity.sh
git commit -m "test(scripts): gate the discovery user-agent

The gate drove one POST and never a GET, so the five sites that sent
claude-code/2.1.246 — two of them spelling it Claude-Code — and the one that
sent no User-Agent at all were unit-tested only.

Three management routes reach /v1/models, one per client method, and none goes
through account selection, so the pooled stub's cooldown does not block them.
The two internal/auth sites are not driven: the refresh route reaches them only
when the pool was created through the server-bound constructor, and FetchProfile
needs a real authorization code. The selector is wider than /v1/models, so both
are still checked if they fire.

The expected value is read out of the baseline's own GET records rather than
written here."
```

---

## Self-Review

**Spec coverage.** T1 has both halves on the wire: the keyless endpoint must be normalized (`[7/9]`) and the keyed one must not be, while keeping its credential (`[8/9]`). T4 has the two `internal/claudecode` sites plus `ValidateAccount`, driven through the three management routes that reach `/v1/models` (`[9/9]`). Its three `internal/auth` sites are **not** driven, for the reasons recorded in Task 3's preamble — neither can be triggered without either a race on pool construction or a real OAuth code. `discovery_records` still covers them if they fire, so the gap is "not asserted to happen", not "not checked".

**Placeholder scan.** One deliberate placeholder is flagged as such and fixed in the step that follows it: `GATE_KEY_SHA` in the test fixture, which Task 1 Step 2 replaces with the real digest before the suite is meaningful. No other step defers work.

**Type consistency.** `check_normalized(baseline, observed)` and `check_passthrough(observed, caller_user_agent, api_key_sha256)` take single records; `check_discovery(baseline_gets, observed_gets, min_records)` takes lists. The tests call all three with exactly those shapes, and `main()` supplies them from `messages_records`, `discovery_records` and `get_records`, all of which return lists. `models_records` is kept because the loader test pins it, and because a future phase may want the narrower selector. `snapshot_phase` takes `(destination, what)` and is called with two arguments at all four sites.

**Tool preflight.** `shasum` is added to the skip list in Task 2 Step 2, so a machine without it skips loudly rather than computing an empty digest and failing `[8/9]` with a confusing mismatch.
