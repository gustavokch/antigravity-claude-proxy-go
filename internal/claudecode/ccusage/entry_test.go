package ccusage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReadUsageFile_Fixture(t *testing.T) {
	path := filepath.Join("testdata", "claude", "projects", "project-alpha", "session-alpha", "chat.jsonl")
	entries, err := ReadUsageFile(path)
	if err != nil {
		t.Fatalf("ReadUsageFile: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	e := entries[1]
	want := Entry{
		Timestamp:     time.Date(2026, 1, 9, 12, 30, 0, 0, time.UTC),
		SessionID:     "session-alpha",
		RequestID:     "req-alpha-2",
		MessageID:     "msg-alpha-2",
		Model:         "claude-opus-4-20250514",
		DisplayModel:  "claude-opus-4-20250514-fast",
		Speed:         "fast",
		Input:         200,
		Output:        75,
		CacheCreate:   50,
		CacheCreate5m: 50,
		CacheRead:     20,
		Project:       "project-alpha",
		PathSessionID: "session-alpha",
		ProjectPath:   "project-alpha",
		Source:        SourceLocal,
	}
	if e.CostUSD == nil || *e.CostUSD != 0.34 {
		t.Errorf("CostUSD = %v, want 0.34", e.CostUSD)
	}
	e.CostUSD = nil
	if !e.Timestamp.Equal(want.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", e.Timestamp, want.Timestamp)
	}
	e.Timestamp = want.Timestamp
	if e != want {
		t.Errorf("entry = %+v\nwant   %+v", e, want)
	}
	if got := e.TotalTokens(); got != 345 {
		t.Errorf("TotalTokens = %d, want 345", got)
	}
	if entries[0].DisplayModel != "claude-sonnet-4-20250514" {
		t.Errorf("standard DisplayModel = %q", entries[0].DisplayModel)
	}
}

const validLine = `{"timestamp":"2026-05-22T02:34:40.000Z","version":"1.2.3","sessionId":"s","requestId":"req_1","message":{"id":"msg_1","model":"claude-sonnet-4-20250514","usage":{"input_tokens":1,"output_tokens":2}}}`

func TestParseLine_Rejects(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"no usage marker", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage": {"input_tokens":1,"output_tokens":2}}}`},
		{"not json", `"usage":{`},
		{"missing timestamp", `{"message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"bad timestamp", `{"timestamp":"yesterday","message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"microsecond timestamp", `{"timestamp":"2026-05-22T02:34:40.000000Z","message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"missing input_tokens", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"output_tokens":2}}}`},
		{"missing output_tokens", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"input_tokens":1}}}`},
		{"null input_tokens", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"input_tokens":null,"output_tokens":2}}}`},
		{"negative tokens", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"input_tokens":-1,"output_tokens":2}}}`},
		{"fractional tokens", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"input_tokens":1.5,"output_tokens":2}}}`},
		{"unknown speed", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"input_tokens":1,"output_tokens":2,"speed":"turbo"}}}`},
		{"null speed", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"input_tokens":1,"output_tokens":2,"speed":null}}}`},
		{"null model", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"model":null,"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"null sessionId", `{"timestamp":"2026-05-22T02:34:40.000Z","sessionId":null,"message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"null costUSD", `{"timestamp":"2026-05-22T02:34:40.000Z","costUSD":null,"message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"empty sessionId", `{"timestamp":"2026-05-22T02:34:40.000Z","sessionId":"","message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"empty requestId", `{"timestamp":"2026-05-22T02:34:40.000Z","requestId":"","message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"empty message id", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"id":"","usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"empty model", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"model":"","usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"non-semver version", `{"timestamp":"2026-05-22T02:34:40.000Z","version":"1.2","message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"wrong type", `{"timestamp":"2026-05-22T02:34:40.000Z","sessionId":7,"message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"duplicate member", `{"type":"assistant","timestamp":"2026-09-12T04:38:42.296Z","sessionId":"session-a","message":{"id":"msg_1","model":"first","model":"second","usage":{"input_tokens":2,"output_tokens":289,"iterations":[{"type":"message","model":null,"input_tokens":2,"output_tokens":289}]}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseLine([]byte(tt.line)); got != nil {
				t.Errorf("ParseLine = %+v, want nil", got)
			}
		})
	}
}

func TestParseLine_Accepts(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"full", validLine},
		{"minimal", `{"timestamp":"2026-05-22T02:34:40Z","message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"semver prefix", `{"timestamp":"2026-05-22T02:34:40.000Z","version":"2.1.3-beta","message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"null content", `{"timestamp":"2026-05-22T02:34:40.000Z","message":{"content":null,"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"null isSidechain", `{"timestamp":"2026-05-22T02:34:40.000Z","isSidechain":null,"message":{"usage":{"input_tokens":1,"output_tokens":2}}}`},
		{"crlf", validLine + "\r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseLine([]byte(tt.line)); len(got) != 1 {
				t.Errorf("ParseLine returned %d entries, want 1", len(got))
			}
		})
	}
}

func TestParseLine_Fields(t *testing.T) {
	line := `{"timestamp":"2026-05-22T08:04:40.123+05:30","sessionId":"s","requestId":"r","isSidechain":true,"costUSD":0.5,"message":{"id":"m","model":"<synthetic>","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":4,"cache_creation_input_tokens":99,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":5}}}}`
	got := ParseLine([]byte(line))
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	e := got[0]
	if want := time.Date(2026, 5, 22, 2, 34, 40, 123e6, time.UTC); !e.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", e.Timestamp, want)
	}
	if e.SessionID != "s" || e.RequestID != "r" || e.MessageID != "m" || !e.IsSidechain {
		t.Errorf("ids = %q %q %q sidechain=%v", e.SessionID, e.RequestID, e.MessageID, e.IsSidechain)
	}
	if e.Model != "<synthetic>" || e.DisplayModel != "" {
		t.Errorf("Model = %q, DisplayModel = %q; want <synthetic> and empty", e.Model, e.DisplayModel)
	}
	if e.CacheCreate5m != 3 || e.CacheCreate1h != 5 || e.CacheCreate != 8 {
		t.Errorf("cache create = %d/%d/%d, want 3/5/8", e.CacheCreate5m, e.CacheCreate1h, e.CacheCreate)
	}
	if e.TotalTokens() != 15 {
		t.Errorf("TotalTokens = %d, want 15", e.TotalTokens())
	}
	if e.CostUSD == nil || *e.CostUSD != 0.5 {
		t.Errorf("CostUSD = %v, want 0.5", e.CostUSD)
	}

	noModel := ParseLine([]byte(`{"timestamp":"2026-05-22T02:34:40.000Z","message":{"usage":{"input_tokens":1,"output_tokens":2,"speed":"fast"}}}`))
	if len(noModel) != 1 || noModel[0].DisplayModel != "" || noModel[0].Speed != "fast" {
		t.Errorf("model-less fast entry = %+v", noModel)
	}
}

func TestParseLine_Advisor(t *testing.T) {
	// From ccusage's loads_advisor_usage_as_a_separate_model_entry.
	line := `{"timestamp":"2026-05-22T02:34:40.000Z","version":"1.2.3","sessionId":"session1","advisorModel":"claude-opus-4-20250514","message":{"id":"msg_123","model":"claude-sonnet-4-20250514","usage":{"input_tokens":2,"output_tokens":491,"cache_creation_input_tokens":7853,"cache_read_input_tokens":226584,"iterations":[{"type":"message","input_tokens":1,"output_tokens":45,"cache_creation_input_tokens":7192,"cache_read_input_tokens":109696},{"type":"advisor_message","model":"claude-opus-4-20250514","input_tokens":159419,"output_tokens":7805,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},{"type":"message","input_tokens":1,"output_tokens":446,"cache_creation_input_tokens":661,"cache_read_input_tokens":116888}]}},"requestId":"req_456","costUSD":1.23}`
	got := ParseLine([]byte(line))
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	main, adv := got[0], got[1]
	if main.MessageID != "msg_123" || main.Input != 2 || main.Output != 491 || main.CostUSD == nil || *main.CostUSD != 1.23 {
		t.Errorf("main entry = %+v", main)
	}
	if adv.MessageID != "msg_123:advisor:0" || adv.Model != "claude-opus-4-20250514" || adv.DisplayModel != "claude-opus-4-20250514" {
		t.Errorf("advisor ids = %q %q %q", adv.MessageID, adv.Model, adv.DisplayModel)
	}
	if adv.Input != 159419 || adv.Output != 7805 || adv.CacheRead != 0 || adv.CacheCreate != 0 {
		t.Errorf("advisor tokens = %d/%d/%d/%d", adv.Input, adv.Output, adv.CacheCreate, adv.CacheRead)
	}
	if adv.CostUSD != nil {
		t.Errorf("advisor CostUSD = %v, want nil", *adv.CostUSD)
	}
	if adv.RequestID != "req_456" || adv.SessionID != "session1" || !adv.Timestamp.Equal(main.Timestamp) {
		t.Errorf("advisor did not inherit line fields: %+v", adv)
	}
}

func TestParseLine_AdvisorEdgeCases(t *testing.T) {
	prefix := `{"timestamp":"2026-05-22T02:34:40.000Z","requestId":"r","message":{"id":"m","model":"main","usage":{"input_tokens":1,"output_tokens":2,"iterations":`
	tests := []struct {
		name       string
		iterations string
		wantIDs    []string
	}{
		{"index counts advisors only", `[{"type":"message","input_tokens":1,"output_tokens":1},{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1},{"type":"advisor_message","model":"b","input_tokens":1,"output_tokens":1}]`, []string{"m", "m:advisor:0", "m:advisor:1"}},
		{"empty and missing advisor model skipped", `[{"type":"advisor_message","model":"","input_tokens":1,"output_tokens":1},{"type":"advisor_message","input_tokens":1,"output_tokens":1},{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1}]`, []string{"m", "m:advisor:0"}},
		{"malformed iteration discards advisors", `[{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1},{"type":"message","input_tokens":1}]`, []string{"m"}},
		{"null iteration model", `[{"type":"message","model":null,"input_tokens":1,"output_tokens":1},{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1,"speed":"fast"}]`, []string{"m", "m:advisor:0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseLine([]byte(prefix + tt.iterations + "}}}"))
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("len = %d, want %d: %+v", len(got), len(tt.wantIDs), got)
			}
			for i, id := range tt.wantIDs {
				if got[i].MessageID != id {
					t.Errorf("entry %d MessageID = %q, want %q", i, got[i].MessageID, id)
				}
			}
		})
	}

	// An advisor iteration's speed is its own, and its display model has no suffix.
	got := ParseLine([]byte(prefix + `[{"type":"advisor_message","model":"a","input_tokens":1,"output_tokens":1,"speed":"fast"}]}}}`))
	if len(got) != 2 || got[1].Speed != "fast" || got[1].DisplayModel != "a" || got[0].Speed != "" {
		t.Errorf("advisor speed entries = %+v", got)
	}
}

func TestParseLine_IterationModelNull(t *testing.T) {
	// The repro line from ccusage #1710.
	line := `{"type":"assistant","timestamp":"2026-09-12T04:38:42.296Z","version":"2.1.268","sessionId":"session-a","requestId":"req_1","message":{"id":"msg_1","model":"claude-fable-5-1","role":"assistant","usage":{"input_tokens":2,"cache_creation_input_tokens":55866,"cache_read_input_tokens":0,"output_tokens":289,"iterations":[{"type":"message","model":null,"input_tokens":2,"output_tokens":289,"cache_creation_input_tokens":55866,"cache_read_input_tokens":0}]}}}`
	got := ParseLine([]byte(line))
	if len(got) != 1 || got[0].DisplayModel != "claude-fable-5-1" || got[0].Output != 289 || got[0].CacheCreate != 55866 {
		t.Errorf("ParseLine = %+v", got)
	}
}

func TestParseLine_UsageLimitReset(t *testing.T) {
	line := `{"timestamp":"2025-01-10T10:00:00.000Z","isApiErrorMessage":true,"message":{"content":[{"text":"Claude AI usage limit reached|1736503200 remaining"}],"usage":{"input_tokens":0,"output_tokens":0}}}`
	got := ParseLine([]byte(line))
	if len(got) != 1 || !got[0].IsAPIError || got[0].UsageLimitResetAt == nil {
		t.Fatalf("ParseLine = %+v", got)
	}
	if want := time.Date(2025, 1, 10, 10, 0, 0, 0, time.UTC); !got[0].UsageLimitResetAt.Equal(want) {
		t.Errorf("UsageLimitResetAt = %v, want %v", got[0].UsageLimitResetAt, want)
	}

	for name, l := range map[string]string{
		"not an api error": `{"timestamp":"2025-01-10T10:00:00.000Z","isApiErrorMessage":false,"message":{"content":[{"text":"Claude AI usage limit reached|1736503200"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
		"zero epoch":       `{"timestamp":"2025-01-10T10:00:00.000Z","isApiErrorMessage":true,"message":{"content":[{"text":"Claude AI usage limit reached|0"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
		"no epoch":         `{"timestamp":"2025-01-10T10:00:00.000Z","isApiErrorMessage":true,"message":{"content":[{"text":"Claude AI usage limit reached|soon"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
	} {
		got := ParseLine([]byte(l))
		if len(got) != 1 || got[0].UsageLimitResetAt != nil {
			t.Errorf("%s: ParseLine = %+v, want one entry without a reset", name, got)
		}
	}
}

func TestHasUnsupportedNullField(t *testing.T) {
	// Ported from ccusage's rejects_null_schema_fields_like_typescript_loader and the
	// iteration-model exemption tests.
	tests := []struct {
		line string
		want bool
	}{
		{`{"message":{"usage":{"speed":null}}}`, true},
		{`{"message":{"model":null,"usage":{"input_tokens":0}}}`, true},
		{`{"message":{"model": null,"usage":{"input_tokens":0}}}`, true},
		{`{"sessionId":null,"message":{"usage":{"input_tokens":0}}}`, true},
		{`{"message":{"content":null,"usage":{"input_tokens":0}}}`, false},
		{`{"message":{"model":"claude-fable-5-1","usage":{"input_tokens":2,"output_tokens":289,"iterations":[{"type":"message","model":null,"input_tokens":2,"output_tokens":289}]}}}`, false},
		{`{"message":{"model":"m","usage":{"input_tokens":2,"iterations":[{"type":"message","input_tokens":2}]}}}`, false},
		{`{"message":{"model":"m","usage":{"input_tokens":2,"iterations":[]}}}`, false},
		{`{"message":{"model":"m","usage":{"iterations":[{"type":"ad]vi\"sor}","model":null,"input_tokens":1}]}}}`, false},
		{`{"message":{"model":null,"usage":{"iterations":[{"type":"message","model":"m"}]}}}`, true},
		{`{"message":{"usage":{"iterations":[{"type":"message","model":"m"}]},"model":null}}`, true},
		{`{"message":{"model":"m","usage":{"iterations":[{"type":"message","model":null,"cache_read_input_tokens":null}]}}}`, true},
		{`{"message": {"model": "m", "usage" : { "input_tokens": 2, "iterations" : [ {"type": "message", "model": null} ] }}}`, false},
		{`{"message":{"model":"m","usage":{"iterations" : [{"type":"message","model" : null}]}}}`, false},
		{`{"toolUseResult":{"iterations":[{"model":"x"}]},"message":{"model":"m","usage":{"iterations":[{"type":"message","model":null}]}}}`, false},
		{`{"toolUseResult":{"iterations":[{"model":null}]},"message":{"model":"m","usage":{"input_tokens":1}}}`, true},
		{`{"usage":"n/a","message":{"model":"m","usage":{"iterations":[{"type":"message","model":null}]}}}`, false},
		{`{"message":{"model":"m","usage":{"server_tool_use":{"iterations":[{"model":null}]},"input_tokens":1}}}`, true},
		{`{"message":{"model":"m","usage":{"iterations":[{"type":"message","metadata":{"model":null}}]}}}`, true},
		{`{"message":{"content":"\"usage\":{\"iterations\":[","model":null,"usage":{"input_tokens":1}}}`, true},
		{`{"message":{"model":"m","usage":{"cache_creation":{"ephemeral_5m_input_tokens":0},"server_tool_use":{"web_search_requests":0},"inference_geo":"[not]{available}","iterations":[{"type":"message","model":null}]}}}`, false},
		{`{"usage":{"iterations":[{"model":"decoy"}]},"message":{"model":"m","usage":{"iterations":[{"type":"message","model":null}]}}}`, false},
		{`{"type":"progress","data":{"message":{"message":{"model":"m","usage":{"iterations":[{"type":"message","model":null}]}}}}}`, false},
		{`{"message":{"model":"m","usage":{"input_tokens":,"iterations":[{"type":"message","model":null}]}}}`, false},
	}
	for i, tt := range tests {
		if got := hasUnsupportedNullField([]byte(tt.line)); got != tt.want {
			t.Errorf("case %d: hasUnsupportedNullField(%s) = %v, want %v", i, tt.line, got, tt.want)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2026-01-09T10:00:00.000Z", time.Date(2026, 1, 9, 10, 0, 0, 0, time.UTC), true},
		{"2026-01-09T10:00:00Z", time.Date(2026, 1, 9, 10, 0, 0, 0, time.UTC), true},
		{"2026-01-09T10:00:00.250-02:00", time.Date(2026, 1, 9, 12, 0, 0, 250e6, time.UTC), true},
		{"2026-01-09T10:00:00+01:00", time.Date(2026, 1, 9, 9, 0, 0, 0, time.UTC), true},
		{"2024-02-29T00:00:00Z", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), true},
		{"2026-02-29T00:00:00Z", time.Time{}, false},
		{"2026-13-01T00:00:00Z", time.Time{}, false},
		{"2026-01-09T24:00:00Z", time.Time{}, false},
		{"2026-01-09 10:00:00Z", time.Time{}, false},
		{"2026-01-09T10:00:00.000000Z", time.Time{}, false},
		{"2026-01-09T10:00:00", time.Time{}, false},
		{"2026-01-09T10:00:00+0100", time.Time{}, false},
	}
	for _, tt := range tests {
		got, ok := parseTimestamp(tt.in)
		if ok != tt.ok || (ok && !got.Equal(tt.want)) {
			t.Errorf("parseTimestamp(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestIsSemverPrefix(t *testing.T) {
	for in, want := range map[string]bool{
		"1.0.0": true, "2.1.268": true, "1.2.3-beta": true, "10.20.3x": true,
		"1.2": false, "1.2.": false, "v1.2.3": false, "": false, "1..3": false, ".1.2": false,
	} {
		if got := isSemverPrefix(in); got != want {
			t.Errorf("isSemverPrefix(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTotalTokens_Saturates(t *testing.T) {
	e := Entry{Input: 1 << 62, Output: 1 << 62, CacheCreate: 1 << 62, CacheRead: 1 << 62}
	if got := e.TotalTokens(); got != 1<<63-1 {
		t.Errorf("TotalTokens = %d, want MaxInt64", got)
	}
}
