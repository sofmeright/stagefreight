package build

import (
	"strconv"
	"time"
)

// LayerEvent represents a completed build layer parsed from buildx output.
type LayerEvent struct {
	Stage       string        // "builder", "stage-1", "" (for internal steps)
	StageStep   string        // "1/7", "2/7", etc.
	Instruction string        // "FROM", "COPY", "RUN", "WORKDIR", "ENV", "ARG", "EXPOSE", "ADD"
	Detail      string        // instruction arguments (truncated)
	Cached      bool          // true if layer was a cache hit
	Duration    time.Duration // layer execution time (0 for cached layers)
	Image       string        // for FROM: the base image name (without digest)
}

// FormatLayerTiming formats a layer's timing for display.
// Returns "cached" for cache hits, or the duration string for completed layers.
func FormatLayerTiming(e LayerEvent) string {
	if e.Cached {
		return "cached"
	}
	if e.Duration > 0 {
		return formatBuildDuration(e.Duration)
	}
	return ""
}

// formatBuildDuration formats a duration for build layer display.
func formatBuildDuration(d time.Duration) string {
	if d >= time.Minute {
		return strconv.FormatFloat(d.Minutes(), 'f', 1, 64) + "m"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}

// FormatLayerInstruction formats a layer event into a display string.
// For FROM instructions, shows the base image name.
// For other instructions, shows the instruction and truncated detail.
func FormatLayerInstruction(e LayerEvent) string {
	if e.Instruction == "FROM" && e.Image != "" {
		return e.Image
	}
	if e.Detail != "" {
		return e.Instruction + " " + e.Detail
	}
	return e.Instruction
}
