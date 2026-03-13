package dependency

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/security"
)

// LoadAdvisories reads the security advisory bridge file.
// Returns empty slice + nil error if file doesn't exist (no advisories = normal).
func LoadAdvisories(rootDir string) ([]security.Advisory, error) {
	return security.ReadAdvisories(rootDir)
}

// FetchAdvisories attempts to download advisories from the latest successful
// security-scan job via the forge API. Writes the file to the standard location
// so subsequent LoadAdvisories calls find it.
// Returns the advisories, or nil+nil if unavailable (not an error).
func FetchAdvisories(ctx context.Context, fc forge.Forge, ref, rootDir string) ([]security.Advisory, error) {
	data, err := fc.DownloadJobArtifact(ctx, ref, "security-scan", ".stagefreight/security/advisories.json")
	if err != nil {
		// Expected absence or unsupported forge — quiet no-op.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, forge.ErrNotSupported) {
			return nil, nil
		}
		// Real failures (auth, server errors, network) — surface to caller.
		return nil, fmt.Errorf("downloading advisories artifact: %w", err)
	}

	// Write to standard location so LoadAdvisories finds it.
	dir := filepath.Join(rootDir, ".stagefreight", "security")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	outPath := filepath.Join(dir, "advisories.json")
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return nil, err
	}

	// Parse and return.
	return LoadAdvisories(rootDir)
}

// EnrichDependencies merges scanner advisories into resolved dependencies.
// Conservative matching: requires both ecosystem AND normalized package name match.
// Skips advisories with ecosystem "unknown" — no guessing.
// Returns count of enrichments added.
func EnrichDependencies(deps []freshness.Dependency, advisories []security.Advisory) int {
	if len(advisories) == 0 {
		return 0
	}

	// Build lookup: (normalized-name, ecosystem) → advisories
	type key struct {
		name      string
		ecosystem string
	}
	lookup := make(map[key][]security.Advisory)
	for _, adv := range advisories {
		if adv.Ecosystem == "unknown" {
			continue
		}
		// Map advisory ecosystem to freshness ecosystem constants.
		ecoKey := mapAdvisoryEcosystem(adv.Ecosystem)
		if ecoKey == "" {
			continue
		}
		k := key{
			name:      security.NormalizePackageName(adv.Package, adv.Ecosystem),
			ecosystem: ecoKey,
		}
		lookup[k] = append(lookup[k], adv)
	}

	if len(lookup) == 0 {
		return 0
	}

	// Deduplicate on (advisory ID + package + ecosystem + fixed_in).
	type dedup struct {
		id, pkg, eco, fix string
	}

	count := 0
	for i := range deps {
		dep := &deps[i]
		k := key{
			name:      security.NormalizePackageName(dep.Name, dep.Ecosystem),
			ecosystem: dep.Ecosystem,
		}
		matches, ok := lookup[k]
		if !ok {
			continue
		}

		// Track existing vulns to avoid duplicates.
		seen := make(map[dedup]bool)
		for _, v := range dep.Vulnerabilities {
			seen[dedup{v.ID, dep.Name, dep.Ecosystem, v.FixedIn}] = true
		}

		for _, adv := range matches {
			dk := dedup{adv.ID, adv.Package, adv.Ecosystem, adv.FixedIn}
			if seen[dk] {
				continue
			}
			seen[dk] = true

			dep.Vulnerabilities = append(dep.Vulnerabilities, freshness.VulnInfo{
				ID:       adv.ID,
				Summary:  "",
				Severity: adv.Severity,
				FixedIn:  adv.FixedIn,
				Source:   adv.Source,
			})
			count++
		}
	}

	return count
}

// mapAdvisoryEcosystem maps advisory ecosystem strings to freshness ecosystem constants.
func mapAdvisoryEcosystem(eco string) string {
	switch strings.ToLower(eco) {
	case "gomod":
		return freshness.EcosystemGoMod
	case "alpine-apk":
		return freshness.EcosystemAlpineAPK
	case "npm":
		return freshness.EcosystemNpm
	case "pip":
		return freshness.EcosystemPip
	case "cargo":
		return freshness.EcosystemCargo
	case "docker-image":
		return freshness.EcosystemDockerImage
	case "docker-tool":
		return freshness.EcosystemDockerTool
	case "debian-apt":
		return freshness.EcosystemDebianAPT
	default:
		return ""
	}
}
