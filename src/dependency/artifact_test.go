package dependency

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestWritePatch freezes the StageFreight patch artifact contract.
//
// deps.patch is a StageFreight-native inspection artifact — NOT git-apply compatible.
// This test locks in that the artifact captures the three fundamental change types
// (modified, deleted, new) before they can drift silently.
func TestWritePatch(t *testing.T) {
	dir := initTestRepo(t, map[string]string{
		"modified.txt": "original content\n",
		"deleted.txt":  "content to delete\n",
	})

	// Apply three change types directly to the working tree (unstaged).
	mustWriteFile(t, dir, "modified.txt", "updated content\n")
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatalf("remove deleted.txt: %v", err)
	}
	mustWriteFile(t, dir, "new.txt", "brand new file\n")

	patch := runWritePatch(t, dir)

	if strings.TrimSpace(patch) == "" {
		t.Fatal("patch output is empty")
	}

	t.Logf("patch output:\n%s", patch)

	// Modified file: both old and new content present.
	if !strings.Contains(patch, "modified.txt") {
		t.Error("patch: missing modified.txt")
	}
	if !strings.Contains(patch, "original content") {
		t.Error("patch: missing original content of modified.txt")
	}
	if !strings.Contains(patch, "updated content") {
		t.Error("patch: missing updated content of modified.txt")
	}

	// Deleted file: old content present, file identified in diff.
	if !strings.Contains(patch, "deleted.txt") {
		t.Error("patch: missing deleted.txt")
	}
	if !strings.Contains(patch, "content to delete") {
		t.Error("patch: missing content of deleted.txt")
	}

	// New untracked file: new content present, file identified in diff.
	if !strings.Contains(patch, "new.txt") {
		t.Error("patch: missing new.txt")
	}
	if !strings.Contains(patch, "brand new file") {
		t.Error("patch: missing content of new.txt")
	}
}

// TestWritePatchOrdering verifies that changed files appear in sorted path order.
// Deterministic output is part of the artifact contract — CI diffs must be stable.
func TestWritePatchOrdering(t *testing.T) {
	// Seed files in intentionally scrambled order so the natural map iteration
	// would produce non-deterministic output if sort.Strings were removed.
	dir := initTestRepo(t, map[string]string{
		"z-last.txt":  "z\n",
		"a-first.txt": "a\n",
		"m-middle.txt": "m\n",
	})

	mustWriteFile(t, dir, "z-last.txt", "z updated\n")
	mustWriteFile(t, dir, "a-first.txt", "a updated\n")
	mustWriteFile(t, dir, "m-middle.txt", "m updated\n")

	patch := runWritePatch(t, dir)

	if strings.TrimSpace(patch) == "" {
		t.Fatal("patch output is empty")
	}

	posA := strings.Index(patch, "diff --git a/a-first.txt")
	posM := strings.Index(patch, "diff --git a/m-middle.txt")
	posZ := strings.Index(patch, "diff --git a/z-last.txt")

	if posA < 0 || posM < 0 || posZ < 0 {
		t.Fatalf("patch missing expected diff headers: a=%d m=%d z=%d", posA, posM, posZ)
	}
	if !(posA < posM && posM < posZ) {
		t.Errorf("diff headers not in sorted order: a-first.txt@%d m-middle.txt@%d z-last.txt@%d", posA, posM, posZ)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// initTestRepo creates a temp git repo, writes the given files, and makes an
// initial commit containing them all. Returns the repo root directory.
func initTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	for name, content := range files {
		mustWriteFile(t, dir, name, content)
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	if _, err := wt.Commit("chore: initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("initial commit: %v", err)
	}

	return dir
}

// runWritePatch calls writePatch on dir and returns the patch content.
func runWritePatch(t *testing.T, dir string) string {
	t.Helper()
	patchFile := filepath.Join(t.TempDir(), "out.patch")
	if err := writePatch(context.Background(), dir, patchFile); err != nil {
		t.Fatalf("writePatch: %v", err)
	}
	data, err := os.ReadFile(patchFile)
	if err != nil {
		t.Fatalf("reading patch output: %v", err)
	}
	return string(data)
}

func mustWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
