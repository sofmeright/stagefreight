package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

const defaultConfigFile = ".stagefreight.yml"

// Config is the top-level StageFreight v2 configuration.
type Config struct {
	// Version must be 1. The pre-version config was an unversioned alpha
	// that never earned a schema number — this is the first stable schema.
	Version int `yaml:"version"`

	// Vars is a user-defined template variable dictionary.
	// Referenced as {var:name} anywhere templates are resolved.
	Vars map[string]string `yaml:"vars,omitempty"`

	// Defaults is inert YAML anchor storage. StageFreight ignores this
	// section entirely — it exists for users to define &anchors.
	Defaults yaml.Node `yaml:"defaults,omitempty"`

	// Sources defines build source configuration.
	Sources SourcesConfig `yaml:"sources"`

	// Policies defines named regex patterns for git tag and branch matching.
	Policies PoliciesConfig `yaml:"policies"`

	// Builds defines named build artifacts.
	Builds []BuildConfig `yaml:"builds"`

	// Targets defines distribution targets and side-effects.
	Targets []TargetConfig `yaml:"targets"`

	// Badges defines badge artifact generation (SVGs).
	// Badge system owns definitions; narrator references them via badge_ref.
	Badges []BadgeConfig `yaml:"badges"`

	// Narrator defines content composition for file targets.
	Narrator []NarratorFile `yaml:"narrator"`

	// Lint holds lint-specific configuration (unchanged from v1).
	Lint LintConfig `yaml:"lint"`

	// Security holds security scanning configuration (unchanged from v1).
	Security SecurityConfig `yaml:"security"`

	// Commit holds configuration for the commit subsystem.
	Commit CommitConfig `yaml:"commit"`

	// Dependency holds configuration for the dependency update subsystem.
	Dependency DependencyConfig `yaml:"dependency"`

	// Docs holds configuration for the docs generation subsystem.
	Docs DocsConfig `yaml:"docs"`

	// Manifest holds configuration for the manifest subsystem.
	Manifest ManifestConfig `yaml:"manifest"`

	// Release holds configuration for the release subsystem.
	Release ReleaseConfig `yaml:"release"`

	// Lifecycle defines the repository lifecycle mode (image, gitops).
	Lifecycle LifecycleConfig `yaml:"lifecycle"`

	// GitOps defines configuration for the gitops lifecycle mode.
	GitOps GitOpsConfig `yaml:"gitops"`

	// Docker defines configuration for the docker lifecycle mode.
	Docker DockerLifecycleConfig `yaml:"docker"`

	// Glossary defines the repo's shared change-language model.
	// Consumed by commit authoring, tag planning, and release rendering.
	Glossary GlossaryConfig `yaml:"glossary"`

	// Presentation defines surface-specific rendering policies.
	Presentation PresentationConfig `yaml:"presentation"`

	// Tag holds workflow defaults for the tag planner.
	Tag TagConfig `yaml:"tag"`
}

// Load reads configuration from a YAML file.
// If path is empty, it tries the default file.
// Returns sensible defaults if the file doesn't exist.
// Discards validation warnings; use LoadWithWarnings for full diagnostics.
func Load(path string) (*Config, error) {
	cfg, _, err := LoadWithWarnings(path)
	return cfg, err
}

// LoadWithWarnings reads configuration from a YAML file and returns
// validation warnings alongside the config.
func LoadWithWarnings(path string) (*Config, []string, error) {
	if path == "" {
		path = defaultConfigFile
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return defaults(), nil, nil
		}
		return nil, nil, err
	}

	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	warnings, verr := Validate(cfg)
	if verr != nil {
		return nil, warnings, fmt.Errorf("validating %s: %w", path, verr)
	}

	return cfg, warnings, nil
}

func defaults() *Config {
	return &Config{
		Version:    1,
		Vars:       map[string]string{},
		Sources:    DefaultSourcesConfig(),
		Policies:   DefaultPoliciesConfig(),
		Lint:       DefaultLintConfig(),
		Security:   DefaultSecurityConfig(),
		Commit:     DefaultCommitConfig(),
		Dependency: DefaultDependencyConfig(),
		Docs:       DefaultDocsConfig(),
		Manifest:     DefaultManifestConfig(),
		Release:      DefaultReleaseConfig(),
		GitOps:       DefaultGitOpsConfig(),
		Docker:       DefaultDockerLifecycleConfig(),
		Glossary:     DefaultGlossaryConfig(),
		Presentation: DefaultPresentationConfig(),
		Tag:          DefaultTagConfig(),
	}
}
