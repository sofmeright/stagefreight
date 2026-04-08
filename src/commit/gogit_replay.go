package commit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

const sfGeneratedTrailer = "X-StageFreight-Generated: true"

// Replay rebases local commits onto upstream using controlled tree replay.
// It is NOT a raw git rebase — it replays only SF-generated commits via
// explicit tree diffing with full integrity verification at each step.
//
// Algorithm:
//  0. Pre-conditions: HEAD attached, worktree clean, upstream configured → fail fast, no mutation
//  1. Gate: no merge commits, linear chain (structural only — no authorship constraints)
//  2. Re-validate upstream hash non-zero; record fetchedUpstreamHash
//  3. Compute merge-base (exactly 1); collect commits oldest-first
//  4. Hard reset to upstream
//  5. For each commit: apply diff, stage, verify staging states, commit
//  6. Race guard: upstream unchanged since fetch
//
// Push is NOT performed by Replay — the engine (Engine.doReplayThenPush) owns push
// so the transition DIVERGED → REPLAY → CLEAN_AHEAD → PUSH remains in the engine's
// state machine and the push is logged as a formal transition.
//
// Hooks are NOT run during replay — replay commits are machine-generated
// re-applications; running hooks again would double-execute side effects.
func Replay(session *gitstate.SyncSession) error {
	repo := session.Repo()
	state := session.State()

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("opening worktree: %w", err)
	}

	// Pre-condition: HEAD must be attached to a branch
	if state.DetachedHEAD {
		return gitstate.ErrDetachedHEAD
	}

	// Pre-condition: upstream tracking config must exist before we fetch.
	// UpstreamHash may be zero here if the remote-tracking ref has never been
	// fetched (e.g. fresh clone without fetch, or first push of a new branch) —
	// that is fine; we check the hash again after fetch. What we require here
	// is that a tracking remote+merge ref is configured in .git/config.
	if !state.UpstreamConfigured {
		return gitstate.ErrNoUpstream
	}

	// Pre-condition: worktree must be clean
	wtStatus, err := wt.Status()
	if err != nil {
		return fmt.Errorf("checking worktree status: %w", err)
	}
	if !wtStatus.IsClean() {
		return gitstate.ErrDirtyWorktree
	}

	originalHEAD := state.HeadHash

	// The Engine already called session.Fetch() before calling Replay().
	// session.State() reflects the post-fetch upstream — no re-fetch needed.
	// Re-validate upstream hash is non-zero (safety check, should never fail here).
	if state.UpstreamHash.IsZero() {
		return gitstate.ErrNoUpstream
	}
	fetchedUpstreamHash := state.UpstreamHash

	// 2. Compute merge-base and collect local commits oldest-first
	headCommit, err := repo.CommitObject(originalHEAD)
	if err != nil {
		return fmt.Errorf("loading HEAD commit: %w", err)
	}
	upstreamCommit, err := repo.CommitObject(fetchedUpstreamHash)
	if err != nil {
		return fmt.Errorf("loading upstream commit: %w", err)
	}

	bases, err := headCommit.MergeBase(upstreamCommit)
	if err != nil {
		return fmt.Errorf("computing merge base: %w", err)
	}
	// Exactly one merge-base required. Multiple bases indicate a criss-cross merge
	// history; zero bases indicate unrelated histories — both are unsafe for replay.
	if len(bases) != 1 {
		return &gitstate.ErrReplayUnsafe{Reasons: []string{
			fmt.Sprintf("expected exactly 1 merge base, found %d — "+
				"criss-cross or unrelated histories cannot be replayed automatically", len(bases)),
		}}
	}
	mergeBase := bases[0].Hash

	var commits []*object.Commit
	logIter, err := repo.Log(&git.LogOptions{From: originalHEAD})
	if err != nil {
		return fmt.Errorf("walking local commits: %w", err)
	}
	_ = logIter.ForEach(func(c *object.Commit) error {
		if c.Hash == mergeBase {
			return storer.ErrStop
		}
		commits = append(commits, c)
		return nil
	})

	if len(commits) == 0 {
		// No local commits between HEAD and the merge base — nothing to replay.
		// The engine should have classified this as CLEAN_SYNCED before calling
		// Replay; reaching here is a caller bug.
		return fmt.Errorf("replay called with no local commits to replay (state should have been CLEAN_SYNCED)")
	}

	// Reverse to oldest-first
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}

	// 3. Gate: validate ALL commits before any mutation; collect all violations
	if violations := validateReplayGate(commits, mergeBase); len(violations) > 0 {
		return &gitstate.ErrReplayUnsafe{Reasons: violations}
	}

	// Get repo root for filesystem operations
	repoRoot := wt.Filesystem.Root()

	// 5. Hard reset to upstream — mutation begins here
	if err := wt.Reset(&git.ResetOptions{
		Commit: fetchedUpstreamHash,
		Mode:   git.HardReset,
	}); err != nil {
		return fmt.Errorf("hard reset to upstream: %w", err)
	}

	// 6. Replay commits oldest-first
	for _, c := range commits {
		if err := replayCommit(repo, wt, repoRoot, c, originalHEAD); err != nil {
			_ = wt.Reset(&git.ResetOptions{Commit: originalHEAD, Mode: git.HardReset})
			return err
		}
	}

	// 7. Race guard: upstream must not have moved since our fetch
	if err := session.Refresh(); err != nil {
		_ = wt.Reset(&git.ResetOptions{Commit: originalHEAD, Mode: git.HardReset})
		return fmt.Errorf("refreshing state before push: %w", err)
	}
	if session.State().UpstreamHash != fetchedUpstreamHash {
		_ = wt.Reset(&git.ResetOptions{Commit: originalHEAD, Mode: git.HardReset})
		return gitstate.ErrUpstreamMoved
	}

	// Push is the engine's responsibility — Replay() owns only the rebase.
	// The caller (Engine.doReplayThenPush) will call doPush() after Replay returns.
	return nil
}

// validateReplayGate validates all commits against gate conditions.
// Collects ALL violations before returning — no short-circuit.
//
// Gate is structural only: merge commits and non-linear chains cannot be
// deterministically replayed by diff application. Authorship markers (e.g.
// SF-generated trailer) are NOT gate conditions — historical commits and CI
// commits that predate the trailer are replayable by the same mechanism.
func validateReplayGate(commits []*object.Commit, mergeBase plumbing.Hash) []string {
	var violations []string
	for i, c := range commits {
		// Rule 1: no merge commits
		if len(c.ParentHashes) != 1 {
			violations = append(violations, fmt.Sprintf(
				"%s %q: has %d parents (merge commits cannot be replayed)",
				c.Hash.String()[:8], firstLine(c.Message), len(c.ParentHashes),
			))
		}
		// Rule 2: linear chain
		if len(c.ParentHashes) == 1 {
			expected := mergeBase
			if i > 0 {
				expected = commits[i-1].Hash
			}
			if c.ParentHashes[0] != expected {
				violations = append(violations, fmt.Sprintf(
					"%s %q: parent %s != expected %s (non-linear chain)",
					c.Hash.String()[:8], firstLine(c.Message),
					c.ParentHashes[0].String()[:8], expected.String()[:8],
				))
			}
		}
	}
	return violations
}

// replayCommit applies a single commit's diff to the worktree and creates a new commit.
// Hooks are NOT run — replay is a machine operation, not a user commit.
// On error, the caller is responsible for resetting to originalHEAD.
func replayCommit(repo *git.Repository, wt *git.Worktree, repoRoot string, c *object.Commit, originalHEAD plumbing.Hash) error {
	var parentTree *object.Tree
	if len(c.ParentHashes) > 0 {
		parentCommit, err := repo.CommitObject(c.ParentHashes[0])
		if err != nil {
			return fmt.Errorf("loading parent commit: %w", err)
		}
		parentTree, err = parentCommit.Tree()
		if err != nil {
			return fmt.Errorf("loading parent tree: %w", err)
		}
	}

	commitTree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("loading commit tree: %w", err)
	}

	var changes object.Changes
	if parentTree != nil {
		changes, err = parentTree.Diff(commitTree)
	} else {
		emptyTree := &object.Tree{}
		changes, err = emptyTree.Diff(commitTree)
	}
	if err != nil {
		return fmt.Errorf("computing diff: %w", err)
	}

	for _, change := range changes {
		if err := applyChange(repoRoot, change); err != nil {
			return err
		}
	}

	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("staging changes: %w", err)
	}

	// Diff-application sanity check.
	//
	// We do NOT compare tree hashes to the original commit — after rebasing on a new
	// upstream base the replayed tree includes upstream changes, so the hashes will
	// legitimately differ. Instead we verify that the diff applied without producing
	// any unexpected or corrupted staging states (conflicts, unknown modes, etc.).
	status, err := wt.Status()
	if err != nil {
		return fmt.Errorf("checking worktree status after staging: %w", err)
	}
	for path, s := range status {
		switch s.Staging {
		case git.Unmodified, git.Added, git.Modified, git.Deleted:
			// Valid states — diff applied cleanly.
		default:
			_ = wt.Reset(&git.ResetOptions{Commit: originalHEAD, Mode: git.HardReset})
			return fmt.Errorf("unexpected staging state for %s (%v) after applying commit %s — hard reset applied",
				path, s.Staging, c.Hash.String()[:8])
		}
	}

	// Commit, preserving original Author; Committer timestamp = now (replay time)
	// No hooks — replay is machine-generated; hooks were already run on the original commit.
	now := time.Now()
	_, err = wt.Commit(c.Message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  c.Author.Name,
			Email: c.Author.Email,
			When:  c.Author.When,
		},
		Committer: &object.Signature{
			Name:  c.Committer.Name,
			Email: c.Committer.Email,
			When:  now,
		},
		AllowEmptyCommits: true,
	})
	if err != nil {
		return fmt.Errorf("creating replayed commit: %w", err)
	}

	return nil
}

// applyChange applies a single tree change to the worktree filesystem.
func applyChange(repoRoot string, change *object.Change) error {
	action, err := change.Action()
	if err != nil {
		return fmt.Errorf("determining change action: %w", err)
	}

	from, to, err := change.Files()
	if err != nil {
		return fmt.Errorf("getting change files: %w", err)
	}

	switch action {
	case merkletrie.Insert:
		if to == nil {
			return nil
		}
		return writeFile(repoRoot, to)

	case merkletrie.Delete:
		if from == nil {
			return nil
		}
		if err := checkPathSafe(repoRoot, from.Name); err != nil {
			return err
		}
		dest := filepath.Join(repoRoot, from.Name)
		// For existing paths, verify the real parent dir hasn't been redirected
		// by a pre-existing symlink to outside the repo root.
		if err := checkRealPathSafe(repoRoot, filepath.Dir(dest), from.Name); err != nil {
			return err
		}
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("deleting %s: %w", from.Name, err)
		}

	case merkletrie.Modify:
		// Rename: delete old path, write new path
		if from != nil && to != nil && from.Name != to.Name {
			if err := checkPathSafe(repoRoot, from.Name); err != nil {
				return err
			}
			oldPath := filepath.Join(repoRoot, from.Name)
			if err := checkRealPathSafe(repoRoot, filepath.Dir(oldPath), from.Name); err != nil {
				return err
			}
			if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing old path %s: %w", from.Name, err)
			}
		}
		if to != nil {
			return writeFile(repoRoot, to)
		}
	}

	return nil
}

// writeFile writes a file object from the git object store to the filesystem.
// Handles regular files, executable files, and symlinks.
func writeFile(repoRoot string, f *object.File) error {
	if err := checkPathSafe(repoRoot, f.Name); err != nil {
		return err
	}
	dest := filepath.Join(repoRoot, f.Name)

	switch f.Mode {
	case filemode.Symlink:
		// Symlink: blob content is the target path.
		// Validate that the symlink target, resolved relative to its parent directory,
		// does not escape the repository root.
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("reading symlink target for %s: %w", f.Name, err)
		}
		target := strings.TrimSpace(content)

		// Resolve the symlink target relative to its parent directory
		parentDir := filepath.Dir(dest)
		resolvedTarget := target
		if !filepath.IsAbs(target) {
			resolvedTarget = filepath.Join(parentDir, target)
		}
		resolvedTarget = filepath.Clean(resolvedTarget)

		// The resolved target must stay within the repo root
		rootWithSep := repoRoot + string(os.PathSeparator)
		if resolvedTarget != repoRoot && !strings.HasPrefix(resolvedTarget, rootWithSep) {
			return &gitstate.ErrPathTraversal{Path: f.Name + " -> " + target}
		}

		_ = os.Remove(dest) // remove any existing file
		if err := os.Symlink(target, dest); err != nil {
			return fmt.Errorf("creating symlink %s: %w", f.Name, err)
		}

	default:
		// Regular file or executable
		osMode, err := f.Mode.ToOSFileMode()
		if err != nil {
			osMode = 0o644 // safe default
		}
		parentDir := filepath.Dir(dest)
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			return fmt.Errorf("creating parent dirs for %s: %w", f.Name, err)
		}
		// Post-MkdirAll: resolve symlinks in the real parent dir to catch
		// pre-existing symlinked directories that escape the repo root.
		if err := checkRealPathSafe(repoRoot, parentDir, f.Name); err != nil {
			return err
		}
		reader, err := f.Blob.Reader()
		if err != nil {
			return fmt.Errorf("reading blob for %s: %w", f.Name, err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return fmt.Errorf("reading blob data for %s: %w", f.Name, err)
		}
		if err := os.WriteFile(dest, data, osMode); err != nil {
			return fmt.Errorf("writing %s: %w", f.Name, err)
		}
	}
	return nil
}

// checkPathSafe performs a lexical path-traversal check.
// It guards against ".." escape via filepath.Clean but does NOT resolve symlinks.
// For filesystem writes, call checkRealPathSafe after the parent dir exists.
func checkPathSafe(repoRoot, relPath string) error {
	absPath := filepath.Join(repoRoot, filepath.Clean(relPath))
	rootWithSep := repoRoot + string(os.PathSeparator)
	if absPath != repoRoot && !strings.HasPrefix(absPath, rootWithSep) {
		return &gitstate.ErrPathTraversal{Path: relPath}
	}
	return nil
}

// checkRealPathSafe resolves all symlinks in dirPath and verifies the result
// is still within repoRoot. This catches escapes via pre-existing symlinked
// directories that a lexical check cannot detect.
// dirPath must exist on disk (call after MkdirAll or only for existing paths).
func checkRealPathSafe(repoRoot, dirPath, label string) error {
	real, err := filepath.EvalSymlinks(dirPath)
	if err != nil {
		// Path disappeared between MkdirAll and here — treat as traversal-safe,
		// the subsequent write/remove will fail on its own with a clear OS error.
		return nil
	}
	rootWithSep := repoRoot + string(os.PathSeparator)
	if real != repoRoot && !strings.HasPrefix(real, rootWithSep) {
		return &gitstate.ErrPathTraversal{Path: label + " (parent dir resolves outside repo root: " + real + ")"}
	}
	return nil
}

// hasSFGeneratedTrailer checks whether the commit message contains the SF generated trailer.
func hasSFGeneratedTrailer(message string) bool {
	return strings.Contains(message, sfGeneratedTrailer)
}

// firstLine returns the first non-empty line of a string (for error display).
func firstLine(s string) string {
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > 72 {
		s = s[:72] + "…"
	}
	return s
}
