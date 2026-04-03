package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/build/pipeline"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/postbuild"
	"github.com/PrPlanIT/StageFreight/src/version"
)

// resolveBuildMode determines the active build mode.
// Priority: recursion guard → CLI flag → config file → default "".
func resolveBuildMode(req Request) string {
	// Recursion guard: inner build always runs standard mode
	if build.IsCrucibleChild() {
		return ""
	}
	// CLI flag takes precedence
	if req.BuildMode != "" {
		return req.BuildMode
	}
	// Check config for matching build
	if req.Config != nil {
		for _, b := range req.Config.Builds {
			if b.Kind == "docker" && b.BuildMode != "" {
				if req.BuildID == "" || b.ID == req.BuildID {
					return b.BuildMode
				}
			}
		}
	}
	return ""
}

// runCrucibleMode orchestrates the two-pass crucible build.
func runCrucibleMode(req Request) error {
	rootDir := req.RootDir
	var err error
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return fmt.Errorf("resolving absolute path: %w", err)
	}

	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	color := output.UseColor()
	w := req.Stdout
	pipelineStart := time.Now()

	// Repo guard
	if err := build.EnsureCrucibleAllowed(rootDir); err != nil {
		return err
	}

	// Generate run ID and temp tags
	runID := build.GenerateCrucibleRunID()
	crucibleTag := CrucibleTag("candidate", runID)
	finalTag := CrucibleTag("verify", runID)

	// Inject project description
	if desc := postbuild.FirstDockerReadmeDescription(req.Config); desc != "" {
		gitver.SetProjectDescription(desc)
	}

	// Banner
	output.Banner(w, output.NewBannerInfo(version.Version, version.Commit, ""), color)

	// CI context block (Pipeline, Runner, Commit, Branch, Registries)
	output.ContextBlock(w, buildContextKV(req))

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
	if req.DryRun {
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

	planCfg := *req.Config
	builds := make([]config.BuildConfig, len(planCfg.Builds))
	copy(builds, planCfg.Builds)
	for i := range builds {
		if builds[i].Kind != "docker" {
			continue
		}
		if req.BuildID != "" && builds[i].ID != req.BuildID {
			continue
		}
		// Force single platform for pass 1 (--load limitation)
		builds[i].Platforms = []string{fmt.Sprintf("linux/%s", runtime.GOARCH)}
		if req.Target != "" {
			builds[i].Target = req.Target
		}
	}
	planCfg.Builds = builds

	plan, err := engine.Plan(ctx, &build.ImagePlanInput{Cfg: &planCfg, BuildID: req.BuildID}, det)
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

	bx := NewBuildx(req.Verbose)
	var stderrBuf, stdoutBuf bytes.Buffer
	bx.Stdout = &stdoutBuf
	if req.Verbose {
		bx.Stderr = req.Stderr
	} else {
		bx.Stderr = &stderrBuf
	}

	var gestResult build.BuildResult
	for _, step := range plan.Steps {
		// Crucible never pushes — strip cache export to avoid auth failures.
		// Cache import (CacheFrom) is kept for layer reuse.
		step.CacheTo = nil

		stdoutBuf.Reset()
		stderrBuf.Reset()
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

			// Semantic error extraction — shared contract via errsurface.go.
			combinedOutput := stdoutBuf.String() + "\n" + stderrBuf.String()
			RenderBuildError(failSec, combinedOutput)

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
	for _, t := range req.Config.Targets {
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
	if req.Local {
		extraFlags = append(extraFlags, "--local")
	}
	if len(req.Platforms) > 0 {
		extraFlags = append(extraFlags, "--platform", strings.Join(req.Platforms, ","))
	}
	for _, t := range req.Tags {
		extraFlags = append(extraFlags, "--tag", t)
	}
	if req.Target != "" {
		extraFlags = append(extraFlags, "--target", req.Target)
	}
	if req.BuildID != "" {
		extraFlags = append(extraFlags, "--build", req.BuildID)
	}
	if req.SkipLint {
		extraFlags = append(extraFlags, "--skip-lint")
	}
	if req.Verbose {
		extraFlags = append(extraFlags, "--verbose")
	}
	if req.ConfigFile != "" {
		extraFlags = append(extraFlags, "--config", req.ConfigFile)
	}

	crucibleResult, crucibleErr := RunCrucible(ctx, CrucibleOpts{
		Image:      crucibleTag,
		FinalTag:   finalTag,
		RepoDir:    rootDir,
		ExtraFlags: extraFlags,
		EnvVars:    envVars,
		RunID:      runID,
		Verbose:    req.Verbose,
	})

	// ═══════════════════════════════════════════════════════════
	// Crucible Verification
	// ═══════════════════════════════════════════════════════════

	var verification *CrucibleVerification
	cruciblePassed := crucibleResult != nil && crucibleResult.Passed

	if cruciblePassed {
		verification, err = VerifyCrucible(ctx, crucibleTag, finalTag)
		if err != nil {
			// Verification infra failure — still viable
			verification = &CrucibleVerification{TrustLevel: build.TrustViable}
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
		output.SummaryRow(w, "verification", "failed", "skipped — verification infra error", color)
	} else {
		output.SummaryRow(w, "verification", "failed", "skipped — publication failed", color)
	}

	// Crucible row
	if cruciblePassed {
		output.SummaryRow(w, "crucible", "success", "self-build verified", color)
	} else {
		output.SummaryRow(w, "crucible", "failed", "candidate build failed", color)
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
					"build_id":  req.BuildID,
					"target":    req.Target,
					"platforms": req.Platforms,
					"local":     req.Local,
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
				"trust_level": trust,
				"version":     version.Version,
				"commit":      version.Commit,
				"plan_sha256": build.NormalizeBuildPlan(plan),
			},
		},
	}
	if provErr := build.WriteProvenance(provPath, stmt); provErr == nil {
		output.SummaryRow(w, "provenance", "success", provPath, color)
	} else {
		output.SummaryRow(w, "provenance", "failed", provErr.Error(), color)
	}

	// Cleanup
	cleanupErr := CleanupCrucibleImages(ctx, crucibleTag, finalTag)
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

	// Read FailureDetail written by the crucible child (pass 2).
	childFailure := pipeline.ReadFailureDetail(rootDir)

	// Exit Reason — rendered between summary and verdict.
	if childFailure != nil {
		pipeline.RenderExitReason(w, childFailure)
	}

	// Verdict — sacred elephant law: these lines do NOT change.
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
		return crucibleErr
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

// buildContextKV returns key-value pairs for the pipeline context block.
func buildContextKV(req Request) []output.KV {
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
	regTargets := pipeline.CollectTargetsByKind(req.Config, "registry")
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
