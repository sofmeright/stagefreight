package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

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
// Invariants:
//   - Never mutates the user's working repo (temp bare clone only)
//   - Credentials are handled by go-git transport natively — no GIT_ASKPASS
//   - Operation is bounded by context cancellation
func MirrorPush(ctx context.Context, worktree string, mirror config.ResolvedRepo) (*MirrorResult, error) {
	start := time.Now()
	result := &MirrorResult{
		AccessoryID: mirror.ID,
	}

	absWorktree, err := filepath.Abs(worktree)
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to resolve worktree path: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}

	// Resolve origin URL from worktree via go-git — no git binary, no safe.directory needed.
	repo, err := gitstate.OpenRepo(absWorktree)
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to open worktree repo: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}
	originURL, err := gitstate.RemoteURL(repo, "origin")
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorUnknown
		result.Message = fmt.Sprintf("failed to resolve origin URL: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}

	// Resolve origin auth for the clone step.
	originAuth, err := gitstate.ResolveAuthForURL(originURL)
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorAuthFailed
		result.Message = fmt.Sprintf("failed to resolve origin auth: %v", err)
		result.Duration = time.Since(start)
		return result, nil
	}

	// 1. Bare clone from origin — all branches + all tags (mirror equivalent).
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

	bareRepo, err := git.PlainCloneContext(ctx, tmpDir, true, &git.CloneOptions{
		URL:        originURL,
		Auth:       originAuth,
		Tags:       git.AllTags,
		NoCheckout: true,
	})
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = classifyFailure(err)
		result.Message = fmt.Sprintf("failed to clone from origin: %v", sanitizeStderr(err))
		result.Duration = time.Since(start)
		return result, nil
	}

	// 2. Resolve mirror credentials.
	creds := credentials.ResolvePrefix(mirror.Credentials)
	if creds.Secret == "" {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = MirrorAuthFailed
		result.Message = fmt.Sprintf("no secret resolved for credentials prefix %q", mirror.Credentials)
		result.Duration = time.Since(start)
		return result, nil
	}

	mirrorGitAuth := resolveGitAuth(mirror.Provider, creds.Secret)
	mirrorAuth := &githttp.BasicAuth{
		Username: mirrorGitAuth.Username,
		Password: mirrorGitAuth.Password,
	}
	remoteURL := buildRemoteURL(mirror)

	// 3. Push all heads + tags with force + prune.
	// Split into two pushes (heads then tags) to avoid forge rejection of
	// --mirror behavior that attempts to delete/recreate the default branch.
	pushHeads := bareRepo.PushContext(ctx, &git.PushOptions{
		RemoteURL: remoteURL,
		RefSpecs:  []gitconfig.RefSpec{"+refs/heads/*:refs/heads/*"},
		Force:     true,
		Prune:     true,
		Auth:      mirrorAuth,
	})
	if pushHeads != nil && pushHeads != git.NoErrAlreadyUpToDate {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = classifyFailure(pushHeads)
		result.Message = sanitizeStderr(pushHeads)
		result.Duration = time.Since(start)
		return result, nil
	}

	pushTags := bareRepo.PushContext(ctx, &git.PushOptions{
		RemoteURL: remoteURL,
		RefSpecs:  []gitconfig.RefSpec{"+refs/tags/*:refs/tags/*"},
		Force:     true,
		Prune:     true,
		Auth:      mirrorAuth,
	})
	if pushTags != nil && pushTags != git.NoErrAlreadyUpToDate {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = classifyFailure(pushTags)
		result.Message = sanitizeStderr(pushTags)
		result.Duration = time.Since(start)
		return result, nil
	}

	result.Status = SyncSuccess
	result.Message = fmt.Sprintf("mirror push to %s succeeded", mirror.ID)
	result.Duration = time.Since(start)
	return result, nil
}

// classifyFailure performs best-effort classification of push failures.
// Inspects the error message string — never crashes, falls back to MirrorUnknown.
func classifyFailure(err error) MirrorFailureReason {
	if err == nil {
		return MirrorUnknown
	}
	stderr := strings.ToLower(sanitizeStderrString(err.Error()))

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

// sanitizeStderr returns a sanitized error string, redacting any embedded credentials.
func sanitizeStderr(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeStderrString(err.Error())
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
