package cmd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/badge"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/postbuild"
)

var (
	bgLabel  string
	bgValue  string
	bgColor  string
	bgStatus string
	bgOutput string
)

var badgeGenerateCmd = &cobra.Command{
	Use:   "generate [name...]",
	Short: "Generate SVG badges from config or flags",
	Long: `Generate SVG badges defined in narrator config items.

Config-driven (no flags): generates all narrator badge items with output paths, or named items if specified.
Ad-hoc (--label + --value): generates a single badge from flags.`,
	RunE: runBadgeGenerate,
}

func init() {
	badgeGenerateCmd.Flags().StringVar(&bgLabel, "label", "", "ad-hoc badge label (left side)")
	badgeGenerateCmd.Flags().StringVar(&bgValue, "value", "", "ad-hoc badge value (right side)")
	badgeGenerateCmd.Flags().StringVar(&bgColor, "color", "#4c1", "ad-hoc badge color (hex)")
	badgeGenerateCmd.Flags().StringVar(&bgStatus, "status", "", "status-driven color: passed, warning, critical")
	badgeGenerateCmd.Flags().StringVar(&bgOutput, "output", ".stagefreight/badges/custom.svg", "output file path")

	badgeCmd.AddCommand(badgeGenerateCmd)
}

func runBadgeGenerate(cmd *cobra.Command, args []string) error {
	eng, err := buildDefaultBadgeEngine()
	if err != nil {
		return err
	}

	// Ad-hoc mode: --label and --value provided
	if bgLabel != "" && bgValue != "" {
		return generateAdHocBadge(eng)
	}

	// Config-driven mode
	return generateConfigBadges(eng, args)
}

// buildDefaultBadgeEngine creates a badge engine with the default font (dejavu-sans 11pt).
// Per-item font overrides are handled in buildItemEngine.
func buildDefaultBadgeEngine() (*badge.Engine, error) {
	return badge.NewDefault()
}

// badgeRow holds display data for a single badge in section output.
type badgeRow struct {
	Name  string
	Out   string
	Font  string
	Size  float64
	Color string
}

func generateAdHocBadge(eng *badge.Engine) error {
	start := time.Now()
	clr := bgColor
	if bgStatus != "" {
		clr = badge.StatusColor(bgStatus)
	}

	svg := eng.Generate(badge.Badge{
		Label: bgLabel,
		Value: bgValue,
		Color: clr,
	})

	if err := os.MkdirAll(filepath.Dir(bgOutput), 0o755); err != nil {
		return fmt.Errorf("creating badge directory: %w", err)
	}
	if err := os.WriteFile(bgOutput, []byte(svg), 0o644); err != nil {
		return fmt.Errorf("writing badge: %w", err)
	}

	elapsed := time.Since(start)
	useColor := output.UseColor()
	w := os.Stdout

	sec := output.NewSection(w, "Badges", elapsed, useColor)
	sec.Row("%-16s%-24s %-8s %dpt  %s", bgLabel, bgOutput, "dejavu-sans", 11, clr)
	sec.Separator()
	sec.Row("1 generated")
	sec.Close()
	return nil
}

// RunConfigBadges generates SVG badges from narrator config items.
// Extracted for reuse by both Cobra command and CI runners.
func RunConfigBadges(appCfg *config.Config, rootDir string, names []string, status string) error {
	eng, err := buildDefaultBadgeEngine()
	if err != nil {
		return err
	}
	return generateConfigBadgesImpl(eng, appCfg, rootDir, names, status)
}

func generateConfigBadges(eng *badge.Engine, names []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	return generateConfigBadgesImpl(eng, cfg, rootDir, names, bgStatus)
}

func generateConfigBadgesImpl(eng *badge.Engine, appCfg *config.Config, rootDir string, names []string, status string) error {
	start := time.Now()

	// Collect badge items from both sources:
	// 1. Top-level badges: config (new, preferred)
	// 2. Narrator badge items with output (legacy, backward compat)
	items := postbuild.CollectNarratorBadgeItems(appCfg)

	// Add badges from top-level config (sorted by ID for deterministic generation).
	if len(appCfg.Badges.Items) > 0 {
		sorted := make([]config.BadgeConfig, len(appCfg.Badges.Items))
		copy(sorted, appCfg.Badges.Items)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
		for _, b := range sorted {
			items = append(items, config.NarratorItem{
				ID:     b.ID,
				Kind:   "badge",
				Text:   b.Text,
				Value:  b.Value,
				Color:  b.Color,
				Output: b.Output,
				Link:   b.Link,
				Font:   b.Font,
			})
		}
	}

	if len(items) == 0 {
		return fmt.Errorf("no badge items configured (check badges: or narrator badge items)")
	}

	// Filter to named items if specified
	if len(names) > 0 {
		nameSet := make(map[string]bool, len(names))
		for _, n := range names {
			nameSet[n] = true
		}
		var filtered []config.NarratorItem
		for _, item := range items {
			// Match by badge text (label) or ID
			if nameSet[item.Text] || (item.ID != "" && nameSet[item.ID]) {
				filtered = append(filtered, item)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("no matching badge items for: %v", names)
		}
		items = filtered
	}

	// Detect version for template resolution
	versionInfo, err := build.DetectVersion(rootDir, appCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: version detection failed: %v\n", err)
	}

	// Inject project description from docker-readme targets
	if desc := postbuild.FirstDockerReadmeDescription(appCfg); desc != "" {
		gitver.SetProjectDescription(desc)
	}

	// Pass 1: resolve version templates for all badges, collect resolved values
	specs := make([]config.BadgeSpec, len(items))
	resolvedValues := make([]string, len(items))
	for i, item := range items {
		specs[i] = item.ToBadgeSpec()
		value := specs[i].Value
		if versionInfo != nil && value != "" {
			value = gitver.ResolveTemplateWithDirAndVars(value, versionInfo, rootDir, appCfg.Vars)
		}
		resolvedValues[i] = value
	}

	// Scan resolved values for {docker.tag.*} patterns to discover tag names
	tagNames := gitver.ExtractDockerTagNames(resolvedValues)

	// Lazy Docker Hub info — fetch repo-level + per-tag info if needed
	var dockerInfo *gitver.DockerHubInfo
	needsDocker := len(tagNames) > 0
	if !needsDocker {
		for _, v := range resolvedValues {
			if strings.Contains(v, "{docker.") {
				needsDocker = true
				break
			}
		}
	}
	if needsDocker {
		ns, repo := postbuild.DockerHubFromConfig(appCfg)
		if ns != "" && repo != "" {
			dockerInfo, _ = gitver.FetchDockerHubInfo(ns, repo)
			if dockerInfo != nil && len(tagNames) > 0 {
				client := &http.Client{Timeout: 10 * time.Second}
				dockerInfo.Tags = gitver.FetchTagInfo(client, ns, repo, tagNames)
			}
		}
	}

	// Pass 2: resolve docker templates and generate SVGs
	var rows []badgeRow
	generated := 0

	for i, item := range items {
		spec := specs[i]

		// Resolve per-item engine if font is overridden.
		itemEng := eng
		if spec.Font != "" || spec.FontFile != "" || spec.FontSize != 0 {
			override, err := buildItemEngine(spec)
			if err != nil {
				return fmt.Errorf("loading font for badge %s: %w", item.Text, err)
			}
			itemEng = override
		}

		value := gitver.ResolveDockerTemplates(resolvedValues[i], dockerInfo)

		// Guard against empty or unresolved template values producing broken badges.
		// Any remaining "{" means a template didn't resolve (missing tag, nil docker info, etc).
		if value == "" || strings.Contains(value, "{") {
			value = "n/a"
		}

		// Resolve color
		badgeColor := spec.Color
		if badgeColor == "" || badgeColor == "auto" {
			badgeColor = badge.StatusColor(status)
		}

		svg := itemEng.Generate(badge.Badge{
			Label: spec.Label,
			Value: value,
			Color: badgeColor,
		})

		if err := os.MkdirAll(filepath.Dir(spec.Output), 0o755); err != nil {
			return fmt.Errorf("creating badge directory for %s: %w", item.Text, err)
		}
		if err := os.WriteFile(spec.Output, []byte(svg), 0o644); err != nil {
			return fmt.Errorf("writing badge %s: %w", item.Text, err)
		}
		generated++

		// Collect row for section output
		fontName := spec.Font
		if fontName == "" {
			fontName = "dejavu-sans"
		}
		size := spec.FontSize
		if size == 0 {
			size = 11
		}
		rows = append(rows, badgeRow{
			Name:  item.Text,
			Out:   spec.Output,
			Font:  fontName,
			Size:  size,
			Color: badgeColor,
		})
	}

	// Sort rows for stable output
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Out < rows[j].Out
	})

	elapsed := time.Since(start)
	useColor := output.UseColor()
	w := os.Stdout

	sec := output.NewSection(w, "Badges", elapsed, useColor)
	for _, r := range rows {
		sec.Row("%-16s%-24s %-8s %.0fpt  %s", r.Name, r.Out, r.Font, r.Size, r.Color)
	}
	sec.Separator()
	sec.Row("%d generated", generated)
	sec.Close()

	return nil
}

// buildItemEngine creates a badge engine for a BadgeSpec with font overrides.
// Falls back to defaults (dejavu-sans 11pt) for any field not set.
func buildItemEngine(spec config.BadgeSpec) (*badge.Engine, error) {
	return badge.NewForSpec(spec.Font, spec.FontSize, spec.FontFile)
}
