package docker

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/config"
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
func BuildCacheFlags(cfg config.BuildCacheConfig, repoID, branch string, targets []config.TargetConfig) (cacheFrom, cacheTo []string) {
	if !cfg.IsActive() {
		return nil, nil
	}

	switch cfg.Mode {
	case "local":
		return localFlags(repoID)

	case "shared":
		return externalFlags(cfg.External, repoID, branch, targets)

	case "hybrid":
		localFrom, localTo := localFlags(repoID)
		extFrom, extTo := externalFlags(cfg.External, repoID, branch, targets)
		return append(localFrom, extFrom...), append(localTo, extTo...)
	}

	return nil, nil
}

// localFlags returns BuildKit local cache flags.
func localFlags(repoID string) (cacheFrom, cacheTo []string) {
	dir := LocalCacheDir(repoID)
	return []string{fmt.Sprintf("type=local,src=%s", dir)},
		[]string{fmt.Sprintf("type=local,dest=%s,mode=max", dir)}
}

// externalFlags returns BuildKit registry cache flags.
func externalFlags(ext config.ExternalCacheConfig, repoID, branch string, targets []config.TargetConfig) (cacheFrom, cacheTo []string) {
	if ext.Target == "" || ext.Path == "" {
		return nil, nil
	}

	baseURL := resolveTargetURL(ext.Target, targets)
	if baseURL == "" {
		return nil, nil
	}

	repo := CanonicalizeRef(repoID)
	br := CanonicalizeRef(branch)
	mode := ext.Mode
	if mode == "" {
		mode = "max"
	}

	branchRef := fmt.Sprintf("%s/%s/%s/%s", baseURL, ext.Path, repo, br)

	// cache-from: branch first, then fallback.
	cacheFrom = []string{fmt.Sprintf("type=registry,ref=%s", branchRef)}
	if ext.Fallback != "" && ext.Fallback != branch {
		fallbackRef := fmt.Sprintf("%s/%s/%s/%s", baseURL, ext.Path, repo, CanonicalizeRef(ext.Fallback))
		cacheFrom = append(cacheFrom, fmt.Sprintf("type=registry,ref=%s", fallbackRef))
	}

	// cache-to: branch only. Never fallback. Caller gates on build success.
	cacheTo = []string{fmt.Sprintf("type=registry,ref=%s,mode=%s", branchRef, mode)}

	return cacheFrom, cacheTo
}

// LocalCacheDir resolves the local cache directory.
// Uses XDG_CACHE_HOME if set, otherwise /tmp. Never inside the repo.
func LocalCacheDir(repoID string) string {
	hash := repoHash(repoID)
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		base = filepath.Join(os.TempDir(), "stagefreight", "cache")
	} else {
		base = filepath.Join(base, "stagefreight")
	}
	return filepath.Join(base, hash, "buildkit")
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

// repoHash returns a hex-encoded hash of a repo identity.
func repoHash(repoID string) string {
	h := sha256.Sum256([]byte(repoID))
	return fmt.Sprintf("%x", h[:8])
}

// resolveTargetURL finds the URL for a target ID from the config targets list.
func resolveTargetURL(targetID string, targets []config.TargetConfig) string {
	for _, t := range targets {
		if t.ID == targetID && t.Kind == "registry" {
			return strings.TrimSuffix(t.URL, "/")
		}
	}
	return ""
}
