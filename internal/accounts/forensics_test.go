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

func TestRecordCarriesTheCalibrationFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	recorder := NewForensics429Recorder(path)

	recorder.Record(Forensics429Entry{
		Timestamp:           time.Unix(1_700_000_000, 0).UTC(),
		Account:             "a@example.com",
		Status:              429,
		Outcome:             OutcomeReject,
		InFlight:            4,
		PriorMinuteRequests: 37,
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var entry Forensics429Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("unmarshal journal line: %v", err)
	}
	if entry.InFlight != 4 {
		t.Fatalf("InFlight: got %d, want 4", entry.InFlight)
	}
	if entry.PriorMinuteRequests != 37 {
		t.Fatalf("PriorMinuteRequests: got %d, want 37", entry.PriorMinuteRequests)
	}
	if entry.Outcome != OutcomeReject {
		t.Fatalf("Outcome: got %q, want %q", entry.Outcome, OutcomeReject)
	}
}

func TestRecoveryRecordCarriesNoBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	recorder := NewForensics429Recorder(path)

	recorder.Record(Forensics429Entry{
		Timestamp: time.Unix(1_700_000_060, 0).UTC(),
		Account:   "a@example.com",
		Status:    200,
		Outcome:   OutcomeRecover,
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if strings.Contains(string(data), `"body"`) {
		t.Fatalf("recovery record carried a body field: %s", data)
	}
	if strings.Contains(string(data), `"headers"`) {
		t.Fatalf("recovery record carried a headers field: %s", data)
	}
}

