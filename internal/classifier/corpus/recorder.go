package corpus

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source names the code path that produced a row. A fine-tune consumes
// SourceUpstream rows only: training on SourceLaya rows would teach the local
// model its own answers and entrench its errors. SourceGateway marks a request
// that an alternate-backend gateway (Kimi, Zen, Claude Code, OpenRouter or a
// custom endpoint) answered, whose grader is whatever model that gateway
// routed to rather than the upstream teacher.
type Source string

const (
	SourceUpstream Source = "upstream"
	SourceStub     Source = "stub"
	SourceRule     Source = "rule"
	SourceLaya     Source = "laya"
	SourceGateway  Source = "gateway"
)

// Entry is one JSONL row. Schema version 1; see the design spec.
type Entry struct {
	Version      int      `json:"v"`
	Timestamp    string   `json:"ts"`
	Kind         string   `json:"kind"`
	Model        string   `json:"model"`
	Action       string   `json:"action"`
	Context      []string `json:"context"`
	SystemSHA256 string   `json:"system_sha256"`
	FooterSHA256 string   `json:"footer_sha256"`
	VerdictRaw   string   `json:"verdict_raw"`
	Severity     int      `json:"severity"`
	Category     string   `json:"category"`
	Thinking     string   `json:"thinking"`
	Status       int      `json:"status"`
	LatencyMs    int64    `json:"latency_ms"`
	Truncated    bool     `json:"truncated"`
	Source       Source   `json:"source"`
}

// EntryInput is everything BuildEntry needs from the request path.
type EntryInput struct {
	RawBody        []byte
	Kind           string
	Model          string
	Source         Source
	Tap            *ResponseTap
	ContextEntries int
}

// BuildEntry assembles a row. It never fails: a body whose transcript cannot
// be read still yields a row with empty Action, because the fact that such a
// request reached the classifier is itself worth keeping.
func BuildEntry(input EntryInput) Entry {
	entry := Entry{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Kind:      input.Kind,
		Model:     input.Model,
		Context:   []string{},
		Severity:  -1,
		Source:    input.Source,
	}

	if parsed, err := ParseRequest(input.RawBody, input.ContextEntries); err == nil {
		entry.Action = parsed.Action
		if parsed.Context != nil {
			entry.Context = parsed.Context
		}
		entry.SystemSHA256 = parsed.SystemSHA256
		entry.FooterSHA256 = parsed.FooterSHA256
	}

	if input.Tap != nil {
		verdict := ParseVerdict(input.Tap.VerdictText())
		entry.VerdictRaw = verdict.Raw
		entry.Severity = verdict.Severity
		entry.Category = verdict.Category
		entry.Thinking = verdict.Thinking
		entry.Status = input.Tap.Status()
		entry.LatencyMs = input.Tap.ElapsedMs()
		entry.Truncated = input.Tap.Truncated()
	}

	return entry
}

// Options tunes file handling and redaction. A MaxFiles or MaxFileBytes of
// zero or less means no limit.
type Options struct {
	MaxFiles     int
	MaxFileBytes int64
	RedactPaths  bool
}

// Recorder appends rows as JSONL, one file per UTC date. It mirrors
// internal/accounts/forensics.go: an empty dir disables it, files are opened
// per record so external rotation and manual deletion stay safe, and every
// error is logged and swallowed.
type Recorder struct {
	dir      string
	options  Options
	home     string
	mu       sync.Mutex
	lastDate string
	dropped  int
	lastWarn time.Time
}

func New(dir string, options Options) *Recorder {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return &Recorder{dir: dir, options: options, home: home}
}

func (recorder *Recorder) Enabled() bool {
	return recorder != nil && recorder.dir != ""
}

// Dropped reports how many rows the size cap rejected.
func (recorder *Recorder) Dropped() int {
	if recorder == nil {
		return 0
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.dropped
}

func (recorder *Recorder) Record(entry Entry) {
	if !recorder.Enabled() {
		return
	}
	if recorder.options.RedactPaths && recorder.home != "" {
		entry.Action = strings.ReplaceAll(entry.Action, recorder.home, "~")
		for i, item := range entry.Context {
			entry.Context[i] = strings.ReplaceAll(item, recorder.home, "~")
		}
		// The verdict and its rationale quote the command under judgement, so
		// they carry absolute paths as often as the action does.
		entry.VerdictRaw = strings.ReplaceAll(entry.VerdictRaw, recorder.home, "~")
		entry.Thinking = strings.ReplaceAll(entry.Thinking, recorder.home, "~")
	}

	line, err := json.Marshal(entry)
	if err != nil {
		slog.Warn("corpus: marshal row", "error", err)
		return
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(recorder.dir, "classifier-"+date+".jsonl")

	if err := os.MkdirAll(recorder.dir, 0o700); err != nil {
		slog.Warn("corpus: create directory", "error", err)
		return
	}
	if date != recorder.lastDate {
		recorder.lastDate = date
		recorder.prune(path)
	}

	// A MaxFileBytes of zero or less means no limit, matching MaxFiles.
	if recorder.options.MaxFileBytes > 0 {
		if info, err := os.Stat(path); err == nil && info.Size() >= recorder.options.MaxFileBytes {
			recorder.dropped++
			if time.Since(recorder.lastWarn) > time.Hour {
				recorder.lastWarn = time.Now()
				slog.Warn("corpus: file at its size cap, dropping rows",
					"file", path, "dropped", recorder.dropped)
			}
			return
		}
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("corpus: open row file", "error", err)
		return
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		slog.Warn("corpus: write row", "error", err)
	}
}

// prune deletes the oldest day files so that at most MaxFiles remain once
// today's row is written. today is that file: on a date rollover it does not
// exist yet, so it is reserved a slot rather than counted among the matches.
// Callers hold the mutex.
func (recorder *Recorder) prune(today string) {
	if recorder.options.MaxFiles <= 0 {
		return
	}
	matches, err := filepath.Glob(filepath.Join(recorder.dir, "classifier-*.jsonl"))
	if err != nil {
		return
	}
	keep := recorder.options.MaxFiles
	if _, err := os.Stat(today); err != nil {
		keep--
	}
	if len(matches) <= keep {
		return
	}
	// ISO dates sort lexicographically in chronological order.
	sort.Strings(matches)
	for _, path := range matches[:len(matches)-keep] {
		if err := os.Remove(path); err != nil {
			slog.Warn("corpus: prune old file", "file", path, "error", err)
			continue
		}
		// Each deleted file holds rows that cost upstream quota to collect, so
		// the loss is logged rather than silent.
		slog.Warn("corpus: pruned old day file to stay within maxFiles",
			"file", path, "maxFiles", recorder.options.MaxFiles)
	}
}
