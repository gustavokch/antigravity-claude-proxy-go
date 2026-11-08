package codex

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// The tool schema rules below are ported from CLIProxyAPI: normalizeToolParameters
// and stripDialectKeywordsFromSchema (translator/codex/claude) and
// NormalizeCodexToolSchemas (client/codex/tool-schema). Each removes a schema
// feature the Codex backend rejects, so a client's tool list does not fail the
// whole request. The client validates tool input itself, so dropping an
// upstream-side constraint never lets through a call the tool would refuse.

// constUnionThreshold is the branch count from which a oneOf/anyOf made only
// of constants becomes an enum. MCP servers emit such unions for enums with a
// description per value.
const constUnionThreshold = 8

// schemaMapKeywords hold a map of subschemas; schemaValueKeywords hold one
// subschema or a list of them.
var (
	schemaMapKeywords = []string{
		"properties", "$defs", "definitions", "patternProperties", "dependentSchemas", "dependencies",
	}
	schemaValueKeywords = []string{
		"items", "prefixItems", "contains", "additionalProperties", "propertyNames",
		"unevaluatedProperties", "unevaluatedItems", "additionalItems", "contentSchema",
		"anyOf", "oneOf", "allOf", "not", "if", "then", "else",
	}
	// numericBoundKeywords are rejected with HTTP 500 when they hold an
	// integer outside int64.
	numericBoundKeywords = []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"}
)

// toolParameters turns an Anthropic input_schema into a Codex function tool's
// parameters: a missing or non-object schema becomes an empty object schema,
// an object schema always carries properties, and the rules above are applied.
func toolParameters(schema any) map[string]any {
	root, ok := schema.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	root = cloneJSON(root).(map[string]any)
	sanitizeSchema(root)
	isObject := false
	switch t := root["type"].(type) {
	case nil:
		root["type"] = "object"
		isObject = true
	case string:
		if t == "" {
			root["type"] = "object"
		}
		isObject = t == "" || t == "object"
	case []any:
		for _, e := range t {
			if e == "object" {
				isObject = true
			}
		}
	}
	if isObject && root["properties"] == nil {
		root["properties"] = map[string]any{}
	}
	collapseConstUnions(root)
	return root
}

// cloneJSON deep-copies a decoded JSON value, so the schema rewrite does not
// reach into the caller's request.
func cloneJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = cloneJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneJSON(e)
		}
		return out
	}
	return v
}

// sanitizeSchema removes, at every depth, the dialect keywords $schema and $id,
// regex patterns the backend's validator cannot compile, and numeric bounds
// outside int64.
func sanitizeSchema(v any) {
	switch schema := v.(type) {
	case map[string]any:
		delete(schema, "$schema")
		delete(schema, "$id")
		if p, ok := schema["pattern"].(string); ok && unsupportedPattern(p) {
			delete(schema, "pattern")
		}
		for _, key := range numericBoundKeywords {
			if n, ok := schema[key].(json.Number); ok && boundOutsideInt64(string(n)) {
				delete(schema, key)
			}
		}
		if props, ok := schema["patternProperties"].(map[string]any); ok {
			for pattern := range props {
				if unsupportedPattern(pattern) {
					delete(props, pattern)
				}
			}
		}
		for _, key := range schemaMapKeywords {
			if sub, ok := schema[key].(map[string]any); ok {
				for _, s := range sub {
					sanitizeSchema(s)
				}
			}
		}
		for _, key := range schemaValueKeywords {
			sanitizeSchema(schema[key])
		}
	case []any:
		for _, item := range schema {
			sanitizeSchema(item)
		}
	}
}

// unsupportedPattern reports a regex the backend's validator rejects: a Unicode
// property escape (\p{…} or \P{…}), which Python's re module cannot compile,
// or the octal NUL escape \0, which validators that re-serialize the schema
// refuse (Claude Code's own Artifact tool sends "^[^\0]*$").
func unsupportedPattern(pattern string) bool {
	for i := 0; i < len(pattern)-1; i++ {
		if pattern[i] != '\\' {
			continue
		}
		next := pattern[i+1]
		if (next == 'p' || next == 'P') && i+2 < len(pattern) && pattern[i+2] == '{' {
			return true
		}
		if next == '0' {
			return true
		}
		i++ // skip the escaped character, an escaped backslash included
	}
	return false
}

// boundOutsideInt64 reports whether a JSON number is an integer outside int64.
// Fractions are never outside: the backend accepts them. The exponent form is
// evaluated without materialising the number, so a schema cannot make the
// proxy allocate a huge integer.
func boundOutsideInt64(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if !strings.ContainsAny(raw, ".eE") {
		_, err := strconv.ParseInt(raw, 10, 64)
		return errors.Is(err, strconv.ErrRange)
	}
	s, negative := raw, false
	switch {
	case strings.HasPrefix(s, "-"):
		s, negative = s[1:], true
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	mantissa, exp := s, int64(0)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]
		e, err := strconv.ParseInt(s[i+1:], 10, 64)
		if err != nil {
			// An exponent too large for int64 is out of range when positive
			// and the mantissa is not zero; anything else is malformed or zero.
			return errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(s[i+1:], "-") &&
				strings.Trim(mantissa, "0.") != ""
		}
		exp = e
	}
	intPart, fracPart, _ := strings.Cut(mantissa, ".")
	if intPart == "" && fracPart == "" || !allDigits(intPart) || !allDigits(fracPart) {
		return false
	}
	digits := strings.TrimLeft(intPart+fracPart, "0")
	if digits == "" {
		return false
	}
	if exp < -int64(len(raw)) {
		return false
	}
	if exp > int64(len(raw))+20 {
		return true
	}
	trimmed := strings.TrimRight(digits, "0")
	effective := exp - int64(len(fracPart)) + int64(len(digits)-len(trimmed))
	if effective < 0 {
		return false
	}
	if effective > 19 || int64(len(trimmed))+effective > 19 {
		return true
	}
	value := trimmed + strings.Repeat("0", int(effective))
	if negative {
		value = "-" + value
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return errors.Is(err, strconv.ErrRange)
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// collapseConstUnions rewrites each top-level property whose oneOf or anyOf
// (not both) has at least constUnionThreshold branches, every one of them a
// distinct constant with at most a title and description, into an enum. A
// property that already has an enum loses the union only when the two list the
// same values; otherwise it is left alone.
func collapseConstUnions(params map[string]any) {
	props, _ := params["properties"].(map[string]any)
	for _, raw := range props {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		_, hasOneOf := prop["oneOf"]
		_, hasAnyOf := prop["anyOf"]
		if hasOneOf == hasAnyOf {
			continue
		}
		key := "oneOf"
		if hasAnyOf {
			key = "anyOf"
		}
		branches, ok := prop[key].([]any)
		if !ok || len(branches) < constUnionThreshold {
			continue
		}
		values, keys, ok := constBranches(branches)
		if !ok {
			continue
		}
		if existing, ok := prop["enum"].([]any); ok {
			if sameValueSet(existing, keys) {
				delete(prop, key)
			}
			continue
		} else if _, present := prop["enum"]; present {
			continue
		}
		prop["enum"] = values
		delete(prop, key)
	}
}

// constBranches returns the constants of a union whose branches are all
// distinct constants, with their comparison keys.
func constBranches(branches []any) (values []any, keys map[string]bool, ok bool) {
	keys = make(map[string]bool, len(branches))
	for _, raw := range branches {
		branch, isMap := raw.(map[string]any)
		if !isMap {
			return nil, nil, false
		}
		value, has := branch["const"]
		if !has {
			return nil, nil, false
		}
		for k := range branch {
			if k != "const" && k != "description" && k != "title" {
				return nil, nil, false
			}
		}
		key, scalar := constKey(value)
		if !scalar || keys[key] {
			// A repeated value breaks oneOf's exclusivity; keep the schema.
			return nil, nil, false
		}
		keys[key] = true
		values = append(values, value)
	}
	return values, keys, true
}

// constKey is a comparison key for a scalar JSON value; numbers compare by
// value, so 1 and 1.0 are the same constant.
func constKey(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return "s:" + t, true
	case json.Number:
		if f, err := strconv.ParseFloat(string(t), 64); err == nil {
			return "n:" + strconv.FormatFloat(f, 'g', -1, 64), true
		}
		return "n:" + string(t), true
	case bool:
		return "b:" + strconv.FormatBool(t), true
	case nil:
		return "null", true
	}
	return "", false
}

func sameValueSet(values []any, keys map[string]bool) bool {
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		key, ok := constKey(v)
		if !ok || !keys[key] {
			return false
		}
		seen[key] = true
	}
	return len(seen) == len(keys) && len(values) == len(keys)
}
