package docker

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/output"
)

// BuildkitPruneResult records what buildx prune did.
type BuildkitPruneResult struct {
	Builder     string
	MaxAge      string // filter applied (human, e.g. "168h")
	KeepBytes   int64  // --keep-storage value in bytes
	Command     string // exact command executed
	Output      string // raw buildx output
	Reclaimed   string // parsed reclaimed amount, or "0 B"
	UsageBefore string // from buildx du before prune
	UsageAfter  string // from buildx du after prune
	Duration    time.Duration
	Error       error
	Skipped     bool
	SkipReason  string
}

// pruneBuildkitCache enforces retention policy against a buildx builder via prune.
// This is the enforcement mechanism for build_cache.local.retention when the cache
// lives inside BuildKit (remote or docker-container driver) rather than on the
// local export filesystem.
//
// Prune operates at builder scope — all cache in the builder is subject to the
// policy, regardless of which repo produced it.
func pruneBuildkitCache(builder string, retention config.LocalRetention, verbose bool) BuildkitPruneResult {
	result := BuildkitPruneResult{Builder: builder}

	if retention.MaxAge == "" && retention.MaxSize == "" {
		result.Skipped = true
		result.SkipReason = "no retention policy configured"
		return result
	}

	// Inspect current usage before prune (informational, never gates decisions).
	result.UsageBefore = inspectBuilderUsage(builder)

	// Build the prune command.
	args := []string{"buildx", "prune", "--builder", builder, "--force"}

	if retention.MaxAge != "" {
		dur, err := config.ParseDuration(retention.MaxAge)
		if err != nil {
			result.Error = fmt.Errorf("invalid max_age %q: %w", retention.MaxAge, err)
			return result
		}
		if dur > 0 {
			result.MaxAge = formatDurationHuman(dur)
			args = append(args, "--filter", fmt.Sprintf("until=%s", formatDurationHuman(dur)))
		}
	}

	if retention.MaxSize != "" {
		bytes, err := config.ParseSize(retention.MaxSize)
		if err != nil {
			result.Error = fmt.Errorf("invalid max_size %q: %w", retention.MaxSize, err)
			return result
		}
		if bytes > 0 {
			result.KeepBytes = bytes
			args = append(args, "--keep-storage", fmt.Sprintf("%d", bytes))
		}
	}

	result.Command = "docker " + strings.Join(args, " ")

	start := time.Now()
	out, err := exec.Command("docker", args...).CombinedOutput()
	result.Duration = time.Since(start)
	result.Output = strings.TrimSpace(string(out))
	result.Error = err

	// Only parse reclaimed on success — failure output may be partial/garbage.
	if err == nil {
		result.Reclaimed = parseReclaimedFromOutput(result.Output)
		// Inspect usage after prune for the before/after/reclaimed trio.
		result.UsageAfter = inspectBuilderUsage(builder)
	} else {
		result.Reclaimed = "0 B"
	}

	return result
}

// inspectBuilderUsage runs `docker buildx du` to capture current cache usage.
// Returns the total line (e.g. "2.3GB") or empty string on failure.
// Informational only — never gates decisions.
func inspectBuilderUsage(builder string) string {
	out, err := exec.Command("docker", "buildx", "du", "--builder", builder).CombinedOutput()
	if err != nil {
		return ""
	}
	// buildx du prints records then a "Total:" summary line.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Total:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Total:"))
		}
	}
	return ""
}

// parseReclaimedFromOutput extracts the "Total: ..." line from buildx prune output.
func parseReclaimedFromOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Total:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Total:"))
		}
	}
	return "0 B"
}

// formatDurationHuman renders a duration without trailing zero components.
// 168h0m0s → 168h, 72h30m0s → 72h30m, 48h → 48h
func formatDurationHuman(d time.Duration) string {
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute

	if m > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dh", h)
}

// renderBuildkitPrune writes structured output for the buildkit cache prune.
func renderBuildkitPrune(w io.Writer, color bool, result BuildkitPruneResult, verbose bool) {
	sec := output.NewSection(w, "Cache Prune (buildkit)", result.Duration, color)

	sec.Row("%-14s%s", "builder", result.Builder)

	if result.Skipped {
		sec.Row("%-14s%s", "status", "skipped")
		sec.Row("%-14s%s", "reason", result.SkipReason)
		sec.Close()
		return
	}

	if result.MaxAge != "" {
		sec.Row("%-14s%s", "filter", fmt.Sprintf("until=%s", result.MaxAge))
	}
	if result.KeepBytes > 0 {
		sec.Row("%-14s%s", "keep-storage", formatBytes(result.KeepBytes))
	}

	if result.Error != nil {
		sec.Row("%-14s%s", "status", "failed")
		sec.Row("%-14s%v", "error", result.Error)
	} else {
		// Before / reclaimed / after trio — observable cache state transition.
		if result.UsageBefore != "" {
			sec.Row("%-14s%s", "usage-before", result.UsageBefore)
		}
		sec.Row("%-14s%s", "reclaimed", result.Reclaimed)
		if result.UsageAfter != "" {
			sec.Row("%-14s%s", "usage-after", result.UsageAfter)
		}
	}

	// Always show the command — exact reproducibility for debugging.
	if result.Command != "" {
		sec.Row("%-14s%s", "command", result.Command)
	}

	sec.Close()
}
