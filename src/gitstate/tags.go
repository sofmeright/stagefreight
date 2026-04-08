package gitstate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// ListTagsSorted returns all tags sorted by version descending, matching git's
// --sort=-version:refname behaviour. Semver tags are sorted by semver; non-semver
// tags are sorted lexicographically after all semver tags.
//
// Parity with git: Masterminds/semver parses the same set of tags as git's
// version-aware sort for real-world repos (semver + v-prefix). Both classify
// tags as version-like or lexicographic using equivalent rules, and both produce
// version-like tags before lexicographic tags in descending order.
// Edge case: tags with identical version values but different prefixes (v1.0.0
// and 1.0.0) are sorted stably by the sort.Slice guarantee — same as git.
func ListTagsSorted(repo *git.Repository) ([]string, error) {
	tagIter, err := repo.Tags()
	if err != nil {
		return nil, fmt.Errorf("listing tags: %w", err)
	}

	var semverTags []struct {
		name string
		v    *semver.Version
	}
	var otherTags []string

	err = tagIter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		v, err := semver.NewVersion(name)
		if err == nil {
			semverTags = append(semverTags, struct {
				name string
				v    *semver.Version
			}{name, v})
		} else {
			otherTags = append(otherTags, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterating tags: %w", err)
	}

	// Sort semver tags descending
	sort.Slice(semverTags, func(i, j int) bool {
		return semverTags[i].v.GreaterThan(semverTags[j].v)
	})

	// Sort non-semver tags lexicographically descending
	sort.Sort(sort.Reverse(sort.StringSlice(otherTags)))

	out := make([]string, 0, len(semverTags)+len(otherTags))
	for _, t := range semverTags {
		out = append(out, t.name)
	}
	out = append(out, otherTags...)
	return out, nil
}

// ExactTagAtHEAD returns the tag name if HEAD is exactly at a tagged commit,
// matching `git describe --tags --exact-match HEAD`. Returns "" if not at a tag.
func ExactTagAtHEAD(repo *git.Repository) (string, error) {
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("resolving HEAD: %w", err)
	}
	headHash := head.Hash()

	tagIter, err := repo.Tags()
	if err != nil {
		return "", fmt.Errorf("listing tags: %w", err)
	}

	var result string
	err = tagIter.ForEach(func(ref *plumbing.Reference) error {
		commitHash := ref.Hash()

		// Dereference annotated tags to their target commit
		tagObj, err := repo.TagObject(ref.Hash())
		if err == nil {
			commitHash = tagObj.Target
		}

		if commitHash == headHash {
			result = ref.Name().Short()
			return storer.ErrStop
		}
		return nil
	})
	if err != nil && err != storer.ErrStop {
		return "", fmt.Errorf("iterating tags: %w", err)
	}

	return result, nil
}

// ResolveRef resolves any git ref (tag, branch, commit SHA, HEAD) to a commit SHA.
// Equivalent to `git rev-parse --verify <ref>^{commit}`.
func ResolveRef(repo *git.Repository, ref string) (string, error) {
	hash, err := repo.ResolveRevision(plumbing.Revision(ref + "^{commit}"))
	if err != nil {
		// Try without ^{commit} for lightweight tags and plain SHAs
		hash, err = repo.ResolveRevision(plumbing.Revision(ref))
		if err != nil {
			return "", fmt.Errorf("ref %q does not resolve to a commit: %w", ref, err)
		}
	}
	return hash.String(), nil
}

// IsAncestor returns true if ancestorRef is a (possibly indirect) ancestor of descendantRef.
// Equivalent to `git merge-base --is-ancestor <ancestor> <descendant>`.
func IsAncestor(repo *git.Repository, ancestorRef, descendantRef string) (bool, error) {
	ancestorHash, err := resolveRefToHash(repo, ancestorRef)
	if err != nil {
		return false, fmt.Errorf("resolving ancestor ref %q: %w", ancestorRef, err)
	}
	descendantHash, err := resolveRefToHash(repo, descendantRef)
	if err != nil {
		return false, fmt.Errorf("resolving descendant ref %q: %w", descendantRef, err)
	}
	return isAncestorHash(repo, ancestorHash, descendantHash)
}

// isAncestorHash checks ancestry by hash.
func isAncestorHash(repo *git.Repository, ancestorHash, descendantHash plumbing.Hash) (bool, error) {
	if ancestorHash == descendantHash {
		return true, nil
	}

	ancestorCommit, err := repo.CommitObject(ancestorHash)
	if err != nil {
		return false, fmt.Errorf("loading ancestor commit: %w", err)
	}
	descendantCommit, err := repo.CommitObject(descendantHash)
	if err != nil {
		return false, fmt.Errorf("loading descendant commit: %w", err)
	}

	bases, err := ancestorCommit.MergeBase(descendantCommit)
	if err != nil {
		return false, fmt.Errorf("computing merge base: %w", err)
	}

	for _, base := range bases {
		if base.Hash == ancestorHash {
			return true, nil
		}
	}
	return false, nil
}

// TagMessage returns the annotation message for an annotated tag.
// Returns "" for lightweight tags or on error (best-effort).
func TagMessage(repo *git.Repository, ref string) string {
	refName := plumbing.NewTagReferenceName(strings.TrimPrefix(ref, "refs/tags/"))
	tagRef, err := repo.Reference(refName, true)
	if err != nil {
		return ""
	}

	tagObj, err := repo.TagObject(tagRef.Hash())
	if err != nil {
		return "" // lightweight tag
	}

	msg := tagObj.Message
	// Strip PGP signature block
	if idx := strings.Index(msg, "-----BEGIN PGP SIGNATURE-----"); idx >= 0 {
		msg = msg[:idx]
	}
	return strings.TrimSpace(msg)
}

// CountCommitsBetween counts commits reachable from `to` but not from `from`.
// Equivalent to `git rev-list --count <from>..<to>`.
func CountCommitsBetween(repo *git.Repository, fromRef, toRef string) (int, error) {
	fromHash, err := resolveRefToHash(repo, fromRef)
	if err != nil {
		return 0, fmt.Errorf("resolving from ref %q: %w", fromRef, err)
	}
	toHash, err := resolveRefToHash(repo, toRef)
	if err != nil {
		return 0, fmt.Errorf("resolving to ref %q: %w", toRef, err)
	}

	// Find merge base (acts as the boundary)
	fromCommit, err := repo.CommitObject(fromHash)
	if err != nil {
		return 0, err
	}
	toCommit, err := repo.CommitObject(toHash)
	if err != nil {
		return 0, err
	}

	bases, err := fromCommit.MergeBase(toCommit)
	if err != nil || len(bases) == 0 {
		return 0, err
	}
	boundary := bases[0].Hash

	count := 0
	logIter, err := repo.Log(&git.LogOptions{From: toHash})
	if err != nil {
		return 0, err
	}
	err = logIter.ForEach(func(c *object.Commit) error {
		if c.Hash == boundary {
			return storer.ErrStop
		}
		count++
		return nil
	})
	if err != nil && err != storer.ErrStop {
		return 0, err
	}
	return count, nil
}

// ParseCommitLog returns commits in the range fromRef..toRef as (hash, subject, body, author) tuples.
func ParseCommitLog(repo *git.Repository, fromRef, toRef string) ([]*object.Commit, error) {
	toHash, err := resolveRefToHash(repo, toRef)
	if err != nil {
		return nil, fmt.Errorf("resolving to ref: %w", err)
	}

	var boundary plumbing.Hash
	if fromRef != "" {
		fromHash, err := resolveRefToHash(repo, fromRef)
		if err != nil {
			return nil, fmt.Errorf("resolving from ref: %w", err)
		}
		fromCommit, err := repo.CommitObject(fromHash)
		if err != nil {
			return nil, err
		}
		toCommit, err := repo.CommitObject(toHash)
		if err != nil {
			return nil, err
		}
		bases, err := fromCommit.MergeBase(toCommit)
		if err == nil && len(bases) > 0 {
			boundary = bases[0].Hash
		} else {
			// Fallback: use fromHash as boundary
			boundary = fromHash
		}
	}

	var commits []*object.Commit
	logIter, err := repo.Log(&git.LogOptions{From: toHash})
	if err != nil {
		return nil, err
	}
	err = logIter.ForEach(func(c *object.Commit) error {
		if fromRef != "" && (c.Hash == boundary) {
			return storer.ErrStop
		}
		commits = append(commits, c)
		return nil
	})
	if err != nil && err != storer.ErrStop {
		return nil, err
	}
	return commits, nil
}

// DiffStats returns file/insertion/deletion counts between two refs.
func DiffStats(repo *git.Repository, fromRef, toRef string) (files, insertions, deletions int, err error) {
	fromHash, err := resolveRefToHash(repo, fromRef)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("resolving from ref: %w", err)
	}
	toHash, err := resolveRefToHash(repo, toRef)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("resolving to ref: %w", err)
	}

	fromCommit, err := repo.CommitObject(fromHash)
	if err != nil {
		return 0, 0, 0, err
	}
	toCommit, err := repo.CommitObject(toHash)
	if err != nil {
		return 0, 0, 0, err
	}

	fromTree, err := fromCommit.Tree()
	if err != nil {
		return 0, 0, 0, err
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return 0, 0, 0, err
	}

	changes, err := fromTree.Diff(toTree)
	if err != nil {
		return 0, 0, 0, err
	}

	files = len(changes)
	for _, change := range changes {
		from, to, err := change.Files()
		if err != nil {
			continue
		}
		var fromLines, toLines int
		if from != nil {
			content, err := from.Contents()
			if err == nil {
				fromLines = strings.Count(content, "\n") + 1
			}
		}
		if to != nil {
			content, err := to.Contents()
			if err == nil {
				toLines = strings.Count(content, "\n") + 1
			}
		}
		if toLines > fromLines {
			insertions += toLines - fromLines
		} else if fromLines > toLines {
			deletions += fromLines - toLines
		}
	}
	return files, insertions, deletions, nil
}

// resolveRefToHash resolves any ref string to a plumbing.Hash.
func resolveRefToHash(repo *git.Repository, ref string) (plumbing.Hash, error) {
	// Handle special cases first
	if ref == "HEAD" {
		head, err := repo.Head()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return head.Hash(), nil
	}

	// Try as a revision
	hash, err := repo.ResolveRevision(plumbing.Revision(ref + "^{commit}"))
	if err == nil {
		return *hash, nil
	}
	hash, err = repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("cannot resolve ref %q: %w", ref, err)
	}
	return *hash, nil
}
