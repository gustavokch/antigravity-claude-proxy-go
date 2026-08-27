package headroom

import (
	"context"
	"strings"
	"testing"
)

func TestTabularConversion_UniformScalarArray(t *testing.T) {
	// 5 rows with repeated key names
	jsonInput := `[
		{"id": 101, "customer_name": "Acme Corporation", "balance_usd": 1250.50, "status": "active"},
		{"id": 102, "customer_name": "Globex Systems Inc", "balance_usd": 9400.00, "status": "pending"},
		{"id": 103, "customer_name": "Soylent Industries", "balance_usd": 320.10, "status": "active"},
		{"id": 104, "customer_name": "Initech Software LLC", "balance_usd": 8500.00, "status": "active"},
		{"id": 105, "customer_name": "Umbrella Technology", "balance_usd": 450.00, "status": "inactive"}
	]`

	out, changed := TryTabularConversion(jsonInput, 0.30)
	if !changed {
		t.Fatalf("expected tabular conversion to succeed, got changed=false")
	}

	if !strings.HasPrefix(out, "| balance_usd | customer_name | id | status |") {
		t.Errorf("unexpected header in output: %s", out)
	}
	if !strings.Contains(out, "| 1250.50 | Acme Corporation | 101 | active |") {
		t.Errorf("missing expected row: %s", out)
	}
	if len(out) >= len(jsonInput) {
		t.Errorf("expected smaller output, got len(out)=%d >= len(in)=%d", len(out), len(jsonInput))
	}
}

func TestTabularConversion_PreservesLargeNumbers(t *testing.T) {
	jsonInput := `[
		{"txn_id": 9223372036854775807, "acc_num": "ACC123456789", "amount": 1000000},
		{"txn_id": 9223372036854775806, "acc_num": "ACC123456790", "amount": 2000000},
		{"txn_id": 9223372036854775805, "acc_num": "ACC123456791", "amount": 3000000},
		{"txn_id": 9223372036854775804, "acc_num": "ACC123456792", "amount": 4000000},
		{"txn_id": 9223372036854775803, "acc_num": "ACC123456793", "amount": 5000000}
	]`

	out, changed := TryTabularConversion(jsonInput, 0.20)
	if !changed {
		t.Fatalf("expected tabular conversion to succeed")
	}
	if !strings.Contains(out, "9223372036854775807") {
		t.Errorf("large integer precision was lost: %s", out)
	}
}

func TestTabularConversion_RejectsNestedObjectsOrArrays(t *testing.T) {
	jsonInput := `[
		{"id": 1, "metadata": {"role": "admin"}},
		{"id": 2, "metadata": {"role": "user"}},
		{"id": 3, "metadata": {"role": "guest"}}
	]`
	out, changed := TryTabularConversion(jsonInput, 0.10)
	if changed {
		t.Errorf("expected nested objects to be rejected, got: %s", out)
	}
}

func TestTabularConversion_RejectsNonUniformKeys(t *testing.T) {
	jsonInput := `[
		{"id": 1, "name": "Alice"},
		{"id": 2, "name": "Bob", "extra": true},
		{"id": 3, "name": "Charlie"}
	]`
	out, changed := TryTabularConversion(jsonInput, 0.10)
	if changed {
		t.Errorf("expected non-uniform keys to be rejected, got: %s", out)
	}
}

func TestTabularConversion_RejectsLowSavings(t *testing.T) {
	// Small 2-row array with very short keys where table overhead > savings
	jsonInput := `[{"a":1},{"a":2}]`
	_, changed := TryTabularConversion(jsonInput, 0.30)
	if changed {
		t.Errorf("expected low savings to be rejected")
	}
}

func TestTabularConversion_HandlesEscaping(t *testing.T) {
	jsonInput := `[
		{"id": 1, "note": "pipe | inside", "details": "line1\nline2"},
		{"id": 2, "note": "normal note", "details": "single line"},
		{"id": 3, "note": "another | note", "details": "more\nlines\nhere"},
		{"id": 4, "note": "extra note", "details": "some details"}
	]`
	out, changed := TryTabularConversion(jsonInput, 0.10)
	if !changed {
		t.Fatalf("expected conversion to succeed")
	}
	if !strings.Contains(out, "pipe \\| inside") {
		t.Errorf("pipe character was not escaped: %s", out)
	}
	if !strings.Contains(out, "line1\\nline2") {
		t.Errorf("newline character was not escaped: %s", out)
	}
}

func TestTabularConversion_Idempotent(t *testing.T) {
	jsonInput := `[
		{"col_a": "alpha", "col_b": "bravo", "col_c": "charlie"},
		{"col_a": "delta", "col_b": "echo", "col_c": "foxtrot"},
		{"col_a": "golf", "col_b": "hotel", "col_c": "india"},
		{"col_a": "juliet", "col_b": "kilo", "col_c": "lima"}
	]`
	out, changed := TryTabularConversion(jsonInput, 0.20)
	if !changed {
		t.Fatalf("expected conversion")
	}
	twice, changedTwice := TryTabularConversion(out, 0.20)
	if changedTwice || twice != out {
		t.Errorf("expected idempotent, got changed=%v", changedTwice)
	}
}

func TestSmartCrusher_TabularArraysFlag(t *testing.T) {
	jsonInput := `[
		{"id": 101, "customer_name": "Acme Corporation", "balance_usd": 1250.50, "status": "active"},
		{"id": 102, "customer_name": "Globex Systems Inc", "balance_usd": 9400.00, "status": "pending"},
		{"id": 103, "customer_name": "Soylent Industries", "balance_usd": 320.10, "status": "active"},
		{"id": 104, "customer_name": "Initech Software LLC", "balance_usd": 8500.00, "status": "active"},
		{"id": 105, "customer_name": "Umbrella Technology", "balance_usd": 450.00, "status": "inactive"}
	]`

	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": jsonInput},
		}},
	}}

	stage := &SmartCrusherStage{}

	// When TabularArrays is false, only json.Compact is used
	reqCtx := &RequestContext{Request: req}
	if err := stage.Execute(context.Background(), reqCtx, &Config{Enabled: true, SmartCrusher: true, TabularArrays: false}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if strings.HasPrefix(got, "| id |") {
		t.Errorf("tabular conversion should not run when TabularArrays is false")
	}
	if !strings.HasPrefix(got, "[{") {
		t.Errorf("expected compacted JSON, got %s", got)
	}

	// When TabularArrays is true, tabular conversion runs
	req2 := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": jsonInput},
		}},
	}}
	reqCtx2 := &RequestContext{Request: req2}
	if err := stage.Execute(context.Background(), reqCtx2, &Config{Enabled: true, SmartCrusher: true, TabularArrays: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got2 := req2["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got2, "| balance_usd |") {
		t.Errorf("expected tabular conversion when TabularArrays is true, got %s", got2)
	}
}

func TestTabularConversion_NullAndEmptyValues(t *testing.T) {
	jsonInput := `[
		{"col_a": "value1", "col_b": null, "col_c": ""},
		{"col_a": "value2", "col_b": "valid", "col_c": "present"},
		{"col_a": null, "col_b": null, "col_c": ""},
		{"col_a": "value4", "col_b": "another", "col_c": null}
	]`
	out, changed := TryTabularConversion(jsonInput, 0.10)
	if !changed {
		t.Fatalf("expected conversion with null/empty values to succeed")
	}
	if !strings.Contains(out, "| value1 | null |  |") {
		t.Errorf("unexpected row format for null and empty string: %s", out)
	}
	if !strings.Contains(out, "| null | null |  |") {
		t.Errorf("unexpected row format for double null: %s", out)
	}
}

func TestTabularConversion_BackslashesAndSpecialChars(t *testing.T) {
	jsonInput := `[
		{"id": 1, "path": "C:\\Windows\\System32", "query": "field:value AND status:ok"},
		{"id": 2, "path": "D:\\Data\\Exports", "query": "field:\"nested string\""},
		{"id": 3, "path": "/usr/local/bin", "query": "count > 100 | grep 'ok'"},
		{"id": 4, "path": "/var/log/syslog", "query": "line1\r\nline2"}
	]`
	out, changed := TryTabularConversion(jsonInput, 0.10)
	if !changed {
		t.Fatalf("expected conversion with special characters to succeed")
	}
	if !strings.Contains(out, "C:\\\\Windows\\\\System32") {
		t.Errorf("backslash was not escaped properly: %s", out)
	}
	if !strings.Contains(out, "\\| grep 'ok'") {
		t.Errorf("pipe was not escaped properly: %s", out)
	}
	if !strings.Contains(out, "line1\\nline2") {
		t.Errorf("crlf newline was not normalized and escaped properly: %s", out)
	}
}

