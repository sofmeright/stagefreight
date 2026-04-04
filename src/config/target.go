package config

// TargetConfig defines a distribution target or side-effect. Each target has a
// unique ID and a kind that determines which fields are valid.
//
// This is a discriminated union keyed by Kind. Only fields relevant to the kind
// should be set — validated at load time.
//
// Target kinds:
//   - registry: Push image tags to a container registry (requires build reference)
//   - docker-readme: Sync README to a container registry (standalone)
//   - gitlab-component: Publish to GitLab CI component catalog (standalone)
//   - release: Create forge release + rolling git tags (standalone)
type TargetConfig struct {
	// ID is the unique identifier for this target (logging, status, enable/disable).
	ID string `yaml:"id"`

	// Kind is the target type. Determines which fields are valid.
	Kind string `yaml:"kind"`

	// Build references a BuildConfig.ID. Required for kind: registry.
	Build string `yaml:"build,omitempty"`

	// When specifies routing conditions for this target.
	When TargetCondition `yaml:"when,omitempty"`

	// RunFrom gates mutation to declared execution origins.
	// e.g. ["primary"] — only mutate when running from sources.primary.
	// Empty = no restriction (backward compat).
	RunFrom RunFromConfig `yaml:"run_from,omitempty"`

	// SelectTags enables CLI filtering via --select.
	SelectTags []string `yaml:"select_tags,omitempty"`

	// ── Shared fields (used by multiple kinds) ────────────────────────────

	// URL is the registry/forge hostname (kind: registry, docker-readme, release).
	URL string `yaml:"url,omitempty"`

	// Provider is the vendor type for auth and API behavior.
	// Registry: docker, ghcr, gitlab, jfrog, harbor, quay, gitea, generic.
	// Release: github, gitlab, gitea.
	// If omitted on registry/docker-readme, inferred from URL.
	Provider string `yaml:"provider,omitempty"`

	// Path is the image path within the registry (kind: registry, docker-readme).
	Path string `yaml:"path,omitempty"`

	// Credentials is the env var prefix for authentication.
	// Resolution: try {PREFIX}_TOKEN first, else {PREFIX}_USER + {PREFIX}_PASS.
	Credentials string `yaml:"credentials,omitempty"`

	// Description is a short description override (kind: registry, docker-readme).
	Description string `yaml:"description,omitempty"`

	// Retention controls cleanup of old tags/releases.
	// Structured only in v2 (no scalar shorthand).
	Retention *RetentionPolicy `yaml:"retention,omitempty"`

	// ── kind: registry ────────────────────────────────────────────────────

	// Tags are tag templates resolved against version info (kind: registry).
	// e.g., ["{version}", "{major}.{minor}", "latest"]
	Tags []string `yaml:"tags,omitempty"`

	// NativeScan enables post-push vulnerability scanning via the registry's own built-in scanner.
	// Distinct from StageFreight's own scan pipeline (Trivy/Grype run by StageFreight itself).
	// Currently supported: Harbor (triggers Harbor's built-in Trivy after each push).
	// No-op for Docker Hub, GHCR, Quay, JFrog, and other providers.
	// Best-effort — scan failures warn but do not fail the build.
	// Push success does not imply scan success; results appear in the registry UI only.
	NativeScan bool `yaml:"native_scan,omitempty"`

	// ── kind: docker-readme ───────────────────────────────────────────────

	// File is the path to the README file (kind: docker-readme).
	File string `yaml:"file,omitempty"`

	// LinkBase is the base URL for relative link rewriting (kind: docker-readme).
	LinkBase string `yaml:"link_base,omitempty"`

	// ── kind: gitlab-component ────────────────────────────────────────────

	// SpecFiles lists component spec file paths (kind: gitlab-component).
	SpecFiles []string `yaml:"spec_files,omitempty"`

	// Catalog enables GitLab Catalog registration (kind: gitlab-component).
	Catalog bool `yaml:"catalog,omitempty"`

	// ── kind: release ─────────────────────────────────────────────────────

	// Aliases are rolling git tag aliases (kind: release).
	// e.g., ["{version}", "{major}.{minor}", "latest"]
	// Named "aliases" to avoid collision with Tags (image tags) and git_tags (policy filters).
	Aliases []string `yaml:"aliases,omitempty"`

	// ProjectID is the "owner/repo" or numeric ID for remote forge targets (kind: release).
	ProjectID string `yaml:"project_id,omitempty"`

	// Mirror references a sources.mirrors[].id for release sync.
	// Forge identity (provider, url, project_id, credentials) is resolved from the mirror.
	// Avoids restating forge connection details in the target.
	Mirror string `yaml:"mirror,omitempty"`

	// SyncRelease syncs release notes + tags to a remote forge (kind: release, remote only).
	SyncRelease bool `yaml:"sync_release,omitempty"`

	// SyncAssets syncs scan artifacts to a remote forge (kind: release, remote only).
	SyncAssets bool `yaml:"sync_assets,omitempty"`

	// ── kind: binary-archive ──────────────────────────────────────────────

	// Archives references a binary-archive target ID (kind: release only).
	Archives string `yaml:"archives,omitempty"`

	// BinaryName overrides the binary name inside the archive (kind: binary-archive).
	// Auto-detected from referenced build if omitted.
	BinaryName string `yaml:"binary_name,omitempty"`

	// Format is the archive format: "tar.gz", "zip", or "auto" (kind: binary-archive).
	// "auto" picks zip for windows, tar.gz for everything else. Default: "auto".
	Format string `yaml:"format,omitempty"`

	// Name is the archive filename template (kind: binary-archive).
	// Supports: {id}, {version}, {os}, {arch}. e.g., "{id}-{version}-{os}-{arch}".
	Name string `yaml:"name,omitempty"`

	// Include lists extra files to bundle into the archive (kind: binary-archive).
	// e.g., ["README.md", "LICENSE"]
	Include []string `yaml:"include,omitempty"`

	// Checksums generates a SHA256SUMS file alongside archives (kind: binary-archive).
	Checksums bool `yaml:"checksums,omitempty"`
}

// IsRemoteRelease returns true if this release target references a remote forge,
// either via explicit forge fields or via a mirror reference.
func (t TargetConfig) IsRemoteRelease() bool {
	if t.Mirror != "" {
		return true
	}
	return t.Provider != "" && t.URL != "" && t.ProjectID != "" && t.Credentials != ""
}

// TargetCondition defines routing conditions for a target.
// All non-empty fields must match (AND logic).
type TargetCondition struct {
	// Branches lists branch filters. Each entry is a policy name or "re:<regex>".
	// Empty = no branch filtering.
	Branches []string `yaml:"branches,omitempty"`

	// GitTags lists git tag filters. Each entry is a policy name or "re:<regex>".
	// Empty = no tag filtering.
	GitTags []string `yaml:"git_tags,omitempty"`

	// Events lists CI event type filters.
	// Supported: push, tag, release, schedule, manual, pull_request, merge_request.
	// Empty = no event filtering.
	Events []string `yaml:"events,omitempty"`
}

// validTargetKinds enumerates all recognized target kinds.
var validTargetKinds = map[string]bool{
	"registry":         true,
	"docker-readme":    true,
	"gitlab-component": true,
	"release":          true,
	"binary-archive":   true,
}

// validArchiveFormats enumerates all recognized archive formats.
var validArchiveFormats = map[string]bool{
	"":       true, // default → "auto"
	"auto":   true,
	"tar.gz": true,
	"zip":    true,
}

// validEvents enumerates all recognized event types.
var validEvents = map[string]bool{
	"push":          true,
	"tag":           true,
	"release":       true,
	"schedule":      true,
	"manual":        true,
	"pull_request":  true,
	"merge_request": true,
}
