package gitver

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// ProjectMeta holds project-level metadata resolved from git and filesystem.
type ProjectMeta struct {
	Name     string // repo name (last path component of git remote)
	URL      string // repo URL (git remote origin)
	License  string // SPDX identifier from LICENSE file
	Language string // auto-detected from lockfiles
}

// projectDescription is set by callers to provide {project.description} from config.
var projectDescription string

// SetProjectDescription sets the project description for {project.description} template resolution.
// Call this before ResolveTemplateWithDir to inject config-sourced descriptions.
func SetProjectDescription(desc string) {
	projectDescription = desc
}

// DetectProject resolves project metadata from git remote, LICENSE file, and lockfiles.
func DetectProject(rootDir string) *ProjectMeta {
	pm := &ProjectMeta{}

	// Name and URL from git remote origin
	if repo, err := gitstate.OpenRepo(rootDir); err == nil {
		if remote, err := gitstate.RemoteURL(repo, "origin"); err == nil && remote != "" {
			pm.URL = remoteToHTTPS(remote)
			pm.Name = repoNameFromRemote(remote)
		}
	}

	// License from LICENSE file
	pm.License = detectLicense(rootDir)

	// Language from lockfiles
	pm.Language = detectLanguage(rootDir)

	return pm
}

// repoNameFromRemote extracts the repository name from a git remote URL.
// Handles SSH (git@host:org/repo.git) and HTTPS (https://host/org/repo.git).
func repoNameFromRemote(remote string) string {
	remote = strings.TrimSuffix(remote, ".git")

	// SSH: git@host:org/repo
	if idx := strings.LastIndex(remote, ":"); idx != -1 && !strings.Contains(remote, "://") {
		remote = remote[idx+1:]
	}

	// Last path component
	if idx := strings.LastIndex(remote, "/"); idx != -1 {
		return remote[idx+1:]
	}
	return remote
}

// remoteToHTTPS converts a git remote URL to HTTPS format for display.
// SSH remotes (git@host:org/repo.git) become https://host/org/repo.
// HTTPS remotes pass through with .git stripped.
func remoteToHTTPS(remote string) string {
	remote = strings.TrimSuffix(remote, ".git")

	// Already HTTPS
	if strings.HasPrefix(remote, "https://") || strings.HasPrefix(remote, "http://") {
		return remote
	}

	// SSH: git@host:org/repo → https://host/org/repo
	if idx := strings.Index(remote, "@"); idx != -1 {
		rest := remote[idx+1:]
		rest = strings.Replace(rest, ":", "/", 1)
		return "https://" + rest
	}

	return remote
}

// detectLicense reads the LICENSE file and returns an SPDX identifier.
func detectLicense(rootDir string) string {
	names := []string{
		"LICENSE", "LICENSE.md", "LICENSE.txt",
		"LICENCE", "LICENCE.md", "LICENCE.txt",
		"COPYING", "COPYING.md",
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(rootDir, name))
		if err != nil {
			continue
		}
		if id := matchLicense(string(data)); id != "" {
			return id
		}
	}
	return ""
}

// matchLicense identifies an SPDX identifier from license text content.
func matchLicense(text string) string {
	lower := strings.ToLower(text)

	switch {
	case strings.Contains(lower, "gnu affero general public license") && strings.Contains(lower, "version 3"):
		return "AGPL-3.0"
	case strings.Contains(lower, "gnu general public license") && strings.Contains(lower, "version 3"):
		return "GPL-3.0"
	case strings.Contains(lower, "gnu general public license") && strings.Contains(lower, "version 2"):
		return "GPL-2.0"
	case strings.Contains(lower, "gnu lesser general public license") && strings.Contains(lower, "version 3"):
		return "LGPL-3.0"
	case strings.Contains(lower, "gnu lesser general public license") && strings.Contains(lower, "version 2"):
		return "LGPL-2.1"
	case strings.Contains(lower, "apache license") && strings.Contains(lower, "version 2.0"):
		return "Apache-2.0"
	case strings.Contains(lower, "mit license"),
		strings.Contains(lower, "permission is hereby granted") && strings.Contains(lower, "the software"):
		return "MIT"
	case strings.Contains(lower, "bsd 3-clause"),
		strings.Contains(lower, "redistribution and use") && strings.Contains(lower, "neither the name"):
		return "BSD-3-Clause"
	case strings.Contains(lower, "bsd 2-clause"),
		strings.Contains(lower, "redistribution and use") && !strings.Contains(lower, "neither the name") && !strings.Contains(lower, "gnu"):
		return "BSD-2-Clause"
	case strings.Contains(lower, "mozilla public license") && strings.Contains(lower, "2.0"):
		return "MPL-2.0"
	case strings.Contains(lower, "isc license"):
		return "ISC"
	case strings.Contains(lower, "the unlicense"):
		return "Unlicense"
	case strings.Contains(lower, "creative commons") && strings.Contains(lower, "attribution 4.0"):
		return "CC-BY-4.0"
	}
	return ""
}

// detectLanguage identifies the primary programming language from lockfiles/manifests.
func detectLanguage(rootDir string) string {
	indicators := map[string]string{
		"go.mod":            "go",
		"Cargo.toml":        "rust",
		"package.json":      "node",
		"package-lock.json": "node",
		"yarn.lock":         "node",
		"pnpm-lock.yaml":    "node",
		"bun.lockb":         "node",
		"requirements.txt":  "python",
		"Pipfile":           "python",
		"pyproject.toml":    "python",
		"Gemfile":           "ruby",
		"composer.json":     "php",
	}

	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if lang, ok := indicators[entry.Name()]; ok {
			return lang
		}
	}
	return ""
}
