package commit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitBackend executes commits via go-git (no git binary required).
type GitBackend struct {
	RootDir string
	// OnCommitLine is called for each stdout/stderr line during commit hooks
	// and sync transition events. stream is one of "stdout", "stderr",
	// "hook_side_effect", or "sync".
	OnCommitLine func(stream string, line string)
}

// Execute stages files, creates a commit (running hooks), and optionally pushes.
func (g *GitBackend) Execute(_ context.Context, plan *Plan, conventional bool) (*Result, error) {
	repo, err := gitstate.OpenRepo(g.RootDir)
	if err != nil {
		return nil, fmt.Errorf("opening repo: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("opening worktree: %w", err)
	}

	// 1. Stage files
	switch plan.StageMode {
	case StageExplicit:
		for _, p := range plan.Paths {
			if _, err := wt.Add(p); err != nil {
				return nil, fmt.Errorf("staging %s: %w", p, err)
			}
		}
	case StageAll:
		if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			return nil, fmt.Errorf("staging all: %w", err)
		}
	case StageStaged:
		// nothing — use whatever is already staged
	}

	// 2. Capture actual staged files via go-git status
	status, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("reading staged files: %w", err)
	}
	files := gitstate.StagedFiles(status)

	// 3. No-op check
	nothingToCommit := len(files) == 0
	if nothingToCommit && !plan.Push.Enabled {
		return &Result{NoOp: true}, nil
	}

	result := &Result{Backend: "gogit", NoOp: nothingToCommit}

	if !nothingToCommit {
		// 4. Ensure git author identity exists (CI images often lack it)
		authorName, authorEmail := g.resolveAuthorIdentity(repo)

		// 5. Run hook sequence: pre-commit → commit-msg → commit → post-commit
		hookCB := g.hookCallback()

		if err := RunPreCommitHook(g.RootDir, wt, hookCB); err != nil {
			return nil, err
		}

		message := plan.Message(conventional)
		message, err = RunCommitMsgHook(g.RootDir, message, hookCB)
		if err != nil {
			return nil, err
		}

		// Trailer invariant: commit-msg hooks must not strip the SF-generated trailer.
		// Replay safety depends on this trailer being present on every SF commit.
		if !strings.Contains(message, sfGeneratedTrailer) {
			return nil, fmt.Errorf(
				"commit-msg hook stripped %q trailer — hooks must preserve SF-generated trailers",
				sfGeneratedTrailer,
			)
		}

		// 6. Commit via go-git
		now := time.Now()
		newHash, err := wt.Commit(message, &git.CommitOptions{
			Author: &object.Signature{
				Name:  authorName,
				Email: authorEmail,
				When:  now,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("creating commit: %w", err)
		}

		RunPostCommitHook(g.RootDir, hookCB)

		result.SHA = newHash.String()
		result.Message = message
		result.Files = files
	}

	// 7. Push via convergence engine
	if plan.Push.Enabled {
		syncResult, err := g.Push(plan.Push)
		if err != nil {
			return nil, err
		}
		result.Sync = syncResult
		result.Pushed = containsAction(syncResult.ActionsExecuted, SyncPush)
	}

	return result, nil
}

// resolveAuthorIdentity returns the git user name and email for commits.
// Falls back to stagefreight defaults when not configured.
func (g *GitBackend) resolveAuthorIdentity(repo *git.Repository) (name, email string) {
	cfg, err := repo.Config()
	if err == nil {
		name = cfg.User.Name
		email = cfg.User.Email
	}
	if name == "" {
		name = "stagefreight"
	}
	if email == "" {
		email = "stagefreight@localhost"
	}
	return name, email
}

// hookCallback wraps OnCommitLine for use with the hook runner.
func (g *GitBackend) hookCallback() func(stream, line string) {
	if g.OnCommitLine == nil {
		return nil
	}
	return g.OnCommitLine
}

// BranchFromRefspec extracts the branch name from a refspec like "HEAD:refs/heads/main".
func BranchFromRefspec(refspec string) string {
	const headsPrefix = "refs/heads/"
	if idx := strings.LastIndex(refspec, headsPrefix); idx >= 0 {
		return refspec[idx+len(headsPrefix):]
	}
	return ""
}
