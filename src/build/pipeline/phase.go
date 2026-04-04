package pipeline

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/PrPlanIT/StageFreight/src/artifact"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/lint/modules"
	"github.com/PrPlanIT/StageFreight/src/output"
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

// BannerPhase renders the StageFreight banner and CI context block.
// extraKV lets each command add engine-specific key-value pairs (registry counts, platforms, etc.).
func BannerPhase(extraKV func(*PipelineContext) []output.KV) Phase {
	return Phase{
		Name: "banner",
		Run: func(pc *PipelineContext) (*PhaseResult, error) {
			output.Banner(pc.Writer, output.NewBannerInfo(version.Version, version.Commit, ""), pc.Color)

			kv := CIContextKV()
			if extraKV != nil {
				kv = append(kv, extraKV(pc)...)
			}
			output.ContextBlock(pc.Writer, kv)

			return &PhaseResult{
				Name:   "banner",
				Status: "success",
			}, nil
		},
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
