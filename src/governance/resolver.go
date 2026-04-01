package governance

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// PresetLoader fetches preset content by path.
// Implementations: local filesystem, git repo checkout.
type PresetLoader interface {
	Load(path string) ([]byte, error)
}

// ResolvePresets walks a config map, finds all preset: references,
// loads the preset files, validates the single-key invariant, and merges.
// Recursive: presets may reference other presets (depth-first resolution).
// sourceRef is the repo identity (e.g., "PrPlanIT/MaintenancePolicy@v1.0.0").
// sourcePath is the current file being processed (e.g., "preset/docker-targets.yml").
// Returns the resolved config + provenance entries.
func ResolvePresets(raw map[string]any, loader PresetLoader, sourceRef, sourcePath string, depth int, seen map[string]bool) (map[string]any, []MergeEntry, error) {
	if seen == nil {
		seen = make(map[string]bool)
	}

	var entries []MergeEntry
	result := make(map[string]any)

	for key, val := range raw {
		section, ok := val.(map[string]any)
		if !ok {
			// Scalar or list — copy directly.
			result[key] = val
			entries = append(entries, MergeEntry{
				Path:      key,
				Source:     sourcePath,
				SourceRef: sourceRef,
				Layer:     depth,
				Operation: "set",
				Value:     val,
			})
			continue
		}

		// Check for preset: reference in this section.
		presetPath, hasPreset := extractPresetPath(section)
		if !hasPreset {
			// No preset — recurse into subsections.
			resolved, subEntries, err := ResolvePresets(section, loader, sourceRef, sourcePath, depth, seen)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", key, err)
			}
			result[key] = resolved
			for i := range subEntries {
				subEntries[i].Path = key + "." + subEntries[i].Path
			}
			entries = append(entries, subEntries...)
			continue
		}

		// Cycle detection.
		if seen[presetPath] {
			return nil, nil, fmt.Errorf("%s: circular preset reference: %s", key, presetPath)
		}
		seen[presetPath] = true

		// Load and validate preset.
		presetContent, err := loader.Load(presetPath)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: loading preset %q: %w", key, presetPath, err)
		}

		topKey, presetParsed, err := ValidatePreset(presetContent)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: preset %q: %w", key, presetPath, err)
		}

		// The preset's top-level key must match the section key it's imported into.
		if topKey != key {
			return nil, nil, fmt.Errorf("%s: preset %q declares top-level key %q, expected %q", key, presetPath, topKey, key)
		}

		presetValue := presetParsed[topKey]
		presetSection, isMap := presetValue.(map[string]any)

		// If the preset value is a list (e.g., targets: [...]), use it directly.
		// No recursive resolution needed for list-type sections.
		if !isMap {
			// List or scalar preset — merge directly. Local sibling keys override.
			localOverrides := withoutKey(section, "preset")
			if len(localOverrides) > 0 {
				// Can't deep-merge into a list — local replaces entirely.
				result[key] = localOverrides
			} else {
				result[key] = presetValue
			}
			entries = append(entries, MergeEntry{
				Path:      key,
				Source:    "preset:" + presetPath,
				SourceRef: sourceRef,
				Layer:     depth + 1,
				Operation: "set",
				Value:     presetValue,
			})
			delete(seen, presetPath)
			continue
		}

		// Check if the loaded preset itself references another preset (nested/chained).
		innerPresetPath, hasInnerPreset := extractPresetPath(presetSection)
		var resolvedPreset map[string]any
		var presetEntries []MergeEntry

		if hasInnerPreset {
			// Cycle detection for inner preset.
			if seen[innerPresetPath] {
				return nil, nil, fmt.Errorf("%s: circular preset reference: %s → %s", key, presetPath, innerPresetPath)
			}

			// Recursively resolve the inner preset first (depth-first).
			innerWrapped := map[string]any{topKey: map[string]any{"preset": innerPresetPath}}
			resolvedInner, innerEntries, err := ResolvePresets(innerWrapped, loader, sourceRef, presetPath, depth+1, seen)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: resolving nested preset %q in %q: %w", key, innerPresetPath, presetPath, err)
			}

			// Merge: inner preset as base, current preset's other keys on top.
			innerSection := resolvedInner[topKey].(map[string]any)
			currentOverrides := withoutKey(presetSection, "preset")
			resolvedPreset = DeepMerge(innerSection, currentOverrides)
			presetEntries = innerEntries
		} else {
			// No nested preset — just recurse into subsections of the preset content.
			var err error
			resolvedPreset, presetEntries, err = ResolvePresets(presetSection, loader, sourceRef, presetPath, depth+1, seen)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: resolving preset %q: %w", key, presetPath, err)
			}
		}

		// Record preset entries with provenance.
		for i := range presetEntries {
			presetEntries[i].Path = key + "." + presetEntries[i].Path
			presetEntries[i].Source = "preset:" + presetPath
		}
		entries = append(entries, presetEntries...)

		// Local siblings (everything except preset: key) override the preset.
		localOverrides := withoutKey(section, "preset")

		merged := DeepMerge(resolvedPreset, localOverrides)

		// Mark preset entries that got overridden by local keys.
		for localKey := range localOverrides {
			overriddenPath := key + "." + localKey
			for i := range entries {
				if entries[i].Path == overriddenPath && !entries[i].Overridden {
					entries[i].Overridden = true
					entries[i].OverriddenBy = sourcePath
				}
			}
		}

		// Record local override entries.
		for localKey, localVal := range localOverrides {
			path := key + "." + localKey
			op := "override"
			// Detect list replacement.
			if _, isList := localVal.([]any); isList {
				op = "replace"
			}
			entries = append(entries, MergeEntry{
				Path:      path,
				Source:     sourcePath,
				SourceRef: sourceRef,
				Layer:     depth,
				Operation: op,
				Value:     localVal,
			})
		}

		result[key] = merged

		// Unmark for cycle detection (allow same preset in different branches).
		delete(seen, presetPath)
	}

	return result, entries, nil
}

// ValidatePreset checks that a preset file declares exactly one top-level key.
// Returns hard error on violation.
func ValidatePreset(content []byte) (string, map[string]any, error) {
	var parsed map[string]any
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		return "", nil, fmt.Errorf("invalid YAML: %w", err)
	}

	// Filter out comment-only keys (none expected, but be safe).
	keys := make([]string, 0, len(parsed))
	for k := range parsed {
		keys = append(keys, k)
	}

	if len(keys) == 0 {
		return "", nil, fmt.Errorf("preset is empty (no top-level keys)")
	}
	if len(keys) > 1 {
		return "", nil, fmt.Errorf("preset declares %d top-level keys (%s), must declare exactly one",
			len(keys), strings.Join(keys, ", "))
	}

	return keys[0], parsed, nil
}

// extractPresetPath checks if a section map has a "preset" key and returns the path.
func extractPresetPath(section map[string]any) (string, bool) {
	val, ok := section["preset"]
	if !ok {
		return "", false
	}
	path, isStr := val.(string)
	if !isStr || path == "" {
		return "", false
	}
	return path, true
}

// withoutKey returns a shallow copy of the map with the specified key removed.
func withoutKey(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}
