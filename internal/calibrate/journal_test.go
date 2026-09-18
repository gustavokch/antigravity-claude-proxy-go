package calibrate

import (
	"os"
	"path/filepath"
	"testing"
)

// writeJournal puts lines in a temp file and returns its path.
func writeJournal(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	return path
}

func TestReadJournalParsesTheCalibrationFields(t *testing.T) {
	path := writeJournal(t,
		`{"timestamp":"2026-09-17T21:40:00Z","account":"a@example.com","status":429,"outcome":"reject","inFlight":4,"priorMinuteRequests":37}`,
	)

	journal, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(journal.Entries) != 1 {
		t.Fatalf("entries: got %d, want 1", len(journal.Entries))
	}
	entry := journal.Entries[0]
	if entry.Account != "a@example.com" {
		t.Fatalf("Account: got %q, want %q", entry.Account, "a@example.com")
	}
	if entry.InFlight != 4 {
		t.Fatalf("InFlight: got %d, want 4", entry.InFlight)
	}
	if entry.PriorMinuteRequests != 37 {
		t.Fatalf("PriorMinuteRequests: got %d, want 37", entry.PriorMinuteRequests)
	}
	if entry.Outcome != "reject" {
		t.Fatalf("Outcome: got %q, want %q", entry.Outcome, "reject")
	}
}

func TestReadJournalTreatsPreMigrationLinesAsRejects(t *testing.T) {
	path := writeJournal(t,
		`{"timestamp":"2026-09-17T21:40:00Z","account":"a@example.com","status":429,"body":"Resource has been exhausted"}`,
	)

	journal, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	entry := journal.Entries[0]
	if entry.Outcome != "reject" {
		t.Fatalf("Outcome for a line written before the field existed: got %q, want %q", entry.Outcome, "reject")
	}
	if entry.InFlight != 0 {
		t.Fatalf("InFlight for a pre-migration line: got %d, want 0", entry.InFlight)
	}
}

func TestReadJournalSkipsUnparseableLinesAndCountsThem(t *testing.T) {
	path := writeJournal(t,
		`{"timestamp":"2026-09-17T21:40:00Z","account":"a@example.com","status":429,"outcome":"reject"}`,
		`{"timestamp":"2026-09-17T21:41:00Z","acc`,
		``,
		`{"timestamp":"2026-09-17T21:42:00Z","account":"a@example.com","status":200,"outcome":"recover"}`,
	)

	journal, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(journal.Entries) != 2 {
		t.Fatalf("entries: got %d, want 2", len(journal.Entries))
	}
	if journal.Skipped != 1 {
		t.Fatalf("Skipped: got %d, want 1 — the blank line is not a skip", journal.Skipped)
	}
}

func TestReadJournalReturnsEmptyForAMissingFile(t *testing.T) {
	journal, err := ReadJournal(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("ReadJournal on a missing file: got error %v, want nil", err)
	}
	if len(journal.Entries) != 0 {
		t.Fatalf("entries: got %d, want 0", len(journal.Entries))
	}
}
