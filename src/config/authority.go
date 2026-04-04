package config

import (
	"strings"
)

// RunFromConfig gates mutation to declared execution origins.
// Attached to mutating surfaces (release, docs.commit, dependency.commit, targets).
type RunFromConfig struct {
	Allow    []string `yaml:"allow,omitempty"`    // permitted origins: "primary"
	Mismatch string   `yaml:"mismatch,omitempty"` // "read-only" (default), "exit", "ignore"
}

// RunFromResult is the evaluated outcome of a run_from guard.
type RunFromResult struct {
	Matched bool   // true if current context matches an allowed origin
	Mode    string // "read-only", "exit", "ignore", or "" (no restriction)
	Reason  string // human-readable explanation (empty when matched or unconfigured)
}

// EvaluateRunFrom checks whether the current execution context is authorized
// to perform a mutation guarded by a run_from config.
//
// ciRepoURL is the CI execution context (SF_CI_REPO_URL).
// primaryURL is sources.primary.url from config.
func EvaluateRunFrom(rf RunFromConfig, ciRepoURL, primaryURL string) RunFromResult {
	// No restriction = always proceed.
	if len(rf.Allow) == 0 {
		return RunFromResult{Matched: true}
	}

	for _, origin := range rf.Allow {
		switch strings.TrimSpace(strings.ToLower(origin)) {
		case "primary":
			if matchesPrimaryOrigin(ciRepoURL, primaryURL) {
				return RunFromResult{Matched: true}
			}
		}
	}

	// Mismatch — resolve mode.
	mode := strings.TrimSpace(strings.ToLower(rf.Mismatch))
	if mode == "" {
		mode = "read-only" // default: safest
	}

	return RunFromResult{
		Matched: false,
		Mode:    mode,
		Reason:  "not executing from allowed origin (allowed: " + strings.Join(rf.Allow, ", ") + ")",
	}
}

// IsActive returns true if run_from has any allow rules configured.
func (rf RunFromConfig) IsActive() bool {
	return len(rf.Allow) > 0
}

// matchesPrimaryOrigin checks if the CI repo URL matches the declared primary source.
func matchesPrimaryOrigin(ciRepoURL, primaryURL string) bool {
	if ciRepoURL == "" || primaryURL == "" {
		return false
	}
	return normalizeForgeURL(ciRepoURL) == normalizeForgeURL(primaryURL)
}

// normalizeForgeURL strips scheme, trailing slashes, and .git suffix for comparison.
func normalizeForgeURL(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "ssh://")
	u = strings.TrimPrefix(u, "git@")
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return strings.ToLower(u)
}
