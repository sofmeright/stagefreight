package build

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strings"
)

// PackageInfo represents a discovered package/dependency from Dockerfile analysis.
// This is the build package's internal model — manifest generation converts
// these to schema types. Keeps the build package reusable without circular coupling.
type PackageInfo struct {
	Name       string // package name
	Version    string // version if known, empty otherwise
	Pinned     bool   // true if version is explicitly pinned
	Source     string // broad category: "dockerfile", "dockerfile_arg", "base_image"
	SourceRef  string // narrow origin: the actual instruction or ARG declaration
	Manager    string // package manager name: "apk", "pip", "npm", "go", "galaxy", "binary", "base", "apt"
	Confidence string // "inferred" for heuristic-derived items, empty for authoritative
	URL        string // download URL for binary installs
}

// ArgDecl holds a parsed Dockerfile ARG with its default value.
type ArgDecl struct {
	Name    string
	Default string
	Line    string // original instruction text
}

// InventoryResult holds all extracted packages grouped by manager.
type InventoryResult struct {
	Versions []PackageInfo // base image version components
	Packages []PackageInfo // all discovered packages
	Args     []ArgDecl     // ARG declarations with defaults
}

// ExtractInventory parses a Dockerfile and extracts package inventory.
// This is the main entry point for inventory extraction.
func ExtractInventory(dockerfilePath string) (*InventoryResult, error) {
	f, err := os.Open(dockerfilePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := &InventoryResult{}

	// Collect raw lines with continuation handling
	var lines []string
	scanner := bufio.NewScanner(f)
	var continued string
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip comments and empty lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if continued != "" {
				// continuation was interrupted by comment/blank — flush
				lines = append(lines, continued)
				continued = ""
			}
			continue
		}

		if strings.HasSuffix(trimmed, "\\") {
			continued += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}

		if continued != "" {
			lines = append(lines, continued+trimmed)
			continued = ""
		} else {
			lines = append(lines, trimmed)
		}
	}
	if continued != "" {
		lines = append(lines, continued)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// First pass: collect ARG declarations
	argMap := make(map[string]ArgDecl)
	for _, line := range lines {
		if decl, ok := parseArgDecl(line); ok {
			argMap[decl.Name] = decl
			result.Args = append(result.Args, decl)
		}
	}

	// Second pass: extract FROM stages and RUN instructions
	for _, line := range lines {
		upper := strings.ToUpper(strings.Fields(line)[0])

		switch upper {
		case "FROM":
			versions := parseBaseImageVersions(line)
			result.Versions = append(result.Versions, versions...)

		case "RUN":
			body := strings.TrimPrefix(line, strings.Fields(line)[0])
			body = strings.TrimSpace(body)
			pkgs := extractRunPackages(body, argMap)
			result.Packages = append(result.Packages, pkgs...)
		}
	}

	// Sort packages deterministically: by manager, then name, then version
	sort.Slice(result.Packages, func(i, j int) bool {
		if result.Packages[i].Manager != result.Packages[j].Manager {
			return result.Packages[i].Manager < result.Packages[j].Manager
		}
		if result.Packages[i].Name != result.Packages[j].Name {
			return result.Packages[i].Name < result.Packages[j].Name
		}
		return result.Packages[i].Version < result.Packages[j].Version
	})

	sort.Slice(result.Versions, func(i, j int) bool {
		return result.Versions[i].Name < result.Versions[j].Name
	})

	return result, nil
}

// ── ARG parsing ──────────────────────────────────────────────────────────────

var argDeclRe = regexp.MustCompile(`(?i)^ARG\s+([A-Za-z_][A-Za-z0-9_]*)(?:=(.*))?$`)

func parseArgDecl(line string) (ArgDecl, bool) {
	m := argDeclRe.FindStringSubmatch(line)
	if m == nil {
		return ArgDecl{}, false
	}
	return ArgDecl{
		Name:    m[1],
		Default: m[2],
		Line:    line,
	}, true
}

// ── Base image version parsing ───────────────────────────────────────────────

var baseImageRe = regexp.MustCompile(`(?i)^FROM\s+(?:--platform=\S+\s+)?(\S+)`)

// parseBaseImageVersions extracts version components from a FROM instruction.
// e.g., "FROM python:3.14.3-alpine3.23" → [python 3.14.3, alpine 3.23]
func parseBaseImageVersions(line string) []PackageInfo {
	m := baseImageRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}

	image := m[1]
	// Skip scratch, build stage references
	if image == "scratch" || !strings.Contains(image, ":") {
		return nil
	}

	parts := strings.SplitN(image, ":", 2)
	if len(parts) != 2 {
		return nil
	}

	imageName := parts[0]
	tag := parts[1]
	sourceRef := "FROM " + image

	// Extract the primary image name (last path component)
	nameParts := strings.Split(imageName, "/")
	primaryName := nameParts[len(nameParts)-1]

	var results []PackageInfo

	// Try to parse version from tag: "3.14.3-alpine3.23" → primary=3.14.3, suffix=alpine3.23
	// Pattern: version[-variant[version]]
	tagParts := strings.SplitN(tag, "-", 2)
	primaryVersion := tagParts[0]

	// Only add if looks like a version (starts with digit)
	if len(primaryVersion) > 0 && primaryVersion[0] >= '0' && primaryVersion[0] <= '9' {
		results = append(results, PackageInfo{
			Name:       primaryName,
			Version:    primaryVersion,
			Pinned:     false,
			Source:     "base_image",
			SourceRef:  sourceRef,
			Manager:    "base",
			Confidence: "inferred",
		})
	}

	// Parse suffix for known distro names
	if len(tagParts) > 1 {
		suffix := tagParts[1]
		distroVersions := parseDistroFromSuffix(suffix)
		for _, dv := range distroVersions {
			results = append(results, PackageInfo{
				Name:       dv.name,
				Version:    dv.version,
				Pinned:     false,
				Source:     "base_image",
				SourceRef:  sourceRef,
				Manager:    "base",
				Confidence: "inferred",
			})
		}
	}

	return results
}

type distroVersion struct {
	name    string
	version string
}

var distroRe = regexp.MustCompile(`^(alpine|debian|ubuntu|bookworm|bullseye|buster|slim|jammy|focal|noble)(.*)$`)

func parseDistroFromSuffix(suffix string) []distroVersion {
	m := distroRe.FindStringSubmatch(suffix)
	if m == nil {
		return nil
	}

	name := m[1]
	version := m[2]

	// Some distro names are codenames without numeric versions
	switch name {
	case "bookworm":
		return []distroVersion{{name: "debian", version: "12"}}
	case "bullseye":
		return []distroVersion{{name: "debian", version: "11"}}
	case "buster":
		return []distroVersion{{name: "debian", version: "10"}}
	case "jammy":
		return []distroVersion{{name: "ubuntu", version: "22.04"}}
	case "focal":
		return []distroVersion{{name: "ubuntu", version: "20.04"}}
	case "noble":
		return []distroVersion{{name: "ubuntu", version: "24.04"}}
	case "slim":
		return nil // slim is a variant, not a distro
	}

	return []distroVersion{{name: name, version: version}}
}

// ── RUN instruction parsing ──────────────────────────────────────────────────

// extractRunPackages parses a RUN instruction body and extracts packages.
func extractRunPackages(body string, args map[string]ArgDecl) []PackageInfo {
	var results []PackageInfo

	// Split on && ; | to get individual commands
	commands := splitShellCommands(body)

	for _, cmd := range commands {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}

		// Try each extractor
		for _, ext := range extractors {
			if pkgs := ext.extract(cmd, args); len(pkgs) > 0 {
				results = append(results, pkgs...)
				break // first match wins
			}
		}
	}

	return results
}

// splitShellCommands splits a shell command string on &&, ;, and |
func splitShellCommands(s string) []string {
	var result []string
	var current strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '&' && s[i+1] == '&' {
			result = append(result, current.String())
			current.Reset()
			i += 2
			continue
		}
		if s[i] == ';' || s[i] == '|' {
			result = append(result, current.String())
			current.Reset()
			i++
			continue
		}
		current.WriteByte(s[i])
		i++
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

// ── Extractor registry ───────────────────────────────────────────────────────

type extractor struct {
	manager string
	extract func(cmd string, args map[string]ArgDecl) []PackageInfo
}

var extractors = []extractor{
	{manager: "apk", extract: extractApk},
	{manager: "apt", extract: extractApt},
	{manager: "pip", extract: extractPip},
	{manager: "galaxy", extract: extractGalaxy},
	{manager: "npm", extract: extractNpm},
	{manager: "go", extract: extractGo},
	{manager: "binary", extract: extractBinary},
}

// ── apk extractor ────────────────────────────────────────────────────────────

var apkAddRe = regexp.MustCompile(`(?:^|\s)apk\s+add\b`)

func extractApk(cmd string, args map[string]ArgDecl) []PackageInfo {
	if !apkAddRe.MatchString(cmd) {
		return nil
	}

	var results []PackageInfo
	tokens := strings.Fields(cmd)
	pastAdd := false
	for _, tok := range tokens {
		if tok == "add" {
			pastAdd = true
			continue
		}
		if !pastAdd {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}
		results = append(results, PackageInfo{
			Name:      tok,
			Source:    "dockerfile",
			SourceRef: "RUN " + strings.TrimSpace(cmd),
			Manager:   "apk",
		})
	}
	return results
}

// ── apt extractor ────────────────────────────────────────────────────────────

var aptInstallRe = regexp.MustCompile(`(?:^|\s)apt(?:-get)?\s+install\b`)

func extractApt(cmd string, args map[string]ArgDecl) []PackageInfo {
	if !aptInstallRe.MatchString(cmd) {
		return nil
	}

	var results []PackageInfo
	tokens := strings.Fields(cmd)
	pastInstall := false
	for _, tok := range tokens {
		if tok == "install" {
			pastInstall = true
			continue
		}
		if !pastInstall {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}
		results = append(results, PackageInfo{
			Name:      tok,
			Source:    "dockerfile",
			SourceRef: "RUN " + strings.TrimSpace(cmd),
			Manager:   "apt",
		})
	}
	return results
}

// ── pip extractor ────────────────────────────────────────────────────────────

var pipInstallRe = regexp.MustCompile(`(?:^|\s)pip[23]?\s+install\b`)

func extractPip(cmd string, args map[string]ArgDecl) []PackageInfo {
	if !pipInstallRe.MatchString(cmd) {
		return nil
	}

	var results []PackageInfo
	tokens := strings.Fields(cmd)
	pastInstall := false
	for _, tok := range tokens {
		if tok == "install" {
			pastInstall = true
			continue
		}
		if !pastInstall {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}

		name, version, pinned := parsePipPackage(tok, args)
		results = append(results, PackageInfo{
			Name:      name,
			Version:   version,
			Pinned:    pinned,
			Source:    "dockerfile",
			SourceRef: "RUN " + strings.TrimSpace(cmd),
			Manager:   "pip",
		})
	}
	return results
}

// parsePipPackage handles pip package specs: pkg, pkg==1.0, pkg==${VAR}
func parsePipPackage(tok string, args map[string]ArgDecl) (name, version string, pinned bool) {
	if idx := strings.Index(tok, "=="); idx >= 0 {
		name = tok[:idx]
		version = tok[idx+2:]
		version = resolveArgVars(version, args)
		return name, version, true
	}
	if idx := strings.Index(tok, ">="); idx >= 0 {
		name = tok[:idx]
		version = tok[idx+2:]
		version = resolveArgVars(version, args)
		return name, version, false
	}
	return tok, "", false
}

// ── galaxy extractor ─────────────────────────────────────────────────────────

var galaxyInstallRe = regexp.MustCompile(`(?:^|\s)ansible-galaxy\s+(?:collection\s+)?install\b`)

func extractGalaxy(cmd string, args map[string]ArgDecl) []PackageInfo {
	if !galaxyInstallRe.MatchString(cmd) {
		return nil
	}

	var results []PackageInfo
	tokens := strings.Fields(cmd)
	pastInstall := false
	for _, tok := range tokens {
		if tok == "install" {
			pastInstall = true
			continue
		}
		if !pastInstall {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}
		// Galaxy collections look like namespace.collection or namespace.collection:version
		name := tok
		var version string
		var pinned bool
		if idx := strings.LastIndex(tok, ":"); idx > 0 {
			name = tok[:idx]
			version = tok[idx+1:]
			pinned = true
		}
		results = append(results, PackageInfo{
			Name:      name,
			Version:   version,
			Pinned:    pinned,
			Source:    "dockerfile",
			SourceRef: "RUN " + strings.TrimSpace(cmd),
			Manager:   "galaxy",
		})
	}
	return results
}

// ── npm extractor ────────────────────────────────────────────────────────────

var npmInstallRe = regexp.MustCompile(`(?:^|\s)npm\s+install\b`)

func extractNpm(cmd string, args map[string]ArgDecl) []PackageInfo {
	if !npmInstallRe.MatchString(cmd) {
		return nil
	}

	var results []PackageInfo
	tokens := strings.Fields(cmd)
	pastInstall := false
	for _, tok := range tokens {
		if tok == "install" {
			pastInstall = true
			continue
		}
		if !pastInstall {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}
		// npm packages: pkg, pkg@version, @scope/pkg@version
		name, version, pinned := parseNpmPackage(tok)
		results = append(results, PackageInfo{
			Name:      name,
			Version:   version,
			Pinned:    pinned,
			Source:    "dockerfile",
			SourceRef: "RUN " + strings.TrimSpace(cmd),
			Manager:   "npm",
		})
	}
	return results
}

func parseNpmPackage(tok string) (name, version string, pinned bool) {
	// Handle @scope/pkg@version — the @ for scope is not a version separator
	search := tok
	if strings.HasPrefix(tok, "@") {
		// Scoped package: find the second @
		rest := tok[1:]
		if idx := strings.Index(rest, "@"); idx >= 0 {
			name = tok[:idx+1]
			version = rest[idx+1:]
			return name, version, true
		}
		return tok, "", false
	}
	if idx := strings.LastIndex(search, "@"); idx > 0 {
		name = tok[:idx]
		version = tok[idx+1:]
		return name, version, true
	}
	return tok, "", false
}

// ── go extractor ─────────────────────────────────────────────────────────────

var goInstallRe = regexp.MustCompile(`(?:^|\s)go\s+install\b`)

func extractGo(cmd string, args map[string]ArgDecl) []PackageInfo {
	if !goInstallRe.MatchString(cmd) {
		return nil
	}

	var results []PackageInfo
	tokens := strings.Fields(cmd)
	pastInstall := false
	for _, tok := range tokens {
		if tok == "install" {
			pastInstall = true
			continue
		}
		if !pastInstall {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}
		// go install pkg@version
		name := tok
		var version string
		pinned := false
		if idx := strings.LastIndex(tok, "@"); idx > 0 {
			name = tok[:idx]
			version = tok[idx+1:]
			if version != "latest" {
				pinned = true
			}
		}
		results = append(results, PackageInfo{
			Name:      name,
			Version:   version,
			Pinned:    pinned,
			Source:    "dockerfile",
			SourceRef: "RUN " + strings.TrimSpace(cmd),
			Manager:   "go",
		})
	}
	return results
}

// ── binary extractor ─────────────────────────────────────────────────────────

var curlOutputRe = regexp.MustCompile(`curl\s+.*?(\S+)`)

func extractBinary(cmd string, args map[string]ArgDecl) []PackageInfo {
	// Look for curl/wget with a URL containing ARG variable references
	if !strings.Contains(cmd, "curl") && !strings.Contains(cmd, "wget") {
		return nil
	}

	var results []PackageInfo

	// Find URLs in the command (anything starting with http)
	tokens := strings.Fields(cmd)
	for _, tok := range tokens {
		// Strip surrounding quotes (common in Dockerfiles)
		tok = strings.Trim(tok, `"'`)
		if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
			continue
		}

		url := tok
		// Check if URL references any ARG variables
		for argName, argDecl := range args {
			placeholder := "${" + argName + "}"
			if strings.Contains(url, placeholder) {
				// Extract binary name from URL path
				binaryName := guessBinaryName(url)
				if binaryName == "" {
					continue
				}

				results = append(results, PackageInfo{
					Name:      binaryName,
					Version:   argDecl.Default,
					Pinned:    argDecl.Default != "",
					Source:    "dockerfile_arg",
					SourceRef: argDecl.Line,
					Manager:   "binary",
					URL:       url,
				})
				break // one binary per URL
			}
		}
	}

	return results
}

// guessBinaryName extracts a likely binary name from a download URL.
func guessBinaryName(url string) string {
	// Common patterns:
	// https://github.com/user/repo/releases/download/v1.0/repo-v1.0.linux.amd64
	// https://github.com/user/repo/releases/download/${VERSION}/repo-${VERSION}.linux.amd64

	parts := strings.Split(url, "/")
	// GitHub releases: .../releases/download/...
	for i, p := range parts {
		if p == "releases" && i >= 1 {
			return strings.ToLower(parts[i-1])
		}
	}

	// Fallback: last path segment, strip extensions and version vars
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		// Remove common suffixes
		for _, suffix := range []string{".tar.gz", ".tgz", ".zip", ".linux", ".amd64", ".arm64"} {
			last = strings.TrimSuffix(last, suffix)
		}
		// Remove variable references
		varRe := regexp.MustCompile(`\$\{[^}]+\}`)
		last = varRe.ReplaceAllString(last, "")
		last = strings.Trim(last, "-_.")
		if last != "" {
			return strings.ToLower(last)
		}
	}

	return ""
}

// ── ARG variable resolution ──────────────────────────────────────────────────

var argVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolveArgVars replaces ${VAR} references with ARG defaults.
func resolveArgVars(s string, args map[string]ArgDecl) string {
	return argVarRe.ReplaceAllStringFunc(s, func(match string) string {
		varName := argVarRe.FindStringSubmatch(match)[1]
		if decl, ok := args[varName]; ok && decl.Default != "" {
			return decl.Default
		}
		return match
	})
}

// SlugifyBuildID sanitizes a build ID for use as a filename.
// Lowercase, [a-z0-9-] only, spaces/underscores → hyphens, slashes forbidden.
func SlugifyBuildID(id string) string {
	id = strings.ToLower(id)
	id = strings.ReplaceAll(id, " ", "-")
	id = strings.ReplaceAll(id, "_", "-")

	var b strings.Builder
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}

	// Collapse multiple hyphens
	result := b.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return strings.Trim(result, "-")
}
