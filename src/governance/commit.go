package governance

import (
	"fmt"
	"strings"
)

// ForgeClient abstracts forge API for file commits.
// Extends ForgeReader with write capability.
type ForgeClient interface {
	ForgeReader

	// CommitFiles commits multiple file changes to a repo's default branch.
	// Returns commit SHA on success.
	CommitFiles(repo, branch, message string, files []FileCommit) (string, error)

	// DefaultBranch returns the default branch name for a repo.
	DefaultBranch(repo string) (string, error)
}

// FileCommit is a single file change within a commit.
type FileCommit struct {
	Path    string
	Content []byte
	Action  string // "create", "update"
}

// CommitDistribution executes distribution plans by committing to target repos.
// Per-repo failure does NOT stop the run. Aggregates results.
// Idempotent: skips repos where all files are unchanged.
// Returns error if ANY repo failed.
func CommitDistribution(plans []DistributionPlan, forge ForgeClient, sourceIdentity, sourceRef string, dryRun bool) ([]CommitResult, error) {
	var results []CommitResult
	var anyError bool

	for _, plan := range plans {
		result := commitRepo(plan, forge, sourceIdentity, sourceRef, dryRun)
		results = append(results, result)
		if result.Error != nil {
			anyError = true
		}
	}

	if anyError {
		return results, fmt.Errorf("governance reconcile completed with errors (see individual results)")
	}
	return results, nil
}

func commitRepo(plan DistributionPlan, forge ForgeClient, sourceIdentity, sourceRef string, dryRun bool) CommitResult {
	result := CommitResult{Repo: plan.Repo}

	// Check if anything needs writing.
	if !plan.HasChanges() {
		result.Status = "skipped-identical"
		result.Message = "all files unchanged"
		return result
	}

	// Check for drift.
	for _, f := range plan.Files {
		if f.Drifted {
			result.Drifted = true
			break
		}
	}

	if dryRun {
		result.Status = "dry-run"
		result.Message = describePlan(plan)
		return result
	}

	// Get default branch.
	branch, err := forge.DefaultBranch(plan.Repo)
	if err != nil {
		result.Status = "error"
		result.Error = fmt.Errorf("getting default branch: %w", err)
		return result
	}

	// Build file commits (skip unchanged).
	var files []FileCommit
	for _, f := range plan.Files {
		if f.Action == "unchanged" {
			continue
		}
		action := "update"
		if f.Action == "create" {
			action = "create"
		}
		files = append(files, FileCommit{
			Path:    f.Path,
			Content: f.Content,
			Action:  action,
		})
	}

	if len(files) == 0 {
		result.Status = "skipped-identical"
		result.Message = "no files to commit after filtering"
		return result
	}

	// Build attributable commit message.
	message := buildCommitMessage(plan, sourceIdentity, sourceRef)

	sha, err := forge.CommitFiles(plan.Repo, branch, message, files)
	if err != nil {
		result.Status = "error"
		result.Error = fmt.Errorf("committing to %s: %w", plan.Repo, err)
		return result
	}

	result.Status = "committed"
	result.SHA = sha
	result.Message = describePlan(plan)
	return result
}

// buildCommitMessage creates an attributable commit message.
func buildCommitMessage(plan DistributionPlan, sourceIdentity, sourceRef string) string {
	var filePaths []string
	for _, f := range plan.Files {
		if f.Action != "unchanged" {
			filePaths = append(filePaths, f.Path)
		}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("chore: governance reconcile from %s %s\n\n", sourceIdentity, sourceRef))
	b.WriteString(fmt.Sprintf("Files: %s\n", strings.Join(filePaths, ", ")))

	// Note drift if detected.
	for _, f := range plan.Files {
		if f.Drifted {
			b.WriteString(fmt.Sprintf("Drift detected: %s (replaced)\n", f.Path))
		}
	}

	return b.String()
}

// describePlan summarizes what a distribution plan will do.
func describePlan(plan DistributionPlan) string {
	var parts []string
	for _, f := range plan.Files {
		if f.Action != "unchanged" {
			label := f.Action
			if f.Drifted {
				label += " (drifted)"
			}
			parts = append(parts, fmt.Sprintf("%s: %s", f.Path, label))
		}
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}
