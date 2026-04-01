package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/governance"
)

var (
	govDryRun    bool
	govSource    string // override governance source repo URL
	govRef       string // override governance source ref
	govPath      string // override governance clusters file path
)

var governanceReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Reconcile governance policy to satellite repos",
	Long: `Reads governance clusters from the policy repo, resolves presets,
generates managed configs, and commits to satellite repos.

Use --dry-run to preview changes without committing.`,
	RunE: runGovernanceReconcile,
}

func init() {
	governanceReconcileCmd.Flags().BoolVar(&govDryRun, "dry-run", false, "Preview changes without committing")
	governanceReconcileCmd.Flags().StringVar(&govSource, "source", "", "Override governance source repo URL")
	governanceReconcileCmd.Flags().StringVar(&govRef, "ref", "", "Override governance source ref")
	governanceReconcileCmd.Flags().StringVar(&govPath, "path", "", "Override governance clusters file path")
	governanceCmd.AddCommand(governanceReconcileCmd)
}

func runGovernanceReconcile(cmd *cobra.Command, args []string) error {
	// Resolve governance source — CLI flags override config.
	source, err := resolveGovernanceSource()
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Governance source: %s @ %s\n", source.RepoURL, source.Ref)
	fmt.Fprintf(os.Stderr, "Clusters path: %s\n", source.Path)

	// Phase 1: Load governance config + presets.
	fmt.Fprintln(os.Stderr, "\nLoading governance config...")
	gov, presetLoader, err := governance.LoadGovernance(source)
	if err != nil {
		return fmt.Errorf("loading governance: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  clusters: %d\n", len(gov.Clusters))
	totalRepos := 0
	for _, c := range gov.Clusters {
		totalRepos += len(c.Targets.Repos)
		fmt.Fprintf(os.Stderr, "  cluster %q: %d repos\n", c.ID, len(c.Targets.Repos))
	}

	// Phase 2: Load skeleton (if configured).
	var skeleton []byte
	if gov.Skeleton.Source.RepoURL != "" {
		fmt.Fprintf(os.Stderr, "\nSkeleton source: %s @ %s path=%s\n",
			gov.Skeleton.Source.RepoURL, gov.Skeleton.Source.Ref, gov.Skeleton.Source.Path)
		// TODO: fetch skeleton from declared source.
		// For now, skeleton distribution requires separate implementation.
		fmt.Fprintln(os.Stderr, "  skeleton distribution: not yet implemented")
	}

	// Phase 3: Load auxiliary files (claude-code settings, etc.).
	auxFiles := loadAuxFiles(presetLoader)

	// Phase 4: Plan distribution.
	fmt.Fprintf(os.Stderr, "\nPlanning distribution for %d repos...\n", totalRepos)

	sourceIdentity := extractIdentity(source.RepoURL)

	// In dry-run, we don't have a forge reader — plan without drift detection.
	plans, err := governance.PlanDistribution(
		gov, presetLoader, skeleton, auxFiles,
		nil, // no forge reader in dry-run (TODO: add real forge reader for live mode)
		sourceIdentity, source.Ref,
	)
	if err != nil {
		return fmt.Errorf("planning distribution: %w", err)
	}

	// Phase 5: Show results.
	fmt.Fprintln(os.Stderr, "")
	for _, plan := range plans {
		if !plan.HasChanges() {
			fmt.Fprintf(os.Stderr, "  %s: unchanged\n", plan.Repo)
			continue
		}

		fmt.Fprintf(os.Stderr, "  %s:\n", plan.Repo)
		for _, f := range plan.Files {
			if f.Action == "unchanged" {
				continue
			}
			label := f.Action
			if f.Drifted {
				label += " (drifted)"
			}
			fmt.Fprintf(os.Stderr, "    %s: %s (%d bytes)\n", f.Path, label, len(f.Content))
		}
	}

	if govDryRun {
		fmt.Fprintln(os.Stderr, "\n--dry-run: no commits made")

		// Output rendered managed configs to stdout for inspection.
		for _, plan := range plans {
			for _, f := range plan.Files {
				if f.Path == ".stagefreight/stagefreight-managed.yml" && f.Action != "unchanged" {
					fmt.Fprintf(os.Stdout, "--- %s ---\n", plan.Repo)
					fmt.Fprint(os.Stdout, string(f.Content))
					fmt.Fprintln(os.Stdout, "")
				}
			}
		}
		return nil
	}

	// Phase 6: Commit (not yet wired — requires forge client).
	fmt.Fprintln(os.Stderr, "\nCommit phase: not yet implemented (use --dry-run to preview)")
	return nil
}

// resolveGovernanceSource determines the governance source from CLI flags or config.
func resolveGovernanceSource() (governance.GovernanceSource, error) {
	source := governance.GovernanceSource{}

	// CLI flags take priority.
	if govSource != "" {
		source.RepoURL = govSource
	}
	if govRef != "" {
		source.Ref = govRef
	}
	if govPath != "" {
		source.Path = govPath
	}

	// Fall back to config if available.
	if cfg != nil {
		// TODO: read governance.source from parsed config once the field exists.
		// For now, CLI flags are required.
	}

	// Defaults.
	if source.Path == "" {
		source.Path = "governance/clusters.yml"
	}

	if source.RepoURL == "" {
		return source, fmt.Errorf("governance source required: use --source or configure governance.source in .stagefreight.yml")
	}
	if source.Ref == "" {
		return source, fmt.Errorf("governance ref required: use --ref (pinned tag or commit SHA)")
	}

	return source, nil
}

// loadAuxFiles loads auxiliary files from the policy repo for distribution.
func loadAuxFiles(loader governance.PresetLoader) map[string][]byte {
	files := make(map[string][]byte)

	// Claude Code project settings.
	if data, err := loader.Load("claude-code/project-settings.json"); err == nil {
		files[".claude/settings.json"] = data
	}

	// Future: precommit, renovate, etc.

	return files
}

// extractIdentity extracts "org/repo" from a full URL.
func extractIdentity(repoURL string) string {
	// Strip protocol.
	s := repoURL
	for _, prefix := range []string{"https://", "http://", "ssh://", "git@"} {
		s = strings.TrimPrefix(s, prefix)
	}
	// Strip host.
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	// Strip .git suffix.
	s = strings.TrimSuffix(s, ".git")
	return s
}
