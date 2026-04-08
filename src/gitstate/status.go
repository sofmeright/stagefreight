package gitstate

import (
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
)

// WorktreeStatus returns the current worktree status (staged + unstaged + untracked).
// Equivalent to `git status --porcelain`.
func WorktreeStatus(wt *git.Worktree) (git.Status, error) {
	return wt.Status()
}

// IsClean returns true when the status has no modifications of any kind.
func IsClean(s git.Status) bool {
	return s.IsClean()
}

// StagedFiles returns paths with staged changes (Staging != Unmodified).
func StagedFiles(s git.Status) []string {
	var out []string
	for path, fs := range s {
		if fs.Staging != git.Unmodified {
			out = append(out, path)
		}
	}
	return out
}

// AllChangedFiles returns paths with any modification (staged, unstaged, or untracked).
func AllChangedFiles(s git.Status) []string {
	var out []string
	for path, fs := range s {
		if fs.Staging != git.Unmodified || fs.Worktree != git.Unmodified {
			out = append(out, path)
		}
	}
	return out
}

// UnstagedDirtyPaths returns paths with unstaged modifications (not staged, not untracked).
func UnstagedDirtyPaths(s git.Status) []string {
	var out []string
	for path, fs := range s {
		if fs.Worktree != git.Unmodified && fs.Worktree != git.Untracked {
			if fs.Staging == git.Unmodified {
				out = append(out, path)
			}
		}
	}
	return out
}

// AllDirtyPaths returns paths with staged or unstaged modifications, excluding
// untracked files. Use for bundle/artifact generation where both staged and
// unstaged changes must be captured. Output is sorted for deterministic ordering.
func AllDirtyPaths(s git.Status) []string {
	var out []string
	for path, fs := range s {
		staged := fs.Staging != git.Unmodified
		unstaged := fs.Worktree != git.Unmodified && fs.Worktree != git.Untracked
		if staged || unstaged {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// HasStagedChanges returns true when any file has a staged modification.
func HasStagedChanges(s git.Status) bool {
	for _, fs := range s {
		if fs.Staging != git.Unmodified {
			return true
		}
	}
	return false
}

// HasUnstagedChanges returns true when any tracked file has unstaged modifications.
func HasUnstagedChanges(s git.Status) bool {
	for _, fs := range s {
		if fs.Worktree != git.Unmodified && fs.Worktree != git.Untracked {
			return true
		}
	}
	return false
}

// StatusFileChange represents a file with its change status for the forge backend.
type StatusFileChange struct {
	Path    string
	Deleted bool
}

// ChangedFiles returns all changed files (staged + unstaged + untracked) with delete status.
// Equivalent to `git status --porcelain=v1 -z -uall`.
func ChangedFiles(s git.Status) []StatusFileChange {
	return filterStatusChanges(s, func(path string, fs *git.FileStatus) bool {
		return fs.Staging != git.Unmodified || fs.Worktree != git.Unmodified
	})
}

// ChangedFilesInDir returns changed files within a specific directory prefix.
func ChangedFilesInDir(s git.Status, dir string) []StatusFileChange {
	prefix := dir
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return filterStatusChanges(s, func(path string, fs *git.FileStatus) bool {
		if !strings.HasPrefix(path, prefix) && path != dir {
			return false
		}
		return fs.Staging != git.Unmodified || fs.Worktree != git.Unmodified
	})
}

// StagedChanges returns staged files with delete status.
// Equivalent to `git diff --cached --name-status`.
func StagedChanges(s git.Status) []StatusFileChange {
	return filterStatusChanges(s, func(_ string, fs *git.FileStatus) bool {
		return fs.Staging != git.Unmodified
	})
}

func filterStatusChanges(s git.Status, include func(string, *git.FileStatus) bool) []StatusFileChange {
	var out []StatusFileChange
	for path, fs := range s {
		if !include(path, fs) {
			continue
		}
		deleted := fs.Staging == git.Deleted || fs.Worktree == git.Deleted
		out = append(out, StatusFileChange{Path: path, Deleted: deleted})
	}
	return out
}
