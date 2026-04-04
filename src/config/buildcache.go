package config

// BuildCacheConfig defines the three-layer build cache subsystem.
// Mode selects the active planes. Advanced config underneath only when needed.
type BuildCacheConfig struct {
	// Mode selects which cache planes are active.
	// "": inactive — no cache flags emitted, no cleanup runs.
	// off: explicitly disabled.
	// local: bounded local buildkit cache only.
	// shared: registry-backed external cache only.
	// hybrid: both local and external.
	Mode string `yaml:"mode,omitempty"`

	// Local configures the bounded local buildkit cache.
	Local LocalCacheConfig `yaml:"local,omitempty"`

	// External configures registry-backed shared cache.
	External ExternalCacheConfig `yaml:"external,omitempty"`

	// Cleanup configures host hygiene — residue pruning, not cache.
	// Only executes when Mode is active (not empty, not off).
	Cleanup HostCleanupConfig `yaml:"cleanup,omitempty"`
}

// IsActive returns true if any cache mode is enabled.
func (c BuildCacheConfig) IsActive() bool {
	return c.Mode != "" && c.Mode != "off"
}

// LocalCacheConfig defines bounded local BuildKit cache.
// Default path: /stagefreight/cache/buildkit (persistent runtime root).
// Override via Path field for non-standard mounts.
type LocalCacheConfig struct {
	Path      string         `yaml:"path,omitempty"` // override local cache root (default: /stagefreight/cache/buildkit)
	Retention LocalRetention `yaml:"retention,omitempty"`
}

// LocalRetention defines what local cache is allowed to exist.
type LocalRetention struct {
	MaxAge  string `yaml:"max_age,omitempty"`  // e.g. "7d"
	MaxSize string `yaml:"max_size,omitempty"` // e.g. "15GB"
}

// ExternalCacheConfig defines registry-backed shared cache.
// References an existing target (kind: registry) by ID — no redeclaring registries.
type ExternalCacheConfig struct {
	// Target references a targets[].id with kind: registry.
	Target string `yaml:"target,omitempty"`

	// Path is appended to the target URL: <target-url>/<path>/<repo>/<branch>.
	Path string `yaml:"path,omitempty"`

	// Fallback is the read-only fallback branch ref (e.g. "default", "main").
	// Never written to unless current ref equals fallback.
	Fallback string `yaml:"fallback,omitempty"`

	// Mode is the BuildKit cache mode (e.g. "max", "min"). Default: "max".
	Mode string `yaml:"mode,omitempty"`

	// Retention defines when stale external cache refs are pruned.
	Retention ExternalRetention `yaml:"retention,omitempty"`
}

// ExternalRetention defines external cache ref lifecycle.
type ExternalRetention struct {
	MaxRefs  int    `yaml:"max_refs,omitempty"`  // max branch cache refs per repo
	StaleAge string `yaml:"stale_age,omitempty"` // prune refs for dead/merged branches
}

// HostCleanupConfig defines host hygiene — classification-first pruning.
// Not cache — residue cleanup. Object classes match DD-UI's operation types.
type HostCleanupConfig struct {
	// Enabled controls whether cleanup runs. Independent of cache mode.
	Enabled bool `yaml:"enabled,omitempty"`

	// Enforcement controls what happens when cleanup cannot execute.
	// best_effort: continue + structured warning.
	// required: fail build immediately.
	Enforcement string `yaml:"enforcement,omitempty"`

	// Protect defines what is never pruned.
	Protect ProtectionPolicy `yaml:"protect,omitempty"`

	// Prune defines what is eligible for removal.
	Prune PrunePolicy `yaml:"prune,omitempty"`
}

// ProtectionPolicy defines what host objects are never pruned.
type ProtectionPolicy struct {
	Images  ImageProtection  `yaml:"images,omitempty"`
	Volumes VolumeProtection `yaml:"volumes,omitempty"`
}

// ImageProtection defines which images are protected from pruning.
type ImageProtection struct {
	Refs []string `yaml:"refs,omitempty"` // glob patterns
}

// VolumeProtection defines which volumes are protected from pruning.
type VolumeProtection struct {
	Named *bool `yaml:"named,omitempty"` // protect all named volumes
}

// PrunePolicy defines which host objects are eligible for removal.
type PrunePolicy struct {
	Images     ImagePrunePolicy      `yaml:"images,omitempty"`
	BuildCache BuildCachePrunePolicy `yaml:"build_cache,omitempty"`
	Containers ContainerPrunePolicy  `yaml:"containers,omitempty"`
	Networks   NetworkPrunePolicy    `yaml:"networks,omitempty"`
}

// ImagePrunePolicy defines image pruning rules by class.
type ImagePrunePolicy struct {
	Dangling     AgePruneRule `yaml:"dangling,omitempty"`
	Unreferenced AgePruneRule `yaml:"unreferenced,omitempty"`
}

// BuildCachePrunePolicy defines build cache pruning rules.
type BuildCachePrunePolicy struct {
	OlderThan   string `yaml:"older_than,omitempty"`   // e.g. "72h"
	KeepStorage string `yaml:"keep_storage,omitempty"` // e.g. "20GB"
}

// ContainerPrunePolicy defines container pruning rules.
type ContainerPrunePolicy struct {
	Exited AgePruneRule `yaml:"exited,omitempty"`
}

// NetworkPrunePolicy defines network pruning rules.
type NetworkPrunePolicy struct {
	Unused bool `yaml:"unused,omitempty"`
}

// AgePruneRule is a simple age-based prune rule.
type AgePruneRule struct {
	OlderThan string `yaml:"older_than,omitempty"` // e.g. "72h"
}

// DefaultBuildCacheConfig returns baseline defaults.
// Mode is empty (inactive) — cleanup and cache only execute when mode is explicitly set.
// Governance presets should override enforcement and thresholds per runner class.
func DefaultBuildCacheConfig() BuildCacheConfig {
	namedTrue := true
	return BuildCacheConfig{
		Mode: "", // inactive until explicitly enabled
		Local: LocalCacheConfig{
			Retention: LocalRetention{
				MaxAge:  "7d",
				MaxSize: "15GB",
			},
		},
		External: ExternalCacheConfig{
			Mode: "max",
			Retention: ExternalRetention{
				MaxRefs:  20,
				StaleAge: "14d",
			},
		},
		Cleanup: HostCleanupConfig{
			Enabled:     false, // never touches host unless explicitly enabled
			Enforcement: "best_effort",
			Protect: ProtectionPolicy{
				Volumes: VolumeProtection{
					Named: &namedTrue,
				},
			},
			Prune: PrunePolicy{
				Images: ImagePrunePolicy{
					Dangling:     AgePruneRule{OlderThan: "72h"},
					Unreferenced: AgePruneRule{OlderThan: "72h"},
				},
				BuildCache: BuildCachePrunePolicy{
					OlderThan:   "72h",
					KeepStorage: "20GB",
				},
				Containers: ContainerPrunePolicy{
					Exited: AgePruneRule{OlderThan: "72h"},
				},
				Networks: NetworkPrunePolicy{
					Unused: true,
				},
			},
		},
	}
}
