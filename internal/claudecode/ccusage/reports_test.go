package ccusage

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"slices"
	"sort"
	"testing"
	"time"
)

// reportEntry is an entry with a logged cost, so CostModeDisplay prices it
// without a pricer.
func reportEntry(t *testing.T, ts, model string, input, output int64, cost float64) Entry {
	t.Helper()
	return Entry{
		Timestamp:    mustTime(t, ts),
		Model:        model,
		DisplayModel: model,
		Input:        input,
		Output:       output,
		CostUSD:      &cost,
	}
}

func utcOpts() ReportOptions {
	return ReportOptions{Location: time.UTC, Mode: CostModeDisplay}
}

func dates(rows []SummaryRow, key func(SummaryRow) string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = key(r)
	}
	return out
}

func TestDaily_TimezoneBoundary(t *testing.T) {
	e := []Entry{
		reportEntry(t, "2026-01-01T23:30:00Z", "m", 1, 0, 0.1),
		reportEntry(t, "2026-01-02T00:30:00Z", "m", 2, 0, 0.2),
		reportEntry(t, "2026-01-02T06:30:00Z", "m", 4, 0, 0.4),
	}
	utc := Daily(e, utcOpts())
	if got := dates(utc.Daily, func(r SummaryRow) string { return r.Date }); !slices.Equal(got, []string{"2026-01-01", "2026-01-02"}) {
		t.Fatalf("UTC dates = %v", got)
	}
	if utc.Daily[1].InputTokens != 6 {
		t.Errorf("UTC 2026-01-02 input = %d, want 6", utc.Daily[1].InputTokens)
	}

	// In Tokyo (UTC+9) the first two entries fall on 2 January.
	tokyo := time.FixedZone("JST", 9*3600)
	opts := utcOpts()
	opts.Location = tokyo
	jst := Daily(e, opts)
	if got := dates(jst.Daily, func(r SummaryRow) string { return r.Date }); !slices.Equal(got, []string{"2026-01-02"}) {
		t.Fatalf("JST dates = %v", got)
	}
	if jst.Totals.InputTokens != 7 || jst.Totals.TotalTokens != 7 || !approx(jst.Totals.TotalCost, 0.7) {
		t.Errorf("JST totals = %+v", jst.Totals)
	}

	// Since and until compare the local date key, inclusively.
	opts.Location = time.UTC
	opts.Since, opts.Until = "20260102", "2026-01-02"
	if got := Daily(e, opts); len(got.Daily) != 1 || got.Daily[0].Date != "2026-01-02" || got.Totals.InputTokens != 6 {
		t.Errorf("filtered = %+v", got)
	}
}

func TestDaily_ModelsAndBreakdowns(t *testing.T) {
	e := []Entry{
		reportEntry(t, "2026-01-02T01:00:00Z", "cheap", 10, 1, 0.01),
		reportEntry(t, "2026-01-02T02:00:00Z", "dear", 20, 2, 0.50),
		reportEntry(t, "2026-01-02T03:00:00Z", "cheap", 30, 3, 0.02),
		// A synthetic message counts toward the totals but names no model.
		{Timestamp: mustTime(t, "2026-01-02T04:00:00Z"), Model: syntheticModel, Input: 5, CostUSD: ptr(0)},
	}
	e[1].CacheCreate, e[1].CacheRead = 7, 9
	row := Daily(e, utcOpts()).Daily[0]
	if !slices.Equal(row.ModelsUsed, []string{"cheap", "dear"}) {
		t.Errorf("models = %v, want first-seen order without synthetic", row.ModelsUsed)
	}
	if len(row.ModelBreakdowns) != 2 || row.ModelBreakdowns[0].ModelName != "dear" || row.ModelBreakdowns[1].ModelName != "cheap" {
		t.Fatalf("breakdowns = %+v, want dear then cheap", row.ModelBreakdowns)
	}
	cheap := row.ModelBreakdowns[1]
	if cheap.InputTokens != 40 || cheap.OutputTokens != 4 || !approx(cheap.Cost, 0.03) {
		t.Errorf("cheap = %+v", cheap)
	}
	dear := row.ModelBreakdowns[0]
	if dear.CacheCreationTokens != 7 || dear.CacheReadTokens != 9 {
		t.Errorf("dear = %+v", dear)
	}
	if row.InputTokens != 65 || row.TotalTokens != 65+6+7+9 || !approx(row.TotalCost, 0.53) {
		t.Errorf("row = %+v", row)
	}
}

func TestDaily_CalculatedCostAndUnpricedModels(t *testing.T) {
	e := []Entry{
		{Timestamp: mustTime(t, "2026-01-02T01:00:00Z"), Model: "test-model", DisplayModel: "test-model", Input: 1000, Output: 100},
		{Timestamp: mustTime(t, "2026-01-02T02:00:00Z"), Model: "mystery", DisplayModel: "mystery", Input: 10},
		// A logged cost needs no price in auto mode.
		{Timestamp: mustTime(t, "2026-01-02T03:00:00Z"), Model: "logged", DisplayModel: "logged", Input: 10, CostUSD: ptr(0.5)},
	}
	opts := ReportOptions{Location: time.UTC, Mode: CostModeAuto, Pricer: testPricer(testPrice)}
	r := Daily(e, opts)
	if !approx(r.Totals.TotalCost, 1000*1.0+100*10.0+0.5) {
		t.Errorf("total cost = %v", r.Totals.TotalCost)
	}
	if !slices.Equal(r.Totals.UnpricedModels, []string{"mystery"}) {
		t.Errorf("unpriced = %v, want [mystery]", r.Totals.UnpricedModels)
	}
	for _, b := range r.Daily[0].ModelBreakdowns {
		if b.MissingPricing != (b.ModelName == "mystery") {
			t.Errorf("%s missingPricing = %v", b.ModelName, b.MissingPricing)
		}
	}
}

func TestDaily_ByAccount(t *testing.T) {
	a := reportEntry(t, "2026-01-02T01:00:00Z", "m", 1, 0, 0.1)
	a.AccountID = "b-acct"
	b := reportEntry(t, "2026-01-02T02:00:00Z", "m", 2, 0, 0.2)
	b.AccountID = "a-acct"
	c := reportEntry(t, "2026-01-01T02:00:00Z", "m", 4, 0, 0.4)
	c.AccountID = "b-acct"
	e := []Entry{a, b, c}

	if got := Daily(e, utcOpts()).Daily; len(got) != 2 || got[1].AccountID != "" || got[1].InputTokens != 3 {
		t.Fatalf("unsplit = %+v", got)
	}
	opts := utcOpts()
	opts.ByAccount = true
	got := Daily(e, opts).Daily
	want := []struct {
		date, account string
		input         int64
	}{{"2026-01-01", "b-acct", 4}, {"2026-01-02", "a-acct", 2}, {"2026-01-02", "b-acct", 1}}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Date != w.date || got[i].AccountID != w.account || got[i].InputTokens != w.input {
			t.Errorf("row %d = %s/%s/%d, want %s/%s/%d", i, got[i].Date, got[i].AccountID, got[i].InputTokens, w.date, w.account, w.input)
		}
	}
	weekly := Weekly(e, opts).Weekly
	if len(weekly) != 2 || weekly[0].AccountID != "a-acct" || weekly[1].AccountID != "b-acct" || weekly[1].InputTokens != 5 {
		t.Errorf("weekly by account = %+v", weekly)
	}
}

func TestWeekStart(t *testing.T) {
	tests := []struct {
		date  string
		start time.Weekday
		want  string
	}{
		{"2026-01-07", time.Sunday, "2026-01-04"}, // Wednesday
		{"2026-01-04", time.Sunday, "2026-01-04"}, // Sunday itself
		{"2026-01-07", time.Monday, "2026-01-05"},
		{"2026-01-04", time.Monday, "2025-12-29"}, // across a year
		{"2026-03-01", time.Saturday, "2026-02-28"},
		{"bad", time.Monday, "bad"},
	}
	for _, tt := range tests {
		if got := weekStart(tt.date, tt.start); got != tt.want {
			t.Errorf("weekStart(%s, %s) = %s, want %s", tt.date, tt.start, got, tt.want)
		}
	}
}

func TestWeekly_StartOfWeek(t *testing.T) {
	e := []Entry{
		reportEntry(t, "2026-01-03T12:00:00Z", "a", 1, 0, 0.1), // Saturday
		reportEntry(t, "2026-01-04T12:00:00Z", "b", 2, 0, 0.3), // Sunday
		reportEntry(t, "2026-01-05T12:00:00Z", "a", 4, 0, 0.1), // Monday
	}
	sunday := Weekly(e, utcOpts())
	if got := dates(sunday.Weekly, func(r SummaryRow) string { return r.Week }); !slices.Equal(got, []string{"2025-12-28", "2026-01-04"}) {
		t.Fatalf("Sunday weeks = %v", got)
	}
	w := sunday.Weekly[1]
	if w.InputTokens != 6 || !slices.Equal(w.ModelsUsed, []string{"b", "a"}) || w.ModelBreakdowns[0].ModelName != "b" || w.Date != "" {
		t.Errorf("second Sunday week = %+v", w)
	}

	opts := utcOpts()
	opts.StartOfWeek = time.Monday
	monday := Weekly(e, opts)
	if got := dates(monday.Weekly, func(r SummaryRow) string { return r.Week }); !slices.Equal(got, []string{"2025-12-29", "2026-01-05"}) {
		t.Fatalf("Monday weeks = %v", got)
	}
	if monday.Weekly[0].InputTokens != 3 || monday.Totals.InputTokens != 7 {
		t.Errorf("Monday weeks = %+v totals %+v", monday.Weekly, monday.Totals)
	}
}

func TestMonthly_Rollup(t *testing.T) {
	e := []Entry{
		reportEntry(t, "2026-01-31T23:00:00Z", "a", 1, 0, 0.25),
		reportEntry(t, "2026-01-02T00:00:00Z", "a", 2, 0, 0.25),
		reportEntry(t, "2026-02-01T00:00:00Z", "b", 4, 0, 1),
		reportEntry(t, "2025-12-31T12:00:00Z", "a", 8, 0, 1),
	}
	r := Monthly(e, utcOpts())
	if got := dates(r.Monthly, func(r SummaryRow) string { return r.Month }); !slices.Equal(got, []string{"2025-12", "2026-01", "2026-02"}) {
		t.Fatalf("months = %v", got)
	}
	jan := r.Monthly[1]
	if jan.InputTokens != 3 || !approx(jan.TotalCost, 0.5) || len(jan.ModelBreakdowns) != 1 || jan.ModelBreakdowns[0].InputTokens != 3 {
		t.Errorf("January = %+v", jan)
	}

	// Since and until select days, so a month can be partial.
	opts := utcOpts()
	opts.Since = "20260115"
	r = Monthly(e, opts)
	if len(r.Monthly) != 2 || r.Monthly[0].InputTokens != 1 {
		t.Errorf("since-filtered months = %+v", r.Monthly)
	}

	// A late entry in UTC is next month in UTC+2.
	opts = utcOpts()
	opts.Location = time.FixedZone("EET", 2*3600)
	r = Monthly(e, opts)
	if r.Monthly[1].InputTokens != 2 || r.Monthly[2].InputTokens != 5 {
		t.Errorf("EET months = %+v", r.Monthly)
	}
}

func TestSession_Grouping(t *testing.T) {
	mk := func(ts, project, pathSession, lineSession string, input int64, cost float64) Entry {
		e := reportEntry(t, ts, "m", input, 0, cost)
		e.ProjectPath, e.PathSessionID, e.SessionID = project, pathSession, lineSession
		return e
	}
	e := []Entry{
		mk("2026-01-02T10:00:00Z", "proj-a", "s1", "s1", 1, 0.1),
		mk("2026-01-02T12:00:00.123456Z", "proj-a", "s1", "other", 2, 0.1), // path session wins
		mk("2026-01-01T09:00:00Z", "proj-a", "s1", "s1", 4, 0.1),
		mk("2026-01-02T10:00:00Z", "proj-b", "s1", "s1", 8, 1.0), // same id, other project
		mk("2026-01-02T10:00:00Z", "proj-c", "s3", "s3", 0, 5.0), // no tokens: dropped
		mk("2026-01-02T11:00:00Z", "", "", "ledger-sess", 16, 0.5),
	}
	r := Session(e, utcOpts())
	if len(r.Sessions) != 3 {
		t.Fatalf("sessions = %+v", r.Sessions)
	}
	b, ledger, a := r.Sessions[0], r.Sessions[1], r.Sessions[2]
	if b.ProjectPath != "proj-b" || b.InputTokens != 8 {
		t.Errorf("most expensive = %+v", b)
	}
	if ledger.SessionID != "ledger-sess" || ledger.InputTokens != 16 {
		t.Errorf("session without a path = %+v", ledger)
	}
	if a.SessionID != "s1" || a.ProjectPath != "proj-a" || a.InputTokens != 7 {
		t.Errorf("proj-a = %+v", a)
	}
	if a.FirstActivity != "2026-01-01T09:00:00.000Z" || a.LastActivity != "2026-01-02T12:00:00.123Z" {
		t.Errorf("activity = %s .. %s", a.FirstActivity, a.LastActivity)
	}
	if r.Totals.InputTokens != 31 {
		t.Errorf("totals = %+v", r.Totals)
	}

	// The date filter applies to entries before grouping.
	opts := utcOpts()
	opts.Since = "20260102"
	for _, s := range Session(e, opts).Sessions {
		if s.ProjectPath == "proj-a" && (s.InputTokens != 3 || s.FirstActivity != "2026-01-02T10:00:00.000Z") {
			t.Errorf("filtered proj-a = %+v", s)
		}
	}
}

func TestSyntheticExcludedEverywhere(t *testing.T) {
	synth := Entry{Timestamp: mustTime(t, "2026-01-02T01:00:00Z"), Model: syntheticModel, Input: 3, CostUSD: ptr(0), PathSessionID: "s"}
	e := []Entry{synth}
	for name, models := range map[string][]string{
		"daily":   Daily(e, utcOpts()).Daily[0].ModelsUsed,
		"weekly":  Weekly(e, utcOpts()).Weekly[0].ModelsUsed,
		"monthly": Monthly(e, utcOpts()).Monthly[0].ModelsUsed,
		"session": Session(e, utcOpts()).Sessions[0].ModelsUsed,
	} {
		if models == nil || len(models) != 0 {
			t.Errorf("%s models = %#v, want empty non-nil", name, models)
		}
	}
}

func TestParseDateBound(t *testing.T) {
	for in, want := range map[string]string{"": "", "20260102": "20260102", "2026-01-02": "20260102"} {
		if got, err := ParseDateBound(in); err != nil || got != want {
			t.Errorf("ParseDateBound(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"2026-1-2", "20261302", "2026010", "yesterday"} {
		if _, err := ParseDateBound(in); err == nil {
			t.Errorf("ParseDateBound(%q) succeeded", in)
		}
	}
}

func TestBlocksReport(t *testing.T) {
	e := blockEntries(t, "2026-01-02T10:05:00Z", "2026-01-02T11:00:00Z", "2026-01-02T20:00:00Z", "2026-01-02T20:30:00Z")
	reset := mustTime(t, "2026-01-02T15:00:00Z")
	e[1].UsageLimitResetAt = &reset
	now := mustTime(t, "2026-01-02T21:00:00Z")
	blocks := identify(e, now)
	r := Blocks(blocks, now, BlocksOptions{Location: time.UTC, TokenLimit: new("max")})
	if len(r.Blocks) != 3 {
		t.Fatalf("blocks = %+v", r.Blocks)
	}
	done, gap, active := r.Blocks[0], r.Blocks[1], r.Blocks[2]
	if done.ID != "2026-01-02T10:00:00.000Z" || done.StartTime != done.ID || done.EndTime != "2026-01-02T15:00:00.000Z" ||
		done.ActualEndTime == nil || *done.ActualEndTime != "2026-01-02T11:00:00.000Z" || done.Entries != 2 ||
		done.TokenCounts.InputTokens != 2000 || done.TotalTokens != 3000 || !approx(done.CostUSD, 0.02) ||
		!slices.Equal(done.Models, []string{"claude-test"}) {
		t.Errorf("completed block = %+v", done)
	}
	if done.BurnRate != nil || done.Projection != nil || done.TokenLimitStatus != nil {
		t.Errorf("inactive block has burn rate or projection: %+v", done)
	}
	if done.UsageLimitResetTime == nil || *done.UsageLimitResetTime != "2026-01-02T15:00:00.000Z" {
		t.Errorf("usage limit reset = %v", done.UsageLimitResetTime)
	}
	if !gap.IsGap || gap.ActualEndTime != nil || gap.Entries != 0 || gap.Models == nil {
		t.Errorf("gap = %+v", gap)
	}
	if !active.IsActive || active.BurnRate == nil || active.Projection == nil {
		t.Fatalf("active = %+v", active)
	}
	// 3000 tokens over 30 minutes, 240 minutes left: 3000 + 100*240.
	st := active.TokenLimitStatus
	if st == nil || st.Limit != 3000 || st.ProjectedUsage != 27000 || st.Status != LimitExceeds || !approx(st.PercentUsed, 900) {
		t.Errorf("token limit status = %+v", st)
	}

	if got := Blocks(blocks, now, BlocksOptions{Location: time.UTC}).Blocks[2].TokenLimitStatus; got != nil {
		t.Errorf("status without a limit = %+v", got)
	}
	if got := Blocks(blocks, now, BlocksOptions{Location: time.UTC, TokenLimit: new("100000")}).Blocks[2].TokenLimitStatus; got == nil || got.Status != LimitOK {
		t.Errorf("status with a numeric limit = %+v", got)
	}
	if got := Blocks(blocks, now, BlocksOptions{Location: time.UTC, Since: "20260103"}).Blocks; len(got) != 0 {
		t.Errorf("since-filtered blocks = %+v", got)
	}
}

// jsonKeys marshals v with both JSON packages and returns the sorted keys of
// the resulting object, failing if the packages disagree.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	keysOf := func(data []byte) []string {
		var m map[string]any
		if err := jsonv1.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		keys := make([]string, 0, len(m))
		for k, val := range m {
			if val == nil {
				k += "=null"
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	v1, err := jsonv1.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	k1, k2 := keysOf(v1), keysOf(v2)
	if !slices.Equal(k1, k2) {
		t.Errorf("encoding/json keys %v differ from json/v2 keys %v", k1, k2)
	}
	return k1
}

func TestReportJSONFieldNames(t *testing.T) {
	e := []Entry{reportEntry(t, "2026-01-02T01:00:00Z", "m", 1, 2, 0.1)}
	e[0].PathSessionID, e[0].ProjectPath = "s", "p"
	tests := []struct {
		name string
		v    any
		want []string
	}{
		{"daily report", Daily(e, utcOpts()), []string{"daily", "totals"}},
		{"weekly report", Weekly(e, utcOpts()), []string{"totals", "weekly"}},
		{"monthly report", Monthly(e, utcOpts()), []string{"monthly", "totals"}},
		{"session report", Session(e, utcOpts()), []string{"sessions", "totals"}},
		{"daily row", Daily(e, utcOpts()).Daily[0], []string{
			"cacheCreationTokens", "cacheReadTokens", "date", "inputTokens", "modelBreakdowns",
			"modelsUsed", "outputTokens", "totalCost", "totalTokens"}},
		{"weekly row", Weekly(e, utcOpts()).Weekly[0], []string{
			"cacheCreationTokens", "cacheReadTokens", "inputTokens", "modelBreakdowns",
			"modelsUsed", "outputTokens", "totalCost", "totalTokens", "week"}},
		{"monthly row", Monthly(e, utcOpts()).Monthly[0], []string{
			"cacheCreationTokens", "cacheReadTokens", "inputTokens", "modelBreakdowns",
			"modelsUsed", "month", "outputTokens", "totalCost", "totalTokens"}},
		{"session row", Session(e, utcOpts()).Sessions[0], []string{
			"cacheCreationTokens", "cacheReadTokens", "firstActivity", "inputTokens", "lastActivity",
			"modelBreakdowns", "modelsUsed", "outputTokens", "projectPath", "sessionId", "totalCost", "totalTokens"}},
		{"breakdown", Daily(e, utcOpts()).Daily[0].ModelBreakdowns[0], []string{
			"cacheCreationTokens", "cacheReadTokens", "cost", "inputTokens", "modelName", "outputTokens"}},
		{"breakdown missing pricing", ModelBreakdown{MissingPricing: true}, []string{
			"cacheCreationTokens", "cacheReadTokens", "cost", "inputTokens", "missingPricing", "modelName", "outputTokens"}},
		{"totals", Daily(e, utcOpts()).Totals, []string{
			"cacheCreationTokens", "cacheReadTokens", "inputTokens", "outputTokens", "totalCost", "totalTokens"}},
		{"totals unpriced", Totals{UnpricedModels: []string{"x"}}, []string{
			"cacheCreationTokens", "cacheReadTokens", "inputTokens", "outputTokens", "totalCost", "totalTokens", "unpricedModels"}},
		{"account row", SummaryRow{Date: "d", AccountID: "a", ModelsUsed: []string{}, ModelBreakdowns: []ModelBreakdown{}}, []string{
			"accountId", "cacheCreationTokens", "cacheReadTokens", "date", "inputTokens", "modelBreakdowns",
			"modelsUsed", "outputTokens", "totalCost", "totalTokens"}},
	}
	for _, tt := range tests {
		if got := jsonKeys(t, tt.v); !slices.Equal(got, tt.want) {
			t.Errorf("%s keys = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBlocksJSONFieldNames(t *testing.T) {
	now := mustTime(t, "2026-01-02T10:30:00Z")
	e := blockEntries(t, "2026-01-02T03:00:00Z", "2026-01-02T10:00:00Z", "2026-01-02T10:20:00Z")
	r := Blocks(identify(e, now), now, BlocksOptions{Location: time.UTC, TokenLimit: new("max")})
	if got := jsonKeys(t, r); !slices.Equal(got, []string{"blocks"}) {
		t.Errorf("report keys = %v", got)
	}
	common := []string{"costUSD", "endTime", "entries", "id", "isActive", "isGap", "models", "startTime", "tokenCounts", "totalTokens"}
	withNulls := func(extra ...string) []string {
		out := append(slices.Clone(common), extra...)
		sort.Strings(out)
		return out
	}
	tests := []struct {
		name string
		v    any
		want []string
	}{
		{"completed", r.Blocks[0], withNulls("actualEndTime", "burnRate=null", "projection=null")},
		{"gap", r.Blocks[1], withNulls("actualEndTime=null", "burnRate=null", "projection=null")},
		{"active", r.Blocks[2], withNulls("actualEndTime", "burnRate", "projection", "tokenLimitStatus")},
		{"token counts", r.Blocks[0].TokenCounts, []string{"cacheCreationInputTokens", "cacheReadInputTokens", "inputTokens", "outputTokens"}},
		{"burn rate", r.Blocks[2].BurnRate, []string{"costPerHour", "tokensPerMinute", "tokensPerMinuteForIndicator"}},
		{"projection", r.Blocks[2].Projection, []string{"remainingMinutes", "totalCost", "totalTokens"}},
		{"limit status", r.Blocks[2].TokenLimitStatus, []string{"limit", "percentUsed", "projectedUsage", "status"}},
	}
	for _, tt := range tests {
		if got := jsonKeys(t, tt.v); !slices.Equal(got, tt.want) {
			t.Errorf("%s keys = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBlocksReport_TokenLimitValues(t *testing.T) {
	e := blockEntries(t, "2026-01-02T10:05:00Z", "2026-01-02T11:00:00Z", "2026-01-02T20:00:00Z", "2026-01-02T20:30:00Z")
	now := mustTime(t, "2026-01-02T21:00:00Z")
	blocks := identify(e, now)
	for _, tt := range []struct {
		limit *string
		want  int64 // 0: no status
	}{
		{nil, 0},
		{new(""), 3000}, // empty means "max", as in ccusage
		{new("max"), 3000},
		{new("50000"), 50000},
		{new("0"), 0},
		{new("-5"), 0},
		{new("12abc"), 0},
	} {
		st := Blocks(blocks, now, BlocksOptions{Location: time.UTC, TokenLimit: tt.limit}).Blocks[2].TokenLimitStatus
		switch {
		case tt.want == 0 && st != nil:
			t.Errorf("limit %v: status %+v, want none", tt.limit, st)
		case tt.want != 0 && (st == nil || st.Limit != tt.want):
			t.Errorf("limit %v: status %+v, want limit %d", tt.limit, st, tt.want)
		}
	}
}
