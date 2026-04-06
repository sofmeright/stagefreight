// Package gitver provides git-based version detection and tag template
// resolution. It is the shared foundation used by both the docker build
// pipeline and the release management system.
package gitver

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/diag"
)

// VersionInfo holds resolved version metadata from git.
type VersionInfo struct {
	Version      string // full version: "1.2.3", "1.2.3-alpha.1", or branch_build format
	Base         string // semver base without prerelease: "1.2.3"
	Major        string
	Minor        string
	Patch        string
	Prerelease   string // "alpha.1", "beta.2", "rc.1", or "" for stable
	SHA          string
	Branch       string
	IsRelease    bool // true if HEAD is exactly at a tag
	IsPrerelease bool // true if tag has a prerelease suffix
}

// VersioningOpts provides versioning configuration to DetectVersionWithOpts.
// It is REQUIRED — nil is rejected with an error. The search-path model
// depends on declared intent (tag sources + branch rules); there is no
// legacy "just guess from git" path anymore.
type VersioningOpts struct {
	// TagSources is the ordered, declared list of named tag patterns.
	// Globally eligible for lineage candidates, but semantically flat:
	// priority lives on branch rules, not here.
	TagSources []TagSource

	// BranchRules is the ordered list of branch-specific rules. First match
	// wins in declaration order. The "default" id is the catch-all and
	// must appear last (validation enforces).
	BranchRules []BranchRule

	// NoLineageMode: "error" (default) or "explicit"
	NoLineageMode string

	// NoLineageVersion: template for explicit mode (must contain {sha} or {time})
	NoLineageVersion string
}

// TagSource is a named tag pattern: operators classify git tags into sources
// and branch rules reference those sources by id.
type TagSource struct {
	ID      string // e.g. "stable", "prerelease"
	Pattern string // regex string — compiled once at detectVersion setup

	// StripPrefix, if non-empty, is removed from a matched tag before semver
	// parsing. Used for scope-prefixed tag lines like "component-v1.0.0",
	// where the pattern matches the full tag but semver parsing needs the
	// suffix. Optional — empty means no transform.
	StripPrefix string
}

// BranchRule selects a version for a matched branch.
//
// A rule walks its BaseFromIDs search path; for each id it looks up the
// precompiled regex and scans tags in git's order. First match wins, then
// advance to next id if zero matches. See detectVersion for the full pipeline.
type BranchRule struct {
	ID          string         // entry id for errors and diagnostics
	IsDefault   bool           // true when this is the catch-all fallback rule
	Match       *regexp.Regexp // nil when IsDefault; compiled at opts-build time
	BaseFromIDs []string       // ordered fallback chain; required (len >= 1)
	Format      string         // version template: {base} {sha} {branch}
}

// semverRe captures major.minor.patch and optional -prerelease suffix.
var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-(.+))?$`)

// DetectVersionWithOpts resolves version info for a repo using the given
// versioning configuration. opts must not be nil — callers are expected to
// build opts from config.Config via build.versioningOptsFromConfig.
//
// INVARIANT — DO NOT VIOLATE (CRITICAL)
//
// Versioning resolution is a SEARCH PATH, not a filter chain.
//
// A branch defines an ORDERED list of tag_sources (base_from).
// This function walks that list and returns the FIRST matching tag.
//
// There is NO:
//   - global tag filtering
//   - candidate set construction
//   - ranking or prioritization
//   - cached "eligible tags"
//
// Any refactor that:
//   - pre-filters tags
//   - builds a global eligible set
//   - caches "matching tags"
//   - merges tag_sources to "simplify"
//
// will BREAK determinism and reintroduce ambiguity.
//
// If you think you need to do that, you are wrong.
// Fix the config, not the algorithm.
func DetectVersionWithOpts(rootDir string, opts *VersioningOpts) (*VersionInfo, error) {
	if opts == nil {
		return nil, fmt.Errorf("versioning opts required (nil opts is forbidden)")
	}
	// Execution invariants: validation should have rejected empty slices
	// at load time, but defend the invariant here too.
	if len(opts.TagSources) == 0 {
		return nil, fmt.Errorf("versioning: no tag_sources defined (validation bug)")
	}
	if len(opts.BranchRules) == 0 {
		return nil, fmt.Errorf("versioning: no branch_builds defined (validation bug)")
	}

	v := &VersionInfo{}

	// Current SHA
	sha, err := gitCmd(rootDir, "rev-parse", "--short=7", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("getting HEAD SHA: %w", err)
	}
	v.SHA = sha

	// Current branch
	branch, err := gitCmd(rootDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		v.Branch = branch
	}

	// Compile tag source regexes once. No runtime compile in the hot path.
	sourceRes, err := compileTagSourceRegexes(opts.TagSources)
	if err != nil {
		return nil, err
	}

	// DO NOT PRE-FILTER TAGS.
	//
	// Tag selection MUST happen inside the base_from search path.
	// Pre-filtering creates a global candidate set and breaks determinism.
	//
	// This function intentionally re-scans tags per source.
	// That is NOT a performance bug — it is required for correctness.
	// If you are reading this and thinking "I can optimize this", stop.
	// Read the INVARIANT comment at the top of this function.
	//
	// WARNING:
	// git tag ordering (--sort=-v:refname) is treated as authoritative
	// iteration order. Git's sort is semver-aware, which means non-semver
	// patterns (e.g. "^release-.*$") will produce iteration order that
	// reflects git's interpretation, not the operator's intent. If tag
	// sources use non-semver patterns, tags MUST sort correctly under
	// git's ordering or results will be surprising.
	// StageFreight WILL NOT reinterpret or re-sort tags — doing so would
	// invite semantic leakage back into gitver.
	tagsOut, _ := gitCmd(rootDir, "tag", "--list", "--sort=-v:refname")
	if tagsOut == "" {
		// Shallow clone? Fetch tags and retry once.
		if shallow, _ := gitCmd(rootDir, "rev-parse", "--is-shallow-repository"); shallow == "true" {
			_, _ = gitCmd(rootDir, "fetch", "--tags", "--depth=1")
			tagsOut, _ = gitCmd(rootDir, "tag", "--list", "--sort=-v:refname")
		}
	}
	tags := splitLines(tagsOut)

	// Defensive warnings (never block a build).
	emitTagSourceOverlapWarnings(tags, sourceRes)
	emitNonSemverOrderingWarnings(tags, sourceRes)

	// CI fast path: if the CI system provides an exact tag, trust it — but
	// only if it parses as semver. Non-semver CI tags fall through to the
	// normal search path (git describe --exact-match handles them).
	if ciTag := os.Getenv("CI_COMMIT_TAG"); ciTag != "" {
		if m := semverRe.FindStringSubmatch(ciTag); m != nil {
			populateSemver(v, m)
			v.IsRelease = true
			return v, nil
		}
	}

	// Select the branch rule for the current branch.
	rule, err := selectBranchRule(opts, v.Branch)
	if err != nil {
		return nil, err
	}

	// Walk the search path: for each source in rule.BaseFromIDs, scan tags.
	// First match wins. We also receive the id of the matching source so
	// we can apply any per-source transform (e.g. StripPrefix for scope-
	// prefixed tag lines).
	chosenTag, chosenSourceID, err := walkBaseFromSearchPath(tags, rule, sourceRes)
	if err != nil {
		return nil, err
	}

	if chosenTag == "" {
		// Search path exhausted — hand off to no_lineage policy.
		return applyNoLineage(v, opts)
	}

	// Determine if HEAD is exactly at the chosen tag. Uses the FULL tag
	// (including any prefix) because git's reference is the untransformed
	// name.
	if headAtTag(rootDir, chosenTag) {
		v.IsRelease = true
	}

	// Apply the per-source StripPrefix transform, if any, before semver
	// parsing. This is how scope-prefixed tag lines ("component-v1.2.3")
	// flow through the same populateSemver path as plain "v1.2.3" tags
	// without gitver needing to know anything about scoping.
	parseTag := chosenTag
	for i := range opts.TagSources {
		if opts.TagSources[i].ID == chosenSourceID {
			if prefix := opts.TagSources[i].StripPrefix; prefix != "" {
				parseTag = strings.TrimPrefix(parseTag, prefix)
			}
			break
		}
	}

	// Parse the chosen tag. Semver tags populate base/major/minor/patch;
	// non-semver tags render raw.
	if m := semverRe.FindStringSubmatch(parseTag); m != nil {
		populateSemver(v, m)
	} else {
		raw := strings.TrimPrefix(parseTag, "v")
		v.Version = raw
		v.Base = raw
	}

	// Non-release commits render with the rule's format template.
	if !v.IsRelease {
		format := rule.Format
		if format == "" {
			format = "{base}-dev+{sha}"
		}
		v.Version = renderVersionFormat(format, v)
	}

	return v, nil
}

// walkBaseFromSearchPath iterates rule.BaseFromIDs in declared order. For
// each id, looks up the precompiled regex and scans tags in git's order.
// Returns the first tag matching the current source's pattern AND the id
// of the source that matched (so callers can apply per-source transforms
// like StripPrefix). If no source in the path yields a hit, returns
// ("", "", nil) so the caller can invoke applyNoLineage.
//
// Do NOT convert "no match" into an error.
// "No match" is a valid state — it triggers no_lineage policy, which the
// operator may have configured as "explicit" mode with a bootstrap template.
// Only invalid config (missing source id, malformed regex) is an error.
func walkBaseFromSearchPath(
	tags []string,
	rule *BranchRule,
	sourceRes map[string]*regexp.Regexp,
) (tag string, sourceID string, err error) {
	// Runtime invariant guard: validation already rejects empty base_from,
	// but defend the invariant here too. If someone weakens validation later,
	// runtime still refuses to lie.
	if len(rule.BaseFromIDs) == 0 {
		return "", "", fmt.Errorf(
			"versioning: branch rule %q has empty base_from (validation bug)",
			rule.ID)
	}
	for _, id := range rule.BaseFromIDs {
		re, ok := sourceRes[id]
		if !ok {
			// Validation bug — fail loud.
			return "", "", fmt.Errorf(
				"versioning: base_from id %q not found in compiled tag_sources",
				id)
		}
		for _, t := range tags {
			if re.MatchString(t) {
				return t, id, nil
			}
		}
		// No match in this source — advance to next id in the path.
	}
	return "", "", nil // search path exhausted
}

// selectBranchRule returns the rule whose Match regex matches the current
// branch, falling back to the default rule if no named rule matches.
//
// The "default" concept is expressed as a boolean flag (BranchRule.IsDefault),
// NOT a magic string comparison. The flag is set at opts-build time from
// the YAML id "default", which gives validation a single place to enforce
// the catch-all semantics without runtime depending on string equality.
//
// Validation guarantees the default rule appears last, so in practice
// defaultRule is only set on the final iteration. The code still walks the
// whole slice before returning default — even if validation is accidentally
// weakened later, the semantics stay correct: default only wins if no named
// rule matched.
func selectBranchRule(opts *VersioningOpts, branch string) (*BranchRule, error) {
	var defaultRule *BranchRule
	for i := range opts.BranchRules {
		r := &opts.BranchRules[i]
		if r.IsDefault {
			defaultRule = r
			continue // do NOT return here — keep scanning
		}
		if r.Match != nil && r.Match.MatchString(branch) {
			return r, nil
		}
	}
	if defaultRule != nil {
		return defaultRule, nil
	}
	return nil, fmt.Errorf(
		"versioning: no branch_build rule matches %q and no default defined",
		branch)
}

// compileTagSourceRegexes compiles each tag source pattern once and returns a
// lookup map keyed by source id. Called at the top of DetectVersionWithOpts
// so the search path walk reads compiled regexes in O(1).
func compileTagSourceRegexes(sources []TagSource) (map[string]*regexp.Regexp, error) {
	out := make(map[string]*regexp.Regexp, len(sources))
	for _, ts := range sources {
		re, err := regexp.Compile(ts.Pattern)
		if err != nil {
			return nil, fmt.Errorf("versioning: tag_sources[%s]: %w", ts.ID, err)
		}
		out[ts.ID] = re
	}
	return out, nil
}

// applyNoLineage handles the case where the search path was exhausted.
// Behavior is controlled by VersioningOpts.NoLineageMode.
func applyNoLineage(v *VersionInfo, opts *VersioningOpts) (*VersionInfo, error) {
	mode := opts.NoLineageMode
	if mode == "" {
		mode = "error"
	}

	switch mode {
	case "error":
		return nil, fmt.Errorf(
			"no version lineage found:\n" +
				"  - no tags match any source in the branch rule's base_from search path\n" +
				"  - repository may be shallow or untagged\n" +
				"set versioning.no_lineage.mode to 'explicit' with a version template to override")
	case "explicit":
		if opts.NoLineageVersion == "" {
			return nil, fmt.Errorf("versioning.no_lineage mode=explicit requires version template")
		}
		rendered := renderVersionFormat(opts.NoLineageVersion, v)
		v.Version = rendered
		// Base/major/minor/patch remain zero — caller treats this as pre-lineage bootstrap
		v.Base = ""
		return v, nil
	default:
		return nil, fmt.Errorf("versioning.no_lineage: unknown mode %q", mode)
	}
}

// populateSemver fills major/minor/patch/base/prerelease from a semverRe
// submatch slice. Extracted for reuse between CI fast path and normal path.
func populateSemver(v *VersionInfo, m []string) {
	v.Major = m[1]
	v.Minor = m[2]
	v.Patch = m[3]
	v.Base = fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])
	if m[4] != "" {
		v.Prerelease = m[4]
		v.IsPrerelease = true
		v.Version = fmt.Sprintf("%s-%s", v.Base, v.Prerelease)
	} else {
		v.Version = v.Base
	}
}

// renderVersionFormat substitutes placeholders in a version format template.
// Supported: {base}, {sha}, {branch}
func renderVersionFormat(format string, v *VersionInfo) string {
	out := format
	out = strings.ReplaceAll(out, "{base}", v.Base)
	out = strings.ReplaceAll(out, "{sha}", v.SHA)
	out = strings.ReplaceAll(out, "{branch}", v.Branch)
	return out
}

// headAtTag returns true if HEAD is exactly at the given tag.
func headAtTag(rootDir, tag string) bool {
	out, err := gitCmd(rootDir, "describe", "--tags", "--exact-match", "HEAD")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == tag
}

// splitLines splits git command output into a clean list of non-empty lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// emitTagSourceOverlapWarnings warns when a single tag matches multiple tag
// sources. Intentional overlap is allowed (it enables base_from fallback
// chains), but silent overlap causes surprising selection. Runtime sampling
// against real git tags is the honest answer — regex intersection is
// undecidable in general.
//
// Routes through diag.Warn, not raw os.Stderr — structured diagnostics flow
// is the only warning channel for core engine code.
func emitTagSourceOverlapWarnings(tags []string, sourceRes map[string]*regexp.Regexp) {
	warned := make(map[string]bool)
	for _, tag := range tags {
		var hits []string
		for id, re := range sourceRes {
			if re.MatchString(tag) {
				hits = append(hits, id)
			}
		}
		if len(hits) > 1 {
			sort.Strings(hits)
			key := strings.Join(hits, ",")
			if !warned[key] {
				warned[key] = true
				diag.Warn("versioning: tag %q matches multiple tag_sources: %v (intentional overlap is OK; tighten patterns if unintended)",
					tag, hits)
			}
		}
	}
}

// emitNonSemverOrderingWarnings warns when a tag source matches non-semver
// tags. git's --sort=-v:refname is semver-aware, so non-semver tags may not
// iterate in chronological or lexical order. The warning is operator
// guidance; StageFreight does not reinterpret or re-sort.
//
// Routes through diag.Warn, not raw os.Stderr.
func emitNonSemverOrderingWarnings(tags []string, sourceRes map[string]*regexp.Regexp) {
	const sampleSize = 10
	const exampleCount = 3
	for id, re := range sourceRes {
		var matched []string
		for _, tag := range tags {
			if re.MatchString(tag) {
				matched = append(matched, tag)
				if len(matched) >= sampleSize {
					break
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		nonSemver := 0
		for _, tag := range matched {
			if !semverRe.MatchString(tag) {
				nonSemver++
			}
		}
		if nonSemver > 0 {
			n := exampleCount
			if n > len(matched) {
				n = len(matched)
			}
			diag.Warn("versioning: tag_source %q matches non-semver tags (sample: %v); git's semver-aware sort may not reflect chronological intent — consider zero-padding or a semver-compatible naming convention",
				id, matched[:n])
		}
	}
}

// gitCmd runs a git command and returns trimmed stdout.
func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
