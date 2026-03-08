package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// GitHubForge implements the Forge interface for GitHub and GitHub Enterprise.
type GitHubForge struct {
	BaseURL string // "https://api.github.com" or "https://ghes.example.com/api/v3"
	Token   string
	Owner   string
	Repo    string
}

// NewGitHub creates a GitHub forge client.
// Token is resolved from env: GITHUB_TOKEN, GH_TOKEN.
// Owner/Repo is resolved from env: GITHUB_REPOSITORY (owner/repo).
func NewGitHub(baseURL string) *GitHubForge {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}

	var owner, repo string
	if ghRepo := os.Getenv("GITHUB_REPOSITORY"); ghRepo != "" {
		if idx := strings.Index(ghRepo, "/"); idx >= 0 {
			owner = ghRepo[:idx]
			repo = ghRepo[idx+1:]
		}
	}

	apiBase := "https://api.github.com"
	if baseURL != "" && !strings.Contains(baseURL, "github.com") {
		// GitHub Enterprise Server
		apiBase = strings.TrimRight(baseURL, "/") + "/api/v3"
	}

	return &GitHubForge{
		BaseURL: apiBase,
		Token:   token,
		Owner:   owner,
		Repo:    repo,
	}
}

func (g *GitHubForge) Provider() Provider { return GitHub }

func (g *GitHubForge) apiURL(path string) string {
	return fmt.Sprintf("%s/repos/%s/%s%s", g.BaseURL, g.Owner, g.Repo, path)
}

// uploadBaseURL returns the upload API base for asset uploads.
// github.com uses uploads.github.com; GHES uses {host}/api/uploads.
func (g *GitHubForge) uploadBaseURL() string {
	if strings.Contains(g.BaseURL, "api.github.com") {
		return "https://uploads.github.com"
	}
	return strings.Replace(g.BaseURL, "/api/v3", "/api/uploads", 1)
}

func (g *GitHubForge) doJSON(ctx context.Context, method, url string, body interface{}, result interface{}) error {
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
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
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

func (g *GitHubForge) CreateRelease(ctx context.Context, opts ReleaseOptions) (*Release, error) {
	payload := map[string]interface{}{
		"tag_name":   opts.TagName,
		"name":       opts.Name,
		"body":       opts.Description,
		"draft":      opts.Draft,
		"prerelease": opts.Prerelease,
	}
	if opts.Ref != "" {
		payload["target_commitish"] = opts.Ref
	}

	var resp struct {
		ID      int    `json:"id"`
		HTMLURL string `json:"html_url"`
	}

	err := g.doJSON(ctx, "POST", g.apiURL("/releases"), payload, &resp)
	if err != nil {
		return nil, err
	}

	return &Release{
		ID:  fmt.Sprintf("%d", resp.ID),
		URL: resp.HTMLURL,
	}, nil
}

func (g *GitHubForge) UploadAsset(ctx context.Context, releaseID string, asset Asset) error {
	f, err := os.Open(asset.FilePath)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}

	uploadURL := fmt.Sprintf("%s/repos/%s/%s/releases/%s/assets?name=%s",
		g.uploadBaseURL(), g.Owner, g.Repo, releaseID, asset.Name)

	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, f)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.ContentLength = stat.Size()

	mimeType := asset.MIMEType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(asset.FilePath))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}
	req.Header.Set("Content-Type", mimeType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub upload asset: %d %s", resp.StatusCode, string(body))
	}
	return nil
}

func (g *GitHubForge) AddReleaseLink(ctx context.Context, releaseID string, link ReleaseLink) error {
	// GitHub doesn't have release links like GitLab.
	// Append the link to the release body instead.
	var rel struct {
		Body string `json:"body"`
	}
	if err := g.doJSON(ctx, "GET", g.apiURL("/releases/"+releaseID), nil, &rel); err != nil {
		return err
	}

	linkLine := fmt.Sprintf("- [%s](%s)", link.Name, link.URL)
	body := rel.Body
	if !strings.Contains(body, "### Container Images") {
		body += "\n\n### Container Images\n"
	}
	body += linkLine + "\n"

	payload := map[string]string{"body": body}
	return g.doJSON(ctx, "PATCH", g.apiURL("/releases/"+releaseID), payload, nil)
}

func (g *GitHubForge) CommitFile(ctx context.Context, opts CommitFileOptions) error {
	fileURL := g.apiURL("/contents/" + opts.Path)

	// Get current file SHA (needed for updates, 404 means create new)
	var existing struct {
		SHA string `json:"sha"`
	}
	_ = g.doJSON(ctx, "GET", fileURL+"?ref="+opts.Branch, nil, &existing)

	payload := map[string]string{
		"message": opts.Message,
		"content": base64.StdEncoding.EncodeToString(opts.Content),
		"branch":  opts.Branch,
	}
	if existing.SHA != "" {
		payload["sha"] = existing.SHA
	}

	return g.doJSON(ctx, "PUT", fileURL, payload, nil)
}

func (g *GitHubForge) CommitFiles(ctx context.Context, opts CommitFilesOptions) (*CommitResult, error) {
	if len(opts.Files) == 0 {
		return nil, nil
	}

	// 1. Get current branch ref → commit SHA
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.doJSON(ctx, "GET", g.apiURL("/git/ref/heads/"+opts.Branch), nil, &ref); err != nil {
		return nil, fmt.Errorf("getting branch ref: %w", err)
	}

	// ExpectedSHA guard: compare against ref we just read (no extra API call)
	if opts.ExpectedSHA != "" && ref.Object.SHA != opts.ExpectedSHA {
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrBranchMoved, opts.ExpectedSHA, ref.Object.SHA)
	}

	// 2. Get commit → base tree SHA
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.doJSON(ctx, "GET", g.apiURL("/git/commits/"+ref.Object.SHA), nil, &commit); err != nil {
		return nil, fmt.Errorf("getting commit: %w", err)
	}

	// 3. Create new tree with file changes
	// Use map[string]interface{} entries to support JSON null for delete (sha: null)
	entries := make([]map[string]interface{}, 0, len(opts.Files))
	for _, f := range opts.Files {
		if f.Delete {
			entries = append(entries, map[string]interface{}{
				"path": f.Path,
				"mode": "100644",
				"type": "blob",
				"sha":  nil, // JSON null = delete
			})
		} else {
			entries = append(entries, map[string]interface{}{
				"path":    f.Path,
				"mode":    "100644",
				"type":    "blob",
				"content": string(f.Content),
			})
		}
	}

	var newTree struct {
		SHA string `json:"sha"`
	}
	treePayload := map[string]interface{}{
		"base_tree": commit.Tree.SHA,
		"tree":      entries,
	}
	if err := g.doJSON(ctx, "POST", g.apiURL("/git/trees"), treePayload, &newTree); err != nil {
		return nil, fmt.Errorf("creating tree: %w", err)
	}

	// 4. Create commit
	var newCommit struct {
		SHA string `json:"sha"`
	}
	commitPayload := map[string]interface{}{
		"message": opts.Message,
		"tree":    newTree.SHA,
		"parents": []string{ref.Object.SHA},
	}
	if err := g.doJSON(ctx, "POST", g.apiURL("/git/commits"), commitPayload, &newCommit); err != nil {
		return nil, fmt.Errorf("creating commit: %w", err)
	}

	// 5. Update branch ref
	refPayload := map[string]interface{}{
		"sha": newCommit.SHA,
	}
	if err := g.doJSON(ctx, "PATCH", g.apiURL("/git/refs/heads/"+opts.Branch), refPayload, nil); err != nil {
		if isConflict(err) {
			return nil, fmt.Errorf("%w: ref update rejected", ErrBranchMoved)
		}
		return nil, fmt.Errorf("updating branch ref: %w", err)
	}

	return &CommitResult{SHA: newCommit.SHA}, nil
}

func (g *GitHubForge) BranchHeadSHA(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.doJSON(ctx, "GET", g.apiURL("/git/ref/heads/"+branch), nil, &ref); err != nil {
		return "", fmt.Errorf("reading branch %s: %w", branch, err)
	}
	return ref.Object.SHA, nil
}

func (g *GitHubForge) CreateMR(ctx context.Context, opts MROptions) (*MR, error) {
	payload := map[string]interface{}{
		"title": opts.Title,
		"body":  opts.Description,
		"head":  opts.SourceBranch,
		"base":  opts.TargetBranch,
		"draft": opts.Draft,
	}

	var resp struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}

	err := g.doJSON(ctx, "POST", g.apiURL("/pulls"), payload, &resp)
	if err != nil {
		return nil, err
	}

	return &MR{
		ID:  fmt.Sprintf("%d", resp.Number),
		URL: resp.HTMLURL,
	}, nil
}

func (g *GitHubForge) ListReleases(ctx context.Context) ([]ReleaseInfo, error) {
	var all []ReleaseInfo
	page := 1

	for {
		url := fmt.Sprintf("%s?per_page=100&page=%d", g.apiURL("/releases"), page)

		var releases []struct {
			ID        int    `json:"id"`
			TagName   string `json:"tag_name"`
			CreatedAt string `json:"created_at"`
		}

		if err := g.doJSON(ctx, "GET", url, nil, &releases); err != nil {
			return all, err
		}

		for _, r := range releases {
			info := ReleaseInfo{
				ID:      fmt.Sprintf("%d", r.ID),
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

func (g *GitHubForge) DeleteRelease(ctx context.Context, tagName string) error {
	// GitHub requires the release ID, not tag name, for deletion.
	// Find the release ID from the tag.
	var rel struct {
		ID int `json:"id"`
	}
	if err := g.doJSON(ctx, "GET", g.apiURL("/releases/tags/"+tagName), nil, &rel); err != nil {
		return fmt.Errorf("finding release for tag %s: %w", tagName, err)
	}
	return g.doJSON(ctx, "DELETE", g.apiURL(fmt.Sprintf("/releases/%d", rel.ID)), nil, nil)
}
