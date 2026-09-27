//go:build ccusage_parity

// Parity test against a pinned upstream ccusage. It is behind a build tag
// because it needs npx and, on first run, network access to fetch the pinned
// package. Run it with:
//
//	go test -tags ccusage_parity -run Parity -v ./internal/claudecode/ccusage/
//
// For each report it runs, from the fixture's point of view:
//
//	TZ=UTC CLAUDE_CONFIG_DIR=testdata/parity \
//	    npx --yes ccusage@20.0.26 claude <daily|weekly|monthly|session|blocks> --json --offline
//
// and diffs the JSON against the Go report built from the same files: read
// every usage file, dedupe, then report in UTC with cost mode auto and the
// embedded LiteLLM snapshot (ccusage's --offline pricing).
//
// The "claude" subcommand is used because the top-level commands of this
// ccusage version merge every detected agent and emit a different row shape
// ("period", "agent", "metadata"); the Claude subcommand emits the shape the
// Go port mirrors.
//
// The fixture (testdata/parity) is dated in 2025 so no block is active and
// the output does not depend on the clock. It covers several days, weeks and
// months, a gap of more than five hours, cache writes with and without a
// 5m/1h breakdown, a request above the 200k tier, an exact duplicate across
// files, a partial streaming copy superseded by the full copy, a sidechain
// replay, logged costUSD values, and fast and standard speed.
package ccusage

import (
	"bytes"
	jsonv1 "encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const parityVersion = "ccusage@20.0.26"

// parityCostRelTol is the relative tolerance for costs. Both sides sum
// float64 products, but not necessarily in the same order, so the last few
// bits can differ.
const parityCostRelTol = 1e-9

func TestParity(t *testing.T) {
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Skip("npx not found")
	}
	dir, err := filepath.Abs(filepath.Join("testdata", "parity"))
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	var entries []Entry
	for _, f := range UsageFiles(ClaudePaths()) {
		es, err := ReadUsageFile(f)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, es...)
	}
	entries = Dedupe(entries)
	if len(entries) == 0 {
		t.Fatal("fixture produced no entries")
	}

	opts := ReportOptions{Location: time.UTC, Mode: CostModeAuto, Pricer: NewLiteLLMPricer()}
	now := time.Now()
	blocks := IdentifyBlocks(entries, 5*time.Hour, now, nil, opts.Mode, opts.Pricer)

	reports := []struct {
		cmd string
		got any
	}{
		{"daily", Daily(entries, opts)},
		{"weekly", Weekly(entries, opts)},
		{"monthly", Monthly(entries, opts)},
		{"session", Session(entries, opts)},
		{"blocks", Blocks(blocks, now, BlocksOptions{Location: time.UTC})},
	}
	for _, r := range reports {
		t.Run(r.cmd, func(t *testing.T) {
			want := runCCUsage(t, npx, dir, r.cmd)
			got := roundTrip(t, r.got)
			var diffs []string
			parityDiff(r.cmd, want, got, &diffs)
			for _, d := range diffs {
				t.Error(d)
			}
			if len(diffs) == 0 {
				t.Logf("%s: identical to %s", r.cmd, parityVersion)
			}
		})
	}
}

func runCCUsage(t *testing.T, npx, dir, cmd string) any {
	t.Helper()
	c := exec.Command(npx, "--yes", parityVersion, "claude", cmd, "--json", "--offline")
	c.Env = append(os.Environ(), "TZ=UTC", "CLAUDE_CONFIG_DIR="+dir, "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		t.Fatalf("%s claude %s: %v\n%s", parityVersion, cmd, err, stderr.String())
	}
	var v any
	dec := jsonv1.NewDecoder(&stdout)
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s claude %s: decode: %v\n%s", parityVersion, cmd, err, stdout.String())
	}
	return v
}

func roundTrip(t *testing.T, report any) any {
	t.Helper()
	data, err := jsonv1.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	dec := jsonv1.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// parityDiff appends a line for every difference between want (ccusage) and
// got (Go) under path. Integers must match exactly; other numbers within
// parityCostRelTol.
func parityDiff(path string, want, got any, diffs *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s: ccusage %v, Go %v", path, want, got))
			return
		}
		keys := make([]string, 0, len(w)+len(g))
		for k := range w {
			keys = append(keys, k)
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !gok:
				*diffs = append(*diffs, fmt.Sprintf("%s.%s: only in ccusage (%v)", path, k, wv))
			case !wok:
				*diffs = append(*diffs, fmt.Sprintf("%s.%s: only in Go (%v)", path, k, gv))
			default:
				parityDiff(path+"."+k, wv, gv, diffs)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			*diffs = append(*diffs, fmt.Sprintf("%s: ccusage %v, Go %v", path, want, got))
			return
		}
		for i := range w {
			parityDiff(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], diffs)
		}
	case jsonv1.Number:
		g, ok := got.(jsonv1.Number)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s: ccusage %v, Go %v", path, want, got))
			return
		}
		if !numbersMatch(w, g) {
			*diffs = append(*diffs, fmt.Sprintf("%s: ccusage %s, Go %s", path, w, g))
		}
	default:
		if want != got {
			*diffs = append(*diffs, fmt.Sprintf("%s: ccusage %v, Go %v", path, want, got))
		}
	}
}

func numbersMatch(w, g jsonv1.Number) bool {
	if w == g {
		return true
	}
	isInt := func(n jsonv1.Number) bool { return !strings.ContainsAny(string(n), ".eE") }
	if isInt(w) && isInt(g) {
		return false
	}
	wf, err1 := w.Float64()
	gf, err2 := g.Float64()
	if err1 != nil || err2 != nil {
		return false
	}
	return math.Abs(wf-gf) <= parityCostRelTol*math.Max(math.Abs(wf), math.Abs(gf))
}
