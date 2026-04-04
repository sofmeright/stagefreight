package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/governance"
)

var (
	govDryRun      bool
	govApply       bool   // explicit flag required to enable real commits
	govSource      string // override governance source repo URL
	govRef         string // override governance source ref
	govPath        string // override governance clusters file path
	govProvider    string // forge provider for target repos (gitlab, github, gitea)
	govForgeURL    string // forge base URL for target repos
	govCredPrefix  string // credential env var prefix for forge API
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
	governanceReconcileCmd.Flags().BoolVar(&govApply, "apply", false, "Actually commit changes (required for real writes)")
	governanceReconcileCmd.Flags().StringVar(&govSource, "source", "", "Override governance source repo URL")
	governanceReconcileCmd.Flags().StringVar(&govRef, "ref", "", "Override governance source ref")
	governanceReconcileCmd.Flags().StringVar(&govPath, "path", "", "Override governance clusters file path")
	governanceReconcileCmd.Flags().StringVar(&govProvider, "provider", "gitlab", "Forge provider for target repos (gitlab, github, gitea)")
	governanceReconcileCmd.Flags().StringVar(&govForgeURL, "forge-url", "", "Forge base URL for target repos (e.g., https://gitlab.prplanit.com)")
	governanceReconcileCmd.Flags().StringVar(&govCredPrefix, "cred-prefix", "GITLAB", "Credential env var prefix for forge API")
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

	// Phase 2: Plan distribution.
	// Assets (skeleton, settings, etc.) are declared in each cluster's stagefreight.assets
	// and resolved by the distributor via AssetFetcher. No separate skeleton/aux code paths.
	fmt.Fprintf(os.Stderr, "\nPlanning distribution for %d repos...\n", totalRepos)

	sourceIdentity := extractIdentity(source.RepoURL)

	// Build forge adapter for drift detection + commits.
	// Single factory, single adapter — used for both read and write.
	var adapter *forgeAdapter
	var forgeReader governance.ForgeReader
	if govForgeURL != "" {
		factory := &forge.BasicFactory{
			ProviderName: govProvider,
			BaseURL:      govForgeURL,
			CredPrefix:   govCredPrefix,
		}
		adapter = &forgeAdapter{factory: factory, ctx: cmd.Context()}
		forgeReader = adapter
		fmt.Fprintf(os.Stderr, "Forge: %s @ %s (cred: %s_*)\n", govProvider, govForgeURL, govCredPrefix)
	} else {
		fmt.Fprintln(os.Stderr, "Forge: not configured (no --forge-url, drift detection disabled)")
	}

	presetSource := governance.PresetSourceInfo{
		Provider:    govProvider,
		ForgeURL:    govForgeURL,
		ProjectID:   sourceIdentity,
		Ref:         source.Ref,
		CachePolicy: "authoritative",
	}

	plans, err := governance.PlanDistribution(
		gov, presetLoader, governance.FetchFile,
		forgeReader,
		presetSource, sourceIdentity,
	)
	if err != nil {
		return fmt.Errorf("planning distribution: %w", err)
	}

	// Phase 5: Render plan view.
	planByRepo := make(map[string]governance.DistributionPlan, len(plans))
	for _, p := range plans {
		planByRepo[p.Repo] = p
	}

	if govDryRun {
		governance.RenderPlanView(os.Stdout, governance.PlanViewData{
			Config: governance.PlanViewConfig{
				Mode:    "dry-run",
				Source:  sourceIdentity,
				Ref:     source.Ref,
				Verbose: verbose,
			},
			Clusters: gov.Clusters,
			Plans:    planByRepo,
		})
		return nil
	}

	// Phase 6: Commit to satellite repos.
	if !govApply {
		fmt.Fprintln(os.Stderr, "\nUse --apply to commit changes, or --dry-run to preview")
		return nil
	}

	if adapter == nil {
		return fmt.Errorf("--forge-url required for --apply mode")
	}

	fmt.Fprintln(os.Stderr, "\nCommitting to satellite repos...")
	results, err := governance.CommitDistribution(plans, adapter, sourceIdentity, source.Ref, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nReconcile completed with errors\n")
	}

	resultByRepo := make(map[string]governance.CommitResult, len(results))
	for _, r := range results {
		resultByRepo[r.Repo] = r
	}

	governance.RenderPlanView(os.Stdout, governance.PlanViewData{
		Config: governance.PlanViewConfig{
			Mode:    "apply",
			Source:  sourceIdentity,
			Ref:     source.Ref,
			Verbose: verbose,
		},
		Clusters: gov.Clusters,
		Plans:    planByRepo,
		Results:  resultByRepo,
	})

	return err
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

// forgeReaderAdapter wraps a forge.Factory to satisfy governance.ForgeReader.
// forgeAdapter wraps forge.Factory to satisfy both governance.ForgeReader and governance.ForgeClient.
// Governance selects repos; the factory materializes per-repo forge clients.
type forgeAdapter struct {
	factory forge.Factory
	ctx     context.Context
}

func (a *forgeAdapter) GetFileContent(repo, path, ref string) ([]byte, error) {
	f, err := a.factory.ForRepo(a.ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("creating forge for %s: %w", repo, err)
	}
	return f.GetFileContent(a.ctx, path, ref)
}

func (a *forgeAdapter) DefaultBranch(repo string) (string, error) {
	f, err := a.factory.ForRepo(a.ctx, repo)
	if err != nil {
		return "", fmt.Errorf("creating forge for %s: %w", repo, err)
	}
	return f.DefaultBranch(a.ctx)
}

func (a *forgeAdapter) CommitFiles(repo, branch, message string, files []governance.FileCommit) (string, error) {
	f, err := a.factory.ForRepo(a.ctx, repo)
	if err != nil {
		return "", fmt.Errorf("creating forge for %s: %w", repo, err)
	}

	// Convert governance FileCommit to forge FileAction.
	forgeFiles := make([]forge.FileAction, 0, len(files))
	for _, fc := range files {
		forgeFiles = append(forgeFiles, forge.FileAction{
			Path:    fc.Path,
			Content: fc.Content,
		})
	}

	result, err := f.CommitFiles(a.ctx, forge.CommitFilesOptions{
		Branch:  branch,
		Message: message,
		Files:   forgeFiles,
	})
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", nil
	}
	return result.SHA, nil
}

