package freshness

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/lint"
)

// githubReleaseLatest is the response from GitHub's releases/latest endpoint.
type githubReleaseLatest struct {
	TagName string `json:"tag_name"`
}

// checkTools resolves pinned tool versions (ENV *_VERSION + GitHub URLs).
func (m *freshnessModule) checkTools(ctx context.Context, file lint.FileInfo, tools []pinnedTool) []Dependency {
	// Enrich tools with GitHub owner/repo by scanning for release URLs.
	ownerRepoMap := scanGitHubURLs(file.AbsPath)

	var deps []Dependency
	for _, tool := range tools {
		owner, repo := matchToolToGitHub(tool.EnvName, ownerRepoMap)
		if owner == "" || repo == "" {
			continue
		}

		dep := Dependency{
			Name:      repo,
			Current:   strings.TrimPrefix(tool.Version, "v"),
			Ecosystem: EcosystemGitHubRelease,
			File:      file.Path,
			Line:      tool.Line,
		}

		ep := m.cfg.Registries.GitHub
		baseURL := m.cfg.registryURL(EcosystemGitHubRelease, "https://api.github.com")
		url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", strings.TrimRight(baseURL, "/"), owner, repo)
		dep.SourceURL = url

		var release githubReleaseLatest
		if err := m.http.fetchJSON(ctx, url, &release, ep); err != nil {
			deps = append(deps, dep)
			continue
		}

		if release.TagName != "" {
			dep.Latest = strings.TrimPrefix(release.TagName, "v")
		}
		deps = append(deps, dep)
	}

	return deps
}

// scanGitHubURLs reads a file looking for github.com/{owner}/{repo}/releases/download/
// patterns and returns a map of "owner/repo" → true.
func scanGitHubURLs(absPath string) map[string]bool {
	result := make(map[string]bool)

	f, err := os.Open(absPath)
	if err != nil {
		return result
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		matches := githubReleaseRe.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			key := m[1] + "/" + m[2]
			result[key] = true
		}
	}

	return result
}

// matchToolToGitHub attempts to find the GitHub owner/repo for a tool
// by matching the ENV variable name to known patterns.
// E.g. BUILDX_VERSION → docker/buildx, TRIVY_VERSION → aquasecurity/trivy.
func matchToolToGitHub(envName string, urlMap map[string]bool) (string, string) {
	// Direct match: if we found GitHub URLs, try to match by tool name.
	lower := strings.ToLower(strings.TrimSuffix(envName, "_VERSION"))
	lower = strings.TrimSuffix(lower, "_ver")

	for key := range urlMap {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			continue
		}
		repoLower := strings.ToLower(parts[1])
		// Match if repo name contains the tool name or vice versa.
		if repoLower == lower || strings.Contains(repoLower, lower) || strings.Contains(lower, repoLower) {
			return parts[0], parts[1]
		}
	}

	return "", ""
}

// checkToolsFromDockerfile is the entry point called from checkDockerfile.
func (m *freshnessModule) checkToolsFromDockerfile(ctx context.Context, file lint.FileInfo, dfInfo *DockerFreshnessInfo) []Dependency {
	if !m.cfg.sourceEnabled(EcosystemGitHubRelease) {
		return nil
	}
	return m.checkTools(ctx, file, dfInfo.PinnedTools)
}
