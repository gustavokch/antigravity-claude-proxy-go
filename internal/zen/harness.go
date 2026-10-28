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
// sends a fixed set of x-opencode-* and session routing headers plus a
// versioned User-Agent. Third-party clients send none of them, which is one
// of the layers the Zen free-tier check inspects (the TLS handshake is
// another — see tls.go).
//
// The header set is the one OpenCode 2.0.20 sends on a model request
// (packages/core/src/session/model-request.ts). Defaults and the ID algorithm
// are ported from the MIT reimplementation in
// kode-ai/providers/opencode/headers.go.
const (
	// HeaderProject declares the OpenCode project id (git root hash or "global").
	HeaderProject = "x-opencode-project"
	// HeaderSession declares the OpenCode session id.
	HeaderSession = "x-opencode-session"
	// HeaderSessionAffinity and HeaderSessionID repeat the session id for
	// provider cache routing.
	HeaderSessionAffinity = "x-session-affinity"
	HeaderSessionID       = "X-Session-Id"
	// HeaderClient declares the calling OpenCode surface (cli, desktop).
	HeaderClient = "x-opencode-client"
	// HeaderUA carries the "opencode/latest/<version>/<client>" identity.
	HeaderUA = "User-Agent"

	// DefaultVersion is the OpenCode release the disguise impersonates.
	DefaultVersion = "2.0.20"
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

// ApplyHarnessHeaderMap stamps the OpenCode harness headers onto an outgoing
// header map. It is a no-op when the harness is disabled. Empty or
// whitespace-only config values fall back to the package defaults;
// OPENCODE_VERSION and OPENCODE_CLIENT override version and client, mirroring
// the genuine client's env behavior. The session id is regenerated on every
// call.
func ApplyHarnessHeaderMap(hdr http.Header) {
	if hdr == nil {
		return
	}
	cfg := GetHarnessConfig()
	if !cfg.Enabled {
		return
	}
	version := strings.TrimSpace(cfg.Version)
	if v := strings.TrimSpace(os.Getenv("OPENCODE_VERSION")); v != "" {
		version = v
	} else if version == "" {
		version = DefaultVersion
	}
	client := strings.TrimSpace(cfg.Client)
	if v := strings.TrimSpace(os.Getenv("OPENCODE_CLIENT")); v != "" {
		client = v
	} else if client == "" {
		client = DefaultHarnessClient
	}
	project := strings.TrimSpace(cfg.Project)
	if project == "" {
		project = DefaultProject
	}

	// OpenCode builds its UA as opencode/<channel>/<version>/<app name> and
	// sends the same app name as x-opencode-client.
	hdr.Set(HeaderUA, "opencode/latest/"+version+"/"+client)
	hdr.Set(HeaderClient, client)
	hdr.Set(HeaderProject, project)
	if sid, err := NewSessionID(); err == nil {
		hdr.Set(HeaderSession, sid)
		hdr.Set(HeaderSessionAffinity, sid)
		hdr.Set(HeaderSessionID, sid)
	}
}

// ApplyHarnessHeaders stamps the OpenCode harness headers onto a request.
// A nil request is ignored.
func ApplyHarnessHeaders(req *http.Request) {
	if req == nil {
		return
	}
	ApplyHarnessHeaderMap(req.Header)
}

// NewSessionID returns an OpenCode-shaped session id ("ses_" + 26 chars: 12
// hex of the inverted timestamp, then 14 base62).
func NewSessionID() (string, error) {
	id, err := createID(time.Now())
	if err != nil {
		return "", err
	}
	return "ses_" + id, nil
}

// createID renders 12 hex chars of the bit-flipped millisecond timestamp (so
// ids sort descending by age) followed by 14 random base62 chars.
func createID(t time.Time) (string, error) {
	if t.IsZero() {
		t = time.Now()
	}
	ts := ^(uint64(t.UnixMilli())*0x1000 + uint64(1))
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
// free-tier gate: the 403 FreeTierError returned to non-OpenCode clients, or
// the 426 UpgradeRequired returned to an old OpenCode version. A
// gzip-compressed body (magic bytes 1f 8b) is decompressed first, so the
// check works regardless of Content-Encoding.
func IsFreeTierGateError(body []byte) bool {
	lower := errorText(body)
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

// IsFreeUsageLimitError reports whether an upstream error body is Zen's 429
// FreeUsageLimitError. It is the per-IP daily budget for free models, which
// the genuine client also hits, and Zen also returns it to a request that
// fails the free-tier check (no <env> block, no read tool). A gzip body is
// decompressed first.
func IsFreeUsageLimitError(body []byte) bool {
	lower := errorText(body)
	return strings.Contains(lower, "freeusagelimiterror") || strings.Contains(lower, "free usage limit reached")
}

// errorText returns an error body lowercased for phrase matching, gunzipped
// first when it is gzip.
func errorText(body []byte) string {
	if decoded, ok := gunzipBody(body); ok {
		body = decoded
	}
	return strings.ToLower(string(body))
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

// gateHint names what the free-tier check wants from a request.
const gateHint = "gate wants an opencode UA, valid x-opencode-session, stream:true body, read tool, and <env>Working directory:</env> in system message"

// WarnFreeTierGate logs one warning for a Zen free-tier gate response or a
// free usage limit response. Other bodies log nothing.
func WarnFreeTierGate(model string, status int, body []byte) {
	switch {
	case IsFreeTierGateError(body):
		slog.Warn("zen free-tier gate rejected request",
			"model", model,
			"status", status,
			"hint", gateHint)
	case IsFreeUsageLimitError(body):
		slog.Warn("zen free usage limit reached",
			"model", model,
			"status", status,
			"hint", "either the daily free budget of this egress IP is spent, or the request failed the free-tier check ("+gateHint+")")
	}
}

// ObserveFreeTierGate reads a 403 (FreeTierError), 426 (UpgradeRequired) or
// 429 (FreeUsageLimitError) response body (up to 1 MiB), logs the warning
// when it is one of Zen's free-tier rejections, and restores the body so the
// downstream reader sees identical bytes: the full body, or — past the limit
// or after a read error — the consumed prefix chained back onto the original
// reader. A read error is logged rather than dropped, and the truncated bytes
// are never substituted for the body. Other responses are never read.
func ObserveFreeTierGate(resp *http.Response, model string) {
	if resp == nil {
		return
	}
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusUpgradeRequired, http.StatusTooManyRequests:
	default:
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
