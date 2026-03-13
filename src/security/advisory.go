package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Advisory represents a fixable vulnerability for the dependency updater.
type Advisory struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	FixedIn   string `json:"fixed_in"`
	Ecosystem string `json:"ecosystem"`
	Source    string `json:"source"` // provenance: "trivy", "grype", or "trivy+grype" (deduped)
}

// AdvisoryFile is the bridge artifact written by security scan for the dependency updater.
type AdvisoryFile struct {
	SchemaVersion int        `json:"schema_version"`
	Generator     string     `json:"generator"`
	Image         string     `json:"image"`
	Advisories    []Advisory `json:"advisories"`
}

// WriteAdvisories writes fixable vulnerabilities as a bridge artifact.
// Only includes vulns with non-empty FixedIn — unfixable vulns aren't actionable.
func WriteAdvisories(outputDir, imageRef string, vulns []Vulnerability) error {
	var advisories []Advisory
	for _, v := range vulns {
		if v.FixedIn == "" {
			continue
		}
		advisories = append(advisories, Advisory{
			ID:        v.ID,
			Severity:  v.Severity,
			Package:   v.Package,
			Installed: v.Installed,
			FixedIn:   v.FixedIn,
			Ecosystem: InferEcosystem(v.Package, v.Installed),
			Source:    v.Source,
		})
	}

	file := AdvisoryFile{
		SchemaVersion: 1,
		Generator:     "stagefreight-security-scan",
		Image:         imageRef,
		Advisories:    advisories,
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(outputDir, "advisories.json"), data, 0o644)
}

// goModPattern matches Go module paths (contain / with domain-like prefix).
var goModPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z]{2,}/`)

// goVersionPattern matches Go standard library versions (v1.x.y).
var goVersionPattern = regexp.MustCompile(`^v1\.\d+\.\d+`)

// alpineRevisionPattern matches Alpine package version suffixes (-rN).
var alpineRevisionPattern = regexp.MustCompile(`-r\d+$`)

// InferEcosystem guesses the StageFreight ecosystem from package metadata.
// Explicitly heuristic — returns "unknown" as the safe default.
// Dep enrichment skips "unknown" entries entirely.
func InferEcosystem(pkg, installed string) string {
	// Go module: contains / with domain-like prefix (e.g., go.opentelemetry.io/..., github.com/...)
	if goModPattern.MatchString(pkg) {
		return "gomod"
	}

	// Go standard library: package "stdlib" with Go-style version
	if pkg == "stdlib" && goVersionPattern.MatchString(installed) {
		return "gomod"
	}

	// Alpine APK: version has -rN suffix (e.g., 1.3.1-r2)
	if alpineRevisionPattern.MatchString(installed) {
		return "alpine-apk"
	}

	// Fallback — dep updater skips these.
	return "unknown"
}

// ReadAdvisories reads the security advisory bridge file from the standard location
// (.stagefreight/security/advisories.json relative to rootDir).
// Returns empty slice + nil error if file doesn't exist (no advisories = normal).
func ReadAdvisories(rootDir string) ([]Advisory, error) {
	path := filepath.Join(rootDir, ".stagefreight", "security", "advisories.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var file AdvisoryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	return file.Advisories, nil
}

// NormalizePackageName returns a deterministic key for matching.
// Rules: trim whitespace, lowercase.
func NormalizePackageName(pkg, ecosystem string) string {
	return strings.ToLower(strings.TrimSpace(pkg))
}
