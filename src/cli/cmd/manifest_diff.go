package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var manifestDiffCmd = &cobra.Command{
	Use:   "diff <manifest-a> <manifest-b>",
	Short: "Compare two manifests (not yet implemented)",
	Long:  `Diff compares two manifest JSON files and shows what changed between them.`,
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("manifest diff is not yet implemented (planned for v1.1)")
	},
}

func init() {
	manifestCmd.AddCommand(manifestDiffCmd)
}
