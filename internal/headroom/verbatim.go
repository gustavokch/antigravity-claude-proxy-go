package headroom

import (
	"regexp"
	"strconv"
	"strings"
)

// ToolUseInfo records what a tool_use block asked for, keyed by its id, so the
// matching tool_result can be classified before any lossy stage runs.
type ToolUseInfo struct {
	ID   string
	Name string
	// Verbatim is true when this tool's output is expected to be quoted back
	// byte-for-byte by a later Edit or patch call.
	Verbatim bool
}

// ToolInspector answers, for each tool_result payload in a request, whether
// that payload must survive the pipeline unchanged.
//
// It is built once per request, before any stage runs. That ordering matters:
// one of the signals is the shape of the payload text itself, and a stage that
// compacted the text first would erase the evidence.
type ToolInspector struct {
	byID map[string]ToolUseInfo
	// verbatimOrdinals is keyed by the payload's position in document order, as
	// produced by walkToolResultText. Stages never add or remove tool_result
	// blocks, so an ordinal identifies the same payload for the whole pipeline.
	verbatimOrdinals map[int]bool
}

// verbatimToolNames are tools whose result is file content the model will be
// asked to quote back exactly.
var verbatimToolNames = map[string]bool{
	"read": true, "read_file": true, "readfile": true,
	"view": true, "view_file": true, "viewfile": true,
	"read_multiple_files": true, "readmultiplefiles": true,
	"cat": true, "open_file": true, "openfile": true,
	"get_file_contents": true, "getfilecontents": true,
	"fs_read": true, "notebookread": true, "notebook_read": true,
	"str_replace_editor": true, "text_editor": true,
}

// nonVerbatimToolNames are tools that name a file but do not return its
// contents. Their results are confirmations, search hits, or diffs the model
// never quotes back, so the inputLooksLikeFileRead over-match must not reach
// them. Names only: a payload that genuinely carries numbered source or a patch
// is still caught by the shape pass.
var nonVerbatimToolNames = map[string]bool{
	"edit": true, "multiedit": true, "write": true, "create_file": true,
	"str_replace": true, "apply_patch": true, "patch": true,
	"notebookedit": true, "notebook_edit": true,
	"glob": true, "grep": true, "search": true, "search_files": true,
	"delete_file": true, "remove_file": true, "move_file": true,
}

func isNonVerbatimToolName(name string) bool {
	return nonVerbatimToolNames[normalizeToolName(name)]
}

// strongPathKeys name a file by contract; any non-empty string value counts.
var strongPathKeys = map[string]bool{
	"file_path": true, "filepath": true, "absolute_path": true,
	"notebook_path": true,
}

// weakPathKeys are generic; the value must also look like a path.
var weakPathKeys = map[string]bool{
	"path": true, "filename": true, "file_name": true,
	"file": true, "uri": true,
}

// numberedLineRe matches the `cat -n` line shape that Read and most MCP file
// tools emit: a small left margin, digits, then a tab or pipe separator.
var numberedLineRe = regexp.MustCompile(`^\s{0,6}(\d+)(\t|\s*\|\s?)`)

// minNumberedLines is the consecutive run length that distinguishes numbered
// source from a log with a leading counter.
const minNumberedLines = 3

// numberedSourceScanCap bounds the scan: the shape is decided in the first
// few lines, so huge payloads cost a bounded prefix scan only.
const numberedSourceScanCap = 64

// NewToolInspector builds the verbatim classification for req.
func NewToolInspector(req map[string]any) *ToolInspector {
	t := &ToolInspector{
		byID:             make(map[string]ToolUseInfo),
		verbatimOrdinals: make(map[int]bool),
	}

	// Pass 1: index every tool_use block and classify by name and input.
	WalkToolUseBlocks(req, func(_ int, block map[string]any) {
		id, _ := block["id"].(string)
		if id == "" {
			return
		}
		name, _ := block["name"].(string)
		info := ToolUseInfo{ID: id, Name: name}
		// Name match first: str_replace_editor and text_editor are dual mode and
		// belong to both sets, and their read command is what must survive.
		if isVerbatimToolName(name) {
			info.Verbatim = true
		} else if !isNonVerbatimToolName(name) {
			if input, ok := block["input"].(map[string]any); ok && inputLooksLikeFileRead(input) {
				info.Verbatim = true
			}
		}
		t.byID[id] = info
	})

	// Pass 2: mark each tool_result payload whose call is verbatim, or whose
	// own text carries the shape of numbered source or a patch.
	WalkToolResultText(req, 0, func(_, ord int, get func() string, _ func(string)) {
		text := get()
		if looksLikeNumberedSource(text) || looksLikeUnifiedDiff(text) {
			t.verbatimOrdinals[ord] = true
		}
	})

	// A payload is also verbatim when its tool_use resolves to a verbatim tool.
	// WalkToolResultText does not expose tool_use_id, so do that mapping here.
	ord := 0
	WalkMessages(req, func(msg map[string]any) {
		blocks, ok := msg["content"].([]any)
		if !ok {
			return
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				continue
			}
			n := countTextPayloads(block)
			if id, _ := block["tool_use_id"].(string); id != "" {
				if info, ok := t.byID[id]; ok && info.Verbatim {
					for j := ord; j < ord+n; j++ {
						t.verbatimOrdinals[j] = true
					}
				}
			}
			ord += n
		}
	})

	return t
}

// Lookup returns the recorded tool_use info for an id.
func (t *ToolInspector) Lookup(toolUseID string) (ToolUseInfo, bool) {
	info, ok := t.byID[toolUseID]
	return info, ok
}

// IsVerbatimOrdinal reports whether the payload at document-order position ord
// must survive the pipeline byte-for-byte.
func (t *ToolInspector) IsVerbatimOrdinal(ord int) bool {
	return t.verbatimOrdinals[ord]
}

// VerbatimCount is how many payloads were classified verbatim.
func (t *ToolInspector) VerbatimCount() int {
	return len(t.verbatimOrdinals)
}

// SkipVerbatim reports whether the payload at ord must be left byte-for-byte,
// recording the skip for telemetry.
func SkipVerbatim(reqCtx *RequestContext, cfg *Config, ord int) bool {
	if !cfg.PreserveVerbatimReads || reqCtx == nil || reqCtx.Verbatim == nil {
		return false
	}
	if !reqCtx.Verbatim.IsVerbatimOrdinal(ord) {
		return false
	}
	reqCtx.VerbatimSkipped++
	return true
}

// normalizeToolName lowercases and strips MCP and namespace decoration, so
// "mcp__filesystem__read_file", "filesystem:read_file", and "fs.readFile" all
// reduce to the bare tool name.
func normalizeToolName(name string) string {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.Split(name, "__")
		name = parts[len(parts)-1]
	}
	if i := strings.LastIndexAny(name, ":."); i != -1 {
		name = name[i+1:]
	}
	return name
}

func isVerbatimToolName(name string) bool {
	return verbatimToolNames[normalizeToolName(name)]
}

// inputLooksLikeFileRead reports whether a tool_use input names a single file
// path. It deliberately over-matches: classifying an Edit or Write
// confirmation as verbatim costs a few bytes of missed compression, while
// missing a real file read costs a broken Edit.
func inputLooksLikeFileRead(input map[string]any) bool {
	for key, raw := range input {
		value, ok := raw.(string)
		if !ok || value == "" {
			continue
		}
		k := strings.ToLower(key)
		if strongPathKeys[k] {
			return true
		}
		if weakPathKeys[k] && (strings.Contains(value, "/") || strings.Contains(value, `\`)) {
			return true
		}
	}
	return false
}

// looksLikeNumberedSource reports whether text is line-numbered file content —
// the `cat -n` shape that Read and most MCP file tools emit. Requires at least
// minNumberedLines consecutive matching lines with non-decreasing numbers so a
// log with a leading counter does not trigger it.
func looksLikeNumberedSource(text string) bool {
	lines := strings.SplitN(text, "\n", numberedSourceScanCap+1)
	if len(lines) > numberedSourceScanCap {
		lines = lines[:numberedSourceScanCap]
	}
	run := 0
	prev := -1
	for _, line := range lines {
		m := numberedLineRe.FindStringSubmatch(line)
		if m == nil {
			run = 0
			prev = -1
			continue
		}
		// A counter too large for int is not a line number. Treat the parse
		// failure as a non-match so the run restarts, rather than comparing a
		// wrapped value.
		n, err := strconv.Atoi(m[1])
		if err != nil {
			run = 0
			prev = -1
			continue
		}
		if run > 0 && n < prev {
			run = 0
		}
		run++
		prev = n
		if run >= minNumberedLines {
			return true
		}
	}
	return false
}

// looksLikeUnifiedDiff reports whether text is a unified diff or patch body.
func looksLikeUnifiedDiff(text string) bool {
	if !strings.HasPrefix(text, "--- ") && !strings.Contains(text, "\n--- ") {
		return false
	}
	return strings.Contains(text, "\n+++ ") && strings.Contains(text, "\n@@ ")
}

// WalkMessages calls fn for every well-formed message map in order.
func WalkMessages(req map[string]any, fn func(msg map[string]any)) {
	messages, ok := req["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range messages {
		if msg, ok := raw.(map[string]any); ok {
			fn(msg)
		}
	}
}

// countTextPayloads mirrors walkToolResultText's ordinal accounting: one per
// string-form payload or per text block inside an array-form payload.
func countTextPayloads(toolResultBlock map[string]any) int {
	switch payload := toolResultBlock["content"].(type) {
	case string:
		return 1
	case []any:
		n := 0
		for _, innerRaw := range payload {
			inner, ok := innerRaw.(map[string]any)
			if !ok || inner["type"] != "text" {
				continue
			}
			if _, ok := inner["text"].(string); ok {
				n++
			}
		}
		return n
	}
	return 0
}

