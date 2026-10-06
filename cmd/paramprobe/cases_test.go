package main

import (
	"encoding/json"
	"strings"
	"testing"
)

var testModels = Models{Claude: "claude-opus-4-6-thinking", Tier: "gemini-3.8-flash-high"}

func TestBuildCasesAreWellFormed(t *testing.T) {
	t.Parallel()
	gates := map[string]bool{
		GateCasing: true, GateAdaptive: true, GateSampling: true, GateMaxBudget: true,
		GateOutputCap: true, GateMinimal: true, GateBoth: true, GateEffort: true,
	}
	seen := map[string]bool{}
	covered := map[string]bool{}
	for _, c := range buildCases(testModels) {
		if seen[c.ID] {
			t.Errorf("duplicate case ID %q", c.ID)
		}
		seen[c.ID] = true
		covered[c.Gate] = true
		if !gates[c.Gate] {
			t.Errorf("%s: unknown gate %q", c.ID, c.Gate)
		}
		if !strings.HasPrefix(c.ID, c.Gate+"-") {
			t.Errorf("%s: ID must start with its gate %q", c.ID, c.Gate)
		}
		if c.Model == "" || c.Prompt == "" || c.Question == "" || len(c.Config) == 0 {
			t.Errorf("%s: incomplete case %+v", c.ID, c)
		}
		if _, err := json.Marshal(c.Config); err != nil {
			t.Errorf("%s: config is not JSON-encodable: %v", c.ID, err)
		}
		if c.Heavy != (c.Gate == GateEffort) {
			t.Errorf("%s: only the effort sweep may be heavy (heavy=%v)", c.ID, c.Heavy)
		}
	}
	for gate := range gates {
		if !covered[gate] {
			t.Errorf("gate %s has no probe", gate)
		}
	}
}

func TestCasesTargetTheRightModelFamily(t *testing.T) {
	t.Parallel()
	for _, c := range buildCases(testModels) {
		_, snakeKeys := c.Config["thinkingConfig"].(map[string]any)["include_thoughts"]
		wantClaude := c.Model == testModels.Claude
		if c.Gate == GateOutputCap || c.Gate == GateMinimal || c.Gate == GateBoth {
			if wantClaude {
				t.Errorf("%s: Gemini gate probes must not run against the Claude route", c.ID)
			}
		}
		if snakeKeys && !wantClaude {
			t.Errorf("%s: snake_case thinkingConfig belongs to the Claude route", c.ID)
		}
	}
}

func TestOutcomeOnlyCountsAnUpstream400AsRejection(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]string{200: "accepted", 400: "rejected", 429: "inconclusive", 500: "inconclusive", 0: "inconclusive"} {
		if got := (Result{Status: status}).Outcome(); got != want {
			t.Errorf("status %d: outcome = %q, want %q", status, got, want)
		}
	}
}

func TestSummaryGroupsByGateInFirstSeenOrder(t *testing.T) {
	t.Parallel()
	got := Summary([]Result{
		{ID: "GA-a", Gate: "GA", Status: 200},
		{ID: "GB-a", Gate: "GB", Status: 400},
		{ID: "GA-b", Gate: "GA", Status: 429},
	})
	want := []string{"GA: GA-a=accepted, GA-b=inconclusive", "GB: GB-a=rejected"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
}

// The probe is only meaningful if the request differs from production in
// exactly one place: the generationConfig under test.
func TestBuildPayloadKeepsTheProductionEnvelopeAndSwapsOnlyTheConfig(t *testing.T) {
	t.Parallel()
	c := buildCases(testModels)[1] // GA-claude-camel
	payload := buildPayload(c, "proj-1", "user@example.com")
	if payload["project"] != "proj-1" || payload["model"] != c.Model ||
		payload["userAgent"] != "antigravity" || payload["requestType"] != "agent" {
		t.Fatalf("envelope = %#v", payload)
	}
	inner, _ := payload["request"].(map[string]any)
	generation, _ := inner["generationConfig"].(map[string]any)
	encoded, _ := json.Marshal(generation)
	want, _ := json.Marshal(c.Config)
	if string(encoded) != string(want) {
		t.Fatalf("generationConfig = %s, want exactly the case config %s", encoded, want)
	}
	if inner["systemInstruction"] == nil || inner["contents"] == nil {
		t.Fatalf("inner request lost the production system instruction or contents: %#v", inner)
	}
}

func TestClaudeCasesCarryTheInterleavedThinkingBeta(t *testing.T) {
	t.Parallel()
	for _, c := range buildCases(testModels) {
		got := requestOptions(c).Headers.Get("anthropic-beta")
		if c.Model == testModels.Claude && got != "interleaved-thinking-2025-05-14" {
			t.Errorf("%s: anthropic-beta = %q", c.ID, got)
		}
		if c.Model != testModels.Claude && got != "" {
			t.Errorf("%s: a non-Claude probe must not send anthropic-beta, got %q", c.ID, got)
		}
	}
}
