package transport

import (
	"crypto/tls"
	_ "embed"
	"net/http"
	"sort"
	"strings"
	"time"

	"antigravity-go-proxy/internal/httpwire"
	"antigravity-go-proxy/internal/tlsreplay"
)

// The two ClientHellos Claude Code 2.1.304 sends to api.anthropic.com, recorded
// off the wire by a listener that read the first TLS record of each connection
// (record header included). Claude Code runs on Bun, which has two TLS stacks:
//
//   - fetch, ALPN http/1.1: POST /v1/messages, /api/eval/*, and part of
//     /api/event_logging/v2/batch (JA4 t13d1713h1_5b57614c22b0_7f527383e249);
//   - node:https via axios, no ALPN: every GET under /api/claude_code/*,
//     /api/claude_cli/bootstrap, /mcp-registry/*, and the rest of the event
//     batches (JA4 t13d181000_5d04281c6031_25d8bb11352d).
//
// The request-to-stack map comes from scripts/mitm_header_dump.py, which
// records the JA4 of the hello that opened each connection. A new Claude Code
// release that upgrades Bun changes these bytes; recapture them then.
var (
	//go:embed claude-code-fetch-clienthello.bin
	claudeCodeFetchHello []byte
	//go:embed claude-code-node-clienthello.bin
	claudeCodeNodeHello []byte
)

// Each stack resumes TLS sessions, as Bun does: after the first connection to a
// host the real client's hello carries pre_shared_key. The cache is per stack,
// so the messages transports share tickets and the discovery ones share theirs.
var (
	fetchHello = tlsreplay.MustNew(claudeCodeFetchHello, []string{"http/1.1"}).WithResumption()
	nodeHello  = tlsreplay.MustNew(claudeCodeNodeHello, nil).WithResumption()
)

// fetchStack is how Claude Code's fetch stack writes a request, from the
// POST /v1/messages records of .reference/claude-code-headers-20261020.jsonl: the
// application's headers in bytewise-sorted order (capitals before lowercase, as
// the capture shows), then Connection, Host, Accept-Encoding and Content-Length.
var fetchStack = httpwire.Stack{
	Order:          sort.Strings,
	Tail:           []string{httpwire.Connection, httpwire.Host, httpwire.AcceptEncoding, httpwire.ContentLength},
	AcceptEncoding: "gzip, deflate, br, zstd",
	KeepAlive:      true,
}

// nodeStack is how the node:https stack (axios) writes one, from the
// GET /api/claude_code/* records: the application's headers, then
// Accept-Encoding, Host and Connection: close, one connection per request.
//
// The application's order is that of the settings request, the one a caller can
// reproduce without knowing the call site: the real client orders each call's
// headers as that call builds them, and policy_limits differs from settings. The
// Content-Length position of a node POST is an inference; no node POST was
// captured.
var nodeStack = httpwire.Stack{
	Order:          nodeOrder,
	Tail:           []string{httpwire.ContentLength, httpwire.AcceptEncoding, httpwire.Host, httpwire.Connection},
	AcceptEncoding: "gzip, compress, deflate, br",
	KeepAlive:      false,
}

// nodeHeaderOrder ranks the headers the node stack writes first; any other
// header follows in bytewise order.
var nodeHeaderOrder = []string{
	"Accept", "Content-Type", "Authorization", "anthropic-version", "anthropic-beta",
	"User-Agent", "Cache-Control", "Pragma",
}

func nodeOrder(names []string) {
	rank := func(name string) int {
		for i, known := range nodeHeaderOrder {
			if strings.EqualFold(known, name) {
				return i
			}
		}
		return len(nodeHeaderOrder)
	}
	sort.Slice(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
}

// ClaudeCodeMessages returns the transport for the requests Claude Code's fetch
// stack sends: POST /v1/messages. It speaks HTTP/1.1, presents that stack's TLS
// handshake and writes a request the way that stack lays one out. Every call
// returns a transport with its own connection pool.
//
// The real client reaches api.anthropic.com over HTTP/1.1 — every record in
// .reference/claude-code-headers-20261020*.jsonl says so, through an interceptor
// that negotiates h2 with any client that offers it — so a transport that offers
// h2 is told apart before a single header is read.
func ClaudeCodeMessages() *httpwire.Transport {
	return claudeCode(fetchHello, fetchStack)
}

// ClaudeCodeDiscovery is ClaudeCodeMessages for the requests the real client
// makes through node:https: the account, settings and model GETs, and the OAuth
// token calls (which are an inference: only api.anthropic.com was captured).
func ClaudeCodeDiscovery() *httpwire.Transport {
	return claudeCode(nodeHello, nodeStack)
}

// claudeCode builds a transport for one stack. A proxy from the environment
// applies to both schemes: http requests go to it in absolute form, and the https
// dialer tunnels through it before the hello is sent.
func claudeCode(hello *tlsreplay.Hello, stack httpwire.Stack) *httpwire.Transport {
	t := &httpwire.Transport{
		Stack:                 stack,
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: ResponseHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
	}
	t.DialTLS = hello.DialFunc(http.ProxyFromEnvironment, func() *tls.Config { return t.TLSClientConfig })
	return t
}
