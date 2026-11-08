package codex

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// build runs BuildRequest on body and decodes the result.
func build(t *testing.T, body string, opts BuildOptions) (map[string]any, map[string]string) {
	t.Helper()
	if opts.Model == "" {
		opts.Model = "gpt-5.5"
	}
	out, names, err := BuildRequest([]byte(body), opts)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode output %s: %v", out, err)
	}
	return decoded, names
}

func inputItems(t *testing.T, req map[string]any) []map[string]any {
	t.Helper()
	raw, ok := req["input"].([]any)
	if !ok {
		t.Fatalf("input is %T, want an array", req["input"])
	}
	items := make([]map[string]any, len(raw))
	for i, r := range raw {
		items[i] = r.(map[string]any)
	}
	return items
}

// fernet returns a well-formed Codex encrypted_content value: version 0x80,
// a timestamp, a zero IV, blocks of ciphertext and a zero HMAC, base64url.
func fernet(blocks int) string {
	raw := make([]byte, 1+8+16+16*blocks+32)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], 1730000000)
	return base64.URLEncoding.EncodeToString(raw)
}

func TestBuildRequestFixedFieldsAndNeverSentFields(t *testing.T) {
	req, _ := build(t, `{
		"model": "claude-opus-4-1",
		"max_tokens": 5,
		"temperature": 0.2, "top_p": 0.9, "top_k": 40,
		"stop_sequences": ["END"],
		"metadata": {"user_id": "u"},
		"previous_response_id": "resp_1",
		"stream": false,
		"messages": [{"role": "user", "content": "hi"}]
	}`, BuildOptions{Model: "gpt-5.6-sol", PromptCacheKey: "key-1"})

	for _, field := range []string{"temperature", "top_p", "top_k", "stop_sequences", "metadata", "previous_response_id"} {
		if _, ok := req[field]; ok {
			t.Errorf("%s was sent; the Codex body never carries it", field)
		}
	}
	if req["model"] != "gpt-5.6-sol" || req["instructions"] != "" || req["stream"] != true || req["store"] != false {
		t.Errorf("model/instructions/stream/store = %v/%q/%v/%v", req["model"], req["instructions"], req["stream"], req["store"])
	}
	if !reflect.DeepEqual(req["include"], []any{"reasoning.encrypted_content"}) {
		t.Errorf("include = %v", req["include"])
	}
	if req["max_output_tokens"] != float64(16) {
		t.Errorf("max_output_tokens = %v, want 5 raised to the backend minimum 16", req["max_output_tokens"])
	}
	if req["prompt_cache_key"] != "key-1" {
		t.Errorf("prompt_cache_key = %v", req["prompt_cache_key"])
	}
	for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if _, ok := req[field]; ok {
			t.Errorf("%s sent on a request without tools", field)
		}
	}
}

func TestBuildRequestOutputBudgetBoundary(t *testing.T) {
	for _, tc := range []struct {
		body string
		want any
	}{
		{`{"max_tokens": 15}`, float64(16)},
		{`{"max_tokens": 16}`, float64(16)},
		{`{"max_tokens": 32000}`, float64(32000)},
		{`{"max_tokens": 0}`, float64(0)},
		{`{"max_output_tokens": 3}`, float64(16)},
	} {
		req, _ := build(t, tc.body, BuildOptions{})
		if req["max_output_tokens"] != tc.want {
			t.Errorf("%s: max_output_tokens = %v, want %v", tc.body, req["max_output_tokens"], tc.want)
		}
	}
}

func TestBuildRequestSystemBecomesOneDeveloperMessage(t *testing.T) {
	req, _ := build(t, `{
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1; cch=abc;"},
			{"type": "text", "text": "You are Claude Code."},
			{"type": "text", "text": ""},
			{"type": "text", "text": "Be brief."}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`, BuildOptions{})
	items := inputItems(t, req)
	want := map[string]any{
		"type": "message", "role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": "You are Claude Code."},
			map[string]any{"type": "input_text", "text": "Be brief."},
		},
	}
	if !reflect.DeepEqual(items[0], want) {
		t.Errorf("first item = %v, want %v", items[0], want)
	}
	if req["instructions"] != "" {
		t.Errorf("instructions = %q; the system prompt goes in the developer message", req["instructions"])
	}

	req, _ = build(t, `{"system": "plain system", "messages": []}`, BuildOptions{})
	if got := inputItems(t, req); len(got) != 1 || got[0]["role"] != "developer" {
		t.Errorf("string system: items = %v", got)
	}
}

func TestBuildRequestToolUseOrderAndResultAlignment(t *testing.T) {
	longID := "toolu_" + strings.Repeat("a", 80)
	req, _ := build(t, `{
		"tools": [{"name": "Bash", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}}}],
		"messages": [
			{"role": "user", "content": "run two"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Running."},
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"command": "ls"}},
				{"type": "tool_use", "id": "`+longID+`", "name": "Bash", "input": {"command": "pwd"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "`+longID+`", "content": "/home"},
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "a"}, {"type": "text", "text": "b"}], "is_error": true},
				{"type": "text", "text": "thanks"}
			]}
		]
	}`, BuildOptions{})
	items := inputItems(t, req)
	var kinds []string
	for _, it := range items {
		kinds = append(kinds, it["type"].(string)+":"+str(it["role"]))
	}
	wantKinds := []string{"message:user", "message:assistant", "function_call:", "function_call:", "function_call_output:", "function_call_output:", "message:user"}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("item order = %v, want %v", kinds, wantKinds)
	}
	if items[1]["content"].([]any)[0].(map[string]any)["type"] != "output_text" {
		t.Errorf("assistant text = %v, want output_text", items[1]["content"])
	}
	shortID := shortenCallID(longID)
	if len(shortID) != 64 || !strings.HasPrefix(shortID, longID[:47]+"_") {
		t.Fatalf("shortened call id %q: want 47 bytes of the id, '_' and 16 hex digits", shortID)
	}
	if items[2]["call_id"] != "toolu_1" || items[3]["call_id"] != shortID {
		t.Errorf("call ids = %v, %v", items[2]["call_id"], items[3]["call_id"])
	}
	if items[2]["arguments"] != `{"command":"ls"}` || items[2]["name"] != "Bash" {
		t.Errorf("function_call = %v", items[2])
	}
	// The results come back in call order, whatever order the client sent.
	if items[4]["call_id"] != "toolu_1" || items[5]["call_id"] != shortID {
		t.Errorf("result order = %v, %v; want aligned to the calls", items[4]["call_id"], items[5]["call_id"])
	}
	if items[4]["output"] != "Error: a\nb" {
		t.Errorf("error result output = %q, want texts joined and prefixed", items[4]["output"])
	}
	if items[5]["output"] != "/home" {
		t.Errorf("string result output = %q", items[5]["output"])
	}
	if req["parallel_tool_calls"] != true || req["tool_choice"] != "auto" {
		t.Errorf("parallel_tool_calls/tool_choice = %v/%v", req["parallel_tool_calls"], req["tool_choice"])
	}
}

func TestBuildRequestToolResultWithImageIsAPartList(t *testing.T) {
	req, _ := build(t, `{"messages": [
		{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {}}]},
		{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "is_error": true, "content": [
			{"type": "text", "text": "screenshot"},
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAA"}}
		]}]}
	]}`, BuildOptions{})
	items := inputItems(t, req)
	want := []any{
		map[string]any{"type": "input_text", "text": "Error: screenshot"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAA"},
	}
	if got := items[1]["output"]; !reflect.DeepEqual(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}
}

func TestBuildRequestImagesAndDocuments(t *testing.T) {
	req, _ := build(t, `{"messages": [{"role": "user", "content": [
		{"type": "image", "source": {"type": "base64", "data": "QUJD"}},
		{"type": "image", "source": {"type": "url", "url": "https://example.com/a.png"}},
		{"type": "image", "source": {"type": "url", "url": "ftp://example.com/a.png"}},
		{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "JVBE"}},
		{"type": "text", "text": "what is this"}
	]}]}`, BuildOptions{})
	items := inputItems(t, req)
	want := []any{
		map[string]any{"type": "input_image", "image_url": "data:application/octet-stream;base64,QUJD"},
		map[string]any{"type": "input_image", "image_url": "https://example.com/a.png"},
		map[string]any{"type": "input_file", "file_data": "data:application/pdf;base64,JVBE", "filename": "document.pdf"},
		map[string]any{"type": "input_text", "text": "what is this"},
	}
	if len(items) != 1 || !reflect.DeepEqual(items[0]["content"], want) {
		t.Errorf("items = %v, want one user message with %v", items, want)
	}
}

func TestBuildRequestRejectsAUserTurnLeftEmpty(t *testing.T) {
	_, _, err := BuildRequest([]byte(`{"messages": [
		{"role": "user", "content": "first"},
		{"role": "user", "content": [{"type": "document", "source": {"type": "text", "media_type": "text/plain", "data": "notes"}}]}
	]}`), BuildOptions{Model: "gpt-5.5"})
	if err == nil || !strings.Contains(err.Error(), "messages[1]") || !strings.Contains(err.Error(), "document") {
		t.Fatalf("err = %v, want the emptied turn and the dropped block type named", err)
	}

	// The same attachment beside text is dropped and the turn still goes out.
	if _, _, err := BuildRequest([]byte(`{"messages": [{"role": "user", "content": [
		{"type": "document", "source": {"type": "text", "data": "notes"}},
		{"type": "text", "text": "and this"}
	]}]}`), BuildOptions{Model: "gpt-5.5"}); err != nil {
		t.Errorf("turn with text beside the unsupported block: %v", err)
	}

	if _, _, err := BuildRequest([]byte(`[1,2]`), BuildOptions{}); err == nil {
		t.Error("a body that is not a JSON object must be refused")
	}
}

func TestBuildRequestThinkingSignatureReplay(t *testing.T) {
	codexSig := fernet(2)
	req, _ := build(t, `{"messages": [
		{"role": "user", "content": "q"},
		{"role": "assistant", "content": [
			{"type": "thinking", "thinking": "secret reasoning", "signature": "`+codexSig+`"},
			{"type": "text", "text": "answer"},
			{"type": "thinking", "thinking": "from claude", "signature": "EqQBCkgIBRABGAIiQL2s"},
			{"type": "thinking", "thinking": "unsigned", "signature": ""},
			{"type": "redacted_thinking", "data": "abc"}
		]},
		{"role": "user", "content": [{"type": "thinking", "thinking": "user side", "signature": "`+codexSig+`"}, {"type": "text", "text": "next"}]}
	]}`, BuildOptions{})
	items := inputItems(t, req)
	if len(items) != 4 {
		t.Fatalf("items = %v, want user, reasoning, assistant text, user", items)
	}
	want := map[string]any{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": codexSig}
	if !reflect.DeepEqual(items[1], want) {
		t.Errorf("reasoning item = %v, want %v", items[1], want)
	}
	out, _ := json.Marshal(items)
	for _, leaked := range []string{"secret reasoning", "from claude", "EqQBCkgI", "user side"} {
		if strings.Contains(string(out), leaked) {
			t.Errorf("%q reached the Codex input", leaked)
		}
	}
}

func TestIsCodexSignature(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  string
		want bool
	}{
		{"fernet one block", fernet(1), true},
		{"fernet unpadded", strings.TrimRight(fernet(3), "="), true},
		{"anthropic signature", "EqQBCkgIBRABGAIiQL2sAAAA", false},
		{"gemini signature", "CiQBVKhc7+5vAAAA", false},
		{"prefix only", "gAAAAAAA", false},
		{"ciphertext not whole blocks", base64.URLEncoding.EncodeToString(append([]byte(mustDecode(fernet(1))), 0)), false},
		{"payload too short", base64.URLEncoding.EncodeToString(append([]byte{0x80}, make([]byte, 60)...)), false},
		{"newline inside", fernet(1)[:20] + "\n" + fernet(1)[20:], false},
	} {
		if got := isCodexSignature(tc.sig); got != tc.want {
			t.Errorf("%s: isCodexSignature = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func mustDecode(s string) string {
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestBuildRequestEffortMappingAndClamping(t *testing.T) {
	full := ModelConfig{Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"}
	capped := ModelConfig{Efforts: []string{"medium", "high"}}
	for _, tc := range []struct {
		name  string
		body  string
		entry ModelConfig
		want  any // reasoning.effort, nil when absent
	}{
		{"output_config effort passes", `{"output_config":{"effort":"High"}}`, full, "high"},
		{"output_config wins over budget", `{"output_config":{"effort":"max"},"thinking":{"type":"enabled","budget_tokens":600}}`, full, "max"},
		{"unknown output_config effort ignored", `{"output_config":{"effort":"ultra"}}`, full, "low"},
		{"budget 512 is minimal, low without it", `{"thinking":{"type":"enabled","budget_tokens":512}}`, full, "low"},
		{"budget 1024 low", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, full, "low"},
		{"budget 1025 medium", `{"thinking":{"type":"enabled","budget_tokens":1025}}`, full, "medium"},
		{"budget 8192 medium", `{"thinking":{"type":"enabled","budget_tokens":8192}}`, full, "medium"},
		{"budget 24576 high", `{"thinking":{"type":"enabled","budget_tokens":24576}}`, full, "high"},
		{"budget above high is xhigh", `{"thinking":{"type":"enabled","budget_tokens":24577}}`, full, "xhigh"},
		{"disabled sends no effort", `{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, full, nil},
		{"nothing uses the entry default", `{}`, full, "low"},
		{"nothing and no default is medium", `{}`, ModelConfig{}, "medium"},
		{"clamped down to the highest published", `{"output_config":{"effort":"max"}}`, capped, "high"},
		{"below every published level takes the lowest", `{"output_config":{"effort":"low"}}`, capped, "medium"},
		{"no published levels pass through", `{"output_config":{"effort":"xhigh"}}`, ModelConfig{}, "xhigh"},
		{"minimal without published levels is low", `{"thinking":{"type":"enabled","budget_tokens":100}}`, ModelConfig{}, "low"},
	} {
		req, _ := build(t, tc.body, BuildOptions{Entry: tc.entry})
		var got any
		if r, ok := req["reasoning"].(map[string]any); ok {
			got = r["effort"]
		}
		if got != tc.want {
			t.Errorf("%s: effort = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildRequestReasoningSummary(t *testing.T) {
	for _, tc := range []struct {
		body string
		want any
	}{
		{`{"thinking":{"type":"enabled","budget_tokens":4000}}`, "auto"},
		{`{"thinking":{"type":"adaptive"}}`, "auto"},
		{`{"thinking":{"type":"adaptive","display":"summarized"}}`, "auto"},
		{`{"thinking":{"type":"adaptive","display":"omitted"}}`, nil},
		{`{"output_config":{"effort":"high"}}`, nil},
		{`{"thinking":{"type":"disabled"}}`, nil},
	} {
		req, _ := build(t, tc.body, BuildOptions{})
		var got any
		if r, ok := req["reasoning"].(map[string]any); ok {
			got = r["summary"]
		}
		if got != tc.want {
			t.Errorf("%s: summary = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestBuildRequestWebSearchTool(t *testing.T) {
	req, _ := build(t, `{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search", "allowed_domains": ["go.dev"], "user_location": {"type": "approximate", "country": "BR"}, "max_uses": 3},
			{"name": "Read", "input_schema": {"type": "object", "properties": {}}}
		],
		"tool_choice": {"type": "tool", "name": "web_search", "disable_parallel_tool_use": true},
		"messages": [{"role": "user", "content": "search"}]
	}`, BuildOptions{})
	tools := req["tools"].([]any)
	wantSearch := map[string]any{
		"type":          "web_search",
		"filters":       map[string]any{"allowed_domains": []any{"go.dev"}},
		"user_location": map[string]any{"type": "approximate", "country": "BR"},
	}
	if !reflect.DeepEqual(tools[0], wantSearch) {
		t.Errorf("web search tool = %v, want %v", tools[0], wantSearch)
	}
	if !reflect.DeepEqual(req["tool_choice"], map[string]any{"type": "web_search"}) {
		t.Errorf("tool_choice = %v", req["tool_choice"])
	}
	if !reflect.DeepEqual(req["include"], []any{"reasoning.encrypted_content", "web_search_call.action.sources"}) {
		t.Errorf("include = %v", req["include"])
	}
	if req["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false under disable_parallel_tool_use", req["parallel_tool_calls"])
	}
}

func TestBuildRequestToolChoice(t *testing.T) {
	for _, tc := range []struct {
		choice string
		want   any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
		{`{"type":"tool","name":"Read"}`, map[string]any{"type": "function", "name": "Read"}},
		{`{"type":"tool"}`, "auto"},
		{`"weird"`, "auto"},
	} {
		req, _ := build(t, `{"tools":[{"name":"Read","input_schema":{}}],"tool_choice":`+tc.choice+`}`, BuildOptions{})
		if !reflect.DeepEqual(req["tool_choice"], tc.want) {
			t.Errorf("%s: tool_choice = %v, want %v", tc.choice, req["tool_choice"], tc.want)
		}
	}
}

func TestBuildRequestShortensLongToolNamesAndMapsThemBack(t *testing.T) {
	server := "mcp__" + strings.Repeat("s", 70) + "__"
	readable := "mcp__github-12345678-9abc-def0-1234-56789abcdef0-12345678_add-issue-comment-with-a-long-name"
	long1 := server + "do_thing"
	long2 := strings.Repeat("x", 70)
	long3 := strings.Repeat("x", 70) + "y"
	req, names := build(t, `{
		"tools": [
			{"name": "`+long1+`", "input_schema": {}},
			{"name": "mcp__other__`+readable+`", "input_schema": {}},
			{"name": "`+long2+`", "input_schema": {}},
			{"name": "`+long3+`", "input_schema": {}},
			{"name": "Bash", "input_schema": {}}
		],
		"tool_choice": {"type": "tool", "name": "`+long3+`"},
		"messages": [{"role": "assistant", "content": [{"type": "tool_use", "id": "t", "name": "`+long1+`", "input": {}}]}]
	}`, BuildOptions{})
	var upstream []string
	for _, raw := range req["tools"].([]any) {
		upstream = append(upstream, raw.(map[string]any)["name"].(string))
	}
	want := []string{
		"mcp__do_thing",
		"mcp__github_add-issue-comment-with-a-long-name",
		strings.Repeat("x", 64),
		strings.Repeat("x", 62) + "_1",
		"Bash",
	}
	if !reflect.DeepEqual(upstream, want) {
		t.Fatalf("upstream names = %v, want %v", upstream, want)
	}
	wantMap := map[string]string{
		want[0]: long1,
		want[1]: "mcp__other__" + readable,
		want[2]: long2,
		want[3]: long3,
	}
	if !reflect.DeepEqual(names, wantMap) {
		t.Errorf("toolNames = %v, want %v", names, wantMap)
	}
	if call := inputItems(t, req)[0]; call["name"] != want[0] {
		t.Errorf("tool_use name = %v, want the declared tool's upstream name", call["name"])
	}
	if !reflect.DeepEqual(req["tool_choice"], map[string]any{"type": "function", "name": want[3]}) {
		t.Errorf("tool_choice = %v", req["tool_choice"])
	}

	if _, names := build(t, `{"tools":[{"name":"Bash","input_schema":{}}]}`, BuildOptions{}); names != nil {
		t.Errorf("toolNames = %v, want nil when nothing was renamed", names)
	}
}

func TestBuildRequestFunctionToolSchemas(t *testing.T) {
	branches := make([]string, 0, 8)
	for _, v := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		branches = append(branches, `{"const":"`+v+`","description":"value `+v+`"}`)
	}
	req, _ := build(t, `{"tools": [
		{"name": "Strict", "description": "d", "cache_control": {"type": "ephemeral"}, "defer_loading": true, "input_schema": {
			"$schema": "http://json-schema.org/draft-07/schema#",
			"type": "object",
			"properties": {
				"name": {"type": "string", "pattern": "^\\p{L}+$"},
				"nul": {"type": "string", "pattern": "^[^\\0]*$"},
				"ok": {"type": "string", "pattern": "^[a-z]+$"},
				"big": {"type": "integer", "maximum": 18446744073709551615, "minimum": 0},
				"exp": {"type": "number", "maximum": 1e30, "minimum": 1.5},
				"mode": {"oneOf": [`+strings.Join(branches, ",")+`]},
				"few": {"anyOf": [{"const": 1}, {"const": 2}]},
				"nested": {"type": "object", "properties": {"inner": {"type": "string", "pattern": "\\P{Lu}"}}}
			}
		}},
		{"type": "bash_20250124", "name": "bash"}
	]}`, BuildOptions{})
	tools := req["tools"].([]any)
	strict := tools[0].(map[string]any)
	for _, absent := range []string{"cache_control", "defer_loading", "input_schema"} {
		if _, ok := strict[absent]; ok {
			t.Errorf("function tool carries %s", absent)
		}
	}
	if strict["type"] != "function" || strict["strict"] != false || strict["description"] != "d" {
		t.Errorf("function tool = %v", strict)
	}
	params := strict["parameters"].(map[string]any)
	if _, ok := params["$schema"]; ok {
		t.Error("$schema kept")
	}
	props := params["properties"].(map[string]any)
	field := func(name string) map[string]any { return props[name].(map[string]any) }
	if _, ok := field("name")["pattern"]; ok {
		t.Error("\\p{…} pattern kept")
	}
	if _, ok := field("nul")["pattern"]; ok {
		t.Error("\\0 pattern kept")
	}
	if field("ok")["pattern"] != "^[a-z]+$" {
		t.Error("a supported pattern was removed")
	}
	if _, ok := field("big")["maximum"]; ok {
		t.Error("integer bound outside int64 kept")
	}
	if field("big")["minimum"] != float64(0) {
		t.Error("in-range bound removed")
	}
	if _, ok := field("exp")["maximum"]; ok {
		t.Error("exponent-form bound outside int64 kept")
	}
	if field("exp")["minimum"] != 1.5 {
		t.Error("fractional bound removed")
	}
	if _, ok := field("mode")["oneOf"]; ok || !reflect.DeepEqual(field("mode")["enum"], []any{"a", "b", "c", "d", "e", "f", "g", "h"}) {
		t.Errorf("8-branch constant oneOf = %v, want an enum", field("mode"))
	}
	if _, ok := field("few")["anyOf"]; !ok {
		t.Error("a short constant union was collapsed")
	}
	inner := field("nested")["properties"].(map[string]any)["inner"].(map[string]any)
	if _, ok := inner["pattern"]; ok {
		t.Error("nested \\P{…} pattern kept")
	}

	bash := tools[1].(map[string]any)
	if bash["type"] != "function" || !reflect.DeepEqual(bash["parameters"], map[string]any{"type": "object", "properties": map[string]any{}}) {
		t.Errorf("server tool without input_schema = %v, want a function with an empty object schema", bash)
	}
}

func TestBoundOutsideInt64(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"9223372036854775807", false},
		{"9223372036854775808", true},
		{"-9223372036854775808", false},
		{"-9223372036854775809", true},
		{"9.3e18", true},
		{"9.2e18", false},
		{"1.5", false},
		{"1e999999999999999999999", true},
		{"1e-999999999999999999999", false},
		{"0e999999999999999999999", false},
	} {
		if got := boundOutsideInt64(tc.raw); got != tc.want {
			t.Errorf("boundOutsideInt64(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestBuildRequestServiceTierAndTextFormat(t *testing.T) {
	req, _ := build(t, `{"speed": "fast", "output_config": {"format": {"type": "json_schema", "schema": {
		"type": "object", "properties": {"a": {"type": "string"}, "b": {"type": "string"}}, "required": ["a"]
	}}}}`, BuildOptions{})
	if req["service_tier"] != "priority" {
		t.Errorf("service_tier = %v, want priority for speed fast", req["service_tier"])
	}
	format := req["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != structuredOutputName || format["strict"] != false {
		t.Errorf("text.format = %v, want strict false when a property is not required", format)
	}

	req, _ = build(t, `{"service_tier": "Priority", "output_config": {"format": {"type": "json_schema", "name": "out", "schema": {
		"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]
	}}}}`, BuildOptions{})
	format = req["text"].(map[string]any)["format"].(map[string]any)
	if req["service_tier"] != "priority" || format["strict"] != true || format["name"] != "out" {
		t.Errorf("service_tier = %v, format = %v", req["service_tier"], format)
	}

	req, _ = build(t, `{"service_tier": "auto"}`, BuildOptions{})
	if _, ok := req["service_tier"]; ok {
		t.Error("service_tier auto must not ask for priority")
	}
}

func TestBuildRequestSystemMessageWaitsForToolResults(t *testing.T) {
	req, _ := build(t, `{"messages": [
		{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {}}]},
		{"role": "system", "content": [{"type": "text", "text": "# Environment"}]},
		{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}, {"type": "text", "text": "go on"}]},
		{"role": "system", "content": "trailing"}
	]}`, BuildOptions{})
	items := inputItems(t, req)
	var order []string
	for _, it := range items {
		switch it["type"] {
		case "message":
			order = append(order, it["content"].([]any)[0].(map[string]any)["text"].(string))
		default:
			order = append(order, it["type"].(string))
		}
	}
	want := []string{
		"function_call",
		"function_call_output",
		"<system-reminder>\n# Environment\n</system-reminder>",
		"go on",
		"<system-reminder>\ntrailing\n</system-reminder>",
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %q, want %q", order, want)
	}
	if items[2]["role"] != "user" {
		t.Errorf("system reminder role = %v, want user", items[2]["role"])
	}
}

func TestPromptCacheKey(t *testing.T) {
	// Expected values from Python's uuid.uuid5(uuid.NAMESPACE_OID, name).
	if got := PromptCacheKey("gpt-5.5", "sess-1", "main"); got != "be3dc11b-3c58-5d60-be7e-de758ade80f6" {
		t.Errorf("PromptCacheKey = %s", got)
	}
	if got := PromptCacheKey("gpt-5.5", "sess-1", "sub"); got != "34f6e034-4d41-5fe8-969c-483bea73460a" {
		t.Errorf("PromptCacheKey for another agent = %s", got)
	}
	if got := PromptCacheKey("gpt-5.5", "", "main"); got != "" {
		t.Errorf("PromptCacheKey without a session = %q, want empty", got)
	}
}
