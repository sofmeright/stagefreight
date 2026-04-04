package governance

import (
	"bytes"
	"fmt"
	"path"
	"strings"
)

// PlanDistribution computes what files need to change for each governed repo.
// Pure planning — does NOT write anything.
// Reads current state from forge to detect drift and determine actions.
// PresetSourceInfo holds the forge coordinates for preset resolution.
// Injected into satellite .stagefreight.yml so repos can resolve presets independently.
type PresetSourceInfo struct {
	Provider    string // "gitlab", "github", "gitea"
	ForgeURL    string // HTTPS base URL (e.g., "https://gitlab.prplanit.com")
	ProjectID   string // "org/repo" or "org/group/repo"
	Ref         string // pinned ref
	CachePolicy string // "authoritative" or "advisory"
}

// AssetFetcher fetches a file from a repo at a specific ref.
type AssetFetcher func(repoURL, ref, path string) ([]byte, error)

func PlanDistribution(
	gov *GovernanceConfig,
	presetLoader PresetLoader,
	assetFetcher AssetFetcher,
	forgeReader ForgeReader,
	presetSource PresetSourceInfo,
	sourceIdentity string, // for seal header display
) ([]DistributionPlan, error) {

	var plans []DistributionPlan

	for _, cluster := range gov.Clusters {
		// DO NOT resolve presets. Pass preset: references through as-is.
		// Presets are addresses of truth, not values to inject.
		// Add preset_source so satellites know where to resolve at runtime.
		config := addPresetSource(deepCopyMap(cluster.Config), presetSource)

		// Render sealed .stagefreight.yml preserving preset references.
		// Assets declared in the cluster config pass through as-is — they are the
		// ongoing authority reference for the satellite to re-sync from source.
		seal := SealMeta{
			SourceRepo: sourceIdentity,
			SourceRef:  presetSource.Ref,
			ClusterID:  cluster.ID,
		}

		sealedContent, err := RenderSealedConfig(seal, config)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: rendering sealed config: %w", cluster.ID, err)
		}

		// Collect preset files referenced in the cluster config for cache distribution.
		presetPaths := collectPresetPaths(cluster.Config)
		presetFiles := make(map[string][]byte)
		for _, p := range presetPaths {
			cachePath, err := sanitizePresetCachePath(p)
			if err != nil {
				return nil, fmt.Errorf("cluster %q: %w", cluster.ID, err)
			}
			data, err := presetLoader.Load(p)
			if err != nil {
				return nil, fmt.Errorf("cluster %q: loading preset %q for cache: %w", cluster.ID, p, err)
			}
			presetFiles[cachePath] = data
		}

		for _, repo := range cluster.Targets.Repos {
			plan := DistributionPlan{Repo: repo}

			// Sealed .stagefreight.yml — preset references preserved, not expanded.
			plan.Files = append(plan.Files, planFile(
				forgeReader, repo,
				".stagefreight.yml",
				sealedContent,
			))

			// Preset cache files — 1:1 copies for runtime resolution.
			for cachePath, cacheContent := range presetFiles {
				plan.Files = append(plan.Files, planFile(
					forgeReader, repo,
					cachePath,
					cacheContent,
				))
			}

			// Resolve declared assets from the cluster's stagefreight config.
			// Assets are fetched from their declared sources and materialized immediately.
			// The same asset declarations remain in the sealed config for ongoing sync.
			if assetFetcher != nil {
				if assetList, ok := cluster.Config["assets"].([]any); ok {
					for _, raw := range assetList {
						asset, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						target, _ := asset["target"].(string)
						source, _ := asset["source"].(map[string]any)
						if target == "" || source == nil {
							continue
						}
						repoURL, _ := source["repo_url"].(string)
						ref, _ := source["ref"].(string)
						srcPath, _ := source["path"].(string)
						if repoURL == "" || srcPath == "" {
							continue
						}
						if ref == "" {
							ref = "main"
						}
						content, err := assetFetcher(repoURL, ref, srcPath)
						if err != nil {
							return nil, fmt.Errorf("cluster %q: fetching asset %q from %s@%s:%s: %w",
								cluster.ID, target, repoURL, ref, srcPath, err)
						}
						plan.Files = append(plan.Files, planFile(
							forgeReader, repo,
							target,
							content,
						))
					}
				}
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

	// File exists but differs — governance replaces drifted files.
	f.Action = "replace"
	f.Drifted = true

	return f
}

// addPresetSource injects a preset_source block into the config so satellites
// know where to resolve presets at runtime independently of governance.
func addPresetSource(config map[string]any, ps PresetSourceInfo) map[string]any {
	out := make(map[string]any, len(config)+1)
	for k, v := range config {
		out[k] = v
	}
	out["preset_source"] = map[string]any{
		"provider":     ps.Provider,
		"repo_url":     ps.ForgeURL,
		"project_id":   ps.ProjectID,
		"ref":          ps.Ref,
		"cache_policy": ps.CachePolicy,
	}
	return out
}

// collectPresetPaths recursively walks a config and returns all unique preset: reference paths.
func collectPresetPaths(config map[string]any) []string {
	seen := map[string]struct{}{}
	var paths []string

	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if p, ok := t["preset"].(string); ok && p != "" {
				if _, dup := seen[p]; !dup {
					seen[p] = struct{}{}
					paths = append(paths, p)
				}
			}
			for _, v := range t {
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}

	walk(config)
	return paths
}

// sanitizePresetCachePath validates and sanitizes a preset path for cache storage.
func sanitizePresetCachePath(p string) (string, error) {
	clean := path.Clean(p)
	if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("preset path %q escapes cache directory", p)
	}
	return path.Join(".stagefreight/preset-cache", clean), nil
}

// deepCopyMap returns a deep copy of a map to prevent cross-cluster mutation.
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			out[k] = deepCopyMap(t)
		case []any:
			cp := make([]any, len(t))
			copy(cp, t)
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
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
