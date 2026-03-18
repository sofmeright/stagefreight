package build

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Crucible environment variables.
const (
	// CrucibleEnvVar is set to "1" inside the pass-2 container to prevent recursion.
	CrucibleEnvVar = "STAGEFREIGHT_CRUCIBLE"

	// CrucibleRunIDEnvVar correlates pass-1 and pass-2 in logs/artifacts.
	CrucibleRunIDEnvVar = "STAGEFREIGHT_CRUCIBLE_RUN_ID"

	// CrucibleAllowEnvVar overrides the repo guard for non-StageFreight repos.
	CrucibleAllowEnvVar = "STAGEFREIGHT_ALLOW_CRUCIBLE"

	// StageFreightModule is the canonical Go module path used by the repo guard.
	StageFreightModule = "github.com/PrPlanIT/StageFreight"
)

// Trust levels, ordered from weakest to strongest.
const (
	TrustViable        = "viable"        // pass 2 succeeded
	TrustConsistent    = "consistent"    // version + build graph match
	TrustDeterministic = "deterministic" // binary hash identical
	TrustReproducible  = "reproducible"  // image digest identical (stretch goal)
)

// IsCrucibleChild returns true when running inside a crucible pass-2 container.
func IsCrucibleChild() bool {
	return os.Getenv(CrucibleEnvVar) == "1"
}

// EnsureCrucibleAllowed checks that the repo is the StageFreight repo itself,
// or that STAGEFREIGHT_ALLOW_CRUCIBLE=1 is set.
func EnsureCrucibleAllowed(rootDir string) error {
	if os.Getenv(CrucibleAllowEnvVar) == "1" {
		return nil
	}

	goMod := filepath.Join(rootDir, "go.mod")
	data, err := os.ReadFile(goMod)
	if err != nil {
		return fmt.Errorf("crucible: cannot read go.mod: %w\n  set %s=1 to override the repo guard", err, CrucibleAllowEnvVar)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			mod := strings.TrimSpace(strings.TrimPrefix(line, "module"))
			if mod == StageFreightModule {
				return nil
			}
			return fmt.Errorf("crucible: module %q is not %s\n  set %s=1 to override the repo guard",
				mod, StageFreightModule, CrucibleAllowEnvVar)
		}
	}

	return fmt.Errorf("crucible: no module directive found in go.mod\n  set %s=1 to override the repo guard", CrucibleAllowEnvVar)
}

// GenerateCrucibleRunID returns a cryptographically random hex string for
// correlating crucible passes. Uses crypto/rand to avoid collisions in CI
// where multiple pipelines may start at the same nanosecond.
func GenerateCrucibleRunID() string {
	b := make([]byte, 6) // 48 bits → 12 hex chars
	if _, err := rand.Read(b); err != nil {
		return "000000000000"
	}
	return fmt.Sprintf("%x", b)
}

// TrustLevelLabel returns human text for the trust level with context when
// the level is below deterministic.
func TrustLevelLabel(level string) string {
	switch level {
	case TrustDeterministic:
		return "deterministic (binary identical)"
	case TrustReproducible:
		return "reproducible (image identical)"
	case TrustConsistent:
		return "consistent (binary mismatch)"
	case TrustViable:
		return "viable (determinism not achieved)"
	default:
		return level
	}
}
