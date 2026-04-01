package governance

import (
	"os"
	"path/filepath"
)

// DetectCapabilities scans the repo filesystem for evidence of each capability domain.
// Independent per capability. No global winner. No config mutation.
func DetectCapabilities(repoRoot string) DetectionReport {
	var caps []CapabilityResult

	caps = append(caps, detectDocker(repoRoot))
	caps = append(caps, detectBinary(repoRoot))
	caps = append(caps, detectHelm(repoRoot))
	caps = append(caps, detectGitOps(repoRoot))
	caps = append(caps, detectAnsible(repoRoot))

	return DetectionReport{Capabilities: caps}
}

// GateExecution combines merged config + detected capabilities into a runnable plan.
// Does NOT modify config. Produces ExecutionPlan only.
func GateExecution(config map[string]any, detection DetectionReport) ExecutionPlan {
	var plan ExecutionPlan

	capMap := make(map[string]bool)
	for _, c := range detection.Capabilities {
		if c.Detected {
			capMap[c.Domain] = true
		}
	}

	// Check each domain that config declares.
	domains := []struct {
		domain    string
		configKey string
	}{
		{"build.docker", "builds"},  // docker builds are in builds[] with kind: docker
		{"build.binary", "builds"},  // binary builds are in builds[] with kind: binary
		{"package.helm", "targets"}, // helm targets
		{"deploy.gitops", "gitops"},
		{"deploy.ansible", ""},
		{"security", "security"},
		{"docs", "docs"},
		{"release", "release"},
		{"dependency", "dependency"},
	}

	for _, d := range domains {
		enabled := isConfigEnabled(config, d.configKey)
		detected := capMap[d.domain]

		// Some domains don't need filesystem evidence (security, docs, release, dependency).
		alwaysRunnable := d.domain == "security" || d.domain == "docs" ||
			d.domain == "release" || d.domain == "dependency"

		if enabled && (detected || alwaysRunnable) {
			plan.Enabled = append(plan.Enabled, EnabledFeature{
				Domain: d.domain,
				Reason: gateReason(detected, alwaysRunnable),
			})
		} else if enabled && !detected {
			plan.Skipped = append(plan.Skipped, SkippedFeature{
				Domain: d.domain,
				Reason: "capability not detected",
			})
		} else if !enabled {
			plan.Skipped = append(plan.Skipped, SkippedFeature{
				Domain: d.domain,
				Reason: "not configured",
			})
		}
	}

	return plan
}

func gateReason(detected, alwaysRunnable bool) string {
	if detected {
		return "config enabled + capability detected"
	}
	if alwaysRunnable {
		return "config enabled (no capability check needed)"
	}
	return "config enabled"
}

func isConfigEnabled(config map[string]any, key string) bool {
	if key == "" {
		return false
	}
	val, ok := config[key]
	if !ok {
		return false
	}

	// Check for explicit "enabled: false".
	if m, isMap := val.(map[string]any); isMap {
		if enabled, hasEnabled := m["enabled"]; hasEnabled {
			if b, isBool := enabled.(bool); isBool {
				return b
			}
		}
		// Map exists without explicit enabled = implicitly enabled.
		return true
	}

	// Lists (builds, targets) — enabled if non-empty.
	if list, isList := val.([]any); isList {
		return len(list) > 0
	}

	return false
}

// --- Individual capability detectors ---

func detectDocker(root string) CapabilityResult {
	r := CapabilityResult{Domain: "build.docker"}

	if exists(filepath.Join(root, "Dockerfile")) {
		r.Detected = true
		r.Confidence = "high"
		r.Evidence = append(r.Evidence, "Dockerfile")
	}
	if exists(filepath.Join(root, "docker-compose.yml")) || exists(filepath.Join(root, "docker-compose.yaml")) {
		r.Evidence = append(r.Evidence, "docker-compose.yml")
		if !r.Detected {
			r.Detected = true
			r.Confidence = "medium"
		}
	}

	if !r.Detected {
		r.Confidence = "none"
	}
	return r
}

func detectBinary(root string) CapabilityResult {
	r := CapabilityResult{Domain: "build.binary"}

	if exists(filepath.Join(root, "go.mod")) {
		r.Evidence = append(r.Evidence, "go.mod")
		r.Detected = true
		r.Confidence = "medium"
	}
	if exists(filepath.Join(root, "cmd")) {
		r.Evidence = append(r.Evidence, "cmd/")
		r.Confidence = "high"
	}
	if exists(filepath.Join(root, "Cargo.toml")) {
		r.Evidence = append(r.Evidence, "Cargo.toml")
		r.Detected = true
		r.Confidence = "high"
	}

	if !r.Detected {
		r.Confidence = "none"
	}
	return r
}

func detectHelm(root string) CapabilityResult {
	r := CapabilityResult{Domain: "package.helm"}

	if exists(filepath.Join(root, "Chart.yaml")) {
		r.Detected = true
		r.Confidence = "high"
		r.Evidence = append(r.Evidence, "Chart.yaml")
	}
	// Check charts/ subdirectory.
	if exists(filepath.Join(root, "charts")) {
		r.Evidence = append(r.Evidence, "charts/")
		if !r.Detected {
			r.Detected = true
			r.Confidence = "medium"
		}
	}

	if !r.Detected {
		r.Confidence = "none"
	}
	return r
}

func detectGitOps(root string) CapabilityResult {
	r := CapabilityResult{Domain: "deploy.gitops"}

	gitopsSignals := []string{
		"fluxcd", "clusters", "infrastructure",
	}
	for _, dir := range gitopsSignals {
		if exists(filepath.Join(root, dir)) {
			r.Evidence = append(r.Evidence, dir+"/")
			r.Detected = true
		}
	}

	// Kustomization files.
	if exists(filepath.Join(root, "kustomization.yaml")) || exists(filepath.Join(root, "kustomization.yml")) {
		r.Evidence = append(r.Evidence, "kustomization.yaml")
		r.Detected = true
	}

	if r.Detected {
		r.Confidence = "high"
	} else {
		r.Confidence = "none"
	}
	return r
}

func detectAnsible(root string) CapabilityResult {
	r := CapabilityResult{Domain: "deploy.ansible"}

	ansibleSignals := []string{
		"roles", "playbooks", "inventory",
	}
	for _, dir := range ansibleSignals {
		if exists(filepath.Join(root, dir)) {
			r.Evidence = append(r.Evidence, dir+"/")
			r.Detected = true
		}
	}

	if exists(filepath.Join(root, "ansible.cfg")) {
		r.Evidence = append(r.Evidence, "ansible.cfg")
		r.Detected = true
	}

	if r.Detected {
		r.Confidence = "high"
	} else {
		r.Confidence = "none"
	}
	return r
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
