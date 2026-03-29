package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/gitops"
)

var gitopsCmd = &cobra.Command{
	Use:   "gitops",
	Short: "GitOps intelligence — inspect, impact, reconcile",
}

var (
	impactBase    string
	impactHead    string
	reconcileDry  bool
	reconcileAll  bool
	reconcileOnly string
)

var gitopsImpactCmd = &cobra.Command{
	Use:   "impact",
	Short: "Compute which kustomizations are affected by recent changes",
	Long: `Determine which Flux Kustomizations are affected by file changes
between two refs. Walks the reverse dependency graph for transitive impact.
Outputs the ordered reconcile set.`,
	RunE: runGitopsImpact,
}

var gitopsReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Reconcile affected Flux kustomizations",
	Long: `Reconcile Flux kustomizations affected by recent changes.
By default, computes impact from HEAD~1..HEAD and reconciles the affected set.
Use --all to reconcile everything, or --only to target a specific kustomization.`,
	RunE: runGitopsReconcile,
}

var gitopsInspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Discover and display the Flux dependency graph",
	Long: `Walk the repository and discover all Flux Kustomization objects.
Display the dependency graph, paths, orphans, and bootstrap state.

No configuration needed — everything is derived from actual manifests.`,
	RunE: runGitopsInspect,
}

func init() {
	gitopsImpactCmd.Flags().StringVar(&impactBase, "base", "HEAD~1", "base ref for diff")
	gitopsImpactCmd.Flags().StringVar(&impactHead, "head", "HEAD", "head ref for diff")

	gitopsReconcileCmd.Flags().BoolVar(&reconcileDry, "dry-run", false, "preview reconcile set without executing")
	gitopsReconcileCmd.Flags().BoolVar(&reconcileAll, "all", false, "reconcile all kustomizations")
	gitopsReconcileCmd.Flags().StringVar(&reconcileOnly, "only", "", "reconcile only this kustomization (ns/name)")

	gitopsCmd.AddCommand(gitopsInspectCmd)
	gitopsCmd.AddCommand(gitopsImpactCmd)
	gitopsCmd.AddCommand(gitopsReconcileCmd)
	rootCmd.AddCommand(gitopsCmd)
}

func runGitopsInspect(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	graph, err := gitops.DiscoverFluxGraph(rootDir)
	if err != nil {
		return fmt.Errorf("discovering flux graph: %w", err)
	}

	if len(graph.Kustomizations) == 0 {
		fmt.Println("  No Flux Kustomizations discovered.")
		return nil
	}

	// Collect and sort for deterministic output
	var keys []gitops.KustomizationKey
	for k := range graph.Kustomizations {
		keys = append(keys, k)
	}
	gitops.SortKeys(keys)

	fmt.Printf("Kustomizations: %d\n\n", len(keys))

	for _, key := range keys {
		node := graph.Kustomizations[key]
		fmt.Printf("  %s\n", key)
		if node.Path != "" {
			fmt.Printf("    path: %s\n", node.Path)
		}
		if node.SourceRef != "" {
			fmt.Printf("    source: %s\n", node.SourceRef)
		}
		if len(node.DependsOn) > 0 {
			deps := make([]string, len(node.DependsOn))
			for i, d := range node.DependsOn {
				deps[i] = d.String()
			}
			fmt.Printf("    dependsOn: [%s]\n", strings.Join(deps, ", "))
		}
		// Show reverse deps (dependents)
		if revDeps := graph.ReverseDeps[key]; len(revDeps) > 0 {
			deps := make([]string, len(revDeps))
			for i, d := range revDeps {
				deps[i] = d.String()
			}
			fmt.Printf("    dependents: [%s]\n", strings.Join(deps, ", "))
		}
		fmt.Println()
	}

	// Duplicate path detection
	dupes := gitops.DuplicatePaths(graph)
	if len(dupes) > 0 {
		fmt.Println("Warnings:")
		for path, owners := range dupes {
			names := make([]string, len(owners))
			for i, o := range owners {
				names[i] = o.String()
			}
			fmt.Printf("  duplicate path owners: %s → %s\n", path, strings.Join(names, ", "))
		}
		fmt.Println()
	}

	// Orphans
	orphans := gitops.Orphans(graph)
	if len(orphans) > 0 {
		fmt.Println("Orphans (no deps, no dependents):")
		for _, o := range orphans {
			fmt.Printf("  %s\n", o)
		}
		fmt.Println()
	}

	// Bootstrap
	bootstrap := gitops.DetectBootstrapRequired(graph)
	if bootstrap.Required {
		fmt.Printf("Bootstrap: REQUIRED — %s\n", bootstrap.Reason)
	} else {
		fmt.Println("Bootstrap: not required")
	}

	return nil
}

func runGitopsImpact(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	graph, err := gitops.DiscoverFluxGraph(rootDir)
	if err != nil {
		return fmt.Errorf("discovering flux graph: %w", err)
	}

	files, err := gitops.GetChangedFiles(rootDir, impactBase, impactHead)
	if err != nil {
		return fmt.Errorf("getting changed files: %w", err)
	}

	if len(files) == 0 {
		fmt.Println("No changed files.")
		return nil
	}

	impact := gitops.ComputeImpact(graph, files)

	fmt.Printf("Changed files: %d\n", len(impact.ChangedFiles))
	for _, f := range impact.ChangedFiles {
		fmt.Printf("  %s\n", f)
	}

	if len(impact.UnmappedFiles) > 0 {
		fmt.Printf("\nUnmapped (not under any kustomization path): %d\n", len(impact.UnmappedFiles))
		for _, f := range impact.UnmappedFiles {
			fmt.Printf("  %s\n", f)
		}
	}

	fmt.Printf("\nDirectly affected: %d\n", len(impact.DirectlyAffected))
	for _, k := range impact.DirectlyAffected {
		fmt.Printf("  %s\n", k)
	}

	if len(impact.TransitivelyAffected) > len(impact.DirectlyAffected) {
		fmt.Printf("\nTransitively affected: %d\n", len(impact.TransitivelyAffected))
		for _, k := range impact.TransitivelyAffected {
			fmt.Printf("  %s\n", k)
		}
	}

	fmt.Printf("\nReconcile set (ordered):\n")
	for i, k := range impact.ReconcileSet {
		fmt.Printf("  %d. %s\n", i+1, k)
	}

	return nil
}

// runGitopsReconcile delegates to the universal reconcile command.
// Kept for backward compatibility: `sf gitops reconcile` = `sf reconcile` when mode=gitops.
func runGitopsReconcile(cmd *cobra.Command, args []string) error {
	reconcileGlobalDry = reconcileDry
	return runReconcile(cmd, args)
}
