package config

// PoliciesConfig defines named regex patterns for branch matching.
// git_tags has moved to versioning.tags — policies retains only branches
// until Phase 4 migrates them to matchers.
type PoliciesConfig struct {
	// Branches maps policy names to regex patterns for branch matching.
	// e.g., "main": "^main$"
	Branches map[string]string `yaml:"branches"`
}

// DefaultPoliciesConfig returns an empty policies config.
func DefaultPoliciesConfig() PoliciesConfig {
	return PoliciesConfig{
		Branches: map[string]string{},
	}
}
