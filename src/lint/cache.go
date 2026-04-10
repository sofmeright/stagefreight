package lint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/PrPlanIT/StageFreight/src/atomicfile"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/version"
)

// cacheSchemaVersion is bumped whenever the cache entry format or
// finding semantics change. This invalidates stale entries automatically.
// Bump this when: finding fields change, severity logic changes, or
// module output format changes.
const cacheSchemaVersion = "2"

// Cache provides content-addressed lint result caching.
// Dir is the resolved cache directory (call ResolveCacheDir to compute it).
type Cache struct {
	Dir     string
	Enabled bool
}

// cacheEntry stores cached findings for a file+module combination.
type cacheEntry struct {
	Findings []Finding `json:"findings"`
	CachedAt int64     `json:"cached_at,omitempty"`
}

// ResolveCacheDir determines the cache directory using the following precedence:
//  1. STAGEFREIGHT_CACHE_DIR env var (used as-is, caller controls the path)
//  2. configDir from .stagefreight.yml cache_dir (resolved relative to rootDir)
//  3. /stagefreight/cache/lint/<project-hash> (persistent runtime root, if mounted)
//  4. os.UserCacheDir()/stagefreight/<project-hash>/lint (XDG-aware default)
//
// The project hash is a truncated SHA-256 of the absolute rootDir path,
// keeping per-project caches isolated without long nested directory names.
func ResolveCacheDir(rootDir string, configDir string) string {
	// 1. Env var takes priority
	if dir := os.Getenv("STAGEFREIGHT_CACHE_DIR"); dir != "" {
		return filepath.Join(dir, "lint")
	}

	// 2. Config-specified directory (relative to project root)
	if configDir != "" {
		if filepath.IsAbs(configDir) {
			return filepath.Join(configDir, "lint")
		}
		return filepath.Join(rootDir, configDir, "lint")
	}

	// Project hash: prefer repo identity (stable across runners/paths),
	// fall back to absolute path (local dev).
	identity := os.Getenv("SF_CI_REPO_URL")
	if identity == "" {
		absRoot, err := filepath.Abs(rootDir)
		if err != nil {
			absRoot = rootDir
		}
		identity = absRoot
	}
	h := sha256.Sum256([]byte(identity))
	projectHash := hex.EncodeToString(h[:])[:12]

	// 3. Persistent runtime root — capability-based: use if mounted AND writable.
	if info, err := os.Stat("/stagefreight"); err == nil && info.IsDir() {
		sfCache := filepath.Join("/stagefreight", "cache", "lint", projectHash)
		if err := os.MkdirAll(sfCache, 0o755); err == nil {
			return sfCache
		}
		// Mounted but not writable — fall through to XDG.
	}

	// 4. XDG-aware default via os.UserCacheDir
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}

	return filepath.Join(base, "stagefreight", "cache", "lint", projectHash)
}

// Key computes a cache key from file content, module name, config, and
// the binary's build commit. Including the commit ensures that cache entries
// are automatically invalidated when the binary changes — no stale results
// from a prior build's module logic can survive a binary update.
func (c *Cache) Key(content []byte, moduleName string, configJSON string) string {
	h := sha256.New()
	h.Write(content)
	h.Write([]byte(moduleName))
	h.Write([]byte(configJSON))
	h.Write([]byte(cacheSchemaVersion))
	h.Write([]byte(version.Commit))
	return hex.EncodeToString(h.Sum(nil))
}

// Get retrieves cached findings. Returns nil, false on cache miss.
// maxAge controls TTL: 0 means no expiry (content-only modules),
// >0 expires entries older than the duration (external-state modules).
//
// On hit, updates the file's mtime so eviction can distinguish
// actively-used entries from dead ones (old file versions that
// are never read again).
func (c *Cache) Get(key string, maxAge time.Duration) ([]Finding, bool) {
	if !c.Enabled {
		return nil, false
	}

	path := c.path(key)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		os.Remove(path) // self-heal corrupted entries
		return nil, false
	}

	// TTL check for external-state modules.
	// Old entries without a timestamp (CachedAt==0) are treated as
	// expired — prevents pre-TTL cache files from being served forever.
	if maxAge > 0 {
		if entry.CachedAt == 0 {
			return nil, false
		}
		if time.Since(time.Unix(entry.CachedAt, 0)) > maxAge {
			return nil, false
		}
	}

	// Touch mtime — marks this entry as actively used for eviction.
	// Best-effort: cache hit correctness must not depend on touch success,
	// but silent failure would make eviction behavior hard to reason about.
	now := time.Now()
	_ = os.Chtimes(path, now, now) // error logged via EvictResult if eviction misbehaves

	return entry.Findings, true
}

// Put stores findings in the cache atomically.
func (c *Cache) Put(key string, findings []Finding) error {
	if !c.Enabled {
		return nil
	}

	entry := cacheEntry{Findings: findings, CachedAt: time.Now().Unix()}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	return atomicfile.WriteFile(c.path(key), data, 0o644)
}

// EvictResult records what cache eviction did.
type EvictResult struct {
	EntriesBefore int
	Evicted       int
	EvictedBytes  int64
	Reason        string // non-empty if skipped/errored
}

// Evict removes stale cache entries to bound unbounded growth.
// Content-addressed caches grow monotonically: every file edit creates a new
// entry, old entries for previous content are never read again.
//
// Strategy: mtime-based (Get touches mtime on hit, so mtime = last access).
//  1. Remove entries with mtime older than maxAge (dead entries)
//  2. If still over maxSize, remove oldest entries until under limit
//
// Both maxAge and maxSize use the same human format as retention config
// (e.g. "7d", "100MB"). Either can be empty to skip that phase.
func (c *Cache) Evict(maxAge string, maxSize string) EvictResult {
	result := EvictResult{}
	if !c.Enabled || c.Dir == "" {
		result.Reason = "cache not enabled"
		return result
	}
	if maxAge == "" && maxSize == "" {
		result.Reason = "no eviction policy configured"
		return result
	}

	type entry struct {
		path    string
		size    int64
		modTime time.Time
	}

	var entries []entry
	var totalSize int64

	// Walk all shard directories.
	filepath.Walk(c.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		entries = append(entries, entry{path: path, size: info.Size(), modTime: info.ModTime()})
		totalSize += info.Size()
		return nil
	})

	result.EntriesBefore = len(entries)

	// Phase 1: evict by age.
	if maxAge != "" {
		ageDur, err := config.ParseDuration(maxAge)
		if err != nil {
			result.Reason = fmt.Sprintf("invalid max_age %q: %v", maxAge, err)
			return result
		}
		if ageDur > 0 {
			cutoff := time.Now().Add(-ageDur)
			var surviving []entry
			for _, e := range entries {
				if e.modTime.Before(cutoff) {
					if os.Remove(e.path) == nil {
						result.Evicted++
						result.EvictedBytes += e.size
						totalSize -= e.size
					}
				} else {
					surviving = append(surviving, e)
				}
			}
			entries = surviving
		}
	}

	// Phase 2: enforce size cap — evict oldest first until under limit.
	if maxSize != "" {
		maxBytes, err := config.ParseSize(maxSize)
		if err != nil {
			result.Reason = fmt.Sprintf("invalid max_size %q: %v", maxSize, err)
			return result
		}
		if maxBytes > 0 && totalSize > maxBytes {
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].modTime.Before(entries[j].modTime)
			})
			for _, e := range entries {
				if totalSize <= maxBytes {
					break
				}
				if os.Remove(e.path) == nil {
					result.Evicted++
					result.EvictedBytes += e.size
					totalSize -= e.size
				}
			}
		}
	}

	return result
}

// Clear removes the entire cache directory.
func (c *Cache) Clear() error {
	return os.RemoveAll(c.Dir)
}

// path returns the filesystem path for a cache key.
// Uses 2-char prefix subdirectory to avoid huge flat directories.
func (c *Cache) path(key string) string {
	prefix := key[:2]
	return filepath.Join(c.Dir, prefix, key+".json")
}
