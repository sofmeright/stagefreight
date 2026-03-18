package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Harbor implements the Registry interface for Harbor v2 container registry.
// Uses the Harbor REST API v2.0 (/api/v2.0/projects/:project/repositories/:repo/artifacts).
// Requires a user with push+delete permissions on the target project.
type Harbor struct {
	client  httpClient
	baseURL string
}

func NewHarbor(registryURL, user, pass string) *Harbor {
	base := normalizeURL(registryURL)

	headers := map[string]string{}
	if user != "" && pass != "" {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}

	return &Harbor{
		client: httpClient{
			base:    base,
			headers: headers,
		},
		baseURL: base,
	}
}

func (h *Harbor) Provider() string { return "harbor" }

func (h *Harbor) ListTags(ctx context.Context, repo string) ([]TagInfo, error) {
	// repo format: "project/repository" (e.g., "library/nginx")
	project, repoName := splitHarborRepo(repo)

	var allTags []TagInfo
	page := 1

	for {
		var artifacts []struct {
			Digest    string    `json:"digest"`
			PushTime  time.Time `json:"push_time"`
			PullTime  time.Time `json:"pull_time"`
			Tags      []struct {
				Name     string    `json:"name"`
				PushTime time.Time `json:"push_time"`
			} `json:"tags"`
		}

		apiURL := fmt.Sprintf("%s/api/v2.0/projects/%s/repositories/%s/artifacts?page=%d&page_size=100&with_tag=true",
			h.baseURL, url.PathEscape(project), url.PathEscape(repoName), page)

		_, err := h.client.doJSON(ctx, "GET", apiURL, nil, &artifacts)
		if err != nil {
			return nil, fmt.Errorf("harbor: listing artifacts for %s: %w", repo, err)
		}

		if len(artifacts) == 0 {
			break
		}

		for _, a := range artifacts {
			for _, t := range a.Tags {
				created := t.PushTime
				if created.IsZero() {
					created = a.PushTime
				}
				allTags = append(allTags, TagInfo{
					Name:      t.Name,
					Digest:    a.Digest,
					CreatedAt: created,
				})
			}
		}

		page++
	}

	return allTags, nil
}

func (h *Harbor) DeleteTag(ctx context.Context, repo string, tag string) error {
	project, repoName := splitHarborRepo(repo)

	apiURL := fmt.Sprintf("%s/api/v2.0/projects/%s/repositories/%s/artifacts/%s/tags/%s",
		h.baseURL, url.PathEscape(project), url.PathEscape(repoName),
		url.PathEscape(tag), url.PathEscape(tag))

	_, err := h.client.doJSON(ctx, "DELETE", apiURL, nil, nil)
	if err != nil {
		return fmt.Errorf("harbor: deleting tag %s in %s: %w", tag, repo, err)
	}
	return nil
}

func (h *Harbor) UpdateDescription(ctx context.Context, repo, short, full string) error {
	project, repoName := splitHarborRepo(repo)

	// Harbor has one description field per repository (no separate short/full).
	// Prefer full markdown; fall back to short.
	desc := full
	if desc == "" {
		desc = short
	}

	payload := map[string]interface{}{
		"description": desc,
	}

	apiURL := fmt.Sprintf("%s/api/v2.0/projects/%s/repositories/%s",
		h.baseURL, url.PathEscape(project), url.PathEscape(repoName))
	_, err := h.client.doJSON(ctx, "PUT", apiURL, payload, nil)
	if err != nil {
		return fmt.Errorf("harbor: updating description for %s: %w", repo, err)
	}
	return nil
}

// TriggerScan initiates a vulnerability scan on a pushed artifact via Harbor's built-in Trivy.
// reference is a tag or digest. Best-effort — callers warn on error rather than failing the build.
func (h *Harbor) TriggerScan(ctx context.Context, repo, reference string) error {
	project, repoName := splitHarborRepo(repo)

	apiURL := fmt.Sprintf("%s/api/v2.0/projects/%s/repositories/%s/artifacts/%s/scan",
		h.baseURL, url.PathEscape(project), url.PathEscape(repoName), url.PathEscape(reference))
	_, err := h.client.doJSON(ctx, "POST", apiURL, nil, nil)
	if err != nil {
		return fmt.Errorf("harbor: triggering scan for %s@%s: %w", repo, reference, err)
	}
	return nil
}

// EnsureProject creates the Harbor project if it does not already exist.
// 409 Conflict is treated as success (project already exists).
// Returns raw-wrapped HTTP errors — callers add credential prefix context.
func (h *Harbor) EnsureProject(ctx context.Context, projectName string) error {
	payload := map[string]interface{}{
		"project_name": projectName,
		"public":       false,
	}
	apiURL := fmt.Sprintf("%s/api/v2.0/projects", h.baseURL)
	_, err := h.client.doJSON(ctx, "POST", apiURL, payload, nil)
	if err == nil {
		return nil
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == 409 {
		return nil // already exists
	}
	return fmt.Errorf("ensuring project %q: %w", projectName, err)
}

// splitHarborRepo splits "project/repo" into project and repository name.
// Handles nested repos like "project/sub/repo" → project="project", repo="sub/repo".
func splitHarborRepo(repo string) (project, repoName string) {
	idx := strings.IndexByte(repo, '/')
	if idx < 0 {
		return "library", repo
	}
	return repo[:idx], repo[idx+1:]
}
