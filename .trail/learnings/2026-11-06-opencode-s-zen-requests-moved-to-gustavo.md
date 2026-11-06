---
title: "OpenCode's Zen requests moved to the inference routes, and the identity gate takes discovery header order from the settings GET"
author: Gustavo
date: 2026-11-06
branch: fix/pr138-drift
id: l-6e8ca8ce
scope: repo
status: active
src: agent
touches:
  - README.md
  - docs/classifier-rules.md
  - internal/api/cachebump_server.go
  - internal/api/dispatch_test.go
  - internal/api/zen_config_test.go
  - internal/api/zen_harness_headers_test.go
  - internal/api/zen_proxy_test.go
  - internal/api/zen_systemone_test.go
  - internal/config/config.go
  - internal/config/config_test.go
---

## Decided
- The Claude Code gate holds the proxy's discovery requests to the header order of the captured `GET /api/claude_code/settings`, chosen by path. It used the first captured GET; the 2.1.311 drift capture happened to record `policy_limits` first, whose order differs, and the gate failed on all three `/v1/models` requests with no change in the proxy.
- OpenCode 2.2.3 sends Zen model requests to `/inference/<family>/v1/*`, the base URLs models.dev lists for its opencode provider (anthropic models at `/inference/anthropic/v1`, openai and openai-compatible at `/inference/openai/v1`), and Jev to `/inference/systemone/v1/systemone`. 2.0.20 used `/zen/v1/*`. The proxy now uses the same routes from base `https://opencode.ai/inference`. The user chose to make the move in the drift PR itself, not as a follow-up PR.
- 2.2.3 also sends `x-opencode-session-id` (the session's own id; OpenCode fills the other three session headers from `parentID ?? fork.sessionID ?? id`, so they agree for a proxy conversation) and a new ClientHello: JA4 `t13d1713h1_5b57614c22b0_7f527383e249`, JA3 unchanged.
- A stored `zen.baseUrl` of `https://opencode.ai/zen` (with or without `/v1`) follows the new default on Load and is dropped on Save, as is the current default. The WebUI posts its default back with every Zen save, which is how the old base got into every config.json.

## Tried and ruled out
- Keeping `/zen/v1/*` because Zen still answers there (on 2026-11-06 `GET /zen/v1/models` and `/inference/openai/v1/models` returned the same 80-id catalog): the path is visible upstream and differs from every current OpenCode client.

## Watch out
- The drift tool found OpenCode requests only on `/zen/`, so the 2.2.3 version and ClientHello came through as "not found" and "no zen connection" manual items, not as drift. The opencode baseline from the 2.0.20 run never merged, so the header diff had to be made by hand from two run directories under `~/.cache/antigravity-proxy/client-drift/runs`.
- The drift tool rebuilds `chore/client-identity-drift` from `fork/main` on every run. The 09:03 run force-pushed over this work (`cd4498d..59a680f`) six minutes after it was pushed. `publish` now refuses while the branch carries a commit without `COMMIT_MARKER` and pushes with a lease, but a run uses the `client_drift.py` on `fork/main`, so the guard protects a PR only after it merges.
- Jev classifier backends keep the URL the operator saved; one saved with the old `/zen/v1/systemone` still works but no longer matches the client. OpenCode's Jev route also sends `TypeSafe-Danger-Zone-Images: true`, read from the binary and not captured on the wire, so the proxy does not send it.

In the code: `scripts/check_identity_policy.py` (`ORDER_REFERENCE_PATH`), `scripts/client_drift.py` (`ZEN_PATHS`, `zen_request`), `internal/zen/zen.go` (route constants), `internal/zen/harness.go` (`HeaderOpencodeSessionID`), `internal/config/config.go` (`seededZenBaseURL`, `dropDefaultZenBaseURL`).
