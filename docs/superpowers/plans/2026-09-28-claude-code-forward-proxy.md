# Claude Code Forward Proxy (Observe-Only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional, loopback-only, observe-only forward proxy (`internal/mitm`) that lets the WebUI list Claude Code cloud sessions, whose control traffic bypasses `ANTHROPIC_BASE_URL`.

**Architecture:** A second listener accepts `CONNECT`. Hosts under `anthropic.com`, `claude.ai`, `claude.com` are terminated with a local name-constrained CA and relayed as raw HTTP/1.1 (byte-exact heads, framing-aware bodies) to a standard Go TLS upstream; every other host is a blind byte tunnel. An observer turns exchanges into masked summaries in a capped registry, exposed read-only through the management API and a WebUI tab.

**Tech Stack:** Go 1.27rc2, standard library only for `internal/mitm`; Alpine.js WebUI (no build step); `httptest`-style fakes, `-race`.

**Spec:** `docs/superpowers/specs/2026-09-28-claude-code-forward-proxy-design.md` (design), with the deviations listed below. Background: `docs/superpowers/specs/2026-09-28-claude-code-cloud-feasibility.md` §9 Q1.

## Global Constraints

Copied from the spec; every task's requirements include these.

- Observe-only: no request or response is rewritten. No model aliasing, no keepalive rewriting.
- Loopback only: the listener binds `127.0.0.1` (or `::1`/`localhost`); any other `listen` is rejected at config save, at startup and in `StartRuntime`.
- Off by default: `mitm.enabled: false`; existing configs need no migration (there is no config version field).
- CA: ECDSA P-256 under `<configdir>/mitm/`, key file mode 0600, name-constrained (critical) to `anthropic.com`, `claude.ai`, `claude.com`. Leaf certs valid 24 h, cached in memory. Only the certificate is ever exported. Never install the CA in a system trust store.
- Blind tunnel for every host outside the permitted domains (and for permitted hosts on ports other than 443).
- Upstream: empty `tls.Config{}` (only `ServerName` is filled), HTTP/1.1 only (no ALPN offered), no total stream timeout.
- Server-side TLS toward the CLI offers ALPN `http/1.1` only.
- Header names and order are forwarded exactly as the client sent them (raw head relay); bodies pass through unmodified.
- Limits: 64 KB message head cap, 2 min keep-alive idle timeout, registry default 1000 entries and 24 h TTL.
- The observer receives a parsed summary only (masked route, status, enum-like fields), never headers or bodies. Field values are kept only if they match `^[A-Za-z0-9_.:/-]{1,64}$`. Session ids are stored as a 12-character sha256 prefix. Titles, prompts and raw ids are never stored or logged.
- Unknown routes are forwarded untouched; an observer panic or parse error never affects forwarding.
- Never touch `internal/cloudcode` (AGENTS.md: agy fingerprint gate). Never commit credentials.
- Run gofmt before committing (`make install-hooks` enables the repo hook).

## Deviations from the spec (decided while verifying this plan)

1. **No config-save special case for `mitm`.** The spec (section 5) copied the `modelMapping` special case, but every `mitm` field is a scalar, so the generic merge is correct and a partial `{"mitm":{"enabled":true}}` keeps the other fields (covered by `TestMitmConfigPartialJSONKeepsDefaults` and `TestConfigSaveRejectsNonLoopbackMitmListen`). The spec text is amended in Task 9.
2. **Byte-exact relay instead of `http.Transport`.** `net/http` writes headers sorted and canonicalized, which would break the spec's "header case and order preserved" claim, so the relay reads and forwards raw message heads (`httpwire.go`) and only parses them for framing.
3. **`Expect: 100-continue` is refused with 417**, and **Upgrade/101 is spliced** both ways, recording the route only. Both were left open in the spec.
4. **WebUI has an enable toggle** on the Cloud tab (writes `mitm.enabled`, applies on restart), and polls every 5 s.
5. **`internal/mitm` owns the loopback check** (`ValidateListen`); `internal/config` imports it.
6. **Brotli decoding in the observer** (`github.com/andybalholm/brotli`, already in the module cache). Found during the Step 5 acceptance run: the CLI sends `Accept-Encoding: br` and Anthropic answers `Content-Encoding: br`, so `summarizeBody` saw 534 compressed bytes and extracted nothing. The wire is still forwarded byte-exact; only the observer decode handles gzip and br. This breaks the plan's "standard library only for `internal/mitm`" line, which is amended here.

## File Structure

| File | Responsibility |
|---|---|
| `internal/mitm/ca.go` | Name-constrained CA: load/create, leaf minting, fingerprint, `HostPermitted` |
| `internal/mitm/httpwire.go` | Raw HTTP/1.1 head reading, framing decisions, body copy (fixed, chunked, until-close), capped capture buffer |
| `internal/mitm/observe.go` | Route classification and response-body summarizing (`Observation`) |
| `internal/mitm/registry.go` | Capped, TTL-bound session registry keyed by hashed id |
| `internal/mitm/server.go` | `CONNECT` listener, blind tunnel, TLS termination, relay loop, stats |
| `internal/mitm/runtime.go` | `ValidateListen`, `StartRuntime` bundling CA + registry + server |
| `internal/config/mitm.go` | `MitmConfig`, defaults, validation |
| `internal/api/mitm_management.go` | Read-only management handlers |
| `cmd/proxy/main.go`, `internal/api/server.go`, `internal/api/management.go`, `internal/config/config.go` | Wiring (small diffs) |
| `internal/webui/public/js/components/cloud-sessions.js`, `views/settings.html`, `store.js`, `index.html`, translations | "Cloud" settings tab |
| `docs/claude-code-forward-proxy.md`, `README.md` | User guide |

Every Go test below was run with `go test -race -count=3 ./internal/mitm/` against exactly this code before the plan was written (36 tests in `internal/mitm`, all passing). A mutation check (buffering the response body) made `TestSSEStreamsIncrementally` fail, confirming that test can fail.

Known pre-existing failures unrelated to this work: `go test ./internal/auth/` fails in environments without the `agy` binary (`TestGetAuthorizationURL`, `TestOAuthManager_HTTPHandler`). Run all other packages.

---

### Task 1: Name-constrained CA (`ca.go`)

**Files:**
- Create: `internal/mitm/ca.go`
- Create: `internal/mitm/ca_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces (used by Tasks 4, 5, 7): `PermittedDomains []string`, `ErrHostNotPermitted error`, `HostPermitted(host string) bool`, `type CA`, `LoadOrCreateCA(dir string, now func() time.Time) (*CA, error)`, `(*CA).CertPEM() []byte`, `(*CA).Fingerprint() string`, `(*CA).NotAfter() time.Time`, `(*CA).LeafFor(host string) (*tls.Certificate, error)`, unexported `randomSerial()`, `(*CA).cert`, `(*CA).key`, and the test helper `newTestCA(t) *CA` (defined in `ca_test.go`, reused by later tests).

- [ ] **Step 1: Write the failing test**

Create `internal/mitm/ca_test.go`:

```go
package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(filepath.Join(t.TempDir(), "mitm"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestLeafVerifiesForPermittedHost(t *testing.T) {
	ca := newTestCA(t)
	leaf, err := ca.LeafFor("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "api.anthropic.com"}); err != nil {
		t.Fatalf("leaf should verify: %v", err)
	}
}

func TestLeafRefusedOutsideConstraint(t *testing.T) {
	ca := newTestCA(t)
	for _, host := range []string{"evil.com", "anthropic.com.evil.com", "notanthropic.com", "example.org"} {
		if _, err := ca.LeafFor(host); !errors.Is(err, ErrHostNotPermitted) {
			t.Errorf("LeafFor(%q) err = %v, want ErrHostNotPermitted", host, err)
		}
	}
}

// The constraint must be enforced by the certificate itself, not only by
// LeafFor: a leaf hand-signed with the CA key for another domain must fail
// verification.
func TestCAConstraintEmbeddedInCertificate(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := randomSerial()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "evil.com"}, DNSNames: []string{"evil.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	evil, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := evil.Verify(x509.VerifyOptions{Roots: pool, DNSName: "evil.com"}); err == nil {
		t.Fatal("hand-signed leaf for evil.com verified; name constraint is not enforced")
	}
}

func TestCAPersistsAndKeyIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mitm")
	first, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("reload produced a different CA")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, caKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("key mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestCARegeneratedWhenNearExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mitm")
	past := func() time.Time { return time.Now().Add(-364 * 24 * time.Hour) }
	old, err := LoadOrCreateCA(dir, past)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if old.Fingerprint() == fresh.Fingerprint() {
		t.Fatal("near-expiry CA was not regenerated")
	}
}

func TestHostPermitted(t *testing.T) {
	cases := map[string]bool{
		"api.anthropic.com": true, "anthropic.com": true, "claude.ai": true, "x.claude.com": true,
		"API.Anthropic.COM.": true, "evil.com": false, "anthropic.com.evil.com": false, "xclaude.ai": false, "": false,
	}
	for host, want := range cases {
		if got := HostPermitted(host); got != want {
			t.Errorf("HostPermitted(%q) = %v, want %v", host, got, want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mitm/`
Expected: FAIL, build errors such as `undefined: LoadOrCreateCA`, `undefined: ErrHostNotPermitted`, `undefined: HostPermitted`.

- [ ] **Step 3: Write the implementation**

Create `internal/mitm/ca.go`:

```go
// Package mitm is an observe-only forward proxy for Claude Code cloud-session
// traffic. It terminates TLS for a small set of Anthropic-owned hosts with a
// locally generated, name-constrained CA, relays HTTP/1.1 byte for byte, and
// reports masked route summaries to a registry. It never rewrites traffic.
package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PermittedDomains are the only domains the CA may issue for. The same list
// is embedded in the CA certificate as a critical name constraint.
var PermittedDomains = []string{"anthropic.com", "claude.ai", "claude.com"}

// ErrHostNotPermitted is returned when a leaf is requested for a host outside
// PermittedDomains.
var ErrHostNotPermitted = errors.New("mitm: host is not in the permitted domain set")

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"
	caLifetime = 365 * 24 * time.Hour
	// A CA with less than this much life left is regenerated on load.
	caRenewWindow = 7 * 24 * time.Hour
	leafLifetime  = 24 * time.Hour
)

// HostPermitted reports whether host equals, or is a subdomain of, a
// permitted domain.
func HostPermitted(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range PermittedDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// CA is a local certificate authority. Only its certificate is ever exported.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	now     func() time.Time

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// LoadOrCreateCA loads the CA from dir, creating it (dir 0700, key 0600) when
// either file is missing, unreadable as PEM, or close to expiry.
func LoadOrCreateCA(dir string, now func() time.Time) (*CA, error) {
	if now == nil {
		now = time.Now
	}
	if ca, err := loadCA(dir, now); err == nil {
		return ca, nil
	}
	return createCA(dir, now)
}

func loadCA(dir string, now func() time.Time) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, errors.New("mitm: CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	if now().Add(caRenewWindow).After(cert.NotAfter) {
		return nil, errors.New("mitm: CA is close to expiry")
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, now: now, leaves: map[string]*tls.Certificate{}}, nil
}

func createCA(dir string, now func() time.Time) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mitm: create CA dir: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "antigravity-proxy local MITM CA"},
		NotBefore:                   now().Add(-time.Hour),
		NotAfter:                    now().Add(caLifetime),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         PermittedDomains,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("mitm: write CA key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, caCertFile), certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("mitm: write CA cert: %w", err)
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, now: now, leaves: map[string]*tls.Certificate{}}, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// CertPEM returns the CA certificate in PEM form. The key is never exposed.
func (ca *CA) CertPEM() []byte { return append([]byte(nil), ca.certPEM...) }

// Fingerprint is the hex SHA-256 of the CA certificate.
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// NotAfter is the CA certificate expiry.
func (ca *CA) NotAfter() time.Time { return ca.cert.NotAfter }

// LeafFor returns a cached or freshly minted TLS certificate for host.
func (ca *CA) LeafFor(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !HostPermitted(host) {
		return nil, fmt.Errorf("%w: %q", ErrHostNotPermitted, host)
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if leaf, ok := ca.leaves[host]; ok && leaf.Leaf != nil && ca.now().Add(time.Hour).Before(leaf.Leaf.NotAfter) {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    ca.now().Add(-time.Hour),
		NotAfter:     ca.now().Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}
	ca.leaves[host] = leaf
	return leaf, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./internal/mitm/ && go test -race -count=1 ./internal/mitm/`
Expected: `ok`. `TestCAConstraintEmbeddedInCertificate` proves the constraint lives in the certificate itself, not only in `LeafFor`.

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/ca.go internal/mitm/ca_test.go
git commit -m "feat(mitm): add name-constrained local CA for the forward proxy"
```

### Task 2: Raw HTTP/1.1 wire helpers (`httpwire.go`)

**Files:**
- Create: `internal/mitm/httpwire.go`
- Create: `internal/mitm/httpwire_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces (used by Tasks 3-4): `maxHeadBytes`, `errHeadTooLarge`, `errBadFraming`, `type framing` (`framingNone`, `framingLength`, `framingChunked`, `framingUntilClose`), `type msgHead {Raw []byte; Header http.Header; Method, Target string; Status int; Proto string; Framing framing; ContentLength int64; Close, Upgrade, Expect100 bool}`, `readHead(*bufio.Reader) ([]byte, error)`, `parseRequestHead([]byte) (*msgHead, error)`, `parseResponseHead(raw []byte, requestMethod string) (*msgHead, error)`, `copyBody(dst io.Writer, br *bufio.Reader, f framing, n int64, sink io.Writer) error`, `type capBuffer {limit int; ...}` with `Write` and `Bytes() []byte` (nil after overflow).

Why this exists: `net/http` canonicalizes header names (`X-Stainless-OS` becomes `X-Stainless-Os`) and writes headers sorted, so it cannot relay the CLI's requests unchanged. Heads are read and forwarded raw and parsed only to learn message framing. A request with both `Transfer-Encoding` and `Content-Length`, or conflicting `Content-Length` values, is rejected (request-smuggling shape).

- [ ] **Step 1: Write the failing test**

Create `internal/mitm/httpwire_test.go`:

```go
package mitm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func br(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func TestReadHeadReturnsExactBytes(t *testing.T) {
	in := "POST /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nX-Stainless-OS: MacOS\r\nx-app: cli\r\nContent-Length: 2\r\n\r\nhi"
	reader := br(in)
	head, err := readHead(reader)
	if err != nil {
		t.Fatal(err)
	}
	want := in[:strings.Index(in, "hi")]
	if string(head) != want {
		t.Fatalf("head = %q, want %q", head, want)
	}
	rest, _ := io.ReadAll(reader)
	if string(rest) != "hi" {
		t.Fatalf("body remainder = %q", rest)
	}
}

func TestReadHeadRejectsOversizedHead(t *testing.T) {
	in := "GET / HTTP/1.1\r\nX: " + strings.Repeat("a", maxHeadBytes+10) + "\r\n\r\n"
	if _, err := readHead(br(in)); !errors.Is(err, errHeadTooLarge) {
		t.Fatalf("err = %v, want errHeadTooLarge", err)
	}
}

func TestRequestFraming(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want framing
		n    int64
		err  bool
	}{
		{"none", "GET / HTTP/1.1\r\nHost: a\r\n\r\n", framingNone, 0, false},
		{"length", "POST / HTTP/1.1\r\nContent-Length: 5\r\n\r\n", framingLength, 5, false},
		{"chunked", "POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n", framingChunked, 0, false},
		{"both is smuggling", "POST / HTTP/1.1\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n", 0, 0, true},
		{"conflicting lengths", "POST / HTTP/1.1\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n", 0, 0, true},
		{"repeated same length ok", "POST / HTTP/1.1\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\n", framingLength, 5, false},
		{"bad length", "POST / HTTP/1.1\r\nContent-Length: x\r\n\r\n", 0, 0, true},
	}
	for _, c := range cases {
		head, err := parseRequestHead([]byte(c.raw))
		if c.err {
			if err == nil {
				t.Errorf("%s: expected error", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if head.Framing != c.want || head.ContentLength != c.n {
			t.Errorf("%s: framing=%v n=%d", c.name, head.Framing, head.ContentLength)
		}
	}
}

func TestResponseFraming(t *testing.T) {
	head, err := parseResponseHead([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"), http.MethodGet)
	if err != nil || head.Framing != framingUntilClose {
		t.Fatalf("no length response: %v %v", head, err)
	}
	head, _ = parseResponseHead([]byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n"), http.MethodHead)
	if head.Framing != framingNone {
		t.Fatal("HEAD response must have no body")
	}
	head, _ = parseResponseHead([]byte("HTTP/1.1 204 No Content\r\n\r\n"), http.MethodGet)
	if head.Framing != framingNone {
		t.Fatal("204 must have no body")
	}
}

func TestConnectionFlags(t *testing.T) {
	head, _ := parseRequestHead([]byte("GET / HTTP/1.1\r\nConnection: close\r\n\r\n"))
	if !head.Close {
		t.Error("Connection: close not detected")
	}
	head, _ = parseRequestHead([]byte("GET / HTTP/1.0\r\n\r\n"))
	if !head.Close {
		t.Error("HTTP/1.0 without keep-alive should close")
	}
	head, _ = parseRequestHead([]byte("GET /x HTTP/1.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"))
	if !head.Upgrade {
		t.Error("upgrade not detected")
	}
}

func TestCopyChunkedIsByteExactAndSinkGetsPayload(t *testing.T) {
	wire := "5;ext=1\r\nhello\r\n6\r\n world\r\n0\r\nX-Trailer: t\r\n\r\n"
	var dst, sink bytes.Buffer
	if err := copyBody(&dst, br(wire+"NEXT"), framingChunked, 0, &sink); err != nil {
		t.Fatal(err)
	}
	if dst.String() != wire {
		t.Fatalf("dst = %q, want %q", dst.String(), wire)
	}
	if sink.String() != "hello world" {
		t.Fatalf("sink = %q", sink.String())
	}
}

func TestCopyChunkedTruncated(t *testing.T) {
	var dst bytes.Buffer
	err := copyBody(&dst, br("5\r\nhel"), framingChunked, 0, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF", err)
	}
}

func TestCopyLengthTruncated(t *testing.T) {
	var dst bytes.Buffer
	err := copyBody(&dst, br("abc"), framingLength, 10, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF", err)
	}
}

func TestCapBufferDropsOnOverflow(t *testing.T) {
	c := &capBuffer{limit: 4}
	c.Write([]byte("abc"))
	if string(c.Bytes()) != "abc" {
		t.Fatal("within limit should be kept")
	}
	c.Write([]byte("de"))
	if c.Bytes() != nil {
		t.Fatal("overflow must drop the body entirely")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mitm/ -run 'ReadHead|Framing|ConnectionFlags|CopyChunked|CopyLength|CapBuffer'`
Expected: FAIL, build errors such as `undefined: readHead`, `undefined: framingNone`.

- [ ] **Step 3: Write the implementation**

Create `internal/mitm/httpwire.go`:

```go
package mitm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// The relay forwards HTTP/1.1 messages byte for byte. Go's net/http cannot do
// that: it canonicalizes header names (X-Stainless-OS becomes X-Stainless-Os)
// and writes headers in sorted order. So heads are read and forwarded raw, and
// parsed only to learn message framing.

const (
	maxHeadBytes = 64 << 10
	maxLineBytes = 8 << 10
)

var (
	errHeadTooLarge = errors.New("mitm: message head exceeds 64 KiB")
	errBadFraming   = errors.New("mitm: ambiguous or invalid message framing")
)

type framing int

const (
	framingNone framing = iota
	framingLength
	framingChunked
	framingUntilClose
)

// msgHead is a parsed message head. Raw is what arrived on the wire and is
// what gets forwarded; the other fields are read-only views of it.
type msgHead struct {
	Raw           []byte
	Header        http.Header // canonicalized copy, for lookups only
	Method        string
	Target        string
	Status        int
	Proto         string
	Framing       framing
	ContentLength int64
	Close         bool
	Upgrade       bool
	Expect100     bool
}

// readHead reads one message head up to and including the blank line,
// returning the exact bytes read. Leading empty lines are skipped.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		line, err := br.ReadSlice('\n')
		head = append(head, line...)
		if len(head) > maxHeadBytes {
			return nil, errHeadTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if err == io.EOF && len(head) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if isBlank(line) {
			if len(head) == len(line) {
				head = head[:0]
				continue
			}
			return head, nil
		}
	}
}

func isBlank(line []byte) bool {
	return bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))
}

func splitHead(raw []byte) (start string, header http.Header, err error) {
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	start, err = tp.ReadLine()
	if err != nil {
		return "", nil, err
	}
	mime, err := tp.ReadMIMEHeader()
	if err != nil {
		return "", nil, err
	}
	return start, http.Header(mime), nil
}

func parseRequestHead(raw []byte) (*msgHead, error) {
	start, header, err := splitHead(raw)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(start, " ", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/1.") {
		return nil, fmt.Errorf("mitm: malformed request line %q", start)
	}
	head := &msgHead{Raw: raw, Header: header, Method: parts[0], Target: parts[1], Proto: parts[2]}
	head.Framing, head.ContentLength, err = bodyFraming(header, false, head.Method, 0)
	if err != nil {
		return nil, err
	}
	head.Close, head.Upgrade = connectionFlags(header, head.Proto)
	head.Expect100 = strings.EqualFold(header.Get("Expect"), "100-continue")
	return head, nil
}

func parseResponseHead(raw []byte, requestMethod string) (*msgHead, error) {
	start, header, err := splitHead(raw)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(start, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return nil, fmt.Errorf("mitm: malformed status line %q", start)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("mitm: malformed status code %q", parts[1])
	}
	head := &msgHead{Raw: raw, Header: header, Method: requestMethod, Status: status, Proto: parts[0]}
	head.Framing, head.ContentLength, err = bodyFraming(header, true, requestMethod, status)
	if err != nil {
		return nil, err
	}
	head.Close, head.Upgrade = connectionFlags(header, head.Proto)
	return head, nil
}

// connectionFlags reports whether the sender asked to close the connection
// after this message, and whether it asked to upgrade the protocol.
func connectionFlags(h http.Header, proto string) (closeAfter, upgrade bool) {
	tokens := tokenSet(h["Connection"])
	closeAfter = tokens["close"] || (proto == "HTTP/1.0" && !tokens["keep-alive"])
	upgrade = tokens["upgrade"] && h.Get("Upgrade") != ""
	return closeAfter, upgrade
}

func tokenSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			set[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	return set
}

// bodyFraming decides how the body is delimited. Requests with both
// Transfer-Encoding and Content-Length, or conflicting Content-Length values,
// are rejected: that is the request-smuggling shape.
func bodyFraming(h http.Header, isResponse bool, method string, status int) (framing, int64, error) {
	if isResponse && (method == http.MethodHead || status/100 == 1 || status == 204 || status == 304) {
		return framingNone, 0, nil
	}
	te := strings.ToLower(strings.Join(h["Transfer-Encoding"], ","))
	lengths := h["Content-Length"]
	if te != "" {
		if !isResponse && len(lengths) > 0 {
			return 0, 0, errBadFraming
		}
		if strings.HasSuffix(strings.TrimSpace(te), "chunked") {
			return framingChunked, 0, nil
		}
		if !isResponse {
			return 0, 0, errBadFraming
		}
		return framingUntilClose, 0, nil
	}
	if len(lengths) > 0 {
		var length int64 = -1
		for _, value := range lengths {
			for _, part := range strings.Split(value, ",") {
				n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
				if err != nil || n < 0 || (length >= 0 && n != length) {
					return 0, 0, errBadFraming
				}
				length = n
			}
		}
		if length == 0 {
			return framingNone, 0, nil
		}
		return framingLength, length, nil
	}
	if isResponse {
		return framingUntilClose, 0, nil
	}
	return framingNone, 0, nil
}

// copyBody copies one message body from br to dst as it arrived, including
// chunk framing. sink, when non-nil, also receives the decoded payload.
func copyBody(dst io.Writer, br *bufio.Reader, f framing, n int64, sink io.Writer) error {
	switch f {
	case framingNone:
		return nil
	case framingLength:
		return copyN(dst, br, n, sink)
	case framingChunked:
		return copyChunked(dst, br, sink)
	case framingUntilClose:
		w := dst
		if sink != nil {
			w = io.MultiWriter(dst, sink)
		}
		_, err := io.Copy(w, br)
		return err
	}
	return nil
}

func copyN(dst io.Writer, br *bufio.Reader, n int64, sink io.Writer) error {
	w := dst
	if sink != nil {
		w = io.MultiWriter(dst, sink)
	}
	if _, err := io.CopyN(w, br, n); err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

func copyChunked(dst io.Writer, br *bufio.Reader, sink io.Writer) error {
	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		if _, err := dst.Write(line); err != nil {
			return err
		}
		size, err := parseChunkSize(line)
		if err != nil {
			return err
		}
		if size == 0 {
			for { // optional trailers, then the blank line
				trailer, err := readLine(br)
				if err != nil {
					return err
				}
				if _, err := dst.Write(trailer); err != nil {
					return err
				}
				if isBlank(trailer) {
					return nil
				}
			}
		}
		if err := copyN(dst, br, size, sink); err != nil {
			return err
		}
		crlf := make([]byte, 2)
		if _, err := io.ReadFull(br, crlf); err != nil {
			return io.ErrUnexpectedEOF
		}
		if crlf[0] != '\r' || crlf[1] != '\n' {
			return errBadFraming
		}
		if _, err := dst.Write(crlf); err != nil {
			return err
		}
	}
}

// readLine returns one line including its terminator. The slice is only valid
// until the next read; callers write it out immediately.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errBadFraming
	}
	if err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if len(line) > maxLineBytes {
		return nil, errBadFraming
	}
	return line, nil
}

func parseChunkSize(line []byte) (int64, error) {
	text := strings.TrimSpace(string(line))
	if i := strings.IndexByte(text, ';'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	size, err := strconv.ParseInt(text, 16, 64)
	if err != nil || size < 0 {
		return 0, errBadFraming
	}
	return size, nil
}

// capBuffer keeps up to limit bytes and silently drops everything once the
// limit is exceeded, so a partial body is never parsed.
type capBuffer struct {
	limit    int
	buf      bytes.Buffer
	overflow bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.overflow {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.limit {
		c.overflow = true
		c.buf.Reset()
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// Bytes returns the captured body, or nil if it overflowed.
func (c *capBuffer) Bytes() []byte {
	if c.overflow {
		return nil
	}
	return c.buf.Bytes()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./internal/mitm/ && go test -race -count=1 ./internal/mitm/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/httpwire.go internal/mitm/httpwire_test.go
git commit -m "feat(mitm): add byte-exact HTTP/1.1 head and body relay helpers"
```

### Task 3: Route classifier, body summarizer, registry (`observe.go`, `registry.go`)

**Files:**
- Create: `internal/mitm/observe.go`, `internal/mitm/registry.go`
- Create: `internal/mitm/observe_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (used by Tasks 4-7): `type Observation {Route, RawID string; Status int; Fields map[string]string}`, `classifyRoute(method, target string) (route, rawID string)`, `parsesBody(route string) bool`, `summarizeBody(body []byte, contentEncoding string) (rawID string, fields map[string]string)`, `maxObservedBody`, `type Session` (JSON tags `id`, `environmentKind`, `model`, `sessionStatus`, `statusBucket`, `connectionStatus`, `createdAt`, `lastSeenAt`, `requests`, `lastRoute`), `NewRegistry(max int, ttl time.Duration, now func() time.Time) *Registry`, `HashID(raw string) string` (12 hex chars), `(*Registry).Observe(Observation)`, `.List() []Session`, `.Get(id string) (Session, bool)`.

Field vocabulary comes from the real 2026-09-28 capture (`POST /v1/sessions`, `GET /v1/code/sessions/{id}`, `POST /v1/code/sessions/{id}/events`): `id`, `session_status`, `status_bucket`, `environment_kind`, `connection_status`, `configured_model` or `config.model`, `created_at`; the read route wraps its payload under `response_shape`.

Bug the tests caught while writing this (keep the fix): a new registry entry must have `LastSeenAt` set before eviction runs, otherwise the zero time makes the newest entry the eviction victim.

- [ ] **Step 1: Write the failing test**

Create `internal/mitm/observe_test.go`:

```go
package mitm

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClassifyRoute(t *testing.T) {
	cases := []struct {
		method, target, route, id string
	}{
		{"POST", "/v1/sessions", "sessions.create", ""},
		{"POST", "/v1/sessions?x=1", "sessions.create", ""},
		{"GET", "/v1/environment_providers", "environments.list", ""},
		{"GET", "/v1/code/sessions/session_01ABCDEFGHJK", "code.session.get", "session_01ABCDEFGHJK"},
		{"POST", "/v1/code/sessions/session_01ABCDEFGHJK/events", "code.session.events.post", "session_01ABCDEFGHJK"},
		{"GET", "/v1/code/sessions/session_01ABCDEFGHJK/events/stream?after=3", "code.session.events.stream", "session_01ABCDEFGHJK"},
		{"POST", "/v1/code/sessions/session_01ABCDEFGHJK/archive", "code.session.other", "session_01ABCDEFGHJK"},
		{"GET", "/v1/sessions/session_01ABCDEFGHJK", "sessions.get", "session_01ABCDEFGHJK"},
		{"GET", "/v1/messages", "", ""},
		{"GET", "/v1/code/sessions/x", "", ""}, // too short to be an id
		{"GET", "/v1/oauth/token", "", ""},
	}
	for _, c := range cases {
		route, id := classifyRoute(c.method, c.target)
		if route != c.route || id != c.id {
			t.Errorf("%s %s = (%q,%q), want (%q,%q)", c.method, c.target, route, id, c.route, c.id)
		}
	}
}

func TestSummarizeCreateResponse(t *testing.T) {
	body := `{"id":"session_01ABCDEFGHJK","session_status":"running","status_bucket":"working","environment_kind":"anthropic_cloud",
	 "connection_status":"connected","configured_model":"claude-opus-5-5","created_at":"2026-09-28T20:00:00Z",
	 "title":"my private prompt text","session_url":"https://claude.ai/code/session_01ABCDEFGHJK"}`
	id, fields := summarizeBody([]byte(body), "")
	if id != "session_01ABCDEFGHJK" {
		t.Fatalf("id = %q", id)
	}
	want := map[string]string{"sessionStatus": "running", "statusBucket": "working", "environmentKind": "anthropic_cloud",
		"connectionStatus": "connected", "model": "claude-opus-5-5", "createdAt": "2026-09-28T20:00:00Z"}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("fields[%s] = %q, want %q", k, fields[k], v)
		}
	}
	for _, v := range fields {
		if strings.Contains(v, "private") || strings.Contains(v, "http") {
			t.Errorf("free text leaked into fields: %q", v)
		}
	}
}

func TestSummarizeReadResponseWrapperAndConfigModel(t *testing.T) {
	body := `{"response_shape":{"connection_status":"connected","config":{"model":"claude-opus-5-5"},"environment_kind":"anthropic_cloud"}}`
	_, fields := summarizeBody([]byte(body), "")
	if fields["model"] != "claude-opus-5-5" || fields["environmentKind"] != "anthropic_cloud" {
		t.Fatalf("fields = %v", fields)
	}
}

func TestSummarizeDropsFreeTextValues(t *testing.T) {
	body := `{"id":"session_01ABCDEFGHJK","session_status":"waiting for the user to reply, please","status_bucket":"ok"}`
	_, fields := summarizeBody([]byte(body), "")
	if _, ok := fields["sessionStatus"]; ok {
		t.Fatal("free text must not be kept")
	}
	if fields["statusBucket"] != "ok" {
		t.Fatal("token value should be kept")
	}
}

func TestSummarizeGzipAndUnknownEncoding(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(`{"id":"session_01ABCDEFGHJK","status_bucket":"ok"}`))
	zw.Close()
	id, fields := summarizeBody(buf.Bytes(), "gzip")
	if id == "" || fields["statusBucket"] != "ok" {
		t.Fatalf("gzip body not decoded: %q %v", id, fields)
	}
	if id, fields := summarizeBody([]byte("garbage"), "br"); id != "" || len(fields) != 0 {
		t.Fatal("unknown encoding must yield nothing")
	}
	if id, _ := summarizeBody([]byte("not json"), ""); id != "" {
		t.Fatal("non-JSON must yield nothing")
	}
}

func TestRegistryHashesIDsAndMerges(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry(10, time.Hour, func() time.Time { return now })
	reg.Observe(Observation{Route: "sessions.create", RawID: "session_01ABCDEFGHJK", Fields: map[string]string{"model": "claude-opus-5-5"}})
	reg.Observe(Observation{Route: "code.session.events.post", RawID: "session_01ABCDEFGHJK"})
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("len = %d", len(list))
	}
	s := list[0]
	if s.ID != HashID("session_01ABCDEFGHJK") || len(s.ID) != 12 || strings.Contains(s.ID, "session") {
		t.Fatalf("id = %q", s.ID)
	}
	if s.Requests != 2 || s.Model != "claude-opus-5-5" || s.LastRoute != "code.session.events.post" {
		t.Fatalf("session = %+v", s)
	}
	blob, _ := json.Marshal(list)
	if strings.Contains(string(blob), "01ABCDEFGHJK") {
		t.Fatal("raw id leaked into JSON")
	}
	if got, ok := reg.Get(s.ID); !ok || got.ID != s.ID {
		t.Fatal("Get by display id failed")
	}
}

func TestRegistryIgnoresObservationsWithoutID(t *testing.T) {
	reg := NewRegistry(10, time.Hour, nil)
	reg.Observe(Observation{Route: "environments.list"})
	if len(reg.List()) != 0 {
		t.Fatal("no id, no session")
	}
}

func TestRegistryTTLAndCap(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	reg := NewRegistry(2, time.Hour, clock)
	reg.Observe(Observation{RawID: "session_AAAAAAAA1"})
	now = now.Add(time.Minute)
	reg.Observe(Observation{RawID: "session_BBBBBBBB2"})
	now = now.Add(time.Minute)
	reg.Observe(Observation{RawID: "session_CCCCCCCC3"})
	if len(reg.List()) != 2 {
		t.Fatalf("cap not enforced: %d", len(reg.List()))
	}
	if _, ok := reg.Get(HashID("session_AAAAAAAA1")); ok {
		t.Fatal("oldest should have been evicted")
	}
	now = now.Add(2 * time.Hour)
	if len(reg.List()) != 0 {
		t.Fatal("TTL not enforced")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mitm/ -run 'ClassifyRoute|Summarize|Registry'`
Expected: FAIL, build errors such as `undefined: classifyRoute`, `undefined: NewRegistry`.

- [ ] **Step 3: Write the implementation**

Create `internal/mitm/observe.go`:

```go
package mitm

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Observation is everything the registry learns from one exchange. It carries
// no headers and no body text: RawID is hashed on entry to the registry, and
// Fields hold enum-like tokens only.
type Observation struct {
	Route  string
	RawID  string
	Status int
	Fields map[string]string
}

var (
	idSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	// A field value is kept only if it looks like a plain token. Titles,
	// prompts and other free text never match.
	tokenValue = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,64}$`)
)

const (
	maxObservedBody = 1 << 20
	maxInflatedBody = 4 << 20
)

// classifyRoute maps a request to a masked route name and the raw session id
// found in the path. It returns an empty route for anything it does not track.
func classifyRoute(method, target string) (route, rawID string) {
	path := target
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	lower := strings.ToLower(method)
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case method == http.MethodPost && path == "/v1/sessions":
		return "sessions.create", ""
	case method == http.MethodGet && path == "/v1/environment_providers":
		return "environments.list", ""
	case len(segs) >= 4 && segs[0] == "v1" && segs[1] == "code" && segs[2] == "sessions" && idSegment.MatchString(segs[3]):
		rest := segs[4:]
		switch {
		case len(rest) == 0:
			return "code.session." + lower, segs[3]
		case len(rest) == 1 && rest[0] == "events":
			return "code.session.events." + lower, segs[3]
		case len(rest) == 2 && rest[0] == "events" && rest[1] == "stream":
			return "code.session.events.stream", segs[3]
		default:
			return "code.session.other", segs[3]
		}
	case len(segs) >= 3 && segs[0] == "v1" && segs[1] == "sessions" && idSegment.MatchString(segs[2]):
		if len(segs) == 3 {
			return "sessions." + lower, segs[2]
		}
		return "sessions.other", segs[2]
	}
	return "", ""
}

// parsesBody reports whether the response body of route is worth reading.
func parsesBody(route string) bool {
	switch route {
	case "sessions.create", "sessions.get", "code.session.get":
		return true
	}
	return false
}

// summarizeBody extracts the session id and enum-like status fields from a
// JSON response body. Unknown shapes yield nothing rather than an error.
func summarizeBody(body []byte, contentEncoding string) (rawID string, fields map[string]string) {
	if strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return "", nil
		}
		inflated, err := io.ReadAll(io.LimitReader(zr, maxInflatedBody+1))
		if err != nil || len(inflated) > maxInflatedBody {
			return "", nil
		}
		body = inflated
	} else if strings.TrimSpace(contentEncoding) != "" && !strings.EqualFold(contentEncoding, "identity") {
		return "", nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", nil
	}
	// The read route wraps its payload under "response_shape".
	if inner, ok := doc["response_shape"].(map[string]any); ok {
		doc = inner
	}
	fields = map[string]string{}
	if id, ok := doc["id"].(string); ok && idSegment.MatchString(id) {
		rawID = id
	}
	set := func(name string, value any) {
		if s, ok := value.(string); ok && tokenValue.MatchString(s) {
			fields[name] = s
		}
	}
	set("environmentKind", doc["environment_kind"])
	set("sessionStatus", doc["session_status"])
	set("statusBucket", doc["status_bucket"])
	set("connectionStatus", doc["connection_status"])
	set("model", doc["configured_model"])
	if cfg, ok := doc["config"].(map[string]any); ok {
		if _, has := fields["model"]; !has {
			set("model", cfg["model"])
		}
	}
	if created, ok := doc["created_at"].(string); ok {
		fields["createdAt"] = created // parsed, and dropped if not RFC3339, by the registry
	}
	return rawID, fields
}
```

Create `internal/mitm/registry.go`:

```go
package mitm

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// Session is one observed cloud session. ID is a 12-character hash: the raw
// session id is never stored.
type Session struct {
	ID               string    `json:"id"`
	EnvironmentKind  string    `json:"environmentKind,omitempty"`
	Model            string    `json:"model,omitempty"`
	SessionStatus    string    `json:"sessionStatus,omitempty"`
	StatusBucket     string    `json:"statusBucket,omitempty"`
	ConnectionStatus string    `json:"connectionStatus,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	LastSeenAt       time.Time `json:"lastSeenAt"`
	Requests         int       `json:"requests"`
	LastRoute        string    `json:"lastRoute,omitempty"`
}

// Registry is a capped, TTL-bound, in-memory store of observed sessions.
type Registry struct {
	mu   sync.Mutex
	max  int
	ttl  time.Duration
	now  func() time.Time
	byID map[string]*Session
}

// NewRegistry returns a registry holding at most max sessions for ttl each.
func NewRegistry(max int, ttl time.Duration, now func() time.Time) *Registry {
	if max <= 0 {
		max = 1000
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if now == nil {
		now = time.Now
	}
	return &Registry{max: max, ttl: ttl, now: now, byID: map[string]*Session{}}
}

// HashID reduces a raw session id to its display id.
func HashID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:12]
}

// Observe merges one observation. Observations without a session id are
// ignored: the registry only tracks sessions.
func (r *Registry) Observe(o Observation) {
	if o.RawID == "" {
		return
	}
	id := HashID(o.RawID)
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	s, ok := r.byID[id]
	if !ok {
		s = &Session{ID: id, CreatedAt: now, LastSeenAt: now}
		r.byID[id] = s
		r.evictLocked()
	}
	s.LastSeenAt = now
	s.Requests++
	s.LastRoute = o.Route
	if v := o.Fields["environmentKind"]; v != "" {
		s.EnvironmentKind = v
	}
	if v := o.Fields["model"]; v != "" {
		s.Model = v
	}
	if v := o.Fields["sessionStatus"]; v != "" {
		s.SessionStatus = v
	}
	if v := o.Fields["statusBucket"]; v != "" {
		s.StatusBucket = v
	}
	if v := o.Fields["connectionStatus"]; v != "" {
		s.ConnectionStatus = v
	}
	if v := o.Fields["createdAt"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			s.CreatedAt = t
		}
	}
}

func (r *Registry) expireLocked(now time.Time) {
	for id, s := range r.byID {
		if now.Sub(s.LastSeenAt) > r.ttl {
			delete(r.byID, id)
		}
	}
}

func (r *Registry) evictLocked() {
	for len(r.byID) > r.max {
		var oldestID string
		var oldest time.Time
		for id, s := range r.byID {
			if oldestID == "" || s.LastSeenAt.Before(oldest) {
				oldestID, oldest = id, s.LastSeenAt
			}
		}
		delete(r.byID, oldestID)
	}
}

// List returns all live sessions, most recently seen first.
func (r *Registry) List() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.now())
	out := make([]Session, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	return out
}

// Get returns one session by its display id.
func (r *Registry) Get(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.now())
	s, ok := r.byID[id]
	if !ok {
		return Session{}, false
	}
	return *s, true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/mitm && go vet ./internal/mitm/ && go test -race -count=1 ./internal/mitm/`
Expected: no gofmt output, then `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/observe.go internal/mitm/registry.go internal/mitm/observe_test.go
git commit -m "feat(mitm): classify session routes and keep a masked session registry"
```

### Task 4: The proxy server (`server.go`)

**Files:**
- Create: `internal/mitm/server.go`
- Create: `internal/mitm/server_test.go`

**Interfaces:**
- Consumes: Task 1 (`CA`, `HostPermitted`, `newTestCA`), Task 2 (`readHead`, `parseRequestHead`, `parseResponseHead`, `copyBody`, `capBuffer`, `msgHead`, framing consts, `maxObservedBody`), Task 3 (`Observation`, `Registry`, `classifyRoute`, `parsesBody`, `summarizeBody`, `HashID`).
- Produces (used by Task 5): `type Options {CA *CA; Registry *Registry; Logger *slog.Logger; Dial func(ctx, network, addr string) (net.Conn, error); UpstreamTLS *tls.Config; IdleTimeout time.Duration; Now func() time.Time}`, `type Stats {Terminated, Tunnelled, HandshakeFailures, UpstreamErrors, Requests, Observed int64}` (JSON camelCase), `New(Options) (*Server, error)`, `(*Server).Serve(net.Listener) error`, `.Shutdown(ctx) error`, `.Stats() Stats`, `bufferedConn`.

Behavior to keep (each has a test): only `CONNECT` is accepted (405 otherwise); permitted host on port 443 is terminated with ALPN `http/1.1` only; anything else is a blind tunnel; the relay reuses one upstream connection per client connection and redials when the upstream went idle-closed (`upstream.alive` probes with a 1 ms read deadline); `Expect: 100-continue` gets 417; upstream failure gets a 502 JSON body; `101 Switching Protocols` is spliced both ways; logs contain only the method and masked route, never headers, tokens or raw ids. `UpstreamTLS` exists for tests (custom roots); production leaves it nil, which means an empty `tls.Config` plus `ServerName`.

- [ ] **Step 1: Write the failing test**

Create `internal/mitm/server_test.go`:

```go
package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream is a raw TLS server standing in for api.anthropic.com. It
// records every request head exactly as received and answers via respond.
type fakeUpstream struct {
	addr    string
	accepts atomic.Int64
	heads   chan string
	bodies  chan string
}

func startFakeUpstream(t *testing.T, respond func(head string, body []byte, conn net.Conn)) (*fakeUpstream, *x509.CertPool) {
	t.Helper()
	upstreamCA := newTestCA(t) // an unrelated CA that "signs" the fake Anthropic cert
	leaf, err := upstreamCA.LeafFor("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*leaf}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeUpstream{addr: l.Addr().String(), heads: make(chan string, 32), bodies: make(chan string, 32)}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			f.accepts.Add(1)
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					raw, err := readHead(br)
					if err != nil {
						return
					}
					head, err := parseRequestHead(raw)
					if err != nil {
						return
					}
					var body bytes.Buffer
					var decoded bytes.Buffer
					if err := copyBody(&body, br, head.Framing, head.ContentLength, &decoded); err != nil {
						return
					}
					f.heads <- string(raw)
					f.bodies <- decoded.String()
					respond(string(raw), decoded.Bytes(), conn)
				}
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(upstreamCA.cert)
	return f, pool
}

func jsonResponse(body string) string {
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

func okJSON(body string) func(string, []byte, net.Conn) {
	return func(_ string, _ []byte, conn net.Conn) { io.WriteString(conn, jsonResponse(body)) }
}

type proxyHarness struct {
	srv      *Server
	ca       *CA
	registry *Registry
	addr     string
	logs     *bytes.Buffer
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// startProxy runs a Server on a loopback port. redirect maps a dialled
// "host:port" to a local address, so the tests never touch the network.
func startProxy(t *testing.T, redirect map[string]string, upstreamRoots *x509.CertPool) *proxyHarness {
	t.Helper()
	ca := newTestCA(t)
	registry := NewRegistry(100, time.Hour, nil)
	logs := &bytes.Buffer{}
	srv, err := New(Options{
		CA: ca, Registry: registry,
		Logger: slog.New(slog.NewTextHandler(&syncWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			if target, ok := redirect[addr]; ok {
				addr = target
			}
			return d.DialContext(ctx, network, addr)
		},
		UpstreamTLS: &tls.Config{RootCAs: upstreamRoots},
		IdleTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &proxyHarness{srv: srv, ca: ca, registry: registry, addr: l.Addr().String(), logs: logs}
}

func (h *proxyHarness) connect(t *testing.T, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	raw, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(raw)
	head, err := readHead(br)
	if err != nil || !strings.Contains(string(head), " 200 ") {
		t.Fatalf("CONNECT %s: head=%q err=%v", target, head, err)
	}
	return raw, br
}

// connectTLS does CONNECT host:443 and completes TLS as the CLI would,
// trusting only the proxy CA.
func (h *proxyHarness) connectTLS(t *testing.T, host string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	raw, br := h.connect(t, host+":443")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.ca.CertPEM())
	tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{RootCAs: pool, ServerName: host, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if tc.ConnectionState().NegotiatedProtocol != "http/1.1" {
		t.Fatalf("ALPN = %q, want http/1.1", tc.ConnectionState().NegotiatedProtocol)
	}
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	return tc, bufio.NewReader(tc)
}

// readResponse reads one full response (head and body) as received.
func readResponse(t *testing.T, br *bufio.Reader, method string) (*msgHead, string) {
	t.Helper()
	raw, err := readHead(br)
	if err != nil {
		t.Fatalf("read response head: %v", err)
	}
	head, err := parseResponseHead(raw, method)
	if err != nil {
		t.Fatal(err)
	}
	var wire, payload bytes.Buffer
	if err := copyBody(&wire, br, head.Framing, head.ContentLength, &payload); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return head, payload.String()
}

func TestTerminatedRequestPreservesHeaderCaseOrderAndBody(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{"ok":true}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")

	req := "POST /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nX-Stainless-OS: MacOS\r\nx-app: cli\r\nAccept: application/json\r\nContent-Length: 2\r\n\r\nhi"
	io.WriteString(conn, req)
	resp, body := readResponse(t, br, "POST")

	if resp.Status != 200 || body != `{"ok":true}` {
		t.Fatalf("status=%d body=%q", resp.Status, body)
	}
	if got := <-up.heads; got != strings.TrimSuffix(req, "hi") {
		t.Fatalf("upstream head not byte-exact:\n got %q\nwant %q", got, strings.TrimSuffix(req, "hi"))
	}
	if got := <-up.bodies; got != "hi" {
		t.Fatalf("upstream body = %q", got)
	}
}

func TestChunkedRequestBodyForwarded(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n")
	readResponse(t, br, "POST")
	<-up.heads
	if got := <-up.bodies; got != "hello world" {
		t.Fatalf("upstream body = %q", got)
	}
}

func TestSSEStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	up, roots := startFakeUpstream(t, func(_ string, _ []byte, conn net.Conn) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		io.WriteString(conn, "a\r\nevent: a\n\n\r\n")
		<-release
		io.WriteString(conn, "a\r\nevent: b\n\n\r\n0\r\n\r\n")
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /v1/code/sessions/session_01ABCDEFGHJK/events/stream HTTP/1.1\r\nHost: a\r\n\r\n")

	if _, err := readHead(br); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	size, err := br.ReadString('\n') // first chunk arrives while upstream is still blocked
	if err != nil || size != "a\r\n" {
		t.Fatalf("first chunk did not stream through: %q %v", size, err)
	}
	payload := make([]byte, 12)
	if _, err := io.ReadFull(br, payload); err != nil || !strings.HasPrefix(string(payload), "event: a") {
		t.Fatalf("payload = %q err=%v", payload, err)
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil && !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "event: b") || !strings.HasSuffix(string(rest), "0\r\n\r\n") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestKeepAliveReusesUpstreamConnection(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	for i := 0; i < 3; i++ {
		io.WriteString(conn, "GET /a HTTP/1.1\r\nHost: a\r\n\r\n")
		readResponse(t, br, "GET")
	}
	if n := up.accepts.Load(); n != 1 {
		t.Fatalf("upstream accepts = %d, want 1 (connection should be reused)", n)
	}
}

func TestStaleUpstreamConnectionIsRedialed(t *testing.T) {
	up, roots := startFakeUpstream(t, func(_ string, _ []byte, conn net.Conn) {
		io.WriteString(conn, jsonResponse(`{}`))
		time.AfterFunc(10*time.Millisecond, func() { conn.Close() }) // server drops the idle connection
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /a HTTP/1.1\r\nHost: a\r\n\r\n")
	readResponse(t, br, "GET")
	time.Sleep(150 * time.Millisecond)
	io.WriteString(conn, "GET /b HTTP/1.1\r\nHost: a\r\n\r\n")
	if resp, _ := readResponse(t, br, "GET"); resp.Status != 200 {
		t.Fatalf("second request status = %d", resp.Status)
	}
	if n := up.accepts.Load(); n != 2 {
		t.Fatalf("upstream accepts = %d, want 2 (redial after idle close)", n)
	}
}

func TestBlindTunnelForOtherHosts(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err == nil {
			io.Copy(conn, conn)
			conn.Close()
		}
	}()
	h := startProxy(t, map[string]string{"example.org:443": echo.Addr().String()}, nil)
	raw, br := h.connect(t, "example.org:443")
	io.WriteString(raw, "plain bytes, no TLS")
	got := make([]byte, len("plain bytes, no TLS"))
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "plain bytes, no TLS" {
		t.Fatalf("tunnel echo = %q err=%v", got, err)
	}
	if s := h.srv.Stats(); s.Tunnelled != 1 || s.Terminated != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestObserverRegistersCloudSession(t *testing.T) {
	up, roots := startFakeUpstream(t, func(head string, _ []byte, conn net.Conn) {
		if strings.HasPrefix(head, "POST /v1/sessions ") {
			io.WriteString(conn, jsonResponse(`{"id":"session_01ABCDEFGHJK","session_status":"running","environment_kind":"anthropic_cloud","configured_model":"claude-opus-5-5","title":"secret title"}`))
			return
		}
		io.WriteString(conn, jsonResponse(`{"results":[]}`))
	})
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "POST /v1/sessions HTTP/1.1\r\nHost: a\r\nContent-Length: 2\r\n\r\n{}")
	readResponse(t, br, "POST")
	io.WriteString(conn, "POST /v1/code/sessions/session_01ABCDEFGHJK/events HTTP/1.1\r\nHost: a\r\nContent-Length: 2\r\n\r\n{}")
	readResponse(t, br, "POST")

	list := h.registry.List()
	if len(list) != 1 {
		t.Fatalf("sessions = %+v", list)
	}
	s := list[0]
	if s.ID != HashID("session_01ABCDEFGHJK") || s.Requests != 2 || s.Model != "claude-opus-5-5" ||
		s.EnvironmentKind != "anthropic_cloud" || s.SessionStatus != "running" || s.LastRoute != "code.session.events.post" {
		t.Fatalf("session = %+v", s)
	}
}

func TestLogsNeverContainCredentialsOrRawIDs(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{"id":"session_01ABCDEFGHJK"}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /v1/code/sessions/session_01ABCDEFGHJK HTTP/1.1\r\nHost: a\r\nAuthorization: Bearer SECRET-TOKEN-123\r\nCookie: k=SECRET-COOKIE\r\n\r\n")
	readResponse(t, br, "GET")
	logs := h.logs.String()
	if logs == "" {
		t.Fatal("expected debug logs")
	}
	for _, bad := range []string{"SECRET-TOKEN-123", "SECRET-COOKIE", "Authorization", "01ABCDEFGHJK"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("logs contain %q:\n%s", bad, logs)
		}
	}
}

func TestUpstreamDownReturns502(t *testing.T) {
	h := startProxy(t, map[string]string{"api.anthropic.com:443": "127.0.0.1:1"}, nil)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "GET /a HTTP/1.1\r\nHost: a\r\n\r\n")
	resp, body := readResponse(t, br, "GET")
	if resp.Status != 502 || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("status=%d body=%q", resp.Status, body)
	}
	if s := h.srv.Stats(); s.UpstreamErrors != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestUntrustedClientHandshakeIsCounted(t *testing.T) {
	h := startProxy(t, nil, nil)
	raw, br := h.connect(t, "api.anthropic.com:443")
	tc := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "api.anthropic.com"})
	if err := tc.Handshake(); err == nil {
		t.Fatal("handshake should fail without the proxy CA")
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.srv.Stats().HandshakeFailures == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.srv.Stats().HandshakeFailures != 1 {
		t.Fatalf("stats = %+v", h.srv.Stats())
	}
}

func TestNonConnectRequestRejected(t *testing.T) {
	h := startProxy(t, nil, nil)
	raw, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	io.WriteString(raw, "GET http://example.org/ HTTP/1.1\r\nHost: example.org\r\n\r\n")
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(raw).ReadString('\n')
	if !strings.Contains(line, "405") {
		t.Fatalf("status line = %q", line)
	}
}

func TestExpectContinueRefused(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, br := h.connectTLS(t, "api.anthropic.com")
	io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 1\r\n\r\n")
	if resp, _ := readResponse(t, br, "POST"); resp.Status != 417 {
		t.Fatalf("status = %d, want 417", resp.Status)
	}
}

func TestShutdownClosesLiveConnections(t *testing.T) {
	up, roots := startFakeUpstream(t, okJSON(`{}`))
	h := startProxy(t, map[string]string{"api.anthropic.com:443": up.addr}, roots)
	conn, _ := h.connectTLS(t, "api.anthropic.com")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection should be closed after shutdown")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mitm/ -run 'Terminated|Chunked|SSE|KeepAlive|Stale|BlindTunnel|Observer|Logs|UpstreamDown|Untrusted|NonConnect|Expect|Shutdown'`
Expected: FAIL, build errors such as `undefined: New`, `undefined: Options`, `undefined: bufferedConn`.

- [ ] **Step 3: Write the implementation**

Create `internal/mitm/server.go`:

```go
package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Server.
type Options struct {
	CA       *CA
	Registry *Registry
	Logger   *slog.Logger
	// Dial opens outbound connections (tests redirect it). Nil uses net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// UpstreamTLS is for tests only (custom roots). Production leaves it nil,
	// which means an empty tls.Config{}: the standard Go client handshake.
	UpstreamTLS *tls.Config
	// IdleTimeout bounds a keep-alive connection waiting for the next request.
	IdleTimeout time.Duration
	Now         func() time.Time
}

// Stats are process-lifetime counters.
type Stats struct {
	Terminated        int64 `json:"terminated"`
	Tunnelled         int64 `json:"tunnelled"`
	HandshakeFailures int64 `json:"handshakeFailures"`
	UpstreamErrors    int64 `json:"upstreamErrors"`
	Requests          int64 `json:"requests"`
	Observed          int64 `json:"observed"`
}

// Server is the CONNECT listener.
type Server struct {
	opts   Options
	logger *slog.Logger

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	closing  bool
	wg       sync.WaitGroup

	terminated, tunnelled, handshakeFailures, upstreamErrors, requests, observed atomic.Int64
	warnedHosts                                                                  sync.Map
}

// New validates options and returns a Server.
func New(opts Options) (*Server, error) {
	if opts.CA == nil {
		return nil, errors.New("mitm: CA is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 2 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		opts.Dial = dialer.DialContext
	}
	return &Server{opts: opts, logger: opts.Logger, conns: map[net.Conn]struct{}{}}, nil
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Terminated: s.terminated.Load(), Tunnelled: s.tunnelled.Load(),
		HandshakeFailures: s.handshakeFailures.Load(), UpstreamErrors: s.upstreamErrors.Load(),
		Requests: s.requests.Load(), Observed: s.observed.Load(),
	}
}

// Serve accepts connections on l until it is closed or Shutdown is called.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return http.ErrServerClosed
	}
	s.listener = l
	s.mu.Unlock()
	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return http.ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if !s.track(conn) {
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.handleConn(conn)
		}()
	}
}

// Shutdown stops accepting, closes every live connection and waits for the
// handlers to return or ctx to end.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	if s.listener != nil {
		s.listener.Close()
	}
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	conn.Close()
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// bufferedConn serves reads from a bufio.Reader that already holds bytes the
// client sent right behind the CONNECT head.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (s *Server) handleConn(conn net.Conn) {
	br := bufio.NewReaderSize(conn, 32<<10)
	conn.SetReadDeadline(s.opts.Now().Add(10 * time.Second))
	raw, err := readHead(br)
	if err != nil {
		return
	}
	req, err := parseRequestHead(raw)
	if err != nil {
		writeError(conn, http.StatusBadRequest, "malformed request")
		return
	}
	if req.Method != http.MethodConnect {
		conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\nAllow: CONNECT\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		return
	}
	host, port, err := net.SplitHostPort(req.Target)
	if err != nil {
		writeError(conn, http.StatusBadRequest, "CONNECT target must be host:port")
		return
	}
	conn.SetReadDeadline(time.Time{})
	if port == "443" && HostPermitted(host) {
		s.terminate(conn, br, strings.ToLower(host))
		return
	}
	s.tunnel(conn, br, host, port)
}

// tunnel is a blind byte relay: the proxy sees the destination and nothing else.
func (s *Server) tunnel(conn net.Conn, br *bufio.Reader, host, port string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	up, err := s.opts.Dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		s.upstreamErrors.Add(1)
		writeError(conn, http.StatusBadGateway, "cannot reach "+host)
		return
	}
	defer up.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	s.tunnelled.Add(1)
	splice(conn, br, up, bufio.NewReader(up))
}

// splice copies both directions until both end. Each direction half-closes
// its destination when its source finishes.
func splice(a net.Conn, aBuf *bufio.Reader, b net.Conn, bBuf *bufio.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst net.Conn, src *bufio.Reader) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go pipe(b, aBuf)
	go pipe(a, bBuf)
	wg.Wait()
}

func (s *Server) terminate(conn net.Conn, br *bufio.Reader, host string) {
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// Server side only: this config faces the local CLI. The upstream
	// handshake toward Anthropic uses a separate, empty tls.Config.
	tlsConn := tls.Server(&bufferedConn{Conn: conn, r: br}, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.opts.CA.LeafFor(host) },
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := tlsConn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		s.handshakeFailures.Add(1)
		if _, seen := s.warnedHosts.LoadOrStore(host, true); !seen {
			s.logger.Warn("mitm: client TLS handshake failed; the CLI probably does not trust the proxy CA (set NODE_EXTRA_CA_CERTS)", "host", host, "error", err)
		}
		return
	}
	s.terminated.Add(1)
	s.relay(tlsConn, host)
}

type upstream struct {
	conn net.Conn
	br   *bufio.Reader
}

// alive reports whether the upstream connection is still open and idle. A
// pending read timeout is the healthy state; EOF, an error, or unexpected
// buffered data all mean the connection cannot be reused.
func (u *upstream) alive() bool {
	u.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := u.br.Peek(1)
	u.conn.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *Server) dialUpstream(host string) (*upstream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := s.opts.Dial(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{}
	if s.opts.UpstreamTLS != nil {
		cfg = s.opts.UpstreamTLS.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return &upstream{conn: tc, br: bufio.NewReaderSize(tc, 32<<10)}, nil
}

// relay serves one terminated client connection: an HTTP/1.1 keep-alive loop
// that forwards each message byte for byte.
func (s *Server) relay(client net.Conn, host string) {
	cbr := bufio.NewReaderSize(client, 32<<10)
	var up *upstream
	defer func() {
		if up != nil {
			up.conn.Close()
		}
	}()
	for {
		client.SetReadDeadline(s.opts.Now().Add(s.opts.IdleTimeout))
		raw, err := readHead(cbr)
		if err != nil {
			if errors.Is(err, errHeadTooLarge) {
				writeError(client, http.StatusRequestHeaderFieldsTooLarge, "request head too large")
			}
			return
		}
		client.SetReadDeadline(time.Time{})
		req, err := parseRequestHead(raw)
		if err != nil {
			writeError(client, http.StatusBadRequest, "malformed or ambiguous request")
			return
		}
		if req.Expect100 {
			writeError(client, http.StatusExpectationFailed, "Expect: 100-continue is not supported by the proxy")
			return
		}
		if up != nil && !up.alive() {
			up.conn.Close()
			up = nil
		}
		if up == nil {
			if up, err = s.dialUpstream(host); err != nil {
				s.upstreamErrors.Add(1)
				s.logger.Warn("mitm: upstream dial failed", "host", host, "error", err)
				writeError(client, http.StatusBadGateway, "cannot reach "+host)
				return
			}
		}
		s.requests.Add(1)
		if keep := s.exchange(client, cbr, up, req); !keep {
			return
		}
	}
}

// exchange forwards one request and its response. It returns true when both
// connections can carry another request.
func (s *Server) exchange(client net.Conn, cbr *bufio.Reader, up *upstream, req *msgHead) bool {
	route, rawID := classifyRoute(req.Method, req.Target)
	s.logger.Debug("mitm: request", "method", req.Method, "route", route)

	if _, err := up.conn.Write(req.Raw); err != nil {
		s.upstreamFailed(client, err)
		return false
	}
	if err := copyBody(up.conn, cbr, req.Framing, req.ContentLength, nil); err != nil {
		s.upstreamFailed(client, err)
		return false
	}

	var resp *msgHead
	for {
		rawResp, err := readHead(up.br)
		if err != nil {
			s.upstreamFailed(client, err)
			return false
		}
		if resp, err = parseResponseHead(rawResp, req.Method); err != nil {
			s.upstreamFailed(client, err)
			return false
		}
		if resp.Status/100 == 1 && resp.Status != http.StatusSwitchingProtocols {
			if _, err := client.Write(resp.Raw); err != nil {
				return false
			}
			continue
		}
		break
	}
	if _, err := client.Write(resp.Raw); err != nil {
		return false
	}
	if resp.Status == http.StatusSwitchingProtocols {
		s.observe(Observation{Route: route, RawID: rawID, Status: resp.Status})
		splice(client, cbr, up.conn, up.br)
		return false
	}

	var sink io.Writer
	var captured *capBuffer
	if parsesBody(route) && resp.Status/100 == 2 && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		captured = &capBuffer{limit: maxObservedBody}
		sink = captured
	}
	if err := copyBody(client, up.br, resp.Framing, resp.ContentLength, sink); err != nil {
		s.logger.Debug("mitm: response copy ended", "error", err)
		return false
	}
	if route != "" {
		obs := Observation{Route: route, RawID: rawID, Status: resp.Status}
		if captured != nil && captured.Bytes() != nil {
			id, fields := summarizeBody(captured.Bytes(), resp.Header.Get("Content-Encoding"))
			if obs.RawID == "" {
				obs.RawID = id
			}
			obs.Fields = fields
		}
		s.observe(obs)
	}
	return !req.Close && !resp.Close && resp.Framing != framingUntilClose
}

func (s *Server) upstreamFailed(client net.Conn, err error) {
	s.upstreamErrors.Add(1)
	s.logger.Warn("mitm: upstream exchange failed", "error", err)
	writeError(client, http.StatusBadGateway, "upstream error")
}

// observe reports to the registry. Any panic is swallowed: observation must
// never affect forwarding.
func (s *Server) observe(o Observation) {
	if s.opts.Registry == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Warn("mitm: observer panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	s.opts.Registry.Observe(o)
	s.observed.Add(1)
}

func writeError(conn net.Conn, status int, message string) {
	body := fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":%q}}`, message)
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/mitm && go vet ./internal/mitm/ && go test -race -count=3 ./internal/mitm/`
Expected: no gofmt output, then `ok` (36 tests, three runs).

Optional mutation check (proves the streaming test is meaningful): temporarily change the response `copyBody(client, ...)` call in `exchange` to copy into a `bytes.Buffer` first; `TestSSEStreamsIncrementally` must fail with `first chunk did not stream through`. Revert it.

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/server.go internal/mitm/server_test.go
git commit -m "feat(mitm): add CONNECT proxy with TLS termination and byte-exact relay"
```

### Task 5: Runtime bundle and loopback check (`runtime.go`)

**Files:**
- Create: `internal/mitm/runtime.go`, `internal/mitm/runtime_test.go`

**Interfaces:**
- Consumes: Tasks 1, 3, 4 (`LoadOrCreateCA`, `NewRegistry`, `New`, `Options`, `Server`).
- Produces (used by Tasks 6-7): `ValidateListen(addr string) error`, `type RuntimeConfig {Dir, Listen string; RegistryMax int; RegistryTTL time.Duration; Logger *slog.Logger}`, `type Runtime {CA *CA; Registry *Registry; Server *Server; Addr string}`, `StartRuntime(RuntimeConfig) (*Runtime, error)`, `(*Runtime).Shutdown(ctx) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/mitm/runtime_test.go`:

```go
package mitm

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateListen(t *testing.T) {
	good := []string{"127.0.0.1:8092", "[::1]:8092", "localhost:8092", "127.0.0.1:0"}
	bad := []string{"0.0.0.0:8092", ":8092", "192.168.1.5:8092", "example.com:8092", "127.0.0.1", "127.0.0.1:99999", "127.0.0.1:x"}
	for _, addr := range good {
		if err := ValidateListen(addr); err != nil {
			t.Errorf("ValidateListen(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range bad {
		if err := ValidateListen(addr); err == nil {
			t.Errorf("ValidateListen(%q) = nil, want error", addr)
		}
	}
}

func TestStartRuntimeServesAndShutsDown(t *testing.T) {
	rt, err := StartRuntime(RuntimeConfig{Dir: filepath.Join(t.TempDir(), "mitm"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", rt.Addr)
	if err != nil {
		t.Fatalf("listener not reachable: %v", err)
	}
	conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rt.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", rt.Addr, 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after Shutdown")
	}
}

func TestStartRuntimeRefusesNonLoopback(t *testing.T) {
	if _, err := StartRuntime(RuntimeConfig{Dir: t.TempDir(), Listen: "0.0.0.0:0"}); err == nil {
		t.Fatal("non-loopback listen must be refused")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mitm/ -run 'ValidateListen|StartRuntime'`
Expected: FAIL, build errors such as `undefined: ValidateListen`, `undefined: StartRuntime`.

- [ ] **Step 3: Write the implementation**

Create `internal/mitm/runtime.go`:

```go
package mitm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ValidateListen accepts only loopback listen addresses: the CA key and the
// decrypted traffic must never be reachable from another machine.
func ValidateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("mitm listen %q must be host:port", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("mitm listen port %q is not valid", port)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("mitm listen host %q must be a loopback address (127.0.0.1, ::1 or localhost)", host)
}

// RuntimeConfig is what StartRuntime needs; it deliberately mirrors no other
// package's config type.
type RuntimeConfig struct {
	Dir         string // directory holding the CA files
	Listen      string
	RegistryMax int
	RegistryTTL time.Duration
	Logger      *slog.Logger
}

// Runtime bundles a running forward proxy with what the management API reads.
type Runtime struct {
	CA       *CA
	Registry *Registry
	Server   *Server
	// Addr is the address actually bound (useful when Listen used port 0).
	Addr string
}

// StartRuntime loads or creates the CA, binds the listener and serves in the
// background. Any error leaves nothing running.
func StartRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if err := ValidateListen(cfg.Listen); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ca, err := LoadOrCreateCA(cfg.Dir, nil)
	if err != nil {
		return nil, err
	}
	registry := NewRegistry(cfg.RegistryMax, cfg.RegistryTTL, nil)
	srv, err := New(Options{CA: ca, Registry: registry, Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("mitm: listen: %w", err)
	}
	rt := &Runtime{CA: ca, Registry: registry, Server: srv, Addr: l.Addr().String()}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cfg.Logger.Error("mitm forward proxy stopped", "error", err)
		}
	}()
	return rt, nil
}

// Shutdown stops the listener and closes live connections.
func (r *Runtime) Shutdown(ctx context.Context) error { return r.Server.Shutdown(ctx) }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/mitm && go test -race -count=1 ./internal/mitm/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/runtime.go internal/mitm/runtime_test.go
git commit -m "feat(mitm): add runtime bundle and loopback-only listen validation"
```

### Task 6: Config block (`internal/config`)

**Files:**
- Create: `internal/config/mitm.go`, `internal/config/mitm_test.go`
- Modify: `internal/config/config.go` (add the `Mitm` field to `Config` and to `DefaultConfig`)

**Interfaces:**
- Consumes: Task 5 (`mitm.ValidateListen`).
- Produces (used by Task 7): `config.MitmConfig {Enabled bool; Listen string; RegistryMax, RegistryTTLMinutes int}`, `config.DefaultMitmConfig()`, `(MitmConfig).Validate() error`, `Config.Mitm`.

- [ ] **Step 1: Write the failing test**

Create `internal/config/mitm_test.go`:

```go
package config

import (
	"encoding/json"
	"testing"
)

func TestDefaultMitmConfigIsDisabledAndValid(t *testing.T) {
	cfg := DefaultConfig().Mitm
	if cfg.Enabled {
		t.Fatal("mitm must be off by default")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8092" || cfg.RegistryMax != 1000 || cfg.RegistryTTLMinutes != 1440 {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestMitmConfigValidate(t *testing.T) {
	ok := DefaultMitmConfig()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*MitmConfig){
		"non-loopback": func(c *MitmConfig) { c.Listen = "0.0.0.0:8092" },
		"no port":      func(c *MitmConfig) { c.Listen = "127.0.0.1" },
		"zero max":     func(c *MitmConfig) { c.RegistryMax = 0 },
		"huge max":     func(c *MitmConfig) { c.RegistryMax = 100001 },
		"zero ttl":     func(c *MitmConfig) { c.RegistryTTLMinutes = 0 },
		"huge ttl":     func(c *MitmConfig) { c.RegistryTTLMinutes = 10081 },
	}
	for name, mutate := range cases {
		cfg := DefaultMitmConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// A saved partial block must merge over the defaults, not zero them.
func TestMitmConfigPartialJSONKeepsDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(`{"mitm":{"enabled":true}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Mitm.Enabled || cfg.Mitm.Listen != "127.0.0.1:8092" {
		t.Fatalf("partial block lost defaults: %+v", cfg.Mitm)
	}
	if err := cfg.Mitm.Validate(); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run Mitm`
Expected: FAIL, build errors such as `undefined: DefaultMitmConfig`, `DefaultConfig().Mitm undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/config/mitm.go`:

```go
package config

import (
	"fmt"

	"antigravity-go-proxy/internal/mitm"
)

// MitmConfig configures the observe-only Claude Code forward proxy
// (internal/mitm). It is off by default. Changes apply on restart.
type MitmConfig struct {
	Enabled            bool   `json:"enabled"`
	Listen             string `json:"listen,omitempty"`
	RegistryMax        int    `json:"registryMax,omitempty"`
	RegistryTTLMinutes int    `json:"registryTtlMinutes,omitempty"`
}

// DefaultMitmConfig is the disabled default.
func DefaultMitmConfig() MitmConfig {
	return MitmConfig{Enabled: false, Listen: "127.0.0.1:8092", RegistryMax: 1000, RegistryTTLMinutes: 1440}
}

// Validate rejects non-loopback listeners and out-of-range limits.
func (m MitmConfig) Validate() error {
	if err := mitm.ValidateListen(m.Listen); err != nil {
		return err
	}
	if m.RegistryMax < 1 || m.RegistryMax > 100000 {
		return fmt.Errorf("mitm registryMax must be between 1 and 100000")
	}
	if m.RegistryTTLMinutes < 1 || m.RegistryTTLMinutes > 10080 {
		return fmt.Errorf("mitm registryTtlMinutes must be between 1 and 10080")
	}
	return nil
}
```

Edit `internal/config/config.go`:

```diff
diff --git a/internal/config/config.go b/internal/config/config.go
index 7e47ddb..49afb97 100644
--- a/internal/config/config.go
+++ b/internal/config/config.go
@@ -178,4 +178,5 @@ type Config struct {
 	GatewayOrder                GatewayOrderConfig        `json:"gatewayOrder"`
 	Classifier                  ClassifierConfig          `json:"classifier"`
+	Mitm                        MitmConfig                `json:"mitm"`
 }
 
@@ -622,4 +623,5 @@ func DefaultConfig() Config {
 		CustomEndpoints:        make(map[string]EndpointConfig),
 		ModelMapping:           make(map[string]any),
+		Mitm:                   DefaultMitmConfig(),
 		CacheBump: CacheBumpConfig{
 			Enabled:             false,
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/config && go test -count=1 ./internal/config/`
Expected: no gofmt output, then `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/config/mitm.go internal/config/mitm_test.go internal/config/config.go
git commit -m "feat(config): add the mitm forward proxy config block, disabled by default"
```

### Task 7: Management API and startup wiring

**Files:**
- Create: `internal/api/mitm_management.go`, `internal/api/mitm_management_test.go`
- Modify: `internal/api/server.go`, `internal/api/management.go`, `cmd/proxy/main.go`

**Interfaces:**
- Consumes: Task 5 (`mitm.Runtime`, `mitm.StartRuntime`, `mitm.RuntimeConfig`, `mitm.HashID`, `mitm.Observation`), Task 6 (`config.MitmConfig`, `Validate`).
- Produces: `api.Options.Mitm *mitm.Runtime`; `Server.mitm`; routes `GET /api/mitm/status`, `GET /api/mitm/ca.pem`, `GET /api/sessions/cloud`, `GET /api/sessions/cloud/{id}`; a `mitm` validation branch in `handleConfigSave`. All routes sit behind the existing WebUI-password check because `handleManagement` protects every `/api/` path except `/api/auth/url` and `GET /api/config`. The CA endpoint serves the certificate only, never the key.

- [ ] **Step 1: Write the failing test**

Create `internal/api/mitm_management_test.go`:

```go
package api

import (
	"antigravity-go-proxy/internal/config"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/mitm"
)

func startTestMitm(t *testing.T, srv *Server) *mitm.Runtime {
	t.Helper()
	rt, err := mitm.StartRuntime(mitm.RuntimeConfig{Dir: filepath.Join(t.TempDir(), "mitm"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Shutdown(t.Context()) })
	srv.mitm = rt
	return rt
}

func getJSON(t *testing.T, srv *Server, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestMitmManagementWhenDisabled(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	if _, body := getJSON(t, srv, "/api/mitm/status"); body["enabled"] != false {
		t.Fatalf("status = %v", body)
	}
	if rec, _ := getJSON(t, srv, "/api/mitm/ca.pem"); rec.Code != http.StatusNotFound {
		t.Fatalf("ca.pem code = %d, want 404", rec.Code)
	}
	if _, body := getJSON(t, srv, "/api/sessions/cloud"); body["enabled"] != false {
		t.Fatalf("sessions = %v", body)
	}
	if rec, _ := getJSON(t, srv, "/api/sessions/cloud/abc123abc123"); rec.Code != http.StatusNotFound {
		t.Fatalf("session get code = %d, want 404", rec.Code)
	}
}

func TestMitmManagementWhenRunning(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rt := startTestMitm(t, srv)
	rt.Registry.Observe(mitm.Observation{Route: "sessions.create", RawID: "session_01ABCDEFGHJK",
		Fields: map[string]string{"model": "claude-opus-5-5"}})

	_, status := getJSON(t, srv, "/api/mitm/status")
	if status["enabled"] != true || status["caFingerprint"] != rt.CA.Fingerprint() {
		t.Fatalf("status = %v", status)
	}
	if _, has := status["stats"]; !has {
		t.Fatal("status must carry stats")
	}

	rec, _ := getJSON(t, srv, "/api/mitm/ca.pem")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("ca.pem = %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
		t.Fatal("CA key must never be served")
	}

	_, list := getJSON(t, srv, "/api/sessions/cloud")
	sessions, _ := list["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v", list)
	}
	id := mitm.HashID("session_01ABCDEFGHJK")
	if got := sessions[0].(map[string]any)["id"]; got != id {
		t.Fatalf("id = %v, want %s", got, id)
	}
	if strings.Contains(rec.Body.String()+toJSON(t, list), "01ABCDEFGHJK") {
		t.Fatal("raw session id leaked")
	}
	if rec, one := getJSON(t, srv, "/api/sessions/cloud/"+id); rec.Code != http.StatusOK || one["model"] != "claude-opus-5-5" {
		t.Fatalf("get one = %d %v", rec.Code, one)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMitmRoutesRequireWebUIPassword(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	cfg := config.Get()
	cfg.WebUIPassword = "s3cret"
	config.SetForTest(cfg)
	for _, path := range []string{"/api/mitm/status", "/api/mitm/ca.pem", "/api/sessions/cloud"} {
		rec, _ := getJSON(t, srv, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without password = %d, want 401", path, rec.Code)
		}
	}
}

func TestConfigSaveRejectsNonLoopbackMitmListen(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"mitm":{"enabled":true,"listen":"0.0.0.0:8092"}}`))
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "loopback") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	ok := httptest.NewRecorder()
	srv.Handler().ServeHTTP(ok, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"mitm":{"enabled":true}}`)))
	if ok.Code != http.StatusOK {
		t.Fatalf("valid partial mitm block rejected: %d %s", ok.Code, ok.Body.String())
	}
	if !config.Get().Mitm.Enabled || config.Get().Mitm.Listen != "127.0.0.1:8092" {
		t.Fatalf("saved = %+v", config.Get().Mitm)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run 'Mitm|CloudSession'`
Expected: FAIL, build errors such as `srv.mitm undefined (type *Server has no field or method mitm)`.

- [ ] **Step 3: Write the implementation**

Create `internal/api/mitm_management.go`:

```go
package api

import (
	"net/http"
	"strings"
	"time"
)

// handleMitmStatus reports whether the forward proxy is running, its CA
// identity (never the key) and its counters.
func (server *Server) handleMitmStatus(writer http.ResponseWriter, request *http.Request) {
	rt := server.mitm
	if rt == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"enabled":       true,
		"listen":        rt.Addr,
		"caFingerprint": rt.CA.Fingerprint(),
		"caNotAfter":    rt.CA.NotAfter().UTC().Format(time.RFC3339),
		"stats":         rt.Server.Stats(),
	})
}

// handleMitmCA serves the CA certificate only.
func (server *Server) handleMitmCA(writer http.ResponseWriter, request *http.Request) {
	rt := server.mitm
	if rt == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "forward proxy is not running"})
		return
	}
	writer.Header().Set("Content-Type", "application/x-pem-file")
	writer.Header().Set("Content-Disposition", `attachment; filename="antigravity-proxy-mitm-ca.pem"`)
	_, _ = writer.Write(rt.CA.CertPEM())
}

// handleCloudSessionsList lists observed Claude Code cloud sessions.
func (server *Server) handleCloudSessionsList(writer http.ResponseWriter, request *http.Request) {
	rt := server.mitm
	if rt == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"enabled": false, "sessions": []any{}})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"enabled": true, "sessions": rt.Registry.List()})
}

// handleCloudSessionGet returns one observed session by its display id.
func (server *Server) handleCloudSessionGet(writer http.ResponseWriter, request *http.Request, id string) {
	rt := server.mitm
	if rt != nil {
		if session, ok := rt.Registry.Get(strings.TrimSpace(id)); ok {
			writeJSON(writer, http.StatusOK, session)
			return
		}
	}
	writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "unknown session"})
}
```

Apply these edits (save as a patch and `git apply`, or edit by hand):

```diff
diff --git a/cmd/proxy/main.go b/cmd/proxy/main.go
index 47a3811..8e8d7ac 100644
--- a/cmd/proxy/main.go
+++ b/cmd/proxy/main.go
@@ -25,4 +25,5 @@ import (
 	proxyformat "antigravity-go-proxy/internal/format"
 	"antigravity-go-proxy/internal/logger"
+	"antigravity-go-proxy/internal/mitm"
 	"antigravity-go-proxy/internal/openrouter"
 	"antigravity-go-proxy/internal/stats"
@@ -236,4 +237,24 @@ func runServer(args []string) {
 	}
 
+	// The observe-only Claude Code forward proxy. A failure to start it is
+	// logged and never stops the main proxy.
+	var mitmRuntime *mitm.Runtime
+	if cfg.Mitm.Enabled {
+		if err := cfg.Mitm.Validate(); err != nil {
+			slogger.Warn("mitm forward proxy disabled: invalid config", "error", err)
+		} else if rt, err := mitm.StartRuntime(mitm.RuntimeConfig{
+			Dir:         filepath.Join(config.GetConfigDir(), "mitm"),
+			Listen:      cfg.Mitm.Listen,
+			RegistryMax: cfg.Mitm.RegistryMax,
+			RegistryTTL: time.Duration(cfg.Mitm.RegistryTTLMinutes) * time.Minute,
+			Logger:      slogger,
+		}); err != nil {
+			slogger.Warn("mitm forward proxy disabled", "error", err)
+		} else {
+			mitmRuntime = rt
+			slogger.Info("mitm forward proxy listening", "address", rt.Addr, "caFingerprint", rt.CA.Fingerprint())
+		}
+	}
+
 	handler, err := api.New(api.Options{
 		APIKey:         *apiKey,
@@ -247,4 +268,5 @@ func runServer(args []string) {
 		Tracker:        tracker,
 		CCUsage:        ccUsage,
+		Mitm:           mitmRuntime,
 	})
 	if err != nil {
@@ -286,4 +308,9 @@ func runServer(args []string) {
 			slogger.Error("graceful shutdown failed", "error", err)
 		}
+		if mitmRuntime != nil {
+			if err := mitmRuntime.Shutdown(ctx); err != nil {
+				slogger.Warn("mitm forward proxy shutdown", "error", err)
+			}
+		}
 		// After the drain, so usage recorded by the last requests reaches
 		// the ledger.
diff --git a/internal/api/management.go b/internal/api/management.go
index 319c5a8..3c46ac3 100644
--- a/internal/api/management.go
+++ b/internal/api/management.go
@@ -174,4 +174,16 @@ func (server *Server) handleManagement(writer http.ResponseWriter, request *http
 		server.handleHeadroomStats(writer, request)
 		return true
+	case path == "/api/mitm/status" && method == http.MethodGet:
+		server.handleMitmStatus(writer, request)
+		return true
+	case path == "/api/mitm/ca.pem" && method == http.MethodGet:
+		server.handleMitmCA(writer, request)
+		return true
+	case path == "/api/sessions/cloud" && method == http.MethodGet:
+		server.handleCloudSessionsList(writer, request)
+		return true
+	case strings.HasPrefix(path, "/api/sessions/cloud/") && method == http.MethodGet:
+		server.handleCloudSessionGet(writer, request, strings.TrimPrefix(path, "/api/sessions/cloud/"))
+		return true
 	case path == "/api/cache-bump" && method == http.MethodGet:
 		server.handleCacheBumpGet(writer, request)
@@ -1261,4 +1273,20 @@ func (server *Server) handleConfigSave(writer http.ResponseWriter, request *http
 	}
 
+	if rawMitm, ok := updates["mitm"]; ok && rawMitm != nil {
+		merged := config.Get().Mitm
+		mitmBytes, err := json.Marshal(rawMitm)
+		if err == nil {
+			err = json.Unmarshal(mitmBytes, &merged)
+		}
+		if err != nil {
+			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid mitm configuration format"})
+			return
+		}
+		if err := merged.Validate(); err != nil {
+			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
+			return
+		}
+	}
+
 	if rawClassifier, ok := updates["classifier"]; ok && rawClassifier != nil {
 		classifierBytes, err := json.Marshal(rawClassifier)
diff --git a/internal/api/server.go b/internal/api/server.go
index 8c27371..cbe5f33 100644
--- a/internal/api/server.go
+++ b/internal/api/server.go
@@ -46,4 +46,5 @@ import (
 	"antigravity-go-proxy/internal/kimi"
 	"antigravity-go-proxy/internal/logger"
+	"antigravity-go-proxy/internal/mitm"
 	"antigravity-go-proxy/internal/modelcatalog"
 	"antigravity-go-proxy/internal/openrouter"
@@ -99,4 +100,6 @@ type Options struct {
 	// CCUsage is the Claude Code usage engine; nil turns usage tracking off.
 	CCUsage *ccusage.Engine
+	// Mitm is the running observe-only forward proxy; nil when disabled.
+	Mitm *mitm.Runtime
 }
 
@@ -117,4 +120,5 @@ type Server struct {
 	tracker            *stats.Tracker
 	ccUsage            *ccusage.Engine
+	mitm               *mitm.Runtime
 	ccCalibration      claudeCodeCalibrator
 	kimiOAuthMgr       *auth.KimiOAuthManager
@@ -181,4 +185,5 @@ func New(options Options) (*Server, error) {
 		claudeCodeOAuthMgr: options.ClaudeCodeOAuthMgr,
 		ccUsage:            options.CCUsage,
+		mitm:               options.Mitm,
 		projects:           make(map[string]string),
 	}
```

Note for `cmd/proxy/main.go`: a failure to start the forward proxy is logged as a warning and never stops the main proxy; `Shutdown` runs after `httpServer.Shutdown`. Changes to `mitm` in config apply on restart.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l cmd internal && go build -o bin/proxy ./cmd/proxy && go test -count=1 ./internal/api/ ./internal/mitm/ ./internal/config/`
Expected: no gofmt output, build succeeds, then `ok` for all three packages.

- [ ] **Step 5: Commit**

```bash
git add internal/api/mitm_management.go internal/api/mitm_management_test.go internal/api/server.go internal/api/management.go cmd/proxy/main.go
git commit -m "feat(api): expose the mitm forward proxy status, CA and observed cloud sessions"
```

### Task 8: WebUI "Cloud" tab

**Files:**
- Create: `internal/webui/public/js/components/cloud-sessions.js`
- Modify: `internal/webui/public/views/settings.html`, `internal/webui/public/js/store.js`, `internal/webui/public/index.html`, `internal/webui/public/js/translations/en.js`, `internal/webui/public/js/translations/pt.js`, `internal/webui/translations_test.go`

**Interfaces:**
- Consumes: Task 7 routes and `GET /api/config` (`config.mitm.enabled`), `POST /api/config`.
- Produces: settings sub-tab `cloudsessions`, component `window.Components.cloudSessions`, i18n keys listed in `cloudSessionsKeys`.

The tab shows: an enable toggle when the proxy is not running (writes `mitm.enabled`, applies on restart), and when running the listen address, CA fingerprint and expiry, a CA download button (fetched with the password header), a copy-ready `HTTPS_PROXY=... NODE_EXTRA_CA_CERTS=...` snippet, counters, and a sessions table refreshed every 5 s while the tab is open. The tab label is short ("Cloud" / "Nuvem") because the tab strip clips longer labels.

- [ ] **Step 1: Write the failing test**

Apply the `translations_test.go` part of the diff below first (new key list and two tests):

```diff
diff --git a/internal/webui/translations_test.go b/internal/webui/translations_test.go
index 19cbdb2..49615b4 100644
--- a/internal/webui/translations_test.go
+++ b/internal/webui/translations_test.go
@@ -85,4 +85,19 @@ var cacheBumpKeys = []string{
 }
 
+// cloudSessionsKeys are the i18n keys referenced by the Cloud Sessions tab in
+// views/settings.html and js/components/cloud-sessions.js. Every locale must
+// define them.
+var cloudSessionsKeys = []string{
+	"tabCloudSessions", "cloudSessionsTitle", "cloudSessionsDesc",
+	"cloudSessionsDisabled", "cloudSessionsEnable", "cloudSessionsRestartNote",
+	"cloudSessionsSaved", "cloudSessionsRefresh", "cloudSessionsListen",
+	"cloudSessionsCaFingerprint", "cloudSessionsCaExpires", "cloudSessionsDownloadCa",
+	"cloudSessionsEnvSnippet", "cloudSessionsCopy", "cloudSessionsCopied",
+	"cloudSessionsTerminated", "cloudSessionsTunnelled", "cloudSessionsHandshakeFailures",
+	"cloudSessionsUpstreamErrors", "cloudSessionsNoSessions", "cloudSessionsColId",
+	"cloudSessionsColModel", "cloudSessionsColStatus", "cloudSessionsColEnvironment",
+	"cloudSessionsColRequests", "cloudSessionsColLastSeen", "cloudSessionsColLastRoute",
+}
+
 var locales = []string{"en", "pt"}
 
@@ -495,2 +510,33 @@ func TestTranslations_QuotaUsageKeysReferenced(t *testing.T) {
 	}
 }
+
+func TestTranslations_CloudSessionsKeys(t *testing.T) {
+	for _, locale := range locales {
+		src := loadLocale(t, locale)
+		for _, key := range cloudSessionsKeys {
+			re := regexp.MustCompile(`(?m)^\s+` + key + `\s*:`)
+			if !re.MatchString(src) {
+				t.Errorf("locale %s missing key %q", locale, key)
+			}
+		}
+	}
+}
+
+func TestTranslations_CloudSessionsTemplateReferences(t *testing.T) {
+	b, err := Assets.ReadFile("public/views/settings.html")
+	if err != nil {
+		t.Fatalf("read settings.html: %v", err)
+	}
+	src := string(b)
+	for _, key := range cloudSessionsKeys {
+		if key == "cloudSessionsSaved" {
+			continue // referenced from cloud-sessions.js, not the template
+		}
+		if !strings.Contains(src, fmt.Sprintf("t('%s')", key)) {
+			t.Errorf("settings.html does not reference translation key %q", key)
+		}
+	}
+	if !strings.Contains(src, "settingsTab === 'cloudsessions'") {
+		t.Error("settings.html must define the cloudsessions tab")
+	}
+}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/webui/ -run CloudSessions`
Expected: FAIL: `locale en missing key "tabCloudSessions"` (and the same for every key, for `pt`), and `settings.html does not reference translation key ...`.

- [ ] **Step 3: Write the implementation**

Create `internal/webui/public/js/components/cloud-sessions.js`:

`internal/webui/public/js/components/cloud-sessions.js`:

```js
/**
 * Cloud Sessions Component
 * Read-only view of Claude Code cloud sessions seen by the observe-only
 * forward proxy (internal/mitm), plus its CA download and setup snippet.
 * The proxy binds loopback only; enabling it applies on restart.
 */
window.Components = window.Components || {};

window.Components.cloudSessions = () => ({
    loading: false,
    saving: false,
    error: '',
    configuredEnabled: false,
    status: { enabled: false },
    sessions: [],
    copied: false,
    _timer: null,

    init() {
        this.refresh();
        if (Alpine.store('global').settingsTab === 'cloudsessions') {
            this.startPolling();
        }
        if (this.$watch) {
            this.$watch('$store.global.settingsTab', (tab) => {
                if (tab === 'cloudsessions') {
                    this.refresh();
                    this.startPolling();
                } else {
                    this.stopPolling();
                }
            });
        }
    },

    startPolling() {
        this.stopPolling();
        this._timer = setInterval(() => this.refresh(), 5000);
    },

    stopPolling() {
        if (this._timer) {
            clearInterval(this._timer);
            this._timer = null;
        }
    },

    async getJSON(url) {
        const store = Alpine.store('global');
        const { response, newPassword } = await window.utils.request(url, {}, store?.webuiPassword);
        if (newPassword && store) store.webuiPassword = newPassword;
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json();
    },

    async refresh() {
        this.loading = true;
        this.error = '';
        try {
            const [config, status, list] = await Promise.all([
                this.getJSON('/api/config'),
                this.getJSON('/api/mitm/status'),
                this.getJSON('/api/sessions/cloud'),
            ]);
            this.configuredEnabled = config?.config?.mitm?.enabled === true;
            this.status = status || { enabled: false };
            this.sessions = Array.isArray(list?.sessions) ? list.sessions : [];
        } catch (err) {
            this.error = err.message;
            console.error('Failed to load cloud sessions:', err);
        } finally {
            this.loading = false;
        }
    },

    // Persist mitm.enabled. The listener starts or stops on the next restart.
    async setEnabled(value) {
        this.saving = true;
        this.error = '';
        try {
            const store = Alpine.store('global');
            const { response, newPassword } = await window.utils.request('/api/config', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ mitm: { enabled: value } }),
            }, store?.webuiPassword);
            if (newPassword && store) store.webuiPassword = newPassword;
            if (!response.ok) {
                const data = await response.json().catch(() => ({}));
                throw new Error(data.error || `HTTP ${response.status}`);
            }
            this.configuredEnabled = value;
            store.showToast(store.t('cloudSessionsSaved'), 'success');
        } catch (err) {
            this.error = err.message;
            Alpine.store('global').showToast(err.message, 'error');
        } finally {
            this.saving = false;
        }
    },

    envSnippet() {
        const listen = this.status.listen || '127.0.0.1:8092';
        return `HTTPS_PROXY=http://${listen} NODE_EXTRA_CA_CERTS=/path/to/antigravity-proxy-mitm-ca.pem`;
    },

    async copySnippet() {
        try {
            await navigator.clipboard.writeText(this.envSnippet());
            this.copied = true;
            setTimeout(() => { this.copied = false; }, 2000);
        } catch (err) {
            console.error('Clipboard unavailable:', err);
        }
    },

    // Fetched with the password header, so it works when a WebUI password is set.
    async downloadCA() {
        try {
            const store = Alpine.store('global');
            const { response, newPassword } = await window.utils.request('/api/mitm/ca.pem', {}, store?.webuiPassword);
            if (newPassword && store) store.webuiPassword = newPassword;
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const blob = await response.blob();
            const link = document.createElement('a');
            link.href = URL.createObjectURL(blob);
            link.download = 'antigravity-proxy-mitm-ca.pem';
            link.click();
            URL.revokeObjectURL(link.href);
        } catch (err) {
            this.error = err.message;
        }
    },

    formatTime(iso) {
        if (!iso) return '';
        const date = new Date(iso);
        return isNaN(date.getTime()) ? '' : date.toLocaleString();
    },
});
```

Apply the remaining edits:

```diff
diff --git a/internal/webui/public/index.html b/internal/webui/public/index.html
index 173e7dc..ddf2efb 100644
--- a/internal/webui/public/index.html
+++ b/internal/webui/public/index.html
@@ -573,4 +573,5 @@
     <script src="js/components/classifier-audit-feed.js"></script>
     <script src="js/components/gateway-order.js"></script>
+    <script src="js/components/cloud-sessions.js"></script>
     <script src="js/components/add-account-modal.js"></script>
     <!-- 4. App (registers Alpine components from window.Components) -->
diff --git a/internal/webui/public/js/store.js b/internal/webui/public/js/store.js
index 47ae80e..37f8f11 100644
--- a/internal/webui/public/js/store.js
+++ b/internal/webui/public/js/store.js
@@ -9,5 +9,5 @@ document.addEventListener('alpine:init', () => {
             // Hash-based routing
             const validTabs = ['dashboard', 'models', 'accounts', 'logs', 'settings'];
-            const validSettingsTabs = ['ui', 'claude', 'models', 'server', 'classifier'];
+            const validSettingsTabs = ['ui', 'claude', 'models', 'server', 'classifier', 'cloudsessions'];
             const getHash = () => window.location.hash.substring(1);
 
diff --git a/internal/webui/public/js/translations/en.js b/internal/webui/public/js/translations/en.js
index 9c55a68..bd63131 100644
--- a/internal/webui/public/js/translations/en.js
+++ b/internal/webui/public/js/translations/en.js
@@ -792,4 +792,31 @@ window.translations.en = {
     // Security Monitor (Classifier)
     tabClassifier: "Security Monitor",
+    tabCloudSessions: "Cloud",
+    cloudSessionsTitle: "Claude Code Cloud Sessions",
+    cloudSessionsDesc: "Read-only view of Claude Code cloud sessions seen by the local observe-only forward proxy. Nothing is rewritten.",
+    cloudSessionsDisabled: "The forward proxy is not running.",
+    cloudSessionsEnable: "Enable forward proxy",
+    cloudSessionsRestartNote: "Changes apply on the next restart. The listener binds 127.0.0.1 only.",
+    cloudSessionsSaved: "Forward proxy setting saved (applies on restart)",
+    cloudSessionsRefresh: "Refresh",
+    cloudSessionsListen: "Listening on",
+    cloudSessionsCaFingerprint: "CA fingerprint (SHA-256)",
+    cloudSessionsCaExpires: "CA expires",
+    cloudSessionsDownloadCa: "Download CA certificate",
+    cloudSessionsEnvSnippet: "Run Claude Code with",
+    cloudSessionsCopy: "Copy",
+    cloudSessionsCopied: "Copied",
+    cloudSessionsTerminated: "Intercepted",
+    cloudSessionsTunnelled: "Tunnelled",
+    cloudSessionsHandshakeFailures: "TLS trust failures",
+    cloudSessionsUpstreamErrors: "Upstream errors",
+    cloudSessionsNoSessions: "No cloud sessions seen yet.",
+    cloudSessionsColId: "Session",
+    cloudSessionsColModel: "Model",
+    cloudSessionsColStatus: "Status",
+    cloudSessionsColEnvironment: "Environment",
+    cloudSessionsColRequests: "Requests",
+    cloudSessionsColLastSeen: "Last seen",
+    cloudSessionsColLastRoute: "Last route",
     classifierSettingsTitle: "Security Monitor Configuration",
     classifierSettingsDesc: "Interception, rerouting, parameter tuning, and stubbing for Claude Code safety classification requests.",
diff --git a/internal/webui/public/js/translations/pt.js b/internal/webui/public/js/translations/pt.js
index 596d02b..346dda8 100644
--- a/internal/webui/public/js/translations/pt.js
+++ b/internal/webui/public/js/translations/pt.js
@@ -735,4 +735,31 @@ window.translations.pt = {
     // Security Monitor (Classifier)
     tabClassifier: "Monitor de Segurança",
+    tabCloudSessions: "Nuvem",
+    cloudSessionsTitle: "Sessões na Nuvem do Claude Code",
+    cloudSessionsDesc: "Visão somente leitura das sessões na nuvem do Claude Code vistas pelo proxy de encaminhamento local. Nada é reescrito.",
+    cloudSessionsDisabled: "O proxy de encaminhamento não está em execução.",
+    cloudSessionsEnable: "Ativar proxy de encaminhamento",
+    cloudSessionsRestartNote: "As alterações valem na próxima reinicialização. O listener usa somente 127.0.0.1.",
+    cloudSessionsSaved: "Configuração do proxy de encaminhamento salva (vale após reiniciar)",
+    cloudSessionsRefresh: "Atualizar",
+    cloudSessionsListen: "Escutando em",
+    cloudSessionsCaFingerprint: "Impressão digital da CA (SHA-256)",
+    cloudSessionsCaExpires: "CA expira em",
+    cloudSessionsDownloadCa: "Baixar certificado da CA",
+    cloudSessionsEnvSnippet: "Execute o Claude Code com",
+    cloudSessionsCopy: "Copiar",
+    cloudSessionsCopied: "Copiado",
+    cloudSessionsTerminated: "Interceptadas",
+    cloudSessionsTunnelled: "Em túnel",
+    cloudSessionsHandshakeFailures: "Falhas de confiança TLS",
+    cloudSessionsUpstreamErrors: "Erros de upstream",
+    cloudSessionsNoSessions: "Nenhuma sessão na nuvem vista ainda.",
+    cloudSessionsColId: "Sessão",
+    cloudSessionsColModel: "Modelo",
+    cloudSessionsColStatus: "Status",
+    cloudSessionsColEnvironment: "Ambiente",
+    cloudSessionsColRequests: "Requisições",
+    cloudSessionsColLastSeen: "Visto por último",
+    cloudSessionsColLastRoute: "Última rota",
     classifierSettingsTitle: "Configuração do Monitor de Segurança",
     classifierSettingsDesc: "Interceptação, redirecionamento, ajuste de parâmetros e respostas simuladas para requisições de classificação do Claude Code.",
diff --git a/internal/webui/public/views/settings.html b/internal/webui/public/views/settings.html
index fa7ce48..fdf2c29 100644
--- a/internal/webui/public/views/settings.html
+++ b/internal/webui/public/views/settings.html
@@ -62,4 +62,12 @@
                     <span x-text="$store.global.t('tabClassifier')">Security Monitor</span>
                 </button>
+                <button @click="$store.global.settingsTab = 'cloudsessions'"
+                    class="transition-colors font-medium text-sm flex items-center gap-2 whitespace-nowrap"
+                    :class="$store.global.settingsTab === 'cloudsessions' ? 'text-white' : 'text-gray-500 hover:text-gray-300'">
+                    <svg xmlns="http://www.w3.org/2000/svg" class="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
+                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 15a4 4 0 004 4h9a5 5 0 10-.1-9.999 5.002 5.002 0 10-9.78 2.096A4.001 4.001 0 003 15z" />
+                    </svg>
+                    <span x-text="$store.global.t('tabCloudSessions')">Cloud Sessions</span>
+                </button>
             </div>
         </div>
@@ -4874,4 +4882,106 @@
             </div>
 
+
+            <!-- Tab: Cloud Sessions (observe-only forward proxy) -->
+            <div x-show="$store.global.settingsTab === 'cloudsessions'" x-data="window.Components.cloudSessions()"
+                class="space-y-6 animate-fade-in pb-10">
+                <div class="card bg-space-900/30 border border-neon-cyan/40 p-6 space-y-5">
+                    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 w-full">
+                        <div class="space-y-1">
+                            <h4 class="text-sm font-bold text-white uppercase tracking-wider" x-text="$store.global.t('cloudSessionsTitle')">Claude Code Cloud Sessions</h4>
+                            <p class="text-xs text-gray-400" x-text="$store.global.t('cloudSessionsDesc')">Read-only view of cloud sessions seen by the local observe-only forward proxy.</p>
+                        </div>
+                        <button class="btn btn-xs btn-ghost text-gray-400" @click="refresh()" :disabled="loading"
+                            x-text="$store.global.t('cloudSessionsRefresh')">Refresh</button>
+                    </div>
+
+                    <div x-show="error" class="text-xs text-red-400" x-text="error"></div>
+
+                    <!-- Disabled state -->
+                    <div x-show="!status.enabled" class="space-y-3 w-full">
+                        <p class="text-xs text-gray-400" x-text="$store.global.t('cloudSessionsDisabled')">The forward proxy is not running.</p>
+                        <label class="flex items-center gap-3 cursor-pointer">
+                            <input type="checkbox" class="toggle toggle-sm toggle-info"
+                                :checked="configuredEnabled" :disabled="saving"
+                                @change="setEnabled($event.target.checked)">
+                            <span class="text-xs text-gray-300" x-text="$store.global.t('cloudSessionsEnable')">Enable forward proxy</span>
+                        </label>
+                        <p class="text-[11px] text-gray-500" x-text="$store.global.t('cloudSessionsRestartNote')">Changes apply on the next restart. The listener binds 127.0.0.1 only.</p>
+                    </div>
+
+                    <!-- Running state -->
+                    <div x-show="status.enabled" class="space-y-4 w-full">
+                        <div class="grid grid-cols-1 md:grid-cols-2 gap-3 text-xs w-full">
+                            <div>
+                                <span class="text-gray-500" x-text="$store.global.t('cloudSessionsListen')">Listening on</span>
+                                <div class="font-mono text-gray-200" x-text="status.listen"></div>
+                            </div>
+                            <div>
+                                <span class="text-gray-500" x-text="$store.global.t('cloudSessionsCaFingerprint')">CA fingerprint (SHA-256)</span>
+                                <div class="font-mono text-gray-200 break-all" x-text="status.caFingerprint"></div>
+                            </div>
+                            <div>
+                                <span class="text-gray-500" x-text="$store.global.t('cloudSessionsCaExpires')">CA expires</span>
+                                <div class="font-mono text-gray-200" x-text="formatTime(status.caNotAfter)"></div>
+                            </div>
+                            <div class="flex items-end">
+                                <button class="btn btn-xs btn-outline" @click="downloadCA()" x-text="$store.global.t('cloudSessionsDownloadCa')">Download CA certificate</button>
+                            </div>
+                        </div>
+
+                        <div class="space-y-1 w-full">
+                            <span class="text-xs text-gray-500" x-text="$store.global.t('cloudSessionsEnvSnippet')">Run Claude Code with</span>
+                            <div class="flex items-center gap-2">
+                                <code class="flex-1 text-[11px] font-mono bg-space-800 rounded px-2 py-1.5 text-gray-200 break-all" x-text="envSnippet()"></code>
+                                <button class="btn btn-xs btn-ghost" @click="copySnippet()"
+                                    x-text="copied ? $store.global.t('cloudSessionsCopied') : $store.global.t('cloudSessionsCopy')">Copy</button>
+                            </div>
+                        </div>
+
+                        <div class="grid grid-cols-2 md:grid-cols-4 gap-3 text-xs w-full">
+                            <div><span class="text-gray-500" x-text="$store.global.t('cloudSessionsTerminated')">Intercepted</span>
+                                <div class="font-mono text-gray-200" x-text="status.stats?.terminated ?? 0"></div></div>
+                            <div><span class="text-gray-500" x-text="$store.global.t('cloudSessionsTunnelled')">Tunnelled</span>
+                                <div class="font-mono text-gray-200" x-text="status.stats?.tunnelled ?? 0"></div></div>
+                            <div><span class="text-gray-500" x-text="$store.global.t('cloudSessionsHandshakeFailures')">TLS trust failures</span>
+                                <div class="font-mono text-gray-200" x-text="status.stats?.handshakeFailures ?? 0"></div></div>
+                            <div><span class="text-gray-500" x-text="$store.global.t('cloudSessionsUpstreamErrors')">Upstream errors</span>
+                                <div class="font-mono text-gray-200" x-text="status.stats?.upstreamErrors ?? 0"></div></div>
+                        </div>
+                    </div>
+                </div>
+
+                <div x-show="status.enabled" class="card bg-space-900/30 border border-space-border/50 p-4 overflow-x-auto">
+                    <table class="table table-xs w-full">
+                        <thead>
+                            <tr class="text-gray-500">
+                                <th x-text="$store.global.t('cloudSessionsColId')">Session</th>
+                                <th x-text="$store.global.t('cloudSessionsColModel')">Model</th>
+                                <th x-text="$store.global.t('cloudSessionsColStatus')">Status</th>
+                                <th x-text="$store.global.t('cloudSessionsColEnvironment')">Environment</th>
+                                <th x-text="$store.global.t('cloudSessionsColRequests')">Requests</th>
+                                <th x-text="$store.global.t('cloudSessionsColLastSeen')">Last seen</th>
+                                <th x-text="$store.global.t('cloudSessionsColLastRoute')">Last route</th>
+                            </tr>
+                        </thead>
+                        <tbody>
+                            <template x-for="session in sessions" :key="session.id">
+                                <tr class="text-gray-300">
+                                    <td class="font-mono" x-text="session.id"></td>
+                                    <td class="font-mono" x-text="session.model || '-'"></td>
+                                    <td x-text="session.sessionStatus || session.statusBucket || '-'"></td>
+                                    <td x-text="session.environmentKind || '-'"></td>
+                                    <td x-text="session.requests"></td>
+                                    <td x-text="formatTime(session.lastSeenAt)"></td>
+                                    <td class="font-mono" x-text="session.lastRoute || '-'"></td>
+                                </tr>
+                            </template>
+                        </tbody>
+                    </table>
+                    <p x-show="sessions.length === 0" class="text-xs text-gray-500 py-4 text-center"
+                        x-text="$store.global.t('cloudSessionsNoSessions')">No cloud sessions seen yet.</p>
+                </div>
+            </div>
+
         </div>
     </div>
```

- [ ] **Step 4: Run the tests and check the page**

Run: `node --check internal/webui/public/js/components/cloud-sessions.js && go test -count=1 ./internal/webui/`
Expected: `ok`.

Manual check (optional but recommended): start the proxy with `{"mitm":{"enabled":true,"listen":"127.0.0.1:18092"}}` in the config directory, open `http://127.0.0.1:<port>/#settings/cloudsessions`, and confirm the tab lists the CA fingerprint and an empty sessions table. In a locked-down environment the Alpine/Chart.js CDN scripts may not load; a pre-existing `Unexpected identifier 's'` page error from Alpine also appears on the unmodified build and is not caused by this tab.

- [ ] **Step 5: Commit**

```bash
git add internal/webui
git commit -m "feat(webui): add the Cloud tab for the observe-only forward proxy"
```

### Task 9: Verification, docs and spec amendments

**Files:**
- Create: `docs/claude-code-forward-proxy.md`
- Modify: `README.md`, `docs/superpowers/specs/2026-09-28-claude-code-forward-proxy-design.md`

- [ ] **Step 1: Add the user guide and README link**

Create `docs/claude-code-forward-proxy.md`:

`docs/claude-code-forward-proxy.md`:

```markdown
# Claude Code forward proxy (observe-only)

`claude --cloud` sessions talk to `api.anthropic.com` directly and ignore
`ANTHROPIC_BASE_URL`, so the normal gateway never sees them. This optional
forward proxy lets the WebUI list those sessions. It only observes: nothing is
rewritten, and inference still uses `ANTHROPIC_BASE_URL` as before.

## Enable

1. Add to `config.json` (or use Settings → Cloud in the WebUI) and restart:

   ```json
   "mitm": { "enabled": true }
   ```

   Defaults: `listen` `127.0.0.1:8092` (loopback only, anything else is
   rejected), `registryMax` 1000, `registryTtlMinutes` 1440.

2. Download the CA certificate from Settings → Cloud, or
   `GET /api/mitm/ca.pem`. It is created on first start under
   `<configdir>/mitm/`. The private key (`ca-key.pem`, mode 0600) never leaves
   that directory.

3. Start Claude Code with the proxy and the CA, per process:

   ```sh
   HTTPS_PROXY=http://127.0.0.1:8092 \
   NODE_EXTRA_CA_CERTS=/path/to/antigravity-proxy-mitm-ca.pem \
   claude --cloud "your task"
   ```

   Use `NODE_EXTRA_CA_CERTS`, which adds a root. Do not use `SSL_CERT_FILE`,
   which replaces the whole root pool. Do not install the CA in the system
   trust store.

## What it does and does not do

- Decrypts traffic to `anthropic.com`, `claude.ai` and `claude.com` only. The
  CA is name-constrained to those domains. Every other host is a blind tunnel:
  the proxy sees the destination and nothing else.
- Because paths are only visible after TLS is terminated, **all**
  `api.anthropic.com` traffic from a process started with `HTTPS_PROXY` is
  decrypted, including tokens. Keep it loopback-only and leave it off when you
  do not need it.
- Records only a masked route, status and enum-like fields (model, status,
  environment kind) per session, keyed by a 12-character hash. It never stores
  headers, tokens, prompts, titles or raw session ids, and never logs them.
- Upstream requests are forwarded byte for byte over HTTP/1.1 (header case and
  order preserved) with a standard Go TLS handshake. That handshake differs from
  the Claude Code CLI's own TLS stack.

## API

All routes need the WebUI password when one is set.

| Route | Result |
|---|---|
| `GET /api/mitm/status` | enabled, listen address, CA fingerprint and expiry, counters |
| `GET /api/mitm/ca.pem` | CA certificate (404 when disabled) |
| `GET /api/sessions/cloud` | observed sessions, most recent first |
| `GET /api/sessions/cloud/{id}` | one session by its hashed id |

## Troubleshooting

- `TLS trust failures` rising in the WebUI: the process does not trust the CA.
  Check `NODE_EXTRA_CA_CERTS` points at the certificate you downloaded.
- `Expect: 100-continue` requests get a 417; the CLI does not send them.
- If the proxy is down while `HTTPS_PROXY` is set, the CLI's Anthropic calls
  fail visibly, the same as when the gateway is down.
- To rotate the CA, stop the proxy, delete `<configdir>/mitm/`, and start it
  again; then re-download the certificate.
```

Edit `README.md`:

```diff
diff --git a/README.md b/README.md
index 82e0a14..e040a8a 100644
--- a/README.md
+++ b/README.md
@@ -663,4 +663,11 @@ claude --bare -p --model sonnet 'Reply with OK'
 ```
 
+### Claude Code cloud sessions (optional forward proxy)
+
+`claude --cloud` sessions bypass `ANTHROPIC_BASE_URL`. An optional, loopback-only,
+observe-only forward proxy lists them in the WebUI (Settings → Cloud). It is off
+by default; see [docs/claude-code-forward-proxy.md](docs/claude-code-forward-proxy.md)
+for setup and the security trade-offs.
+
 ### Hermes Agent Integration
 
```

- [ ] **Step 2: Amend the spec for the deviations**

In `docs/superpowers/specs/2026-09-28-claude-code-forward-proxy-design.md`:
- Section 5, delete the bullet "The generic config merge cannot delete keys, so `mitm` gets the same save special case as `modelMapping`." and add: "All `mitm` fields are scalars, so the generic config merge is correct; a partial block keeps the defaults."
- Section 2 (`upstream.go` bullet): replace the `http.Transport` wording with "Requests are relayed as raw HTTP/1.1 messages (`httpwire.go`) over a connection opened with an empty `tls.Config{}`; `net/http` is not used for the wire because it sorts and canonicalizes headers."
- Section 4: add "`Expect: 100-continue` returns 417. `101` upgrades are spliced."
- Section 5 (WebUI): add "The tab has an enable toggle (applies on restart)."

- [ ] **Step 3: Run the whole suite**

Run: `gofmt -l . && go vet ./... && go build -o bin/proxy ./cmd/proxy && go test -race -count=1 ./internal/mitm/ && go test -count=1 ./...`
Expected: no gofmt output; everything `ok` except the pre-existing `internal/auth` failures noted above (they need the `agy` binary). If `graft` is installed, run `graft build` to refresh the repo graph (AGENTS.md).

- [ ] **Step 4: Real-client smoke test, no credentials**

Start the proxy with a scratch config directory and the feature enabled:

```bash
export ANTIGRAVITY_CONFIG_DIR=$(mktemp -d)
echo '{"mitm":{"enabled":true,"listen":"127.0.0.1:18092"}}' > "$ANTIGRAVITY_CONFIG_DIR/config.json"
./bin/proxy -listen 127.0.0.1:18091 &
curl -s http://127.0.0.1:18091/api/mitm/ca.pem -o /tmp/mitm-ca.pem
curl -sS --noproxy '' -x http://127.0.0.1:18092 --cacert /tmp/mitm-ca.pem \
  -H 'x-api-key: dummy' -H 'anthropic-version: 2023-06-01' \
  -o /dev/null -w 'http=%{http_code} ver=%{http_version}\n' \
  https://api.anthropic.com/v1/environment_providers
curl -s http://127.0.0.1:18091/api/mitm/status
```

Expected: `http=401 ver=1.1` (a real answer from Anthropic, relayed over HTTP/1.1), and `stats.terminated` is at least 1. Use `--noproxy ''` if your shell sets `no_proxy` for `api.anthropic.com` (Claude Code containers do), otherwise curl silently bypasses the proxy. A request to `https://example.org/` through the same proxy must increment `tunnelled`, not `terminated`. With the real CLI: `env -i PATH="$PATH" HOME=$(mktemp -d) HTTPS_PROXY=http://127.0.0.1:18092 NODE_EXTRA_CA_CERTS=/tmp/mitm-ca.pem ANTHROPIC_API_KEY=sk-ant-dummy claude -p hi` must show `terminated` and `requests` increasing with `handshakeFailures` staying 0 (verified on claude 2.1.284 while planning: 3 terminated, 10 requests, 0 failures).

- [ ] **Step 5: Acceptance on the user's machine (real cloud session)**

Needs the user's login and creates one real cloud session, so run it only with their approval. From a throwaway directory containing one dummy file (`claude --cloud` uploads the working directory):

```bash
HTTPS_PROXY=http://127.0.0.1:8092 NODE_EXTRA_CA_CERTS=/path/to/antigravity-proxy-mitm-ca.pem \
  claude --cloud "Reply with exactly CLOUD_PROBE_OK. Do not modify any files."
curl -s -H "x-webui-password: $PW" http://127.0.0.1:8091/api/sessions/cloud
```

Expected: one entry with a 12-character `id`, `environmentKind` `anthropic_cloud`, a `model`, `requests` at least 2, and `lastRoute` such as `code.session.events.post`. `grep -i -E 'authorization|bearer|session_' <proxy log>` must find nothing. Archive the session at claude.ai/code afterwards. The Cloud tab shows the same row.

**Result (2026-09-28, claude 2.1.280):** entry `145502b68670` — 12-char id, `environmentKind` `anthropic_cloud`, `model` `claude-opus-5-5`, `requests` 4, `lastRoute` `code.session.other`; log grep clean. Two expectations did not hold: (1) `code.session.events.post` never crosses the client machine — the cloud-session events flow is server-side (feasibility report: "the interactive re-attach produced no records"), so the observable routes are `sessions.create`, `code.session.get` and `code.session.other`; (2) the CLI's inference in a cloud session goes to `POST /v1/messages?beta=true` like normal CLI traffic. The `405 status code (no body)` errors during the run were the CLI's own `ANTHROPIC_BASE_URL=http://localhost:8080` config (absolute-URI requests to a non-CONNECT proxy), not a proxy defect; unsetting that env var cleared them.

- [ ] **Step 6: Record the upstream fingerprint (documented, not gated)**

On the user's machine (macOS: `-i pktap,all -P` as in `.reference/fingerprint-recheck-20260924.txt`), capture proxy to `api.anthropic.com` and, separately, the real CLI to `api.anthropic.com`:

```bash
tcpdump -i any -w /tmp/mitm-upstream.pcap host api.anthropic.com -c 60   # while running the Step 5 command
tshark -r /tmp/mitm-upstream.pcap -Y 'tls.handshake.type==1' -T fields -e tls.handshake.ja4 -e tls.handshake.extensions_alpn_str
```

Save both outputs to `.reference/mitm-upstream-fingerprint-<date>.txt`. The proxy's ClientHello is the standard Go one (no ALPN, because `NextProtos` is unset), and it will not match the CLI's (a native Bun binary); the point is to have the difference on record. The AGENTS.md JA4 gate concerns agy and Cloud Code, and this work never touches `internal/cloudcode`.

- [ ] **Step 7: Commit**

```bash
git add docs README.md
git commit -m "docs: document the observe-only Claude Code forward proxy"
```

---

## Self-review (spec coverage)

| Spec requirement | Task |
|---|---|
| Name-constrained CA, 0600 key, leaf 24 h, cert-only export | 1, 7 |
| `CONNECT`, TLS termination for permitted hosts, blind tunnel otherwise | 4 |
| Byte-exact header case and order, no total stream timeout, 64 KB cap, 2 min idle | 2, 4 |
| Observer sees summaries only; hashed ids; enum-only values; leak test | 3, 4 |
| Registry cap and TTL | 3 |
| Errors: CA missing, handshake failure (counted, hint once), upstream 502, Upgrade splice, observer panic | 4, 5, 7 |
| Config block default off, loopback validation at save and startup | 5, 6, 7 |
| API routes read-only behind the WebUI password | 7 |
| WebUI status card, CA download, snippet, sessions table, polling | 8 |
| Real-client check, acceptance with a real cloud session, fingerprint record | 9 |
| Prerequisite not in scope: constant-time WebUI password compare (`management.go:29-40`) | not implemented; tracked in the feasibility report §7 risk 3 |
