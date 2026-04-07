package governance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/PrPlanIT/StageFreight/src/config/preset"
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
func fetchRepo(repoURL, ref string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "sf-governance-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	// Shallow clone at the specific ref.
	cmd := exec.Command("git", "clone", "--depth=1", "--branch", ref, "--single-branch", repoURL, tmpDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// If --branch fails (commit SHA), try fetch approach.
		os.RemoveAll(tmpDir)
		return fetchBySHA(repoURL, ref)
	}

	return tmpDir, nil
}

// fetchBySHA handles commit SHA refs that can't use --branch.
func fetchBySHA(repoURL, sha string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "sf-governance-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	// Init + fetch specific commit.
	cmds := [][]string{
		{"git", "init", tmpDir},
		{"git", "-C", tmpDir, "remote", "add", "origin", repoURL},
		{"git", "-C", tmpDir, "fetch", "--depth=1", "origin", sha},
		{"git", "-C", tmpDir, "checkout", "FETCH_HEAD"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.RemoveAll(tmpDir)
			return "", fmt.Errorf("git %s: %w", strings.Join(args[1:], " "), err)
		}
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
		checkoutDir, err = fetchBySHA(repoURL, ref)
		if err != nil {
			return nil, fmt.Errorf("fetching %s@%s: %w", repoURL, ref, err)
		}
	}
	defer os.RemoveAll(checkoutDir)

	filePath := filepath.Join(checkoutDir, path)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading %s from %s@%s: %w", path, repoURL, ref, err)
	}

	return data, nil
}

