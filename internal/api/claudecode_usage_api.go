package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/claudecode/ccusage"
	"antigravity-go-proxy/internal/config"
)

// claudeCodeUnattributed is the account key usage reports give entries
// that belong to no configured account.
const claudeCodeUnattributed = "unattributed"

// claudeCodeRecentBlocks is how far back a "recent" blocks report reaches,
// as ccusage's blocks --recent.
const claudeCodeRecentBlocks = 3 * 24 * time.Hour

// claudeCodeReloadTimeout bounds how long a usage reload request waits for
// the rescan; a rescan that takes longer finishes in the background.
var claudeCodeReloadTimeout = 30 * time.Second

// claudeCodeUsageWindows returns the /v1/usage windows of the Claude Code
// subscription accounts: a 5h and a weekly window for each enabled
// subscription account that has pool data, in account order.
func (server *Server) claudeCodeUsageWindows(now time.Time) []any {
	cc := config.Get().ClaudeCode
	snapshots := server.claudeCodeAccountSnapshots(cc)
	if len(snapshots) == 0 {
		return nil
	}
	modelIDs := claudeCodeModelIDs(cc)
	limits := claudeCodeUsageLimitsByID(cc)
	var windows []any
	for _, acc := range snapshots {
		if !acc.Enabled || acc.Type != "oauth" && acc.Type != "setup_token" {
			continue
		}
		pools, _, _ := server.claudeCodePoolsAndUsage(acc, limits[acc.ID], now)
		name := claudeCodeDisplayName(acc)
		for _, w := range []struct{ pool, label string }{
			{claudeCodePool5h, "Claude 5h"},
			{claudeCodePoolWeekly, "Claude weekly"},
		} {
			p, ok := pools[w.pool]
			if !ok || p.RemainingFraction == nil {
				continue
			}
			remaining := min(1, max(0, *p.RemainingFraction))
			windows = append(windows, map[string]any{
				"label":              w.label + " (" + name + ")",
				"remaining_fraction": remaining,
				"used_percent":       (1 - remaining) * 100,
				"reset_at":           p.ResetTime,
				"model_ids":          modelIDs,
				"provider":           "claudecode",
				"account_id":         acc.ID,
			})
		}
	}
	return windows
}

// claudeCodeModelIDs lists, sorted, the model IDs and aliases the Claude
// Code gateway serves, the Claude Code models /account-limits reports.
func claudeCodeModelIDs(cc claudecode.Config) []string {
	seen := map[string]bool{}
	ids := []string{}
	forEachClaudeCodeModel(cc, func(id string, _ int) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	})
	sort.Strings(ids)
	return ids
}

// claudeCodeUsageQuery is a parsed GET /api/claudecode/usage request.
type claudeCodeUsageQuery struct {
	report      string
	since       string
	until       string
	account     string
	hasAccount  bool
	loc         *time.Location
	recent      bool
	active      bool
	startOfWeek time.Weekday
	tokenLimit  *string
}

// parseClaudeCodeUsageQuery validates the report parameters. defaultLoc is
// used when no timezone is given.
func parseClaudeCodeUsageQuery(request *http.Request, defaultLoc *time.Location) (claudeCodeUsageQuery, error) {
	query := request.URL.Query()
	q := claudeCodeUsageQuery{report: strings.ToLower(query.Get("report")), loc: defaultLoc}
	switch q.report {
	case "":
		q.report = "daily"
	case "daily", "weekly", "monthly", "session", "blocks":
	default:
		return q, fmt.Errorf("unknown report %q: use daily, weekly, monthly, session or blocks", q.report)
	}
	var err error
	if q.since, err = ccusage.ParseDateBound(query.Get("since")); err != nil {
		return q, fmt.Errorf("since: %w", err)
	}
	if q.until, err = ccusage.ParseDateBound(query.Get("until")); err != nil {
		return q, fmt.Errorf("until: %w", err)
	}
	if q.since != "" && q.until != "" && q.since > q.until {
		return q, fmt.Errorf("since %s is after until %s", q.since, q.until)
	}
	if a := query.Get("account"); a != "" {
		q.account, q.hasAccount = a, true
	}
	if tz := query.Get("timezone"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return q, fmt.Errorf("unknown timezone %q", tz)
		}
		q.loc = loc
	}
	if q.recent, err = claudeCodeQueryBool(query, "recent"); err != nil {
		return q, err
	}
	if q.active, err = claudeCodeQueryBool(query, "active"); err != nil {
		return q, err
	}
	if s := query.Get("startOfWeek"); s != "" {
		day, ok := claudeCodeWeekdays[strings.ToLower(s)]
		if !ok {
			return q, fmt.Errorf("unknown startOfWeek %q: use sunday to saturday", s)
		}
		q.startOfWeek = day
	}
	if query.Has("tokenLimit") {
		limit := strings.ToLower(query.Get("tokenLimit"))
		if limit != "" && limit != "max" {
			if n, err := strconv.ParseInt(limit, 10, 64); err != nil || n <= 0 {
				return q, fmt.Errorf("tokenLimit %q is not max or a positive number", limit)
			}
		}
		q.tokenLimit = &limit
	}
	return q, nil
}

var claudeCodeWeekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

// claudeCodeQueryBool reads a flag parameter: absent is false, and a bare
// "?name" is true.
func claudeCodeQueryBool(query map[string][]string, name string) (bool, error) {
	values, ok := query[name]
	if !ok {
		return false, nil
	}
	if len(values) == 0 || values[0] == "" {
		return true, nil
	}
	v, err := strconv.ParseBool(values[0])
	if err != nil {
		return false, fmt.Errorf("%s %q is not a boolean", name, values[0])
	}
	return v, nil
}

// handleClaudeCodeUsageReport serves GET /api/claudecode/usage: a ccusage
// report split by account, over the engine's in-memory window plus, when
// since reaches before it, the ledger's older days. Claude Code's own
// transcripts count only inside the window; partialLocal flags a report
// that reaches before it while they are scanned. Unattributed usage is
// under the account key "unattributed". Without a usage engine it answers
// {"enabled": false}.
func (server *Server) handleClaudeCodeUsageReport(writer http.ResponseWriter, request *http.Request) {
	en := server.ccUsage
	dur, mode, pricer, loc := en.ReportSettings()
	q, err := parseClaudeCodeUsageQuery(request, loc)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	if en == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"enabled": false})
		return
	}

	entries, windowStart, partialLocal := en.ReportEntries(q.since, q.loc)
	kept := entries[:0]
	for _, e := range entries {
		if e.AccountID == "" {
			e.AccountID = claudeCodeUnattributed
		}
		if q.hasAccount && e.AccountID != q.account {
			continue
		}
		kept = append(kept, e)
	}
	entries = kept

	out := map[string]any{
		"enabled":  true,
		"report":   q.report,
		"timezone": q.loc.String(),
		"since":    q.since,
		"until":    q.until,
		// Reports reach back into the ledger's history; windowStart is
		// where the in-memory window, the only part with local
		// transcripts, begins.
		"windowStart":  windowStart.UTC().Format(time.RFC3339),
		"partialLocal": partialLocal,
	}
	opts := ccusage.ReportOptions{
		Location:    q.loc,
		Mode:        mode,
		Pricer:      pricer,
		Since:       q.since,
		Until:       q.until,
		ByAccount:   true,
		StartOfWeek: q.startOfWeek,
	}
	switch q.report {
	case "daily":
		r := ccusage.Daily(entries, opts)
		out["daily"], out["totals"] = nonNilRows(r.Daily), r.Totals
	case "weekly":
		r := ccusage.Weekly(entries, opts)
		out["weekly"], out["totals"] = nonNilRows(r.Weekly), r.Totals
	case "monthly":
		r := ccusage.Monthly(entries, opts)
		out["monthly"], out["totals"] = nonNilRows(r.Monthly), r.Totals
	case "session":
		r := ccusage.Session(entries, opts)
		sessions := r.Sessions
		if sessions == nil {
			sessions = []ccusage.SessionRow{}
		}
		out["sessions"], out["totals"] = sessions, r.Totals
	case "blocks":
		out["blocks"] = server.claudeCodeBlockRows(entries, q, dur, mode, pricer)
	}
	writeJSON(writer, http.StatusOK, out)
}

func nonNilRows(rows []ccusage.SummaryRow) []ccusage.SummaryRow {
	if rows == nil {
		return []ccusage.SummaryRow{}
	}
	return rows
}

// claudeCodeBlockRow is a ccusage blocks row with the account it belongs
// to.
type claudeCodeBlockRow struct {
	AccountID string `json:"accountId"`
	ccusage.BlockRow
}

// claudeCodeBlockRows builds the blocks report. Blocks are per account:
// each account's entries are grouped with its known 5h window starts, and
// the rows of all accounts are listed together by start time.
func (server *Server) claudeCodeBlockRows(entries []ccusage.Entry, q claudeCodeUsageQuery, dur time.Duration, mode ccusage.CostMode, pricer ccusage.Pricer) []claudeCodeBlockRow {
	now := server.now()
	byAccount := map[string][]ccusage.Entry{}
	for _, e := range entries {
		byAccount[e.AccountID] = append(byAccount[e.AccountID], e)
	}
	live := map[string][]time.Time{}
	for _, acc := range server.claudeCodeAccountSnapshots(config.Get().ClaudeCode) {
		live[acc.ID] = claudeCodeUsageAnchors(acc.RateLimits.Unified)
	}

	rows := []claudeCodeBlockRow{}
	for id, accEntries := range byAccount {
		engineID := id
		if id == claudeCodeUnattributed {
			engineID = ""
		}
		anchors := append(server.ccUsage.Summary(engineID).Anchors5h, live[engineID]...)
		blocks := ccusage.IdentifyBlocks(accEntries, dur, now, anchors, mode, pricer)

		// As ccusage, a "max" token limit comes from every block, before
		// the recent and active filters.
		tokenLimit := q.tokenLimit
		if tokenLimit != nil && (*tokenLimit == "" || *tokenLimit == "max") {
			limit := strconv.FormatInt(ccusage.MaxTokensFromHistory(blocks), 10)
			tokenLimit = &limit
		}
		kept := blocks[:0]
		for _, b := range blocks {
			if q.active && !b.IsActive {
				continue
			}
			if q.recent && !b.IsActive && b.Start.Before(now.Add(-claudeCodeRecentBlocks)) {
				continue
			}
			kept = append(kept, b)
		}
		report := ccusage.Blocks(kept, now, ccusage.BlocksOptions{
			Location:   q.loc,
			Since:      q.since,
			Until:      q.until,
			TokenLimit: tokenLimit,
		})
		for _, row := range report.Blocks {
			rows = append(rows, claudeCodeBlockRow{AccountID: id, BlockRow: row})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].StartTime != rows[j].StartTime {
			return rows[i].StartTime < rows[j].StartTime
		}
		return rows[i].AccountID < rows[j].AccountID
	})
	return rows
}

// handleClaudeCodeUsageReload serves POST /api/claudecode/usage/reload: it
// rescans the ledger and local transcripts now and returns the engine's
// counters. Concurrent reloads share one rescan. A rescan that outlasts
// claudeCodeReloadTimeout keeps running in the background and the request
// answers 504.
func (server *Server) handleClaudeCodeUsageReload(writer http.ResponseWriter, request *http.Request) {
	en := server.ccUsage
	if en == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "enabled": false})
		return
	}
	done := en.Reload()
	timer := time.NewTimer(claudeCodeReloadTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		writeJSON(writer, http.StatusGatewayTimeout, map[string]any{"status": "error", "error": "usage reload timed out; it continues in the background"})
		return
	case <-request.Context().Done():
		return
	}
	stats := en.Stats()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"enabled": true,
		"stats": map[string]any{
			"entries": stats.Entries,
			"files":   stats.Files,
			"ledger": map[string]any{
				"written": stats.Ledger.Written,
				"dropped": stats.Ledger.Dropped,
				"errors":  stats.Ledger.Errors,
			},
		},
	})
}
