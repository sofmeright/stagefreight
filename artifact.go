package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/version"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	gitdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	textdiff "github.com/go-git/go-git/v5/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// resolveJSON is the top-level structure for resolve.json (schemaVersion 1).
type resolveJSON struct {
	SchemaVersion       int              `json:"schemaVersion"`
	GeneratedAt         string           `json:"generatedAt"`
	StagefreightVersion string           `json:"stagefreightVersion"`
	Policy              string           `json:"policy"`
	Ecosystems          []string         `json:"ecosystems"`
	Deps                []resolveDepJSON `json:"deps"`
}

// resolveDepJSON is the per-dependency entry in resolve.json.
// Field names are frozen — never rename or reorder.
type resolveDepJSON struct {
	Name            string     `json:"name"`
	Current         string     `json:"current"`
	Latest          string     `json:"latest"`
	Target          string     `json:"target"`
	Ecosystem       string     `json:"ecosystem"`
	File            string     `json:"file"`
	Line            int        `json:"line"`
	Source          string     `json:"source"`
	SourceURL       string     `json:"sourceURL"`
	Vulnerabilities []vulnJSON `json:"vulnerabilities"`
	UpdateType      string     `json:"updateType"`
	Decision        string     `json:"decision"`
	Reason          string     `json:"reason"`
}

type vulnJSON struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Severity string `json:"severity"`
	FixedIn  string `json:"fixedIn,omitempty"`
}

// GenerateArtifacts creates output files in the specified directory.
// Uses repoRoot for all git operations via go-git (no git binary required).
func GenerateArtifacts(ctx context.Context, repoRoot, outputDir string, result *UpdateResult, bundle bool) ([]string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}

	var artifacts []string

	// 1. resolve.json (always)
	resolveFile := filepath.Join(outputDir, "resolve.json")
	if err := writeResolveJSON(resolveFile, result); err != nil {
		return artifacts, fmt.Errorf("writing resolve.json: %w", err)
	}
	artifacts = append(artifacts, resolveFile)

	// 2. deps-report.md (always)
	reportFile := filepath.Join(outputDir, "deps-report.md")
	if err := writeReport(reportFile, result); err != nil {
		return artifacts, fmt.Errorf("writing deps-report.md: %w", err)
	}
	artifacts = append(artifacts, reportFile)

	// 3. deps.patch (only if changes exist in working tree)
	if len(result.Applied) > 0 {
		patchFile := filepath.Join(outputDir, "deps.patch")
		if err := writePatch(ctx, repoRoot, patchFile); err != nil {
			return artifacts, fmt.Errorf("writing deps.patch: %w", err)
		}
		// writePatch always writes the file (empty patch = no changes).
		artifacts = append(artifacts, patchFile)
	}

	// 4. deps-updated.tgz (only if bundle && changes exist in working tree)
	if bundle && len(result.Applied) > 0 {
		bundleFile := filepath.Join(outputDir, "deps-updated.tgz")
		if err := writeBundle(ctx, repoRoot, bundleFile); err != nil {
			return artifacts, fmt.Errorf("writing deps-updated.tgz: %w", err)
		}
		if _, err := os.Stat(bundleFile); err == nil {
			artifacts = append(artifacts, bundleFile)
		}
	}

	return artifacts, nil
}

func writeResolveJSON(path string, result *UpdateResult) error {
	rj := resolveJSON{
		SchemaVersion:       1,
		GeneratedAt:         time.Now().UTC().Format(time.RFC3339),
		StagefreightVersion: version.Version,
		Deps:                make([]resolveDepJSON, 0),
	}

	// Applied deps → decision: "update"
	for _, a := range result.Applied {
		dep := a.Dep
		rj.Deps = append(rj.Deps, resolveDepJSON{
			Name:            dep.Name,
			Current:         a.OldVer,
			Latest:          dep.Latest,
			Target:          a.NewVer,
			Ecosystem:       dep.Ecosystem,
			File:            dep.File,
			Line:            dep.Line,
			Source:          sourceShortName(dep),
			SourceURL:       dep.SourceURL,
			Vulnerabilities: vulnsToJSON(dep.Vulnerabilities),
			UpdateType:      a.UpdateType,
			Decision:        "update",
			Reason:          "",
		})
	}

	// Skipped deps → decision: "skip"
	for _, s := range result.Skipped {
		dep := s.Dep
		ut := "tag"
		if dep.Latest != "" && dep.Current != dep.Latest {
			delta := freshness.CompareDependencyVersions(dep.Current, dep.Latest, dep.Ecosystem)
			if !delta.IsZero() {
				ut = freshness.DominantUpdateType(delta)
			}
		}
		rj.Deps = append(rj.Deps, resolveDepJSON{
			Name:            dep.Name,
			Current:         dep.Current,
			Latest:          dep.Latest,
			Target:          "",
			Ecosystem:       dep.Ecosystem,
			File:            dep.File,
			Line:            dep.Line,
			Source:          sourceShortName(dep),
			SourceURL:       dep.SourceURL,
			Vulnerabilities: vulnsToJSON(dep.Vulnerabilities),
			UpdateType:      ut,
			Decision:        "skip",
			Reason:          s.Reason,
		})
	}

	data, err := json.MarshalIndent(rj, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func writeReport(path string, result *UpdateResult) error {
	var b strings.Builder

	b.WriteString("# Dependency Update Report\n\n")
	b.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339)))

	// Applied updates
	if len(result.Applied) > 0 {
		b.WriteString("## Applied Updates\n\n")
		b.WriteString("| Dependency | From | To | Type | CVEs Fixed |\n")
		b.WriteString("|------------|------|----|------|------------|\n")
		for _, a := range result.Applied {
			cves := "-"
			if len(a.CVEsFixed) > 0 {
				cves = strings.Join(a.CVEsFixed, ", ")
			}
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
				a.Dep.Name, a.OldVer, a.NewVer, a.UpdateType, cves))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("## No updates applied\n\n")
	}

	// Skipped deps
	if len(result.Skipped) > 0 {
		b.WriteString("## Skipped Dependencies\n\n")
		b.WriteString("| Dependency | Current | Latest | Reason |\n")
		b.WriteString("|------------|---------|--------|--------|\n")
		for _, s := range result.Skipped {
			latest := s.Dep.Latest
			if latest == "" {
				latest = "-"
			}
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				s.Dep.Name, s.Dep.Current, latest, s.Reason))
		}
		b.WriteString("\n")
	}

	// Verification log
	if result.Verified {
		b.WriteString("## Verification\n\n")
		if result.VerifyErr != nil {
			b.WriteString("**Status: FAILED**\n\n")
			b.WriteString("verification failed; patch still provided for review.\n\n")
		} else {
			b.WriteString("**Status: PASSED**\n\n")
		}
		if result.VerifyLog != "" {
			b.WriteString("```\n")
			b.WriteString(result.VerifyLog)
			b.WriteString("```\n\n")
		}
	}

	// Patch not generated note
	if len(result.Applied) == 0 {
		b.WriteString("## Patch\n\n(not generated; no changes)\n\n")
	}

	// Apply/verify snippets
	b.WriteString("## How to apply\n\n")
	b.WriteString("```bash\n")
	b.WriteString("git apply deps.patch\n")
	b.WriteString("```\n\n")
	b.WriteString("## Verify locally\n\n")
	b.WriteString("```bash\n")
	b.WriteString("go test ./... && govulncheck ./...\n")
	b.WriteString("```\n")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// writePatch generates a unified diff of HEAD vs the working tree using go-git.
// The output is `git apply`-compatible unified diff format.
// No git binary required — diff encoding uses go-git's UnifiedEncoder.
func writePatch(_ context.Context, repoRoot, patchFile string) error {
	repo, err := gitstate.OpenRepo(repoRoot)
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return fmt.Errorf("resolving HEAD commit: %w", err)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return fmt.Errorf("resolving HEAD tree: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("opening worktree: %w", err)
	}
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("reading worktree status: %w", err)
	}

	var fps []gitdiff.FilePatch
	for _, filePath := range gitstate.AllDirtyPaths(status) {
		var oldContent string
		existsInHead := false
		if f, ferr := headTree.File(filePath); ferr == nil {
			if oldContent, err = f.Contents(); err != nil {
				return fmt.Errorf("reading %s from HEAD: %w", filePath, err)
			}
			existsInHead = true
		}

		var newContent string
		existsOnDisk := false
		if data, rerr := os.ReadFile(filepath.Join(repoRoot, filePath)); rerr == nil {
			newContent = string(data)
			existsOnDisk = true
		}

		if existsInHead && existsOnDisk && oldContent == newContent {
			continue
		}

		fps = append(fps, buildFilePatch(filePath, oldContent, newContent, !existsInHead, !existsOnDisk))
	}

	var buf bytes.Buffer
	enc := gitdiff.NewUnifiedEncoder(&buf, gitdiff.DefaultContextLines)
	if err := enc.Encode(&unifiedPatch{fps: fps}); err != nil {
		return fmt.Errorf("encoding patch: %w", err)
	}
	// Always write the file — empty patch is a valid artifact (means no changes).
	return os.WriteFile(patchFile, buf.Bytes(), 0o644)
}

// buildFilePatch constructs a FilePatch for a single file using go-diff Myers diff.
func buildFilePatch(path, oldContent, newContent string, added, deleted bool) gitdiff.FilePatch {
	var from, to gitdiff.File
	if !added {
		from = &patchFile{path: path}
	}
	if !deleted {
		to = &patchFile{path: path}
	}

	diffs := textdiff.Do(oldContent, newContent)
	var chunks []gitdiff.Chunk
	for _, d := range diffs {
		if d.Text == "" {
			continue
		}
		var op gitdiff.Operation
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			op = gitdiff.Equal
		case diffmatchpatch.DiffInsert:
			op = gitdiff.Add
		case diffmatchpatch.DiffDelete:
			op = gitdiff.Delete
		default:
			continue
		}
		chunks = append(chunks, &patchChunk{content: d.Text, op: op})
	}

	return &filePatch{from: from, to: to, chunks: chunks}
}

// unifiedPatch, filePatch, patchFile, and patchChunk implement the
// plumbing/format/diff interfaces consumed by UnifiedEncoder.

type unifiedPatch struct{ fps []gitdiff.FilePatch }

func (p *unifiedPatch) FilePatches() []gitdiff.FilePatch { return p.fps }
func (p *unifiedPatch) Message() string                  { return "" }

type filePatch struct {
	from, to gitdiff.File
	chunks   []gitdiff.Chunk
}

func (f *filePatch) IsBinary() bool                      { return false }
func (f *filePatch) Files() (gitdiff.File, gitdiff.File) { return f.from, f.to }
func (f *filePatch) Chunks() []gitdiff.Chunk             { return f.chunks }

type patchFile struct{ path string }

func (f *patchFile) Hash() plumbing.Hash     { return plumbing.ZeroHash }
func (f *patchFile) Mode() filemode.FileMode { return filemode.Regular }
func (f *patchFile) Path() string            { return f.path }

type patchChunk struct {
	content string
	op      gitdiff.Operation
}

func (c *patchChunk) Content() string         { return c.content }
func (c *patchChunk) Type() gitdiff.Operation { return c.op }

func writeBundle(ctx context.Context, repoRoot, bundleFile string) error {
	repo, err := gitstate.OpenRepo(repoRoot)
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	status, err := wt.Status()
	if err != nil {
		return err
	}

	// Include both staged and unstaged dirty paths — dependency updates may
	// stage changes before the bundle is generated.
	dirtyPaths := gitstate.AllDirtyPaths(status)
	if len(dirtyPaths) == 0 {
		return nil
	}

	// Create tar.gz of changed files
	args := []string{"-czf", bundleFile}
	for _, f := range dirtyPaths {
		args = append(args, "-C", repoRoot, f)
	}
	tarCmd := exec.CommandContext(ctx, "tar", args...)
	if tarOut, err := tarCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creating bundle: %s\n%w", string(tarOut), err)
	}

	return nil
}

// sourceShortName returns a short resolver name for a dependency.
func sourceShortName(dep freshness.Dependency) string {
	switch dep.Ecosystem {
	case freshness.EcosystemGoMod:
		return "proxy.golang.org"
	case freshness.EcosystemDockerImage:
		return "dockerhub"
	case freshness.EcosystemGitHubRelease:
		return "github"
	case freshness.EcosystemNpm:
		return "npmjs"
	case freshness.EcosystemPip:
		return "pypi"
	case freshness.EcosystemCargo:
		return "crates.io"
	case freshness.EcosystemAlpineAPK:
		return "alpine"
	case freshness.EcosystemDebianAPT:
		return "debian"
	default:
		return "unknown"
	}
}

func vulnsToJSON(vulns []freshness.VulnInfo) []vulnJSON {
	out := make([]vulnJSON, len(vulns))
	for i, v := range vulns {
		out[i] = vulnJSON{
			ID:       v.ID,
			Summary:  v.Summary,
			Severity: v.Severity,
			FixedIn:  v.FixedIn,
		}
	}
	return out
}
