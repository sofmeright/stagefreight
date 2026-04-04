package docker

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/output"
)

// cleanupPhase returns a pipeline phase that runs host cleanup before builds.
// Skips silently if build_cache mode is inactive or cleanup has no commands.
// Enforcement mode controls failure behavior:
//   - best_effort: warn on failure, continue
//   - required: fail the build on any cleanup error
func cleanupPhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "cleanup",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			if !pc.Config.BuildCache.Cleanup.Enabled {
				return nil, nil // cleanup not enabled — skip silently
			}

			commands := BuildCleanupCommands(pc.Config.BuildCache.Cleanup)
			if len(commands) == 0 {
				return nil, nil // nothing to clean
			}

			enforcement := pc.Config.BuildCache.Cleanup.Enforcement
			if enforcement == "" {
				enforcement = "best_effort"
			}

			start := time.Now()
			result := executeCleanup(commands, enforcement)
			elapsed := time.Since(start)

			// Render structured output.
			renderCleanupSection(pc, result, enforcement, elapsed)

			// Determine phase outcome based on enforcement mode.
			if result.hasError() && enforcement == "required" {
				return &pipeline.PhaseResult{
					Name:    "cleanup",
					Status:  "failed",
					Summary: fmt.Sprintf("%d/%d commands failed (enforcement: required)", result.failCount(), len(commands)),
					Elapsed: elapsed,
				}, fmt.Errorf("host cleanup failed (enforcement: required)")
			}

			summary := fmt.Sprintf("%d commands, %d reclaimed", len(commands), result.successCount())
			if result.hasError() {
				summary += fmt.Sprintf(", %d failed (best_effort)", result.failCount())
			}

			return &pipeline.PhaseResult{
				Name:    "cleanup",
				Status:  "success",
				Summary: summary,
				Elapsed: elapsed,
			}, nil
		},
	}
}

// executeCleanup runs cleanup commands sequentially, collecting results.
func executeCleanup(commands []CleanupCommand, enforcement string) CleanupResult {
	result := CleanupResult{Executed: true}

	for _, cmd := range commands {
		cr := CleanupCommandResult{
			Class:   cmd.Class,
			Command: cmd.Command,
		}

		out, err := exec.Command("sh", "-c", cmd.Command).CombinedOutput()
		cr.Output = strings.TrimSpace(string(out))
		cr.Error = err

		result.Results = append(result.Results, cr)

		// In required mode, stop on first failure.
		if err != nil && enforcement == "required" {
			break
		}
	}

	return result
}

// renderCleanupSection writes structured cleanup output.
func renderCleanupSection(pc *pipeline.PipelineContext, result CleanupResult, enforcement string, elapsed time.Duration) {
	sec := output.NewSection(pc.Writer, "Cleanup", elapsed, pc.Color)
	sec.Row("%-14s%s", "enforcement", enforcement)
	sec.Row("%-14s%d", "commands", len(result.Results))

	for _, cr := range result.Results {
		icon := output.StatusIcon("success", pc.Color)
		detail := ""
		if cr.Error != nil {
			icon = output.StatusIcon("failed", pc.Color)
			detail = cr.Error.Error()
		} else if cr.Output != "" {
			// Show first line of output (reclaimed space, etc.)
			lines := strings.SplitN(cr.Output, "\n", 2)
			detail = lines[0]
		}
		sec.Row("  %s %-28s %s", icon, cr.Class, detail)
	}

	sec.Close()
}

func (r CleanupResult) hasError() bool {
	for _, cr := range r.Results {
		if cr.Error != nil {
			return true
		}
	}
	return false
}

func (r CleanupResult) failCount() int {
	n := 0
	for _, cr := range r.Results {
		if cr.Error != nil {
			n++
		}
	}
	return n
}

func (r CleanupResult) successCount() int {
	n := 0
	for _, cr := range r.Results {
		if cr.Error == nil {
			n++
		}
	}
	return n
}
