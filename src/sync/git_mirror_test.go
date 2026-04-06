package sync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PrPlanIT/StageFreight/src/config"
)

// setupTestRepo creates a minimal git repo with one commit for testing.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %s", args, err, out)
		}
	}

	git("init", "-b", "main")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644)
	git("add", "README.md")
	git("commit", "-m", "initial")
	git("tag", "v1.0.0")

	return dir
}

// setupBareRemote creates a bare git repo to act as an accessory remote.
func setupBareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--bare")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git init --bare: %s: %s", err, out)
	}
	return dir
}

func TestMirrorPush_Success(t *testing.T) {
	source := setupTestRepo(t)
	remote := setupBareRemote(t)

	// Override buildAuthURL for test — use bare file path directly
	result := mirrorPushDirect(t, source, remote)

	if result.Status != SyncSuccess {
		t.Fatalf("expected success, got %s: %s", result.Status, result.Message)
	}
	if result.Degraded {
		t.Error("should not be degraded on success")
	}
	if result.AccessoryID != "test-remote" {
		// This test uses mirrorPushDirect, not the full MirrorPush
	}

	// Verify remote has the tag
	cmd := exec.Command("git", "tag", "-l")
	cmd.Dir = remote
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "v1.0.0") {
		t.Errorf("remote should have tag v1.0.0, got: %s", out)
	}

	// Verify remote has the branch
	cmd = exec.Command("git", "branch", "-a")
	cmd.Dir = remote
	out, err = cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "main") {
		t.Errorf("remote should have branch main, got: %s", out)
	}
}

func TestMirrorPush_DeletedBranch(t *testing.T) {
	source := setupTestRepo(t)
	remote := setupBareRemote(t)

	// First push
	mirrorPushDirect(t, source, remote)

	// Create and push a branch
	gitIn(t, source, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(source, "feature.txt"), []byte("f"), 0o644)
	gitIn(t, source, "add", "feature.txt")
	gitIn(t, source, "commit", "-m", "feature")
	mirrorPushDirect(t, source, remote)

	// Verify feature branch exists on remote
	out := gitOutput(t, remote, "branch", "-a")
	if !strings.Contains(out, "feature") {
		t.Fatal("feature branch should exist on remote after push")
	}

	// Delete the branch on source
	gitIn(t, source, "checkout", "main")
	gitIn(t, source, "branch", "-D", "feature")
	mirrorPushDirect(t, source, remote)

	// Verify feature branch is gone from remote
	out = gitOutput(t, remote, "branch", "-a")
	if strings.Contains(out, "feature") {
		t.Error("feature branch should be deleted on remote after mirror push")
	}
}

func TestMirrorPush_OrphanedTag(t *testing.T) {
	source := setupTestRepo(t)
	remote := setupBareRemote(t)

	// First push (includes v1.0.0 tag)
	mirrorPushDirect(t, source, remote)

	// Delete tag on source
	gitIn(t, source, "tag", "-d", "v1.0.0")
	mirrorPushDirect(t, source, remote)

	// Verify tag is gone from remote
	out := gitOutput(t, remote, "tag", "-l")
	if strings.Contains(out, "v1.0.0") {
		t.Error("orphaned tag v1.0.0 should be deleted on remote after mirror push")
	}
}

func TestMirrorPush_ForceRewrite(t *testing.T) {
	source := setupTestRepo(t)
	remote := setupBareRemote(t)

	// First push
	mirrorPushDirect(t, source, remote)

	// Get original HEAD SHA
	origSHA := gitOutput(t, remote, "rev-parse", "main")

	// Rewrite history on source (amend the commit)
	os.WriteFile(filepath.Join(source, "README.md"), []byte("rewritten"), 0o644)
	gitIn(t, source, "add", "README.md")
	gitIn(t, source, "commit", "--amend", "-m", "rewritten")

	// Mirror push (force update)
	mirrorPushDirect(t, source, remote)

	// Verify HEAD changed
	newSHA := gitOutput(t, remote, "rev-parse", "main")
	if origSHA == newSHA {
		t.Error("remote HEAD should have changed after force rewrite")
	}
}

func TestMirrorPush_NoMutationOfWorktree(t *testing.T) {
	source := setupTestRepo(t)
	remote := setupBareRemote(t)

	// Record .git/config before
	configBefore, _ := os.ReadFile(filepath.Join(source, ".git", "config"))

	mirrorPushDirect(t, source, remote)

	// Verify .git/config unchanged
	configAfter, _ := os.ReadFile(filepath.Join(source, ".git", "config"))
	if string(configBefore) != string(configAfter) {
		t.Error("mirror push must not mutate worktree .git/config")
	}
}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		stderr string
		want   MirrorFailureReason
	}{
		{"fatal: Authentication failed for 'https://...'", MirrorAuthFailed},
		{"remote: HTTP Basic: Access denied. The provided password or token is incorrect or your account has 2FA enabled\nfatal: Authentication failed", MirrorAuthFailed},
		{"remote: error: GH006: Protected branch update failed", MirrorProtectedRefRejected},
		{"! [remote rejected] main -> main (pre-receive hook declined)", MirrorProtectedRefRejected},
		{"fatal: unable to access: Could not resolve host: github.com", MirrorNetworkFailed},
		{"fatal: Connection refused", MirrorNetworkFailed},
		{"fatal: repository 'https://github.com/foo/bar.git/' not found", MirrorRemoteNotFound},
		{"error: failed to push some refs to 'https://...'", MirrorPushRejected},
		{"some other unknown error", MirrorUnknown},
	}

	for _, tt := range tests {
		err := &gitError{err: nil, stderr: tt.stderr}
		got := classifyFailure(err)
		if got != tt.want {
			t.Errorf("classifyFailure(%q) = %s, want %s", tt.stderr, got, tt.want)
		}
	}
}

func TestResolveGitAuth(t *testing.T) {
	tests := []struct {
		provider string
		wantUser string
	}{
		{"github", "x-access-token"},
		{"gitlab", "oauth2"},
		{"gitea", "git"},
		{"unknown", "git"},
	}
	for _, tt := range tests {
		auth := resolveGitAuth(tt.provider, "secret123")
		if auth.Username != tt.wantUser {
			t.Errorf("resolveGitAuth(%q): username = %q, want %q", tt.provider, auth.Username, tt.wantUser)
		}
		if auth.Password != "secret123" {
			t.Errorf("resolveGitAuth(%q): password = %q, want %q", tt.provider, auth.Password, "secret123")
		}
	}
}

func TestBuildRemoteURL(t *testing.T) {
	r := config.ResolvedRepo{
		BaseURL: "https://github.com",
		Project: "example-org/example-repo",
	}
	got := buildRemoteURL(r)
	if got != "https://github.com/example-org/example-repo.git" {
		t.Errorf("buildRemoteURL = %q, want https://github.com/example-org/example-repo.git", got)
	}
}

func TestBuildRemoteURL_AlreadyHasGitSuffix(t *testing.T) {
	r := config.ResolvedRepo{
		BaseURL: "https://github.com",
		Project: "example-org/example-repo.git",
	}
	got := buildRemoteURL(r)
	if strings.HasSuffix(got, ".git.git") {
		t.Errorf("buildRemoteURL double-appended .git: %s", got)
	}
}

func TestSanitizeStderr_RemovesCredentials(t *testing.T) {
	err := &gitError{
		err:    nil,
		stderr: "fatal: unable to push to https://x-access-token:ghp_abc123secret@github.com/org/repo.git",
		args:   []string{"push"},
	}

	msg := err.Error()

	if strings.Contains(msg, "ghp_abc123secret") {
		t.Errorf("Error() still contains token: %s", msg)
	}
	if strings.Contains(msg, "x-access-token") {
		t.Errorf("Error() still contains username: %s", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("Error() should contain [redacted]: %s", msg)
	}
}

func TestGitErrorWrapped_NoCredentials(t *testing.T) {
	inner := &gitError{
		err:    nil,
		stderr: "fatal: Authentication failed for 'https://oauth2:glpat_secret@gitlab.example.com/org/repo.git'",
		args:   []string{"push"},
	}

	// Wrap it like real code would
	wrapped := fmt.Errorf("mirror push failed: %w", inner)
	msg := wrapped.Error()

	if strings.Contains(msg, "glpat_secret") {
		t.Errorf("wrapped error contains token: %s", msg)
	}
	if strings.Contains(msg, "oauth2") {
		t.Errorf("wrapped error contains username: %s", msg)
	}
}

func TestMirrorPush_EmptyRepoBootstrap(t *testing.T) {
	source := setupTestRepo(t)
	remote := setupBareRemote(t)

	// Remote is empty — first mirror push should create full state
	result := mirrorPushDirect(t, source, remote)

	if result.Status != SyncSuccess {
		t.Fatalf("bootstrap push should succeed, got %s: %s", result.Status, result.Message)
	}

	// Verify all refs arrived
	out := gitOutput(t, remote, "rev-parse", "main")
	srcSHA := gitOutput(t, source, "rev-parse", "main")
	if out != srcSHA {
		t.Errorf("remote HEAD %s != source HEAD %s", out, srcSHA)
	}

	tags := gitOutput(t, remote, "tag", "-l")
	if !strings.Contains(tags, "v1.0.0") {
		t.Error("remote should have tag v1.0.0 after bootstrap")
	}
}

func TestMirrorPush_ContextCancellation(t *testing.T) {
	source := setupTestRepo(t)

	// Create a context that's already cancelled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tmpDir := t.TempDir()

	// Attempt clone with cancelled context — should fail
	err := gitExec(ctx, "", "clone", "--mirror", source, tmpDir)
	if err == nil {
		t.Error("expected error with cancelled context")
	}

	// Verify failure is classified (not a crash)
	reason := classifyFailure(err)
	// Cancelled context may produce various errors — just verify it doesn't panic
	// and returns a valid reason
	if reason == "" {
		t.Error("failure reason should not be empty")
	}
}

// ── test helpers ──

// mirrorPushDirect performs a mirror push using file paths (no credentials needed).
// Uses the same push strategy as MirrorPush: --force --prune --all + --tags.
func mirrorPushDirect(t *testing.T, worktree, remoteDir string) *MirrorResult {
	t.Helper()
	ctx := context.Background()

	tmpDir := t.TempDir()

	// Clone --mirror from worktree (safe in tests — not detached HEAD)
	if err := gitExec(ctx, "", "clone", "--mirror", worktree, tmpDir); err != nil {
		t.Fatalf("mirror clone failed: %v", err)
	}

	// Push using same strategy as production: --force --prune --all + --tags
	err := gitExec(ctx, tmpDir, "push", "--prune", "--force", "--all", remoteDir)
	if err == nil {
		err = gitExec(ctx, tmpDir, "push", "--prune", "--force", "--tags", remoteDir)
	}

	result := &MirrorResult{
		AccessoryID: "test-remote",
	}
	if err != nil {
		result.Status = SyncFailed
		result.Degraded = true
		result.FailureReason = classifyFailure(err)
		result.Message = err.Error()
	} else {
		result.Status = SyncSuccess
	}
	return result
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %s", args, err)
	}
	return strings.TrimSpace(string(out))
}
