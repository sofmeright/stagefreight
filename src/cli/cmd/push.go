package cmd

import (
	"fmt"
	"os"

	"github.com/PrPlanIT/StageFreight/src/commit"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/spf13/cobra"
)

var (
	pushRemote          string
	pushRefspec         string
	pushNoRebase        bool
)

func init() {
	pushCmd.Flags().StringVar(&pushRemote, "remote", "origin", "git remote to push to")
	pushCmd.Flags().StringVar(&pushRefspec, "refspec", "", "push refspec (e.g. HEAD:refs/heads/main)")
	pushCmd.Flags().BoolVar(&pushNoRebase, "no-rebase", false, "fail instead of rebasing on diverged branch")
	rootCmd.AddCommand(pushCmd)
}

var pushCmd = &cobra.Command{
	Use:   "push",
	Short: "Synchronize the current branch with its remote",
	Long: `Push the current branch to its remote using the convergence engine.

Handles diverged branches, missing upstream tracking, and up-to-date states.
Push behavior is shared with 'commit --push' — same engine, standalone entry point.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		rootDir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolving working directory: %w", err)
		}

		opts := commit.PushOptions{
			Enabled:         true,
			Remote:          pushRemote,
			Refspec:         pushRefspec,
			RebaseOnDiverge: !pushNoRebase,
		}

		backend := &commit.GitBackend{RootDir: rootDir}
		result, err := backend.Push(opts)
		if err != nil {
			return err
		}

		useColor := output.UseColor()
		sec := output.NewSection(os.Stdout, "Push", 0, useColor)

		if result.Noop {
			sec.Row("%-16s%s", "status", "already up to date")
		} else {
			output.RowStatus(sec, "pushed", opts.Remote, "success", useColor)
			for _, action := range result.ActionsExecuted {
				switch action {
				case commit.SyncRebase:
					sec.Row("%-16s%s", "sync", "rebased onto upstream before push")
				case commit.SyncFastForward:
					sec.Row("%-16s%s", "sync", "fast-forwarded to upstream")
				case commit.SyncSetUpstream:
					sec.Row("%-16s%s", "sync", "tracking branch configured")
				}
			}
		}

		sec.Close()
		return nil
	},
}

