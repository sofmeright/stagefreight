package config

import (
	"path/filepath"
	"strings"
)

// BuildConfig defines a named build artifact. Each build has a unique ID
// (referenced by targets) and a kind that determines which fields are valid.
//
// This is a discriminated union keyed by Kind — only fields relevant to the
// kind should be set. Validated at load time by v2 validation.
type BuildConfig struct {
	// ID is the unique identifier for this build, referenced by targets.
	ID string `yaml:"id"`

	// Kind is the build type. Determines which fields are valid.
	// Supported: "docker", "binary".
	Kind string `yaml:"kind"`

	// SelectTags enables CLI filtering via --select.
	SelectTags []string `yaml:"select_tags,omitempty"`

	// BuildMode controls the build execution strategy.
	// Supported: "" (standard), "crucible" (self-proving rebuild).
	BuildMode string `yaml:"build_mode,omitempty"`

	// DependsOn references another build ID that must complete before this one.
	// Enables build ordering: binary builds before docker builds that consume them.
	DependsOn string `yaml:"depends_on,omitempty"`

	// ── kind: docker ──────────────────────────────────────────────────────

	// Dockerfile is the path to the Dockerfile. Default: auto-detect.
	Dockerfile string `yaml:"dockerfile,omitempty"`

	// Context is the Docker build context path. Default: "." (repo root).
	Context string `yaml:"context,omitempty"`

	// Target is the --target stage name for multi-stage builds.
	Target string `yaml:"target,omitempty"`

	// Platforms lists the target platforms. Default: [linux/{current_arch}].
	Platforms []string `yaml:"platforms,omitempty"`

	// BuildArgs are key-value pairs passed as --build-arg. Supports templates.
	BuildArgs map[string]string `yaml:"build_args,omitempty"`

	// Cache holds build cache settings.
	Cache CacheConfig `yaml:"cache,omitempty"`

	// ── kind: binary ──────────────────────────────────────────────────────
	// Generic build schema: builder selects toolchain, args are raw vendor-native
	// arguments passed directly to the builder's command. No language-specific
	// config branches — one stable object model for all binary builders.

	// Builder is the toolchain that interprets the build.
	// Supported: "go". Future: "rust", "zig", "cargo".
	Builder string `yaml:"builder,omitempty"`

	// Command is the builder subcommand. e.g., "build" for "go build".
	// Default: "build".
	Command string `yaml:"command,omitempty"`

	// From is the source/input root or entry point.
	// e.g., "./src/cli" (Go package), "./src/main.rs" (Rust).
	From string `yaml:"from,omitempty"`

	// Output is the artifact name. Windows platforms auto-append ".exe".
	// Default: basename of From.
	Output string `yaml:"output,omitempty"`

	// Args are ordered raw arguments passed directly to the selected builder.
	// For Go: raw args to "go build". For Rust: raw args to "cargo build".
	// Supports template variables: {version}, {sha}, {sha:N}, {date}.
	Args []string `yaml:"args,omitempty"`

	// Env are build environment variables. e.g., {"CGO_ENABLED": "0"}
	Env map[string]string `yaml:"env,omitempty"`

	// Compress enables UPX compression on the output binary. Default: false.
	Compress bool `yaml:"compress,omitempty"`

	// Crucible holds crucible-specific configuration for binary builds.
	Crucible *CrucibleConfig `yaml:"crucible,omitempty"`
}

// CrucibleConfig holds crucible-specific build configuration.
type CrucibleConfig struct {
	// ToolchainImage is the pinned container image for pass-2 verification.
	// e.g., "docker.io/library/golang:1.24-alpine"
	ToolchainImage string `yaml:"toolchain_image,omitempty"`
}

// BuilderCommand returns the builder command, defaulting to "build".
func (b BuildConfig) BuilderCommand() string {
	if b.Command != "" {
		return b.Command
	}
	return "build"
}

// OutputName returns the output artifact name, defaulting to basename of From.
func (b BuildConfig) OutputName() string {
	if b.Output != "" {
		return b.Output
	}
	if b.From != "" {
		// Strip trailing .go/.rs suffixes, then take basename
		from := b.From
		for _, suffix := range []string{".go", ".rs"} {
			from = strings.TrimSuffix(from, suffix)
		}
		return filepath.Base(from)
	}
	return b.ID
}
