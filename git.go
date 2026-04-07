package commit

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// GitBackend executes commits via the git CLI.
type GitBackend struct {
	RootDir string
	// OnCommitLine is called for each stdout/stderr line during git commit.
	// This allows the commit command to render hook output in structured form.
	// If nil, hook output is captured but not forwarded.
	OnCommitLine func(stream string, line string) // stream: "stdout" or "stderr"
}

// Execute stages files, creates a commit, and optionally pushes via git.
func (g *GitBackend) Execute(_ context.Context, plan *Plan, conventional bool) (*Result, error) {
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
		// nothing — use whatever is already staged
	}

	// 2. Capture actual staged files
	files, err := gitStagedFiles(g.RootDir)
	if err != nil {
		return nil, fmt.Errorf("reading staged files: %w", err)
	}

	// 3. No-op check — "nothing to commit" is only terminal when push was not requested.
	// When push IS requested, skip commit creation but still converge and publish.
	nothingToCommit := len(files) == 0
	if nothingToCommit && !plan.Push.Enabled {
		return &Result{NoOp: true}, nil
	}

	result := &Result{Backend: "git", NoOp: nothingToCommit}

	if !nothingToCommit {
		// 4. Ensure git author identity exists (CI images often lack it)
		g.ensureAuthorIdentity()

		// 5. Build commit command
		subject := plan.Subject(conventional)
		commitArgs := []string{"commit", "-m", subject}
		if plan.Body != "" {
			commitArgs = append(commitArgs, "-m", plan.Body)
		}
		if plan.SignOff {
			commitArgs = append(commitArgs, "--signoff")
		}

		// Execute commit with streaming — hooks are observable live.
		handlers := StreamHandlers{}
		if g.OnCommitLine != nil {
			handlers.OnStdoutLine = func(line string) { g.OnCommitLine("stdout", line) }
			handlers.OnStderrLine = func(line string) { g.OnCommitLine("stderr", line) }
		}
		if _, err := g.gitStream(handlers, commitArgs...); err != nil {
			return nil, fmt.Errorf("committing: %w", err)
		}

		// 6. Capture SHA
		sha, err := g.gitOutput("rev-parse", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("reading commit SHA: %w", err)
		}
		result.SHA = sha
		result.Message = plan.Message(conventional)
		result.Files = files
	}

	// 7. Push via convergence engine — runs regardless of whether a new commit
	// was created. "Nothing to commit" does not mean "nothing to publish."
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

// Push synchronizes the current branch with its remote using the convergence
// engine: detect state → plan → execute. This is the shared implementation
// for both `commit --push` and `stagefreight push`.
func (g *GitBackend) Push(opts PushOptions) (*SyncResult, error) {
	state, err := g.DetectRepoState()
	if err != nil {
		return nil, fmt.Errorf("detecting repo state for push: %w", err)
	}
	syncPlan, err := PlanSync(state, opts.Remote, opts.Refspec, opts.RebaseOnDiverge)
	if err != nil {
		return nil, fmt.Errorf("planning push: %w", err)
	}
	result, err := g.Sync(syncPlan)
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	return result, nil
}

// BranchFromRefspec extracts the branch name from a refspec like "HEAD:refs/heads/main".
func BranchFromRefspec(refspec string) string {
	if idx := strings.LastIndex(refspec, "refs/heads/"); idx >= 0 {
		return refspec[idx+len("refs/heads/"):]
	}
	return ""
}

// ensureAuthorIdentity sets repo-local git user.name and user.email if not already configured.
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
// StageFreight's commit renderer uses these to build structured hook output.
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

// gitStream executes a git command with incremental line-by-line output
// delivery to caller callbacks. Captures full stdout/stderr separately.
// Never writes directly to the terminal — StageFreight owns presentation.
// Used only for git commit where hooks need to be observed live.
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

	// Read stdout incrementally
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
		// Preserve any trailing partial fragment
		if b := scanner.Bytes(); len(b) > 0 {
			stdoutBuf.Write(b)
			if h.OnStdoutLine != nil {
				h.OnStdoutLine(string(b))
			}
		}
	}()

	// Read stderr incrementally
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

	result := &StreamResult{
		Stdout: stdoutBuf.Bytes(),
		Stderr: stderrBuf.Bytes(),
	}

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
