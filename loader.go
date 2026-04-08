package governance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"gopkg.in/yaml.v3"

	"github.com/PrPlanIT/StageFreight/src/config/preset"
	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// LoadGovernance loads governance config and returns a preset loader.
// When source.LocalPath is set (CI mode), uses the local checkout directly.
// Otherwise, fetches the policy repo at the pinned ref.
// Ref must be pinned (tag or commit SHA) unless AllowFloating is true.
func LoadGovernance(source GovernanceSource) (*GovernanceConfig, PresetLoader, error) {
	var checkoutDir string

	if source.LocalPath != "" {
		// CI mode — repo is already checked out at the correct ref.
		checkoutDir = source.LocalPath
	} else {
		if err := ValidateRef(source.Ref, source.AllowFloating); err != nil {
			return nil, nil, fmt.Errorf("governance source: %w", err)
		}

		var err error
		checkoutDir, err = fetchRepo(source.RepoURL, source.Ref)
		if err != nil {
			return nil, nil, fmt.Errorf("fetching policy repo: %w", err)
		}
	}

	// Parse governance config.
	configPath := filepath.Join(checkoutDir, source.Path)
	gov, err := parseClusters(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing governance config: %w", err)
	}

	// Return a loader rooted in the checkout dir.
	loader := preset.NewLocalLoader(checkoutDir)

	return gov, loader, nil
}

// ValidateRef checks pinning rules.
// Pinned tag or commit SHA: always allowed.
// Branch ref: only if allowFloating is true.
// Empty: hard error.
func ValidateRef(ref string, allowFloating bool) error {
	if ref == "" {
		return fmt.Errorf("ref is required (pinned tag or commit SHA)")
	}

	// SHA pattern: 7-40 hex chars.
	if isSHA.MatchString(ref) {
		return nil
	}

	// Tag pattern: starts with v and has dots, or is a semver-ish string.
	if isTag.MatchString(ref) {
		return nil
	}

	// Anything else is treated as a branch.
	if !allowFloating {
		return fmt.Errorf("ref %q looks like a branch; pinned tag or commit SHA required (set allow_floating: true to override)", ref)
	}

	return nil
}

var (
	isSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	isTag = regexp.MustCompile(`^v?\d+\.\d+`)
)

// fetchRepo clones the policy repo at the given ref into a temp directory.
// Returns the checkout path. Caller should NOT clean up — immutable for the run.
// Tries tag ref first, then branch ref, then falls back to fetchBySHA for commit SHAs.
func fetchRepo(repoURL, ref string) (string, error) {
	auth, err := gitstate.ResolveAuthForURL(repoURL)
	if err != nil {
		return "", fmt.Errorf("resolving auth: %w", err)
	}

	cloneWithRef := func(refName plumbing.ReferenceName) (string, error) {
		dir, err := os.MkdirTemp("", "sf-governance-*")
		if err != nil {
			return "", err
		}
		_, err = git.PlainClone(dir, false, &git.CloneOptions{
			URL:           repoURL,
			ReferenceName: refName,
			SingleBranch:  true,
			Depth:         1,
			Auth:          auth,
		})
		if err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		return dir, nil
	}

	// Try as tag (most governance refs are version tags)
	if dir, err := cloneWithRef(plumbing.NewTagReferenceName(ref)); err == nil {
		return dir, nil
	}
	// Try as branch
	if dir, err := cloneWithRef(plumbing.NewBranchReferenceName(ref)); err == nil {
		return dir, nil
	}
	// Fall back to SHA fetch
	return fetchBySHA(repoURL, ref, auth)
}

// fetchBySHA handles commit SHA refs that can't be cloned via --branch.
// Uses PlainInit + CreateRemote + Fetch + Checkout to retrieve a specific commit.
func fetchBySHA(repoURL, sha string, auth transport.AuthMethod) (string, error) {
	tmpDir, err := os.MkdirTemp("", "sf-governance-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("git init: %w", err)
	}

	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("adding remote: %w", err)
	}

	// Fetch all named refs before checking out by SHA.
	//
	// Design tradeoff: this fetches the full ref namespace (heads + tags), not
	// just the target commit. Most servers refuse to advertise arbitrary commit
	// objects by hash directly (uploadpack.allowReachableSHA1InWant is off by
	// default). Fetching named refs is the only portable way to make the target
	// commit reachable for checkout.
	//
	// This is intentionally more expensive than a single-ref fetch. Governance
	// policy repos are expected to be small, and SHA-based refs are rare (used
	// only when a tag or branch ref cannot be used). Depth is intentionally
	// omitted here because shallow clones can prevent commit object resolution.
	if err := repo.Fetch(&git.FetchOptions{
		RemoteName: "origin",
		RefSpecs: []config.RefSpec{
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*",
		},
		Auth: auth,
	}); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("git fetch for %s: %w", sha, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("worktree: %w", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(sha), Force: true}); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("checkout %s: %w", sha, err)
	}

	return tmpDir, nil
}

// parseClusters reads and parses the governance clusters file.
func parseClusters(path string) (*GovernanceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// The file has a top-level "governance" key.
	var wrapper struct {
		Governance GovernanceConfig `yaml:"governance"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	return &wrapper.Governance, nil
}

// FetchFile fetches a single file from a git repo at a specific ref.
// Used as the AssetFetcher for governance distribution.
func FetchFile(repoURL, ref, path string) ([]byte, error) {
	if ref == "" {
		ref = "HEAD"
	}

	checkoutDir, err := fetchRepo(repoURL, ref)
	if err != nil {
		return nil, fmt.Errorf("fetching %s@%s: %w", repoURL, ref, err)
	}
	defer os.RemoveAll(checkoutDir)

	filePath := filepath.Join(checkoutDir, path)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading %s from %s@%s: %w", path, repoURL, ref, err)
	}

	return data, nil
}

