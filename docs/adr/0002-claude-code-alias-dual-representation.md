# Claude Code Allowlist Alias Dual Representation

## Context

In `claudecode.ModelConfig`, aliases for allowlisted models are represented by two fields: `Alias string` (a comma-separated string) and `Aliases []string` (a string slice). A strict domain model would avoid Primitive Obsession by maintaining only `Aliases []string`. However, existing persisted configurations written by earlier WebUI releases carry comma-joined strings in `alias` with `aliases` omitted or empty. Furthermore, the WebUI settings view binds an inline editable input to `m.alias`.

## Decision

We retain both `Alias` and `Aliases` on `ModelConfig` and provide `(m ModelConfig) ExpandAliases() []string` as a read-time helper. `ExpandAliases` splits `Alias` by comma, combines the elements with `Aliases`, trims whitespace, drops empty strings, and deduplicates case-insensitively while preserving original casing. In the WebUI, `aliases` is treated as the source of truth, and `alias` is derived as a comma-separated display representation before persistence.

## Consequences

- Existing user configuration files on disk continue to function without requiring an explicit migration step.
- Persisted allowlist entries remain compatible with legacy configurations while allowing individual alias resolution across all routing and catalog endpoints (`GET /v1/models`, `isClaudeModel`, `handleAccountLimits`).
- Dropping `Alias` in favor of a single `Aliases []string` field should only occur if the project implements a general configuration schema migration framework.
