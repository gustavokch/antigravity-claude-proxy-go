package zen

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/transport"
)

// Zen meters its free models per public egress IP: a daily budget that resets
// at 00:00 UTC, after which every request from that IP — the genuine OpenCode
// client's included — gets 429 FreeUsageLimitError. Another IP has its own
// budget, so the only way to keep going is to leave from a different address.
// The fallback egress is a proxy (in practice a SOCKS5 server on another host
// of the tailnet) that Zen traffic moves to once the direct IP is exhausted.

// EgressConfig is the live egress configuration for Zen generation requests.
type EgressConfig struct {
	// FallbackProxy is the proxy URL (socks5, socks5h, http or https) Zen
	// requests go through once the direct egress hits the free usage limit.
	// Empty disables the failover.
	FallbackProxy string
}

// ParseFallbackProxy validates a fallback proxy URL. Empty input means no
// fallback and returns nil without error.
func ParseFallbackProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Zen fallback proxy %q: %w", raw, err)
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return nil, fmt.Errorf("invalid Zen fallback proxy %q: scheme must be socks5, socks5h, http or https", raw)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("invalid Zen fallback proxy %q: host and port are required", raw)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid Zen fallback proxy %q: path, query and fragment are not allowed", raw)
	}
	return u, nil
}

type egressRoute int

const (
	routeDirect egressRoute = iota
	routeFallback
)

func (r egressRoute) String() string {
	if r == routeFallback {
		return "fallback"
	}
	return "direct"
}

var (
	// fallbackURL and the failover client built on it are guarded by tlsMu,
	// so a TLS disguise change rebuilds both legs with the new handshake.
	fallbackURL    *url.URL
	failoverClient *http.Client

	// exhaustedUntil records, per route, when Zen's free usage limit is
	// expected to lift.
	exhaustMu      sync.Mutex
	exhaustedUntil [2]time.Time

	// now is replaced in tests.
	now = time.Now
)

// SetEgressConfig replaces the live egress configuration. An invalid proxy URL
// disables the failover and is returned as an error. Changing the proxy forgets
// the old fallback's exhaustion; the direct egress keeps its own.
func SetEgressConfig(cfg EgressConfig) error {
	u, err := ParseFallbackProxy(cfg.FallbackProxy)
	tlsMu.Lock()
	changed := urlString(fallbackURL) != urlString(u)
	fallbackURL = u
	failoverClient = nil
	tlsMu.Unlock()
	if changed {
		exhaustMu.Lock()
		exhaustedUntil[routeFallback] = time.Time{}
		exhaustMu.Unlock()
	}
	return err
}

func urlString(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// directLocked is the round tripper that leaves from this host: the utls
// transport while the disguise is on, the shared plain one otherwise. Callers
// hold tlsMu.
func directLocked() http.RoundTripper {
	if tr := transportLocked(); tr != nil {
		return tr
	}
	return upstreamTransport
}

// fallbackLocked builds the transport that leaves through the fallback proxy,
// presenting the same handshake as the direct leg tunnelled through it.
// Callers hold tlsMu and have checked fallbackURL.
func fallbackLocked() http.RoundTripper {
	base := transport.Upstream()
	base.Proxy = http.ProxyURL(fallbackURL)
	if tlsCfg.Enabled {
		if hello, err := bunReplay(); err == nil && hello != nil {
			return hello.Transport(base)
		}
	}
	return base
}

// GenerationClient returns the client for Zen requests that spend the free
// budget: TLSClient() while no fallback proxy is configured, else one client
// (cached per configuration) whose transport fails over between the direct
// and fallback egress. Like TLSClient it carries no timeout: streaming
// callers own their deadlines.
func GenerationClient() *http.Client {
	tlsMu.Lock()
	if fallbackURL == nil {
		tlsMu.Unlock()
		return TLSClient()
	}
	defer tlsMu.Unlock()
	if failoverClient == nil {
		failoverClient = &http.Client{Transport: &failoverTransport{legs: [2]http.RoundTripper{
			routeDirect:   directLocked(),
			routeFallback: fallbackLocked(),
		}}}
	}
	return failoverClient
}

// GenerationTransport is GenerationClient's round tripper, for callers that
// bring their own HTTP layer (the reverse proxy).
func GenerationTransport() http.RoundTripper {
	return GenerationClient().Transport
}

// failoverTransport sends a request on the egress Zen has not exhausted and,
// when Zen answers 429 FreeUsageLimitError, replays it once on the other one.
type failoverTransport struct {
	legs [2]http.RoundTripper
}

func (f *failoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t := now()
	first, second := routeDirect, routeFallback
	if exhausted(routeDirect, t) && !exhausted(routeFallback, t) {
		first, second = routeFallback, routeDirect
	}
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil

	resp, err := f.legs[first].RoundTrip(req)
	if err != nil || !hitFreeUsageLimit(resp) {
		return resp, err
	}
	until := markExhausted(first, resp, t)
	if exhausted(second, t) || !replayable {
		slog.Warn("zen free usage limit reached; no other egress available",
			"egress", first.String(), "until", until.Format(time.RFC3339))
		return resp, nil
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return resp, nil
		}
		retry.Body = body
	}
	slog.Warn("zen free usage limit reached; failing over",
		"from", first.String(), "to", second.String(), "until", until.Format(time.RFC3339))
	next, err := f.legs[second].RoundTrip(retry)
	if err != nil {
		slog.Error("zen egress failover failed; returning the original 429",
			"egress", second.String(), "error", err)
		return resp, nil
	}
	resp.Body.Close()
	if hitFreeUsageLimit(next) {
		markExhausted(second, next, t)
		slog.Warn("zen free usage limit reached on both egress routes", "egress", second.String())
	}
	return next, nil
}

func exhausted(r egressRoute, t time.Time) bool {
	exhaustMu.Lock()
	defer exhaustMu.Unlock()
	return t.Before(exhaustedUntil[r])
}

// markExhausted records when r should be tried again: after the 429's
// Retry-After when Zen sends one, else at the next 00:00 UTC budget reset.
func markExhausted(r egressRoute, resp *http.Response, t time.Time) time.Time {
	until := nextUTCMidnight(t)
	if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
		until = t.Add(time.Duration(secs) * time.Second)
	}
	exhaustMu.Lock()
	exhaustedUntil[r] = until
	exhaustMu.Unlock()
	return until
}

func nextUTCMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

// freeUsageErrorCap bounds how much of a 429 body is read to classify it.
const freeUsageErrorCap = 64 << 10

// hitFreeUsageLimit reports whether resp is Zen's 429 FreeUsageLimitError.
// A 429 body is read and put back, so the caller still sees it whole.
func hitFreeUsageLimit(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests || resp.Body == nil {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, freeUsageErrorCap))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	if err != nil {
		return false
	}
	return IsFreeUsageLimitError(head)
}
