package governance

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// keyedListSections maps section path to the item key field for ordered list composition.
// Only sections listed here may use the presets: [...] form.
var keyedListSections = map[string]string{
	"targets":                  "id",
	"builds":                   "id",
	"badges.items":             "id",
	"versioning.tag_sources":   "id",
	"versioning.branch_builds": "id",
	"narrator":                 "file",
}

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
	return resolvePresetsInner(raw, loader, sourceRef, sourcePath, depth, seen, "")
}

// resolvePresetsInner is the internal recursive implementation.
// pathPrefix is the absolute dotted path of the parent section (empty at root).
// All MergeEntry.Path values returned are absolute (pathPrefix + "." + key).
func resolvePresetsInner(raw map[string]any, loader PresetLoader, sourceRef, sourcePath string, depth int, seen map[string]bool, pathPrefix string) (map[string]any, []MergeEntry, error) {
	var entries []MergeEntry
	result := make(map[string]any)

	for key, val := range raw {
		currentPath := key
		if pathPrefix != "" {
			currentPath = pathPrefix + "." + key
		}

		section, ok := val.(map[string]any)
		if !ok {
			// Scalar or list — copy directly.
			result[key] = val
			entries = append(entries, MergeEntry{
				Path:      currentPath,
				Source:    sourcePath,
				SourceRef: sourceRef,
				Layer:     depth,
				Operation: "set",
				Value:     val,
			})
			continue
		}

		presetPath, hasPreset := extractPresetPath(section)
		presetsList, hasPresets := extractPresetsList(section)

		if hasPreset && hasPresets {
			return nil, nil, fmt.Errorf("%s: cannot specify both preset: and presets:", currentPath)
		}

		// presets: [...] — ordered composition for keyed-collection sections only.
		if hasPresets {
			keyField, isKeyed := keyedListSections[currentPath]
			if !isKeyed {
				return nil, nil, fmt.Errorf("%s: presets: is only allowed on keyed-collection sections (targets, builds, narrator, badges.items, versioning.tag_sources, versioning.branch_builds)", currentPath)
			}
			list, listEntries, err := resolvePresetList(presetsList, currentPath, section, loader, sourceRef, sourcePath, depth, seen, keyField)
			if err != nil {
				return nil, nil, err
			}
			if currentPath == "narrator" {
				list, err = mergeNarratorEntries(list)
				if err != nil {
					return nil, nil, err
				}
			}
			result[key] = list
			entries = append(entries, listEntries...)
			continue
		}

		if !hasPreset {
			// No preset — recurse into subsections.
			resolved, subEntries, err := resolvePresetsInner(section, loader, sourceRef, sourcePath, depth, seen, currentPath)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", key, err)
			}
			result[key] = resolved
			entries = append(entries, subEntries...)
			continue
		}

		// --- Single preset: "path" handling ---

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
		if !isMap {
			localOverrides := withoutKey(section, "preset")
			if len(localOverrides) > 0 {
				result[key] = localOverrides
			} else {
				result[key] = presetValue
			}
			entries = append(entries, MergeEntry{
				Path:      currentPath,
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
			// Use pathPrefix="" so the inner wrapped map builds absolute paths from root.
			innerWrapped := map[string]any{topKey: map[string]any{"preset": innerPresetPath}}
			resolvedInner, innerEntries, err := resolvePresetsInner(innerWrapped, loader, sourceRef, presetPath, depth+1, seen, "")
			if err != nil {
				return nil, nil, fmt.Errorf("%s: resolving nested preset %q in %q: %w", key, innerPresetPath, presetPath, err)
			}

			// Merge: inner preset as base, current preset's other keys on top.
			innerSection := resolvedInner[topKey].(map[string]any)
			currentOverrides := withoutKey(presetSection, "preset")
			resolvedPreset = DeepMerge(innerSection, currentOverrides)
			presetEntries = innerEntries
		} else {
			// No nested preset — recurse into subsections of the preset content.
			// Pass currentPath so entries are built with absolute paths.
			resolvedPreset, presetEntries, err = resolvePresetsInner(presetSection, loader, sourceRef, presetPath, depth+1, seen, currentPath)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: resolving preset %q: %w", key, presetPath, err)
			}
		}

		// Tag all preset entries with this preset's source.
		// Paths are already absolute — no prefixing needed.
		for i := range presetEntries {
			presetEntries[i].Source = "preset:" + presetPath
		}
		entries = append(entries, presetEntries...)

		// Local siblings (everything except preset: key) override the preset.
		localOverrides := withoutKey(section, "preset")
		merged := DeepMerge(resolvedPreset, localOverrides)

		// Mark preset entries that got overridden by local keys.
		for localKey := range localOverrides {
			overriddenPath := currentPath + "." + localKey
			for i := range entries {
				if entries[i].Path == overriddenPath && !entries[i].Overridden {
					entries[i].Overridden = true
					entries[i].OverriddenBy = sourcePath
				}
			}
		}

		// Record local override entries.
		for localKey, localVal := range localOverrides {
			path := currentPath + "." + localKey
			op := "override"
			if _, isList := localVal.([]any); isList {
				op = "replace"
			}
			entries = append(entries, MergeEntry{
				Path:      path,
				Source:    sourcePath,
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

// resolvePresetList loads and merges an ordered list of presets for a keyed-collection section.
// sectionPath is the dotted path (e.g., "targets", "badges.items", "versioning.tag_sources").
// section is the raw section map (contains "presets" key and any local sibling items).
// keyField is the item identity field ("id" or "file").
// Items are collected in preset declaration order; local siblings are appended last.
// Duplicate key field values across presets are a hard error naming both contributing presets.
func resolvePresetList(
	presets []string,
	sectionPath string,
	section map[string]any,
	loader PresetLoader,
	sourceRef, sourcePath string,
	depth int,
	seen map[string]bool,
	keyField string,
) ([]any, []MergeEntry, error) {
	// Parse navigation path from sectionPath.
	// "targets"        → topKey="targets", navPath=[]
	// "badges.items"   → topKey="badges",  navPath=["items"]
	parts := strings.SplitN(sectionPath, ".", 2)
	topKey := parts[0]
	var navPath []string
	if len(parts) > 1 {
		navPath = strings.Split(parts[1], ".")
	}

	var collected []any
	var entries []MergeEntry
	seenIDs := make(map[string]string) // key-field value → contributing preset path

	for _, presetPath := range presets {
		if seen[presetPath] {
			return nil, nil, fmt.Errorf("%s: circular preset reference: %s", sectionPath, presetPath)
		}
		seen[presetPath] = true

		content, err := loader.Load(presetPath)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: loading preset %q: %w", sectionPath, presetPath, err)
		}

		loadedTopKey, parsed, err := ValidatePreset(content)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: preset %q: %w", sectionPath, presetPath, err)
		}

		if loadedTopKey != topKey {
			return nil, nil, fmt.Errorf("%s: preset %q declares top-level key %q, expected %q", sectionPath, presetPath, loadedTopKey, topKey)
		}

		// Navigate to the list using navPath.
		var listVal any = parsed[topKey]
		for _, nav := range navPath {
			m, ok := listVal.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%s: preset %q: expected map while navigating to %q", sectionPath, presetPath, nav)
			}
			next, exists := m[nav]
			if !exists {
				return nil, nil, fmt.Errorf("%s: preset %q: missing key %q in navigation path", sectionPath, presetPath, nav)
			}
			listVal = next
		}

		items, ok := listVal.([]any)
		if !ok || listVal == nil {
			// Empty or missing list — skip this preset.
			delete(seen, presetPath)
			continue
		}

		for _, item := range items {
			itemMap, ok := item.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%s: preset %q: item is not a map", sectionPath, presetPath)
			}
			idVal, hasID := itemMap[keyField]
			if !hasID {
				return nil, nil, fmt.Errorf("%s: preset %q: item missing %q field", sectionPath, presetPath, keyField)
			}
			idStr := fmt.Sprintf("%v", idVal)
			// Narrator outer keys (file:) may repeat across presets — mergeNarratorEntries
			// handles same-file merging and enforces uniqueness at the item id level.
			if sectionPath != "narrator" {
				if firstPath, dup := seenIDs[idStr]; dup {
					return nil, nil, fmt.Errorf("%s: duplicate %s %q\n  first contributed by: %s\n  duplicate from:       %s",
						sectionPath, keyField, idStr, firstPath, presetPath)
				}
				seenIDs[idStr] = presetPath
			}
			collected = append(collected, item)
			entries = append(entries, MergeEntry{
				Path:      fmt.Sprintf("%s[%s]", sectionPath, idStr),
				Source:    "preset:" + presetPath,
				SourceRef: sourceRef,
				Layer:     depth + 1,
				Operation: "append",
				Value:     item,
			})
		}

		delete(seen, presetPath)
	}

	// Inline items: list under "items" key in the section map.
	// Same shape as preset items — consistent validation path.
	if inlineList, ok := section["items"].([]any); ok {
		for _, item := range inlineList {
			itemMap, ok := item.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%s: inline item is not a map", sectionPath)
			}
			idVal, hasID := itemMap[keyField]
			if !hasID {
				return nil, nil, fmt.Errorf("%s: inline item missing %q field", sectionPath, keyField)
			}
			idStr := fmt.Sprintf("%v", idVal)
			if firstPath, dup := seenIDs[idStr]; dup {
				return nil, nil, fmt.Errorf("%s: duplicate %s %q\n  first contributed by: %s\n  duplicate from:       %s (inline)",
					sectionPath, keyField, idStr, firstPath, sourcePath)
			}
			seenIDs[idStr] = sourcePath
			collected = append(collected, itemMap)
			entries = append(entries, MergeEntry{
				Path:      fmt.Sprintf("%s[%s]", sectionPath, idStr),
				Source:    sourcePath,
				SourceRef: sourceRef,
				Layer:     depth,
				Operation: "append",
				Value:     itemMap,
			})
		}
	}

	if collected == nil {
		collected = []any{}
	}
	return collected, entries, nil
}

// mergeNarratorEntries post-processes the narrator list after resolvePresetList.
// narrator entries are keyed by file:; multiple presets may target the same file.
// Same-file entries are merged: their items: lists are concatenated in encounter order.
// Duplicate item id within a file is a hard error.
func mergeNarratorEntries(list []any) ([]any, error) {
	type fileEntry struct {
		fileStr    string
		seenItemID map[string]bool
		raw        map[string]any // all fields except items
		items      []any
	}

	var order []string
	byFile := make(map[string]*fileEntry)

	for _, entry := range list {
		entryMap, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		fileVal, ok := entryMap["file"]
		if !ok {
			continue
		}
		fileStr := fmt.Sprintf("%v", fileVal)

		fe, exists := byFile[fileStr]
		if !exists {
			fe = &fileEntry{
				fileStr:    fileStr,
				seenItemID: make(map[string]bool),
				raw:        make(map[string]any),
			}
			for k, v := range entryMap {
				if k != "items" {
					fe.raw[k] = v
				}
			}
			order = append(order, fileStr)
			byFile[fileStr] = fe
		}

		items, _ := entryMap["items"].([]any)
		for _, item := range items {
			itemMap, ok := item.(map[string]any)
			if !ok {
				fe.items = append(fe.items, item)
				continue
			}
			idVal, hasID := itemMap["id"]
			if !hasID {
				fe.items = append(fe.items, item)
				continue
			}
			idStr := fmt.Sprintf("%v", idVal)
			if fe.seenItemID[idStr] {
				return nil, fmt.Errorf("narrator: duplicate item id %q for file %q", idStr, fileStr)
			}
			fe.seenItemID[idStr] = true
			fe.items = append(fe.items, item)
		}
	}

	result := make([]any, 0, len(order))
	for _, fileStr := range order {
		fe := byFile[fileStr]
		m := make(map[string]any, len(fe.raw)+1)
		for k, v := range fe.raw {
			m[k] = v
		}
		m["items"] = fe.items
		result = append(result, m)
	}
	return result, nil
}

// ValidatePreset checks that a preset file declares exactly one top-level key.
// Returns hard error on violation.
func ValidatePreset(content []byte) (string, map[string]any, error) {
	var parsed map[string]any
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		return "", nil, fmt.Errorf("invalid YAML: %w", err)
	}

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

// extractPresetsList checks if a section map has a "presets" key containing a string list.
func extractPresetsList(section map[string]any) ([]string, bool) {
	val, ok := section["presets"]
	if !ok {
		return nil, false
	}
	list, isList := val.([]any)
	if !isList {
		return nil, false
	}
	var paths []string
	for _, item := range list {
		s, isStr := item.(string)
		if !isStr || s == "" {
			return nil, false
		}
		paths = append(paths, s)
	}
	return paths, true
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

// DeepMerge merges two maps. Override (second arg) wins on conflict.
// Objects: deep merge. Scalars: override replaces. Lists: override replaces.
func DeepMerge(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(override))

	for k, v := range base {
		result[k] = v
	}

	for k, overrideVal := range override {
		baseVal, exists := result[k]
		if !exists {
			result[k] = overrideVal
			continue
		}

		baseMap, baseIsMap := baseVal.(map[string]any)
		overrideMap, overrideIsMap := overrideVal.(map[string]any)

		if baseIsMap && overrideIsMap {
			result[k] = DeepMerge(baseMap, overrideMap)
		} else {
			result[k] = overrideVal
		}
	}

	return result
}
