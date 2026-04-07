package governance

// Preset resolution has moved to src/config/preset.
// This file re-exports the public API so existing governance callers
// continue to compile without changes.
//
// New callers should import github.com/PrPlanIT/StageFreight/src/config/preset directly.

import "github.com/PrPlanIT/StageFreight/src/config/preset"

// PresetLoader re-exported from config/preset.
type PresetLoader = preset.PresetLoader

// ResolvePresets re-exported from config/preset.
var ResolvePresets = preset.ResolvePresets

// ValidatePreset re-exported from config/preset.
var ValidatePreset = preset.ValidatePreset

// DeepMerge re-exported from config/preset.
var DeepMerge = preset.DeepMerge
