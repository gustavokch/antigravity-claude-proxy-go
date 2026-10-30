---
title: "A raw packet capture cannot show HTTP/2 stream resets; use Go's HTTP/2 debug log"
author: Gustavo
date: 2026-10-30
branch: feat/agy-generation-body
cost: { minutes: 29, tokens: 179323 }
id: l-3515f6b2
scope: repo
status: active
src: agent
touches:
  - AGENTS.md
---

## Decided
To prove the proxy sends (or does not send) an HTTP/2 RST_STREAM to Cloud Code, read Go's own HTTP/2 frame log, not a packet capture. Run the proxy with `GODEBUG=http2debug=2` and count the frames it wrote. This only adds logging: it does not change TLS, so the JA4 match is not affected.

## Tried and ruled out
- `tshark -r <cap> -Y 'http2.type == 3'` on a tcpdump capture: the HTTP/2 frames are inside TLS, so the filter always matches nothing. It "passes" even when resets are sent. The proxy has no TLS key log, and adding one (`KeyLogWriter`) would touch tls.Config, which AGENTS.md forbids on the Cloud Code path.

## Watch out
- The log line comes from Go's HTTP/2 framer: `http2: Framer 0x...: wrote RST_STREAM stream=N len=4 ErrCode=CANCEL`. This was read from the Go source. It was not run on 2026-10-30, because the sandbox had no outbound network. Confirm the string the first time you use it.
- The log is very verbose, so send stderr to a file.

In the code:
- `go build -o bin/proxy ./cmd/proxy && GODEBUG=http2debug=2 ANTIGRAVITY_PROXY_LISTEN=127.0.0.1:8099 ./bin/proxy 2> /tmp/h2.log`
- `grep -c 'wrote RST_STREAM' /tmp/h2.log`
- First used for PR #142 finding 4: at max_tokens, the proxy reads upstream to EOF instead of cancelling (`internal/accounts/outputlimit.go`).
