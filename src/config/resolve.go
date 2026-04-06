package config

import (
	"fmt"
	"strings"
)

// ResolvedRepo is a fully resolved repo with all identity fields populated
// from the forge + repo graph. Ready for forge client creation.
//
// DefaultBranch is the flattened view of repo.Branches.Default — resolved
// once at load time so consumers don't need to know the nested shape.
type ResolvedRepo struct {
	ID            string
	Provider      string   // from forge
	BaseURL       string   // from forge
	Project       string   // from repo
	Credentials   string   // from forge
	Roles         []string // from repo (primary, mirror, publish-origin)
	DefaultBranch string   // flattened from repo.Branches.Default
	Worktree      string
	Ref           string
	Sync          SyncConfig
}

// HasRole returns true if the resolved repo has the given role.
func (r ResolvedRepo) HasRole(role string) bool {
	for _, s := range r.Roles {
		if s == role {
			return true
		}
	}
	return false
}

// ResolvedRegistry is a fully resolved registry target with all identity
// fields populated from the registry graph. Ready for buildx/push.
type ResolvedRegistry struct {
	ID          string
	Provider    string // from registry
	URL         string // from registry
	Path        string // from registry default_path or target override
	Credentials string // from registry
}

// ResolveRepo resolves a repo entry against the forge graph.
// Returns error if the referenced forge doesn't exist.
func ResolveRepo(repo RepoConfig, forges []ForgeConfig, vars map[string]string) (*ResolvedRepo, error) {
	forge := FindForgeByID(forges, repo.Forge)
	if forge == nil {
		return nil, fmt.Errorf("repo %s: forge %q not found", repo.ID, repo.Forge)
	}

	return &ResolvedRepo{
		ID:            repo.ID,
		Provider:      forge.Provider,
		BaseURL:       resolveVarsInline(forge.URL, vars),
		Project:       resolveVarsInline(repo.Project, vars),
		Credentials:   forge.Credentials,
		Roles:         repo.Roles,
		DefaultBranch: repo.Branches.Default,
		Worktree:      repo.Worktree,
		Ref:           repo.Ref,
		Sync:          repo.Sync,
	}, nil
}

// ResolveRegistryForTarget resolves a target's registry identity from the
// registry graph. Path comes from target override or registry default_path.
func ResolveRegistryForTarget(target TargetConfig, registries []RegistryConfig, vars map[string]string) (*ResolvedRegistry, error) {
	if target.Registry == "" {
		return nil, fmt.Errorf("target %s: registry: is required", target.ID)
	}

	reg := FindRegistryByID(registries, target.Registry)
	if reg == nil {
		return nil, fmt.Errorf("target %s: registry %q not found", target.ID, target.Registry)
	}

	path := reg.DefaultPath
	if target.Path != "" {
		path = target.Path // target override
	}

	return &ResolvedRegistry{
		ID:          reg.ID,
		Provider:    reg.Provider,
		URL:         resolveVarsInline(reg.URL, vars),
		Path:        resolveVarsInline(path, vars),
		Credentials: reg.Credentials,
	}, nil
}

// ResolveAllMirrors resolves all mirror repos against the forge graph.
func ResolveAllMirrors(repos []RepoConfig, forges []ForgeConfig, vars map[string]string) ([]*ResolvedRepo, error) {
	var mirrors []*ResolvedRepo
	for _, r := range repos {
		if !r.HasRole("mirror") {
			continue
		}
		resolved, err := ResolveRepo(r, forges, vars)
		if err != nil {
			return nil, err
		}
		mirrors = append(mirrors, resolved)
	}
	return mirrors, nil
}

// ResolvePrimary resolves the primary repo against the forge graph.
func ResolvePrimary(repos []RepoConfig, forges []ForgeConfig, vars map[string]string) (*ResolvedRepo, error) {
	primary := FindRepoWithRole(repos, "primary")
	if primary == nil {
		return nil, fmt.Errorf("no primary repo defined")
	}
	return ResolveRepo(*primary, forges, vars)
}

// PrimaryURL returns the full URL of the primary repo, or empty string.
// Thin wrapper over ResolvePrimary for call sites that only need the URL.
func PrimaryURL(cfg *Config) string {
	resolved, err := ResolvePrimary(cfg.Repos, cfg.Forges, cfg.Vars)
	if err != nil || resolved == nil {
		return ""
	}
	return resolved.BaseURL + "/" + resolved.Project
}

// PrimaryWorktree returns the worktree path of the primary repo, or ".".
func PrimaryWorktree(cfg *Config) string {
	resolved, err := ResolvePrimary(cfg.Repos, cfg.Forges, cfg.Vars)
	if err != nil || resolved == nil || resolved.Worktree == "" {
		return "."
	}
	return resolved.Worktree
}

// PrimaryDefaultBranch returns the default branch of the primary repo.
func PrimaryDefaultBranch(cfg *Config) string {
	resolved, err := ResolvePrimary(cfg.Repos, cfg.Forges, cfg.Vars)
	if err != nil || resolved == nil {
		return ""
	}
	return resolved.DefaultBranch
}

// resolveVarsInline does simple {var:name} replacement.
// This is a lightweight inline resolver for identity fields only.
// Full template resolution happens in the gitver package.
func resolveVarsInline(s string, vars map[string]string) string {
	if vars == nil || !strings.Contains(s, "{var:") {
		return s
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{var:"+k+"}", v)
	}
	return s
}
