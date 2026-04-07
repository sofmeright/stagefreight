package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/trace"
)

// ErrDryRunExit is a sentinel returned by DryRunGate to signal a clean exit.
// Pipeline.Run recognizes this and returns nil after rendering a partial summary.
var ErrDryRunExit = errors.New("dry-run exit")

// ErrContractViolation is returned when one or more emissions were never rendered.
// A contract violation means the UI did not surface all execution truth.
// CI must treat this as a hard failure — exit code 5 by convention.
var ErrContractViolation = errors.New("contract violation: unrendered emissions")

// PipelineContext is the shared state bag threaded through all phases.
type PipelineContext struct {
	Ctx           context.Context
	RootDir       string
	Config        *config.Config
	Writer        io.Writer
	Color         bool
	CI            bool
	Verbose       bool
	SkipLint      bool
	DryRun        bool
	Local         bool
	PipelineStart time.Time
	Manifest      artifact.PublishManifest // accumulated by execute phases
	BuildPlan     *build.BuildPlan      // set by build planning phases when applicable; nil for pipelines with no build plan
	Results       []PhaseResult        // accumulated by pipeline runner

	// Trace is the truth emission collector for this pipeline run.
	// All inputs, decisions, mutations, and side effects emit through it.
	// Panels render from it; enforcement validates completeness at end of run.
	// Never nil — initialized by Pipeline.Run if not set by caller.
	Trace *trace.Collector

	// Scratch is a state bag for command-specific data flowing between phases.
	// Keys are package-scoped conventions (e.g., "binary.steps", "docker.engine").
	// Cross-package pipeline state uses typed fields above, not Scratch.
	Scratch map[string]any
}

// Pipeline orchestrates build phases and hooks.
type Pipeline struct {
	Phases []Phase
	Hooks  []PostBuildHook
}

// Run iterates phases in order.
// On phase error (and StopOnPhaseError): renders partial summary, returns error.
// On ErrDryRunExit: renders partial summary, returns nil (clean exit).
// Then runs hooks conditionally (nil Condition = always run; errors recorded, not fatal).
// Finally renders summary table from pc.Results.
func (p *Pipeline) Run(pc *PipelineContext) error {
	if pc.Writer == nil {
		pc.Writer = os.Stdout
	}
	if pc.Scratch == nil {
		pc.Scratch = make(map[string]any)
	}
	if pc.Trace == nil {
		pc.Trace = trace.NewCollector()
	}

	var phaseErr error
	for _, phase := range p.Phases {
		start := time.Now()
		result, err := phase.Run(pc)
		elapsed := time.Since(start)

		if result != nil {
			if result.Elapsed == 0 {
				result.Elapsed = elapsed
			}
			pc.Results = append(pc.Results, *result)
		} else if err != nil && !errors.Is(err, ErrDryRunExit) {
			// Phase failed without returning a result — synthesize one
			pc.Results = append(pc.Results, PhaseResult{
				Name:    phase.Name,
				Status:  "failed",
				Summary: err.Error(),
				Elapsed: elapsed,
			})
		}

		if err != nil {
			if errors.Is(err, ErrDryRunExit) {
				renderSummary(pc)
				return nil
			}
			phaseErr = err
			break
		}
	}

	if phaseErr != nil {
		renderSummary(pc)
		return phaseErr
	}

	// Run hooks — errors are recorded but not fatal
	for _, hook := range p.Hooks {
		if hook.Condition != nil && !hook.Condition(pc) {
			continue
		}
		start := time.Now()
		result, err := hook.Run(pc)
		elapsed := time.Since(start)

		if result != nil {
			if result.Elapsed == 0 {
				result.Elapsed = elapsed
			}
			pc.Results = append(pc.Results, *result)
		} else if err != nil {
			pc.Results = append(pc.Results, PhaseResult{
				Name:    hook.Name,
				Status:  "failed",
				Summary: fmt.Sprintf("hook error: %v", err),
				Elapsed: elapsed,
			})
		}
		// Hook errors are non-fatal — continue to next hook
	}

	renderSummary(pc)

	// Contract enforcement: any emitted fact that was never rendered is a violation.
	// The Contract panel surfaces what was hidden.
	// ErrContractViolation is returned so the CLI can map it to exit code 5.
	if pc.Trace != nil {
		if unrendered := pc.Trace.Unrendered(); len(unrendered) > 0 {
			RenderContractPanel(pc.Writer, unrendered, pc.Color)
			return fmt.Errorf("%w: %d emission(s) in domains: %s",
				ErrContractViolation, len(unrendered), contractDomains(unrendered))
		}
	}

	return nil
}

// contractDomains returns a comma-separated list of domains with unrendered emissions.
func contractDomains(unrendered []trace.Emission) string {
	seen := make(map[string]bool)
	var domains []string
	for _, e := range unrendered {
		if !seen[e.Domain] {
			seen[e.Domain] = true
			domains = append(domains, e.Domain)
		}
	}
	result := ""
	for i, d := range domains {
		if i > 0 {
			result += ", "
		}
		result += d
	}
	return result
}
