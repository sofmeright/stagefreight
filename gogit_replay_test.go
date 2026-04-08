package commit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ─── test infrastructure ─────────────────────────────────────────────────────

// replayTestRepo creates a pair of repos: a local working repo and a bare remote,
// wired together with a configured tracking branch.
type replayTestRepo struct {
	localDir  string
	remoteDir string
	repo      *git.Repository
}

func newReplayTestRepo(t *testing.T) *replayTestRepo {
	t.Helper()
	localDir := t.TempDir()
	remoteDir := t.TempDir()

	// Init bare remote
	_, err := git.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatalf("init bare remote: %v", err)
	}

	// Init local repo
	repo, err := git.PlainInit(localDir, false)
	if err != nil {
		t.Fatalf("init local: %v", err)
	}

	// Create initial commit on local
	wt, _ := repo.Worktree()
	writeTestFile(t, localDir, "README.md", "initial")
	wt.Add("README.md")
	testCommit(t, wt, "chore: initial commit\n\nX-StageFreight-Generated: true")

	// Add remote and set upstream tracking
	_, err = repo.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{remoteDir},
	})
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}

	// Push initial commit to remote
	err = repo.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{"refs/heads/main:refs/heads/main"},
	})
	if err != nil {
		t.Fatalf("initial push: %v", err)
	}

	// Configure upstream tracking
	cfg, _ := repo.Config()
	cfg.Branches["main"] = &gitconfig.Branch{
		Name:   "main",
		Remote: "origin",
		Merge:  plumbing.ReferenceName("refs/heads/main"),
	}
	repo.SetConfig(cfg)

	// Fetch to populate refs/remotes/origin/main
	repo.Fetch(&git.FetchOptions{RemoteName: "origin"})

	return &replayTestRepo{
		localDir:  localDir,
		remoteDir: remoteDir,
		repo:      repo,
	}
}

// addSFCommit creates an SF-generated commit in the local repo.
func (r *replayTestRepo) addSFCommit(t *testing.T, filename, content, message string) plumbing.Hash {
	t.Helper()
	wt, _ := r.repo.Worktree()
	writeTestFile(t, r.localDir, filename, content)
	wt.Add(filename)
	hash, err := wt.Commit(
		message+"\n\n"+sfGeneratedTrailer,
		commitOpts("sf-author"),
	)
	if err != nil {
		t.Fatalf("sf commit: %v", err)
	}
	return hash
}

// advanceRemote adds a commit to the remote (simulates the remote moving ahead).
// Uses a temporary clone since bare repos have no worktree.
func (r *replayTestRepo) advanceRemote(t *testing.T, filename, content string) {
	t.Helper()
	cloneDir := t.TempDir()
	clone, err := git.PlainClone(cloneDir, false, &git.CloneOptions{
		URL: r.remoteDir,
	})
	if err != nil {
		t.Fatalf("clone remote for advance: %v", err)
	}
	cloneWt, _ := clone.Worktree()
	writeTestFile(t, cloneDir, filename, content)
	cloneWt.Add(filename)
	testCommit(t, cloneWt, "chore: remote advance\n\nX-StageFreight-Generated: true")
	if err := clone.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push remote advance: %v", err)
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func testCommit(t *testing.T, wt *git.Worktree, message string) plumbing.Hash {
	t.Helper()
	hash, err := wt.Commit(message, commitOpts("test-author"))
	if err != nil {
		t.Fatalf("commit %q: %v", message, err)
	}
	return hash
}

func commitOpts(name string) *git.CommitOptions {
	return &git.CommitOptions{
		Author: &object.Signature{
			Name:  name,
			Email: name + "@test",
			When:  time.Now(),
		},
	}
}

func openSession(t *testing.T, dir string) *gitstate.SyncSession {
	t.Helper()
	session, err := gitstate.OpenSyncSession(dir)
	if err != nil {
		t.Fatalf("open sync session: %v", err)
	}
	return session
}

// ─── Basic cases ──────────────────────────────────────────────────────────────

func TestReplay_SingleFileModify(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "from upstream")

	// Add a local SF commit that modifies a file
	r.addSFCommit(t, "README.md", "modified content", "fix: update readme")

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	// Verify tree equivalence: remote HEAD tree == what we committed
	assertRemoteHasFile(t, r.remoteDir, "README.md", "modified content")
}

func TestReplay_MultiFileModify(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "from upstream")

	wt, _ := r.repo.Worktree()
	writeTestFile(t, r.localDir, "a.txt", "file a")
	writeTestFile(t, r.localDir, "b.txt", "file b")
	wt.Add("a.txt")
	wt.Add("b.txt")
	testCommit(t, wt, fmt.Sprintf("feat: add multiple files\n\n%s", sfGeneratedTrailer))

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay failed: %v", err)
	}

	assertRemoteHasFile(t, r.remoteDir, "a.txt", "file a")
	assertRemoteHasFile(t, r.remoteDir, "b.txt", "file b")
}

func TestReplay_AddFile(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream")
	r.addSFCommit(t, "newfile.txt", "hello new file", "feat: add new file")

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	assertRemoteHasFile(t, r.remoteDir, "newfile.txt", "hello new file")
}

func TestReplay_DeleteFile(t *testing.T) {
	r := newReplayTestRepo(t)
	// First add the file to be deleted
	r.addSFCommit(t, "todelete.txt", "content", "feat: add file to delete")
	// Push this first
	session0 := openSession(t, r.localDir)
	if err := session0.Push("origin", "", false); err != nil {
		t.Fatalf("push before delete test: %v", err)
	}
	session0.Refresh()

	// Advance remote
	r.advanceRemote(t, "upstream.txt", "upstream advance")

	// Now delete the file in a local SF commit
	wt, _ := r.repo.Worktree()
	wt.Remove("todelete.txt")
	testCommit(t, wt, fmt.Sprintf("fix: remove file\n\n%s", sfGeneratedTrailer))

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	assertRemoteFileMissing(t, r.remoteDir, "todelete.txt")
}

// ─── Edge cases ───────────────────────────────────────────────────────────────

func TestReplay_BinaryFile(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream")

	// Write binary content
	binaryPath := filepath.Join(r.localDir, "data.bin")
	binaryContent := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}
	os.WriteFile(binaryPath, binaryContent, 0o644)

	wt, _ := r.repo.Worktree()
	wt.Add("data.bin")
	testCommit(t, wt, fmt.Sprintf("feat: add binary file\n\n%s", sfGeneratedTrailer))

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	assertRemoteHasFileBytes(t, r.remoteDir, "data.bin", binaryContent)
}

func TestReplay_EmptyCommit(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream")

	// Create an empty commit (no changes, message only)
	wt, _ := r.repo.Worktree()
	hash, err := wt.Commit(
		"chore: empty commit\n\n"+sfGeneratedTrailer,
		&git.CommitOptions{
			AllowEmptyCommits: true,
			Author: &object.Signature{
				Name:  "test",
				Email: "test@test",
				When:  time.Now(),
			},
		},
	)
	if err != nil {
		t.Fatalf("empty commit: %v", err)
	}
	_ = hash

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay failed on empty commit: %v", err)
	}
}

// ─── Graph cases ──────────────────────────────────────────────────────────────

func TestReplay_SingleCommit(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream1")
	r.addSFCommit(t, "local.txt", "local content", "feat: local change")

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay single commit: %v", err)
	}
	assertRemoteHasFile(t, r.remoteDir, "local.txt", "local content")
}

func TestReplay_NCommits(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "from remote")

	// Add 5 local SF commits
	for i := 0; i < 5; i++ {
		r.addSFCommit(t,
			fmt.Sprintf("file%d.txt", i),
			fmt.Sprintf("content %d", i),
			fmt.Sprintf("feat: add file %d", i),
		)
	}

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay N commits: %v", err)
	}

	// All 5 files should be on remote
	for i := 0; i < 5; i++ {
		assertRemoteHasFile(t, r.remoteDir, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("content %d", i))
	}
}

func TestReplay_TreeEquivalence(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream")

	// Multiple local SF commits
	r.addSFCommit(t, "a.txt", "a content", "feat: a")
	r.addSFCommit(t, "b.txt", "b content", "feat: b")

	// Capture original local HEAD tree hash
	repo, _ := git.PlainOpen(r.localDir)
	head, _ := repo.Head()
	headCommit, _ := repo.CommitObject(head.Hash())
	origTree, _ := headCommit.Tree()
	origTreeHash := origTree.Hash

	session := openSession(t, r.localDir)
	if err := Replay(session); err != nil {
		t.Fatalf("replay: %v", err)
	}

	// Verify remote HEAD tree == original local HEAD tree
	remoteRepo, _ := git.PlainOpen(r.remoteDir)
	remoteHead, _ := remoteRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	remoteCommit, _ := remoteRepo.CommitObject(remoteHead.Hash())
	remoteTree, _ := remoteCommit.Tree()

	if remoteTree.Hash != origTreeHash {
		t.Errorf("tree equivalence failed: local=%s remote=%s", origTreeHash, remoteTree.Hash)
	}
}

// ─── Gate failure cases ───────────────────────────────────────────────────────

func TestReplay_GateRejects_MergeCommit(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream")

	// Create a merge commit by adding a branch and merging
	repo := r.repo
	wt, _ := repo.Worktree()

	// Create and checkout a feature branch
	err := wt.Checkout(&git.CheckoutOptions{
		Branch: "refs/heads/feature",
		Create: true,
	})
	if err != nil {
		t.Fatalf("checkout feature: %v", err)
	}
	writeTestFile(t, r.localDir, "feature.txt", "feature")
	wt.Add("feature.txt")
	testCommit(t, wt, "feat: feature\n\n"+sfGeneratedTrailer)

	// Checkout main and create another commit
	wt.Checkout(&git.CheckoutOptions{Branch: "refs/heads/main"})
	writeTestFile(t, r.localDir, "main.txt", "main")
	wt.Add("main.txt")
	testCommit(t, wt, "feat: main\n\n"+sfGeneratedTrailer)

	// Merge feature into main (creates merge commit — cannot have trailer verification issue here
	// because merge commits have 2 parents, which is what we're testing)
	featureRef, _ := repo.Reference(plumbing.NewBranchReferenceName("feature"), true)
	mainRef, _ := repo.Head()
	featureCommit, _ := repo.CommitObject(featureRef.Hash())
	mainCommit, _ := repo.CommitObject(mainRef.Hash())

	// Manually create a merge commit with 2 parents
	mergeHash, err := wt.Commit(
		"Merge branch 'feature'\n\n"+sfGeneratedTrailer,
		&git.CommitOptions{
			Author: &object.Signature{Name: "test", Email: "test@test", When: time.Now()},
			Parents: []plumbing.Hash{mainCommit.Hash, featureCommit.Hash},
		},
	)
	if err != nil {
		t.Fatalf("create merge commit: %v", err)
	}
	_ = mergeHash

	session := openSession(t, r.localDir)
	err = Replay(session)
	if err == nil {
		t.Fatal("expected ErrReplayUnsafe for merge commit, got nil")
	}
	var replayErr *gitstate.ErrReplayUnsafe
	if !isReplayUnsafe(err, &replayErr) {
		t.Errorf("expected ErrReplayUnsafe, got %T: %v", err, err)
	}
}

func TestReplay_GateRejects_MissingTrailer(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream")

	// Add a commit WITHOUT the SF trailer
	wt, _ := r.repo.Worktree()
	writeTestFile(t, r.localDir, "manual.txt", "manual change")
	wt.Add("manual.txt")
	testCommit(t, wt, "fix: manual fix (no trailer)")

	session := openSession(t, r.localDir)
	err := Replay(session)
	if err == nil {
		t.Fatal("expected ErrReplayUnsafe for missing trailer, got nil")
	}
	var replayErr *gitstate.ErrReplayUnsafe
	if !isReplayUnsafe(err, &replayErr) {
		t.Errorf("expected ErrReplayUnsafe, got %T: %v", err, err)
	}
}

func TestReplay_GateDoesNotMutate(t *testing.T) {
	r := newReplayTestRepo(t)
	r.advanceRemote(t, "upstream.txt", "upstream")

	// Add a commit without trailer — gate should reject before any mutation
	wt, _ := r.repo.Worktree()
	writeTestFile(t, r.localDir, "bad.txt", "bad commit")
	wt.Add("bad.txt")
	testCommit(t, wt, "feat: human commit (no trailer)")

	repo, _ := git.PlainOpen(r.localDir)
	headBefore, _ := repo.Head()

	session := openSession(t, r.localDir)
	_ = Replay(session)

	headAfter, _ := repo.Head()
	if headBefore.Hash() != headAfter.Hash() {
		t.Error("gate rejection must not mutate HEAD")
	}
}

// ─── assertion helpers ────────────────────────────────────────────────────────

func assertRemoteHasFile(t *testing.T, remoteDir, filename, expectedContent string) {
	t.Helper()
	checkoutDir := t.TempDir()
	clone, err := git.PlainClone(checkoutDir, false, &git.CloneOptions{URL: remoteDir})
	if err != nil {
		t.Fatalf("clone for assertion: %v", err)
	}
	head, _ := clone.Head()
	commit, _ := clone.CommitObject(head.Hash())
	tree, _ := commit.Tree()
	file, err := tree.File(filename)
	if err != nil {
		t.Fatalf("file %s not found in remote: %v", filename, err)
	}
	content, _ := file.Contents()
	if content != expectedContent {
		t.Errorf("file %s: got %q, want %q", filename, content, expectedContent)
	}
}

func assertRemoteHasFileBytes(t *testing.T, remoteDir, filename string, expected []byte) {
	t.Helper()
	checkoutDir := t.TempDir()
	clone, err := git.PlainClone(checkoutDir, false, &git.CloneOptions{URL: remoteDir})
	if err != nil {
		t.Fatalf("clone for assertion: %v", err)
	}
	head, _ := clone.Head()
	commit, _ := clone.CommitObject(head.Hash())
	tree, _ := commit.Tree()
	file, err := tree.File(filename)
	if err != nil {
		t.Fatalf("file %s not found in remote: %v", filename, err)
	}
	reader, _ := file.Blob.Reader()
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != string(expected) {
		t.Errorf("file %s: binary content mismatch (%d bytes got, %d expected)", filename, len(data), len(expected))
	}
}

func assertRemoteFileMissing(t *testing.T, remoteDir, filename string) {
	t.Helper()
	checkoutDir := t.TempDir()
	clone, err := git.PlainClone(checkoutDir, false, &git.CloneOptions{URL: remoteDir})
	if err != nil {
		t.Fatalf("clone for assertion: %v", err)
	}
	head, _ := clone.Head()
	commit, _ := clone.CommitObject(head.Hash())
	tree, _ := commit.Tree()
	if _, err := tree.File(filename); err == nil {
		t.Errorf("file %s should be missing from remote, but it exists", filename)
	}
}

func isReplayUnsafe(err error, target **gitstate.ErrReplayUnsafe) bool {
	if rErr, ok := err.(*gitstate.ErrReplayUnsafe); ok {
		*target = rErr
		return true
	}
	return false
}
