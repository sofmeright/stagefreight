// Package security provides vulnerability scanning and SBOM generation.
// Orchestrates external tools (Trivy, Grype, Syft) and produces structured
// results that feed into release notes and forge uploads.
package security

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/diag"
	"github.com/PrPlanIT/StageFreight/src/output"
)

// ScanConfig holds security scan configuration.
type ScanConfig struct {
	Enabled        bool      // run vulnerability scan
	TrivyEnabled   bool      // run Trivy scanner
	GrypeEnabled   bool      // run Grype scanner
	SBOMEnabled    bool      // generate SBOM
	FailOnCritical bool      // fail if critical vulns found
	ImageRef       string    // image reference or tarball path to scan
	OutputDir      string    // directory for scan artifacts
	SectionWriter  io.Writer // writer for CI section markers (nil = os.Stderr)
	TrivyCacheMax  string    // max_size for Trivy DB cache (full-clear when exceeded)
	GrypeCacheMax  string    // max_size for Grype DB cache (full-clear when exceeded)
}

// Vulnerability is a single parsed vulnerability from the scan.
type Vulnerability struct {
	ID          string // CVE ID (e.g., "CVE-2026-1234")
	Severity    string // CRITICAL, HIGH, MEDIUM, LOW
	Package     string // affected package name
	Installed   string // installed version
	FixedIn     string // version that fixes the vuln
	Description string // one-line description
	Source      string // scanner provenance: "trivy" or "grype"
}

// RefStability classifies how stable/immutable a reference is.
type RefStability string

const (
	StabilityDigest        RefStability = "digest"          // @sha256: — content-addressed, immutable
	StabilityTagWithDigest RefStability = "tag_with_digest" // tag + known digest — resolved instance
	StabilityTag           RefStability = "tag"             // bare tag — always mutable, even semver
)

// TargetSource describes how the scan target was selected.
type TargetSource string

const (
	TargetExplicit        TargetSource = "explicit"
	TargetPositionalArg   TargetSource = "positional_arg"
	TargetPublishManifest TargetSource = "publish_manifest"
)

// CandidateInfo describes a potential scan target candidate.
type CandidateInfo struct {
	Ref               string       `json:"ref"`
	Digest            string       `json:"digest,omitempty"`
	ObservedDigest    string       `json:"observed_digest,omitempty"`
	ObservedDigestAlt string       `json:"observed_digest_alt,omitempty"`
	Stability         RefStability `json:"stability"`
}

// ScanTarget describes the resolved scan target with full provenance.
type ScanTarget struct {
	Ref               string          `json:"ref"`
	DiscoveredTag     string          `json:"discovered_tag,omitempty"`     // original tag before digest resolution
	Digest            string          `json:"digest,omitempty"`
	ObservedDigest    string          `json:"observed_digest,omitempty"`
	ObservedDigestAlt string          `json:"observed_digest_alt,omitempty"`
	DigestMatch       *bool           `json:"digest_match,omitempty"`
	Source            TargetSource    `json:"source"`
	SelectionReason   string          `json:"selection_reason"`
	Stability         RefStability    `json:"stability"`
	Candidates        []CandidateInfo `json:"candidates,omitempty"`
	ExpectedTags      []string        `json:"expected_tags,omitempty"`
	ExpectedCommit    string          `json:"expected_commit,omitempty"`
	SigningAttempted   bool            `json:"signing_attempted,omitempty"`
}

// ScannerInfo describes a scanner that was run or attempted.
type ScannerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ScanResult holds the outcome of a security scan.
type ScanResult struct {
	Critical        int             // count of critical vulnerabilities
	High            int             // count of high vulnerabilities
	Medium          int             // count of medium vulnerabilities
	Low             int             // count of low vulnerabilities
	Vulnerabilities []Vulnerability // parsed vulnerability details (for detailed/full output)
	Status          string          // "passed", "warning", "critical"
	Artifacts       []string        // paths to generated files (JSON, SARIF, SBOM)
	Summary         string          // markdown summary for embedding in release notes
	EngineVersion   string          // best-effort: from `trivy --version` or empty
	OS              string          // "alpine 3.21.3" (from Trivy JSON Metadata.OS)
	Target          ScanTarget      // resolved scan target with provenance
	ScannersRun     []ScannerInfo   // scanners that completed successfully
	ScannersFailed  []ScannerInfo   // scanners that failed or were unavailable
	Partial         bool            // true if any enabled scanner failed
	CacheMode       string          // "persistent" or "ephemeral" — vuln DB cache location
}

// ClassifyRefStability determines the stability classification of an image reference.
func ClassifyRefStability(ref string, knownDigest string) RefStability {
	if strings.Contains(ref, "@sha256:") {
		return StabilityDigest
	}
	if knownDigest != "" {
		return StabilityTagWithDigest
	}
	return StabilityTag
}

// Scan runs vulnerability scans (Trivy + Grype when available),
// deduplicates results, and optionally generates SBOMs.
func Scan(ctx context.Context, cfg ScanConfig) (*ScanResult, error) {
	if !cfg.Enabled {
		return &ScanResult{Status: "skipped"}, nil
	}

	result := &ScanResult{}

	if cfg.OutputDir == "" {
		cfg.OutputDir = "."
	}

	// Best-effort engine version (silent capture, no stdout/stderr connection).
	result.EngineVersion = buildEngineVersion()

	// Section writer for CI collapsed sections (default: os.Stderr).
	sw := cfg.SectionWriter
	if sw == nil {
		sw = os.Stderr
	}

	// Resolve persistent cache for scanner vulnerability DBs.
	// Capability-based: use /stagefreight/cache/<tool> if mounted, else tools use defaults.
	// Enforce size cap before scan: full-clear when over max_size (opaque DBs, no granular eviction).
	trivyCacheDir := resolveScannerCacheDir("trivy")
	grypeCacheDir := resolveScannerCacheDir("grype")
	if trivyCacheDir != "" || grypeCacheDir != "" {
		result.CacheMode = "persistent"
	} else {
		result.CacheMode = "ephemeral"
	}
	if trivyCacheDir != "" && cfg.TrivyCacheMax != "" {
		enforceScannerCacheCap(trivyCacheDir, cfg.TrivyCacheMax)
	}
	if grypeCacheDir != "" && cfg.GrypeCacheMax != "" {
		enforceScannerCacheCap(grypeCacheDir, cfg.GrypeCacheMax)
	}

	// Run Trivy if enabled and available.
	if cfg.TrivyEnabled {
		output.SectionStartCollapsed(sw, "sf_trivy_raw", "Trivy scanner (raw)")
		if _, lookErr := exec.LookPath("trivy"); lookErr == nil {
			trivyVer := trivyVersion()
			// Trivy JSON scan
			jsonPath := cfg.OutputDir + "/security-scan.json"
			if err := runTrivy(ctx, cfg.ImageRef, "json", jsonPath, trivyCacheDir); err != nil {
				result.ScannersFailed = append(result.ScannersFailed, ScannerInfo{Name: "trivy", Version: trivyVer})
				result.Partial = true
				diag.Warn("trivy scan failed: %v", err)
			} else {
				// Trivy SARIF scan
				sarifPath := cfg.OutputDir + "/vulnerability-report.sarif"
				if err := runTrivy(ctx, cfg.ImageRef, "sarif", sarifPath, trivyCacheDir); err != nil {
					diag.Warn("trivy SARIF generation failed (continuing): %v", err)
				} else {
					result.Artifacts = append(result.Artifacts, sarifPath)
				}
				result.Artifacts = append(result.Artifacts, jsonPath)
				result.ScannersRun = append(result.ScannersRun, ScannerInfo{Name: "trivy", Version: trivyVer})

				// Parse Trivy vulnerabilities
				if err := parseTrivyVulnerabilities(jsonPath, result); err != nil {
					diag.Warn("parsing trivy results: %v", err)
				}
			}
		} else {
			result.ScannersFailed = append(result.ScannersFailed, ScannerInfo{Name: "trivy"})
			result.Partial = true
			diag.Warn("trivy not found on PATH — skipping Trivy scan")
		}
		output.SectionEnd(sw, "sf_trivy_raw")
	}

	// Run Grype if enabled and available.
	if cfg.GrypeEnabled {
		output.SectionStartCollapsed(sw, "sf_grype_raw", "Grype scanner (raw)")
		if _, lookErr := exec.LookPath("grype"); lookErr == nil {
			grypeVer := grypeVersion()
			grypeJSON := cfg.OutputDir + "/security-scan-grype.json"
			if err := runGrype(ctx, cfg.ImageRef, "json", grypeJSON, grypeCacheDir); err != nil {
				result.ScannersFailed = append(result.ScannersFailed, ScannerInfo{Name: "grype", Version: grypeVer})
				result.Partial = true
				diag.Warn("grype scan failed (continuing without Grype): %v", err)
			} else {
				result.Artifacts = append(result.Artifacts, grypeJSON)
				result.ScannersRun = append(result.ScannersRun, ScannerInfo{Name: "grype", Version: grypeVer})
				grypeVulns, parseErr := parseGrypeVulnerabilities(grypeJSON)
				if parseErr != nil {
					diag.Warn("grype parse failed (continuing without Grype): %v", parseErr)
				} else {
					result.Vulnerabilities = append(result.Vulnerabilities, grypeVulns...)
				}
			}
		} else {
			result.ScannersFailed = append(result.ScannersFailed, ScannerInfo{Name: "grype"})
			result.Partial = true
			diag.Warn("grype not found on PATH — skipping Grype scan")
		}
		output.SectionEnd(sw, "sf_grype_raw")
	}

	// Deduplicate across scanners and recount.
	if len(result.Vulnerabilities) > 0 {
		result.Vulnerabilities = deduplicateVulnerabilities(result.Vulnerabilities)
		result.Critical, result.High, result.Medium, result.Low = countSeverities(result.Vulnerabilities)
	}

	// Determine status
	switch {
	case result.Critical > 0:
		result.Status = "critical"
	case result.High > 0:
		result.Status = "warning"
	default:
		result.Status = "passed"
	}

	// Generate SBOM if enabled
	if cfg.SBOMEnabled {
		output.SectionStartCollapsed(sw, "sf_syft_raw", "Syft SBOM (raw)")
		spdxPath := cfg.OutputDir + "/sbom.spdx.json"
		spdxErr := runSyft(ctx, cfg.ImageRef, "spdx-json", spdxPath)
		if spdxErr == nil {
			result.Artifacts = append(result.Artifacts, spdxPath)
			cdxPath := cfg.OutputDir + "/sbom.cyclonedx.json"
			if err := runSyft(ctx, cfg.ImageRef, "cyclonedx-json", cdxPath); err != nil {
				output.SectionEnd(sw, "sf_syft_raw")
				return nil, fmt.Errorf("syft cyclonedx: %w", err)
			}
			result.Artifacts = append(result.Artifacts, cdxPath)
		}
		output.SectionEnd(sw, "sf_syft_raw")
		if spdxErr != nil {
			return nil, fmt.Errorf("syft spdx: %w", spdxErr)
		}
	}

	// Write advisory bridge artifact for dependency enrichment.
	if err := WriteAdvisories(cfg.OutputDir, cfg.ImageRef, result.Vulnerabilities); err != nil {
		diag.Warn("could not write security advisories: %v", err)
	} else {
		result.Artifacts = append(result.Artifacts, cfg.OutputDir+"/advisories.json")
	}

	return result, nil
}

// BuildSummary generates a markdown summary at the specified detail level.
// Returns (tile, body):
//   - tile: single-line status for hero area (e.g., "🛡️ ✅ **Passed** — no critical or high vulnerabilities")
//   - body: full section content (status line + optional <details> block with CVE data)
//
// Detail levels: "none", "counts", "detailed", "full".
func BuildSummary(result *ScanResult, detail string) (tile, body string) {
	if result.Status == "skipped" || detail == "none" {
		return "", ""
	}

	tile = buildStatusTile(result)

	switch detail {
	case "full":
		body = buildFullBody(result, tile)
	case "detailed":
		body = buildDetailedBody(result, tile)
	default: // "counts" or unrecognized
		body = tile + "\n"
	}
	return tile, body
}

// buildStatusTile produces the one-line security status.
func buildStatusTile(result *ScanResult) string {
	return fmt.Sprintf("🛡️ %s — %s", statusEmoji(result.Status), statusDetail(result))
}

func statusEmoji(status string) string {
	switch status {
	case "passed":
		return "✅ **Passed**"
	case "warning":
		return "⚠️ **Warning**"
	case "critical":
		return "❌ **Critical**"
	case "skipped":
		return "⏭️ **Skipped**"
	default:
		return status
	}
}

func statusDetail(result *ScanResult) string {
	total := result.Critical + result.High + result.Medium + result.Low
	if total == 0 {
		return "no vulnerabilities found"
	}
	switch {
	case result.Critical > 0 && result.High > 0:
		return fmt.Sprintf("%d critical and %d high vulnerabilities detected", result.Critical, result.High)
	case result.Critical > 0:
		return fmt.Sprintf("%d critical vulnerabilities detected", result.Critical)
	case result.High > 0:
		return fmt.Sprintf("%d high vulnerabilities detected", result.High)
	default:
		return fmt.Sprintf("%d vulnerabilities (%d medium, %d low)", total, result.Medium, result.Low)
	}
}

// vulnCountsSuffix builds a compact counts string for <summary> tags.
// Only includes non-zero severities.
func vulnCountsSuffix(result *ScanResult) string {
	var parts []string
	if result.Critical > 0 {
		parts = append(parts, fmt.Sprintf("%d critical", result.Critical))
	}
	if result.High > 0 {
		parts = append(parts, fmt.Sprintf("%d high", result.High))
	}
	if result.Medium > 0 {
		parts = append(parts, fmt.Sprintf("%d medium", result.Medium))
	}
	if result.Low > 0 {
		parts = append(parts, fmt.Sprintf("%d low", result.Low))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func buildDetailedBody(result *ScanResult, tile string) string {
	var b strings.Builder
	b.WriteString(tile)
	b.WriteString("\n")

	total := result.Critical + result.High + result.Medium + result.Low
	if total == 0 {
		return b.String()
	}

	// Collapsible CVE lists for critical and high
	b.WriteString(fmt.Sprintf("\n<details>\n<summary>Vulnerability details %s</summary>\n", vulnCountsSuffix(result)))

	maxPerSeverity := 5
	for _, sev := range []string{"CRITICAL", "HIGH"} {
		vulns := filterBySeverity(result.Vulnerabilities, sev)
		if len(vulns) == 0 {
			continue
		}

		b.WriteString(fmt.Sprintf("\n#### %s Vulnerabilities\n", titleCase(sev)))
		shown := 0
		for _, v := range vulns {
			if shown >= maxPerSeverity {
				remaining := len(vulns) - maxPerSeverity
				b.WriteString(fmt.Sprintf("- ... and %d more (see full report in release assets)\n", remaining))
				break
			}
			desc := v.Description
			if len(desc) > 80 {
				desc = desc[:77] + "..."
			}
			b.WriteString(fmt.Sprintf("- **%s** — %s (%s)\n", v.ID, desc, v.Package))
			shown++
		}
	}

	b.WriteString("\n</details>\n")
	return b.String()
}

func buildFullBody(result *ScanResult, tile string) string {
	var b strings.Builder
	b.WriteString(tile)
	b.WriteString("\n")

	total := result.Critical + result.High + result.Medium + result.Low
	if total == 0 {
		return b.String()
	}

	b.WriteString(fmt.Sprintf("\n<details>\n<summary>Vulnerability details %s</summary>\n\n", vulnCountsSuffix(result)))

	b.WriteString("| Severity | CVE | Package | Installed | Fixed | Description |\n")
	b.WriteString("|---|---|---|---|---|---|\n")

	for _, sev := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
		vulns := filterBySeverity(result.Vulnerabilities, sev)
		for _, v := range vulns {
			sevDisplay := titleCase(sev)
			if sev == "CRITICAL" {
				sevDisplay = "**Critical**"
			}
			desc := v.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fixedIn := v.FixedIn
			if fixedIn == "" {
				fixedIn = "—"
			}
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n",
				sevDisplay, v.ID, v.Package, v.Installed, fixedIn, desc))
		}
	}

	b.WriteString("\n</details>\n")
	return b.String()
}

func filterBySeverity(vulns []Vulnerability, severity string) []Vulnerability {
	var out []Vulnerability
	for _, v := range vulns {
		if strings.EqualFold(v.Severity, severity) {
			out = append(out, v)
		}
	}
	return out
}

// resolveScannerCacheDir returns a persistent cache path for a scanner's
// vulnerability DB, or empty string if no persistent cache is available.
// Capability-based: uses /stagefreight/cache/<tool> if mounted and writable.
// Failure to use persistent cache is non-fatal — scanners fall back to defaults.
func resolveScannerCacheDir(tool string) string {
	sfCache := filepath.Join("/stagefreight", "cache", tool)
	if info, err := os.Stat("/stagefreight"); err == nil && info.IsDir() {
		if err := os.MkdirAll(sfCache, 0o755); err == nil {
			return sfCache
		}
	}
	return ""
}

// enforceScannerCacheCap checks if a scanner cache directory exceeds maxSize.
// If over cap, deletes the entire directory (full-clear). These are opaque
// tool-managed DBs — no granular eviction, just bounded hosting.
// Errors are non-fatal but observable — silent failure would make cache
// behavior non-deterministic.
func enforceScannerCacheCap(cacheDir, maxSize string) {
	maxBytes, err := config.ParseSize(maxSize)
	if err != nil || maxBytes <= 0 {
		return
	}

	var totalSize int64
	if walkErr := filepath.Walk(cacheDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		totalSize += info.Size()
		return nil
	}); walkErr != nil {
		diag.Warn("scanner cache %s: walk failed: %v", cacheDir, walkErr)
		return
	}

	if totalSize > maxBytes {
		diag.Info("scanner cache %s exceeds %s (%d bytes), clearing", cacheDir, maxSize, totalSize)
		if err := os.RemoveAll(cacheDir); err != nil {
			diag.Warn("scanner cache %s: clear failed: %v", cacheDir, err)
			return
		}
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			diag.Warn("scanner cache %s: recreate failed: %v", cacheDir, err)
		}
	}
}

func titleCase(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func runTrivy(ctx context.Context, imageRef, format, output, cacheDir string) error {
	args := []string{"image", "--format", format, "--output", output}
	if cacheDir != "" {
		args = append(args, "--cache-dir", cacheDir)
	}
	args = append(args, imageRef)
	cmd := exec.CommandContext(ctx, "trivy", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runSyft(ctx context.Context, imageRef, format, output string) error {
	cmd := exec.CommandContext(ctx, "syft", imageRef, "-o", format, "-v")
	outFile, err := os.Create(output)
	if err != nil {
		return err
	}
	defer outFile.Close()
	cmd.Stdout = outFile
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func parseTrivyVulnerabilities(jsonPath string, result *ScanResult) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}

	// Trivy JSON structure
	var report struct {
		Metadata struct {
			OS struct {
				Family string `json:"Family"`
				Name   string `json:"Name"`
			} `json:"OS"`
		} `json:"Metadata"`
		Results []struct {
			Vulnerabilities []struct {
				VulnerabilityID  string `json:"VulnerabilityID"`
				Severity         string `json:"Severity"`
				PkgName          string `json:"PkgName"`
				InstalledVersion string `json:"InstalledVersion"`
				FixedVersion     string `json:"FixedVersion"`
				Title            string `json:"Title"`
				Description      string `json:"Description"`
			} `json:"Vulnerabilities"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return err
	}

	// Extract OS metadata (best-effort).
	family := strings.TrimSpace(report.Metadata.OS.Family)
	name := strings.TrimSpace(report.Metadata.OS.Name)
	if family != "" && name != "" {
		result.OS = family + " " + name
	} else if family != "" {
		result.OS = family
	} else if name != "" {
		result.OS = name
	}

	for _, r := range report.Results {
		for _, v := range r.Vulnerabilities {
			sev := strings.ToUpper(v.Severity)
			switch sev {
			case "CRITICAL":
				result.Critical++
			case "HIGH":
				result.High++
			case "MEDIUM":
				result.Medium++
			case "LOW":
				result.Low++
			}

			// Use Title if available, fall back to truncated Description
			desc := v.Title
			if desc == "" && v.Description != "" {
				desc = v.Description
				if len(desc) > 100 {
					desc = desc[:97] + "..."
				}
			}

			result.Vulnerabilities = append(result.Vulnerabilities, Vulnerability{
				ID:          v.VulnerabilityID,
				Severity:    sev,
				Package:     v.PkgName,
				Installed:   v.InstalledVersion,
				FixedIn:     v.FixedVersion,
				Description: desc,
				Source:      "trivy",
			})
		}
	}
	return nil
}

func runGrype(ctx context.Context, imageRef, format, output, cacheDir string) error {
	args := []string{imageRef, "-o", format, "--file", output, "-v"}
	cmd := exec.CommandContext(ctx, "grype", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if cacheDir != "" {
		cmd.Env = append(os.Environ(), "GRYPE_DB_CACHE_DIR="+cacheDir)
	}
	err := cmd.Run()
	if err != nil {
		// Grype exits 1 when vulnerabilities are found — output is still valid.
		if _, statErr := os.Stat(output); statErr == nil {
			return nil
		}
	}
	return err
}

func parseGrypeVulnerabilities(jsonPath string) ([]Vulnerability, error) {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}

	var report struct {
		Matches []struct {
			Vulnerability struct {
				ID          string `json:"id"`
				Severity    string `json:"severity"`
				Description string `json:"description"`
				Fix         struct {
					Versions []string `json:"versions"`
					State    string   `json:"state"`
				} `json:"fix"`
			} `json:"vulnerability"`
			Artifact struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"artifact"`
			RelatedVulnerabilities []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
			} `json:"relatedVulnerabilities"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}

	var vulns []Vulnerability
	for _, m := range report.Matches {
		sev := strings.ToUpper(m.Vulnerability.Severity)

		fixedIn := ""
		if len(m.Vulnerability.Fix.Versions) > 0 {
			fixedIn = m.Vulnerability.Fix.Versions[0]
		}

		desc := m.Vulnerability.Description
		if desc == "" && len(m.RelatedVulnerabilities) > 0 {
			desc = m.RelatedVulnerabilities[0].Description
		}
		if len(desc) > 100 {
			desc = desc[:97] + "..."
		}

		vulns = append(vulns, Vulnerability{
			ID:          m.Vulnerability.ID,
			Severity:    sev,
			Package:     m.Artifact.Name,
			Installed:   m.Artifact.Version,
			FixedIn:     fixedIn,
			Description: desc,
			Source:      "grype",
		})
	}
	return vulns, nil
}

// deduplicateVulnerabilities merges findings from multiple scanners.
// Key: (ID, Package). When duplicated, keeps the entry with more detail.
func deduplicateVulnerabilities(vulns []Vulnerability) []Vulnerability {
	type key struct {
		ID      string
		Package string
	}
	seen := make(map[key]int)
	var result []Vulnerability

	for _, v := range vulns {
		k := key{v.ID, v.Package}
		if idx, ok := seen[k]; ok {
			existing := result[idx]
			if v.FixedIn != "" && existing.FixedIn == "" {
				result[idx].FixedIn = v.FixedIn
			}
			if len(v.Description) > len(existing.Description) {
				result[idx].Description = v.Description
			}
			// Merge source provenance with stable output.
			if v.Source != "" {
				result[idx].Source = mergeSources(existing.Source, v.Source)
			}
			continue
		}
		seen[k] = len(result)
		result = append(result, v)
	}
	return result
}

// mergeSources combines source provenance strings with dedup and stable ordering.
func mergeSources(a, b string) string {
	seen := make(map[string]bool)
	var sources []string
	for _, s := range strings.Split(a, "+") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			sources = append(sources, s)
		}
	}
	for _, s := range strings.Split(b, "+") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			sources = append(sources, s)
		}
	}
	sort.Strings(sources)
	return strings.Join(sources, "+")
}

// countSeverities tallies severity counts from a vulnerability slice.
func countSeverities(vulns []Vulnerability) (critical, high, medium, low int) {
	for _, v := range vulns {
		switch strings.ToUpper(v.Severity) {
		case "CRITICAL":
			critical++
		case "HIGH":
			high++
		case "MEDIUM":
			medium++
		case "LOW":
			low++
		}
	}
	return
}

// trivyVersion returns the Trivy version string, or empty if unavailable.
func trivyVersion() string {
	out, err := exec.Command("trivy", "--version").Output()
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if ln == "" {
			continue
		}
		for _, tok := range strings.Fields(ln) {
			t := strings.TrimPrefix(tok, "v")
			if strings.Count(t, ".") >= 2 && len(t) >= 5 {
				return t
			}
		}
		break
	}
	return ""
}

// grypeVersion returns the Grype version string, or empty if unavailable.
func grypeVersion() string {
	out, err := exec.Command("grype", "version", "-o", "json").Output()
	if err != nil {
		return ""
	}
	var ver struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(out, &ver) == nil {
		return ver.Version
	}
	return ""
}

// buildEngineVersion returns a version string listing available scanners.
func buildEngineVersion() string {
	var parts []string
	if v := trivyVersion(); v != "" {
		parts = append(parts, "Trivy "+v)
	}
	if v := grypeVersion(); v != "" {
		parts = append(parts, "Grype "+v)
	}
	return strings.Join(parts, " + ")
}
