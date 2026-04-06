// Package postbuild contains post-build hook adapters that coordinate between
// the pipeline framework and external system packages (registry, badge, etc.).
// These are integration glue — they belong to neither the pure system packages
// nor the generic pipeline framework.
package postbuild

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/badge"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/output"
)

// BadgeRunner generates badges and returns a summary + elapsed time.
type BadgeRunner func(w io.Writer, color bool, rootDir string) (string, time.Duration)

// BadgeHook generates configured badges.
// Condition: returns true only if narrator config has badge items.
// runner is required and non-nil by contract — every caller has a legitimate badge runner.
func BadgeHook(appCfg *config.Config, runner BadgeRunner) pipeline.PostBuildHook {
	return pipeline.PostBuildHook{
		Name: "badges",
		Condition: func(pc *pipeline.PipelineContext) bool {
			for _, f := range appCfg.Narrator {
				for _, item := range f.Items {
					if item.HasGeneration() {
						return true
					}
				}
			}
			return false
		},
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			summary, _ := runner(pc.Writer, pc.Color, pc.RootDir)
			return &pipeline.PhaseResult{
				Name:    "badges",
				Status:  "success",
				Summary: summary,
			}, nil
		},
	}
}

// RunBadgeSection generates configured badges with section-formatted output.
func RunBadgeSection(w io.Writer, color bool, rootDir string, appCfg *config.Config) (string, time.Duration) {
	output.SectionStartCollapsed(w, "sf_badges", "Badges")
	start := time.Now()

	eng, err := badge.NewDefault()
	if err != nil {
		elapsed := time.Since(start)
		sec := output.NewSection(w, "Badges", elapsed, color)
		sec.Row("error: %v", err)
		sec.Close()
		output.SectionEnd(w, "sf_badges")
		return fmt.Sprintf("error: %v", err), elapsed
	}

	items := CollectNarratorBadgeItems(appCfg)

	// Detect version for template resolution
	vi, _ := build.DetectVersion(rootDir, appCfg)

	// Pass 1: resolve version templates for all badges, collect resolved values
	specs := make([]config.BadgeSpec, len(items))
	resolvedValues := make([]string, len(items))
	for i, item := range items {
		specs[i] = item.ToBadgeSpec()
		value := specs[i].Value
		if vi != nil && value != "" {
			value = gitver.ResolveTemplateWithDirAndVars(value, vi, rootDir, appCfg.Vars)
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
		ns, repo := DockerHubFromConfig(appCfg)
		if ns != "" && repo != "" {
			dockerInfo, _ = gitver.FetchDockerHubInfo(ns, repo)
			if dockerInfo != nil && len(tagNames) > 0 {
				client := &http.Client{Timeout: 10 * time.Second}
				dockerInfo.Tags = gitver.FetchTagInfo(client, ns, repo, tagNames)
			}
		}
	}

	// Pass 2: resolve docker templates and generate SVGs
	var generated int
	for i := range items {
		spec := specs[i]

		// Per-item engine if font is overridden
		itemEng := eng
		if spec.Font != "" || spec.FontFile != "" || spec.FontSize != 0 {
			override, oErr := badge.NewForSpec(spec.Font, spec.FontSize, spec.FontFile)
			if oErr != nil {
				continue
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
			badgeColor = badge.StatusColor("passed")
		}

		svg := itemEng.Generate(badge.Badge{
			Label: spec.Label,
			Value: value,
			Color: badgeColor,
		})

		if mkErr := os.MkdirAll(filepath.Dir(spec.Output), 0o755); mkErr != nil {
			continue
		}
		if wErr := os.WriteFile(spec.Output, []byte(svg), 0o644); wErr != nil {
			continue
		}
		generated++
	}

	elapsed := time.Since(start)
	sec := output.NewSection(w, "Badges", elapsed, color)
	for _, item := range items {
		spec := item.ToBadgeSpec()
		fontName := spec.Font
		if fontName == "" {
			fontName = "dejavu-sans"
		}
		size := spec.FontSize
		if size == 0 {
			size = 11
		}
		badgeColor := spec.Color
		if badgeColor == "" {
			badgeColor = "auto"
		}
		sec.Row("%-16s%-24s %-8s %.0fpt  %s", item.Text, spec.Output, fontName, size, badgeColor)
	}
	sec.Close()
	output.SectionEnd(w, "sf_badges")

	summary := fmt.Sprintf("%d generated", generated)
	return summary, elapsed
}

// CollectNarratorBadgeItems returns all narrator items with badge generation configured.
func CollectNarratorBadgeItems(appCfg *config.Config) []config.NarratorItem {
	var items []config.NarratorItem
	for _, f := range appCfg.Narrator {
		for _, item := range f.Items {
			if item.HasGeneration() {
				items = append(items, item)
			}
		}
	}
	return items
}

// DockerHubFromConfig returns the namespace and repo for the first docker.io registry target.
func DockerHubFromConfig(appCfg *config.Config) (string, string) {
	for _, t := range appCfg.Targets {
		if t.Kind == "registry" && t.URL == "docker.io" && t.Path != "" {
			resolved := gitver.ResolveVars(t.Path, appCfg.Vars)
			parts := strings.SplitN(resolved, "/", 2)
			if len(parts) == 2 {
				return parts[0], parts[1]
			}
		}
	}
	return "", ""
}
