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
			// Gateway teachers stop at their token limit after the digits.
			name:     "unclosed severity tag",
			text:     "<severity>10",
			severity: 10,
		},
		{
			name:     "unclosed severity tag with trailing text",
			text:     "<severity>15\nThe action",
			severity: 15,
		},
		{
			// A well-formed tag still wins over an earlier unclosed one.
			name:     "closed tag wins over an earlier unclosed one",
			text:     "<severity>9 <severity>20</severity>",
			severity: 20,
		},
		{
			// A truncated tag quoted inside thinking is rationale, not verdict.
			name:     "unclosed tag inside thinking is not recovered",
			text:     "<thinking>A force push would be <severity>90</thinking>",
			severity: -1,
			thinking: "A force push would be <severity>90",
		},
		{
			// A teacher cut off inside its rationale left no verdict at all.
			name:     "unclosed tag inside truncated thinking is not recovered",
			text:     "<thinking>A force push would be <severity>90",
			severity: -1,
		},
		{
			name:     "closed tag inside truncated thinking is not a verdict",
			text:     "<thinking>A force push would be <severity>90</severity>, but",
			severity: -1,
		},
		{
			name:     "multiline thinking",
			text:     "<thinking>line one\nline two</thinking><severity>12</severity>",
			severity: 12,
			thinking: "line one\nline two",
		},
		{
			// A rationale that quotes a severity tag must not hide the final
			// verdict: the quoted 90 would mislabel an allow as a block.
			name:     "thinking quotes a severity tag before the final verdict",
			text:     "<thinking>A force push would be <severity>90</severity>; this is a local edit.</thinking><severity>10</severity>",
			severity: 10,
			thinking: "A force push would be <severity>90</severity>; this is a local edit.",
		},
		{
			name:     "thinking quotes a category the final verdict does not carry",
			text:     "<thinking>Not <category>Destructive Git</category>, only a status check.</thinking><severity>3</severity>",
			severity: 3,
			thinking: "Not <category>Destructive Git</category>, only a status check.",
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
