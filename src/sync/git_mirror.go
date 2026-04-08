package sync

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/credentials"
	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// gitAuth holds provider-adapted credentials for git transport.
// Username semantics vary by forge; this is resolved once in resolveGitAuth.
type gitAuth struct {
	Username string
	Password string
}

// resolveGitAuth maps a provider and secret to the correct git transport
// username/password pair. This is the ONLY place provider-specific username
// rules live — do not duplicate elsewhere.
func resolveGitAuth(provider, secret string) gitAuth {
	switch provider {
	case "github":
		return gitAuth{Username: "x-access-token", Password: secret}
	case "gitlab":
		return gitAuth{Username: "oauth2", Password: secret}
	case "gitea":
		return gitAuth{Username: "git", Password: secret}
	default:
		return gitAuth{Username: "git", Password: secret}
	}
}

// buildRemoteURL constructs a plain HTTPS URL for the mirror remote.
// No credentials are embedded — auth is injected via GIT_ASKPASS.
func buildRemoteURL(repo config.ResolvedRepo) string {
	baseURL := strings.TrimRight(repo.BaseURL, "/")
	projectPath := strings.TrimLeft(repo.Project, "/")
	u := baseURL + "/" + projectPath
	if !strings.HasSuffix(u, ".git") {
		u += ".git"
	}
	return u
}

// MirrorPush performs an authoritative git mirror push from the primary
// forge (origin) to a mirror forge. It clones from origin into a temp bare
// repo and pushes all heads + tags with force and prune.
//
// NOTE: This function calls the git binary for clone and push operations.
// It is the SOLE remaining git CLI dependency in StageFreight. All other
// git operations use go-git. This function requires a git binary in the
// runtime environment or will fail gracefully with Degraded status.
// Replacement with a native go-git mirror implementation is tracked in the
// forge-sync project (see project_stagefreight_forge_sync.md).
//
// Invariants:
//   - Never mutates the user's working repo (temp bare clone only)
//   - Credentials are injected via GIT_ASKPASS self-reexec, never in URLs or argv
//   - Process is killed on context cancellation
func MirrorPush(ctx context.Context, worktree string, mirror config.ResolvedRepo) (*MirrorResult, error) {
	start := time.Now()
	result := &MirrorResult{
		AccessoryID: mirror.ID,
	}

	// Resolve worktree to absolute path for safe.directory and origin detection.
	absWorktree, err := filepath.Abs(worktree)
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to resolve worktree path: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}

	// Resolve the origin remote URL from the worktree. We mirror from origin
	// (the authoritative distribution surface), not from the worktree filesystem.
	originURL, err := resolveOriginURL(ctx, absWorktree)
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to resolve origin URL: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}

	// 1. Clone --mirror from origin to get a complete bare repo with all refs.
	tmpDir, err := os.MkdirTemp("", "sf-mirror-*")
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to create temp directory: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}
	defer os.RemoveAll(tmpDir)

	if err := gitExec(ctx, absWorktree, "clone", "--mirror", originURL, tmpDir); err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = classifyFailure(err)
		result.Message = fmt.Sprintf("failed to clone from origin: %v", sanitizeStderr(err))
		result.Duration = time.Since(start)
		return result, nil
	}

	// 2. Resolve credentials and build auth + remote URL.
	creds := credentials.ResolvePrefix(mirror.Credentials)
	if creds.Secret == "" {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorAuthFailed
		result.Message = fmt.Sprintf("no secret resolved for credentials prefix %q", mirror.Credentials)
		result.Duration = time.Since(start)
		return result, nil
	}

	auth := resolveGitAuth(mirror.Provider, creds.Secret)
	remoteURL := buildRemoteURL(mirror)

	// 3. Replicate heads and tags with force + prune.
	// We do NOT use --mirror because it attempts to delete and recreate
	// the default branch, which GitHub (and other forges) refuse.
	pushErr := gitExecWithAuth(ctx, tmpDir, auth, "push", "--prune", "--force", "--all", remoteURL)
	if pushErr == nil {
		pushErr = gitExecWithAuth(ctx, tmpDir, auth, "push", "--prune", "--force", "--tags", remoteURL)
	}

	result.Duration = time.Since(start)

	if pushErr != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = classifyFailure(pushErr)
		result.Message = sanitizeStderr(pushErr)
		return result, nil
	}

	result.Status = SyncSuccess
	result.Message = fmt.Sprintf("mirror push to %s succeeded", mirror.ID)
	return result, nil
}

// resolveOriginURL reads the origin remote URL from the worktree's git config.
func resolveOriginURL(_ context.Context, worktree string) (string, error) {
	repo, err := gitstate.OpenRepo(worktree)
	if err != nil {
		return "", fmt.Errorf("opening repo: %w", err)
	}
	u, err := gitstate.RemoteURL(repo, "origin")
	if err != nil {
		return "", fmt.Errorf("failed to resolve origin URL: %w", err)
	}
	if strings.TrimSpace(u) == "" {
		return "", fmt.Errorf("origin remote URL is empty")
	}
	return u, nil
}

// gitExecWithAuth runs a git command with credentials injected via GIT_ASKPASS.
// StageFreight re-executes itself in askpass mode — no temp scripts, no URL auth.
// Credentials are passed via STAGEFREIGHT_GIT_USERNAME/PASSWORD env vars that
// only the askpass handler reads.
func gitExecWithAuth(ctx context.Context, dir string, auth gitAuth, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating executable for askpass: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_ASKPASS="+exe,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"STAGEFREIGHT_ASKPASS=1",
		"STAGEFREIGHT_GIT_USERNAME="+auth.Username,
		"STAGEFREIGHT_GIT_PASSWORD="+auth.Password,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return &gitError{
			err:    err,
			stderr: stderr.String(),
			args:   args,
		}
	}
	return nil
}

// gitExec runs a git command with context cancellation support (no auth).
func gitExec(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return &gitError{
			err:    err,
			stderr: stderr.String(),
			args:   args,
		}
	}
	return nil
}

// gitError wraps a git command failure with captured stderr for classification.
// Error() never surfaces raw stderr — it is sanitized to remove any potential
// credential material that git might echo.
type gitError struct {
	err    error
	stderr string
	args   []string
}

func (e *gitError) Error() string {
	// Sanitize stderr: remove anything that looks like a credential in a URL.
	safe := sanitizeStderrString(e.stderr)
	return fmt.Sprintf("git %s: %s", e.args[0], safe)
}

func (e *gitError) Unwrap() error {
	return e.err
}

// classifyFailure performs best-effort classification of git push failures
// via stderr substring matching. Never crashes — falls back to MirrorUnknown.
func classifyFailure(err error) MirrorFailureReason {
	ge, ok := err.(*gitError)
	if !ok {
		return MirrorUnknown
	}

	stderr := strings.ToLower(ge.stderr)

	switch {
	case strings.Contains(stderr, "authentication failed") ||
		strings.Contains(stderr, "invalid credentials") ||
		strings.Contains(stderr, "could not read password") ||
		strings.Contains(stderr, "401") ||
		strings.Contains(stderr, "403"):
		return MirrorAuthFailed

	case strings.Contains(stderr, "protected branch") ||
		strings.Contains(stderr, "pre-receive hook declined") ||
		strings.Contains(stderr, "deny updating a hidden ref"):
		return MirrorProtectedRefRejected

	case strings.Contains(stderr, "could not resolve host") ||
		strings.Contains(stderr, "connection refused") ||
		strings.Contains(stderr, "connection timed out") ||
		strings.Contains(stderr, "network is unreachable"):
		return MirrorNetworkFailed

	case (strings.Contains(stderr, "repository") && strings.Contains(stderr, "not found")) ||
		strings.Contains(stderr, "does not exist") ||
		strings.Contains(stderr, "404"):
		return MirrorRemoteNotFound

	case strings.Contains(stderr, "rejected") ||
		strings.Contains(stderr, "failed to push"):
		return MirrorPushRejected

	default:
		return MirrorUnknown
	}
}

// sanitizeStderr extracts and sanitizes the stderr message from an error.
func sanitizeStderr(err error) string {
	ge, ok := err.(*gitError)
	if !ok {
		return err.Error()
	}
	return sanitizeStderrString(ge.stderr)
}

// sanitizeStderrString removes credential-bearing content from git stderr output.
// Redacts tokens embedded in URLs (https://token@host pattern).
func sanitizeStderrString(s string) string {
	// Redact any https://user:pass@host or https://token@host patterns
	if idx := strings.Index(s, "@"); idx > 0 {
		for _, scheme := range []string{"https://", "http://"} {
			if schemeIdx := strings.Index(s, scheme); schemeIdx >= 0 && schemeIdx < idx {
				s = s[:schemeIdx+len(scheme)] + "[redacted]" + s[idx:]
				break
			}
		}
	}
	return s
}
