---
title: An agy refresh orders OAuth clients by the login stored in the token file it refreshes
author: Gustavo
date: 2026-10-30
branch: fix/agy-token-refresh-deadlock
cost: { minutes: 6, tokens: 106118 }
id: l-5b4452a6
scope: repo
status: active
src: agent
touches:
  - internal/auth/agy_oauth_test.go
  - internal/auth/agy_oauth.go
  - internal/auth/oauth_server.go
  - internal/auth/token.go
  - docs/superpowers/plans/2026-10-30-pr140-remediation.md
  - internal/accounts/credentials.go
---

## Decided
Manager.Get puts first the OAuth client named by the id_token `azp` claim of the token file it is refreshing, parsed from the bytes it already holds under its exclusive flock. A refresh token is scoped to the client that issued it, and the id_token beside it names that client. A token file with no id_token gets the default order (last ID, first secret), not the default agy login's claim: another file's id_token describes a different grant.

## Tried and ruled out
- Re-reading the token file inside the refresh (PR #137): a shared flock on a second descriptor waits on this process's own exclusive lock forever. PR #140's first fix (an unlocked os.ReadFile of DefaultTokenPath) stopped the hang but read another login whenever an account sets a non-default `agyTokenPath`.
- A locked read for sign-in and FromRefreshToken: it would make oauth-account refreshes and sign-in wait on any agy refresh that holds the exclusive lock. The claim is only a hint, so those callers read the default agy token file without a lock.

## Watch out
Never open the agy token file a second time while Manager.Get holds its lock; flock locks belong to the open file description, so even a shared lock in the same process blocks.

In the code: internal/auth/token.go Manager.Get passes `func() string { return loginClientIDFromToken(raw) }` to refresh; internal/auth/agy_oauth.go agyLoginClientID (default file, unlocked) and loginClientIDFromToken; internal/accounts/credentials.go sets manager.Path = account.AgyTokenPath. Tests: TestManagerGetRefreshesTheDefaultAgyTokenWithoutDeadlock, TestManagerGetOrdersByTheLoginOfTheTokenItRefreshes.
