package release

import (
	"fmt"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitstate"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return "", fmt.Errorf("opening repo: %w", err)
	}
	return gitstate.ResolveRef(repo, ref)
}

// CreateAnnotatedTag creates an annotated git tag on a specific commit.
func CreateAnnotatedTag(repoDir, tag, targetSHA, message string) error {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}

	// Resolve tagger identity from git config, fall back to stagefreight defaults
	taggerName := "stagefreight"
	taggerEmail := "stagefreight@localhost"
	if cfg, cfgErr := repo.Config(); cfgErr == nil {
		if cfg.User.Name != "" {
			taggerName = cfg.User.Name
		}
		if cfg.User.Email != "" {
			taggerEmail = cfg.User.Email
		}
	}

	hash := plumbing.NewHash(targetSHA)
	_, err = repo.CreateTag(tag, hash, &git.CreateTagOptions{
		Tagger: &object.Signature{
			Name:  taggerName,
			Email: taggerEmail,
			When:  time.Now(),
		},
		Message: message,
	})
	if err != nil {
		return fmt.Errorf("creating tag %s: %w", tag, err)
	}
	return nil
}

// PushTag pushes a tag to the given remote.
func PushTag(repoDir, remote, tag string) error {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}
	session, err := gitstate.OpenSyncSession(repoDir)
	if err != nil {
		return fmt.Errorf("opening sync session: %w", err)
	}

	refspec := gitconfig.RefSpec(fmt.Sprintf("refs/tags/%s:refs/tags/%s", tag, tag))
	err = repo.Push(&git.PushOptions{
		RemoteName: remote,
		RefSpecs:   []gitconfig.RefSpec{refspec},
		Auth:       session.Auth(),
	})
	if err == git.NoErrAlreadyUpToDate {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pushing tag %s: %w", tag, err)
	}
	return nil
}

// tagExists checks if a git tag already exists.
func tagExists(repoDir, tag string) bool {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return false
	}
	_, err = repo.Tag(tag)
	return err == nil
}

// countCommits returns the number of commits in a range.
func countCommits(repoDir, from, to string) int {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return 0
	}
	n, err := gitstate.CountCommitsBetween(repo, from, to)
	if err != nil {
		return 0
	}
	return n
}

type diffStatsResult struct {
	files      int
	insertions int
	deletions  int
}

// diffStats returns diff statistics for a range.
func diffStats(repoDir, from, to string) diffStatsResult {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return diffStatsResult{}
	}
	files, insertions, deletions, err := gitstate.DiffStats(repo, from, to)
	if err != nil {
		return diffStatsResult{}
	}
	return diffStatsResult{files: files, insertions: insertions, deletions: deletions}
}
