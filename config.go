package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/PrPlanIT/StageFreight/src/config/preset"
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

	// Forges declares git hosts. Each entry is a host identity (provider, URL, credentials).
	Forges []ForgeConfig `yaml:"forges,omitempty"`

	// Repos declares projects on forges. References forges by id. Has role (primary/mirror).
	Repos []RepoConfig `yaml:"repos,omitempty"`

	// Registries declares OCI registry hosts. Referenced by targets.
	Registries []RegistryConfig `yaml:"registries,omitempty"`

	// Versioning controls how version identity is derived from git state.
	Versioning VersioningConfig `yaml:"versioning"`

	// Matchers defines reusable named patterns for branches (and future
	// dimensions). Pattern definitions only — no behavior. Referenced by
	// branch_builds[].match and target.when.branches.
	Matchers MatchersConfig `yaml:"matchers"`

	// Builds defines named build artifacts.
	Builds []BuildConfig `yaml:"builds"`

	// Targets defines distribution targets and side-effects.
	Targets []TargetConfig `yaml:"targets"`

	// Badges defines badge artifact generation (SVGs).
	// Badge system owns definitions; narrator references them via badge_ref.
	// Artifact serving URL derived from publish-origin repo role.
	Badges BadgesConfig `yaml:"badges"`

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

	// Lifecycle defines the repository lifecycle mode (image, gitops, governance).
	Lifecycle LifecycleConfig `yaml:"lifecycle"`

	// Governance defines configuration for the governance lifecycle mode.
	// Only valid in the control repo (lifecycle.mode: governance).
	Governance GovernanceConfig `yaml:"governance"`

	// GitOps defines configuration for the gitops lifecycle mode.
	GitOps GitOpsConfig `yaml:"gitops"`

	// Docker defines configuration for the docker lifecycle mode.
	Docker DockerLifecycleConfig `yaml:"docker"`

	// BuildCache defines the build cache subsystem (local, shared, hybrid).
	BuildCache BuildCacheConfig `yaml:"build_cache"`

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

// loadResolved is the canonical config load implementation.
// Reads manifest → resolves presets → decodes struct → validates → normalizes.
// Returns resolved config, validation warnings, and preset merge entries.
// LoadWithWarnings and LoadWithReport are consumers of this function — neither
// owns config loading semantics.
// loadResolved is the ONLY entry point for constructing a runtime Config.
//
// All execution paths MUST go through this function to guarantee:
//   - preset resolution is applied before struct decode
//   - validation and normalization are always run
//   - a single source of truth exists for config state
//
// Do NOT construct Config via yaml.Unmarshal, yaml.NewDecoder, or any alternate
// loader. Doing so bypasses preset resolution and violates StageFreight invariants.
// See docs/invariants.md.
func loadResolved(path string) (*Config, []string, []preset.MergeEntry, error) {
	if path == "" {
		path = defaultConfigFile
	}

	absPath, _ := filepath.Abs(path)

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return defaults(), nil, nil, nil
		}
		return nil, nil, nil, err
	}

	var rawMap map[string]any
	if err := yaml.Unmarshal(data, &rawMap); err != nil {
		return nil, nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if rawMap == nil {
		return defaults(), nil, nil, nil
	}

	// Resolve presets before struct decode. All modalities (build, deps, docs,
	// security, release, reconcile, validate) execute against this resolved config.
	// Presets are a manifest-level feature — resolution belongs in the core load path.
	configDir := filepath.Dir(absPath)
	loader := preset.NewLocalLoader(configDir)
	resolvedMap, entries, resolveErr := preset.ResolvePresets(rawMap, loader, "local", absPath, 0, nil)

	var warnings []string
	if resolveErr != nil {
		// Resolution failed — fall back to raw map so execution can continue.
		// The warning surfaces in ConfigReport.Status for operator visibility.
		warnings = append(warnings, "preset resolution incomplete: "+resolveErr.Error())
		resolvedMap = rawMap
		entries = nil
	}

	// Re-marshal resolved map → decode into typed struct.
	// preset: directives are stripped by the resolver; what remains is merged config.
	resolvedData, err := yaml.Marshal(resolvedMap)
	if err != nil {
		return nil, warnings, entries, fmt.Errorf("re-encoding resolved config: %w", err)
	}

	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(resolvedData))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, warnings, entries, fmt.Errorf("parsing %s: %w", path, err)
	}

	valWarnings, verr := Validate(cfg)
	warnings = append(warnings, valWarnings...)
	if verr != nil {
		return nil, warnings, entries, fmt.Errorf("validating %s: %w", path, verr)
	}

	if err := Normalize(cfg); err != nil {
		return nil, warnings, entries, fmt.Errorf("normalizing %s: %w", path, err)
	}

	if err := AssertNormalized(cfg); err != nil {
		return nil, warnings, entries, fmt.Errorf("normalizing %s: %w", path, err)
	}

	return cfg, warnings, entries, nil
}

// LoadWithWarnings is a thin wrapper over loadResolved for callers that need
// warnings but not the full ConfigReport. It MUST NOT introduce any alternate
// config loading behavior — all resolution logic lives in loadResolved.
func LoadWithWarnings(path string) (*Config, []string, error) {
	cfg, warnings, _, err := loadResolved(path)
	return cfg, warnings, err
}

func defaults() *Config {
	return &Config{
		Version:    1,
		Vars:       map[string]string{},
		Versioning: DefaultVersioningConfig(),
		Matchers:   DefaultMatchersConfig(),
		Lint:       DefaultLintConfig(),
		Security:   DefaultSecurityConfig(),
		Commit:     DefaultCommitConfig(),
		Dependency: DefaultDependencyConfig(),
		Docs:       DefaultDocsConfig(),
		Manifest:     DefaultManifestConfig(),
		Release:      DefaultReleaseConfig(),
		GitOps:       DefaultGitOpsConfig(),
		BuildCache:   DefaultBuildCacheConfig(),
		Docker:       DefaultDockerLifecycleConfig(),
		Glossary:     DefaultGlossaryConfig(),
		Presentation: DefaultPresentationConfig(),
		Tag:          DefaultTagConfig(),
	}
}
