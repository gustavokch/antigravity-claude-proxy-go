# Cloud Code 429 Throttle — Dimension Investigation and Fix

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Identify which dimension Google's Cloud Code 429 throttle is keyed on, then make the proxy back off, rotate, and answer clients correctly under that throttle.

**Architecture:** Two halves. First, evidence: the proxy already persists every 429 verbatim (Task 1, done); the MITM harness learns to capture responses, and a new `cmd/probe429` holds every axis fixed but one — model, account, project, endpoint — to name the dimension. Second, the fix: an escalating jittered cooldown replaces the flat 30 s guess, the pool detects a *shared* throttle and waits instead of spending each remaining account on a certain rejection, and an exhausted pool answers the client with HTTP 429 + `Retry-After` instead of today's 400.

**Tech Stack:** Go 1.27rc2 (stdlib only for this work), mitmproxy 12 + Python 3 stdlib `unittest`, existing `internal/accounts` dispatcher/manager, `internal/api` error mapping.

**Spec:** `docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md` — read it first. It holds the evidence table, the open question, defects D1–D5 and requirements R1–R8. Every task below cites the requirement it implements.

## Global Constraints

- Go version: **1.27rc2** (`go.mod`). No new third-party dependencies — stdlib only for every Go file in this plan.
- Python: **stdlib only**. Tests run under `python3 -m unittest`; `mitmproxy` must not be an import requirement for the tests.
- Redaction (R2): never write an OAuth token, cookie, API key, or user prompt text into a log, into `.jsonl` forensics, or into `.reference/`. Response bodies of **successful** calls are generated user content and must never be captured.
- Every Go change ships with tests in the same package. Gate: `make test` (runs `go test ./...`). Format: `gofmt -w` on every touched Go file.
- Commit after every task, Conventional Commits style (`feat:`, `fix:`, `test:`, `docs:`, `chore:`).
- Branch: `fix/429-forensics` (already checked out; Task 1 is its first commit).
- Do not change the account-selection strategy (sticky / round-robin / hybrid) or daily-quota (`quotaInfo`) accounting — both are explicit non-goals in the spec.

## File Structure

| File | Responsibility | Task |
|------|----------------|------|
| `internal/accounts/forensics.go` | Append-only JSONL record of every upstream 429 | 1 (done) |
| `scripts/mitm_header_dump.py` | mitmproxy addon: request **and response** capture, redacted | 2 |
| `scripts/test_mitm_header_dump.py` | stdlib unittest for the addon's record builder | 2 |
| `cmd/probe429/matrix.go` | Pure probe-result model and the dimension verdict | 3 |
| `cmd/probe429/matrix_test.go` | Tests for the verdict logic | 3 |
| `cmd/probe429/main.go` | Probe runner: baseline, four axes, optional burst and window scan | 3 |
| `internal/accounts/retry.go` | Backoff ladder, jitter, `RateLimitError` | 5, 7 |
| `internal/accounts/manager.go` | Shared-throttle detection and pool-wide wait | 6 |
| `internal/accounts/dispatcher.go` | Applies jitter; returns the typed rate-limit error | 5, 7 |
| `internal/config/config.go` | `SharedThrottleWindowMs` replaces the dead `RateLimitDedupWindowMs` | 6 |
| `internal/api/server.go` | Maps rate limits to client 429 + `Retry-After` | 7 |
| `internal/api/management.go` | Shows the active shared throttle in the status output | 8 |

---

### Task 1: Persist every upstream 429 verbatim — ALREADY DONE

**Status:** complete, committed as `c79332a` on `fix/429-forensics`. Implements **R1** and **R2**.

Do not redo this task. Read it before Task 3 and Task 4 — it defines the record format the analysis consumes.

**Files (for reference):**
- `internal/accounts/forensics.go` — `Forensics429Entry`, `Forensics429Recorder`, `forensicsHeaders`
- `internal/accounts/dispatcher.go:398,615,~690` — `record429` called on both the stream and the rotation 429 paths
- `internal/config/config.go` — `Upstream429ForensicsEnabled` (JSON `upstream429ForensicsEnabled`, **off by default**)
- `cmd/proxy/main.go:190-215` — writes to `<configDir>/forensics/upstream-429.jsonl`
- `cmd/debug429/main.go` — standalone replay diagnostic

**Interfaces produced (consumed by Tasks 3, 4, 6, 8):**

```go
type Forensics429Entry struct {
	Timestamp   time.Time         `json:"timestamp"`
	Account     string            `json:"account,omitempty"`
	Project     string            `json:"project,omitempty"`
	Model       string            `json:"model,omitempty"`
	Endpoint    string            `json:"endpoint,omitempty"`
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
	AppliedWait string            `json:"appliedWait,omitempty"`
	Failures    int               `json:"failures,omitempty"`
}

func NewForensics429Recorder(path string) *Forensics429Recorder
func (recorder *Forensics429Recorder) Enabled() bool
func (recorder *Forensics429Recorder) Record(entry Forensics429Entry)
```

- [ ] **Step 1: Enable forensics in the running proxy**

Edit the proxy config (`<configDir>/config.json`) and set:

```json
"upstream429ForensicsEnabled": true
```

Restart the proxy. Confirm the startup log line `upstream 429 forensics enabled` names the path. Everything after this point depends on that file filling during the next wave.

---

### Task 2: Capture responses in the MITM harness

**Implements:** R3. Today `scripts/mitm_header_dump.py` records request headers only. The 429 status, the response headers, and the error body — the only place the throttle dimension is named — are all discarded. This task makes the addon record them, while keeping successful bodies (the user's own generated text) out of the capture entirely.

**Files:**
- Modify: `scripts/mitm_header_dump.py` (full rewrite of the module body; redaction rules unchanged)
- Create: `scripts/test_mitm_header_dump.py`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `build_record(flow) -> dict`, a module-level function importable without `mitmproxy` installed. Record keys: `ts`, `http_version`, `method`, `host`, `path`, `request_bytes`, `headers`, and — when a response exists — `status`, `response_headers`, and `response_body` (error statuses only).

- [ ] **Step 1: Write the failing test**

Create `scripts/test_mitm_header_dump.py`:

```python
"""Unit tests for the mitmproxy header/response dump addon.

Run with: python3 -m unittest discover -s scripts -p 'test_*.py' -v

mitmproxy itself is NOT imported here: the addon guards its own import so
the record builder stays testable on a machine without mitmproxy.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mitm_header_dump import MAX_RESPONSE_BODY, build_record


class FakeHeaders:
    def __init__(self, pairs):
        self._pairs = list(pairs)

    def items(self):
        return list(self._pairs)


class FakeRequest:
    def __init__(self, headers, content=b""):
        self.headers = FakeHeaders(headers)
        self.content = content
        self.pretty_host = "cloudcode-pa.googleapis.com"
        self.path = "/v1internal:streamGenerateContent?alt=sse"
        self.http_version = "HTTP/2.0"
        self.method = "POST"


class FakeResponse:
    def __init__(self, status_code, headers, content=b""):
        self.status_code = status_code
        self.headers = FakeHeaders(headers)
        self.content = content


class FakeFlow:
    def __init__(self, request, response):
        self.request = request
        self.response = response


def flow(status, response_headers=(), response_body=b"", request_headers=()):
    return FakeFlow(
        FakeRequest(list(request_headers) or [("content-type", "application/json")]),
        FakeResponse(status, list(response_headers), response_body),
    )


class BuildRecordTest(unittest.TestCase):
    def test_error_body_and_status_are_captured(self):
        body = b'{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}'
        record = build_record(flow(429, [("retry-after", "17")], body))
        self.assertEqual(record["status"], 429)
        self.assertEqual(record["response_body"], body.decode())
        self.assertIn(["retry-after", "17"], record["response_headers"])

    def test_success_body_is_never_captured(self):
        record = build_record(flow(200, [], b"data: user generated text\n\n"))
        self.assertEqual(record["status"], 200)
        self.assertNotIn("response_body", record)

    def test_response_body_is_truncated(self):
        record = build_record(flow(429, [], b"x" * (MAX_RESPONSE_BODY + 500)))
        self.assertEqual(len(record["response_body"]), MAX_RESPONSE_BODY)

    def test_request_authorization_is_redacted(self):
        record = build_record(
            flow(429, request_headers=[("authorization", "Bearer ya29.SECRET")])
        )
        name, value = record["headers"][0]
        self.assertEqual(name, "authorization")
        self.assertTrue(value["redacted"])
        self.assertEqual(value["scheme"], "Bearer")
        self.assertNotIn("SECRET", repr(record))

    def test_response_cookie_is_redacted(self):
        record = build_record(flow(429, [("set-cookie", "sid=abc123")]))
        self.assertIn(["set-cookie", "[redacted]"], record["response_headers"])
        self.assertNotIn("abc123", repr(record))

    def test_request_bytes_recorded(self):
        f = flow(429)
        f.request.content = b"12345"
        self.assertEqual(build_record(f)["request_bytes"], 5)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `python3 -m unittest discover -s scripts -p 'test_*.py' -v`
Expected: FAIL — `ImportError: cannot import name 'build_record'` (and `mitmproxy` may fail to import at all).

- [ ] **Step 3: Rewrite the addon**

Replace the whole body of `scripts/mitm_header_dump.py` with:

```python
"""mitmproxy addon: dump Cloud Code requests AND responses as ordered JSONL.

Records every intercepted exchange with the two Cloud Code hosts as one JSON
line: ts, http_version, method, host, path, request_bytes, headers, status,
response_headers and — for error statuses only — response_body.

The Authorization value is a live OAuth token, so it is redacted HERE,
before anything is written: scheme (first whitespace token) plus a sha256
and length of the credential token only. Matching is a substring,
case-insensitive match on the header name so variants like
x-goog-iam-authorization-list are over-redacted on purpose.

Response bodies are kept for error statuses ONLY. A 200 on
streamGenerateContent is the user's own generated text and must never land
in a capture file; the 4xx body is what carries the throttle dimension this
capture exists to find.

Output path comes from $MITM_DUMP_OUT (default /tmp/agy-headers-mitm.jsonl),
appended, flushed per line.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
from datetime import datetime, timezone

try:  # mitmproxy is absent when the unit tests import this module
    import mitmproxy.http
except ImportError:  # pragma: no cover - only outside mitmdump
    mitmproxy = None

CLOUDCODE_HOSTS = {
    "cloudcode-pa.googleapis.com",
    "daily-cloudcode-pa.googleapis.com",
}

# Substring match on purpose: over-redact anything auth-ish. Cookie and
# api-key names are included so a future agy that grows a session cookie or
# key header never lands a live credential in the JSONL.
SENSITIVE_NAME = re.compile(
    r"authorization|proxy-authorization|x-goog-iam-authorization|cookie|api[_-]?key",
    re.IGNORECASE,
)

# Header values carrying credentials as "<scheme> <token>" get scheme kept in
# the clear and the token hashed. Values in any other shape (Cookie crumbs,
# raw keys) are hashed whole — nothing of the value stays readable.
AUTH_SCHEMES = {"bearer", "basic", "digest", "token", "negotiate", "oauth"}

# A Google error payload is well under 1 KB. The cap only stops an HTML
# error page from bloating the capture.
MAX_RESPONSE_BODY = 8192


def _redact(value: str) -> dict:
    parts = value.split(None, 1)
    scheme = parts[0] if parts and parts[0].lower() in AUTH_SCHEMES else ""
    token = parts[1] if scheme and len(parts) > 1 else value
    return {
        "redacted": True,
        "scheme": scheme,
        "sha256": hashlib.sha256(token.encode("utf-8")).hexdigest(),
        "len": len(token),
    }


def build_record(flow) -> dict:
    """Return the JSON record for one intercepted flow."""
    req = flow.request

    headers = []
    for name, value in req.headers.items():
        if SENSITIVE_NAME.search(name):
            headers.append([name, _redact(value)])
        else:
            headers.append([name, value])

    record = {
        "ts": datetime.now(timezone.utc).isoformat(),
        "http_version": req.http_version,
        "method": req.method,
        "host": req.pretty_host,
        "path": req.path,
        "request_bytes": len(req.content or b""),
        "headers": headers,
    }

    res = getattr(flow, "response", None)
    if res is None:
        return record

    record["status"] = res.status_code
    # Response headers carry no credential of ours, but a Set-Cookie could
    # start a session; redact by the same rule rather than reason about it.
    record["response_headers"] = [
        [name, "[redacted]" if SENSITIVE_NAME.search(name) else value]
        for name, value in res.headers.items()
    ]
    if res.status_code >= 400:
        body = (res.content or b"").decode("utf-8", "replace")
        record["response_body"] = body[:MAX_RESPONSE_BODY]
    return record


class HeaderDump:
    def __init__(self) -> None:
        self.out_path = os.environ.get("MITM_DUMP_OUT", "/tmp/agy-headers-mitm.jsonl")

    def response(self, flow: mitmproxy.http.HTTPFlow) -> None:
        if flow.request.pretty_host not in CLOUDCODE_HOSTS:
            return
        record = build_record(flow)
        with open(self.out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record, ensure_ascii=False) + "\n")
            f.flush()


addons = [HeaderDump()]
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `python3 -m unittest discover -s scripts -p 'test_*.py' -v`
Expected: PASS, 6 tests.

- [ ] **Step 5: Verify the addon still loads inside mitmdump**

Run: `mitmdump -s scripts/mitm_header_dump.py --set "allow_hosts=^(cloudcode|daily-cloudcode)-pa\.googleapis\.com(:443)?$" &` then `sleep 3` and kill it.
Expected: no traceback in the output; mitmdump reports it is listening. A `NameError` on `mitmproxy` here means the guarded import broke the annotation — confirm `from __future__ import annotations` is the first statement after the docstring.

- [ ] **Step 6: Commit**

```bash
git add scripts/mitm_header_dump.py scripts/test_mitm_header_dump.py
git commit -m "feat(scripts): capture Cloud Code response status, headers and error bodies in MITM dump"
```

---

### Task 3: Probe the throttle dimension

**Implements:** R4. Holds every axis fixed but one so a success on exactly one axis names the dimension. The verdict logic is pure and tested; the network runner is thin.

**Files:**
- Create: `cmd/probe429/matrix.go`
- Create: `cmd/probe429/matrix_test.go`
- Create: `cmd/probe429/main.go`

**Interfaces:**
- Consumes: `accounts.DefaultConfigPath`, `accounts.Load`, `accounts.NewCredentialResolver`, `auth.Manager{}`, `cloudcode.New`, `cloudcode.Client.DoSSE`, `cloudcode.ProdEndpoint`, `cloudcode.DailyEndpoint`, `cloudcode.PathStreamGenerate`, `cloudcode.HTTPError` — all exist today; `cmd/debug429/main.go` is the working example of this exact call shape.
- Produces: `ProbeResult`, `Conclude([]ProbeResult) string`, and a JSONL artifact consumed by Task 4.

- [ ] **Step 1: Write the failing test**

Create `cmd/probe429/matrix_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestConcludeRequiresABaseline(t *testing.T) {
	got := Conclude([]ProbeResult{{Axis: AxisModel, Status: 200}})
	if !strings.Contains(got, "no baseline") {
		t.Fatalf("Conclude without a baseline = %q, want a 'no baseline' verdict", got)
	}
}

func TestConcludeReportsAnIdleUpstream(t *testing.T) {
	got := Conclude([]ProbeResult{{Axis: AxisBaseline, Status: 200}})
	if !strings.Contains(got, "no throttle active") {
		t.Fatalf("Conclude with a succeeding baseline = %q, want 'no throttle active'", got)
	}
}

// One axis free and the rest rejected is the whole point of the matrix: it
// names the dimension the throttle is keyed on.
func TestConcludeNamesTheFreeAxis(t *testing.T) {
	got := Conclude([]ProbeResult{
		{Axis: AxisBaseline, Status: 429},
		{Axis: AxisModel, Status: 200},
		{Axis: AxisAccount, Status: 429},
		{Axis: AxisProject, Status: 429},
		{Axis: AxisEndpoint, Status: 429},
	})
	if !strings.Contains(got, "model-scoped") {
		t.Fatalf("Conclude = %q, want a model-scoped verdict", got)
	}
}

// Every axis rejected is the outcome the 2026-09-17 evidence predicts:
// project-wide or model capacity, which the axes cannot separate.
func TestConcludeReportsASharedThrottle(t *testing.T) {
	got := Conclude([]ProbeResult{
		{Axis: AxisBaseline, Status: 429},
		{Axis: AxisModel, Status: 429},
		{Axis: AxisAccount, Status: 429},
		{Axis: AxisProject, Status: 429},
		{Axis: AxisEndpoint, Status: 429},
	})
	if !strings.Contains(got, "shared across every probed axis") {
		t.Fatalf("Conclude = %q, want a shared-throttle verdict", got)
	}
}

// Two or more axes freed means the wave ended mid-run, not that two
// dimensions exist. Saying so prevents a false conclusion.
func TestConcludeRejectsMultipleFreeAxes(t *testing.T) {
	got := Conclude([]ProbeResult{
		{Axis: AxisBaseline, Status: 429},
		{Axis: AxisModel, Status: 200},
		{Axis: AxisAccount, Status: 200},
	})
	if !strings.Contains(got, "inconclusive") {
		t.Fatalf("Conclude = %q, want an inconclusive verdict", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/probe429/ -run TestConclude -v`
Expected: FAIL to build — `undefined: Conclude`, `undefined: ProbeResult`, `undefined: AxisBaseline`.

- [ ] **Step 3: Write the verdict logic**

Create `cmd/probe429/matrix.go`:

```go
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Probe axes. Each probe holds every dimension fixed but one, so a success on
// exactly one axis names the dimension the throttle is keyed on. See
// docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md
// section 2 for what each answer implies for the proxy.
const (
	AxisBaseline = "baseline"
	AxisModel    = "model"
	AxisAccount  = "account"
	AxisProject  = "project"
	AxisEndpoint = "endpoint"
)

// ProbeResult is one upstream call made by the matrix. Status 0 means the
// call failed before a response (transport error); Error carries the detail.
type ProbeResult struct {
	Axis      string    `json:"axis"`
	Account   string    `json:"account"`
	Project   string    `json:"project"`
	Model     string    `json:"model"`
	Endpoint  string    `json:"endpoint"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	LatencyMS int64     `json:"latencyMs"`
	At        time.Time `json:"at"`
}

func (result ProbeResult) Throttled() bool { return result.Status == 429 }
func (result ProbeResult) Succeeded() bool { return result.Status == 200 }

// Conclude names the throttle dimension from one matrix run. The axes are
// only meaningful while the baseline is rejected: the window is short, so a
// success on a later axis is evidence only if the run is fast enough that the
// wave cannot plausibly have ended in between — hence the multi-axis guard.
func Conclude(results []ProbeResult) string {
	byAxis := make(map[string]ProbeResult, len(results))
	for _, result := range results {
		byAxis[result.Axis] = result
	}
	baseline, ok := byAxis[AxisBaseline]
	if !ok {
		return "inconclusive: no baseline probe in this run"
	}
	if !baseline.Throttled() {
		return fmt.Sprintf(
			"no throttle active: baseline returned %d; re-run during a 429 wave or with -burst",
			baseline.Status)
	}

	var freed []string
	for _, axis := range []string{AxisModel, AxisAccount, AxisProject, AxisEndpoint} {
		if result, ok := byAxis[axis]; ok && result.Succeeded() {
			freed = append(freed, axis)
		}
	}
	sort.Strings(freed)

	switch len(freed) {
	case 0:
		return "throttle is shared across every probed axis (model, account, project, endpoint): " +
			"project-wide or model capacity. Re-run with -window to measure how long it holds"
	case 1:
		return fmt.Sprintf("throttle is %s-scoped: the %s axis succeeded while the baseline was rejected",
			freed[0], freed[0])
	default:
		return fmt.Sprintf("inconclusive: %s all succeeded — the wave probably ended mid-run; re-run",
			strings.Join(freed, ", "))
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/probe429/ -run TestConclude -v`
Expected: PASS, 5 tests.

- [ ] **Step 5: Write the probe runner**

Create `cmd/probe429/main.go`:

```go
// Command probe429 identifies the dimension Google's Cloud Code 429 throttle
// is keyed on. It takes a baseline probe, then — while that baseline is
// rejected — re-probes with exactly one dimension changed: model, account,
// project, or endpoint. A success on exactly one axis names the dimension.
//
// It talks to the user's own accounts with ordinary client requests. -burst
// deliberately induces a throttle and is capped; leave it off unless a wave
// is needed on demand.
//
// Not part of the proxy. See
// docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
)

// maxBurst caps -burst. The point is to trip a short-window throttle, which
// takes a handful of concurrent calls; a larger burst only deepens the hole
// the probe then has to measure.
const maxBurst = 20

func main() {
	model := flag.String("model", "gemini-3.8-flash-high", "baseline upstream model ID")
	altModel := flag.String("alt-model", "gemini-2.5-pro", "model for the model axis")
	altProject := flag.String("alt-project", "", "project ID for the project axis (empty skips the axis)")
	promptKB := flag.Int("kb", 0, "pad the prompt to this many KB")
	burst := flag.Int("burst", 0, "fire N concurrent requests first to induce a throttle (0 = off, max 20)")
	window := flag.Duration("window", 0, "after the matrix, re-probe the baseline until it succeeds, up to this long (0 = off)")
	spacing := flag.Duration("spacing", 30*time.Second, "delay between -window probes")
	out := flag.String("out", "", "append results as JSONL to this path")
	flag.Parse()

	pool, err := loadAccounts()
	if err != nil {
		fmt.Println("load accounts:", err)
		os.Exit(1)
	}
	if len(pool) == 0 {
		fmt.Println("no accounts in the pool")
		os.Exit(1)
	}

	ctx := context.Background()
	primary := pool[0]
	var results []ProbeResult

	if *burst > 0 {
		count := min(*burst, maxBurst)
		fmt.Printf("=== burst: %d concurrent requests on %s to induce a throttle\n", count, primary.Email)
		results = append(results, runBurst(ctx, primary, *model, *promptKB, count)...)
	}

	fmt.Println("=== baseline")
	results = append(results, probe(ctx, AxisBaseline, primary, cloudcode.ProdEndpoint, primary.Project, *model, *promptKB))

	fmt.Println("=== axis: model")
	results = append(results, probe(ctx, AxisModel, primary, cloudcode.ProdEndpoint, primary.Project, *altModel, *promptKB))

	if len(pool) > 1 {
		fmt.Println("=== axis: account")
		second := pool[1]
		results = append(results, probe(ctx, AxisAccount, second, cloudcode.ProdEndpoint, second.Project, *model, *promptKB))
	} else {
		fmt.Println("=== axis: account SKIPPED (pool has one account)")
	}

	if *altProject != "" {
		fmt.Println("=== axis: project")
		results = append(results, probe(ctx, AxisProject, primary, cloudcode.ProdEndpoint, *altProject, *model, *promptKB))
	} else {
		fmt.Println("=== axis: project SKIPPED (pass -alt-project)")
	}

	fmt.Println("=== axis: endpoint")
	results = append(results, probe(ctx, AxisEndpoint, primary, cloudcode.DailyEndpoint, primary.Project, *model, *promptKB))

	verdict := Conclude(results)

	if *window > 0 {
		fmt.Printf("=== window scan: re-probing the baseline every %s for up to %s\n", *spacing, *window)
		results = append(results, scanWindow(ctx, primary, *model, *promptKB, *window, *spacing)...)
	}

	for _, result := range results {
		fmt.Printf("  %-9s %-28s %-24s %-22s status=%d %s\n",
			result.Axis, result.Account, result.Model, shortEndpoint(result.Endpoint), result.Status, result.Error)
	}
	fmt.Println()
	fmt.Println("VERDICT:", verdict)

	if *out != "" {
		if err := writeJSONL(*out, results); err != nil {
			fmt.Println("write results:", err)
			os.Exit(1)
		}
		fmt.Println("results written to", *out)
	}
}

// probeAccount is one pool account with its resolved token and project.
type probeAccount struct {
	Email   string
	Project string
	Client  *cloudcode.Client
}

func loadAccounts() ([]probeAccount, error) {
	path, err := accounts.DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	file, err := accounts.Load(path)
	if err != nil {
		return nil, err
	}
	resolver := accounts.NewCredentialResolver(auth.Manager{}, nil)
	var pool []probeAccount
	for _, account := range file.Accounts {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		credentials, err := resolver.Resolve(ctx, account)
		cancel()
		if err != nil {
			fmt.Printf("  skip %s: resolve: %v\n", account.Email, err)
			continue
		}
		pool = append(pool, probeAccount{
			Email:   account.Email,
			Project: account.ProjectID,
			Client:  cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: 60 * time.Second}),
		})
	}
	return pool, nil
}

// probe makes exactly one upstream call with every dimension pinned, so the
// caller controls which one varies. It calls DoSSE with a single-element
// endpoint list on purpose: the client's normal failover would silently move
// off the endpoint the endpoint axis is measuring.
func probe(ctx context.Context, axis string, account probeAccount, endpoint, project, model string, promptKB int) ProbeResult {
	text := "Say OK"
	if promptKB > 0 {
		text = strings.Repeat("lorem ipsum dolor sit amet ", promptKB*1024/27) + " Say OK"
	}
	payload := map[string]any{
		"project": project,
		"model":   model,
		"request": map[string]any{
			"contents": []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": text}},
			}},
		},
	}
	result := ProbeResult{
		Axis: axis, Account: account.Email, Project: project,
		Model: model, Endpoint: endpoint, At: time.Now(),
	}
	started := time.Now()
	response, err := account.Client.DoSSE(ctx, []string{endpoint}, cloudcode.PathStreamGenerate, payload,
		cloudcode.RequestOptions{}, func(cloudcode.SSEEvent) error { return nil })
	result.LatencyMS = time.Since(started).Milliseconds()
	if err == nil {
		result.Status = response.StatusCode
		if result.Status == 0 {
			result.Status = 200
		}
		return result
	}
	var upstreamError *cloudcode.HTTPError
	if errors.As(err, &upstreamError) {
		result.Status = upstreamError.StatusCode
		result.Error = strings.Join(strings.Fields(upstreamError.Body), " ")
		return result
	}
	result.Error = err.Error()
	return result
}

// runBurst fires count concurrent baseline requests to trip a short-window
// throttle on demand, so the matrix does not have to wait for a natural wave.
func runBurst(ctx context.Context, account probeAccount, model string, promptKB, count int) []ProbeResult {
	results := make([]ProbeResult, count)
	var group sync.WaitGroup
	for i := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index] = probe(ctx, fmt.Sprintf("burst-%d", index), account, cloudcode.ProdEndpoint, account.Project, model, promptKB)
		}(i)
	}
	group.Wait()
	throttled := 0
	for _, result := range results {
		if result.Throttled() {
			throttled++
		}
	}
	fmt.Printf("  burst: %d/%d rejected with 429\n", throttled, count)
	return results
}

// scanWindow measures how long the throttle actually holds: it re-probes the
// baseline until it succeeds or the budget runs out. This is the number the
// proxy's backoff ladder has to cover.
func scanWindow(ctx context.Context, account probeAccount, model string, promptKB int, budget, spacing time.Duration) []ProbeResult {
	var results []ProbeResult
	deadline := time.Now().Add(budget)
	started := time.Now()
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		result := probe(ctx, fmt.Sprintf("window-%d", attempt), account, cloudcode.ProdEndpoint, account.Project, model, promptKB)
		results = append(results, result)
		if result.Succeeded() {
			fmt.Printf("  recovered after %s\n", time.Since(started).Round(time.Second))
			return results
		}
		select {
		case <-ctx.Done():
			return results
		case <-time.After(spacing):
		}
	}
	fmt.Printf("  still throttled after %s\n", budget)
	return results
}

func writeJSONL(path string, results []ProbeResult) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, result := range results {
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	return nil
}

func shortEndpoint(endpoint string) string {
	return strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "www.")
}
```

- [ ] **Step 6: Build and run the full package test**

Run: `gofmt -w cmd/probe429/ && go build ./cmd/probe429/ && go test ./cmd/probe429/ -v`
Expected: build succeeds; 5 tests PASS.

- [ ] **Step 7: Smoke-test against upstream with no throttle active**

Run: `go run ./cmd/probe429 -out /tmp/probe429-smoke.jsonl`
Expected: the baseline returns 200 and the verdict reads `no throttle active: baseline returned 200; ...`. That is the correct output when the upstream is healthy — it proves the transport works before Task 4 relies on it.

- [ ] **Step 8: Commit**

```bash
git add cmd/probe429/
git commit -m "feat(probe429): add throttle-dimension probe matrix with axis verdict"
```

---

### Task 4: Run the experiments and record the findings

**Implements:** the spec's open question (section 2). No production code changes. This task converts the tooling from Tasks 1–3 into an answer, and that answer decides the parameters of Tasks 5–8.

**Files:**
- Create: `.reference/cloudcode-429-probe-<YYYYMMDD>.jsonl` (probe matrix output)
- Create: `.reference/agy-429-mitm-<YYYYMMDD>.jsonl` (live agy capture)
- Modify: `docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md` (append a "Findings" section)

**Interfaces:**
- Consumes: `Forensics429Entry` JSONL from Task 1, `build_record` output from Task 2, `ProbeResult` + `Conclude` from Task 3.
- Produces: a named dimension, and a measured throttle window in seconds, both consumed by Tasks 5 and 6.

- [ ] **Step 1: Capture a live agy run through the MITM harness**

`agy` is the ground-truth client. If agy is rejected at the same moment as the proxy, the throttle is server-side and nothing about the proxy's request shape causes it. If agy sails through while the proxy is rejected, the cause is in what the proxy sends.

Trust the mitmproxy CA first (see `docs/superpowers/plans/2026-09-03-agy-mitm-header-capture.md` Task 2), then run:

```bash
MITM_DUMP_OUT=.reference/agy-429-mitm-$(date +%Y%m%d).jsonl \
  scripts/capture-agy-headers.sh env \
  HTTPS_PROXY=http://127.0.0.1:18080 HTTP_PROXY=http://127.0.0.1:18080 \
  agy --print='say OK'
```

Record in your notes whether agy returned content or an error.

- [ ] **Step 2: Induce a throttle and run the matrix**

Run the matrix with a burst so the run does not depend on catching a natural wave. Use the model that showed the 2026-09-17 wave:

```bash
go run ./cmd/probe429 \
  -model gemini-3.8-flash-high \
  -alt-model gemini-2.5-pro \
  -burst 8 \
  -window 20m -spacing 30s \
  -out .reference/cloudcode-429-probe-$(date +%Y%m%d).jsonl
```

If a second project ID is available for the primary account, re-run adding `-alt-project <id>`; without it the project axis cannot be tested and the verdict can only report "shared across every probed axis".

Expected: the burst trips at least one 429, the baseline is rejected, and the run prints a VERDICT line plus the recovery time from the window scan.

- [ ] **Step 3: Read the recorded 429 bodies**

With forensics enabled (Task 1, Step 1), the proxy has been filing every rejection. Read the dimension out of the bodies:

```bash
python3 -c "
import json,sys
for line in open('$HOME/.config/antigravity-proxy/forensics/upstream-429.jsonl'):
    e = json.loads(line)
    print(e['timestamp'], e.get('account'), e.get('model'), e.get('status'))
    print('   ', e.get('body','')[:400])
    print('   headers:', {k: v for k, v in (e.get('headers') or {}).items() if 'quota' in k.lower() or 'rate' in k.lower() or 'retry' in k.lower()})
"
```

The phrase to look for in the body is the quota metric name — Google names the dimension there, e.g. `per project per minute`, `per user per minute`, or a model-capacity string. Note it verbatim.

- [ ] **Step 4: Append the findings to the spec**

Add a `## 6. Findings (<date>)` section to `docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md` recording, with no interpretation beyond what the data shows:

1. The verbatim 429 body and any quota/rate/retry response headers.
2. The matrix VERDICT line.
3. The measured throttle window (recovery time from the window scan).
4. Whether live agy was rejected in the same window.
5. This decision table, with the row that applies marked:

| Verdict | Task 5 ladder | Task 6 shared throttle | Extra follow-up |
|---------|---------------|------------------------|-----------------|
| model-scoped | cap the top tier at the measured window | keep: rejections still cluster per model | file a follow-up for sibling-model failover |
| project-scoped | as above | keep — it is exactly this case | file a follow-up for project rotation |
| endpoint-scoped | as above | keep | file a follow-up for endpoint failover |
| account-scoped | as above | **drop Task 6** — rotation is already correct | none |
| shared across every axis | as above | keep — the primary fix | none |

- [ ] **Step 5: Verify no secret reached the artifacts**

Run: `grep -ric "bearer \|ya29\.\|refresh_token" .reference/*.jsonl`
Expected: `0` for every file. A non-zero count means redaction failed — delete the artifact and fix Task 2 before continuing.

- [ ] **Step 6: Commit**

```bash
git add .reference/ docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md
git commit -m "docs(429): record throttle-dimension probe results and live agy capture"
```

---

### Task 5: Escalate and decorrelate the rate-limit cooldown

**Implements:** R5, fixing D1 and D2. `SmartBackoff` returns a constant 30 s for `ReasonRateLimit` (`internal/accounts/retry.go:142`), so a 19-minute wave produces ~38 doomed calls per account, and two accounts rejected in the same second wake in lockstep. This task adds an escalating ladder and additive jitter.

Jitter is **additive only**. Subtracting could retry before a server-specified reset, which is the one number upstream actually gave us.

**Files:**
- Modify: `internal/accounts/retry.go` (`SmartBackoff`, new `rateLimitTiers`, new `Decorrelate`)
- Modify: `internal/accounts/dispatcher.go` (new `Random` option; apply at both 429 sites)
- Test: `internal/accounts/retry_test.go`

**Interfaces:**
- Consumes: the measured throttle window from Task 4, Step 4 — use it to set the top tier.
- Produces: `func Decorrelate(wait time.Duration, random func() float64) time.Duration`, `DispatcherOptions.Random func() float64`.

- [ ] **Step 1: Write the failing test**

Append to `internal/accounts/retry_test.go`:

```go
// A flat cooldown made the proxy re-probe a throttled model ~38 times across
// the 2026-09-17 wave. Each tier must strictly exceed the previous one.
func TestSmartBackoffRateLimitEscalates(t *testing.T) {
	previous := time.Duration(0)
	for failures := range len(rateLimitTiers) {
		wait := SmartBackoff(ReasonRateLimit, 0, failures)
		if wait <= previous {
			t.Fatalf("SmartBackoff(failures=%d) = %s, want more than the previous tier %s", failures, wait, previous)
		}
		previous = wait
	}
	capped := SmartBackoff(ReasonRateLimit, 0, len(rateLimitTiers)+5)
	if capped != rateLimitTiers[len(rateLimitTiers)-1] {
		t.Fatalf("SmartBackoff past the last tier = %s, want the cap %s", capped, rateLimitTiers[len(rateLimitTiers)-1])
	}
}

// The dispatcher fast-retries the SAME account when the computed wait is
// <= 10s. A first tier at or below that boundary would turn a rate limit into
// a tight retry loop on the account that was just rejected.
func TestSmartBackoffRateLimitFirstTierExceedsFastRetryWindow(t *testing.T) {
	if got := SmartBackoff(ReasonRateLimit, 0, 0); got <= 10*time.Second {
		t.Fatalf("first rate-limit tier = %s, want more than 10s", got)
	}
}

// Jitter must only add: shortening a server-specified reset would retry
// before upstream said we may.
func TestDecorrelateOnlyAdds(t *testing.T) {
	base := 30 * time.Second
	if got := Decorrelate(base, func() float64 { return 0 }); got != base {
		t.Fatalf("Decorrelate at random=0 = %s, want %s", got, base)
	}
	got := Decorrelate(base, func() float64 { return 1 })
	if got <= base {
		t.Fatalf("Decorrelate at random=1 = %s, want more than %s", got, base)
	}
	if got > base+base/4 {
		t.Fatalf("Decorrelate at random=1 = %s, want at most %s", got, base+base/4)
	}
}

func TestDecorrelateHandlesNilSourceAndZero(t *testing.T) {
	if got := Decorrelate(30*time.Second, nil); got != 30*time.Second {
		t.Fatalf("Decorrelate with a nil source = %s, want the input unchanged", got)
	}
	if got := Decorrelate(0, func() float64 { return 1 }); got != 0 {
		t.Fatalf("Decorrelate(0) = %s, want 0", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/accounts/ -run 'TestSmartBackoffRateLimit|TestDecorrelate' -v`
Expected: FAIL to build — `undefined: rateLimitTiers`, `undefined: Decorrelate`.

- [ ] **Step 3: Implement the ladder and the jitter**

In `internal/accounts/retry.go`, add above `SmartBackoff`:

```go
// rateLimitTiers escalates the guessed cooldown when Google returns a 429
// carrying no Retry-After and no quotaResetDelay. The old flat 30 s produced
// roughly 38 doomed upstream calls per account across the 19-minute wave of
// 2026-09-17, and on a sliding-window throttle every rejected call can extend
// the window. The first tier stays above 10 s deliberately: the dispatcher
// fast-retries the same account when the computed wait is <= 10 s.
var rateLimitTiers = []time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
}

// jitterFraction is how much Decorrelate may add, as a fraction of the wait.
const jitterFraction = 0.25

// Decorrelate spreads a cooldown by adding 0–25% of it. Two accounts rejected
// in the same second are otherwise marked for the same duration and wake in
// lockstep, re-entering the same throttle together. Jitter is additive only —
// subtracting could retry before a server-specified reset. A nil source
// returns the input unchanged so tests stay deterministic.
func Decorrelate(wait time.Duration, random func() float64) time.Duration {
	if wait <= 0 || random == nil {
		return wait
	}
	return wait + time.Duration(float64(wait)*jitterFraction*random())
}
```

Replace the `ReasonRateLimit` case in `SmartBackoff`:

```go
	case ReasonRateLimit:
		return rateLimitTiers[min(failures, len(rateLimitTiers)-1)]
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/accounts/ -run 'TestSmartBackoffRateLimit|TestDecorrelate' -v`
Expected: PASS, 4 tests.

- [ ] **Step 5: Wire the jitter source into the dispatcher**

In `internal/accounts/dispatcher.go`, add to `DispatcherOptions` (after `Forensics429`):

```go
	// Random supplies the jitter fraction for Decorrelate. Nil uses
	// math/rand/v2, which is safe for concurrent use. Tests inject a constant.
	Random func() float64
```

Add the field to the `Dispatcher` struct (after `forensics429`):

```go
	random func() float64
```

In `NewDispatcher`, before the struct literal:

```go
	if options.Random == nil {
		options.Random = rand.Float64
	}
```

and inside the literal:

```go
		random:                   options.Random,
```

Add `"math/rand/v2"` to the import block.

Apply the jitter at both 429 sites. In `StreamGenerateContent`, replace:

```go
				wait := SmartBackoff(reason, reset, failures)
```

with:

```go
				wait := Decorrelate(SmartBackoff(reason, reset, failures), dispatcher.random)
```

In `rotateForError`, replace:

```go
		wait := SmartBackoff(ClassifyError(body, upstreamError.StatusCode), ParseResetTime(upstreamError.Header, body, dispatcher.now()), dispatcher.manager.FailureCount(account))
```

with:

```go
		wait := Decorrelate(SmartBackoff(ClassifyError(body, upstreamError.StatusCode), ParseResetTime(upstreamError.Header, body, dispatcher.now()), dispatcher.manager.FailureCount(account)), dispatcher.random)
```

- [ ] **Step 6: Run the full package suite**

Run: `gofmt -w internal/accounts/ && go test ./internal/accounts/ -v`
Expected: PASS. Watch specifically for existing tests that assert a 30 s wait — jitter only adds, and the dispatcher's `wait <= 10*time.Second` fast-retry branch is unaffected because tier 0 is 30 s. Any test that asserts an exact wait must be updated to inject `Random: func() float64 { return 0 }` via `DispatcherOptions`, not to relax the assertion.

- [ ] **Step 7: Commit**

```bash
git add internal/accounts/retry.go internal/accounts/retry_test.go internal/accounts/dispatcher.go
git commit -m "fix(accounts): escalate and decorrelate the guessed rate-limit cooldown"
```

---

### Task 6: Stop rotating into a shared throttle

**Implements:** R6, fixing D3. When account A and account B are rejected on the same model within a second — the observed 2026-09-17 behaviour — the bucket is shared, and trying B after A is a guaranteed failure that costs a round trip, a failure count, and one more rejected request against the window. The pool must recognise the pattern and wait instead.

**Skip this task** if Task 4's verdict was `account-scoped`; in that case rotation is already correct. Record that decision in the commit message of Task 5 and move to Task 7.

**Files:**
- Modify: `internal/config/config.go` (replace the dead `RateLimitDedupWindowMs` with `SharedThrottleWindowMs`)
- Modify: `internal/accounts/manager.go` (`sharedThrottle`, `recordSharedThrottleLocked`, `sharedThrottleWaitLocked`, `SharedThrottleWait`, `SharedThrottles`; hooks in `Select`, `Available`, `MarkRateLimited`, `MarkSuccess`, `clearExpiredLocked`)
- Test: `internal/accounts/manager_test.go`

**Interfaces:**
- Consumes: `rateLimitModelKey(model) string`, `Selection{Account *Account; Wait time.Duration}`, `manager.now() time.Time` — all existing.
- Produces: `func (manager *Manager) SharedThrottleWait(model string) time.Duration` and `func (manager *Manager) SharedThrottles() map[string]time.Duration`, both consumed by Tasks 7 and 8.

Note on `RateLimitDedupWindowMs`: it is declared at `internal/config/config.go:118` and defaulted to 2000 at line 252, and **read nowhere in the codebase**. It is dead. Replacing it is safe — `encoding/json` ignores the old key in existing config files.

- [ ] **Step 1: Write the failing test**

Append to `internal/accounts/manager_test.go`:

```go
func sharedThrottleManager(t *testing.T, clock *time.Time, emails ...string) *Manager {
	t.Helper()
	pool := make([]*Account, 0, len(emails))
	for _, email := range emails {
		pool = append(pool, &Account{Email: email, Enabled: true})
	}
	manager, err := New(Options{
		Accounts:             pool,
		SharedThrottleWindow: 10 * time.Second,
		Now:                  func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// Two accounts rejected on the same model inside the window means one shared
// bucket. Selecting must then return no account and a wait, so the dispatcher
// waits instead of spending the rest of the pool on certain rejections.
func TestSharedThrottleBlocksSelectionAfterTwoAccounts(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	clock = clock.Add(time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	selection := manager.Select("gemini-3.8-flash-high")
	if selection.Account != nil {
		t.Fatalf("Select returned %s, want no account under a shared throttle", selection.Account.Email)
	}
	if selection.Wait <= 0 {
		t.Fatal("Select must report a wait under a shared throttle")
	}
	if got := manager.Available("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("Available = %d, want 0 under a shared throttle", got)
	}
}

// One account rejected is an ordinary per-account rate limit. Rotation to the
// other account must still happen.
func TestSharedThrottleNotSetForASingleAccount(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)

	selection := manager.Select("gemini-3.8-flash-high")
	if selection.Account == nil || selection.Account.Email != "b@example.com" {
		t.Fatal("one rejected account must still rotate to the other account")
	}
}

// Rejections far apart are two independent per-account limits, not one
// bucket. Treating them as shared would stall the pool on coincidence.
func TestSharedThrottleNotSetOutsideTheWindow(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	clock = clock.Add(11 * time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	if got := manager.SharedThrottleWait("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("SharedThrottleWait = %s, want 0 for rejections outside the window", got)
	}
}

// A success proves the bucket reopened; holding the pool-wide wait after that
// would idle a working model.
func TestMarkSuccessClearsTheSharedThrottle(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkSuccess(manager.GetAllAccounts()[0], "gemini-3.8-flash-high")

	if got := manager.SharedThrottleWait("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("SharedThrottleWait after a success = %s, want 0", got)
	}
}

// The wait must expire on its own even without a success.
func TestSharedThrottleExpires(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)
	clock = clock.Add(31 * time.Second)

	if got := manager.SharedThrottleWait("gemini-3.8-flash-high"); got != 0 {
		t.Fatalf("SharedThrottleWait after expiry = %s, want 0", got)
	}
	if manager.Select("gemini-3.8-flash-high").Account == nil {
		t.Fatal("Select must return an account once the shared throttle expires")
	}
}

// A shared throttle on one model must not stall a different model.
func TestSharedThrottleIsPerModel(t *testing.T) {
	clock := time.Date(2026, 9, 17, 17, 0, 0, 0, time.UTC)
	manager := sharedThrottleManager(t, &clock, "a@example.com", "b@example.com")

	manager.MarkRateLimited(manager.GetAllAccounts()[0], "gemini-3.8-flash-high", 30*time.Second)
	manager.MarkRateLimited(manager.GetAllAccounts()[1], "gemini-3.8-flash-high", 30*time.Second)

	if manager.Select("gemini-2.5-pro").Account == nil {
		t.Fatal("a shared throttle on one model must not block another model")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/accounts/ -run 'TestSharedThrottle|TestMarkSuccessClears' -v`
Expected: FAIL to build — `unknown field SharedThrottleWindow`, `undefined: manager.SharedThrottleWait`.

- [ ] **Step 3: Replace the dead config knob**

In `internal/config/config.go`, replace line 118:

```go
	RateLimitDedupWindowMs   int                       `json:"rateLimitDedupWindowMs,omitempty"`
```

with:

```go
	// SharedThrottleWindowMs is how close together two accounts must be
	// rejected on one model for the pool to treat the throttle as shared
	// rather than per-account. Replaces the never-read rateLimitDedupWindowMs.
	SharedThrottleWindowMs   int                       `json:"sharedThrottleWindowMs,omitempty"`
```

and in the defaults block, replace `RateLimitDedupWindowMs: 2000,` with:

```go
		SharedThrottleWindowMs: 10000,
```

- [ ] **Step 4: Implement shared-throttle tracking in the manager**

In `internal/accounts/manager.go`, add the `SharedThrottleWindow` field to `Options`:

```go
type Options struct {
	Accounts             []*Account
	ActiveIndex          int
	Strategy             string
	ConfigPath           string
	Settings             map[string]any
	SelectionConfig      config.AccountSelectionConfig
	GlobalQuotaThreshold float64
	SharedThrottleWindow time.Duration
	Now                  func() time.Time
}
```

Add the type and the manager field. Place the type near `Selection`:

```go
// sharedThrottle tracks one model's pool-wide rejection window. Both pool
// accounts resolve to the same Google-assigned project (aicode-consumers), so
// a project- or model-scoped throttle rejects every account within about a
// second. Once two distinct accounts are rejected inside the window, trying
// the rest of the pool is a guaranteed failure that only feeds the throttle.
type sharedThrottle struct {
	untilMS int64
	recent  map[string]int64 // account email -> last rejection, unix ms
}
```

Add to the `Manager` struct, alongside the other state fields:

```go
	sharedThrottleWindow time.Duration
	modelThrottles       map[string]*sharedThrottle
```

In `New`, after the `globalQuotaThreshold` block, resolve the window and set both fields on the constructed manager:

```go
	sharedThrottleWindow := options.SharedThrottleWindow
	if sharedThrottleWindow <= 0 {
		if ms := config.Get().SharedThrottleWindowMs; ms > 0 {
			sharedThrottleWindow = time.Duration(ms) * time.Millisecond
		} else {
			sharedThrottleWindow = 10 * time.Second
		}
	}
```

and in the returned `Manager` literal add:

```go
		sharedThrottleWindow: sharedThrottleWindow,
		modelThrottles:       make(map[string]*sharedThrottle),
```

Add the tracking functions next to `MinWait`:

```go
// recordSharedThrottleLocked notes one rejection and promotes the model to a
// pool-wide throttle once two distinct accounts are rejected inside the
// window. A single-account pool can never be "shared" — there is nothing to
// rotate to — so it is skipped.
func (manager *Manager) recordSharedThrottleLocked(key, email string, wait time.Duration) {
	if key == "" || len(manager.accounts) < 2 {
		return
	}
	if manager.modelThrottles == nil {
		manager.modelThrottles = make(map[string]*sharedThrottle)
	}
	nowMS := manager.now().UnixMilli()
	entry := manager.modelThrottles[key]
	if entry == nil {
		entry = &sharedThrottle{recent: make(map[string]int64)}
		manager.modelThrottles[key] = entry
	}
	entry.recent[email] = nowMS

	cutoff := nowMS - manager.sharedThrottleWindow.Milliseconds()
	for recorded, at := range entry.recent {
		if at < cutoff {
			delete(entry.recent, recorded)
		}
	}
	if len(entry.recent) < 2 {
		return
	}
	if until := nowMS + wait.Milliseconds(); until > entry.untilMS {
		entry.untilMS = until
		slog.Warn("shared upstream throttle",
			"model", key, "accounts", len(entry.recent), "wait", wait.Round(time.Second))
	}
}

// SharedThrottleWait returns how long the whole pool must wait on a model, or
// 0 when no shared throttle is active.
func (manager *Manager) SharedThrottleWait(model string) time.Duration {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.sharedThrottleWaitLocked(rateLimitModelKey(model))
}

func (manager *Manager) sharedThrottleWaitLocked(key string) time.Duration {
	entry := manager.modelThrottles[key]
	if entry == nil {
		return 0
	}
	remaining := entry.untilMS - manager.now().UnixMilli()
	if remaining <= 0 {
		return 0
	}
	return time.Duration(remaining) * time.Millisecond
}

// SharedThrottles returns every model under an active pool-wide throttle and
// how long each has left. Used by the status output.
func (manager *Manager) SharedThrottles() map[string]time.Duration {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	active := make(map[string]time.Duration)
	for key := range manager.modelThrottles {
		if wait := manager.sharedThrottleWaitLocked(key); wait > 0 {
			active[key] = wait
		}
	}
	return active
}
```

Hook it into `MarkRateLimited`, immediately after the `account.ModelRateLimits[key] = ...` assignment and before `account.ConsecutiveFailure++`:

```go
	manager.recordSharedThrottleLocked(key, account.Email, wait)
```

Gate selection. In `Select`, after `manager.clearExpiredLocked()` and before the strategy switch:

```go
	if wait := manager.sharedThrottleWaitLocked(rateLimitModelKey(model)); wait > 0 {
		return Selection{Wait: wait}
	}
```

In `Available`, after `manager.clearExpiredLocked()`:

```go
	if manager.sharedThrottleWaitLocked(rateLimitModelKey(model)) > 0 {
		return 0
	}
```

Clear on success. In `MarkSuccess`, inside the existing `if key := rateLimitModelKey(model); ...` block, after the `delete(account.ModelRateLimits, key)` line:

```go
		delete(manager.modelThrottles, key)
```

Prune expired entries. In `clearExpiredLocked`, after the per-account loop:

```go
	for key, entry := range manager.modelThrottles {
		if entry == nil || entry.untilMS <= now {
			delete(manager.modelThrottles, key)
		}
	}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `gofmt -w internal/accounts/ internal/config/ && go test ./internal/accounts/ -run 'TestSharedThrottle|TestMarkSuccessClears' -v`
Expected: PASS, 6 tests.

- [ ] **Step 6: Run the full suite**

Run: `make test`
Expected: PASS. The dispatcher needs no change: `StreamGenerateContent` already handles `Selection{Account: nil, Wait: w}` by sleeping `w+500ms` and retrying without consuming an attempt (`internal/accounts/dispatcher.go:284-304`).

Deliberate side effect to verify here: `internal/api/server.go:1002` reads `server.accountManager.Available(model) == 0` as `noCapacity`. Making `Available` return 0 under a shared throttle therefore also routes requests down that no-capacity path. That is the intended behaviour — the pool genuinely has no capacity for the model — but confirm the tests covering that path still pass and that the path does not itself re-probe the throttled model.

- [ ] **Step 7: Commit**

```bash
git add internal/accounts/manager.go internal/accounts/manager_test.go internal/config/config.go
git commit -m "fix(accounts): detect a pool-wide throttle and wait instead of burning accounts"
```

---

### Task 7: Answer the client with 429 and Retry-After

**Implements:** R7, fixing D4. An upstream 429 is mapped to HTTP **400 `invalid_request_error`** (`internal/api/server.go:3161`) with no `Retry-After`, and Claude Code treats 400 as permanent and never retries. Pool exhaustion returns a plain error that falls through to 500. Both must become 429 with retry guidance.

**Files:**
- Modify: `internal/accounts/retry.go` (new `RateLimitError`)
- Modify: `internal/accounts/dispatcher.go` (return it on pool exhaustion)
- Modify: `internal/api/server.go` (`classifyError`, `writeError`, new `retryAfterSeconds`)
- Test: `internal/api/server_test.go`, `internal/accounts/dispatcher_test.go`

**Interfaces:**
- Consumes: `manager.SharedThrottleWait` (Task 6). If Task 6 was skipped, set `Shared: false` and drop that call.
- Produces: `accounts.RateLimitError{Model string; RetryAfter time.Duration; Shared bool}`.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/server_test.go`:

```go
// Claude Code treats 400 as permanently invalid and never retries it. An
// exhausted pool is temporary, so it must answer 429 with Retry-After.
func TestClassifyErrorMapsPoolExhaustionTo429(t *testing.T) {
	err := &accounts.RateLimitError{Model: "gemini-3.8-flash-high", RetryAfter: 42 * time.Second, Shared: true}
	status, kind, _ := classifyError(err)
	if status != http.StatusTooManyRequests {
		t.Fatalf("classifyError status = %d, want 429", status)
	}
	if kind != "rate_limit_error" {
		t.Fatalf("classifyError kind = %q, want rate_limit_error", kind)
	}
}

func TestClassifyErrorMapsUpstream429To429(t *testing.T) {
	err := &cloudcode.HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429", Body: "RESOURCE_EXHAUSTED"}
	status, kind, _ := classifyError(err)
	if status != http.StatusTooManyRequests {
		t.Fatalf("classifyError status = %d, want 429", status)
	}
	if kind != "rate_limit_error" {
		t.Fatalf("classifyError kind = %q, want rate_limit_error", kind)
	}
}

// Retry-After is rounded up: rounding down would invite a retry before the
// pool is ready, which re-enters the throttle.
func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	err := &accounts.RateLimitError{Model: "m", RetryAfter: 1500 * time.Millisecond}
	if got := retryAfterSeconds(err); got != 2 {
		t.Fatalf("retryAfterSeconds = %d, want 2", got)
	}
}

func TestRetryAfterSecondsReadsUpstreamHeader(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "17")
	err := &cloudcode.HTTPError{StatusCode: http.StatusTooManyRequests, Header: header}
	if got := retryAfterSeconds(err); got != 17 {
		t.Fatalf("retryAfterSeconds = %d, want 17", got)
	}
}

func TestRetryAfterSecondsIsZeroForOtherErrors(t *testing.T) {
	if got := retryAfterSeconds(errors.New("boom")); got != 0 {
		t.Fatalf("retryAfterSeconds = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run 'TestClassifyErrorMaps|TestRetryAfterSeconds' -v`
Expected: FAIL to build — `undefined: accounts.RateLimitError`, `undefined: retryAfterSeconds`.

- [ ] **Step 3: Add the typed error**

In `internal/accounts/retry.go`, add `"fmt"` to the import block and append:

```go
// RateLimitError reports that the pool cannot serve a model right now and
// cannot wait the throttle out inside maxWait. It carries the reset so the
// API layer can answer 429 + Retry-After: a bare 400 tells Claude Code the
// request is permanently invalid and it never retries.
type RateLimitError struct {
	Model      string
	RetryAfter time.Duration
	Shared     bool
}

func (err *RateLimitError) Error() string {
	scope := "account"
	if err.Shared {
		scope = "pool-wide"
	}
	return fmt.Sprintf("RESOURCE_EXHAUSTED: %s rate limit on %s; retry after %s",
		scope, err.Model, err.RetryAfter.Round(time.Second))
}
```

In `internal/accounts/dispatcher.go`, replace the exhaustion return in `StreamGenerateContent`:

```go
			if wait > dispatcher.maxWait {
				return cloudcode.Response{}, fmt.Errorf("RESOURCE_EXHAUSTED: rate limited on %s; quota resets after %s", model, wait.Round(time.Second))
			}
```

with:

```go
			if wait > dispatcher.maxWait {
				return cloudcode.Response{}, &RateLimitError{
					Model:      model,
					RetryAfter: wait,
					Shared:     dispatcher.manager.SharedThrottleWait(model) > 0,
				}
			}
```

- [ ] **Step 4: Map it in the API layer**

In `internal/api/server.go`, add to `classifyError`, immediately after the `*modelcatalog.SelectionError` block:

```go
	var rateLimitError *accounts.RateLimitError
	if errors.As(err, &rateLimitError) {
		return http.StatusTooManyRequests, "rate_limit_error", rateLimitError.Error()
	}
```

Replace the upstream 429 case:

```go
		case http.StatusTooManyRequests:
			return http.StatusBadRequest, "invalid_request_error", "RESOURCE_EXHAUSTED: capacity is exhausted for this model. Please wait for quota to reset."
```

with:

```go
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "rate_limit_error", "RESOURCE_EXHAUSTED: the upstream throttled this model. Retry after the interval in Retry-After."
```

Set the header in `writeError`, replacing its last two lines:

```go
	status, kind, message := classifyError(err)
	if seconds := retryAfterSeconds(err); seconds > 0 {
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	writeAPIError(writer, status, kind, message)
```

and add, next to `classifyError`:

```go
// retryAfterSeconds returns the Retry-After value to advertise, rounded UP:
// rounding down invites a retry before the pool is ready, which re-enters the
// same throttle. 0 means the response carries no Retry-After.
func retryAfterSeconds(err error) int {
	var rateLimitError *accounts.RateLimitError
	if errors.As(err, &rateLimitError) && rateLimitError.RetryAfter > 0 {
		return ceilSeconds(rateLimitError.RetryAfter)
	}
	var upstreamError *cloudcode.HTTPError
	if errors.As(err, &upstreamError) && upstreamError.StatusCode == http.StatusTooManyRequests {
		if wait := accounts.ParseResetTime(upstreamError.Header, upstreamError.Body, time.Now()); wait > 0 {
			return ceilSeconds(wait)
		}
	}
	return 0
}

func ceilSeconds(value time.Duration) int {
	return int((value + time.Second - 1) / time.Second)
}
```

`accounts`, `cloudcode`, `errors`, `strconv`, `time` and `net/http` are all already imported in this file.

- [ ] **Step 5: Run the test to verify it passes**

Run: `gofmt -w internal/api/ internal/accounts/ && go test ./internal/api/ -run 'TestClassifyErrorMaps|TestRetryAfterSeconds' -v`
Expected: PASS, 5 tests.

- [ ] **Step 6: Run the full suite**

Run: `make test`
Expected: PASS. Any existing test asserting 400 for an upstream 429 must be updated to 429 — that mapping is the defect being fixed, so change the assertion, not the code.

- [ ] **Step 7: Commit**

```bash
git add internal/accounts/retry.go internal/accounts/dispatcher.go internal/api/server.go internal/api/server_test.go
git commit -m "fix(api): answer throttled requests with 429 and Retry-After instead of 400"
```

---

### Task 8: Show the shared throttle in the status output

**Implements:** R8. The per-account rate-limit count in the status table cannot show a pool-wide throttle — every account looks individually limited, which is exactly the misreading that sent this investigation toward daily quota in the first place.

**Skip this task** if Task 6 was skipped.

**Files:**
- Modify: `internal/api/management.go` (the text status table, around line 280-315)
- Test: `internal/api/management_test.go`
- Modify: `docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md` (operating notes)

**Interfaces:**
- Consumes: `manager.SharedThrottles() map[string]time.Duration` (Task 6).
- Produces: nothing consumed downstream.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/management_test.go`:

```go
// A pool-wide throttle looks identical to two independent per-account limits
// in the per-account table. Naming it is the whole point of the line.
func TestStatusTextShowsSharedThrottle(t *testing.T) {
	line := formatSharedThrottleLine(map[string]time.Duration{
		"gemini-3.8-flash-high": 252 * time.Second,
	})
	if !strings.Contains(line, "gemini-3.8-flash-high") {
		t.Fatalf("shared throttle line = %q, want the model name", line)
	}
	if !strings.Contains(line, "4m12s") {
		t.Fatalf("shared throttle line = %q, want the remaining wait", line)
	}
}

func TestStatusTextOmitsTheLineWhenNoThrottleIsActive(t *testing.T) {
	if got := formatSharedThrottleLine(map[string]time.Duration{}); got != "" {
		t.Fatalf("shared throttle line = %q, want empty when nothing is throttled", got)
	}
}

// Deterministic ordering keeps the output diffable between polls.
func TestStatusTextSortsThrottledModels(t *testing.T) {
	line := formatSharedThrottleLine(map[string]time.Duration{
		"zeta-model":  time.Minute,
		"alpha-model": time.Minute,
	})
	if strings.Index(line, "alpha-model") > strings.Index(line, "zeta-model") {
		t.Fatalf("shared throttle line = %q, want models in sorted order", line)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestStatusText -v`
Expected: FAIL to build — `undefined: formatSharedThrottleLine`.

- [ ] **Step 3: Implement the status line**

In `internal/api/management.go`, add:

```go
// formatSharedThrottleLine renders the pool-wide throttles above the
// per-account table. Without it a shared throttle is indistinguishable from
// two independent per-account rate limits, which is the misreading that sent
// the 2026-09-17 investigation toward daily quota.
func formatSharedThrottleLine(throttles map[string]time.Duration) string {
	if len(throttles) == 0 {
		return ""
	}
	models := make([]string, 0, len(throttles))
	for model := range throttles {
		models = append(models, model)
	}
	sort.Strings(models)
	parts := make([]string, 0, len(models))
	for _, model := range models {
		parts = append(parts, fmt.Sprintf("%s (%s left)", model, throttles[model].Round(time.Second)))
	}
	return "SHARED THROTTLE: " + strings.Join(parts, ", ")
}
```

`fmt`, `sort`, `strings` and `time` are all already imported in this file. In the text-status handler, immediately before `fmt.Fprintln(w, "EMAIL\tSTATUS\t...")`:

```go
		if server.accountManager != nil {
			if line := formatSharedThrottleLine(server.accountManager.SharedThrottles()); line != "" {
				fmt.Fprintln(&buf, line)
				fmt.Fprintln(&buf)
			}
		}
```

The nil guard matches the existing style at `internal/api/server.go:1002`, where `accountManager` is checked before use.

Note the write goes to `&buf`, not `w`: the tabwriter would align the line into the table's columns.

- [ ] **Step 4: Run the test to verify it passes**

Run: `gofmt -w internal/api/ && go test ./internal/api/ -run TestStatusText -v`
Expected: PASS, 3 tests.

- [ ] **Step 5: Document the operating knobs**

Append a `## 7. Operating notes` section to `docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md`:

```markdown
## 7. Operating notes

| Config key | Default | Effect |
|------------|---------|--------|
| `upstream429ForensicsEnabled` | `false` | Appends every upstream 429 verbatim to `<configDir>/forensics/upstream-429.jsonl`. Turn on before a suspected wave. |
| `sharedThrottleWindowMs` | `10000` | How close together two accounts must be rejected on one model for the pool to treat the throttle as shared and wait instead of rotating. Replaces the never-read `rateLimitDedupWindowMs`. |
| `maxWaitBeforeErrorMs` | `120000` | Above this wait the client gets HTTP 429 + `Retry-After` instead of the proxy blocking. |

Diagnostics:

- `go run ./cmd/debug429` — replay one minimal request per account, print status, rate-limit headers, full body.
- `go run ./cmd/probe429 -burst 8 -window 20m` — induce a throttle, name the dimension, measure the window.
- `scripts/capture-agy-headers.sh` — capture live agy requests and responses through mitmproxy.

`antigravity-proxy status` prints a `SHARED THROTTLE:` line above the account
table whenever the pool is waiting out a model-wide rejection.
```

- [ ] **Step 6: Run the full gate**

Run: `make test`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/management.go internal/api/management_test.go docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md
git commit -m "feat(api): surface the active pool-wide throttle in the status output"
```

---

## Verification

After Task 8, verify end to end:

1. `make test` — the whole suite passes.
2. `gofmt -l ./cmd ./internal ./scripts` — prints nothing.
3. `python3 -m unittest discover -s scripts -p 'test_*.py'` — passes.
4. `grep -ric "bearer \|ya29\.\|refresh_token" .reference/*.jsonl ~/.config/antigravity-proxy/forensics/*.jsonl` — every count is 0.
5. With forensics on, run `go run ./cmd/probe429 -burst 8` against the live proxy and confirm: the proxy logs `shared upstream throttle`, the client receives HTTP 429 with a `Retry-After` header, and `antigravity-proxy status` shows the `SHARED THROTTLE:` line.
