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

- `go build ./...`
- `go vet ./internal/...`
- `go test ./internal/...`
- `python3 -m pytest scripts/test_diff_claude_code_identity.py scripts/test_mitm_header_dump.py -q`
- `gofmt -l internal/ccidentity internal/claudecode internal/api` must be empty

## Not done here

- The 30 unrelated plan docs in commit `27c59b4` stay. Removing them is a history
  edit on a pushed branch and is the operator's call.
- `omitempty` on the two `Identity` struct fields is a no-op but harmless.
- The `cli` entrypoint and API-key auth mode are still uncaptured. T5 makes the
  API-key case refuse rather than guess.
