package freshness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OSV ecosystem identifiers mapped from our internal ecosystem constants.
// See https://ossf.github.io/osv-schema/#affectedpackage-field
var osvEcosystemMap = map[string]string{
	EcosystemGoMod:       "Go",
	EcosystemNpm:         "npm",
	EcosystemPip:         "PyPI",
	EcosystemCargo:       "crates.io",
	EcosystemAlpineAPK:   "Alpine",
	EcosystemDebianAPT:   "Debian",
	EcosystemDockerImage: "", // no OSV ecosystem for container images
	EcosystemGitHubRelease: "", // tools checked via GitHub advisories, not OSV
}

// osvQueryRequest is the POST body for api.osv.dev/v1/query.
type osvQueryRequest struct {
	Package *osvPackage `json:"package,omitempty"`
	Version string      `json:"version,omitempty"`
}

type osvPackage struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
}

// osvQueryResponse is the response from api.osv.dev/v1/query.
type osvQueryResponse struct {
	Vulns []osvVuln `json:"vulns"`
}

type osvVuln struct {
	ID       string           `json:"id"`
	Summary  string           `json:"summary"`
	Severity []osvSeverity    `json:"severity"`
	Affected []osvAffected    `json:"affected"`
}

type osvSeverity struct {
	Type  string `json:"type"`  // "CVSS_V3", "CVSS_V2"
	Score string `json:"score"` // CVSS vector string
}

type osvAffected struct {
	Package *osvPackage  `json:"package"`
	Ranges  []osvRange   `json:"ranges"`
}

type osvRange struct {
	Type   string      `json:"type"` // "ECOSYSTEM", "SEMVER", "GIT"
	Events []osvEvent  `json:"events"`
}

type osvEvent struct {
	Introduced string `json:"introduced,omitempty"`
	Fixed      string `json:"fixed,omitempty"`
}

// correlateVulns queries the OSV API for each dependency and populates
// the Vulnerabilities field. Only queries ecosystems that OSV supports.
func (m *freshnessModule) correlateVulns(ctx context.Context, deps []Dependency) {
	if !m.cfg.vulnEnabled() {
		return
	}

	for i := range deps {
		dep := &deps[i]

		osvEco, ok := osvEcosystemMap[dep.Ecosystem]
		if !ok || osvEco == "" {
			continue // ecosystem not supported by OSV
		}

		// Clean version for OSV query (strip leading 'v').
		version := strings.TrimPrefix(dep.Current, "v")
		if version == "" {
			continue
		}

		vulns, err := m.queryOSV(ctx, dep.Name, osvEco, version)
		if err != nil {
			continue // non-fatal
		}

		// Filter by min severity.
		for _, v := range vulns {
			if !meetsMinSeverity(v.Severity, m.cfg.Vulnerability.MinSeverity) {
				continue
			}
			vi := VulnInfo{
				ID:       v.ID,
				Summary:  v.Summary,
				Severity: extractHighestSeverity(v.Severity),
				FixedIn:  extractFixedVersion(v.Affected, dep.Name, osvEco),
				Source:   "osv",
			}
			// Skip vulns already fixed in the installed version.
			if vi.FixedIn != "" {
				delta := compareDependencyVersions(version, vi.FixedIn, dep.Ecosystem)
				if delta.Major < 0 || (delta.Major == 0 && delta.Minor < 0) || (delta.Major == 0 && delta.Minor == 0 && delta.Patch <= 0) {
					continue
				}
			}
			dep.Vulnerabilities = append(dep.Vulnerabilities, vi)
		}
	}
}

// queryOSV queries the OSV API for vulnerabilities affecting a specific
// package version.
func (m *freshnessModule) queryOSV(ctx context.Context, name, ecosystem, version string) ([]osvVuln, error) {
	body := osvQueryRequest{
		Package: &osvPackage{
			Name:      name,
			Ecosystem: ecosystem,
		},
		Version: version,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.osv.dev/v1/query", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("freshness: OSV query for %s: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("freshness: OSV query for %s: status %d", name, resp.StatusCode)
	}

	var result osvQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("freshness: OSV decode for %s: %w", name, err)
	}

	return result.Vulns, nil
}

// severityRank maps severity strings to numeric ranks for comparison.
var severityRank = map[string]int{
	"LOW":      1,
	"MODERATE": 2,
	"HIGH":     3,
	"CRITICAL": 4,
}

// meetsMinSeverity checks if any severity in the vuln meets the minimum threshold.
func meetsMinSeverity(severities []osvSeverity, minSev string) bool {
	if minSev == "" {
		return true
	}
	minRank := severityRank[strings.ToUpper(minSev)]
	if minRank == 0 {
		return true // unknown min → accept all
	}

	highest := extractHighestSeverity(severities)
	rank := severityRank[strings.ToUpper(highest)]
	if rank == 0 {
		// No CVSS score available — include by default (conservative).
		return true
	}
	return rank >= minRank
}

// extractHighestSeverity derives a severity label from CVSS vectors.
func extractHighestSeverity(severities []osvSeverity) string {
	bestScore := 0.0
	for _, s := range severities {
		if s.Type != "CVSS_V3" && s.Type != "CVSS_V2" {
			continue
		}
		score := parseCVSSBaseScore(s.Score)
		if score > bestScore {
			bestScore = score
		}
	}

	switch {
	case bestScore >= 9.0:
		return "CRITICAL"
	case bestScore >= 7.0:
		return "HIGH"
	case bestScore >= 4.0:
		return "MODERATE"
	case bestScore > 0:
		return "LOW"
	default:
		return "UNKNOWN"
	}
}

// parseCVSSBaseScore extracts the base score from a CVSS vector string.
// CVSS v3 vectors look like: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"
// We compute a rough score from the vector components.
// For simplicity, if the vector doesn't parse, return 0.
func parseCVSSBaseScore(vector string) float64 {
	// OSV sometimes includes the score directly in a "score" field alongside
	// the vector. The CVSS vector string itself requires full computation.
	// For a practical approach, we use known severity patterns from the vector.
	v := strings.ToUpper(vector)

	// Count high-impact components as a rough severity estimate.
	var score float64
	if strings.Contains(v, "/AV:N") {
		score += 2.5 // network attack vector
	}
	if strings.Contains(v, "/AC:L") {
		score += 1.5 // low complexity
	}
	if strings.Contains(v, "/PR:N") {
		score += 1.5 // no privileges required
	}
	if strings.Contains(v, "/C:H") {
		score += 1.5 // high confidentiality impact
	}
	if strings.Contains(v, "/I:H") {
		score += 1.5 // high integrity impact
	}
	if strings.Contains(v, "/A:H") {
		score += 1.5 // high availability impact
	}

	return score
}

// extractFixedVersion finds the earliest fixed version from affected ranges.
func extractFixedVersion(affected []osvAffected, name, ecosystem string) string {
	for _, a := range affected {
		if a.Package == nil {
			continue
		}
		if !strings.EqualFold(a.Package.Name, name) || a.Package.Ecosystem != ecosystem {
			continue
		}
		for _, r := range a.Ranges {
			for _, e := range r.Events {
				if e.Fixed != "" {
					return e.Fixed
				}
			}
		}
	}
	return ""
}
