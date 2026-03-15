package manifest

import (
	"encoding/json"
	"fmt"
)

// InspectOptions controls manifest inspection behavior.
type InspectOptions struct {
	Section string // dot-path to extract (empty = whole manifest)
	Format  string // "json", "table", "human" (default: "human")
}

// Inspect pretty-prints a manifest or a specific section.
func Inspect(m *Manifest, opts InspectOptions) (string, error) {
	format := opts.Format
	if format == "" {
		format = "human"
	}

	if opts.Section == "" {
		// Whole manifest
		switch format {
		case "json":
			data, err := MarshalDeterministic(m)
			if err != nil {
				return "", err
			}
			return string(data), nil
		default:
			return inspectWholeHuman(m)
		}
	}

	// Extract section
	data, err := ExtractSection(m, opts.Section)
	if err != nil {
		return "", err
	}

	switch format {
	case "json":
		out, jerr := json.MarshalIndent(data, "", "  ")
		if jerr != nil {
			return "", jerr
		}
		return string(out) + "\n", nil

	case "table":
		return RenderTable(data, nil)

	default: // "human"
		// Try table first, fall back to kv, fall back to json
		if table, err := RenderTable(data, nil); err == nil {
			return table, nil
		}
		if kv, err := RenderKV(data); err == nil {
			return kv, nil
		}
		out, jerr := json.MarshalIndent(data, "", "  ")
		if jerr != nil {
			return "", jerr
		}
		return string(out) + "\n", nil
	}
}

func inspectWholeHuman(m *Manifest) (string, error) {
	var s string

	s += fmt.Sprintf("Manifest: %s (schema v%d)\n", m.Scope.Name, m.SchemaVersion)
	s += fmt.Sprintf("State: %s | Mode: %s\n", m.Metadata.State, m.Metadata.Mode)

	if m.Repo.URL != "" {
		s += fmt.Sprintf("Repo: %s @ %s\n", m.Repo.URL, m.Repo.Commit)
	}

	s += fmt.Sprintf("Build: %s (Dockerfile: %s)\n", m.Build.BuildID, m.Build.Dockerfile)

	if m.Build.BaseImage != "" {
		s += fmt.Sprintf("Base: %s\n", m.Build.BaseImage)
	}

	// Inventory summary
	counts := map[string]int{
		"versions": len(m.Inventories.Versions),
		"apk":      len(m.Inventories.Apk),
		"apt":      len(m.Inventories.Apt),
		"pip":      len(m.Inventories.Pip),
		"galaxy":   len(m.Inventories.Galaxy),
		"npm":      len(m.Inventories.Npm),
		"go":       len(m.Inventories.Go),
		"binaries": len(m.Inventories.Binaries),
	}

	var nonEmpty []string
	for name, count := range counts {
		if count > 0 {
			nonEmpty = append(nonEmpty, fmt.Sprintf("%s(%d)", name, count))
		}
	}
	if len(nonEmpty) > 0 {
		sortStrings(nonEmpty)
		s += fmt.Sprintf("Inventory: %s\n", join(nonEmpty, ", "))
	}

	s += fmt.Sprintf("Completeness: image=%v security=%v sbom=%v\n",
		m.Completeness.ImageMeta, m.Completeness.SecurityMeta, m.Completeness.SBOMImported)

	return s, nil
}

func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += sep + p
	}
	return result
}
