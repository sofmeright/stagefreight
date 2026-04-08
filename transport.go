package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sfxssh "github.com/PrPlanIT/StageFreight/src/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
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
// Uses golang.org/x/crypto/ssh for all SSH operations — no ssh or scp binary required.
type SSHTransport struct {
	Host    string
	User    string
	KeyPath string

	mu     sync.Mutex
	client *gossh.Client
}

// dial creates a new SSH client connection.
// Auth resolution and host key verification are fully delegated to gitstate —
// this function owns only connection establishment and ClientConfig assembly.
func (s *SSHTransport) dial() (*gossh.Client, error) {
	authMethods, err := sfxssh.ResolveAuthMethods(s.KeyPath)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := sfxssh.ResolveHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("resolving known_hosts: %w", err)
	}

	user := s.User
	if user == "" {
		user = "root"
	}

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	host := s.Host
	if !strings.Contains(host, ":") {
		host += ":22"
	}

	return gossh.Dial("tcp", host, cfg)
}

// getClient returns the lazily-initialized SSH client, connecting on first call.
// If the connection has dropped, it is closed and a fresh connection is established.
// Thread-safe: multiple goroutines may call getClient concurrently.
func (s *SSHTransport) getClient() (*gossh.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		// Probe the connection with a keepalive. If it fails the connection is
		// dead and must be replaced — a stale non-nil client is worse than nil.
		if _, _, err := s.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			_ = s.client.Close()
			s.client = nil
		}
	}

	if s.client == nil {
		c, err := s.dial()
		if err != nil {
			return nil, err
		}
		s.client = c
	}
	return s.client, nil
}

func (s *SSHTransport) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		err := s.client.Close()
		s.client = nil
		return err
	}
	return nil
}

func (s *SSHTransport) ExecuteAction(ctx context.Context, action StackAction) (ExecResult, error) {
	start := time.Now()
	result := ExecResult{}

	// Create remote tmpdir.
	mkdirResult, err := s.sshExec(ctx, "mktemp", "-d", "/tmp/sf-bundle-XXXXXX")
	if err != nil {
		mkdirResult.Duration = time.Since(start)
		return mkdirResult, fmt.Errorf("creating remote tmpdir: %w", err)
	}
	if !mkdirResult.Success {
		mkdirResult.Duration = time.Since(start)
		return mkdirResult, fmt.Errorf("creating remote tmpdir failed: %s", strings.TrimSpace(mkdirResult.Stderr))
	}
	remoteDir := strings.TrimSpace(mkdirResult.Stdout)

	// Always clean up remote tmpdir.
	defer func() {
		s.sshExec(ctx, "rm", "-rf", remoteDir) //nolint:errcheck
	}()

	// Upload bundle to remote.
	if err := s.scpDir(ctx, action.BundleDir, remoteDir); err != nil {
		result.Duration = time.Since(start)
		result.Stderr = err.Error()
		return result, err
	}

	// Build remote action with remoteDir as the bundle root.
	remoteAction := action
	remoteAction.BundleDir = remoteDir

	// Pre hooks.
	for _, hook := range remoteAction.Hooks {
		if hook.Phase != "pre" {
			continue
		}
		hookResult := s.sshExecInDir(ctx, remoteDir, "bash", hook.Path)
		if !hookResult.Success {
			hookResult.Duration = time.Since(start)
			return hookResult, fmt.Errorf("pre hook %s failed: exit %d", hook.Path, hookResult.ExitCode)
		}
	}

	// Compose action — executed from BundleDir.
	args := composeArgsRemote(remoteAction)
	composeResult := s.sshExecInDir(ctx, remoteDir, "docker", args...)
	result.Stdout = composeResult.Stdout
	result.Stderr = composeResult.Stderr
	result.ExitCode = composeResult.ExitCode
	result.Success = composeResult.Success

	if !composeResult.Success {
		result.Duration = time.Since(start)
		return result, fmt.Errorf("docker compose %s failed: exit %d", action.Action, composeResult.ExitCode)
	}

	// Post hooks.
	for _, hook := range remoteAction.Hooks {
		if hook.Phase != "post" {
			continue
		}
		hookResult := s.sshExecInDir(ctx, remoteDir, "bash", hook.Path)
		if !hookResult.Success {
			result.Duration = time.Since(start)
			result.Success = false
			result.Stderr += "\n" + hookResult.Stderr
			return hookResult, fmt.Errorf("post hook %s failed: exit %d", hook.Path, hookResult.ExitCode)
		}
	}

	result.Duration = time.Since(start)
	return result, nil
}

// InspectStack queries running containers for a compose project on the remote host.
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

// runSession executes a remote command string via a new SSH session and returns the result.
// Context cancellation sends SIGTERM to the remote process.
func (s *SSHTransport) runSession(client *gossh.Client, ctx context.Context, remoteCmd string) ExecResult {
	session, err := client.NewSession()
	if err != nil {
		return ExecResult{ExitCode: 1, Stderr: fmt.Sprintf("opening SSH session: %v", err)}
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(remoteCmd) }()

	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		session.Signal(gossh.SIGTERM) //nolint:errcheck — best-effort signal on cancellation
		runErr = ctx.Err()
	}

	r := ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if runErr == nil {
		r.Success = true
	} else if exitErr, ok := runErr.(*gossh.ExitError); ok {
		r.ExitCode = exitErr.ExitStatus()
	} else {
		r.ExitCode = 1
		if r.Stderr == "" {
			r.Stderr = runErr.Error()
		}
	}
	return r
}

// sshExecResult executes a command on the remote host and returns the result.
// Arguments are individually shell-escaped to preserve argument boundaries.
func (s *SSHTransport) sshExecResult(ctx context.Context, cmd string, args ...string) ExecResult {
	client, err := s.getClient()
	if err != nil {
		return ExecResult{ExitCode: 1, Stderr: fmt.Sprintf("SSH connect to %s: %v", s.Host, err)}
	}
	return s.runSession(client, ctx, joinForShell(cmd, args...))
}

// sshExec executes a command and returns an error when the command fails.
func (s *SSHTransport) sshExec(ctx context.Context, cmd string, args ...string) (ExecResult, error) {
	r := s.sshExecResult(ctx, cmd, args...)
	if !r.Success {
		return r, fmt.Errorf("ssh exec failed: exit %d", r.ExitCode)
	}
	return r, nil
}

// sshExecInDir executes a command from a specific remote working directory.
// Equivalent to: bash -c 'cd <dir> && <cmd> <args...>'
func (s *SSHTransport) sshExecInDir(ctx context.Context, dir string, cmd string, args ...string) ExecResult {
	client, err := s.getClient()
	if err != nil {
		return ExecResult{ExitCode: 1, Stderr: fmt.Sprintf("SSH connect to %s: %v", s.Host, err)}
	}
	inner := "cd " + shellescape(dir) + " && " + joinForShell(cmd, args...)
	return s.runSession(client, ctx, "bash -c "+shellescape(inner))
}

// scpDir uploads a local directory tree to a remote path using SFTP.
// No scp binary required — uses the already-established SSH connection.
func (s *SSHTransport) scpDir(_ context.Context, localDir, remoteDir string) error {
	client, err := s.getClient()
	if err != nil {
		return fmt.Errorf("SSH connect to %s: %w", s.Host, err)
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("opening sftp: %w", err)
	}
	defer sftpClient.Close()

	return uploadDirRecursive(sftpClient, localDir, remoteDir)
}

func uploadDirRecursive(sftpClient *sftp.Client, localDir, remoteDir string) error {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", localDir, err)
	}
	for _, e := range entries {
		src := filepath.Join(localDir, e.Name())
		dst := path.Join(remoteDir, e.Name())
		if e.IsDir() {
			if err := sftpClient.MkdirAll(dst); err != nil {
				return fmt.Errorf("mkdir %s: %w", dst, err)
			}
			if err := uploadDirRecursive(sftpClient, src, dst); err != nil {
				return err
			}
		} else {
			if err := sftpUploadFile(sftpClient, src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

func sftpUploadFile(sftpClient *sftp.Client, localPath, remotePath string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", localPath, err)
	}
	defer src.Close()

	dst, err := sftpClient.Create(remotePath)
	if err != nil {
		return fmt.Errorf("creating remote %s: %w", remotePath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("uploading %s: %w", localPath, err)
	}
	return nil
}

// joinForShell builds a shell command string with each argument individually
// single-quote escaped, so the remote shell preserves argument boundaries.
func joinForShell(cmd string, args ...string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, shellescape(cmd))
	for _, a := range args {
		parts = append(parts, shellescape(a))
	}
	return strings.Join(parts, " ")
}

// shellescape wraps s in single quotes, escaping any embedded single quotes.
func shellescape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// composeArgs builds docker compose arguments from a StackAction.
// For normal stacks: paths relative to BundleDir.
// For orphan teardown (no ComposeFile): project-name-only, directory-independent.
func composeArgs(action StackAction) []string {
	args := []string{"compose"}

	if action.ComposeFile != "" {
		// Normal stack: compose file + env files from bundle.
		composePath := filepath.Join(action.BundleDir, action.ComposeFile)
		args = append(args, "-f", composePath)
		for _, ef := range action.EnvFiles {
			args = append(args, "--env-file", filepath.Join(action.BundleDir, ef))
		}
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
