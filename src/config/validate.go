package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Validate checks structural invariants of a loaded v2 Config.
// Returns warnings (soft issues) and a hard error if the config is invalid.
func Validate(cfg *Config) (warnings []string, err error) {
	var errs []string

	// ── Version ───────────────────────────────────────────────────────────

	if cfg.Version != 1 {
		errs = append(errs, fmt.Sprintf("version: must be 1, got %d", cfg.Version))
	}

	// ── Policies ──────────────────────────────────────────────────────────

	for name := range cfg.Policies.GitTags {
		if !isIdentifier(name) {
			errs = append(errs, fmt.Sprintf("policies.git_tags: key %q is not a valid identifier (must match [a-zA-Z][a-zA-Z0-9_.\\-]*)", name))
		}
	}
	for name := range cfg.Policies.Branches {
		if !isIdentifier(name) {
			errs = append(errs, fmt.Sprintf("policies.branches: key %q is not a valid identifier (must match [a-zA-Z][a-zA-Z0-9_.\\-]*)", name))
		}
	}

	// ── Builds ────────────────────────────────────────────────────────────

	buildIDs := make(map[string]bool)
	for i, b := range cfg.Builds {
		bpath := fmt.Sprintf("builds[%d]", i)

		if b.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: id is required", bpath))
		} else if buildIDs[b.ID] {
			errs = append(errs, fmt.Sprintf("%s: duplicate build id %q", bpath, b.ID))
		} else {
			buildIDs[b.ID] = true
		}

		if b.Kind == "" {
			errs = append(errs, fmt.Sprintf("%s: kind is required", bpath))
		} else if b.Kind != "docker" {
			errs = append(errs, fmt.Sprintf("%s: unknown build kind %q (supported: docker)", bpath, b.Kind))
		}

		if b.BuildMode != "" && b.BuildMode != "crucible" {
			errs = append(errs, fmt.Sprintf("%s: unknown build_mode %q (supported: crucible)", bpath, b.BuildMode))
		}
	}

	// ── Targets ───────────────────────────────────────────────────────────

	targetIDs := make(map[string]bool)
	for i, t := range cfg.Targets {
		tpath := fmt.Sprintf("targets[%d]", i)

		if t.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: id is required", tpath))
		} else if targetIDs[t.ID] {
			errs = append(errs, fmt.Sprintf("%s: duplicate target id %q", tpath, t.ID))
		} else {
			targetIDs[t.ID] = true
		}

		if t.Kind == "" {
			errs = append(errs, fmt.Sprintf("%s: kind is required", tpath))
		} else if !validTargetKinds[t.Kind] {
			kinds := make([]string, 0, len(validTargetKinds))
			for k := range validTargetKinds {
				kinds = append(kinds, k)
			}
			errs = append(errs, fmt.Sprintf("%s: unknown target kind %q (supported: %s)", tpath, t.Kind, strings.Join(kinds, ", ")))
		}

		// Build reference validation
		if t.Build != "" && !buildIDs[t.Build] {
			errs = append(errs, fmt.Sprintf("%s: references unknown build %q", tpath, t.Build))
		}

		// Kind-specific validation
		terrs := validateTarget(t, tpath, buildIDs, cfg.Policies)
		errs = append(errs, terrs...)

		// When block validation
		werrs := validateWhen(t.When, tpath, cfg.Policies)
		errs = append(errs, werrs...)
	}

	// ── Narrator ──────────────────────────────────────────────────────────

	for fi, f := range cfg.Narrator {
		fpath := fmt.Sprintf("narrator[%d]", fi)

		if f.File == "" {
			errs = append(errs, fmt.Sprintf("%s: file is required", fpath))
		}

		itemIDs := make(map[string]bool)
		for ii, item := range f.Items {
			ipath := fmt.Sprintf("%s.items[%d]", fpath, ii)

			if item.ID != "" {
				if itemIDs[item.ID] {
					errs = append(errs, fmt.Sprintf("%s: duplicate item id %q", ipath, item.ID))
				}
				itemIDs[item.ID] = true
			}

			ierrs := validateNarratorItem(item, ipath)
			errs = append(errs, ierrs...)
		}
	}

	// ── Commit ────────────────────────────────────────────────────────────

	commitTypeKeys := make(map[string]bool)
	commitTypeKeyRe := regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	for i, ct := range cfg.Commit.Types {
		cpath := fmt.Sprintf("commit.types[%d]", i)

		if ct.Key == "" {
			errs = append(errs, fmt.Sprintf("%s: key is required", cpath))
			continue
		}
		if !commitTypeKeyRe.MatchString(ct.Key) {
			errs = append(errs, fmt.Sprintf("%s: key %q must match ^[a-z][a-z0-9_-]*$", cpath, ct.Key))
		}
		if commitTypeKeys[ct.Key] {
			errs = append(errs, fmt.Sprintf("%s: duplicate key %q", cpath, ct.Key))
		} else {
			commitTypeKeys[ct.Key] = true
		}

		if ct.AliasFor != "" {
			if !commitTypeKeys[ct.AliasFor] {
				// Check forward: is target defined later?
				found := false
				for _, other := range cfg.Commit.Types {
					if other.Key == ct.AliasFor {
						found = true
						break
					}
				}
				if !found {
					errs = append(errs, fmt.Sprintf("%s: alias_for %q references unknown type", cpath, ct.AliasFor))
				}
			}
			// Check alias doesn't target another alias (no chains)
			for _, other := range cfg.Commit.Types {
				if other.Key == ct.AliasFor && other.AliasFor != "" {
					errs = append(errs, fmt.Sprintf("%s: alias_for %q targets another alias (chains not allowed)", cpath, ct.AliasFor))
				}
			}
		}
	}

	// ── Dependency ───────────────────────────────────────────────────────

	if cfg.Dependency.Output != "" {
		if pathErrs := validateOutputPath(cfg.Dependency.Output, "dependency.output"); len(pathErrs) > 0 {
			errs = append(errs, pathErrs...)
		}
	}
	if cfg.Dependency.Commit.Type != "" {
		commitTypeKeyRe2 := regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
		if !commitTypeKeyRe2.MatchString(cfg.Dependency.Commit.Type) {
			errs = append(errs, fmt.Sprintf("dependency.commit.type: %q must match ^[a-z][a-z0-9_-]*$", cfg.Dependency.Commit.Type))
		}
	}
	if cfg.Dependency.Enabled {
		if !cfg.Dependency.Scope.GoModules && !cfg.Dependency.Scope.DockerfileEnv {
			errs = append(errs, "dependency: at least one scope must be true when enabled")
		}
		if cfg.Dependency.Commit.Enabled && cfg.Dependency.Commit.Message == "" {
			errs = append(errs, "dependency.commit: message is required when commit enabled")
		}
	}

	// ── Docs ─────────────────────────────────────────────────────────────

	if cfg.Docs.Commit.Type != "" {
		commitTypeKeyRe3 := regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
		if !commitTypeKeyRe3.MatchString(cfg.Docs.Commit.Type) {
			errs = append(errs, fmt.Sprintf("docs.commit.type: %q must match ^[a-z][a-z0-9_-]*$", cfg.Docs.Commit.Type))
		}
	}
	for i, p := range cfg.Docs.Commit.Add {
		if pathErrs := validateOutputPath(p, fmt.Sprintf("docs.commit.add[%d]", i)); len(pathErrs) > 0 {
			errs = append(errs, pathErrs...)
		}
	}
	if cfg.Docs.Enabled {
		g := cfg.Docs.Generators
		if !g.Badges && !g.ReferenceDocs && !g.Narrator && !g.DockerReadme {
			errs = append(errs, "docs: at least one generator must be true when enabled")
		}
		if cfg.Docs.Commit.Enabled && cfg.Docs.Commit.Message == "" {
			errs = append(errs, "docs.commit: message is required when commit enabled")
		}
	}

	// ── Manifest ────────────────────────────────────────────────────

	if !validManifestModes[cfg.Manifest.Mode] {
		errs = append(errs, fmt.Sprintf("manifest.mode: unknown mode %q (supported: ephemeral, workspace, commit, publish)", cfg.Manifest.Mode))
	}
	if cfg.Manifest.OutputDir != "" {
		if pathErrs := validateOutputPath(cfg.Manifest.OutputDir, "manifest.output_dir"); len(pathErrs) > 0 {
			errs = append(errs, pathErrs...)
		}
	}

	// ── Security ─────────────────────────────────────────────────────────

	if cfg.Security.OutputDir != "" {
		if pathErrs := validateOutputPath(cfg.Security.OutputDir, "security.output"); len(pathErrs) > 0 {
			errs = append(errs, pathErrs...)
		}
	}

	// ── Release ──────────────────────────────────────────────────────────

	if cfg.Release.SecuritySummary != "" {
		if pathErrs := validateOutputPath(cfg.Release.SecuritySummary, "release.security_summary"); len(pathErrs) > 0 {
			errs = append(errs, pathErrs...)
		}
	}

	if len(errs) > 0 {
		return warnings, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return warnings, nil
}

// validateTarget checks kind-specific field constraints on a target.
func validateTarget(t TargetConfig, path string, buildIDs map[string]bool, policies PoliciesConfig) []string {
	var errs []string

	switch t.Kind {
	case "registry":
		if t.Build == "" {
			errs = append(errs, fmt.Sprintf("%s: kind registry requires build reference", path))
		}
		if t.URL == "" {
			errs = append(errs, fmt.Sprintf("%s: kind registry requires url", path))
		}
		if t.Path == "" {
			errs = append(errs, fmt.Sprintf("%s: kind registry requires path", path))
		}
		// Disallow release-only fields
		if len(t.Aliases) > 0 {
			errs = append(errs, fmt.Sprintf("%s: aliases is not valid for kind registry (use tags)", path))
		}
		if t.SyncRelease || t.SyncAssets {
			errs = append(errs, fmt.Sprintf("%s: sync_release/sync_assets are not valid for kind registry", path))
		}

	case "docker-readme":
		if t.URL == "" {
			errs = append(errs, fmt.Sprintf("%s: kind docker-readme requires url", path))
		}
		if t.Path == "" {
			errs = append(errs, fmt.Sprintf("%s: kind docker-readme requires path", path))
		}
		if t.Build != "" {
			errs = append(errs, fmt.Sprintf("%s: kind docker-readme does not use build reference", path))
		}

	case "gitlab-component":
		if len(t.SpecFiles) == 0 {
			errs = append(errs, fmt.Sprintf("%s: kind gitlab-component requires spec_files", path))
		}
		if t.Build != "" {
			errs = append(errs, fmt.Sprintf("%s: kind gitlab-component does not use build reference", path))
		}

	case "release":
		// Primary vs remote mode validation
		remoteFields := 0
		if t.Provider != "" {
			remoteFields++
		}
		if t.URL != "" {
			remoteFields++
		}
		if t.ProjectID != "" {
			remoteFields++
		}
		if t.Credentials != "" {
			remoteFields++
		}

		if remoteFields > 0 && remoteFields < 4 {
			errs = append(errs, fmt.Sprintf("%s: remote release requires all of provider, url, project_id, credentials (got %d of 4)", path, remoteFields))
		}

		isPrimary := remoteFields == 0
		if isPrimary {
			if t.SyncRelease {
				errs = append(errs, fmt.Sprintf("%s: sync_release is only valid for remote release targets", path))
			}
			if t.SyncAssets {
				errs = append(errs, fmt.Sprintf("%s: sync_assets is only valid for remote release targets", path))
			}
		}

		if t.Build != "" {
			errs = append(errs, fmt.Sprintf("%s: kind release does not use build reference", path))
		}
	}

	return errs
}

// validateWhen checks the when block for valid policy references and events.
func validateWhen(w TargetCondition, path string, policies PoliciesConfig) []string {
	var errs []string

	for _, entry := range w.GitTags {
		if strings.HasPrefix(entry, "re:") {
			continue // inline regex, skip policy lookup
		}
		if !isIdentifier(entry) {
			continue // not a policy name, will be treated as regex by match logic
		}
		if _, ok := policies.GitTags[entry]; !ok {
			errs = append(errs, fmt.Sprintf("%s.when.git_tags: unknown policy %q (not in policies.git_tags)", path, entry))
		}
	}

	for _, entry := range w.Branches {
		if strings.HasPrefix(entry, "re:") {
			continue
		}
		if !isIdentifier(entry) {
			continue
		}
		if _, ok := policies.Branches[entry]; !ok {
			errs = append(errs, fmt.Sprintf("%s.when.branches: unknown policy %q (not in policies.branches)", path, entry))
		}
	}

	for _, event := range w.Events {
		if !validEvents[event] {
			events := make([]string, 0, len(validEvents))
			for e := range validEvents {
				events = append(events, e)
			}
			errs = append(errs, fmt.Sprintf("%s.when.events: unknown event %q (supported: %s)", path, event, strings.Join(events, ", ")))
		}
	}

	return errs
}

// validateNarratorItem checks kind, placement, and field constraints for a narrator item.
func validateNarratorItem(item NarratorItem, path string) []string {
	var errs []string

	// Kind validation
	if item.Kind == "" {
		errs = append(errs, fmt.Sprintf("%s: kind is required", path))
		return errs
	}
	if !validNarratorItemKinds[item.Kind] {
		kinds := make([]string, 0, len(validNarratorItemKinds))
		for k := range validNarratorItemKinds {
			kinds = append(kinds, k)
		}
		errs = append(errs, fmt.Sprintf("%s: unknown narrator item kind %q (supported: %s)", path, item.Kind, strings.Join(kinds, ", ")))
		return errs
	}

	// Placement validation (break kind doesn't need placement,
	// build-contents can use output_file instead — validated in kind-specific block)
	if item.Kind != "break" && item.Kind != "build-contents" {
		if !hasPlacementSelector(item.Placement) {
			errs = append(errs, fmt.Sprintf("%s: placement requires at least one selector (between, after, before, or heading)", path))
		}
	}

	// Placement mode validation
	if !validPlacementModes[item.Placement.Mode] {
		errs = append(errs, fmt.Sprintf("%s: unknown placement mode %q", path, item.Placement.Mode))
	}

	// Kind-specific validation
	switch item.Kind {
	case "badge":
		if item.Text == "" {
			errs = append(errs, fmt.Sprintf("%s: kind badge requires text (badge label)", path))
		}
		if item.Output != "" {
			if pathErrs := validateOutputPath(item.Output, path); len(pathErrs) > 0 {
				errs = append(errs, pathErrs...)
			}
		}

	case "shield":
		if item.Shield == "" {
			errs = append(errs, fmt.Sprintf("%s: kind shield requires shield (shields.io path)", path))
		}

	case "text":
		if item.Content == "" {
			errs = append(errs, fmt.Sprintf("%s: kind text requires content", path))
		}

	case "component":
		if item.Spec == "" {
			errs = append(errs, fmt.Sprintf("%s: kind component requires spec (component spec file path)", path))
		}

	case "include":
		if item.Path == "" {
			errs = append(errs, fmt.Sprintf("%s: kind include requires path (file path to include)", path))
		}

	case "props":
		if item.Type == "" {
			errs = append(errs, fmt.Sprintf("%s: kind props requires type (props resolver type ID)", path))
		}

	case "build-contents":
		if item.Section == "" {
			errs = append(errs, fmt.Sprintf("%s: kind build-contents requires section (dot-path into manifest)", path))
		}
		if item.Renderer == "" {
			errs = append(errs, fmt.Sprintf("%s: kind build-contents requires renderer (table, list, or kv)", path))
		} else if item.Renderer != "table" && item.Renderer != "list" && item.Renderer != "kv" {
			errs = append(errs, fmt.Sprintf("%s: unknown renderer %q (supported: table, list, kv)", path, item.Renderer))
		}
		if item.OutputFile != "" {
			if pathErrs := validateOutputPath(item.OutputFile, path+".output_file"); len(pathErrs) > 0 {
				errs = append(errs, pathErrs...)
			}
		}
		// build-contents can work with either placement (section embedding) or output_file, or both
		// but needs at least one destination
		if !hasPlacementSelector(item.Placement) && item.OutputFile == "" {
			errs = append(errs, fmt.Sprintf("%s: kind build-contents requires placement selector or output_file (at least one destination)", path))
		}
	}

	return errs
}

// hasPlacementSelector returns true if at least one placement selector is set.
func hasPlacementSelector(p NarratorPlacement) bool {
	return (p.Between != [2]string{}) || p.After != "" || p.Before != "" || p.Heading != ""
}

// validateOutputPath checks that an output path is safe.
func validateOutputPath(p string, itemPath string) []string {
	var errs []string

	if p == "" {
		errs = append(errs, fmt.Sprintf("%s: output path is empty", itemPath))
		return errs
	}

	// Absolute path
	if filepath.IsAbs(p) {
		errs = append(errs, fmt.Sprintf("%s: output path %q must be relative, not absolute", itemPath, p))
		return errs
	}

	// Tilde
	if strings.HasPrefix(p, "~") {
		errs = append(errs, fmt.Sprintf("%s: output path %q must not start with ~", itemPath, p))
		return errs
	}

	// Windows drive prefix
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		errs = append(errs, fmt.Sprintf("%s: output path %q looks like a Windows drive path", itemPath, p))
		return errs
	}

	// Path traversal
	if strings.Contains(p, "..") {
		errs = append(errs, fmt.Sprintf("%s: output path %q must not contain '..'", itemPath, p))
		return errs
	}

	// Normalize: strip leading ./ then compare with filepath.Clean
	normalized := strings.TrimPrefix(p, "./")
	clean := filepath.Clean(normalized)
	if clean != normalized {
		errs = append(errs, fmt.Sprintf("%s: output path %q is not in canonical form (cleaned to %q)", itemPath, p, clean))
		return errs
	}

	return errs
}
