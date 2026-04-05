package governance

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
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
		if err := cluster.Targets.ValidateTargets(); err != nil {
			return nil, fmt.Errorf("cluster %q: %w", cluster.ID, err)
		}

		// Resolve vars preset if present — vars are resolved at governance time,
		// not passed through as references. This produces concrete org-level vars.
		baseConfig := deepCopyMap(cluster.Config)
		resolveVarsPreset(baseConfig, presetLoader)

		// Add preset_source so satellites know where to resolve section presets at runtime.
		// Section presets (targets, badges, etc.) pass through as-is — addresses of truth.
		baseConfig = addPresetSource(baseConfig, presetSource)

		seal := SealMeta{
			SourceRepo: sourceIdentity,
			SourceRef:  presetSource.Ref,
			ClusterID:  cluster.ID,
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

		// Per-repo: merge satellite-owned vars, render sealed config, plan files.
		// Sealed content is per-repo because each satellite may have different local vars.
		for _, resolved := range cluster.Targets.AllRepos() {
			repo := resolved.ID
			plan := DistributionPlan{Repo: repo, Credentials: cluster.Targets.Credentials}

			// Merge satellite-owned vars into governance vars.
			// Governance keys are authoritative. Undeclared satellite keys are preserved.
			repoConfig := deepCopyMap(baseConfig)
			mergeSatelliteVars(repoConfig, forgeReader, repo)

			sealedContent, err := RenderSealedConfig(seal, repoConfig)
			if err != nil {
				return nil, fmt.Errorf("cluster %q repo %q: rendering sealed config: %w", cluster.ID, repo, err)
			}

			// Sealed .stagefreight.yml — section presets preserved, vars resolved.
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

// resolveVarsPreset resolves a vars preset reference into concrete values.
// If config["vars"] is a map with a "preset" key, loads the preset file,
// parses its vars section, and replaces the reference with concrete values.
// Vars presets are resolved at governance time — they are not passed through.
func resolveVarsPreset(config map[string]any, loader PresetLoader) {
	varsRaw, ok := config["vars"]
	if !ok {
		return
	}
	varsMap, ok := varsRaw.(map[string]any)
	if !ok {
		return
	}
	presetPath, ok := varsMap["preset"].(string)
	if !ok || presetPath == "" {
		return
	}

	data, err := loader.Load(presetPath)
	if err != nil {
		return // preset not found — leave vars as-is
	}

	var parsed struct {
		Vars map[string]any `yaml:"vars"`
	}
	if yaml.Unmarshal(data, &parsed) != nil || parsed.Vars == nil {
		return
	}

	// Replace the preset reference with resolved concrete values.
	config["vars"] = parsed.Vars
}

// mergeSatelliteVars reads the satellite repo's existing .stagefreight.yml,
// extracts its vars, and merges satellite-owned keys into the governance config.
// Governance keys are authoritative (already in config). Satellite keys that
// governance does not declare are preserved. This implements the var ownership contract:
//   - governance-declared keys → governance-owned
//   - undeclared keys → satellite-owned, preserved
func mergeSatelliteVars(config map[string]any, reader ForgeReader, repo string) {
	if reader == nil {
		return
	}

	existing, err := reader.GetFileContent(repo, ".stagefreight.yml", "HEAD")
	if err != nil {
		return // no existing config — nothing to merge
	}

	var parsed struct {
		Vars map[string]any `yaml:"vars"`
	}
	if err := yaml.Unmarshal(existing, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: %s: failed to parse existing config for vars merge: %v\n", repo, err)
		return
	}
	if parsed.Vars == nil {
		return
	}

	// Get or create the governance vars map.
	govVars, _ := config["vars"].(map[string]any)
	if govVars == nil {
		govVars = make(map[string]any)
	}

	// Merge with ownership contract enforcement:
	// - governance-declared keys are authoritative
	// - undeclared satellite keys are preserved
	// - ownership takeover (governance now declares a key the satellite had) is logged
	for k, satelliteVal := range parsed.Vars {
		govVal, governed := govVars[k]
		if !governed {
			// Satellite-owned key — preserve.
			govVars[k] = satelliteVal
		} else if fmt.Sprintf("%v", govVal) != fmt.Sprintf("%v", satelliteVal) {
			// Ownership takeover — governance now declares a key that existed locally
			// with a different value. Governance wins, but log the takeover.
			fmt.Fprintf(os.Stderr, "  drift: %s: var %q governance=%v satellite=%v (governance wins)\n",
				repo, k, govVal, satelliteVal)
		}
	}

	config["vars"] = govVars
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
