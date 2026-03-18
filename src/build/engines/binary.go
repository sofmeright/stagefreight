package engines

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build"
)

func init() {
	build.RegisterV2("binary", func() build.EngineV2 { return &binaryEngine{} })
}

// binaryEngine compiles Go binaries. Plan + ExecuteStep only.
// All orchestration (ordering, concurrency, logging, artifact recording,
// checksums, publish manifest) lives in core.
type binaryEngine struct{}

func (e *binaryEngine) Name() string { return "binary" }

func (e *binaryEngine) Capabilities() build.Capabilities {
	return build.Capabilities{
		SupportsCrossCompile: true,
		SupportsCrucible:     true,
		ProducesArchives:     false, // archives are a target concern, not engine
		ProducesOCI:          false,
	}
}

func (e *binaryEngine) Detect(ctx context.Context, rootDir string) (*build.Detection, error) {
	det, err := build.DetectRepo(rootDir)
	if err != nil {
		return det, err
	}

	// Extend detection with Go main package discovery
	if det.Language == "go" {
		gb := build.NewGoBuild(false)
		mains, _ := gb.DetectMainPackages(rootDir)
		det.MainPackages = mains
	}

	return det, nil
}

func (e *binaryEngine) Plan(ctx context.Context, cfg build.BuildConfig) ([]build.UniversalStep, error) {
	if cfg.Builder != "go" {
		return nil, fmt.Errorf("binary engine: unsupported builder %q (supported: go)", cfg.Builder)
	}

	if cfg.From == "" {
		return nil, fmt.Errorf("binary engine: from is required")
	}

	// Resolve output artifact name
	binaryName := cfg.Output
	if binaryName == "" {
		// Default: basename of from path
		from := cfg.From
		if strings.HasSuffix(from, ".go") {
			from = filepath.Dir(from)
		}
		binaryName = filepath.Base(from)
	}

	// Resolve template variables in args
	args := make([]string, len(cfg.Args))
	for i, a := range cfg.Args {
		args[i] = resolveTemplateVars(a, cfg)
	}

	// Default env
	env := cfg.Env
	if env == nil {
		env = map[string]string{}
	}

	var steps []build.UniversalStep
	for _, plat := range cfg.Platforms {
		// Physical binary name: append .exe on Windows
		physicalName := binaryName
		if plat.OS == "windows" {
			physicalName += ".exe"
		}

		// Output path: dist/{os}-{arch}/{binary_name}
		outputPath := fmt.Sprintf("dist/%s-%s/%s", plat.OS, plat.Arch, physicalName)
		if cfg.Version != nil && cfg.Version.Version != "" {
			// Include version in path when available
			outputPath = fmt.Sprintf("dist/%s-%s/%s", plat.OS, plat.Arch, physicalName)
		}

		stepID := build.StepIDForPlatform(cfg.ID, plat)

		step := build.UniversalStep{
			BuildID:  cfg.ID,
			StepID:   stepID,
			Engine:   "binary",
			Platform: plat,
			Outputs: []build.ArtifactRef{
				{Path: outputPath, Type: "binary"},
			},
			Meta: BinaryMeta{
				From:       cfg.From,
				BinaryName: physicalName,
				OutputPath: outputPath,
				Args:       args,
				Env:        env,
				Compress:   cfg.Compress,
			},
		}

		steps = append(steps, step)
	}

	return steps, nil
}

func (e *binaryEngine) ExecuteStep(ctx context.Context, step build.UniversalStep) (*build.UniversalStepResult, error) {
	meta, ok := step.Meta.(BinaryMeta)
	if !ok {
		return nil, fmt.Errorf("binary engine: expected BinaryMeta, got %T", step.Meta)
	}

	start := time.Now()

	gb := build.NewGoBuild(false)

	// Get toolchain version for metadata
	toolchain, _ := gb.ToolchainVersion(ctx)

	result, err := gb.Build(ctx, build.GoBuildOpts{
		Entry:      meta.From,
		OutputPath: meta.OutputPath,
		GOOS:       step.Platform.OS,
		GOARCH:     step.Platform.Arch,
		Args:       meta.Args,
		Env:        meta.Env,
	})
	if err != nil {
		return nil, err
	}

	return &build.UniversalStepResult{
		Artifacts: []build.ProducedArtifact{
			{
				Path:   result.Path,
				Type:   "binary",
				Size:   result.Size,
				SHA256: result.SHA256,
			},
		},
		Metadata: map[string]string{
			"toolchain":   toolchain,
			"binary_name": meta.BinaryName,
		},
		Metrics: build.StepMetrics{
			Duration: time.Since(start),
		},
	}, nil
}

// resolveTemplateVars expands template variables in args and similar strings.
func resolveTemplateVars(s string, cfg build.BuildConfig) string {
	if cfg.Version != nil {
		s = strings.ReplaceAll(s, "{version}", cfg.Version.Version)
		s = strings.ReplaceAll(s, "{sha}", cfg.Version.SHA)
		// Support {sha:N} for truncated SHA
		for n := 1; n <= 40; n++ {
			placeholder := fmt.Sprintf("{sha:%d}", n)
			if strings.Contains(s, placeholder) {
				sha := cfg.Version.SHA
				if len(sha) > n {
					sha = sha[:n]
				}
				s = strings.ReplaceAll(s, placeholder, sha)
			}
		}
	}
	s = strings.ReplaceAll(s, "{date}", time.Now().UTC().Format(time.RFC3339))
	return s
}
