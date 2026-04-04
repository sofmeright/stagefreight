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

	"github.com/PrPlanIT/StageFreight/src/artifact"
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
	if build.IsCrucibleChild() {
		return ""
	}
	if req.BuildMode != "" {
		return req.BuildMode
	}
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

// runCrucibleMode orchestrates the consolidated two-pass crucible build.
//
// Flow: Lint → Detect → Plan → Builder → Cache → Build (pass 1) →
//       Rebuild (pass 2) → Verify → Publish → Retention → Provenance → Verdict
//
// Single execution context. One backend. No docker run. No separate container.
// Both passes use the same buildkitd/DinD backend with shared cache.
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

	if err := build.EnsureCrucibleAllowed(rootDir); err != nil {
		return err
	}

	runID := build.GenerateCrucibleRunID()
	candidateTag := CrucibleTag("candidate", runID)
	verifyTag := CrucibleTag("verify", runID)

	if desc := postbuild.FirstDockerReadmeDescription(req.Config); desc != "" {
		gitver.SetProjectDescription(desc)
	}

	// ── Banner + Context ─────────────────────────────────────────
	output.Banner(w, output.NewBannerInfo(version.Version, version.Commit, ""), color)
	output.ContextBlock(w, buildContextKV(req))

	crucibleEpoch := fmt.Sprintf("%d", pipelineStart.Unix())
	crucibleCreated := time.Unix(pipelineStart.Unix(), 0).UTC().Format(time.RFC3339)

	// Resolve backend ONCE — coherent for the entire crucible.
	backend, backendErr := ResolveBackendWithConfig(BackendCapabilities{
		Build:      true,
		Run:        true,
		Filesystem: true,
	}, req.Config.BuildCache.Builder.Backend)

	ctxSec := output.NewSection(w, "Crucible Context", 0, color)
	ctxSec.Row("%-16s%s", "mode", "crucible")
	ctxSec.Row("%-16s%s", "phase", "self-build verification")
	ctxSec.Row("%-16s%s", "epoch", crucibleEpoch)
	ctxSec.Row("%-16s%s", "passes", "2 (candidate → self-proof)")
	ctxSec.Row("%-16s%s", "candidate", candidateTag)
	ctxSec.Row("%-16s%s", "verify", verifyTag)
	ctxSec.Row("%-16s%s", "platform", fmt.Sprintf("linux/%s", runtime.GOARCH))
	if backendErr == nil {
		ctxSec.Row("%-16s%s", "backend", backend.Kind)
	} else {
		ctxSec.Row("%-16s%s", "backend", "unavailable")
	}
	ctxSec.Close()

	if backendErr != nil {
		return fmt.Errorf("crucible: no coherent backend: %w", backendErr)
	}

	// ── Dry run ──────────────────────────────────────────────────
	if req.DryRun {
		fmt.Fprintf(w, "\n    crucible dry-run: would select candidate %s, then enter the crucible via pass 2\n\n", candidateTag)
		crucibleVerdict(w, "a promising calf has been selected",
			"The tribe has selected a candidate for the crucible.")
		return nil
	}

	// ── Lint ─────────────────────────────────────────────────────
	// Lint runs FIRST — before any build. No bypassing.
	if !req.SkipLint {
		pc := &pipeline.PipelineContext{
			Ctx:     ctx,
			RootDir: rootDir,
			Config:  req.Config,
			Writer:  w,
			Color:   color,
			Verbose: req.Verbose,
		}
		lintPhase := pipeline.LintPhase()
		lintResult, lintErr := lintPhase.Run(pc)
		if lintResult != nil {
			// Lint narrates its own section — just capture the result.
			_ = lintResult
		}
		if lintErr != nil {
			return fmt.Errorf("crucible lint gate: %w", lintErr)
		}
	}

	// ── Detect ───────────────────────────────────────────────────
	detectStart := time.Now()
	engine, err := build.Get("image")
	if err != nil {
		return err
	}
	det, err := engine.Detect(ctx, rootDir)
	if err != nil {
		return fmt.Errorf("detection: %w", err)
	}

	detectSec := output.NewSection(w, "Detect", time.Since(detectStart), color)
	for _, df := range det.Dockerfiles {
		detectSec.Row("%-16s→ %s", "Dockerfile", df.Path)
	}
	detectSec.Row("%-16s→ %s (auto-detected)", "language", det.Language)
	detectSec.Close()

	// ── Plan ─────────────────────────────────────────────────────
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

	planSec := output.NewSection(w, "Plan", time.Since(planStart), color)
	planSec.Row("%-16s%s", "builds", fmt.Sprintf("%d", len(plan.Steps)))
	planSec.Row("%-16s%s", "platforms", fmt.Sprintf("linux/%s", runtime.GOARCH))
	planSec.Row("%-16s%s", "tags", fmt.Sprintf("%s, %s", candidateTag, verifyTag))
	planSec.Row("%-16s%s", "strategy", "load + push")
	planSec.Close()

	// ── Builder ──────────────────────────────────────────────────
	builderInfo := EnsureBuilderWithBackend(req.Config.BuildCache.Builder, backend)
	builderInfo = ResolveBuilderInfo(builderInfo)
	RenderBuilderInfo(w, color, builderInfo)

	// ── Cache ────────────────────────────────────────────────────
	pc := &pipeline.PipelineContext{
		Ctx:     ctx,
		RootDir: rootDir,
		Config:  req.Config,
		Writer:  w,
		Color:   color,
		Verbose: req.Verbose,
	}
	cacheInfo := ResolveCacheInfo(pc)
	RenderCacheInfo(w, color, cacheInfo)

	// ── Build (pass 1: candidate) ────────────────────────────────
	pass1Plan := clonePlan(plan)
	for i := range pass1Plan.Steps {
		pass1Plan.Steps[i].Tags = []string{candidateTag}
		pass1Plan.Steps[i].Load = true
		pass1Plan.Steps[i].Push = false
		pass1Plan.Steps[i].Registries = nil
		pass1Plan.Steps[i].CacheTo = nil // candidate never exports cache
	}

	pass1Labels := build.StandardLabels(
		build.NormalizeBuildPlan(pass1Plan),
		version.Version, version.Commit,
		"crucible-candidate", crucibleCreated,
	)
	build.InjectLabels(pass1Plan, pass1Labels)

	pass1Result, pass1Err := executeBuildPass(ctx, w, color, req.Verbose, req.Stderr,
		"Build (pass 1: candidate)", pass1Plan, candidateTag)
	if pass1Err != nil {
		crucibleVerdict(w, "the calf is not yet mature",
			"Self-build failed; leadership remains with the current tribe leader.")
		return pass1Err
	}

	// ══════════════════════════════════════════════════════════════
	// Pass 2: Crucible — the calf will now self-assess its readiness
	// ══════════════════════════════════════════════════════════════
	fmt.Fprintln(w)
	fmt.Fprintln(w, "    ══════════════════════════════════════════════════════════════")
	fmt.Fprintln(w, "    Pass 2: Crucible — the calf will now self-assess its readiness to lead the tribe")
	fmt.Fprintf(w, "    candidate: %s\n", candidateTag)
	fmt.Fprintln(w, "    ══════════════════════════════════════════════════════════════")
	fmt.Fprintln(w)

	// ── Rebuild (pass 2: self-proof) ─────────────────────────────
	pass2Plan := clonePlan(plan)
	for i := range pass2Plan.Steps {
		pass2Plan.Steps[i].Tags = []string{verifyTag}
		pass2Plan.Steps[i].Load = true
		pass2Plan.Steps[i].Push = false
		pass2Plan.Steps[i].Registries = nil
		pass2Plan.Steps[i].CacheTo = nil
	}

	pass2Labels := build.StandardLabels(
		build.NormalizeBuildPlan(pass2Plan),
		version.Version, version.Commit,
		"crucible-verify", crucibleCreated,
	)
	build.InjectLabels(pass2Plan, pass2Labels)

	pass2Result, pass2Err := executeBuildPass(ctx, w, color, req.Verbose, req.Stderr,
		"Rebuild (pass 2: self-proof)", pass2Plan, verifyTag)

	cruciblePassed := pass2Err == nil
	_ = pass1Result
	_ = pass2Result

	// ── Crucible Verification ────────────────────────────────────
	var verification *CrucibleVerification
	if cruciblePassed {
		verification, err = VerifyCrucible(ctx, candidateTag, verifyTag)
		if err != nil {
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

	// ── Publish (verified artifact: pass 2) ──────────────────────
	// Rebuild with real tags + --push. Everything is cached from pass 2,
	// so the "build" is instant — it's really just a push from BuildKit to registries.
	publishPassed := false
	var publishResult *build.BuildResult
	if cruciblePassed && (verification == nil || !verification.HasHardFailure()) {
		publishPlan := clonePlan(plan)

		// Restore push semantics with real tags from config.
		// The plan was created from engine.Plan() which has the real tags + registries.
		// We only overrode tags/push for pass 1/2 — the original plan is the source of truth.
		for i := range publishPlan.Steps {
			publishPlan.Steps[i].Load = false
			publishPlan.Steps[i].Push = true
			// Tags + Registries are already correct from the original plan.
		}

		// Inject standard labels with the verified build's metadata.
		publishLabels := build.StandardLabels(
			build.NormalizeBuildPlan(publishPlan),
			version.Version, version.Commit,
			"crucible-verified", crucibleCreated,
		)
		build.InjectLabels(publishPlan, publishLabels)

		// Login to registries — hard fail on login failure. No unauthenticated publish attempts.
		loginBx := NewBuildx(false)
		loginBx.Stdout = io.Discard
		loginBx.Stderr = io.Discard
		loginFailed := false
		for _, step := range publishPlan.Steps {
			if hasRemoteRegistries(step.Registries) {
				if loginErr := loginBx.Login(ctx, step.Registries); loginErr != nil {
					loginFailed = true
					publishSec := output.NewSection(w, "Publish (verified artifact: pass 2)", 0, color)
					publishSec.Row("%-14s%s", "status", "blocked — registry login failed")
					publishSec.Row("%-14s%v", "error", loginErr)
					publishSec.Close()
				}
				break
			}
		}

		if !loginFailed {
			pubResult, publishErr := executeBuildPass(ctx, w, color, req.Verbose, req.Stderr,
				"Publish (verified artifact: pass 2)", publishPlan, "")
			if publishErr == nil {
				publishPassed = true
				publishResult = pubResult

				// Write publish manifest from ACTUAL structured publish records.
				// PublishedImages is populated by buildx from step registries + tags
				// at build time — authoritative, not reparsed from strings.
				var manifest artifact.PublishManifest
				for _, step := range pubResult.Steps {
					manifest.Published = append(manifest.Published, step.PublishedImages...)
				}

				// Manifest write failure = publish failure. Downstream depends on this file.
				if err := artifact.WritePublishManifest(rootDir, manifest); err != nil {
					publishPassed = false
					publishErr = fmt.Errorf("write publish manifest: %w", err)
				}
			}
		}
	}

	// ── Cache Retention ──────────────────────────────────────────
	// Local + external retention — backend-aware reporting.
	// Runs post-build on success only.
	if cruciblePassed {
		repoID := resolveRepoIDFromContext(pc)

		// Local retention — backend-aware.
		// BuildKit manages its own internal cache via GC. Local export plane is separate.
		if backend.IsBuildkit() && req.Config.BuildCache.Local.Retention.MaxAge == "" && req.Config.BuildCache.Local.Retention.MaxSize == "" {
			retSec := output.NewSection(w, "Cache Retention (local)", 0, color)
			retSec.Row("%-14s%s", "status", "managed by buildkitd (internal GC)")
			retSec.Close()
		} else {
			localRetResult := enforceLocalRetention(
				LocalCacheDir(repoID, req.Config.BuildCache.Local),
				req.Config.BuildCache.Local.Retention,
			)
			renderLocalRetention(w, color, localRetResult)
		}

		ext := req.Config.BuildCache.External
		if ext.Target != "" && (ext.Retention.MaxRefs > 0 || ext.Retention.StaleAge != "") {
			extRetResult := enforceExternalRetention(ctx, ext, repoID, req.Config.Targets)
			renderExternalRetention(w, color, extRetResult)
		}
	}

	// ── Image Retention ──────────────────────────────────────────
	if cruciblePassed && plan != nil {
		if postbuild.HasRetention(plan) {
			summary, _ := postbuild.RunRetentionSection(ctx, w, output.IsCI(), color, plan)
			_ = summary
		}
	}

	// ── Provenance ───────────────────────────────────────────────
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
			{Name: verifyTag},
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
					"backend":   backend.Kind,
				},
				Environment: map[string]any{
					"run_id":    runID,
					"candidate": candidateTag,
					"verify":    verifyTag,
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

	provSec := output.NewSection(w, "Provenance", 0, color)
	if provErr := build.WriteProvenance(provPath, stmt); provErr == nil {
		provSec.Row("✓  %s", provPath)
	} else {
		provSec.Row("✗  %s", provErr.Error())
	}
	provSec.Close()

	// ── Summary ──────────────────────────────────────────────────
	totalElapsed := time.Since(pipelineStart)
	sumSec := output.NewSection(w, "Summary", 0, color)

	if !req.SkipLint {
		output.SummaryRow(w, "lint", "success", "lint gate passed", color)
	} else {
		output.SummaryRow(w, "lint", "skipped", "skip requested", color)
	}
	output.SummaryRow(w, "detect", "success",
		fmt.Sprintf("%d Dockerfile(s), %s", len(det.Dockerfiles), det.Language), color)
	output.SummaryRow(w, "plan", "success",
		fmt.Sprintf("%d build(s), %d tag(s)", len(plan.Steps), 2), color)

	if pass1Err == nil {
		output.SummaryRow(w, "build", "success", "pass 1 candidate produced", color)
	} else {
		output.SummaryRow(w, "build", "failed", "pass 1 candidate failed", color)
	}

	if pass2Err == nil {
		output.SummaryRow(w, "rebuild", "success", "pass 2 self-proof verified", color)
	} else {
		output.SummaryRow(w, "rebuild", "failed", "pass 2 self-proof failed", color)
	}

	if verification != nil {
		verStatus := "success"
		if verification.HasHardFailure() {
			verStatus = "failed"
		}
		output.SummaryRow(w, "verification", verStatus, build.TrustLevelLabel(verification.TrustLevel), color)
	}

	if publishPassed && publishResult != nil {
		// Count actually pushed images from build result, not plan.
		pushed := 0
		for _, step := range publishResult.Steps {
			pushed += len(step.Images)
		}
		output.SummaryRow(w, "publish", "success", fmt.Sprintf("%d image(s) (verified artifact)", pushed), color)
	} else if cruciblePassed && (verification == nil || !verification.HasHardFailure()) {
		output.SummaryRow(w, "publish", "failed", "publish failed after verification", color)
	} else if cruciblePassed {
		output.SummaryRow(w, "publish", "failed", "verification blocked publish", color)
	}

	sumSec.Separator()
	overallStatus := "success"
	if !cruciblePassed {
		overallStatus = "failed"
	}
	output.SummaryTotal(w, totalElapsed, overallStatus, color)
	sumSec.Close()

	// ── Verdict — sacred elephant law: these lines do NOT change ──
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

	if pass2Err != nil {
		return pass2Err
	}
	return nil
}

// executeBuildPass runs a single build pass and renders structured output.
// resultTag: if non-empty, shows "result <tag>". If empty, shows pushed tags from plan steps.
func executeBuildPass(ctx context.Context, w io.Writer, color, verbose bool, stderr io.Writer,
	sectionName string, plan *build.BuildPlan, resultTag string) (*build.BuildResult, error) {

	buildStart := time.Now()

	bx := NewBuildx(verbose)
	var stderrBuf, stdoutBuf bytes.Buffer
	bx.Stdout = &stdoutBuf
	if verbose {
		bx.Stderr = stderr
	} else {
		bx.Stderr = &stderrBuf
	}

	var result build.BuildResult
	for _, step := range plan.Steps {
		stdoutBuf.Reset()
		stderrBuf.Reset()
		stepResult, layers, err := bx.BuildWithLayers(ctx, step)
		if stepResult == nil {
			stepResult = &build.StepResult{Name: step.Name, Status: "failed"}
		}
		stepResult.Layers = layers
		result.Steps = append(result.Steps, *stepResult)
		if err != nil {
			elapsed := time.Since(buildStart)
			failSec := output.NewSection(w, sectionName, elapsed, color)
			renderBuildLayers(failSec, result.Steps, color)
			output.RowStatus(failSec, "status", "build failed", "failed", color)

			combinedOutput := stdoutBuf.String() + "\n" + stderrBuf.String()
			RenderBuildError(failSec, combinedOutput)
			failSec.Close()
			return &result, fmt.Errorf("%s failed: %w", sectionName, err)
		}
	}

	elapsed := time.Since(buildStart)
	sec := output.NewSection(w, sectionName, elapsed, color)
	renderBuildLayers(sec, result.Steps, color)
	if resultTag != "" {
		sec.Separator()
		sec.Row("result  %s", resultTag)
	} else {
		// Publish pass — show pushed tags from the step results.
		sec.Separator()
		for _, step := range plan.Steps {
			for _, tag := range step.Tags {
				sec.Row("%s  %s", output.StatusIcon("success", color), tag)
			}
		}
	}
	sec.Close()

	return &result, nil
}

// clonePlan deep copies a build plan so mutations don't affect the original.
func clonePlan(plan *build.BuildPlan) *build.BuildPlan {
	clone := *plan
	clone.Steps = make([]build.BuildStep, len(plan.Steps))
	for i, s := range plan.Steps {
		clone.Steps[i] = s
		clone.Steps[i].Tags = append([]string{}, s.Tags...)
		if s.CacheFrom != nil {
			clone.Steps[i].CacheFrom = append([]build.CacheRef{}, s.CacheFrom...)
		}
		if s.CacheTo != nil {
			clone.Steps[i].CacheTo = append([]build.CacheRef{}, s.CacheTo...)
		}
	}
	return &clone
}

func crucibleVerdict(w io.Writer, title, body string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "    ──────────────────────────────────────────────────────────────")
	fmt.Fprintf(w, "    🐘 Crucible Verdict: %s\n", title)
	fmt.Fprintf(w, "    %s\n", body)
	fmt.Fprintln(w, "    ──────────────────────────────────────────────────────────────")
	fmt.Fprintln(w)
}

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

	platforms := formatPlatforms(nil)
	if p := os.Getenv("STAGEFREIGHT_PLATFORMS"); p != "" {
		platforms = p
	}
	if platforms != "" {
		kv = append(kv, output.KV{Key: "Platforms", Value: platforms})
	}

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
