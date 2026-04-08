package gitstate

import (
	"fmt"

	git "github.com/go-git/go-git/v5"
)

// OpenRepo is the single entry point for all *git.Repository instances.
// No package outside src/gitstate/ or src/commit/ may call git.PlainOpen directly.
// DetectDotGit walks parent directories to find .git, matching git CLI behaviour.
func OpenRepo(rootDir string) (*git.Repository, error) {
	return git.PlainOpenWithOptions(rootDir, &git.PlainOpenOptions{
		DetectDotGit: true,
	})
}

// RepoRoot returns the absolute path of the repository root directory (the
// directory containing .git). Use this instead of wt.Filesystem.Root() directly
// — encapsulates the go-git worktree filesystem contract in one place.
func RepoRoot(repo *git.Repository) (string, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("opening worktree: %w", err)
	}
	return wt.Filesystem.Root(), nil
}
