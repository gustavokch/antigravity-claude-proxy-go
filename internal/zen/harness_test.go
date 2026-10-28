package zen

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDefaultHarnessConstants(t *testing.T) {
	if DefaultVersion != "2.0.20" {
		t.Errorf("DefaultVersion = %q, want %q", DefaultVersion, "2.0.20")
	}
	if DefaultHarnessClient != "cli" {
		t.Errorf("DefaultHarnessClient = %q, want %q", DefaultHarnessClient, "cli")
	}
	if DefaultProject != "global" {
		t.Errorf("DefaultProject = %q, want %q", DefaultProject, "global")
	}
	if HeaderProject != "x-opencode-project" {
		t.Errorf("HeaderProject = %q", HeaderProject)
	}
	if HeaderSession != "x-opencode-session" {
		t.Errorf("HeaderSession = %q", HeaderSession)
	}
	if HeaderClient != "x-opencode-client" {
		t.Errorf("HeaderClient = %q", HeaderClient)
	}
	if HeaderUA != "User-Agent" {
		t.Errorf("HeaderUA = %q", HeaderUA)
	}
}

// withHarness installs cfg for the test and restores the previous value.
func withHarness(t *testing.T, cfg HarnessConfig) {
	t.Helper()
	prev := GetHarnessConfig()
	SetHarnessConfig(cfg)
	t.Cleanup(func() { SetHarnessConfig(prev) })
}

func TestApplyHarnessHeaderMap(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: true})
		hdr := http.Header{}
		ApplyHarnessHeaderMap(hdr, "")

		if got := hdr.Get(HeaderUA); got != "opencode/latest/"+DefaultVersion+"/cli" {
			t.Errorf("User-Agent = %q, want %q", got, "opencode/latest/"+DefaultVersion+"/cli")
		}
		if got := hdr.Get(HeaderClient); got != DefaultHarnessClient {
			t.Errorf("x-opencode-client = %q, want %q", got, DefaultHarnessClient)
		}
		if got := hdr.Get(HeaderProject); got != DefaultProject {
			t.Errorf("x-opencode-project = %q, want %q", got, DefaultProject)
		}
		session := hdr.Get(HeaderSession)
		if !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(session) {
			t.Errorf("x-opencode-session = %q, want ses_ + 26 base62 chars", session)
		}
		requireSessionMirrors(t, hdr, session)
	})

	t.Run("overrides", func(t *testing.T) {
		withHarness(t, HarnessConfig{
			Enabled: true,
			Version: "9.9.9",
			Client:  "desktop",
			Project: "abc123",
		})
		hdr := http.Header{}
		ApplyHarnessHeaderMap(hdr, "")

		// OpenCode ends the UA with the same app name it sends as
		// x-opencode-client (packages/core/src/app.ts useragent).
		if got := hdr.Get(HeaderUA); got != "opencode/latest/9.9.9/desktop" {
			t.Errorf("User-Agent = %q, want %q", got, "opencode/latest/9.9.9/desktop")
		}
		if got := hdr.Get(HeaderClient); got != "desktop" {
			t.Errorf("x-opencode-client = %q, want %q", got, "desktop")
		}
		if got := hdr.Get(HeaderProject); got != "abc123" {
			t.Errorf("x-opencode-project = %q, want %q", got, "abc123")
		}
	})

	t.Run("whitespace-only values fall back to defaults", func(t *testing.T) {
		withHarness(t, HarnessConfig{
			Enabled: true,
			Version: "  ",
			Client:  "\t\n",
			Project: "   ",
		})
		hdr := http.Header{}
		ApplyHarnessHeaderMap(hdr, "")

		if got := hdr.Get(HeaderUA); got != "opencode/latest/"+DefaultVersion+"/cli" {
			t.Errorf("User-Agent = %q, want %q", got, "opencode/latest/"+DefaultVersion+"/cli")
		}
		if got := hdr.Get(HeaderClient); got != DefaultHarnessClient {
			t.Errorf("x-opencode-client = %q, want %q", got, DefaultHarnessClient)
		}
		if got := hdr.Get(HeaderProject); got != DefaultProject {
			t.Errorf("x-opencode-project = %q, want %q", got, DefaultProject)
		}
	})

	t.Run("disabled is a no-op", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: false, Version: "9.9.9"})
		hdr := http.Header{}
		ApplyHarnessHeaderMap(hdr, "")

		if len(hdr) != 0 {
			t.Errorf("disabled harness wrote headers: %v", hdr)
		}
	})

	t.Run("overwrites pre-existing identity headers", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: true})
		hdr := http.Header{}
		hdr.Set(HeaderUA, "claude-cli/2.0.0")
		ApplyHarnessHeaderMap(hdr, "")

		if got := hdr.Get(HeaderUA); got != "opencode/latest/"+DefaultVersion+"/cli" {
			t.Errorf("User-Agent = %q, want it overwritten with the disguise", got)
		}
	})

	t.Run("env overrides config", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: true, Version: "9.9.9", Client: "desktop"})
		t.Setenv("OPENCODE_VERSION", "8.8.8")
		t.Setenv("OPENCODE_CLIENT", "acp")
		hdr := http.Header{}
		ApplyHarnessHeaderMap(hdr, "")

		if got := hdr.Get(HeaderUA); got != "opencode/latest/8.8.8/acp" {
			t.Errorf("User-Agent = %q, want OPENCODE_VERSION and OPENCODE_CLIENT to win", got)
		}
		if got := hdr.Get(HeaderClient); got != "acp" {
			t.Errorf("x-opencode-client = %q, want OPENCODE_CLIENT to win", got)
		}
	})

	t.Run("nil header map does not panic", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: true})
		ApplyHarnessHeaderMap(nil, "")
	})
}

func TestApplyHarnessHeaders(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: true})

	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	ApplyHarnessHeaders(req)

	if got := req.Header.Get(HeaderUA); got != "opencode/latest/"+DefaultVersion+"/cli" {
		t.Errorf("User-Agent = %q, want %q", got, "opencode/latest/"+DefaultVersion+"/cli")
	}
	if got := req.Header.Get(HeaderClient); got != DefaultHarnessClient {
		t.Errorf("x-opencode-client = %q, want %q", got, DefaultHarnessClient)
	}
	if got := req.Header.Get(HeaderProject); got != DefaultProject {
		t.Errorf("x-opencode-project = %q, want %q", got, DefaultProject)
	}
	session := req.Header.Get(HeaderSession)
	if !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(session) {
		t.Errorf("x-opencode-session = %q, want ses_ + 26 base62 chars", session)
	}
	requireSessionMirrors(t, req.Header, session)

	ApplyHarnessHeaders(nil) // must not panic
}

// The genuine client keeps one session id for a whole session, and Zen routes
// x-session-affinity on it: every turn of a conversation must send the same
// id, another conversation a different one, and a request that names no
// conversation a fresh one.
func TestSessionIDPerConversation(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: true})
	send := func(ctx context.Context) http.Header {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://opencode.ai/zen/v1/messages", nil)
		ApplyHarnessHeaders(req)
		requireSessionMirrors(t, req.Header, req.Header.Get(HeaderSession))
		return req.Header
	}
	conversation := WithConversation(context.Background(), "session-a\x00first prompt")
	first, again := send(conversation), send(conversation)
	if sid := first.Get(HeaderSession); sid == "" || again.Get(HeaderSession) != sid {
		t.Errorf("turns of one conversation sent %q then %q, want one id", sid, again.Get(HeaderSession))
	}
	other := send(WithConversation(context.Background(), "session-b\x00first prompt"))
	if other.Get(HeaderSession) == first.Get(HeaderSession) {
		t.Error("a second conversation reused the first one's session id")
	}
	if send(context.Background()).Get(HeaderSession) == send(context.Background()).Get(HeaderSession) {
		t.Error("requests that name no conversation shared a session id")
	}
	hdr := http.Header{}
	ApplyHarnessHeaderMap(hdr, "session-a\x00first prompt")
	if got := hdr.Get(HeaderSession); got != first.Get(HeaderSession) {
		t.Errorf("header map for the same conversation = %q, want %q", got, first.Get(HeaderSession))
	}
}

// The id table is bounded: past sessionCapacity the oldest conversation is
// forgotten and gets a new id, while a newer one keeps its id.
func TestSessionIDForgetsOldestConversation(t *testing.T) {
	reset := func() {
		sessions.Lock()
		sessions.ids, sessions.order, sessions.next = make(map[string]string), nil, 0
		sessions.Unlock()
	}
	reset()
	t.Cleanup(reset)
	oldest, _ := sessionIDFor("evict-0")
	second, _ := sessionIDFor("evict-1")
	for i := 2; i <= sessionCapacity; i++ {
		if _, err := sessionIDFor(fmt.Sprintf("evict-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := sessionIDFor("evict-1"); got != second {
		t.Errorf("second conversation id = %q after one eviction, want %q kept", got, second)
	}
	if got, _ := sessionIDFor("evict-0"); got == oldest {
		t.Errorf("oldest conversation kept %q past capacity, want a new id", got)
	}
}

func TestHarnessIDs(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		sid, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(sid) {
			t.Fatalf("session id %q does not match ses_ + 26 base62 chars", sid)
		}
		if seen[sid] {
			t.Fatalf("duplicate id generated: %q", sid)
		}
		seen[sid] = true
	}
}

func TestCreateIDTimestampEncoding(t *testing.T) {
	when := time.UnixMilli(1_700_000_000_000)

	id, err := createID(when)
	if err != nil {
		t.Fatalf("createID: %v", err)
	}
	if len(id) != idLength {
		t.Errorf("id length = %d, want %d", len(id), idLength)
	}
	// 12 hex chars of ^uint64(ms*0x1000 + 1); the plain form is bcfe56800001.
	if got, want := id[:12], "4301a97ffffe"; got != want {
		t.Errorf("timestamp prefix = %q, want %q", got, want)
	}
	// Ids must sort in reverse chronological order.
	later, err := createID(when.Add(time.Second))
	if err != nil {
		t.Fatalf("createID later: %v", err)
	}
	if !(later[:12] < id[:12]) {
		t.Errorf("prefix not descending: later=%q now=%q", later[:12], id[:12])
	}
}

func TestIsFreeTierGateError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "exact free tier gate",
			body: `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}`,
			want: true,
		},
		{
			name: "version gate variant (observed 403 body)",
			body: `{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode 1.18.0 or newer is required to use the free tier"}}`,
			want: true,
		},
		{
			name: "observed 426 upgrade required body",
			body: `{"type":"error","error":{"type":"UpgradeRequired","message":"Error from provider (Console): OpenCode 1.18.0 or newer is required to use the free tier"}}`,
			want: true,
		},
		{
			name: "freetiererror type only",
			body: `{"error":{"type":"freetiererror"}}`,
			want: true,
		},
		{
			name: "case-insensitive message",
			body: `{"error":{"message":"opencode's FREE TIER can only be used from within opencode"}}`,
			want: true,
		},
		{name: "free usage limit is not the gate", body: freeUsageLimitBody, want: false},
		{name: "generic 403", body: `{"type":"error","error":{"type":"permission_error","message":"insufficient credits"}}`, want: false},
		{name: "not found", body: `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`, want: false},
		{name: "empty", body: ``, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsFreeTierGateError([]byte(tc.body)); got != tc.want {
				t.Errorf("IsFreeTierGateError(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// freeUsageLimitBody is the observed 429 body: Zen's per-IP free budget, which
// Zen also returns to a request that fails its free-tier check.
const freeUsageLimitBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`

func TestIsFreeUsageLimitError(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(freeUsageLimitBody))
	_ = zw.Close()

	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{name: "observed 429 body", body: []byte(freeUsageLimitBody), want: true},
		{name: "gzip body", body: gz.Bytes(), want: true},
		{name: "translated Anthropic envelope", body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Zen: Free usage limit reached."}}`), want: true},
		{name: "free-tier gate", body: []byte(`{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`), want: false},
		{name: "plain rate limit", body: []byte(`{"type":"error","error":{"type":"RateLimitError","message":"Rate limit exceeded. Please try again later."}}`), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsFreeUsageLimitError(tc.body); got != tc.want {
				t.Errorf("IsFreeUsageLimitError(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsFreeTierGateError_GzipBody(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`))
	_ = zw.Close()

	if !IsFreeTierGateError(buf.Bytes()) {
		t.Error("gzip-compressed gate body not detected")
	}
	// Truncated/corrupt gzip falls back to raw matching without panicking.
	if IsFreeTierGateError([]byte{0x1f, 0x8b, 0x00}) {
		t.Error("corrupt gzip must not report a gate")
	}
}

func TestObserveFreeTierGate_LogsAndPreservesBody(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	gate := `{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader(gate)),
	}
	ObserveFreeTierGate(resp, "mimo-v2.6-flash-free")

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(got) != gate {
		t.Errorf("body changed:\n got %s\nwant %s", got, gate)
	}
	if !strings.Contains(logBuf.String(), "zen free-tier gate rejected request") ||
		!strings.Contains(logBuf.String(), "model=mimo-v2.6-flash-free") {
		t.Errorf("gate warning missing:\n%s", logBuf.String())
	}

	// Non-403 responses are never read.
	logBuf.Reset()
	tracker := &trackingBody{Reader: strings.NewReader("stream")}
	okResp := &http.Response{StatusCode: http.StatusOK, Body: tracker}
	ObserveFreeTierGate(okResp, "m")
	if tracker.read != 0 {
		t.Errorf("non-403 body was read (%d bytes)", tracker.read)
	}
	if logBuf.Len() != 0 {
		t.Errorf("unexpected log for non-403:\n%s", logBuf.String())
	}

	// Non-gate 403: body preserved, nothing logged.
	logBuf.Reset()
	plain := `{"error":{"message":"insufficient credits"}}`
	resp2 := &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(plain))}
	ObserveFreeTierGate(resp2, "m")
	got2, _ := io.ReadAll(resp2.Body)
	if string(got2) != plain || logBuf.Len() != 0 {
		t.Errorf("plain 403: body=%s log=%q", got2, logBuf.String())
	}
}

// UA below 1.18.0 gets 426 UpgradeRequired; it is a gate response too and
// must be observed and logged like the 403.
func TestObserveFreeTierGate_UpgradeRequired(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	upgrade := `{"type":"error","error":{"type":"UpgradeRequired","message":"Error from provider (Console): OpenCode 1.18.0 or newer is required to use the free tier"}}`
	resp := &http.Response{
		StatusCode: http.StatusUpgradeRequired,
		Body:       io.NopCloser(strings.NewReader(upgrade)),
	}
	ObserveFreeTierGate(resp, "mimo-v2.6-flash-free")

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(got) != upgrade {
		t.Errorf("body changed:\n got %s\nwant %s", got, upgrade)
	}
	if !strings.Contains(logBuf.String(), "zen free-tier gate rejected request") {
		t.Errorf("gate warning missing for 426:\n%s", logBuf.String())
	}
}

// A 429 FreeUsageLimitError means a spent daily budget or a request that
// failed the free-tier check. It is logged as the usage limit, naming both
// causes, not as the gate; a plain 429 logs nothing.
func TestObserveFreeTierGate_FreeUsageLimit(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(freeUsageLimitBody)),
	}
	ObserveFreeTierGate(resp, "deepseek-v4.1-flash-free")

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(got) != freeUsageLimitBody {
		t.Errorf("body changed:\n got %s\nwant %s", got, freeUsageLimitBody)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "zen free usage limit reached") || !strings.Contains(logged, "daily free budget") {
		t.Errorf("usage-limit warning missing:\n%s", logged)
	}
	if strings.Contains(logged, "gate rejected request") {
		t.Errorf("usage limit logged as the free-tier gate:\n%s", logged)
	}

	logBuf.Reset()
	plain := `{"type":"error","error":{"type":"RateLimitError","message":"Rate limit exceeded. Please try again later."}}`
	resp = &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(plain))}
	ObserveFreeTierGate(resp, "m")
	if got, _ := io.ReadAll(resp.Body); string(got) != plain || logBuf.Len() != 0 {
		t.Errorf("plain 429: body=%s log=%q", got, logBuf.String())
	}
}

type trackingBody struct {
	io.Reader
	read int
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackingBody) Close() error { return nil }

// errAfterBytes yields its data once, then keeps returning err — the sticky
// read error a broken upstream connection produces.
type errAfterBytes struct {
	data []byte
	err  error
	pos  int
}

func (r *errAfterBytes) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *errAfterBytes) Close() error { return nil }

// A partial read must not be swallowed: the read error is logged, and the
// restored body still yields the bytes that did arrive plus the same error —
// never truncated bytes passed off as a complete body.
func TestObserveFreeTierGate_ReadErrorPreservesBody(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	gate := `{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`
	boom := errors.New("upstream reset")
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       &errAfterBytes{data: []byte(gate), err: boom},
	}
	ObserveFreeTierGate(resp, "mimo-v2.6-flash-free")

	if !strings.Contains(logBuf.String(), "body read failed") ||
		!strings.Contains(logBuf.String(), "upstream reset") {
		t.Errorf("read error not logged:\n%s", logBuf.String())
	}
	got, err := io.ReadAll(resp.Body)
	if string(got) != gate {
		t.Errorf("partial bytes lost: got %q, want %q", got, gate)
	}
	if !errors.Is(err, boom) {
		t.Errorf("read error swallowed: %v, want %v", err, boom)
	}
}

// A body at or over the sniff limit still gets the gate warning: a real
// rejection must never hide behind the size cutoff. The reader is handed
// back as a continuing stream so no byte is lost.
func TestObserveFreeTierGate_LargeBodyStillLogs(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	gate := `{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`
	// gzip body over 1 MiB: a compressible prefix carrying the gate phrase
	// followed by incompressible padding, so the sniffed prefix decompresses
	// cleanly while the compressed body exceeds the limit.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(gate))
	_, _ = zw.Write(make([]byte, 4<<20))
	rnd := make([]byte, 2<<20)
	if _, err := rand.Read(rnd); err != nil {
		t.Fatal(err)
	}
	_, _ = zw.Write(rnd)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 1<<20 {
		t.Fatalf("compressed body = %d bytes, want >= 1 MiB", buf.Len())
	}

	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(bytes.NewReader(buf.Bytes())),
	}
	ObserveFreeTierGate(resp, "mimo-v2.6-flash-free")

	if !strings.Contains(logBuf.String(), "zen free-tier gate rejected request") ||
		!strings.Contains(logBuf.String(), "model=mimo-v2.6-flash-free") {
		t.Errorf("gate warning missing for large body:\n%s", logBuf.String())
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if len(got) != buf.Len() {
		t.Errorf("body truncated: got %d bytes, want %d", len(got), buf.Len())
	}
}
