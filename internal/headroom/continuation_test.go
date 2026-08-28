package headroom

import (
	"context"
	"strings"
	"testing"
)

func TestClassifyContinuation(t *testing.T) {
	cases := []struct {
		name      string
		req       map[string]any
		inspector *ToolInspector
		want      continuationKind
	}{
		{
			name: "Last msg is plain user string",
			req: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": "hello world"},
				},
			},
			inspector: nil,
			want:      kindInteractive,
		},
		{
			name: "Mixed text and tool_result blocks",
			req: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "text", "text": "here is the result:"},
						map[string]any{"type": "tool_result", "content": "ok"},
					}},
				},
			},
			inspector: nil,
			want:      kindInteractive,
		},
		{
			name: "Any is_error: true",
			req: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "tool_result", "content": "error: file not found", "is_error": true},
					}},
				},
			},
			inspector: nil,
			want:      kindInteractive,
		},
		{
			name: "Empty content array",
			req: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": []any{}},
				},
			},
			inspector: nil,
			want:      kindInteractive,
		},
		{
			name: "Single small tool_result, tool Glob, 40 bytes",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_glob1", "name": "Glob", "input": map[string]any{"pattern": "*.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_glob1", "content": "main.go\nserver.go\n"},
						},
					},
				},
			},
			inspector: NewToolInspector(map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_glob1", "name": "Glob", "input": map[string]any{"pattern": "*.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_glob1", "content": "main.go\nserver.go\n"},
						},
					},
				},
			}),
			want: kindMechanical,
		},
		{
			name: "Single tool_result from Read, verbatim ordinal",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_read1", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_read1", "content": "package main\n\nfunc main() {}\n"},
						},
					},
				},
			},
			inspector: NewToolInspector(map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_read1", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_read1", "content": "package main\n\nfunc main() {}\n"},
						},
					},
				},
			}),
			want: kindCoding,
		},
		{
			name: "tool_result from Edit (applied)",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_edit1", "name": "Edit", "input": map[string]any{"file_path": "main.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_edit1", "content": "applied"},
						},
					},
				},
			},
			inspector: NewToolInspector(map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_edit1", "name": "Edit", "input": map[string]any{"file_path": "main.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_edit1", "content": "applied"},
						},
					},
				},
			}),
			want: kindCoding,
		},
		{
			name: "tool_result from Bash running go test, output --- FAIL",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_bash1", "name": "Bash", "input": map[string]any{"command": "go test ./..."}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_bash1", "content": "--- FAIL: TestFoo (0.01s)\n"},
						},
					},
				},
			},
			inspector: NewToolInspector(map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_bash1", "name": "Bash", "input": map[string]any{"command": "go test ./..."}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_bash1", "content": "--- FAIL: TestFoo (0.01s)\n"},
						},
					},
				},
			}),
			want: kindCoding,
		},
		{
			name: "tool_result 4 KB, unknown tool, prose",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "content": strings.Repeat("a random prose sentence without code features.\n", 100)},
						},
					},
				},
			},
			inspector: nil,
			want:      kindCoding,
		},
		{
			name: "tool_result containing unified diff",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "content": "--- a/file.go\n+++ b/file.go\n@@ -1,3 +1,3 @@\n-old\n+new\n"},
						},
					},
				},
			},
			inspector: nil,
			want:      kindCoding,
		},
		{
			name: "tool_result with cat -n shape, no matching tool_use",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "content": "  1\tpackage main\n  2\t\n  3\tfunc main() {}\n"},
						},
					},
				},
			},
			inspector: nil,
			want:      kindCoding,
		},
		{
			name: "Two tool_results: one tiny Glob, one Read",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_glob2", "name": "Glob", "input": map[string]any{"pattern": "*.go"}},
							map[string]any{"type": "tool_use", "id": "call_read2", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_glob2", "content": "main.go\n"},
							map[string]any{"type": "tool_result", "tool_use_id": "call_read2", "content": "package main\n"},
						},
					},
				},
			},
			inspector: NewToolInspector(map[string]any{
				"messages": []any{
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "tool_use", "id": "call_glob2", "name": "Glob", "input": map[string]any{"pattern": "*.go"}},
							map[string]any{"type": "tool_use", "id": "call_read2", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
						},
					},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_glob2", "content": "main.go\n"},
							map[string]any{"type": "tool_result", "tool_use_id": "call_read2", "content": "package main\n"},
						},
					},
				},
			}),
			want: kindCoding,
		},
		{
			name: "inspector == nil, Read-shaped content",
			req: map[string]any{
				"messages": []any{
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "tool_result", "content": "  1\tpackage main\n  2\t\n  3\tfunc run() {}\n"},
						},
					},
				},
			},
			inspector: nil,
			want:      kindCoding,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyContinuation(tc.req, tc.inspector, 0)
			if got != tc.want {
				t.Errorf("classifyContinuation() = %v (%s), want %v (%s)", got, got.String(), tc.want, tc.want.String())
			}
		})
	}
}

// The coding classifier reads tool names out of the ToolInspector. Building it
// only under PreserveVerbatimReads means turning that flag off silently demotes
// coding continuations to mechanical and clamps thinking mid-edit — the exact
// failure effort routing was fixed to avoid.
func TestEngine_EffortRoutingKeepsInspector(t *testing.T) {
	newReq := func() map[string]any {
		return map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": 16000},
			"messages": []any{
				map[string]any{"role": "user", "content": "edit the file"},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "tool_use", "id": "tu_1", "name": "Edit",
						"input": map[string]any{"file_path": "/repo/main.go"}},
				}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tu_1",
						"content": "Applied 1 edit."},
				}},
			},
		}
	}

	cfg := Config{
		Enabled:               true,
		PreserveVerbatimReads: false,
		OutputShaper: OutputShaperConfig{
			Enabled:                  true,
			EffortRouting:            true,
			MechanicalThinkingBudget: 1024,
		},
	}

	engine := NewEngine(cfg)
	reqCtx, err := engine.Process(context.Background(), newReq())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reqCtx.ContinuationKind != "coding" {
		t.Errorf("continuation = %q; want \"coding\" with PreserveVerbatimReads off", reqCtx.ContinuationKind)
	}
	if reqCtx.EffortClamped {
		t.Error("coding continuation must keep its thinking budget")
	}
}
