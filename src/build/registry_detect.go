package build

import "strings"

// DetectProvider determines the registry vendor from the URL.
// Well-known domains are matched directly. For unknown domains, returns "generic"
// (future: probe the registry API to identify the vendor).
func DetectProvider(registryURL string) string {
	host := strings.ToLower(registryURL)
	// Strip scheme if present
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	// Strip path
	if idx := strings.IndexByte(host, '/'); idx >= 0 {
		host = host[:idx]
	}

	switch {
	case host == "docker.io" || host == "registry-1.docker.io" || host == "index.docker.io":
		return "dockerhub"
	case host == "ghcr.io":
		return "ghcr"
	case host == "quay.io":
		return "quay"
	case strings.Contains(host, "gitlab"):
		return "gitlab"
	case strings.Contains(host, "jfrog") || strings.Contains(host, "artifactory") || strings.Contains(host, "jcr"):
		return "jfrog"
	case strings.Contains(host, "harbor"):
		return "harbor"
	case strings.HasSuffix(host, ".amazonaws.com") && strings.Contains(host, ".dkr.ecr."):
		return "ecr"
	case strings.HasSuffix(host, ".pkg.dev"):
		return "gar"
	case strings.HasSuffix(host, ".azurecr.io"):
		return "acr"
	default:
		return "generic"
	}
}
