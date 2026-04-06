package release

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/config"
)

// TagPlan is the fully resolved release tag plan.
type TagPlan struct {
	PreviousTag  string
	TargetRef    string
	TargetSHA    string
	NextTag      string
	Message      string
	CommitCount  int
	FilesChanged int
	Insertions   int
	Deletions    int
}

// BuildTagPlanOptions configures tag plan resolution.
type BuildTagPlanOptions struct {
	ExplicitVersion string
	BumpKind        string // patch | minor | major
	TargetRef       string // default HEAD
	FromRef         string // optional previous boundary override
	MessageOverride string
	TagPatterns     []string // from versioning.tag_sources
	Glossary        config.GlossaryConfig
	Presentation    config.TagPresentation
}

// BuildTagPlan resolves a complete tag plan from the repo and options.
func BuildTagPlan(repoDir string, opts BuildTagPlanOptions) (*TagPlan, error) {
	plan := &TagPlan{}

	// 1. Resolve target ref
	targetRef := opts.TargetRef
	if targetRef == "" {
		targetRef = "HEAD"
	}
	plan.TargetRef = targetRef

	sha, err := ResolveGitRef(repoDir, targetRef)
	if err != nil {
		return nil, fmt.Errorf("resolving target ref %q: %w", targetRef, err)
	}
	plan.TargetSHA = sha

	// 2. Find previous release tag
	if opts.FromRef != "" {
		plan.PreviousTag = opts.FromRef
	} else {
		prev, err := PreviousReleaseTag(repoDir, targetRef, opts.TagPatterns)
		if err != nil {
			// No previous tag is OK for first release
			plan.PreviousTag = ""
		} else {
			plan.PreviousTag = prev
		}
	}

	// 3. Resolve next version
	if opts.ExplicitVersion != "" {
		plan.NextTag = opts.ExplicitVersion
	} else if opts.BumpKind != "" {
		if plan.PreviousTag == "" {
			return nil, fmt.Errorf("cannot bump %s: no previous release tag found", opts.BumpKind)
		}
		// Validate --from is a release tag when bumping
		if opts.FromRef != "" {
			isRelease := false
			for _, pattern := range opts.TagPatterns {
				if config.MatchPatterns([]string{pattern}, opts.FromRef) {
					isRelease = true
					break
				}
			}
			if !isRelease {
				return nil, fmt.Errorf("cannot bump from %q: not a release tag (does not match any git_tags policy)", opts.FromRef)
			}
		}
		next, err := BumpVersion(plan.PreviousTag, opts.BumpKind)
		if err != nil {
			return nil, err
		}
		plan.NextTag = next
	}

	// 4. Check tag doesn't already exist
	if plan.NextTag != "" {
		if tagExists(repoDir, plan.NextTag) {
			return nil, fmt.Errorf("tag %q already exists", plan.NextTag)
		}
	}

	// 5. Generate commit range stats
	if plan.PreviousTag != "" {
		plan.CommitCount = countCommits(repoDir, plan.PreviousTag, targetRef)
		stats := diffStats(repoDir, plan.PreviousTag, targetRef)
		plan.FilesChanged = stats.files
		plan.Insertions = stats.insertions
		plan.Deletions = stats.deletions
	}

	// 6. Generate message
	if opts.MessageOverride != "" {
		plan.Message = opts.MessageOverride
	} else {
		commits, _ := ParseCommits(repoDir, plan.PreviousTag, targetRef)
		processed := ProcessCommits(commits, opts.Glossary)
		plan.Message = FormatHighlights(processed, opts.Presentation.MaxEntries)
	}

	return plan, nil
}

// ResolveGitRef resolves any git ref to a commit SHA.
func ResolveGitRef(repoDir, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", ref+"^{commit}")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ref %q does not resolve to a commit", ref)
	}
	return strings.TrimSpace(string(out)), nil
}

// CreateAnnotatedTag creates an annotated git tag on a specific commit.
func CreateAnnotatedTag(repoDir, tag, targetSHA, message string) error {
	cmd := exec.Command("git", "tag", "-a", tag, targetSHA, "-m", message)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating tag %s: %s: %w", tag, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// PushTag pushes a tag to the given remote.
func PushTag(repoDir, remote, tag string) error {
	cmd := exec.Command("git", "push", remote, tag)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pushing tag %s: %s: %w", tag, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// tagExists checks if a git tag already exists.
func tagExists(repoDir, tag string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/tags/"+tag)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

// countCommits returns the number of commits in a range.
func countCommits(repoDir, from, to string) int {
	cmd := exec.Command("git", "rev-list", "--count", from+".."+to)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

type diffStatsResult struct {
	files      int
	insertions int
	deletions  int
}

// diffStats returns diff statistics for a range.
func diffStats(repoDir, from, to string) diffStatsResult {
	cmd := exec.Command("git", "diff", "--shortstat", from+".."+to)
	cmd.Dir = repoDir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return diffStatsResult{}
	}

	var result diffStatsResult
	line := strings.TrimSpace(stdout.String())
	// Parse: " 18 files changed, 421 insertions(+), 97 deletions(-)"
	fmt.Sscanf(line, "%d file", &result.files)
	if idx := strings.Index(line, "insertion"); idx > 0 {
		// Find the number before "insertion"
		sub := line[:idx]
		parts := strings.Fields(sub)
		if len(parts) > 0 {
			fmt.Sscanf(parts[len(parts)-1], "%d", &result.insertions)
		}
	}
	if idx := strings.Index(line, "deletion"); idx > 0 {
		sub := line[:idx]
		parts := strings.Fields(sub)
		if len(parts) > 0 {
			fmt.Sscanf(parts[len(parts)-1], "%d", &result.deletions)
		}
	}

	return result
}
