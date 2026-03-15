package config

// DependencyHandoff controls what happens when dependency repair creates a new commit.
//   - "continue"          — advisory only, current pipeline continues regardless
//   - "restart_pipeline"  — request pipeline rerun on repaired revision; downstream shipping stops
//   - "fail"              — fail hard if repair was needed but couldn't be handed off
type DependencyHandoff string

const (
	HandoffContinue        DependencyHandoff = "continue"
	HandoffRestartPipeline DependencyHandoff = "restart_pipeline"
	HandoffFail            DependencyHandoff = "fail"
)

// DependencyCIConfig controls CI-level behavior when deps creates a commit.
type DependencyCIConfig struct {
	Handoff DependencyHandoff `yaml:"handoff"` // default: continue
}

// DependencyConfig holds configuration for the dependency update subsystem.
type DependencyConfig struct {
	Enabled bool                   `yaml:"enabled"`
	Output  string                 `yaml:"output"`
	Scope   DependencyScopeConfig  `yaml:"scope"`
	Commit  DependencyCommitConfig `yaml:"commit"`
	CI      DependencyCIConfig     `yaml:"ci"`
}

// DependencyScopeConfig controls which dependency ecosystems are managed.
type DependencyScopeConfig struct {
	GoModules    bool `yaml:"go_modules"`
	DockerfileEnv bool `yaml:"dockerfile_env"` // umbrella for docker-image + github-release
}

// DependencyCommitConfig controls auto-commit behavior for dependency updates.
type DependencyCommitConfig struct {
	Enabled bool   `yaml:"enabled"`
	Type    string `yaml:"type"`
	Message string `yaml:"message"`
	Push    bool   `yaml:"push"`
	SkipCI  bool   `yaml:"skip_ci"`
}

// DefaultDependencyConfig returns sensible defaults for dependency management.
func DefaultDependencyConfig() DependencyConfig {
	return DependencyConfig{
		Enabled: true,
		Output:  ".stagefreight/deps",
		Scope: DependencyScopeConfig{
			GoModules:    true,
			DockerfileEnv: true,
		},
		Commit: DependencyCommitConfig{
			Enabled: true,
			Type:    "chore",
			Message: "update managed dependencies",
			Push:    true,
			SkipCI:  true,
		},
		CI: DependencyCIConfig{
			Handoff: HandoffContinue,
		},
	}
}

// ScopeToEcosystems converts scope booleans to ecosystem filter strings
// compatible with dependency.UpdateConfig.Ecosystems.
// Returns nil (all ecosystems) if all scopes are enabled.
func (s DependencyScopeConfig) ScopeToEcosystems() []string {
	if s.GoModules && s.DockerfileEnv {
		return nil // all
	}
	var ecosystems []string
	if s.GoModules {
		ecosystems = append(ecosystems, "gomod")
	}
	if s.DockerfileEnv {
		ecosystems = append(ecosystems, "docker-image", "github-release")
	}
	return ecosystems
}
