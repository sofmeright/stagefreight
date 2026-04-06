package docker

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitver"
	"github.com/PrPlanIT/StageFreight/src/registry"
)

func init() {
	build.Register("image", func() build.Engine { return &imageEngine{} })
}

// imageEngine builds container images and pushes to registries.
type imageEngine struct{}

func (e *imageEngine) Name() string { return "image" }

func (e *imageEngine) Detect(ctx context.Context, rootDir string) (*build.Detection, error) {
	return build.DetectRepo(rootDir)
}

func (e *imageEngine) Plan(ctx context.Context, cfgRaw interface{}, det *build.Detection) (*build.BuildPlan, error) {
	input, ok := cfgRaw.(*build.ImagePlanInput)
	if !ok {
		return nil, fmt.Errorf("image engine: expected *build.ImagePlanInput, got %T", cfgRaw)
	}
	cfg := input.Cfg

	plan := &build.BuildPlan{}

	// Resolve version for templates (tags, paths, URLs)
	versionInfo, _ := build.DetectVersion(det.RootDir)
	if versionInfo == nil {
		versionInfo = &build.VersionInfo{
			Version: "dev",
			Base:    "0.0.0",
			SHA:     "unknown",
			Branch:  "unknown",
		}
	}

	// Resolve current branch and tag for target filtering
	currentBranch := resolveBranch(det, versionInfo)
	currentGitTag := os.Getenv("CI_COMMIT_TAG")

	// Filter builds to kind: docker, optionally by --build ID
	var dockerBuilds []config.BuildConfig
	for _, b := range cfg.Builds {
		if b.Kind != "docker" {
			continue
		}
		if input.BuildID != "" && b.ID != input.BuildID {
			continue
		}
		dockerBuilds = append(dockerBuilds, b)
	}

	if len(dockerBuilds) == 0 {
		if input.BuildID != "" {
			return nil, fmt.Errorf("no docker build found with id %q", input.BuildID)
		}
		return nil, fmt.Errorf("no docker builds defined")
	}

	// One BuildStep per docker build entry
	for _, b := range dockerBuilds {
		step, err := planDockerBuild(b, cfg, det, versionInfo, currentBranch, currentGitTag)
		if err != nil {
			return nil, fmt.Errorf("build %q: %w", b.ID, err)
		}
		plan.Steps = append(plan.Steps, *step)
	}

	// Inject build cache flags from config.
	// CacheFrom (import) goes on all steps.
	// CacheTo (export) only on steps that push — auth is only available there.
	// Crucible/load-only steps get import but not export.
	if cfg.BuildCache.IsActive() {
		repoID := resolveRepoID(det, versionInfo)
		branch := currentBranch
		if branch == "" {
			branch = "default"
		}
		cacheFrom, cacheTo := BuildCacheFlags(cfg.BuildCache, repoID, branch, cfg.Targets)
		for i := range plan.Steps {
			plan.Steps[i].CacheFrom = cacheFrom
			if plan.Steps[i].Push {
				plan.Steps[i].CacheTo = cacheTo
			}
		}
	}

	return plan, nil
}

// planDockerBuild creates a BuildStep for a single docker build entry,
// resolving its registry targets from cfg.Targets.
func planDockerBuild(b config.BuildConfig, cfg *config.Config, det *build.Detection, versionInfo *gitver.VersionInfo, currentBranch, currentGitTag string) (*build.BuildStep, error) {
	// Resolve Dockerfile path
	dockerfile := b.Dockerfile
	if dockerfile == "" && len(det.Dockerfiles) > 0 {
		dockerfile = det.Dockerfiles[0].Path
	}
	if dockerfile == "" {
		return nil, fmt.Errorf("no Dockerfile found")
	}

	// Resolve context
	buildContext := b.Context
	if buildContext == "" {
		buildContext = "."
	}

	// Resolve platforms
	platforms := b.Platforms
	if len(platforms) == 0 {
		platforms = []string{fmt.Sprintf("linux/%s", runtime.GOARCH)}
	}

	// Collect registry targets that reference this build
	var tags []string
	var registries []build.RegistryTarget

	for i, t := range cfg.Targets {
		if t.Kind != "registry" || t.Build != b.ID {
			continue
		}

		// Check when conditions
		if !targetAllowed(t, currentBranch, currentGitTag, cfg.Policies) {
			continue
		}

		// Resolve registry identity from identity graph or legacy inline fields.
		resolved, err := config.ResolveRegistryForTarget(t, cfg.Registries, cfg.Vars)
		if err != nil {
			return nil, fmt.Errorf("target[%d] %q: %w", i, t.ID, err)
		}

		resolvedURL := build.ResolveTemplate(resolved.URL, versionInfo)
		resolvedPath := build.ResolveTemplate(resolved.Path, versionInfo)

		tagTemplates := make([]string, len(t.Tags))
		for j, tmpl := range t.Tags {
			tagTemplates[j] = gitver.ResolveVars(tmpl, cfg.Vars)
		}
		resolvedTags := build.ResolveTags(tagTemplates, versionInfo)

		// Resolve provider: from registry/target, or auto-detect from URL.
		provider := resolved.Provider
		if provider == "" {
			provider = build.DetectProvider(resolvedURL)
		}

		// Validate resolved tags conform to OCI spec.
		for _, tag := range resolvedTags {
			if err := registry.ValidateTag(tag); err != nil {
				return nil, fmt.Errorf("target[%d] %q (%s/%s): resolved tag: %w", i, t.ID, resolvedURL, resolvedPath, err)
			}
		}

		// Map retention (pointer to value).
		var retention config.RetentionPolicy
		if t.Retention != nil {
			retention = *t.Retention
		}

		target := build.RegistryTarget{
			URL:         resolvedURL,
			Path:        resolvedPath,
			Tags:        resolvedTags,
			Credentials: resolved.Credentials,
			Provider:    provider,
			Retention:   retention,
			TagPatterns: t.Tags,
			NativeScan:  t.NativeScan,
		}
		registries = append(registries, target)

		for _, tag := range resolvedTags {
			var ref string
			if provider == "local" {
				ref = fmt.Sprintf("%s:%s", resolvedPath, tag)
			} else {
				ref = fmt.Sprintf("%s/%s:%s", resolvedURL, resolvedPath, tag)
			}
			tags = append(tags, ref)
		}
	}

	// Auto-inject standard build args
	buildArgs := b.BuildArgs
	if buildArgs == nil {
		buildArgs = map[string]string{}
	}
	// Resolve vars in build args
	for k, v := range buildArgs {
		buildArgs[k] = gitver.ResolveVars(v, cfg.Vars)
	}
	buildArgs = autoInjectBuildArgs(buildArgs, det, versionInfo, dockerfile)

	step := &build.BuildStep{
		Name:       b.ID,
		Dockerfile: dockerfile,
		Context:    buildContext,
		Target:     b.Target,
		Platforms:  platforms,
		BuildArgs:  buildArgs,
		Tags:       tags,
		Output:     build.OutputImage,
		Registries: registries,
	}

	return step, nil
}

// targetAllowed checks if the current branch and git tag permit executing a target.
// Uses policy-aware pattern matching for when.branches and when.git_tags.
func targetAllowed(t config.TargetConfig, branch, gitTag string, policies config.PoliciesConfig) bool {
	// Resolve when.branches patterns against policies
	if len(t.When.Branches) > 0 {
		resolved := resolveWhenPatterns(t.When.Branches, policies.Branches)
		if !config.MatchPatterns(resolved, branch) {
			return false
		}
	}

	// Resolve when.git_tags patterns against policies
	if len(t.When.GitTags) > 0 {
		resolved := resolveWhenPatterns(t.When.GitTags, policies.GitTags)
		if gitTag == "" || !config.MatchPatterns(resolved, gitTag) {
			return false
		}
	}

	return true
}

// resolveWhenPatterns resolves when condition entries to regex patterns.
// Entries prefixed with "re:" are inline regex (strip prefix).
// Other entries are looked up as policy names.
func resolveWhenPatterns(entries []string, policyMap map[string]string) []string {
	resolved := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(entry) > 3 && entry[:3] == "re:" {
			// Inline regex
			resolved = append(resolved, entry[3:])
		} else if regex, ok := policyMap[entry]; ok {
			// Policy name lookup
			resolved = append(resolved, regex)
		} else {
			// Pass through as-is (may be treated as regex by MatchPatterns)
			resolved = append(resolved, entry)
		}
	}
	return resolved
}

// resolveBranch determines the current branch from detection, version info,
// or CI environment variables (for detached HEAD in CI).
// Priority cascade:
//  1. Git detection (branch from local .git)
//  2. Version info (uses git rev-parse which may work when detection doesn't)
//  3. CI env vars (CI_COMMIT_BRANCH, GITHUB_REF_NAME — needed for detached HEAD in CI)
func resolveBranch(det *build.Detection, v *build.VersionInfo) string {
	// 1. Git detection — most reliable when available
	if det.GitInfo != nil && det.GitInfo.Branch != "" {
		return det.GitInfo.Branch
	}
	// 2. Version info — uses git rev-parse which may work when detection doesn't
	if v != nil && v.Branch != "" && v.Branch != "HEAD" {
		return v.Branch
	}
	// 3. CI env vars — needed for detached HEAD in CI pipelines
	if b := os.Getenv("CI_COMMIT_BRANCH"); b != "" {
		return b
	}
	if b := os.Getenv("GITHUB_REF_NAME"); b != "" {
		return b
	}
	return ""
}

// resolveRepoID returns a stable repo identity for cache scoping.
// Uses CI project path, git remote origin, or directory name as fallback.
func resolveRepoID(det *build.Detection, v *build.VersionInfo) string {
	// CI project path (most reliable in CI).
	if p := os.Getenv("CI_PROJECT_PATH"); p != "" {
		return p
	}
	// Git remote URL.
	if det.GitInfo != nil && det.GitInfo.Remote != "" {
		return det.GitInfo.Remote
	}
	// Fallback to root dir basename.
	if det.RootDir != "" {
		return det.RootDir
	}
	return "unknown"
}

// autoInjectBuildArgs adds VERSION, COMMIT, and BUILD_DATE build args when the
// Dockerfile declares matching ARGs and no explicit override is set.
func autoInjectBuildArgs(existing map[string]string, det *build.Detection, v *build.VersionInfo, dockerfilePath string) map[string]string {
	if v == nil {
		return existing
	}

	// Find the parsed Dockerfile info that matches our chosen Dockerfile
	var dfArgs []string
	for _, df := range det.Dockerfiles {
		if df.Path == dockerfilePath {
			dfArgs = df.Args
			break
		}
	}
	if len(dfArgs) == 0 {
		return existing
	}

	// Build a set of Dockerfile ARG names for fast lookup
	argSet := make(map[string]bool, len(dfArgs))
	for _, a := range dfArgs {
		argSet[a] = true
	}

	// Inject if Dockerfile declares the ARG and no explicit override exists
	if argSet["VERSION"] {
		if _, ok := existing["VERSION"]; !ok {
			existing["VERSION"] = v.Version
		}
	}
	if argSet["COMMIT"] {
		if _, ok := existing["COMMIT"]; !ok {
			existing["COMMIT"] = v.SHA
		}
	}
	if argSet["BUILD_DATE"] {
		if _, ok := existing["BUILD_DATE"]; !ok {
			if pinned := os.Getenv("STAGEFREIGHT_BUILD_DATE"); pinned != "" {
				existing["BUILD_DATE"] = pinned
			} else {
				existing["BUILD_DATE"] = time.Now().UTC().Format(time.RFC3339)
			}
		}
	}

	return existing
}

func (e *imageEngine) Execute(ctx context.Context, plan *build.BuildPlan) (*build.BuildResult, error) {
	start := time.Now()
	result := &build.BuildResult{}

	bx := NewBuildx(false)

	// Authenticate to registries before building.
	// All steps share the same daemon auth state, so one login pass is sufficient.
	for _, step := range plan.Steps {
		if step.Push && len(step.Registries) > 0 {
			if err := bx.Login(ctx, step.Registries); err != nil {
				return result, err
			}
			break // all steps share the same daemon auth state
		}
	}

	for _, step := range plan.Steps {
		stepResult, err := bx.Build(ctx, step)
		result.Steps = append(result.Steps, *stepResult)
		if err != nil {
			result.Duration = time.Since(start)
			return result, err
		}

		// Save image tarball for downstream scanning/attestation
		if step.SavePath != "" && len(step.Tags) > 0 {
			if err := bx.Save(ctx, step.Tags[0], step.SavePath); err != nil {
				return result, err
			}
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}
