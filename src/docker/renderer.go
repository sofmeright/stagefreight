package docker

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/runtime"
)

// RenderPlan renders a Docker lifecycle plan to the writer.
// Shows all stacks with drift status, grouped by scope.
func RenderPlan(w io.Writer, plan *runtime.LifecyclePlan, elapsed time.Duration, color bool) {
	sec := output.NewSection(w, "Docker Plan", elapsed, color)

	if len(plan.Actions) == 0 {
		sec.Row("No compose stacks discovered.")
		sec.Close()
		return
	}

	// Group actions by scope
	type scopeGroup struct {
		scope     string
		scopeKind string
		actions   []runtime.PlannedAction
	}
	groups := map[string]*scopeGroup{}
	var order []string

	for _, a := range plan.Actions {
		scope := a.Metadata["scope"]
		if scope == "" {
			scope = "unknown"
		}
		g, ok := groups[scope]
		if !ok {
			g = &scopeGroup{
				scope:     scope,
				scopeKind: a.Metadata["scope_kind"],
			}
			groups[scope] = g
			order = append(order, scope)
		}
		g.actions = append(g.actions, a)
	}

	// Count drifted vs total
	drifted := 0
	total := len(plan.Actions)
	for _, a := range plan.Actions {
		if a.Action != "noop" {
			drifted++
		}
	}

	sec.Row("Stacks: %d total, %d drifted", total, drifted)
	sec.Separator()

	// Render per-scope sections
	for _, scope := range order {
		g := groups[scope]
		kindLabel := "group"
		if g.scopeKind == "host" {
			kindLabel = "host"
		}
		sec.Row("")
		sec.Row("%s (%s)", g.scope, kindLabel)

		for _, a := range g.actions {
			status := "success"
			actionLabel := "noop"
			detail := a.Description

			switch a.Action {
			case "up":
				status = "warning"
				actionLabel = "deploy"
			case "noop":
				status = "success"
				actionLabel = "ok"
				detail = "no drift"
			case "error":
				status = "failed"
				actionLabel = "error"
			}

			label := fmt.Sprintf("  %s", a.Metadata["stack"])
			suffix := ""
			if plan.DryRun {
				suffix = " (dry-run)"
			}

			output.RowStatus(sec, label, fmt.Sprintf(" [%s] %s%s", actionLabel, detail, suffix), status, color)
		}
	}

	sec.Close()
}

// RenderResult renders the execution outcome.
func RenderResult(w io.Writer, plan *runtime.LifecyclePlan, result *runtime.LifecycleResult, elapsed time.Duration, color bool) {
	sec := output.NewSection(w, "Docker Reconcile", elapsed, color)

	if result == nil || len(result.Actions) == 0 {
		sec.Row("No actions executed.")
		sec.Close()
		return
	}

	succeeded := 0
	failed := 0

	for i, ar := range result.Actions {
		status := "success"
		suffix := ""

		if !ar.Success {
			status = "failed"
			failed++
		} else {
			succeeded++
		}

		if ar.Duration > 0 {
			suffix = fmt.Sprintf(" (%s)", ar.Duration.Truncate(100*time.Millisecond))
		}

		// Find the corresponding plan action for scope context
		scope := ""
		stack := ar.Name
		if i < len(plan.Actions) {
			scope = plan.Actions[i].Metadata["scope"]
		}
		if scope != "" {
			stack = scope + "/" + strings.TrimPrefix(ar.Name, scope+"/")
		}

		label := fmt.Sprintf("[%d/%d] %s", i+1, len(result.Actions), stack)
		output.RowStatus(sec, label, suffix, status, color)

		if !ar.Success && ar.Message != "" {
			fmt.Fprintf(w, "    │   %s\n", ar.Message)
		}
	}

	sec.Separator()
	sec.Row("%d/%d succeeded", succeeded, len(result.Actions))
	sec.Close()
}
