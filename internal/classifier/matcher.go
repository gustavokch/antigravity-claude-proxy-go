package classifier

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"antigravity-go-proxy/internal/config"
)

// compiledPattern is one condition test with its regex already compiled.
// Compiling at config-apply time rather than per request keeps the matcher
// off the hot path of every message the proxy forwards.
type compiledPattern struct {
	re     *regexp.Regexp
	substr string
}

func (p compiledPattern) match(text string) bool {
	if p.re != nil {
		return p.re.MatchString(text)
	}
	return p.substr != "" && strings.Contains(text, p.substr)
}

type compiledRule struct {
	rule   config.Rule
	system []compiledPattern
	footer []compiledPattern
}

// ConfigurableMatcher evaluates operator-defined rules against raw request
// bodies. It is safe for concurrent use: Match takes a read lock, and
// UpdateRules swaps a fully-built rule set under a write lock so a request in
// flight never sees a half-applied config.
type ConfigurableMatcher struct {
	mu       sync.RWMutex
	rules    []compiledRule
	backends map[string]config.TargetBackend
}

// NewConfigurableMatcher builds a matcher, returning an error if any rule
// carries a regex that does not compile.
func NewConfigurableMatcher(rules []config.Rule, backends map[string]config.TargetBackend) (*ConfigurableMatcher, error) {
	matcher := &ConfigurableMatcher{}
	if err := matcher.UpdateRules(rules, backends); err != nil {
		return nil, err
	}
	return matcher, nil
}

func compilePatterns(patterns []config.MatchPattern) ([]compiledPattern, error) {
	compiled := make([]compiledPattern, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern.Pattern == "" {
			continue
		}
		if pattern.Type == config.PatternRegex {
			re, err := regexp.Compile(pattern.Pattern)
			if err != nil {
				return nil, fmt.Errorf("classifier: invalid regex %q: %w", pattern.Pattern, err)
			}
			compiled = append(compiled, compiledPattern{re: re})
			continue
		}
		compiled = append(compiled, compiledPattern{substr: pattern.Pattern})
	}
	return compiled, nil
}

// UpdateRules replaces the rule set. On error nothing is swapped in, so a
// rejected config leaves the previously working rules active rather than
// silently disabling interception.
func (matcher *ConfigurableMatcher) UpdateRules(rules []config.Rule, backends map[string]config.TargetBackend) error {
	compiled := make([]compiledRule, 0, len(rules))
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		system, err := compilePatterns(rule.Conditions.SystemPromptPatterns)
		if err != nil {
			return err
		}
		footer, err := compilePatterns(rule.Conditions.FooterPatterns)
		if err != nil {
			return err
		}
		compiled = append(compiled, compiledRule{rule: rule, system: system, footer: footer})
	}

	copied := make(map[string]config.TargetBackend, len(backends))
	for name, backend := range backends {
		copied[name] = backend
	}

	matcher.mu.Lock()
	matcher.rules = compiled
	matcher.backends = copied
	matcher.mu.Unlock()
	return nil
}

// matchRequest is the minimal view of an Anthropic Messages request the rule
// conditions need. System and message content are RawMessage because Claude
// Code sends both the bare-string and the content-block forms.
type matchRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// Match returns the first enabled rule whose conditions all hold, plus its
// backend when the action is reroute. A reroute rule naming an unknown
// backend is skipped rather than matched, so a stale rule cannot black-hole
// traffic.
func (matcher *ConfigurableMatcher) Match(body []byte) (*config.Rule, *config.TargetBackend, bool) {
	matcher.mu.RLock()
	rules := matcher.rules
	backends := matcher.backends
	matcher.mu.RUnlock()

	if len(rules) == 0 {
		return nil, nil, false
	}

	var req matchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, false
	}

	systemText := flattenText(req.System)
	footerText := ""
	if len(req.Messages) > 0 {
		footerText = flattenText(req.Messages[len(req.Messages)-1].Content)
	}

	for _, candidate := range rules {
		if !matchesConditions(candidate, &req, systemText, footerText) {
			continue
		}
		rule := candidate.rule
		if rule.Action == config.RuleActionReroute {
			backend, ok := backends[rule.TargetBackend]
			if !ok {
				continue
			}
			return &rule, &backend, true
		}
		return &rule, nil, true
	}
	return nil, nil, false
}

func matchesConditions(candidate compiledRule, req *matchRequest, systemText, footerText string) bool {
	conditions := candidate.rule.Conditions
	if !anyMatch(candidate.system, systemText) {
		return false
	}
	if !anyMatch(candidate.footer, footerText) {
		return false
	}
	if conditions.MaxTokensMin > 0 && req.MaxTokens < conditions.MaxTokensMin {
		return false
	}
	if conditions.MaxTokensMax > 0 && req.MaxTokens > conditions.MaxTokensMax {
		return false
	}
	if len(conditions.Models) > 0 {
		found := false
		for _, model := range conditions.Models {
			if model == req.Model {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// anyMatch treats an empty pattern list as "no constraint", so an operator
// who fills in only the footer marker is not forced to invent a system regex.
func anyMatch(patterns []compiledPattern, text string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if pattern.match(text) {
			return true
		}
	}
	return false
}

// flattenText renders either wire form of a system prompt or message content
// as one string. Unparseable input yields "", which simply fails to match.
func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, block := range blocks {
		builder.WriteString(block.Text)
		builder.WriteString("\n")
	}
	return builder.String()
}
