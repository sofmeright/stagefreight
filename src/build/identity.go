package build

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Standard OCI label keys emitted by StageFreight on every build.
const (
	LabelCreated   = "org.opencontainers.image.created"
	LabelRevision  = "org.opencontainers.image.revision"
	LabelVersion   = "org.opencontainers.image.version"
	LabelBuildMode = "org.stagefreight.build.mode"
	LabelPlanHash  = "org.stagefreight.plan.sha256"
)

// StandardLabels returns the set of OCI labels that should be applied to
// every image built by StageFreight, regardless of build mode.
func StandardLabels(planHash, sfVersion, commit, mode, createdRFC3339 string) map[string]string {
	if createdRFC3339 == "" {
		createdRFC3339 = time.Now().UTC().Format(time.RFC3339)
	}
	labels := map[string]string{
		LabelCreated:  createdRFC3339,
		LabelPlanHash: planHash,
	}
	if sfVersion != "" {
		labels[LabelVersion] = sfVersion
	}
	if commit != "" {
		labels[LabelRevision] = commit
	}
	if mode == "" {
		mode = "standard"
	}
	labels[LabelBuildMode] = mode
	return labels
}

// InjectLabels merges labels into every step of a plan. Existing labels
// on a step are preserved; new labels do not overwrite.
func InjectLabels(plan *BuildPlan, labels map[string]string) {
	for i := range plan.Steps {
		if plan.Steps[i].Labels == nil {
			plan.Steps[i].Labels = make(map[string]string)
		}
		for k, v := range labels {
			if _, exists := plan.Steps[i].Labels[k]; !exists {
				plan.Steps[i].Labels[k] = v
			}
		}
	}
}

// NormalizeBuildPlan produces a deterministic fingerprint of a BuildPlan,
// excluding ephemeral/runtime-derived fields. Used globally for provenance,
// and by crucible for build graph verification between passes.
//
// Included fields (build-affecting):
//   - BuildStep.Name, Dockerfile, Context, Target (build identity)
//   - BuildStep.Platforms (affects output binary)
//   - BuildStep.BuildArgs (minus BUILD_DATE — ephemeral timestamp)
//
// Excluded fields (ephemeral or derived at runtime):
//   - BuildStep.Tags (output naming, not build-affecting)
//   - BuildStep.Registries (output destinations, not build-affecting)
//   - BuildStep.Output (always "image" for docker)
//   - BuildStep.Load, Push, SavePath (runtime strategy decisions)
//   - BuildStep.Labels (metadata, not build-affecting)
//   - BuildStep.Extract (artifact mode only)
//   - RegistryTarget.Credentials (auth, not build-affecting)
//   - RegistryTarget.Provider (inferred, not build-affecting)
//   - RegistryTarget.Retention, TagPatterns (post-build operations)
//   - BuildArgs["BUILD_DATE"] (timestamp, always differs between runs)
//   - Map iteration order (all maps sorted by key)
//   - Empty/zero-value fields (omitted, not hashed)
//   - Builder-generated metadata (layer IDs, cache keys, etc.)
func NormalizeBuildPlan(plan *BuildPlan) string {
	h := sha256.New()
	for _, step := range plan.Steps {
		fmt.Fprintf(h, "step:%s\n", step.Name)
		fmt.Fprintf(h, "dockerfile:%s\n", filepath.Clean(step.Dockerfile))
		fmt.Fprintf(h, "context:%s\n", filepath.Clean(step.Context))
		if step.Target != "" {
			fmt.Fprintf(h, "target:%s\n", step.Target)
		}

		platforms := make([]string, len(step.Platforms))
		copy(platforms, step.Platforms)
		sort.Strings(platforms)
		fmt.Fprintf(h, "platforms:%s\n", strings.Join(platforms, ","))

		// Sorted build args, excluding ephemeral keys
		argKeys := make([]string, 0, len(step.BuildArgs))
		for k := range step.BuildArgs {
			if k == "BUILD_DATE" {
				continue
			}
			argKeys = append(argKeys, k)
		}
		sort.Strings(argKeys)
		for _, k := range argKeys {
			fmt.Fprintf(h, "arg:%s=%s\n", k, step.BuildArgs[k])
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// EnvFingerprint returns an informational hash of the build environment.
// Non-authoritative — useful for debugging but never a primary signal.
func EnvFingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "os:%s\n", runtime.GOOS)
	fmt.Fprintf(h, "arch:%s\n", runtime.GOARCH)
	fmt.Fprintf(h, "go:%s\n", runtime.Version())
	return fmt.Sprintf("%x", h.Sum(nil))
}

// TruncHash truncates a hash string for display.
func TruncHash(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}
