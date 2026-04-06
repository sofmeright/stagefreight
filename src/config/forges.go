package config

import "fmt"

// ForgeConfig declares a git host. Declared once, referenced by repos.
// A forge is an identity — provider type, base URL, credentials.
type ForgeConfig struct {
	ID          string `yaml:"id"`                    // unique identifier (e.g., "prplanit-gitlab")
	Provider    string `yaml:"provider"`              // gitlab, github, gitea
	URL         string `yaml:"url"`                   // base URL (e.g., "https://gitlab.prplanit.com")
	Credentials string `yaml:"credentials,omitempty"` // env var prefix for token resolution
}

// RepoConfig declares a project on a forge. References forge by id.
// Roles are explicit and composable:
//   - "primary": exactly 1 required — authoritative source
//   - "mirror": 0 to many — derivative copies
//   - "publish-origin": 0 or 1 — where rendered artifacts are served from
//   - empty: addressable, no special behavior
//
// Forbidden combinations: primary + mirror (authoritative ≠ derivative).
// Allowed combinations: publish-origin + primary, publish-origin + mirror.
type RepoConfig struct {
	ID       string         `yaml:"id"`                 // unique identifier
	Forge    string         `yaml:"forge"`              // references forges[].id
	Project  string         `yaml:"project"`            // project path on the forge (e.g., "{var:gitlab_group}/{var:repo}")
	Roles    []string       `yaml:"roles,omitempty"`    // ["primary"] | ["mirror"] | ["mirror", "publish-origin"] | []
	Branches BranchesConfig `yaml:"branches,omitempty"` // branch identity (default, protected, etc.)
	Worktree string         `yaml:"worktree,omitempty"` // local working tree path (primary only)
	Ref      string         `yaml:"ref,omitempty"`      // pinned ref for non-primary repos (governance, presets)
	Sync     SyncConfig     `yaml:"sync,omitempty"`     // mirror sync domains
}

// BranchesConfig declares branch identity for a repo.
// Currently carries only Default; reserved for future additions like
// protected branches, release branches, etc. — those stay on the repo
// because they're topology, not versioning or promotion.
type BranchesConfig struct {
	// Default is the default branch name (e.g., "main"). Required for primary.
	Default string `yaml:"default,omitempty"`
}

// HasRole returns true if the repo has the given role.
func (r RepoConfig) HasRole(role string) bool {
	for _, s := range r.Roles {
		if s == role {
			return true
		}
	}
	return false
}

// RegistryConfig declares an OCI registry host. Declared once, referenced by targets.
type RegistryConfig struct {
	ID          string `yaml:"id"`                    // unique identifier (e.g., "dockerhub")
	Provider    string `yaml:"provider"`              // docker, harbor, ghcr, quay, gitea, generic
	URL         string `yaml:"url"`                   // registry URL (e.g., "docker.io")
	Credentials string `yaml:"credentials,omitempty"` // env var prefix for token resolution
	DefaultPath string `yaml:"default_path,omitempty"` // default image path (e.g., "{var:org}/{var:repo}")
}

// ValidateIdentityGraph checks structural invariants of forges, repos, and registries.
// Returns all errors found (not just the first).
func ValidateIdentityGraph(forges []ForgeConfig, repos []RepoConfig, registries []RegistryConfig) []string {
	var errs []string

	// Forge IDs unique.
	forgeIDs := make(map[string]bool)
	for _, f := range forges {
		if f.ID == "" {
			errs = append(errs, "forges: entry missing id")
			continue
		}
		if forgeIDs[f.ID] {
			errs = append(errs, fmt.Sprintf("forges: duplicate id %q", f.ID))
		}
		forgeIDs[f.ID] = true

		if f.Provider == "" {
			errs = append(errs, fmt.Sprintf("forges[%s]: provider is required", f.ID))
		}
		if f.URL == "" {
			errs = append(errs, fmt.Sprintf("forges[%s]: url is required", f.ID))
		}
	}

	// Repo IDs unique + forge references valid + role validation.
	repoIDs := make(map[string]bool)
	primaryCount := 0
	publishOriginCount := 0
	validRoles := map[string]bool{"primary": true, "mirror": true, "publish-origin": true}

	for _, r := range repos {
		if r.ID == "" {
			errs = append(errs, "repos: entry missing id")
			continue
		}
		if repoIDs[r.ID] {
			errs = append(errs, fmt.Sprintf("repos: duplicate id %q", r.ID))
		}
		repoIDs[r.ID] = true

		if r.Forge == "" {
			errs = append(errs, fmt.Sprintf("repos[%s]: forge is required", r.ID))
		} else if !forgeIDs[r.Forge] {
			errs = append(errs, fmt.Sprintf("repos[%s]: forge %q not found in forges", r.ID, r.Forge))
		}

		if r.Project == "" {
			errs = append(errs, fmt.Sprintf("repos[%s]: project is required", r.ID))
		}

		// Validate individual roles.
		for _, role := range r.Roles {
			if !validRoles[role] {
				errs = append(errs, fmt.Sprintf("repos[%s]: unknown role %q (expected primary, mirror, or publish-origin)", r.ID, role))
			}
		}

		// Forbidden combination: primary + mirror.
		if r.HasRole("primary") && r.HasRole("mirror") {
			errs = append(errs, fmt.Sprintf("repos[%s]: primary and mirror are mutually exclusive (authoritative ≠ derivative)", r.ID))
		}

		// Count roles for cardinality checks.
		if r.HasRole("primary") {
			primaryCount++
			if r.Branches.Default == "" {
				errs = append(errs, fmt.Sprintf("repos[%s]: branches.default is required for primary", r.ID))
			}
		}
		if r.HasRole("publish-origin") {
			publishOriginCount++
		}

		// Worktree only valid for primary.
		if r.Worktree != "" && !r.HasRole("primary") {
			errs = append(errs, fmt.Sprintf("repos[%s]: worktree is only valid for primary repos", r.ID))
		}

		// Mirror constraints.
		if r.HasRole("mirror") && r.Worktree != "" {
			errs = append(errs, fmt.Sprintf("repos[%s]: mirrors cannot have worktree", r.ID))
		}
	}

	if len(repos) > 0 && primaryCount == 0 {
		errs = append(errs, "repos: exactly one repo must have roles including primary")
	}
	if primaryCount > 1 {
		errs = append(errs, fmt.Sprintf("repos: exactly one primary allowed, found %d", primaryCount))
	}
	if publishOriginCount > 1 {
		errs = append(errs, fmt.Sprintf("repos: at most one publish-origin allowed, found %d", publishOriginCount))
	}

	// Registry IDs unique + required fields.
	registryIDs := make(map[string]bool)
	for _, r := range registries {
		if r.ID == "" {
			errs = append(errs, "registries: entry missing id")
			continue
		}
		if registryIDs[r.ID] {
			errs = append(errs, fmt.Sprintf("registries: duplicate id %q", r.ID))
		}
		registryIDs[r.ID] = true

		if r.URL == "" {
			errs = append(errs, fmt.Sprintf("registries[%s]: url is required", r.ID))
		}
		if r.Provider == "" {
			errs = append(errs, fmt.Sprintf("registries[%s]: provider is required", r.ID))
		}
	}

	// No mixed models — if new identity graph is used, legacy sources must not be.
	// (Enforced at call site in validate.go, not here.)

	return errs
}

// ValidateTargetRegistryRefs checks that targets with registry: reference existing registries,
// and that registry + inline fields don't conflict.
func ValidateTargetRegistryRefs(targets []TargetConfig, registries []RegistryConfig) []string {
	var errs []string
	registryIDs := make(map[string]bool)
	for _, r := range registries {
		registryIDs[r.ID] = true
	}

	for _, t := range targets {
		if t.Registry != "" {
			if !registryIDs[t.Registry] {
				errs = append(errs, fmt.Sprintf("targets[%s]: registry %q not found in registries", t.ID, t.Registry))
			}
			// Registry reference and inline fields must not coexist (except path override).
			if t.URL != "" {
				errs = append(errs, fmt.Sprintf("targets[%s]: url must not be set when registry is referenced", t.ID))
			}
			if t.Provider != "" {
				errs = append(errs, fmt.Sprintf("targets[%s]: provider must not be set when registry is referenced", t.ID))
			}
			if t.Credentials != "" {
				errs = append(errs, fmt.Sprintf("targets[%s]: credentials must not be set when registry is referenced", t.ID))
			}
			// path is allowed — overrides default_path
		}

		// Registry targets must have identity from somewhere.
		if (t.Kind == "registry" || t.Kind == "docker-readme") && t.Registry == "" && t.URL == "" {
			errs = append(errs, fmt.Sprintf("targets[%s]: kind %s requires registry: or url:", t.ID, t.Kind))
		}

		// Inline-mode registry targets (no registry: ref) must be a complete identity.
		if (t.Kind == "registry" || t.Kind == "docker-readme") && t.Registry == "" && t.URL != "" {
			if t.Kind == "registry" && t.Path == "" {
				errs = append(errs, fmt.Sprintf("targets[%s]: inline registry target requires path:", t.ID))
			}
		}
	}

	return errs
}

// FindForgeByID returns the forge with the given id, or nil.
func FindForgeByID(forges []ForgeConfig, id string) *ForgeConfig {
	for i := range forges {
		if forges[i].ID == id {
			return &forges[i]
		}
	}
	return nil
}

// FindRepoByID returns the repo with the given id, or nil.
func FindRepoByID(repos []RepoConfig, id string) *RepoConfig {
	for i := range repos {
		if repos[i].ID == id {
			return &repos[i]
		}
	}
	return nil
}

// FindRepoWithRole returns the first repo that has the given role, or nil.
func FindRepoWithRole(repos []RepoConfig, role string) *RepoConfig {
	for i := range repos {
		if repos[i].HasRole(role) {
			return &repos[i]
		}
	}
	return nil
}

// FindRegistryByID returns the registry with the given id, or nil.
func FindRegistryByID(registries []RegistryConfig, id string) *RegistryConfig {
	for i := range registries {
		if registries[i].ID == id {
			return &registries[i]
		}
	}
	return nil
}

// MirrorRepos returns all repos with the mirror role.
func MirrorRepos(repos []RepoConfig) []RepoConfig {
	var mirrors []RepoConfig
	for _, r := range repos {
		if r.HasRole("mirror") {
			mirrors = append(mirrors, r)
		}
	}
	return mirrors
}
