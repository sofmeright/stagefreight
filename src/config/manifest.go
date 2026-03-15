package config

// ManifestConfig holds configuration for the manifest subsystem.
type ManifestConfig struct {
	// Enabled controls whether manifest generation is active (default: false).
	Enabled bool `yaml:"enabled"`

	// Mode controls where the manifest is stored.
	// ephemeral: temp location, use during run, discard after.
	// workspace: generate to .stagefreight/manifests/, don't auto-commit.
	// commit: generate and include in docs commit.
	// publish: generate and export as release asset / CI artifact.
	Mode string `yaml:"mode,omitempty"`

	// OutputDir is the output directory for manifest files.
	// Default: .stagefreight/manifests
	OutputDir string `yaml:"output_dir,omitempty"`
}

// DefaultManifestConfig returns sensible defaults for manifest generation.
func DefaultManifestConfig() ManifestConfig {
	return ManifestConfig{
		Enabled:   false,
		Mode:      "ephemeral",
		OutputDir: ".stagefreight/manifests",
	}
}

// validManifestModes enumerates all recognized manifest modes.
var validManifestModes = map[string]bool{
	"":          true, // default = ephemeral
	"ephemeral": true,
	"workspace": true,
	"commit":    true,
	"publish":   true,
}
