package ci

import (
	"os"

	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/gitstate"
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
// Falls back to git inspection for local (non-CI) runs.
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

	// Local fallbacks via go-git inspection (no git binary required).
	// Only runs when SF_CI_* env vars are not set (local dev, not CI).
	if ctx.SHA == "" || (ctx.Branch == "" && ctx.Tag == "") {
		repo, repoErr := gitstate.OpenRepo(ctx.Workspace)
		if repoErr != nil {
			diag.Debug(true, "ci context: could not open repo at %s: %v", ctx.Workspace, repoErr)
		} else {
			head, headErr := repo.Head()
			if headErr != nil {
				diag.Debug(true, "ci context: could not resolve HEAD: %v", headErr)
			} else {
				if ctx.SHA == "" {
					ctx.SHA = head.Hash().String()
				}
				if ctx.Branch == "" && ctx.Tag == "" {
					if head.Name().IsBranch() {
						ctx.Branch = head.Name().Short()
					} else {
						// Detached HEAD — check if at a tag
						if tag, tagErr := gitstate.ExactTagAtHEAD(repo); tagErr != nil {
							diag.Debug(true, "ci context: could not resolve tag at HEAD: %v", tagErr)
						} else {
							ctx.Tag = tag
						}
					}
				}
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

