package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/cistate"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/output"
)

// LocalRetentionResult records what the local cache retention executor did.
type LocalRetentionResult struct {
	Dir         string
	EntriesBefore int
	Pruned      int
	PrunedBytes int64
	Reason      string // "" if nothing to do
}

// localCacheRetentionPhase enforces cache retention post-build.
// Backend-aware: if the builder is buildkitd, prunes via buildx prune.
// Otherwise prunes the local export directory by age then size.
func localCacheRetentionPhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "cache-retention-local",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			if !pc.Config.BuildCache.IsActive() {
				return nil, nil
			}
			localCfg := pc.Config.BuildCache.Local
			if localCfg.Retention.MaxAge == "" && localCfg.Retention.MaxSize == "" {
				return nil, nil
			}

			// BuildKit cache lives inside the daemon (remote or docker-container driver).
			// Prune via buildx prune instead of filesystem operations.
			// Prune failure = retention policy not enforced = build failure.
			if info, ok := pc.Scratch["docker.builderInfo"].(BuilderInfo); ok && (info.Driver == "remote" || info.Driver == "docker-container") {
				pruneResult := pruneBuildkitCache(info.Name, localCfg.Retention, pc.Verbose)
				renderBuildkitPrune(pc.Writer, pc.Color, pruneResult, pc.Verbose)

				if pruneResult.Error != nil {
					return &pipeline.PhaseResult{
						Name:    "cache-retention-local",
						Status:  "failed",
						Summary: "cache prune failed — retention policy not enforced",
					}, fmt.Errorf("buildkit cache prune failed: %w", pruneResult.Error)
				}

				summary := fmt.Sprintf("reclaimed %s", pruneResult.Reclaimed)
				if pruneResult.Skipped {
					summary = pruneResult.SkipReason
				}
				return &pipeline.PhaseResult{
					Name:    "cache-retention-local",
					Status:  "success",
					Summary: summary,
				}, nil
			}

			// Local export directory — prune by age then size.
			repoID := resolveRepoIDFromContext(pc)
			dir := LocalCacheDir(repoID, localCfg)

			result := enforceLocalRetention(dir, localCfg.Retention)
			renderLocalRetention(pc.Writer, pc.Color, result)

			// Record in pipeline state for governance/diagnostics.
			if err := cistate.UpdateState(pc.RootDir, func(st *cistate.State) {
				st.Retention.Local = &cistate.LocalRetentionRecord{
					Dir:           result.Dir,
					EntriesBefore: result.EntriesBefore,
					Pruned:        result.Pruned,
					PrunedBytes:   result.PrunedBytes,
				}
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: retention state write failed: %v\n", err)
			}

			summary := fmt.Sprintf("pruned %d entries", result.Pruned)
			if result.Pruned == 0 {
				summary = "within limits"
			}

			return &pipeline.PhaseResult{
				Name:    "cache-retention-local",
				Status:  "success",
				Summary: summary,
			}, nil
		},
	}
}

// enforceLocalRetention prunes local BuildKit cache by age then size.
// BuildKit's type=local cache stores blobs in the cache directory.
// We prune at the directory level — remove oldest entries first.
func enforceLocalRetention(dir string, retention config.LocalRetention) LocalRetentionResult {
	result := LocalRetentionResult{Dir: dir}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			result.Reason = "cache dir does not exist (no prior cache)"
			return result
		}
		result.Reason = fmt.Sprintf("cache dir unreadable: %v", err)
		return result
	}
	result.EntriesBefore = len(entries)

	// Phase 1: prune by max_age.
	if retention.MaxAge != "" {
		maxAge, err := config.ParseDuration(retention.MaxAge)
		if err != nil {
			result.Reason = fmt.Sprintf("invalid max_age %q: %v", retention.MaxAge, err)
			return result
		}
		if maxAge > 0 {
			cutoff := time.Now().Add(-maxAge)
			for _, e := range entries {
				info, err := e.Info()
				if err != nil {
					continue
				}
				if info.ModTime().Before(cutoff) {
					path := filepath.Join(dir, e.Name())
					size := dirSize(path)
					if os.RemoveAll(path) == nil {
						result.Pruned++
						result.PrunedBytes += size
					}
				}
			}
		}
	}

	// Phase 2: enforce max_size by evicting oldest entries.
	if retention.MaxSize != "" {
		maxBytes, err := config.ParseSize(retention.MaxSize)
		if err != nil {
			result.Reason = fmt.Sprintf("invalid max_size %q: %v", retention.MaxSize, err)
			return result
		}
		if maxBytes > 0 {
			// Re-read after age pruning.
			entries, _ = os.ReadDir(dir)
			type entry struct {
				name    string
				size    int64
				modTime time.Time
			}
			var all []entry
			var totalSize int64
			for _, e := range entries {
				path := filepath.Join(dir, e.Name())
				size := dirSize(path)
				info, err := e.Info()
				if err != nil {
					continue
				}
				all = append(all, entry{name: e.Name(), size: size, modTime: info.ModTime()})
				totalSize += size
			}

			// Sort oldest first.
			sort.Slice(all, func(i, j int) bool {
				return all[i].modTime.Before(all[j].modTime)
			})

			for _, e := range all {
				if totalSize <= maxBytes {
					break
				}
				path := filepath.Join(dir, e.name)
				if os.RemoveAll(path) == nil {
					result.Pruned++
					result.PrunedBytes += e.size
					totalSize -= e.size
				}
			}
		}
	}

	return result
}

func renderLocalRetention(w interface{ Write([]byte) (int, error) }, color bool, result LocalRetentionResult) {
	sec := output.NewSection(w, "Cache Retention (local)", 0, color)
	sec.Row("%-14s%s", "path", result.Dir)
	sec.Row("%-14s%d", "entries", result.EntriesBefore)
	sec.Row("%-14s%d", "pruned", result.Pruned)
	if result.PrunedBytes > 0 {
		sec.Row("%-14s%s", "reclaimed", formatBytes(result.PrunedBytes))
	}
	if result.Reason != "" {
		sec.Row("%-14s%s", "note", result.Reason)
	}
	sec.Close()
}

}

// dirSize returns the total size of a directory tree.
func dirSize(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// resolveRepoIDFromContext extracts a repo identifier from pipeline context.
func resolveRepoIDFromContext(pc *pipeline.PipelineContext) string {
	if pc.Config != nil && pc.Config.Sources.Primary.URL != "" {
		return pc.Config.Sources.Primary.URL
	}
	return os.Getenv("SF_CI_REPO_URL")
}
