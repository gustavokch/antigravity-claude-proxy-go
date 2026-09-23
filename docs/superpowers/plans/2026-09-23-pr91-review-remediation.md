# PR 91 review remediation — Claude Code wire identity

Goal: close the findings from the review of
https://github.com/gustavokch/antigravity-claude-proxy-go/pull/91, in severity
order. Three are blocking, five are risks, one is a cheap allocation nit.

Architecture: unchanged. `internal/ccidentity` owns the captured identity,
`internal/claudecode` applies it on the pooled gateway path, `internal/api`
applies it on the custom-endpoint path, and `scripts/diff_claude_code_identity.py`
gates both against `.reference/claude-code-headers-20260923*.jsonl`.

Tech stack: Go 1.x (`go test ./internal/...`), Python 3 (`python3 -m pytest
scripts/test_*.py -q`).

Spec reference: `.reference/claude-code-headers-20260923.txt` and the two JSONL
captures. Nothing below infers a value the captures do not show; where a value
is unknowable from the capture, the task says so and picks the conservative
option.

## Evidence for T1

Extracted from the committed captures. `X-Claude-Code-Session-Id` and
`cc_prompt_id` are never equal, and both are stable inside one process:

| capture | row | `X-Claude-Code-Session-Id` | `cc_prompt_id` |
|---|---|---|---|
| 20260923 | 2 | `5c1c7cec-f043-4f01-a9f9-fe25fb98b338` | `7cd5023f-9b41-4558-8901-c8f46878fbf1` |
| 20260923 | 5 | `ca6f5fe3-f6b5-4ba1-b80c-c87b30a9242e` | `9f414346-a03a-4073-9d18-24dbb1a54673` |
| 20260923 | 6 | `ca6f5fe3-f6b5-4ba1-b80c-c87b30a9242e` | `9f414346-a03a-4073-9d18-24dbb1a54673` |
| 20260923-acct2 | 2 | `ccbbdab8-5568-4c03-b45b-b41d4ff2339e` | `b0f9a831-9dab-4c75-b324-d34d7c34acce` |
| 20260923-acct2 | 5 | `2ca893cf-a232-4b7a-9af1-ee9abcf049a5` | `fc5d5bcf-f0d7-4c81-b35c-bdbbf04df0d2` |
| 20260923-acct2 | 6 | `2ca893cf-a232-4b7a-9af1-ee9abcf049a5` | `fc5d5bcf-f0d7-4c81-b35c-bdbbf04df0d2` |

`metadata.session_id` is redacted to `<uuid>` in both captures, so its relation
to the header is unknowable. T1 leaves it equal to the header value — the
conservative choice, since only the prompt-id collision is disproved.

### Third dataset, captured after the fix

Two verification runs of `capture-claude-code-headers.sh oauth` on 2026-09-23
produced six more `POST /v1/messages` records on the first account. They agree:

| run | `X-Claude-Code-Session-Id` | `cc_prompt_id` |
|---|---|---|
| 19:16Z | `50a3c033-8257-4734-a246-a74862f651e2` | `32621f51-c8a5-4d69-b7fb-611e56d53e49` |
| 19:16Z | `eaa80bf0-6803-47ce-be91-5affe66453fc` | `7384faf7-6599-4504-8b35-3fc17ec80e9e` (×2 turns) |
| 19:21Z | `a2fb70b7-a0c9-42e2-a02c-913862cf2904` | `0c8259d8-67bd-449f-bfd9-3bff7a17cbbc` |
| 19:21Z | `3f25755c-7c6f-4095-bb82-eea944ee85f6` | `fc2f6e3d-032e-4863-b81e-cf37b9fee4e2` (×2 turns) |

Twelve of twelve now, over four runs and two accounts, zero identical pairs. The
paired turns repeat both values, confirming again that both ids are stable across
the turns of one process — which is what makes two salts, rather than one random
value, the right fix. `account_uuid` was empty in all of them too, so the earlier
finding holds at 12 of 12 as well.

The 19:21Z run is committed as `claude-code-headers-20260923-run2.{jsonl,meta.txt}`:
it is a properly tagged capture with its own meta, it is the evidence base for the
correlation check added in T2, and it was screened for credentials before
committing (Authorization reduced to a SHA-256 and length, identifiers redacted to
`<hex>`/`<uuid>`, no `sk-ant-`/`oat0` literals anywhere in the file). The 19:16Z
records are NOT committed: they arrived by appending to the committed baseline,
which is the hazard that run exposed (T10), so the baseline was restored to its
committed 7.

Run against the frozen baseline, the fresh capture exercises the differ and the
new correlation check on data neither was built from:

```
no drift: 23 headers over HTTP/1.1, path /v1/messages?beta=true,
metadata.user_id and system[0] both match the captured format
```

## T1 — `cc_prompt_id` must not equal the session id

Modify: `internal/ccidentity/apply.go`
Test: `internal/ccidentity/apply_test.go`

Consumes: `Identity.SessionKey`. Produces: a `promptUUID` distinct from
`sessionUUID` for the same identity, stable for that identity.

1. Add `TestBillingHeaderPromptIDDiffersFromSessionID`: build the header for one
   identity, extract `cc_prompt_id`, assert it differs from `sessionUUID(id)`
   and from the `X-Claude-Code-Session-Id` that `ApplyHeaders` writes, and
   assert two calls for one identity agree.
2. `go test ./internal/ccidentity/ -run PromptID` — expect failure.
3. Add `promptUUID(id Identity) string`, salt `ccidentity-prompt:`, same version-5
   shaping as `sessionUUID`. Use it in `BillingHeader`.
4. Re-run — expect pass.
5. `git commit -m "fix(ccidentity): stop cc_prompt_id colliding with the session id"`

## T2 — the drift gate must relate the two ids

Modify: `scripts/diff_claude_code_identity.py`
Test: `scripts/test_diff_claude_code_identity.py`

1. Add a test that a record whose `cc_prompt_id` equals its
   `x-claude-code-session-id` is reported as drift.
2. `python3 -m pytest scripts/test_diff_claude_code_identity.py -q` — expect failure.
3. Add the cross-field check, naming the 6/6 capture evidence in the message.
4. Re-run — expect pass.
5. `git commit -m "test(scripts): gate the session id against cc_prompt_id"`

## T3 — system block 0 must not destroy the caller's prompt

Modify: `internal/ccidentity/apply.go`
Test: `internal/ccidentity/apply_test.go`

The capture shows block 0 is the billing header on an 80KB request, so the real
client's own system prompt lives at block 1 and later. Prepending is therefore
both more faithful and non-destructive. `TestApplyBodySystemBlockZeroIsTheBillingHeader`
currently pins the destructive behavior and is rewritten here.

1. Rewrite that test: a two-block system array must come back with the billing
   header at 0 and BOTH original blocks preserved after it; a request whose
   block 0 is already a billing header must be replaced, not duplicated.
2. `go test ./internal/ccidentity/ -run System` — expect failure.
3. Prepend unless block 0's text already starts with `x-anthropic-billing-header:`.
4. Re-run — expect pass.
5. `git commit -m "fix(ccidentity): keep the caller's system prompt when marking block zero"`

## T4 — the custom-endpoint path must fail closed

Modify: `internal/api/server.go`
Test: `internal/api/ccidentity_proxy_test.go`

`ApplyBody` is fallible and `Rewrite` cannot report an error, so the call moves
out of `Rewrite` to before the proxy is built, where a 502 can still be written.

1. Add a test: an Anthropic-shaped endpoint with normalization on and a body that
   is valid JSON but not an object must get 502 and the upstream must see no
   request.
2. `go test ./internal/api/ -run CustomEndpoint` — expect failure.
3. Normalize once up front; on error write a 502 and return. `Rewrite` then only
   installs the already-normalized bytes.
4. Re-run — expect pass.
5. `git commit -m "fix(api): fail closed when custom endpoint normalization fails"`

## T5 — normalization and API-key auth are incompatible

Modify: `internal/claudecode/client.go`
Test: `internal/claudecode/client_test.go`

1. Add a test: `Normalize: true` with a non-OAuth token must not put any
   `x-api-key` on the wire.
2. `go test ./internal/claudecode/ -run Normalize` — expect failure.
3. Skip normalization for a non-OAuth token, since the captured identity is an
   OAuth identity and the two cannot both be honest.
4. Re-run — expect pass.
5. `git commit -m "fix(claudecode): do not claim the oauth identity with an api key"`

## T6 — the custom-endpoint path must send the captured query

Modify: `internal/api/server.go`
Test: `internal/api/ccidentity_proxy_test.go`

1. Add a test asserting the upstream sees `beta=true` when a messages request is
   normalized.
2. Run — expect failure.
3. Add the query when normalizing, without clobbering an existing one.
4. Re-run — expect pass.
5. `git commit -m "fix(api): send the captured beta query when normalizing"`

## T7 — remove the two dead Profile fields

Modify: `internal/ccidentity/profile.go`, `internal/ccidentity/defaults.go`
Test: existing suite.

`Static` and `MetadataFields` are written by `DefaultProfile` and read nowhere.
Their doc comments claim they are live. Delete both; the package-level
`MetadataFields` var stays, since it documents the captured key order.

1. Delete the fields and their assignments.
2. `go build ./... && go test ./internal/ccidentity/` — expect pass.
3. `git commit -m "refactor(ccidentity): drop the two Profile fields nothing reads"`

## T8 — reject control characters in the identity overrides

Modify: `internal/api/claudecode_management.go`, `internal/config/config.go`
Test: `internal/api/ccidentity_proxy_test.go`

Six override strings reach header values. A CR or LF makes Go's transport reject
every request, with an error naming neither the field nor the panel.

1. Add a test: POST a config whose `identity.userAgent` contains `\r\n` and
   expect 400 with the field named, and expect the stored config unchanged.
2. Run — expect failure.
3. Validate the six strings in the config POST handler.
4. Re-run — expect pass.
5. `git commit -m "fix(api): reject control characters in the identity overrides"`

## T9 — build the default profile once

Modify: `internal/ccidentity/defaults.go`
Test: existing suite.

1. Back `DefaultProfile` with a package-level value built in `init`, keeping the
   function so callers do not change. Omit/Betas are shared read-only slices.
2. `go test ./internal/...` — expect pass.
3. `git commit -m "perf(ccidentity): stop rebuilding the default profile per call"`

## Verification gate

Run after the last task. Results:

- `go build ./...` — clean
- `go vet ./internal/...` — clean
- `go test ./internal/...` — all packages pass
- `python3 -m pytest scripts/test_diff_claude_code_identity.py scripts/test_mitm_header_dump.py -q` — 54 passed (was 51; T2 adds 3)
- `gofmt -l internal/ccidentity internal/claudecode internal/api` — empty

NOT run: `scripts/verify-claude-code-identity.sh`. It starts mitmdump and a proxy
listener, which this session's permission classifier refuses as an external write.
The gate's comparison logic is the Python half and is covered by the 54 tests
above; the live leg is still unrun for this branch and needs an operator to
execute it by hand.

### The gate ran, and what it settled

The operator ran it. It passed:

```
no drift: 21 headers over HTTP/2.0, path /v1/messages?beta=true,
metadata.user_id and system[0] both match the captured format
Claude Code identity gate PASSED
```

Proven on the wire by that run:

- **T1.** `compare_correlations` ran with real values on both sides, not
  placeholders: `SENSITIVE_NAME` in `mitm_header_dump.py` matches
  authorization/cookie/api-key only, so `X-Claude-Code-Session-Id` is recorded
  verbatim, and `cc_prompt_id` comes from the unredacted `system_first_block`. The
  proxy's own emitted pair was compared and differed.
- Header normalization end to end: a foreign `User-Agent`, a foreign `x-app` and a
  beta absent from the baseline were all overwritten, 21 headers matching.
- Body normalization: `temperature` stripped, `metadata.user_id` shape matching,
  `?beta=true` on the path.

NOT exercised by that run, and unit-tested only:

- **T3.** The driven request carries no `system` field, so
  `systemWithBillingHeader` took its `default:` branch — the one that was never
  destructive. The `[]any` branch that used to delete the caller's prompt did not
  run. **Closed in T11 below.**
- **T4, T6.** The gate narrows `gatewayOrder` to `["claudecode"]`, so
  `forwardToCustomEndpoint` never runs. Both fixes live there.
- **T5.** The stub credential is OAuth-shaped, so the API-key skip never triggers.

## T11 — give T3 a wire-level guard

Modify: `scripts/verify-claude-code-identity.sh`

The driven request now sends one text system block, and the gate asserts two
arrive. One block in, two out: the marker prepended, the caller's prompt intact.
`<x1>` means block 0 was overwritten and the prompt was destroyed.

The assertion sits in the gate, not the differ, because it is about what THIS
request sent. The differ only knows baseline versus observed, and real Claude
Code's own count is not a bound on ours — the committed captures show `<x4>`, so a
baseline-relative rule would be meaningless here.

It reads `request_body.shape.system[1]`. The fingerprint encodes a list as
`[shape-of-first, "<xN>"]`, so the count is already recorded and no change to
`mitm_header_dump.py` was needed. `compare_body` never compares `shape`, so this
disturbs nothing.

Falsified in both directions without a live run, by feeding the real `ApplyBody`
output for exactly the driven body through `body_fingerprint`:

| Implementation | `shape.system` | Gate | Differ |
|---|---|---|---|
| this branch (prepends) | `[…, "<x2>"]` | passes | no drift |
| pre-fix (overwrites) | `[…, "<x1>"]` | **fails** | no drift |

The second row is the point: the differ reports no drift either way, because
`system_first_block` sees block 0 alone. The new assertion is the only thing that
catches it.

Also confirmed: the added `system` field does not create drift. Normalized
`top_level_keys` are `max_tokens, messages, metadata, model, system`, all present
in the baseline's set, and `temperature` is still stripped.

Live status: every component is proven, but the assertion has not run inside a real
gate invocation. One `./scripts/verify-claude-code-identity.sh` settles it.

RUN by the operator afterwards: `capture-claude-code-headers.sh oauth`, twice.
The second run printed `exit=0` on a successful 7-record capture, which is the
live proof the T-exit fix needed — the same command exited 1 before it. The first
run surfaced a new defect, fixed in the same pass:

## T10 — refuse to append to an existing capture

Modify: `scripts/capture-claude-code-headers.sh`

`CAPTURE_TAG` defaults to `date +%Y%m%d` and the addon opens its output with
`"a"`, so the documented `oauth` command, run on the same calendar day as an
existing capture, appends to it and regenerates its `.meta.txt` from the combined
file. The committed 7-record baseline became 14 records with no warning. Its
first 7 lines stayed byte-identical, so nothing was lost, but two things follow
that the script's output does not show:

- The frozen reference stops being frozen, visible only as a dirty working tree.
  One `git add -A` commits a mutated baseline.
- The drift gate gets LOOSER. `compare_headers` unions the expected header set
  over every baseline record, so a header appearing only in the appended run
  becomes expected and stops being reported as unexpected.

Now refuses when the target holds records, naming `CLAUDE_CAPTURE_TAG` for a
separate capture and `CLAUDE_CAPTURE_APPEND=1` as the deliberate override. All
three states exercised: existing file rejects, override proceeds, absent file
proceeds.

Pre-existing gofmt breakage outside this feature, untouched here:
`internal/accounts/{manager,manager_test,forensics_test}.go`,
`internal/classifier/audit_test.go`, `internal/cloudcode/{client,quota}.go`,
`internal/logger/stream_test.go`.

## Outcome per task

| Task | State | Note |
|---|---|---|
| T1 `cc_prompt_id` collision | done | `485c318` |
| T2 gate cross-field check | done | `d2a47ee` |
| T3 system block prepend | done | `75e254c` |
| T4 fail closed | done | `857632c`, carries T6 and T9 |
| T5 API key vs normalization | done | `a67f142` |
| T6 captured beta query | done | in `857632c` |
| T7 dead Profile fields | done | `e5a7904` |
| T8 reject control characters | done | `76beecd`, both config surfaces |
| T9 profile built once | done | in `857632c` |

T4's test calls `forwardToCustomEndpoint` directly. The router reads `model` out
of the body before reaching it, so a non-object body cannot arrive through the
live path today — the fail-open branch was unreachable in practice. It is fixed
anyway, because the function should not depend on a guarantee its caller happens
to provide, and the two normalize paths disagreeing on error discipline is the
kind of difference that becomes a bug when a caller changes. Recorded here rather
than claimed as a live bug fix.

## Not done here

- The 30 unrelated plan docs in commit `27c59b4` stay. Removing them is a history
  edit on a pushed branch and is the operator's call.
- `omitempty` on the two `Identity` struct fields is a no-op but harmless.
- The `cli` entrypoint and API-key auth mode are still uncaptured. T5 makes the
  API-key case refuse rather than guess.
