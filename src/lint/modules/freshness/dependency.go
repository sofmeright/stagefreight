package freshness

// Dependency is a version-pinned reference extracted from a project file.
// It is the bridge type consumed by both lint reporting and future update
// commands (à la Renovate managers).
type Dependency struct {
	Name      string // e.g. "golang", "github.com/spf13/cobra", "react"
	Current   string // currently pinned version string
	Latest    string // latest available (filled by resolver)
	Ecosystem string // one of the Ecosystem* constants below
	File      string // relative path from repo root
	Line      int    // line number of the pinned version
	Indirect  bool   // e.g. go.mod // indirect
	SourceURL string // registry/API URL that was queried

	// Vulnerability info populated by the OSV correlation pass.
	Vulnerabilities []VulnInfo // known CVEs affecting the current version

	// Advisory is an optional informational message set by the resolver
	// when a non-versioned or pre-release tag has stable releases available.
	Advisory string

	// Fields populated by the config/rule engine after resolution.
	// Used by future update commands for MR grouping and automerge.
	Group     string // assigned group name from package rules
	Automerge bool   // whether this dep's MR should automerge
}

// VulnInfo describes a single known vulnerability affecting a dependency.
type VulnInfo struct {
	ID       string // e.g. "GHSA-xxxx-yyyy-zzzz", "CVE-2024-12345"
	Summary  string // short description
	Severity string // "LOW", "MODERATE", "HIGH", "CRITICAL" (from OSV/CVSS)
	FixedIn  string // version that fixes the vulnerability (if known)
	Source   string // provenance: "osv" (default), "trivy", "grype", "trivy+grype"
}

// Ecosystem constants identify the origin of a dependency.
const (
	EcosystemDockerImage = "docker-image"
	EcosystemDockerTool  = "docker-tool"
	EcosystemGoMod       = "gomod"
	EcosystemCargo       = "cargo"
	EcosystemNpm         = "npm"
	EcosystemAlpineAPK   = "alpine-apk"
	EcosystemDebianAPT   = "debian-apt"
	EcosystemPip         = "pip"
)
