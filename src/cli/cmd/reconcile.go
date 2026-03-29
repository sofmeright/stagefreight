package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/ci"
	"github.com/PrPlanIT/StageFreight/src/docker"
	_ "github.com/PrPlanIT/StageFreight/src/docker"  // register compose backend
	_ "github.com/PrPlanIT/StageFreight/src/gitops"   // register flux backend
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/runtime"
)

var (
	reconcileGlobalDry bool
)

var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Reconcile infrastructure to declared state",
	Long: `Universal lifecycle reconciliation trigger.

Reads lifecycle.mode from .stagefreight.yml and dispatches to the
configured backend (flux, compose, etc.). All intelligence lives
in StageFreight — CI and CLI are just transports.

Examples:
  stagefreight reconcile
  stagefreight reconcile --dry-run`,
	RunE: runReconcile,
}

func init() {
	reconcileCmd.Flags().BoolVar(&reconcileGlobalDry, "dry-run", false, "show plan without executing")
	rootCmd.AddCommand(reconcileCmd)
}

func runReconcile(cmd *cobra.Command, args []string) error {
	start := time.Now()
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	mode := cfg.Lifecycle.Mode
	if mode == "" {
		return fmt.Errorf("lifecycle.mode not set in .stagefreight.yml")
	}

	// Build runtime context.
	ciCtx := ci.ResolveContext()
	rctx := &runtime.RuntimeContext{
		CI:       ciCtx,
		Invoker:  runtime.DetectInvoker(ciCtx),
		RepoRoot: rootDir,
		DryRun:   reconcileGlobalDry,
	}

	// RunLifecycle: Resolve → Validate → Prepare → Plan → Execute → Cleanup.
	if err := runtime.RunLifecycle(cmd.Context(), cfg, rctx); err != nil {
		return err
	}

	// Report — dispatch rendering by mode.
	plan := rctx.Plan()
	result := rctx.Result()
	w := os.Stdout
	color := output.UseColor()
	elapsed := time.Since(start)

	switch mode {
	case "gitops":
		renderGitopsPlan(w, plan, result, rctx.DryRun, elapsed, color)
	case "docker":
		if rctx.DryRun || result == nil {
			docker.RenderPlan(w, plan, elapsed, color)
		} else {
			docker.RenderPlan(w, plan, elapsed, color)
			docker.RenderResult(w, plan, result, elapsed, color)
		}
	default:
		return fmt.Errorf("no renderer for lifecycle mode: %q", mode)
	}

	// Check for failures
	if result != nil {
		for _, ar := range result.Actions {
			if !ar.Success {
				failed := 0
				for _, a := range result.Actions {
					if !a.Success {
						failed++
					}
				}
				return fmt.Errorf("%d/%d actions failed", failed, len(result.Actions))
			}
		}
	}

	return nil
}

// renderGitopsPlan renders gitops plan/result using Section output.
// Extracted from gitops.go for reuse by the universal reconcile command.
func renderGitopsPlan(w *os.File, plan *runtime.LifecyclePlan, result *runtime.LifecycleResult, dryRun bool, elapsed time.Duration, color bool) {
	sec := output.NewSection(w, "Reconcile", elapsed, color)

	if plan == nil || len(plan.Actions) == 0 {
		sec.Row("No affected kustomizations — nothing to reconcile.")
		sec.Close()
		return
	}

	succeeded := 0
	failed := 0

	for i, action := range plan.Actions {
		status := "success"
		suffix := ""

		if dryRun {
			suffix = " (dry-run)"
		} else if result != nil && i < len(result.Actions) {
			ar := result.Actions[i]
			if !ar.Success {
				status = "failed"
				failed++
			} else {
				succeeded++
			}
			if ar.Duration > 0 {
				suffix = fmt.Sprintf(" (%s)", ar.Duration.Truncate(100*time.Millisecond))
			}
		} else {
			succeeded++
		}

		label := fmt.Sprintf("[%d/%d] %s", i+1, len(plan.Actions), action.Name)
		output.RowStatus(sec, label, suffix, status, color)

		if !dryRun && result != nil && i < len(result.Actions) && !result.Actions[i].Success && result.Actions[i].Message != "" {
			fmt.Fprintf(w, "    │   %s\n", result.Actions[i].Message)
		}
	}

	sec.Separator()
	if dryRun {
		sec.Row("%d actions planned (dry-run)", len(plan.Actions))
	} else {
		sec.Row("%d/%d succeeded", succeeded, len(plan.Actions))
	}
	sec.Close()
}
