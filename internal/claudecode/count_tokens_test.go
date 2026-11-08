package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
)

// countCapture is Claude Code 2.1.311's own count_tokens traffic, recorded
// from /context.
var countCapture = filepath.Join("..", "..", ".reference", "claude-code-count-tokens-20261107.jsonl")

// capturedCount is the first count_tokens request in countCapture: its path,
// its header names in wire order, and the value of every header the capture
// kept in clear (credentials are recorded redacted, as objects).
func capturedCount(t *testing.T) (path string, names []string, values map[string]string) {
	t.Helper()
	data, err := os.ReadFile(countCapture)
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Method  string              `json:"method"`
			Path    string              `json:"path"`
			Headers [][]json.RawMessage `json:"headers"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("capture record: %v", err)
		}
		if record.Method != http.MethodPost || !strings.HasPrefix(record.Path, "/v1/messages/count_tokens") {
			continue
		}
		values = make(map[string]string)
		for _, pair := range record.Headers {
			var name, value string
			if len(pair) != 2 || json.Unmarshal(pair[0], &name) != nil {
				t.Fatalf("capture header %s is not a [name, value] pair", pair)
			}
			names = append(names, name)
			if json.Unmarshal(pair[1], &value) == nil {
				values[name] = value
			}
		}
		return record.Path, names, values
	}
	t.Fatalf("%s holds no count_tokens request", countCapture)
	return "", nil, nil
}

// wireRequest is one request as a raw HTTP/1.1 listener read it.
type wireRequest struct {
	target string
	names  []string
	values map[string]string
	body   string
}

// rawCountServer answers one count with {"input_tokens":123} and hands back the
// request as the wire carried it: header order and name case are the subject,
// and net/http's server keeps neither.
func rawCountServer(t *testing.T) (string, <-chan wireRequest) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan wireRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		request, err := readWireRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		got <- request
		const answer = `{"input_tokens":123}`
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(answer), answer)
	}()
	return "http://" + ln.Addr().String(), got
}

func readWireRequest(br *bufio.Reader) (wireRequest, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return wireRequest{}, err
	}
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return wireRequest{}, fmt.Errorf("request line %q", line)
	}
	request := wireRequest{target: fields[1], values: make(map[string]string)}
	length := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return wireRequest{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, _ := strings.Cut(line, ": ")
		request.names = append(request.names, name)
		request.values[name] = value
		if strings.EqualFold(name, "Content-Length") {
			length, _ = strconv.Atoi(value)
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return wireRequest{}, err
	}
	request.body = string(body)
	return request, nil
}

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// A normalized count is held to the committed capture: the same header names
// in the same order, and the same value for every header that does not vary
// by request, machine or release. The body goes out as given.
func TestClient_CountTokensSendsTheCapturedIdentity(t *testing.T) {
	path, names, values := capturedCount(t)
	url, sent := rawCountServer(t)
	body := `{"messages":[{"role":"user","content":"hi"}],"model":"claude-sonnet-5","tools":[]}`
	identity, normalize := IdentityConfig{}.WireIdentity("", "session-count")
	resp, err := NewClient(url, nil).CountTokens(context.Background(), CountTokensRequest{
		Token: "sk-ant-oat01-count", Body: []byte(body), Normalize: normalize, Identity: identity,
		// A foreign caller's headers: none of them may reach the wire.
		ClientHeaders: http.Header{
			"User-Agent":          {"foreign/1.0"},
			"X-Stainless-Timeout": {"600"},
			"Anthropic-Beta":      {ccidentity.AdvisorToolBeta},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var got wireRequest
	select {
	case got = <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("no count_tokens request reached the listener")
	}
	if got.target != path {
		t.Errorf("request target = %q, want captured %q", got.target, path)
	}
	if !slices.Equal(got.names, names) {
		t.Errorf("header names in wire order:\n got %q\nwant %q", got.names, names)
	}
	// Values that vary per request (credential, ids, body length), per machine
	// (Host, X-Stainless-OS and -Arch) or per Claude Code release, which the
	// client-drift pins move from the messages capture (X-Stainless-Package-Version,
	// X-Stainless-Runtime-Version). The User-Agent carries the release too and is
	// checked below with only its version moved.
	varying := map[string]bool{
		"Authorization": true, "Host": true, "Content-Length": true,
		"X-Claude-Code-Session-Id": true, "x-client-request-id": true,
		"X-Stainless-OS": true, "X-Stainless-Arch": true,
		"User-Agent": true, "X-Stainless-Package-Version": true, "X-Stainless-Runtime-Version": true,
	}
	for _, name := range names {
		if varying[name] {
			continue
		}
		if got.values[name] != values[name] {
			t.Errorf("%s = %q, want captured %q", name, got.values[name], values[name])
		}
	}
	if got.values["Authorization"] != "Bearer sk-ant-oat01-count" {
		t.Errorf("Authorization = %q, want the account's bearer token", got.values["Authorization"])
	}
	for _, name := range []string{"X-Claude-Code-Session-Id", "x-client-request-id"} {
		if !uuidShape.MatchString(got.values[name]) {
			t.Errorf("%s = %q, want a UUID", name, got.values[name])
		}
	}
	if want := capturedCountUserAgent(values); got.values["User-Agent"] != want {
		t.Errorf("User-Agent = %q, want the captured %q", got.values["User-Agent"], want)
	}
	if got.values["Content-Length"] != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got.values["Content-Length"], len(body))
	}
	if got.body != body {
		t.Errorf("body = %s, want the caller's bytes %s", got.body, body)
	}
}

// capturedCountUserAgent is the captured count User-Agent with its version
// replaced by the one the proxy claims, which client-drift moves each release.
func capturedCountUserAgent(values map[string]string) string {
	product, mode, _ := strings.Cut(values["User-Agent"], " ")
	name, _, _ := strings.Cut(product, "/")
	return name + "/" + ccidentity.ClientVersion + " " + mode
}

// Claude Code counts only from /context, which only an interactive session
// has, so a count carries the interactive User-Agent whatever entrypoint the
// messages path claims. An explicit User-Agent override still wins.
func TestClient_CountTokensUserAgentIsTheInteractiveOne(t *testing.T) {
	_, _, values := capturedCount(t)
	captured := capturedCountUserAgent(values)
	for _, tc := range []struct {
		name   string
		config IdentityConfig
		want   string
	}{
		{"default entrypoint", IdentityConfig{}, captured},
		{"sdk-cli entrypoint", IdentityConfig{Entrypoint: ccidentity.EntrypointSDKCLI}, captured},
		{"cli entrypoint", IdentityConfig{Entrypoint: ccidentity.EntrypointInteractive}, captured},
		{"user agent override", IdentityConfig{Entrypoint: ccidentity.EntrypointSDKCLI, UserAgent: "custom/1.0"}, "custom/1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotUA string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotUA = r.Header.Get("User-Agent")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"input_tokens":5}`)
			}))
			defer server.Close()

			identity, normalize := tc.config.WireIdentity("", "session-count")
			resp, err := NewClient(server.URL, nil).CountTokens(context.Background(), CountTokensRequest{
				Token: "sk-ant-oat01-count", Body: []byte(`{"model":"claude-sonnet-5","messages":[]}`),
				Normalize: normalize, Identity: identity,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if gotUA != tc.want {
				t.Errorf("User-Agent = %q, want %q", gotUA, tc.want)
			}
		})
	}
}

// An API key never claims the OAuth identity, on counts as on messages: the
// request goes to the plain path with the caller's betas.
func TestClient_CountTokensWithAnAPIKeyClaimsNoIdentity(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotApp, gotClass, gotBeta, gotVersion, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotKey = r.Header.Get("x-api-key")
		gotApp = r.Header.Get("x-app")
		gotClass = r.Header.Get("x-claude-code-request-class")
		gotBeta = r.Header.Get("anthropic-beta")
		gotVersion = r.Header.Get("anthropic-version")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":5}`)
	}))
	defer server.Close()

	body := `{"model":"claude-sonnet-5","messages":[]}`
	identity, _ := IdentityConfig{}.WireIdentity("", "session-count")
	resp, err := NewClient(server.URL, nil).CountTokens(context.Background(), CountTokensRequest{
		Token: "sk-ant-api03-key", Body: []byte(body), Normalize: true, Identity: identity,
		ClientHeaders: http.Header{"Anthropic-Beta": {"token-counting-2024-11-01"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotPath != "/v1/messages/count_tokens" || gotQuery != "" {
		t.Errorf("path = %q?%q, want /v1/messages/count_tokens with no query", gotPath, gotQuery)
	}
	if gotKey != "sk-ant-api03-key" {
		t.Errorf("x-api-key = %q, want the API key", gotKey)
	}
	if gotApp != "" || gotClass != "" {
		t.Errorf("x-app %q, request class %q: an API-key count claimed the Claude Code identity", gotApp, gotClass)
	}
	if gotBeta != "token-counting-2024-11-01" || gotVersion != DefaultAnthropicVersion {
		t.Errorf("anthropic-beta %q, anthropic-version %q: want the caller's beta and the default version", gotBeta, gotVersion)
	}
	if gotBody != body {
		t.Errorf("body = %s, want %s", gotBody, body)
	}
}
