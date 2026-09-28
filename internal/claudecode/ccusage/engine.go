package ccusage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Engine defaults.
const (
	// DefaultEngineWindow is how far back an Engine keeps entries in memory.
	// It covers a 7-day window plus a day of slack for its first block.
	DefaultEngineWindow = 8 * 24 * time.Hour
	// DefaultPollInterval is how often an Engine tails its files.
	DefaultPollInterval = 30 * time.Second
	// DefaultSnapshotTTL is how long an account's Snapshot is reused.
	DefaultSnapshotTTL = 5 * time.Second
	// SummaryFileName is the per-account summary file in the ledger root.
	SummaryFileName = "summary.json"
)

const (
	// maxSummaryAnchors caps the window starts a summary remembers.
	maxSummaryAnchors = 64
	// pruneSlack is how far past the cutoff the oldest entry may be before
	// the deduper is compacted; reads filter by the cutoff regardless.
	pruneSlack = time.Hour
	// addChunk is how many entries a refresh adds per hold of the lock.
	addChunk = 2048
	// ctxCheckLines is how often a tail checks for cancellation.
	ctxCheckLines = 1024
	// tailMarkBytes is how much of a file before its offset is kept to
	// tell an append from a rewrite.
	tailMarkBytes = 256
	// anchorMergeGap merges anchors observed with clock jitter, as
	// IdentifyBlocks does.
	anchorMergeGap = time.Minute
	sevenDays      = 7 * 24 * time.Hour
)

// AccountRef is what an Engine needs to know about a configured account to
// attribute local usage.
type AccountRef struct {
	ID string
	// AutoImport marks an account imported from the local Claude Code
	// login, the one local transcripts most likely belong to.
	AutoImport bool
}

// EngineOptions configures an Engine. Zero values take the defaults.
type EngineOptions struct {
	// LedgerRoot is the ledger directory; it is required.
	LedgerRoot    string
	RetentionDays int
	QueueSize     int

	// ScanLocalLogs enables reading Claude Code's own transcripts from
	// LocalPaths, which defaults to ClaudePaths.
	ScanLocalLogs bool
	LocalPaths    func() []string

	// LocalAccountID, when set, owns every admitted local entry that does
	// not dedupe against the ledger. Otherwise Accounts is consulted.
	LocalAccountID string
	Accounts       func() []AccountRef

	// SessionDuration is the billing block length; it must be a whole
	// number of hours and defaults to DefaultBlockDuration.
	SessionDuration time.Duration
	CostMode        CostMode
	Pricer          Pricer
	// Location decides where "today" starts. Nil means time.Local.
	Location *time.Location

	Window       time.Duration
	PollInterval time.Duration
	SnapshotTTL  time.Duration
	Now          func() time.Time
	Logger       *slog.Logger
}

// Summary is what an Engine keeps about an account beyond its in-memory
// window. It is persisted in SummaryFileName.
type Summary struct {
	// MaxBlockTokens and MaxBlockCostUSD are the largest token total and
	// cost of any completed block seen, possibly of different blocks.
	MaxBlockTokens  int64     `json:"maxBlockTokens"`
	MaxBlockCostUSD float64   `json:"maxBlockCostUSD"`
	MaxBlockStart   time.Time `json:"maxBlockStart,omitzero"`
	// Calibrated limits are implied window costs derived from the unified
	// rate-limit headers; zero means none yet. CalibratedAt5h and
	// CalibratedAt7d say when each was taken and CalibratedUtilization5h
	// and CalibratedUtilization7d from which header utilization.
	// CalibratedAt is the latest of the two; summaries written before the
	// per-window times existed carry only it.
	CalibratedCostUSD5h     float64   `json:"calibratedCostUSD5h,omitzero"`
	CalibratedCostUSD7d     float64   `json:"calibratedCostUSD7d,omitzero"`
	CalibratedAt            time.Time `json:"calibratedAt,omitzero"`
	CalibratedAt5h          time.Time `json:"calibratedAt5h,omitzero"`
	CalibratedAt7d          time.Time `json:"calibratedAt7d,omitzero"`
	CalibratedUtilization5h float64   `json:"calibratedUtilization5h,omitzero"`
	CalibratedUtilization7d float64   `json:"calibratedUtilization7d,omitzero"`
	// Anchors5h are the known 5-hour window starts inside the engine window.
	Anchors5h []time.Time `json:"anchors5h,omitempty"`
	// Reset7d is the last known 7-day window reset from the unified
	// rate-limit headers; zero means none seen.
	Reset7d time.Time `json:"reset7d,omitzero"`
}

func (s Summary) clone() Summary {
	s.Anchors5h = append([]time.Time(nil), s.Anchors5h...)
	return s
}

// WindowUsage is the usage inside one window.
type WindowUsage struct {
	Start   time.Time
	End     time.Time
	Entries int
	Tokens  int64
	CostUSD float64
}

// ModelUsage is one model's usage inside a window.
type ModelUsage struct {
	Model       string
	Input       int64
	Output      int64
	CacheCreate int64
	CacheRead   int64
	TotalTokens int64
	CostUSD     float64
}

// Snapshot is an account's usage as seen at one instant. A Snapshot may be
// shared between callers and must not be modified.
type Snapshot struct {
	AccountID string
	At        time.Time
	// Blocks are the account's billing blocks inside the engine window.
	Blocks []Block
	// Active is the block containing At, or nil.
	Active     *Block
	BurnRate   *BurnRate
	Projection *Projection
	// Window5h is the active block's usage; zero when there is none.
	Window5h WindowUsage
	// Window7d is the rolling seven days ending at At.
	Window7d WindowUsage
	// Today runs from midnight in the engine's location to At.
	Today WindowUsage
	// Models splits Window7d by display model, highest cost first.
	Models  []ModelUsage
	Summary Summary
	// Inferred reports that some entries were attributed to the account only
	// because it is the single auto-imported account.
	Inferred bool

	// running holds the entries up to At in timestamp order with running
	// totals, so Usage answers any window without walking the entries.
	running []usagePoint
}

// usagePoint is one entry's place in a Snapshot's running totals: the
// tokens and cost of every entry up to and including it.
type usagePoint struct {
	at     time.Time
	tokens int64
	cost   float64
}

// Usage returns the usage of the entries at or after start and before end,
// counting only entries up to At. It is cheap: two binary searches over
// the snapshot's running totals.
func (s *Snapshot) Usage(start, end time.Time) WindowUsage {
	w := WindowUsage{Start: start, End: end}
	if s == nil || !end.After(start) {
		return w
	}
	r := s.running
	i := sort.Search(len(r), func(k int) bool { return !r[k].at.Before(start) })
	j := sort.Search(len(r), func(k int) bool { return !r[k].at.Before(end) })
	if j <= i {
		return w
	}
	w.Entries = j - i
	w.Tokens = r[j-1].tokens
	w.CostUSD = r[j-1].cost
	if i > 0 {
		w.Tokens -= r[i-1].tokens
		w.CostUSD -= r[i-1].cost
	}
	// Running sums can leave a rounding residue on subtraction.
	if w.CostUSD < 0 {
		w.CostUSD = 0
	}
	return w
}

// EngineStats counts what an Engine holds.
type EngineStats struct {
	Entries int
	Files   int
	Ledger  LedgerStats
}

// Engine keeps the recent usage of every account in memory: what the proxy
// records through its ledger, merged with Claude Code's own transcripts. It
// loads the last Window of both, tails the files every PollInterval and
// answers per-account snapshots. All methods are safe for concurrent use and
// are no-ops on a nil *Engine.
type Engine struct {
	root         string
	ledger       *Ledger
	scanLocal    bool
	localPaths   func() []string
	localAccount string
	accounts     func() []AccountRef
	dur          time.Duration
	mode         CostMode
	pricer       Pricer
	loc          *time.Location
	window       time.Duration
	poll         time.Duration
	ttl          time.Duration
	now          func() time.Time
	log          *slog.Logger

	// refreshMu serialises refreshes; files belongs to the refresh.
	refreshMu sync.Mutex
	files     map[string]*tailFile
	nFiles    atomic.Int64

	mu        sync.Mutex
	dedup     *Deduper
	oldest    time.Time // lower bound on the kept entries' timestamps
	summaries map[string]*Summary
	dirty     bool
	cache     map[string]cachedSnapshot

	saveMu sync.Mutex

	// baseCtx is cancelled by Close; reloads run under it. reloading is
	// the done channel of the reload in flight, shared by every caller.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	reloadMu   sync.Mutex
	reloading  chan struct{}

	lifeMu  sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	done    chan struct{}
}

type tailFile struct {
	offset int64
	// info is the file as last read; mark is up to tailMarkBytes read just
	// before offset. A different file or different mark bytes mean the
	// file was replaced or rewritten, and it is read again from the start.
	info   os.FileInfo
	mark   []byte
	ledger bool
	// dirAccount is the account a ledger file's directory names.
	dirAccount string
	meta       FileMeta
}

type cachedSnapshot struct {
	at      time.Time
	anchors string
	snap    *Snapshot
}

// NewEngine opens the ledger under opts.LedgerRoot and loads the persisted
// summaries. Files are read by Refresh, which Start runs first.
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.LedgerRoot == "" {
		return nil, errors.New("ccusage: engine ledger root is empty")
	}
	if opts.SessionDuration == 0 {
		opts.SessionDuration = DefaultBlockDuration
	}
	if opts.SessionDuration < time.Hour || opts.SessionDuration%time.Hour != 0 {
		return nil, fmt.Errorf("ccusage: session duration %v is not a whole number of hours", opts.SessionDuration)
	}
	if opts.CostMode == "" {
		opts.CostMode = CostModeAuto
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Window <= 0 {
		opts.Window = DefaultEngineWindow
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.SnapshotTTL <= 0 {
		opts.SnapshotTTL = DefaultSnapshotTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.LocalPaths == nil {
		opts.LocalPaths = ClaudePaths
	}
	ledger, err := NewLedger(opts.LedgerRoot, LedgerOptions{
		QueueSize:     opts.QueueSize,
		RetentionDays: opts.RetentionDays,
		Now:           opts.Now,
		Logger:        opts.Logger,
	})
	if err != nil {
		return nil, err
	}
	en := &Engine{
		root:         opts.LedgerRoot,
		ledger:       ledger,
		scanLocal:    opts.ScanLocalLogs,
		localPaths:   opts.LocalPaths,
		localAccount: opts.LocalAccountID,
		accounts:     opts.Accounts,
		dur:          opts.SessionDuration,
		mode:         opts.CostMode,
		pricer:       opts.Pricer,
		loc:          opts.Location,
		window:       opts.Window,
		poll:         opts.PollInterval,
		ttl:          opts.SnapshotTTL,
		now:          opts.Now,
		log:          opts.Logger,
		files:        make(map[string]*tailFile),
		dedup:        NewDeduper(),
		summaries:    make(map[string]*Summary),
		cache:        make(map[string]cachedSnapshot),
	}
	en.baseCtx, en.baseCancel = context.WithCancel(context.Background())
	en.loadSummaries()
	return en, nil
}

// Root returns the ledger directory.
func (en *Engine) Root() string {
	if en == nil {
		return ""
	}
	return en.root
}

// ReportSettings returns what the engine prices and groups entries with,
// so reports over Entries match its snapshots: the block length, the cost
// mode, the pricer and the location of "today".
func (en *Engine) ReportSettings() (dur time.Duration, mode CostMode, p Pricer, loc *time.Location) {
	if en == nil {
		return DefaultBlockDuration, CostModeAuto, nil, time.Local
	}
	return en.dur, en.mode, en.pricer, en.loc
}

// Start loads the files in the background and keeps tailing them every
// poll interval until ctx is done or Close is called. Later calls do
// nothing.
func (en *Engine) Start(ctx context.Context) {
	if en == nil {
		return
	}
	en.lifeMu.Lock()
	defer en.lifeMu.Unlock()
	if en.started || en.closed {
		return
	}
	en.started = true
	ctx, en.cancel = context.WithCancel(ctx)
	en.done = make(chan struct{})
	go en.run(ctx)
}

func (en *Engine) run(ctx context.Context) {
	defer close(en.done)
	en.refresh(ctx)
	ticker := time.NewTicker(en.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			en.refresh(ctx)
		}
	}
}

// Close stops the poller, saves the summaries and closes the ledger, which
// writes out everything already recorded. It is idempotent.
func (en *Engine) Close() error {
	if en == nil {
		return nil
	}
	en.lifeMu.Lock()
	if en.closed {
		en.lifeMu.Unlock()
		return nil
	}
	en.closed = true
	started := en.started
	en.lifeMu.Unlock()
	if started {
		en.cancel()
		<-en.done
	}
	en.baseCancel()
	en.reloadMu.Lock()
	reloading := en.reloading
	en.reloadMu.Unlock()
	if reloading != nil {
		<-reloading
	}
	en.saveSummaries()
	return en.ledger.Close()
}

// Stats returns the engine's counters.
func (en *Engine) Stats() EngineStats {
	if en == nil {
		return EngineStats{}
	}
	en.mu.Lock()
	entries := en.dedup.Len()
	en.mu.Unlock()
	return EngineStats{Entries: entries, Files: int(en.nFiles.Load()), Ledger: en.ledger.Stats()}
}

// Record is the hot path for usage the proxy served or caused: it queues e
// for the ledger and adds it to memory, and never waits on the disk. e is
// stored as a ledger entry; a zero timestamp means now, an empty Origin
// means OriginProxy, and an entry without a message ID gets the synthetic
// ID "<origin>:<accountId>:<unix nanoseconds>" so the copy read back from
// the ledger dedupes against it.
func (en *Engine) Record(e Entry) {
	if en == nil {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = en.now()
	}
	if e.Origin == "" {
		e.Origin = OriginProxy
	}
	if e.MessageID == "" {
		prefix := e.Origin
		if prefix == OriginCacheBump {
			prefix = "bump"
		}
		e.MessageID = prefix + ":" + e.AccountID + ":" + strconv.FormatInt(e.Timestamp.UnixNano(), 10)
	}
	e.Source = SourceLedger
	// Round-trip through the ledger format so the in-memory entry is
	// exactly what a later read of the ledger yields.
	line, err := MarshalLedgerLine(e)
	if err != nil {
		en.log.Warn("ccusage engine: record", slog.String("error", err.Error()))
		return
	}
	parsed := ParseLedgerLine(bytes.TrimSuffix(line, []byte{'\n'}))
	if !en.ledger.Append(e) {
		en.log.Debug("ccusage engine: ledger queue full, entry kept in memory only")
	}
	cutoff := en.now().Add(-en.window)
	en.mu.Lock()
	for _, p := range parsed {
		if !p.Timestamp.Before(cutoff) {
			en.addLocked(p)
		}
	}
	en.mu.Unlock()
}

// addLocked adds e to the deduper and keeps oldest a lower bound. Callers
// hold en.mu.
func (en *Engine) addLocked(e Entry) {
	if _, kept := en.dedup.Add(e); kept && (en.oldest.IsZero() || e.Timestamp.Before(en.oldest)) {
		en.oldest = e.Timestamp
	}
}

// pruneLocked compacts the deduper once its oldest entry is well past the
// cutoff. Callers hold en.mu.
func (en *Engine) pruneLocked(cutoff time.Time) {
	if en.oldest.IsZero() || !en.oldest.Before(cutoff.Add(-pruneSlack)) {
		return
	}
	en.dedup.Prune(cutoff)
	en.oldest = time.Time{}
	for i := range en.dedup.entries {
		if ts := en.dedup.entries[i].Timestamp; en.oldest.IsZero() || ts.Before(en.oldest) {
			en.oldest = ts
		}
	}
}

// Refresh reads whatever the ledger and local files gained since the last
// read and prunes entries older than the window. Start calls it on every
// poll; calling it directly is for tests and reloads. It runs under the
// engine's own context, so Close cuts a pass short.
func (en *Engine) Refresh() {
	if en == nil {
		return
	}
	en.refresh(en.baseCtx)
}

// Reload starts a Refresh in the background unless one it started is
// still running, and returns a channel that is closed when that pass ends.
// Concurrent callers share one pass. Close cancels the pass and waits for
// it.
func (en *Engine) Reload() <-chan struct{} {
	if en == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	en.reloadMu.Lock()
	defer en.reloadMu.Unlock()
	if en.reloading != nil {
		return en.reloading
	}
	// A closed engine does not reload.
	if en.baseCtx.Err() != nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	done := make(chan struct{})
	en.reloading = done
	go func() {
		en.refresh(en.baseCtx)
		en.reloadMu.Lock()
		en.reloading = nil
		en.reloadMu.Unlock()
		close(done)
	}()
	return done
}

func (en *Engine) refresh(ctx context.Context) {
	en.refreshMu.Lock()
	defer en.refreshMu.Unlock()
	cutoff := en.now().Add(-en.window)

	// Files already read advanced their offsets, so what they yielded is
	// kept even when ctx ends the pass early.
	seen := make(map[string]bool)
	var batch []Entry
	paths := en.ledgerFiles(cutoff)
	nLedger := len(paths)
	if en.scanLocal {
		paths = append(paths, en.localFiles(cutoff)...)
	}
	complete := true
	for i, path := range paths {
		if ctx.Err() != nil {
			complete = false
			break
		}
		seen[path] = true
		batch = append(batch, en.tail(ctx, path, i < nLedger, cutoff)...)
	}
	if complete {
		for path := range en.files {
			if !seen[path] {
				delete(en.files, path)
			}
		}
	}
	en.nFiles.Store(int64(len(en.files)))

	// The first pass can load days of transcripts; add them in chunks so
	// Record and Snapshot are not held up for the whole batch.
	for start := 0; start < len(batch); start += addChunk {
		en.mu.Lock()
		for _, e := range batch[start:min(start+addChunk, len(batch))] {
			en.addLocked(e)
		}
		en.mu.Unlock()
	}
	en.mu.Lock()
	en.pruneLocked(cutoff)
	en.mu.Unlock()
	en.saveSummaries()
}

// ledgerFiles lists the ledger's day files that can hold entries after
// cutoff.
func (en *Engine) ledgerFiles(cutoff time.Time) []string {
	matches, err := filepath.Glob(filepath.Join(en.root, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil
	}
	first := cutoff.UTC().Truncate(24 * time.Hour)
	var out []string
	for _, path := range matches {
		date, ok := ledgerFileDate(filepath.Base(path))
		if ok && !date.Before(first) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// localFiles lists the transcripts modified after cutoff; older ones hold
// nothing inside the window, so a tracked one is dropped. A config
// directory that is the ledger itself is skipped.
func (en *Engine) localFiles(cutoff time.Time) []string {
	root := filepath.Clean(en.root)
	var paths []string
	for _, p := range en.localPaths() {
		p = filepath.Clean(p)
		if p == root || p == filepath.Join(root, "projects") {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil
	}
	var out []string
	for _, path := range UsageFiles(paths) {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		out = append(out, path)
	}
	return out
}

// tail reads the complete lines path gained since its stored offset. A
// file that was replaced (a different inode), shrank, or was rewritten in
// place (the bytes before the offset changed) is read again from the
// start; a trailing line without a newline is left for the next read. It
// stops early, keeping what it consumed, when ctx is done.
func (en *Engine) tail(ctx context.Context, path string, ledger bool, cutoff time.Time) []Entry {
	st := en.files[path]
	if st == nil {
		st = &tailFile{ledger: ledger}
		if ledger {
			dir := filepath.Base(filepath.Dir(path))
			if dir != unattributedDir && !strings.Contains(dir, hashedDirMark) {
				st.dirAccount = dir
			}
		} else {
			st.meta = FileMetaFor(path)
		}
		en.files[path] = st
	}
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			en.log.Warn("ccusage engine: open usage file", slog.String("path", path), slog.String("error", err.Error()))
		}
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	size := info.Size()
	switch {
	case st.info != nil && !os.SameFile(st.info, info):
		st.offset = 0
	case size < st.offset:
		st.offset = 0
	case st.offset > 0 && !info.ModTime().Equal(st.info.ModTime()) && !st.markMatches(f):
		st.offset = 0
	}
	st.info = info
	if size == st.offset {
		st.updateMark(f)
		return nil
	}
	defer st.updateMark(f)
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return nil
	}
	r := bufio.NewReaderSize(io.LimitReader(f, size-st.offset), 64<<10)
	var out []Entry
	for n := 1; ; n++ {
		if n%ctxCheckLines == 0 && ctx.Err() != nil {
			return out
		}
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			st.offset += int64(len(line))
			out = st.parse(out, line[:len(line)-1], cutoff)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				en.log.Warn("ccusage engine: read usage file", slog.String("path", path), slog.String("error", err.Error()))
			}
			return out
		}
	}
}

// markMatches reports whether the bytes before the offset are still the
// ones last read there.
func (st *tailFile) markMatches(f *os.File) bool {
	if len(st.mark) == 0 {
		return true
	}
	buf := make([]byte, len(st.mark))
	if _, err := f.ReadAt(buf, st.offset-int64(len(buf))); err != nil {
		return false
	}
	return bytes.Equal(buf, st.mark)
}

// updateMark remembers the bytes just before the offset.
func (st *tailFile) updateMark(f *os.File) {
	n := min(st.offset, tailMarkBytes)
	if n == 0 {
		st.mark = nil
		return
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.offset-n); err != nil {
		st.mark = nil
		return
	}
	st.mark = buf
}

func (st *tailFile) parse(out []Entry, line []byte, cutoff time.Time) []Entry {
	if st.ledger {
		for _, e := range ParseLedgerLine(line) {
			if e.Timestamp.Before(cutoff) {
				continue
			}
			if e.AccountID == "" && st.dirAccount != "" {
				e.AccountID = st.dirAccount
				setLedgerMeta(&e)
			}
			out = append(out, e)
		}
		return out
	}
	entries := ParseLine(line)
	if len(entries) == 0 || !AdmitLocal(entries[0]) {
		return out
	}
	for _, e := range entries {
		if e.Timestamp.Before(cutoff) {
			continue
		}
		st.meta.Apply(&e)
		e.Source = SourceLocal
		out = append(out, e)
	}
	return out
}

// proxyMessageID matches the message IDs this proxy makes up for responses
// it translates from other gateways.
var proxyMessageID = regexp.MustCompile(`^msg_[0-9a-f]{32}$|^msg_(zen|clf|stub)_`)

// AdmitLocal reports whether a transcript entry is usage Anthropic served:
// it carries an Anthropic request ID ("req_"), a message ID the proxy did
// not make up for a translated response, and a Claude model. Claude Code's
// transcripts also record turns other gateways served through this proxy,
// which must not count against Anthropic windows. For a line with advisor
// iterations, pass the response's own entry and admit the line as a whole.
func AdmitLocal(e Entry) bool {
	return strings.HasPrefix(e.RequestID, "req_") &&
		!proxyMessageID.MatchString(e.MessageID) &&
		strings.HasPrefix(e.Model, "claude-")
}

// localAttribution returns the account admitted local entries belong to
// when they do not dedupe against the ledger, and whether that account was
// inferred.
func (en *Engine) localAttribution() (string, bool) {
	if en.localAccount != "" {
		return en.localAccount, false
	}
	if en.accounts == nil {
		return "", false
	}
	id, n := "", 0
	for _, a := range en.accounts() {
		if a.AutoImport {
			id, n = a.ID, n+1
		}
	}
	if n == 1 {
		return id, true
	}
	return "", false
}

// collect returns copies of the kept entries attributed to accountID, or of
// every entry when all is set, with AccountID and Inferred resolved.
func (en *Engine) collect(accountID string, all bool) []Entry {
	return en.collectFrom(accountID, all, en.now().Add(-en.window))
}

// collectFrom is collect with the window starting at cutoff.
func (en *Engine) collectFrom(accountID string, all bool, cutoff time.Time) []Entry {
	localID, inferred := en.localAttribution()
	en.mu.Lock()
	defer en.mu.Unlock()
	var out []Entry
	for i := range en.dedup.entries {
		e := &en.dedup.entries[i]
		if e.Timestamp.Before(cutoff) {
			continue
		}
		acct, inf := e.AccountID, false
		if e.Source == SourceLocal {
			acct, inf = localID, inferred && localID != ""
		}
		if !all && acct != accountID {
			continue
		}
		c := *e
		c.AccountID, c.Inferred = acct, inf
		out = append(out, c)
	}
	return out
}

// Entries returns every entry in the window, deduplicated and attributed,
// in timestamp order. Unattributed entries have an empty AccountID.
func (en *Engine) Entries() []Entry {
	if en == nil {
		return nil
	}
	out := en.collect("", true)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// ReportEntries returns the entries a report from since (YYYYMMDD in loc;
// empty is unbounded) reads, in timestamp order: Entries, preceded by the
// ledger's entries older than the in-memory window when since reaches
// before it. Those older entries are read from disk outside the engine
// lock. windowStart is the start of the in-memory window. Claude Code's own
// transcripts are only read inside the window, so partialLocal reports
// that since reaches before it while local transcripts are scanned: the
// older part of the report then lacks their usage.
func (en *Engine) ReportEntries(since string, loc *time.Location) (entries []Entry, windowStart time.Time, partialLocal bool) {
	if en == nil {
		return nil, time.Time{}, false
	}
	if loc == nil {
		loc = en.loc
	}
	cutoff := en.now().Add(-en.window)
	window := en.collectFrom("", true, cutoff)
	history := since == ""
	var sinceDay time.Time
	if !history {
		t, err := time.ParseInLocation("20060102", since, loc)
		if err != nil {
			history = true
		} else {
			sinceDay = t
			history = t.Before(cutoff)
		}
	}
	if history {
		entries = en.historyEntries(sinceDay, cutoff)
		partialLocal = en.scanLocal
	}
	entries = append(entries, window...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
	return entries, cutoff, partialLocal
}

// historyEntries reads the ledger's entries before cutoff, deduplicated,
// from the day files that can hold them: dated no later than cutoff's UTC
// day and, when since is set, no earlier than the UTC day before it.
func (en *Engine) historyEntries(since, cutoff time.Time) []Entry {
	matches, err := filepath.Glob(filepath.Join(en.root, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil
	}
	last := cutoff.UTC().Truncate(24 * time.Hour)
	var first time.Time
	if !since.IsZero() {
		first = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	}
	dedup := NewDeduper()
	for _, path := range matches {
		date, ok := ledgerFileDate(filepath.Base(path))
		if !ok || date.After(last) || !first.IsZero() && date.Before(first) {
			continue
		}
		read, err := ReadLedgerFile(path)
		if err != nil {
			en.log.Warn("ccusage engine: read ledger history", slog.String("path", path), slog.String("error", err.Error()))
			continue
		}
		for _, e := range read {
			if e.Timestamp.Before(cutoff) {
				dedup.Add(e)
			}
		}
	}
	return dedup.Entries()
}

// Summary returns a copy of an account's summary.
func (en *Engine) Summary(accountID string) Summary {
	if en == nil {
		return Summary{}
	}
	en.mu.Lock()
	defer en.mu.Unlock()
	if s := en.summaries[accountID]; s != nil {
		return s.clone()
	}
	return Summary{}
}

// UpdateSummary changes an account's summary under the engine lock; the
// change is saved on the next poll or at Close.
func (en *Engine) UpdateSummary(accountID string, fn func(*Summary)) {
	if en == nil || fn == nil {
		return
	}
	en.mu.Lock()
	defer en.mu.Unlock()
	fn(en.summaryLocked(accountID))
	en.dirty = true
	delete(en.cache, accountID)
}

func (en *Engine) summaryLocked(accountID string) *Summary {
	s := en.summaries[accountID]
	if s == nil {
		s = &Summary{}
		en.summaries[accountID] = s
	}
	return s
}

// Snapshot returns accountID's usage at now. anchors are known 5-hour
// window starts, such as a unified 5h reset minus five hours; they are
// remembered in the account's summary, so earlier windows stay anchored.
// The result is reused for SnapshotTTL while the anchors are the same.
// accountID "" is the unattributed usage.
func (en *Engine) Snapshot(accountID string, now time.Time, anchors []time.Time) *Snapshot {
	if en == nil {
		return nil
	}
	key := anchorsKey(anchors)
	en.mu.Lock()
	if c, ok := en.cache[accountID]; ok && c.anchors == key && !now.Before(c.at) && now.Sub(c.at) < en.ttl {
		en.mu.Unlock()
		return c.snap
	}
	merged, changed := mergeAnchors(en.summaryLocked(accountID).Anchors5h, anchors, now.Add(-en.window))
	if changed {
		en.summaries[accountID].Anchors5h = merged
		en.dirty = true
	}
	merged = append([]time.Time(nil), merged...)
	en.mu.Unlock()

	entries := en.collect(accountID, false)
	snap := en.build(accountID, now, entries, merged)

	en.mu.Lock()
	s := en.summaryLocked(accountID)
	for i := range snap.Blocks {
		b := &snap.Blocks[i]
		if b.IsActive || b.IsGap {
			continue
		}
		if tok := b.TotalTokens(); tok > s.MaxBlockTokens {
			s.MaxBlockTokens, s.MaxBlockStart = tok, b.Start
			en.dirty = true
		}
		if b.CostUSD > s.MaxBlockCostUSD {
			s.MaxBlockCostUSD = b.CostUSD
			en.dirty = true
		}
	}
	snap.Summary = s.clone()
	en.cache[accountID] = cachedSnapshot{at: now, anchors: key, snap: snap}
	en.mu.Unlock()
	return snap
}

func (en *Engine) build(accountID string, now time.Time, entries []Entry, anchors []time.Time) *Snapshot {
	snap := &Snapshot{AccountID: accountID, At: now}
	snap.Blocks = IdentifyBlocks(entries, en.dur, now, anchors, en.mode, en.pricer)
	for i := range snap.Blocks {
		b := &snap.Blocks[i]
		if b.IsActive {
			snap.Active = b
			snap.BurnRate = BurnRateOf(*b)
			snap.Projection = Project(*b, now)
			snap.Window5h = WindowUsage{Start: b.Start, End: b.End, Entries: len(b.Entries), Tokens: b.TotalTokens(), CostUSD: b.CostUSD}
		}
	}

	local := now.In(en.loc)
	snap.Window7d = WindowUsage{Start: now.Add(-sevenDays), End: now}
	snap.Today = WindowUsage{Start: time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, en.loc), End: now}
	models := make(map[string]*ModelUsage)
	for i := range entries {
		e := &entries[i]
		if e.Timestamp.After(now) {
			continue
		}
		if e.Inferred {
			snap.Inferred = true
		}
		cost := EntryCost(*e, en.mode, en.pricer)
		tok := e.TotalTokens()
		snap.running = append(snap.running, usagePoint{at: e.Timestamp, tokens: tok, cost: cost})
		in7d := !e.Timestamp.Before(snap.Window7d.Start)
		inToday := !e.Timestamp.Before(snap.Today.Start)
		if !in7d && !inToday {
			continue
		}
		if inToday {
			snap.Today.add(tok, cost)
		}
		if !in7d {
			continue
		}
		snap.Window7d.add(tok, cost)
		if e.DisplayModel == "" {
			continue
		}
		m := models[e.DisplayModel]
		if m == nil {
			m = &ModelUsage{Model: e.DisplayModel}
			models[e.DisplayModel] = m
		}
		m.Input += e.Input
		m.Output += e.Output
		m.CacheCreate += e.CacheCreate
		m.CacheRead += e.CacheRead
		m.TotalTokens += tok
		m.CostUSD += cost
	}
	sort.SliceStable(snap.running, func(i, j int) bool { return snap.running[i].at.Before(snap.running[j].at) })
	for i := 1; i < len(snap.running); i++ {
		snap.running[i].tokens += snap.running[i-1].tokens
		snap.running[i].cost += snap.running[i-1].cost
	}
	for _, m := range models {
		snap.Models = append(snap.Models, *m)
	}
	sort.Slice(snap.Models, func(i, j int) bool {
		a, b := snap.Models[i], snap.Models[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return a.Model < b.Model
	})
	return snap
}

func (w *WindowUsage) add(tokens int64, cost float64) {
	w.Entries++
	w.Tokens += tokens
	w.CostUSD += cost
}

func anchorsKey(anchors []time.Time) string {
	var b strings.Builder
	for _, a := range anchors {
		b.WriteString(strconv.FormatInt(a.UnixMilli(), 36))
		b.WriteByte(',')
	}
	return b.String()
}

// mergeAnchors adds fresh anchors to known ones, drops those before cutoff,
// merges anchors less than a minute apart into the later one and keeps the
// latest maxSummaryAnchors. changed reports whether the result differs from
// known.
func mergeAnchors(known, fresh []time.Time, cutoff time.Time) (merged []time.Time, changed bool) {
	all := make([]time.Time, 0, len(known)+len(fresh))
	for _, a := range append(append([]time.Time(nil), known...), fresh...) {
		if a.IsZero() || a.Before(cutoff) {
			continue
		}
		all = append(all, a.UTC().Truncate(time.Millisecond))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
	for _, a := range all {
		if n := len(merged); n > 0 && a.Sub(merged[n-1]) < anchorMergeGap {
			merged[n-1] = a
			continue
		}
		merged = append(merged, a)
	}
	if len(merged) > maxSummaryAnchors {
		merged = merged[len(merged)-maxSummaryAnchors:]
	}
	if len(merged) != len(known) {
		return merged, true
	}
	for i := range merged {
		if !merged[i].Equal(known[i]) {
			return merged, true
		}
	}
	return merged, false
}

func (en *Engine) summaryPath() string { return filepath.Join(en.root, SummaryFileName) }

func (en *Engine) loadSummaries() {
	data, err := os.ReadFile(en.summaryPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			en.log.Warn("ccusage engine: read summaries", slog.String("error", err.Error()))
		}
		return
	}
	var stored map[string]*Summary
	if err := json.Unmarshal(data, &stored); err != nil {
		en.log.Warn("ccusage engine: parse summaries", slog.String("error", err.Error()))
		return
	}
	for id, s := range stored {
		if s != nil {
			en.summaries[id] = s
		}
	}
}

// saveSummaries writes the summaries when they changed, atomically.
func (en *Engine) saveSummaries() {
	en.saveMu.Lock()
	defer en.saveMu.Unlock()
	en.mu.Lock()
	if !en.dirty {
		en.mu.Unlock()
		return
	}
	stored := make(map[string]Summary, len(en.summaries))
	for id, s := range en.summaries {
		// Unattributed usage has no account to keep a summary for.
		if id != "" {
			stored[id] = s.clone()
		}
	}
	en.dirty = false
	en.mu.Unlock()

	data, err := json.Marshal(stored)
	if err == nil {
		err = writeFileAtomic(en.summaryPath(), data)
	}
	if err != nil {
		en.log.Warn("ccusage engine: save summaries", slog.String("error", err.Error()))
		en.mu.Lock()
		en.dirty = true
		en.mu.Unlock()
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}
