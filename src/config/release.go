package config

// ReleaseConfig holds configuration for the release subsystem.
type ReleaseConfig struct {
	Enabled         bool     `yaml:"enabled"`
	Required        *bool    `yaml:"required,omitempty"` // failure is hard pipeline fail (default: false)
	SecuritySummary string   `yaml:"security_summary"`
	RegistryLinks   bool     `yaml:"registry_links"`
	CatalogLinks    bool     `yaml:"catalog_links"`
	RunFrom         RunFromConfig `yaml:"run_from,omitempty"` // gate mutation to declared origin
}

// IsRequired returns whether release failure is a hard pipeline fail. Default: false.
func (r ReleaseConfig) IsRequired() bool {
	if r.Required != nil {
		return *r.Required
	}
	return false
}

// DefaultReleaseConfig returns sensible defaults for release management.
func DefaultReleaseConfig() ReleaseConfig {
	return ReleaseConfig{
		Enabled:         true,
		SecuritySummary: ".stagefreight/security",
		RegistryLinks:   true,
		CatalogLinks:    true,
	}
}
