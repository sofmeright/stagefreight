// Package forge provides a platform-agnostic abstraction over git forges
// (GitLab, GitHub, Gitea/Forgejo). Every write operation (release creation,
// badge update, file commit, MR/PR creation) goes through this interface
// so StageFreight works identically regardless of where the repo is hosted.
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/credentials"
)

// ErrBranchMoved is returned when the target branch has moved since the expected SHA.
var ErrBranchMoved = errors.New("target branch moved during commit")

// ErrNotSupported is returned when a forge does not support a particular operation.
var ErrNotSupported = errors.New("operation not supported by this forge")

// CommitResult holds the result of a forge commit operation.
type CommitResult struct {
	SHA string
}

// APIError is returned by doJSON when the API returns a non-2xx status.
type APIError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.StatusCode, e.Body)
}

// isConflict checks whether an error is an API conflict (409) or validation error (422).
func isConflict(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 409 || apiErr.StatusCode == 422
	}
	return false
}

// Provider identifies a git forge platform.
type Provider string

const (
	GitLab  Provider = "gitlab"
	GitHub  Provider = "github"
	Gitea   Provider = "gitea"
	Unknown Provider = "unknown"
)

// Forge is the interface every platform implements.
type Forge interface {
	// Provider returns which platform this forge represents.
	Provider() Provider

	// CreateRelease creates a release/tag on the forge.
	CreateRelease(ctx context.Context, opts ReleaseOptions) (*Release, error)

	// UploadAsset attaches a file to an existing release.
	UploadAsset(ctx context.Context, releaseID string, asset Asset) error

	// AddReleaseLink adds a URL link to a release (e.g., registry image links).
	AddReleaseLink(ctx context.Context, releaseID string, link ReleaseLink) error

	// CommitFile creates or updates a file in the repo via the forge API.
	// Used for badge SVG updates without a local clone.
	CommitFile(ctx context.Context, opts CommitFileOptions) error

	// CommitFiles creates or updates multiple files in a single atomic commit
	// via the forge API. Used by the commit subsystem for CI push.
	CommitFiles(ctx context.Context, opts CommitFilesOptions) (*CommitResult, error)

	// BranchHeadSHA returns the current HEAD SHA of a branch via the forge API.
	BranchHeadSHA(ctx context.Context, branch string) (string, error)

	// CreateMR opens a merge/pull request.
	CreateMR(ctx context.Context, opts MROptions) (*MR, error)

	// CancelPipeline cancels the currently running pipeline (best-effort cleanup
	// after deps pushes a repaired commit). Returns nil if the provider doesn't
	// support pipeline cancellation.
	CancelPipeline(ctx context.Context, pipelineID string) error

	// ListReleases returns all releases, newest first.
	ListReleases(ctx context.Context) ([]ReleaseInfo, error)

	// DeleteRelease removes a release by its tag name.
	DeleteRelease(ctx context.Context, tagName string) error

	// CreateTag creates a lightweight git tag pointing at a ref.
	// Used for rolling tag aliases (e.g., "latest") that are not releases.
	CreateTag(ctx context.Context, tagName, ref string) error

	// DeleteTag deletes a git tag.
	DeleteTag(ctx context.Context, tagName string) error

	// DownloadJobArtifact fetches a single file from the latest successful job's
	// artifacts for the given ref. Returns the raw file bytes.
	// Returns os.ErrNotExist (or equivalent) if no artifacts found.
	// Implementations may return ErrNotSupported if the forge doesn't support this.
	DownloadJobArtifact(ctx context.Context, ref, jobName, artifactPath string) ([]byte, error)

	// GetFileContent reads a file from the repo at the given ref.
	// Returns the raw file content. Returns os.ErrNotExist if file is not found.
	// Used by governance drift detection, asset sync, and repo inspection.
	GetFileContent(ctx context.Context, path, ref string) ([]byte, error)

	// DefaultBranch returns the default branch name for the repo.
	DefaultBranch(ctx context.Context) (string, error)
}

// Factory creates Forge instances for target repos.
// Used by governance and asset distribution for cross-repo operations.
// Centralizes provider selection and credential resolution.
type Factory interface {
	ForRepo(ctx context.Context, repo string) (Forge, error)
}

// BasicFactory creates Forge instances for repos on a single forge platform.
// Used by governance reconciler for cross-repo operations.
type BasicFactory struct {
	ProviderName string // "gitlab", "github", "gitea"
	BaseURL      string // e.g., "https://gitlab.prplanit.com"
	CredPrefix   string // credential env var prefix
}

// ForRepo creates a Forge client for the given repo (e.g., "PrPlanIT/HASteward").
func (f *BasicFactory) ForRepo(ctx context.Context, repo string) (Forge, error) {
	return NewFromAccessory(f.ProviderName, f.BaseURL, repo, f.CredPrefix)
}

// ReleaseOptions configures a new release.
type ReleaseOptions struct {
	TagName     string
	Ref         string // commit SHA, branch, or tag to create the release from (required by GitLab when tag doesn't exist)
	Name        string
	Description string // markdown body (release notes)
	Draft       bool
	Prerelease  bool
}

// Release is a created release on a forge.
type Release struct {
	ID  string // platform-specific ID
	URL string // web URL to the release page
}

// Asset is a file to attach to a release.
type Asset struct {
	Name     string // display name
	FilePath string // local file to upload
	MIMEType string // e.g., "application/json"
}

// ReleaseLink is a URL to embed in a release (e.g., registry image link).
type ReleaseLink struct {
	Name     string // display name (e.g., "Docker Hub 1.3.0")
	URL      string // target URL
	LinkType string // "image", "package", "other"
}

// CommitFileOptions configures a file commit via forge API.
type CommitFileOptions struct {
	Branch  string
	Path    string // file path in repo
	Content []byte
	Message string
}

// CommitFilesOptions configures a multi-file atomic commit via forge API.
type CommitFilesOptions struct {
	Branch      string
	Message     string
	Files       []FileAction
	ExpectedSHA string // optional: fail if branch head != this SHA
}

// FileAction describes a single file operation within a multi-file commit.
type FileAction struct {
	Path    string
	Content []byte // nil for deletes
	Delete  bool   // true = delete this file
}

// MROptions configures a merge/pull request.
type MROptions struct {
	Title        string
	Description  string
	SourceBranch string
	TargetBranch string
	Draft        bool
}

// MR is a created merge/pull request.
type MR struct {
	ID  string
	URL string
}

// ReleaseInfo describes an existing release on a forge.
type ReleaseInfo struct {
	ID        string    // platform-specific ID (numeric for GitHub/Gitea, tag_name for GitLab)
	TagName   string
	CreatedAt time.Time
}

// NewFromAccessory creates a forge client from an accessory config.
// This is a config adapter only — no orchestration or sync logic.
// Uses credentials.ResolvePrefix for token resolution — same model as
// the mirror layer and all other StageFreight credential consumers.
func NewFromAccessory(provider, baseURL, projectID, credPrefix string) (Forge, error) {
	creds := credentials.ResolvePrefix(credPrefix)
	token := creds.Secret
	if token == "" {
		return nil, fmt.Errorf("accessory: no secret resolved for credentials prefix %q", credPrefix)
	}

	switch Provider(provider) {
	case GitLab:
		gl := NewGitLab(baseURL)
		gl.Token = token
		if projectID != "" {
			gl.ProjectID = projectID
		}
		return gl, nil
	case GitHub:
		gh := NewGitHub(baseURL)
		gh.Token = token
		if projectID != "" {
			owner, repo, err := splitOwnerRepo(projectID)
			if err != nil {
				return nil, fmt.Errorf("accessory %s (%s): %w", provider, projectID, err)
			}
			gh.Owner = owner
			gh.Repo = repo
		}
		return gh, nil
	case Gitea:
		gt := NewGitea(baseURL)
		gt.Token = token
		if projectID != "" {
			owner, repo, err := splitOwnerRepo(projectID)
			if err != nil {
				return nil, fmt.Errorf("accessory %s (%s): %w", provider, projectID, err)
			}
			gt.Owner = owner
			gt.Repo = repo
		}
		return gt, nil
	default:
		return nil, fmt.Errorf("accessory: unknown provider %q", provider)
	}
}

// splitOwnerRepo splits "owner/repo" into (owner, repo).
// Returns an error if the input is not a valid owner/repo pair.
func splitOwnerRepo(path string) (string, string, error) {
	idx := strings.IndexByte(path, '/')
	if idx <= 0 || idx == len(path)-1 {
		return "", "", fmt.Errorf("invalid project_id %q: expected owner/repo format", path)
	}
	owner := path[:idx]
	repo := path[idx+1:]
	// Strip any trailing path segments (e.g., "owner/repo/extra")
	if slash := strings.IndexByte(repo, '/'); slash >= 0 {
		repo = repo[:slash]
	}
	return owner, repo, nil
}
