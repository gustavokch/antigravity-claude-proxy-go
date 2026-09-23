# Transparent Forwarding via Reverse Proxy for Custom Endpoints

## Context
Clients using this proxy (such as Claude Code CLI) occasionally need access to models not hosted by Google Cloud Code (e.g., official Anthropic endpoints, local mock endpoints, or third-party providers) without changing client configuration files.

## Decision
We route models configured in `customEndpoints` through `httputil.ReverseProxy` directly to the target URL, bypassing the translation pipeline entirely. The proxy rewrites the target host and request URL, injects configured authentication headers (`x-api-key`), and transparently streams the client request and server response without altering the body payload.

## Consequences
- Zero payload translation overhead and zero format compatibility bugs for non-Google endpoints.
- Server-Sent Events (SSE) stream directly with chunk flushing handled natively by Go's reverse proxy.
- Eliminates the need to maintain multi-provider translation adapters within the proxy core. (Amended by ADR-0003 for scoped classifier security-monitor rule rerouting).
- The "without altering the body payload" guarantee above is narrowed by ADR-0004, which scopes Claude Code wire identity normalization to Anthropic-shaped custom endpoints that carry no configured `apiKey`. Every other custom endpoint, and any endpoint with `identity.disabled`, is forwarded as described here.
