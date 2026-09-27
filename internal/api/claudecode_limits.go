package api

import (
	"math"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/claudecode/ccusage"
)

// Where a Claude Code pool value comes from, in order of precedence.
const (
	claudeCodeSourceHeaders    = "headers"
	claudeCodeSourceCalibrated = "calibrated"
	claudeCodeSourceConfig     = "config"
	claudeCodeSourceMax        = "max"
)

// claudeCodeCalibrationMinUtilization is the lowest header utilization a
// calibration is taken from. The headers carry two decimals, too coarse
// below this to imply a limit.
const claudeCodeCalibrationMinUtilization = 0.20

// claudeCodeCostBasis labels usage costs: API list prices, not what a
// subscription is billed.
const claudeCodeCostBasis = "api-equivalent"

// Utilization thresholds for a window's status, as ccusage's LimitStatus.
const (
	claudeCodeWarningUtilization = 0.8
	claudeCodeExceedsUtilization = 1.0
)

const sevenDays = 7 * 24 * time.Hour

// claudeCodePool is a quota pool of a Claude Code account: the Google pool
// fields unchanged, plus where the value came from.
type claudeCodePool struct {
	accounts.ModelQuota
	Source string `json:"source"`
}

// claudeCodeUsage is the "usage" object of a Claude Code /account-limits
// row.
type claudeCodeUsage struct {
	Window5h     claudeCodeUsageWindow `json:"window5h"`
	Window7d     claudeCodeUsageWindow `json:"window7d"`
	BurnRate     *ccusage.BurnRate     `json:"burnRate"`
	TodayCostUSD float64               `json:"todayCostUSD"`
	CostBasis    string                `json:"costBasis"`
}

// claudeCodeUsageWindow is one window of a claudeCodeUsage. Utilization is
// the present use of the window's limit; ProjectedUtilization extrapolates
// the active block's burn rate and is never used for remainingFraction.
// Fields that are unknown are null.
type claudeCodeUsageWindow struct {
	Start                *string  `json:"start"`
	End                  *string  `json:"end"`
	CostUSD              float64  `json:"costUSD"`
	Tokens               int64    `json:"tokens"`
	Utilization          *float64 `json:"utilization"`
	ProjectedUtilization *float64 `json:"projectedUtilization"`
	Status               *string  `json:"status"`
	Source               *string  `json:"source"`
}

// claudeCodeWindowSpec names one subscription window.
type claudeCodeWindowSpec struct {
	pool string
	dur  time.Duration
}

var (
	claudeCodeWindow5h = claudeCodeWindowSpec{pool: claudeCodePool5h, dur: 5 * time.Hour}
	claudeCodeWindow7d = claudeCodeWindowSpec{pool: claudeCodePoolWeekly, dur: sevenDays}
)

// claudeCodeResolvedWindow is one window of one account as /account-limits
// reports it.
type claudeCodeResolvedWindow struct {
	spec  claudeCodeWindowSpec
	usage ccusage.WindowUsage
	// hasBounds is false when no window is known at all: no anchor and no
	// active block. hasReset is false for a rolling window.
	hasBounds bool
	hasReset  bool
	source    string
	util      *float64
	projected *float64
	// byTokens marks a utilization measured in tokens rather than cost.
	byTokens bool
}

// claudeCodeLimitsAndUsage works out an account's quota pools and usage
// object. Pools go only to subscription accounts, and to API-key accounts
// with configured limits. Without a usage engine this is exactly the
// header pools and no usage. Calibrations from fresh headers are saved to
// the engine summary.
func (server *Server) claudeCodeLimitsAndUsage(acc claudecode.AccountSnapshot, limits *claudecode.UsageLimits, now time.Time) (map[string]claudeCodePool, any, *claudeCodeUsage) {
	return server.claudeCodePoolsAndUsage(acc, limits, now, true)
}

// claudeCodePoolsAndUsage is claudeCodeLimitsAndUsage; saveCalibration
// false leaves the engine summary's calibration alone, for read paths that
// only report the pools.
func (server *Server) claudeCodePoolsAndUsage(acc claudecode.AccountSnapshot, limits *claudecode.UsageLimits, now time.Time, saveCalibration bool) (map[string]claudeCodePool, any, *claudeCodeUsage) {
	subscription := acc.Type == "oauth" || acc.Type == "setup_token"
	u := acc.RateLimits.Unified
	pools := map[string]claudeCodePool{}
	var lastChecked any
	if subscription {
		pools, lastChecked = claudeCodeQuotaPools(u, now)
	}
	en := server.ccUsage
	if en == nil {
		return pools, lastChecked, nil
	}
	snap := en.Snapshot(acc.ID, now, claudeCodeUsageAnchors(u))
	if snap == nil {
		return pools, lastChecked, nil
	}
	sum := snap.Summary

	var fresh5h, fresh7d claudecode.UnifiedWindow
	if subscription && u != nil {
		fresh5h, fresh7d = u.FiveHour, u.SevenDay
	}
	var cal5h, cal7d float64
	w5 := claudeCodeResolveWindow(snap, claudeCodeWindow5h, fresh5h, now, &cal5h)
	w7 := claudeCodeResolveWindow(snap, claudeCodeWindow7d, fresh7d, now, &cal7d)

	var reset7d time.Time
	if u != nil && u.SevenDay.Reset.After(sum.Reset7d) {
		reset7d = u.SevenDay.Reset
	}
	if saveCalibration && (cal5h > 0 && !claudeCodeSameFloat(cal5h, sum.CalibratedCostUSD5h) ||
		cal7d > 0 && !claudeCodeSameFloat(cal7d, sum.CalibratedCostUSD7d) || !reset7d.IsZero()) {
		en.UpdateSummary(acc.ID, func(s *ccusage.Summary) {
			if cal5h > 0 {
				s.CalibratedCostUSD5h, s.CalibratedAt = cal5h, now
			}
			if cal7d > 0 {
				s.CalibratedCostUSD7d, s.CalibratedAt = cal7d, now
			}
			if reset7d.After(s.Reset7d) {
				s.Reset7d = reset7d
			}
		})
	}

	// Stale or missing headers fall back to calibration (subscriptions
	// only), then configured limits, then the largest completed block.
	for _, w := range []*claudeCodeResolvedWindow{w5, w7} {
		if w.source != "" {
			continue
		}
		is5h := w.spec == claudeCodeWindow5h
		calibrated := sum.CalibratedCostUSD7d
		if is5h {
			calibrated = sum.CalibratedCostUSD5h
		}
		switch {
		case subscription && calibrated > 0:
			w.set(claudeCodeSourceCalibrated, w.usage.CostUSD/calibrated, false)
		case limits != nil && claudeCodeConfigLimit(limits, is5h) != nil:
			l := claudeCodeConfigLimit(limits, is5h)
			if l.cost > 0 {
				w.set(claudeCodeSourceConfig, w.usage.CostUSD/l.cost, false)
			} else {
				w.set(claudeCodeSourceConfig, float64(w.usage.Tokens)/float64(l.tokens), true)
			}
		case subscription && is5h && sum.MaxBlockCostUSD > 0:
			w.set(claudeCodeSourceMax, w.usage.CostUSD/sum.MaxBlockCostUSD, false)
		case subscription && is5h && sum.MaxBlockTokens > 0:
			w.set(claudeCodeSourceMax, float64(w.usage.Tokens)/float64(sum.MaxBlockTokens), true)
		}
	}

	for _, w := range []*claudeCodeResolvedWindow{w5, w7} {
		w.project(snap, now)
		if w.source == "" || w.source == claudeCodeSourceHeaders {
			continue
		}
		frac := math.Min(math.Max(1-*w.util, 0), 1)
		p := claudeCodePool{ModelQuota: accounts.ModelQuota{RemainingFraction: &frac}, Source: w.source}
		if w.hasReset {
			p.ResetTime = w.usage.End.UTC().Format(time.RFC3339)
		}
		pools[w.spec.pool] = p
	}

	return pools, lastChecked, &claudeCodeUsage{
		Window5h:     w5.json(),
		Window7d:     w7.json(),
		BurnRate:     snap.BurnRate,
		TodayCostUSD: snap.Today.CostUSD,
		CostBasis:    claudeCodeCostBasis,
	}
}

// claudeCodeResolveWindow finds a window's bounds and usage. A fresh
// header window is used as it is, and a calibration is taken from it
// when its utilization is high enough; *calibration is set to the implied
// limit then. Otherwise the last known anchor is stepped forward in whole
// periods to contain now; without one, 5h is the active ccusage block and
// 7d the rolling seven days.
func claudeCodeResolveWindow(snap *ccusage.Snapshot, spec claudeCodeWindowSpec, fresh claudecode.UnifiedWindow, now time.Time, calibration *float64) *claudeCodeResolvedWindow {
	w := &claudeCodeResolvedWindow{spec: spec}
	if fresh.Utilization != nil && fresh.Reset.After(now) {
		w.usage = snap.Usage(fresh.Reset.Add(-spec.dur), fresh.Reset)
		w.hasBounds, w.hasReset = true, true
		util := *fresh.Utilization
		w.source, w.util = claudeCodeSourceHeaders, &util
		if util >= claudeCodeCalibrationMinUtilization && w.usage.CostUSD > 0 {
			*calibration = w.usage.CostUSD / util
		}
		return w
	}

	var anchor time.Time
	if spec == claudeCodeWindow5h {
		for _, a := range snap.Summary.Anchors5h {
			if a.After(anchor) {
				anchor = a
			}
		}
	} else if !snap.Summary.Reset7d.IsZero() {
		anchor = snap.Summary.Reset7d.Add(-spec.dur)
	}
	if !fresh.Reset.IsZero() && fresh.Reset.Add(-spec.dur).After(anchor) {
		anchor = fresh.Reset.Add(-spec.dur)
	}
	switch {
	case !anchor.IsZero():
		start := claudeCodeStepWindow(anchor, spec.dur, now)
		w.usage = snap.Usage(start, start.Add(spec.dur))
		w.hasBounds, w.hasReset = true, true
	case spec == claudeCodeWindow5h && snap.Active != nil:
		w.usage = snap.Window5h
		w.hasBounds, w.hasReset = true, true
	case spec == claudeCodeWindow7d:
		w.usage = snap.Window7d
		w.hasBounds = true
	}
	return w
}

// claudeCodeStepWindow moves a window start by whole periods so the window
// [start, start+dur) contains now.
func claudeCodeStepWindow(anchor time.Time, dur time.Duration, now time.Time) time.Time {
	// The division truncates towards zero, so an anchor after now can land
	// one period late.
	start := anchor.Add(now.Sub(anchor) / dur * dur)
	if start.After(now) {
		start = start.Add(-dur)
	}
	return start
}

// claudeCodeLimit is one configured window limit; cost wins over tokens.
type claudeCodeLimit struct {
	cost   float64
	tokens int64
}

// claudeCodeConfigLimit returns the configured limit of a window, or nil.
func claudeCodeConfigLimit(l *claudecode.UsageLimits, is5h bool) *claudeCodeLimit {
	c, t := l.CostUSD7d, l.Tokens7d
	if is5h {
		c, t = l.CostUSD5h, l.Tokens5h
	}
	switch {
	case c > 0:
		return &claudeCodeLimit{cost: c}
	case t > 0:
		return &claudeCodeLimit{tokens: t}
	}
	return nil
}

func (w *claudeCodeResolvedWindow) set(source string, util float64, byTokens bool) {
	if math.IsNaN(util) || math.IsInf(util, 0) {
		return
	}
	w.source, w.util, w.byTokens = source, &util, byTokens
}

// project extrapolates the active block's burn rate to the end of the
// window, or of the block when that comes first:
// utilization × projected/current. It never goes below the utilization.
func (w *claudeCodeResolvedWindow) project(snap *ccusage.Snapshot, now time.Time) {
	if w.util == nil {
		return
	}
	projected := *w.util
	current := w.usage.CostUSD
	if w.byTokens {
		current = float64(w.usage.Tokens)
	}
	if b := snap.BurnRate; b != nil && snap.Active != nil && current > 0 {
		end := snap.Active.End
		if w.hasReset && w.usage.End.Before(end) {
			end = w.usage.End
		}
		if left := end.Sub(now); left > 0 {
			extra := b.CostPerHour * left.Hours()
			if w.byTokens {
				extra = b.TokensPerMinute * left.Minutes()
			}
			if p := *w.util * (current + extra) / current; p > projected {
				projected = p
			}
		}
	}
	w.projected = &projected
}

func (w *claudeCodeResolvedWindow) json() claudeCodeUsageWindow {
	out := claudeCodeUsageWindow{
		CostUSD:              w.usage.CostUSD,
		Tokens:               w.usage.Tokens,
		Utilization:          w.util,
		ProjectedUtilization: w.projected,
	}
	if w.hasBounds {
		start := w.usage.Start.UTC().Format(time.RFC3339)
		end := w.usage.End.UTC().Format(time.RFC3339)
		out.Start, out.End = &start, &end
	}
	if w.source != "" {
		source := w.source
		out.Source = &source
	}
	if w.projected != nil {
		status := ccusage.LimitOK
		switch {
		case *w.projected > claudeCodeExceedsUtilization:
			status = ccusage.LimitExceeds
		case *w.projected > claudeCodeWarningUtilization:
			status = ccusage.LimitWarning
		}
		out.Status = &status
	}
	return out
}

// claudeCodeSameFloat reports whether two calibrations are the same value
// up to rounding, so an unchanged one is not saved again.
func claudeCodeSameFloat(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}
