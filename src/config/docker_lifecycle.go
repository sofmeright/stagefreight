package config

// DockerLifecycleConfig defines configuration for the docker lifecycle mode.
// This is Docker lifecycle orchestration, not docker-compose wrapping.
type DockerLifecycleConfig struct {
	// Backend selects the Docker execution engine (e.g. "compose").
	Backend string `yaml:"backend"`

	// Targets defines how reconciliation targets are resolved.
	Targets DockerTargetsConfig `yaml:"targets"`

	// IaC defines the Infrastructure as Code directory layout.
	IaC DockerIaCConfig `yaml:"iac"`

	// Secrets defines the secrets decryption provider.
	Secrets DockerSecretsConfig `yaml:"secrets"`

	// Drift defines drift detection and reconciliation policy.
	Drift DockerDriftPolicy `yaml:"drift"`
}

// DockerDriftPolicy configures drift detection behavior.
type DockerDriftPolicy struct {
	Tier2Action               string `yaml:"tier2_action"`                // report | reconcile (default: report)
	OrphanAction              string `yaml:"orphan_action"`               // report | down | prune (default: report)
	OrphanThreshold           int    `yaml:"orphan_threshold"`            // block if more than N orphans (default: 5)
	PruneRequiresConfirmation bool   `yaml:"prune_requires_confirmation"` // require --force for prune (default: true)
}

// DockerTargetsConfig defines target resolution for Docker reconciliation.
type DockerTargetsConfig struct {
	// Source is the inventory adapter (e.g. "ansible").
	Source string `yaml:"source"`

	// Inventory is the path to the inventory file (relative to repo root).
	Inventory string `yaml:"inventory"`

	// Selector declares which hosts from inventory are eligible.
	Selector DockerTargetSelector `yaml:"selector"`
}

// DockerTargetSelector declares eligibility via existing inventory groups.
// Group-based initially. Richer selectors deferred until groups become insufficient.
type DockerTargetSelector struct {
	Groups []string `yaml:"groups"`
}

// DockerIaCConfig defines the IaC directory layout.
type DockerIaCConfig struct {
	// Path is the IaC directory relative to repo root (default: "docker-compose").
	Path string `yaml:"path"`
}

// DockerSecretsConfig defines the secrets provider.
type DockerSecretsConfig struct {
	// Provider selects the secrets backend (e.g. "sops", "vault", "infisical").
	Provider string `yaml:"provider"`
}

// DefaultDockerLifecycleConfig returns sensible defaults.
func DefaultDockerLifecycleConfig() DockerLifecycleConfig {
	return DockerLifecycleConfig{
		Backend: "compose",
		Targets: DockerTargetsConfig{
			Source: "ansible",
		},
		IaC: DockerIaCConfig{
			Path: "docker-compose",
		},
		Secrets: DockerSecretsConfig{
			Provider: "sops",
		},
		Drift: DockerDriftPolicy{
			Tier2Action:               "report",
			OrphanAction:              "report",
			OrphanThreshold:           5,
			PruneRequiresConfirmation: true,
		},
	}
}
