# PR 91 second-pass remediation — Claude Code wire identity

Goal: close the findings from the two-axis review of
https://github.com/gustavokch/antigravity-claude-proxy-go/pull/91
(comment `#issuecomment-5802097787`), in severity order. One is blocking and
ships a live credential bug, two are documented-standard breaches, four are spec
tasks the first pass recorded as done but did not finish, five are design
judgement calls, and the last is bookkeeping that the first-pass plan and the PR
description now state falsely.

Numbers below are this plan's. Where a task revisits one from
`2026-09-23-pr91-review-remediation.md`, it names it as "first-pass Tn".

Architecture: unchanged. `internal/ccidentity` owns the captured identity,
`internal/claudecode` applies it on the pooled gateway path, `internal/api`
applies it on the custom-endpoint path, and `scripts/diff_claude_code_identity.py`
gates both against `.reference/claude-code-headers-20260923*.jsonl`.

Tech stack: Go 1.x (`go test ./internal/...`), Python 3 (`python3 -m pytest
scripts/test_*.py -q`).

Spec reference: `.reference/claude-code-headers-20260923.txt` and the three JSONL
captures. No task below invents a wire value the captures do not show.

## T1 — a custom endpoint authenticated by API key must keep its credential

Modify: `internal/api/server.go`
Test: `internal/api/ccidentity_proxy_test.go`

This is the blocking finding. It is a live regression on upgrade, with no config
change required to trigger it.

`customEndpointIdentity` (`server.go:1038`) gates normalization on
`isAnthropicEndpoint(endpoint.URL)` alone. It does not look at `endpoint.APIKey`.
`x-api-key` is in `omittedHeaders` (`internal/ccidentity/defaults.go:138`), and
`ApplyHeaders` deletes every omitted name (`apply.go:64-68`). On both custom
endpoint paths the auth block sets the key first and `ApplyHeaders` runs after
it:

| path | sets `x-api-key` | runs `ApplyHeaders` |
|---|---|---|
| non-streaming | `server.go:1083` | `server.go:1086` |
| reverse proxy `Rewrite` | `server.go:1178` | `server.go:1188` |

A zero-value `IdentityConfig` has `Disabled: false`, so `IdentityConfig.Identity`
(`internal/claudecode/types.go:149`) returns `normalize = true`. Normalization is
therefore on by default for every existing Anthropic-shaped custom endpoint. Such
an endpoint loses its `x-api-key` silently. Only relays that also accept the
`Authorization: Bearer` header set alongside it at `:1082` / `:1177` still
authenticate, which is why the live runs returned 200 and the bug was invisible.

The fix mirrors first-pass T5, which already established the rule for the pooled
gateway at `internal/claudecode/client.go:349`: normalization claims an OAuth
Claude Code identity, and an API-key credential contradicts that claim, so the
request is sent unnormalized rather than sent broken.

1. Add `TestCustomEndpointAPIKeySurvivesNormalization`: an Anthropic-shaped
   endpoint with `APIKey` set, driven through both `forwardToCustomEndpoint`
   paths, must reach the upstream recorder carrying `x-api-key`. Assert the
   header is present and equal to the configured key on each path.
2. `go test ./internal/api/ -run CustomEndpointAPIKey` — expect failure on both.
3. In `customEndpointIdentity`, return `false` when
   `strings.TrimSpace(endpoint.APIKey) != ""`, with a comment naming first-pass
   T5 as the precedent and the omit-list deletion as the mechanism.
4. Re-run — expect pass.
5. Add `TestCustomEndpointWithoutAPIKeyStillNormalizes` so the fix cannot be
   widened into "normalization is off for custom endpoints".
6. `git commit -m "fix(api): do not normalize a custom endpoint authenticated by api key"`

Rejected alternative: reordering the auth block after `ApplyHeaders`. It would
keep the key on the wire, but the key would then travel inside a request that
claims to be OAuth Claude Code — the exact contradiction first-pass T5 refused.

## T2 — record the transparent-forwarding exception as an ADR

Modify: `docs/adr/0004-*.md` (new), `docs/adr/0001-transparent-forwarding-reverse-proxy.md`

ADR-0001 says transparent forwarding "transparently streams the client request
and server response without altering the body payload". ADR-0003 §2 restates it
as the Main Route Invariant: "General user requests directed through
`customEndpoints` continue to use pure, transparent `httputil.ReverseProxy`
without payload alteration."

`server.go:1069` and `:1129` now call `ccidentity.ApplyBody` on exactly that
path. `ApplyBody` rewrites `metadata`, prepends a system block, and deletes
`temperature`, `top_p`, `top_k`, `stop_sequences`, `tool_choice` and
`service_tier`. That is payload alteration on the main route. No ADR amends it,
and `docs/agents/domain.md` requires an ADR conflict to be flagged rather than
silently overridden.

1. Write `docs/adr/0004-claude-code-identity-normalization-exception.md`,
   following the shape of ADR-0003: state that ADR-0001 remains authoritative,
   scope the exception to Anthropic-shaped custom endpoints with normalization
   enabled, list every field `ApplyBody` touches, and name the opt-out
   (`identity.disabled`) plus the two gates that already narrow it
   (`isAnthropicEndpoint`, and the API-key refusal from T1).
2. Add a forward-reference line to ADR-0001 pointing at ADR-0004, as ADR-0001
   presumably already does for ADR-0003.
3. `git commit -m "docs(adr): scope the identity normalization exception to adr-0001"`

## T3 — name the concept once in the glossary

Modify: `CONTEXT.md`

The feature ships one concept under four names: the `ccidentity` package,
`claudecode.IdentityConfig`, `Config.SpoofIdentity` and
`MessageRequest.Normalize`. "Spoof", "normalize" and "identity" are used
interchangeably. `CONTEXT.md` already defines **Account** as a Google credential,
so a bare "Identity" now collides with it.

1. Pick one term. Recommended: **Wire Identity** — the header and body shape a
   request presents to Anthropic, distinct from **Account**, which is the
   credential it authenticates with.
2. Add the glossary entry, and list "spoof", "normalize" and bare "identity"
   under `_Avoid_` pointing at it.
3. `git commit -m "docs(context): define wire identity and retire its three synonyms"`

## T4 — retire the five stale `2.1.246` literals

Modify: `internal/auth/claudecode_oauth.go`, `internal/claudecode/client.go`
Test: `internal/claudecode/client_test.go`

First-pass B2 asked for this and the first pass did not do it. All five sites are
unchanged, and `ccidentity.DiscoveryUserAgent` (`defaults.go:20`, the captured
`claude-code/2.1.280`) is declared and referenced by nothing:

| file:line | current value | wrong how |
|---|---|---|
| `internal/auth/claudecode_oauth.go:629` | `claude-code/2.1.246` | stale version |
| `internal/auth/claudecode_oauth.go:680` | `claude-code/2.1.246` | stale version |
| `internal/auth/claudecode_oauth.go:733` | `claude-code/2.1.246` | stale version |
| `internal/claudecode/client.go:480` | `Claude-Code/2.1.246` | stale version, and the capture shows lowercase |
| `internal/claudecode/client.go:546` | `Claude-Code/2.1.246` | stale version, and the capture shows lowercase |

1. Add a test asserting the discovery and validation requests carry
   `ccidentity.DiscoveryUserAgent` verbatim, including its case.
2. `go test ./internal/claudecode/ -run UserAgent` — expect failure.
3. Replace all five literals with the constant. Import `ccidentity` in
   `internal/auth` if that does not introduce a cycle; if it does, say so here
   and re-export the constant rather than copying the string.
4. Re-run, plus `go build ./...` — expect pass.
5. `git commit -m "fix(auth,claudecode): send the captured discovery user-agent"`

## T5 — delete the falsified session-header comment

Modify: `internal/api/claudecode_proxy.go`

`claudecode_proxy.go:142-146` still reads "Claude Code does not send a session
header: it carries the identifier in metadata.user_id". The first-pass plan's own
corrected model table records `X-Claude-Code-Session-Id` as present in every
captured record and stable across the turns of one process. First-pass B2 asked
for this and the first pass did not do it.

1. Rewrite the comment to state what the captures show: Claude Code does send
   `X-Claude-Code-Session-Id`; this function reads the four foreign-client
   spellings it accepts from third-party harnesses and falls back to the body,
   which is a different question from what Claude Code itself sends.
2. If `X-Claude-Code-Session-Id` belongs in the header list at `:149`, that is a
   behaviour change — file it, do not fold it into a comment fix.
3. `git commit -m "docs(api): correct the session-header comment against the capture"`

## T6 — build the default profile once, on the pooled path too

Modify: `internal/ccidentity/defaults.go`, `internal/claudecode/client.go`
Test: `internal/ccidentity/defaults_test.go`

First-pass T9 asked for "a package-level value built in `init`" and was recorded
done inside `857632c`. Only `server.go:1007` gained a cached variable.
`DefaultProfile` (`defaults.go:185`) still allocates the omit slice, the beta
slice and two closures on every call, and `client.go:356,364,381` calls it three
times per request. The pooled gateway is the one path the live gate exercises, so
the PR description's "built once instead of three times per request" is false
exactly where it was measured.

1. Add a test asserting two `DefaultProfile()` calls return slices backed by the
   same array (compare `&p1.Omit[0] == &p2.Omit[0]`), which fails while each call
   reallocates.
2. `go test ./internal/ccidentity/ -run DefaultProfile` — expect failure.
3. Back `DefaultProfile` with a package-level value. Keep `StaticFor` a function:
   its comment already explains that three captured values depend on the
   identity, so only the profile is hoistable.
4. Hoist the three `client.go` call sites to one local.
5. Re-run — expect pass.
6. `git commit -m "perf(ccidentity): build the default profile once"`

Note in the commit body that the shared value makes `Omit` and `Betas` aliased
across callers; neither is mutated today, and step 1's test would catch a caller
that started to.

## T7 — pin the missing `Go-http-client` regression

Modify: `internal/claudecode/client.go`
Test: `internal/claudecode/client_test.go`

First-pass B4 asked for "a regression test that a request built by
`claudecode.SendMessage` never carries `Go-http-client`". No such test exists.
After first-pass T5 the non-normalized branch (`client.go:392-412`) sets no
`User-Agent` at all, so an API-key send emits Go's default `Go-http-client/1.1` —
the exact fingerprint leak this feature exists to remove. `ValidateAccount`
(`client.go:432`) has the same hole.

1. Add `TestSendMessageNeverSendsGoDefaultUserAgent`: drive both branches
   (OAuth, normalized; API key, not normalized) at a recorder and assert no
   request carries a `User-Agent` containing `Go-http-client`. Do the same for
   `ValidateAccount`.
2. `go test ./internal/claudecode/ -run UserAgent` — expect failure on the
   non-normalized branch.
3. Set the captured User-Agent on the non-normalized branch too. It is not an
   identity claim: the version string is public, and the alternative is
   announcing the Go standard library.
4. Re-run — expect pass.
5. `git commit -m "fix(claudecode): never send the go default user-agent"`

## T8 — resolve the unpopulated `Turn`

Modify: `internal/ccidentity/profile.go`, `internal/ccidentity/defaults.go`

No production caller populates `ccidentity.Turn`: `server.go:1069,1086,1129,1188`
all pass `ccidentity.Turn{}`, and `claudecode.MessageRequest.Turn`
(`client.go:338`) is never set. `cc_prev_req` therefore never reaches the wire.
Two captured constants are equally unreferenced.

Decide per identifier rather than deleting the group:

- `Turn` and `cc_prev_req`: **keep**. They encode a real captured behaviour
  (`apply.go:243,251`), they are covered by `apply_test.go:442-446`, and the
  follow-up-turn value cannot be reconstructed from the capture later if dropped.
  Add one comment at the type naming why no caller sets it today and what a
  caller would need to thread through to.
- `MetadataFields` (`defaults.go:59`): **delete**. Nothing reads it, and
  `apply.go` states the opposite — that key order "is fixed by the declaration
  order of the struct that encodes it". A constant that contradicts the code is
  worse than no constant.
- `EntrypointInteractive` (`defaults.go:28`): **keep**. It is half of a
  documented captured pair and is exercised at `apply_test.go:452,477`.
- `DiscoveryUserAgent`: becomes live in T4.

1. Apply the four decisions.
2. `go build ./... && go test ./internal/ccidentity/`.
3. `git commit -m "refactor(ccidentity): drop the contradicted metadata key list"`

## T9 — drop the `SpoofIdentity` delegate

Modify: `internal/claudecode/types.go`, callers
Test: existing

`Config.SpoofIdentity` (`types.go:172`) is a one-line delegate to
`c.Identity.Identity(...)`. Two names for one call, and it is one of the three
synonyms T3 retires.

1. Inline it at its call sites, or keep it and delete
   `IdentityConfig.Identity`'s external use — one name, not two.
2. `go build ./... && go test ./internal/...`.
3. `git commit -m "refactor(claudecode): one name for the wire identity lookup"`

## T10 — one identity validator, not two

Modify: `internal/api/claudecode_management.go`
Test: `internal/api/management_test.go`

`validateIdentityField` (`:69-80`) and the inline block in
`handleClaudeCodeConfigPost` (`:103-113`) are the same marshal → unmarshal into
`IdentityConfig` → `Validate` shape. Two copies of a validator drift, and this
one guards the control-character rejection added by first-pass T8.

1. Add a test that both config surfaces reject the same malformed identity
   payload, including a control character.
2. Call `validateIdentityField` from the POST handler; delete the inline copy.
3. `go test ./internal/api/ -run Identity` — expect pass.
4. `git commit -m "refactor(api): validate the identity field through one helper"`

## T11 — collapse the repeated identity field clump in the web UI

Modify: `internal/webui/public/views/settings.html`,
`internal/webui/public/js/components/models.js`

The seven-term `ccConfig.identity?.x || ...` disjunction appears three times in
`settings.html:1597-1604` (`:open`, `:class`, `x-text`), and the same field clump
is spelled out three times in `models.js` (default, load, save). Seven fields
travelling together three times over is the type that wants to be born.

1. Add `ccIdentityIsCustom` as one getter; use it at all three template sites.
2. Add `ccIdentityDefaults()` in `models.js`; use it at all three sites.
3. `go test ./internal/webui/` (the translations test guards the label keys).
4. Manually load the settings view and toggle the field group once.
5. `git commit -m "refactor(webui): one source for the identity field defaults"`

## T12 — `randomHex(3)[:5]`

Modify: `internal/ccidentity/apply.go`

`apply.go:275` generates six hex characters to use five, against a `randomHex`
documented at `:290` as returning `2*n`. Either the function takes characters or
the slice needs a reason.

1. Prefer the comment: the captured `cch` value is five characters, and three
   bytes is the smallest read that covers it. One line saying so at `:275`.
2. `git commit -m "docs(ccidentity): say why cch takes five of six hex characters"`

## T13 — correct the record

Modify: `docs/superpowers/plans/2026-09-23-pr91-review-remediation.md`, the PR
description

Three statements are now false:

- That plan's line 381 says the pre-existing gofmt breakage in
  `internal/accounts/manager.go`, `internal/cloudcode/{client,quota}.go`,
  `internal/logger/stream_test.go` and `internal/api/classifier_rules.go` is
  "untouched here". Commits `40fc443` and `ed8fae0` touched all of it, and
  `classifier_rules.go` is on the originating plan's out-of-scope list.
- The PR description says the default profile is built once per request. T6 is
  what makes that true; until then it is false for the pooled gateway.
- The PR description says seven identity overrides are validated.
  `types.go:120` checks six strings, because `Disabled` is a bool.

1. Amend line 381 to record what was actually reformatted and why the whole-repo
   gofmt pass was folded in.
2. Update the PR description's two claims once T6 lands.
3. `git commit -m "docs(plan): correct three statements the first pass left false"`

## Scope creep — disposition, not tasks

- **The whole-repo gofmt commits (`40fc443`, `ed8fae0`)**: keep. Reverting churns
  a pushed branch to restore unformatted files. T13 corrects the record instead.
- **`scripts/git-hooks/pre-commit` and its Makefile targets (`07a6eec`)**: keep,
  but it is unrequested by either spec and installing hooks is opt-in. Add one
  line to `AGENTS.md` naming `make install-hooks` so it is documented rather than
  discovered.
- **The 30 unrelated plan documents (~14k lines) in `27c59b4`**: unchanged from
  the first pass — removing them is a history edit on a pushed branch and stays
  the operator's call.

## Order and gates

T1 first and alone: it is the only finding that breaks a working deployment, and
it should be reviewable without the rest. T2 and T3 next, because they are the
documented-standard breaches and they are pure documentation. T4–T7 are the spec
debt. T8–T12 are design cleanups and can land as one reviewable batch. T13 last,
after T6 makes its second bullet true.

Full gate after T7 and again after T12:

```
go build ./... && go test ./internal/...
python3 -m pytest scripts/test_*.py -q
```

`scripts/verify-claude-code-identity.sh` is the live wire gate. It needs the
operator to run it, as recorded in the first-pass plan, and T1 changes what it
should show for an API-key endpoint — the header set for such an endpoint must
now be the caller's, not the captured one.
