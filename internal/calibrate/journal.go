// Package calibrate derives the proxy's pacing and cooldown settings from the
// 429 journal the dispatcher writes in production. It reads; it never writes
// configuration.
package calibrate

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"antigravity-go-proxy/internal/config"
)

// Outcome values, mirroring internal/accounts. They are duplicated rather than
// imported so the calibrator can read a journal written by any build.
const (
	OutcomeReject  = "reject"
	OutcomeRecover = "recover"
)

// Entry is one journal line. It carries the calibration fields and drops the
// forensic ones — bodies and headers answer the dimension question, not the
// pacing question.
type Entry struct {
	Timestamp           time.Time `json:"timestamp"`
	Account             string    `json:"account"`
	Model               string    `json:"model"`
	Endpoint            string    `json:"endpoint"`
	Status              int       `json:"status"`
	Outcome             string    `json:"outcome"`
	InFlight            int       `json:"inFlight"`
	PriorMinuteRequests int       `json:"priorMinuteRequests"`
	Failures            int       `json:"failures"`
}

// Journal is a parsed journal file. Skipped counts lines that did not parse, so
// a report can say how much of the file it could not read.
type Journal struct {
	Entries []Entry
	Skipped int
}

// maxJournalLine bounds one line. Bodies are capped at 4 KB upstream, so a
// larger line is corruption rather than a record.
const maxJournalLine = 64 * 1024

// DefaultJournalPath is where the dispatcher writes when forensics is enabled.
func DefaultJournalPath() string {
	return filepath.Join(config.GetConfigDir(), "forensics", "upstream-429.jsonl")
}

// ReadJournal parses path. A missing file is an empty journal, not an error:
// the operator may simply not have hit a 429 yet. Unparseable lines are skipped
// and counted — an append-only file written by a live process can end mid-line.
func ReadJournal(path string) (Journal, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Journal{}, nil
		}
		return Journal{}, err
	}
	defer file.Close()

	var journal Journal
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxJournalLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			journal.Skipped++
			continue
		}
		if entry.Outcome == "" {
			// Written before the field existed. Every line in that era was a
			// rejection, because successes were not journalled at all.
			entry.Outcome = OutcomeReject
		}
		journal.Entries = append(journal.Entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return journal, err
	}
	return journal, nil
}
