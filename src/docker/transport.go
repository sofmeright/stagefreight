package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LocalTransport executes stack actions directly on the local host.
// Compiles StackAction → local exec commands.
type LocalTransport struct{}

func (l *LocalTransport) ExecuteAction(ctx context.Context, action StackAction) (ExecResult, error) {
	start := time.Now()
	result := ExecResult{}

	// Execute hooks and compose action as ordered steps.
	// Pre hooks → compose action → post hooks.
	for _, hook := range action.Hooks {
		if hook.Phase != "pre" {
			continue
		}
		scriptPath := filepath.Join(action.BundleDir, hook.Path)
		er := l.execLocal(ctx, "bash", scriptPath)
		if !er.Success {
			er.Duration = time.Since(start)
			return er, fmt.Errorf("pre hook %s failed: exit %d", hook.Path, er.ExitCode)
		}
	}

	// Compose action — execute from BundleDir.
	args := composeArgs(action)
	er := l.execLocalInDir(ctx, action.BundleDir, "docker", args...)
	result.Stdout = er.Stdout
	result.Stderr = er.Stderr
	result.ExitCode = er.ExitCode
	result.Success = er.Success

	if !er.Success {
		result.Duration = time.Since(start)
		return result, fmt.Errorf("docker compose %s failed: exit %d", action.Action, er.ExitCode)
	}

	// Post hooks.
	for _, hook := range action.Hooks {
		if hook.Phase != "post" {
			continue
		}
		scriptPath := filepath.Join(action.BundleDir, hook.Path)
		er := l.execLocal(ctx, "bash", scriptPath)
		if !er.Success {
			result.Duration = time.Since(start)
			result.Success = false
			result.Stderr += "\n" + er.Stderr
			return result, fmt.Errorf("post hook %s failed: exit %d", hook.Path, er.ExitCode)
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

func (l *LocalTransport) execLocal(ctx context.Context, cmd string, args ...string) ExecResult {
	return l.execLocalInDir(ctx, "", cmd, args...)
}

func (l *LocalTransport) execLocalInDir(ctx context.Context, dir string, cmd string, args ...string) ExecResult {
	c := exec.CommandContext(ctx, cmd, args...)
	if dir != "" {
		c.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()

	r := ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			r.ExitCode = exitErr.ExitCode()
		} else {
			r.ExitCode = 1
		}
	} else {
		r.Success = true
	}
	return r
}

// InspectStack queries running containers for a compose project by label.
// Thin: lists container IDs, delegates parsing to inspect.go.
func (l *LocalTransport) InspectStack(ctx context.Context, project string) (StackInspection, error) {
	filter := fmt.Sprintf("label=com.docker.compose.project=%s", project)
	psResult := l.execLocal(ctx, "docker", "ps", "-aq", "--filter", filter)
	if !psResult.Success || strings.TrimSpace(psResult.Stdout) == "" {
		return StackInspection{Project: project}, nil
	}
	ids := strings.Fields(strings.TrimSpace(psResult.Stdout))
	return inspectLocalContainers(ctx, l, project, ids)
}

// ListProjects returns all compose project names on the local host.
func (l *LocalTransport) ListProjects(ctx context.Context) ([]string, error) {
	r := l.execLocal(ctx, "docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project",
		"--format", "{{index .Labels \"com.docker.compose.project\"}}")
	if !r.Success {
		return nil, fmt.Errorf("listing projects: %s", strings.TrimSpace(r.Stderr))
	}
	return dedupeLines(r.Stdout), nil
}

func (l *LocalTransport) Close() error { return nil }

// SSHTransport executes stack actions on a remote host via SSH.
// Copies bundle to remote tmpdir, executes, cleans up.
type SSHTransport struct {
	Host    string
	User    string
	KeyPath string
}

func (s *SSHTransport) ExecuteAction(ctx context.Context, action StackAction) (ExecResult, error) {
	start := time.Now()
	result := ExecResult{}

	// Create remote tmpdir for the bundle.
	remoteTmp, er := s.sshExec(ctx, "mktemp", "-d", "/tmp/sf-bundle-XXXXXX")
	if !er.Success {
		er.Duration = time.Since(start)
		return er, fmt.Errorf("creating remote tmpdir: %s", strings.TrimSpace(er.Stderr))
	}
	remoteDir := strings.TrimSpace(remoteTmp.Stdout)

	// Always clean up remote tmpdir.
	defer func() {
		s.sshExec(ctx, "rm", "-rf", remoteDir)
	}()

	// Copy bundle to remote.
	if err := s.scpDir(ctx, action.BundleDir, remoteDir); err != nil {
		result.Duration = time.Since(start)
		result.Stderr = err.Error()
		return result, err
	}

	// Build remote action with remoteDir as the bundle root.
	remoteAction := action
	remoteAction.BundleDir = remoteDir

	// Execute hooks and compose as ordered steps via SSH.
	// All commands run from the remote BundleDir for consistent working directory.

	// Pre hooks.
	for _, hook := range remoteAction.Hooks {
		if hook.Phase != "pre" {
			continue
		}
		er := s.sshExecInDir(ctx, remoteDir, "bash", hook.Path)
		if !er.Success {
			er.Duration = time.Since(start)
			return er, fmt.Errorf("pre hook %s failed: exit %d", hook.Path, er.ExitCode)
		}
	}

	// Compose action — executed from BundleDir.
	args := composeArgsRemote(remoteAction)
	er = s.sshExecInDir(ctx, remoteDir, "docker", args...)
	result.Stdout = er.Stdout
	result.Stderr = er.Stderr
	result.ExitCode = er.ExitCode
	result.Success = er.Success

	if !er.Success {
		result.Duration = time.Since(start)
		return result, fmt.Errorf("docker compose %s failed: exit %d", action.Action, er.ExitCode)
	}

	// Post hooks.
	for _, hook := range remoteAction.Hooks {
		if hook.Phase != "post" {
			continue
		}
		er := s.sshExecInDir(ctx, remoteDir, "bash", hook.Path)
		if !er.Success {
			result.Duration = time.Since(start)
			result.Success = false
			result.Stderr += "\n" + er.Stderr
			return result, fmt.Errorf("post hook %s failed: exit %d", hook.Path, er.ExitCode)
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// InspectStack queries running containers for a compose project on the remote host.
// Thin: lists container IDs via SSH, delegates parsing to inspect.go.
func (s *SSHTransport) InspectStack(ctx context.Context, project string) (StackInspection, error) {
	filter := fmt.Sprintf("label=com.docker.compose.project=%s", project)
	psResult := s.sshExecResult(ctx, "docker", "ps", "-aq", "--filter", filter)
	if !psResult.Success || strings.TrimSpace(psResult.Stdout) == "" {
		return StackInspection{Project: project}, nil
	}
	ids := strings.Fields(strings.TrimSpace(psResult.Stdout))
	return inspectRemoteContainers(ctx, s, project, ids)
}

// ListProjects returns all compose project names on the remote host.
func (s *SSHTransport) ListProjects(ctx context.Context) ([]string, error) {
	r := s.sshExecResult(ctx, "docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project",
		"--format", "{{index .Labels \"com.docker.compose.project\"}}")
	if !r.Success {
		return nil, fmt.Errorf("listing projects: %s", strings.TrimSpace(r.Stderr))
	}
	return dedupeLines(r.Stdout), nil
}

func (s *SSHTransport) sshExec(ctx context.Context, cmd string, args ...string) (ExecResult, error) {
	r := s.sshExecResult(ctx, cmd, args...)
	if !r.Success {
		return r, fmt.Errorf("ssh exec failed: exit %d", r.ExitCode)
	}
	return r, nil
}

// sshExecInDir executes a command on the remote host from a specific working directory.
// Uses `cd <dir> && cmd args...` to enforce execution context.
func (s *SSHTransport) sshExecInDir(ctx context.Context, dir string, cmd string, args ...string) ExecResult {
	// Build the full remote command: cd <dir> && cmd arg1 arg2 ...
	parts := []string{"cd", dir, "&&", cmd}
	parts = append(parts, args...)
	fullCmd := strings.Join(parts, " ")

	sshArgs := s.baseArgs()
	sshArgs = append(sshArgs, "--", "bash", "-c", fullCmd)

	c := exec.CommandContext(ctx, "ssh", sshArgs...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()

	r := ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			r.ExitCode = exitErr.ExitCode()
		} else {
			r.ExitCode = 1
		}
	} else {
		r.Success = true
	}
	return r
}

func (s *SSHTransport) sshExecResult(ctx context.Context, cmd string, args ...string) ExecResult {
	sshArgs := s.baseArgs()
	sshArgs = append(sshArgs, "--")
	sshArgs = append(sshArgs, cmd)
	sshArgs = append(sshArgs, args...)

	c := exec.CommandContext(ctx, "ssh", sshArgs...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()

	r := ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			r.ExitCode = exitErr.ExitCode()
		} else {
			r.ExitCode = 1
		}
	} else {
		r.Success = true
	}
	return r
}

func (s *SSHTransport) scpDir(ctx context.Context, localDir, remoteDir string) error {
	scpArgs := []string{"-r"}
	if s.KeyPath != "" {
		scpArgs = append(scpArgs, "-i", s.KeyPath)
	}
	scpArgs = append(scpArgs, "-o", "StrictHostKeyChecking=accept-new", "-o", "BatchMode=yes")

	target := s.Host + ":" + remoteDir + "/"
	if s.User != "" {
		target = s.User + "@" + target
	}

	// Copy contents of localDir into remoteDir.
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return fmt.Errorf("reading bundle dir: %w", err)
	}
	for _, e := range entries {
		src := filepath.Join(localDir, e.Name())
		args := append(scpArgs, src, target)
		cmd := exec.CommandContext(ctx, "scp", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("scp %s: %s", e.Name(), strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (s *SSHTransport) Close() error { return nil }

func (s *SSHTransport) baseArgs() []string {
	args := []string{}
	if s.KeyPath != "" {
		args = append(args, "-i", s.KeyPath)
	}
	args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	args = append(args, "-o", "BatchMode=yes")
	args = append(args, "-o", "ConnectTimeout=10")
	if s.User != "" {
		args = append(args, s.User+"@"+s.Host)
	} else {
		args = append(args, s.Host)
	}
	return args
}

// composeArgs builds docker compose arguments from a StackAction.
// Paths are relative to BundleDir (local execution).
func composeArgs(action StackAction) []string {
	composePath := filepath.Join(action.BundleDir, action.ComposeFile)
	args := []string{"compose", "-f", composePath}
	for _, ef := range action.EnvFiles {
		args = append(args, "--env-file", filepath.Join(action.BundleDir, ef))
	}
	args = append(args, "-p", action.ProjectName)

	switch action.Action {
	case "up":
		args = append(args, "up", "-d")
	case "down":
		args = append(args, "down")
	case "prune":
		args = append(args, "down", "--volumes", "--remove-orphans")
	case "restart":
		args = append(args, "restart")
	}
	return args
}

// composeArgsRemote builds docker compose arguments for remote execution.
// Same logic but paths relative to remote BundleDir.
func composeArgsRemote(action StackAction) []string {
	// Same as local — BundleDir is already the remote path.
	return composeArgs(action)
}

// ResolveTransport creates the appropriate transport for a host target.
func ResolveTransport(target HostTarget) HostTransport {
	if target.Vars["docker_local"] == "true" {
		return &LocalTransport{}
	}

	user := target.Vars["ansible_user"]
	keyPath := target.Vars["ansible_ssh_private_key_file"]
	addr := target.Address
	if addr == "" {
		addr = target.Name
	}

	return &SSHTransport{
		Host:    addr,
		User:    user,
		KeyPath: keyPath,
	}
}

// dedupeLines splits output by newlines, deduplicates, ignores empties.
func dedupeLines(output string) []string {
	seen := map[string]bool{}
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			result = append(result, line)
		}
	}
	sort.Strings(result)
	return result
}
