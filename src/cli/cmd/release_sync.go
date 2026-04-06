package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/output"
)

var (
	relSyncDryRun bool
)

var releaseSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Project releases from primary forge to mirrors",
	Long: `Reads releases from the primary forge and projects missing ones
to mirrors that declare sync.releases: true.

Use --dry-run to preview what would be created without making changes.
Without --dry-run, missing releases are created on each mirror.`,
	RunE: runReleaseSync,
}

func init() {
	releaseSyncCmd.Flags().BoolVar(&relSyncDryRun, "dry-run", false, "Preview only, do not create releases")
	releaseCmd.AddCommand(releaseSyncCmd)
}

func runReleaseSync(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if cfg == nil {
		return fmt.Errorf("no config loaded")
	}

	rootDir, err := os.Getwd()
	if err != nil {
		return err
	}

	w := os.Stdout
	color := output.UseColor()

	// Detect primary forge.
	remoteURL, err := detectRemoteURL(rootDir)
	if err != nil {
		return fmt.Errorf("detecting remote: %w", err)
	}
	provider := forge.DetectProvider(remoteURL)
	primaryClient, err := newForgeClient(provider, remoteURL)
	if err != nil {
		return fmt.Errorf("creating primary forge client: %w", err)
	}

	// List primary releases.
	primaryReleases, err := primaryClient.ListReleases(ctx)
	if err != nil {
		return fmt.Errorf("listing primary releases: %w", err)
	}

	if len(primaryReleases) == 0 {
		fmt.Fprintln(w, "No releases found on primary forge.")
		return nil
	}

	start := time.Now()
	output.SectionStart(w, "sf_release_sync", "Release Sync")
	sec := output.NewSection(w, "Release Sync", 0, color)
	sec.Row("%-16s%s", "primary", string(provider))
	sec.Row("%-16s%d", "releases", len(primaryReleases))

	// Project to each mirror with sync.releases: true.
	var totalCreated, totalSkipped, totalFailed int

	resolvedMirrors, _ := config.ResolveAllMirrors(cfg.Repos, cfg.Forges, cfg.Vars)
	for _, m := range resolvedMirrors {
		if !m.Sync.Releases {
			continue
		}

		mirrorClient, err := forge.NewFromAccessory(m.Provider, m.BaseURL, m.Project, m.Credentials)
		if err != nil {
			sec.Row("%s mirror:%s — %v", output.StatusIcon("failed", color), m.ID, err)
			totalFailed++
			continue
		}

		mirrorReleases, err := mirrorClient.ListReleases(ctx)
		if err != nil {
			sec.Row("%s mirror:%s — failed to list releases: %v", output.StatusIcon("failed", color), m.ID, err)
			totalFailed++
			continue
		}

		// Build set of existing tags on mirror.
		mirrorTags := make(map[string]bool, len(mirrorReleases))
		for _, r := range mirrorReleases {
			mirrorTags[r.TagName] = true
		}

		// Find missing releases.
		var missing []forge.ReleaseInfo
		for _, r := range primaryReleases {
			if !mirrorTags[r.TagName] {
				missing = append(missing, r)
			}
		}

		if len(missing) == 0 {
			sec.Row("%s mirror:%s — in sync (%d releases)", output.StatusIcon("success", color), m.ID, len(mirrorReleases))
			totalSkipped += len(primaryReleases)
			continue
		}

		sec.Separator()
		sec.Row("%-16s%s (%d missing)", "mirror", m.ID, len(missing))

		for _, r := range missing {
			name := r.Name
			if name == "" {
				name = r.TagName
			}
			description := r.Description

			if relSyncDryRun {
				sec.Row("  %s %s → %s/%s (dry-run)", output.StatusIcon("success", color), r.TagName, m.Provider, m.Project)
				totalCreated++
				continue
			}

			_, createErr := mirrorClient.CreateRelease(ctx, forge.ReleaseOptions{
				TagName:     r.TagName,
				Name:        name,
				Description: description,
				Draft:       r.Draft,
				Prerelease:  r.Prerelease,
			})
			if createErr != nil {
				sec.Row("  %s %s — %v", output.StatusIcon("failed", color), r.TagName, createErr)
				totalFailed++
			} else {
				sec.Row("  %s %s → %s/%s", output.StatusIcon("success", color), r.TagName, m.Provider, m.Project)
				totalCreated++
			}
		}
	}

	elapsed := time.Since(start)
	sec.Separator()
	mode := "apply"
	if relSyncDryRun {
		mode = "dry-run"
	}
	sec.Row("%-16s%s", "mode", mode)
	sec.Row("%-16s%d", "created", totalCreated)
	sec.Row("%-16s%d", "skipped", totalSkipped)
	sec.Row("%-16s%d", "failed", totalFailed)
	sec.Row("%-16s%s", "elapsed", elapsed.Round(time.Millisecond))
	sec.Close()
	output.SectionEnd(w, "sf_release_sync")

	if totalFailed > 0 {
		return fmt.Errorf("release sync completed with %d failures", totalFailed)
	}
	return nil
}

