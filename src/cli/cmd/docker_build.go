package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/PrPlanIT/StageFreight/src/badge"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/engines"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/publish"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/registry"
	"github.com/PrPlanIT/StageFreight/src/version"
)

var (
	dbLocal     bool
	dbPlatforms []string
	dbTags      []string
	dbTarget    string
	dbBuildID   string
	dbSkipLint  bool
	dbDryRun    bool
	dbBuildMode string
)

var dockerBuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build and push container images",
	Long: `Build container images using docker buildx.

Detects Dockerfiles, resolves tags from git, and pushes to configured registries.
Runs lint as a pre-build gate unless --skip-lint is set.`,
	RunE: runDockerBuild,
}

func init() {
	dockerBuildCmd.Flags().BoolVar(&dbLocal, "local", false, "build for current platform, load into daemon")
	dockerBuildCmd.Flags().StringSliceVar(&dbPlatforms, "platform", nil, "override platforms (comma-separated)")
	dockerBuildCmd.Flags().StringSliceVar(&dbTags, "tag", nil, "override/add tags")
	dockerBuildCmd.Flags().StringVar(&dbTarget, "target", "", "override Dockerfile target stage")
	dockerBuildCmd.Flags().StringVar(&dbBuildID, "build", "", "build a specific entry by ID (default: all)")
	dockerBuildCmd.Flags().BoolVar(&dbSkipLint, "skip-lint", false, "skip pre-build lint")
	dockerBuildCmd.Flags().BoolVar(&dbDryRun, "dry-run", false, "show the plan without executing")
	dockerBuildCmd.Flags().StringVar(&dbBuildMode, "build-mode", "", "build execution strategy: crucible (self-proving self-build)")

	dockerCmd.AddCommand(dockerBuildCmd)
}

func runDockerBuild(cmd *cobra.Command, args []string) error {
	if resolveBuildMode() == "crucible" {
		return runCrucibleMode(cmd, args)
	}

	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	if len(args) > 0 {
		rootDir = args[0]
	}

	// Inject project description from docker-readme targets for {project.description} templates
	if desc := firstDockerReadmeDescription(cfg); desc != "" {
		gitver.SetProjectDescription(desc)
	}

	pc := &pipeline.PipelineContext{
		Ctx:           context.Background(),
		RootDir:       rootDir,
		Config:        cfg,
		Writer:        os.Stdout,
		Color:         output.UseColor(),
		CI:            output.IsCI(),
		Verbose:       verbose,
		SkipLint:      dbSkipLint,
		DryRun:        dbDryRun,
		Local:         dbLocal,
		PipelineStart: time.Now(),
		Scratch:       make(map[string]any),
	}

	// Register badge runner for BadgeHook
	pc.Scratch["badge.run"] = runBadgeSection

	p := &pipeline.Pipeline{
		Phases: []pipeline.Phase{
			pipeline.BannerPhase(dockerContextKV),
			pipeline.LintPhase(),
			dockerDetectPhase(),
			dockerPlanPhase(),
			pipeline.DryRunGate(renderDockerPlan),
			dockerExecutePhase(),   // build + push + sign
			dockerPublishPhase(),   // write publish manifest (docker-specific, handles its own merge)
		},
		Hooks: []pipeline.PostBuildHook{
			pipeline.BadgeHook(cfg),
			dockerReadmeHook(),
			dockerRetentionHook(),
		},
	}
	return p.Run(pc)
}

// dockerContextKV returns docker-specific KV pairs for the banner context block.
func dockerContextKV(pc *pipeline.PipelineContext) []output.KV {
	var kv []output.KV

	// Count configured registry targets
	regTargets := pipeline.CollectTargetsByKind(pc.Config, "registry")
	if len(regTargets) > 0 {
		var regNames []string
		seen := make(map[string]bool)
		for _, t := range regTargets {
			if !seen[t.URL] {
				regNames = append(regNames, t.URL)
				seen[t.URL] = true
			}
		}
		kv = append(kv, output.KV{Key: "Registries", Value: fmt.Sprintf("%d (%s)", len(regTargets), strings.Join(regNames, ", "))})
	}

	return kv
}

// dockerDetectPhase discovers Dockerfiles and the repo language.
func dockerDetectPhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "detect",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			output.SectionStartCollapsed(pc.Writer, "sf_detect", "Detect")
			detectStart := time.Now()

			engine, err := build.Get("image")
			if err != nil {
				output.SectionEnd(pc.Writer, "sf_detect")
				return nil, err
			}
			pc.Scratch["docker.engine"] = engine

			det, err := engine.Detect(pc.Ctx, pc.RootDir)
			if err != nil {
				output.SectionEnd(pc.Writer, "sf_detect")
				return nil, fmt.Errorf("detection: %w", err)
			}
			pc.Scratch["docker.det"] = det

			detectElapsed := time.Since(detectStart)

			detectSec := output.NewSection(pc.Writer, "Detect", detectElapsed, pc.Color)
			for _, df := range det.Dockerfiles {
				detectSec.Row("%-16s→ %s", "Dockerfile", df.Path)
			}
			detectSec.Row("%-16s→ %s (auto-detected)", "language", det.Language)
			detectSec.Row("%-16s→ %s", "context", ".")
			if dbTarget != "" {
				detectSec.Row("%-16s→ %s", "target", dbTarget)
			} else {
				detectSec.Row("%-16s→ %s", "target", "(default)")
			}
			detectSec.Close()
			output.SectionEnd(pc.Writer, "sf_detect")

			summary := fmt.Sprintf("%d Dockerfile(s), %s", len(det.Dockerfiles), det.Language)
			return &pipeline.PhaseResult{
				Name:    "detect",
				Status:  "success",
				Summary: summary,
				Elapsed: detectElapsed,
			}, nil
		},
	}
}

// dockerPlanPhase resolves registry targets, tags, platforms, and build strategy.
func dockerPlanPhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "plan",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			output.SectionStartCollapsed(pc.Writer, "sf_plan", "Plan")
			planStart := time.Now()

			engine := pc.Scratch["docker.engine"].(build.Engine)
			det := pc.Scratch["docker.det"].(*build.Detection)

			// Apply CLI overrides to builds
			planCfg := *pc.Config
			if dbTarget != "" || len(dbPlatforms) > 0 || pc.Local {
				builds := make([]config.BuildConfig, len(planCfg.Builds))
				copy(builds, planCfg.Builds)
				for i := range builds {
					if builds[i].Kind != "docker" {
						continue
					}
					if dbBuildID != "" && builds[i].ID != dbBuildID {
						continue
					}
					if dbTarget != "" {
						builds[i].Target = dbTarget
					}
					if len(dbPlatforms) > 0 {
						builds[i].Platforms = dbPlatforms
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
					// Non-docker builds are fine — engine.Plan filters to docker only.
					// But if someone explicitly passes a non-docker build ID, catch it.
					if dbBuildID != "" && b.ID == dbBuildID && b.Kind != "docker" {
						output.SectionEnd(pc.Writer, "sf_plan")
						return nil, fmt.Errorf("docker plan received non-docker build %q (kind=%s)", b.ID, b.Kind)
					}
				}
			}

			plan, err := engine.Plan(pc.Ctx, &engines.ImagePlanInput{Cfg: &planCfg, BuildID: dbBuildID}, det)
			if err != nil {
				output.SectionEnd(pc.Writer, "sf_plan")
				return nil, fmt.Errorf("planning: %w", err)
			}

			// Apply CLI tag overrides
			if len(dbTags) > 0 {
				for i := range plan.Steps {
					plan.Steps[i].Tags = append(plan.Steps[i].Tags, dbTags...)
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
				} else if build.IsMultiPlatform(*step) {
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

			pc.Scratch["docker.plan"] = plan

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

// renderDockerPlan renders the dry-run plan output for docker builds.
func renderDockerPlan(pc *pipeline.PipelineContext) {
	plan := pc.Scratch["docker.plan"].(*build.BuildPlan)
	for _, step := range plan.Steps {
		fmt.Fprintf(pc.Writer, "step: %s\n", step.Name)
		fmt.Fprintf(pc.Writer, "  dockerfile: %s\n", step.Dockerfile)
		fmt.Fprintf(pc.Writer, "  context:    %s\n", step.Context)
		fmt.Fprintf(pc.Writer, "  target:     %s\n", step.Target)
		fmt.Fprintf(pc.Writer, "  platforms:  %v\n", step.Platforms)
		fmt.Fprintf(pc.Writer, "  tags:       %v\n", step.Tags)
		fmt.Fprintf(pc.Writer, "  load:       %v\n", step.Load)
		fmt.Fprintf(pc.Writer, "  push:       %v\n", step.Push)
		if len(step.BuildArgs) > 0 {
			fmt.Fprintf(pc.Writer, "  build_args: %v\n", step.BuildArgs)
		}
	}
}

// dockerExecutePhase builds images via buildx, pushes, and signs.
// Build + push + sign are kept in one phase because they share buildx state,
// publish manifest accumulation, and deferred metadata file cleanup.
func dockerExecutePhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "build",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			plan := pc.Scratch["docker.plan"].(*build.BuildPlan)

			// Publish manifest tracking
			var publishManifest build.PublishManifest
			var publishModeUsed bool

			buildInst := build.BuildInstance{
				Commit:     os.Getenv("CI_COMMIT_SHA"),
				PipelineID: os.Getenv("CI_PIPELINE_ID"),
				JobID:      os.Getenv("CI_JOB_ID"),
				CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			}

			output.SectionStart(pc.Writer, "sf_build", "Build")
			buildStart := time.Now()

			// Always capture output for structured display; verbose forwards stderr in real-time
			bx := build.NewBuildx(pc.Verbose)
			var stderrBuf bytes.Buffer
			bx.Stdout = io.Discard
			if pc.Verbose {
				bx.Stderr = os.Stderr
			} else {
				bx.Stderr = &stderrBuf
			}

			// Login to remote registries
			for _, step := range plan.Steps {
				if hasRemoteRegistries(step.Registries) {
					loginBx := *bx
					loginBx.Stdout = io.Discard
					loginBx.Stderr = io.Discard
					if err := loginBx.Login(pc.Ctx, step.Registries); err != nil {
						output.SectionEnd(pc.Writer, "sf_build")
						return nil, err
					}
					if err := publish.EnsureHarborProjects(pc.Ctx, step.Registries); err != nil {
						output.SectionEnd(pc.Writer, "sf_build")
						return nil, err
					}
					break
				}
			}

			// Set up metadata files for digest capture on push builds
			var metadataCleanup []string
			for i := range plan.Steps {
				if plan.Steps[i].Push {
					metaFile, tmpErr := os.CreateTemp("", "buildx-metadata-*.json")
					if tmpErr == nil {
						plan.Steps[i].MetadataFile = metaFile.Name()
						metaFile.Close()
						metadataCleanup = append(metadataCleanup, metaFile.Name())
					}
				}
			}
			defer func() {
				for _, f := range metadataCleanup {
					os.Remove(f)
				}
			}()

			// Build each step
			var result build.BuildResult
			for _, step := range plan.Steps {
				stepResult, layers, err := bx.BuildWithLayers(pc.Ctx, step)
				if stepResult == nil {
					stepResult = &build.StepResult{Name: step.Name, Status: "failed"}
				}
				stepResult.Layers = layers

				result.Steps = append(result.Steps, *stepResult)
				if err != nil {
					buildElapsed := time.Since(buildStart)
					failSec := output.NewSection(pc.Writer, "Build", buildElapsed, pc.Color)
					renderBuildLayers(failSec, result.Steps, pc.Color)
					output.RowStatus(failSec, "status", "build failed", "failed", pc.Color)
					failSec.Close()

					if pc.CI {
						output.SectionStartCollapsed(pc.Writer, "sf_build_raw", "Build Output (raw)")
						fmt.Fprint(pc.Writer, stderrBuf.String())
						output.SectionEnd(pc.Writer, "sf_build_raw")
					} else if pc.Verbose {
						fmt.Fprint(os.Stderr, stderrBuf.String())
					}

					output.SectionEnd(pc.Writer, "sf_build")
					return &pipeline.PhaseResult{
						Name:    "build",
						Status:  "failed",
						Summary: "build failed",
						Elapsed: buildElapsed,
					}, err
				}
			}
			buildElapsed := time.Since(buildStart)

			// Trigger Harbor scans after multi-platform push
			for _, step := range plan.Steps {
				if step.Push {
					publish.TriggerHarborScans(pc.Ctx, step.Registries)
				}
			}

			// Record multi-platform pushes (step.Push = true → buildx --push)
			for _, step := range plan.Steps {
				if !step.Push {
					continue
				}
				publishModeUsed = true

				var capturedDigest string
				if step.MetadataFile != "" {
					for attempt := 0; attempt < 3; attempt++ {
						if d, mErr := build.ParseMetadataDigest(step.MetadataFile); mErr == nil {
							capturedDigest = d
							break
						} else if attempt == 2 {
							diag.Warn("could not parse buildx metadata digest: %v", mErr)
						}
						time.Sleep(200 * time.Millisecond)
					}
				}

				for _, reg := range step.Registries {
					if reg.Provider == "local" {
						continue
					}
					host := registry.NormalizeHost(reg.URL)
					provider := reg.Provider
					if p, err := registry.CanonicalProvider(provider); err == nil {
						provider = p
					}

					allTags := make([]string, len(reg.Tags))
					copy(allTags, reg.Tags)

					for _, tag := range reg.Tags {
						ref := host + "/" + reg.Path + ":" + tag

						var observedBuildx string
						for i := 0; i < 3; i++ {
							d, rErr := build.ResolveDigest(pc.Ctx, ref)
							if rErr == nil {
								observedBuildx = d
								break
							}
							time.Sleep(time.Second)
						}

						var observedAPI string
						apiDigest, apiErr := registry.CheckManifestDigest(pc.Ctx, host, reg.Path, tag, nil, reg.Credentials)
						if apiErr == nil {
							observedAPI = apiDigest
						}

						if observedBuildx != "" && observedAPI != "" && observedBuildx != observedAPI {
							diag.Warn("registry inconsistency: buildx saw %s, registry API saw %s — possible shadow write", observedBuildx, observedAPI)
						}
						if capturedDigest != "" && observedBuildx != "" && capturedDigest != observedBuildx {
							diag.Warn("registry propagation lag: expected %s, registry served %s", capturedDigest, observedBuildx)
						}

						publishManifest.Published = append(publishManifest.Published, build.PublishedImage{
							Host:              host,
							Path:              reg.Path,
							Tag:               tag,
							Ref:               ref,
							Provider:          provider,
							CredentialRef:     reg.Credentials,
							BuildInstance:     buildInst,
							Digest:            capturedDigest,
							Registry:          host,
							ObservedDigest:    observedBuildx,
							ObservedDigestAlt: observedAPI,
							ObservedBy:        "buildx",
							ObservedByAlt:     "registry_api",
							ExpectedTags:      allTags,
							ExpectedCommit:    buildInst.Commit,
						})
					}
				}
			}

			// Build section output
			buildSec := output.NewSection(pc.Writer, "Build", buildElapsed, pc.Color)
			if renderBuildLayers(buildSec, result.Steps, pc.Color) {
				buildSec.Separator()
			}

			var buildImageCount int
			for _, sr := range result.Steps {
				for _, img := range sr.Images {
					buildSec.Row("result  %-40s", img)
					buildImageCount++
				}
			}
			buildSec.Close()
			output.SectionEnd(pc.Writer, "sf_build")

			// --- Push (single-platform load-then-push) ---
			remoteTags := collectRemoteTags(plan)
			var pushSummary string
			if len(remoteTags) > 0 {
				output.SectionStart(pc.Writer, "sf_push", "Push")
				pushStart := time.Now()

				pushBx := *bx
				pushBx.Stdout = io.Discard
				if pc.Verbose {
					pushBx.Stderr = os.Stderr
				} else {
					pushBx.Stderr = io.Discard
				}
				if err := pushBx.PushTags(pc.Ctx, remoteTags); err != nil {
					output.SectionEnd(pc.Writer, "sf_push")
					return nil, err
				}

				// Trigger Harbor scans after single-platform push
				for _, step := range plan.Steps {
					if step.Load && !step.Push {
						publish.TriggerHarborScans(pc.Ctx, step.Registries)
					}
				}

				pushElapsed := time.Since(pushStart)
				pushSec := output.NewSection(pc.Writer, "Push", pushElapsed, pc.Color)
				for _, tag := range remoteTags {
					pushSec.Row("%s  %s", output.StatusIcon("success", pc.Color), tag)
				}
				pushSec.Close()

				regSet := make(map[string]bool)
				for _, tag := range remoteTags {
					parts := strings.SplitN(tag, "/", 2)
					if len(parts) > 0 {
						regSet[parts[0]] = true
					}
				}
				pushSummary = fmt.Sprintf("%d tag(s) → %d registry", len(remoteTags), len(regSet))
				output.SectionEnd(pc.Writer, "sf_push")

				// Record single-platform pushes
				publishModeUsed = true
				for _, step := range plan.Steps {
					if !step.Load || step.Push {
						continue
					}
					for _, reg := range step.Registries {
						if reg.Provider == "local" {
							continue
						}
						host := registry.NormalizeHost(reg.URL)
						provider := reg.Provider
						if p, err := registry.CanonicalProvider(provider); err == nil {
							provider = p
						}

						allTags := make([]string, len(reg.Tags))
						copy(allTags, reg.Tags)

						for _, tag := range reg.Tags {
							ref := host + "/" + reg.Path + ":" + tag

							var capturedDigest string
							for i := 0; i < 6; i++ {
								d, rErr := build.ResolveDigest(pc.Ctx, ref)
								if rErr == nil {
									capturedDigest = d
									break
								}
								if i == 5 {
									diag.Warn("could not resolve digest for %s via registry after push: %v", ref, rErr)
								}
								time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
							}

							if capturedDigest == "" {
								if d, lErr := build.ResolveLocalDigest(pc.Ctx, ref); lErr == nil {
									capturedDigest = d
									diag.Info("publish: resolved digest via local RepoDigests fallback for %s", ref)
								}
							}

							if capturedDigest == "" {
								diag.Warn("published %s with no immutable digest — security will fall back to tag-based scanning", ref)
							}

							var observedAPI string
							apiDigest, apiErr := registry.CheckManifestDigest(pc.Ctx, host, reg.Path, tag, nil, reg.Credentials)
							if apiErr == nil {
								observedAPI = apiDigest
							}

							if capturedDigest != "" && observedAPI != "" && capturedDigest != observedAPI {
								diag.Warn("registry inconsistency: buildx saw %s, registry API saw %s — possible shadow write", capturedDigest, observedAPI)
							}

							publishManifest.Published = append(publishManifest.Published, build.PublishedImage{
								Host:              host,
								Path:              reg.Path,
								Tag:               tag,
								Ref:               ref,
								Provider:          provider,
								CredentialRef:     reg.Credentials,
								BuildInstance:     buildInst,
								Digest:            capturedDigest,
								Registry:          host,
								ObservedDigest:    capturedDigest,
								ObservedDigestAlt: observedAPI,
								ObservedBy:        "buildx",
								ObservedByAlt:     "registry_api",
								ExpectedTags:      allTags,
								ExpectedCommit:    buildInst.Commit,
							})
						}
					}
				}
			}

			// --- Cosign signing (best-effort) ---
			if publishModeUsed {
				cosignKey := build.ResolveCosignKey()
				cosignOnPath := build.CosignAvailable()
				signingAttempted := cosignOnPath && cosignKey != ""

				if signingAttempted {
					for i, img := range publishManifest.Published {
						if img.Digest == "" {
							continue
						}
						digestRef := img.Host + "/" + img.Path + "@" + img.Digest
						multiArch := false
						for _, step := range plan.Steps {
							if step.Push && len(step.Platforms) > 1 {
								multiArch = true
								break
							}
						}

						dssePath := filepath.Join(pc.RootDir, ".stagefreight", "provenance.dsse.json")
						if _, statErr := os.Stat(filepath.Join(pc.RootDir, ".stagefreight", "provenance.json")); statErr == nil {
							provenanceData, readErr := os.ReadFile(filepath.Join(pc.RootDir, ".stagefreight", "provenance.json"))
							if readErr == nil {
								var stmt build.ProvenanceStatement
								if jsonErr := json.Unmarshal(provenanceData, &stmt); jsonErr == nil {
									_ = build.WriteDSSEProvenance(dssePath, stmt)
								}
							}
						}

						signErr := build.CosignSign(pc.Ctx, digestRef, cosignKey, multiArch)

						if _, statErr := os.Stat(dssePath); statErr == nil {
							_ = build.CosignAttest(pc.Ctx, digestRef, dssePath, cosignKey)
						}

						if signErr != nil {
							publishManifest.Published[i].SigningAttempted = true
						} else {
							artifacts, _ := registry.DiscoverArtifacts(pc.Ctx, img, nil)
							publishManifest.Published[i].Attestation = &build.AttestationRecord{
								Type:           build.AttestationCosign,
								SignatureRef:   artifacts.Signature,
								AttestationRef: artifacts.Provenance,
								VerifiedDigest: img.Digest,
							}
						}
					}
				} else {
					diag.Debug(pc.Verbose, "cosign: not configured, skipping signing (cosign on PATH: %v, key available: %v)", cosignOnPath, cosignKey != "")
				}
			}

			// Store publish manifest and build result in Scratch for downstream phases
			pc.Scratch["docker.publishManifest"] = &publishManifest
			pc.Scratch["docker.publishModeUsed"] = publishModeUsed
			pc.Scratch["docker.buildResult"] = &result

			buildSummary := fmt.Sprintf("%d image(s)", buildImageCount)
			if pushSummary != "" {
				buildSummary += ", " + pushSummary
			}

			return &pipeline.PhaseResult{
				Name:    "build",
				Status:  "success",
				Summary: buildSummary,
				Elapsed: buildElapsed,
			}, nil
		},
	}
}

// dockerPublishPhase writes the docker publish manifest.
// Separate from the generic PublishManifestPhase because docker builds manage their own
// manifest accumulation (multi-platform digests, signing records, etc.).
func dockerPublishPhase() pipeline.Phase {
	return pipeline.Phase{
		Name: "publish",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			publishModeUsed, _ := pc.Scratch["docker.publishModeUsed"].(bool)
			if !publishModeUsed {
				return &pipeline.PhaseResult{
					Name:    "publish",
					Status:  "skipped",
					Summary: "no artifacts published",
				}, nil
			}

			publishManifest := pc.Scratch["docker.publishManifest"].(*build.PublishManifest)
			if err := build.WritePublishManifest(pc.RootDir, *publishManifest); err != nil {
				return nil, fmt.Errorf("writing publish manifest: %w", err)
			}

			// Also print image references
			if result, ok := pc.Scratch["docker.buildResult"].(*build.BuildResult); ok {
				fmt.Fprintf(pc.Writer, "\n    Image References\n")
				for _, sr := range result.Steps {
					for _, img := range sr.Images {
						fmt.Fprintf(pc.Writer, "    → %s\n", img)
					}
				}
				fmt.Fprintln(pc.Writer)
			}

			return &pipeline.PhaseResult{
				Name:    "publish",
				Status:  "success",
				Summary: fmt.Sprintf("%d image(s)", len(publishManifest.Published)),
			}, nil
		},
	}
}

// dockerReadmeHook syncs README to docker-readme targets.
func dockerReadmeHook() pipeline.PostBuildHook {
	return pipeline.PostBuildHook{
		Name: "readme",
		Condition: func(pc *pipeline.PipelineContext) bool {
			targets := pipeline.CollectTargetsByKind(pc.Config, "docker-readme")
			return len(targets) > 0 && !pc.Local
		},
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			targets := pipeline.CollectTargetsByKind(pc.Config, "docker-readme")
			summary, _ := runReadmeSyncSection(pc.Ctx, pc.Writer, pc.CI, pc.Color, targets, pc.RootDir)
			return &pipeline.PhaseResult{
				Name:    "readme",
				Status:  "success",
				Summary: summary,
			}, nil
		},
	}
}

// dockerRetentionHook applies tag retention to configured registries.
func dockerRetentionHook() pipeline.PostBuildHook {
	return pipeline.PostBuildHook{
		Name: "retention",
		Condition: func(pc *pipeline.PipelineContext) bool {
			plan, ok := pc.Scratch["docker.plan"].(*build.BuildPlan)
			return ok && hasRetention(plan)
		},
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			plan := pc.Scratch["docker.plan"].(*build.BuildPlan)
			summary, _ := runRetentionSection(pc.Ctx, pc.Writer, pc.CI, pc.Color, plan)
			return &pipeline.PhaseResult{
				Name:    "retention",
				Status:  "success",
				Summary: summary,
			}, nil
		},
	}
}

// resolveBuildMode determines the active build mode.
// Priority: recursion guard → CLI flag → config file → default "".
func resolveBuildMode() string {
	// Recursion guard: inner build always runs standard mode
	if build.IsCrucibleChild() {
		return ""
	}
	// CLI flag takes precedence
	if dbBuildMode != "" {
		return dbBuildMode
	}
	// Check config for matching build
	if cfg != nil {
		for _, b := range cfg.Builds {
			if b.Kind == "docker" && b.BuildMode != "" {
				if dbBuildID == "" || b.ID == dbBuildID {
					return b.BuildMode
				}
			}
		}
	}
	return ""
}

// runCrucibleMode orchestrates the two-pass crucible build.
func runCrucibleMode(cmd *cobra.Command, args []string) error {
	rootDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	if len(args) > 0 {
		rootDir = args[0]
	}
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return fmt.Errorf("resolving absolute path: %w", err)
	}

	ctx := context.Background()
	color := output.UseColor()
	w := os.Stdout
	pipelineStart := time.Now()

	// Repo guard
	if err := build.EnsureCrucibleAllowed(rootDir); err != nil {
		return err
	}

	// Generate run ID and temp tags
	runID := build.GenerateCrucibleRunID()
	crucibleTag := build.CrucibleTag("candidate", runID)
	finalTag := build.CrucibleTag("verify", runID)

	// Inject project description
	if desc := firstDockerReadmeDescription(cfg); desc != "" {
		gitver.SetProjectDescription(desc)
	}

	// Banner
	output.Banner(w, output.NewBannerInfo(version.Version, version.Commit, ""), color)

	// CI context block (Pipeline, Runner, Commit, Branch, Registries)
	output.ContextBlock(w, buildContextKV())

	// Crucible Context section — mode, lifecycle, and execution details
	crucibleCtx := output.NewSection(w, "Crucible Context", 0, color)
	crucibleCtx.Row("%-16s%s", "mode", "crucible")
	crucibleEpoch := fmt.Sprintf("%d", pipelineStart.Unix())
	crucibleCreated := time.Unix(pipelineStart.Unix(), 0).UTC().Format(time.RFC3339)

	crucibleCtx.Row("%-16s%s", "phase", "self-build verification")
	crucibleCtx.Row("%-16s%s", "epoch", crucibleEpoch)
	crucibleCtx.Row("%-16s%s", "passes", "2 (gestation → crucible)")
	crucibleCtx.Row("%-16s%s", "candidate", crucibleTag)
	crucibleCtx.Row("%-16s%s", "verify", finalTag)
	crucibleCtx.Row("%-16s%s", "platform p1", fmt.Sprintf("linux/%s", runtime.GOARCH))
	crucibleCtx.Row("%-16s%s", "platform p2", "configured build platforms")
	crucibleCtx.Close()

	// --- Dry run ---
	if dbDryRun {
		fmt.Fprintf(w, "\n    crucible dry-run: would select candidate %s, then enter the crucible via pass 2\n\n", crucibleTag)
		crucibleVerdict(w, "a promising calf has been selected",
			"The tribe has selected a candidate for the crucible.")
		return nil
	}

	// ═══════════════════════════════════════════════════════════
	// Pass 1: Gestation
	// ═══════════════════════════════════════════════════════════

	// Gestation: Detect
	detectStart := time.Now()
	engine, err := build.Get("image")
	if err != nil {
		return err
	}
	det, err := engine.Detect(ctx, rootDir)
	if err != nil {
		return fmt.Errorf("detection: %w", err)
	}
	detectElapsed := time.Since(detectStart)

	gestDetect := output.NewSection(w, "Gestation: Detect", detectElapsed, color)
	for _, df := range det.Dockerfiles {
		gestDetect.Row("%-16s→ %s", "Dockerfile", df.Path)
	}
	gestDetect.Row("%-16s→ %s (auto-detected)", "language", det.Language)
	gestDetect.Close()

	// Gestation: Plan
	planStart := time.Now()

	planCfg := *cfg
	builds := make([]config.BuildConfig, len(planCfg.Builds))
	copy(builds, planCfg.Builds)
	for i := range builds {
		if builds[i].Kind != "docker" {
			continue
		}
		if dbBuildID != "" && builds[i].ID != dbBuildID {
			continue
		}
		// Force single platform for pass 1 (--load limitation)
		builds[i].Platforms = []string{fmt.Sprintf("linux/%s", runtime.GOARCH)}
		if dbTarget != "" {
			builds[i].Target = dbTarget
		}
	}
	planCfg.Builds = builds

	plan, err := engine.Plan(ctx, &engines.ImagePlanInput{Cfg: &planCfg, BuildID: dbBuildID}, det)
	if err != nil {
		return fmt.Errorf("planning: %w", err)
	}

	// Override plan for gestation: load only, no push, crucible tag only
	for i := range plan.Steps {
		plan.Steps[i].Tags = []string{crucibleTag}
		plan.Steps[i].Load = true
		plan.Steps[i].Push = false
		plan.Steps[i].Registries = nil
	}

	// Inject standard labels into the gestation image.
	gestLabels := build.StandardLabels(
		build.NormalizeBuildPlan(plan),
		version.Version,
		version.Commit,
		"crucible-gestation",
		crucibleCreated,
	)
	build.InjectLabels(plan, gestLabels)
	planElapsed := time.Since(planStart)

	gestPlan := output.NewSection(w, "Gestation: Plan", planElapsed, color)
	gestPlan.Row("%-16s%s", "builds", fmt.Sprintf("%d", len(plan.Steps)))
	gestPlan.Row("%-16s%s", "platforms", fmt.Sprintf("linux/%s", runtime.GOARCH))
	gestPlan.Row("%-16s%s", "tags", crucibleTag)
	gestPlan.Row("%-16s%s", "strategy", "local")
	gestPlan.Close()

	// Gestation: Build
	buildStart := time.Now()

	bx := build.NewBuildx(verbose)
	var stderrBuf bytes.Buffer
	bx.Stdout = io.Discard
	if verbose {
		bx.Stderr = os.Stderr
	} else {
		bx.Stderr = &stderrBuf
	}

	var gestResult build.BuildResult
	for _, step := range plan.Steps {
		stepResult, layers, err := bx.BuildWithLayers(ctx, step)
		if stepResult == nil {
			stepResult = &build.StepResult{Name: step.Name, Status: "failed"}
		}
		stepResult.Layers = layers
		gestResult.Steps = append(gestResult.Steps, *stepResult)
		if err != nil {
			buildElapsed := time.Since(buildStart)
			failSec := output.NewSection(w, "Gestation: Build", buildElapsed, color)
			renderBuildLayers(failSec, gestResult.Steps, color)
			output.RowStatus(failSec, "status", "build failed", "failed", color)
			failSec.Close()
			return fmt.Errorf("gestation build failed: %w", err)
		}
	}
	buildElapsed := time.Since(buildStart)

	gestBuild := output.NewSection(w, "Gestation: Build", buildElapsed, color)
	renderBuildLayers(gestBuild, gestResult.Steps, color)
	gestBuild.Separator()
	gestBuild.Row("result  %s", crucibleTag)
	gestBuild.Close()

	// Gestation: Summary
	gestSum := output.NewSection(w, "Gestation: Summary", 0, color)
	output.SummaryRow(w, "detect", "success",
		fmt.Sprintf("%d Dockerfile(s), %s", len(det.Dockerfiles), det.Language), color)
	output.SummaryRow(w, "plan", "success",
		fmt.Sprintf("%d build(s), local", len(plan.Steps)), color)
	output.SummaryRow(w, "build", "success", "candidate loaded", color)
	gestSum.Separator()
	gestSum.Row("%-16s%s", "Invocation", fmt.Sprintf("self-proving rebuild via %s", crucibleTag))
	gestSum.Close()

	// ═══════════════════════════════════════════════════════════
	// Pass 2: Crucible — output streamed from candidate container
	// ═══════════════════════════════════════════════════════════

	fmt.Fprintln(w)
	fmt.Fprintln(w, "    ══════════════════════════════════════════════════════════════")
	fmt.Fprintln(w, "    Pass 2: Crucible — the calf will now self-assess its readiness to lead the tribe")
	fmt.Fprintf(w, "    candidate: %s\n", crucibleTag)
	fmt.Fprintln(w, "    ══════════════════════════════════════════════════════════════")
	fmt.Fprintln(w)

	// Collect credential env vars to forward
	var envVars []string
	credSeen := make(map[string]bool)
	for _, t := range cfg.Targets {
		if t.Credentials == "" || credSeen[t.Credentials] {
			continue
		}
		credSeen[t.Credentials] = true
		prefix := strings.ToUpper(t.Credentials)
		for _, suffix := range []string{"_USER", "_PASS", "_TOKEN", "_KEY", "_SECRET"} {
			key := prefix + suffix
			if v := os.Getenv(key); v != "" {
				envVars = append(envVars, key+"="+v)
			}
		}
	}
	// Forward BUILD_DATE from pass-1 plan to pin timestamps across passes
	for _, step := range plan.Steps {
		if bd, ok := step.BuildArgs["BUILD_DATE"]; ok {
			envVars = append(envVars, "STAGEFREIGHT_BUILD_DATE="+bd)
			break
		}
	}
	// Forward SOURCE_DATE_EPOCH to pin timestamps across passes
	envVars = append(envVars, "SOURCE_DATE_EPOCH="+crucibleEpoch)
	// Forward CI env vars
	for _, ciVar := range []string{
		"CI", "CI_PIPELINE_ID", "CI_COMMIT_SHORT_SHA", "CI_COMMIT_SHA",
		"CI_COMMIT_BRANCH", "CI_COMMIT_TAG", "CI_RUNNER_DESCRIPTION",
		"GITLAB_CI", "GITHUB_REF_NAME",
	} {
		if v := os.Getenv(ciVar); v != "" {
			envVars = append(envVars, ciVar+"="+v)
		}
	}

	// Build forwarded flags: original user flags minus --build-mode
	var extraFlags []string
	if dbLocal {
		extraFlags = append(extraFlags, "--local")
	}
	if len(dbPlatforms) > 0 {
		extraFlags = append(extraFlags, "--platform", strings.Join(dbPlatforms, ","))
	}
	for _, t := range dbTags {
		extraFlags = append(extraFlags, "--tag", t)
	}
	if dbTarget != "" {
		extraFlags = append(extraFlags, "--target", dbTarget)
	}
	if dbBuildID != "" {
		extraFlags = append(extraFlags, "--build", dbBuildID)
	}
	if dbSkipLint {
		extraFlags = append(extraFlags, "--skip-lint")
	}
	if verbose {
		extraFlags = append(extraFlags, "--verbose")
	}
	if cfgFile != "" {
		extraFlags = append(extraFlags, "--config", cfgFile)
	}

	crucibleResult, crucibleErr := build.RunCrucible(ctx, build.CrucibleOpts{
		Image:      crucibleTag,
		FinalTag:   finalTag,
		RepoDir:    rootDir,
		ExtraFlags: extraFlags,
		EnvVars:    envVars,
		RunID:      runID,
		Verbose:    verbose,
	})

	// ═══════════════════════════════════════════════════════════
	// Crucible Verification
	// ═══════════════════════════════════════════════════════════

	var verification *build.CrucibleVerification
	cruciblePassed := crucibleResult != nil && crucibleResult.Passed

	if cruciblePassed {
		verification, err = build.VerifyCrucible(ctx, crucibleTag, finalTag)
		if err != nil {
			// Verification infra failure — still viable
			verification = &build.CrucibleVerification{TrustLevel: build.TrustViable}
		}
		verifySec := output.NewSection(w, "Crucible Verification", 0, color)
		for _, c := range verification.ArtifactChecks {
			icon := checkStatusIcon(c.Status, color)
			verifySec.Row("%-10s/ %-18s %s  %s", "artifact", c.Name, icon, c.Detail)
		}
		for _, c := range verification.ExecutionChecks {
			icon := checkStatusIcon(c.Status, color)
			verifySec.Row("%-10s/ %-18s %s  %s", "execution", c.Name, icon, c.Detail)
		}
		verifySec.Separator()
		verifySec.Row("%-16s%s", "trust level", build.TrustLevelLabel(verification.TrustLevel))
		verifySec.Close()
	}

	// ═══════════════════════════════════════════════════════════
	// Crucible Summary
	// ═══════════════════════════════════════════════════════════

	totalElapsed := time.Since(pipelineStart)
	sumSec := output.NewSection(w, "Crucible Summary", 0, color)

	// Gestation row
	output.SummaryRow(w, "gestation", "success", "candidate built and loaded", color)

	// Verification row
	if verification != nil {
		verStatus := "success"
		if verification.HasHardFailure() {
			verStatus = "failed"
		}
		output.SummaryRow(w, "verification", verStatus, build.TrustLevelLabel(verification.TrustLevel), color)
	} else if cruciblePassed {
		output.SummaryRow(w, "verification", "failed", "verification unavailable", color)
	} else {
		output.SummaryRow(w, "verification", "failed", "not reached", color)
	}

	// Crucible row
	if cruciblePassed {
		output.SummaryRow(w, "crucible", "success", "self-build verified", color)
	} else {
		output.SummaryRow(w, "crucible", "failed", "self-build failed", color)
	}

	// Provenance
	trust := "failed"
	reproducible := false
	if cruciblePassed && verification != nil {
		trust = build.TrustLevelLabel(verification.TrustLevel)
		reproducible = verification.TrustLevel == build.TrustReproducible
	}
	provPath := filepath.Join(rootDir, ".stagefreight", "provenance", fmt.Sprintf("crucible-%s.json", runID))
	stmt := build.ProvenanceStatement{
		Type:          "https://in-toto.io/Statement/v1",
		PredicateType: "https://slsa.dev/provenance/v1",
		Subject: []build.ProvenanceSubject{
			{Name: finalTag},
		},
		Predicate: build.ProvenancePredicate{
			BuildType: "https://stagefreight.dev/build/crucible/v1",
			Builder: build.ProvenanceBuilder{
				ID: "pkg:docker/stagefreight/crucible",
			},
			Invocation: build.ProvenanceInvocation{
				Parameters: map[string]any{
					"mode":      "crucible",
					"build_id":  dbBuildID,
					"target":    dbTarget,
					"platforms": dbPlatforms,
					"local":     dbLocal,
				},
				Environment: map[string]any{
					"run_id":    runID,
					"candidate": crucibleTag,
					"final":     finalTag,
				},
			},
			Metadata: build.ProvenanceMetadata{
				BuildStartedOn:  pipelineStart.UTC().Format(time.RFC3339),
				BuildFinishedOn: time.Now().UTC().Format(time.RFC3339),
				Completeness: map[string]bool{
					"parameters":  true,
					"environment": true,
					"materials":   false,
				},
				Reproducible: reproducible,
			},
			StageFreight: map[string]any{
				"trust_level":  trust,
				"version":      version.Version,
				"commit":       version.Commit,
				"plan_sha256":  build.NormalizeBuildPlan(plan),
			},
		},
	}
	if provErr := build.WriteProvenance(provPath, stmt); provErr == nil {
		output.SummaryRow(w, "provenance", "success", provPath, color)
	} else {
		output.SummaryRow(w, "provenance", "failed", provErr.Error(), color)
	}

	// Cleanup
	cleanupErr := build.CleanupCrucibleImages(ctx, crucibleTag, finalTag)
	if cleanupErr != nil {
		output.SummaryRow(w, "cleanup", "failed", cleanupErr.Error(), color)
	} else {
		output.SummaryRow(w, "cleanup", "success", "temp images removed", color)
	}

	sumSec.Separator()
	overallStatus := "success"
	if !cruciblePassed {
		overallStatus = "failed"
	}
	output.SummaryTotal(w, totalElapsed, overallStatus, color)
	sumSec.Close()

	switch {
	case !cruciblePassed:
		crucibleVerdict(w, "the calf is not yet mature",
			"Self-build failed; leadership remains with the current tribe leader.")
	case verification != nil && verification.HasHardFailure():
		crucibleVerdict(w, "self-awareness remains incomplete",
			"The calf's self-assessment differs from the judgment of the tribe leader.")
	default:
		crucibleVerdict(w, "the calf has proven its maturity",
			"This build now leads the tribe.")
	}

	if crucibleErr != nil {
		return fmt.Errorf("crucible: %w", crucibleErr)
	}

	return nil
}

func crucibleVerdict(w io.Writer, title, body string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "    ──────────────────────────────────────────────────────────────")
	fmt.Fprintf(w, "    🐘 Crucible Verdict: %s\n", title)
	fmt.Fprintf(w, "    %s\n", body)
	fmt.Fprintln(w, "    ──────────────────────────────────────────────────────────────")
	fmt.Fprintln(w)
}

// checkStatusIcon returns the appropriate icon for a verification check status.
func checkStatusIcon(status string, color bool) string {
	switch status {
	case "match":
		return output.StatusIcon("success", color)
	case "differs":
		return output.StatusIcon("failed", color)
	case "info":
		return output.StatusIcon("warning", color)
	default:
		return output.StatusIcon("skipped", color)
	}
}

// hasRetention returns true if any step has a registry with retention configured.
func hasRetention(plan *build.BuildPlan) bool {
	for _, step := range plan.Steps {
		if len(step.Registries) == 0 {
			continue
		}
		for _, reg := range step.Registries {
			if reg.Retention.Active() {
				return true
			}
		}
	}
	return false
}

// runRetentionSection applies tag retention with section-formatted output.
// Returns a summary string and elapsed time for the summary table.
func runRetentionSection(ctx context.Context, w io.Writer, _ bool, color bool, plan *build.BuildPlan) (string, time.Duration) {
	output.SectionStartCollapsed(w, "sf_retention", "Retention")
	retStart := time.Now()

	var totalDeleted int
	var totalKept int
	var totalSkipped int
	var totalErrors int
	var deletedNames []string
	var skippedNames []string

	for _, step := range plan.Steps {
		if len(step.Registries) == 0 {
			continue
		}
		for _, reg := range step.Registries {
			if !reg.Retention.Active() {
				continue
			}

			client, err := registry.NewRegistry(reg.Provider, reg.URL, reg.Credentials)
			if err != nil {
				fmt.Fprintf(w, "  ERROR: %s/%s: %v\n", reg.URL, reg.Path, err)
				totalErrors++
				continue
			}

			// Copy policy and protect produced tags from deletion.
			policy := reg.Retention
			policy.Protect = append([]string{}, policy.Protect...)
			for _, t := range reg.Tags {
				policy.Protect = append(policy.Protect, t)
			}

			result, err := registry.ApplyRetention(ctx, client, reg.Path, reg.TagPatterns, policy)
			if err != nil {
				fmt.Fprintf(w, "  ERROR: %s/%s: %v\n", reg.URL, reg.Path, err)
				totalErrors++
				continue
			}

			for _, e := range result.Errors {
				fmt.Fprintf(w, "  ERROR: %v\n", e)
			}

			totalKept += result.Kept
			totalDeleted += len(result.Deleted)
			totalSkipped += len(result.Skipped)
			totalErrors += len(result.Errors)
			deletedNames = append(deletedNames, result.Deleted...)
			skippedNames = append(skippedNames, result.Skipped...)
		}
	}

	retElapsed := time.Since(retStart)

	sec := output.NewSection(w, "Retention", retElapsed, color)
	for _, step := range plan.Steps {
		for _, reg := range step.Registries {
			if !reg.Retention.Active() {
				continue
			}
			sec.Row("%-40skept %d, pruned %d", reg.URL+"/"+reg.Path, totalKept, totalDeleted)
		}
	}
	for _, d := range deletedNames {
		sec.Row("  - %s", d)
	}
	for _, s := range skippedNames {
		sec.Row("  ~ %s (digest shared with protected tag)", s)
	}
	sec.Close()
	output.SectionEnd(w, "sf_retention")

	summary := fmt.Sprintf("kept %d, pruned %d", totalKept, totalDeleted)
	if totalSkipped > 0 {
		summary += fmt.Sprintf(", %d skipped", totalSkipped)
	}
	if totalErrors > 0 {
		summary += fmt.Sprintf(", %d error(s)", totalErrors)
	}

	return summary, retElapsed
}

// hasRemoteRegistries returns true if the registry list has any non-local providers.
func hasRemoteRegistries(registries []build.RegistryTarget) bool {
	for _, r := range registries {
		if r.Provider != "local" {
			return true
		}
	}
	return false
}

// collectRemoteTags returns fully qualified image refs for all remote registry
// tags in load-then-push steps (single-platform, Load=true, has remote registries).
func collectRemoteTags(plan *build.BuildPlan) []string {
	var tags []string
	for _, step := range plan.Steps {
		// Only for load-then-push (single-platform loaded into daemon)
		if !step.Load || step.Push {
			continue
		}
		for _, reg := range step.Registries {
			if reg.Provider == "local" {
				continue
			}
			for _, t := range reg.Tags {
				tags = append(tags, fmt.Sprintf("%s/%s:%s", reg.URL, reg.Path, t))
			}
		}
	}
	return tags
}

func formatPlatforms(steps []build.BuildStep) string {
	if len(steps) == 0 {
		return ""
	}
	// Collect unique platforms across all steps
	seen := make(map[string]bool)
	var platforms []string
	for _, s := range steps {
		for _, p := range s.Platforms {
			if !seen[p] {
				seen[p] = true
				platforms = append(platforms, p)
			}
		}
	}
	if len(platforms) == 0 {
		return runtime.GOOS + "/" + runtime.GOARCH
	}
	return strings.Join(platforms, ",")
}

// buildContextKV returns key-value pairs for the pipeline context block.
func buildContextKV() []output.KV {
	var kv []output.KV

	if pipe := os.Getenv("CI_PIPELINE_ID"); pipe != "" {
		kv = append(kv, output.KV{Key: "Pipeline", Value: pipe})
	}
	if runner := os.Getenv("CI_RUNNER_DESCRIPTION"); runner != "" {
		kv = append(kv, output.KV{Key: "Runner", Value: runner})
	}

	if sha := os.Getenv("CI_COMMIT_SHORT_SHA"); sha != "" {
		kv = append(kv, output.KV{Key: "Commit", Value: sha})
	} else if sha := os.Getenv("CI_COMMIT_SHA"); sha != "" && len(sha) >= 8 {
		kv = append(kv, output.KV{Key: "Commit", Value: sha[:8]})
	}
	if branch := os.Getenv("CI_COMMIT_BRANCH"); branch != "" {
		kv = append(kv, output.KV{Key: "Branch", Value: branch})
	} else if tag := os.Getenv("CI_COMMIT_TAG"); tag != "" {
		kv = append(kv, output.KV{Key: "Tag", Value: tag})
	}

	platforms := formatPlatforms(nil) // filled after plan, but context block is pre-plan
	if p := os.Getenv("STAGEFREIGHT_PLATFORMS"); p != "" {
		platforms = p
	}
	if platforms != "" {
		kv = append(kv, output.KV{Key: "Platforms", Value: platforms})
	}

	// Count configured registry targets
	regTargets := collectTargetsByKind(cfg, "registry")
	if len(regTargets) > 0 {
		var regNames []string
		seen := make(map[string]bool)
		for _, t := range regTargets {
			if !seen[t.URL] {
				regNames = append(regNames, t.URL)
				seen[t.URL] = true
			}
		}
		kv = append(kv, output.KV{Key: "Registries", Value: fmt.Sprintf("%d (%s)", len(regTargets), strings.Join(regNames, ", "))})
	}

	return kv
}

// hasNarratorBadgeItems returns true if any narrator item has badge generation configured.
func hasNarratorBadgeItems() bool {
	for _, f := range cfg.Narrator {
		for _, item := range f.Items {
			if item.HasGeneration() {
				return true
			}
		}
	}
	return false
}

// collectNarratorBadgeItems returns all narrator items with badge generation.
func collectNarratorBadgeItems() []config.NarratorItem {
	var items []config.NarratorItem
	for _, f := range cfg.Narrator {
		for _, item := range f.Items {
			if item.HasGeneration() {
				items = append(items, item)
			}
		}
	}
	return items
}

// runBadgeSection generates configured badges with section-formatted output.
func runBadgeSection(w io.Writer, color bool, rootDir string) (string, time.Duration) {
	output.SectionStartCollapsed(w, "sf_badges", "Badges")
	start := time.Now()

	eng, err := buildDefaultBadgeEngine()
	if err != nil {
		elapsed := time.Since(start)
		sec := output.NewSection(w, "Badges", elapsed, color)
		sec.Row("error: %v", err)
		sec.Close()
		output.SectionEnd(w, "sf_badges")
		return fmt.Sprintf("error: %v", err), elapsed
	}

	items := collectNarratorBadgeItems()

	// Detect version for template resolution
	vi, _ := build.DetectVersion(rootDir)

	// Pass 1: resolve version templates for all badges, collect resolved values
	specs := make([]config.BadgeSpec, len(items))
	resolvedValues := make([]string, len(items))
	for i, item := range items {
		specs[i] = item.ToBadgeSpec()
		value := specs[i].Value
		if vi != nil && value != "" {
			value = gitver.ResolveTemplateWithDirAndVars(value, vi, rootDir, cfg.Vars)
		}
		resolvedValues[i] = value
	}

	// Scan resolved values for {docker.tag.*} patterns to discover tag names
	tagNames := gitver.ExtractDockerTagNames(resolvedValues)

	// Lazy Docker Hub info — fetch repo-level + per-tag info if needed
	var dockerInfo *gitver.DockerHubInfo
	needsDocker := len(tagNames) > 0
	if !needsDocker {
		for _, v := range resolvedValues {
			if strings.Contains(v, "{docker.") {
				needsDocker = true
				break
			}
		}
	}
	if needsDocker {
		ns, repo := dockerHubFromConfig()
		if ns != "" && repo != "" {
			dockerInfo, _ = gitver.FetchDockerHubInfo(ns, repo)
			if dockerInfo != nil && len(tagNames) > 0 {
				client := &http.Client{Timeout: 10 * time.Second}
				dockerInfo.Tags = gitver.FetchTagInfo(client, ns, repo, tagNames)
			}
		}
	}

	// Pass 2: resolve docker templates and generate SVGs
	var generated int
	for i := range items {
		spec := specs[i]

		// Per-item engine if font is overridden
		itemEng := eng
		if spec.Font != "" || spec.FontFile != "" || spec.FontSize != 0 {
			override, oErr := buildItemEngine(spec)
			if oErr != nil {
				continue
			}
			itemEng = override
		}

		value := gitver.ResolveDockerTemplates(resolvedValues[i], dockerInfo)

		// Guard against empty or unresolved template values producing broken badges.
		// Any remaining "{" means a template didn't resolve (missing tag, nil docker info, etc).
		if value == "" || strings.Contains(value, "{") {
			value = "n/a"
		}

		// Resolve color
		badgeColor := spec.Color
		if badgeColor == "" || badgeColor == "auto" {
			badgeColor = badge.StatusColor("passed")
		}

		svg := itemEng.Generate(badge.Badge{
			Label: spec.Label,
			Value: value,
			Color: badgeColor,
		})

		if mkErr := os.MkdirAll(filepath.Dir(spec.Output), 0o755); mkErr != nil {
			continue
		}
		if wErr := os.WriteFile(spec.Output, []byte(svg), 0o644); wErr != nil {
			continue
		}
		generated++
	}

	elapsed := time.Since(start)
	sec := output.NewSection(w, "Badges", elapsed, color)
	for _, item := range items {
		spec := item.ToBadgeSpec()
		fontName := spec.Font
		if fontName == "" {
			fontName = "dejavu-sans"
		}
		size := spec.FontSize
		if size == 0 {
			size = 11
		}
		badgeColor := spec.Color
		if badgeColor == "" {
			badgeColor = "auto"
		}
		sec.Row("%-16s%-24s %-8s %.0fpt  %s", item.Text, spec.Output, fontName, size, badgeColor)
	}
	sec.Close()
	output.SectionEnd(w, "sf_badges")

	summary := fmt.Sprintf("%d generated", generated)
	return summary, elapsed
}

// runReadmeSyncSection syncs README to docker-readme targets with section-formatted output.
func runReadmeSyncSection(ctx context.Context, w io.Writer, _ bool, color bool, targets []config.TargetConfig, rootDir string) (string, time.Duration) {
	output.SectionStartCollapsed(w, "sf_readme", "README Sync")
	start := time.Now()

	var synced, errors int

	for _, t := range targets {
		// Resolve {var:...} templates in target fields
		resolvedPath := gitver.ResolveVars(t.Path, cfg.Vars)
		resolvedDesc := gitver.ResolveVars(t.Description, cfg.Vars)
		resolvedLinkBase := gitver.ResolveVars(t.LinkBase, cfg.Vars)

		file := t.File
		if file == "" {
			file = "README.md"
		}

		content, err := registry.PrepareReadmeFromFile(file, resolvedDesc, resolvedLinkBase, rootDir)
		if err != nil {
			errors++
			continue
		}

		provider := t.Provider
		if provider == "" {
			provider = build.DetectProvider(t.URL)
		}

		client, err := registry.NewRegistry(provider, t.URL, t.Credentials)
		if err != nil {
			errors++
			continue
		}

		short := content.Short
		if resolvedDesc != "" {
			short = resolvedDesc
		}

		if err := client.UpdateDescription(ctx, resolvedPath, short, content.Full); err != nil {
			errors++
			continue
		}
		synced++
	}

	elapsed := time.Since(start)
	sec := output.NewSection(w, "Readme", elapsed, color)
	for _, t := range targets {
		resolvedPath := gitver.ResolveVars(t.Path, cfg.Vars)
		sec.Row("%-40ssynced", t.URL+"/"+resolvedPath)
	}
	sec.Close()
	output.SectionEnd(w, "sf_readme")

	summary := fmt.Sprintf("%d synced", synced)
	if errors > 0 {
		summary += fmt.Sprintf(", %d error(s)", errors)
	}
	return summary, elapsed
}

// renderBuildLayers renders parsed layer events into a Section.
// Returns true if any layers were rendered.
func renderBuildLayers(sec *output.Section, steps []build.StepResult, color bool) bool {
	hasLayers := false
	for _, sr := range steps {
		for _, layer := range sr.Layers {
			instr := build.FormatLayerInstruction(layer)
			timing := build.FormatLayerTiming(layer)

			var label string
			if layer.Instruction == "FROM" {
				label = "base"
			} else {
				label = layer.Instruction
			}

			timingStr := timing
			if layer.Cached {
				timingStr = output.Dimmed("cached", color)
			}
			sec.Row("%-8s%-42s %s", label, instr, timingStr)
			hasLayers = true
		}
	}
	return hasLayers
}

// dockerHubFromConfig returns the namespace and repo for the first docker.io registry target.
func dockerHubFromConfig() (string, string) {
	for _, t := range cfg.Targets {
		if t.Kind == "registry" && t.URL == "docker.io" && t.Path != "" {
			resolved := gitver.ResolveVars(t.Path, cfg.Vars)
			parts := strings.SplitN(resolved, "/", 2)
			if len(parts) == 2 {
				return parts[0], parts[1]
			}
		}
	}
	return "", ""
}

// collectTargetsByKind returns all targets matching the given kind.
func collectTargetsByKind(cfg *config.Config, kind string) []config.TargetConfig {
	var targets []config.TargetConfig
	for _, t := range cfg.Targets {
		if t.Kind == kind {
			targets = append(targets, t)
		}
	}
	return targets
}

// firstDockerReadmeDescription returns the description from the first docker-readme target.
func firstDockerReadmeDescription(cfg *config.Config) string {
	for _, t := range cfg.Targets {
		if t.Kind == "docker-readme" && t.Description != "" {
			return t.Description
		}
	}
	return ""
}
