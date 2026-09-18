package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForensics429RecorderAppendsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "upstream-429.jsonl")
	recorder := NewForensics429Recorder(path)
	entry := Forensics429Entry{
		Timestamp:   time.Date(2026, 9, 17, 20, 0, 0, 0, time.UTC),
		Account:     "a@example.com",
		Project:     "aicode-consumers",
		Model:       "gemini-3.8-flash-high",
		Endpoint:    "https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse",
		Status:      429,
		Headers:     map[string]string{"Content-Type": "application/json"},
		Body:        `{"error":{"message":"Quota exceeded for aicode-consumers per project per minute"}}`,
		AppliedWait: "30s",
		Failures:    1,
	}
	recorder.Record(entry)
	recorder.Record(entry)

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (append-only)", len(lines))
	}
	var decoded Forensics429Entry
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	if decoded.Body != entry.Body || decoded.Account != entry.Account || decoded.Project != entry.Project {
		t.Fatalf("round-trip mismatch: %+v", decoded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestForensics429RecorderCapsBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	recorder := NewForensics429Recorder(path)
	recorder.Record(Forensics429Entry{Timestamp: time.Now(), Status: 429, Body: strings.Repeat("x", maxForensicsBodyLen*2)})

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Forensics429Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(contents))), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Body) > maxForensicsBodyLen {
		t.Fatalf("body len = %d, want <= %d", len(decoded.Body), maxForensicsBodyLen)
	}
}

// A disabled recorder (empty path) must be a safe no-op.
func TestForensics429RecorderDisabledNoop(t *testing.T) {
	recorder := NewForensics429Recorder("")
	recorder.Record(Forensics429Entry{Status: 429, Body: "x"})
}
