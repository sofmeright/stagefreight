package dependency

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
	"github.com/PrPlanIT/StageFreight/src/version"
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
		// writePatch skips writing when git diff is empty (e.g. dry-run)
		if _, err := os.Stat(patchFile); err == nil {
			artifacts = append(artifacts, patchFile)
		}
	}

	// 4. deps-updated.tgz (only if bundle && changes exist in working tree)
	if bundle && len(result.Applied) > 0 {
		bundleFile := filepath.Join(outputDir, "deps-updated.tgz")
		if err := writeBundle(ctx, repoRoot, bundleFile, result.FilesChanged); err != nil {
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

// --- go-git diff interface implementations ---

type diffFileEntry struct {
	hash plumbing.Hash
	mode filemode.FileMode
	path string
}

func (f *diffFileEntry) Hash() plumbing.Hash     { return f.hash }
func (f *diffFileEntry) Mode() filemode.FileMode { return f.mode }
func (f *diffFileEntry) Path() string            { return f.path }

type diffChunk struct {
	content string
	op      fdiff.Operation
}

func (c *diffChunk) Content() string       { return c.content }
func (c *diffChunk) Type() fdiff.Operation { return c.op }

type diffFilePatch struct {
	from, to fdiff.File
	chunks   []fdiff.Chunk
}

func (p *diffFilePatch) IsBinary() bool                  { return false }
func (p *diffFilePatch) Files() (fdiff.File, fdiff.File) { return p.from, p.to }
func (p *diffFilePatch) Chunks() []fdiff.Chunk           { return p.chunks }

type diffPatch struct {
	filePatches []fdiff.FilePatch
}

func (p *diffPatch) FilePatches() []fdiff.FilePatch { return p.filePatches }
func (p *diffPatch) Message() string                { return "" }

// writePatch generates a StageFreight-native unified diff artifact.
// This is NOT git-apply compatible — it is a deterministic inspection artifact.
// Character-level diffs via diffmatchpatch; no rename/copy detection; no binary handling.
func writePatch(_ context.Context, repoRoot, patchFile string) error {
	repo, err := gitstate.OpenRepo(repoRoot)
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("opening worktree: %w", err)
	}

	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("reading worktree status: %w", err)
	}

	headRef, err := repo.Head()
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}
	headCommit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return fmt.Errorf("resolving HEAD commit: %w", err)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return fmt.Errorf("resolving HEAD tree: %w", err)
	}

	dmp := diffmatchpatch.New()

	// Sort paths for deterministic output.
	paths := make([]string, 0, len(status))
	for p := range status {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var filePatches []fdiff.FilePatch
	for _, path := range paths {
		fs := status[path]
		if fs.Worktree == git.Unmodified && fs.Staging == git.Unmodified {
			continue
		}

		var oldContent, newContent string
		var fromFile, toFile fdiff.File

		// Read old content from HEAD tree (nil for new/untracked files).
		if treeFile, tErr := headTree.File(path); tErr == nil {
			var cErr error
			oldContent, cErr = treeFile.Contents()
			if cErr != nil {
				return fmt.Errorf("reading %s from HEAD: %w", path, cErr)
			}
			fromFile = &diffFileEntry{
				hash: plumbing.ComputeHash(plumbing.BlobObject, []byte(oldContent)),
				mode: filemode.Regular,
				path: path,
			}
		}

		// Read new content from disk (nil for deleted files).
		if data, rErr := os.ReadFile(filepath.Join(repoRoot, path)); rErr == nil {
			newContent = string(data)
			toFile = &diffFileEntry{
				hash: plumbing.ComputeHash(plumbing.BlobObject, []byte(newContent)),
				mode: filemode.Regular,
				path: path,
			}
		}

		if fromFile == nil && toFile == nil {
			continue
		}

		diffs := dmp.DiffMain(oldContent, newContent, false)
		chunks := make([]fdiff.Chunk, 0, len(diffs))
		for _, d := range diffs {
			var op fdiff.Operation
			switch d.Type {
			case diffmatchpatch.DiffEqual:
				op = fdiff.Equal
			case diffmatchpatch.DiffInsert:
				op = fdiff.Add
			case diffmatchpatch.DiffDelete:
				op = fdiff.Delete
			default:
				continue
			}
			chunks = append(chunks, &diffChunk{content: d.Text, op: op})
		}

		filePatches = append(filePatches, &diffFilePatch{
			from:   fromFile,
			to:     toFile,
			chunks: chunks,
		})
	}

	if len(filePatches) == 0 {
		return nil // no changes to write
	}

	f, err := os.Create(patchFile)
	if err != nil {
		return fmt.Errorf("creating patch file: %w", err)
	}
	defer f.Close()

	patch := &diffPatch{filePatches: filePatches}
	if err := fdiff.NewUnifiedEncoder(f, fdiff.DefaultContextLines).Encode(patch); err != nil {
		return fmt.Errorf("encoding patch: %w", err)
	}

	return nil
}

func writeBundle(ctx context.Context, repoRoot, bundleFile string, filesChanged []string) error {
	if len(filesChanged) == 0 {
		return nil
	}

	args := []string{"-czf", bundleFile}
	for _, f := range filesChanged {
		if f != "" {
			args = append(args, "-C", repoRoot, f)
		}
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
