package cmd

import (
	"github.com/spf13/cobra"
)

var governanceCmd = &cobra.Command{
	Use:   "governance",
	Short: "Governance reconciliation and fleet management",
	Long:  "Commands for reconciling governance policy across governed repositories.",
}

func init() {
	rootCmd.AddCommand(governanceCmd)
}
