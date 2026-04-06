package config

// VersioningConfig controls how version identity is derived from git state.
// This is the single source of truth for version meaning — git provides raw
// facts (tags, commits, branches), versioning rules decide what they mean.
//
// Precedence: tag match > branch_builds > no_lineage
//
// A branch defines a path through tag sources; the first reachable match
// defines the version. That is the entire versioning contract.
type VersioningConfig struct {
	// TagSources defines named places where version bases can come from.
	// e.g., {id: "stable", pattern: "^v?\\d+\\.\\d+\\.\\d+$"}
	// Globally eligible, no inherent priority. Order is declaration-preserving
	// but semantically flat — priority lives on branch rules, not here.
	TagSources []TagSourceConfig `yaml:"tag_sources"`

	// BranchBuilds defines version format for non-tag commits per branch.
	// Evaluated in declaration order; first matching rule wins. An entry with
	// id: "default" is required and must be the last entry — it catches any
	// branch that did not match a named rule.
	BranchBuilds []BranchBuildConfig `yaml:"branch_builds,omitempty"`

	// NoLineage defines behavior when no tag lineage exists (no matching tags).
	NoLineage NoLineageConfig `yaml:"no_lineage,omitempty"`
}

// TagSourceConfig is a named tag pattern used for version lineage candidates.
type TagSourceConfig struct {
	// ID is the unique identifier (e.g. "stable", "prerelease").
	// Referenced by branch_builds[].base_from and target.when.git_tags.
	ID string `yaml:"id"`

	// Pattern is the regex that identifies tags belonging to this source.
	// e.g., "^v?\\d+\\.\\d+\\.\\d+$"
	Pattern string `yaml:"pattern"`
}

// BranchBuildConfig defines how dev versions are formatted for a branch.
type BranchBuildConfig struct {
	// ID is the unique identifier. "default" is the catch-all entry and must
	// appear last in the BranchBuilds slice.
	ID string `yaml:"id"`

	// Match references a declared branch matcher name. Required for named
	// branch_builds entries. The "default" entry rejects Match — it catches
	// any branch that did not match a named entry.
	Match string `yaml:"match,omitempty"`

	// BaseFrom is the ordered fallback chain of tag_sources ids. The runtime
	// walks this list in order; for each source id, it scans tags for a match
	// and returns the first hit. Fallback advances only when a source yields
	// zero matches.
	//
	// Reads as "look in prerelease first; if nothing, look in stable":
	//   base_from: [prerelease, stable]
	//
	// It does NOT mean "prerelease beats stable by priority" — ordering is a
	// declared search path, not a ranking.
	BaseFrom []string `yaml:"base_from"`

	// Format is the version template for non-release commits.
	// Supported placeholders: {base}, {sha}, {branch}
	// e.g., "{base}-dev+{sha}"
	Format string `yaml:"format"`
}

// NoLineageConfig defines behavior when no tag source yields a match along
// the branch rule's base_from search path.
type NoLineageConfig struct {
	// Mode controls the response to missing lineage.
	//   "error" (default): fail fast with explanation and suggested fix
	//   "explicit": use the provided version template
	Mode string `yaml:"mode,omitempty"`

	// Version is the template used when mode is "explicit".
	// Must contain {sha} or {time} — hardcoded versions are rejected.
	// e.g., "0.1.0-bootstrap+{sha}"
	Version string `yaml:"version,omitempty"`
}

// DefaultVersioningConfig returns a versioning config with sensible defaults.
func DefaultVersioningConfig() VersioningConfig {
	return VersioningConfig{
		TagSources:   []TagSourceConfig{},
		BranchBuilds: []BranchBuildConfig{},
		NoLineage:    NoLineageConfig{Mode: "error"},
	}
}
