# PR #148 Remediation Plan

**Goal:** Fix every finding of the PR #148 review comment on `chore/claude-code-count-tokens-recapture`. Three changes: Claude Code 2.1.311 thread continuations no longer get a false `version_suffix_expected`, the drift checks stop reporting them as drift, and the capture's meta and the spec match the re-run.

**Architecture:** One worktree on the PR branch (HEAD `f6f4b64`) and three sequential commits. Task 3 rewrites the capture with the reason string Task 1 defines, so the order matters. No Go code changes. The Go suites run only as a regression gate, because `internal/claudecode/count_tokens_test.go` reads this capture.

**Tech stack:** Python 3 `unittest` (`scripts/test_cc_billing.py`, `scripts/test_diff_claude_code_identity.py`, `scripts/test_check_identity_policy.py`, `scripts/test_client_drift.py`) and Go 1.27rc2 `go test`.

**Spec:** `docs/superpowers/specs/2026-11-07-conversation-aware-cloud-code-design.md` §6.2 (L138). ADR 0004 §7. Review: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/148#issuecomment-6729827696

**Evidence behind finding 1** (taken from the PR head; JSONL line N is record N-1):

| line | class | `thread.previous_message_id` | `version_suffix` | `version_suffix_expected` |
|---|---|---|---|---|
| 14 | auxiliary | — (no thread) | a4d | a4d |
| 15 | main | absent (thread `{type}`) | c6a | c6a |
| 16 | main | present | c6a | 972 |
| 18 | main | present | c6a | e01 |
| 55 | main | present | c6a | 760 |
| 56 | main | present | c6a | 972 |
| 17, 19, 57, 60 | auxiliary / compaction | — (no thread) | c6a | c6a |

Every mismatching record has `thread.previous_message_id` and every matching one lacks it. The same holds in the replaced capture (its lines 16, 19, 56, 57). In the continuations, `messages` starts with a `tool_result` message or with the new prompt, never with the session's first prompt. The client keeps sending `c6a`, the suffix of that first prompt. `cch` equals `cch_expected` on all 10 billed records.

Checked with `cc_billing.version_suffix(prompt, "2.1.311")`: step 1's prompt "Read calc.py and tell me in one sentence what add does" hashes to `c6a`, the value the client sends throughout. Step 2's prompt "What language is @calc.py written in? Answer in one word." hashes to `e01`, the false expected value on line 18. A body whose only user message is a `tool_result` has no prompt text and derives `972`, the false value on lines 16 and 56. So the addon hashed the delta, and the client kept the first prompt. The Task 1 Red inputs below were dry-run against the current `cc_billing.py`: the continuation case derives `972`, so it fails as planned, and the first-request case derives `c6a`.

**Setup:**

```bash
git worktree add .worktrees/pr148 chore/claude-code-count-tokens-recapture
cd .worktrees/pr148
git log --oneline -1   # f6f4b64 chore(reference): recapture Claude Code count_tokens ...
```

---

### Task 1: a thread continuation leaves the suffix unchecked (🟡)

- **Modify:** `scripts/cc_billing.py`: the module docstring, a new `THREAD_CONTINUATION` constant, a new `_continues_thread`, and `billing_verdict`.
- **Test:** `scripts/test_cc_billing.py`, new class `ThreadContinuationTest`.
- **Consumes:** `billing_verdict(content: bytes) -> dict | None`, `version_suffix(prompt, version)`.
- **Produces:** when a body continues a thread, the verdict has `version_suffix_expected: None` and a new key `version_suffix_unchecked: THREAD_CONTINUATION`. The key is absent otherwise. `cch` and `cch_expected` stay as they are. Task 2 reads the key and Task 3 writes the constant.

1. **Red:** append this to `scripts/test_cc_billing.py`, above `if __name__ == "__main__":`.

   ```python
   FIRST_PROMPT = "Read calc.py and tell me in one sentence what add does"


   def billed_body(content, thread):
       """A 2.1.311-shaped /v1/messages body with one user message."""
       body = {
           "model": "claude-sonnet-5-5",
           "messages": [{"role": "user", "content": content}],
           "system": [{
               "type": "text",
               "text": "x-anthropic-billing-header: cc_version=2.1.311.c6a; "
                       "cc_entrypoint=cli; cch=00000;",
           }],
           "max_tokens": 128000,
           "thread": thread,
       }
       return json.dumps(body, separators=(",", ":")).encode()


   class ThreadContinuationTest(unittest.TestCase):
       """2.1.311 main follow-ups name the previous reply in
       thread.previous_message_id and send only the messages after it
       (.reference/claude-code-count-tokens-20261107.jsonl lines 16, 18, 55, 56),
       so the first prompt the suffix hashes is not in the body."""

       def test_a_thread_continuation_leaves_the_suffix_unchecked(self):
           tool_result = [{"type": "tool_result", "tool_use_id": "toolu_01",
                           "content": "def add(a, b): return a + b"}]
           verdict = cc_billing.billing_verdict(
               billed_body(tool_result, {"type": "t", "previous_message_id": "msg_01"}))
           self.assertEqual(verdict["version_suffix"], "c6a")
           self.assertIsNone(verdict["version_suffix_expected"])
           self.assertEqual(verdict["version_suffix_unchecked"], cc_billing.THREAD_CONTINUATION)
           self.assertIsNotNone(verdict["cch_expected"])

       def test_a_thread_without_a_previous_message_is_checked(self):
           # A conversation's first request (line 15) carries thread with no
           # previous_message_id and the whole first prompt.
           prompt = [{"type": "text", "text": FIRST_PROMPT}]
           verdict = cc_billing.billing_verdict(billed_body(prompt, {"type": "t"}))
           self.assertEqual(verdict["version_suffix_expected"], "c6a")
           self.assertNotIn("version_suffix_unchecked", verdict)
   ```

   Add `import json` to the imports. The capture records only the shape of `thread.type`, not its value, so the tests use a placeholder and the predicate ignores it.

2. Run `cd scripts && python3 -m unittest test_cc_billing -v`. `test_a_thread_continuation_leaves_the_suffix_unchecked` must FAIL: `AssertionError: '…' is not None`, or `AttributeError: … 'THREAD_CONTINUATION'`. `test_a_thread_without_a_previous_message_is_checked` already passes. It guards against a predicate that is too broad.

3. **Green:** in `scripts/cc_billing.py`:
   - Add the constant and the helper above `billing_verdict`:

     ```python
     # Claude Code 2.1.311 main follow-ups name the previous reply in
     # thread.previous_message_id and send only the messages after it, so the
     # conversation's first prompt, which the suffix hashes, is not in the body.
     THREAD_CONTINUATION = "thread continuation: the body omits the first prompt"


     def _continues_thread(body: dict) -> bool:
         thread = body.get("thread")
         return isinstance(thread, dict) and bool(thread.get("previous_message_id"))
     ```

   - In `billing_verdict`, replace the `if version:` block with:

     ```python
     if version:
         verdict["version_suffix"] = version.group(2)
         if _continues_thread(body):
             verdict["version_suffix_unchecked"] = THREAD_CONTINUATION
         else:
             verdict["version_suffix_expected"] = version_suffix(
                 first_prompt_text(body), version.group(1))
     ```

   - Extend the `billing_verdict` docstring: "A thread continuation's suffix is left None with `version_suffix_unchecked` naming why."
   - In the module docstring, after "Both rules reproduce every body captured from the real client (…)", add: "A 2.1.311 thread continuation (`thread.previous_message_id`) omits the first prompt, so its suffix cannot be derived from the body and is left unchecked."

4. Run `cd scripts && python3 -m unittest test_cc_billing test_mitm_header_dump -v` and expect PASS. `test_reproduces_every_captured_body` must still pass: the 2.1.304 fixture has no `thread`.

5. Commit:

   ```bash
   git add scripts/cc_billing.py scripts/test_cc_billing.py
   git commit -m "fix(billing): leave the cc_version suffix unchecked on a thread continuation"
   ```

### Task 2: an unchecked suffix is not a drift (🟡)

- **Modify:** `scripts/diff_claude_code_identity.py`: `compare_billing` (L412-L443).
- **Test:** `scripts/test_diff_claude_code_identity.py`, new class `BillingDriftTest`.
- **Consumes:** the verdict key `<field>_unchecked` from Task 1.
- **Callers that inherit the change:** `scripts/client_drift.py:analyze_claude` (L1060), `scripts/check_identity_policy.py:check_normalized` (L207), and `diff_claude_code_identity.main` (L480).

1. **Red:** add this after `BodyDriftTest`.

   ```python
   CONTINUATION_BILLING = {
       "cch": "75a04", "cch_expected": "75a04",
       "version_suffix": "c6a", "version_suffix_expected": None,
       "version_suffix_unchecked": "thread continuation: the body omits the first prompt",
   }


   def billed(billing):
       return record(BASELINE_HEADERS, dict(BASELINE_BODY, billing=billing))


   class BillingDriftTest(unittest.TestCase):
       def test_an_unchecked_suffix_is_not_drift(self):
           self.assertEqual(differ.compare_billing(billed(CONTINUATION_BILLING)), [])

       def test_an_unchecked_suffix_still_checks_the_cch(self):
           drifts = differ.compare_billing(billed(dict(CONTINUATION_BILLING, cch="abcde")))
           self.assertEqual(len(drifts), 1, drifts)
           self.assertIn("billing cch is abcde", drifts[0])

       def test_a_missing_suffix_without_a_reason_is_drift(self):
           billing = dict(CONTINUATION_BILLING)
           del billing["version_suffix_unchecked"]
           drifts = differ.compare_billing(billed(billing))
           self.assertTrue(any("version_suffix could not be located" in d for d in drifts), drifts)
   ```

2. Run `cd scripts && python3 -m unittest test_diff_claude_code_identity -v`. The first two tests must FAIL, because `billing version_suffix could not be located in the body` is reported. The third already passes. It keeps the skip limited to verdicts that name a reason.

3. **Green:** in `compare_billing`, make the field loop skip an unchecked field:

   ```python
   for field, what in (("cch", "the hash of the body as sent"),
                       ("version_suffix", "the hash of the first prompt")):
       if verdict.get(field + "_unchecked"):
           continue
       sent, expected = verdict.get(field), verdict.get(field + "_expected")
   ```

   Add to the docstring: "A field the verdict marks unchecked (`version_suffix_unchecked`, a thread continuation without its first prompt) is skipped, because the body cannot show what the real client hashed."

4. Run `cd scripts && python3 -m unittest test_diff_claude_code_identity test_check_identity_policy test_client_drift -v` and expect PASS.

5. Commit:

   ```bash
   git add scripts/diff_claude_code_identity.py scripts/test_diff_claude_code_identity.py
   git commit -m "fix(drift): an unchecked billing suffix is not a drift"
   ```

### Task 3: re-derive the capture's continuation verdicts, then fix the meta and the spec (🟡 + 🔵 ×3)

- **Modify:**
  - `.reference/claude-code-count-tokens-20261107.jsonl`: lines 16, 18, 55 and 56 only.
  - `.reference/claude-code-count-tokens-20261107.meta.txt`: L4 (the name note), L39-L40 (burst timing), after L52 (a new finding) and L65-L67 (provenance).
  - `docs/superpowers/specs/2026-11-07-conversation-aware-cloud-code-design.md`: L138.
- **Consumes:** `cc_billing.THREAD_CONTINUATION` (Task 1).
- **Test:** a scratch check, not committed. This is reference data and no permanent test applies. `internal/claudecode/count_tokens_test.go` reads only line 21, which stays untouched.

The bodies are not stored, so `billing_verdict` cannot be rerun on them. The record still decides the rule: `request_body.shape.thread` holds the key `previous_message_id` exactly on these four lines. The addon writes `json.dumps(record, ensure_ascii=False)` (`scripts/mitm_header_dump.py:L392`), and all 62 lines round-trip byte for byte under that call, so the other 58 lines stay identical. A post-hoc rewrite of this capture has a precedent: PR #144 Task D1 redacted it the same way. The alternative is an interactive tmux recapture with the fixed addon (never `-p`). That is a live session and gives no extra evidence for this field.

1. **Red (scratch check):**

   ```bash
   python3 - <<'EOF'
   import json
   bad = []
   for n, line in enumerate(open('.reference/claude-code-count-tokens-20261107.jsonl', encoding='utf-8'), 1):
       b = (json.loads(line).get('request_body') or {}).get('billing')
       if b and not b.get('version_suffix_unchecked') and b['version_suffix'] != b['version_suffix_expected']:
           bad.append(n)
   print(bad)
   raise SystemExit(1 if bad else 0)
   EOF
   ```

   Expect `[16, 18, 55, 56]` and exit 1.

2. **Green (one-off transform, not committed):**

   ```bash
   python3 - <<'EOF'
   import json, sys
   sys.path.insert(0, 'scripts')
   from cc_billing import THREAD_CONTINUATION
   path = '.reference/claude-code-count-tokens-20261107.jsonl'
   out = []
   for line in open(path, encoding='utf-8').read().splitlines():
       record = json.loads(line)
       body = record.get('request_body') or {}
       thread = (body.get('shape') or {}).get('thread')
       if 'billing' in body and isinstance(thread, dict) and 'previous_message_id' in thread:
           body['billing']['version_suffix_expected'] = None
           body['billing']['version_suffix_unchecked'] = THREAD_CONTINUATION
           line = json.dumps(record, ensure_ascii=False)
       out.append(line)
   open(path, 'w', encoding='utf-8').write('\n'.join(out) + '\n')
   EOF
   git diff --numstat -- .reference/claude-code-count-tokens-20261107.jsonl   # expect: 4  4
   ```

   The new key goes after `version_suffix_expected`, which is where the fixed addon puts it.

3. Rerun the scratch check from step 1 and expect `[]` and exit 0.

4. **Meta edits** (`.reference/claude-code-count-tokens-20261107.meta.txt`):
   - After L4, add:

     ```
               The file name keeps 20261107, the local date of the first run,
               because ADR 0004 §7, internal/claudecode/count_tokens_test.go and
               internal/ccidentity cite it.
     ```

   - Replace L39-L40. The span is 0.465 s, from line 21 at 00:35:12.123 to line 54 at 00:35:12.588.

     ```
     - count_tokens is sent only by /context: 34 POST /v1/messages/count_tokens
       within 0.47 s, all 200. Steps 1, 2, 4 and 5 sent none.
     ```

   - After L52, add:

     ```
     - Main follow-ups (lines 16, 18, 55, 56) carry thread.previous_message_id
       and send only the messages after that reply, so the body no longer holds
       the first prompt the cc_version suffix hashes. The client still sends the
       session's first-prompt suffix (c6a). Their billing verdicts mark the
       suffix unchecked (version_suffix_unchecked) instead of deriving one. cch
       matches on all 10 billed requests.
     ```

   - Replace L65-L67:

     ```
     Provenance note: every header value, path, status and TLS fingerprint in this
     capture is a verbatim quote from observed traffic. request_body is a shape
     fingerprint, not the body. Its billing.*_expected values are derived by
     scripts/cc_billing.py from the body as sent, and the four
     version_suffix_unchecked verdicts were re-derived from the recorded thread
     shape after the addon learned the thread-continuation rule. Credential and
     account values are redacted before writing. Anything not confirmed
     byte-for-byte is marked unknown rather than inferred.
     ```

5. **Spec edit** (L138): replace "(Claude Code 2.1.311, interactive, 2026-11-07, personal account)" with "(Claude Code 2.1.311, interactive, re-captured 2026-11-09T00:37Z with the endpoint pinned to api.anthropic.com, personal account)". Replace "34 requests within 0.7 s" with "34 requests within 0.5 s". Leave the rest of the line as it is.

6. Run `go test -count=1 ./internal/claudecode/ ./internal/ccidentity/` and `cd scripts && python3 -m unittest test_cc_billing test_diff_claude_code_identity test_check_identity_policy test_client_drift`. Expect PASS. 313 Go tests passed before this change.

7. Commit:

   ```bash
   git add .reference/claude-code-count-tokens-20261107.jsonl \
           .reference/claude-code-count-tokens-20261107.meta.txt \
           docs/superpowers/specs/2026-11-07-conversation-aware-cloud-code-design.md
   git commit -m "chore(reference): leave the suffix unchecked on the capture's thread continuations"
   ```

---

## Verification and push

1. **Gate:** `go test -race ./...`, `go vet ./...`, `gofmt -l .` (empty), and `cd scripts && python3 -m unittest discover -p 'test_*.py'`.
2. `scripts/secret-scan.sh push`. The trufflehog extension also runs it before the push.
3. `git push fork chore/claude-code-count-tokens-recapture`. Per AGENTS.md, push to `fork`, never to `origin`.
4. Reply on the PR with one line per finding, each naming the commit that fixes it.

## Out of scope: addressed separately

The `ccidentity.TurnOf` concern about thread continuations is fixed on its own branch, `fix/ccidentity-message-threads` (commit `a38b81e`, ADR-0004 §8).

- **The fix:** `ApplyBody` refuses any body that carries `thread`. Both normalized paths then answer `400` with a message naming the client's message-threads beta and `anthropic-beta`.
- **Reachability:** the Claude Code 2.1.311 bundle gates threading on `ANTHROPIC_BASE_URL` being unset or `api.anthropic.com`. Only `CLAUDE_CODE_TETHER_LIVE` overrides that, so a client pointed at the proxy never sends `thread`.
- **Recovery, observed live:** Claude Code with that override, against a stub upstream that gives the same 400, retried without `thread` and stayed stateless for the rest of the session.

None of this changes a PR #148 task.
