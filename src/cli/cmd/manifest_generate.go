package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/manifest"
	"github.com/PrPlanIT/StageFreight/src/output"
)

var (
	mgBuildID  string
	mgPlatform string
	mgDryRun   bool
	mgOutput   string
)

var manifestGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate manifest from build config and Dockerfile",
	Long: `Generate creates a normalized manifest JSON for each build defined
in .stagefreight.yml. The manifest captures inventory (packages, binaries,
base image versions) extracted from Dockerfile analysis.

Output location is controlled by manifest.mode in config:
  ephemeral   temp location, discarded after use (default)
  workspace   .stagefreight/manifests/, not auto-committed
  commit      included in docs commit
  publish     exported as release asset`,
	RunE: runManifestGenerate,
}

func init() {
	manifestGenerateCmd.Flags().StringVar(&mgBuildID, "build-id", "", "generate for a specific build ID only")
	manifestGenerateCmd.Flags().StringVar(&mgPlatform, "platform", "", "filter to a specific platform (os/arch)")
	manifestGenerateCmd.Flags().BoolVar(&mgDryRun, "dry-run", false, "preview manifest without writing files")
	manifestGenerateCmd.Flags().StringVar(&mgOutput, "output", "", "output format: json (default: summary)")

	manifestCmd.AddCommand(manifestGenerateCmd)
}

func runManifestGenerate(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	start := time.Now()

	opts := manifest.GenerateOptions{
		RootDir:  rootDir,
		BuildID:  mgBuildID,
		Platform: mgPlatform,
		DryRun:   mgDryRun,
	}

	manifests, err := manifest.Generate(cfg, opts)
	if err != nil {
		return err
	}

	if mgOutput == "json" || mgDryRun {
		for _, m := range manifests {
			data, merr := manifest.MarshalDeterministic(m)
			if merr != nil {
				return merr
			}
			fmt.Print(string(data))
		}
		return nil
	}

	// Summary output
	elapsed := time.Since(start)
	color := output.UseColor()
	w := os.Stdout

	sec := output.NewSection(w, "Manifest Generate", elapsed, color)
	for _, m := range manifests {
		total := countInventory(m)
		detail := fmt.Sprintf("%d items", total)
		output.RowStatus(sec, m.Scope.Name, detail, "success", color)
	}
	sec.Separator()
	sec.Row("%d manifest(s) generated", len(manifests))
	sec.Close()

	return nil
}

func countInventory(m *manifest.Manifest) int {
	return len(m.Inventories.Versions) +
		len(m.Inventories.Apk) +
		len(m.Inventories.Apt) +
		len(m.Inventories.Pip) +
		len(m.Inventories.Galaxy) +
		len(m.Inventories.Npm) +
		len(m.Inventories.Go) +
		len(m.Inventories.Binaries)
}

