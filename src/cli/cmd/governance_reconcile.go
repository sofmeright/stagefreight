package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/ci"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/governance"
)

var (
	govDryRun bool
	govApply  bool   // explicit flag required to enable real commits
	govSource string // override governance source repo URL
	govRef    string // override governance source ref
	govPath   string // override governance clusters file path
)

var governanceReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Reconcile governance policy to satellite repos",
	Long: `Reads governance clusters from the policy repo, resolves presets,
generates managed configs, and commits to satellite repos.

Forge identity (provider, URL, credentials) is read from sources.primary
in .stagefreight.yml — the same config every StageFreight repo uses.

Use --dry-run to preview changes without committing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return executeGovernanceReconcile(cmd.Context(), GovernanceReconcileOpts{
			DryRun:  govDryRun,
			Apply:   govApply,
			Source:  govSource,
			Ref:     govRef,
			Path:    govPath,
			Config:  cfg,
			Verbose: verbose,
		})
	},
}

func init() {
	governanceReconcileCmd.Flags().BoolVar(&govDryRun, "dry-run", false, "Preview changes without committing")
	governanceReconcileCmd.Flags().BoolVar(&govApply, "apply", false, "Actually commit changes (required for real writes)")
	governanceReconcileCmd.Flags().StringVar(&govSource, "source", "", "Override governance source repo URL")
	governanceReconcileCmd.Flags().StringVar(&govRef, "ref", "", "Override governance source ref")
	governanceReconcileCmd.Flags().StringVar(&govPath, "path", "", "Override governance clusters file path")
	governanceCmd.AddCommand(governanceReconcileCmd)
}

// GovernanceReconcileOpts carries all inputs for governance reconciliation.
// No package globals, no cobra dependency — all execution state is explicit.
type GovernanceReconcileOpts struct {
	DryRun  bool
	Apply   bool
	Source  string // override governance source repo URL
	Ref     string // override governance source ref
	Path    string // override governance clusters file path
	Config  *config.Config
	CICtx   *ci.CIContext // nil = not in CI (source resolution skips CI layer)
	Verbose bool
}

// executeGovernanceReconcile is the core reconcile logic.
// Takes an explicit context and opts — no cobra dependency, no package globals.
func executeGovernanceReconcile(ctx context.Context, opts GovernanceReconcileOpts) error {
	// Mode validation — exactly one must be set.
	if opts.DryRun && opts.Apply {
		return fmt.Errorf("invalid options: dry-run and apply are mutually exclusive")
	}
	if !opts.DryRun && !opts.Apply {
		return fmt.Errorf("must specify either --apply or --dry-run")
	}

	// Resolve governance source.
	source, err := resolveGovernanceSourceFromOpts(opts)
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
		n := len(c.Targets.AllRepos())
		totalRepos += n
		fmt.Fprintf(os.Stderr, "  cluster %q: %d repos\n", c.ID, n)
	}

	// Phase 2: Plan distribution.
	fmt.Fprintf(os.Stderr, "\nPlanning distribution for %d repos...\n", totalRepos)

	_, _, sourceIdentity, parseErr := config.ParseForgeURL(source.RepoURL)
	if parseErr != nil {
		return fmt.Errorf("parsing governance source URL: %w", parseErr)
	}

	// Resolve forge from sources.primary — standard StageFreight config resolution.
	forgeProvider, forgeBaseURL, forgeCreds, err := resolveGovernanceForgeFromConfig(opts.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Forge: %v (drift detection disabled)\n", err)
	}

	var adapter *forgeAdapter
	var forgeReader governance.ForgeReader
	if forgeBaseURL != "" {
		factory := &forge.BasicFactory{
			ProviderName: forgeProvider,
			BaseURL:      forgeBaseURL,
			CredPrefix:   forgeCreds,
		}
		adapter = &forgeAdapter{factory: factory, ctx: ctx}
		forgeReader = adapter
		fmt.Fprintf(os.Stderr, "Forge: %s @ %s (cred: %s_*)\n", forgeProvider, forgeBaseURL, forgeCreds)
	}

	presetSource := governance.PresetSourceInfo{
		Provider:    forgeProvider,
		ForgeURL:    forgeBaseURL,
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

	if opts.DryRun {
		governance.RenderPlanView(os.Stdout, governance.PlanViewData{
			Config: governance.PlanViewConfig{
				Mode:    "dry-run",
				Source:  sourceIdentity,
				Ref:     source.Ref,
				Verbose: opts.Verbose,
			},
			Clusters: gov.Clusters,
			Plans:    planByRepo,
		})
		return nil
	}

	// Phase 6: Commit to satellite repos (Apply mode — validated at entry).
	if adapter == nil {
		return fmt.Errorf("sources.primary required for --apply mode (forge identity not resolved)")
	}

	// Populate per-repo credential overrides from cluster targets.
	credOverrides := make(map[string]string)
	for _, p := range plans {
		if p.Credentials != "" {
			credOverrides[p.Repo] = p.Credentials
		}
	}
	adapter.credOverrides = credOverrides

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
			Verbose: opts.Verbose,
		},
		Clusters: gov.Clusters,
		Plans:    planByRepo,
		Results:  resultByRepo,
	})

	return err
}

// resolveGovernanceSourceFromOpts determines the governance source via three
// explicit resolution layers, applied in priority order:
//  1. Explicit overrides (CLI flags or caller-provided opts)
//  2. CI context inference (when running in a governance repo in CI)
//  3. Config fallback (sources.primary.url)
func resolveGovernanceSourceFromOpts(opts GovernanceReconcileOpts) (governance.GovernanceSource, error) {
	source := governance.GovernanceSource{}

	// Layer 1: Explicit overrides — highest priority.
	source.RepoURL = opts.Source
	source.Ref = opts.Ref
	source.Path = opts.Path

	// Layer 2: CI context — infer from caller-provided CI state.
	if opts.CICtx != nil && opts.CICtx.IsCI() && opts.Config != nil && opts.Config.Lifecycle.Mode == "governance" {
		if source.RepoURL == "" {
			source.RepoURL = opts.CICtx.RepoURL
		}
		if source.Ref == "" {
			source.Ref = opts.CICtx.SHA
		}
		if opts.CICtx.Workspace != "" {
			source.LocalPath = opts.CICtx.Workspace
		}
	}

	// Layer 3: Config fallback — sources.primary.url from .stagefreight.yml.
	if source.RepoURL == "" && opts.Config != nil && opts.Config.Sources.Primary.URL != "" {
		source.RepoURL = opts.Config.Sources.Primary.URL
	}

	// Default path.
	if source.Path == "" {
		source.Path = ".stagefreight.yml"
	}

	// Validate — both are required after all layers applied.
	if source.RepoURL == "" {
		return source, fmt.Errorf("governance source required: set sources.primary.url in .stagefreight.yml")
	}
	if source.Ref == "" {
		return source, fmt.Errorf("governance ref required: use --ref (pinned tag or commit SHA)")
	}

	return source, nil
}

// resolveGovernanceForgeFromConfig reads forge identity from the standard sources.primary config.
func resolveGovernanceForgeFromConfig(appCfg *config.Config) (provider, baseURL, credPrefix string, err error) {
	if appCfg == nil || appCfg.Sources.Primary.URL == "" {
		return "", "", "", fmt.Errorf("sources.primary.url not configured")
	}

	provider, baseURL, _, err = config.ParseForgeURL(appCfg.Sources.Primary.URL)
	if err != nil {
		return "", "", "", fmt.Errorf("parsing sources.primary.url: %w", err)
	}

	credPrefix = strings.ToUpper(provider)
	return provider, baseURL, credPrefix, nil
}

// forgeAdapter wraps forge.Factory to satisfy both governance.ForgeReader and governance.ForgeClient.
// Supports per-repo credential overrides via credOverrides map.
type forgeAdapter struct {
	factory       forge.Factory
	ctx           context.Context
	credOverrides map[string]string // repo → credential prefix override
}

// forgeForRepo returns a forge client for the given repo, respecting credential overrides.
func (a *forgeAdapter) forgeForRepo(repo string) (forge.Forge, error) {
	if cred, ok := a.credOverrides[repo]; ok && cred != "" {
		// Use overridden credentials — create factory with different prefix.
		baseFactory := a.factory.(*forge.BasicFactory)
		overrideFactory := &forge.BasicFactory{
			ProviderName: baseFactory.ProviderName,
			BaseURL:      baseFactory.BaseURL,
			CredPrefix:   cred,
		}
		return overrideFactory.ForRepo(a.ctx, repo)
	}
	return a.factory.ForRepo(a.ctx, repo)
}

func (a *forgeAdapter) GetFileContent(repo, path, ref string) ([]byte, error) {
	f, err := a.forgeForRepo(repo)
	if err != nil {
		return nil, fmt.Errorf("creating forge for %s: %w", repo, err)
	}
	return f.GetFileContent(a.ctx, path, ref)
}

func (a *forgeAdapter) DefaultBranch(repo string) (string, error) {
	f, err := a.forgeForRepo(repo)
	if err != nil {
		return "", fmt.Errorf("creating forge for %s: %w", repo, err)
	}
	return f.DefaultBranch(a.ctx)
}

func (a *forgeAdapter) CommitFiles(repo, branch, message string, files []governance.FileCommit) (string, error) {
	f, err := a.forgeForRepo(repo)
	if err != nil {
		return "", fmt.Errorf("creating forge for %s: %w", repo, err)
	}

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
