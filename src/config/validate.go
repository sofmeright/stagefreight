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

	// ── Source Mirrors ───────────────────────────────────────────────────

	mirrorIDs := make(map[string]bool)
	validProviders := map[string]bool{"github": true, "gitlab": true, "gitea": true}
	for i, m := range cfg.Sources.Mirrors {
		mpath := fmt.Sprintf("sources.mirrors[%d]", i)

		if m.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: id is required", mpath))
		} else if mirrorIDs[m.ID] {
			errs = append(errs, fmt.Sprintf("%s: duplicate mirror id %q", mpath, m.ID))
		} else {
			mirrorIDs[m.ID] = true
		}

		if m.Provider == "" {
			errs = append(errs, fmt.Sprintf("%s: provider is required", mpath))
		} else if !validProviders[m.Provider] {
			errs = append(errs, fmt.Sprintf("%s: unknown provider %q (supported: github, gitlab, gitea)", mpath, m.Provider))
		}

		if m.URL == "" {
			errs = append(errs, fmt.Sprintf("%s: url is required", mpath))
		}
		if m.ProjectID == "" {
			errs = append(errs, fmt.Sprintf("%s: project_id is required", mpath))
		}
		if m.Credentials == "" {
			errs = append(errs, fmt.Sprintf("%s: credentials is required", mpath))
		}

		// At least one sync domain must be enabled
		if !m.Sync.Git && !m.Sync.Releases && !m.Sync.Docs {
			errs = append(errs, fmt.Sprintf("%s: at least one sync domain must be enabled (git, releases, docs)", mpath))
		}

		// git + docs is mutually exclusive — docs arrive through git mirror
		if m.Sync.Git && m.Sync.Docs {
			errs = append(errs, fmt.Sprintf("%s: sync.git and sync.docs are mutually exclusive (docs arrive through git mirror)", mpath))
		}

		// releases without git — warn, don't error
		if m.Sync.Releases && !m.Sync.Git {
			warnings = append(warnings, fmt.Sprintf("%s: sync.releases without sync.git — release projection does not guarantee referenced commits exist on mirror", mpath))
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
		} else if b.Kind != "docker" && b.Kind != "binary" {
			errs = append(errs, fmt.Sprintf("%s: unknown build kind %q (supported: docker, binary)", bpath, b.Kind))
		}

		// DependsOn reference validation (deferred until all IDs collected)
		// Binary-specific validation
		if b.Kind == "binary" {
			if b.Builder == "" {
				errs = append(errs, fmt.Sprintf("%s: kind binary requires builder (supported: go)", bpath))
			} else if b.Builder != "go" {
				errs = append(errs, fmt.Sprintf("%s: unknown builder %q (supported: go)", bpath, b.Builder))
			}
			if b.From == "" {
				errs = append(errs, fmt.Sprintf("%s: kind binary requires from (source entry point)", bpath))
			}
			// Docker-only fields should not be set on binary builds
			if b.Dockerfile != "" {
				errs = append(errs, fmt.Sprintf("%s: dockerfile is not valid for kind binary", bpath))
			}
			if b.Context != "" {
				errs = append(errs, fmt.Sprintf("%s: context is not valid for kind binary", bpath))
			}
			if b.Target != "" {
				errs = append(errs, fmt.Sprintf("%s: target is not valid for kind binary", bpath))
			}
			if len(b.BuildArgs) > 0 {
				errs = append(errs, fmt.Sprintf("%s: build_args is not valid for kind binary (use args)", bpath))
			}
		}

		// Docker-only: binary fields should not be set
		if b.Kind == "docker" {
			if b.Builder != "" {
				errs = append(errs, fmt.Sprintf("%s: builder is not valid for kind docker", bpath))
			}
			if b.From != "" {
				errs = append(errs, fmt.Sprintf("%s: from is not valid for kind docker", bpath))
			}
			if len(b.Args) > 0 {
				errs = append(errs, fmt.Sprintf("%s: args is not valid for kind docker", bpath))
			}
			if len(b.Env) > 0 {
				errs = append(errs, fmt.Sprintf("%s: env is not valid for kind docker", bpath))
			}
		}

		if b.BuildMode != "" && b.BuildMode != "crucible" {
			errs = append(errs, fmt.Sprintf("%s: unknown build_mode %q (supported: crucible)", bpath, b.BuildMode))
		}
	}

	// ── Build depends_on validation (all IDs now known) ─────────────────

	for i, b := range cfg.Builds {
		if b.DependsOn != "" {
			bpath := fmt.Sprintf("builds[%d]", i)
			if !buildIDs[b.DependsOn] {
				errs = append(errs, fmt.Sprintf("%s: depends_on references unknown build %q", bpath, b.DependsOn))
			}
			if b.DependsOn == b.ID {
				errs = append(errs, fmt.Sprintf("%s: depends_on cannot reference itself", bpath))
			}
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

	// ── Sources: publish_origin ──────────────────────────────────────────

	if po := cfg.Sources.PublishOrigin; po != nil {
		// default_branch is required for forge-based resolution (primary and mirror)
		if po.Kind == "primary" || po.Kind == "mirror" {
			if cfg.Sources.Primary.DefaultBranch == "" {
				errs = append(errs, "sources.primary.default_branch is required when publish_origin is used")
			}
		}
		switch po.Kind {
		case "primary":
			if cfg.Sources.Primary.URL == "" {
				errs = append(errs, "sources.publish_origin (kind: primary): sources.primary.url is required")
			}
		case "mirror":
			if po.Ref == "" {
				errs = append(errs, "sources.publish_origin (kind: mirror): ref is required")
			} else if FindMirrorByID(cfg.Sources.Mirrors, po.Ref) == nil {
				errs = append(errs, fmt.Sprintf("sources.publish_origin ref %q not found in sources.mirrors", po.Ref))
			}
		case "url":
			if po.Base == "" {
				errs = append(errs, "sources.publish_origin (kind: url): base is required")
			}
		default:
			errs = append(errs, fmt.Sprintf("sources.publish_origin: unknown kind %q (expected primary, mirror, or url)", po.Kind))
		}
	}

	// ── Badges ───────────────────────────────────────────────────────────

	badgeIDs := make(map[string]bool)
	for i, b := range cfg.Badges.Items {
		bpath := fmt.Sprintf("badges.items[%d]", i)
		if b.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: id is required", bpath))
		} else if badgeIDs[b.ID] {
			errs = append(errs, fmt.Sprintf("%s: duplicate badge id %q", bpath, b.ID))
		} else {
			badgeIDs[b.ID] = true
		}
		if b.Output == "" {
			errs = append(errs, fmt.Sprintf("%s: output is required", bpath))
		}
		if b.Value == "" {
			errs = append(errs, fmt.Sprintf("%s: value is required", bpath))
		}
		if b.Text == "" {
			errs = append(errs, fmt.Sprintf("%s: text is required", bpath))
		}
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

	// ── Cross-validation: badge_ref requires publish_origin ─────────────

	hasBadgeRef := false
	for _, f := range cfg.Narrator {
		for _, item := range f.Items {
			if item.Kind == "badge_ref" {
				hasBadgeRef = true
				break
			}
		}
		if hasBadgeRef {
			break
		}
	}
	if hasBadgeRef && cfg.Sources.PublishOrigin == nil {
		errs = append(errs, "narrator uses badge_ref but sources.publish_origin is not configured")
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
	if p := cfg.Dependency.Commit.Promotion; p != "" && p != PromotionDirect && p != PromotionMR {
		errs = append(errs, fmt.Sprintf("dependency.commit.promotion: %q is invalid (expected %q or %q)", p, PromotionDirect, PromotionMR))
	}
	if cfg.Dependency.Commit.Promotion == PromotionMR && !cfg.Dependency.Commit.Push {
		errs = append(errs, "dependency.commit: promotion \"mr\" requires push to be true (no remote branch means no merge request)")
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

	// ── Duration/Size unit validation ───────────────────────────────────
	// Reject invalid values at load time, not at consumption time.

	for _, dv := range []struct{ path, val string }{
		{"lint.cache.max_age", cfg.Lint.Cache.MaxAge},
		{"build_cache.local.retention.max_age", cfg.BuildCache.Local.Retention.MaxAge},
		{"build_cache.external.retention.stale_age", cfg.BuildCache.External.Retention.StaleAge},
		{"build_cache.cleanup.prune.images.dangling.older_than", cfg.BuildCache.Cleanup.Prune.Images.Dangling.OlderThan},
		{"build_cache.cleanup.prune.images.unreferenced.older_than", cfg.BuildCache.Cleanup.Prune.Images.Unreferenced.OlderThan},
		{"build_cache.cleanup.prune.build_cache.older_than", cfg.BuildCache.Cleanup.Prune.BuildCache.OlderThan},
		{"build_cache.cleanup.prune.containers.exited.older_than", cfg.BuildCache.Cleanup.Prune.Containers.Exited.OlderThan},
	} {
		if dv.val != "" {
			if _, err := ParseDuration(dv.val); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", dv.path, err))
			}
		}
	}

	for _, sv := range []struct{ path, val string }{
		{"lint.cache.max_size", cfg.Lint.Cache.MaxSize},
		{"build_cache.local.retention.max_size", cfg.BuildCache.Local.Retention.MaxSize},
		{"build_cache.cleanup.prune.build_cache.keep_storage", cfg.BuildCache.Cleanup.Prune.BuildCache.KeepStorage},
		{"security.cache.trivy.max_size", cfg.Security.Cache.Trivy.MaxSize},
		{"security.cache.grype.max_size", cfg.Security.Cache.Grype.MaxSize},
	} {
		if sv.val != "" {
			if _, err := ParseSize(sv.val); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", sv.path, err))
			}
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

	case "binary-archive":
		if t.Build == "" {
			errs = append(errs, fmt.Sprintf("%s: kind binary-archive requires build reference", path))
		}
		if !validArchiveFormats[t.Format] {
			errs = append(errs, fmt.Sprintf("%s: unknown archive format %q (supported: auto, tar.gz, zip)", path, t.Format))
		}
		// Disallow registry-only fields
		if t.URL != "" {
			errs = append(errs, fmt.Sprintf("%s: url is not valid for kind binary-archive", path))
		}
		if t.Path != "" {
			errs = append(errs, fmt.Sprintf("%s: path is not valid for kind binary-archive", path))
		}
		if len(t.Tags) > 0 {
			errs = append(errs, fmt.Sprintf("%s: tags is not valid for kind binary-archive (use name template)", path))
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
		} else if item.Renderer != "table" && item.Renderer != "list" && item.Renderer != "kv" && item.Renderer != "badges" && item.Renderer != "versions" {
			errs = append(errs, fmt.Sprintf("%s: unknown renderer %q (supported: table, list, kv, badges, versions)", path, item.Renderer))
		}
		if item.OutputFile != "" {
			if pathErrs := validateOutputPath(item.OutputFile, path+".output_file"); len(pathErrs) > 0 {
				errs = append(errs, pathErrs...)
			}
		}
		// Wrap validation
		if item.Wrap != "" && item.Wrap != "details" {
			errs = append(errs, fmt.Sprintf("%s: unknown wrap value %q (supported: details)", path, item.Wrap))
		}
		if item.Wrap == "details" && item.Summary == "" {
			errs = append(errs, fmt.Sprintf("%s: summary is required when wrap=details", path))
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
