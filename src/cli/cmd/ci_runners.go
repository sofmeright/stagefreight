package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/ci"
	"github.com/PrPlanIT/StageFreight/src/commit"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/dependency"
	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/pipeline"
)

// buildCIRegistry returns a registry of all CI subsystem runners.
// All runner implementations live here in cmd — ci package stays pure types.
func buildCIRegistry() ci.Registry {
	return ci.Registry{
		"build":    buildRunner,
		"deps":     depsRunner,
		"security": securityRunner,
		"docs":     docsRunner,
		"release":  releaseRunner,
	}
}

// resolveWorkspace returns the workspace directory from CI context or cwd.
func resolveWorkspace(ciCtx *ci.CIContext) string {
	if ciCtx.Workspace != "" {
		return ciCtx.Workspace
	}
	dir, _ := os.Getwd()
	return dir
}

// ── build runner ─────────────────────────────────────────────────────────────
// Temporary exception: calls runDockerBuild directly from cmd package.
// docker_build.go is too large to extract safely in this PR.
func buildRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	_ = appCfg // build reads its own config via Cobra pre-run

	rootDir := resolveWorkspace(ciCtx)

	// Initialize pipeline state with CI context
	if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
		st.CI = pipeline.InitFromCI(ciCtx)
		st.Build.Attempted = true
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
	}

	if err := runDockerBuild(dockerBuildCmd, nil); err != nil {
		if stErr := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Build.Reason = err.Error()
		}); stErr != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
		}
		return fmt.Errorf("build subsystem: %w", err)
	}

	// Read publish manifest to determine what was produced.
	// Distinguish "not found" (no targets matched) from "unreadable" (real error).
	// Unreadable manifest is NOT treated as "completed with no images" — that would
	// cause security to skip silently. Instead, Completed stays false so downstream
	// stages proceed and fail with diagnostic errors.
	manifest, manifestErr := build.ReadPublishManifest(rootDir)

	switch {
	case manifestErr == nil:
		count := len(manifest.Published)
		if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Build.Completed = true
			st.Build.ProducedImages = count > 0
			st.Build.PublishedCount = count
			if count > 0 {
				st.Build.ManifestPath = build.PublishManifestPath
			}
			if count == 0 {
				st.Build.Reason = "publish manifest exists but contains no images"
			}
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}

	case errors.Is(manifestErr, build.ErrPublishManifestNotFound):
		if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Build.Completed = true
			st.Build.ProducedImages = false
			st.Build.Reason = "no targets matched current ref"
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}

	default:
		// Manifest exists but is unreadable — do NOT mark Completed.
		// Security will proceed (not skip) and fail with a diagnostic from resolveTarget.
		reason := fmt.Sprintf("publish manifest unreadable: %v", manifestErr)
		fmt.Fprintf(os.Stderr, "warning: %s\n", reason)
		if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Build.Reason = reason
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
	}

	return nil
}

// ── deps runner ──────────────────────────────────────────────────────────────
func depsRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	if !appCfg.Dependency.Enabled {
		fmt.Println("  dependency update disabled in config")
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)

	// Fetch security advisories from prior pipeline (cross-pipeline bridge).
	if ciCtx.IsCI() {
		ref := ciCtx.Branch
		if ref == "" {
			ref = ciCtx.DefaultBranch
		}
		fc, fcErr := newForgeClient(forge.Provider(ciCtx.Provider), ciCtx.RepoURL)
		if fcErr == nil {
			advisories, fetchErr := dependency.FetchAdvisories(ctx, fc, ref, rootDir)
			if fetchErr != nil {
				fmt.Fprintf(os.Stderr, "  deps: advisory fetch failed (continuing without): %v\n", fetchErr)
			} else if len(advisories) > 0 {
				fmt.Printf("  deps: fetched %d advisories from prior security scan\n", len(advisories))
			}
		}
	}

	// Run dependency update via the same code path as the CLI command
	result, err := runDependencyUpdateLogic(ctx, appCfg, rootDir, opts.Verbose)
	if err != nil {
		return fmt.Errorf("deps subsystem: %w", err)
	}

	// Structured output — same format as `stagefreight dependency update`
	w := os.Stdout
	color := output.UseColor()
	updateSec := output.NewSection(w, "Update", 0, color)

	appliedDeps := toOutputApplied(result.Applied)
	output.SectionApplied(updateSec, "Applied", appliedDeps, color)

	skippedGroups := aggregateSkipped(result.Skipped)
	output.SectionSkipped(updateSec, "Skipped", skippedGroups, color)

	cves := collectCVEsFixed(result.Applied)
	output.SectionCVEs(updateSec, cves, color)

	if result.Verified {
		status := "success"
		if result.VerifyErr != nil {
			status = "failed"
		}
		output.RowStatus(updateSec, "verify", "", status, color)
	}

	updateSec.Separator()
	updateSec.Row("%-16s%d", "applied", len(result.Applied))
	updateSec.Row("%-16s%d", "skipped", len(result.Skipped))
	updateSec.Row("%-16s%d", "files changed", len(result.FilesChanged))
	updateSec.Close()

	// Auto-commit if configured and files changed
	if appCfg.Dependency.Commit.Enabled && len(result.FilesChanged) > 0 {
		commitResult, commitErr := autoCommitViaPlanner(ctx, appCfg, rootDir, commit.PlannerOptions{
			Type:    appCfg.Dependency.Commit.Type,
			Scope:   "deps",
			Message: appCfg.Dependency.Commit.Message,
			Paths:   result.FilesChanged,
			SkipCI:  boolPtr(appCfg.Dependency.Commit.SkipCI),
			Push:    boolPtr(appCfg.Dependency.Commit.Push),
		})
		if commitErr != nil {
			fmt.Fprintf(os.Stderr, "warning: dependency auto-commit failed: %v\n", commitErr)
		}

		// Evaluate handoff when a commit was actually created and pushed
		if commitResult != nil && !commitResult.NoOp && commitResult.Pushed {
			handoff := ci.EvaluateHandoff(ciCtx, appCfg.Dependency.CI.Handoff, commitResult.SHA)
			if msg := ci.FormatHandoffMessage(handoff); msg != "" {
				fmt.Println(msg)
			}
			if handoff.Decision == ci.HandoffRestart && ciCtx.PipelineID != "" {
				fc, fcErr := newForgeClient(forge.Provider(ciCtx.Provider), ciCtx.RepoURL)
				if fcErr == nil {
					if cancelErr := fc.CancelPipeline(ctx, ciCtx.PipelineID); cancelErr != nil {
						fmt.Fprintf(os.Stderr, "warning: pipeline cancel failed (freshness guards will handle): %v\n", cancelErr)
					}
				}
			}
			if handoff.Decision == ci.HandoffFail {
				return fmt.Errorf("deps subsystem: dependency repair at handoff depth %d — policy requires clean revision after handoff", handoff.Depth)
			}
		}
	}

	return nil
}

// runDependencyUpdateLogic runs the dependency update pipeline (resolve → filter → apply → verify → artifacts).
// Extracted from the Cobra command for reuse by CI runners.
func runDependencyUpdateLogic(ctx context.Context, appCfg *config.Config, rootDir string, isVerbose bool) (*dependency.UpdateResult, error) {
	w := os.Stdout

	// Load freshness options from config
	var freshnessOpts map[string]any
	if mc, ok := appCfg.Lint.Modules["freshness"]; ok {
		freshnessOpts = mc.Options
	}

	// Resolve ecosystems from config
	ecosystems := appCfg.Dependency.Scope.ScopeToEcosystems()

	// Collect files via lint engine
	output.SectionStart(w, "sf_deps_resolve", "Resolve")

	engine, err := lint.NewEngine(appCfg.Lint, rootDir, []string{"freshness"}, nil, isVerbose, nil)
	if err != nil {
		output.SectionEnd(w, "sf_deps_resolve")
		return nil, fmt.Errorf("creating lint engine: %w", err)
	}

	files, err := engine.CollectFiles()
	if err != nil {
		output.SectionEnd(w, "sf_deps_resolve")
		return nil, fmt.Errorf("collecting files: %w", err)
	}

	deps, err := freshness.ResolveDeps(ctx, freshnessOpts, files)
	if err != nil {
		output.SectionEnd(w, "sf_deps_resolve")
		return nil, fmt.Errorf("resolving dependencies: %w", err)
	}

	// Enrich dependencies with security scanner advisories from prior pipeline run.
	advisories, advErr := dependency.LoadAdvisories(rootDir)
	if advErr == nil && len(advisories) > 0 {
		enriched := dependency.EnrichDependencies(deps, advisories)
		if enriched > 0 {
			fmt.Printf("  deps: enriched %d dependencies with security advisories\n", enriched)
		}
	}

	output.SectionEnd(w, "sf_deps_resolve")

	// Build update config
	outputDir := appCfg.Dependency.Output
	if outputDir == "" {
		outputDir = ".stagefreight/deps"
	}

	updateCfg := dependency.UpdateConfig{
		RootDir:    rootDir,
		OutputDir:  outputDir,
		DryRun:     false,
		Verify:     true,
		Vulncheck:  true,
		Ecosystems: ecosystems,
		Policy:     "all",
	}

	result, err := dependency.Update(ctx, updateCfg, deps)
	if err != nil && result == nil {
		return nil, fmt.Errorf("dependency update: %w", err)
	}
	if err != nil {
		return result, fmt.Errorf("dependency update: %w", err)
	}

	return result, nil
}

// ── security runner ──────────────────────────────────────────────────────────
func securityRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	if !appCfg.Security.Enabled {
		fmt.Println("  security scan disabled in config")
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)

	// Pre-flight: check pipeline state for build output.
	// Only skip when build explicitly COMPLETED and produced nothing.
	// Missing state = proceed (local dev, or state not written yet).
	// Build failed (Attempted && !Completed) = proceed (let scan fail naturally with good error).
	if ciCtx.IsCI() {
		st, _ := pipeline.ReadState(rootDir)
		if st.Build.Attempted && st.Build.Completed && !st.Build.ProducedImages {
			reason := "build completed but produced no images"
			if st.Build.Reason != "" {
				reason += " (" + st.Build.Reason + ")"
			}
			fmt.Printf("  security: skipping — %s\n", reason)
			if err := pipeline.UpdateState(rootDir, func(s *pipeline.State) {
				s.Security.Skipped = true
				s.Security.Reason = reason
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
			}
			return nil
		}
	}

	// Mark attempted before running scan
	if ciCtx.IsCI() {
		if err := pipeline.UpdateState(rootDir, func(s *pipeline.State) {
			s.Security.Attempted = true
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
	}

	// Set config defaults on package-level vars and invoke the scan function directly.
	// This avoids os/exec while reusing the full scan pipeline.
	if appCfg.Security.OutputDir != "" {
		secScanOutputDir = appCfg.Security.OutputDir
	}

	if err := runSecurityScan(securityScanCmd, nil); err != nil {
		if ciCtx.IsCI() {
			if stErr := pipeline.UpdateState(rootDir, func(s *pipeline.State) {
				s.Security.Reason = err.Error()
			}); stErr != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
			}
		}
		return fmt.Errorf("security subsystem: %w", err)
	}

	if ciCtx.IsCI() {
		if err := pipeline.UpdateState(rootDir, func(s *pipeline.State) {
			s.Security.Completed = true
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
	}

	return nil
}

// ── docs runner ──────────────────────────────────────────────────────────────
func docsRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	if !appCfg.Docs.Enabled {
		fmt.Println("  docs generation disabled in config")
		return nil
	}

	if !ci.IsBranchHeadFresh(ciCtx) {
		fmt.Println("  docs: skipping — pipeline SHA is not branch HEAD (newer pipeline will ship)")
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)
	gen := appCfg.Docs.Generators

	if gen.Badges {
		if err := RunConfigBadges(appCfg, rootDir, nil, ""); err != nil {
			return fmt.Errorf("docs subsystem (badges): %w", err)
		}
	}

	if gen.ReferenceDocs {
		outDir := rootDir + "/docs/modules"
		if err := RunDocsGenerate(rootCmd, outDir); err != nil {
			return fmt.Errorf("docs subsystem (reference docs): %w", err)
		}
	}

	if gen.Narrator {
		if err := RunNarrator(appCfg, rootDir, false, opts.Verbose); err != nil {
			return fmt.Errorf("docs subsystem (narrator): %w", err)
		}
	}

	if gen.DockerReadme {
		if err := RunDockerReadme(ctx, appCfg, rootDir, false); err != nil {
			fmt.Fprintf(os.Stderr, "warning: docker readme sync failed: %v\n", err)
			// Non-fatal — registry sync may fail without credentials
		}
	}

	// Auto-commit if configured
	if appCfg.Docs.Commit.Enabled {
		if _, err := autoCommitViaPlanner(ctx, appCfg, rootDir, commit.PlannerOptions{
			Type:    appCfg.Docs.Commit.Type,
			Message: appCfg.Docs.Commit.Message,
			Paths:   appCfg.Docs.Commit.Add,
			SkipCI:  boolPtr(appCfg.Docs.Commit.SkipCI),
			Push:    boolPtr(appCfg.Docs.Commit.Push),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: docs auto-commit failed: %v\n", err)
		}
	}

	return nil
}

// ── release runner ───────────────────────────────────────────────────────────
func releaseRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	rootDir := resolveWorkspace(ciCtx)

	if !appCfg.Release.Enabled {
		fmt.Println("  release disabled in config")
		if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Release.Skipped = true
			st.Release.Reason = "release disabled in config"
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		return nil
	}

	if !ci.IsBranchHeadFresh(ciCtx) {
		fmt.Println("  release: skipping — pipeline SHA is not branch HEAD (newer pipeline will ship)")
		if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Release.Skipped = true
			st.Release.Reason = "pipeline SHA is not branch HEAD"
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		return nil
	}

	tag := opts.Tag
	if tag == "" {
		tag = ciCtx.Tag
	}
	if tag == "" {
		return fmt.Errorf("release subsystem: no tag available (set SF_CI_TAG or pass --tag)")
	}

	// Policy gate: check if tag matches ANY release target's when conditions.
	// Uses the same target enumeration as runReleaseCreate (collectTargetsByKind + targetWhenMatches).
	if !releaseTagMatchesAnyTarget(appCfg, tag) {
		reason := fmt.Sprintf("tag %q does not match any release policy", tag)
		fmt.Printf("  release: skipping — %s\n", reason)
		if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Release.Skipped = true
			st.Release.Reason = reason
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		return nil
	}

	// Tag matches — mark eligible and attempted before running
	if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
		st.Release.Eligible = true
		st.Release.Attempted = true
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
	}

	// Set config defaults on package-level vars and invoke release create directly.
	rcTag = tag
	if appCfg.Release.SecuritySummary != "" {
		rcSecuritySummary = appCfg.Release.SecuritySummary
	}
	rcRegistryLinks = appCfg.Release.RegistryLinks
	rcCatalogLinks = appCfg.Release.CatalogLinks

	if err := runReleaseCreate(releaseCreateCmd, nil); err != nil {
		if stErr := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
			st.Release.Reason = err.Error()
		}); stErr != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
		}
		return fmt.Errorf("release subsystem: %w", err)
	}

	if err := pipeline.UpdateState(rootDir, func(st *pipeline.State) {
		st.Release.Completed = true
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
	}

	return nil
}

// releaseTagMatchesAnyTarget returns true if the tag matches at least one
// release target's when conditions. Uses the same target enumeration as
// runReleaseCreate (collectTargetsByKind + targetWhenMatches).
// Returns true if no release targets have when constraints (backward compat).
func releaseTagMatchesAnyTarget(appCfg *config.Config, tag string) bool {
	releaseTargets := collectTargetsByKind(appCfg, "release")
	if len(releaseTargets) == 0 {
		return true // no release targets configured
	}

	hasConstraints := false
	for _, t := range releaseTargets {
		if len(t.When.GitTags) == 0 && len(t.When.Branches) == 0 {
			continue
		}
		hasConstraints = true
		if targetWhenMatches(t, tag) {
			return true
		}
	}

	return !hasConstraints
}

// ── commit helpers ───────────────────────────────────────────────────────────

// autoCommitViaPlanner uses commit.BuildPlan + backend.Execute for auto-commit.
// Returns the commit result for callers that need to inspect it (e.g. handoff).
// Non-fatal — callers should log warnings on error.
func autoCommitViaPlanner(ctx context.Context, appCfg *config.Config, rootDir string, opts commit.PlannerOptions) (*commit.Result, error) {
	registry := commit.NewTypeRegistry(appCfg.Commit.Types)
	plan, err := commit.BuildPlan(opts, appCfg.Commit, registry, rootDir)
	if err != nil {
		return nil, fmt.Errorf("auto-commit plan: %w", err)
	}

	backend := &commit.GitBackend{RootDir: rootDir}
	result, err := backend.Execute(ctx, plan, appCfg.Commit.Conventional)
	if err != nil {
		return nil, fmt.Errorf("auto-commit execute: %w", err)
	}
	if result.NoOp {
		fmt.Println("  auto-commit: nothing to commit")
		return result, nil
	}
	fmt.Printf("  auto-commit: %s\n", result.SHA)
	return result, nil
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}
