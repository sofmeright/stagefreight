package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// MatchCondition evaluates a Condition against the current CI environment.
// Returns true if all present fields match (AND logic).
// Empty condition = catch-all (always true).
func MatchCondition(c Condition) bool {
	tag := os.Getenv("CI_COMMIT_TAG")
	branch := os.Getenv("CI_COMMIT_BRANCH")
	if branch == "" {
		branch = os.Getenv("GITHUB_REF_NAME")
	}

	return MatchConditionWith(c, tag, branch)
}

// MatchConditionWith evaluates a Condition against explicit tag/branch values.
// Use this when you already have the values resolved (e.g., from git detection).
func MatchConditionWith(c Condition, tag, branch string) bool {
	if c.Tag != "" {
		// Tag rule present but no tag in env → no match
		if tag == "" {
			return false
		}
		if !matchPattern(c.Tag, tag) {
			return false
		}
	}

	if c.Branch != "" {
		if !matchPattern(c.Branch, branch) {
			return false
		}
	}

	return true
}

// matchPattern evaluates a single pattern against a value.
//
// Syntax:
//
//	"^main$"         — regex match
//	"!^feature/.*"   — negated regex
//	"main"           — treated as regex (anchored or not, user decides)
//	"!develop"       — negated match
func matchPattern(pattern, value string) bool {
	negate := false
	if strings.HasPrefix(pattern, "!") {
		negate = true
		pattern = pattern[1:]
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		// Invalid regex → treat as literal
		matched := pattern == value
		if negate {
			return !matched
		}
		return matched
	}

	matched := re.MatchString(value)
	if negate {
		return !matched
	}
	return matched
}

// MatchPatterns evaluates a list of patterns against a value (OR logic).
// Any pattern matching means the value is allowed.
// Empty list = always allowed (no filter).
// Supports ! negation — a negated pattern excludes even if others include.
//
// Evaluation: exclude patterns (!) are checked first. If any exclude matches,
// the value is rejected. Then include patterns are checked — if any matches,
// the value is allowed. If only exclude patterns exist and none matched,
// the value is allowed (exclude-only = allowlist by negation).
func MatchPatterns(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}

	var includes []string
	var excludes []string
	for _, p := range patterns {
		if strings.HasPrefix(p, "!") {
			excludes = append(excludes, p[1:])
		} else {
			includes = append(includes, p)
		}
	}

	// Excludes checked first — any exclude match = rejected
	for _, p := range excludes {
		re, err := regexp.Compile(p)
		if err != nil {
			if p == value {
				return false
			}
			continue
		}
		if re.MatchString(value) {
			return false
		}
	}

	// No includes = exclude-only mode (everything not excluded is allowed)
	if len(includes) == 0 {
		return true
	}

	// Any include match = allowed
	for _, p := range includes {
		re, err := regexp.Compile(p)
		if err != nil {
			if p == value {
				return true
			}
			continue
		}
		if re.MatchString(value) {
			return true
		}
	}

	return false
}

// ── Policy-aware pattern matching ─────────────────────────────────────────

// identifierRe matches valid policy key names: letter-first, alphanumeric + _ . -
var identifierRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.\-]*$`)

// regexMetaChars are characters that indicate a string is an intentional regex, not a typo.
const regexMetaChars = `^$.*+?()[]{}|\`

// isIdentifier returns true if s looks like a policy name (letter-first identifier).
func isIdentifier(s string) bool {
	return identifierRe.MatchString(s)
}

// containsRegexMeta returns true if s contains any regex metacharacters.
func containsRegexMeta(s string) bool {
	for _, c := range s {
		if strings.ContainsRune(regexMetaChars, c) {
			return true
		}
	}
	return false
}

// CompiledPatterns holds pre-compiled include and exclude regex patterns.
// Avoids repeated regex compilation during planning and matching.
type CompiledPatterns struct {
	Include []*regexp.Regexp
	Exclude []*regexp.Regexp
}

// Match evaluates the compiled patterns against a value.
// Exclude-first semantics: if any exclude matches, rejected.
// Empty include list with no excludes = pass (no constraints).
// Empty include list with only excludes = everything not excluded passes.
func (cp *CompiledPatterns) Match(value string) bool {
	if cp == nil {
		return true
	}

	// Excludes checked first
	for _, re := range cp.Exclude {
		if re.MatchString(value) {
			return false
		}
	}

	// No includes = no positive filter (pass)
	if len(cp.Include) == 0 {
		return true
	}

	// Any include match = allowed
	for _, re := range cp.Include {
		if re.MatchString(value) {
			return true
		}
	}

	return false
}

// CompilePatterns resolves pattern tokens against a policy map and compiles
// them into include/exclude regex groups. Discards warnings.
func CompilePatterns(patterns []string, policyMap map[string]string) (*CompiledPatterns, error) {
	cp, _, err := CompilePatternsWithWarnings(patterns, policyMap)
	return cp, err
}

// CompilePatternsWithWarnings resolves pattern tokens against a policy map,
// compiles them into include/exclude regex groups, and returns any warnings
// (e.g., typo detection for unknown policy names).
func CompilePatternsWithWarnings(patterns []string, policyMap map[string]string) (*CompiledPatterns, []string, error) {
	if len(patterns) == 0 {
		return &CompiledPatterns{}, nil, nil
	}

	resolved := ResolvePatterns(patterns, policyMap)
	var warnings []string

	// Collect typo warnings from resolution
	for _, token := range patterns {
		raw := token
		if strings.HasPrefix(raw, "!") {
			raw = raw[1:]
		}
		_, warn := resolveTokenWithWarning(raw, policyMap)
		if warn != "" {
			warnings = append(warnings, warn)
		}
	}

	cp := &CompiledPatterns{}
	for _, p := range resolved {
		negate := false
		pat := p
		if strings.HasPrefix(pat, "!") {
			negate = true
			pat = pat[1:]
		}

		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, warnings, fmt.Errorf("invalid pattern %q: %w", pat, err)
		}

		if negate {
			cp.Exclude = append(cp.Exclude, re)
		} else {
			cp.Include = append(cp.Include, re)
		}
	}

	return cp, warnings, nil
}

// ResolvePatterns resolves named tokens in a pattern list to their regex
// values from the provided name→regex map (e.g. matchers.branches or a
// flattened tag_sources map). Direct regex patterns pass through unchanged.
// Negation prefix (!) is preserved.
func ResolvePatterns(patterns []string, patternMap map[string]string) []string {
	if len(patterns) == 0 {
		return nil
	}

	resolved := make([]string, 0, len(patterns))
	for _, token := range patterns {
		negate := false
		raw := token
		if strings.HasPrefix(raw, "!") {
			negate = true
			raw = raw[1:]
		}

		// Try to resolve as a named pattern
		val, _ := resolveTokenWithWarning(raw, patternMap)

		prefix := ""
		if negate {
			prefix = "!"
		}
		resolved = append(resolved, prefix+val)
	}
	return resolved
}

// resolveTokenWithWarning resolves a single token against the pattern map.
// Returns the resolved pattern and an optional warning string.
func resolveTokenWithWarning(token string, patternMap map[string]string) (string, string) {
	// If it's an identifier and exists in the map, resolve it
	if isIdentifier(token) {
		if regex, ok := patternMap[token]; ok {
			return regex, ""
		}
		// Identifier-like but not in pattern map
		if !containsRegexMeta(token) {
			// Looks like a typo — identifier-like, not in map, no metacharacters
			return token, fmt.Sprintf("unknown pattern name %q; treating as regex", token)
		}
	}

	// Not an identifier or contains metacharacters — pass through as regex
	return token, ""
}

// MatchPatternsWithPolicy resolves named patterns and evaluates them against a value.
// Convenience wrapper combining ResolvePatterns + MatchPatterns.
// (Name retained for backward compat with existing call sites.)
func MatchPatternsWithPolicy(patterns []string, value string, policyMap map[string]string) bool {
	if len(patterns) == 0 {
		return true
	}
	resolved := ResolvePatterns(patterns, policyMap)
	return MatchPatterns(resolved, value)
}
