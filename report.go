package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/config/preset"
)

// SectionState is the resolved state of one config domain section.
//
// Provenance only applies when Active == true.
// When Active == false, Provenance MUST be "none" — inactive sections have
// no provenance because they do not exist in the runtime model.
//
// StageFreight never performs work the operator did not explicitly request
// via manifest or preset. Absence is not a behavior.
//
// Rule: inactive ≠ unimportant. Capability sections that are inactive when
// unconfigured are full StageFreight domains. Their absence is informational,
// not a downgrade.
type SectionState struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`              // "execution" | "capability" | "structural"
	Active           bool   `json:"active"`            // true = present in the runtime model
	SourcePresent    bool   `json:"source_present"`    // true = declared in .stagefreight.yml
	Provenance       string `json:"provenance"`        // "manifest" | "preset" | "none"
	ResolutionStatus string `json:"resolution_status"` // "resolved" | "partial" | "none"
}

// validate enforces the SectionState contract:
//
//	Active == false → Provenance MUST be "none"
//	Active == true  → Provenance MUST NOT be "none"
//
// Returns an error string if the contract is violated. Callers that produce
// SectionState values (buildSectionsFromMap, buildSectionsFromEntries) must
// call this and panic on violation — a bad SectionState is a programmer error,
// not a runtime condition.
func (s SectionState) validate() string {
	if !s.Active && s.Provenance != "none" {
		return s.Name + ": inactive section has non-none provenance (" + s.Provenance + ")"
	}
	if s.Active && s.Provenance == "none" {
		return s.Name + ": active section has provenance=none"
	}
	return ""
}

// ConfigReport is the result of loading and resolving configuration.
// Surfaces the "Explain" layer of the Load → Resolve → Explain → Validate → Execute pipeline.
//
// Built from real preset resolver output — not YAML key scraping.
// Status is "ok" when the resolver ran cleanly; "partial" when resolution
// was incomplete (e.g., a preset file could not be loaded).
type ConfigReport struct {
	SourceFile   string         `json:"source_file"`
	Presets      []string       `json:"presets,omitempty"`   // preset paths applied, in resolution order
	Overrides    int            `json:"overrides,omitempty"` // keys overridden by explicit config over preset
	Sections     []SectionState `json:"sections,omitempty"`  // per-section active/inactive/provenance state
	VarsApplied  int            `json:"vars_applied,omitempty"`
	Warnings     []string       `json:"warnings,omitempty"`
	Status       string         `json:"status"`       // "ok" | "partial" | "error"
	Completeness string         `json:"completeness"` // "complete" | "partial"
	Error        string         `json:"error,omitempty"`
}

// sectionDef records name and kind for a known config section.
type sectionDef struct {
	name string
	kind string
}

// allKnownSections defines every config section StageFreight tracks.
//
// Kind semantics:
//   - "execution": participates in every run — decisions are made even when absent.
//   - "capability": full StageFreight domain, inactive when unconfigured.
//     Inactive ≠ unimportant. Shown in the inactive row so operators can see
//     which domains are dormant.
//   - "structural": identity/infrastructure config (forges, repos, registries).
//     Shown in active row when present; not shown in inactive row.
var allKnownSections = []sectionDef{
	// Execution sections
	{name: "builds", kind: "execution"},
	{name: "versioning", kind: "execution"},
	{name: "lint", kind: "execution"},
	{name: "security", kind: "execution"},
	{name: "commit", kind: "execution"},
	{name: "dependency", kind: "execution"},
	{name: "docs", kind: "execution"},
	{name: "release", kind: "execution"},
	// Capability domains — full SF domains, inactive when unconfigured
	{name: "gitops", kind: "capability"},
	{name: "governance", kind: "capability"},
	{name: "glossary", kind: "capability"},
	{name: "presentation", kind: "capability"},
	{name: "manifest", kind: "capability"},
	{name: "tag", kind: "capability"},
	// Structural config — identity graph
	{name: "forges", kind: "structural"},
	{name: "repos", kind: "structural"},
	{name: "registries", kind: "structural"},
	{name: "build_cache", kind: "structural"},
	{name: "matchers", kind: "structural"},
}

// LoadWithReport loads config and returns a ConfigReport with real provenance.
// Calls loadResolved — the same core path used by all runners — and builds the
// explainability layer from the merge entries that loadResolved already produced.
// Does not run the resolver twice. Report is derived from resolved state, never
// the other way around.
//
// LoadWithReport is a thin wrapper over loadResolved for callers that need
// explainability output (ConfigReport). It MUST NOT introduce any alternate
// config loading behavior — all resolution logic lives in loadResolved.
func LoadWithReport(path string) (*Config, ConfigReport, error) {
	if path == "" {
		path = defaultConfigFile
	}

	absPath, _ := filepath.Abs(path)
	report := ConfigReport{
		SourceFile:   absPath,
		Status:       "ok",
		Completeness: "complete",
	}

	cfg, warnings, entries, err := loadResolved(path)
	if err != nil {
		report.Status = "error"
		report.Completeness = "partial"
		report.Error = err.Error()
		return nil, report, err
	}

	// Surface preset resolution failure in report status.
	for _, w := range warnings {
		if strings.HasPrefix(w, "preset resolution incomplete:") {
			report.Status = "partial"
			report.Completeness = "partial"
		}
	}
	report.Warnings = warnings

	if report.Status == "partial" {
		// Resolver failed — fall back to key scan for section provenance.
		data, _ := os.ReadFile(path)
		report.Sections = buildSectionsFromKeys(parseToplevelKeys(data))
	} else {
		report.Sections, report.Presets, report.Overrides = buildSectionsFromEntries(entries, absPath)
	}

	report.VarsApplied = len(cfg.Vars)
	return cfg, report, nil
}

// buildSectionsFromEntries builds SectionState from real resolver merge entries.
// Returns sections, preset paths (in sorted order), and override count.
func buildSectionsFromEntries(entries []preset.MergeEntry, configPath string) ([]SectionState, []string, int) {
	sectionSource := make(map[string]string)
	presetPaths := make(map[string]bool)
	overrides := 0

	for _, e := range entries {
		section := strings.SplitN(e.Path, ".", 2)[0]
		if strings.HasPrefix(e.Source, "preset:") {
			presetPath := strings.TrimPrefix(e.Source, "preset:")
			presetPaths[presetPath] = true
			if _, seen := sectionSource[section]; !seen {
				sectionSource[section] = "preset"
			}
		} else {
			if _, seen := sectionSource[section]; !seen {
				sectionSource[section] = "manifest"
			}
		}
		if e.Overridden {
			overrides++
		}
	}

	var presets []string
	for p := range presetPaths {
		presets = append(presets, p)
	}
	sort.Strings(presets)

	return buildSectionsFromMap(sectionSource), presets, overrides
}

// buildSectionsFromKeys is the fallback when the resolver cannot run.
// Treats all present keys as manifest provenance.
func buildSectionsFromKeys(present map[string]bool) []SectionState {
	sources := make(map[string]string)
	for k := range present {
		sources[k] = "manifest"
	}
	return buildSectionsFromMap(sources)
}

// buildSectionsFromMap constructs the ordered SectionState slice from a name→provenance map.
func buildSectionsFromMap(sectionSource map[string]string) []SectionState {
	var sections []SectionState
	for _, def := range allKnownSections {
		src := sectionSource[def.name]
		active := src != ""
		provenance := src
		resStatus := "none"
		if !active {
			provenance = "none"
		} else {
			resStatus = "resolved"
		}
		ss := SectionState{
			Name:             def.name,
			Kind:             def.kind,
			Active:           active,
			SourcePresent:    src == "manifest",
			Provenance:       provenance,
			ResolutionStatus: resStatus,
		}
		if msg := ss.validate(); msg != "" {
			panic("SectionState invariant violated: " + msg)
		}
		sections = append(sections, ss)
	}
	return sections
}

// parseToplevelKeys returns the set of top-level YAML keys present in data.
// Used as a fallback when the preset resolver cannot run.
func parseToplevelKeys(data []byte) map[string]bool {
	present := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			if key != "" {
				present[key] = true
			}
		}
	}
	return present
}
