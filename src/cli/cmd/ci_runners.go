package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/build/docker"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/ci"
	"github.com/PrPlanIT/StageFreight/src/cistate"
	"github.com/PrPlanIT/StageFreight/src/commit"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/dependency"
	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/output"
	stagefreightsync "github.com/PrPlanIT/StageFreight/src/sync"
)

// buildCIRegistry returns a registry of all CI subsystem runners.
// All runner implementations live here in cmd — ci package stays pure types.
func buildCIRegistry() ci.Registry {
	return ci.Registry{
		"build":     buildRunner,
		"deps":      depsRunner,
		"docs":      docsRunner,
		"reconcile": reconcileRunner,
		"release":   releaseRunner,
		"security":  securityRunner,
		"validate":  validateRunner,
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
// Runs binary builds first (if any), then docker builds.
// Binary builds execute before docker builds to satisfy depends_on ordering.
func buildRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	// Policy gate: skip non-release tags (e.g., rolling "latest" tag)
	if ciCtx.IsTag() && !tagMatchesReleasePolicy(ciCtx.Tag, appCfg.Policies) {
		fmt.Printf("  build: skipping — tag %q does not match any release policy\n", ciCtx.Tag)
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)

	// Initialize pipeline state with CI context
	if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
		st.CI = cistate.InitFromCI(ciCtx)
		st.Build.Attempted = true
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
	}

	// Run binary builds first (if configured) — needed for depends_on ordering
	hasBinaryBuilds := false
	for _, b := range appCfg.Builds {
		if b.Kind == "binary" {
			hasBinaryBuilds = true
			break
		}
	}
	if hasBinaryBuilds {
		if err := runBuildBinary(buildBinaryCmd, nil); err != nil {
			if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) {
				st.Build.Reason = "binary build: " + err.Error()
			}); stErr != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
			}
			return fmt.Errorf("build subsystem (binary): %w", err)
		}
	}

	// Run docker builds (if configured)
	hasDockerBuilds := false
	for _, b := range appCfg.Builds {
		if b.Kind == "docker" {
			hasDockerBuilds = true
			break
		}
	}
	if hasDockerBuilds {
		if err := docker.Run(docker.Request{
			Context: ctx,
			RootDir: rootDir,
			Config:  appCfg,
			Verbose: opts.Verbose,
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
		}); err != nil {
			if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) {
				st.Build.Reason = err.Error()
			}); stErr != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
			}
			return silentExit(err)
		}
	}

	if !hasBinaryBuilds && !hasDockerBuilds {
		fmt.Fprintln(os.Stderr, "build: no builds configured — skipping")
		if ciCtx.IsCI() {
			if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
				st.Build.Completed = true
				st.Build.ProducedImages = false
				st.Build.Reason = "no builds configured"
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
			}
		}
		return nil
	}

	// Read publish manifest to determine what was produced.
	// Distinguish "not found" (no targets matched) from "unreadable" (real error).
	// Unreadable manifest is NOT treated as "completed with no images" — that would
	// cause security to skip silently. Instead, Completed stays false so downstream
	// stages proceed and fail with diagnostic errors.
	manifest, manifestErr := artifact.ReadPublishManifest(rootDir)

	switch {
	case manifestErr == nil:
		count := len(manifest.Published)
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.Build.Completed = true
			st.Build.ProducedImages = count > 0
			st.Build.PublishedCount = count
			if count > 0 {
				st.Build.ManifestPath = artifact.PublishManifestPath
			}
			if count == 0 {
				st.Build.Reason = "publish manifest exists but contains no images"
			}
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}

	case errors.Is(manifestErr, artifact.ErrPublishManifestNotFound):
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
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
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
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

	skippedGroups := aggregateSkippedItemized(result.Skipped)
	output.SectionSkippedItemized(updateSec, "Skipped", skippedGroups, color)

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
		// Compute bot branch for MR promotion mode
		var botBranch string
		if appCfg.Dependency.Commit.Promotion == config.PromotionMR && !ciCtx.IsCI() {
			fmt.Fprintf(os.Stderr, "warning: promotion \"mr\" requires CI context; falling back to direct mode\n")
		}
		if appCfg.Dependency.Commit.Promotion == config.PromotionMR && ciCtx.IsCI() {
			prefix := sanitizeBranchPrefix(appCfg.Dependency.Commit.MR.BranchPrefix)
			if prefix == "" {
				fmt.Fprintf(os.Stderr, "warning: invalid mr.branch_prefix %q; using default %q\n",
					appCfg.Dependency.Commit.MR.BranchPrefix, "stagefreight/deps")
				prefix = "stagefreight/deps"
			}
			shortSHA := ciCtx.SHA
			if len(shortSHA) > 8 {
				shortSHA = shortSHA[:8]
			}
			id := ciCtx.PipelineID
			if id == "" {
				id = shortSHA
			}
			botBranch = fmt.Sprintf("%s-%s-%s", prefix, id, shortSHA)
		}

		mode := "direct"
		if botBranch != "" {
			mode = "mr"
		}
		fmt.Printf("  deps: commit promotion mode: %s\n", mode)

		plannerOpts := commit.PlannerOptions{
			Type:    appCfg.Dependency.Commit.Type,
			Scope:   "deps",
			Message: appCfg.Dependency.Commit.Message,
			Paths:   result.FilesChanged,
			SkipCI:  boolPtr(appCfg.Dependency.Commit.SkipCI),
			Push:    boolPtr(appCfg.Dependency.Commit.Push),
		}
		if botBranch != "" {
			plannerOpts.Refspec = "HEAD:refs/heads/" + botBranch
			fmt.Printf("  deps: pushing to bot branch %s\n", botBranch)
		}

		commitResult, commitErr := autoCommitViaPlanner(ctx, appCfg, rootDir, plannerOpts)
		if commitErr != nil {
			fmt.Fprintf(os.Stderr, "warning: dependency auto-commit failed: %v\n", commitErr)
		}

		// MR mode: open merge request after successful push to bot branch
		if commitResult != nil && !commitResult.NoOp && commitResult.Pushed && botBranch != "" {
			target := appCfg.Dependency.Commit.MR.TargetBranch
			if target == "" {
				target = ciCtx.DefaultBranch
			}
			if target == "" {
				target = ciCtx.Branch
			}
			fc, fcErr := newForgeClient(forge.Provider(ciCtx.Provider), ciCtx.RepoURL)
			if fcErr != nil {
				fmt.Fprintf(os.Stderr, "warning: forge client init failed, cannot create MR: %v\n", fcErr)
			} else {
				commitSubject := strings.SplitN(commitResult.Message, "\n", 2)[0]
				mr, mrErr := fc.CreateMR(ctx, forge.MROptions{
					Title:        commitSubject,
					Description:  buildMRDescription(result),
					SourceBranch: botBranch,
					TargetBranch: target,
				})
				if mrErr != nil {
					fmt.Fprintf(os.Stderr, "warning: merge request creation failed: %v\n", mrErr)
				} else {
					fmt.Printf("  deps: opened merge request %s\n", mr.URL)
				}
			}
		}

		// Evaluate handoff only in direct mode — MR mode uses merge requests instead
		if botBranch == "" && commitResult != nil && !commitResult.NoOp && commitResult.Pushed {
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
		details := dependency.EnrichDependencies(deps, advisories)
		if len(details) > 0 {
			fmt.Printf("  deps: enriched %d dependencies with security advisories\n", len(details))
			for _, d := range details {
				plural := "advisory"
				if d.Advisories != 1 {
					plural = "advisories"
				}
				fmt.Printf("    %-30s %-12s %d %s\n", d.Name, d.Version, d.Advisories, plural)
			}
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
	// Policy gate: skip non-release tags
	if ciCtx.IsTag() && !tagMatchesReleasePolicy(ciCtx.Tag, appCfg.Policies) {
		fmt.Printf("  security: skipping — tag %q does not match any release policy\n", ciCtx.Tag)
		return nil
	}

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
		st, _ := cistate.ReadState(rootDir)
		if st.Build.Attempted && st.Build.Completed && !st.Build.ProducedImages {
			reason := "build completed but produced no images"
			if st.Build.Reason != "" {
				reason += " (" + st.Build.Reason + ")"
			}
			fmt.Printf("  security: skipping — %s\n", reason)
			if err := cistate.UpdateState(rootDir, func(s *cistate.State) {
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
		if err := cistate.UpdateState(rootDir, func(s *cistate.State) {
			s.Security.Attempted = true
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
	}

	if err := RunSecurityScan(SecurityScanRequest{
		Ctx:       ctx,
		RootDir:   rootDir,
		Config:    appCfg,
		OutputDir: appCfg.Security.OutputDir,
		SBOM:      true,
		Writer:    os.Stdout,
	}); err != nil {
		if ciCtx.IsCI() {
			if stErr := cistate.UpdateState(rootDir, func(s *cistate.State) {
				s.Security.Reason = err.Error()
			}); stErr != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
			}
		}
		return fmt.Errorf("security subsystem: %w", err)
	}

	if ciCtx.IsCI() {
		if err := cistate.UpdateState(rootDir, func(s *cistate.State) {
			s.Security.Completed = true
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
	}

	return nil
}

// ── docs runner ──────────────────────────────────────────────────────────────
func docsRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	// Policy gate: skip non-release tags
	if ciCtx.IsTag() && !tagMatchesReleasePolicy(ciCtx.Tag, appCfg.Policies) {
		fmt.Printf("  docs: skipping — tag %q does not match any release policy\n", ciCtx.Tag)
		return nil
	}

	if !appCfg.Docs.Enabled {
		fmt.Println("  docs generation disabled in config")
		return nil
	}

	if !ci.IsBranchHeadFresh(ciCtx) {
		fmt.Println("  docs: skipping — pipeline SHA is not branch HEAD (newer pipeline will ship)")
		return nil
	}

	// Loop prevention: if the current commit was created by StageFreight's docs subsystem,
	// do not re-run docs. StageFreight recognizes its own output.
	// This is intelligence, not [skip ci] suppression.
	if isDocsAutoCommit(appCfg, ciCtx) {
		fmt.Println("  docs: skipping — current commit is a StageFreight docs auto-commit")
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
			Body:    "Narrator: StageFreight\nCue: docs/narrator",
			Paths:   appCfg.Docs.Commit.Add,
			SkipCI:  boolPtr(appCfg.Docs.Commit.SkipCI),
			Push:    boolPtr(appCfg.Docs.Commit.Push),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: docs auto-commit failed: %v\n", err)
		}
	}

	// Sync accessories (git mirror on push events — no release data).
	// Mirror push is idempotent — safe even when no repo mutation occurred.
	syncMirrors(ctx, appCfg, nil)

	return nil
}

// isDocsAutoCommit detects if the current commit was created by StageFreight's docs subsystem.
// Uses Cue trailer for deterministic detection — not fuzzy message matching.
// Secondary guard (belt + suspenders). Primary loop prevention is deterministic output.
func isDocsAutoCommit(appCfg *config.Config, ciCtx *ci.CIContext) bool {
	workspace := resolveWorkspace(ciCtx)
	body := gitCommitBody(workspace, "HEAD")
	return hasTrailer(body, "Cue", "docs/narrator")
}

func gitCommitBody(repoDir, rev string) string {
	cmd := exec.Command("git", "log", "-1", "--format=%B", rev)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func hasTrailer(body, key, value string) bool {
	target := key + ": " + value
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == target {
			return true
		}
	}
	return false
}

// ── release runner ───────────────────────────────────────────────────────────
func releaseRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	rootDir := resolveWorkspace(ciCtx)

	if !appCfg.Release.Enabled {
		renderReleaseSkip(ciCtx, releaseSkipDisabled, "release disabled in config")
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.Release.Skipped = true
			st.Release.Reason = "release disabled in config"
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		return nil
	}

	if !ci.IsBranchHeadFresh(ciCtx) {
		renderReleaseSkip(ciCtx, releaseSkipNotHead, "pipeline SHA is not branch HEAD")
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
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
		renderReleaseSkip(ciCtx, releaseSkipNoTag, "no tag context")
		if ciCtx.IsCI() {
			if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
				st.Release.Skipped = true
				st.Release.Reason = "no tag context"
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
			}
		}
		return nil
	}

	// Policy gate: check if tag matches ANY release target's when conditions.
	// Uses the same target enumeration as RunReleaseCreate (collectTargetsByKind + targetWhenMatches).
	if !releaseTagMatchesAnyTarget(appCfg, tag) {
		reason := fmt.Sprintf("tag %q does not match any release policy", tag)
		renderReleaseSkip(ciCtx, releaseSkipPolicyMismatch, reason)
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.Release.Skipped = true
			st.Release.Reason = reason
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		return nil
	}

	// Tag matches — mark eligible and attempted before running
	if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
		st.Release.Eligible = true
		st.Release.Attempted = true
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
	}

	if err := RunReleaseCreate(ReleaseCreateRequest{
		Ctx:             ctx,
		RootDir:         rootDir,
		Config:          appCfg,
		Tag:             tag,
		SecuritySummary: appCfg.Release.SecuritySummary,
		RegistryLinks:   appCfg.Release.RegistryLinks,
		CatalogLinks:    appCfg.Release.CatalogLinks,
		Writer:          os.Stdout,
		Verbose:         opts.Verbose,
	}); err != nil {
		if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.Release.Reason = err.Error()
		}); stErr != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
		}
		return fmt.Errorf("release subsystem: %w", err)
	}

	if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
		st.Release.Completed = true
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
	}

	// Sync accessories — git mirror only on release.
	// Release projection is NOT wired here yet — full release data
	// (notes, assets, links) must be passed from RunReleaseCreate.
	// TODO: plumb release data from RunReleaseCreate into sync.
	syncMirrors(ctx, appCfg, nil)

	return nil
}

// releaseTagMatchesAnyTarget returns true if the tag matches at least one
// release target's when conditions. Uses the same target enumeration as
// releaseSkipCode identifies why a release was skipped.
type releaseSkipCode string

const (
	releaseSkipNoTag          releaseSkipCode = "no_tag_context"
	releaseSkipDisabled       releaseSkipCode = "disabled"
	releaseSkipNotHead        releaseSkipCode = "not_head"
	releaseSkipPolicyMismatch releaseSkipCode = "policy_mismatch"
)

// renderReleaseSkip renders a structured skip section for the release subsystem.
func renderReleaseSkip(ciCtx *ci.CIContext, code releaseSkipCode, reason string) {
	color := output.UseColor()
	sec := output.NewSection(os.Stdout, "Release", 0, color)
	sec.Row("%-14s%s", "status", "skipped")
	sec.Row("%-14s%s", "reason", reason)
	ref := ciCtx.Branch
	if ref == "" {
		ref = ciCtx.Tag
	}
	sec.Row("%-14s%s", "ref", ref)
	sec.Row("%-14s%s", "sha", shortSHA(ciCtx.SHA))
	tag := ciCtx.Tag
	if tag == "" {
		tag = "none"
	}
	sec.Row("%-14s%s", "tag", tag)
	sec.Row("%-14s%s", "result", releaseSkipResult(code))
	sec.Close()
}

// releaseSkipResult maps a skip code to a human-readable outcome.
func releaseSkipResult(code releaseSkipCode) string {
	switch code {
	case releaseSkipNoTag:
		return "nothing to release"
	case releaseSkipDisabled:
		return "release disabled"
	case releaseSkipNotHead:
		return "superseded by newer pipeline"
	case releaseSkipPolicyMismatch:
		return "no matching release policy"
	default:
		return "skipped"
	}
}

// RunReleaseCreate (collectTargetsByKind + targetWhenMatches).
// Returns true if no release targets have when constraints (backward compat).
func releaseTagMatchesAnyTarget(appCfg *config.Config, tag string) bool {
	releaseTargets := pipeline.CollectTargetsByKind(appCfg, "release")
	if len(releaseTargets) == 0 {
		return true // no release targets configured
	}

	hasConstraints := false
	for _, t := range releaseTargets {
		if len(t.When.GitTags) == 0 && len(t.When.Branches) == 0 {
			continue
		}
		hasConstraints = true
		if targetWhenMatches(t, tag, appCfg.Policies) {
			return true
		}
	}

	return !hasConstraints
}

// tagMatchesReleasePolicy returns true if the tag matches any git_tags policy
// pattern (stable or prerelease). Used to gate subsystem runners on tag events
// so rolling tags like "latest" don't trigger full builds/scans/docs.
//
// The skeleton defines generic CI event classes; StageFreight enforces
// repo-specific tag eligibility at runtime from .stagefreight.yml policy.
func tagMatchesReleasePolicy(tag string, policies config.PoliciesConfig) bool {
	if len(policies.GitTags) == 0 {
		return true // no policies = all tags are eligible (backward compat)
	}
	for _, pattern := range policies.GitTags {
		if config.MatchPatterns([]string{pattern}, tag) {
			return true
		}
	}
	return false
}

// ── mirror sync ─────────────────────────────────────────────────────────────

// syncMirrors runs per-mirror sync: git mirror first, then artifact
// projection gated on mirror success. Strictly sequential per mirror.
//
// releaseData is optional — nil means no release projection for this run.
// Mirror source is always sources.primary.worktree (resolved to absolute).
// Mirror push is idempotent — safe to call even when no mutation occurred.
func syncMirrors(ctx context.Context, appCfg *config.Config, releaseData *stagefreightsync.ReleaseData) {
	if len(appCfg.Sources.Mirrors) == 0 {
		return
	}

	// Mirror source is the authoritative local worktree, not rootDir.
	worktree := appCfg.Sources.Primary.Worktree
	if worktree == "" {
		worktree = "."
	}

	hasDegraded := false

	for _, m := range appCfg.Sources.Mirrors {
		// Resolve {var:...} templates in mirror config fields.
		m.URL = gitver.ResolveVars(m.URL, appCfg.Vars)
		m.ProjectID = gitver.ResolveVars(m.ProjectID, appCfg.Vars)

		// 1. Git mirror (if enabled)
		if m.Sync.Git {
			result, err := stagefreightsync.MirrorPush(ctx, worktree, m)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  sync: %s: mirror error: %v\n", m.ID, err)
				hasDegraded = true
				continue // skip artifact sync for this mirror
			}

			if result.Status == stagefreightsync.SyncSuccess {
				fmt.Printf("  sync: %s: mirror ✓ (%s)\n", m.ID, result.Duration.Truncate(100*time.Millisecond))
			} else {
				fmt.Fprintf(os.Stderr, "  sync: %s: mirror DEGRADED — %s: %s\n", m.ID, result.FailureReason, result.Message)
				hasDegraded = true
				// Do NOT proceed to artifact sync for this mirror
				continue
			}
		}

		// 2. Release projection (if enabled and data provided)
		if m.Sync.Releases && releaseData != nil {
			result := stagefreightsync.SyncRelease(ctx, m, *releaseData)
			if result.Status == stagefreightsync.SyncSuccess {
				fmt.Printf("  sync: %s: release ✓\n", m.ID)
			} else {
				fmt.Fprintf(os.Stderr, "  sync: %s: release warning — %s\n", m.ID, result.Message)
			}
		}
	}

	if hasDegraded {
		fmt.Fprintf(os.Stderr, "\n  ⚠ DEGRADED REPLICATION: one or more mirrors failed\n")
	}
}

// LegacySyncOverlapsMirror returns true if a legacy sync target's provider
// is also declared as a mirror, meaning the mirror should take precedence.
// Exported for use in release_create.go where legacy sync targets are executed.
func LegacySyncOverlapsMirror(targetProvider string, appCfg *config.Config) bool {
	for _, m := range appCfg.Sources.Mirrors {
		if m.Provider == targetProvider {
			return true
		}
	}
	return false
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

	// Select backend from config — same logic as the commit CLI command.
	// Decision is explicit and deterministic: forge, git, or auto (CI → forge).
	var useForge bool
	switch appCfg.Commit.Backend {
	case "forge":
		useForge = true
	case "git":
		useForge = false
	case "":
		useForge = output.IsCI()
	default:
		return nil, fmt.Errorf("auto-commit: unknown backend %q", appCfg.Commit.Backend)
	}

	var backend commit.Backend
	if useForge {
		fc, branch, fErr := detectForgeForPush(rootDir, plan)
		if fErr != nil {
			if appCfg.Commit.Backend == "forge" {
				return nil, fmt.Errorf("auto-commit: forge backend requested but detection failed: %w", fErr)
			}
			// Implicit forge (CI auto-detection) failed — fall back to git with warning.
			fmt.Fprintf(os.Stderr, "warning: forge backend auto-detection failed, falling back to git: %v\n", fErr)
			backend = &commit.GitBackend{RootDir: rootDir}
		} else {
			backend = &commit.ForgeBackend{
				RootDir:     rootDir,
				ForgeClient: fc,
				Branch:      branch,
			}
		}
	} else {
		backend = &commit.GitBackend{RootDir: rootDir}
	}

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

// ── MR description builder ───────────────────────────────────────────────────

// buildMRDescription formats a rich markdown description for dependency update MRs.
func buildMRDescription(result *dependency.UpdateResult) string {
	if result == nil {
		return "No dependency update result available.\n\n---\n\n> Automated by **StageFreight**\n"
	}

	var b strings.Builder

	cves := collectCVEsFixed(result.Applied)
	files := uniqueSortedStrings(result.FilesChanged)

	// --- Summary header ---
	b.WriteString("## Dependency Updates\n\n")

	if len(result.Applied) == 0 {
		b.WriteString("No dependency updates were applied.\n")
	} else {
		b.WriteString(fmt.Sprintf("**%s updated**  \n", pluralize(len(result.Applied), "dependency", "dependencies")))
		if len(cves) > 0 {
			b.WriteString(fmt.Sprintf("**%s fixed**  \n", pluralize(len(cves), "security advisory", "security advisories")))
		}
		if len(files) > 0 {
			b.WriteString(fmt.Sprintf("**%s modified**  \n", pluralize(len(files), "file", "files")))
		}
	}

	mrWriteDivider(&b)

	// --- Updated Dependencies table ---
	if len(result.Applied) > 0 {
		mrSection(&b, "\U0001F4E6 Updated Dependencies")
		b.WriteString("| Dependency | From | To | Type |\n")
		b.WriteString("|---|---:|---:|:---|\n")
		for _, u := range result.Applied {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				mrEscapeCell(u.Dep.Name),
				mrEscapeCell(u.OldVer),
				mrEscapeCell(u.NewVer),
				mrEscapeCell(u.UpdateType),
			))
		}
		mrWriteDivider(&b)
	}

	// --- Security Fixes table ---
	if len(cves) > 0 {
		mrSection(&b, "\U0001F510 Security Fixes")
		b.WriteString("| CVE | Severity | Fixed By |\n")
		b.WriteString("|---|:---:|---|\n")
		for _, c := range cves {
			b.WriteString(fmt.Sprintf("| %s | %s | %s |\n",
				mrEscapeCell(c.ID),
				mrEscapeCell(c.Severity),
				mrEscapeCell(c.FixedBy),
			))
		}
		mrWriteDivider(&b)
	}

	// --- Files Changed ---
	if len(files) > 0 {
		mrSection(&b, "\U0001F4C2 Files Changed")
		for _, f := range files {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
		mrWriteDivider(&b)
	}

	// --- Skipped Dependencies ---
	if len(result.Skipped) > 0 {
		type reasonGroup struct {
			reason string
			items  []dependency.SkippedDep
		}
		groupMap := make(map[string]*reasonGroup)
		var groupOrder []string
		for _, s := range result.Skipped {
			r := dependency.NormalizeSkipReason(s.Reason)
			g, ok := groupMap[r]
			if !ok {
				g = &reasonGroup{reason: r}
				groupMap[r] = g
				groupOrder = append(groupOrder, r)
			}
			g.items = append(g.items, s)
		}
		sort.Strings(groupOrder)

		b.WriteString(fmt.Sprintf("\n<details>\n<summary>Skipped Dependencies (%d)</summary>\n\n", len(result.Skipped)))
		for _, r := range groupOrder {
			g := groupMap[r]
			sort.Slice(g.items, func(i, j int) bool {
				return g.items[i].Dep.Name < g.items[j].Dep.Name
			})
			b.WriteString(fmt.Sprintf("#### %s\n", r))
			cap := 5
			for i, s := range g.items {
				if i >= cap {
					b.WriteString(fmt.Sprintf("- ... and %d more\n", len(g.items)-cap))
					break
				}
				b.WriteString(fmt.Sprintf("- %s %s\n", s.Dep.Name, s.Dep.Current))
			}
			b.WriteString("\n")
		}
		b.WriteString("</details>\n")
		mrWriteDivider(&b)
	}

	// --- Verification ---
	if result.Verified {
		if result.VerifyErr != nil {
			b.WriteString("\u274C Verification: failed\n")
		} else {
			b.WriteString("\u2705 Verification: passed\n")
		}
		mrWriteDivider(&b)
	}

	// --- Footer ---
	b.WriteString("> Automated by **StageFreight**\n")

	return b.String()
}

// mrSection writes a markdown section heading with correct blank-line spacing.
func mrSection(b *strings.Builder, title string) {
	b.WriteString("## " + title + "\n\n")
}

// mrWriteDivider writes a horizontal rule with correct blank-line spacing.
func mrWriteDivider(b *strings.Builder) {
	b.WriteString("\n---\n\n")
}

// mrEscapeCell escapes pipe characters and strips newlines for markdown table cells.
func mrEscapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

// uniqueSortedStrings returns a deduplicated, sorted copy of ss.
func uniqueSortedStrings(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	cp := make([]string, len(ss))
	copy(cp, ss)
	sort.Strings(cp)
	out := cp[:1]
	for _, s := range cp[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// pluralize returns "N thing" or "N things" based on count.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// sanitizeBranchPrefix cleans a user-provided branch prefix for safety.
// Returns empty string if the input is invalid.
func sanitizeBranchPrefix(raw string) string {
	p := strings.TrimSpace(raw)
	p = strings.TrimPrefix(p, "refs/heads/")
	p = strings.Trim(p, "/")
	p = strings.TrimRight(p, "-")
	if p == "" || strings.Contains(p, "..") || strings.Contains(p, " ") {
		return ""
	}
	if strings.HasSuffix(p, ".lock") || strings.HasPrefix(p, "-") || strings.Contains(p, "@{") {
		return ""
	}
	return p
}

// ── validate runner ─────────────────────────────────────────────────────────
func validateRunner(_ context.Context, appCfg *config.Config, ciCtx *ci.CIContext, _ ci.RunOptions) error {
	start := time.Now()
	if strings.TrimSpace(string(appCfg.Lint.Level)) == "" {
		renderCISkip("Validate", start, "no validation configured")
		return nil
	}
	// Thin shim: delegate to existing lint command.
	return runLint(&cobra.Command{}, []string{})
}

// ── reconcile runner ────────────────────────────────────────────────────────
func reconcileRunner(_ context.Context, appCfg *config.Config, ciCtx *ci.CIContext, _ ci.RunOptions) error {
	start := time.Now()

	hasGitOps := strings.TrimSpace(appCfg.GitOps.Cluster.Name) != ""
	hasGovernanceClusters := len(appCfg.Governance.Clusters) > 0
	hasGovernanceSource := governanceSourceConfigured(appCfg)

	if !hasGitOps && !hasGovernanceClusters {
		renderCISkip("Reconcile", start, "no reconcile target configured")
		return nil
	}

	// GitOps reconcile — requires cluster auth.
	if hasGitOps {
		if strings.TrimSpace(appCfg.GitOps.OIDC.Audience) != "" && strings.TrimSpace(os.Getenv("STAGEFREIGHT_OIDC")) == "" {
			renderCISkip("Reconcile", start, "cluster auth unavailable")
		} else {
			if err := runReconcile(&cobra.Command{}, []string{}); err != nil {
				return err
			}
		}
	}

	// Governance reconcile — requires clusters AND source configured.
	// Not mutually exclusive with gitops — both can run.
	if hasGovernanceClusters {
		if !hasGovernanceSource {
			renderCISkip("Reconcile", start, "governance source not configured")
		} else {
			if err := runGovernanceReconcile(&cobra.Command{}, []string{}); err != nil {
				return err
			}
		}
	}

	return nil
}

// ── shared CI skip renderer ─────────────────────────────────────────────────

// renderCISkip renders a structured skip section for any CI subsystem.
func renderCISkip(section string, start time.Time, reason string) {
	color := output.UseColor()
	sec := output.NewSection(os.Stdout, section, time.Since(start), color)
	sec.Row("%-14s%s", "status", "skipped")
	sec.Row("%-14s%s", "reason", reason)
	sec.Row("%-14s%s", "result", ciSkipResult(reason))
	sec.Close()
}

// governanceSourceConfigured checks if governance has a resolvable source.
func governanceSourceConfigured(_ *config.Config) bool {
	src, err := resolveGovernanceSource()
	return err == nil && src.RepoURL != ""
}

// ciSkipResult maps a skip reason to a human-readable outcome.
func ciSkipResult(reason string) string {
	switch reason {
	case "no validation configured":
		return "validation not configured"
	case "no reconcile target configured":
		return "nothing to reconcile"
	case "cluster auth unavailable":
		return "reconcile skipped — auth not available in this environment"
	case "governance source not configured":
		return "reconcile skipped — governance source not configured"
	default:
		return "skipped"
	}
}
