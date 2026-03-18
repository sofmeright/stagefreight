package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/postbuild"
	"github.com/PrPlanIT/StageFreight/src/registry"
)

// executePhase builds images via buildx, pushes, and signs.
// Build + push + sign are kept in one phase because they share buildx state,
// publish manifest accumulation, and deferred metadata file cleanup.
func executePhase(req Request) pipeline.Phase {
	return pipeline.Phase{
		Name: "build",
		Run: func(pc *pipeline.PipelineContext) (*pipeline.PhaseResult, error) {
			plan := pc.BuildPlan
		if plan == nil {
			return nil, fmt.Errorf("missing build plan")
		}

			// Publish manifest tracking
			var publishManifest artifact.PublishManifest
			var publishModeUsed bool

			buildInst := artifact.BuildInstance{
				Commit:     os.Getenv("CI_COMMIT_SHA"),
				PipelineID: os.Getenv("CI_PIPELINE_ID"),
				JobID:      os.Getenv("CI_JOB_ID"),
				CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			}

			output.SectionStart(pc.Writer, "sf_build", "Build")
			buildStart := time.Now()

			// Always capture output for structured display; verbose forwards stderr in real-time
			bx := NewBuildx(pc.Verbose)
			var stderrBuf bytes.Buffer
			bx.Stdout = io.Discard
			if pc.Verbose {
				bx.Stderr = req.Stderr
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
				stderrBuf.Reset()
				stepResult, layers, err := bx.BuildWithLayers(pc.Ctx, step)
				if stepResult == nil {
					stepResult = &build.StepResult{Name: step.Name, Status: "failed"}
				}
				stepResult.Layers = layers

				// Harbor push-first recovery: if a multi-platform --push build fails
				// because the Harbor project doesn't exist yet, create it and retry once.
				if err != nil && step.Push && postbuild.IsHarborProjectMissingPushError(step.Registries, stderrBuf.String()) {
					diag.Info("harbor: push failed with project-not-found, attempting auto-create and retry")
					if ensureErr := postbuild.EnsureHarborProjects(pc.Ctx, step.Registries); ensureErr == nil {
						stderrBuf.Reset()
						stepResult, layers, err = bx.BuildWithLayers(pc.Ctx, step)
						if stepResult == nil {
							stepResult = &build.StepResult{Name: step.Name, Status: "failed"}
						}
						stepResult.Layers = layers
					} else {
						diag.Warn("harbor: auto-create failed: %v", ensureErr)
					}
				}

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
						fmt.Fprint(req.Stderr, stderrBuf.String())
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
					postbuild.TriggerHarborScans(pc.Ctx, step.Registries)
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
						if d, mErr := ParseMetadataDigest(step.MetadataFile); mErr == nil {
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
							d, rErr := ResolveDigest(pc.Ctx, ref)
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

						publishManifest.Published = append(publishManifest.Published, artifact.PublishedImage{
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
				var pushStderrBuf bytes.Buffer
				if pc.Verbose {
					pushBx.Stderr = io.MultiWriter(req.Stderr, &pushStderrBuf)
				} else {
					pushBx.Stderr = &pushStderrBuf
				}
				if err := pushBx.PushTags(pc.Ctx, remoteTags); err != nil {
					// Harbor push-first recovery: collect registries from load-then-push steps
					// and check if the failure is a missing Harbor project.
					var pushRegs []build.RegistryTarget
					for _, step := range plan.Steps {
						if !step.Load || step.Push {
							continue
						}
						for _, reg := range step.Registries {
							if reg.Provider != "local" {
								pushRegs = append(pushRegs, reg)
							}
						}
					}
					if postbuild.IsHarborProjectMissingPushError(pushRegs, pushStderrBuf.String()) {
						diag.Info("harbor: push failed with project-not-found, attempting auto-create and retry")
						if ensureErr := postbuild.EnsureHarborProjects(pc.Ctx, pushRegs); ensureErr == nil {
							pushStderrBuf.Reset()
							if pc.Verbose {
								pushBx.Stderr = io.MultiWriter(req.Stderr, &pushStderrBuf)
							} else {
								pushBx.Stderr = &pushStderrBuf
							}
							err = pushBx.PushTags(pc.Ctx, remoteTags)
						} else {
							diag.Warn("harbor: auto-create failed: %v", ensureErr)
						}
					}
					if err != nil {
						output.SectionEnd(pc.Writer, "sf_push")
						return nil, err
					}
				}

				// Trigger Harbor scans after single-platform push
				for _, step := range plan.Steps {
					if step.Load && !step.Push {
						postbuild.TriggerHarborScans(pc.Ctx, step.Registries)
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
								d, rErr := ResolveDigest(pc.Ctx, ref)
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
								if d, lErr := ResolveLocalDigest(pc.Ctx, ref); lErr == nil {
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

							publishManifest.Published = append(publishManifest.Published, artifact.PublishedImage{
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
				cosignKey := ResolveCosignKey()
				cosignOnPath := CosignAvailable()
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

						signErr := CosignSign(pc.Ctx, digestRef, cosignKey, multiArch)

						if _, statErr := os.Stat(dssePath); statErr == nil {
							_ = CosignAttest(pc.Ctx, digestRef, dssePath, cosignKey)
						}

						if signErr != nil {
							publishManifest.Published[i].SigningAttempted = true
						} else {
							artifacts, _ := registry.DiscoverArtifacts(pc.Ctx, img, nil)
							publishManifest.Published[i].Attestation = &artifact.AttestationRecord{
								Type:           artifact.AttestationCosign,
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
