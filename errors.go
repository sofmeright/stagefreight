package gitstate

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
)

// ErrDetachedHEAD is returned when HEAD is not on a named branch.
var ErrDetachedHEAD = errors.New("HEAD is not on a branch — checkout a named branch first")

// ErrDirtyWorktree is returned when uncommitted changes are present before replay.
var ErrDirtyWorktree = errors.New("worktree has uncommitted changes — commit or stash first")

// ErrUpstreamMoved is returned when the remote ref changes between fetch and push.
var ErrUpstreamMoved = errors.New("upstream ref changed between fetch and push — retry from fetch")

// ErrNoUpstream is returned when the current branch has no tracking upstream configured.
var ErrNoUpstream = errors.New("branch has no upstream tracking ref — run: git branch --set-upstream-to=<remote>/<branch>")

// ErrReplayUnsafe is returned when the replay gate rejects commits before any mutation.
// This is distinct from ErrReplayCorrupted: no mutation has occurred.
type ErrReplayUnsafe struct {
	Reasons []string // descriptions of gate violations per commit
}

func (e *ErrReplayUnsafe) Error() string {
	return fmt.Sprintf("replay gate failed (%d violation(s)):\n  - %s",
		len(e.Reasons), joinLines(e.Reasons))
}

// ErrIndexDrift is returned when the index diverges from the intended tree mid-replay.
// A hard reset to originalHEAD is performed before this error is returned.
type ErrIndexDrift struct {
	CommitHash plumbing.Hash
}

func (e *ErrIndexDrift) Error() string {
	return fmt.Sprintf("index drifted from intended tree at commit %s — hard reset applied", e.CommitHash)
}

// ErrReplayCorrupted is returned when the post-replay tree does not match the original HEAD tree.
// Mutation occurred and diverged. A hard reset to originalHEAD is performed before returning.
type ErrReplayCorrupted struct {
	Original plumbing.Hash
	Replayed plumbing.Hash
}

func (e *ErrReplayCorrupted) Error() string {
	return fmt.Sprintf("replay corrupted: original tree %s != replayed tree %s — hard reset applied",
		e.Original, e.Replayed)
}

// ErrHookRejected is returned when a pre-commit or commit-msg hook exits non-zero.
type ErrHookRejected struct {
	Hook     string
	ExitCode int
}

func (e *ErrHookRejected) Error() string {
	return fmt.Sprintf("hook %q rejected commit (exit %d)", e.Hook, e.ExitCode)
}

// ErrPathTraversal is returned when a diff path would escape the repository root.
type ErrPathTraversal struct {
	Path string
}

func (e *ErrPathTraversal) Error() string {
	return fmt.Sprintf("path traversal detected: %q escapes repository root — aborting", e.Path)
}

// ErrTransport wraps SSH auth or push failures with an actionable message.
type ErrTransport struct {
	Err error
	Msg string
}

func (e *ErrTransport) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "transport error"
}

func (e *ErrTransport) Unwrap() error { return e.Err }

func joinLines(lines []string) string {
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n  - "
		}
		result += l
	}
	return result
}
