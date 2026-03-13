package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// GitLabForge implements the Forge interface for GitLab instances.
type GitLabForge struct {
	BaseURL   string // e.g., "https://gitlab.prplanit.com"
	Token     string // private token or job token
	ProjectID string // numeric ID or "group/project" URL-encoded path
}

// NewGitLab creates a GitLab forge client.
// Token is resolved from env: GITLAB_TOKEN, CI_JOB_TOKEN.
// ProjectID is resolved from env: CI_PROJECT_ID, CI_PROJECT_PATH.
func NewGitLab(baseURL string) *GitLabForge {
	token := os.Getenv("GITLAB_TOKEN")
	if token == "" {
		token = os.Getenv("CI_JOB_TOKEN")
	}

	projectID := os.Getenv("CI_PROJECT_ID")
	if projectID == "" {
		projectID = os.Getenv("CI_PROJECT_PATH")
	}

	return &GitLabForge{
		BaseURL:   baseURL,
		Token:     token,
		ProjectID: projectID,
	}
}

func (g *GitLabForge) Provider() Provider { return GitLab }

func (g *GitLabForge) apiURL(path string) string {
	return fmt.Sprintf("%s/api/v4/projects/%s%s", g.BaseURL, url.PathEscape(g.ProjectID), path)
}

func (g *GitLabForge) doJSON(ctx context.Context, method, url string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return &APIError{Method: method, URL: url, StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	if result != nil {
		return json.Unmarshal(respBody, result)
	}
	return nil
}

func (g *GitLabForge) CreateRelease(ctx context.Context, opts ReleaseOptions) (*Release, error) {
	payload := map[string]interface{}{
		"tag_name":    opts.TagName,
		"name":        opts.Name,
		"description": opts.Description,
	}
	if opts.Ref != "" {
		payload["ref"] = opts.Ref
	}

	var resp struct {
		TagName string `json:"tag_name"`
		Links   struct {
			Self string `json:"self"`
		} `json:"_links"`
	}

	err := g.doJSON(ctx, "POST", g.apiURL("/releases"), payload, &resp)
	if err != nil {
		return nil, err
	}

	return &Release{
		ID:  resp.TagName,
		URL: fmt.Sprintf("%s/-/releases/%s", g.projectWebURL(), resp.TagName),
	}, nil
}

func (g *GitLabForge) UploadAsset(ctx context.Context, releaseID string, asset Asset) error {
	// GitLab: upload to project, then link to release
	uploadURL := g.apiURL("/uploads")

	f, err := os.Open(asset.FilePath)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filepath.Base(asset.FilePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var uploadResp struct {
		URL      string `json:"url"`
		Markdown string `json:"markdown"`
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GitLab upload: %d %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		return err
	}

	// Link the uploaded file to the release
	return g.AddReleaseLink(ctx, releaseID, ReleaseLink{
		Name:     asset.Name,
		URL:      g.BaseURL + uploadResp.URL,
		LinkType: "other",
	})
}

func (g *GitLabForge) AddReleaseLink(ctx context.Context, releaseID string, link ReleaseLink) error {
	payload := map[string]string{
		"name":      link.Name,
		"url":       link.URL,
		"link_type": link.LinkType,
	}
	linkURL := g.apiURL(fmt.Sprintf("/releases/%s/assets/links", url.PathEscape(releaseID)))
	return g.doJSON(ctx, "POST", linkURL, payload, nil)
}

func (g *GitLabForge) CommitFile(ctx context.Context, opts CommitFileOptions) error {
	payload := map[string]string{
		"branch":         opts.Branch,
		"content":        base64.StdEncoding.EncodeToString(opts.Content),
		"commit_message": opts.Message,
		"encoding":       "base64",
	}
	encodedPath := url.PathEscape(opts.Path)
	fileURL := g.apiURL(fmt.Sprintf("/repository/files/%s", encodedPath))

	// Try update first (PUT), fall back to create (POST) if file doesn't exist.
	err := g.doJSON(ctx, "PUT", fileURL, payload, nil)
	if err != nil {
		return g.doJSON(ctx, "POST", fileURL, payload, nil)
	}
	return nil
}

func (g *GitLabForge) CommitFiles(ctx context.Context, opts CommitFilesOptions) (*CommitResult, error) {
	if len(opts.Files) == 0 {
		return nil, nil
	}

	// ExpectedSHA preflight: best-effort stale-head check
	if opts.ExpectedSHA != "" {
		head, err := g.BranchHeadSHA(ctx, opts.Branch)
		if err != nil {
			return nil, fmt.Errorf("reading branch head: %w", err)
		}
		if head != opts.ExpectedSHA {
			return nil, fmt.Errorf("%w: expected %s, got %s", ErrBranchMoved, opts.ExpectedSHA, head)
		}
	}

	// Determine action per file: "delete", "update" if exists, "create" if not.
	type glAction struct {
		Action   string `json:"action"`
		FilePath string `json:"file_path"`
		Content  string `json:"content,omitempty"`
		Encoding string `json:"encoding,omitempty"`
	}

	actions := make([]glAction, 0, len(opts.Files))
	for _, f := range opts.Files {
		if f.Delete {
			actions = append(actions, glAction{
				Action:   "delete",
				FilePath: f.Path,
			})
			continue
		}
		action := "update"
		if !g.fileExists(ctx, f.Path, opts.Branch) {
			action = "create"
		}
		actions = append(actions, glAction{
			Action:   action,
			FilePath: f.Path,
			Content:  base64.StdEncoding.EncodeToString(f.Content),
			Encoding: "base64",
		})
	}

	payload := map[string]interface{}{
		"branch":         opts.Branch,
		"commit_message": opts.Message,
		"actions":        actions,
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := g.doJSON(ctx, "POST", g.apiURL("/repository/commits"), payload, &resp); err != nil {
		return nil, err
	}
	return &CommitResult{SHA: resp.ID}, nil
}

func (g *GitLabForge) BranchHeadSHA(ctx context.Context, branch string) (string, error) {
	var resp struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	branchURL := g.apiURL(fmt.Sprintf("/repository/branches/%s", url.PathEscape(branch)))
	if err := g.doJSON(ctx, "GET", branchURL, nil, &resp); err != nil {
		return "", fmt.Errorf("reading branch %s: %w", branch, err)
	}
	return resp.Commit.ID, nil
}

// fileExists checks whether a file exists on a branch via the repository files API.
func (g *GitLabForge) fileExists(ctx context.Context, path, branch string) bool {
	encodedPath := url.PathEscape(path)
	fileURL := g.apiURL(fmt.Sprintf("/repository/files/%s?ref=%s", encodedPath, url.QueryEscape(branch)))

	req, err := http.NewRequestWithContext(ctx, "HEAD", fileURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (g *GitLabForge) CreateMR(ctx context.Context, opts MROptions) (*MR, error) {
	payload := map[string]interface{}{
		"title":         opts.Title,
		"description":   opts.Description,
		"source_branch": opts.SourceBranch,
		"target_branch": opts.TargetBranch,
	}
	if opts.Draft {
		payload["title"] = "Draft: " + opts.Title
	}

	var resp struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}

	err := g.doJSON(ctx, "POST", g.apiURL("/merge_requests"), payload, &resp)
	if err != nil {
		return nil, err
	}

	return &MR{
		ID:  fmt.Sprintf("%d", resp.IID),
		URL: resp.WebURL,
	}, nil
}

func (g *GitLabForge) CancelPipeline(ctx context.Context, pipelineID string) error {
	cancelURL := g.apiURL(fmt.Sprintf("/pipelines/%s/cancel", pipelineID))
	return g.doJSON(ctx, "POST", cancelURL, nil, nil)
}

func (g *GitLabForge) ListReleases(ctx context.Context) ([]ReleaseInfo, error) {
	var all []ReleaseInfo
	page := 1

	for {
		url := fmt.Sprintf("%s?per_page=100&page=%d&order_by=released_at&sort=desc", g.apiURL("/releases"), page)

		var releases []struct {
			TagName   string `json:"tag_name"`
			CreatedAt string `json:"created_at"`
		}

		if err := g.doJSON(ctx, "GET", url, nil, &releases); err != nil {
			return all, err
		}

		for _, r := range releases {
			info := ReleaseInfo{
				ID:      r.TagName,
				TagName: r.TagName,
			}
			if t, err := parseTime(r.CreatedAt); err == nil {
				info.CreatedAt = t
			}
			all = append(all, info)
		}

		if len(releases) < 100 {
			break
		}
		page++
	}

	return all, nil
}

func (g *GitLabForge) DeleteRelease(ctx context.Context, tagName string) error {
	releaseURL := g.apiURL(fmt.Sprintf("/releases/%s", url.PathEscape(tagName)))
	return g.doJSON(ctx, "DELETE", releaseURL, nil, nil)
}

func (g *GitLabForge) projectWebURL() string {
	// CI_PROJECT_PATH is already "group/project", just join with base
	return fmt.Sprintf("%s/%s", g.BaseURL, g.ProjectID)
}
