package ccusage

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// dateKeyLayout is the layout of a report's date key.
const dateKeyLayout = "2006-01-02"

// ReportOptions configures Daily, Weekly, Monthly and Session.
type ReportOptions struct {
	// Location decides which day an entry falls on. Nil means time.Local,
	// as ccusage uses the system time zone by default.
	Location *time.Location
	Mode     CostMode
	Pricer   Pricer
	// Since and Until are inclusive YYYYMMDD (or YYYY-MM-DD) bounds on the
	// date key; empty means unbounded. See ParseDateBound.
	Since string
	Until string
	// ByAccount splits every row by Entry.AccountID and fills the rows'
	// AccountID.
	ByAccount bool
	// StartOfWeek is the first day of a Weekly bucket; the zero value is
	// Sunday, ccusage's default.
	StartOfWeek time.Weekday
}

// ModelBreakdown is one model's share of a report row.
type ModelBreakdown struct {
	ModelName           string  `json:"modelName"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
	// MissingPricing marks a model that had calculated usage the pricer
	// could not price.
	MissingPricing bool `json:"missingPricing,omitzero"`
}

// SummaryRow is one row of a daily, weekly or monthly report. Exactly one of
// Date, Week and Month is set.
type SummaryRow struct {
	Date string `json:"date,omitempty"`
	// Week is the date of the week's first day.
	Week  string `json:"week,omitempty"`
	Month string `json:"month,omitempty"`
	// AccountID is set only when the report is split by account.
	AccountID           string           `json:"accountId,omitempty"`
	InputTokens         int64            `json:"inputTokens"`
	OutputTokens        int64            `json:"outputTokens"`
	CacheCreationTokens int64            `json:"cacheCreationTokens"`
	CacheReadTokens     int64            `json:"cacheReadTokens"`
	TotalTokens         int64            `json:"totalTokens"`
	TotalCost           float64          `json:"totalCost"`
	ModelsUsed          []string         `json:"modelsUsed"`
	ModelBreakdowns     []ModelBreakdown `json:"modelBreakdowns"`
}

// SessionRow is one row of a session report.
type SessionRow struct {
	SessionID   string `json:"sessionId"`
	ProjectPath string `json:"projectPath"`
	// AccountID is set only when the report is split by account.
	AccountID           string  `json:"accountId,omitempty"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	TotalTokens         int64   `json:"totalTokens"`
	TotalCost           float64 `json:"totalCost"`
	// FirstActivity and LastActivity are RFC 3339 UTC with milliseconds.
	FirstActivity   string           `json:"firstActivity"`
	LastActivity    string           `json:"lastActivity"`
	ModelsUsed      []string         `json:"modelsUsed"`
	ModelBreakdowns []ModelBreakdown `json:"modelBreakdowns"`
}

// Totals sums a report's rows.
type Totals struct {
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	TotalTokens         int64   `json:"totalTokens"`
	TotalCost           float64 `json:"totalCost"`
	// UnpricedModels lists, sorted, the models with MissingPricing set in
	// any row.
	UnpricedModels []string `json:"unpricedModels,omitempty"`
}

// DailyReport is ccusage's "daily --json" output.
type DailyReport struct {
	Daily  []SummaryRow `json:"daily"`
	Totals Totals       `json:"totals"`
}

// WeeklyReport is ccusage's "weekly --json" output.
type WeeklyReport struct {
	Weekly []SummaryRow `json:"weekly"`
	Totals Totals       `json:"totals"`
}

// MonthlyReport is ccusage's "monthly --json" output.
type MonthlyReport struct {
	Monthly []SummaryRow `json:"monthly"`
	Totals  Totals       `json:"totals"`
}

// SessionReport is ccusage's "session --json" output.
type SessionReport struct {
	Sessions []SessionRow `json:"sessions"`
	Totals   Totals       `json:"totals"`
}

// ParseDateBound validates a since or until bound, YYYYMMDD or YYYY-MM-DD,
// and returns it as YYYYMMDD. The empty string stays empty.
func ParseDateBound(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	compact := s
	if len(s) == 10 && s[4] == '-' && s[7] == '-' {
		compact = s[:4] + s[5:7] + s[8:]
	}
	if len(compact) != 8 {
		return "", fmt.Errorf("ccusage: date %q is not YYYYMMDD", s)
	}
	if _, err := time.Parse("20060102", compact); err != nil {
		return "", fmt.Errorf("ccusage: date %q is not YYYYMMDD", s)
	}
	return compact, nil
}

// Daily groups entries by day in opts.Location, keeping days within
// opts.Since and opts.Until, in ascending date order. Entries should already
// be deduplicated.
func Daily(entries []Entry, opts ReportOptions) DailyReport {
	rows := dailyRows(entries, opts)
	return DailyReport{Daily: rows, Totals: totalsOf(rows)}
}

// Weekly sums the Daily rows into weeks starting on opts.StartOfWeek. As in
// ccusage, since and until select days, not weeks, so an edge week can be
// partial.
func Weekly(entries []Entry, opts ReportOptions) WeeklyReport {
	start := opts.StartOfWeek
	rows := bucketRows(dailyRows(entries, opts), func(date string) string { return weekStart(date, start) },
		func(r *SummaryRow, key string) { r.Week = key })
	return WeeklyReport{Weekly: rows, Totals: totalsOf(rows)}
}

// Monthly sums the Daily rows into calendar months, keyed YYYY-MM.
func Monthly(entries []Entry, opts ReportOptions) MonthlyReport {
	rows := bucketRows(dailyRows(entries, opts), func(date string) string { return date[:7] },
		func(r *SummaryRow, key string) { r.Month = key })
	return MonthlyReport{Monthly: rows, Totals: totalsOf(rows)}
}

// Session groups entries by project path and session, the session named by
// the transcript path (or the line's sessionId when no path named one). Only
// entries whose day is within opts.Since and opts.Until count. Sessions
// without tokens are left out, and the rest are ordered by cost, highest
// first.
func Session(entries []Entry, opts ReportOptions) SessionReport {
	loc := reportLocation(opts.Location)
	since, until := compactBound(opts.Since), compactBound(opts.Until)
	type sessionKey struct{ account, project, session string }
	type session struct {
		acc         accumulator
		first, last time.Time
		lastEntry   *Entry
	}
	var groups []*session
	index := make(map[sessionKey]int)
	for i := range entries {
		e := &entries[i]
		if !withinDateRange(dateKey(e.Timestamp.Truncate(time.Millisecond), loc), since, until) {
			continue
		}
		key := sessionKey{project: e.ProjectPath, session: e.PathSessionID}
		if key.session == "" {
			key.session = e.SessionID
		}
		if opts.ByAccount {
			key.account = e.AccountID
		}
		n, ok := index[key]
		if !ok {
			n = len(groups)
			index[key] = n
			groups = append(groups, &session{})
		}
		g := groups[n]
		g.acc.add(e, opts.Mode, opts.Pricer)
		ts := e.Timestamp.Truncate(time.Millisecond)
		if g.lastEntry == nil || ts.After(g.last) {
			g.last, g.lastEntry = ts, e
		}
		if g.first.IsZero() || ts.Before(g.first) {
			g.first = ts
		}
	}

	rows := make([]SessionRow, 0, len(groups))
	for _, g := range groups {
		if g.acc.input == 0 && g.acc.output == 0 && g.acc.cacheCreate == 0 && g.acc.cacheRead == 0 {
			continue
		}
		s := g.acc.summary()
		row := SessionRow{
			SessionID:           g.lastEntry.PathSessionID,
			ProjectPath:         g.lastEntry.ProjectPath,
			InputTokens:         s.InputTokens,
			OutputTokens:        s.OutputTokens,
			CacheCreationTokens: s.CacheCreationTokens,
			CacheReadTokens:     s.CacheReadTokens,
			TotalTokens:         s.TotalTokens,
			TotalCost:           s.TotalCost,
			FirstActivity:       g.first.UTC().Format(blockIDLayout),
			LastActivity:        g.last.UTC().Format(blockIDLayout),
			ModelsUsed:          s.ModelsUsed,
			ModelBreakdowns:     s.ModelBreakdowns,
		}
		if row.SessionID == "" {
			row.SessionID = g.lastEntry.SessionID
		}
		if opts.ByAccount {
			row.AccountID = g.lastEntry.AccountID
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b SessionRow) int { return cmp.Compare(b.TotalCost, a.TotalCost) })

	var totals Totals
	var unpriced []string
	for i := range rows {
		r := &rows[i]
		totals.add(r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens, r.TotalCost)
		unpriced = appendUnpriced(unpriced, r.ModelBreakdowns)
	}
	totals.UnpricedModels = sortedUnique(unpriced)
	return SessionReport{Sessions: rows, Totals: totals}
}

// dailyRows groups entries by date (and account), keeps the dates within the
// options' bounds and sorts by date, then account.
func dailyRows(entries []Entry, opts ReportOptions) []SummaryRow {
	loc := reportLocation(opts.Location)
	since, until := compactBound(opts.Since), compactBound(opts.Until)
	type dayKey struct{ date, account string }
	groups := make(map[dayKey]*accumulator)
	for i := range entries {
		e := &entries[i]
		key := dayKey{date: dateKey(e.Timestamp.Truncate(time.Millisecond), loc)}
		if !withinDateRange(key.date, since, until) {
			continue
		}
		if opts.ByAccount {
			key.account = e.AccountID
		}
		acc := groups[key]
		if acc == nil {
			acc = &accumulator{}
			groups[key] = acc
		}
		acc.add(e, opts.Mode, opts.Pricer)
	}
	rows := make([]SummaryRow, 0, len(groups))
	for key, acc := range groups {
		row := acc.summary()
		row.Date, row.AccountID = key.date, key.account
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b SummaryRow) int {
		return cmp.Or(cmp.Compare(a.Date, b.Date), cmp.Compare(a.AccountID, b.AccountID))
	})
	return rows
}

// bucketRows merges date-sorted daily rows into buckets named by bucketOf,
// as ccusage's summarize_summaries_by_bucket does: token counts and costs are
// summed, models keep their first-seen order and breakdowns are merged by
// model and sorted by cost. The result is sorted by bucket, then account.
func bucketRows(daily []SummaryRow, bucketOf func(date string) string, setKey func(*SummaryRow, string)) []SummaryRow {
	type bucketKey struct{ bucket, account string }
	type bucket struct {
		key   bucketKey
		row   SummaryRow
		index map[string]int
	}
	var buckets []*bucket
	byKey := make(map[bucketKey]*bucket)
	for i := range daily {
		d := &daily[i]
		key := bucketKey{bucketOf(d.Date), d.AccountID}
		b := byKey[key]
		if b == nil {
			b = &bucket{key: key, index: make(map[string]int)}
			b.row.AccountID = d.AccountID
			b.row.ModelsUsed = []string{}
			b.row.ModelBreakdowns = []ModelBreakdown{}
			setKey(&b.row, key.bucket)
			byKey[key] = b
			buckets = append(buckets, b)
		}
		r := &b.row
		r.InputTokens = satAdd(r.InputTokens, d.InputTokens)
		r.OutputTokens = satAdd(r.OutputTokens, d.OutputTokens)
		r.CacheCreationTokens = satAdd(r.CacheCreationTokens, d.CacheCreationTokens)
		r.CacheReadTokens = satAdd(r.CacheReadTokens, d.CacheReadTokens)
		r.TotalCost += d.TotalCost
		for _, m := range d.ModelsUsed {
			if !slices.Contains(r.ModelsUsed, m) {
				r.ModelsUsed = append(r.ModelsUsed, m)
			}
		}
		for _, item := range d.ModelBreakdowns {
			n, ok := b.index[item.ModelName]
			if !ok {
				n = len(r.ModelBreakdowns)
				b.index[item.ModelName] = n
				r.ModelBreakdowns = append(r.ModelBreakdowns, ModelBreakdown{ModelName: item.ModelName})
			}
			mb := &r.ModelBreakdowns[n]
			mb.InputTokens = satAdd(mb.InputTokens, item.InputTokens)
			mb.OutputTokens = satAdd(mb.OutputTokens, item.OutputTokens)
			mb.CacheCreationTokens = satAdd(mb.CacheCreationTokens, item.CacheCreationTokens)
			mb.CacheReadTokens = satAdd(mb.CacheReadTokens, item.CacheReadTokens)
			mb.Cost += item.Cost
			mb.MissingPricing = mb.MissingPricing || item.MissingPricing
		}
	}
	rows := make([]SummaryRow, 0, len(buckets))
	for _, b := range buckets {
		r := b.row
		r.TotalTokens = satAdd(satAdd(r.InputTokens, r.OutputTokens), satAdd(r.CacheCreationTokens, r.CacheReadTokens))
		sortBreakdowns(r.ModelBreakdowns)
		rows = append(rows, r)
	}
	slices.SortStableFunc(rows, func(a, b SummaryRow) int {
		return cmp.Or(cmp.Compare(a.Week+a.Month, b.Week+b.Month), cmp.Compare(a.AccountID, b.AccountID))
	})
	return rows
}

// accumulator sums entries into a report row.
type accumulator struct {
	input, output, cacheCreate, cacheRead int64
	cost                                  float64
	breakdowns                            []ModelBreakdown
	index                                 map[string]int
}

func (a *accumulator) add(e *Entry, mode CostMode, p Pricer) {
	cost := EntryCost(*e, mode, p)
	a.input = satAdd(a.input, e.Input)
	a.output = satAdd(a.output, e.Output)
	a.cacheCreate = satAdd(a.cacheCreate, e.CacheCreate)
	a.cacheRead = satAdd(a.cacheRead, e.CacheRead)
	a.cost += cost
	if e.DisplayModel == "" {
		return
	}
	if a.index == nil {
		a.index = make(map[string]int)
	}
	n, ok := a.index[e.DisplayModel]
	if !ok {
		n = len(a.breakdowns)
		a.index[e.DisplayModel] = n
		a.breakdowns = append(a.breakdowns, ModelBreakdown{ModelName: e.DisplayModel})
	}
	b := &a.breakdowns[n]
	b.InputTokens = satAdd(b.InputTokens, e.Input)
	b.OutputTokens = satAdd(b.OutputTokens, e.Output)
	b.CacheCreationTokens = satAdd(b.CacheCreationTokens, e.CacheCreate)
	b.CacheReadTokens = satAdd(b.CacheReadTokens, e.CacheRead)
	b.Cost += cost
	if missingPricing(e, mode, p) {
		b.MissingPricing = true
	}
}

// summary returns the row with models in first-seen order and breakdowns
// sorted by cost, highest first.
func (a *accumulator) summary() SummaryRow {
	models := make([]string, len(a.breakdowns))
	for i := range a.breakdowns {
		models[i] = a.breakdowns[i].ModelName
	}
	breakdowns := slices.Clone(a.breakdowns)
	if breakdowns == nil {
		breakdowns = []ModelBreakdown{}
	}
	sortBreakdowns(breakdowns)
	return SummaryRow{
		InputTokens:         a.input,
		OutputTokens:        a.output,
		CacheCreationTokens: a.cacheCreate,
		CacheReadTokens:     a.cacheRead,
		TotalTokens:         satAdd(satAdd(a.input, a.output), satAdd(a.cacheCreate, a.cacheRead)),
		TotalCost:           a.cost,
		ModelsUsed:          models,
		ModelBreakdowns:     breakdowns,
	}
}

// missingPricing reports whether e's cost was to be calculated from tokens
// but p had no price for its model, as ccusage's
// missing_pricing_model_for_usage decides.
func missingPricing(e *Entry, mode CostMode, p Pricer) bool {
	if mode == CostModeDisplay || (mode != CostModeCalculate && e.CostUSD != nil) {
		return false
	}
	if e.TotalTokens() == 0 || e.Model == "" || p == nil {
		return false
	}
	_, ok := p.Find(e.Model)
	return !ok
}

// sortBreakdowns orders breakdowns by cost, highest first, keeping ties in
// first-seen order.
func sortBreakdowns(b []ModelBreakdown) {
	slices.SortStableFunc(b, func(x, y ModelBreakdown) int { return cmp.Compare(y.Cost, x.Cost) })
}

func (t *Totals) add(input, output, cacheCreate, cacheRead int64, cost float64) {
	t.InputTokens = satAdd(t.InputTokens, input)
	t.OutputTokens = satAdd(t.OutputTokens, output)
	t.CacheCreationTokens = satAdd(t.CacheCreationTokens, cacheCreate)
	t.CacheReadTokens = satAdd(t.CacheReadTokens, cacheRead)
	t.TotalTokens = satAdd(satAdd(t.InputTokens, t.OutputTokens), satAdd(t.CacheCreationTokens, t.CacheReadTokens))
	t.TotalCost += cost
}

func totalsOf(rows []SummaryRow) Totals {
	var t Totals
	var unpriced []string
	for i := range rows {
		r := &rows[i]
		t.add(r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens, r.TotalCost)
		unpriced = appendUnpriced(unpriced, r.ModelBreakdowns)
	}
	t.UnpricedModels = sortedUnique(unpriced)
	return t
}

func appendUnpriced(dst []string, breakdowns []ModelBreakdown) []string {
	for i := range breakdowns {
		if breakdowns[i].MissingPricing {
			dst = append(dst, breakdowns[i].ModelName)
		}
	}
	return dst
}

func sortedUnique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	slices.Sort(s)
	return slices.Compact(s)
}

func reportLocation(loc *time.Location) *time.Location {
	if loc == nil {
		return time.Local
	}
	return loc
}

// dateKey is t's YYYY-MM-DD date in loc.
func dateKey(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(dateKeyLayout)
}

// compactBound strips the dashes from a YYYY-MM-DD bound.
func compactBound(s string) string {
	return strings.ReplaceAll(s, "-", "")
}

// withinDateRange compares a YYYY-MM-DD date with compact YYYYMMDD bounds as
// strings, like ccusage's date_within_range.
func withinDateRange(date, since, until string) bool {
	if since == "" && until == "" {
		return true
	}
	compact := compactBound(date)
	return (since == "" || compact >= since) && (until == "" || compact <= until)
}

// weekStart returns the date of the first day, start, of date's week, using
// plain calendar arithmetic as ccusage's week_start does. An unparsable date
// is its own bucket.
func weekStart(date string, start time.Weekday) string {
	d, err := time.Parse(dateKeyLayout, date)
	if err != nil {
		return date
	}
	shift := ((int(d.Weekday())-int(start))%7 + 7) % 7
	return d.AddDate(0, 0, -shift).Format(dateKeyLayout)
}

// BlocksOptions configures Blocks.
type BlocksOptions struct {
	// Location decides the day a block starts on for Since and Until. Nil
	// means time.Local.
	Location *time.Location
	// Since and Until are inclusive YYYYMMDD bounds on the block's start
	// date; empty means unbounded.
	Since string
	Until string
	// TokenLimit adds a tokenLimitStatus to an active block's row, like
	// ccusage's --token-limit: nil adds none, "" or "max" uses
	// MaxTokensFromHistory of the reported blocks, and a number is used as is.
	TokenLimit *string
}

// BlockTokenCounts is a block row's token buckets.
type BlockTokenCounts struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
}

// TokenLimitStatus compares an active block's projection with a token limit.
type TokenLimitStatus struct {
	Limit          int64   `json:"limit"`
	ProjectedUsage int64   `json:"projectedUsage"`
	PercentUsed    float64 `json:"percentUsed"`
	// Status is LimitOK, LimitWarning or LimitExceeds.
	Status string `json:"status"`
}

// BlockRow is one block of ccusage's "blocks --json" output. Times are RFC
// 3339 UTC with milliseconds.
type BlockRow struct {
	ID            string           `json:"id"`
	StartTime     string           `json:"startTime"`
	EndTime       string           `json:"endTime"`
	ActualEndTime *string          `json:"actualEndTime"`
	IsActive      bool             `json:"isActive"`
	IsGap         bool             `json:"isGap"`
	Entries       int              `json:"entries"`
	TokenCounts   BlockTokenCounts `json:"tokenCounts"`
	TotalTokens   int64            `json:"totalTokens"`
	CostUSD       float64          `json:"costUSD"`
	Models        []string         `json:"models"`
	// BurnRate and Projection are set for an active block only.
	BurnRate            *BurnRate         `json:"burnRate"`
	Projection          *Projection       `json:"projection"`
	TokenLimitStatus    *TokenLimitStatus `json:"tokenLimitStatus,omitempty"`
	UsageLimitResetTime *string           `json:"usageLimitResetTime,omitempty"`
}

// BlocksReport is ccusage's "blocks --json" output.
type BlocksReport struct {
	Blocks []BlockRow `json:"blocks"`
}

// Blocks renders blocks from IdentifyBlocks as ccusage's blocks report, as
// seen at now: blocks starting outside the date bounds are left out, and the
// rest are listed in start order.
func Blocks(blocks []Block, now time.Time, opts BlocksOptions) BlocksReport {
	loc := reportLocation(opts.Location)
	since, until := compactBound(opts.Since), compactBound(opts.Until)
	kept := make([]Block, 0, len(blocks))
	for i := range blocks {
		if withinDateRange(dateKey(blocks[i].Start, loc), since, until) {
			kept = append(kept, blocks[i])
		}
	}
	slices.SortStableFunc(kept, func(a, b Block) int { return a.Start.Compare(b.Start) })

	limit := tokenLimit(opts.TokenLimit, kept)
	rows := make([]BlockRow, 0, len(kept))
	for i := range kept {
		rows = append(rows, blockRow(kept[i], now, limit))
	}
	return BlocksReport{Blocks: rows}
}

// tokenLimit resolves BlocksOptions.TokenLimit as ccusage's
// parse_token_limit does; 0 means none.
func tokenLimit(s *string, blocks []Block) int64 {
	if s == nil {
		return 0
	}
	switch *s {
	case "", "max":
		return MaxTokensFromHistory(blocks)
	}
	// Unlike ccusage, a limit that does not parse or is 0 or less adds no
	// status rather than failing or reporting an infinite percentage.
	n, err := strconv.ParseInt(*s, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func blockRow(b Block, now time.Time, limit int64) BlockRow {
	row := BlockRow{
		ID:        b.ID,
		StartTime: b.Start.UTC().Format(blockIDLayout),
		EndTime:   b.End.UTC().Format(blockIDLayout),
		IsActive:  b.IsActive,
		IsGap:     b.IsGap,
		Entries:   len(b.Entries),
		TokenCounts: BlockTokenCounts{
			InputTokens:              b.InputTokens,
			OutputTokens:             b.OutputTokens,
			CacheCreationInputTokens: b.CacheCreateTokens,
			CacheReadInputTokens:     b.CacheReadTokens,
		},
		TotalTokens: b.TotalTokens(),
		CostUSD:     b.CostUSD,
		Models:      slices.Clone(b.Models),
	}
	if row.Models == nil {
		row.Models = []string{}
	}
	if !b.IsGap && !b.ActualEnd.IsZero() {
		s := b.ActualEnd.UTC().Format(blockIDLayout)
		row.ActualEndTime = &s
	}
	if b.IsActive {
		row.BurnRate = BurnRateOf(b)
		row.Projection = Project(b, now)
	}
	if row.Projection != nil && limit > 0 {
		projected := row.Projection.TotalTokens
		row.TokenLimitStatus = &TokenLimitStatus{
			Limit:          limit,
			ProjectedUsage: projected,
			PercentUsed:    float64(projected) / float64(limit) * 100,
			Status:         LimitStatus(projected, limit),
		}
	}
	if b.UsageLimitResetAt != nil {
		s := b.UsageLimitResetAt.UTC().Format(blockIDLayout)
		row.UsageLimitResetTime = &s
	}
	return row
}
