package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/postbuild"
)

// Run is the entry point for docker build orchestration.
// It replaces the former runDockerBuild cobra handler body.
func Run(req Request) error {
	if req.Config == nil {
		return fmt.Errorf("docker.Run: config must not be nil")
	}
	if req.Context == nil {
		req.Context = context.Background()
	}
	if req.Stdout == nil {
		req.Stdout = os.Stdout
	}
	if req.Stderr == nil {
		req.Stderr = os.Stderr
	}

	if resolveBuildMode(req) == "crucible" {
		return runCrucibleMode(req)
	}

	// Inject project description for {project.description} templates
	if desc := postbuild.FirstDockerReadmeDescription(req.Config); desc != "" {
		gitver.SetProjectDescription(desc)
	}

	pc := &pipeline.PipelineContext{
		Ctx:           req.Context,
		RootDir:       req.RootDir,
		Config:        req.Config,
		Writer:        req.Stdout,
		Color:         output.UseColor(),
		CI:            output.IsCI(),
		Verbose:       req.Verbose,
		SkipLint:      req.SkipLint,
		DryRun:        req.DryRun,
		Local:         req.Local,
		PipelineStart: time.Now(),
		Scratch:       make(map[string]any),
	}

	p := &pipeline.Pipeline{
		Phases: []pipeline.Phase{
			pipeline.BannerPhase(contextKV),
			pipeline.LintPhase(),
			detectPhase(req),
			planPhase(req),
			pipeline.DryRunGate(renderPlan),
			cleanupPhase(),
			executePhase(req),
			publishPhase(),
			localCacheRetentionPhase(),
		},
		Hooks: []pipeline.PostBuildHook{
			postbuild.BadgeHook(req.Config, func(w io.Writer, color bool, rootDir string) (string, time.Duration) {
				return postbuild.RunBadgeSection(w, color, rootDir, req.Config)
			}),
			postbuild.ReadmeHook(),
			postbuild.RetentionHook(),
			externalCacheRetentionHook(),
		},
	}
	return p.Run(pc)
}

// contextKV returns docker-specific KV pairs for the banner context block.
func contextKV(pc *pipeline.PipelineContext) []output.KV {
	var kv []output.KV

	regTargets := pipeline.CollectTargetsByKind(pc.Config, "registry")
	if len(regTargets) > 0 {
		// Resolve each target through the identity graph so registry-id
		// references surface their URL for the diagnostic header.
		var regNames []string
		seen := make(map[string]bool)
		for _, t := range regTargets {
			reg, err := config.ResolveRegistryForTarget(t, pc.Config.Registries, pc.Config.Vars)
			if err != nil || reg == nil || reg.URL == "" {
				continue
			}
			if !seen[reg.URL] {
				regNames = append(regNames, reg.URL)
				seen[reg.URL] = true
			}
		}
		kv = append(kv, output.KV{Key: "Registries", Value: fmt.Sprintf("%d (%s)", len(regTargets), strings.Join(regNames, ", "))})
	}

	return kv
}
