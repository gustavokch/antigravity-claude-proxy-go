# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

One operator: the person who runs the proxy on their own machine. They keep the
console open beside Claude Code (or another Anthropic Messages client) during
working sessions. No friends, team, or public visitors use the WebUI.

## Product Purpose

The WebUI is the operating console of `antigravity-go-proxy`, a local Go proxy
that serves the Anthropic Messages API to Claude Code and routes each request to
Google Cloud Code accounts (translation route) or to transparent gateways (Kimi,
OpenCode Zen, Claude Code, OpenRouter, custom endpoints).

The operator opens it to:

1. See how much quota is left per account and per model, and when it resets,
   so they can pick the next model.
2. Find out why requests fail: rate limits, banned or disabled accounts,
   gateway errors, live logs.
3. Change routing and gateway configuration: model mappings, gateway
   precedence, gateway keys and allowlists, classifier backends.
4. Look at usage trends: request volume, latency, tokens/s, cache hit rate.

Success: each of these takes one glance or one short path, and the console
stays readable during a long working session.

## Positioning

The console shows one proxy that sits between a single client and many
upstreams. Its distinct facts are multi-account quota pools with reset windows,
account health and rotation strategy (`sticky`, `round-robin`, `hybrid`), and
a gateway precedence list that decides which upstream answers a requested
model.

## Operating Context

- Served by the Go binary from `internal/webui/public` via `embed.FS`; listens
  on `127.0.0.1:8080` by default (`-listen` / `ANTIGRAVITY_PROXY_LISTEN`);
  optional `webuiPassword` gate.
- Alpine.js views (`views/*.html`) loaded into a shell (`index.html`);
  components in `js/components/`; locales `en` and `pt`, with per-feature key
  tests in `translations_test.go`.
- Chart.js draws dashboard charts.
- Used on a desktop monitor during long coding sessions; must also work on a
  narrow window or phone for a quick check.

## Capabilities and Constraints

- Five views: Dashboard, Models, Accounts, Logs, Settings (Settings holds
  Interface, Claude CLI, Models/gateways, Server, Security Monitor, Cloud tabs).
- Styles are hand-written CSS served from the embed. No Node toolchain in the
  build: `go build` is the only build step. The existing `style.css` is a
  frozen Tailwind/DaisyUI output with no config in the repo.
- All behavior, endpoints, and data shown today must keep working.

## Evidence on Hand

Real live data from the operator's accounts and stats endpoints. No
testimonials, customers, or marketing material; none are needed.

## Product Principles

- State before decoration: quota left, reset time, and failure cause outrank
  everything else on a screen.
- One system across views: the same table, control, and status vocabulary
  everywhere.
- Calm at rest, loud only on trouble: a healthy proxy reads quiet; a banned
  account or exhausted pool is impossible to miss.
- Honest data: show real values or say why a value is missing.

## Accessibility & Inclusion

WCAG 2.2 AA: text contrast, keyboard access for every control, visible focus,
accessible names on icon buttons and toggles, `prefers-reduced-motion`
respected. Earlier audits in `.impeccable/audit/` list the known failures.
