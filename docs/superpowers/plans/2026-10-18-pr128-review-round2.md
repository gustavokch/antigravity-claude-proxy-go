# PR #128 Review Round 2 — drift-test honesty

**Goal:** The drift test's exemption reasons are true, and the lists it checks are the lists `init()` registers.

**Architecture:** `internal/zen/client.go` registers `AnthropicWireIDs`, `ChatWireIDs`, `ResponsesWireIDs` into `wireSet` in `init()`. `internal/zen/catalog_drift_test.go` walks a copy of that registration (`wireLists`) for the stale and disjointness checks.

**Tech stack:** Go 1.27rc2, `go test`.

**Spec reference:** PR #128 round-2 review comment.

## Task 1 — Correct the systemone exemption reason

**Files:** Modify `internal/zen/catalog_drift_test.go`.

- Step 1: reword `reasonSystemoneWire`: the Messages router cannot carry the systemone protocol; `jev-*` reaches Zen through the proxy's `POST /v1/systemone` passthrough (`zen.ForwardSystemOne`). Comment-level change, no test can fail first.
- Step 2: `go test -count=1 ./internal/zen` → PASS.

## Task 2 — One wire-list registration table

**Files:** Modify `internal/zen/client.go`, `internal/zen/catalog_drift_test.go`.

**Produces:** package-level `wireLists []struct{ name string; ids []string; wire Wire }` in `client.go`.

- Step 1: move `wireLists` into `client.go` with a `wire` field; `init()` sizes and fills `wireSet` from it; delete the test copy.
- Step 2: `go test -count=1 ./internal/zen` → PASS (refactor; the existing drift, disjointness, and `WireFor` tests are the guard).
- Step 3: `git commit -m "test(zen): one wire-list table for init and the drift checks; fix systemone reason"`.

## Verification

`gofmt -l internal`, `go vet ./...`, `go test -count=1 ./...`; then `git push fork fix/zen-free-model-wires`.
