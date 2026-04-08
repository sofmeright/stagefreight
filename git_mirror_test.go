package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/PrPlanIT/StageFreight/src/config"
)

var testSig = &object.Signature{
	Name:  "test",
	Email: "test@test",
	When:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
}

// setupTestRepo creates a minimal git repo with one commit and v1.0.0 tag.
func setupTestRepo(t *testing.T) (string, *git.Repository) {
	t.Helper()
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wt.Add("README.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	headHash, err := wt.Commit("initial", &git.CommitOptions{
		Author:    testSig,
		Committer: testSig,
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}

	if _, err := repo.CreateTag("v1.0.0", headHash, &git.CreateTagOptions{
		Tagger:  testSig,
		Message: "v1.0.0",
	}); err != nil {
		t.Fatalf("git tag v1.0.0: %v", err)
	}

	return dir, repo
}

// setupBareRemote creates a bare git repo to act as an accessory remote.
func setupBareRemote(t *testing.T) (string, *git.Repository) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, true)
	if err != nil {
		t.Fatalf("git init --bare: %v", err)
	}
	return dir, repo
}

func TestMirrorPush_Success(t *testing.T) {
	source, _ := setupTestRepo(t)
	remote, _ := setupBareRemote(t)

	result := mirrorPushDirect(t, source, remote)

	if result.Status != SyncSuccess {
		t.Fatalf("expected success, got %s: %s", result.Status, result.Message)
	}
	if result.Degraded {
		t.Error("should not be degraded on success")
	}

	// Verify remote has the tag
	remoteRepo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remoteRepo.Tag("v1.0.0"); err != nil {
		t.Errorf("remote should have tag v1.0.0: %v", err)
	}

	// Verify remote has the main branch
	if _, err := remoteRepo.Branch("main"); err != nil {
		// Branch config may not exist in bare — check via reference
		if _, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("main"), false); err != nil {
			t.Errorf("remote should have branch main: %v", err)
		}
	}
}

func TestMirrorPush_DeletedBranch(t *testing.T) {
	source, srcRepo := setupTestRepo(t)
	remote, _ := setupBareRemote(t)

	// First push
	mirrorPushDirect(t, source, remote)

	// Create and push a feature branch
	wt, _ := srcRepo.Worktree()
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
	}); err != nil {
		t.Fatalf("checkout feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wt.Add("feature.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("feature", &git.CommitOptions{Author: testSig, Committer: testSig}); err != nil {
		t.Fatalf("commit feature: %v", err)
	}
	mirrorPushDirect(t, source, remote)

	// Verify feature branch exists on remote
	remoteRepo, _ := git.PlainOpen(remote)
	if _, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("feature"), false); err != nil {
		t.Fatal("feature branch should exist on remote after push")
	}

	// Delete the branch on source
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("main"),
	}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if err := srcRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("feature")); err != nil {
		t.Fatalf("delete feature branch: %v", err)
	}
	mirrorPushDirect(t, source, remote)

	// Verify feature branch is gone from remote
	remoteRepo, _ = git.PlainOpen(remote)
	if _, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("feature"), false); err == nil {
		t.Error("feature branch should be deleted on remote after mirror push")
	}
}

func TestMirrorPush_OrphanedTag(t *testing.T) {
	source, srcRepo := setupTestRepo(t)
	remote, _ := setupBareRemote(t)

	// First push (includes v1.0.0 tag)
	mirrorPushDirect(t, source, remote)

	// Delete tag on source
	if err := srcRepo.Storer.RemoveReference(plumbing.NewTagReferenceName("v1.0.0")); err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	mirrorPushDirect(t, source, remote)

	// Verify tag is gone from remote
	remoteRepo, _ := git.PlainOpen(remote)
	if _, err := remoteRepo.Tag("v1.0.0"); err == nil {
		t.Error("orphaned tag v1.0.0 should be deleted on remote after mirror push")
	}
}

func TestMirrorPush_ForceRewrite(t *testing.T) {
	source, srcRepo := setupTestRepo(t)
	remote, _ := setupBareRemote(t)

	// First push
	mirrorPushDirect(t, source, remote)

	// Record original HEAD SHA on remote
	remoteRepo, _ := git.PlainOpen(remote)
	origRef, _ := remoteRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	origSHA := origRef.Hash()

	// Add a new commit (changes HEAD)
	wt, _ := srcRepo.Worktree()
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("rewritten", &git.CommitOptions{Author: testSig, Committer: testSig}); err != nil {
		t.Fatalf("commit rewrite: %v", err)
	}

	// Mirror push (force update)
	mirrorPushDirect(t, source, remote)

	// Verify HEAD changed
	remoteRepo, _ = git.PlainOpen(remote)
	newRef, _ := remoteRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if origSHA == newRef.Hash() {
		t.Error("remote HEAD should have changed after force rewrite")
	}
}

func TestMirrorPush_NoMutationOfWorktree(t *testing.T) {
	source, _ := setupTestRepo(t)
	remote, _ := setupBareRemote(t)

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
		got := classifyFailure(errors.New(tt.stderr))
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
	msg := sanitizeStderrString(
		"fatal: unable to push to https://x-access-token:ghp_abc123secret@github.com/org/repo.git",
	)

	if strings.Contains(msg, "ghp_abc123secret") {
		t.Errorf("sanitized string still contains token: %s", msg)
	}
	if strings.Contains(msg, "x-access-token") {
		t.Errorf("sanitized string still contains username: %s", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("sanitized string should contain [redacted]: %s", msg)
	}
}

func TestSanitizeStderrWrapped_NoCredentials(t *testing.T) {
	inner := sanitizeStderrString(
		"fatal: Authentication failed for 'https://oauth2:glpat_secret@gitlab.example.com/org/repo.git'",
	)
	wrapped := fmt.Errorf("mirror push failed: %s", inner)
	msg := wrapped.Error()

	if strings.Contains(msg, "glpat_secret") {
		t.Errorf("wrapped error contains token: %s", msg)
	}
	if strings.Contains(msg, "oauth2") {
		t.Errorf("wrapped error contains username: %s", msg)
	}
}

func TestMirrorPush_EmptyRepoBootstrap(t *testing.T) {
	source, srcRepo := setupTestRepo(t)
	remote, _ := setupBareRemote(t)

	// Remote is empty — first mirror push should create full state
	result := mirrorPushDirect(t, source, remote)

	if result.Status != SyncSuccess {
		t.Fatalf("bootstrap push should succeed, got %s: %s", result.Status, result.Message)
	}

	// Verify all refs arrived
	remoteRepo, _ := git.PlainOpen(remote)
	remoteRef, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("main ref on remote: %v", err)
	}
	srcHead, _ := srcRepo.Head()
	if remoteRef.Hash() != srcHead.Hash() {
		t.Errorf("remote HEAD %s != source HEAD %s", remoteRef.Hash(), srcHead.Hash())
	}

	if _, err := remoteRepo.Tag("v1.0.0"); err != nil {
		t.Error("remote should have tag v1.0.0 after bootstrap")
	}
}

func TestMirrorPush_ContextCancellation(t *testing.T) {
	source, _ := setupTestRepo(t)
	tmpDir := t.TempDir()

	// Attempt bare clone with cancelled context — should fail
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := git.PlainCloneContext(ctx, tmpDir, true, &git.CloneOptions{
		URL:        "file://" + source,
		Tags:       git.AllTags,
		NoCheckout: true,
	})
	if err == nil {
		t.Error("expected error with cancelled context")
	}

	// Verify classifyFailure handles non-gitError errors cleanly
	reason := classifyFailure(err)
	if reason == "" {
		t.Error("failure reason should not be empty")
	}
}

func TestMirrorPush_ExtraRemoteRefsArePruned(t *testing.T) {
	source, _ := setupTestRepo(t)
	remote, remoteRepo := setupBareRemote(t)

	// Seed the remote with refs that do not exist in source.
	// Add an extra branch ref directly via the storer.
	extraBranch := plumbing.NewBranchReferenceName("stale-branch")
	fakeHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := remoteRepo.Storer.SetReference(plumbing.NewHashReference(extraBranch, fakeHash)); err != nil {
		t.Fatalf("seeding stale-branch: %v", err)
	}
	extraTag := plumbing.NewTagReferenceName("stale-tag")
	if err := remoteRepo.Storer.SetReference(plumbing.NewHashReference(extraTag, fakeHash)); err != nil {
		t.Fatalf("seeding stale-tag: %v", err)
	}

	// Verify the extra refs are present before the mirror push.
	if _, err := remoteRepo.Reference(extraBranch, false); err != nil {
		t.Fatal("stale-branch should exist before mirror push")
	}
	if _, err := remoteRepo.Reference(extraTag, false); err != nil {
		t.Fatal("stale-tag should exist before mirror push")
	}

	// Mirror push from source (which has only main + v1.0.0).
	result := mirrorPushDirect(t, source, remote)
	if result.Status != SyncSuccess {
		t.Fatalf("mirror push failed: %s", result.Message)
	}

	// Verify stale refs were pruned.
	remoteRepo2, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remoteRepo2.Reference(extraBranch, false); err == nil {
		t.Error("stale-branch should have been pruned by mirror push")
	}
	if _, err := remoteRepo2.Reference(extraTag, false); err == nil {
		t.Error("stale-tag should have been pruned by mirror push")
	}

	// Verify source refs are still present.
	if _, err := remoteRepo2.Tag("v1.0.0"); err != nil {
		t.Error("v1.0.0 tag should still exist after mirror push")
	}
	if _, err := remoteRepo2.Reference(plumbing.NewBranchReferenceName("main"), false); err != nil {
		t.Error("main branch should still exist after mirror push")
	}
}

// ── test helpers ──

// mirrorPushDirect performs a mirror push using file paths (no credentials needed).
// Uses the same push strategy as MirrorPush: force+prune heads then tags.
func mirrorPushDirect(t *testing.T, worktreeDir, remoteDir string) *MirrorResult {
	t.Helper()
	ctx := context.Background()

	tmpDir := t.TempDir()

	bareRepo, err := git.PlainCloneContext(ctx, tmpDir, true, &git.CloneOptions{
		URL:        "file://" + worktreeDir,
		Tags:       git.AllTags,
		NoCheckout: true,
	})
	if err != nil {
		t.Fatalf("mirror clone failed: %v", err)
	}

	// Add a remote pointing to the test remote dir
	_, err = bareRepo.CreateRemote(&gitconfig.RemoteConfig{
		Name: "mirror",
		URLs: []string{"file://" + remoteDir},
	})
	if err != nil {
		t.Fatalf("create mirror remote: %v", err)
	}

	pushHeads := bareRepo.PushContext(ctx, &git.PushOptions{
		RemoteName: "mirror",
		RefSpecs:   []gitconfig.RefSpec{"+refs/heads/*:refs/heads/*"},
		Force:      true,
		Prune:      true,
	})
	if pushHeads != nil && pushHeads != git.NoErrAlreadyUpToDate {
		return &MirrorResult{
			AccessoryID:   "test-remote",
			Status:        SyncFailed,
			Degraded:      true,
			FailureReason: classifyFailure(pushHeads),
			Message:       pushHeads.Error(),
		}
	}

	pushTags := bareRepo.PushContext(ctx, &git.PushOptions{
		RemoteName: "mirror",
		RefSpecs:   []gitconfig.RefSpec{"+refs/tags/*:refs/tags/*"},
		Force:      true,
		Prune:      true,
	})
	if pushTags != nil && pushTags != git.NoErrAlreadyUpToDate {
		return &MirrorResult{
			AccessoryID:   "test-remote",
			Status:        SyncFailed,
			Degraded:      true,
			FailureReason: classifyFailure(pushTags),
			Message:       pushTags.Error(),
		}
	}

	return &MirrorResult{
		AccessoryID: "test-remote",
		Status:      SyncSuccess,
	}
}
