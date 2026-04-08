package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
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
	"github.com/PrPlanIT/StageFreight/src/gitstate"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/runner"
	stagefreightsync "github.com/PrPlanIT/StageFreight/src/sync"
	"github.com/PrPlanIT/StageFreight/src/trace"
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
	if ciCtx.IsTag() && !tagMatchesReleasePolicy(ciCtx.Tag, appCfg.Versioning) {
		fmt.Printf("  build: skipping — tag %q does not match any release tag source\n", ciCtx.Tag)
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)

	// Resolve build policy from config. If ANY build is required, the subsystem is required.
	buildRequired := false
	dockerRequired := false
	isCrucible := false
	for _, b := range appCfg.Builds {
		if b.IsRequired() {
			buildRequired = true
		}
		if b.Kind == "docker" {
			dockerRequired = true
		}
		if b.BuildMode == "crucible" {
			isCrucible = true
		}
	}
	if len(appCfg.Builds) == 0 {
		buildRequired = true // no builds configured = subsystem still required (will report not_applicable)
	}

	// Execution spine: Runner → Config → Lint (universal, always present)
	if r := runnerPreflight(rootDir, runner.Options{DockerRequired: dockerRequired, IsCrucible: isCrucible}); r.Health == runner.Unhealthy {
		return fmt.Errorf("build subsystem: substrate unhealthy")
	}
	if err := runConfigPhase(rootDir); err != nil {
		return fmt.Errorf("build subsystem: %w", err)
	}
	if err := runUniversalLint(ctx, appCfg, rootDir, ciCtx.IsCI(), opts.Verbose); err != nil {
		return fmt.Errorf("build subsystem (lint): %w", err)
	}

	// Initialize pipeline state with CI context
	if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
		st.CI = cistate.InitFromCI(ciCtx)
		st.RecordSubsystem(cistate.SubsystemState{
			Name: "build", Attempted: true, Required: buildRequired, Outcome: "failed",
		})
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
				st.RecordSubsystem(cistate.SubsystemState{
					Name: "build", Attempted: true, Required: buildRequired,
					Outcome: "failed", Reason: "binary build: " + err.Error(),
				})
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
			Context:  ctx,
			RootDir:  rootDir,
			Config:   appCfg,
			Verbose:  opts.Verbose,
			Stdout:   os.Stdout,
			Stderr:   os.Stderr,
			SkipLint: true, // lint already ran via runUniversalLint above
		}); err != nil {
			if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) {
				st.RecordSubsystem(cistate.SubsystemState{
					Name: "build", Attempted: true, Required: buildRequired,
					Outcome: "failed", Reason: err.Error(),
				})
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
				st.Build.ProducedImages = false
				st.RecordSubsystem(cistate.SubsystemState{
					Name: "build", Attempted: true, Required: buildRequired,
					Outcome: "not_applicable", Reason: "no builds configured",
				})
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
			}
		}
		runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
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
			st.Build.ProducedImages = count > 0
			st.Build.PublishedCount = count
			if count > 0 {
				st.Build.ManifestPath = artifact.PublishManifestPath
			}
			reason := ""
			if count == 0 {
				reason = "publish manifest exists but contains no images"
			}
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "build", Attempted: true, Completed: true, Required: buildRequired,
				Outcome: "success", Reason: reason,
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}

	case errors.Is(manifestErr, artifact.ErrPublishManifestNotFound):
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.Build.ProducedImages = false
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "build", Attempted: true, Completed: true, Required: buildRequired,
				Outcome: "success", Reason: "no targets matched current ref",
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}

	default:
		// Manifest exists but is unreadable — do NOT mark Completed.
		// Security will proceed (not skip) and fail with a diagnostic from resolveTarget.
		reason := fmt.Sprintf("publish manifest unreadable: %v", manifestErr)
		fmt.Fprintf(os.Stderr, "warning: %s\n", reason)
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "build", Attempted: true, Required: buildRequired,
				Outcome: "failed", Reason: reason,
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
	}

	runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
	return nil
}

// ── deps runner ──────────────────────────────────────────────────────────────
func depsRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	if !appCfg.Dependency.Enabled {
		fmt.Println("  dependency update disabled in config")
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)

	if r := runnerPreflight(rootDir, runner.Options{DockerRequired: false}); r.Health == runner.Unhealthy {
		return fmt.Errorf("deps subsystem: substrate unhealthy")
	}
	if err := runConfigPhase(rootDir); err != nil {
		return fmt.Errorf("deps subsystem: %w", err)
	}
	if err := runUniversalLint(ctx, appCfg, rootDir, ciCtx.IsCI(), opts.Verbose); err != nil {
		return fmt.Errorf("deps subsystem (lint): %w", err)
	}

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

	// Emit update summary facts — panel renders from emissions only.
	updateCol := trace.NewCollector()
	updateCol.SideEffect("deps", "applied", fmt.Sprintf("%d", len(result.Applied)), "", "deps-updater", trace.StatusOK)
	updateCol.SideEffect("deps", "skipped", fmt.Sprintf("%d", len(result.Skipped)), "", "deps-updater", trace.StatusInfo)
	updateCol.SideEffect("deps", "files_changed", fmt.Sprintf("%d", len(result.FilesChanged)), "", "deps-updater", trace.StatusOK)
	updateSec.Separator()
	for _, e := range updateCol.ForDomain("deps") {
		updateSec.Row("%-16s%s", e.Key, e.RenderValue())
		updateCol.MarkRendered(e)
	}
	updateSec.Close()

	// Auto-commit if configured and files changed — gated by run_from.
	if appCfg.Dependency.Commit.Enabled && len(result.FilesChanged) > 0 {
		if rfResult := config.EvaluateRunFrom(appCfg.Dependency.Commit.RunFrom, ciCtx.RepoURL, config.PrimaryURL(appCfg)); !rfResult.Matched && rfResult.Mode != "ignore" {
			fmt.Fprintf(os.Stderr, "  deps commit: %s (%s)\n", rfResult.Mode, rfResult.Reason)
			return nil
		}
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

	runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
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
	if ciCtx.IsTag() && !tagMatchesReleasePolicy(ciCtx.Tag, appCfg.Versioning) {
		fmt.Printf("  security: skipping — tag %q does not match any release tag source\n", ciCtx.Tag)
		return nil
	}

	if !appCfg.Security.Enabled {
		fmt.Println("  security scan disabled in config")
		return nil
	}

	secAllowFailure := !appCfg.Security.IsRequired()
	rootDir := resolveWorkspace(ciCtx)

	if r := runnerPreflight(rootDir, runner.Options{DockerRequired: true}); r.Health == runner.Unhealthy {
		return fmt.Errorf("security subsystem: substrate unhealthy")
	}
	if err := runConfigPhase(rootDir); err != nil {
		return fmt.Errorf("security subsystem: %w", err)
	}
	if err := runUniversalLint(ctx, appCfg, rootDir, ciCtx.IsCI(), opts.Verbose); err != nil {
		return fmt.Errorf("security subsystem (lint): %w", err)
	}

	// Pre-flight: check pipeline state for build output.
	// Only skip when build completed successfully and produced nothing.
	// Missing state = proceed (local dev, or state not written yet).
	// Build failed = proceed (let scan fail naturally with good error).
	if ciCtx.IsCI() {
		st, _ := cistate.ReadState(rootDir)
		buildSub := st.GetSubsystem("build")
		if buildSub != nil && buildSub.Outcome == "success" && !st.Build.ProducedImages {
			reason := "build completed but produced no images"
			if buildSub.Reason != "" {
				reason += " (" + buildSub.Reason + ")"
			}
			fmt.Printf("  security: skipping — %s\n", reason)
			if err := cistate.UpdateState(rootDir, func(s *cistate.State) {
				s.RecordSubsystem(cistate.SubsystemState{
					Name: "security", Attempted: true, Skipped: true, AllowFailure: secAllowFailure,
					Outcome: "not_applicable", Reason: reason,
				})
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
			}
			runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
			return nil
		}
	}

	// Mark attempted before running scan
	if ciCtx.IsCI() {
		if err := cistate.UpdateState(rootDir, func(s *cistate.State) {
			s.RecordSubsystem(cistate.SubsystemState{
				Name: "security", Attempted: true, AllowFailure: secAllowFailure, Outcome: "failed",
			})
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
				s.RecordSubsystem(cistate.SubsystemState{
					Name: "security", Attempted: true, AllowFailure: secAllowFailure,
					Outcome: "failed", Reason: err.Error(),
				})
			}); stErr != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
			}
		}
		return fmt.Errorf("security subsystem: %w", err)
	}

	if ciCtx.IsCI() {
		if err := cistate.UpdateState(rootDir, func(s *cistate.State) {
			s.RecordSubsystem(cistate.SubsystemState{
				Name: "security", Attempted: true, Completed: true, AllowFailure: secAllowFailure,
				Outcome: "success",
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
	}

	runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
	return nil
}

// ── docs runner ──────────────────────────────────────────────────────────────
func docsRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	// Policy gate: skip non-release tags
	if ciCtx.IsTag() && !tagMatchesReleasePolicy(ciCtx.Tag, appCfg.Versioning) {
		fmt.Printf("  docs: skipping — tag %q does not match any release tag source\n", ciCtx.Tag)
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

	if r := runnerPreflight(rootDir, runner.Options{DockerRequired: false}); r.Health == runner.Unhealthy {
		return fmt.Errorf("docs subsystem: substrate unhealthy")
	}
	if err := runConfigPhase(rootDir); err != nil {
		return fmt.Errorf("docs subsystem: %w", err)
	}
	if err := runUniversalLint(ctx, appCfg, rootDir, ciCtx.IsCI(), opts.Verbose); err != nil {
		return fmt.Errorf("docs subsystem (lint): %w", err)
	}

	// Universal docs: badges + narrator + docker_readme (with BUILD_STATUS from cistate).
	// Reference docs and auto-commit are docsRunner-exclusive — they stay here.
	runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)

	// Reference docs — docsRunner exclusive (generates cobra command reference).
	if appCfg.Docs.Generators.ReferenceDocs {
		outDir := rootDir + "/docs/modules"
		if err := RunDocsGenerate(rootCmd, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: docs subsystem (reference docs): %v\n", err)
		}
	}

	// Auto-commit if configured — gated by run_from.
	if appCfg.Docs.Commit.Enabled {
		rfResult := config.EvaluateRunFrom(appCfg.Docs.Commit.RunFrom, ciCtx.RepoURL, config.PrimaryURL(appCfg))
		switch {
		case !rfResult.Matched && rfResult.Mode == "exit":
			fmt.Fprintf(os.Stderr, "  docs commit: blocked (%s)\n", rfResult.Reason)
		case !rfResult.Matched && rfResult.Mode == "read-only":
			fmt.Fprintf(os.Stderr, "  docs commit: read-only (%s)\n", rfResult.Reason)
		default: // matched or ignore
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
	}

	// Sync accessories (git mirror on push events — no release data).
	// Mirror push is idempotent — safe even when no repo mutation occurred.
	syncMirrors(ctx, appCfg)

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
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return ""
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return ""
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(commit.Message)
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
	relAllowFailure := !appCfg.Release.IsRequired()
	rootDir := resolveWorkspace(ciCtx)

	if r := runnerPreflight(rootDir, runner.Options{DockerRequired: false}); r.Health == runner.Unhealthy {
		return fmt.Errorf("release subsystem: substrate unhealthy")
	}
	if err := runConfigPhase(rootDir); err != nil {
		return fmt.Errorf("release subsystem: %w", err)
	}
	if err := runUniversalLint(ctx, appCfg, rootDir, ciCtx.IsCI(), opts.Verbose); err != nil {
		return fmt.Errorf("release subsystem (lint): %w", err)
	}

	// run_from gate — controls mutation authority for release.
	rfResult := config.EvaluateRunFrom(appCfg.Release.RunFrom, ciCtx.RepoURL, config.PrimaryURL(appCfg))
	if !rfResult.Matched && rfResult.Mode == "exit" {
		reason := fmt.Sprintf("run_from: exit (%s)", rfResult.Reason)
		renderReleaseSkip(ciCtx, releaseSkipDisabled, reason)
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "release", Attempted: true, Skipped: true, AllowFailure: relAllowFailure,
				Outcome: "skipped", Reason: reason,
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
		return nil
	}
	releaseReadOnly := !rfResult.Matched && rfResult.Mode == "read-only"
	if releaseReadOnly {
		fmt.Fprintf(os.Stderr, "  release: read-only mode (%s)\n", rfResult.Reason)
	}

	if !appCfg.Release.Enabled {
		renderReleaseSkip(ciCtx, releaseSkipDisabled, "release disabled in config")
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "release", Attempted: true, Skipped: true, AllowFailure: relAllowFailure,
				Outcome: "not_applicable", Reason: "release disabled in config",
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
		return nil
	}

	if !ci.IsBranchHeadFresh(ciCtx) {
		renderReleaseSkip(ciCtx, releaseSkipNotHead, "pipeline SHA is not branch HEAD")
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "release", Attempted: true, Skipped: true, AllowFailure: relAllowFailure,
				Outcome: "skipped", Reason: "pipeline SHA is not branch HEAD",
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
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
				st.RecordSubsystem(cistate.SubsystemState{
					Name: "release", Attempted: true, Skipped: true, AllowFailure: relAllowFailure,
					Outcome: "not_applicable", Reason: "no tag context",
				})
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
			}
		}
		runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
		return nil
	}

	// Policy gate: check if tag matches ANY release target's when conditions.
	// Uses the same target enumeration as RunReleaseCreate (collectTargetsByKind + targetWhenMatches).
	if !releaseTagMatchesAnyTarget(appCfg, tag) {
		reason := fmt.Sprintf("tag %q does not match any release tag source", tag)
		renderReleaseSkip(ciCtx, releaseSkipPolicyMismatch, reason)
		if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "release", Attempted: true, Skipped: true, AllowFailure: relAllowFailure,
				Outcome: "not_applicable", Reason: reason,
			})
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
		}
		runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
		return nil
	}

	// Tag matches — mark eligible and attempted before running
	if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
		st.Release.Eligible = true
		st.RecordSubsystem(cistate.SubsystemState{
			Name: "release", Attempted: true, AllowFailure: relAllowFailure, Outcome: "failed",
		})
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
		ReadOnly:        releaseReadOnly,
		Writer:          os.Stdout,
		Verbose:         opts.Verbose,
	}); err != nil {
		if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) {
			st.RecordSubsystem(cistate.SubsystemState{
				Name: "release", Attempted: true, AllowFailure: relAllowFailure,
				Outcome: "failed", Reason: err.Error(),
			})
		}); stErr != nil {
			fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
		}
		return fmt.Errorf("release subsystem: %w", err)
	}

	if err := cistate.UpdateState(rootDir, func(st *cistate.State) {
		st.RecordSubsystem(cistate.SubsystemState{
			Name: "release", Attempted: true, Completed: true, AllowFailure: relAllowFailure,
			Outcome: "success",
		})
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", err)
	}

	// Sync mirrors — git + release reconciliation from primary.
	syncMirrors(ctx, appCfg)

	runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
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

// renderReleaseSkip renders the Release skip panel, emission-driven.
func renderReleaseSkip(ciCtx *ci.CIContext, code releaseSkipCode, reason string) {
	col := trace.NewCollector()
	col.Decision("release", "status", "skipped", reason, "release-gate", trace.StatusSkipped)
	col.Decision("release", "reason", reason, "", "release-gate", trace.StatusInfo)
	ref := ciCtx.Branch
	if ref == "" {
		ref = ciCtx.Tag
	}
	col.Public("release", trace.CategoryInput, "ref", ref, "ci", trace.StatusInfo)
	col.Public("release", trace.CategoryInput, "sha", shortSHA(ciCtx.SHA), "ci", trace.StatusInfo)
	tag := ciCtx.Tag
	if tag == "" {
		tag = "none"
	}
	col.Public("release", trace.CategoryInput, "tag", tag, "ci", trace.StatusInfo)
	col.Decision("release", "result", releaseSkipResult(code), "", "release-gate", trace.StatusSkipped)

	color := output.UseColor()
	sec := output.NewSection(os.Stdout, "Release", 0, color)
	for _, e := range col.ForDomain("release") {
		sec.Row("%-14s%s", e.Key, e.RenderValue())
		col.MarkRendered(e)
	}
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

	// CRITICAL:
	// This map is ONLY for when.git_tags lookup on target conditions.
	//
	// versioning.tag_sources MUST remain an ORDERED SEARCH PATH for version
	// resolution. DO NOT reuse this map for version selection. Doing so
	// reintroduces global filtering and breaks the search-path invariant.
	//
	// If you find yourself thinking "I can share this map with gitver", stop
	// and re-read the INVARIANT comment at the top of gitver.DetectVersionWithOpts.
	tagPatternMap := tagPatternLookupForConditionsOnly(appCfg.Versioning.TagSources)

	hasConstraints := false
	for _, t := range releaseTargets {
		if len(t.When.GitTags) == 0 && len(t.When.Branches) == 0 {
			continue
		}
		hasConstraints = true
		if targetWhenMatches(t, tag, tagPatternMap, appCfg.Matchers.Branches) {
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
// tagPatternLookupForConditionsOnly flattens a tag_sources slice into an
// id → pattern map for the SOLE purpose of resolving target.when.git_tags
// references. The name is deliberately hostile: any reuse of this map
// for version selection would reintroduce global filtering and break the
// search-path invariant enforced by gitver.DetectVersionWithOpts.
//
// Do NOT:
//   - pass this map to gitver
//   - reuse it in detectVersion
//   - cache it at package scope
//   - rename it to something friendlier
//
// If you need a pattern lookup somewhere else in the codebase, build your
// own local map at the call site with the same CRITICAL guard comment.
// Keeping a second copy is cheaper than sharing one that tempts misuse.
func tagPatternLookupForConditionsOnly(sources []config.TagSourceConfig) map[string]string {
	m := make(map[string]string, len(sources))
	for _, ts := range sources {
		m[ts.ID] = ts.Pattern
	}
	return m
}

func tagMatchesReleasePolicy(tag string, versioning config.VersioningConfig) bool {
	if len(versioning.TagSources) == 0 {
		return true // no tag sources = all tags are eligible
	}
	for _, ts := range versioning.TagSources {
		if config.MatchPatterns([]string{ts.Pattern}, tag) {
			return true
		}
	}
	return false
}

// ── mirror sync ─────────────────────────────────────────────────────────────

// syncMirrors runs per-mirror sync: git mirror first, then release reconciliation.
// Both sync domains read from the primary — no data needs to be passed in.
// Mirror push is idempotent — safe to call even when no mutation occurred.
// Release sync is idempotent — existing releases are not recreated.
func syncMirrors(ctx context.Context, appCfg *config.Config) {
	syncMirrorsWithMode(ctx, appCfg, false)
}

func syncMirrorsWithMode(ctx context.Context, appCfg *config.Config, readOnly bool) {
	// Resolve mirrors from identity graph.
	mirrors, err := config.ResolveAllMirrors(appCfg.Repos, appCfg.Forges, appCfg.Vars)
	if err != nil {
		renderSyncPanel([]syncMirrorOutcome{{id: "mirrors", err: err}})
		return
	}
	if len(mirrors) == 0 {
		return
	}

	worktree := config.PrimaryWorktree(appCfg)

	var outcomes []syncMirrorOutcome

	// Check if any mirror wants release sync — resolve primary releases once.
	var primaryReleases []forge.ReleaseInfo
	hasReleaseSyncMirror := false
	for _, m := range mirrors {
		if m.Sync.Releases {
			hasReleaseSyncMirror = true
			break
		}
	}
	if hasReleaseSyncMirror {
		primaryURL := config.PrimaryURL(appCfg)
		if primaryURL != "" {
			provider := forge.DetectProvider(primaryURL)
			primaryClient, clientErr := newForgeClient(provider, primaryURL)
			if clientErr == nil {
				rels, listErr := primaryClient.ListReleases(ctx)
				if listErr == nil {
					primaryReleases = rels
				} else {
					outcomes = append(outcomes, syncMirrorOutcome{id: "primary/releases", err: listErr})
				}
			} else {
				outcomes = append(outcomes, syncMirrorOutcome{id: "primary/client", err: clientErr})
			}
		}
	}

	for _, m := range mirrors {
		skipReleaseSync := false

		// 1. Git mirror (if enabled)
		if m.Sync.Git {
			if readOnly {
				outcomes = append(outcomes, syncMirrorOutcome{id: m.ID, skipped: true})
			} else {
				result, err := stagefreightsync.MirrorPush(ctx, worktree, *m)
				if err != nil {
					outcomes = append(outcomes, syncMirrorOutcome{id: m.ID, err: err})
					skipReleaseSync = true
				} else if result.Status == stagefreightsync.SyncSuccess {
					outcomes = append(outcomes, syncMirrorOutcome{id: m.ID, duration: result.Duration})
				} else {
					outcomes = append(outcomes, syncMirrorOutcome{
						id:       m.ID,
						degraded: true,
						reason:   result.FailureReason,
					})
					skipReleaseSync = true
				}
			}
		}

		if skipReleaseSync {
			continue
		}

		// 2. Release reconciliation (if enabled).
		// Reads from primary, projects missing releases to mirror. Idempotent.
		if m.Sync.Releases && len(primaryReleases) > 0 {
			mirrorClient, err := forge.NewFromAccessory(m.Provider, m.BaseURL, m.Project, m.Credentials)
			if err != nil {
				outcomes = append(outcomes, syncMirrorOutcome{id: m.ID + "/releases", err: err})
				continue
			}
			mirrorReleases, err := mirrorClient.ListReleases(ctx)
			if err != nil {
				outcomes = append(outcomes, syncMirrorOutcome{id: m.ID + "/releases", err: err})
				continue
			}
			mirrorTags := make(map[string]bool, len(mirrorReleases))
			for _, r := range mirrorReleases {
				mirrorTags[r.TagName] = true
			}

			created := 0
			var releaseErr error
			for _, r := range primaryReleases {
				if mirrorTags[r.TagName] {
					continue
				}
				name := r.Name
				if name == "" {
					name = r.TagName
				}
				if readOnly {
					created++
					continue
				}
				_, createErr := mirrorClient.CreateRelease(ctx, forge.ReleaseOptions{
					TagName:     r.TagName,
					Name:        name,
					Description: r.Description,
					Draft:       r.Draft,
					Prerelease:  r.Prerelease,
				})
				if createErr != nil {
					releaseErr = createErr
				} else {
					created++
				}
			}
			if releaseErr != nil {
				outcomes = append(outcomes, syncMirrorOutcome{id: m.ID + "/releases", err: releaseErr})
			} else if created > 0 {
				label := "projected"
				if readOnly {
					label = "would project"
				}
				outcomes = append(outcomes, syncMirrorOutcome{
					id:    m.ID + "/releases",
					value: fmt.Sprintf("%d %s", created, label),
				})
			} else {
				outcomes = append(outcomes, syncMirrorOutcome{id: m.ID + "/releases", value: "in sync"})
			}
		}
	}

	renderSyncPanel(outcomes)
}

// LegacySyncOverlapsMirror returns true if a legacy sync target's provider
// is also declared as a mirror, meaning the mirror should take precedence.
// Exported for use in release_create.go where legacy sync targets are executed.
func LegacySyncOverlapsMirror(targetProvider string, appCfg *config.Config) bool {
	for _, r := range appCfg.Repos {
		if r.HasRole("mirror") {
			f := config.FindForgeByID(appCfg.Forges, r.Forge)
			if f != nil && f.Provider == targetProvider {
				return true
			}
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
func validateRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	start := time.Now()
	if strings.TrimSpace(string(appCfg.Lint.Level)) == "" {
		renderCISkip("Validate", start, "no validation configured")
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)

	if r := runnerPreflight(rootDir, runner.Options{DockerRequired: false}); r.Health == runner.Unhealthy {
		return fmt.Errorf("validate subsystem: substrate unhealthy")
	}
	if err := runConfigPhase(rootDir); err != nil {
		return fmt.Errorf("validate subsystem: %w", err)
	}

	// runUniversalLint IS the modality work for validateRunner.
	if err := runUniversalLint(ctx, appCfg, rootDir, ciCtx.IsCI(), opts.Verbose); err != nil {
		return fmt.Errorf("validate subsystem: %w", err)
	}

	runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
	return nil
}

// ── reconcile runner ────────────────────────────────────────────────────────
func reconcileRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	start := time.Now()

	hasGitOps := strings.TrimSpace(appCfg.GitOps.Cluster.Name) != ""
	hasGovernanceClusters := len(appCfg.Governance.Clusters) > 0
	hasGovernanceSource := governanceSourceConfigured(appCfg, ciCtx)

	if !hasGitOps && !hasGovernanceClusters {
		renderCISkip("Reconcile", start, "no reconcile target configured")
		return nil
	}

	rootDir := resolveWorkspace(ciCtx)

	if r := runnerPreflight(rootDir, runner.Options{DockerRequired: false}); r.Health == runner.Unhealthy {
		return fmt.Errorf("reconcile subsystem: substrate unhealthy")
	}
	if err := runConfigPhase(rootDir); err != nil {
		return fmt.Errorf("reconcile subsystem: %w", err)
	}
	if err := runUniversalLint(ctx, appCfg, rootDir, ciCtx.IsCI(), opts.Verbose); err != nil {
		return fmt.Errorf("reconcile subsystem (lint): %w", err)
	}

	// GitOps reconcile — auth resolved at runtime (CA cert, OIDC, or kubeconfig).
	// No pre-flight gate — let the runtime detect available auth and fail
	// with a clear error if nothing works.
	if hasGitOps {
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		if err := runReconcile(cmd, []string{}); err != nil {
			return err
		}
	}

	// Governance reconcile — requires clusters AND source resolvable.
	// Not mutually exclusive with gitops — both can run.
	// In CI, source is auto-resolved from CI context and apply is implicit.
	if hasGovernanceClusters {
		if !hasGovernanceSource {
			renderCISkip("Reconcile", start, "governance source not configured")
		} else {
			// CI implies --apply: the reconcile stage exists to apply.
			if err := executeGovernanceReconcile(ctx, GovernanceReconcileOpts{
				Apply:   true,
				Config:  appCfg,
				CICtx:   ciCtx,
				Verbose: opts.Verbose,
			}); err != nil {
				return err
			}
		}
	}

	runUniversalDocs(ctx, appCfg, rootDir, opts.Verbose)
	return nil
}

// ── shared runner preflight ─────────────────────────────────────────────────

// runnerPreflight runs substrate assessment, renders the Runner panel, and
// persists the report to cistate. Returns the report so callers can inspect
// Health and return subsystem-specific errors on Unhealthy.
func runnerPreflight(rootDir string, opts runner.Options) runner.ExecutionReport {
	start := time.Now()
	report := runner.Run(rootDir, opts)
	pipeline.RenderRunnerSection(os.Stdout, report, opts, output.UseColor(), time.Since(start))
	if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) { st.Runner = report }); stErr != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
	}
	return report
}

// runConfigPhase loads config, emits all config truth to a Collector, renders
// the Config panel exclusively from those emissions, and enforces the contract.
// Returns ErrContractViolation if any emission was not rendered.
func runConfigPhase(rootDir string) error {
	_, report, _ := config.LoadWithReport(rootDir + "/.stagefreight.yml")

	col := trace.NewCollector()

	// — Emit config truth ———————————————————————————————————————————————————
	col.Public("config", trace.CategoryInput, "source", report.SourceFile, "config", trace.StatusOK)

	if len(report.Presets) > 0 {
		col.Public("config", trace.CategoryInput, "presets", strings.Join(report.Presets, " → "), "config", trace.StatusOK)
		if report.Overrides > 0 {
			col.Public("config", trace.CategoryInput, "overrides", fmt.Sprintf("%d", report.Overrides), "config", trace.StatusOK)
		}
	} else {
		col.Decision("config", "presets", "none", "no presets configured", "config", trace.StatusInfo)
	}

	// Active/inactive section display — provenance per active section, dormant capability domains.
	// "inactive" only shows capability domains (full SF domains that are dormant when unconfigured).
	// Structural sections (forges, repos, etc.) are not shown in inactive — absence is self-evident.
	var activeParts, inactiveParts []string
	for _, s := range report.Sections {
		if s.Active {
			activeParts = append(activeParts, s.Name+" "+configProvenanceIcon(s.Provenance))
		} else if s.Kind == "capability" {
			inactiveParts = append(inactiveParts, s.Name+" ⛔")
		}
	}
	if len(activeParts) > 0 {
		col.Public("config", trace.CategoryInput, "active", strings.Join(activeParts, "   "), "config", trace.StatusOK)
	}
	if len(inactiveParts) > 0 {
		col.Decision("config", "inactive", strings.Join(inactiveParts, "   "), "capability domains not configured", "config", trace.StatusInfo)
	}

	if report.VarsApplied > 0 {
		col.Public("config", trace.CategoryInput, "vars", fmt.Sprintf("%d applied", report.VarsApplied), "config", trace.StatusOK)
	}

	for i, w := range report.Warnings {
		col.PublicDetail("config", trace.CategoryDecision, fmt.Sprintf("warning_%d", i+1), w, w, "config", trace.StatusWarn)
	}

	resStatus := trace.StatusOK
	if report.Status == "partial" {
		resStatus = trace.StatusWarn
	} else if report.Status == "error" {
		resStatus = trace.StatusFail
	}
	col.Decision("config", "resolution", report.Status, "config resolution completeness", "resolver", resStatus)

	modelStatus := trace.StatusOK
	modelValue := report.Completeness
	if report.Completeness != "complete" {
		modelStatus = trace.StatusWarn
		modelValue = report.Completeness + " (resolution incomplete)"
	}
	col.Decision("config", "model", modelValue, "truth completeness signal", "resolver", modelStatus)

	// — Render from emissions ONLY ——————————————————————————————————————————
	color := output.UseColor()
	sec := output.NewSection(os.Stdout, "Config", 0, color)
	for _, e := range col.ForDomain("config") {
		icon := ""
		switch e.Status {
		case trace.StatusOK:
			icon = " " + output.StatusIcon("success", color)
		case trace.StatusWarn:
			icon = " " + output.StatusIcon("warning", color)
		case trace.StatusFail:
			icon = " " + output.StatusIcon("failed", color)
		}
		sec.Row("%-16s%s%s", e.Key, e.RenderValue(), icon)
		col.MarkRendered(e)
	}
	sec.Close()

	// — Contract enforcement ————————————————————————————————————————————————
	if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) { st.Config = report }); stErr != nil {
		fmt.Fprintf(os.Stderr, "warning: pipeline state write failed: %v\n", stErr)
	}
	if unrendered := col.Unrendered(); len(unrendered) > 0 {
		pipeline.RenderContractPanel(os.Stdout, unrendered, color)
		return fmt.Errorf("%w: config panel", pipeline.ErrContractViolation)
	}
	return nil
}

// runUniversalLint runs the lint gate for any runner modality.
// Lint is always present — either runs with results, or renders explicit "skipped" (Rule 3).
// Skip case is emission-driven. Returns a non-nil error only on critical findings or engine failure.
func runUniversalLint(ctx context.Context, appCfg *config.Config, rootDir string, isCI bool, verbose bool) error {
	color := output.UseColor()
	if strings.TrimSpace(string(appCfg.Lint.Level)) == "" {
		col := trace.NewCollector()
		col.Decision("lint", "status", "skipped — not configured", "no lint level set", "config", trace.StatusSkipped)
		sec := output.NewSection(os.Stdout, "Lint", 0, color)
		for _, e := range col.ForDomain("lint") {
			sec.Row("%-16s%s", e.Key, e.RenderValue())
			col.MarkRendered(e)
		}
		sec.Close()
		return nil
	}
	_, err := pipeline.RunLint(ctx, appCfg, rootDir, isCI, color, verbose, os.Stdout)
	return err
}

// docsOutcome tracks per-generator outcomes for the Docs panel.
// Panels render from this — not from stderr. Rule 13: panel status is authoritative.
type docsOutcome struct {
	badgesOK        bool
	badgesErr       error
	narratorOK      bool
	narratorErr     error
	dockerReadmeOK  bool
	dockerReadmeErr error
	configured      bool // true if at least one generator is enabled
}

// runUniversalDocs generates badges and narrator if configured, renders the Docs panel.
// Universal postamble — every modality runs this (Rule 12).
// Does NOT commit or sync mirrors — those are docsRunner's exclusive territory.
// BUILD_STATUS is resolved from cistate before rendering so badges reflect pipeline truth.
//
// Emission-driven: all docs outcomes are emitted to a local Collector.
// Panel renders exclusively from col.ForDomain("docs"). No direct sec.Row with docs data.
func runUniversalDocs(ctx context.Context, appCfg *config.Config, rootDir string, verbose bool) {
	col := trace.NewCollector()
	gen := appCfg.Docs.Generators
	configured := gen.Badges || gen.Narrator || gen.DockerReadme

	if !configured {
		col.Decision("docs", "status", "skipped — not configured", "no docs generators enabled", "config", trace.StatusSkipped)
		renderDocsPanel(col)
		return
	}

	// Resolve BUILD_STATUS — emit the decision so operator sees where it came from.
	buildStatusSrc := "env"
	if os.Getenv("BUILD_STATUS") == "" {
		if st, err := cistate.ReadState(rootDir); err == nil {
			os.Setenv("BUILD_STATUS", st.PipelineStatus())
			buildStatusSrc = "cistate"
		} else {
			os.Setenv("BUILD_STATUS", "failing")
			buildStatusSrc = "default (state unreadable)"
		}
	}
	col.Decision("docs", "build_status", os.Getenv("BUILD_STATUS")+" (source: "+buildStatusSrc+")",
		"resolved from "+buildStatusSrc, buildStatusSrc, trace.StatusOK)

	// Execute generators — collect outcomes, then emit.
	out := docsOutcome{configured: true}

	if gen.Badges {
		if err := RunConfigBadges(appCfg, rootDir, nil, ""); err != nil {
			out.badgesErr = err
		} else {
			out.badgesOK = true
		}
	}
	if gen.Narrator {
		if err := RunNarrator(appCfg, rootDir, false, verbose); err != nil {
			out.narratorErr = err
		} else {
			out.narratorOK = true
		}
	}
	if gen.DockerReadme {
		if err := RunDockerReadme(ctx, appCfg, rootDir, false); err != nil {
			out.dockerReadmeErr = err
		} else {
			out.dockerReadmeOK = true
		}
	}

	// Emit generator outcomes as side effects.
	if gen.Badges {
		if out.badgesOK {
			col.SideEffect("docs", "badges", "generated", "", "badges-generator", trace.StatusOK)
		} else {
			col.SideEffect("docs", "badges", "failed", out.badgesErr.Error(), "badges-generator", trace.StatusFail)
		}
	}
	if gen.Narrator {
		if out.narratorOK {
			col.SideEffect("docs", "narrator", "generated", "", "narrator-generator", trace.StatusOK)
		} else {
			col.SideEffect("docs", "narrator", "failed", out.narratorErr.Error(), "narrator-generator", trace.StatusFail)
		}
	}
	if gen.DockerReadme {
		if out.dockerReadmeOK {
			col.SideEffect("docs", "docker_readme", "synced", "", "docker-readme-sync", trace.StatusOK)
		} else {
			col.SideEffect("docs", "docker_readme", "failed", out.dockerReadmeErr.Error(), "docker-readme-sync", trace.StatusFail)
		}
	}

	// Aggregate status emission — derived from generator outcomes.
	hasFailure := out.badgesErr != nil || out.narratorErr != nil || out.dockerReadmeErr != nil
	if hasFailure {
		col.Decision("docs", "status", "degraded", "one or more generators failed", "docs-aggregate", trace.StatusWarn)
	} else {
		col.Decision("docs", "status", "success", "", "docs-aggregate", trace.StatusOK)
	}

	renderDocsPanel(col)
}

// renderDocsPanel renders the Docs section exclusively from emissions in col.
// No direct sec.Row calls with docs data — panel is purely a projection of collected emissions.
func renderDocsPanel(col *trace.Collector) {
	color := output.UseColor()
	sec := output.NewSection(os.Stdout, "Docs", 0, color)
	for _, e := range col.ForDomain("docs") {
		icon := ""
		switch e.Status {
		case trace.StatusOK:
			icon = " " + output.StatusIcon("success", color)
		case trace.StatusWarn:
			icon = " " + output.StatusIcon("warning", color)
		case trace.StatusFail:
			icon = " " + output.StatusIcon("failed", color)
		}
		row := e.RenderValue()
		if e.Detail != "" && e.Status != trace.StatusOK {
			row += "  " + e.Detail
		}
		sec.Row("%-16s%s%s", e.Key, row, icon)
		col.MarkRendered(e)
	}
	sec.Close()

	// Contract enforcement — any emission not rendered above is a violation.
	if unrendered := col.Unrendered(); len(unrendered) > 0 {
		pipeline.RenderContractPanel(os.Stdout, unrendered, color)
	}
}

// renderSyncPanel renders the DomainSync panel for mirror push outcomes.
// Emission-driven: per-mirror side effects are emitted first, then panel renders
// exclusively from col.ForDomain("sync"). No direct sec.Row with sync data.
func renderSyncPanel(results []syncMirrorOutcome) {
	if len(results) == 0 {
		return
	}
	col := trace.NewCollector()

	degraded := false
	for _, r := range results {
		switch {
		case r.err != nil || r.degraded:
			degraded = true
			detail := ""
			if r.err != nil {
				detail = r.err.Error()
			} else if r.reason != "" {
				detail = string(r.reason)
			}
			col.SideEffect("sync", r.id, "failed", detail, "mirror-push", trace.StatusFail)
		case r.skipped:
			col.Decision("sync", r.id, "skipped (read-only)", "read-only mode", "mirror-push", trace.StatusSkipped)
		default:
			v := r.value
			if v == "" {
				v = "pushed (" + r.duration.Truncate(100*time.Millisecond).String() + ")"
			}
			col.SideEffect("sync", r.id, v, "", "mirror-push", trace.StatusOK)
		}
	}
	if degraded {
		col.Decision("sync", "status", "degraded", "one or more mirrors failed", "sync-aggregate", trace.StatusWarn)
	} else {
		col.Decision("sync", "status", "success", "", "sync-aggregate", trace.StatusOK)
	}

	color := output.UseColor()
	sec := output.NewSection(os.Stdout, "Sync", 0, color)
	emissions := col.ForDomain("sync")
	// Per-mirror rows first, status row last (status is always the final emission).
	for _, e := range emissions {
		icon := ""
		switch e.Status {
		case trace.StatusOK:
			icon = " " + output.StatusIcon("success", color)
		case trace.StatusWarn:
			icon = " " + output.StatusIcon("warning", color)
		case trace.StatusFail:
			icon = " " + output.StatusIcon("failed", color)
		}
		row := e.RenderValue()
		if e.Detail != "" {
			row += "  " + e.Detail
		}
		if e.Key == "status" {
			sec.Separator()
		}
		sec.Row("%-16s%s%s", e.Key, row, icon)
		col.MarkRendered(e)
	}
	sec.Close()

	if unrendered := col.Unrendered(); len(unrendered) > 0 {
		pipeline.RenderContractPanel(os.Stdout, unrendered, color)
	}
}

// syncMirrorOutcome captures the result of one mirror operation for panel rendering.
// value overrides the computed success string when set (used for non-git operations
// like release sync where the rendered text differs from "pushed (duration)").
type syncMirrorOutcome struct {
	id       string
	degraded bool
	skipped  bool
	err      error
	reason   stagefreightsync.MirrorFailureReason
	duration time.Duration
	value    string // optional: overrides computed success value in renderSyncPanel
}

// ── shared CI skip renderer ─────────────────────────────────────────────────

// configProvenanceIcon returns the icon for a section's provenance in the Config panel.
//
//	📋 manifest — operator declared it in .stagefreight.yml
//	♻️  preset   — preset engine activated it (assets that move through presets are also this)
//
// No other provenance types exist. Absence means inactive — StageFreight never
// performs work the operator did not explicitly request via manifest or preset.
func configProvenanceIcon(provenance string) string {
	switch provenance {
	case "manifest":
		return "📋"
	case "preset":
		return "♻️"
	default:
		return ""
	}
}

// renderCISkip renders a structured skip section for any CI subsystem, emission-driven.
func renderCISkip(section string, start time.Time, reason string) {
	domain := strings.ToLower(section)
	col := trace.NewCollector()
	col.Decision(domain, "status", "skipped", reason, "runner-gate", trace.StatusSkipped)
	col.Decision(domain, "reason", reason, "", "runner-gate", trace.StatusInfo)
	col.Decision(domain, "result", ciSkipResult(reason), "", "runner-gate", trace.StatusInfo)

	color := output.UseColor()
	sec := output.NewSection(os.Stdout, section, time.Since(start), color)
	for _, e := range col.ForDomain(domain) {
		sec.Row("%-14s%s", e.Key, e.RenderValue())
		col.MarkRendered(e)
	}
	sec.Close()
}

// governanceSourceConfigured checks if governance has a resolvable source.
func governanceSourceConfigured(appCfg *config.Config, ciCtx *ci.CIContext) bool {
	src, err := resolveGovernanceSourceFromOpts(GovernanceReconcileOpts{Config: appCfg, CICtx: ciCtx})
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
