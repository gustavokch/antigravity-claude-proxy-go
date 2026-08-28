# Remediation Plan for PR #34 Review Findings

**Goal**: Remediate review findings on PR #34 regarding deterministic longest-prefix model resolution, comprehensive alias exposure in `/v1/models`, and test coverage for overlapping prefix aliases.

**Architecture**:
- `internal/claudecode/router.go`: Maintain sorted slice of prefixes (or sort by descending length) to guarantee deterministic greedy prefix matching.
- `internal/api/server.go`: Iterate over all aliases in `item.Aliases` when populating `/v1/models` so all configured aliases appear as selectable models.
- `internal/claudecode/router_test.go`: Add test cases for longest-prefix precedence.
- `internal/api/models_discovery_test.go`: Assert that multiple aliases in `item.Aliases` are registered as standalone models.

**Tech Stack**: Go 1.22+

---

## Tasks

### Task 1: Deterministic Longest-Prefix Matching in Claude Code Router

- **Target Files**:
  - `internal/claudecode/router.go` (Modify)
  - `internal/claudecode/router_test.go` (Modify)
- **Step 1**: Add failing test in `router_test.go` verifying that overlapping prefixes (e.g., `sonnet-3-5-extra` vs `sonnet-3-extra`) deterministically resolve to the longer matching alias.
- **Step 2**: Run `go test -v -run TestRouter_LongestPrefixMatch ./internal/claudecode` (Confirm failure / behavior).
- **Step 3**: Update `router.go` to sort prefix candidates by descending length or precalculate sorted prefix slices.
- **Step 4**: Run `go test -v ./internal/claudecode` (Confirm pass).
- **Step 5**: Git commit fix.

### Task 2: Expose All Aliases in `/v1/models`

- **Target Files**:
  - `internal/api/server.go` (Modify)
  - `internal/api/models_discovery_test.go` (Modify)
- **Step 1**: Add test in `models_discovery_test.go` checking that every alias in `item.Aliases` is listed in `/v1/models`.
- **Step 2**: Run `go test -v -run TestClaudeCodeModelDiscovery ./internal/api` to verify failure.
- **Step 3**: Modify `server.go` to iterate over all entries in `aliases := item.Aliases` (plus `item.Alias`) and append missing models to `models` slice.
- **Step 4**: Run `go test -v -run TestClaudeCodeModelDiscovery ./internal/api` (Confirm pass).
- **Step 5**: Git commit fix.

### Task 3: Full Verification & Remote Push

- **Target Files**: All packages
- **Step 1**: Run `go test ./...` to verify 100% pass across all packages.
- **Step 2**: Push to remote branch `feat/claude-model-aliasing`.
