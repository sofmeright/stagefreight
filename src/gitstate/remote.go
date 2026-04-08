package gitstate

import (
	"fmt"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// RemoteURL returns the URL for the given remote (typically "origin").
func RemoteURL(repo *git.Repository, remoteName string) (string, error) {
	cfg, err := repo.Config()
	if err != nil {
		return "", fmt.Errorf("reading repo config: %w", err)
	}
	r, ok := cfg.Remotes[remoteName]
	if !ok || len(r.URLs) == 0 {
		return "", fmt.Errorf("remote %q not configured", remoteName)
	}
	return r.URLs[0], nil
}

// RemoteRefHash returns the hash of a specific ref on the remote.
// Equivalent to `git ls-remote origin refs/heads/<branch>`.
// Requires network access.
func RemoteRefHash(repo *git.Repository, remoteName, refName string, auth transport.AuthMethod) (plumbing.Hash, error) {
	rem, err := repo.Remote(remoteName)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("opening remote %q: %w", remoteName, err)
	}

	refs, err := rem.List(&git.ListOptions{Auth: auth})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("listing remote refs: %w", err)
	}

	target := plumbing.NewBranchReferenceName(refName)
	for _, ref := range refs {
		if ref.Name() == target {
			return ref.Hash(), nil
		}
	}

	return plumbing.ZeroHash, fmt.Errorf("ref %q not found on remote %q", refName, remoteName)
}
