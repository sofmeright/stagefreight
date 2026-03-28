package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/commit"
	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/output"
)

var (
	commitType    string
	commitScope   string
	commitMessage string
	commitBody    string
	commitAdd     []string
	commitAll     bool
	commitBreak   bool
	commitSkipCI  bool
	commitPush    bool
	commitRemote  string
	commitRefspec string
	commitDryRun  bool
	commitSignOff bool
)

var commitCmd = &cobra.Command{
	Use:   "commit [summary] [paths...]",
	Short: "Create a conventional commit from staged or specified files",
	Long: `Create a git commit with conventional commit formatting.

Summary can be provided as a positional argument or via --message.
Paths can be provided as positional args (after summary or after --),
via --add flags, --all, or from the existing staging area.

In CI environments, the push refspec is auto-detected from CI_COMMIT_REF_NAME
or CI_COMMIT_BRANCH. Use --refspec for explicit control.

Examples:
  stagefreight commit -t docs -m "refresh generated docs"
  stagefreight commit -t docs "refresh generated docs"
  stagefreight commit -t feat "add api validation" src/api/ src/config/config.go
  stagefreight commit -t fix -m "handle nil config" -- src/api/ src/config/config.go
  stagefreight commit -t docs --add README.md -m "document commit flow" -- docs/ examples/
  stagefreight commit --dry-run -t docs -m "test generated docs" --add docs/ -- README.md
  stagefreight commit -t feat --breaking -m "replace auth middleware" -- src/auth/
  stagefreight commit -t docs -m "refresh docs" --push --refspec HEAD:refs/heads/main
  stagefreight commit -t feat -m "hotfix auth flow" --push --refspec HEAD:refs/heads/release/v1`,
	Args: cobra.ArbitraryArgs,
	RunE: runCommit,
}

func init() {
	commitCmd.Flags().StringVarP(&commitType, "type", "t", "", "commit type (e.g. feat, fix, docs, chore)")
	commitCmd.Flags().StringVarP(&commitScope, "scope", "s", "", "commit scope")
	commitCmd.Flags().StringVarP(&commitMessage, "message", "m", "", "commit summary message")
	commitCmd.Flags().StringVar(&commitBody, "body", "", "commit body (appended after blank line)")
	commitCmd.Flags().StringSliceVar(&commitAdd, "add", nil, "files/dirs to stage (repeatable, supports globs)")
	commitCmd.Flags().BoolVar(&commitAll, "all", false, "stage all changes (git add -A)")
	commitCmd.Flags().BoolVar(&commitBreak, "breaking", false, "mark as breaking change (!)")
	commitCmd.Flags().BoolVar(&commitSkipCI, "skip-ci", false, "append [skip ci] to subject line")
	commitCmd.Flags().BoolVar(&commitPush, "push", false, "push after commit")
	commitCmd.Flags().StringVar(&commitRemote, "remote", "origin", "git remote for push")
	commitCmd.Flags().StringVar(&commitRefspec, "refspec", "", "push refspec (e.g. HEAD:refs/heads/main)")
	commitCmd.Flags().BoolVar(&commitDryRun, "dry-run", false, "show what would be committed without executing")
	commitCmd.Flags().BoolVar(&commitSignOff, "sign-off", false, "add Signed-off-by trailer")

	rootCmd.AddCommand(commitCmd)
}

func runCommit(cmd *cobra.Command, args []string) error {
	start := time.Now()
	ctx := context.Background()

	// Resolve summary and positional paths.
	// With -m: all positional args are paths.
	// Without -m: first positional arg is summary, rest are paths.
	summary := commitMessage
	var pathArgs []string
	if summary != "" {
		pathArgs = args
	} else if len(args) > 0 {
		summary = args[0]
		pathArgs = args[1:]
	}
	commitAdd = append(commitAdd, pathArgs...)

	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	registry := commit.NewTypeRegistry(cfg.Commit.Types)

	// Build planner options
	opts := commit.PlannerOptions{
		Type:    commitType,
		Scope:   commitScope,
		Message: summary,
		Body:    commitBody,
		Paths:   commitAdd,
		All:     commitAll,
		SignOff: commitSignOff,
		Remote:  commitRemote,
		Refspec: commitRefspec,
	}
	if cmd.Flags().Changed("breaking") {
		opts.Breaking = commitBreak
	}
	if cmd.Flags().Changed("skip-ci") {
		opts.SkipCI = &commitSkipCI
	}
	if cmd.Flags().Changed("push") {
		opts.Push = &commitPush
	}

	plan, err := commit.BuildPlan(opts, cfg.Commit, registry, rootDir)
	if err != nil {
		return err
	}

	// Validate backend config
	switch cfg.Commit.Backend {
	case "", "git", "forge":
		// valid
	default:
		return fmt.Errorf("invalid commit backend %q (valid: git, forge)", cfg.Commit.Backend)
	}

	// Select backend
	var backend commit.Backend
	switch {
	case commitDryRun:
		backend = &commit.DryRunBackend{RootDir: rootDir}
	case shouldUseForge(plan, cfg.Commit.Backend):
		fc, branch, err := detectForgeForPush(rootDir, plan)
		if err != nil {
			if cfg.Commit.Backend == "forge" {
				return fmt.Errorf("forge backend requested but detection failed: %w", err)
			}
			if verbose {
				fmt.Fprintf(os.Stderr, "commit: forge detection failed: %s\n", err)
				fmt.Fprintf(os.Stderr, "commit: falling back to git push\n")
			}
			backend = &commit.GitBackend{RootDir: rootDir}
		} else {
			backend = &commit.ForgeBackend{
				RootDir:     rootDir,
				ForgeClient: fc,
				Branch:      branch,
			}
		}
	default:
		backend = &commit.GitBackend{RootDir: rootDir}
	}

	// Wire hook output renderer for git backend
	if gb, ok := backend.(*commit.GitBackend); ok {
		hookSectionOpened := false
		gb.OnCommitLine = func(stream, line string) {
			if !hookSectionOpened {
				fmt.Fprintln(os.Stdout)
				hookSectionOpened = true
			}
			fmt.Fprintf(os.Stdout, "    │ %s\n", line)
		}
	}

	result, err := backend.Execute(ctx, plan, cfg.Commit.Conventional)
	if err != nil {
		return err
	}

	// Render output
	elapsed := time.Since(start)
	useColor := output.UseColor()
	w := os.Stdout
	sec := output.NewSection(w, "Commit", elapsed, useColor)

	// Type display
	typeDisplay := plan.Type
	if plan.Scope != "" {
		typeDisplay += fmt.Sprintf("(%s)", plan.Scope)
	}
	if plan.Breaking {
		typeDisplay += "!"
	}
	sec.Row("%-16s%s", "type", typeDisplay)

	if result.Backend != "" {
		sec.Row("%-16s%s", "backend", result.Backend)
	}

	if result.NoOp {
		sec.Row("%-16s%s", "status", "nothing to commit")
		sec.Close()
		return nil
	}

	// Message
	sec.Row("%-16s%s", "message", plan.Subject(cfg.Commit.Conventional))

	if commitDryRun {
		sec.Row("%-16s%s", "mode", "dry-run")
	}

	// Staged files
	sec.Row("%-16s%d files", "staged", len(result.Files))
	for _, f := range result.Files {
		output.RowStatus(sec, f, "", "success", useColor)
	}

	// SHA
	if result.SHA != "" {
		sec.Row("%-16s%s", "sha", result.SHA)
	}

	// Push status
	if plan.Push.Enabled {
		if result.Pushed {
			pushTarget := plan.Push.Remote
			output.RowStatus(sec, "pushed", pushTarget, "success", useColor)
		} else if commitDryRun {
			sec.Row("%-16s%s (dry-run)", "push", plan.Push.Remote)
		}
	}

	sec.Close()
	return nil
}

// shouldUseForge returns true when the forge backend should be used for push.
func shouldUseForge(plan *commit.Plan, configBackend string) bool {
	if !plan.Push.Enabled {
		return false
	}
	if configBackend == "forge" {
		return true
	}
	return output.IsCI()
}

// detectForgeForPush detects the forge platform and resolves the target branch
// for forge-based push.
func detectForgeForPush(rootDir string, plan *commit.Plan) (forge.Forge, string, error) {
	remoteURL, err := detectRemoteURL(rootDir)
	if err != nil {
		return nil, "", fmt.Errorf("no git remote URL found: %w", err)
	}

	provider := forge.DetectProvider(remoteURL)
	if provider == forge.Unknown {
		return nil, "", fmt.Errorf("unknown forge provider for remote %s", remoteURL)
	}

	fc, err := newForgeClient(provider, remoteURL)
	if err != nil {
		return nil, "", fmt.Errorf("%s forge client init failed: %w", provider, err)
	}

	branch := commit.BranchFromRefspec(plan.Push.Refspec)
	if branch == "" {
		for _, env := range []string{"CI_COMMIT_BRANCH", "CI_COMMIT_REF_NAME", "GITHUB_REF_NAME"} {
			if v := os.Getenv(env); v != "" {
				branch = v
				break
			}
		}
	}
	if branch == "" {
		return nil, "", fmt.Errorf("could not resolve target branch (checked refspec + CI_COMMIT_BRANCH/CI_COMMIT_REF_NAME/GITHUB_REF_NAME)")
	}

	return fc, branch, nil
}
