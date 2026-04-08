package release

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var notesSig = &object.Signature{
	Name:  "test",
	Email: "test@test",
	When:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
}

// setupTestRepo creates a temporary git repo with a linear commit history
// and the specified tags. Returns the repo directory.
// Each commit is a trivial file change so tags land on distinct commits.
func setupTestRepo(t *testing.T, commits int, tags map[int][]string) string {
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

	for i := 1; i <= commits; i++ {
		f := filepath.Join(dir, "file.txt")
		if err := os.WriteFile(f, []byte{byte(i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := wt.Add("file.txt"); err != nil {
			t.Fatalf("git add commit %d: %v", i, err)
		}
		sig := *notesSig
		sig.When = sig.When.Add(time.Duration(i) * time.Second)
		hash, err := wt.Commit(fmt.Sprintf("commit %d", i), &git.CommitOptions{
			Author:    &sig,
			Committer: &sig,
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}

		if tagNames, ok := tags[i]; ok {
			for _, tag := range tagNames {
				sig2 := sig
				if _, err := repo.CreateTag(tag, hash, &git.CreateTagOptions{
					Tagger:  &sig2,
					Message: tag,
				}); err != nil {
					t.Fatalf("tag %s at commit %d: %v", tag, i, err)
				}
			}
		}
	}

	return dir
}

func TestPreviousReleaseTag_SkipsLatest(t *testing.T) {
	// Commit history: 1(v0.0.2) -> 2(latest) -> 3(v0.1.0)
	repo := setupTestRepo(t, 3, map[int][]string{
		1: {"v0.0.2"},
		2: {"latest"},
		3: {"v0.1.0"},
	})

	got, err := PreviousReleaseTag(repo, "v0.1.0", []string{`^v?\d+\.\d+\.\d+$`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.2" {
		t.Errorf("got %q, want %q", got, "v0.0.2")
	}
}

func TestPreviousReleaseTag_SkipsSameVersionAlias(t *testing.T) {
	// Commit history: 1(v0.0.2) -> 2(0.1.0) -> 3(v0.1.0)
	// 0.1.0 is a stale bare-version alias from a failed release attempt.
	repo := setupTestRepo(t, 3, map[int][]string{
		1: {"v0.0.2"},
		2: {"0.1.0"},
		3: {"v0.1.0"},
	})

	got, err := PreviousReleaseTag(repo, "v0.1.0", []string{`^v?\d+\.\d+\.\d+$`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.2" {
		t.Errorf("got %q, want %q", got, "v0.0.2")
	}
}

func TestPreviousReleaseTag_SkipsSameCommitAlias(t *testing.T) {
	// v0.1.0 and 0.1.0 on the SAME commit (rolling alias created during release).
	// Must still find v0.0.2.
	repo := setupTestRepo(t, 2, map[int][]string{
		1: {"v0.0.2"},
		2: {"v0.1.0", "0.1.0", "latest"},
	})

	got, err := PreviousReleaseTag(repo, "v0.1.0", []string{`^v?\d+\.\d+\.\d+$`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.2" {
		t.Errorf("got %q, want %q", got, "v0.0.2")
	}
}

func TestPreviousReleaseTag_DefaultPatternFallback(t *testing.T) {
	// No patterns provided — should fall back to default semver matcher.
	repo := setupTestRepo(t, 3, map[int][]string{
		1: {"v0.0.2"},
		2: {"latest"},
		3: {"v0.1.0"},
	})

	got, err := PreviousReleaseTag(repo, "v0.1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.2" {
		t.Errorf("got %q, want %q", got, "v0.0.2")
	}
}

func TestPreviousReleaseTag_PatternExcludesBareVersion(t *testing.T) {
	// Policy only matches v-prefixed tags. Bare 0.1.0 must be ignored
	// even though it's a valid ancestor.
	repo := setupTestRepo(t, 3, map[int][]string{
		1: {"v0.0.2"},
		2: {"0.1.0"},
		3: {"v0.2.0"},
	})

	got, err := PreviousReleaseTag(repo, "v0.2.0", []string{`^v\d+\.\d+\.\d+$`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.2" {
		t.Errorf("got %q, want %q", got, "v0.0.2")
	}
}

func TestPreviousReleaseTag_NonAncestorSkipped(t *testing.T) {
	// Create a branch with a higher version tag that's not an ancestor of main.
	dir := t.TempDir()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	sig := func(i int) *object.Signature {
		s := *notesSig
		s.When = s.When.Add(time.Duration(i) * time.Second)
		return &s
	}

	writeAndCommit := func(content, msg string, i int) plumbing.Hash {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := wt.Add("f.txt"); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(msg, &git.CommitOptions{Author: sig(i), Committer: sig(i)})
		if err != nil {
			t.Fatalf("commit %q: %v", msg, err)
		}
		return h
	}

	createTag := func(name string, hash plumbing.Hash, i int) {
		t.Helper()
		if _, err := repo.CreateTag(name, hash, &git.CreateTagOptions{
			Tagger:  sig(i),
			Message: name,
		}); err != nil {
			t.Fatalf("tag %s: %v", name, err)
		}
	}

	// Commit 1: base on main
	baseHash := writeAndCommit("1", "base", 1)
	createTag("v0.0.1", baseHash, 1)

	// Branch off to "other"
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("other"),
		Create: true,
	}); err != nil {
		t.Fatalf("checkout other: %v", err)
	}
	otherHash := writeAndCommit("other", "other branch", 2)
	createTag("v0.9.0", otherHash, 2) // higher version, not on main's history

	// Back to main
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("main"),
	}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	mainHash := writeAndCommit("2", "main commit", 3)
	createTag("v0.1.0", mainHash, 3)

	// v0.9.0 exists but is NOT an ancestor of v0.1.0
	got, err := PreviousReleaseTag(dir, "v0.1.0", []string{`^v?\d+\.\d+\.\d+$`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.1" {
		t.Errorf("got %q, want %q", got, "v0.0.1")
	}
}

func TestPreviousReleaseTag_NoPreviousTag(t *testing.T) {
	// Only the current tag exists. Should return empty, not error.
	repo := setupTestRepo(t, 1, map[int][]string{
		1: {"v0.1.0"},
	})

	got, err := PreviousReleaseTag(repo, "v0.1.0", []string{`^v?\d+\.\d+\.\d+$`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestPreviousReleaseTag_BareCurrentRef(t *testing.T) {
	// currentRef passed as bare version "0.1.0" — same-version exclusion
	// must still work against v0.1.0 tags in history.
	repo := setupTestRepo(t, 3, map[int][]string{
		1: {"v0.0.2"},
		2: {"v0.1.0"},
		3: {"0.1.0"},
	})

	got, err := PreviousReleaseTag(repo, "0.1.0", []string{`^v?\d+\.\d+\.\d+$`})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.0.2" {
		t.Errorf("got %q, want %q", got, "v0.0.2")
	}
}

func TestPreviousReleaseTag_PrereleaseIncluded(t *testing.T) {
	// Prerelease tags matching the prerelease policy should be eligible.
	repo := setupTestRepo(t, 3, map[int][]string{
		1: {"v0.0.2"},
		2: {"v0.1.0-rc1"},
		3: {"v0.1.0"},
	})

	patterns := []string{
		`^v?\d+\.\d+\.\d+$`,
		`^v?\d+\.\d+\.\d+-.+`,
	}

	got, err := PreviousReleaseTag(repo, "v0.1.0", patterns)
	if err != nil {
		t.Fatal(err)
	}
	// v0.1.0-rc1 is a different normalized version (0.1.0-rc1 != 0.1.0)
	// and is closer than v0.0.2, so it should be found first.
	if got != "v0.1.0-rc1" {
		t.Errorf("got %q, want %q", got, "v0.1.0-rc1")
	}
}

func TestNormalizeReleaseVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"v0.1.0", "0.1.0"},
		{"0.1.0", "0.1.0"},
		{"refs/tags/v0.1.0", "0.1.0"},
		{"refs/tags/0.1.0", "0.1.0"},
		{"v1.2.3-rc1", "1.2.3-rc1"},
		{"latest", "latest"},
	}
	for _, tt := range tests {
		got := normalizeReleaseVersion(tt.input)
		if got != tt.want {
			t.Errorf("normalizeReleaseVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCompileReleaseTagMatchers_InvalidPattern(t *testing.T) {
	_, err := compileReleaseTagMatchers([]string{`[invalid`})
	if err == nil {
		t.Error("expected error for invalid regex pattern, got nil")
	}
}

func TestCompileReleaseTagMatchers_EmptyFallback(t *testing.T) {
	matchers, err := compileReleaseTagMatchers(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(matchers) != 1 {
		t.Fatalf("expected 1 default matcher, got %d", len(matchers))
	}
	if !matchers[0].MatchString("v0.1.0") {
		t.Error("default matcher should match v0.1.0")
	}
	if !matchers[0].MatchString("0.1.0") {
		t.Error("default matcher should match 0.1.0")
	}
	if matchers[0].MatchString("latest") {
		t.Error("default matcher should NOT match latest")
	}
}
