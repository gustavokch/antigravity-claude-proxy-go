package ccusage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Ledger defaults.
const (
	DefaultLedgerQueueSize     = 4096
	DefaultLedgerRetentionDays = 60
)

// unattributedDir holds ledger entries that carry no account.
const unattributedDir = "_unattributed"

const ledgerDateLayout = "2006-01-02"

// LedgerOptions configures a Ledger. Zero values take the defaults.
type LedgerOptions struct {
	QueueSize     int
	RetentionDays int
	Now           func() time.Time
	Logger        *slog.Logger

	// writeHook, when set, runs on the writer goroutine before each entry is
	// written. Tests use it to hold the writer.
	writeHook func(Entry)
}

// LedgerStats counts what a Ledger has done since it was opened.
type LedgerStats struct {
	Written uint64 // lines written
	Dropped uint64 // entries dropped because the queue was full or closed
	Errors  uint64 // marshal, open, write, sync and prune failures
}

// Ledger is an append-only usage log in Claude Code's transcript schema,
// laid out as <root>/projects/<accountId>/<YYYY-MM-DD>.jsonl so ccusage can
// read it with CLAUDE_CONFIG_DIR=<root>. Append never blocks: entries go
// through a buffered queue to a single writer goroutine, which owns every
// file. Dates are UTC.
type Ledger struct {
	root      string
	now       func() time.Time
	log       *slog.Logger
	retention int
	writeHook func(Entry)

	mu     sync.RWMutex // guards closed against the close of queue
	closed bool
	queue  chan Entry
	done   chan struct{}

	closeOnce sync.Once
	closeErr  error

	written atomic.Uint64
	dropped atomic.Uint64
	errs    atomic.Uint64

	// Writer goroutine state.
	files map[string]*ledgerFile // by account directory
}

type ledgerFile struct {
	date string
	f    *os.File
}

// NewLedger creates <root>/projects, prunes files past retention, and starts
// the writer goroutine.
func NewLedger(root string, opts LedgerOptions) (*Ledger, error) {
	if root == "" {
		return nil, errors.New("ccusage: ledger root is empty")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultLedgerQueueSize
	}
	if opts.RetentionDays <= 0 {
		opts.RetentionDays = DefaultLedgerRetentionDays
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
		return nil, fmt.Errorf("ccusage: create ledger directory: %w", err)
	}
	l := &Ledger{
		root:      root,
		now:       opts.Now,
		log:       opts.Logger,
		retention: opts.RetentionDays,
		writeHook: opts.writeHook,
		queue:     make(chan Entry, opts.QueueSize),
		done:      make(chan struct{}),
		files:     make(map[string]*ledgerFile),
	}
	l.prune()
	go l.run()
	return l, nil
}

// Root returns the directory to use as CLAUDE_CONFIG_DIR for this ledger.
func (l *Ledger) Root() string { return l.root }

// Append queues e for writing. It never blocks: when the queue is full or
// the ledger is closed the entry is dropped and false is returned.
func (l *Ledger) Append(e Entry) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		l.dropped.Add(1)
		return false
	}
	select {
	case l.queue <- e:
		return true
	default:
		l.dropped.Add(1)
		return false
	}
}

// Stats returns the ledger's counters.
func (l *Ledger) Stats() LedgerStats {
	return LedgerStats{Written: l.written.Load(), Dropped: l.dropped.Load(), Errors: l.errs.Load()}
}

// Close stops accepting entries, writes everything already queued, fsyncs
// and closes the open files, and stops the writer. It is idempotent.
func (l *Ledger) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.queue)
		l.mu.Unlock()
		<-l.done
	})
	return l.closeErr
}

func (l *Ledger) run() {
	defer close(l.done)
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case e, ok := <-l.queue:
			if !ok {
				l.closeErr = l.closeAll()
				return
			}
			if l.writeHook != nil {
				l.writeHook(e)
			}
			l.write(e)
		case <-ticker.C:
			l.prune()
		}
	}
}

func (l *Ledger) write(e Entry) {
	if e.Timestamp.IsZero() {
		e.Timestamp = l.now()
	}
	line, err := MarshalLedgerLine(e)
	if err != nil {
		l.fail("marshal ledger line", err)
		return
	}
	dir := LedgerAccountDir(e.AccountID)
	f, err := l.fileFor(dir, e.Timestamp.UTC().Format(ledgerDateLayout))
	if err != nil {
		l.fail("open ledger file", err)
		return
	}
	if _, err := f.Write(line); err != nil {
		l.fail("write ledger line", err)
		return
	}
	l.written.Add(1)
}

// fileFor returns the open file for an account and day. A different day
// rotates the account's file: the old one is fsynced and closed.
func (l *Ledger) fileFor(dir, date string) (*os.File, error) {
	if lf := l.files[dir]; lf != nil {
		if lf.date == date {
			return lf.f, nil
		}
		delete(l.files, dir)
		if err := syncClose(lf.f); err != nil {
			l.fail("rotate ledger file", err)
		}
	}
	accountDir := filepath.Join(l.root, "projects", dir)
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(accountDir, date+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	l.files[dir] = &ledgerFile{date: date, f: f}
	return f, nil
}

func (l *Ledger) closeAll() error {
	var errs []error
	for dir, lf := range l.files {
		if err := syncClose(lf.f); err != nil {
			l.fail("close ledger file", err)
			errs = append(errs, err)
		}
		delete(l.files, dir)
	}
	return errors.Join(errs...)
}

func syncClose(f *os.File) error {
	err := f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (l *Ledger) fail(what string, err error) {
	l.errs.Add(1)
	l.log.Warn("ccusage ledger: "+what, slog.String("error", err.Error()))
}

// prune deletes <root>/projects/*/<YYYY-MM-DD>.jsonl files whose date is
// more than the retention period before today. Nothing else is touched.
func (l *Ledger) prune() {
	today := l.now().UTC()
	cutoff := time.Date(today.Year(), today.Month(), today.Day()-l.retention, 0, 0, 0, 0, time.UTC)
	projects := filepath.Join(l.root, "projects")
	accounts, err := os.ReadDir(projects)
	if err != nil {
		l.fail("list ledger accounts", err)
		return
	}
	for _, acct := range accounts {
		if !acct.IsDir() {
			continue
		}
		dir := filepath.Join(projects, acct.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			l.fail("list ledger files", err)
			continue
		}
		for _, f := range files {
			if !f.Type().IsRegular() {
				continue
			}
			date, ok := ledgerFileDate(f.Name())
			if !ok || !date.Before(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, f.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				l.fail("prune ledger file", err)
			}
		}
	}
}

// ledgerFileDate parses a YYYY-MM-DD.jsonl file name.
func ledgerFileDate(name string) (time.Time, bool) {
	stem, ok := strings.CutSuffix(name, ".jsonl")
	if !ok || len(stem) != len(ledgerDateLayout) {
		return time.Time{}, false
	}
	t, err := time.Parse(ledgerDateLayout, stem)
	return t, err == nil
}

// LedgerAccountDir maps an account ID to its directory name under projects/.
// IDs made of letters, digits and "._@+-" that do not start with a dot are
// used as they are. Any other ID has its other characters replaced with "_"
// and a short hash of the original appended, so distinct IDs stay apart. An
// empty ID maps to "_unattributed".
func LedgerAccountDir(accountID string) string {
	if accountID == "" {
		return unattributedDir
	}
	safe := len(accountID) <= 128 && accountID[0] != '.' && accountID != unattributedDir
	var b strings.Builder
	for i := 0; i < len(accountID) && b.Len() < 96; i++ {
		c := accountID[i]
		if isASCIIAlnum(c) || strings.IndexByte("._@+-", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
			safe = false
		}
	}
	if safe {
		return accountID
	}
	sum := sha256.Sum256([]byte(accountID))
	name := strings.TrimLeft(b.String(), ".")
	if name == "" {
		name = "account"
	}
	return name + "-" + hex.EncodeToString(sum[:4])
}

type ledgerLine struct {
	Type      string        `json:"type"`
	Timestamp string        `json:"timestamp"`
	SessionID string        `json:"sessionId,omitzero"`
	RequestID string        `json:"requestId,omitzero"`
	Message   ledgerMessage `json:"message"`
	CostUSD   *float64      `json:"costUSD,omitzero"`
	AccountID string        `json:"accountId,omitzero"`
	Source    string        `json:"source"`
}

type ledgerMessage struct {
	ID    string      `json:"id,omitzero"`
	Model string      `json:"model,omitzero"`
	Usage ledgerUsage `json:"usage"`
}

type ledgerUsage struct {
	InputTokens              int64             `json:"input_tokens"`
	OutputTokens             int64             `json:"output_tokens"`
	CacheCreationInputTokens int64             `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64             `json:"cache_read_input_tokens"`
	CacheCreation            *rawCacheCreation `json:"cache_creation,omitzero"`
	Speed                    string            `json:"speed,omitzero"`
}

// MarshalLedgerLine renders e as one newline-terminated ledger line that
// ParseLine accepts. The timestamp is written in UTC with milliseconds;
// empty IDs, an empty model, a non-finite cost and a speed other than
// "standard" or "fast" are left out; negative token counts are written as 0.
// The cache_creation breakdown is written only when CacheCreate5m and
// CacheCreate1h add up to CacheCreate, so a total without a breakdown reads
// back as 5-minute writes. An empty Origin is written as "proxy".
func MarshalLedgerLine(e Entry) ([]byte, error) {
	nonNeg := func(v int64) int64 { return max(v, 0) }
	origin := e.Origin
	if origin == "" {
		origin = OriginProxy
	}
	line := ledgerLine{
		Type:      "assistant",
		Timestamp: e.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		SessionID: e.SessionID,
		RequestID: e.RequestID,
		Message: ledgerMessage{
			ID:    e.MessageID,
			Model: e.Model,
			Usage: ledgerUsage{
				InputTokens:              nonNeg(e.Input),
				OutputTokens:             nonNeg(e.Output),
				CacheCreationInputTokens: nonNeg(e.CacheCreate),
				CacheReadInputTokens:     nonNeg(e.CacheRead),
			},
		},
		AccountID: e.AccountID,
		Source:    origin,
	}
	if e.CostUSD != nil && !math.IsNaN(*e.CostUSD) && !math.IsInf(*e.CostUSD, 0) {
		cost := *e.CostUSD
		line.CostUSD = &cost
	}
	if e.Speed == "standard" || e.Speed == "fast" {
		line.Message.Usage.Speed = e.Speed
	}
	if c5, c1 := nonNeg(e.CacheCreate5m), nonNeg(e.CacheCreate1h); satAdd(c5, c1) == line.Message.Usage.CacheCreationInputTokens {
		line.Message.Usage.CacheCreation = &rawCacheCreation{Ephemeral5m: uint64(c5), Ephemeral1h: uint64(c1)}
	}
	data, err := json.Marshal(&line)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ParseLedgerLine parses one ledger line, for callers that tail ledger files.
// Entries have Source SourceLedger, AccountID and Origin from the line, and
// are grouped for session reports by their sessionId under the project path
// "proxy/<accountId>" ("proxy" when unattributed). A line without a sessionId takes its UTC date, the
// name of the file it is written to, as ccusage does.
func ParseLedgerLine(line []byte) []Entry {
	entries := ParseLine(line)
	if len(entries) == 0 {
		return nil
	}
	var extra struct {
		AccountID string `json:"accountId"`
		Source    string `json:"source"`
	}
	_ = json.Unmarshal(line, &extra)
	for i := range entries {
		e := &entries[i]
		e.Source = SourceLedger
		e.AccountID = extra.AccountID
		e.Origin = extra.Source
		if e.SessionID == "" {
			e.SessionID = e.Timestamp.UTC().Format(ledgerDateLayout)
		}
		setLedgerMeta(e)
	}
	return entries
}

func setLedgerMeta(e *Entry) {
	e.Project = LedgerAccountDir(e.AccountID)
	e.PathSessionID = e.SessionID
	e.ProjectPath = "proxy"
	if e.AccountID != "" {
		e.ProjectPath += "/" + e.AccountID
	}
}

// ReadLedgerFile parses every line of a ledger file. A line without an
// accountId takes the account from the file's directory.
func ReadLedgerFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dirAccount := filepath.Base(filepath.Dir(path))
	if dirAccount == unattributedDir {
		dirAccount = ""
	}
	var entries []Entry
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		for _, e := range ParseLedgerLine(line) {
			if e.AccountID == "" && dirAccount != "" {
				e.AccountID = dirAccount
				setLedgerMeta(&e)
			}
			entries = append(entries, e)
		}
	}
	return entries, nil
}
