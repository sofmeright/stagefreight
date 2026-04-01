package governance

import (
	"bytes"
	"fmt"
	"time"
)

// PlanDistribution computes what files need to change for each governed repo.
// Pure planning — does NOT write anything.
// Reads current state from forge to detect drift and determine actions.
func PlanDistribution(
	gov *GovernanceConfig,
	presetLoader PresetLoader,
	skeleton []byte,
	auxFiles map[string][]byte, // e.g., ".claude/settings.json" → content
	forgeReader ForgeReader,
	sourceIdentity string, // e.g., "PrPlanIT/MaintenancePolicy"
	sourceRef string,
) ([]DistributionPlan, error) {

	var plans []DistributionPlan

	for _, cluster := range gov.Clusters {
		// Resolve presets in the cluster's stagefreight config.
		resolvedConfig, _, err := ResolvePresets(
			cluster.Config, presetLoader,
			sourceIdentity+"@"+sourceRef,
			"governance/clusters.yml",
			0, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: resolving presets: %w", cluster.ID, err)
		}

		// Render managed config.
		managed := ManagedConfig{
			Source:      sourceIdentity,
			Ref:         sourceRef,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			ClusterID:   cluster.ID,
			Config:      resolvedConfig,
		}

		managedContent, err := RenderManagedConfig(managed)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: rendering managed config: %w", cluster.ID, err)
		}

		for _, repo := range cluster.Targets.Repos {
			plan := DistributionPlan{Repo: repo}

			// Managed config file.
			plan.Files = append(plan.Files, planFile(
				forgeReader, repo,
				".stagefreight/stagefreight-managed.yml",
				managedContent,
			))

			// CI skeleton (if configured).
			if len(skeleton) > 0 {
				plan.Files = append(plan.Files, planFile(
					forgeReader, repo,
					".gitlab-ci.yml",
					skeleton,
				))
			}

			// Auxiliary files (claude-code settings, precommit, etc.).
			for path, content := range auxFiles {
				plan.Files = append(plan.Files, planFile(
					forgeReader, repo,
					path,
					content,
				))
			}

			plans = append(plans, plan)
		}
	}

	return plans, nil
}

// ForgeReader reads current file content from a remote repo.
// Used to detect drift and determine create vs update actions.
type ForgeReader interface {
	GetFileContent(repo, path, ref string) ([]byte, error)
}

// planFile determines the action for a single file in a target repo.
func planFile(reader ForgeReader, repo, path string, newContent []byte) DistributedFile {
	f := DistributedFile{
		Path:    path,
		Content: newContent,
	}

	if reader == nil {
		// No reader available — assume create.
		f.Action = "create"
		return f
	}

	existing, err := reader.GetFileContent(repo, path, "HEAD")
	if err != nil {
		// File doesn't exist — create.
		f.Action = "create"
		return f
	}

	if bytes.Equal(existing, newContent) {
		f.Action = "unchanged"
		return f
	}

	// File exists but differs.
	// For managed files, any difference is drift (machine-owned, never hand-edited).
	if path == ".stagefreight/stagefreight-managed.yml" {
		f.Action = "replace-drifted"
		f.Drifted = true
	} else {
		f.Action = "update"
	}

	return f
}

// HasChanges returns true if this plan has any files that need writing.
func (p DistributionPlan) HasChanges() bool {
	for _, f := range p.Files {
		if f.Action != "unchanged" {
			return true
		}
	}
	return false
}
