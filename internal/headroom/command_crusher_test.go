package headroom

import (
	"context"
	"strings"
	"testing"
)

func crusherConfig() Config {
	return Config{Enabled: true, CommandCrusher: true, PreserveVerbatimReads: true}
}

func TestCommandCrusher_IsErrorUntouched(t *testing.T) {
	payload := "collected 1 items\n\ntest_a.py F [100%]\n\n=== 1 failed in 0.01s ==="
	req := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "is_error": true, "content": payload},
	}}}}

	reqCtx := &RequestContext{Request: req}
	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &Config{Enabled: true, CommandCrusher: true}); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("is_error payload must pass through byte-for-byte, got %q", got)
	}
}

func TestCommandCrusher_VerbatimSkipped(t *testing.T) {
	// cat -n numbered source that happens to contain a go-test-looking line.
	payload := "     1\tpackage main\n     2\t// === RUN fake\n     3\tfunc main() {}\n"
	req := map[string]any{"messages": []any{toolResultMsg(payload)}}
	cfg := crusherConfig()
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}

	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &cfg); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("verbatim payload corrupted, got %q", got)
	}
	if reqCtx.VerbatimSkipped != 1 {
		t.Errorf("expected VerbatimSkipped=1, got %d", reqCtx.VerbatimSkipped)
	}
}

func TestCommandCrusher_I3_AssistantTextUntouched(t *testing.T) {
	assistantText := "collected 1 items\n=== 1 passed in 0.01s ==="
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": assistantText},
		}},
		toolResultMsg("collected 1 items\n\ntest_a.py . [100%]\n\n=== 1 passed in 0.01s ==="),
	}}
	cfg := crusherConfig()
	reqCtx := &RequestContext{Request: req}

	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &cfg); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if got != assistantText {
		t.Errorf("assistant text mutated (I3 violation), got %q", got)
	}
}

func TestErrorOrdinals_MixedBlocks(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "a", "is_error": true, "content": "err"},
			map[string]any{"type": "tool_result", "tool_use_id": "b", "content": []any{
				map[string]any{"type": "text", "text": "one"},
				map[string]any{"type": "text", "text": "two"},
			}},
		}},
	}}
	errs := errorOrdinals(req)
	if !errs[0] || errs[1] || errs[2] {
		t.Errorf("expected only ordinal 0 marked, got %v", errs)
	}
}

func TestPytestFilter(t *testing.T) {
	input := `collected 3 items

test_calc.py ..F [100%]

=================================== FAILURES ===================================
______________________________ test_add ______________________________

    def test_add():
>       assert add(1, 1) == 3
E       AssertionError: assert 2 == 3

test_calc.py:10: AssertionError
=========================== short test summary info ============================
FAILED test_calc.py::test_add - AssertionError: assert 2 == 3
=== 1 failed, 2 passed in 0.12s ===`
	got, changed := crushPytest(input)
	if !changed {
		t.Fatal("expected change")
	}
	for _, want := range []string{"AssertionError: assert 2 == 3", "FAILED test_calc.py::test_add", "=== 1 failed, 2 passed in 0.12s ==="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[100%]") {
		t.Errorf("progress line not stripped:\n%s", got)
	}
}

func TestPytestFilter_AllPass(t *testing.T) {
	input := "collected 50 items\n\ntest_a.py ............................................ [ 50%]\n\ntest_b.py  [100%]\n\n=== 50 passed in 1.40s ==="
	got, changed := crushPytest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "....") {
		t.Errorf("dot progress lines survive:\n%s", got)
	}
	if !strings.Contains(got, "=== 50 passed in 1.40s ===") {
		t.Errorf("summary lost:\n%s", got)
	}
}

func TestUnittestFilter(t *testing.T) {
	input := "...\n...\nF..\n======================================================================\nFAIL: test_add (test_calc.TestCalc)\n----------------------------------------------------------------------\nTraceback (most recent call last):\n  File \"test_calc.py\", line 10, in test_add\n    self.assertEqual(add(1, 1), 3)\nAssertionError: 2 != 3\n\n----------------------------------------------------------------------\nRan 9 tests in 0.002s\n\nFAILED (failures=1)\n"
	got, changed := crushUnittest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "...\n...") {
		t.Errorf("dot-only lines survive:\n%q", got)
	}
	if !strings.Contains(got, "F..") || !strings.Contains(got, "FAILED (failures=1)") || !strings.Contains(got, "Traceback") {
		t.Errorf("failure evidence lost:\n%q", got)
	}
}

func TestRuffFilter_Dedupes(t *testing.T) {
	input := "a.py:4:1: E402 module level import not at top\nb.py:9:1: E402 module level import not at top\na.py:4:1: E402 module level import not at top\nFound 3 errors."
	got, changed := crushRuff(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Count(got, "a.py:4:1: E402") != 1 {
		t.Errorf("duplicate violation survives:\n%q", got)
	}
	if !strings.Contains(got, "b.py:9:1: E402") || !strings.Contains(got, "Found 3 errors.") {
		t.Errorf("unique lines lost:\n%q", got)
	}
}

func TestJestFilter(t *testing.T) {
	input := `PASS src/add.test.ts (12ms)
✓ adds numbers (2ms)
✓ subtracts numbers (1ms)
FAIL src/div.test.ts
✕ divides by zero (3ms)

● divides by zero

expect(received).toBe(expected)

Expected: Infinity
Received: NaN

Tests:       1 failed, 45 passed, 46 total
Snapshots:   0 total
Time:        1.234s`
	got, changed := crushJest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "✓") || strings.Contains(got, "PASS ") {
		t.Errorf("passing lines survive:\n%s", got)
	}
	for _, want := range []string{"✕ divides by zero", "FAIL src/div.test.ts", "Expected: Infinity", "Received: NaN", "Tests:       1 failed, 45 passed, 46 total"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestMochaFilter(t *testing.T) {
	input := "  Calculator\n    ✓ adds\n    ✓ subtracts\n    1) divides by zero\n\n\n  2 passing (5ms)\n  1 failing\n\n  1) Calculator\n       divides by zero:\n     Error: boom\n      at Context.<anonymous> (test.js:10:5)\n"
	got, changed := crushMocha(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "✓") {
		t.Errorf("checkmarks survive:\n%q", got)
	}
	if !strings.Contains(got, "2 passing (5ms)") || !strings.Contains(got, "Error: boom") {
		t.Errorf("failure evidence lost:\n%q", got)
	}
}

func TestTypeScriptCompilerFilter(t *testing.T) {
	input := "src/a.ts(12,5): error TS2322: Type 'string' is not assignable to type 'number'.\nsrc/a.ts(12,5): error TS2322: Type 'string' is not assignable to type 'number'.\nsrc/b.ts(3,1): error TS2304: Cannot find name 'foo'."
	got, changed := crushTSC(input)
	if !changed {
		t.Fatal("expected dedupe change")
	}
	if strings.Count(got, "error TS2322") != 1 || !strings.Contains(got, "error TS2304") {
		t.Errorf("bad tsc output:\n%q", got)
	}
}

func TestESLintFilter(t *testing.T) {
	input := "/app/src/a.ts\n  1:5  error  'x' is defined but never used  no-unused-vars\n  1:5  error  'x' is defined but never used  no-unused-vars\n  2:9  warning  Unexpected console statement  no-console\n\n✖ 3 problems (2 errors, 1 warning)"
	got, changed := crushESLint(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Count(got, "no-unused-vars") != 1 {
		t.Errorf("duplicate eslint line survives:\n%q", got)
	}
	if !strings.Contains(got, "no-console") || !strings.Contains(got, "✖ 3 problems") {
		t.Errorf("unique lines lost:\n%q", got)
	}
}
