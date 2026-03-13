package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/registry"
)

var drDryRun bool

var dockerReadmeCmd = &cobra.Command{
	Use:   "readme",
	Short: "Sync README to container registries",
	Long: `Push README content to container registries that support description APIs.

Docker Hub receives both short (100-char) and full markdown descriptions.
Quay and Harbor receive short descriptions only.
Other registries are silently skipped.`,
	RunE: runDockerReadme,
}

func init() {
	dockerReadmeCmd.Flags().BoolVar(&drDryRun, "dry-run", false, "show prepared content without pushing")
	dockerCmd.AddCommand(dockerReadmeCmd)
}

// readmeSyncResult tracks the outcome of syncing to a single registry.
type readmeSyncResult struct {
	Registry string
	Status   string // "success" | "skipped" | "failed"
	Detail   string
	Err      error
}

// RunDockerReadme syncs README content to container registries.
// Extracted for reuse by both Cobra command and CI runners.
func RunDockerReadme(ctx context.Context, appCfg *config.Config, rootDir string, dryRun bool) error {
	// Collect docker-readme targets
	targets := collectTargetsByKind(appCfg, "docker-readme")
	if len(targets) == 0 {
		return fmt.Errorf("no docker-readme targets configured")
	}

	return runDockerReadmeImpl(ctx, appCfg, rootDir, dryRun, targets)
}

func runDockerReadme(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	if len(args) > 0 {
		rootDir = args[0]
	}
	ctx := context.Background()
	return RunDockerReadme(ctx, cfg, rootDir, drDryRun)
}

func runDockerReadmeImpl(ctx context.Context, appCfg *config.Config, rootDir string, dryRun bool, targets []config.TargetConfig) error {
	color := output.UseColor()
	w := os.Stdout

	// For dry-run, show content from the first target's file
	if dryRun {
		t := targets[0]
		// Resolve {var:...} templates in target fields
		resolvedDesc := gitver.ResolveVars(t.Description, appCfg.Vars)
		resolvedLinkBase := gitver.ResolveVars(t.LinkBase, appCfg.Vars)

		file := t.File
		if file == "" {
			file = "README.md"
		}
		content, err := registry.PrepareReadmeFromFile(file, resolvedDesc, resolvedLinkBase, rootDir)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Short description (%d chars):\n  %s\n\n", len(content.Short), content.Short)
		fmt.Fprintf(w, "Full description (%d bytes):\n%s\n", len(content.Full), content.Full)
		return nil
	}

	start := time.Now()
	var results []readmeSyncResult

	for _, t := range targets {
		// Resolve {var:...} templates in target fields
		resolvedPath := gitver.ResolveVars(t.Path, appCfg.Vars)
		resolvedDesc := gitver.ResolveVars(t.Description, appCfg.Vars)
		resolvedLinkBase := gitver.ResolveVars(t.LinkBase, appCfg.Vars)

		file := t.File
		if file == "" {
			file = "README.md"
		}

		content, err := registry.PrepareReadmeFromFile(file, resolvedDesc, resolvedLinkBase, rootDir)
		if err != nil {
			results = append(results, readmeSyncResult{
				Registry: t.URL + "/" + resolvedPath,
				Status:   "failed",
				Detail:   err.Error(),
				Err:      err,
			})
			continue
		}

		// Resolve provider from explicit config or auto-detect from URL
		provider := t.Provider
		if provider == "" {
			provider = build.DetectProvider(t.URL)
		}

		// Only docker, github, quay, harbor support description APIs
		switch provider {
		case "docker", "dockerhub", "github", "quay", "harbor":
			// supported
		default:
			results = append(results, readmeSyncResult{
				Registry: t.URL + "/" + resolvedPath,
				Status:   "skipped",
				Detail:   "no description API",
			})
			continue
		}

		client, err := registry.NewRegistry(provider, t.URL, t.Credentials)
		if err != nil {
			results = append(results, readmeSyncResult{
				Registry: t.URL + "/" + resolvedPath,
				Status:   "failed",
				Detail:   err.Error(),
				Err:      err,
			})
			continue
		}

		// Per-target description override
		short := content.Short
		if resolvedDesc != "" {
			short = resolvedDesc
		}

		err = client.UpdateDescription(ctx, resolvedPath, short, content.Full)

		// Surface credential warnings (populated during auth)
		if warner, ok := client.(registry.Warner); ok {
			for _, warn := range warner.Warnings() {
				fmt.Fprintf(os.Stderr, "warning: %s/%s: %s\n", t.URL, resolvedPath, warn)
			}
		}

		if err != nil {
			if registry.IsForbidden(err) {
				results = append(results, readmeSyncResult{
					Registry: t.URL + "/" + resolvedPath,
					Status:   "skipped",
					Detail:   "forbidden (ensure PAT has read/write/delete scope)",
				})
				continue
			}
			results = append(results, readmeSyncResult{
				Registry: t.URL + "/" + resolvedPath,
				Status:   "failed",
				Detail:   err.Error(),
				Err:      err,
			})
			continue
		}

		results = append(results, readmeSyncResult{
			Registry: t.URL + "/" + resolvedPath,
			Status:   "success",
		})
	}

	elapsed := time.Since(start)

	// Tally
	var synced, skipped, errCount int
	for _, r := range results {
		switch r.Status {
		case "success":
			synced++
		case "skipped":
			skipped++
		case "failed":
			errCount++
		}
	}

	// ── README Sync section ──
	output.SectionStart(w, "sf_readme", "README Sync")
	sec := output.NewSection(w, "README Sync", elapsed, color)

	for _, r := range results {
		output.RowStatus(sec, r.Registry, r.Detail, r.Status, color)
	}

	sec.Separator()
	sec.Row("%d synced, %d skipped, %d errors", synced, skipped, errCount)

	sec.Close()
	output.SectionEnd(w, "sf_readme")

	if errCount > 0 {
		return fmt.Errorf("readme sync had %d error(s)", errCount)
	}
	return nil
}
