package commit

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// GitBackend executes commits via go-git — no git binary required.
// Push/sync is also handled via go-git through the Engine.
// The legacy shell-out path (STAGEFREIGHT_GOGIT_ENGINE=off) is available
// for local debugging only and requires a git binary in PATH.
type GitBackend struct {
	RootDir string
	// OnCommitLine is called for each output line during hook execution and sync
	// transition events. stream: "stdout", "stderr", "hook_side_effect", "sync".
	// If nil, output is captured but not forwarded.
	OnCommitLine func(stream string, line string)
}

// Execute stages files, creates a commit, and optionally pushes.
// Routes through go-git by default; shell-out legacy path when STAGEFREIGHT_GOGIT_ENGINE=off.
func (g *GitBackend) Execute(_ context.Context, plan *Plan, conventional bool) (*Result, error) {
	if gogitEngineMode() == gogitModeOff {
		return g.executeViaShellOut(plan, conventional)
	}
	return g.executeViaEngine(plan, conventional)
}

// executeViaEngine creates a commit using pure go-git — no git binary required.
func (g *GitBackend) executeViaEngine(plan *Plan, conventional bool) (*Result, error) {
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

	// 2. Read staged files
	status, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("reading worktree status: %w", err)
	}
	files := gitstate.StagedFiles(status)

	// 3. No-op check
	nothingToCommit := len(files) == 0
	if nothingToCommit && !plan.Push.Enabled {
		return &Result{NoOp: true}, nil
	}

	result := &Result{Backend: "go-git", NoOp: nothingToCommit}

	if !nothingToCommit {
		// 4. Resolve author identity: local config → global config → built-in defaults
		sig := resolveAuthorSignature(repo)

		// 5. Build full commit message including SF trailer
		msg := plan.Message(conventional)
		if plan.SignOff {
			msg += "\n\nSigned-off-by: " + sig.Name + " <" + sig.Email + ">"
		}

		// 6. pre-commit hook — abort on non-zero exit
		if err := RunPreCommitHook(g.RootDir, wt, g.OnCommitLine); err != nil {
			return nil, fmt.Errorf("pre-commit hook: %w", err)
		}

		// 7. commit-msg hook — hook may modify the message; re-read after
		msg, err = RunCommitMsgHook(g.RootDir, msg, g.OnCommitLine)
		if err != nil {
			return nil, fmt.Errorf("commit-msg hook: %w", err)
		}

		// 8. Create commit
		hash, err := wt.Commit(msg, &git.CommitOptions{
			Author:    &sig,
			Committer: &sig,
		})
		if err != nil {
			return nil, fmt.Errorf("committing: %w", err)
		}

		// 9. post-commit hook — non-zero exit is a warning, not an abort
		RunPostCommitHook(g.RootDir, g.OnCommitLine)

		result.SHA = hash.String()
		result.Message = msg
		result.Files = files
	}

	// 10. Push via the unified push entry point — runs even when nothing was committed
	if plan.Push.Enabled {
		syncResult, err := g.Push(plan.Push)
		if err != nil {
			return nil, fmt.Errorf("push: %w", err)
		}
		result.Sync = syncResult
		result.Pushed = containsAction(syncResult.ActionsExecuted, SyncPush)
	}

	return result, nil
}

// resolveAuthorSignature reads user.name and user.email from git config.
// Resolution order: local repo config → global config → built-in defaults.
func resolveAuthorSignature(repo *git.Repository) object.Signature {
	name, email := "stagefreight", "stagefreight@localhost"

	if cfg, err := repo.Config(); err == nil {
		if cfg.User.Name != "" {
			name = cfg.User.Name
		}
		if cfg.User.Email != "" {
			email = cfg.User.Email
		}
	}

	// Fall back to global config when local has no user identity configured
	if name == "stagefreight" || email == "stagefreight@localhost" {
		if global, err := gitconfig.LoadConfig(gitconfig.GlobalScope); err == nil {
			if global.User.Name != "" && name == "stagefreight" {
				name = global.User.Name
			}
			if global.User.Email != "" && email == "stagefreight@localhost" {
				email = global.User.Email
			}
		}
	}

	return object.Signature{Name: name, Email: email, When: time.Now()}
}

// BranchFromRefspec extracts the branch name from a refspec like "HEAD:refs/heads/main".
func BranchFromRefspec(refspec string) string {
	if idx := strings.LastIndex(refspec, "refs/heads/"); idx >= 0 {
		return refspec[idx+len("refs/heads/"):]
	}
	return ""
}

// executeViaShellOut is the legacy shell-out commit path.
// Only reachable when STAGEFREIGHT_GOGIT_ENGINE=off — requires a git binary in PATH.
func (g *GitBackend) executeViaShellOut(plan *Plan, conventional bool) (*Result, error) {
	// 1. Stage files
	switch plan.StageMode {
	case StageExplicit:
		for _, p := range plan.Paths {
			if err := g.git("add", p); err != nil {
				return nil, fmt.Errorf("staging %s: %w", p, err)
			}
		}
	case StageAll:
		if err := g.git("add", "-A"); err != nil {
			return nil, fmt.Errorf("staging all: %w", err)
		}
	case StageStaged:
		// nothing
	}

	// 2. Read staged files
	out, err := g.gitOutput("diff", "--cached", "--name-only")
	if err != nil {
		return nil, fmt.Errorf("reading staged files: %w", err)
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}

	// 3. No-op check
	nothingToCommit := len(files) == 0
	if nothingToCommit && !plan.Push.Enabled {
		return &Result{NoOp: true}, nil
	}

	result := &Result{Backend: "git", NoOp: nothingToCommit}

	if !nothingToCommit {
		g.ensureAuthorIdentity()

		subject := plan.Subject(conventional)
		commitArgs := []string{"commit", "-m", subject}
		if plan.Body != "" {
			commitArgs = append(commitArgs, "-m", plan.Body)
		}
		if plan.SignOff {
			commitArgs = append(commitArgs, "--signoff")
		}

		handlers := StreamHandlers{}
		if g.OnCommitLine != nil {
			handlers.OnStdoutLine = func(line string) { g.OnCommitLine("stdout", line) }
			handlers.OnStderrLine = func(line string) { g.OnCommitLine("stderr", line) }
		}
		if _, err := g.gitStream(handlers, commitArgs...); err != nil {
			return nil, fmt.Errorf("committing: %w", err)
		}

		sha, err := g.gitOutput("rev-parse", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("reading commit SHA: %w", err)
		}
		result.SHA = sha
		result.Message = plan.Message(conventional)
		result.Files = files
	}

	if plan.Push.Enabled {
		syncResult, err := g.Push(plan.Push)
		if err != nil {
			return nil, fmt.Errorf("push: %w", err)
		}
		result.Sync = syncResult
		result.Pushed = containsAction(syncResult.ActionsExecuted, SyncPush)
	}

	return result, nil
}

func (g *GitBackend) ensureAuthorIdentity() {
	if name, _ := g.gitOutput("config", "user.name"); name == "" {
		_ = g.git("config", "user.name", "stagefreight")
	}
	if email, _ := g.gitOutput("config", "user.email"); email == "" {
		_ = g.git("config", "user.email", "stagefreight@localhost")
	}
}

func (g *GitBackend) git(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.RootDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StreamHandlers receives incremental stdout/stderr lines from gitStream.
type StreamHandlers struct {
	OnStdoutLine func(string)
	OnStderrLine func(string)
}

// StreamResult holds captured output from a streaming git execution.
type StreamResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

func (g *GitBackend) gitStream(h StreamHandlers, args ...string) (*StreamResult, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.RootDir

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting git %s: %w", args[0], err)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutBuf.WriteString(line + "\n")
			if h.OnStdoutLine != nil {
				h.OnStdoutLine(line)
			}
		}
		if b := scanner.Bytes(); len(b) > 0 {
			stdoutBuf.Write(b)
			if h.OnStdoutLine != nil {
				h.OnStdoutLine(string(b))
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			stderrBuf.WriteString(line + "\n")
			if h.OnStderrLine != nil {
				h.OnStderrLine(line)
			}
		}
		if b := scanner.Bytes(); len(b) > 0 {
			stderrBuf.Write(b)
			if h.OnStderrLine != nil {
				h.OnStderrLine(string(b))
			}
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()

	result := &StreamResult{Stdout: stdoutBuf.Bytes(), Stderr: stderrBuf.Bytes()}
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = 1
		}
		return result, fmt.Errorf("git %s: exit %d: %s", args[0], result.ExitCode,
			strings.TrimSpace(string(result.Stderr)))
	}
	return result, nil
}

func (g *GitBackend) gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.RootDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
