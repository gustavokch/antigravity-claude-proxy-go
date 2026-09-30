package zen

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDefaultHarnessConstants(t *testing.T) {
	if DefaultVersion != "1.18.31" {
		t.Errorf("DefaultVersion = %q, want %q", DefaultVersion, "1.18.31")
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
	if HeaderRequest != "x-opencode-request" {
		t.Errorf("HeaderRequest = %q", HeaderRequest)
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
		ApplyHarnessHeaderMap(hdr)

		if got := hdr.Get(HeaderUA); got != "opencode/"+DefaultVersion {
			t.Errorf("User-Agent = %q, want %q", got, "opencode/"+DefaultVersion)
		}
		if got := hdr.Get(HeaderClient); got != DefaultHarnessClient {
			t.Errorf("x-opencode-client = %q, want %q", got, DefaultHarnessClient)
		}
		if got := hdr.Get(HeaderProject); got != DefaultProject {
			t.Errorf("x-opencode-project = %q, want %q", got, DefaultProject)
		}
		if got := hdr.Get(HeaderSession); !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(got) {
			t.Errorf("x-opencode-session = %q, want ses_ + 26 base62 chars", got)
		}
		if got := hdr.Get(HeaderRequest); !regexp.MustCompile(`^msg_[0-9A-Za-z]{26}$`).MatchString(got) {
			t.Errorf("x-opencode-request = %q, want msg_ + 26 base62 chars", got)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		withHarness(t, HarnessConfig{
			Enabled: true,
			Version: "9.9.9",
			Client:  "desktop",
			Project: "abc123",
		})
		hdr := http.Header{}
		ApplyHarnessHeaderMap(hdr)

		if got := hdr.Get(HeaderUA); got != "opencode/9.9.9" {
			t.Errorf("User-Agent = %q, want %q", got, "opencode/9.9.9")
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
		ApplyHarnessHeaderMap(hdr)

		if got := hdr.Get(HeaderUA); got != "opencode/"+DefaultVersion {
			t.Errorf("User-Agent = %q, want %q", got, "opencode/"+DefaultVersion)
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
		ApplyHarnessHeaderMap(hdr)

		if len(hdr) != 0 {
			t.Errorf("disabled harness wrote headers: %v", hdr)
		}
	})

	t.Run("overwrites pre-existing identity headers", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: true})
		hdr := http.Header{}
		hdr.Set(HeaderUA, "claude-cli/2.0.0")
		ApplyHarnessHeaderMap(hdr)

		if got := hdr.Get(HeaderUA); got != "opencode/"+DefaultVersion {
			t.Errorf("User-Agent = %q, want it overwritten with the disguise", got)
		}
	})

	t.Run("env overrides config", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: true, Version: "9.9.9", Client: "desktop"})
		t.Setenv("OPENCODE_VERSION", "8.8.8")
		t.Setenv("OPENCODE_CLIENT", "cli")
		hdr := http.Header{}
		ApplyHarnessHeaderMap(hdr)

		if got := hdr.Get(HeaderUA); got != "opencode/8.8.8" {
			t.Errorf("User-Agent = %q, want OPENCODE_VERSION to win", got)
		}
		if got := hdr.Get(HeaderClient); got != "cli" {
			t.Errorf("x-opencode-client = %q, want OPENCODE_CLIENT to win", got)
		}
	})

	t.Run("nil header map does not panic", func(t *testing.T) {
		withHarness(t, HarnessConfig{Enabled: true})
		ApplyHarnessHeaderMap(nil)
	})
}

func TestApplyHarnessHeaders(t *testing.T) {
	withHarness(t, HarnessConfig{Enabled: true})

	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	ApplyHarnessHeaders(req)

	if got := req.Header.Get(HeaderUA); got != "opencode/"+DefaultVersion {
		t.Errorf("User-Agent = %q, want %q", got, "opencode/"+DefaultVersion)
	}
	if got := req.Header.Get(HeaderClient); got != DefaultHarnessClient {
		t.Errorf("x-opencode-client = %q, want %q", got, DefaultHarnessClient)
	}
	if got := req.Header.Get(HeaderProject); got != DefaultProject {
		t.Errorf("x-opencode-project = %q, want %q", got, DefaultProject)
	}
	if got := req.Header.Get(HeaderSession); !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(got) {
		t.Errorf("x-opencode-session = %q, want ses_ + 26 base62 chars", got)
	}
	if got := req.Header.Get(HeaderRequest); !regexp.MustCompile(`^msg_[0-9A-Za-z]{26}$`).MatchString(got) {
		t.Errorf("x-opencode-request = %q, want msg_ + 26 base62 chars", got)
	}

	ApplyHarnessHeaders(nil) // must not panic
}

func TestHarnessIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		sid, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		rid, err := NewRequestID()
		if err != nil {
			t.Fatalf("NewRequestID: %v", err)
		}
		if !regexp.MustCompile(`^ses_[0-9A-Za-z]{26}$`).MatchString(sid) {
			t.Fatalf("session id %q does not match ses_ + 26 base62 chars", sid)
		}
		if !regexp.MustCompile(`^msg_[0-9A-Za-z]{26}$`).MatchString(rid) {
			t.Fatalf("request id %q does not match msg_ + 26 base62 chars", rid)
		}
		if seen[sid] || seen[rid] {
			t.Fatalf("duplicate id generated: %q / %q", sid, rid)
		}
		seen[sid] = true
		seen[rid] = true
	}
}

func TestCreateIDTimestampEncoding(t *testing.T) {
	when := time.UnixMilli(1_700_000_000_000)

	ascending, err := createID(false, when)
	if err != nil {
		t.Fatalf("createID ascending: %v", err)
	}
	descending, err := createID(true, when)
	if err != nil {
		t.Fatalf("createID descending: %v", err)
	}

	wantAsc := "bcfe56800001" // low 48 bits of ms<<12|1
	if got := ascending[:12]; got != wantAsc {
		t.Errorf("ascending timestamp prefix = %q, want %q", got, wantAsc)
	}
	// 12 hex chars of ^uint64(ms*0x1000 + 1) — inversion must differ from
	// the ascending form and stay 12 chars.
	if len(descending) != idLength {
		t.Errorf("descending id length = %d, want %d", len(descending), idLength)
	}
	if got, want := descending[:12], "4301a97ffffe"; got != want {
		t.Errorf("descending timestamp prefix = %q, want %q", got, want)
	}
	if descending[:12] == ascending[:12] {
		t.Errorf("descending timestamp prefix must be inverted, both = %q", ascending[:12])
	}
	// Descending ids must sort in reverse chronological order.
	later, err := createID(true, when.Add(time.Second))
	if err != nil {
		t.Fatalf("createID later: %v", err)
	}
	if !(later[:12] < descending[:12]) {
		t.Errorf("descending prefix not monotonic: later=%q now=%q", later[:12], descending[:12])
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
