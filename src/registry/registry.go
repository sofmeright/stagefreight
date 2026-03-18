// Package registry provides a platform-agnostic abstraction over container
// registries (Docker Hub, GitLab, GHCR, Quay, JFrog, Harbor, Gitea).
// Every registry operation (list tags, delete tags) goes through the Registry
// interface so StageFreight's retention engine works identically regardless
// of where images are hosted.
package registry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/credentials"
	"github.com/PrPlanIT/StageFreight/src/diag"
)

// Registry is the interface every container registry provider implements.
type Registry interface {
	// Provider returns the registry vendor name.
	Provider() string

	// ListTags returns all tags for a repository, sorted by creation time descending.
	ListTags(ctx context.Context, repo string) ([]TagInfo, error)

	// DeleteTag removes a single tag from a repository.
	DeleteTag(ctx context.Context, repo string, tag string) error

	// UpdateDescription pushes short and full descriptions to the registry.
	// Returns nil for providers that don't support description APIs.
	UpdateDescription(ctx context.Context, repo, short, full string) error
}

// Warner is an optional interface for registries that emit warnings
// (e.g., credential hygiene nudges). Callers can type-assert to check.
type Warner interface {
	Warnings() []string
}

// TagInfo describes a single tag in a container registry.
type TagInfo struct {
	Name      string
	Digest    string
	CreatedAt time.Time
}

// NormalizeProvider maps provider aliases to their canonical platform names.
// Canonical names are the platform brand: docker, github, gitlab, quay, jfrog, harbor, gitea.
// Legacy aliases (dockerhub, ghcr) are accepted and mapped to canonical forms.
func NormalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "dockerhub":
		return "docker"
	case "ghcr":
		return "github"
	default:
		return p
	}
}

// NewRegistry creates a registry client for the given provider.
// Credentials are resolved via credentials.ResolvePrefix — see that package
// for the full resolution order (_TOKEN → _PASS → _PASSWORD).
// The registryURL is the base URL (e.g., "docker.io", "ghcr.io").
func NewRegistry(provider, registryURL, credentialPrefix string) (Registry, error) {
	provider = NormalizeProvider(provider)
	cred := credentials.ResolvePrefix(credentialPrefix)
	if cred.Kind == credentials.SecretPassword {
		diag.Warn("credentials %s: authenticating with %s — consider using %s_TOKEN instead (scoped, revocable)",
			credentialPrefix, cred.SecretEnv, strings.ToUpper(credentialPrefix))
	}

	switch provider {
	case "local":
		return NewLocal(), nil
	case "docker":
		return NewDockerHub(cred.User, cred.Secret), nil
	case "gitlab":
		return NewGitLab(registryURL, cred.User, cred.Secret), nil
	case "github":
		return NewGHCR(cred.User, cred.Secret), nil
	case "quay":
		return NewQuay(registryURL, cred.User, cred.Secret), nil
	case "jfrog":
		return NewJFrog(registryURL, cred.User, cred.Secret), nil
	case "harbor":
		return NewHarbor(registryURL, cred.User, cred.Secret), nil
	case "gitea", "forgejo":
		return NewGitea(registryURL, cred.User, cred.Secret), nil
	default:
		return nil, fmt.Errorf("registry: unsupported provider %q (valid: docker, github, gitlab, quay, jfrog, harbor, gitea, forgejo)", provider)
	}
}
