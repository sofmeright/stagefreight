package gitops

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// ImpactResult describes which kustomizations are affected by a set of changes.
type ImpactResult struct {
	ChangedFiles         []string
	DirectlyAffected     []KustomizationKey
	TransitivelyAffected []KustomizationKey
	ReconcileSet         []KustomizationKey // topologically sorted
	UnmappedFiles        []string           // changed files not under any kustomization path
}

// GetChangedFiles returns files changed between two refs.
// Uses three-dot semantics (changes on head's side since divergence from base).
// Falls back to two-dot diff if no merge-base exists.
func GetChangedFiles(repoDir, base, head string) ([]string, error) {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return nil, fmt.Errorf("opening repo: %w", err)
	}

	baseHash, err := repo.ResolveRevision(plumbing.Revision(base))
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", base, err)
	}
	headHash, err := repo.ResolveRevision(plumbing.Revision(head))
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", head, err)
	}

	baseCommit, err := repo.CommitObject(*baseHash)
	if err != nil {
		return nil, fmt.Errorf("base commit: %w", err)
	}
	headCommit, err := repo.CommitObject(*headHash)
	if err != nil {
		return nil, fmt.Errorf("head commit: %w", err)
	}

	// Three-dot: find merge-base, then diff merge-base → head.
	bases, err := baseCommit.MergeBase(headCommit)
	if err == nil && len(bases) > 0 {
		return diffCommitFiles(bases[0], headCommit)
	}

	// Fall back to two-dot diff.
	return diffCommitFiles(baseCommit, headCommit)
}

// diffCommitFiles returns the file paths that differ between two commits.
func diffCommitFiles(from, to *object.Commit) ([]string, error) {
	fromTree, err := from.Tree()
	if err != nil {
		return nil, err
	}
	toTree, err := to.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := fromTree.Diff(toTree)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, c := range changes {
		name := c.To.Name
		if name == "" {
			name = c.From.Name
		}
		if name != "" {
			files = append(files, NormalizePath(name))
		}
	}
	return files, nil
}

// ComputeImpact determines which kustomizations are affected by changed files.
// Walks the reverse dependency graph to find transitive dependents.
func ComputeImpact(graph *FluxGraph, files []string) ImpactResult {
	result := ImpactResult{
		ChangedFiles: files,
	}

	// Map changed files → directly affected kustomizations
	direct := map[KustomizationKey]bool{}
	mapped := map[string]bool{}

	for _, f := range files {
		matched := false
		for _, k := range graph.Kustomizations {
			if k.Path == "" {
				continue
			}
			if pathMatches(f, k.Path) {
				direct[k.Key] = true
				matched = true
			}
		}
		if matched {
			mapped[f] = true
		}
	}

	// Collect unmapped files
	for _, f := range files {
		if !mapped[f] {
			result.UnmappedFiles = append(result.UnmappedFiles, f)
		}
	}

	// BFS reverse deps for transitive impact
	visited := map[KustomizationKey]bool{}
	queue := make([]KustomizationKey, 0, len(direct))
	for k := range direct {
		queue = append(queue, k)
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		for _, dep := range graph.ReverseDeps[cur] {
			if !visited[dep] {
				queue = append(queue, dep)
			}
		}
	}

	// Build result lists
	for k := range direct {
		result.DirectlyAffected = append(result.DirectlyAffected, k)
	}
	SortKeys(result.DirectlyAffected)

	for k := range visited {
		result.TransitivelyAffected = append(result.TransitivelyAffected, k)
	}
	SortKeys(result.TransitivelyAffected)

	// Topological sort for reconcile order
	result.ReconcileSet = TopoSort(graph, result.TransitivelyAffected)

	return result
}

// pathMatches returns true if a file is under a kustomization's path.
func pathMatches(file, kpath string) bool {
	if file == kpath {
		return true
	}
	return strings.HasPrefix(file, kpath+"/")
}

// TopoSort produces a deterministic topological order for a subset of the graph.
// Dependencies come before dependents. Ties broken by namespace/name sort.
func TopoSort(graph *FluxGraph, subset []KustomizationKey) []KustomizationKey {
	inSet := map[KustomizationKey]bool{}
	for _, k := range subset {
		inSet[k] = true
	}

	// Compute in-degree within the subset
	inDegree := map[KustomizationKey]int{}
	for _, k := range subset {
		inDegree[k] = 0
	}
	for _, k := range subset {
		node, ok := graph.Kustomizations[k]
		if !ok {
			continue
		}
		for _, dep := range node.DependsOn {
			if inSet[dep] {
				inDegree[k]++
			}
		}
	}

	// Seed queue with zero in-degree nodes
	var queue []KustomizationKey
	for k, d := range inDegree {
		if d == 0 {
			queue = append(queue, k)
		}
	}
	SortKeys(queue)

	var result []KustomizationKey
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		result = append(result, cur)

		for _, dep := range graph.ReverseDeps[cur] {
			if !inSet[dep] {
				continue
			}
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
				SortKeys(queue)
			}
		}
	}

	// Fallback if cycle detected
	if len(result) != len(subset) {
		SortKeys(subset)
		return subset
	}

	return result
}
