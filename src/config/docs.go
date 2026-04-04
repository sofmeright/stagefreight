package config

// DocsConfig holds configuration for the docs generation subsystem.
type DocsConfig struct {
	Enabled    bool                `yaml:"enabled"`
	Generators DocsGeneratorsConfig `yaml:"generators"`
	Commit     DocsCommitConfig    `yaml:"commit"`
}

// DocsGeneratorsConfig controls which documentation generators are enabled.
type DocsGeneratorsConfig struct {
	Badges       bool `yaml:"badges"`
	ReferenceDocs bool `yaml:"reference_docs"`
	Narrator     bool `yaml:"narrator"`
	DockerReadme bool `yaml:"docker_readme"`
}

// DocsCommitConfig controls auto-commit behavior for generated docs.
type DocsCommitConfig struct {
	Enabled bool     `yaml:"enabled"`
	Type    string   `yaml:"type"`
	Message string   `yaml:"message"`
	Add     []string `yaml:"add"`
	Push    bool     `yaml:"push"`
	SkipCI  bool     `yaml:"skip_ci"`
	RunFrom RunFromConfig `yaml:"run_from,omitempty"` // gate mutation to declared origin
}

// DefaultDocsConfig returns sensible defaults for docs generation.
func DefaultDocsConfig() DocsConfig {
	return DocsConfig{
		Enabled: true,
		Generators: DocsGeneratorsConfig{
			Badges:       true,
			ReferenceDocs: true,
			Narrator:     true,
			DockerReadme: true,
		},
		Commit: DocsCommitConfig{
			Enabled: true,
			Type:    "docs",
			Message: "refresh generated docs and badges",
			Add: []string{
				"README.md",
				"docs/modules",
				"docs/reference",
				"docs/Component.md",
				".stagefreight/badges",
			},
			Push:   true,
			SkipCI: false,
		},
	}
}
