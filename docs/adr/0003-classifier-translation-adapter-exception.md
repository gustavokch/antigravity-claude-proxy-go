# Classifier Translation Adapter Scope and Exception to ADR-0001

## Context

ADR-0001 states that transparent forwarding via reverse proxy eliminates the need to maintain multi-provider translation adapters within the proxy core.

However, Claude Code executes autonomous background security-monitor (classifier) requests during tool execution. When an operator wishes to reroute these classification calls away from expensive primary models to local or third-party models (e.g. vLLM, Ollama, or OpenAI-compatible gateways), the target backends frequently speak the OpenAI Chat Completions wire format (`/v1/chat/completions`) rather than the Anthropic Messages API format (`/v1/messages`).

## Decision

We introduce a scoped bidirectional translation adapter (`internal/classifier/translation.go`) and an outbound dispatch mechanism (`internal/api/classifier_rules.go`) strictly confined to the security-monitor classifier interception path.

1. **Scoped Boundary**: The translation adapter is utilized exclusively when a matched `config.Rule` specifies `action = "reroute"` and the target `config.TargetBackend` specifies `format = "openai"`.
2. **Main Route Invariant**: ADR-0001 remains fully authoritative for all main-agent chat and tool dispatch traffic. General user requests directed through `customEndpoints` continue to use pure, transparent `httputil.ReverseProxy` without payload alteration.
3. **Synthetic SSE Bridge**: If the client requested streaming (`stream: true`), the classifier subsystem synthesizes standard Anthropic Messages SSE event frames (`message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`) from the completed upstream response to satisfy the client without requiring streaming from the classifier backend.

## Consequences

- Operators can reroute classifier calls to any OpenAI-compatible server or model with zero client reconfiguration.
- The proxy core avoids becoming an arbitrary multi-provider translation layer for general conversation traffic.
- Transparent forwarding guarantees established in ADR-0001 remain intact for primary model routing.
