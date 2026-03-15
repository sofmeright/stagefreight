package cmd

import (
	"github.com/spf13/cobra"
)

var manifestCmd = &cobra.Command{
	Use:   "manifest",
	Short: "Generate and inspect build manifests",
	Long: `Manifest generates a normalized view of build evidence from Dockerfile
analysis, SBOM data, and security scans into a single deterministic JSON document.

Subcommands:
  generate    Create manifest from build config and Dockerfile
  inspect     Pretty-print manifest or specific sections
  diff        Compare two manifests (not yet implemented)`,
}

func init() {
	rootCmd.AddCommand(manifestCmd)
}
