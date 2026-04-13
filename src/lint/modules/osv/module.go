package osv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/lint"
	"github.com/PrPlanIT/StageFreight/src/toolchain"
)

// lockfiles maps base filenames to whether osv-scanner supports them.
var lockfiles = map[string]bool{
	"go.mod":             true,
	"package-lock.json":  true,
	"yarn.lock":          true,
	"pnpm-lock.yaml":     true,
	"Cargo.lock":         true,
	"requirements.txt":   true,
	"poetry.lock":        true,
	"Pipfile.lock":       true,
	"composer.lock":      true,
	"Gemfile.lock":       true,
	"pubspec.lock":       true,
	"pom.xml":            true,
	"gradle.lockfile":    true,
}

type osvModule struct {
	once     sync.Once
	binPath  string // resolved binary path, empty if resolution failed
	resolveErr error // non-nil if pinned version failed
	desired  map[string]config.ToolPinConfig
}

func (m *osvModule) SetToolchainDesired(desired map[string]config.ToolPinConfig) {
	m.desired = desired
}

func newModule() *osvModule { return &osvModule{} }

func (m *osvModule) Name() string        { return "osv" }
func (m *osvModule) DefaultEnabled() bool { return true }

func (m *osvModule) CacheTTL() time.Duration { return 5 * time.Minute }

func (m *osvModule) AutoDetect() []string {
	return []string{
		"go.mod",
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
		"Cargo.lock",
		"requirements.txt",
		"poetry.lock",
		"Pipfile.lock",
		"composer.lock",
		"Gemfile.lock",
	}
}

func (m *osvModule) Check(ctx context.Context, file lint.FileInfo) ([]lint.Finding, error) {
	base := filepath.Base(file.Path)
	if !lockfiles[base] {
		return nil, nil
	}

	m.once.Do(func() {
		rootDir, _ := os.Getwd()
		ver, pinned := toolchain.ResolveVersion("osv-scanner", "", m.desired)
		result, err := toolchain.Resolve(rootDir, "osv-scanner", ver)
		if err != nil {
			if pinned {
				m.resolveErr = fmt.Errorf("osv-scanner pinned version %s failed to resolve: %w", ver, err)
			}
			return
		}
		m.binPath = result.Path
	})
	if m.resolveErr != nil {
		return nil, m.resolveErr // hard fail — pinned version contract
	}
	if m.binPath == "" {
		return nil, nil // not pinned, not available — silent skip
	}

	return m.scan(ctx, file)
}

func (m *osvModule) scan(ctx context.Context, file lint.FileInfo) ([]lint.Finding, error) {
	cmd := exec.CommandContext(ctx, m.binPath, "scan", "--format", "json", "-L", file.AbsPath)
	cmd.Env = toolchain.CleanEnv()
	out, err := cmd.Output()

	// osv-scanner exits 1 when vulnerabilities are found — still valid JSON.
	if err != nil && len(out) == 0 {
		// Real failure (binary crashed, bad args, etc.)
		return nil, fmt.Errorf("osv-scanner: %w", err)
	}

	var report osvReport
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("osv-scanner: parsing output: %w", err)
	}

	var findings []lint.Finding
	for _, result := range report.Results {
		for _, pkg := range result.Packages {
			for _, group := range pkg.Groups {
				if len(group.IDs) == 0 {
					continue
				}
				primaryID := group.IDs[0]
				summary := vulnSummary(group.IDs, pkg.Vulnerabilities)
				sev := severityFromScore(group.MaxSeverity)

				fixedIn := vulnFixedIn(primaryID, pkg.Vulnerabilities, pkg.Package.Name, pkg.Package.Ecosystem)

				msg := fmt.Sprintf("%s: %s (%s@%s", primaryID, summary, pkg.Package.Name, pkg.Package.Version)
				if fixedIn != "" {
					msg += ", fixed in " + fixedIn
				}
				msg += ")"

				findings = append(findings, lint.Finding{
					File:     file.Path,
					Line:     0,
					Module:   "osv",
					Severity: sev,
					Message:  msg,
				})
			}
		}
	}
	return findings, nil
}

// osvReport is the top-level osv-scanner v2 JSON output.
type osvReport struct {
	Results []osvResult `json:"results"`
}

type osvResult struct {
	Source   osvSource     `json:"source"`
	Packages []osvPackage `json:"packages"`
}

type osvSource struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

type osvPackage struct {
	Package         osvPkgInfo  `json:"package"`
	Vulnerabilities []osvVuln   `json:"vulnerabilities"`
	Groups          []osvGroup  `json:"groups"`
}

type osvPkgInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Ecosystem string `json:"ecosystem"`
}

type osvVuln struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Aliases  []string `json:"aliases"`
	Affected []struct {
		Ranges []struct {
			Events []struct {
				Fixed string `json:"fixed,omitempty"`
			} `json:"events"`
		} `json:"ranges"`
		Package *osvPkgInfo `json:"package"`
	} `json:"affected"`
	DatabaseSpecific map[string]any `json:"database_specific"`
}

type osvGroup struct {
	IDs         []string `json:"ids"`
	Aliases     []string `json:"aliases"`
	MaxSeverity string   `json:"max_severity"`
}

// vulnSummary finds the summary for any of the IDs in a group.
func vulnSummary(ids []string, vulns []osvVuln) string {
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	for _, v := range vulns {
		if idSet[v.ID] && v.Summary != "" {
			return v.Summary
		}
	}
	return "no description available"
}

// vulnFixedIn extracts the earliest fixed version for the given vuln/package.
func vulnFixedIn(id string, vulns []osvVuln, pkgName, ecosystem string) string {
	for _, v := range vulns {
		if v.ID != id {
			continue
		}
		for _, a := range v.Affected {
			if a.Package != nil && !strings.EqualFold(a.Package.Name, pkgName) {
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
	}
	return ""
}

// severityFromScore maps a CVSS numeric score string to lint severity.
func severityFromScore(score string) lint.Severity {
	f, err := strconv.ParseFloat(score, 64)
	if err != nil {
		return lint.SeverityWarning
	}
	switch {
	case f >= 9.0:
		return lint.SeverityCritical
	case f >= 7.0:
		return lint.SeverityCritical
	case f >= 4.0:
		return lint.SeverityWarning
	default:
		return lint.SeverityInfo
	}
}
