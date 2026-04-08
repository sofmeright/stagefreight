package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/PrPlanIT/StageFreight/src/dependency"
	"github.com/PrPlanIT/StageFreight/src/gitstate"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/output"
)

// Exit codes for dependency update.
const (
	exitOK         = 0
	exitVerifyFail = 1
	exitUpdateFail = 2
)

var (
	depDryRun     bool
	depBundle     bool
	depNoVerify   bool
	depNoVuln     bool
	depEcosystems []string
	depOutputDir  string
	depPolicy     string
)

var dependencyUpdateCmd = &cobra.Command{
	Use:   "update [path]",
	Short: "Update outdated dependencies",
	Long: `Resolve, update, and verify project dependencies.

Generates artifacts: deps.patch, deps-report.md, resolve.json.
Use --dry-run to resolve and report without applying changes.`,
	RunE: runDependencyUpdate,
}

func init() {
	dependencyUpdateCmd.Flags().BoolVar(&depDryRun, "dry-run", false, "resolve and report without applying changes")
	dependencyUpdateCmd.Flags().BoolVar(&depBundle, "bundle", false, "include deps-updated.tgz")
	dependencyUpdateCmd.Flags().BoolVar(&depNoVerify, "no-verify", false, "skip go test after update")
	dependencyUpdateCmd.Flags().BoolVar(&depNoVuln, "no-vulncheck", false, "skip govulncheck after update")
	dependencyUpdateCmd.Flags().StringSliceVar(&depEcosystems, "ecosystem", nil, "filter to specific ecosystem(s)")
	dependencyUpdateCmd.Flags().StringVar(&depOutputDir, "output", ".stagefreight/deps", "output directory for artifacts")
	dependencyUpdateCmd.Flags().StringVar(&depPolicy, "policy", "all", "update policy: all, security")

	dependencyCmd.AddCommand(dependencyUpdateCmd)
}

func runDependencyUpdate(cmd *cobra.Command, args []string) error {
	// Config-first: disabled = clean no-op
	if !cfg.Dependency.Enabled {
		fmt.Println("  dependency update disabled in config")
		return nil
	}

	// Apply config defaults when CLI flags are not explicitly set
	if !cmd.Flags().Changed("output") && cfg.Dependency.Output != "" {
		depOutputDir = cfg.Dependency.Output
	}
	if !cmd.Flags().Changed("ecosystem") {
		ecosystems := cfg.Dependency.Scope.ScopeToEcosystems()
		if len(ecosystems) > 0 {
			depEcosystems = ecosystems
		}
	}

	// Validate policy
	if depPolicy != "all" && depPolicy != "security" {
		return &ExitError{
			Code: exitUpdateFail,
			Err:  fmt.Errorf("unknown policy %q: valid values are \"all\", \"security\"", depPolicy),
		}
	}

	rootDir, err := os.Getwd()
	if err != nil {
		return &ExitError{Code: exitUpdateFail, Err: fmt.Errorf("getting working directory: %w", err)}
	}
	if len(args) > 0 {
		rootDir = args[0]
	}

	ctx := context.Background()
	color := output.UseColor()
	w := os.Stdout

	// Load freshness options from config
	var freshnessOpts map[string]any
	if mc, ok := cfg.Lint.Modules["freshness"]; ok {
		freshnessOpts = mc.Options
	}

	// Collect files via lint engine (reuse existing patterns)
	start := time.Now()
	output.SectionStart(w, "sf_deps_resolve", "Resolve")

	engine, err := lint.NewEngine(cfg.Lint, rootDir, []string{"freshness"}, nil, verbose, nil)
	if err != nil {
		output.SectionEnd(w, "sf_deps_resolve")
		return &ExitError{Code: exitUpdateFail, Err: fmt.Errorf("creating lint engine: %w", err)}
	}

	files, err := engine.CollectFiles()
	if err != nil {
		output.SectionEnd(w, "sf_deps_resolve")
		return &ExitError{Code: exitUpdateFail, Err: fmt.Errorf("collecting files: %w", err)}
	}

	// Resolve dependencies
	deps, err := freshness.ResolveDeps(ctx, freshnessOpts, files)
	if err != nil {
		output.SectionEnd(w, "sf_deps_resolve")
		return &ExitError{Code: exitUpdateFail, Err: fmt.Errorf("resolving dependencies: %w", err)}
	}

	resolveElapsed := time.Since(start)
	sec := output.NewSection(w, "Resolve", resolveElapsed, color)
	sec.Row("%-16s%d", "files scanned", len(files))
	sec.Row("%-16s%d", "dependencies", len(deps))

	outdated := 0
	withCVE := 0
	for _, d := range deps {
		if d.Latest != "" && d.Current != d.Latest {
			outdated++
		}
		if len(d.Vulnerabilities) > 0 {
			withCVE++
		}
	}
	sec.Row("%-16s%d", "outdated", outdated)
	sec.Row("%-16s%d", "with CVEs", withCVE)
	sec.Close()
	output.SectionEnd(w, "sf_deps_resolve")

	// Build update config
	updateCfg := dependency.UpdateConfig{
		RootDir:    rootDir,
		OutputDir:  depOutputDir,
		DryRun:     depDryRun,
		Bundle:     depBundle,
		Verify:     !depNoVerify,
		Vulncheck:  !depNoVuln,
		Ecosystems: depEcosystems,
		Policy:     depPolicy,
	}

	if depDryRun {
		// Dry run: generate resolve.json + report only
		return runDryRun(ctx, w, color, updateCfg, deps)
	}

	// Apply updates
	output.SectionStart(w, "sf_deps_update", "Update")
	updateStart := time.Now()

	result, err := dependency.Update(ctx, updateCfg, deps)
	updateElapsed := time.Since(updateStart)

	// Defense-in-depth: Update() should always return non-nil result,
	// but guard against regressions to prevent nil-deref panics.
	if err != nil && result == nil {
		output.SectionEnd(w, "sf_deps_update")
		return &ExitError{Code: exitUpdateFail, Err: fmt.Errorf("dependency.Update returned nil result: %w", err)}
	}

	updateSec := output.NewSection(w, "Update", updateElapsed, color)

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

	// Artifacts — print absolute paths for CI clarity
	if result.ArtifactErr != nil {
		output.RowStatus(updateSec, "artifacts", result.ArtifactErr.Error(), "warning", color)
	}
	for _, a := range result.Artifacts {
		abs, _ := filepath.Abs(a)
		updateSec.Row("artifact  %s", abs)
	}
	updateSec.Close()
	output.SectionEnd(w, "sf_deps_update")

	if err != nil {
		return &ExitError{Code: exitUpdateFail, Err: err}
	}

	if result.VerifyErr != nil {
		return &ExitError{Code: exitVerifyFail, Err: result.VerifyErr}
	}

	return nil
}

func runDryRun(ctx context.Context, w *os.File, color bool, cfg dependency.UpdateConfig, deps []freshness.Dependency) error {
	output.SectionStart(w, "sf_deps_dryrun", "Dry Run")
	start := time.Now()

	// Discover repo root for artifact generation
	repoRoot, err := discoverRepoRootFromDir(cfg.RootDir)
	if err != nil {
		output.SectionEnd(w, "sf_deps_dryrun")
		return &ExitError{Code: exitUpdateFail, Err: fmt.Errorf("not a git repository: %w", err)}
	}

	// Get tracked files for filtering
	trackedFiles, err := gitTrackedFilesFromDir(ctx, repoRoot)
	if err != nil {
		trackedFiles = nil // non-fatal for dry-run
	}

	// Filter to show what would be updated
	candidates, skipped := dependency.FilterUpdateCandidates(deps, cfg, trackedFiles)

	// Build a dry-run result for artifact generation
	result := &dependency.UpdateResult{
		Skipped: skipped,
	}
	for _, c := range candidates {
		result.Applied = append(result.Applied, dependency.AppliedUpdate{
			Dep:        c,
			OldVer:     c.Current,
			NewVer:     c.Latest,
			UpdateType: dryRunUpdateType(c),
		})
	}

	// Generate resolve.json + report (no patch in dry-run)
	outputDir := cfg.OutputDir
	if outputDir == "" {
		outputDir = ".stagefreight/deps"
	}
	if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join(repoRoot, outputDir)
	}

	artifacts, genErr := dependency.GenerateArtifacts(ctx, repoRoot, outputDir, result, false)

	elapsed := time.Since(start)
	sec := output.NewSection(w, "Dry Run", elapsed, color)

	appliedDeps := toOutputApplied(result.Applied)
	output.SectionApplied(sec, "Would update", appliedDeps, color)

	skippedGroups := aggregateSkippedItemized(result.Skipped)
	output.SectionSkippedItemized(sec, "Would skip", skippedGroups, color)

	cves := collectCVEsFixed(result.Applied)
	output.SectionCVEs(sec, cves, color)

	sec.Separator()

	if genErr != nil {
		output.RowStatus(sec, "artifacts", genErr.Error(), "warning", color)
	}
	for _, a := range artifacts {
		abs, _ := filepath.Abs(a)
		sec.Row("artifact  %s", abs)
	}
	sec.Close()
	output.SectionEnd(w, "sf_deps_dryrun")

	return nil
}

func dryRunUpdateType(dep freshness.Dependency) string {
	if dep.Latest == "" || dep.Current == dep.Latest {
		return "tag"
	}
	delta := freshness.CompareDependencyVersions(dep.Current, dep.Latest, dep.Ecosystem)
	if delta.IsZero() {
		return "tag"
	}
	return freshness.DominantUpdateType(delta)
}

// discoverRepoRootFromDir finds the git repository root from the given directory.
func discoverRepoRootFromDir(dir string) (string, error) {
	repo, err := gitstate.OpenRepo(dir)
	if err != nil {
		return "", err
	}
	return gitstate.RepoRoot(repo)
}

func gitTrackedFilesFromDir(_ context.Context, repoRoot string) (map[string]bool, error) {
	repo, err := gitstate.OpenRepo(repoRoot)
	if err != nil {
		return nil, err
	}
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolving HEAD: %w", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("loading HEAD commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("loading HEAD tree: %w", err)
	}
	tracked := make(map[string]bool)
	err = tree.Files().ForEach(func(f *object.File) error {
		tracked[f.Name] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterating tree: %w", err)
	}
	return tracked, nil
}

// toOutputApplied converts dependency.AppliedUpdate to output.AppliedDep.
// Falls back to Dep.Vulnerabilities IDs when CVEsFixed is empty (dry-run path).
func toOutputApplied(updates []dependency.AppliedUpdate) []output.AppliedDep {
	out := make([]output.AppliedDep, len(updates))
	for i, u := range updates {
		cveIDs := u.CVEsFixed
		if len(cveIDs) == 0 && len(u.Dep.Vulnerabilities) > 0 {
			for _, v := range u.Dep.Vulnerabilities {
				cveIDs = append(cveIDs, v.ID)
			}
		}
		out[i] = output.AppliedDep{
			Name:       u.Dep.Name,
			OldVer:     u.OldVer,
			NewVer:     u.NewVer,
			UpdateType: u.UpdateType,
			CVEsFixed:  cveIDs,
		}
	}
	return out
}

// aggregateSkippedItemized groups skipped deps by reason, preserving item details.
func aggregateSkippedItemized(skipped []dependency.SkippedDep) []output.SkippedItemGroup {
	type groupAcc struct {
		items []output.SkippedItem
	}
	acc := make(map[string]*groupAcc)
	for _, s := range skipped {
		reason := dependency.NormalizeSkipReason(s.Reason)
		g, ok := acc[reason]
		if !ok {
			g = &groupAcc{}
			acc[reason] = g
		}
		g.items = append(g.items, output.SkippedItem{
			Name:      s.Dep.Name,
			Version:   s.Dep.Current,
			Ecosystem: s.Dep.Ecosystem,
		})
	}

	groups := make([]output.SkippedItemGroup, 0, len(acc))
	for reason, g := range acc {
		// Sort items alphabetically by name
		sort.Slice(g.items, func(i, j int) bool {
			return g.items[i].Name < g.items[j].Name
		})
		groups = append(groups, output.SkippedItemGroup{
			Reason: reason,
			Count:  len(g.items),
			Items:  g.items,
		})
	}
	return groups
}

// collectCVEsFixed deduplicates and sorts CVEs resolved by applied updates.
func collectCVEsFixed(updates []dependency.AppliedUpdate) []output.CVEFixed {
	seen := make(map[string]bool)
	var cves []output.CVEFixed

	for _, u := range updates {
		for _, v := range u.Dep.Vulnerabilities {
			if seen[v.ID] {
				continue
			}
			seen[v.ID] = true
			cves = append(cves, output.CVEFixed{
				ID:       v.ID,
				Severity: v.Severity,
				Summary:  v.Summary,
				FixedIn:  u.NewVer,
				FixedBy:  u.Dep.Name,
			})
		}
	}

	sort.SliceStable(cves, func(i, j int) bool {
		ri, rj := cveSeverityRank(cves[i].Severity), cveSeverityRank(cves[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return cves[i].ID < cves[j].ID
	})

	return cves
}

// cveSeverityRank returns a sort rank (lower = more severe).
func cveSeverityRank(severity string) int {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "CRITICAL":
		return 0
	case "HIGH":
		return 1
	case "MODERATE", "MEDIUM":
		return 2
	case "LOW":
		return 3
	default:
		return 4
	}
}
