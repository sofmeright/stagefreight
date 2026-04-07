package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/config/preset"
)

var resolveVerbose bool

var configResolveCmd = &cobra.Command{
	Use:   "resolve",
	Short: "Show the config resolution chain with provenance",
	Long: `Shows how the effective config was resolved:
- Preset sources and what they contributed
- Source provenance for each value`,
	RunE: func(cmd *cobra.Command, args []string) error {
		rootDir, err := os.Getwd()
		if err != nil {
			return err
		}

		configPath := filepath.Join(rootDir, ".stagefreight.yml")
		raw, err := loadConfig(rootDir)
		if err != nil {
			return err
		}

		// Resolve presets to build trace.
		absPath, _ := filepath.Abs(configPath)
		loader := preset.NewLocalLoader(filepath.Dir(absPath))
		_, entries, resolveErr := preset.ResolvePresets(raw, loader, "local", absPath, 0, nil)

		fmt.Printf("Config: .stagefreight.yml\n")
		fmt.Printf("  entries: %d\n", len(entries))
		if resolveErr != nil {
			fmt.Fprintf(os.Stderr, "  resolve error: %v\n", resolveErr)
		}

		if resolveVerbose {
			fmt.Println()
			trace := preset.MergeTrace{Entries: entries}
			fmt.Print(preset.ExplainTrace(trace))
		}

		return nil
	},
}

func init() {
	configResolveCmd.Flags().BoolVarP(&resolveVerbose, "verbose", "v", false, "Show full resolution trace")
	configCmd.AddCommand(configResolveCmd)
}
