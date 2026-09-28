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

// hashedDirMark separates the cleaned prefix of a hashed account directory
// from its hash. It is outside the characters an ID may use as its own
// directory name, so a hashed directory is recognisable by it.
const hashedDirMark = "="

// Account ID length limits: an ID up to maxAccountDirLen bytes may be its
// own directory name; a hashed name keeps at most hashedPrefixLen bytes of
// the cleaned ID.
const (
	maxAccountDirLen = 128
	hashedPrefixLen  = 96
)

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

	// Writer goroutine state: open files by account directory and date, and
	// the latest date written. Files of the latest date and the day before
	// stay open, so entries straddling midnight do not reopen files.
	files  map[ledgerFileKey]*os.File
	latest string
}

type ledgerFileKey struct{ dir, date string }

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
	// The root may be a directory the user chose, so its mode is left
	// alone; only a root created here is private. Everything below
	// projects/ is the ledger's own and is kept private.
	if err := mkdirRoot(root); err != nil {
		return nil, fmt.Errorf("ccusage: create ledger directory: %w", err)
	}
	if err := mkdirPrivate(filepath.Join(root, "projects")); err != nil {
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
		files:     make(map[ledgerFileKey]*os.File),
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
	key := ledgerFileKey{dir: LedgerAccountDir(e.AccountID), date: e.Timestamp.UTC().Format(ledgerDateLayout)}
	// A timestamp skewed into the future must not become the latest date,
	// or every normal entry after it would count as late.
	limit := l.now().UTC().AddDate(0, 0, 1).Format(ledgerDateLayout)
	future := key.date > limit
	l.rotate(min(key.date, limit))
	f, err := l.fileFor(key)
	if err != nil {
		l.fail("open ledger file", err)
		return
	}
	if _, err := f.Write(line); err != nil {
		// A short write can leave a partial line. Dropping the handle makes
		// the next write reopen the file and terminate that line first.
		delete(l.files, key)
		f.Close()
		l.fail("write ledger line", err)
		return
	}
	l.written.Add(1)
	if future || key.date < previousDate(l.latest) {
		// A late entry for an older day, or one past tomorrow: do not keep
		// its file open.
		delete(l.files, key)
		if err := syncClose(f); err != nil {
			l.fail("close ledger file", err)
		}
	}
}

// rotate records date as the latest when it is newer, and fsyncs and closes
// the files older than the day before the latest.
func (l *Ledger) rotate(date string) {
	if date <= l.latest {
		return
	}
	l.latest = date
	keep := previousDate(date)
	for key, f := range l.files {
		if key.date < keep {
			delete(l.files, key)
			if err := syncClose(f); err != nil {
				l.fail("rotate ledger file", err)
			}
		}
	}
}

// previousDate returns the YYYY-MM-DD before date, or "" when date does not
// parse.
func previousDate(date string) string {
	t, err := time.Parse(ledgerDateLayout, date)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, -1).Format(ledgerDateLayout)
}

// fileFor returns the open file for an account and day, opening it when
// needed. A file whose last byte is not a newline, left by a crash or a
// failed write, gets one before any new line is appended.
func (l *Ledger) fileFor(key ledgerFileKey) (*os.File, error) {
	if f := l.files[key]; f != nil {
		return f, nil
	}
	accountDir := filepath.Join(l.root, "projects", key.dir)
	if err := mkdirPrivate(accountDir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(accountDir, key.date+".jsonl"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := terminateLastLine(f); err != nil {
		f.Close()
		return nil, err
	}
	l.files[key] = f
	return f, nil
}

// terminateLastLine makes a private, newline-terminated file of f.
func terminateLastLine(f *os.File) error {
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// mkdirRoot creates dir with mode 0700 when it does not exist, and leaves
// an existing directory's mode unchanged.
func mkdirRoot(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	return os.MkdirAll(dir, 0o700)
}

// mkdirPrivate creates dir if needed and makes it 0700 either way.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func (l *Ledger) closeAll() error {
	var errs []error
	for key, f := range l.files {
		if err := syncClose(f); err != nil {
			l.fail("close ledger file", err)
			errs = append(errs, err)
		}
		delete(l.files, key)
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
// IDs of at most 128 bytes made of letters, digits and "._@+-" that do not
// start with a dot are used as they are. Any other ID becomes the first 96
// bytes of its cleaned form (other characters replaced with "_", leading dots
// removed), "=" and a short hash of the original, so distinct IDs stay
// apart. An empty ID maps to "_unattributed".
func LedgerAccountDir(accountID string) string {
	if accountID == "" {
		return unattributedDir
	}
	if isPlainAccountDir(accountID) {
		return accountID
	}
	var b strings.Builder
	for i := 0; i < len(accountID); i++ {
		if c := accountID[i]; isAccountDirByte(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	name := strings.TrimLeft(b.String(), ".")
	if len(name) > hashedPrefixLen {
		name = name[:hashedPrefixLen]
	}
	if name == "" {
		name = "account"
	}
	sum := sha256.Sum256([]byte(accountID))
	return name + hashedDirMark + hex.EncodeToString(sum[:4])
}

// isPlainAccountDir reports whether an account ID can be its own directory
// name. Every byte is checked.
func isPlainAccountDir(id string) bool {
	if len(id) > maxAccountDirLen || id[0] == '.' || id == unattributedDir {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isAccountDirByte(id[i]) {
			return false
		}
	}
	return true
}

func isAccountDirByte(c byte) bool {
	return isASCIIAlnum(c) || strings.IndexByte("._@+-", c) >= 0
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
// empty IDs, an empty model, a negative or non-finite cost and a speed other
// than "standard" or "fast" are left out; negative token counts are written
// as 0. CacheCreate is the cache write total. Its breakdown is written as
// {5m: CacheCreate-CacheCreate1h, 1h: CacheCreate1h}, so 1-hour writes keep
// their price and the rest count as 5-minute writes. It is left out, and the
// total reads back as 5-minute writes, when both parts are zero or the
// 1-hour part exceeds the total. An empty Origin is written as "proxy".
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
	if e.CostUSD != nil && *e.CostUSD >= 0 && !math.IsInf(*e.CostUSD, 0) {
		cost := *e.CostUSD
		line.CostUSD = &cost
	}
	if e.Speed == "standard" || e.Speed == "fast" {
		line.Message.Usage.Speed = e.Speed
	}
	total := line.Message.Usage.CacheCreationInputTokens
	if c5, c1 := nonNeg(e.CacheCreate5m), nonNeg(e.CacheCreate1h); (c5 != 0 || c1 != 0) && c1 <= total {
		line.Message.Usage.CacheCreation = &rawCacheCreation{Ephemeral5m: uint64(total - c1), Ephemeral1h: uint64(c1)}
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
// accountId takes the account from the file's directory, unless that is the
// unattributed directory or a hashed name (see LedgerAccountDir), which does
// not give the ID back.
func ReadLedgerFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dirAccount := filepath.Base(filepath.Dir(path))
	if dirAccount == unattributedDir || strings.Contains(dirAccount, hashedDirMark) {
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
