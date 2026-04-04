// Package governance implements the StageFreight governance engine:
// preset resolution, two-file config merge, governance reconciliation,
// capability detection, and execution gating.
package governance

// GovernanceSource declares where governance inputs come from.
// Declared in .stagefreight.yml under governance.source.
type GovernanceSource struct {
	RepoURL       string `yaml:"repo_url"`       // policy repo URL
	Ref           string `yaml:"ref"`             // pinned tag or commit SHA (required)
	Path          string `yaml:"path"`            // path to governance config within repo
	AllowFloating bool   `yaml:"allow_floating"`  // if true, branch refs allowed (dev/unsafe)
	LocalPath     string `yaml:"-"`               // if set, use local checkout instead of cloning
}

// GovernanceConfig is the parsed clusters.yml from the policy repo.
type GovernanceConfig struct {
	Clusters []Cluster `yaml:"clusters"`
}

// Cluster assigns lifecycle doctrine to a group of repos.
// The StageFreight block is normal StageFreight config — same grammar.
// Assets (CI skeletons, settings files, etc.) are declared inside the
// stagefreight config as assets: entries — no separate skeleton construct.
type Cluster struct {
	ID      string         `yaml:"id"`
	Targets ClusterTargets `yaml:"targets"`
	Config  map[string]any `yaml:"stagefreight"` // raw StageFreight config
}

// ClusterTargets identifies which repos belong to this cluster.
type ClusterTargets struct {
	Repos []string `yaml:"repos"` // "org/repo" or "org/group/repo"
}

// PresetRef is a reference to an external preset fragment.
// Appears as preset: "path" within any config section.
type PresetRef struct {
	Path string
}

// ResolvedPreset is a loaded and validated preset fragment.
type ResolvedPreset struct {
	Path        string         // source path within policy repo
	TopLevelKey string         // the single top-level key this preset declares
	Content     map[string]any // parsed YAML content under that key
}

// MergeTrace records how each config value was resolved.
type MergeTrace struct {
	Entries []MergeEntry
}

// MergeEntry records the provenance of a single config value.
type MergeEntry struct {
	Path         string // dot-path (e.g., "security.sbom")
	Source       string // "managed", "local", "preset:preset/security.yml"
	SourceRef    string // "PrPlanIT/MaintenancePolicy@v1.0.0" for presets
	Layer        int    // resolution depth (0=innermost preset, N=outermost, N+1=managed, N+2=local)
	Operation    string // "set", "override", "merge", "replace"
	Value        any
	Overridden   bool
	OverriddenBy string
}

// DetectionReport is the output of capability discovery.
type DetectionReport struct {
	Capabilities []CapabilityResult
}

// CapabilityResult records whether a specific capability was detected.
type CapabilityResult struct {
	Domain     string   // e.g., "build.docker", "build.binary", "package.helm"
	Detected   bool
	Confidence string   // "high", "medium", "low"
	Evidence   []string // filesystem signals that supported detection
}

// ExecutionPlan is the gated output — what will actually run.
// Produced by GateExecution. Does NOT modify config.
type ExecutionPlan struct {
	Enabled []EnabledFeature
	Skipped []SkippedFeature
}

// EnabledFeature is a feature that passed both config and capability gates.
type EnabledFeature struct {
	Domain string
	Reason string // "config enabled + capability detected"
}

// SkippedFeature is a feature that was gated out.
type SkippedFeature struct {
	Domain string
	Reason string // "capability not detected" or "config disabled"
}

// DistributionPlan describes what files to write to a target repo.
type DistributionPlan struct {
	Repo  string             // "org/repo"
	Files []DistributedFile
}

// DistributedFile is a single file to write/update in a target repo.
type DistributedFile struct {
	Path    string // e.g., ".stagefreight/stagefreight-managed.yml"
	Content []byte
	Action  string // "create", "replace", "unchanged", "delete"
	Drifted bool   // true if existing file differs from governance intent
}

// CommitResult records what happened for each repo during distribution.
type CommitResult struct {
	Repo    string
	Status  string // "committed", "unchanged", "dry-run", "skipped-identical", "error"
	SHA     string // commit SHA if committed
	Message string
	Drifted bool   // true if managed file was drifted before replacement
	Error   error
}

// CanonicalKeyOrder defines the fixed top-level key order for rendered YAML.
// Prevents noisy diffs and unstable commits.
var CanonicalKeyOrder = []string{
	"version",
	"vars",
	"sources",
	"policies",
	"builds",
	"targets",
	"badges",
	"narrator",
	"lint",
	"security",
	"dependency",
	"docs",
	"commit",
	"release",
	"lifecycle",
	"gitops",
	"docker",
	"glossary",
	"presentation",
	"tag",
	"manifest",
}
