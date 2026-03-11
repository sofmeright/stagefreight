package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/sofmeright/stagefreight/src/build"
	"github.com/sofmeright/stagefreight/src/component"
	"github.com/sofmeright/stagefreight/src/config"
	"github.com/sofmeright/stagefreight/src/gitver"
	"github.com/sofmeright/stagefreight/src/narrator"
	"github.com/sofmeright/stagefreight/src/output"
	"github.com/sofmeright/stagefreight/src/props"
	"github.com/sofmeright/stagefreight/src/registry"
)

var (
	nrDryRun bool
)

var narratorRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run narrator items from config",
	Long: `Execute all narrator items defined in the narrator config.

Each item is composed from its kind and placed into the target file
according to its placement markers. Existing managed content between
markers is replaced idempotently.

Items sharing the same placement markers are composed together:
inline items are joined with spaces, block items with newlines.`,
	RunE: runNarratorRun,
}

func init() {
	narratorRunCmd.Flags().BoolVar(&nrDryRun, "dry-run", false, "preview changes without writing files")

	narratorCmd.AddCommand(narratorRunCmd)
}

// narratorFileResult tracks the outcome for a single narrator file.
type narratorFileResult struct {
	File   string
	Status string // "success" | "skipped"
	Detail string // "updated" | "would update" | "unchanged"
}

func runNarratorRun(cmd *cobra.Command, args []string) error {
	if len(cfg.Narrator) == 0 {
		return fmt.Errorf("no narrator files configured")
	}

	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Detect version for template resolution.
	var versionInfo *gitver.VersionInfo
	versionInfo, err = build.DetectVersion(rootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: version detection failed: %v\n", err)
	}

	start := time.Now()
	color := output.UseColor()
	w := os.Stdout

	var results []narratorFileResult
	for _, fileCfg := range cfg.Narrator {
		result, content, err := processNarratorFile(fileCfg, rootDir, versionInfo)
		if err != nil {
			return err
		}
		if nrDryRun && content != "" {
			fmt.Fprintln(w, content)
		}
		results = append(results, result)
	}

	elapsed := time.Since(start)
	sec := output.NewSection(w, "Narrator Run", elapsed, color)

	var changed, unchanged int
	for _, r := range results {
		output.RowStatus(sec, r.File, r.Detail, r.Status, color)
		switch r.Detail {
		case "updated", "would update":
			changed++
		default:
			unchanged++
		}
	}

	sec.Separator()
	if nrDryRun {
		sec.Row("%d would update, %d unchanged", changed, unchanged)
	} else {
		sec.Row("%d updated, %d unchanged", changed, unchanged)
	}
	sec.Close()

	return nil
}

// placementKey is the grouping key for items sharing the same placement.
// Items with identical placement markers, mode, and inline flag are composed together.
type placementKey struct {
	StartMarker string
	EndMarker   string
	Mode        string
	Inline      bool
}

// placementGroup holds items sharing the same placement.
type placementGroup struct {
	Key   placementKey
	Items []config.NarratorItem
}

func processNarratorFile(fileCfg config.NarratorFile, rootDir string, vi *gitver.VersionInfo) (narratorFileResult, string, error) {
	path := fileCfg.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootDir, path)
	}

	// Resolve URL bases from per-file config.
	linkBase := strings.TrimRight(gitver.ResolveVars(fileCfg.LinkBase, cfg.Vars), "/")
	rawBase := ""
	if linkBase != "" {
		rawBase = registry.DeriveRawBase(linkBase)
	}
	rawBase = strings.TrimRight(rawBase, "/")

	// Read existing file (or start empty).
	content := ""
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return narratorFileResult{}, "", fmt.Errorf("narrator: reading %s: %w", fileCfg.File, err)
		}
		// File doesn't exist yet — start fresh.
	} else {
		content = string(raw)
	}

	original := content

	// Group items by placement (items sharing the same markers are composed together).
	groups := groupItemsByPlacement(fileCfg.Items)

	for _, group := range groups {
		// Build modules from items in this group.
		modules := buildModulesV2(group.Items, linkBase, rawBase, vi, rootDir)
		if len(modules) == 0 {
			continue
		}

		// Compose modules: inline items joined with space, block items with newline.
		var composed string
		if group.Key.Inline {
			composed = narrator.ComposeInline(modules)
		} else {
			composed = narrator.Compose(modules)
		}
		if composed == "" {
			continue
		}

		// Replace content between the placement markers.
		if group.Key.StartMarker != "" && group.Key.EndMarker != "" {
			updated, found := registry.ReplaceBetween(content, group.Key.StartMarker, group.Key.EndMarker, composed)
			if found {
				content = updated
			} else if verbose {
				fmt.Fprintf(os.Stderr, "  warning: markers not found in %s: %s ... %s\n",
					fileCfg.File, group.Key.StartMarker, group.Key.EndMarker)
			}
		}
	}

	if nrDryRun {
		if content != original {
			return narratorFileResult{File: fileCfg.File, Status: "success", Detail: "would update"}, content, nil
		}
		return narratorFileResult{File: fileCfg.File, Status: "skipped", Detail: "unchanged"}, "", nil
	}

	if content == original {
		return narratorFileResult{File: fileCfg.File, Status: "skipped", Detail: "unchanged"}, "", nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return narratorFileResult{}, "", fmt.Errorf("narrator: creating directory for %s: %w", fileCfg.File, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return narratorFileResult{}, "", fmt.Errorf("narrator: writing %s: %w", fileCfg.File, err)
	}
	return narratorFileResult{File: fileCfg.File, Status: "success", Detail: "updated"}, "", nil
}

// groupItemsByPlacement groups items by their placement key, preserving declaration order.
// Items with the same (between markers, mode, inline) are collected into one group.
func groupItemsByPlacement(items []config.NarratorItem) []placementGroup {
	var groups []placementGroup
	keyIndex := make(map[placementKey]int)

	for _, item := range items {
		key := placementKey{
			StartMarker: item.Placement.Between[0],
			EndMarker:   item.Placement.Between[1],
			Mode:        item.Placement.Mode,
			Inline:      item.Placement.Inline,
		}

		if idx, ok := keyIndex[key]; ok {
			groups[idx].Items = append(groups[idx].Items, item)
		} else {
			keyIndex[key] = len(groups)
			groups = append(groups, placementGroup{
				Key:   key,
				Items: []config.NarratorItem{item},
			})
		}
	}

	return groups
}

// buildModulesV2 converts v2 NarratorItem entries into narrator.Module instances.
// Dispatches on item.Kind instead of checking which field is set.
func buildModulesV2(items []config.NarratorItem, linkBase, rawBase string, vi *gitver.VersionInfo, rootDir string) []narrator.Module {
	var modules []narrator.Module

	for _, item := range items {
		switch item.Kind {
		case "break":
			modules = append(modules, narrator.BreakModule{})

		case "badge":
			link := gitver.ResolveVars(item.Link, cfg.Vars)
			if vi != nil {
				link = gitver.ResolveTemplateWithDirAndVars(link, vi, rootDir, cfg.Vars)
			}
			resolved := item
			resolved.Link = link
			mod := resolveBadgeItemV2(resolved, linkBase, rawBase)
			if mod != nil {
				modules = append(modules, mod)
			}

		case "shield":
			shieldPath := gitver.ResolveVars(item.Shield, cfg.Vars)
			link := gitver.ResolveVars(item.Link, cfg.Vars)
			label := item.Text
			if label == "" {
				label = shieldPath
			}
			if vi != nil {
				label = gitver.ResolveTemplateWithDirAndVars(label, vi, "", cfg.Vars)
			}
			modules = append(modules, narrator.ShieldModule{
				Path:  shieldPath,
				Label: label,
				Link:  resolveLink(link, linkBase),
			})

		case "text":
			text := item.Content
			if vi != nil {
				text = gitver.ResolveTemplateWithDirAndVars(text, vi, "", cfg.Vars)
			}
			modules = append(modules, narrator.TextModule{Text: text})

		case "component":
			spec, err := component.ParseSpec(item.Spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "narrator: component %s: %v\n", item.Spec, err)
				continue
			}
			docs := component.GenerateDocs([]*component.SpecFile{spec})
			modules = append(modules, narrator.ComponentModule{Docs: strings.TrimSpace(docs)})

		case "include":
			incPath := item.Path
			if !filepath.IsAbs(incPath) {
				incPath = filepath.Join(rootDir, incPath)
			}
			data, err := os.ReadFile(incPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "narrator: include %s: %v\n", item.Path, err)
				continue
			}
			modules = append(modules, narrator.IncludeModule{Content: strings.TrimSpace(string(data))})

		case "props":
			def, ok := props.Get(item.Type)
			if !ok {
				fmt.Fprintf(os.Stderr, "narrator: props type %q not found\n", item.Type)
				continue
			}
			// Resolve {var:...} templates in params values.
			resolvedParams := make(map[string]string, len(item.Params))
			for k, v := range item.Params {
				resolvedParams[k] = gitver.ResolveVars(v, cfg.Vars)
			}
			opts := props.RenderOptions{
				Label:   item.Label,
				Link:    gitver.ResolveVars(item.Link, cfg.Vars),
				Style:   item.Style,
				Logo:    item.Logo,
				Variant: props.VariantClassic,
			}
			resolved, err := props.ResolveDefinition(def, resolvedParams, opts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "narrator: props %s: %v\n", item.Type, err)
				continue
			}
			modules = append(modules, narrator.PropsModule{
				Resolved: resolved,
				Variant:  props.VariantClassic,
			})
		}
	}

	return modules
}

// resolveBadgeItemV2 resolves a v2 badge NarratorItem to a BadgeModule for markdown composition.
// Uses the badge's Output path (SVG file) with rawBase to construct the image URL.
func resolveBadgeItemV2(item config.NarratorItem, linkBase, rawBase string) narrator.Module {
	var imgURL string
	if item.Output != "" && rawBase != "" {
		imgURL = rawBase + "/" + strings.TrimPrefix(item.Output, "./")
	}

	if imgURL == "" {
		return nil
	}

	return narrator.BadgeModule{
		Alt:    item.Text,
		ImgURL: imgURL,
		Link:   resolveLink(item.Link, linkBase),
	}
}

// resolveLink resolves a relative link against a base URL.
func resolveLink(link, linkBase string) string {
	if link == "" {
		return ""
	}
	if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "/") {
		return link
	}
	if linkBase != "" {
		return linkBase + "/" + strings.TrimPrefix(link, "./")
	}
	return link
}
