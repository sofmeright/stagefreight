package dependency

import (
	"strings"

	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
)

// SkippedDep records a dependency that was not updated, with a reason.
type SkippedDep struct {
	Dep    freshness.Dependency
	Reason string
}

// autoUpdatableEcosystems defines which ecosystems can be automatically updated.
var autoUpdatableEcosystems = map[string]bool{
	freshness.EcosystemDockerImage: true,
	freshness.EcosystemGitHubRelease: true,
	freshness.EcosystemGoMod:       true,
	freshness.EcosystemCargo:       false,
	freshness.EcosystemNpm:         false,
	freshness.EcosystemAlpineAPK:   false,
	freshness.EcosystemDebianAPT:   false,
	freshness.EcosystemPip:         false,
}

// FilterUpdateCandidates separates deps into actionable candidates and skipped.
// Each skipped dep gets an explicit reason string.
func FilterUpdateCandidates(deps []freshness.Dependency, cfg UpdateConfig, trackedFiles map[string]bool) (candidates []freshness.Dependency, skipped []SkippedDep) {
	ecosystemFilter := make(map[string]bool, len(cfg.Ecosystems))
	for _, e := range cfg.Ecosystems {
		ecosystemFilter[e] = true
	}

	for _, dep := range deps {
		if reason := skipReason(dep, cfg, ecosystemFilter, trackedFiles); reason != "" {
			skipped = append(skipped, SkippedDep{Dep: dep, Reason: reason})
			continue
		}
		candidates = append(candidates, dep)
	}
	return
}

func skipReason(dep freshness.Dependency, cfg UpdateConfig, ecosystemFilter map[string]bool, trackedFiles map[string]bool) string {
	// Up to date
	if dep.Current == dep.Latest || dep.Latest == "" {
		return "up to date"
	}

	// Indirect dependency (go.mod // indirect)
	if dep.Indirect {
		return "indirect dependency"
	}

	// Ecosystem filter
	if len(ecosystemFilter) > 0 && !ecosystemFilter[dep.Ecosystem] {
		return "ecosystem filtered out"
	}

	// Ecosystem not auto-updatable
	if updatable, known := autoUpdatableEcosystems[dep.Ecosystem]; known && !updatable {
		return "ecosystem not auto-updatable"
	}

	// Security-only policy: skip deps without vulnerabilities
	if cfg.Policy == "security" && len(dep.Vulnerabilities) == 0 {
		return "no CVE (security-only policy)"
	}

	// File not tracked by git
	if trackedFiles != nil && !trackedFiles[dep.File] {
		return "file not tracked by git"
	}

	// Docker-image specific skips
	if dep.Ecosystem == freshness.EcosystemDockerImage {
		if reason := dockerImageSkipReason(dep); reason != "" {
			return reason
		}
	}

	return ""
}

func dockerImageSkipReason(dep freshness.Dependency) string {
	name := dep.Name

	// Digest-pinned images
	if strings.Contains(name, "@sha256:") {
		return "digest-pinned image"
	}

	// ARG-based dynamic base images
	if strings.ContainsAny(name, "$") {
		return "ARG-based dynamic base image"
	}

	// Determine tag: split on last : after the last /
	tag := extractTag(name)
	if tag == "" {
		return "untagged image"
	}
	if tag == "latest" {
		return "latest tag"
	}

	return ""
}

// extractTag extracts the tag portion from a Docker image reference.
// It splits on the last : after the last / to avoid host:port confusion.
func extractTag(image string) string {
	// Find the last /
	lastSlash := strings.LastIndex(image, "/")
	nameAndTag := image
	if lastSlash >= 0 {
		nameAndTag = image[lastSlash+1:]
	}

	// Find : in the portion after the last /
	colonIdx := strings.LastIndex(nameAndTag, ":")
	if colonIdx < 0 {
		return ""
	}
	return nameAndTag[colonIdx+1:]
}
