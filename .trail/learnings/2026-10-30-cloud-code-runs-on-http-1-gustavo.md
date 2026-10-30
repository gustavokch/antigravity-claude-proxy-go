---
title: "Cloud Code runs on HTTP/1.1, so check for an early cancel with per-connection bytes, not HTTP/2 resets"
author: Gustavo
date: 2026-10-30
branch: feat/agy-generation-body
cost: { minutes: 8, tokens: 63233 }
id: l-4e1f1f2a
scope: repo
status: active
replaces: l-3515f6b2
src: agent
touches:
  - internal/accounts/outputlimit.go
---

## Decided
The proxy's Cloud Code connection is HTTP/1.1. agy sends no ALPN (its JA4 `t13d131100_...` ends in `00`), and the Go client copies that. So an early cancel cannot show as an HTTP/2 RST_STREAM. It shows as a TCP connection that closes before the response ends. To prove the proxy reads a response to its end, watch that connection's received bytes and its state:
- After the client gets its finish, the connection's bytes_in keeps growing until upstream ends.
- After that, the connection stays ESTABLISHED. Go keeps an HTTP/1.1 connection open for reuse only when the body was read to EOF. An early close drops the connection.
Measured 2026-10-30 on PR #142 (max_tokens 50, claude-sonnet-5-5): the client finished at 5.0 s, and upstream bytes grew from 10.8 KB to 335.7 KB over the next 31 s. The connection was still ESTABLISHED 37 s after that.

## Tried and ruled out
- `tshark -Y 'http2.type == 3'` on a tcpdump capture: the frames are encrypted, and Cloud Code is not HTTP/2, so the filter is always empty. It "passes" whatever the proxy does.
- `GODEBUG=http2debug=2`: it logs only the proxy's HTTP/2 hosts (openrouter.ai), not Cloud Code. It also writes request header values, including `authorization`, to stderr in plain text. Do not use it on a real config. If you must use it, delete the log.

## Watch out
- Run a test proxy with its own HOME. The proxy writes `server.pid` and `accounts.json` under `~/.config/antigravity-proxy`, so a second instance with the real HOME overwrites the live proxy's files.
- In the copied config, set `claudecode.enabled` to false. If you do not, Claude models can go to the Claude Code upstream and not to Cloud Code.
- Each account gets its own upstream connection. Use one request per account, or follow the connection that the request opened.

In the code:
- `T=$(mktemp -d)`, copy `~/.config/antigravity-proxy/{accounts,config}.json` (with `jq '.claudecode.enabled=false'`) and `~/.gemini/antigravity-cli/antigravity-oauth-token` into `$T`, then run `HOME=$T ./bin/proxy -listen 127.0.0.1:8099`. Afterwards run `rm -rf "$T"`.
- Bytes per connection: `nettop -m tcp -p <pid> -L 1 -J bytes_in,bytes_out -x` (sample about every 0.3 s). Connection state: `lsof -nP -a -p <pid> -iTCP`.
- Code under test: `internal/accounts/outputlimit.go` (the background read to EOF after the max_tokens finish).
