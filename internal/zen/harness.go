package zen

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// OpenCode gates Zen free-tier models to its own harness: the genuine client
// sends a fixed set of x-opencode-* routing headers plus a versioned
// User-Agent. Third-party clients send none of them, which is one of the two
// layers the Zen free-tier check inspects (the other is the TLS handshake —
// see tls.go).
//
// Header names, defaults and the ID algorithm are ported from the MIT
// reimplementation in kode-ai/providers/opencode/headers.go.
const (
	// HeaderProject declares the OpenCode project id (git root hash or "global").
	HeaderProject = "x-opencode-project"
	// HeaderSession declares the OpenCode session id.
	HeaderSession = "x-opencode-session"
	// HeaderRequest declares the OpenCode per-request message id.
	HeaderRequest = "x-opencode-request"
	// HeaderClient declares the calling OpenCode surface (cli, desktop).
	HeaderClient = "x-opencode-client"
	// HeaderUA carries the "opencode/<version>" identity.
	HeaderUA = "User-Agent"

	// DefaultVersion is the OpenCode release the disguise impersonates.
	DefaultVersion = "1.18.31"
	// DefaultHarnessClient is the disguise surface when no override exists.
	DefaultHarnessClient = "cli"
	// DefaultProject is the project id used outside a git repository.
	DefaultProject = "global"

	idLength    = 26
	idRandom    = idLength - 12
	base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// HarnessConfig is the live disguise configuration for zen-bound requests.
type HarnessConfig struct {
	Enabled bool
	Version string
	Client  string
	Project string
}

var (
	harnessMu sync.RWMutex
	// Disguise-by-default: a caller that never wires config still sends the
	// genuine header set, consistent with the proxy's purpose.
	harnessCfg = HarnessConfig{
		Enabled: true,
		Version: DefaultVersion,
		Client:  DefaultHarnessClient,
		Project: DefaultProject,
	}
)

// SetHarnessConfig replaces the live harness configuration. Safe for
// concurrent use with request handling; called from the config hook only.
func SetHarnessConfig(cfg HarnessConfig) {
	harnessMu.Lock()
	harnessCfg = cfg
	harnessMu.Unlock()
}

// GetHarnessConfig returns a copy of the live harness configuration.
func GetHarnessConfig() HarnessConfig {
	harnessMu.RLock()
	defer harnessMu.RUnlock()
	return harnessCfg
}

// resolveHarnessIdentity resolves the version, client and project identity
// fields from cfg and the environment. Empty or whitespace-only config values
// fall back to the package defaults; OPENCODE_VERSION and OPENCODE_CLIENT
// override version and client, mirroring the genuine client's env behavior.
func resolveHarnessIdentity(cfg HarnessConfig) (version, client, project string) {
	version = strings.TrimSpace(cfg.Version)
	if v := strings.TrimSpace(os.Getenv("OPENCODE_VERSION")); v != "" {
		version = v
	} else if version == "" {
		version = DefaultVersion
	}
	client = strings.TrimSpace(cfg.Client)
	if v := strings.TrimSpace(os.Getenv("OPENCODE_CLIENT")); v != "" {
		client = v
	} else if client == "" {
		client = DefaultHarnessClient
	}
	project = strings.TrimSpace(cfg.Project)
	if project == "" {
		project = DefaultProject
	}
	return version, client, project
}

// stampHarnessHeaders writes the full harness identity for cfg onto hdr with
// freshly generated session/request ids. No-op when cfg disables the harness.
func stampHarnessHeaders(hdr http.Header, cfg HarnessConfig) {
	if !cfg.Enabled {
		return
	}
	version, client, project := resolveHarnessIdentity(cfg)
	hdr.Set(HeaderUA, "opencode/"+version)
	hdr.Set(HeaderClient, client)
	hdr.Set(HeaderProject, project)
	if sid, err := NewSessionID(); err == nil {
		hdr.Set(HeaderSession, sid)
	}
	if rid, err := NewRequestID(); err == nil {
		hdr.Set(HeaderRequest, rid)
	}
}

// ApplyHarnessHeaderMap stamps the OpenCode harness headers onto an outgoing
// header map. It is a no-op when the harness is disabled. Session/request ids
// are regenerated on every call; see resolveHarnessIdentity for how the
// remaining fields are resolved.
func ApplyHarnessHeaderMap(hdr http.Header) {
	if hdr == nil {
		return
	}
	stampHarnessHeaders(hdr, GetHarnessConfig())
}

// getHeader retrieves the first value for key in hdr. It checks hdr.Get(key)
// first (canonical lookup), falling back to case-insensitive key matching so
// that non-canonical map literals (e.g. in tests) behave identically.
func getHeader(hdr http.Header, key string) string {
	if hdr == nil {
		return ""
	}
	if v := hdr.Get(key); v != "" {
		return v
	}
	for k, vv := range hdr {
		if strings.EqualFold(k, key) && len(vv) > 0 {
			return vv[0]
		}
	}
	return ""
}

// isOpenCodeClient reports whether the inbound headers come from a genuine
// OpenCode client: an opencode/ User-Agent or any x-opencode-* identity header.
func isOpenCodeClient(hdr http.Header) bool {
	if strings.HasPrefix(strings.ToLower(getHeader(hdr, HeaderUA)), "opencode/") {
		return true
	}
	for _, k := range []string{HeaderSession, HeaderClient, HeaderProject, HeaderRequest} {
		if getHeader(hdr, k) != "" {
			return true
		}
	}
	return false
}

// ApplyHarnessHeaderMapPreserving stamps the harness identity onto dst. A
// genuine OpenCode client's session, client, project and request id are copied
// from src; missing fields are filled from config/env/defaults. The inbound
// User-Agent is kept only when it is itself an opencode/ UA, so a foreign UA on
// an opencode-labelled request never reaches Zen. Every other caller gets the
// full disguise. No-op when the harness is disabled.
func ApplyHarnessHeaderMapPreserving(dst, src http.Header) {
	cfg := GetHarnessConfig()
	if dst == nil || !cfg.Enabled {
		return
	}
	if !isOpenCodeClient(src) {
		stampHarnessHeaders(dst, cfg)
		return
	}
	version, client, project := resolveHarnessIdentity(cfg)
	if ua := getHeader(src, HeaderUA); strings.HasPrefix(strings.ToLower(ua), "opencode/") {
		dst.Set(HeaderUA, ua)
	} else {
		dst.Set(HeaderUA, "opencode/"+version)
	}
	setOr := func(key, fallback string) {
		if v := getHeader(src, key); v != "" {
			dst.Set(key, v)
		} else if fallback != "" {
			dst.Set(key, fallback)
		}
	}
	setOr(HeaderClient, client)
	setOr(HeaderProject, project)
	var sid, rid string
	if getHeader(src, HeaderSession) == "" {
		sid, _ = NewSessionID()
	}
	if getHeader(src, HeaderRequest) == "" {
		rid, _ = NewRequestID()
	}
	setOr(HeaderSession, sid)
	setOr(HeaderRequest, rid)
}

// ApplyHarnessHeadersPreserving is ApplyHarnessHeaderMapPreserving for a request.
func ApplyHarnessHeadersPreserving(dst *http.Request, src http.Header) {
	if dst == nil {
		return
	}
	ApplyHarnessHeaderMapPreserving(dst.Header, src)
}

// ApplyHarnessHeaders stamps the OpenCode harness headers onto a request.
// A nil request is ignored.
func ApplyHarnessHeaders(req *http.Request) {
	if req == nil {
		return
	}
	ApplyHarnessHeaderMap(req.Header)
}

// NewSessionID returns an OpenCode-shaped session id ("ses_" + 26 base62 chars).
// The timestamp portion is inverted so session ids sort descending by age.
func NewSessionID() (string, error) {
	id, err := createID(true, time.Now())
	if err != nil {
		return "", err
	}
	return "ses_" + id, nil
}

// NewRequestID returns an OpenCode-shaped request id ("msg_" + 26 base62 chars).
func NewRequestID() (string, error) {
	id, err := createID(false, time.Now())
	if err != nil {
		return "", err
	}
	return "msg_" + id, nil
}

// createID renders 12 hex chars of the millisecond timestamp (optionally
// bit-flipped so ids sort descending) followed by 14 random base62 chars.
func createID(descending bool, t time.Time) (string, error) {
	if t.IsZero() {
		t = time.Now()
	}
	ts := uint64(t.UnixMilli())*0x1000 + uint64(1)
	if descending {
		ts = ^ts
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], ts)
	random, err := randomBase62(idRandom)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x%s", buf[2:], random), nil
}

func randomBase62(length int) (string, error) {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random id bytes: %w", err)
	}
	var sb strings.Builder
	sb.Grow(length)
	for _, v := range b {
		sb.WriteByte(base62Chars[int(v)%len(base62Chars)])
	}
	return sb.String(), nil
}

// IsFreeTierGateError reports whether an upstream error body is Zen's
// free-tier gate: the 403 FreeTierError returned to non-OpenCode clients.
// A gzip-compressed body (magic bytes 1f 8b) is decompressed first, so the
// check works regardless of Content-Encoding.
func IsFreeTierGateError(body []byte) bool {
	if decoded, ok := gunzipBody(body); ok {
		body = decoded
	}
	lower := strings.ToLower(string(body))
	for _, phrase := range []string{
		"free tier can only be used from within opencode",
		"freetiererror",
		"or newer is required to use the free tier",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// gunzipBody decompresses a gzip payload, bounded to 1 MiB. It reports
// false for non-gzip input or any decompression error (the caller then
// inspects the raw bytes).
func gunzipBody(body []byte) ([]byte, bool) {
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return nil, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, 1<<20))
	if err != nil {
		return nil, false
	}
	return out, true
}

// WarnFreeTierGate logs the single distinct warning for a Zen free-tier gate
// response. Non-gate bodies log nothing.
func WarnFreeTierGate(model string, status int, body []byte) {
	if !IsFreeTierGateError(body) {
		return
	}
	slog.Warn("zen free-tier gate rejected request",
		"model", model,
		"status", status,
		"hint", "gate wants an opencode >=1.18.0 UA, a fresh x-opencode-session, and a stream:true body carrying bash+read tools")
}

// ObserveFreeTierGate reads a 403 (FreeTierError) or 426 (UpgradeRequired)
// response body (up to 1 MiB), logs the gate warning when it is Zen's
// free-tier gate, and restores the body so the downstream reader sees
// identical bytes: the full body, or — past the limit or after a read
// error — the consumed prefix chained back onto the original reader. A read
// error is logged rather than dropped, and the truncated bytes are never
// substituted for the body. Other responses are never read.
func ObserveFreeTierGate(resp *http.Response, model string) {
	if resp == nil || (resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUpgradeRequired) {
		return
	}
	const limit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil && len(raw) == 0 {
		return
	}
	original := resp.Body
	if err != nil {
		slog.Error("zen gate observe: upstream body read failed",
			"error", err, "model", model, "status", resp.StatusCode)
		chainBody(resp, raw, original)
		return
	}
	if len(raw) >= limit {
		WarnFreeTierGate(model, resp.StatusCode, raw)
		chainBody(resp, raw, original)
		return
	}
	_ = original.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	WarnFreeTierGate(model, resp.StatusCode, raw)
}

// chainBody hands back the bytes already read in front of the original
// reader, so no byte is lost and the reader's own error still surfaces
// downstream.
func chainBody(resp *http.Response, raw []byte, original io.ReadCloser) {
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), original), original}
}
