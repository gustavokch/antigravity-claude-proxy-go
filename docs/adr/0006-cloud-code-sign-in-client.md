# 0006 — Cloud Code sign-in uses the agy CLI OAuth client

Date: 2026-11-10
Status: accepted

## Context

The installed `agy` embeds three OAuth client registrations:

| Client | Loopback redirect | Verdict |
|---|---|---|
| `1071006060591-…` | accepted | the client `agy` itself signs in with |
| `884354919052-…` | accepted | token is not entitled to a Cloud Code project |
| `1098991175725-…` | `redirect_uri_mismatch` | registered only for `https://antigravity.google/oauth-callback` |

The entitlement is bound to the client that issued the access token, not to the
account. Calling `v1internal:loadCodeAssist` for the same account
(`ghlsem@gmail.com`) with byte-identical requests, `User-Agent` and body
(`{"metadata":{"ideType":"ANTIGRAVITY"}}`), produced:

- token from `1071006060591` (`aud` of the stored `agy` token):
  `currentTier: free-tier`, `cloudaicompanionProject: "aicode-consumers"`.
- token from `884354919052`: `allowedTiers` offers only `standard-tier`
  (`userDefinedCloudaicompanionProject: true`), `ineligibleTiers` reports
  `GOOGLE_TOS_NOT_SUPPORTED_BY_CLIENT` for `free-tier`, and no project is named.
  `onboardUser` on that tier answers `done: true` with an empty project, so the
  account can never generate.

An account signed in through the wrong client therefore refreshes fine and then
fails every request with "loadCodeAssist response did not include a Cloud Code
project". Sign-in previously took the first embedded client whose loopback
redirect Google accepted, which selected `884354919052`.

## Decision

`internal/auth` prefers the `agy` CLI client (`agyCLIClientID`) when building a
sign-in URL: candidates are reordered so that client is probed first, and the
client Google accepts for the loopback redirect is the one whose credential
pairs redeem the code. An agy login that names its client (`azp` of its stored
id token, or the login file) still wins, because that is the same client. The
registration probe stays as a guard: a client registered only for the hosted
callback is still skipped.

The registration probe runs before locking the active flow registry. Only a
successfully prepared replacement closes the old listener and replaces its
state; a slow or failed probe leaves the active callback usable.

Refresh is unchanged: a refresh token is scoped to its issuing client, so the
candidate list still tries every embedded pair and continues on both
`invalid_client` (unknown secret) and `unauthorized_client` (valid client, token
issued elsewhere).

## Consequences

- Accounts signed in before this decision hold tokens from `884354919052` and
  must be removed and added again; their refresh tokens cannot move clients.
- Replacements are visible only at first use: `accounts.UnentitledClientError`
  turns the missing-project answer into an instruction to re-add the account.
- The client ID is a pin that no capture can refresh (wire captures redact
  tokens). `scripts/client_drift.py` should keep reporting the pinned values in
  `internal/cloudcode/client.go`; a future `agy` that changes its login client
  needs this constant updated by hand, verified against the token `aud`.

## Evidence

- `agy` 1.5.5 token `aud` (macOS keyring item `gemini`, account `antigravity`):
  `1071006060591-…`.
- `mitmdump` capture of `agy models`: `loadCodeAssist` returns
  `cloudaicompanionProject: "aicode-consumers"` for that token.
- Direct `loadCodeAssist` with a `884354919052` token: no project, standard-tier
  only.
