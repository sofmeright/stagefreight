package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/PrPlanIT/StageFreight/src/ci"
	"github.com/PrPlanIT/StageFreight/src/commit"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/dependency"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/output"
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

	if err := runDockerBuild(dockerBuildCmd, nil); err != nil {
		return fmt.Errorf("build subsystem: %w", err)
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

	// Run dependency update via the same code path as the CLI command
	result, err := runDependencyUpdateLogic(ctx, appCfg, rootDir, opts.Verbose)
	if err != nil {
		return fmt.Errorf("deps subsystem: %w", err)
	}

	// Auto-commit if configured and files changed
	if appCfg.Dependency.Commit.Enabled && len(result.FilesChanged) > 0 {
		if err := autoCommitViaPlanner(ctx, appCfg, rootDir, commit.PlannerOptions{
			Type:    appCfg.Dependency.Commit.Type,
			Scope:   "deps",
			Message: appCfg.Dependency.Commit.Message,
			Paths:   result.FilesChanged,
			SkipCI:  boolPtr(appCfg.Dependency.Commit.SkipCI),
			Push:    boolPtr(appCfg.Dependency.Commit.Push),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: dependency auto-commit failed: %v\n", err)
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

	// Set config defaults on package-level vars and invoke the scan function directly.
	// This avoids os/exec while reusing the full scan pipeline.
	if appCfg.Security.OutputDir != "" {
		secScanOutputDir = appCfg.Security.OutputDir
	}

	if err := runSecurityScan(securityScanCmd, nil); err != nil {
		return fmt.Errorf("security subsystem: %w", err)
	}
	return nil
}

// ── docs runner ──────────────────────────────────────────────────────────────
func docsRunner(ctx context.Context, appCfg *config.Config, ciCtx *ci.CIContext, opts ci.RunOptions) error {
	if !appCfg.Docs.Enabled {
		fmt.Println("  docs generation disabled in config")
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
		if err := autoCommitViaPlanner(ctx, appCfg, rootDir, commit.PlannerOptions{
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
	if !appCfg.Release.Enabled {
		fmt.Println("  release disabled in config")
		return nil
	}

	tag := opts.Tag
	if tag == "" {
		tag = ciCtx.Tag
	}
	if tag == "" {
		return fmt.Errorf("release subsystem: no tag available (set SF_CI_TAG or pass --tag)")
	}

	// Set config defaults on package-level vars and invoke release create directly.
	rcTag = tag
	if appCfg.Release.SecuritySummary != "" {
		rcSecuritySummary = appCfg.Release.SecuritySummary
	}
	rcRegistryLinks = appCfg.Release.RegistryLinks
	rcCatalogLinks = appCfg.Release.CatalogLinks

	if err := runReleaseCreate(releaseCreateCmd, nil); err != nil {
		return fmt.Errorf("release subsystem: %w", err)
	}
	return nil
}

// ── commit helpers ───────────────────────────────────────────────────────────

// autoCommitViaPlanner uses commit.BuildPlan + backend.Execute for auto-commit.
// Non-fatal — callers should log warnings on error.
func autoCommitViaPlanner(ctx context.Context, appCfg *config.Config, rootDir string, opts commit.PlannerOptions) error {
	registry := commit.NewTypeRegistry(appCfg.Commit.Types)
	plan, err := commit.BuildPlan(opts, appCfg.Commit, registry, rootDir)
	if err != nil {
		return fmt.Errorf("auto-commit plan: %w", err)
	}

	backend := &commit.GitBackend{RootDir: rootDir}
	result, err := backend.Execute(ctx, plan, appCfg.Commit.Conventional)
	if err != nil {
		return fmt.Errorf("auto-commit execute: %w", err)
	}
	if result.NoOp {
		fmt.Println("  auto-commit: nothing to commit")
		return nil
	}
	fmt.Printf("  auto-commit: %s\n", result.SHA)
	return nil
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}
