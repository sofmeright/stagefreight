package ci

import (
	"os"
	"os/exec"
	"strings"
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

	// Local fallbacks via git inspection
	if ctx.SHA == "" {
		ctx.SHA = gitOutput("rev-parse", "HEAD")
	}
	if ctx.Branch == "" && ctx.Tag == "" {
		ctx.Branch = gitOutput("rev-parse", "--abbrev-ref", "HEAD")
		if ctx.Branch == "HEAD" {
			// Detached HEAD — check for tag
			ctx.Branch = ""
			ctx.Tag = gitOutput("describe", "--tags", "--exact-match", "HEAD")
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

// gitOutput runs a git command and returns trimmed stdout, or empty string on error.
func gitOutput(args ...string) string {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
