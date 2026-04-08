package gitstate

import (
	"fmt"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// RepoState is the result of interrogating the current repository condition.
// ReadRepoState always returns a fully populated struct — callers must check
// DetachedHEAD and UpstreamConfigured before interpreting other fields.
//
// All fields are resolved once at read time. No polling.
type RepoState struct {
	Branch             string        // current branch name (empty if DetachedHEAD)
	UpstreamRef        string        // e.g. "origin/main" — empty if not configured
	UpstreamConfigured bool
	AheadCount         int           // commits local has that remote does not
	BehindCount        int           // commits remote has that local does not
	DetachedHEAD       bool
	WorktreeClean      bool          // true when no staged or unstaged changes exist
	HeadHash           plumbing.Hash // current commit hash
	UpstreamHash       plumbing.Hash // remote tracking hash (zero if not configured)
	RemoteName         string        // e.g. "origin"
}

// Diverged returns true when local and remote have independent commits.
func (s RepoState) Diverged() bool {
	return s.AheadCount > 0 && s.BehindCount > 0
}

// ReadRepoState reads the current repository state.
// This is the single read-only knowledge pool for all repo facts.
// No package should read HEAD, upstream, or branch state independently.
func ReadRepoState(repo *git.Repository) (RepoState, error) {
	var state RepoState

	// Worktree cleanliness — resolved before anything else so Classify() works
	// immediately on the returned state without a second call.
	wt, wtErr := repo.Worktree()
	if wtErr != nil {
		return state, fmt.Errorf("opening worktree: %w", wtErr)
	}
	wtStatus, sErr := wt.Status()
	if sErr != nil {
		return state, fmt.Errorf("reading worktree status: %w", sErr)
	}
	state.WorktreeClean = wtStatus.IsClean()

	// Read raw HEAD to distinguish attached vs detached.
	// repo.Head() always resolves to the commit hash — reading plumbing.HEAD
	// directly is the only reliable way to detect detached HEAD.
	rawHEAD, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return state, fmt.Errorf("reading HEAD: %w", err)
	}

	// Resolve HEAD to commit hash
	head, err := repo.Head()
	if err != nil {
		return state, fmt.Errorf("resolving HEAD: %w", err)
	}
	state.HeadHash = head.Hash()

	// Detached HEAD: rawHEAD is a hash reference (not a symbolic ref to a branch)
	if rawHEAD.Type() != plumbing.SymbolicReference {
		state.DetachedHEAD = true
		return state, nil
	}

	// Attached HEAD: rawHEAD.Target() is refs/heads/<branch>
	if !rawHEAD.Target().IsBranch() {
		state.DetachedHEAD = true
		return state, nil
	}

	state.Branch = rawHEAD.Target().Short()

	// Look up upstream tracking configuration from .git/config
	cfg, err := repo.Config()
	if err != nil {
		return state, fmt.Errorf("reading git config: %w", err)
	}

	branchCfg, ok := cfg.Branches[state.Branch]
	if !ok || branchCfg.Remote == "" || branchCfg.Merge == "" {
		// No upstream configured — normal for a freshly created branch
		state.UpstreamConfigured = false
		return state, nil
	}

	state.UpstreamConfigured = true
	state.RemoteName = branchCfg.Remote
	// UpstreamRef in the "origin/main" form used throughout the codebase
	state.UpstreamRef = branchCfg.Remote + "/" + branchCfg.Merge.Short()

	// Resolve the upstream tracking ref hash from local ref store (no network)
	upstreamRefName := plumbing.NewRemoteReferenceName(branchCfg.Remote, branchCfg.Merge.Short())
	upstreamRef, err := repo.Reference(upstreamRefName, true)
	if err != nil {
		// Upstream ref not yet fetched — no counts available, not an error
		return state, nil
	}
	state.UpstreamHash = upstreamRef.Hash()

	// Compute ahead/behind counts
	ahead, behind, err := countAheadBehind(repo, state.HeadHash, state.UpstreamHash)
	if err != nil {
		// Non-fatal: leave counts at zero if merge-base fails
		return state, nil
	}
	state.AheadCount = ahead
	state.BehindCount = behind

	return state, nil
}

// countAheadBehind returns the number of commits head is ahead of upstream
// and the number it is behind, using the merge-base as the boundary.
func countAheadBehind(repo *git.Repository, headHash, upstreamHash plumbing.Hash) (ahead, behind int, err error) {
	if headHash == upstreamHash {
		return 0, 0, nil
	}

	headCommit, err := repo.CommitObject(headHash)
	if err != nil {
		return 0, 0, fmt.Errorf("loading HEAD commit: %w", err)
	}
	upstreamCommit, err := repo.CommitObject(upstreamHash)
	if err != nil {
		return 0, 0, fmt.Errorf("loading upstream commit: %w", err)
	}

	bases, err := headCommit.MergeBase(upstreamCommit)
	if err != nil {
		return 0, 0, fmt.Errorf("computing merge base: %w", err)
	}

	var mergeBase plumbing.Hash
	if len(bases) > 0 {
		mergeBase = bases[0].Hash
	}

	// Count commits from HEAD to merge-base (these are "ahead" = local only)
	logIter, err := repo.Log(&git.LogOptions{From: headHash})
	if err != nil {
		return 0, 0, err
	}
	_ = logIter.ForEach(func(c *object.Commit) error {
		if !mergeBase.IsZero() && c.Hash == mergeBase {
			return storer.ErrStop
		}
		ahead++
		return nil
	})

	// Count commits from upstream to merge-base (these are "behind" = remote only)
	logIter2, err := repo.Log(&git.LogOptions{From: upstreamHash})
	if err != nil {
		return 0, 0, err
	}
	_ = logIter2.ForEach(func(c *object.Commit) error {
		if !mergeBase.IsZero() && c.Hash == mergeBase {
			return storer.ErrStop
		}
		behind++
		return nil
	})

	return ahead, behind, nil
}
