# Claude Code Wire Identity Normalization and its Exception to ADR-0001

## Context

ADR-0001 states that custom endpoints are forwarded transparently, streaming "the client request and server response without altering the body payload". ADR-0003 §2 restates that as the Main Route Invariant: general user requests directed through `customEndpoints` continue to use pure, transparent `httputil.ReverseProxy` without payload alteration.

Anthropic distinguishes first-party Claude Code traffic from third-party traffic by fingerprint, not only by credential. A request forwarded verbatim from a foreign harness (Cursor, an OpenAI shim, any SDK) advertises that harness through its headers and through body fields Claude Code never sends. Forwarding it unaltered to an Anthropic-shaped endpoint is what the fingerprint check is looking for.

`internal/ccidentity` therefore rewrites the request to match `.reference/claude-code-headers-20260923*.jsonl`, the captured first-party traffic. `internal/api/server.go` calls `ccidentity.ApplyBody` on the custom-endpoint path — the exact path ADR-0001 and ADR-0003 §2 declare untouched. That is payload alteration on the main route, so it is recorded here rather than left as a silent override.

## Decision

ADR-0001 remains authoritative. Its payload-alteration guarantee is narrowed by one exception, scoped as follows.

1. **Scoped Boundary**: normalization applies only when the endpoint is Anthropic-shaped (`isAnthropicEndpoint`: a URL path ending in `/messages`, or a host under `anthropic.com`), and only when the endpoint carries no configured `apiKey`. Any other custom endpoint is forwarded exactly as ADR-0001 describes.

2. **Payload Alteration Performed**: within that scope, `ApplyBody` makes three changes and no others.
   - `metadata.user_id` is set to the captured stringified JSON object (`device_id`, `account_uuid`, `session_id`, in that order). Other `metadata` keys are preserved.
   - A system block carrying the `x-anthropic-billing-header` line is prepended to `system`. The caller's own system prompt is preserved after it.
   - Six top-level fields the captured traffic never carries are deleted: `temperature`, `top_p`, `top_k`, `stop_sequences`, `tool_choice`, `service_tier`.

   Messages, tools, model and `max_tokens` are untouched. `context_management`, `diagnostics` and `output_config` are deliberately not synthesised: the capture shows Claude Code sending them, and this package cannot invent their contents.

3. **Credential Invariant**: an endpoint with a configured `apiKey` is never normalized. `x-api-key` is on the header omit list, so normalizing such an endpoint would delete the credential it authenticates with; and the normalized request claims an OAuth Claude Code identity that an API-key credential contradicts. `internal/claudecode/client.go` applies the same refusal on the pooled gateway path.

4. **Fail Closed**: normalization runs before the reverse proxy is constructed, because `ApplyBody` can fail and `httputil.ReverseProxy`'s `Rewrite` hook has no error return. A body that cannot be normalized yields 502; it is not forwarded unaltered. An unnormalized body is the fingerprint this feature exists to remove, so forwarding it is worse than refusing.

5. **Opt-out**: `identity.disabled` on the endpoint (or on the Claude Code config for the pooled path) restores ADR-0001 behaviour exactly. The flag is a *disable*, so the zero value normalizes; an operator who wants transparent forwarding for an Anthropic-shaped keyless endpoint must set it.

## Consequences

- Foreign harnesses reach Anthropic-shaped endpoints without advertising themselves, which is the feature's purpose.
- ADR-0001's guarantee no longer holds unconditionally for `customEndpoints`. It holds for every endpoint outside the scope in §1, and for any endpoint with `identity.disabled`.
- Six request fields are silently dropped in scope. A caller that depends on `temperature` reaching an Anthropic-shaped endpoint must disable normalization for it.
- The capture is the specification. `scripts/diff_claude_code_identity.py` gates the produced request against the committed JSONL, so a drift from the captured shape is a test failure rather than a wire surprise.
