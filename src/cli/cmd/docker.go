package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/ci"
	"github.com/PrPlanIT/StageFreight/src/docker"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/runtime"
)

var dockerCmd = &cobra.Command{
	Use:   "docker",
	Short: "Docker lifecycle — build, drift, reconcile",
	Long:  "Docker lifecycle intelligence and container image management.",
}

var dockerDriftCmd = &cobra.Command{
	Use:   "drift",
	Short: "Show drift status for all Docker compose stacks",
	Long: `Scan IaC, resolve inventory targets, and compute drift for each stack.
Read-only — no mutations. Reuses the same plan model as reconcile.

Examples:
  stagefreight docker drift`,
	RunE: runDockerDrift,
}

func init() {
	dockerCmd.AddCommand(dockerDriftCmd)
	rootCmd.AddCommand(dockerCmd)
}

// runDockerDrift runs the lifecycle through Plan only (dry-run), then renders.
// Read-only: no Execute phase. Same plan model as sf reconcile.
func runDockerDrift(cmd *cobra.Command, args []string) error {
	start := time.Now()
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	if cfg.Lifecycle.Mode != "docker" {
		return fmt.Errorf("lifecycle.mode is %q, not docker", cfg.Lifecycle.Mode)
	}

	ciCtx := ci.ResolveContext()
	rctx := &runtime.RuntimeContext{
		CI:       ciCtx,
		Invoker:  runtime.DetectInvoker(ciCtx),
		RepoRoot: rootDir,
		DryRun:   true, // read-only — Plan only, no Execute
	}

	if err := runtime.RunLifecycle(cmd.Context(), cfg, rctx); err != nil {
		return err
	}

	plan := rctx.Plan()
	w := os.Stdout
	color := output.UseColor()
	docker.RenderPlan(w, plan, time.Since(start), color)

	return nil
}
