package gitver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScopedVersioningUsesMainDAG is a structural invariant test that
// enforces the single-call-site rule for `git describe --match` within the
// gitver package.
//
// The rule: scoped version detection MUST flow through DetectVersionWithOpts
// (the main search-path DAG). There must be exactly ZERO direct
// `git describe --match` invocations anywhere in gitver — the only tag
// discovery primitive allowed is `git tag --list` (used by the main
// detection pipeline).
//
// If this test fails, someone has reintroduced a parallel "find the latest
// tag" path that bypasses versioning.tag_sources and base_from semantics.
// Do NOT add a second git-describe call site to silence this test. Fix the
// caller to route through DetectVersionWithOpts with a synthetic
// VersioningOpts instead (see detectScopedVersionForTemplate in template.go
// for the reference pattern).
//
// The exact-match HEAD check inside headAtTag uses
// `git describe --tags --exact-match HEAD` which does NOT include --match
// flags, so it does not count against this rule. The rule targets pattern-
// filtered tag discovery specifically.
func TestScopedVersioningUsesMainDAG(t *testing.T) {
	pkgDir := "."
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}

	violations := []string{}
	const forbidden = `"describe"` // portion of an exec.Command arg sequence
	const describeMatch = `"--match"`

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip this file itself — it intentionally names the forbidden
		// token in comments and strings.
		if name == "isolation_test.go" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(pkgDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		src := string(data)

		// Look for any line that passes both "describe" and "--match" to
		// git via gitCmd / exec. This is the forbidden pattern.
		for i, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, forbidden) {
				continue
			}
			if !strings.Contains(line, describeMatch) {
				continue
			}
			// Skip the headAtTag call which uses --exact-match, not --match.
			// headAtTag doesn't filter by pattern, it checks HEAD specifically.
			if strings.Contains(line, "--exact-match") {
				continue
			}
			violations = append(violations,
				filepath.Join(pkgDir, name)+":"+lineNum(i+1)+": "+strings.TrimSpace(line))
		}
	}

	if len(violations) > 0 {
		t.Fatalf(
			"INVARIANT VIOLATION: gitver must not contain any pattern-filtered "+
				"`git describe --match` call sites.\n\n"+
				"Scoped version detection MUST route through DetectVersionWithOpts "+
				"with a synthetic VersioningOpts (see detectScopedVersionForTemplate).\n\n"+
				"Violations found:\n  %s\n\n"+
				"DO NOT add a second git-describe path to silence this test. "+
				"Route the caller through the main DAG.",
			strings.Join(violations, "\n  "))
	}
}

// lineNum converts an int to a string without importing strconv (keeps the
// test file dependency-free).
func lineNum(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
