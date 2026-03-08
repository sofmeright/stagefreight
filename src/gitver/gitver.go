// Package gitver provides git-based version detection and tag template
// resolution. It is the shared foundation used by both the docker build
// pipeline and the release management system.
package gitver

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// VersionInfo holds resolved version metadata from git.
type VersionInfo struct {
	Version      string // full version: "1.2.3", "1.2.3-alpha.1", "0.0.0-dev+abc1234"
	Base         string // semver base without prerelease: "1.2.3"
	Major        string
	Minor        string
	Patch        string
	Prerelease   string // "alpha.1", "beta.2", "rc.1", or "" for stable
	SHA          string
	Branch       string
	IsRelease    bool // true if HEAD is exactly at a tag
	IsPrerelease bool // true if tag has a prerelease suffix
}

// semverRe captures major.minor.patch and optional -prerelease suffix.
var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-(.+))?$`)

// DetectVersion resolves version info from git tags and refs.
// Looks for tags matching v* (plain semver tags).
func DetectVersion(rootDir string) (*VersionInfo, error) {
	return DetectScopedVersion(rootDir, "")
}

// DetectScopedVersion resolves version info from git tags with an optional scope prefix.
// When scope is empty, matches plain v* tags (e.g., v0.2.1).
// When scope is set, matches SCOPE-v* tags (e.g., component-v1.0.3) and strips the prefix.
//
// This enables independent versioning for multiple artifacts in the same repo:
//
//	DetectScopedVersion(dir, "")          → v0.2.1          → "0.2.1"
//	DetectScopedVersion(dir, "component") → component-v1.0.3 → "1.0.3"
func DetectScopedVersion(rootDir string, scope string) (*VersionInfo, error) {
	v := &VersionInfo{}

	// Get current SHA
	sha, err := gitCmd(rootDir, "rev-parse", "--short=7", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("getting HEAD SHA: %w", err)
	}
	v.SHA = sha

	// Get current branch
	branch, err := gitCmd(rootDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		v.Branch = branch
	}

	// Build tag match patterns.
	// git describe supports multiple --match flags (Git 2.13+).
	// Match both v-prefixed (v1.2.3) and bare semver (17.9.0) tags.
	// Scoped builds also match scope-prefixed bare versions (component-17.9.0).
	var matchArgs []string
	if scope != "" {
		matchArgs = []string{
			"--match", scope + "-v*",
			"--match", scope + "-[0-9]*",
		}
	} else {
		matchArgs = []string{
			"--match", "v*",
			"--match", "[0-9]*",
		}
	}

	// CI fast path: if the CI system provides an exact tag, trust it.
	// Avoids shallow-clone weirdness and makes tag pipelines deterministic.
	if ciTag := os.Getenv("CI_COMMIT_TAG"); ciTag != "" {
		cand := ciTag
		if scope != "" {
			cand = strings.TrimPrefix(cand, scope+"-")
		}
		if m := semverRe.FindStringSubmatch(cand); m != nil {
			v.Major = m[1]
			v.Minor = m[2]
			v.Patch = m[3]
			v.Base = fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])
			if m[4] != "" {
				v.Prerelease = m[4]
				v.IsPrerelease = true
				v.Version = fmt.Sprintf("%s-%s", v.Base, v.Prerelease)
			} else {
				v.Version = v.Base
			}
			v.IsRelease = true
			return v, nil
		}
	}

	// Try to get version from git describe (nearest matching tag)
	descArgs := append([]string{"describe", "--tags", "--abbrev=0"}, matchArgs...)
	desc, err := gitCmd(rootDir, descArgs...)
	if err != nil {
		// Shallow clone? Fetch tags and retry once.
		if shallow, _ := gitCmd(rootDir, "rev-parse", "--is-shallow-repository"); shallow == "true" {
			gitCmd(rootDir, "fetch", "--tags", "--depth=1")
			desc, err = gitCmd(rootDir, descArgs...)
		}
	}
	if err != nil {
		// No matching tags — use dev version
		v.Version = fmt.Sprintf("0.0.0-dev+%s", v.SHA)
		v.Base = "0.0.0"
		v.Major = "0"
		v.Minor = "0"
		v.Patch = "0"
		return v, nil
	}

	// Check if HEAD is exactly the tag (clean release)
	exactArgs := append([]string{"describe", "--tags", "--exact-match"}, matchArgs...)
	exactTag, exactErr := gitCmd(rootDir, exactArgs...)
	v.IsRelease = exactTag != "" && exactErr == nil

	// Strip scope prefix before parsing semver
	tag := strings.TrimSpace(desc)
	if scope != "" {
		tag = strings.TrimPrefix(tag, scope+"-")
	}

	// Validate described tag is semver; fall back to tag list if not.
	// This handles repos with non-version tags that git describe might pick up.
	if _, ok := tryParseSemver(tag); !ok {
		listArgs := append([]string{"tag", "--list", "--sort=-v:refname"}, matchArgs...)
		out, listErr := gitCmd(rootDir, listArgs...)
		found := false
		if listErr == nil && out != "" {
			for _, t := range strings.Split(out, "\n") {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				cand := t
				if scope != "" {
					cand = strings.TrimPrefix(cand, scope+"-")
				}
				if _, ok2 := tryParseSemver(cand); ok2 {
					tag = cand
					found = true
					break
				}
			}
		}
		if !found {
			v.Version = fmt.Sprintf("0.0.0-dev+%s", v.SHA)
			v.Base = "0.0.0"
			v.Major = "0"
			v.Minor = "0"
			v.Patch = "0"
			return v, nil
		}
	}

	// Parse semver from tag
	if m := semverRe.FindStringSubmatch(tag); m != nil {
		v.Major = m[1]
		v.Minor = m[2]
		v.Patch = m[3]
		v.Base = fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])

		if m[4] != "" {
			// Has prerelease suffix: 0.2.0-alpha.1
			v.Prerelease = m[4]
			v.IsPrerelease = true
			v.Version = fmt.Sprintf("%s-%s", v.Base, v.Prerelease)
		} else {
			v.Version = v.Base
		}
	} else {
		// Non-semver tag — use it raw
		raw := strings.TrimPrefix(tag, "v")
		v.Version = raw
		v.Base = raw
	}

	// If not a release, append dev suffix
	if !v.IsRelease {
		v.Version = fmt.Sprintf("%s-dev+%s", v.Version, v.SHA)
	}

	return v, nil
}

// tryParseSemver checks whether s is a valid semver string (with optional v prefix
// and prerelease suffix). Returns the clean version without v prefix.
func tryParseSemver(s string) (string, bool) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	ver := fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])
	if m[4] != "" {
		ver += "-" + m[4]
	}
	return ver, true
}

// gitCmd runs a git command and returns trimmed stdout.
func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
