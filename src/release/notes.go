// Package release handles release notes generation, release creation,
// and cross-platform sync.
package release

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitstate"
	"github.com/PrPlanIT/StageFreight/src/gitver"
)

// CommitCategory represents a group of commits by type.
type CommitCategory struct {
	Title   string // display title (e.g., "Features", "Bug Fixes")
	Prefix  string // conventional commit prefix (e.g., "feat", "fix")
	Commits []Commit
}

// Commit is a parsed conventional commit.
type Commit struct {
	Hash     string
	Type     string // feat, fix, chore, etc.
	Scope    string // optional scope in parens
	Summary  string
	Body     string
	Author   string
	Breaking bool
}

var conventionalRe = regexp.MustCompile(`^(\w+)(?:\(([^)]+)\))?(!)?\s*:\s*(.+)`)

// categoryOrder defines the display order for release notes.
var categoryOrder = []struct {
	prefix string
	title  string
}{
	{"BREAKING", "Breaking Changes"},
	{"feat", "Features"},
	{"fix", "Bug Fixes"},
	{"perf", "Performance"},
	{"security", "Security"},
	{"refactor", "Refactoring"},
	{"docs", "Documentation"},
	{"test", "Tests"},
	{"ci", "CI/CD"},
	{"chore", "Maintenance"},
	{"style", "Style"},
	{"migration", "Migrations"},
	{"hotfix", "Hotfixes"},
}

// ResolvedTag is a single tag with its deterministic UI URL.
type ResolvedTag struct {
	Name string // e.g., "1.0.0"
	URL  string // provider-derived tag page URL
}

// ImageRow is a single registry/image row for the Image Availability table.
type ImageRow struct {
	RegistryLabel string        // human label (e.g., "Docker Hub")
	RegistryURL   string        // provider-derived repo page URL
	ImageRef      string        // full image ref (e.g., "docker.io/prplanit/stagefreight")
	Tags          []ResolvedTag // resolved tags with URLs
	DigestRef     string        // host/path@sha256:... (for pull command)
	SBOM          string        // pull ref for SBOM artifact
	Provenance    string        // pull ref for provenance artifact
	Signature     string        // pull ref for signature artifact
}

// BinaryRow is a single binary or archive artifact for the Downloads table.
type BinaryRow struct {
	Name     string // filename (e.g., "stagefreight-linux-amd64.tar.gz")
	Platform string // "linux/amd64", "darwin/arm64"
	Size     int64  // bytes
	SHA256   string // hex-encoded checksum
}

// NotesInput holds all data needed to render release notes.
type NotesInput struct {
	RepoDir      string         // git repository directory
	FromRef      string         // start ref (empty = auto-detect previous tag)
	ToRef        string         // end ref (default: HEAD)
	TagPatterns  []string       // regex patterns for release tags (from versioning.tag_sources)
	Config       *config.Config // config for auto-detect version (nil = skip auto-detect)
	SecurityTile string         // one-line status (e.g., "🛡️ ✅ **Passed** — no vulnerabilities")
	SecurityBody string         // full section: status line + optional <details> CVE block
	TagMessage   string         // annotated tag message (optional, auto-detected if empty)
	ProjectName  string         // project name (auto-detected if empty)
	Version      string         // version string (auto-detected if empty)
	SHA          string         // short commit hash (auto-detected if empty)
	IsPrerelease bool           // true if version has prerelease suffix
	Images       []ImageRow     // resolved registry image rows for availability table
	Downloads    []BinaryRow    // binary/archive artifacts for downloads table
}

// GenerateNotes produces markdown release notes from git log between two refs.
func GenerateNotes(input NotesInput) (string, error) {
	if input.ToRef == "" {
		input.ToRef = "HEAD"
	}

	// Find previous tag if not specified
	if input.FromRef == "" {
		prev, err := PreviousReleaseTag(input.RepoDir, input.ToRef, input.TagPatterns)
		if err != nil || prev == "" {
			input.FromRef = ""
		} else {
			input.FromRef = prev
		}
	}

	// Auto-detect project metadata if not provided.
	// Requires input.Config — without it, auto-detect is skipped and the
	// caller is responsible for supplying Version/SHA directly.
	if (input.ProjectName == "" || input.Version == "" || input.SHA == "") && input.Config != nil {
		if vi, err := build.DetectVersion(input.RepoDir, input.Config); err == nil {
			if input.Version == "" {
				input.Version = vi.Version
			}
			if input.SHA == "" {
				input.SHA = vi.SHA
				if len(input.SHA) > 8 {
					input.SHA = input.SHA[:8]
				}
			}
			if !input.IsPrerelease {
				input.IsPrerelease = vi.IsPrerelease
			}
		}
		if input.ProjectName == "" {
			pm := gitver.DetectProject(input.RepoDir)
			if pm != nil {
				input.ProjectName = pm.Name
			}
		}
	}

	// Auto-detect tag message
	if input.TagMessage == "" {
		input.TagMessage = tagMessage(input.RepoDir, input.ToRef)
	}

	// Get commits
	commits, err := ParseCommits(input.RepoDir, input.FromRef, input.ToRef)
	if err != nil {
		return "", err
	}

	// Categorize
	categories := categorize(commits)

	return renderNotes(input, categories, commits), nil
}

// previousReleaseTag finds the most recent release tag that is an ancestor of
// currentRef and matches the configured tag patterns. It replaces the naive
// git-describe approach which matched any tag (including rolling aliases like
// "latest" or bare-version aliases like "0.1.0").
func PreviousReleaseTag(repoDir, currentRef string, tagPatterns []string) (string, error) {
	currentVersion := normalizeReleaseVersion(currentRef)

	matchers, err := compileReleaseTagMatchers(tagPatterns)
	if err != nil {
		return "", err
	}

	tags, err := listTagsByVersion(repoDir)
	if err != nil {
		return "", err
	}

	for _, tag := range tags {
		if !matchesAnyTagPattern(tag, matchers) {
			continue
		}

		// Do not treat same-version aliases as the "previous" release.
		// Example: currentRef=v0.1.0, stale alias tag=0.1.0
		if normalizeReleaseVersion(tag) == currentVersion {
			continue
		}

		ok, err := isAncestor(repoDir, tag, currentRef)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}

		return tag, nil
	}

	return "", nil
}

// compileReleaseTagMatchers compiles tag patterns into regex matchers.
// Falls back to a default semver pattern when no patterns are provided.
func compileReleaseTagMatchers(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		patterns = []string{`^v?\d+\.\d+\.\d+$`}
	}

	matchers := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile release tag pattern %q: %w", pattern, err)
		}
		matchers = append(matchers, re)
	}

	if len(matchers) == 0 {
		re, err := regexp.Compile(`^v?\d+\.\d+\.\d+$`)
		if err != nil {
			return nil, fmt.Errorf("compile default release tag pattern: %w", err)
		}
		matchers = append(matchers, re)
	}

	return matchers, nil
}

// matchesAnyTagPattern returns true if the tag matches at least one pattern.
func matchesAnyTagPattern(tag string, matchers []*regexp.Regexp) bool {
	for _, re := range matchers {
		if re.MatchString(tag) {
			return true
		}
	}
	return false
}

// normalizeReleaseVersion strips refs/tags/ prefix and optional v-prefix
// for same-version comparison.
func normalizeReleaseVersion(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "refs/tags/")
	ref = strings.TrimPrefix(ref, "v")
	return ref
}

// listTagsByVersion returns all git tags sorted by version descending.
func listTagsByVersion(repoDir string) ([]string, error) {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return nil, fmt.Errorf("opening repo: %w", err)
	}
	return gitstate.ListTagsSorted(repo)
}

// isAncestor returns true if ancestorRef is an ancestor of descendantRef.
func isAncestor(repoDir, ancestorRef, descendantRef string) (bool, error) {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return false, fmt.Errorf("opening repo: %w", err)
	}
	return gitstate.IsAncestor(repo, ancestorRef, descendantRef)
}

// tagMessage extracts the annotation message from an annotated tag.
// Returns empty for lightweight tags or on error.
func tagMessage(repoDir, ref string) string {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return ""
	}
	return gitstate.TagMessage(repo, ref)
}

// bulletize converts a multi-line text into markdown bullets.
// Lines already starting with "- " are kept as-is.
func bulletize(text string) string {
	lines := strings.Split(text, "\n")
	var bullets []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			line = "- " + line
		}
		bullets = append(bullets, line)
	}
	return strings.Join(bullets, "\n")
}

// formatBytes formats a byte count as a human-readable size.
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// truncHash returns the first 12 chars of a hex hash for compact display.
func truncHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// releaseType returns a human-readable release type.
func releaseType(isPrerelease bool) string {
	if isPrerelease {
		return "prerelease"
	}
	return "stable"
}

// ParseCommits extracts conventional commits from a git log range.
func ParseCommits(repoDir, fromRef, toRef string) ([]Commit, error) {
	repo, err := gitstate.OpenRepo(repoDir)
	if err != nil {
		return nil, fmt.Errorf("opening repo: %w", err)
	}

	rawCommits, err := gitstate.ParseCommitLog(repo, fromRef, toRef)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	var commits []Commit
	for _, raw := range rawCommits {
		subject, body := splitCommitMessage(raw.Message)
		hash := raw.Hash.String()
		if len(hash) > 7 {
			hash = hash[:7]
		}
		c := Commit{
			Hash:    hash,
			Summary: subject,
			Body:    body,
			Author:  raw.Author.Name,
		}

		// Parse conventional commit
		if m := conventionalRe.FindStringSubmatch(c.Summary); m != nil {
			c.Type = strings.ToLower(m[1])
			c.Scope = m[2]
			c.Breaking = m[3] == "!" || strings.Contains(strings.ToUpper(c.Body), "BREAKING CHANGE")
			c.Summary = m[4]
		}

		// Detect breaking change from body even without prefix
		if strings.Contains(strings.ToUpper(c.Body), "BREAKING CHANGE") {
			c.Breaking = true
		}

		commits = append(commits, c)
	}

	return commits, nil
}

// splitCommitMessage splits a raw git commit message into subject and body.
func splitCommitMessage(msg string) (subject, body string) {
	msg = strings.TrimSpace(msg)
	parts := strings.SplitN(msg, "\n", 2)
	subject = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		body = strings.TrimSpace(parts[1])
	}
	return
}

func categorize(commits []Commit) []CommitCategory {
	buckets := make(map[string][]Commit)
	for _, c := range commits {
		key := c.Type
		if c.Breaking {
			key = "BREAKING"
		}
		if key == "" {
			key = "other"
		}
		buckets[key] = append(buckets[key], c)
	}

	var categories []CommitCategory
	for _, cat := range categoryOrder {
		if cs, ok := buckets[cat.prefix]; ok {
			categories = append(categories, CommitCategory{
				Title:   cat.title,
				Prefix:  cat.prefix,
				Commits: cs,
			})
			delete(buckets, cat.prefix)
		}
	}

	// Any remaining uncategorized commits
	var otherCommits []Commit
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		otherCommits = append(otherCommits, buckets[k]...)
	}
	if len(otherCommits) > 0 {
		categories = append(categories, CommitCategory{
			Title:   "Other Changes",
			Prefix:  "other",
			Commits: otherCommits,
		})
	}

	return categories
}

func renderNotes(input NotesInput, categories []CommitCategory, allCommits []Commit) string {
	var b strings.Builder

	// 1. Hero header
	version := input.Version
	if version == "" {
		version = "unreleased"
	}
	project := input.ProjectName
	if project == "" {
		project = "release"
	}
	b.WriteString(fmt.Sprintf("## 📦 %s — `v%s`\n", project, version))

	// Metadata line
	var meta []string
	meta = append(meta, fmt.Sprintf("**Release type:** %s", releaseType(input.IsPrerelease)))
	if input.SHA != "" {
		meta = append(meta, fmt.Sprintf("**Commit:** `%s`", input.SHA))
	}
	b.WriteString(fmt.Sprintf("> %s\n\n", strings.Join(meta, " • ")))

	// 2. Security tile (compact status in hero area)
	if input.SecurityTile != "" {
		b.WriteString(fmt.Sprintf("**Security:** %s\n\n", input.SecurityTile))
	}

	// 3. Image Availability table
	if len(input.Images) > 0 {
		b.WriteString("## Image Availability\n\n")
		b.WriteString("| Registry | Image | Tags |\n")
		b.WriteString("|----------|-------|------|\n")
		for _, img := range input.Images {
			// Registry cell: linked label or plain text
			var regCell string
			if img.RegistryURL != "" {
				regCell = fmt.Sprintf("[%s](%s)", img.RegistryLabel, img.RegistryURL)
			} else {
				regCell = img.RegistryLabel
			}

			// Tags cell: linked code spans or plain code
			tagParts := make([]string, 0, len(img.Tags))
			for _, t := range img.Tags {
				if t.URL != "" {
					tagParts = append(tagParts, fmt.Sprintf("[`%s`](%s)", t.Name, t.URL))
				} else {
					tagParts = append(tagParts, fmt.Sprintf("`%s`", t.Name))
				}
			}

			b.WriteString(fmt.Sprintf("| %s | `%s` | %s |\n", regCell, img.ImageRef, strings.Join(tagParts, " ")))
		}
		b.WriteString("\n")

		// Digest pull commands and artifact links
		hasExtras := false
		for _, img := range input.Images {
			if img.DigestRef != "" || img.SBOM != "" || img.Provenance != "" || img.Signature != "" {
				hasExtras = true
				break
			}
		}
		if hasExtras {
			b.WriteString("<details>\n<summary>Digest pull commands & supply chain artifacts</summary>\n\n")
			for _, img := range input.Images {
				if img.DigestRef == "" && img.SBOM == "" && img.Provenance == "" && img.Signature == "" {
					continue
				}
				b.WriteString(fmt.Sprintf("**%s**\n", img.ImageRef))
				if img.DigestRef != "" {
					b.WriteString(fmt.Sprintf("```\ndocker pull %s\n```\n", img.DigestRef))
				}
				if img.SBOM != "" {
					b.WriteString(fmt.Sprintf("- SBOM: `%s`\n", img.SBOM))
				}
				if img.Provenance != "" {
					b.WriteString(fmt.Sprintf("- Provenance: `%s`\n", img.Provenance))
				}
				if img.Signature != "" {
					b.WriteString(fmt.Sprintf("- Signature: `%s`\n", img.Signature))
				}
				b.WriteString("\n")
			}
			b.WriteString("</details>\n\n")
		}
	}

	// 3b. Downloads table (binary/archive artifacts)
	if len(input.Downloads) > 0 {
		b.WriteString("## Downloads\n\n")
		b.WriteString("| Platform | File | Size | SHA-256 |\n")
		b.WriteString("|----------|------|------|---------|\n")
		for _, dl := range input.Downloads {
			b.WriteString(fmt.Sprintf("| `%s` | `%s` | %s | `%s` |\n",
				dl.Platform, dl.Name, formatBytes(dl.Size), truncHash(dl.SHA256)))
		}
		b.WriteString("\n")

		// Full checksums in collapsible block
		b.WriteString("<details>\n<summary>Full checksums</summary>\n\n")
		b.WriteString("```\n")
		for _, dl := range input.Downloads {
			b.WriteString(fmt.Sprintf("%s  %s\n", dl.SHA256, dl.Name))
		}
		b.WriteString("```\n</details>\n\n")
	}

	// 4. Highlights (tag message)
	if input.TagMessage != "" {
		b.WriteString("## Highlights\n")
		b.WriteString(bulletize(input.TagMessage))
		b.WriteString("\n\n")
	}

	// 5. Notable Changes (H2 wrapper, H4 categories)
	// Deduplicate commits within each category by summary+scope+author.
	if len(categories) > 0 {
		b.WriteString("## Notable Changes\n\n")
		for _, cat := range categories {
			b.WriteString(fmt.Sprintf("#### %s\n", cat.Title))
			type dedupKey struct{ scope, summary, author string }
			seen := make(map[dedupKey]int)
			type dedupEntry struct {
				key   dedupKey
				count int
			}
			var entries []dedupEntry
			for _, c := range cat.Commits {
				k := dedupKey{c.Scope, c.Summary, c.Author}
				if idx, ok := seen[k]; ok {
					entries[idx].count++
				} else {
					seen[k] = len(entries)
					entries = append(entries, dedupEntry{key: k, count: 1})
				}
			}
			for _, e := range entries {
				scope := ""
				if e.key.scope != "" {
					scope = fmt.Sprintf("**%s**: ", e.key.scope)
				}
				author := ""
				if e.key.author != "" {
					author = fmt.Sprintf(" (%s)", e.key.author)
				}
				countSuffix := ""
				if e.count > 1 {
					countSuffix = fmt.Sprintf(" ×%d", e.count)
				}
				b.WriteString(fmt.Sprintf("- %s%s%s%s\n", scope, e.key.summary, author, countSuffix))
			}
			b.WriteString("\n")
		}
	}

	// 6. Security section
	if input.SecurityBody != "" {
		b.WriteString("## Security\n\n")
		b.WriteString(input.SecurityBody)
		b.WriteString("\n")
	}

	// 7. Horizontal rule
	b.WriteString("---\n\n")

	// 8. Full changelog (always present, collapsible)
	b.WriteString("<details>\n<summary>Full changelog</summary>\n\n")
	if len(allCommits) == 0 {
		b.WriteString("No changes found.\n")
	} else {
		for _, c := range allCommits {
			author := ""
			if c.Author != "" {
				author = fmt.Sprintf(" (%s)", c.Author)
			}
			b.WriteString(fmt.Sprintf("- [`%s`] %s%s\n", c.Hash, c.Summary, author))
		}
	}
	b.WriteString("\n</details>\n")

	return b.String()
}
