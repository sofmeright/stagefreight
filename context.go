package ci

import (
	"os"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
	git "github.com/go-git/go-git/v5"
)

// CIContext holds normalized CI environment information.
// Provider skeletons translate forge-native vars into SF_CI_* env vars;
// CIContext reads those to provide a provider-neutral execution context.
type CIContext struct {
	Provider      string // gitlab, github, gitea, forgejo, jenkins
	Event         string // push, tag, merge_request, schedule
	Branch        string // current branch (empty on tags)
	Tag           string // current tag (empty on branches)
	SHA           string // full commit SHA
	DefaultBranch string // repo default branch name
	RepoURL       string // repository URL
	Workspace     string // working directory
	PipelineID    string // provider pipeline/run ID (for cancel API)
}

// IsCI returns true when running in a CI environment (SF_CI_PROVIDER is set).
func (c *CIContext) IsCI() bool {
	return c.Provider != ""
}

// IsBranch returns true when the current context is a branch build (not a tag).
func (c *CIContext) IsBranch() bool {
	return c.Branch != "" && c.Tag == ""
}

// IsTag returns true when the current context is a tag build.
func (c *CIContext) IsTag() bool {
	return c.Tag != ""
}

// ResolveContext reads SF_CI_* env vars to build a CIContext.
// Falls back to go-git inspection for local (non-CI) runs.
func ResolveContext() *CIContext {
	ctx := &CIContext{
		Provider:      os.Getenv("SF_CI_PROVIDER"),
		Event:         os.Getenv("SF_CI_EVENT"),
		Branch:        os.Getenv("SF_CI_BRANCH"),
		Tag:           os.Getenv("SF_CI_TAG"),
		SHA:           os.Getenv("SF_CI_SHA"),
		DefaultBranch: os.Getenv("SF_CI_DEFAULT_BRANCH"),
		RepoURL:       os.Getenv("SF_CI_REPO_URL"),
		Workspace:     os.Getenv("SF_CI_WORKSPACE"),
		PipelineID:    os.Getenv("SF_CI_PIPELINE_ID"),
	}

	// Local fallbacks via go-git inspection
	if ctx.SHA == "" || (ctx.Branch == "" && ctx.Tag == "") {
		cwd, cwdErr := os.Getwd()
		if cwdErr == nil {
			if repo, err := gitstate.OpenRepo(cwd); err == nil {
				fillFromRepo(ctx, repo)
			}
		}
	}

	if ctx.DefaultBranch == "" {
		ctx.DefaultBranch = "main"
	}
	if ctx.Workspace == "" {
		ctx.Workspace, _ = os.Getwd()
	}
	if ctx.Event == "" && ctx.Tag != "" {
		ctx.Event = "tag"
	} else if ctx.Event == "" {
		ctx.Event = "push"
	}

	return ctx
}

// fillFromRepo populates missing CIContext fields from the local repo.
func fillFromRepo(ctx *CIContext, repo *git.Repository) {
	head, err := repo.Head()
	if err != nil {
		return
	}

	if ctx.SHA == "" {
		ctx.SHA = head.Hash().String()
	}

	if ctx.Branch == "" && ctx.Tag == "" {
		if head.Name().IsBranch() {
			ctx.Branch = head.Name().Short()
		} else {
			// Detached HEAD — check for tag at this commit
			if tag, tagErr := gitstate.ExactTagAtHEAD(repo); tagErr == nil && tag != "" {
				ctx.Tag = tag
			}
		}
	}
}
