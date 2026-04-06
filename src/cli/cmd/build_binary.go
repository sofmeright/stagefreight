package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	_ "github.com/PrPlanIT/StageFreight/src/build/engines" // register binary engine
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/postbuild"
)

var (
	bbLocal     bool
	bbPlatforms []string
	bbBuildID   string
	bbSkipLint  bool
	bbDryRun    bool
	bbOutputDir string
)

var buildBinaryCmd = &cobra.Command{
	Use:   "binary",
	Short: "Build Go binaries",
	Long: `Build Go binaries for configured platforms.

Compiles Go binaries using go build, cross-compiling for all configured platforms.
Injects version, commit, and build date via ldflags.`,
	RunE: runBuildBinary,
}

func init() {
	buildBinaryCmd.Flags().BoolVar(&bbLocal, "local", false, "build for current platform only")
	buildBinaryCmd.Flags().StringSliceVar(&bbPlatforms, "platform", nil, "override platforms (comma-separated)")
	buildBinaryCmd.Flags().StringVar(&bbBuildID, "build", "", "build specific entry by ID (default: all)")
	buildBinaryCmd.Flags().BoolVar(&bbSkipLint, "skip-lint", false, "skip pre-build lint gate")
	buildBinaryCmd.Flags().BoolVar(&bbDryRun, "dry-run", false, "show plan without executing")
	buildBinaryCmd.Flags().StringVar(&bbOutputDir, "output-dir", "", "override output directory")

	buildCmd.AddCommand(buildBinaryCmd)
}

func runBuildBinary(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	pc := &pipeline.PipelineContext{
		Ctx:           context.Background(),
		RootDir:       rootDir,
		Config:        cfg,
		Writer:        os.Stdout,
		Color:         output.UseColor(),
		CI:            output.IsCI(),
		Verbose:       verbose,
		SkipLint:      bbSkipLint,
		DryRun:        bbDryRun,
		Local:         bbLocal,
		PipelineStart: time.Now(),
		Scratch:       make(map[string]any),
	}

	p := &pipeline.Pipeline{
		Phases: []pipeline.Phase{
			pipeline.BannerPhase(binaryContextKV),
			pipeline.LintPhase(),
			binaryDetectPhase(),
			binaryPlanPhase(),
			pipeline.DryRunGate(renderBinaryPlan),
			binaryExecutePhase(),
			binaryArchivePhase(),
			pipeline.PublishManifestPhase(),
		},
		Hooks: []pipeline.PostBuildHook{
			postbuild.BadgeHook(cfg, cmdBadgeRunner(cfg)),
		},
	}
	return p.Run(pc)
}

// binaryContextKV returns binary-specific KV pairs for the banner context block.
func binaryContextKV(pc *pipeline.PipelineContext) []output.KV {
	var kv []output.KV

	// Count binary builds that will actually run (respects --build filter)
	var count int
	for _, b := range pc.Config.Builds {
		if b.Kind != "binary" {
			continue
		}
		if bbBuildID != "" && b.ID != bbBuildID {
			continue
		}
		count++
	}
	if count > 0 {
		kv = append(kv, output.KV{Key: "Builds", Value: fmt.Sprintf("%d binary", count)})
	}

	return kv
}

// binaryDetectPhase discovers the repo and filters to binary builds.
func binaryDetectPhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "detect",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			output.SectionStartCollapsed(pc.Writer, "sf_detect", "Detect")
			detectStart := time.Now()

			engine, err := build.GetV2("binary")
			if err != nil {
				output.SectionEnd(pc.Writer, "sf_detect")
				return nil, fmt.Errorf("loading binary engine: %w", err)
			}
			pc.Scratch["binary.engine"] = engine

			det, err := engine.Detect(pc.Ctx, pc.RootDir)
			if err != nil {
				output.SectionEnd(pc.Writer, "sf_detect")
				return nil, fmt.Errorf("detection: %w", err)
			}
			pc.Scratch["binary.det"] = det

			versionInfo, _ := build.DetectVersion(pc.RootDir, pc.Config)
			if versionInfo == nil {
				versionInfo = &gitver.VersionInfo{
					Version: "dev",
					Base:    "0.0.0",
					SHA:     "unknown",
					Branch:  "unknown",
				}
			}
			pc.Scratch["binary.version"] = versionInfo

			// Filter builds to kind: binary
			var binaryBuilds []config.BuildConfig
			for _, b := range pc.Config.Builds {
				if b.Kind != "binary" {
					continue
				}
				if bbBuildID != "" && b.ID != bbBuildID {
					continue
				}
				binaryBuilds = append(binaryBuilds, b)
			}

			detectElapsed := time.Since(detectStart)

			detectSec := output.NewSection(pc.Writer, "Detect", detectElapsed, pc.Color)
			detectSec.Row("%-16s→ %s (auto-detected)", "language", det.Language)
			if len(det.MainPackages) > 0 {
				detectSec.Row("%-16s→ %d package(s)", "main", len(det.MainPackages))
			}
			detectSec.Row("%-16s→ %d configured", "builds", len(binaryBuilds))
			detectSec.Close()
			output.SectionEnd(pc.Writer, "sf_detect")

			if len(binaryBuilds) == 0 {
				if bbBuildID != "" {
					return nil, fmt.Errorf("no binary build found with id %q", bbBuildID)
				}
				// Detection is informational only — binary builds require explicit
				// kind: binary entries in .stagefreight.yml. Log detected mains as
				// a hint for the user.
				hint := "no binary builds defined in config"
				if det.Language == "go" && len(det.MainPackages) > 0 {
					hint += fmt.Sprintf(" (detected %d Go main package(s) — add kind: binary builds to .stagefreight.yml)", len(det.MainPackages))
				}
				return nil, fmt.Errorf("%s", hint)
			}

			pc.Scratch["binary.builds"] = binaryBuilds

			summary := fmt.Sprintf("%s, %d build(s)", det.Language, len(binaryBuilds))
			return &pipeline.PhaseResult{
				Name:    "detect",
				Status:  "success",
				Summary: summary,
				Elapsed: detectElapsed,
			}, nil
		},
	}
}

// binaryPlanPhase plans all binary builds with topological ordering.
func binaryPlanPhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "plan",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			output.SectionStartCollapsed(pc.Writer, "sf_plan", "Plan")
			planStart := time.Now()

			binaryBuilds, ok := pc.Scratch["binary.builds"].([]config.BuildConfig)
			if !ok {
				output.SectionEnd(pc.Writer, "sf_plan")
				return nil, fmt.Errorf("missing binary builds in scratch")
			}
			versionInfo, ok := pc.Scratch["binary.version"].(*gitver.VersionInfo)
			if !ok {
				output.SectionEnd(pc.Writer, "sf_plan")
				return nil, fmt.Errorf("missing binary version in scratch")
			}
			engine, ok := pc.Scratch["binary.engine"].(build.EngineV2)
			if !ok {
				output.SectionEnd(pc.Writer, "sf_plan")
				return nil, fmt.Errorf("missing binary engine in scratch")
			}

			// Fail-fast guardrail: reject non-binary builds
			for _, b := range binaryBuilds {
				if b.Kind != "binary" {
					output.SectionEnd(pc.Writer, "sf_plan")
					return nil, fmt.Errorf("binary plan received non-binary build %q (kind=%s)", b.ID, b.Kind)
				}
			}

			// Topological sort for depends_on ordering
			ordered, err := build.BuildOrder(binaryBuilds)
			if err != nil {
				output.SectionEnd(pc.Writer, "sf_plan")
				return nil, fmt.Errorf("build ordering: %w", err)
			}

			// Plan all builds
			var allSteps []build.UniversalStep
			for _, b := range ordered {
				buildCfg := toBuildConfig(b, versionInfo)

				// CLI overrides
				if pc.Local {
					buildCfg.Platforms = []build.Platform{
						{OS: runtime.GOOS, Arch: runtime.GOARCH},
					}
				}
				if len(bbPlatforms) > 0 {
					buildCfg.Platforms = parsePlatformFlags(bbPlatforms)
				}

				steps, err := engine.Plan(pc.Ctx, buildCfg)
				if err != nil {
					output.SectionEnd(pc.Writer, "sf_plan")
					return nil, fmt.Errorf("planning build %q: %w", b.ID, err)
				}
				allSteps = append(allSteps, steps...)
			}

			// Validate build graph
			if err := build.ValidateBuildGraph(allSteps); err != nil {
				output.SectionEnd(pc.Writer, "sf_plan")
				return nil, fmt.Errorf("build graph validation: %w", err)
			}

			pc.Scratch["binary.steps"] = allSteps

			planElapsed := time.Since(planStart)

			// Collect unique platforms
			seen := make(map[string]bool)
			var platforms []string
			for _, s := range allSteps {
				p := s.Platform.OS + "/" + s.Platform.Arch
				if !seen[p] {
					seen[p] = true
					platforms = append(platforms, p)
				}
			}

			planSec := output.NewSection(pc.Writer, "Plan", planElapsed, pc.Color)
			planSec.Row("%-16s%s", "steps", fmt.Sprintf("%d", len(allSteps)))
			planSec.Row("%-16s%s", "platforms", strings.Join(platforms, ", "))
			planSec.Row("%-16s%s", "version", versionInfo.Version)
			planSec.Close()
			output.SectionEnd(pc.Writer, "sf_plan")

			summary := fmt.Sprintf("%d step(s), %s", len(allSteps), strings.Join(platforms, ","))
			return &pipeline.PhaseResult{
				Name:    "plan",
				Status:  "success",
				Summary: summary,
				Elapsed: planElapsed,
			}, nil
		},
	}
}

// renderBinaryPlan renders the dry-run plan output for binary builds.
func renderBinaryPlan(pc *pipeline.PipelineContext) {
	allSteps, ok := pc.Scratch["binary.steps"].([]build.UniversalStep)
	if !ok {
		return
	}
	fmt.Fprintf(pc.Writer, "Binary build plan (%d steps):\n", len(allSteps))
	for _, s := range allSteps {
		fmt.Fprintf(pc.Writer, "  %-30s  %s/%s  → %s\n",
			s.StepID, s.Platform.OS, s.Platform.Arch,
			formatOutputs(s.Outputs))
	}
}

// binaryExecutePhase compiles all planned binary steps.
func binaryExecutePhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "build",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			allSteps, ok := pc.Scratch["binary.steps"].([]build.UniversalStep)
			if !ok {
				return nil, fmt.Errorf("missing binary steps in scratch")
			}
			versionInfo, ok := pc.Scratch["binary.version"].(*gitver.VersionInfo)
			if !ok {
				return nil, fmt.Errorf("missing binary version in scratch")
			}
			engine, ok := pc.Scratch["binary.engine"].(build.EngineV2)
			if !ok {
				return nil, fmt.Errorf("missing binary engine in scratch")
			}

			output.SectionStart(pc.Writer, "sf_build", "Build")
			buildStart := time.Now()

			var publishedBinaries []artifact.PublishedBinary

			buildSec := output.NewSection(pc.Writer, "Build", 0, pc.Color)
			for _, step := range allSteps {
				result, err := engine.ExecuteStep(pc.Ctx, step)
				if err != nil {
					buildSec.Row("%-30s  %s/%s  %s", step.StepID, step.Platform.OS, step.Platform.Arch,
						output.StatusIcon("failed", pc.Color))
					buildSec.Close()
					output.SectionEnd(pc.Writer, "sf_build")
					return &pipeline.PhaseResult{
						Name:    "build",
						Status:  "failed",
						Summary: fmt.Sprintf("step %s: %v", step.StepID, err),
						Elapsed: time.Since(buildStart),
					}, fmt.Errorf("step %s: %w", step.StepID, err)
				}

				buildSec.Row("%-30s  %s/%s  %s  (%.1fs)", step.StepID,
					step.Platform.OS, step.Platform.Arch,
					output.StatusIcon("success", pc.Color),
					result.Metrics.Duration.Seconds())

				for _, out := range result.Artifacts {
					publishedBinaries = append(publishedBinaries, artifact.PublishedBinary{
						Name:      result.Metadata["binary_name"],
						OS:        step.Platform.OS,
						Arch:      step.Platform.Arch,
						Path:      out.Path,
						Size:      out.Size,
						SHA256:    out.SHA256,
						BuildID:   step.BuildID,
						Version:   versionInfo.Version,
						Commit:    versionInfo.SHA,
						Toolchain: result.Metadata["toolchain"],
					})
				}
			}
			buildElapsed := time.Since(buildStart)
			buildSec.Close()
			output.SectionEnd(pc.Writer, "sf_build")

			pc.Scratch["binary.published"] = publishedBinaries
			pc.Manifest.Binaries = append(pc.Manifest.Binaries, publishedBinaries...)

			summary := fmt.Sprintf("%d binary(ies)", len(publishedBinaries))
			return &pipeline.PhaseResult{
				Name:    "build",
				Status:  "success",
				Summary: summary,
				Elapsed: buildElapsed,
			}, nil
		},
	}
}

// binaryArchivePhase creates archives for configured binary-archive targets.
func binaryArchivePhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "archive",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			publishedBinaries, ok := pc.Scratch["binary.published"].([]artifact.PublishedBinary)
			if !ok {
				return nil, fmt.Errorf("missing binary.published in scratch")
			}
			versionInfo, ok := pc.Scratch["binary.version"].(*gitver.VersionInfo)
			if !ok {
				return nil, fmt.Errorf("missing binary version in scratch")
			}

			archiveTargets := pipeline.CollectTargetsByKind(pc.Config, "binary-archive")
			if len(archiveTargets) == 0 {
				return &pipeline.PhaseResult{
					Name:    "archive",
					Status:  "skipped",
					Summary: "no archive targets",
				}, nil
			}

			output.SectionStartCollapsed(pc.Writer, "sf_archive", "Archive")
			archiveStart := time.Now()

			var allArchives []artifact.PublishedArchive
			archiveSec := output.NewSection(pc.Writer, "Archive", 0, pc.Color)

			for _, t := range archiveTargets {
				// Track archives created for this target only (checksums are per-target)
				var targetArchives []artifact.PublishedArchive

				for _, pb := range publishedBinaries {
					if t.Build != pb.BuildID {
						continue
					}

					archiveBinaryName := t.BinaryName
					if archiveBinaryName == "" {
						archiveBinaryName = pb.Name
					}

					nameTemplate := t.Name
					if nameTemplate == "" {
						nameTemplate = "{id}-{version}-{os}-{arch}"
					}

					archResult, err := build.CreateArchive(build.ArchiveOpts{
						Format:       t.Format,
						OutputDir:    filepath.Join(pc.RootDir, "dist"),
						NameTemplate: nameTemplate,
						BinaryPath:   pb.Path,
						BinaryName:   archiveBinaryName,
						IncludeFiles: t.Include,
						RepoRoot:     pc.RootDir,
						Platform:     build.Platform{OS: pb.OS, Arch: pb.Arch},
						BuildID:      pb.BuildID,
						Version:      versionInfo,
					})
					if err != nil {
						archiveSec.Close()
						output.SectionEnd(pc.Writer, "sf_archive")
						return nil, fmt.Errorf("archive for %s/%s: %w", pb.OS, pb.Arch, err)
					}

					archiveSec.Row("%-40s %s  (%s, %d bytes)",
						filepath.Base(archResult.Path),
						output.StatusIcon("success", pc.Color),
						archResult.Format, archResult.Size)

					targetArchives = append(targetArchives, artifact.PublishedArchive{
						Name:     filepath.Base(archResult.Path),
						Format:   archResult.Format,
						Path:     archResult.Path,
						Size:     archResult.Size,
						SHA256:   archResult.SHA256,
						Contents: archResult.Contents,
						BuildID:  pb.BuildID,
						Binary:   pb,
					})
				}

				// Write checksums scoped to this target's archives only
				if t.Checksums && len(targetArchives) > 0 {
					var archiveResults []*build.ArchiveResult
					for _, pa := range targetArchives {
						archiveResults = append(archiveResults, &build.ArchiveResult{
							Path:   pa.Path,
							SHA256: pa.SHA256,
						})
					}
					checksumPath, err := build.WriteChecksums(filepath.Join(pc.RootDir, "dist"), archiveResults)
					if err != nil {
						archiveSec.Close()
						output.SectionEnd(pc.Writer, "sf_archive")
						return nil, fmt.Errorf("writing checksums: %w", err)
					}
					archiveSec.Row("%-40s %s  checksums", filepath.Base(checksumPath),
						output.StatusIcon("success", pc.Color))
				}

				allArchives = append(allArchives, targetArchives...)
			}

			archiveElapsed := time.Since(archiveStart)
			archiveSec.Close()
			output.SectionEnd(pc.Writer, "sf_archive")

			pc.Manifest.Archives = append(pc.Manifest.Archives, allArchives...)

			summary := fmt.Sprintf("%d archive(s)", len(allArchives))
			return &pipeline.PhaseResult{
				Name:    "archive",
				Status:  "success",
				Summary: summary,
				Elapsed: archiveElapsed,
			}, nil
		},
	}
}

// toBuildConfig converts a config.BuildConfig to a build.BuildConfig for the engine.
func toBuildConfig(b config.BuildConfig, v *gitver.VersionInfo) build.BuildConfig {
	platforms := build.ParsePlatforms(b.Platforms)
	if len(platforms) == 0 {
		platforms = []build.Platform{
			{OS: runtime.GOOS, Arch: runtime.GOARCH},
		}
	}

	return build.BuildConfig{
		ID:         b.ID,
		Kind:       b.Kind,
		Platforms:  platforms,
		BuildMode:  b.BuildMode,
		SelectTags: b.SelectTags,
		DependsOn:  b.DependsOn,
		Version:    v,
		Builder:    b.Builder,
		Command:    b.BuilderCommand(),
		From:       b.From,
		Output:     b.OutputName(),
		Args:       b.Args,
		Env:        b.Env,
		Compress:   b.Compress,
	}
}

// parsePlatformFlags converts CLI platform strings to Platform structs.
func parsePlatformFlags(flags []string) []build.Platform {
	var platforms []build.Platform
	for _, f := range flags {
		for _, p := range strings.Split(f, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				platforms = append(platforms, build.ParsePlatform(p))
			}
		}
	}
	return platforms
}

func formatOutputs(refs []build.ArtifactRef) string {
	var paths []string
	for _, r := range refs {
		paths = append(paths, r.Path)
	}
	return strings.Join(paths, ", ")
}

// cmdBadgeRunner returns a postbuild.BadgeRunner that uses cmd-local badge helpers.
func cmdBadgeRunner(appCfg *config.Config) postbuild.BadgeRunner {
	return func(w io.Writer, color bool, rootDir string) (string, time.Duration) {
		start := time.Now()
		err := RunConfigBadges(appCfg, rootDir, nil, "passed")
		elapsed := time.Since(start)
		if err != nil {
			return fmt.Sprintf("error: %v", err), elapsed
		}
		items := postbuild.CollectNarratorBadgeItems(appCfg)
		return fmt.Sprintf("%d generated", len(items)), elapsed
	}
}
