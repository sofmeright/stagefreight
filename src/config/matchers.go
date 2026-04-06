package config

// MatchersConfig defines reusable named patterns for matching branches
// (and, in the future, other dimensions like events or refs). Matchers are
// declarative pattern definitions ONLY — they do not carry behavior. They
// participate in versioning's execution graph via reference (branch_builds
// look up matchers by name) and in target condition resolution (when.branches
// look up matchers by name).
//
// Matchers are keyed lookups, not ordered lists — no entry implies priority
// over another. They are purely id → pattern associations, so a map is the
// right shape.
type MatchersConfig struct {
	// Branches maps matcher names to regex patterns for branch matching.
	// e.g., "main": "^main$"
	Branches map[string]string `yaml:"branches"`
}

// DefaultMatchersConfig returns an empty matchers config.
func DefaultMatchersConfig() MatchersConfig {
	return MatchersConfig{
		Branches: map[string]string{},
	}
}
