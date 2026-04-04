package docker

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/output"
)

// CacheResolution records what was checked during cache setup.
// Populated by the executor after probing, not by flag building.
type CacheResolution struct {
	Mode     string // off | local | shared | hybrid
	Local    string // hit | miss | skipped
	External string // hit | miss | skipped
	Fallback string // hit | miss | skipped
	Result   string // "using local" | "using external (branch)" | "cold build"
	Builder  string // "reused (sf-abc123)" | "created (sf-abc123)"
}

// BuildCacheFlags computes --cache-from and --cache-to flags from config.
// Does NOT determine hits/misses — that's the executor's job after probing.
//
// Invariants enforced here:
//   - Fallback never in cache-to (read-only)
//   - Ref canonicalization: normalized prefix + hash suffix
//   - Precedence ordering: local before external in cache-from list
func BuildCacheFlags(cfg config.BuildCacheConfig, repoID, branch string, targets []config.TargetConfig) (cacheFrom, cacheTo []build.CacheRef) {
	if !cfg.IsActive() {
		return nil, nil
	}

	switch cfg.Mode {
	case "local":
		return localFlags(repoID, cfg.Local)

	case "shared":
		return externalFlags(cfg.External, repoID, branch, targets)

	case "hybrid":
		localFrom, localTo := localFlags(repoID, cfg.Local)
		extFrom, extTo := externalFlags(cfg.External, repoID, branch, targets)
		return append(localFrom, extFrom...), append(localTo, extTo...)
	}

	return nil, nil
}

// localFlags returns BuildKit local cache refs.
// Returns nil if the local cache path isn't available (not mounted/writable).
func localFlags(repoID string, localCfg config.LocalCacheConfig) (cacheFrom, cacheTo []build.CacheRef) {
	if !LocalCacheAvailable(repoID, localCfg) {
		return nil, nil
	}
	dir := LocalCacheDir(repoID, localCfg)
	return []build.CacheRef{{Type: "local", Ref: dir, Direction: "import"}},
		[]build.CacheRef{{Type: "local", Ref: dir, Mode: "max", Direction: "export"}}
}

// externalFlags returns BuildKit registry cache refs.
func externalFlags(ext config.ExternalCacheConfig, repoID, branch string, targets []config.TargetConfig) (cacheFrom, cacheTo []build.CacheRef) {
	if ext.Target == "" {
		return nil, nil
	}

	targetRef := resolveTargetRef(ext.Target, targets)
	if targetRef == "" {
		return nil, nil
	}

	// Cache refs are tags on the target repo.
	// Pattern: <registry>/<path>:<prefix>-<repo-hash>-<branch-canonical>
	// e.g. cr.pcfae.com/prplanit/stagefreight:cache-0d6e4079-main-a1b2c3d4
	// Repo hash prevents cross-repo collision on shared cache targets.
	prefix := ext.Path
	if prefix == "" {
		prefix = "cache"
	}

	repoScope := repoHash(repoID)[:8]
	br := CanonicalizeRef(branch)
	mode := ext.Mode
	if mode == "" {
		mode = "max"
	}

	tagPrefix := fmt.Sprintf("%s-%s", prefix, repoScope)
	branchRef := fmt.Sprintf("%s:%s-%s", targetRef, tagPrefix, br)

	// cache-from: branch first, then fallback.
	cacheFrom = []build.CacheRef{{Type: "registry", Ref: branchRef, Direction: "import"}}
	if ext.Fallback != "" && ext.Fallback != branch {
		fallbackRef := fmt.Sprintf("%s:%s-%s", targetRef, tagPrefix, CanonicalizeRef(ext.Fallback))
		cacheFrom = append(cacheFrom, build.CacheRef{Type: "registry", Ref: fallbackRef, Direction: "import"})
	}

	// cache-to: branch only. Never fallback. Caller gates on build success.
	cacheTo = []build.CacheRef{{Type: "registry", Ref: branchRef, Mode: mode, Direction: "export"}}

	return cacheFrom, cacheTo
}

// LocalCacheDir resolves the local cache directory.
// Default: /stagefreight/cache/buildkit/<repo-hash> (persistent runtime root).
// Falls back to config override if set. Never uses /tmp.
func LocalCacheDir(repoID string, cfg config.LocalCacheConfig) string {
	hash := repoHash(repoID)
	base := cfg.Path
	if base == "" {
		base = "/stagefreight/cache/buildkit"
	}
	return filepath.Join(base, hash)
}

// LocalCacheAvailable checks if the local cache path is mounted and writable.
func LocalCacheAvailable(repoID string, cfg config.LocalCacheConfig) bool {
	dir := LocalCacheDir(repoID, cfg)
	// Check parent exists (the mount point).
	parent := filepath.Dir(dir)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return false
	}
	// Try creating the repo-scoped dir.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false
	}
	return true
}

// BuilderName returns a deterministic, repo-scoped builder name.
// Prevents cross-repo cache pollution on shared DinD.
func BuilderName(repoID string) string {
	hash := repoHash(repoID)
	return "sf-" + hash[:8]
}

// CanonicalizeRef normalizes a repo or branch name for registry ref construction.
// Uses a normalized prefix (lowercase, safe chars) plus a hash suffix to prevent collisions.
// "feature/a-b" and "feature-a/b" produce different refs because the hash includes the original.
func CanonicalizeRef(s string) string {
	// Normalized human-readable prefix.
	prefix := strings.ToLower(s)
	prefix = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, prefix)
	// Collapse repeated dashes.
	for strings.Contains(prefix, "--") {
		prefix = strings.ReplaceAll(prefix, "--", "-")
	}
	prefix = strings.Trim(prefix, "-")

	// Hash suffix from original (pre-normalization) to prevent collisions.
	h := sha256.Sum256([]byte(s))
	suffix := fmt.Sprintf("%x", h[:4])

	// Registry tags: max 128 chars. Reserve 9 for "-" + 8-char hash.
	if len(prefix) > 119 {
		prefix = prefix[:119]
	}

	return prefix + "-" + suffix
}

// CacheInfo holds resolved cache state for rendering.
// Computed once, rendered by execute.go — cache.go does not render.
type CacheInfo struct {
	Mode     string
	Branch   string
	Fallback string
	Imports  []string // deduped, ordered
	Exports  []string // deduped, ordered
}

// ResolveCacheInfo computes the cache rendering info from the pipeline context.
func ResolveCacheInfo(pc *pipeline.PipelineContext) CacheInfo {
	cfg := pc.Config.BuildCache
	info := CacheInfo{
		Mode: cfg.Mode,
	}

	if !cfg.IsActive() {
		info.Mode = "off"
		return info
	}

	// Branch context from CI env.
	info.Branch = os.Getenv("SF_CI_BRANCH")
	if info.Branch == "" {
		info.Branch = os.Getenv("CI_COMMIT_BRANCH")
	}
	info.Fallback = cfg.External.Fallback
	if info.Fallback == "" {
		info.Fallback = os.Getenv("SF_CI_DEFAULT_BRANCH")
	}

	// Collect and dedupe refs from all steps.
	importSeen := map[string]struct{}{}
	exportSeen := map[string]struct{}{}

	if pc.BuildPlan != nil {
		for _, step := range pc.BuildPlan.Steps {
			for _, cf := range step.CacheFrom {
				if _, dup := importSeen[cf.Ref]; !dup {
					importSeen[cf.Ref] = struct{}{}
					info.Imports = append(info.Imports, cf.Ref)
				}
			}
			for _, ct := range step.CacheTo {
				if _, dup := exportSeen[ct.Ref]; !dup {
					exportSeen[ct.Ref] = struct{}{}
					info.Exports = append(info.Exports, ct.Ref)
				}
			}
		}
	}

	sort.Strings(info.Imports)
	sort.Strings(info.Exports)

	return info
}

// RenderCacheInfo prints structured cache resolution output.
// Called from execute.go — cache.go only resolves, execute.go renders.
func RenderCacheInfo(w io.Writer, color bool, info CacheInfo) {
	sec := output.NewSection(w, "Cache", 0, color)
	sec.Row("%-14s%s", "mode", info.Mode)

	if info.Mode == "off" {
		sec.Close()
		return
	}

	if info.Branch != "" {
		sec.Row("%-14s%s", "branch", info.Branch)
	}
	if info.Fallback != "" {
		sec.Row("%-14s%s", "fallback", info.Fallback)
	}

	for _, ref := range info.Imports {
		sec.Row("%-14s%s", "import", ref)
	}
	for _, ref := range info.Exports {
		sec.Row("%-14s%s", "export", ref)
	}

	sec.Close()
}

// repoHash returns a hex-encoded hash of a repo identity.
func repoHash(repoID string) string {
	h := sha256.Sum256([]byte(repoID))
	return fmt.Sprintf("%x", h[:8])
}

// resolveTargetRef finds the full registry repo ref (url/path) for a target ID.
// Resolves {var:...} templates in the path using the config's Vars map.
func resolveTargetRef(targetID string, targets []config.TargetConfig) string {
	for _, t := range targets {
		if t.ID == targetID && t.Kind == "registry" {
			url := strings.TrimSuffix(t.URL, "/")
			path := strings.Trim(t.Path, "/")
			if path != "" {
				return url + "/" + path
			}
			return url
		}
	}
	return ""
}
