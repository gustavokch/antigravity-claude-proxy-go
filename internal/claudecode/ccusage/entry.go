package ccusage

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"
)

// Entry sources. A ledger entry is written by the proxy for a response it
// served; a local entry comes from Claude Code's own transcripts.
const (
	SourceLedger = "ledger"
	SourceLocal  = "local"
)

// Ledger entry origins: a response the proxy served, or a cache bump it
// made on its own.
const (
	OriginProxy     = "proxy"
	OriginCacheBump = "cachebump"
)

const syntheticModel = "<synthetic>"

// Entry is one usage record: an assistant response or an advisor iteration
// inside one.
//
// Empty RequestID, MessageID and SessionID mean the line did not carry them;
// ParseLine rejects lines where they are present but empty.
type Entry struct {
	Timestamp time.Time
	// SessionID is the line's sessionId, falling back to the session named by
	// the file path. Dedupe keys on it.
	SessionID string
	RequestID string
	MessageID string
	// Model is the model as logged; DisplayModel is the name reports list.
	// DisplayModel carries a "-fast" suffix for fast-mode responses and is
	// empty for synthetic messages and lines without a model, so reports
	// leave those out of their model lists.
	Model        string
	DisplayModel string
	Speed        string // "standard", "fast" or empty

	Input  int64
	Output int64
	// CacheCreate is CacheCreate5m + CacheCreate1h. Without a cache_creation
	// breakdown every cache write counts as a 5-minute write, as in ccusage's
	// cost calculation.
	CacheCreate   int64
	CacheCreate5m int64
	CacheCreate1h int64
	CacheRead     int64

	CostUSD           *float64
	IsSidechain       bool
	IsAPIError        bool
	UsageLimitResetAt *time.Time

	AccountID string
	// Project is the first directory under projects/. PathSessionID and
	// ProjectPath come from SessionParts, which session reports group by.
	Project       string
	PathSessionID string
	ProjectPath   string
	Source        string
	// Origin is the ledger line's source field (OriginProxy or
	// OriginCacheBump); it is empty for local entries.
	Origin string
}

// TotalTokens is the sum of all four token buckets.
func (e Entry) TotalTokens() int64 {
	return satAdd(satAdd(e.Input, e.Output), satAdd(e.CacheCreate, e.CacheRead))
}

// FileMeta is what a usage file's path says about the entries in it.
type FileMeta struct {
	Project     string
	SessionID   string
	ProjectPath string
}

// FileMetaFor derives the project and session of a transcript path.
func FileMetaFor(path string) FileMeta {
	sessionID, projectPath := SessionParts(path)
	return FileMeta{Project: ExtractProject(path), SessionID: sessionID, ProjectPath: projectPath}
}

// Apply attributes e to the file, filling SessionID from the path when the
// line did not name a session.
func (m FileMeta) Apply(e *Entry) {
	e.Project = m.Project
	e.PathSessionID = m.SessionID
	e.ProjectPath = m.ProjectPath
	if e.SessionID == "" {
		e.SessionID = m.SessionID
	}
}

// ReadUsageFile parses every usage line of a Claude Code transcript as local
// entries attributed to the file.
func ReadUsageFile(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta := FileMetaFor(path)
	var entries []Entry
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		for _, e := range ParseLine(line) {
			meta.Apply(&e)
			e.Source = SourceLocal
			entries = append(entries, e)
		}
	}
	return entries, nil
}

var (
	usageMarker      = []byte(`"usage":{`)
	nullMarker       = []byte("null")
	advisorMarker    = []byte(`"advisor_message"`)
	usageLimitMarker = []byte("Claude AI usage limit reached")
)

type rawLine struct {
	SessionID         *string     `json:"sessionId"`
	Timestamp         *string     `json:"timestamp"`
	Version           *string     `json:"version"`
	Message           *rawMessage `json:"message"`
	CostUSD           *float64    `json:"costUSD"`
	RequestID         *string     `json:"requestId"`
	IsAPIErrorMessage *bool       `json:"isApiErrorMessage"`
	IsSidechain       *bool       `json:"isSidechain"`
}

type rawMessage struct {
	Usage *rawUsage `json:"usage"`
	Model *string   `json:"model"`
	ID    *string   `json:"id"`
}

type rawUsage struct {
	InputTokens              *uint64           `json:"input_tokens"`
	OutputTokens             *uint64           `json:"output_tokens"`
	CacheCreationInputTokens uint64            `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     uint64            `json:"cache_read_input_tokens"`
	Speed                    *string           `json:"speed"`
	CacheCreation            *rawCacheCreation `json:"cache_creation"`
}

type rawCacheCreation struct {
	Ephemeral5m uint64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h uint64 `json:"ephemeral_1h_input_tokens"`
}

// valid mirrors the fields serde requires and the speed enum it accepts.
func (u *rawUsage) valid() bool {
	if u == nil || u.InputTokens == nil || u.OutputTokens == nil {
		return false
	}
	return u.Speed == nil || *u.Speed == "standard" || *u.Speed == "fast"
}

func (u *rawUsage) fill(e *Entry) {
	e.Input = clampInt64(*u.InputTokens)
	e.Output = clampInt64(*u.OutputTokens)
	e.CacheRead = clampInt64(u.CacheReadInputTokens)
	if u.CacheCreation != nil {
		e.CacheCreate5m = clampInt64(u.CacheCreation.Ephemeral5m)
		e.CacheCreate1h = clampInt64(u.CacheCreation.Ephemeral1h)
	} else {
		e.CacheCreate5m = clampInt64(u.CacheCreationInputTokens)
		e.CacheCreate1h = 0
	}
	e.CacheCreate = satAdd(e.CacheCreate5m, e.CacheCreate1h)
	e.Speed = ""
	if u.Speed != nil {
		e.Speed = *u.Speed
	}
}

// ParseLine parses one transcript line into its usage entries: the response
// itself followed by one entry per advisor iteration. It returns nil for a
// line that carries no usage or fails ccusage's schema checks. Path-derived
// fields, Source and AccountID are left for the caller.
func ParseLine(line []byte) []Entry {
	if !bytes.Contains(line, usageMarker) {
		return nil
	}
	if bytes.Contains(line, nullMarker) && hasUnsupportedNullField(line) {
		return nil
	}
	var raw rawLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}
	if raw.Timestamp == nil || raw.Message == nil || !raw.Message.Usage.valid() {
		return nil
	}
	ts, ok := parseTimestamp(*raw.Timestamp)
	if !ok || !validRawLine(&raw) {
		return nil
	}

	e := Entry{
		Timestamp:   ts,
		SessionID:   deref(raw.SessionID),
		RequestID:   deref(raw.RequestID),
		MessageID:   deref(raw.Message.ID),
		Model:       deref(raw.Message.Model),
		CostUSD:     raw.CostUSD,
		IsSidechain: raw.IsSidechain != nil && *raw.IsSidechain,
		IsAPIError:  raw.IsAPIErrorMessage != nil && *raw.IsAPIErrorMessage,
	}
	raw.Message.Usage.fill(&e)
	switch {
	case e.Model == "", e.Model == syntheticModel:
	case e.Speed == "fast":
		e.DisplayModel = e.Model + "-fast"
	default:
		e.DisplayModel = e.Model
	}
	if e.IsAPIError {
		e.UsageLimitResetAt = usageLimitResetAt(line)
	}

	entries := []Entry{e}
	for i, adv := range advisorIterations(line) {
		a := e
		if a.MessageID != "" {
			a.MessageID = fmt.Sprintf("%s:advisor:%d", a.MessageID, i)
		}
		a.Model = adv.model
		a.DisplayModel = adv.model
		a.CostUSD = nil
		adv.usage.fill(&a)
		entries = append(entries, a)
	}
	return entries
}

// validRawLine rejects a malformed version and IDs or model that are
// present but empty.
func validRawLine(raw *rawLine) bool {
	if raw.Version != nil && !isSemverPrefix(*raw.Version) {
		return false
	}
	for _, s := range []*string{raw.SessionID, raw.RequestID, raw.Message.ID, raw.Message.Model} {
		if s != nil && *s == "" {
			return false
		}
	}
	return true
}

type rawIteration struct {
	Type     *string `json:"type"`
	Model    *string `json:"model"`
	rawUsage `json:",inline"`
}

type advisorUsage struct {
	model string
	usage rawUsage
}

// advisorIterations returns the advisor_message iterations of a line that
// name a model. Like ccusage, any malformed iteration discards them all.
func advisorIterations(line []byte) []advisorUsage {
	if !bytes.Contains(line, advisorMarker) {
		return nil
	}
	var envelope struct {
		Message *struct {
			Usage *struct {
				Iterations []rawIteration `json:"iterations"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &envelope) != nil ||
		envelope.Message == nil || envelope.Message.Usage == nil {
		return nil
	}
	var out []advisorUsage
	for _, it := range envelope.Message.Usage.Iterations {
		if it.Type == nil || !it.rawUsage.valid() {
			return nil
		}
		if *it.Type == "advisor_message" && it.Model != nil && *it.Model != "" {
			out = append(out, advisorUsage{model: *it.Model, usage: it.rawUsage})
		}
	}
	return out
}

// hasUnsupportedNullField reports whether a line sets one of the fields
// ccusage's schema declares non-nullable to null, anywhere in the document.
// The model of an element of message.usage.iterations (or the AgentProgress
// equivalent under data.message.message) is exempt, because Claude Code
// writes it as null for the main-model iteration. Unparseable lines return
// false and fail later.
func hasUnsupportedNullField(line []byte) bool {
	var root any
	if json.Unmarshal(line, &root) != nil {
		return false
	}
	return hasUnsupportedNull(root, false, nil)
}

var iterationPaths = [][]string{
	{"message", "usage", "iterations"},
	{"data", "message", "message", "usage", "iterations"},
}

func hasUnsupportedNull(v any, allowModel bool, path []string) bool {
	switch v := v.(type) {
	case map[string]any:
		for field, child := range v {
			if child == nil && unsupportedNullable[field] {
				if !(allowModel && field == "model") {
					return true
				}
				continue
			}
			if hasUnsupportedNull(child, false, appendPath(path, field)) {
				return true
			}
		}
	case []any:
		isIterations := false
		for _, p := range iterationPaths {
			if equalPath(path, p) {
				isIterations = true
			}
		}
		for _, child := range v {
			// Array elements never match an object path.
			if hasUnsupportedNull(child, isIterations, appendPath(path, "[]")) {
				return true
			}
		}
	}
	return false
}

func appendPath(path []string, seg string) []string {
	return append(path[:len(path):len(path)], seg)
}

func equalPath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var unsupportedNullable = map[string]bool{
	"id": true, "cwd": true, "model": true, "speed": true, "costUSD": true,
	"version": true, "sessionId": true, "requestId": true, "isApiErrorMessage": true,
	"cache_read_input_tokens": true, "cache_creation_input_tokens": true,
}

// isSemverPrefix reports whether s starts with digits.digits.digit.
func isSemverPrefix(s string) bool {
	i := 0
	for part := 0; part < 2; part++ {
		start := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == start || i >= len(s) || s[i] != '.' {
			return false
		}
		i++
	}
	return i < len(s) && isDigit(s[i])
}

// usageLimitResetAt extracts the epoch after "Claude AI usage limit
// reached|" from the raw line of an API error message.
func usageLimitResetAt(line []byte) *time.Time {
	start := bytes.Index(line, usageLimitMarker)
	if start < 0 {
		return nil
	}
	bar := bytes.IndexByte(line[start:], '|')
	if bar < 0 {
		return nil
	}
	digits := line[start+bar+1:]
	n := 0
	for n < len(digits) && isDigit(digits[n]) {
		n++
	}
	secs, err := strconv.ParseInt(string(digits[:n]), 10, 64)
	if err != nil || secs <= 0 || secs > math.MaxInt64/1000 {
		return nil
	}
	t := time.Unix(secs, 0).UTC()
	return &t
}

// parseTimestamp accepts exactly the RFC 3339 shapes ccusage does: seconds
// or milliseconds, with Z or a ±HH:MM offset.
func parseTimestamp(s string) (time.Time, bool) {
	var millis, tzStart int
	switch {
	case (len(s) == 20 || len(s) == 25) && (s[19] == 'Z' || s[19] == '+' || s[19] == '-'):
		tzStart = 19
	case (len(s) == 24 || len(s) == 29) && s[19] == '.':
		ms, ok := parseDigits(s[20:23])
		if !ok {
			return time.Time{}, false
		}
		millis, tzStart = ms, 23
	default:
		return time.Time{}, false
	}
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	var f [6]int
	for i, span := range [6][2]int{{0, 4}, {5, 7}, {8, 10}, {11, 13}, {14, 16}, {17, 19}} {
		v, ok := parseDigits(s[span[0]:span[1]])
		if !ok {
			return time.Time{}, false
		}
		f[i] = v
	}
	year, month, day, hour, minute, second := f[0], f[1], f[2], f[3], f[4], f[5]
	if hour > 23 || minute > 59 || second > 59 || month < 1 || month > 12 || day < 1 {
		return time.Time{}, false
	}
	if day > time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return time.Time{}, false
	}
	offset, ok := parseOffset(s[tzStart:])
	if !ok {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, millis*int(time.Millisecond), time.UTC)
	return t.Add(-time.Duration(offset) * time.Minute), true
}

func parseOffset(s string) (int, bool) {
	if s == "Z" {
		return 0, true
	}
	if len(s) != 6 || (s[0] != '+' && s[0] != '-') || s[3] != ':' {
		return 0, false
	}
	h, ok1 := parseDigits(s[1:3])
	m, ok2 := parseDigits(s[4:6])
	if !ok1 || !ok2 || h > 23 || m > 59 {
		return 0, false
	}
	if s[0] == '-' {
		return -(h*60 + m), true
	}
	return h*60 + m, true
}

func parseDigits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
