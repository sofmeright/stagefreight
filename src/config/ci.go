package config

// CIConfig holds CI pipeline behavior configuration.
type CIConfig struct {
	// ArtifactExpiry controls how long CI job artifacts are retained.
	// Passed to the CI provider as SF_ARTIFACT_EXPIRY via dotenv artifact.
	// Format matches the provider's expiry syntax (e.g., "4 hours", "1 day", "1 week").
	// Empty string means artifacts never expire.
	ArtifactExpiry string `yaml:"artifact_expiry"`
}

// DefaultCIConfig returns the default CI configuration.
func DefaultCIConfig() CIConfig {
	return CIConfig{}
}
