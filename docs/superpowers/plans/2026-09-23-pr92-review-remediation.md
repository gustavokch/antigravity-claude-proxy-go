# PR #92 review remediation — hold the policy checker to its own standard

**Branch:** `feat/identity-gate-scope-t1-t4`
**Base:** `feat/claude-code-identity-spoof`
**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/92
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/92#issuecomment-5803317102

## Goal

PR #92 argues that a gate which cannot fail proves nothing, and it falsifies both
new assertions to show they can. The checker it adds does not yet hold itself to
that standard in four places: it can assert against the wrong record, it can die
with a traceback under a message blaming the wrong thing, it can silently degrade
to a weaker policy, and its docstring claims no capture-derived value is hardcoded
while three are.

Each task below closes one of those, test-first.

## Architecture

- `scripts/check_identity_policy.py` — the policy checker. Three subcommands:
  `normalized`, `passthrough`, `discovery`.
- `scripts/test_check_identity_policy.py` — unit tests, no mitmproxy needed.
- `scripts/verify-claude-code-identity.sh` — the nine-phase gate that calls it.
- `.reference/claude-code-headers-20260923.jsonl` — the committed capture; the
  single source of truth for every expected value.

## Tech stack

Python 3 standard library only (`argparse`, `json`, `unittest`). Bash for the gate.
Tests run under `python3 -m pytest`.

## Spec reference

The rules being enforced are T1 (an endpoint with a configured `apiKey` must reach
the upstream unnormalized, because `x-api-key` is on the omit list) and T4 (every
non-messages request carries the captured discovery User-Agent).

---

## Task 1 — a phase must assert against exactly one record

**Target files**
- Modify: `scripts/check_identity_policy.py`
- Test: `scripts/test_check_identity_policy.py`

**Consumes:** a capture file path. **Produces:** exit 1 with a named reason when
the file holds other than exactly one `POST /v1/messages` record.

### Why

`main()` reads `messages_records(records)[0]`. `snapshot_phase` in the gate returns
as soon as `OBSERVED` is non-empty, not once the phase's own flow is the only one
there, and `build_record` (`scripts/mitm_header_dump.py:201`) records no model. A
retried or late-flushed phase-5 flow appended after the snapshot is therefore
indistinguishable from phase 7's, sorts first, and becomes the record `[7/9]`
asserts on — a normalized request re-checked as itself. Exactly one request is
driven per phase, so that is the assertion.

### Step 1 — failing test

```python
class RecordSelectionTest(unittest.TestCase):
    def test_rejects_more_than_one_messages_record(self):
        problems = policy.check_record_count([observed_normalized(), observed_normalized()])
        self.assertTrue(any("2" in p for p in problems), problems)

    def test_rejects_no_messages_record(self):
        problems = policy.check_record_count([])
        self.assertTrue(any("no POST /v1/messages" in p for p in problems), problems)

    def test_accepts_exactly_one(self):
        self.assertEqual(policy.check_record_count([observed_normalized()]), [])
```

### Step 2 — confirm failure

```
python3 -m pytest scripts/test_check_identity_policy.py -q
```

### Step 3 — implementation

Add `check_record_count(observed_posts) -> list[str]`; call it from `main()` before
selecting `[0]`, folding its problems into the same reporting path.

### Step 4 — confirm pass

```
python3 -m pytest scripts/test_check_identity_policy.py -q
```

### Step 5 — commit

```
git add scripts/check_identity_policy.py scripts/test_check_identity_policy.py
git commit -m "test(scripts): assert one record per identity phase"
```

---

## Task 2 — a malformed capture line must not read as wire drift

**Target files**
- Modify: `scripts/check_identity_policy.py`
- Test: `scripts/test_check_identity_policy.py`

**Consumes:** a capture file. **Produces:** `ValueError` naming file and line.

### Why

The gate truncates `${OBSERVED}` while mitmdump may be mid-append, and a record
above `PIPE_BUF` is not an atomic write. `json.loads` then raises out of `main()`
and the operator sees a traceback under `fail "the proxy's wire identity has
drifted"` — a message about the wrong thing entirely.

### Step 1 — failing test

```python
class LoaderErrorTest(unittest.TestCase):
    def test_names_the_file_and_line_for_a_malformed_record(self):
        handle = tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False)
        handle.write(json.dumps(observed_normalized()) + "\n")
        handle.write('{"method": "POST", truncated\n')
        handle.close()
        try:
            with self.assertRaises(ValueError) as caught:
                policy.load_records(handle.name)
            self.assertIn("line 2", str(caught.exception))
            self.assertIn(os.path.basename(handle.name), str(caught.exception))
        finally:
            os.unlink(handle.name)
```

### Step 2 — confirm failure

```
python3 -m pytest scripts/test_check_identity_policy.py -q
```

### Step 3 — implementation

`enumerate` the lines; wrap `json.loads` and re-raise as `ValueError` naming path
and line number. Catch `ValueError` in `main()` and return 1 with the message on
stderr, no traceback.

### Step 4 — confirm pass

### Step 5 — commit

```
git commit -m "fix(scripts): name the bad line instead of raising a traceback"
```

---

## Task 3 — a baseline that cannot support the policy is a failure

**Target files**
- Modify: `scripts/check_identity_policy.py`
- Test: `scripts/test_check_identity_policy.py`

### Why

`check_normalized` skips the `x-claude-code-request-class` assertion when the
baseline carries no such header. A regenerated capture that dropped it would leave
`[7/9]` green while proving strictly less than it does today. `check_discovery`
already treats a baseline with no GET as a problem; this is the same rule.

### Step 1 — failing test

```python
    def test_rejects_a_baseline_without_the_normalization_marker(self):
        baseline = baseline_post()
        baseline["headers"] = [["User-Agent", CAPTURED_UA]]
        problems = policy.check_normalized(baseline, observed_normalized())
        self.assertTrue(any("baseline" in p for p in problems), problems)
```

### Step 2 — confirm failure

### Step 3 — implementation

Replace the `if expected_class is not None:` guard with an explicit baseline
problem when the header is absent.

### Step 4 — confirm pass

### Step 5 — commit

```
git commit -m "test(scripts): fail when the baseline cannot carry the policy"
```

---

## Task 4 — no capture-derived value written in the checker

**Target files**
- Modify: `scripts/check_identity_policy.py`, `scripts/verify-claude-code-identity.sh`
- Test: `scripts/test_check_identity_policy.py`

### Why

The module docstring says every expected value is read out of the capture. Three
are not: `beta=true` at L119 and L158, and `x-stainless-os` at L154. Either the
claim or the code has to move.

### Step 1 — failing test

```python
class BaselineDerivationTest(unittest.TestCase):
    def test_captured_query_comes_from_the_baseline(self):
        baseline = baseline_post()
        baseline["path"] = "/v1/messages?beta=other"
        self.assertEqual(policy.captured_query(baseline), "beta=other")

    def test_normalization_markers_come_from_the_baseline(self):
        self.assertEqual(
            policy.normalization_markers(baseline_post()),
            ["x-claude-code-request-class", "x-stainless-os"],
        )

    def test_passthrough_rejects_any_baseline_marker(self):
        observed = observed_passthrough(
            headers=[
                ["user-agent", CALLER_UA],
                ["x-stainless-os", "Linux"],
                ["x-api-key", {"redacted": True, "sha256": GATE_KEY_SHA, "len": 8}],
            ]
        )
        problems = policy.check_passthrough(
            observed, CALLER_UA, GATE_KEY_SHA, baseline_post()
        )
        self.assertTrue(any("x-stainless-os" in p for p in problems), problems)
```

### Step 2 — confirm failure

### Step 3 — implementation

- `captured_query(baseline)` — the query string off the baseline POST path.
- `normalization_markers(baseline)` — baseline header names under the
  `x-claude-code-` and `x-stainless-` prefixes, sorted. Prefixes are structural,
  not fingerprint values, and the set is strictly wider than the two names it
  replaces.
- `check_passthrough` takes the baseline and uses both.
- `main()` grows `--baseline` on the `passthrough` subparser.
- The gate passes `--baseline "${BASELINE}"` at the `[8/9]` call site.
- Delete `models_records`, dead outside its own test.

### Step 4 — confirm pass

### Step 5 — commit

```
git commit -m "refactor(scripts): read every expected value from the capture"
```

---

## Task 5 — test hygiene

**Target files**
- Modify: `scripts/test_check_identity_policy.py`

Drop the unused `GATE_KEY` constant; tighten `test_rejects_too_few_records` from
`any("3" in p ...)` — which almost any message satisfies — to `want 3`.

```
git commit -m "test(scripts): tighten a record-count assertion"
```

---

## Verification

```
python3 -m pytest scripts/test_*.py -q
go test -count=1 ./...
bash -n scripts/verify-claude-code-identity.sh
```

The nine-phase gate itself needs mitmproxy and a live upstream; it is not run
here. Every checker change is covered by a unit test that fails before it.
