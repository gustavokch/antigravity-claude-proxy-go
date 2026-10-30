package ccidentity

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// capturedBodies are three /v1/messages bodies Claude Code 2.1.304 sent, one per
// --print prompt, byte for byte as they left the client. They were captured by
// a local interceptor that answered every request itself, with a placeholder
// API key, so nothing reached Anthropic.
func capturedBodies(t *testing.T) [][]byte {
	t.Helper()
	file, err := os.Open("testdata/claude-code-2.1.304-bodies.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		bodies = append(bodies, bytes.Clone(scanner.Bytes()))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("fixture holds %d bodies, want 3", len(bodies))
	}
	return bodies
}

var capturedCCH = regexp.MustCompile(`cch=([0-9a-f]{5});`)

// The real client's cch is the hash of the body it sends. Re-signing each
// captured body from the placeholder must give back the value it carried.
func TestSignCCHReproducesTheRealClient(t *testing.T) {
	for i, body := range capturedBodies(t) {
		match := capturedCCH.FindSubmatchIndex(body)
		if match == nil {
			t.Fatalf("body %d carries no cch", i)
		}
		want := string(body[match[2]:match[3]])
		copy(body[match[2]:match[3]], "00000")
		signCCH(body, match[2]-len("cch="))
		if got := string(body[match[2]:match[3]]); got != want {
			t.Errorf("body %d: cch = %s, the real client sent %s", i, got, want)
		}
	}
}

// capturedVersion reads the version a fixture body was sent with. The suffix
// hashes that version, so the fixtures stay valid after ClientVersion moves.
var capturedVersion = regexp.MustCompile(`cc_version=(\d+\.\d+\.\d+)\.([0-9a-f]{3});`)

// The real client's cc_version suffix is a hash of its first prompt. Reading
// each captured body as a caller's request must give back the suffix it
// carried: the reminder block the client put before the typed prompt is skipped.
func TestVersionSuffixReproducesTheRealClient(t *testing.T) {
	for i, body := range capturedBodies(t) {
		match := capturedVersion.FindSubmatch(body)
		if match == nil {
			t.Fatalf("body %d carries no cc_version", i)
		}
		turn := TurnOf(body)
		if got := versionSuffix(turn.firstPrompt, string(match[1])); got != string(match[2]) {
			t.Errorf("body %d (prompt %q): suffix = %s, the real client sent %s", i, turn.firstPrompt, got, match[2])
		}
	}
}

// ApplyBody must sign the placeholder in the billing header and nothing else.
// The captured key order puts messages before system, so a user who typed the
// placeholder text would be the first match in the body; signing that one
// would change the user's words and leave the header unsigned.
func TestApplyBodySignsTheBillingHeaderNotMessageContent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"what is cch=00000; for?"}],"model":"claude-opus-4-5","max_tokens":64}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), TurnOf(body))
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	if !bytes.Contains(out, []byte(`"content":"what is cch=00000; for?"`)) {
		t.Fatalf("user content changed:\n%s", out)
	}
	header := regexp.MustCompile(`x-anthropic-billing-header: [^"]*`).Find(out)
	match := capturedCCH.FindSubmatch(header)
	if match == nil || string(match[1]) == "00000" {
		t.Fatalf("billing header left unsigned: %s", header)
	}
	at := bytes.Index(out, header) + bytes.Index(header, []byte("cch="))
	resigned := bytes.Clone(out)
	copy(resigned[at+len("cch="):], "00000")
	signCCH(resigned, at)
	if !bytes.Equal(resigned, out) {
		t.Errorf("cch %s is not the hash of the bytes sent", match[1])
	}
}

// The cch preimage rule is "the first model field", which is the top-level
// one only because the real client writes model first. ApplyBody must keep
// that true whatever order the caller used, and write the rest in the captured
// order with unknown keys last.
func TestApplyBodyWritesTheCapturedKeyOrder(t *testing.T) {
	body := []byte(`{"custom":1,"stream":true,"max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"model":"gpt"}}]}],"system":"s","model":"claude-opus-4-5"}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), TurnOf(body))
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	_, _ = decoder.Token()
	var keys []string
	for decoder.More() {
		key, _ := decoder.Token()
		keys = append(keys, key.(string))
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	want := "model,messages,system,metadata,max_tokens,stream,custom"
	if got := strings.Join(keys, ","); got != want {
		t.Errorf("key order = %s, want %s", got, want)
	}
}

// Values the caller sent are forwarded as its own bytes, and values this
// package writes are not HTML-escaped. encoding/json would sort the caller's
// nested keys and turn "<" into \u003c, neither of which a JavaScript client
// produces.
func TestApplyBodyKeepsCallerBytesAndDoesNotHTMLEscape(t *testing.T) {
	messages := `[{"role":"user","content":[{"type":"text","text":"a <b> & c","cache_control":{"type":"ephemeral"}}]}]`
	body := []byte(`{"model":"m","messages":` + messages + `,"system":"<env>x</env>"}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), TurnOf(body))
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	if !bytes.Contains(out, []byte(`"messages":`+messages+`,`)) {
		t.Errorf("messages were re-encoded:\n%s", out)
	}
	if !bytes.Contains(out, []byte(`{"type":"text","text":"<env>x</env>"}`)) {
		t.Errorf("promoted system block is escaped or reordered:\n%s", out)
	}
	if bytes.Contains(out, []byte(`\u003c`)) {
		t.Errorf("output holds an HTML escape:\n%s", out)
	}
}
