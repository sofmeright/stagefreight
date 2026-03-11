package dependency

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sofmeright/stagefreight/src/lint/modules/freshness"
)

// goRunner executes a go subcommand in the given directory.
type goRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// resolveGoRunner tries strategies in order and returns the first available go runner.
func resolveGoRunner(repoRoot string) (goRunner, error) {
	// Strategy 1: Native go binary in PATH
	if _, err := exec.LookPath("go"); err == nil {
		return nativeGoRunner, nil
	}
	// Strategy 2: Toolcache (STAGEFREIGHT_GO_HOME or /toolcache/go)
	if goHome := toolcacheGoHome(); goHome != "" {
		return toolcacheGoRunner(goHome), nil
	}
	// Strategy 3: Container runtime (docker/podman/nerdctl)
	if rt := detectContainerRuntime(); rt != "" {
		absRoot, err := filepath.Abs(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolving repo root: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
			absRoot = resolved
		}
		return containerGoRunner(rt, absRoot), nil
	}
	// Strategy 4: Error
	return nil, fmt.Errorf("go toolchain not found: install Go, set STAGEFREIGHT_GO_HOME, or ensure a container runtime (docker/podman/nerdctl) is available")
}

func nativeGoRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	return cmd.CombinedOutput()
}

func toolcacheGoRunner(goHome string) goRunner {
	goBin := filepath.Join(goHome, "bin", "go")
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, goBin, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		return cmd.CombinedOutput()
	}
}

func toolcacheGoHome() string {
	if env := os.Getenv("STAGEFREIGHT_GO_HOME"); env != "" {
		if _, err := os.Stat(filepath.Join(env, "bin", "go")); err == nil {
			return env
		}
	}
	if _, err := os.Stat("/toolcache/go/bin/go"); err == nil {
		return "/toolcache/go"
	}
	return ""
}

func detectContainerRuntime() string {
	for _, rt := range []string{"docker", "podman", "nerdctl"} {
		if _, err := exec.LookPath(rt); err == nil {
			return rt
		}
	}
	return ""
}

func containerGoRunner(rt, repoRoot string) goRunner {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		ver := parseGoVersion(dir, repoRoot)
		image := fmt.Sprintf("docker.io/library/golang:%s-alpine", ver)

		relDir, err := filepath.Rel(repoRoot, dir)
		if err != nil {
			relDir = "."
		}
		workDir := "/src"
		if relDir != "." && relDir != "" {
			workDir = "/src/" + filepath.ToSlash(relDir)
		}

		runArgs := []string{"run", "--rm", "--pull=missing", "-v", repoRoot + ":/src", "-w", workDir}

		// Run as current user to avoid root-owned writes on shared volumes
		if uid := os.Getuid(); uid >= 0 {
			runArgs = append(runArgs, "--user", fmt.Sprintf("%d:%d", uid, os.Getgid()))
		}

		// Set HOME and Go caches inside the container
		runArgs = append(runArgs,
			"-e", "HOME=/tmp",
			"-e", "GOCACHE=/tmp/gocache",
			"-e", "GOMODCACHE=/tmp/gomodcache",
		)

		// Pass through Go module-relevant and proxy environment variables
		for _, key := range []string{
			"GOPROXY", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GOFLAGS",
			"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		} {
			if val, ok := os.LookupEnv(key); ok {
				runArgs = append(runArgs, "-e", key+"="+val)
			}
		}

		runArgs = append(runArgs, image, "go")
		runArgs = append(runArgs, args...)

		cmd := exec.CommandContext(ctx, rt, runArgs...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%w\nhint: if running in DinD, ensure the repo path is visible to the Docker daemon", err)
		}
		return out, nil
	}
}

// parseGoVersion extracts the go directive version from go.work or go.mod.
func parseGoVersion(dir, repoRoot string) string {
	// Prefer go.work at repo root (workspace mode)
	if ver := parseGoDirectiveFromFile(filepath.Join(repoRoot, "go.work")); ver != "" {
		return ver
	}
	// Try go.mod in module directory
	if ver := parseGoDirectiveFromFile(filepath.Join(dir, "go.mod")); ver != "" {
		return ver
	}
	// Try go.mod at repo root
	if dir != repoRoot {
		if ver := parseGoDirectiveFromFile(filepath.Join(repoRoot, "go.mod")); ver != "" {
			return ver
		}
	}
	return "1.24"
}

// parseGoDirectiveFromFile reads a go.mod or go.work file and returns the go version directive.
// Prefers the toolchain directive (e.g. "toolchain go1.22.6" → "1.22") over the go directive.
func parseGoDirectiveFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var goVer string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// "toolchain go1.22.6" is a stronger signal than "go 1.22"
		if strings.HasPrefix(line, "toolchain ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ver := strings.TrimPrefix(fields[1], "go")
				// Strip patch: "1.22.6" → "1.22"
				if parts := strings.SplitN(ver, ".", 3); len(parts) >= 2 {
					return parts[0] + "." + parts[1]
				}
				return ver
			}
		}
		if goVer == "" && strings.HasPrefix(line, "go ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				goVer = fields[1]
			}
		}
	}
	return goVer
}

// applyGoUpdates applies Go module dependency updates.
// Returns touched module dirs (repoRoot-relative) as the 3rd value —
// only dirs where go get + go mod tidy both succeeded.
func applyGoUpdates(ctx context.Context, deps []freshness.Dependency, repoRoot string) ([]AppliedUpdate, []SkippedDep, []string, error) {
	runGo, err := resolveGoRunner(repoRoot)
	if err != nil {
		return nil, nil, nil, err
	}

	// Check for go.work — workspace mode uses -C with relative paths
	hasWorkspace := false
	if _, err := os.Stat(filepath.Join(repoRoot, "go.work")); err == nil {
		hasWorkspace = true
	}

	// Group deps by module dir (derived from dep.File)
	type moduleGroup struct {
		dir  string // repoRoot-relative
		deps []freshness.Dependency
	}
	groupMap := make(map[string]*moduleGroup)
	for _, dep := range deps {
		dir := filepath.Dir(dep.File)
		if g, ok := groupMap[dir]; ok {
			g.deps = append(g.deps, dep)
		} else {
			groupMap[dir] = &moduleGroup{dir: dir, deps: []freshness.Dependency{dep}}
		}
	}

	var applied []AppliedUpdate
	var skipped []SkippedDep
	touchedSet := make(map[string]struct{})

	for _, group := range groupMap {
		// Guard: module dir must be safely under repo root
		if strings.HasPrefix(group.dir, "..") || filepath.IsAbs(group.dir) {
			return applied, skipped, nil, fmt.Errorf("module dir %q escapes repo root", group.dir)
		}
		modulePath := filepath.Join(repoRoot, group.dir)

		// Detect replace directives for this module
		replaceSet, err := detectReplaceDirectives(modulePath)
		if err != nil && len(replaceSet) == 0 {
			// Non-fatal — continue without replace detection
			replaceSet = nil
		}

		// Build batch go get args, skipping replaced modules
		var getArgs []string
		for _, dep := range group.deps {
			if replaceSet != nil && replaceSet[dep.Name] {
				skipped = append(skipped, SkippedDep{Dep: dep, Reason: "replace directive present"})
				continue
			}

			getArgs = append(getArgs, dep.Name+"@"+dep.Latest)
			update := AppliedUpdate{
				Dep:        dep,
				OldVer:     dep.Current,
				NewVer:     dep.Latest,
				UpdateType: updateType(dep.Current, dep.Latest),
			}
			for _, v := range dep.Vulnerabilities {
				update.CVEsFixed = append(update.CVEsFixed, v.ID)
			}
			applied = append(applied, update)
		}

		if len(getArgs) == 0 {
			continue
		}

		// Determine working directory and build go get args
		var goDir string
		if hasWorkspace {
			goDir = repoRoot
		} else {
			goDir = modulePath
		}

		var goGetArgs []string
		if hasWorkspace {
			goGetArgs = append([]string{"-C", group.dir, "get"}, getArgs...)
		} else {
			goGetArgs = append([]string{"get"}, getArgs...)
		}

		out, err := runGo(ctx, goDir, goGetArgs...)
		if err != nil {
			return applied, skipped, nil, fmt.Errorf("go get in %s: %s\n%w", group.dir, string(out), err)
		}

		// go mod tidy
		var tidyArgs []string
		if hasWorkspace {
			tidyArgs = []string{"-C", group.dir, "mod", "tidy"}
		} else {
			tidyArgs = []string{"mod", "tidy"}
		}
		out, err = runGo(ctx, goDir, tidyArgs...)
		if err != nil {
			return applied, skipped, nil, fmt.Errorf("go mod tidy in %s: %s\n%w", group.dir, string(out), err)
		}

		// Both go get and go mod tidy succeeded — mark this dir as touched
		touchedSet[group.dir] = struct{}{}
	}

	touchedDirs := make([]string, 0, len(touchedSet))
	for d := range touchedSet {
		touchedDirs = append(touchedDirs, d)
	}
	sort.Strings(touchedDirs)
	return applied, skipped, touchedDirs, nil
}

// goDirectiveSyncTarget maps a Dockerfile golang builder update to its owning module.
type goDirectiveSyncTarget struct {
	ModuleDir string // repo-relative dir containing go.mod ("." for root)
	GoVersion string // target Go version from the builder image (full patch)
	Source    string // Dockerfile path that triggered the sync
}

// ToolchainDependency records a resolved build toolchain for reporting and SBOM.
type ToolchainDependency struct {
	Ecosystem    string // "golang"
	Name         string // "go"
	Version      string // "1.26.1"
	BuilderImage string // "docker.io/library/golang:1.26.1-alpine3.23"
	Dockerfile   string // repo-relative Dockerfile path
	ModuleDir    string // repo-relative module dir
}

// hasAppliedGolangBuilderUpdate returns true if any applied update was a golang builder image.
func hasAppliedGolangBuilderUpdate(applied []AppliedUpdate) bool {
	for _, a := range applied {
		if a.Dep.Ecosystem == freshness.EcosystemDockerImage && isGolangImage(a.Dep.Name) {
			return true
		}
	}
	return false
}

// syncGoDirectivesFromResolved bumps go directives in go.mod files to match their
// associated Dockerfile golang builder versions, using pre-computed sync targets.
func syncGoDirectivesFromResolved(ctx context.Context, repoRoot string, result *UpdateResult, resolved goDirectiveSyncResult) error {
	// Surface conflicted modules as skipped entries with version details
	for _, conflict := range resolved.Conflicted {
		detail := fmt.Sprintf("conflicting golang builder versions in %s: %s (from %s)",
			conflict.ModuleDir,
			strings.Join(conflict.Versions, " vs "),
			strings.Join(conflict.Sources, ", "))
		result.Skipped = append(result.Skipped, SkippedDep{
			Dep: freshness.Dependency{
				Name:      "stdlib",
				Ecosystem: freshness.EcosystemGoMod,
				File:      moduleGoModPath(conflict.ModuleDir),
			},
			Reason: detail,
		})
	}

	if len(resolved.Targets) == 0 {
		return nil
	}

	runGo, err := resolveGoRunner(repoRoot)
	if err != nil {
		return err
	}

	for _, t := range resolved.Targets {
		modFile := filepath.Join(repoRoot, t.ModuleDir, "go.mod")
		if _, err := os.Stat(modFile); err != nil {
			continue
		}

		cur := parseGoDirectiveFromFile(modFile)
		if cur == "" || cur == t.GoVersion {
			continue
		}

		absDir := filepath.Join(repoRoot, t.ModuleDir)

		out, err := runGo(ctx, absDir, "mod", "edit", "-go="+t.GoVersion)
		if err != nil {
			return fmt.Errorf("go mod edit -go=%s in %s: %s\n%w", t.GoVersion, t.ModuleDir, string(out), err)
		}

		out, err = runGo(ctx, absDir, "mod", "tidy")
		if err != nil {
			return fmt.Errorf("go mod tidy in %s: %s\n%w", t.ModuleDir, string(out), err)
		}

		result.Applied = append(result.Applied, AppliedUpdate{
			Dep: freshness.Dependency{
				Name:      "stdlib",
				Current:   cur,
				Latest:    t.GoVersion,
				Ecosystem: freshness.EcosystemGoMod,
				File:      moduleGoModPath(t.ModuleDir),
			},
			OldVer:     cur,
			NewVer:     t.GoVersion,
			UpdateType: updateType(cur, t.GoVersion),
		})

		found := false
		for _, d := range result.TouchedModuleDirs {
			if d == t.ModuleDir {
				found = true
				break
			}
		}
		if !found {
			result.TouchedModuleDirs = append(result.TouchedModuleDirs, t.ModuleDir)
		}
	}

	return nil
}

// goDirectiveConflict records a module with conflicting golang builder versions.
type goDirectiveConflict struct {
	ModuleDir string   // repo-relative module dir
	Versions  []string // the conflicting Go versions (e.g. ["1.26.1", "1.25.7"])
	Sources   []string // Dockerfile paths that caused the conflict
}

// goDirectiveSyncResult holds resolved targets and any conflicted modules.
type goDirectiveSyncResult struct {
	Targets    []goDirectiveSyncTarget
	Conflicted []goDirectiveConflict
}

// collectGoDirectiveSyncTargets maps applied golang builder Docker updates to
// their owning Go module directories. Each Dockerfile is mapped to the nearest
// ancestor directory containing a go.mod file.
//
// If two Dockerfiles in the same module want different Go versions, the module
// is marked conflicted and skipped entirely — no silent winner-picking.
func collectGoDirectiveSyncTargets(repoRoot string, applied []AppliedUpdate) goDirectiveSyncResult {
	byModuleDir := make(map[string]goDirectiveSyncTarget)
	conflicted := make(map[string]bool)

	for _, a := range applied {
		if a.Dep.Ecosystem != freshness.EcosystemDockerImage || !isGolangImage(a.Dep.Name) {
			continue
		}

		goVer := extractGoVersionFromTag(a.NewVer)
		if goVer == "" {
			continue
		}

		moduleDir := findNearestGoMod(repoRoot, filepath.Dir(a.Dep.File))
		if moduleDir == "" {
			continue
		}

		if conflicted[moduleDir] {
			continue
		}

		if existing, ok := byModuleDir[moduleDir]; ok {
			if existing.GoVersion != goVer {
				delete(byModuleDir, moduleDir)
				conflicted[moduleDir] = true
				continue
			}
		}

		byModuleDir[moduleDir] = goDirectiveSyncTarget{
			ModuleDir: moduleDir,
			GoVersion: goVer,
			Source:    a.Dep.File,
		}
	}

	targets := make([]goDirectiveSyncTarget, 0, len(byModuleDir))
	for _, t := range byModuleDir {
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].ModuleDir < targets[j].ModuleDir
	})

	var conflicts []goDirectiveConflict
	for dir := range conflicted {
		conflicts = append(conflicts, collectConflictDetail(applied, repoRoot, dir))
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].ModuleDir < conflicts[j].ModuleDir
	})

	return goDirectiveSyncResult{Targets: targets, Conflicted: conflicts}
}

// collectToolchainDepsFromResolved builds toolchain dependency records from
// pre-computed sync targets, ensuring toolchain metadata matches what was synced.
func collectToolchainDepsFromResolved(resolved goDirectiveSyncResult, applied []AppliedUpdate) []ToolchainDependency {
	if len(resolved.Targets) == 0 {
		return nil
	}

	// Index applied golang builder updates by Dockerfile path
	bySource := make(map[string]AppliedUpdate)
	for _, a := range applied {
		if a.Dep.Ecosystem == freshness.EcosystemDockerImage && isGolangImage(a.Dep.Name) {
			bySource[a.Dep.File] = a
		}
	}

	deps := make([]ToolchainDependency, 0, len(resolved.Targets))
	for _, t := range resolved.Targets {
		a, ok := bySource[t.Source]
		if !ok {
			continue
		}
		deps = append(deps, ToolchainDependency{
			Ecosystem:    "golang",
			Name:         "go",
			Version:      t.GoVersion,
			BuilderImage: a.Dep.Name + ":" + a.NewVer,
			Dockerfile:   t.Source,
			ModuleDir:    t.ModuleDir,
		})
	}
	return deps
}

// collectConflictDetail gathers the specific versions and Dockerfiles that caused
// a conflict for a given module directory.
func collectConflictDetail(applied []AppliedUpdate, repoRoot, moduleDir string) goDirectiveConflict {
	seenVer := make(map[string]bool)
	var versions []string
	var sources []string

	for _, a := range applied {
		if a.Dep.Ecosystem != freshness.EcosystemDockerImage || !isGolangImage(a.Dep.Name) {
			continue
		}
		goVer := extractGoVersionFromTag(a.NewVer)
		if goVer == "" {
			continue
		}
		dir := findNearestGoMod(repoRoot, filepath.Dir(a.Dep.File))
		if dir != moduleDir {
			continue
		}
		sources = append(sources, a.Dep.File)
		if !seenVer[goVer] {
			seenVer[goVer] = true
			versions = append(versions, goVer)
		}
	}

	sort.Strings(versions)
	sort.Strings(sources)

	return goDirectiveConflict{
		ModuleDir: moduleDir,
		Versions:  versions,
		Sources:   sources,
	}
}

// findNearestGoMod walks up from relDir toward repoRoot looking for a go.mod file.
// Returns the repo-relative directory containing go.mod, or "" if not found.
func findNearestGoMod(repoRoot, relDir string) string {
	dir := relDir
	for {
		candidate := filepath.Join(repoRoot, dir, "go.mod")
		if _, err := os.Stat(candidate); err == nil {
			return dir
		}
		if dir == "." || dir == "" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
		return "."
	}
	return ""
}

// moduleGoModPath returns a clean repo-relative path to go.mod for a module dir.
func moduleGoModPath(moduleDir string) string {
	if moduleDir == "." || moduleDir == "" {
		return "go.mod"
	}
	return filepath.Join(moduleDir, "go.mod")
}

// isGolangImage returns true if the image name is a golang builder.
// Handles names with or without tags (e.g. "docker.io/library/golang:1.26.1-alpine3.23").
func isGolangImage(name string) bool {
	n := strings.ToLower(name)
	if idx := strings.LastIndex(n, ":"); idx > 0 {
		n = n[:idx]
	}
	return n == "golang" || n == "library/golang" ||
		strings.HasSuffix(n, "/golang") || strings.HasSuffix(n, "/library/golang")
}

// extractGoVersionFromTag extracts the Go version from a Docker tag.
// Returns the full patch version (e.g. "1.26.1") — the go directive should
// reflect the exact stdlib version used by the builder for accurate CVE scanning.
func extractGoVersionFromTag(tag string) string {
	ver := tag
	if idx := strings.IndexByte(ver, '-'); idx > 0 {
		ver = ver[:idx]
	}
	for _, c := range ver {
		if c != '.' && (c < '0' || c > '9') {
			return ""
		}
	}
	if ver == "" {
		return ""
	}
	return ver
}

// detectGoDirectiveDrift scans all resolved dependencies for golang builder images
// whose version is strictly newer than the corresponding go.mod directive. Returns
// sync targets for modules where the Dockerfile's golang version exceeds go.mod.
func detectGoDirectiveDrift(repoRoot string, allDeps []freshness.Dependency) goDirectiveSyncResult {
	byModuleDir := make(map[string]goDirectiveSyncTarget)
	conflicted := make(map[string]bool)

	for _, dep := range allDeps {
		if dep.Ecosystem != freshness.EcosystemDockerImage || !isGolangImage(dep.Name) {
			continue
		}

		goVer := extractGoVersionFromTag(dep.Current)
		if goVer == "" {
			continue
		}

		moduleDir := findNearestGoMod(repoRoot, filepath.Dir(dep.File))
		if moduleDir == "" {
			continue
		}

		if conflicted[moduleDir] {
			continue
		}

		// Only sync when builder is strictly newer than go.mod
		modFile := filepath.Join(repoRoot, moduleDir, "go.mod")
		cur := parseGoDirectiveFromFile(modFile)
		if cur == "" {
			continue
		}
		delta := freshness.CompareDependencyVersions(cur, goVer, freshness.EcosystemGoMod)
		if delta.Major <= 0 && delta.Minor <= 0 && delta.Patch <= 0 {
			continue // go.mod is equal or newer — no drift
		}

		if existing, ok := byModuleDir[moduleDir]; ok {
			if existing.GoVersion != goVer {
				delete(byModuleDir, moduleDir)
				conflicted[moduleDir] = true
				continue
			}
		}

		byModuleDir[moduleDir] = goDirectiveSyncTarget{
			ModuleDir: moduleDir,
			GoVersion: goVer,
			Source:    dep.File,
		}
	}

	targets := make([]goDirectiveSyncTarget, 0, len(byModuleDir))
	for _, t := range byModuleDir {
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].ModuleDir < targets[j].ModuleDir
	})

	var conflicts []goDirectiveConflict
	for dir := range conflicted {
		conflicts = append(conflicts, goDirectiveConflict{ModuleDir: dir})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].ModuleDir < conflicts[j].ModuleDir
	})

	return goDirectiveSyncResult{Targets: targets, Conflicted: conflicts}
}

// mergeGoDirectiveSyncResults combines two sync results, deduplicating by module dir.
// If both sources have an entry for the same module (target or conflict), the first (a) wins.
func mergeGoDirectiveSyncResults(a, b goDirectiveSyncResult) goDirectiveSyncResult {
	if len(b.Targets) == 0 && len(b.Conflicted) == 0 {
		return a
	}
	if len(a.Targets) == 0 && len(a.Conflicted) == 0 {
		return b
	}

	seen := make(map[string]bool)
	for _, t := range a.Targets {
		seen[t.ModuleDir] = true
	}
	for _, c := range a.Conflicted {
		seen[c.ModuleDir] = true
	}

	for _, t := range b.Targets {
		if !seen[t.ModuleDir] {
			a.Targets = append(a.Targets, t)
			seen[t.ModuleDir] = true
		}
	}
	for _, c := range b.Conflicted {
		if !seen[c.ModuleDir] {
			a.Conflicted = append(a.Conflicted, c)
			seen[c.ModuleDir] = true
		}
	}

	sort.Slice(a.Targets, func(i, j int) bool {
		return a.Targets[i].ModuleDir < a.Targets[j].ModuleDir
	})
	sort.Slice(a.Conflicted, func(i, j int) bool {
		return a.Conflicted[i].ModuleDir < a.Conflicted[j].ModuleDir
	})

	return a
}

// detectReplaceDirectives parses go.mod and returns a set of replaced module paths.
func detectReplaceDirectives(moduleDir string) (map[string]bool, error) {
	gomod := filepath.Join(moduleDir, "go.mod")
	f, err := os.Open(gomod)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	replaced := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	inReplace := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "replace (") || strings.HasPrefix(line, "replace(") {
			inReplace = true
			continue
		}
		if inReplace {
			if line == ")" {
				inReplace = false
				continue
			}
			// Inside replace block: "module => replacement"
			parts := strings.Fields(line)
			if len(parts) >= 3 && parts[1] == "=>" {
				replaced[parts[0]] = true
			}
			continue
		}
		if strings.HasPrefix(line, "replace ") {
			// Single-line replace: "replace module => replacement"
			parts := strings.Fields(line)
			if len(parts) >= 4 && parts[2] == "=>" {
				replaced[parts[1]] = true
			}
		}
	}

	return replaced, scanner.Err()
}

// updateType determines the semver update type between two versions.
func updateType(current, latest string) string {
	delta := freshness.CompareDependencyVersions(current, latest, freshness.EcosystemGoMod)
	if delta.IsZero() {
		return "tag"
	}
	if delta.Major > 0 {
		return "major"
	}
	if delta.Minor > 0 {
		return "minor"
	}
	return "patch"
}
