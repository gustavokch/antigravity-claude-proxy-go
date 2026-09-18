package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Probe axes. Each probe holds every dimension fixed but one, so a success on
// exactly one axis names the dimension the throttle is keyed on. See
// docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md
// section 2 for what each answer implies for the proxy.
const (
	AxisBaseline = "baseline"
	AxisModel    = "model"
	AxisAccount  = "account"
	AxisProject  = "project"
	AxisEndpoint = "endpoint"
)

// ProbeResult is one upstream call made by the matrix. Status 0 means the
// call failed before a response (transport error); Error carries the detail.
type ProbeResult struct {
	Axis      string    `json:"axis"`
	Account   string    `json:"account"`
	Project   string    `json:"project"`
	Model     string    `json:"model"`
	Endpoint  string    `json:"endpoint"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
	LatencyMS int64     `json:"latencyMs"`
	At        time.Time `json:"at"`
}

func (result ProbeResult) Throttled() bool { return result.Status == 429 }
func (result ProbeResult) Succeeded() bool { return result.Status == 200 }

// Conclude names the throttle dimension from one matrix run. The axes are
// only meaningful while the baseline is rejected: the window is short, so a
// success on a later axis is evidence only if the run is fast enough that the
// wave cannot plausibly have ended in between — hence the multi-axis guard.
func Conclude(results []ProbeResult) string {
	byAxis := make(map[string]ProbeResult, len(results))
	for _, result := range results {
		byAxis[result.Axis] = result
	}
	baseline, ok := byAxis[AxisBaseline]
	if !ok {
		return "inconclusive: no baseline probe in this run"
	}
	if !baseline.Throttled() {
		return fmt.Sprintf(
			"no throttle active: baseline returned %d; re-run during a 429 wave or with -burst",
			baseline.Status)
	}

	var freed []string
	for _, axis := range []string{AxisModel, AxisAccount, AxisProject, AxisEndpoint} {
		if result, ok := byAxis[axis]; ok && result.Succeeded() {
			freed = append(freed, axis)
		}
	}
	sort.Strings(freed)

	switch len(freed) {
	case 0:
		return "throttle is shared across every probed axis (model, account, project, endpoint): " +
			"project-wide or model capacity. Re-run with -window to measure how long it holds"
	case 1:
		return fmt.Sprintf("throttle is %s-scoped: the %s axis succeeded while the baseline was rejected",
			freed[0], freed[0])
	default:
		return fmt.Sprintf("inconclusive: %s all succeeded — the wave probably ended mid-run; re-run",
			strings.Join(freed, ", "))
	}
}
