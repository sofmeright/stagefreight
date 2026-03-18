package docker

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/engines"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/version"
)

// planPhase resolves registry targets, tags, platforms, and build strategy.
func planPhase(req Request) pipeline.Phase {
	return pipeline.Phase{
		Name: "plan",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			output.SectionStartCollapsed(pc.Writer, "sf_plan", "Plan")
			planStart := time.Now()

			engine := pc.Scratch["docker.engine"].(build.Engine)
			det := pc.Scratch["docker.det"].(*build.Detection)

			// Apply CLI overrides to builds
			planCfg := *pc.Config
			if req.Target != "" || len(req.Platforms) > 0 || pc.Local {
				builds := make([]config.BuildConfig, len(planCfg.Builds))
				copy(builds, planCfg.Builds)
				for i := range builds {
					if builds[i].Kind != "docker" {
						continue
					}
					if req.BuildID != "" && builds[i].ID != req.BuildID {
						continue
					}
					if req.Target != "" {
						builds[i].Target = req.Target
					}
					if len(req.Platforms) > 0 {
						builds[i].Platforms = req.Platforms
					}
					if pc.Local && len(builds[i].Platforms) == 0 {
						builds[i].Platforms = []string{fmt.Sprintf("linux/%s", runtime.GOARCH)}
					}
				}
				planCfg.Builds = builds
			}

			// Fail-fast guardrail: reject non-docker builds in plan input
			for _, b := range planCfg.Builds {
				if b.Kind != "" && b.Kind != "docker" {
					if req.BuildID != "" && b.ID == req.BuildID && b.Kind != "docker" {
						output.SectionEnd(pc.Writer, "sf_plan")
						return nil, fmt.Errorf("docker plan received non-docker build %q (kind=%s)", b.ID, b.Kind)
					}
				}
			}

			plan, err := engine.Plan(pc.Ctx, &engines.ImagePlanInput{Cfg: &planCfg, BuildID: req.BuildID}, det)
			if err != nil {
				output.SectionEnd(pc.Writer, "sf_plan")
				return nil, fmt.Errorf("planning: %w", err)
			}

			// Apply CLI tag overrides
			if len(req.Tags) > 0 {
				for i := range plan.Steps {
					plan.Steps[i].Tags = append(plan.Steps[i].Tags, req.Tags...)
				}
			}

			// Build strategy:
			//   Single-platform: --load into daemon, then docker push each remote tag.
			//   Multi-platform:  --push directly (can't --load multi-platform in buildx).
			//   --local flag:    force --load, no push regardless.
			for i := range plan.Steps {
				step := &plan.Steps[i]
				if pc.Local {
					step.Load = true
					step.Push = false
					if len(step.Tags) == 0 {
						step.Tags = []string{"stagefreight:dev"}
					}
				} else if len(step.Registries) == 0 {
					step.Load = true
					if len(step.Tags) == 0 {
						step.Tags = []string{"stagefreight:dev"}
					}
				} else if IsMultiPlatform(*step) {
					step.Push = true
				} else {
					step.Load = true
				}
			}

			planElapsed := time.Since(planStart)

			// Plan summary
			tagCount := 0
			var tagNames []string
			for _, s := range plan.Steps {
				tagCount += len(s.Tags)
				tagNames = append(tagNames, s.Tags...)
			}
			step0 := plan.Steps[0]
			var strategy string
			switch {
			case step0.Push:
				strategy = "multi-platform push"
			case step0.Load && hasRemoteRegistries(step0.Registries):
				strategy = "load + push"
			default:
				strategy = "local"
			}

			planSec := output.NewSection(pc.Writer, "Plan", planElapsed, pc.Color)
			planSec.Row("%-16s%s", "builds", fmt.Sprintf("%d", len(plan.Steps)))
			planSec.Row("%-16s%s", "platforms", formatPlatforms(plan.Steps))
			planSec.Row("%-16s%s", "tags", strings.Join(tagNames, ", "))
			planSec.Row("%-16s%s", "strategy", strategy)
			planSec.Close()
			output.SectionEnd(pc.Writer, "sf_plan")

			// Inject standard OCI labels
			buildMode := "standard"
			if build.IsCrucibleChild() {
				buildMode = "crucible-child"
			}
			stdLabels := build.StandardLabels(
				build.NormalizeBuildPlan(plan),
				version.Version,
				version.Commit,
				buildMode,
				"",
			)
			build.InjectLabels(plan, stdLabels)

			pc.BuildPlan = plan

			summary := fmt.Sprintf("%d build(s), %s, %d tag(s), %s", len(plan.Steps), formatPlatforms(plan.Steps), tagCount, strategy)
			return &pipeline.PhaseResult{
				Name:    "plan",
				Status:  "success",
				Summary: summary,
				Elapsed: planElapsed,
			}, nil
		},
	}
}
