package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/registry"
)

// ExternalRetentionResult records what the external cache retention executor did.
type ExternalRetentionResult struct {
	Registry string
	Path     string
	Prefix   string
	Total    int
	Pruned   int
	Kept     int
	Errors   []string
}

// externalCacheRetentionHook enforces external cache retention post-build.
// Runs only on success. Prunes stale branch cache refs by age and count.
// Only touches refs that match StageFreight's deterministic cache naming contract.
func externalCacheRetentionHook() pipeline.PostBuildHook {
	return pipeline.PostBuildHook{
		Name: "cache-retention-external",
		Condition: func(pc *pipeline.PipelineContext) bool {
			if !pc.Config.BuildCache.IsActive() {
				return false
			}
			ext := pc.Config.BuildCache.External
			return ext.Target != "" && (ext.Retention.MaxRefs > 0 || ext.Retention.StaleAge != "")
		},
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			ext := pc.Config.BuildCache.External
			repoID := resolveRepoIDFromContext(pc)

			result := enforceExternalRetention(pc.Ctx, ext, repoID, pc.Config.Targets)
			renderExternalRetention(pc.Writer, pc.Color, result)

			summary := fmt.Sprintf("pruned %d/%d cache refs", result.Pruned, result.Total)
			if result.Pruned == 0 {
				summary = fmt.Sprintf("%d cache refs within limits", result.Total)
			}

			return &pipeline.PhaseResult{
				Name:    "cache-retention-external",
				Status:  "success",
				Summary: summary,
			}, nil
		},
	}
}

// enforceExternalRetention lists cache tags on the registry and prunes stale ones.
// Scope: only tags matching the deterministic cache prefix (e.g., "cache-").
// Ownership proof: tag must start with the configured path prefix + "-".
func enforceExternalRetention(ctx context.Context, ext config.ExternalCacheConfig, repoID string, targets []config.TargetConfig) ExternalRetentionResult {
	result := ExternalRetentionResult{}

	targetRef := resolveTargetRef(ext.Target, targets)
	if targetRef == "" {
		result.Errors = append(result.Errors, "cache target not resolved")
		return result
	}

	// Parse registry URL and path from target ref (e.g., "cr.pcfae.com/prplanit/stagefreight").
	parts := strings.SplitN(targetRef, "/", 2)
	if len(parts) != 2 {
		result.Errors = append(result.Errors, fmt.Sprintf("invalid target ref %q", targetRef))
		return result
	}
	registryURL := parts[0]
	repoPath := parts[1]
	result.Registry = registryURL
	result.Path = repoPath

	// Namespace isolation: prefix includes both the configured path AND a repo hash.
	// Tag pattern: <path>-<repo-hash-8>-<branch-canonical>
	// This ensures we only prune tags StageFreight created for THIS repo,
	// even on shared cache targets.
	pathPrefix := ext.Path
	if pathPrefix == "" {
		pathPrefix = "cache"
	}
	repoScope := repoHash(repoID)[:8]
	prefix := fmt.Sprintf("%s-%s", pathPrefix, repoScope)
	result.Prefix = prefix

	// Find the target config to get provider + credentials.
	var targetCfg *config.TargetConfig
	for i := range targets {
		if targets[i].ID == ext.Target {
			targetCfg = &targets[i]
			break
		}
	}
	if targetCfg == nil {
		result.Errors = append(result.Errors, fmt.Sprintf("target %q not found in config", ext.Target))
		return result
	}

	client, err := registry.NewRegistry(targetCfg.Provider, registryURL, targetCfg.Credentials)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("registry client: %v", err))
		return result
	}

	// List all tags, filter to cache refs matching our prefix.
	allTags, err := client.ListTags(ctx, repoPath)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("listing tags: %v", err))
		return result
	}

	// Filter: only tags matching StageFreight's deterministic cache naming.
	// Pattern: <prefix>-<normalized-branch>-<8-char-hex-hash>
	// The hash suffix is the ownership proof — random branch names won't match.
	// We scope to the configured prefix AND require the hash suffix pattern.
	var cacheTags []registry.TagInfo
	for _, t := range allTags {
		if strings.HasPrefix(t.Name, prefix+"-") && looksLikeSFCacheTag(t.Name, prefix) {
			cacheTags = append(cacheTags, t)
		}
	}
	result.Total = len(cacheTags)

	if len(cacheTags) == 0 {
		return result
	}

	// Sort oldest first for eviction.
	sort.Slice(cacheTags, func(i, j int) bool {
		return cacheTags[i].CreatedAt.Before(cacheTags[j].CreatedAt)
	})

	// Phase 1: prune by stale_age.
	var remaining []registry.TagInfo
	if ext.Retention.StaleAge != "" {
		maxAge, err := parseDuration(ext.Retention.StaleAge)
		if err == nil && maxAge > 0 {
			cutoff := time.Now().Add(-maxAge)
			for _, t := range cacheTags {
				if t.CreatedAt.Before(cutoff) {
					if err := client.DeleteTag(ctx, repoPath, t.Name); err != nil {
						result.Errors = append(result.Errors, fmt.Sprintf("delete %s: %v", t.Name, err))
					} else {
						result.Pruned++
					}
				} else {
					remaining = append(remaining, t)
				}
			}
		} else {
			remaining = cacheTags
		}
	} else {
		remaining = cacheTags
	}

	// Phase 2: enforce max_refs by evicting oldest.
	if ext.Retention.MaxRefs > 0 && len(remaining) > ext.Retention.MaxRefs {
		excess := remaining[:len(remaining)-ext.Retention.MaxRefs]
		for _, t := range excess {
			if err := client.DeleteTag(ctx, repoPath, t.Name); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("delete %s: %v", t.Name, err))
			} else {
				result.Pruned++
			}
		}
	}

	result.Kept = result.Total - result.Pruned
	return result
}

// looksLikeSFCacheTag validates that a tag matches StageFreight's deterministic
// cache naming pattern: <prefix>-<anything>-<8-hex-hash>.
// The 8-char hex suffix is the ownership proof from CanonicalizeRef().
func looksLikeSFCacheTag(tag, prefix string) bool {
	suffix := strings.TrimPrefix(tag, prefix+"-")
	if suffix == tag {
		return false // didn't have prefix
	}
	// Must end with -<8 hex chars> (the hash from CanonicalizeRef).
	idx := strings.LastIndex(suffix, "-")
	if idx < 0 || idx == len(suffix)-1 {
		return false
	}
	hash := suffix[idx+1:]
	if len(hash) != 8 {
		return false
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func renderExternalRetention(w interface{ Write([]byte) (int, error) }, color bool, result ExternalRetentionResult) {
	sec := output.NewSection(w, "Cache Retention (external)", 0, color)
	sec.Row("%-14s%s/%s", "registry", result.Registry, result.Path)
	sec.Row("%-14s%s-*", "prefix", result.Prefix)
	sec.Row("%-14s%d", "total", result.Total)
	sec.Row("%-14s%d", "pruned", result.Pruned)
	sec.Row("%-14s%d", "kept", result.Kept)
	for _, e := range result.Errors {
		sec.Row("%-14s%s", "error", e)
	}
	sec.Close()
}
