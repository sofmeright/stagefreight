package pipeline

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/cistate"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/lint/modules"
	"github.com/PrPlanIT/StageFreight/src/output"
	"github.com/PrPlanIT/StageFreight/src/runner"
	"github.com/PrPlanIT/StageFreight/src/trace"
	"github.com/PrPlanIT/StageFreight/src/version"
)

// FailureDetail captures operator-meaningful error context for the Exit Reason section.
type FailureDetail struct {
	Command  string // "docker push cr.pcfae.com/prplanit/stagefreight:dev-ff98a93"
	ExitCode int    // 1
	Reason   string // "HTTP 500 (registry)"
	Stderr   string // raw stderr for --verbose
}

// PhaseResult is what each phase reports for the summary table.
type PhaseResult struct {
	Name    string
	Status  string // "success", "failed", "skipped"
	Summary string // one-line detail
	Elapsed time.Duration
	Details map[string]string // optional structured metadata
	Failure *FailureDetail    // operator-facing error context; nil on success
}

// Phase is a named unit of pipeline work.
type Phase struct {
	Name string
	Run  func(pc *PipelineContext) (*PhaseResult, error)
}

// PostBuildHook is optional post-build work with a condition gate.
// nil Condition means "always run".
type PostBuildHook struct {
	Name      string
	Condition func(pc *PipelineContext) bool
	Run       func(pc *PipelineContext) (*PhaseResult, error)
}

// BannerPhase renders the StageFreight banner and code identity block.
// Code panel: Commit + Branch/Tag only. No pipeline, runner, platforms, or
// registries — those belong to their domain panels (Execution, Plan, Result).
func BannerPhase() Phase {
	return Phase{
		Name: "banner",
		Run: func(pc *PipelineContext) (*PhaseResult, error) {
			output.Banner(pc.Writer, output.NewBannerInfo(version.Version, version.Commit, ""), pc.Color)
			output.ContextBlock(pc.Writer, CIContextKV(), pc.Color)
			return &PhaseResult{
				Name:   "banner",
				Status: "success",
			}, nil
		},
	}
}

// RunnerPreflightPhase runs execution substrate checks and renders the Runner section.
// This is the DomainExecution panel — it absorbs Pipeline ID, Runner name,
// engine detection, and substrate health from the former ContextBlock.
//
// Skip conditions: crucible child pass (pass-2 substrate is not meaningful to report).
func RunnerPreflightPhase(opts runner.Options) Phase {
	return Phase{
		Name: "runner",
		Run: func(pc *PipelineContext) (*PhaseResult, error) {
			if build.IsCrucibleChild() {
				return &PhaseResult{Name: "runner", Status: "skipped", Summary: "crucible child"}, nil
			}

			start := time.Now()
			report := runner.Run(pc.RootDir, opts)

			RenderRunnerSection(pc.Writer, report, opts, pc.Color, time.Since(start))

			if err := cistate.UpdateState(pc.RootDir, func(st *cistate.State) {
				st.Runner = report
			}); err != nil {
				diag.Warn("runner preflight: state write failed: %v", err)
			}

			switch report.Health {
			case runner.Unhealthy:
				return &PhaseResult{
					Name:    "runner",
					Status:  "failed",
					Summary: "substrate unhealthy",
				}, fmt.Errorf("runner preflight: substrate unhealthy — pipeline aborted")
			case runner.Degraded:
				var warnCount int
				for _, f := range report.Findings {
					if f.Status == "fail" || f.Status == "warn" {
						warnCount++
					}
				}
				return &PhaseResult{
					Name:    "runner",
					Status:  "warning",
					Summary: fmt.Sprintf("%d warning(s)", warnCount),
				}, nil
			default:
				return &PhaseResult{Name: "runner", Status: "success"}, nil
			}
		},
	}
}

// renderRunnerSection renders the DomainExecution panel box.
// Row order is fixed: identity → separator → substrate → separator → health → [findings].
// Rule 8: substrate rows always present when fact exists.
// Rule 10: info-severity findings never appear as finding rows.
// RunnerPreflightWithWriter is the exported equivalent of the cmd-package
// runnerPreflight helper, for callers that own their own io.Writer (e.g. crucible).
// Runs substrate assessment, renders the Runner panel to w, persists to cistate.
// Returns the report so callers can inspect Health and abort on Unhealthy.
func RunnerPreflightWithWriter(w io.Writer, rootDir string, opts runner.Options, color bool) runner.ExecutionReport {
	start := time.Now()
	report := runner.Run(rootDir, opts)
	RenderRunnerSection(w, report, opts, color, time.Since(start))
	if stErr := cistate.UpdateState(rootDir, func(st *cistate.State) { st.Runner = report }); stErr != nil {
		fmt.Fprintf(w, "warning: pipeline state write failed: %v\n", stErr)
	}
	return report
}

// RenderRunnerSection renders the DomainExecution panel box.
// Exported for callers that need to render without running preflight (e.g. tests).
func RenderRunnerSection(w io.Writer, report runner.ExecutionReport, opts runner.Options, color bool, elapsed time.Duration) {
	sec := output.NewSection(w, "Runner", elapsed, color)

	// ── Identity rows ──────────────────────────────────────────────────────────
	// Engine + Run (InvocationID) always first and always paired.
	sec.Row("%-12s%-20s%-10s%s", "Engine", string(report.Engine), "Run", report.InvocationID)

	id := report.Identity
	switch report.Engine {
	case runner.EngineGitLab:
		if id.PipelineID != "" || id.JobID != "" {
			sec.Row("%-12s%-20s%-10s%s", "Pipeline", id.PipelineID, "Job", id.JobID)
		}
		if id.Name != "" {
			sec.Row("%-12s%s", "Runner", id.Name)
		}
	case runner.EngineGitHub:
		if id.Workflow != "" || id.JobID != "" {
			sec.Row("%-12s%-20s%-10s%s", "Workflow", id.Workflow, "Job", id.JobID)
		}
		if id.Name != "" {
			sec.Row("%-12s%s", "Runner", id.Name)
		}
	case runner.EngineForgejo, runner.EngineGitea:
		if id.PipelineID != "" {
			sec.Row("%-12s%s", "Pipeline", id.PipelineID)
		}
		if id.Name != "" {
			sec.Row("%-12s%s", "Runner", id.Name)
		}
	case runner.EngineStageFreight:
		if id.Controller != "" {
			sec.Row("%-12s%s", "Controller", id.Controller)
		}
		if id.Satellite != "" {
			sec.Row("%-12s%s", "Satellite", id.Satellite)
		}
	}

	sec.Separator()

	// ── Substrate rows ─────────────────────────────────────────────────────────
	f := report.Facts

	// workspace + free
	wsLabel := "writable"
	wsIcon := output.StatusIcon("success", color)
	if !f.StagefreightWritable {
		wsLabel = "not writable"
		wsIcon = output.StatusIcon("failed", color)
	}
	diskIcon := runnerFindingIcon(report.Findings, color, "disk_critical", "disk_low")
	sec.Row("%-12s%-20s%-10s%s", "workspace", wsLabel+" "+wsIcon, "free", formatRunnerMB(f.DiskFreeMB)+" "+diskIcon)

	// memory + cpu
	memVal := "-"
	if f.MemAvailableMB >= 0 {
		memIcon := runnerFindingIcon(report.Findings, color, "memory_low")
		memVal = formatRunnerMB(f.MemAvailableMB) + " " + memIcon
	}
	cpuVal := "-"
	if f.CPULoadAvg1 >= 0 {
		cpuVal = fmt.Sprintf("%.2f avg", f.CPULoadAvg1)
	}
	sec.Row("%-12s%-20s%-10s%s", "memory", strings.TrimSpace(memVal), "cpu", cpuVal)

	// policy + thresholds — makes health evaluation traceable (Rule 15)
	// Operator sees WHY a finding fired at a given threshold, and whether crucible elevated it.
	policyLabel := "standard"
	if opts.IsCrucible {
		policyLabel = "crucible (elevated)"
	}
	memThresh := int64(512)
	diskThresh := int64(2048)
	if opts.IsCrucible {
		memThresh *= 2
		diskThresh *= 2
	}
	if opts.MemWarnMB > 0 {
		memThresh = opts.MemWarnMB
	}
	if opts.DiskWarnMB > 0 {
		diskThresh = opts.DiskWarnMB
	}
	sec.Row("%-12s%-20s%-12s%s", "policy", policyLabel, "thresholds",
		fmt.Sprintf("mem≥%dMB disk≥%dMB", memThresh, diskThresh))

	// docker + buildkit — always rendered; severity affects icon/wording, not presence
	sec.Row("%-12s%-20s%-10s%s", "docker",
		formatRunnerDockerStatus(f.DockerAvailable, opts.DockerRequired, color),
		"buildkit",
		formatRunnerBuildkitStatus(f.BuildKitAvailable, opts.DockerRequired, color))

	// dind + buildx — always rendered; both are informational (no icon — Rule 10)
	dindLabel := "not detected"
	if f.DindDetected {
		dindLabel = "detected"
	}
	sec.Row("%-12s%-20s%-10s%s", "dind", dindLabel, "buildx",
		formatRunnerBuildxStatus(f.BuildxAvailable))

	sec.Separator()

	// ── Health line ────────────────────────────────────────────────────────────
	sec.Row("%-12s%s %s", "health", string(report.Health), output.StatusIcon(runnerHealthStatus(report.Health), color))

	// ── Findings block (warn/fail severity only — no info per Rule 10) ─────────
	var actionable []runner.Finding
	for _, finding := range report.Findings {
		if finding.Severity != "info" && (finding.Status == "warn" || finding.Status == "fail") {
			actionable = append(actionable, finding)
		}
	}
	if len(actionable) > 0 {
		sec.Separator()
		for _, finding := range actionable {
			icon := output.StatusIcon("warning", color)
			if finding.Status == "fail" {
				icon = output.StatusIcon("failed", color)
			}
			sec.Row("%s  %-18s%s", icon, finding.ID, finding.Detail)
		}
	}

	sec.Close()
}

// runnerFindingIcon returns the icon for the worst finding matching any of the given IDs.
func runnerFindingIcon(findings []runner.Finding, color bool, ids ...string) string {
	worst := "ok"
	for _, f := range findings {
		for _, id := range ids {
			if f.ID == id && f.Status != "ok" {
				if f.Status == "fail" {
					worst = "fail"
				} else if worst != "fail" {
					worst = "warn"
				}
			}
		}
	}
	switch worst {
	case "fail":
		return output.StatusIcon("failed", color)
	case "warn":
		return output.StatusIcon("warning", color)
	default:
		return output.StatusIcon("success", color)
	}
}

func formatRunnerMB(mb int64) string {
	if mb < 0 {
		return "-"
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

func formatRunnerDockerStatus(available, required bool, color bool) string {
	if available {
		if required {
			return "available " + output.StatusIcon("success", color)
		}
		return "available" // no icon — info only (Rule 10)
	}
	if required {
		return "not available " + output.StatusIcon("failed", color)
	}
	return "not present" // no icon, no alarm — info only
}

func formatRunnerBuildkitStatus(available, required bool, color bool) string {
	if available {
		if required {
			return "available " + output.StatusIcon("success", color)
		}
		return "available"
	}
	if required {
		return "not available " + output.StatusIcon("failed", color)
	}
	return "not available"
}

// formatRunnerBuildxStatus is always informational — buildx absence is not
// a health finding. We don't yet have "multi-platform build planned" signal,
// so no severity threshold can be applied. No icon, always plain text.
func formatRunnerBuildxStatus(available bool) string {
	if available {
		return "available"
	}
	return "not available"
}

func runnerHealthStatus(h runner.HealthGrade) string {
	switch h {
	case runner.Healthy:
		return "success"
	case runner.Degraded:
		return "warning"
	default:
		return "failed"
	}
}

// LintPhase runs the pre-build lint gate.
// Returns a phase that skips if pc.SkipLint is true.
func LintPhase() Phase {
	return Phase{
		Name: "lint",
		Run: func(pc *PipelineContext) (*PhaseResult, error) {
			if pc.SkipLint {
				return &PhaseResult{
					Name:    "lint",
					Status:  "skipped",
					Summary: "--skip-lint",
				}, nil
			}

			output.SectionStart(pc.Writer, "sf_lint", "Lint")
			summary, err := runPreBuildLintImpl(pc.Ctx, pc.RootDir, pc.Config, pc.CI, pc.Color, pc.Verbose, pc.Writer)
			output.SectionEnd(pc.Writer, "sf_lint")

			if err != nil {
				return &PhaseResult{
					Name:    "lint",
					Status:  "failed",
					Summary: summary,
				}, err
			}

			return &PhaseResult{
				Name:    "lint",
				Status:  "success",
				Summary: summary,
			}, nil
		},
	}
}

// runPreBuildLintImpl is the extracted lint logic, independent of package-level vars.
func runPreBuildLintImpl(ctx context.Context, rootDir string, appCfg *config.Config, ci bool, color bool, isVerbose bool, w io.Writer) (string, error) {
	cacheDir := lint.ResolveCacheDir(rootDir, appCfg.Lint.CacheDir)
	cache := &lint.Cache{
		Dir:     cacheDir,
		Enabled: true,
	}

	lintEngine, err := lint.NewEngine(appCfg.Lint, rootDir, nil, nil, isVerbose, cache)
	if err != nil {
		return "", err
	}

	files, err := lintEngine.CollectFiles()
	if err != nil {
		return "", err
	}

	// Delta filtering — skip when config requests full scan.
	if appCfg.Lint.Level != config.LevelFull {
		delta := &lint.Delta{RootDir: rootDir, TargetBranch: appCfg.Lint.TargetBranch, Verbose: isVerbose}
		changedSet, _ := delta.ChangedFiles(ctx)
		if changedSet != nil {
			files = lint.FilterByDelta(files, changedSet)
		}
	}

	start := time.Now()
	findings, modStats, runErr := lintEngine.RunWithStats(ctx, files)
	findings = append(findings, modules.CheckFilenameCollisions(files)...)
	elapsed := time.Since(start)

	// Tally
	var critical, warning, info int
	var totalFiles, totalCached int
	for _, f := range findings {
		switch f.Severity {
		case lint.SeverityCritical:
			critical++
		case lint.SeverityWarning:
			warning++
		case lint.SeverityInfo:
			info++
		}
	}
	for _, ms := range modStats {
		totalFiles += ms.Files
		totalCached += ms.Cached
	}

	// Write JUnit XML in CI for GitLab test reporting
	if ci {
		moduleNames := lintEngine.ModuleNames()
		if jErr := output.WriteLintJUnit(".stagefreight/reports", findings, files, moduleNames, elapsed); jErr != nil {
			diag.Warn("failed to write junit report: %v", jErr)
		}
	}

	// Section output
	sec := output.NewSection(w, "Lint", elapsed, color)
	output.LintTable(w, modStats, color)
	sec.Separator()
	sec.Row("%-16s%5d   %5d   %d findings (%d critical)",
		"total", totalFiles, totalCached, len(findings), critical)
	sec.Close()

	if len(findings) > 0 {
		fSec := output.NewSection(w, "Findings", 0, color)
		output.SectionFindings(fSec, findings, color)
		fSec.Separator()
		fSec.Row("%s", output.FindingsSummaryLine(len(findings), critical, warning, info, len(files), color))
		fSec.Close()
	}

	// Evict stale lint cache entries after run.
	// Touch-on-read (in Cache.Get) marks active entries, so eviction
	// only removes dead entries (old file versions never read again).
	evictResult := cache.Evict(appCfg.Lint.Cache.MaxAge, appCfg.Lint.Cache.MaxSize)
	if evictResult.Evicted > 0 || evictResult.Reason != "" {
		sec = output.NewSection(w, "Lint Cache Eviction", 0, color)
		if evictResult.Reason != "" {
			sec.Row("%-14s%s", "status", "skipped")
			sec.Row("%-14s%s", "reason", evictResult.Reason)
		} else {
			sec.Row("%-14s%d", "before", evictResult.EntriesBefore)
			sec.Row("%-14s%d", "evicted", evictResult.Evicted)
			sec.Row("%-14s%s", "reclaimed", formatEvictBytes(evictResult.EvictedBytes))
		}
		sec.Close()
	}

	if critical > 0 {
		summary := fmt.Sprintf("%d files, %d cached, %d critical", len(files), totalCached, critical)
		return summary, fmt.Errorf("lint failed: %d critical findings", critical)
	}

	summary := fmt.Sprintf("%d files, %d cached, 0 critical", len(files), totalCached)
	if warning > 0 {
		summary = fmt.Sprintf("%d files, %d cached, %d warnings", len(files), totalCached, warning)
	}

	if runErr != nil && isVerbose {
		diag.Warn("lint: %v", runErr)
	}

	return summary, nil
}

func formatEvictBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// DryRunGate checks pc.DryRun and, if true, calls renderPlan then returns ErrDryRunExit.
func DryRunGate(renderPlan func(pc *PipelineContext)) Phase {
	return Phase{
		Name: "dry-run",
		Run: func(pc *PipelineContext) (*PhaseResult, error) {
			if !pc.DryRun {
				return &PhaseResult{
					Name:   "dry-run",
					Status: "skipped",
				}, nil
			}
			if renderPlan != nil {
				renderPlan(pc)
			}
			return &PhaseResult{
				Name:    "dry-run",
				Status:  "success",
				Summary: "plan rendered",
			}, ErrDryRunExit
		},
	}
}

// PublishManifestPhase writes the accumulated publish manifest.
// No-op (records "skipped") when the manifest has no artifacts.
func PublishManifestPhase() Phase {
	return Phase{
		Name: "publish",
		Run: func(pc *PipelineContext) (*PhaseResult, error) {
			m := &pc.Manifest
			hasArtifacts := len(m.Published) > 0 || len(m.Binaries) > 0 || len(m.Archives) > 0

			if !hasArtifacts {
				return &PhaseResult{
					Name:    "publish",
					Status:  "skipped",
					Summary: "no artifacts",
				}, nil
			}

			// Merge with existing manifest (binary builds may have already written)
			existing, err := artifact.ReadPublishManifest(pc.RootDir)
			if err == nil {
				existing.Published = append(existing.Published, m.Published...)
				existing.Binaries = append(existing.Binaries, m.Binaries...)
				existing.Archives = append(existing.Archives, m.Archives...)
				m = existing
			}

			if err := artifact.WritePublishManifest(pc.RootDir, *m); err != nil {
				return &PhaseResult{
					Name:    "publish",
					Status:  "failed",
					Summary: err.Error(),
				}, fmt.Errorf("writing publish manifest: %w", err)
			}

			count := len(m.Published) + len(m.Binaries) + len(m.Archives)
			return &PhaseResult{
				Name:    "publish",
				Status:  "success",
				Summary: fmt.Sprintf("%d artifact(s)", count),
			}, nil
		},
	}
}

// CollectTargetsByKind returns all targets matching the given kind.
func CollectTargetsByKind(cfg *config.Config, kind string) []config.TargetConfig {
	var targets []config.TargetConfig
	for _, t := range cfg.Targets {
		if t.Kind == kind {
			targets = append(targets, t)
		}
	}
	return targets
}

// RunLint runs the lint gate using the given parameters.
// Portable entry point for callers without a PipelineContext (all CI runners).
// Returns a one-line summary and a non-nil error only on critical findings or engine failure.
func RunLint(ctx context.Context, appCfg *config.Config, rootDir string, isCI bool, color bool, verbose bool, w io.Writer) (string, error) {
	return runPreBuildLintImpl(ctx, rootDir, appCfg, isCI, color, verbose, w)
}

// sectionProvenanceIcon returns the display icon for a config section's provenance.
//
//	📋 manifest — operator declared it in .stagefreight.yml
//	♻️  preset   — preset engine activated it
func sectionProvenanceIcon(provenance string) string {
	switch provenance {
	case "manifest":
		return "📋"
	case "preset":
		return "♻️"
	default:
		return ""
	}
}

// ConfigPhase renders the DomainConfig panel from a ConfigReport.
// Always present — surfaces the "Explain" layer of config resolution (Rule 11, Rule 14).
// Status is "partial" when preset resolution failed or was incomplete.
//
// Emission-driven: all config truth is emitted to pc.Trace first.
// Panel renders exclusively from pc.Trace.ForDomain("config").
// The global contract check at end of Pipeline.Run catches anything not rendered.
func ConfigPhase(report config.ConfigReport) Phase {
	return Phase{
		Name: "config",
		Run: func(pc *PipelineContext) (*PhaseResult, error) {
			// — Emit config truth ——————————————————————————————————————————
			pc.Trace.Public("config", trace.CategoryInput, "source", report.SourceFile, "config", trace.StatusOK)

			if len(report.Presets) > 0 {
				pc.Trace.Public("config", trace.CategoryInput, "presets", strings.Join(report.Presets, " → "), "config", trace.StatusOK)
				if report.Overrides > 0 {
					pc.Trace.Public("config", trace.CategoryInput, "overrides", fmt.Sprintf("%d", report.Overrides), "config", trace.StatusOK)
				}
			} else {
				pc.Trace.Decision("config", "presets", "none", "no presets configured", "config", trace.StatusInfo)
			}

			// Active/inactive section display — provenance per active section, dormant capability domains.
			var activeParts, inactiveParts []string
			for _, s := range report.Sections {
				if s.Active {
					activeParts = append(activeParts, s.Name+" "+sectionProvenanceIcon(s.Provenance))
				} else if s.Kind == "capability" {
					inactiveParts = append(inactiveParts, s.Name+" ⛔")
				}
			}
			if len(activeParts) > 0 {
				pc.Trace.Public("config", trace.CategoryInput, "active", strings.Join(activeParts, "   "), "config", trace.StatusOK)
			}
			if len(inactiveParts) > 0 {
				pc.Trace.Decision("config", "inactive", strings.Join(inactiveParts, "   "), "capability domains not configured", "config", trace.StatusInfo)
			}

			if report.VarsApplied > 0 {
				pc.Trace.Public("config", trace.CategoryInput, "vars", fmt.Sprintf("%d applied", report.VarsApplied), "config", trace.StatusOK)
			}

			for i, w := range report.Warnings {
				pc.Trace.PublicDetail("config", trace.CategoryDecision, fmt.Sprintf("warning_%d", i+1), w, w, "config", trace.StatusWarn)
			}

			if report.Error != "" {
				pc.Trace.PublicDetail("config", trace.CategoryDecision, "error", report.Error, report.Error, "config", trace.StatusFail)
			}

			resStatus := trace.StatusOK
			if report.Status == "partial" {
				resStatus = trace.StatusWarn
			} else if report.Status == "error" {
				resStatus = trace.StatusFail
			}
			pc.Trace.Decision("config", "resolution", report.Status, "config resolution completeness", "resolver", resStatus)

			modelStatus := trace.StatusOK
			modelValue := report.Completeness
			if report.Completeness != "complete" {
				modelStatus = trace.StatusWarn
				modelValue = report.Completeness + " (resolution incomplete)"
			}
			pc.Trace.Decision("config", "model", modelValue, "truth completeness signal", "resolver", modelStatus)

			// — Render from emissions ONLY ——————————————————————————————————
			sec := output.NewSection(pc.Writer, "Config", 0, pc.Color)
			for _, e := range pc.Trace.ForDomain("config") {
				icon := ""
				switch e.Status {
				case trace.StatusOK:
					icon = " " + output.StatusIcon("success", pc.Color)
				case trace.StatusWarn:
					icon = " " + output.StatusIcon("warning", pc.Color)
				case trace.StatusFail:
					icon = " " + output.StatusIcon("failed", pc.Color)
				}
				sec.Row("%-16s%s%s", e.Key, e.RenderValue(), icon)
				pc.Trace.MarkRendered(e)
			}
			sec.Close()

			if err := cistate.UpdateState(pc.RootDir, func(st *cistate.State) {
				st.Config = report
			}); err != nil {
				diag.Warn("config phase: state write failed: %v", err)
			}

			if report.Status == "error" {
				return &PhaseResult{Name: "config", Status: "failed", Summary: report.Error},
					fmt.Errorf("config resolution failed: %s", report.Error)
			}
			phaseStatus := "success"
			if report.Status == "partial" {
				phaseStatus = "warning"
			}
			return &PhaseResult{Name: "config", Status: phaseStatus}, nil
		},
	}
}

// RenderContractPanel renders the enforcement panel when unrendered emissions exist.
// Called at end of Pipeline.Run() — never by phase code.
// Groups unrendered emissions by domain for operator readability.
func RenderContractPanel(w io.Writer, unrendered []trace.Emission, color bool) {
	if len(unrendered) == 0 {
		return
	}
	sec := output.NewSection(w, "Contract", 0, color)
	sec.Row("%-16s%s %s", "status", "partial", output.StatusIcon("warning", color))
	sec.Row("%-16s%d emissions", "unrendered", len(unrendered))

	// Group by domain
	byDomain := make(map[string][]trace.Emission)
	var domainOrder []string
	seen := make(map[string]bool)
	for _, e := range unrendered {
		if !seen[e.Domain] {
			domainOrder = append(domainOrder, e.Domain)
			seen[e.Domain] = true
		}
		byDomain[e.Domain] = append(byDomain[e.Domain], e)
	}

	sec.Separator()
	for _, domain := range domainOrder {
		for _, e := range byDomain[domain] {
			icon := output.StatusIcon("warning", color)
			if e.Status == trace.StatusFail {
				icon = output.StatusIcon("failed", color)
			}
			sec.Row("%s  %-30s%s", icon, domain+"/"+e.Key, e.Source)
		}
	}
	sec.Close()
}
