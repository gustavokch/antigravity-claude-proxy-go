package main

import (
	"strings"
	"testing"
)

func TestConcludeRequiresABaseline(t *testing.T) {
	got := Conclude([]ProbeResult{{Axis: AxisModel, Status: 200}})
	if !strings.Contains(got, "no baseline") {
		t.Fatalf("Conclude without a baseline = %q, want a 'no baseline' verdict", got)
	}
}

func TestConcludeReportsAnIdleUpstream(t *testing.T) {
	got := Conclude([]ProbeResult{{Axis: AxisBaseline, Status: 200}})
	if !strings.Contains(got, "no throttle active") {
		t.Fatalf("Conclude with a succeeding baseline = %q, want 'no throttle active'", got)
	}
}

// One axis free and the rest rejected is the whole point of the matrix: it
// names the dimension the throttle is keyed on.
func TestConcludeNamesTheFreeAxis(t *testing.T) {
	got := Conclude([]ProbeResult{
		{Axis: AxisBaseline, Status: 429},
		{Axis: AxisModel, Status: 200},
		{Axis: AxisAccount, Status: 429},
		{Axis: AxisProject, Status: 429},
		{Axis: AxisEndpoint, Status: 429},
	})
	if !strings.Contains(got, "model-scoped") {
		t.Fatalf("Conclude = %q, want a model-scoped verdict", got)
	}
}

// Every axis rejected is the outcome the 2026-09-17 evidence predicts:
// project-wide or model capacity, which the axes cannot separate.
func TestConcludeReportsASharedThrottle(t *testing.T) {
	got := Conclude([]ProbeResult{
		{Axis: AxisBaseline, Status: 429},
		{Axis: AxisModel, Status: 429},
		{Axis: AxisAccount, Status: 429},
		{Axis: AxisProject, Status: 429},
		{Axis: AxisEndpoint, Status: 429},
	})
	if !strings.Contains(got, "shared across every probed axis") {
		t.Fatalf("Conclude = %q, want a shared-throttle verdict", got)
	}
}

// Two or more axes freed means the wave ended mid-run, not that two
// dimensions exist. Saying so prevents a false conclusion.
func TestConcludeRejectsMultipleFreeAxes(t *testing.T) {
	got := Conclude([]ProbeResult{
		{Axis: AxisBaseline, Status: 429},
		{Axis: AxisModel, Status: 200},
		{Axis: AxisAccount, Status: 200},
	})
	if !strings.Contains(got, "inconclusive") {
		t.Fatalf("Conclude = %q, want an inconclusive verdict", got)
	}
}
