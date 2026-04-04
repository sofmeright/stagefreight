package governance

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// PlanViewConfig controls what the plan view renders.
type PlanViewConfig struct {
	Mode     string // "dry-run" or "apply"
	Source   string // e.g. "PrPlanIT/MaintenancePolicy"
	Ref      string // e.g. "dba5d2a" or "v1.0.0"
	Verbose  bool   // expand preset-cache files individually
}

// PlanViewData holds everything needed to render a plan view.
type PlanViewData struct {
	Config   PlanViewConfig
	Clusters []Cluster
	Plans    map[string]DistributionPlan // keyed by repo
	Results  map[string]CommitResult     // keyed by repo (nil for dry-run)
}

// actionMarker returns the symbol for a file action.
func actionMarker(action string) string {
	switch action {
	case "create":
		return "+"
	case "replace":
		return "~"
	case "unchanged":
		return "="
	case "delete":
		return "-"
	default:
		return "?"
	}
}

// repoFileSignature produces a comparable string representing the action set of a repo's plan.
func repoFileSignature(plan DistributionPlan, verbose bool) string {
	var parts []string
	for _, f := range classifyFiles(plan.Files, verbose) {
		parts = append(parts, fmt.Sprintf("%s:%s", f.display, f.action))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// classifiedFile is a display-ready file entry (may be collapsed).
type classifiedFile struct {
	display string // path or collapsed label
	action  string
	marker  string
}

// classifyFiles groups preset-cache files into a collapsed entry unless verbose.
func classifyFiles(files []DistributedFile, verbose bool) []classifiedFile {
	var result []classifiedFile
	var cacheFiles []DistributedFile

	for _, f := range files {
		if strings.HasPrefix(f.Path, ".stagefreight/preset-cache/") {
			cacheFiles = append(cacheFiles, f)
			continue
		}
		result = append(result, classifiedFile{
			display: f.Path,
			action:  f.Action,
			marker:  actionMarker(f.Action),
		})
	}

	if len(cacheFiles) > 0 {
		if verbose {
			for _, f := range cacheFiles {
				result = append(result, classifiedFile{
					display: f.Path,
					action:  f.Action,
					marker:  actionMarker(f.Action),
				})
			}
		} else {
			// Collapse — use the dominant action.
			action := cacheFiles[0].Action
			for _, f := range cacheFiles[1:] {
				if f.Action != action {
					action = "mixed"
					break
				}
			}
			result = append(result, classifiedFile{
				display: fmt.Sprintf(".stagefreight/preset-cache/ (%d files)", len(cacheFiles)),
				action:  action,
				marker:  actionMarker(action),
			})
		}
	}

	return result
}

// repoGroupKey determines the state label for grouping repos in plan view.
func repoPlanState(plan DistributionPlan) string {
	hasCreate, hasReplace := false, false
	for _, f := range plan.Files {
		switch f.Action {
		case "create":
			hasCreate = true
		case "replace":
			hasReplace = true
		}
	}
	switch {
	case hasReplace:
		return "drifted"
	case hasCreate:
		return "missing"
	default:
		return "unchanged"
	}
}

// repoApplyState determines the result label for grouping repos in apply view.
func repoApplyState(result CommitResult) string {
	switch result.Status {
	case "committed":
		return "committed"
	case "skipped-identical", "unchanged":
		return "unchanged"
	case "error":
		return "failed"
	default:
		return result.Status
	}
}

// repoGroup is a set of repos with the same state/result and file signature.
type repoGroup struct {
	state string
	repos []repoEntry
	files []classifiedFile
}

type repoEntry struct {
	name    string
	sha     string // apply only
	errMsg  string // apply only, failed repos
}

// RenderPlanView writes the structured plan view to w.
func RenderPlanView(w io.Writer, data PlanViewData) {
	shortRef := data.Config.Ref
	if len(shortRef) > 8 {
		shortRef = shortRef[:8]
	}

	// Header.
	modeLabel := data.Config.Mode
	fmt.Fprintf(w, "\n    \033[2;36m── Governance Reconcile ──────────────────────── %s ──\033[0m\n", modeLabel)
	row(w, "%-13s%s", "mode", modeLabel)
	row(w, "%-13s%s", "source", data.Config.Source)
	row(w, "%-13s%s", "ref", shortRef)

	totalRepos := 0
	for _, c := range data.Clusters {
		totalRepos += len(c.Targets.AllRepos())
	}
	row(w, "%-13s%d", "clusters", len(data.Clusters))
	row(w, "%-13s%d", "repos", totalRepos)

	separator(w)

	// Per-cluster sections.
	var totalCounts fileCounts
	var totalRepoCounts repoCounts

	for _, cluster := range data.Clusters {
		allRepos := cluster.Targets.AllRepos()
		clusterFileCount := 0
		for _, resolved := range allRepos {
			if p, ok := data.Plans[resolved.ID]; ok {
				for _, f := range p.Files {
					if f.Action != "unchanged" {
						clusterFileCount++
					}
				}
			}
		}

		repoWord := "repos"
		if len(allRepos) == 1 {
			repoWord = "repo"
		}
		fmt.Fprintf(w, "    │\n")
		row(w, "%s%s%d %s · %d files",
			cluster.ID,
			strings.Repeat(" ", max(1, 40-len(cluster.ID))),
			len(allRepos), repoWord, clusterFileCount)

		// Group repos by state + file signature.
		groups := groupRepos(cluster, data)

		for _, g := range groups {
			fmt.Fprintf(w, "    │\n")

			repoWord := "repos"
			if len(g.repos) == 1 {
				repoWord = "repo"
			}
			row(w, "  %s · %d %s", g.state, len(g.repos), repoWord)

			for _, r := range g.repos {
				if r.sha != "" {
					row(w, "    - %-40s [%s]", r.name, r.sha[:min(8, len(r.sha))])
				} else {
					row(w, "    - %s", r.name)
				}
				if r.errMsg != "" {
					row(w, "      %s", r.errMsg)
				}
			}

			// File actions (only if not pure unchanged).
			if g.state != "unchanged" {
				for _, f := range g.files {
					row(w, "    %s %-42s %s", f.marker, f.display, f.action)
				}
			}

			// Accumulate counts from actual plan files (not collapsed display).
			for _, r := range g.repos {
				if p, ok := data.Plans[r.name]; ok {
					for _, f := range p.Files {
						totalCounts.add(f.Action, 1)
					}
				}
				totalRepoCounts.add(g.state, r)
			}
		}

		fmt.Fprintf(w, "    │\n")
	}

	// Footer.
	separator(w)
	totalRepoCounts.render(w, data.Config.Mode)
	totalCounts.render(w)
	fmt.Fprintf(w, "    └─────────────────────────────────────────────────────────────\n")
}

// groupRepos groups repos in a cluster by state + file signature.
func groupRepos(cluster Cluster, data PlanViewData) []repoGroup {
	type groupKey struct {
		state string
		sig   string
	}

	keyOrder := []groupKey{}
	groups := map[groupKey]*repoGroup{}

	for _, resolved := range cluster.Targets.AllRepos() {
		repo := resolved.ID
		plan, ok := data.Plans[repo]
		if !ok {
			continue
		}

		var state string
		var entry repoEntry
		entry.name = repo

		if data.Results != nil {
			result, ok := data.Results[repo]
			if ok {
				state = repoApplyState(result)
				entry.sha = result.SHA
				if result.Error != nil {
					entry.errMsg = result.Error.Error()
				}
			} else {
				state = "unchanged"
			}
		} else {
			state = repoPlanState(plan)
		}

		sig := repoFileSignature(plan, data.Config.Verbose)
		key := groupKey{state: state, sig: sig}

		if _, exists := groups[key]; !exists {
			files := classifyFiles(plan.Files, data.Config.Verbose)
			groups[key] = &repoGroup{
				state: state,
				files: files,
			}
			keyOrder = append(keyOrder, key)
		}

		groups[key].repos = append(groups[key].repos, entry)
	}

	var result []repoGroup
	for _, k := range keyOrder {
		result = append(result, *groups[k])
	}
	return result
}

func row(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "    │ "+format+"\n", args...)
}

func separator(w io.Writer) {
	fmt.Fprintf(w, "    ├─────────────────────────────────────────────────────────────\n")
}

// fileCounts tracks file action totals.
type fileCounts struct {
	create, replace, unchanged, delete int
}

func (c *fileCounts) add(action string, repoCount int) {
	switch action {
	case "create":
		c.create += repoCount
	case "replace":
		c.replace += repoCount
	case "unchanged":
		c.unchanged += repoCount
	case "delete":
		c.delete += repoCount
	}
}

func (c *fileCounts) render(w io.Writer) {
	row(w, "%-20s%d", "files create", c.create)
	row(w, "%-20s%d", "files replace", c.replace)
	row(w, "%-20s%d", "files unchanged", c.unchanged)
	if c.delete > 0 {
		row(w, "%-20s%d", "files delete", c.delete)
	}
}

// repoCounts tracks repo state totals.
type repoCounts struct {
	changed, unchanged, committed, failed int
}

func (c *repoCounts) add(state string, _ repoEntry) {
	switch state {
	case "missing", "drifted":
		c.changed++
	case "unchanged":
		c.unchanged++
	case "committed":
		c.committed++
	case "failed":
		c.failed++
	}
}

func (c *repoCounts) render(w io.Writer, mode string) {
	if mode == "apply" {
		row(w, "%-20s%d", "repos committed", c.committed)
		row(w, "%-20s%d", "repos unchanged", c.unchanged)
		if c.failed > 0 {
			row(w, "%-20s%d", "repos failed", c.failed)
		}
	} else {
		row(w, "%-20s%d", "repos changed", c.changed)
		row(w, "%-20s%d", "repos unchanged", c.unchanged)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
