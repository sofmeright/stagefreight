package sync

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/credentials"
)

// MirrorPush performs an authoritative git mirror push from the local worktree
// to an accessory forge. It creates a temporary bare mirror clone of the local
// repo and pushes --mirror to the accessory remote.
//
// Invariants:
//   - Never mutates the user's working repo (temp bare clone only)
//   - Credentials are never logged, persisted, or surfaced in errors
//   - Process is killed on context cancellation
func MirrorPush(ctx context.Context, worktree string, accessory config.MirrorConfig) (*MirrorResult, error) {
	start := time.Now()
	result := &MirrorResult{
		AccessoryID: accessory.ID,
	}

	// Resolve worktree to absolute path — clone requires a valid path
	// regardless of process cwd.
	absWorktree, err := filepath.Abs(worktree)
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to resolve worktree path: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}

	// Ensure git trusts the worktree — container user may differ from repo owner.
	// This is required for the same reason stagefreight's CI skeleton sets safe.directory.
	_ = gitExec(ctx, absWorktree, "config", "--global", "--add", "safe.directory", absWorktree)

	// 1. Create temporary bare mirror clone from local worktree
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

	// Use --bare (not --mirror) to clone only local refs (heads + tags).
	// --mirror would copy ALL refs including refs/remotes/origin/* and
	// GitLab CI refs like refs/pipelines/*, which must never be pushed
	// to the accessory forge.
	if err := gitExec(ctx, absWorktree, "clone", "--bare", absWorktree, tmpDir); err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to create mirror clone: %v", sanitizeError(err))
		result.Duration = time.Since(start)
		return result, nil
	}

	// CI runners often checkout as detached HEAD. The bare clone may miss
	// branch refs that exist in the source repo's packed-refs. Explicitly
	// fetch all heads and tags from the source to guarantee a complete refset.
	_ = gitExec(ctx, tmpDir, "fetch", absWorktree, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")

	// 2. Build authenticated remote URL (internal only — never surfaced)
	remoteURL, err := buildAuthURL(accessory)
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorAuthFailed
		result.Message = fmt.Sprintf("failed to build auth URL for %s: %v", accessory.ID, err)
		result.Duration = time.Since(start)
		return result, nil
	}

	// 3. Replicate heads and tags with force + prune.
	// We do NOT use --mirror because it attempts to delete and recreate
	// the default branch, which GitHub (and other forges) refuse.
	// Instead: --force --prune --all (branches) + --force --prune --tags.
	// This gives identical mirror semantics without violating forge constraints.
	pushErr := gitExec(ctx, tmpDir, "push", "--prune", "--force", "--all", remoteURL)
	if pushErr == nil {
		pushErr = gitExec(ctx, tmpDir, "push", "--prune", "--force", "--tags", remoteURL)
	}

	result.Duration = time.Since(start)

	if pushErr != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = classifyFailure(pushErr)
		result.Message = sanitizeMessage(pushErr, remoteURL)
		return result, nil
	}

	result.Status = SyncSuccess
	result.Message = fmt.Sprintf("mirror push to %s succeeded", accessory.ID)
	return result, nil
}

// buildAuthURL constructs an HTTPS URL with embedded token for git push.
// The URL is internal only — it must never be logged or surfaced.
func buildAuthURL(accessory config.MirrorConfig) (string, error) {
	creds := credentials.ResolvePrefix(accessory.Credentials)

	token := creds.Secret
	if token == "" {
		return "", fmt.Errorf("no secret resolved for credentials prefix %q (checked %s)", accessory.Credentials, accessory.Credentials+"_TOKEN/_PASS/_PASSWORD")
	}

	// Parse the forge URL and construct the authenticated repo URL
	baseURL := strings.TrimRight(accessory.URL, "/")
	projectPath := strings.TrimLeft(accessory.ProjectID, "/")

	// Construct: https://<token>@github.com/owner/repo.git
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parsing forge URL %q: %w", baseURL, err)
	}

	parsed.User = url.User(token)
	parsed.Path = "/" + projectPath
	if !strings.HasSuffix(parsed.Path, ".git") {
		parsed.Path += ".git"
	}

	return parsed.String(), nil
}

// gitExec runs a git command with context cancellation support.
// The working directory is set to dir. Stderr is captured for failure classification.
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
type gitError struct {
	err    error
	stderr string
	args   []string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %s", strings.Join(e.args[:1], " "), e.stderr)
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

// sanitizeMessage removes credentials from error messages.
// All auth URLs are replaced with the accessory ID.
func sanitizeMessage(err error, authURL string) string {
	msg := err.Error()
	if authURL != "" {
		msg = strings.ReplaceAll(msg, authURL, "[redacted]")
	}
	// Also redact any token-like patterns in case of partial URL leakage
	if idx := strings.Index(msg, "@"); idx > 0 {
		// Find the scheme prefix
		for _, scheme := range []string{"https://", "http://"} {
			if schemeIdx := strings.Index(msg, scheme); schemeIdx >= 0 && schemeIdx < idx {
				msg = msg[:schemeIdx+len(scheme)] + "[redacted]" + msg[idx:]
				break
			}
		}
	}
	return msg
}

// sanitizeError strips credential-bearing content from errors that may contain URLs.
func sanitizeError(err error) string {
	return sanitizeMessage(err, "")
}
