package corpus

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
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

// maxInFlightRecords bounds the row writes running at once. Classifier
// requests arrive about one per tool call, so reaching the bound means the
// disk has stalled; rows past it are dropped and counted rather than queued
// without limit.
const maxInFlightRecords = 64

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
	// dropped and lastWarn are atomic so RecordAsync can count a drop without
	// taking mu, which a stalled write may hold.
	dropped  atomic.Int64
	lastWarn atomic.Int64 // Unix nanoseconds of the last drop warning
	inFlight chan struct{}
	pending  sync.WaitGroup
}

func New(dir string, options Options) *Recorder {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return &Recorder{
		dir:      dir,
		options:  options,
		home:     usableHome(home),
		inFlight: make(chan struct{}, maxInFlightRecords),
	}
}

func (recorder *Recorder) Enabled() bool {
	return recorder != nil && recorder.dir != ""
}

// Dropped reports how many rows were dropped, at the size cap or at the
// in-flight cap.
func (recorder *Recorder) Dropped() int {
	if recorder == nil {
		return 0
	}
	return int(recorder.dropped.Load())
}

// RecordAsync builds and writes the row on its own goroutine, so the request
// handler returns without waiting on the disk: a small response stays in
// net/http's buffer until the handler returns, and the permission prompt
// waits on that response. Call Finish on input.Tap first, and do not write to
// the tap afterwards.
func (recorder *Recorder) RecordAsync(input EntryInput) {
	if !recorder.Enabled() {
		return
	}
	select {
	case recorder.inFlight <- struct{}{}:
	default:
		recorder.noteDrop("corpus: too many row writes in flight, dropping rows")
		return
	}
	recorder.pending.Add(1)
	go func() {
		defer recorder.pending.Done()
		defer func() { <-recorder.inFlight }()
		// A panic here would take the whole proxy down, not one request: the
		// net/http recovery that guarded the old in-handler write does not
		// reach this goroutine.
		defer func() {
			if value := recover(); value != nil {
				slog.Warn("corpus: building a row panicked", "panic", value)
			}
		}()
		recorder.Record(BuildEntry(input))
	}()
}

// Wait blocks until every row started by RecordAsync is written. The proxy
// never calls it; tests call it before they read rows back.
func (recorder *Recorder) Wait() {
	if recorder == nil {
		return
	}
	recorder.pending.Wait()
}

// noteDrop counts a dropped row and logs at most one warning per hour.
func (recorder *Recorder) noteDrop(message string, attrs ...any) {
	count := recorder.dropped.Add(1)
	now := time.Now().UnixNano()
	last := recorder.lastWarn.Load()
	if now-last > int64(time.Hour) && recorder.lastWarn.CompareAndSwap(last, now) {
		slog.Warn(message, append(attrs, "dropped", count)...)
	}
}

func (recorder *Recorder) Record(entry Entry) {
	if !recorder.Enabled() {
		return
	}
	if recorder.options.RedactPaths && recorder.home != "" {
		entry.Action = redactHome(entry.Action, recorder.home)
		for i, item := range entry.Context {
			entry.Context[i] = redactHome(item, recorder.home)
		}
		// The verdict and its rationale quote the command under judgement, so
		// they carry absolute paths as often as the action does.
		entry.VerdictRaw = redactHome(entry.VerdictRaw, recorder.home)
		entry.Thinking = redactHome(entry.Thinking, recorder.home)
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
			recorder.noteDrop("corpus: file at its size cap, dropping rows", "file", path)
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
