package corpus

import "testing"

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		severity int
		category string
		thinking string
	}{
		{
			name:     "stage 1 bare severity",
			text:     "<severity>0</severity>",
			severity: 0,
		},
		{
			name:     "stage 2 thinking then severity",
			text:     "<thinking>Routine cleanup.</thinking><severity>8</severity>",
			severity: 8,
			thinking: "Routine cleanup.",
		},
		{
			name:     "stage 2 blocking verdict carries a category",
			text:     "<thinking>Deletes history.</thinking><severity>80</severity><category>Destructive Git</category>",
			severity: 80,
			thinking: "Deletes history.",
			category: "Destructive Git",
		},
		{
			name:     "multiline thinking",
			text:     "<thinking>line one\nline two</thinking><severity>12</severity>",
			severity: 12,
			thinking: "line one\nline two",
		},
		{
			name:     "no severity tag",
			text:     "<block>true</block>",
			severity: -1,
		},
		{
			name:     "severity is not a number",
			text:     "<severity>high</severity>",
			severity: -1,
		},
		{
			name:     "empty input",
			text:     "",
			severity: -1,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verdict := ParseVerdict(testCase.text)
			if verdict.Raw != testCase.text {
				t.Errorf("Raw = %q, want %q", verdict.Raw, testCase.text)
			}
			if verdict.Severity != testCase.severity {
				t.Errorf("Severity = %d, want %d", verdict.Severity, testCase.severity)
			}
			if verdict.Category != testCase.category {
				t.Errorf("Category = %q, want %q", verdict.Category, testCase.category)
			}
			if verdict.Thinking != testCase.thinking {
				t.Errorf("Thinking = %q, want %q", verdict.Thinking, testCase.thinking)
			}
		})
	}
}
