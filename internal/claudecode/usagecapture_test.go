package claudecode

import (
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

type captureCloser struct {
	io.Reader
	closed bool
}

func (c *captureCloser) Close() error {
	c.closed = true
	return nil
}

// drainInterceptor reads the whole body through the interceptor in small
// chunks, so SSE lines are split across reads, and returns what was read and
// every Usage reported.
func drainInterceptor(t *testing.T, body string) (string, []Usage) {
	t.Helper()
	var got []Usage
	src := &captureCloser{Reader: strings.NewReader(body)}
	ic := NewUsageInterceptor(src, func(u Usage) { got = append(got, u) })
	var out strings.Builder
	buf := make([]byte, 7)
	for {
		n, err := ic.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if err := ic.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !src.closed {
		t.Errorf("underlying body not closed")
	}
	return out.String(), got
}

func TestUsageInterceptor_StreamWithCacheSplit(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_01abc","type":"message","role":"assistant","model":"claude-opus-5","usage":{"input_tokens":12,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200},"output_tokens":1,"speed":"standard"}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the usage is \"usage\""}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":250,"iterations":[{"type":"message","input_tokens":12,"output_tokens":250,"cache_read_input_tokens":4000,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}]}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	out, got := drainInterceptor(t, sse)
	if out != sse {
		t.Fatalf("body altered:\n got %q\nwant %q", out, sse)
	}
	if len(got) != 1 {
		t.Fatalf("onComplete called %d times, want 1", len(got))
	}
	want := Usage{
		MessageID:     "msg_01abc",
		Model:         "claude-opus-5",
		Input:         12,
		Output:        250,
		CacheRead:     4000,
		CacheCreate:   300,
		CacheCreate5m: 100,
		CacheCreate1h: 200,
		Speed:         "standard",
		Iterations: []IterationUsage{{
			Type: "message", Input: 12, Output: 250, CacheRead: 4000,
			CacheCreate: 300, CacheCreate5m: 100, CacheCreate1h: 200,
		}},
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("usage = %+v\nwant   %+v", got[0], want)
	}
}

func TestUsageInterceptor_DeltaFallbackRules(t *testing.T) {
	tests := []struct {
		name string
		sse  string
		want Usage
	}{
		{
			// Translated upstreams report zero in message_start and the
			// real figures in message_delta.
			name: "zero start falls back to delta",
			sse: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_z\",\"model\":\"m\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n" +
				"data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":70,\"cache_read_input_tokens\":30,\"cache_creation_input_tokens\":9,\"output_tokens\":5}}\n",
			want: Usage{MessageID: "msg_z", Model: "m", Input: 70, Output: 5, CacheRead: 30, CacheCreate: 9, CacheCreate5m: 9},
		},
		{
			// Upstreams reporting in both events must not be double-counted,
			// and a smaller delta figure must not overwrite message_start.
			name: "start wins for non-zero input fields",
			sse: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":50,\"output_tokens\":1}}}\n" +
				"data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":3,\"output_tokens\":40}}\n",
			want: Usage{Input: 100, Output: 40, CacheRead: 50},
		},
		{
			name: "no cache_creation object puts all writes in 5m",
			sse:  "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"cache_creation_input_tokens\":64}}}\n",
			want: Usage{Input: 1, CacheCreate: 64, CacheCreate5m: 64},
		},
		{
			name: "missing trailing newline is still parsed",
			sse:  "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":8}}",
			want: Usage{Output: 8},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, got := drainInterceptor(t, tc.sse)
			if len(got) != 1 {
				t.Fatalf("onComplete called %d times, want 1", len(got))
			}
			if !reflect.DeepEqual(got[0], tc.want) {
				t.Errorf("usage = %+v\nwant   %+v", got[0], tc.want)
			}
		})
	}
}

// An aborted stream (client disconnect: Close before EOF) must still report
// the usage seen so far, exactly once.
func TestUsageInterceptor_CloseBeforeEOFReportsPartialOnce(t *testing.T) {
	head := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_p\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":33,\"cache_read_input_tokens\":11,\"output_tokens\":1}}}\n\n"
	rest := "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":999}}\n\n"
	calls := 0
	var got Usage
	src := &captureCloser{Reader: strings.NewReader(head + rest)}
	ic := NewUsageInterceptor(src, func(u Usage) { calls++; got = u })

	buf := make([]byte, len(head))
	if _, err := io.ReadFull(ic, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if calls != 0 {
		t.Fatalf("onComplete fired before the stream ended")
	}
	_ = ic.Close()
	_ = ic.Close()
	_, _ = ic.Read(make([]byte, 8))

	if calls != 1 {
		t.Fatalf("onComplete called %d times, want 1", calls)
	}
	want := Usage{MessageID: "msg_p", Model: "claude-sonnet-5", Input: 33, Output: 1, CacheRead: 11}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usage = %+v\nwant   %+v", got, want)
	}
}

type errReader struct {
	data string
	done bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, errors.New("connection reset")
	}
	e.done = true
	return copy(p, e.data), nil
}

func (e *errReader) Close() error { return nil }

func TestUsageInterceptor_ReadErrorFinalizes(t *testing.T) {
	calls := 0
	var got Usage
	ic := NewUsageInterceptor(&errReader{data: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_e\",\"usage\":{\"input_tokens\":4}}}\n"},
		func(u Usage) { calls++; got = u })
	_, _ = io.ReadAll(ic)
	_ = ic.Close()
	if calls != 1 {
		t.Fatalf("onComplete called %d times, want 1", calls)
	}
	if got.MessageID != "msg_e" || got.Input != 4 {
		t.Errorf("usage = %+v", got)
	}
}

func TestParseUsageJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Usage
	}{
		{
			name: "unary with cache split and speed",
			body: `{"id":"msg_01u","type":"message","model":"claude-opus-5","content":[{"type":"text","text":"hi"}],` +
				`"usage":{"input_tokens":1200,"output_tokens":345,"cache_read_input_tokens":800,"cache_creation_input_tokens":100,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":60},"speed":"fast"}}`,
			want: Usage{MessageID: "msg_01u", Model: "claude-opus-5", Input: 1200, Output: 345, CacheRead: 800,
				CacheCreate: 100, CacheCreate5m: 40, CacheCreate1h: 60, Speed: "fast"},
		},
		{
			name: "unary without cache_creation",
			body: `{"id":"msg_2","type":"message","model":"m","usage":{"input_tokens":5,"output_tokens":10,"cache_creation_input_tokens":7}}`,
			want: Usage{MessageID: "msg_2", Model: "m", Input: 5, Output: 10, CacheCreate: 7, CacheCreate5m: 7},
		},
		{
			name: "error envelope",
			body: `{"type":"error","error":{"type":"overloaded_error","message":"x"}}`,
			want: Usage{},
		},
		{
			name: "malformed",
			body: `{not json`,
			want: Usage{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseUsageJSON([]byte(tc.body)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseUsageJSON = %+v\nwant           %+v", got, tc.want)
			}
		})
	}
}

func TestUsageCost_DefaultMatchesCalculateCost(t *testing.T) {
	u := Usage{Input: 1000, Output: 500, CacheRead: 2000, CacheCreate: 300, CacheCreate5m: 100, CacheCreate1h: 200}
	want := CalculateCost("claude-opus-5", 1000, 500, 300, 2000)
	if got := UsageCost("claude-opus-5", u); math.Abs(got-want) > 1e-12 {
		t.Errorf("UsageCost = %v, want %v", got, want)
	}
}

func TestSetPricer_RoutesComputeFinalMetrics(t *testing.T) {
	var seen Usage
	SetPricer(func(model string, u Usage) float64 {
		seen = u
		return 1.5
	})
	defer SetPricer(nil)

	m := RequestMetrics{Model: "x", InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30,
		CacheCreationTokens: 40, CacheCreation1hTokens: 15, Speed: "fast", SessionID: "pricer-test"}
	m.ComputeFinalMetrics(NewSessionTracker())
	if m.CallCost != 1.5 {
		t.Errorf("CallCost = %v, want 1.5 from the installed pricer", m.CallCost)
	}
	want := Usage{Model: "x", Input: 10, Output: 20, CacheRead: 30, CacheCreate: 40, CacheCreate5m: 25, CacheCreate1h: 15, Speed: "fast"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("pricer saw %+v\nwant       %+v", seen, want)
	}

	SetPricer(nil)
	if got, want := UsageCost("x", want), DefaultPricer("x", want); got != want {
		t.Errorf("after SetPricer(nil) UsageCost = %v, want default %v", got, want)
	}
}
