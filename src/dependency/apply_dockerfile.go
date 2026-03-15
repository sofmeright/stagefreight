package dependency

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/lint/modules/freshness"
)

// NormalizeSkipReason maps internal parser-detail skip reasons to operator-facing language.
// Reasons that are already operator-friendly pass through unchanged.
func NormalizeSkipReason(reason string) string {
	switch {
	case reason == "line does not match ENV VERSION pattern":
		return "version not resolvable from source"
	case reason == "line does not match FROM pattern":
		return "version not resolvable from source"
	case strings.HasPrefix(reason, "ENV value "):
		return "source value mismatch"
	case reason == "current version not found in image token":
		return "source value mismatch"
	default:
		return reason
	}
}

// Dockerfile regexes: capture prefix/token/suffix groups for minimal diffs.
var (
	// FROM prefix(group1) image-token(group2) suffix(group3)
	fromRe = regexp.MustCompile(`^(FROM\s+(?:--platform=\S+\s+)?)(\S+)(.*)$`)

	// ENV prefix(group1) version-value(group2) suffix(group3)
	envVersionRe = regexp.MustCompile(`^(ENV\s+[A-Z0-9_]+_VERSION[= ])(\S+)(.*)$`)
)

// dockerfileEdit represents a pending line replacement with hash guard.
type dockerfileEdit struct {
	dep      freshness.Dependency
	line     int    // 1-based line number
	origHash [32]byte
	newLine  string
}

// applyDockerfileUpdates applies Dockerfile dependency updates.
// Returns touchedFiles (repo-root-relative Dockerfile paths) as the 3rd value.
func applyDockerfileUpdates(deps []freshness.Dependency, repoRoot string) ([]AppliedUpdate, []SkippedDep, []string, error) {
	var applied []AppliedUpdate
	var skipped []SkippedDep

	// Group deps by file, build edits
	type fileEdits struct {
		absPath string
		edits   []dockerfileEdit
	}
	byFile := make(map[string]*fileEdits)

	for _, dep := range deps {
		absPath := filepath.Join(repoRoot, dep.File)

		// Read the specific line to compute hash and build replacement
		origLine, err := readLineAt(absPath, dep.Line)
		if err != nil {
			skipped = append(skipped, SkippedDep{Dep: dep, Reason: fmt.Sprintf("cannot read line %d: %v", dep.Line, err)})
			continue
		}

		newLine, skip := buildReplacement(dep, origLine)
		if skip != "" {
			skipped = append(skipped, SkippedDep{Dep: dep, Reason: skip})
			continue
		}
		if newLine == origLine {
			skipped = append(skipped, SkippedDep{Dep: dep, Reason: "no change after replacement"})
			continue
		}

		fe, ok := byFile[dep.File]
		if !ok {
			fe = &fileEdits{absPath: absPath}
			byFile[dep.File] = fe
		}
		fe.edits = append(fe.edits, dockerfileEdit{
			dep:      dep,
			line:     dep.Line,
			origHash: sha256.Sum256([]byte(origLine)),
			newLine:  newLine,
		})

		update := AppliedUpdate{
			Dep:        dep,
			OldVer:     dep.Current,
			NewVer:     dep.Latest,
			UpdateType: updateType(dep.Current, dep.Latest),
		}
		for _, v := range dep.Vulnerabilities {
			update.CVEsFixed = append(update.CVEsFixed, v.ID)
		}
		applied = append(applied, update)
	}

	// Apply edits file by file, track touched files
	var touchedFiles []string
	for file, fe := range byFile {
		if err := applyFileEdits(fe.absPath, fe.edits); err != nil {
			return applied, skipped, nil, fmt.Errorf("editing %s: %w", file, err)
		}
		touchedFiles = append(touchedFiles, file)
	}
	sort.Strings(touchedFiles)

	return applied, skipped, touchedFiles, nil
}

// buildReplacement constructs the replacement line for a Dockerfile dependency.
// Returns the new line and a skip reason (empty if eligible).
func buildReplacement(dep freshness.Dependency, origLine string) (string, string) {
	switch dep.Ecosystem {
	case freshness.EcosystemDockerImage:
		return buildFromReplacement(dep, origLine)
	case freshness.EcosystemGitHubRelease:
		return buildEnvReplacement(dep, origLine)
	default:
		return origLine, "unsupported ecosystem for Dockerfile edit"
	}
}

// buildFromReplacement handles FROM line image tag replacement.
func buildFromReplacement(dep freshness.Dependency, origLine string) (string, string) {
	m := fromRe.FindStringSubmatch(origLine)
	if m == nil {
		return origLine, "line does not match FROM pattern"
	}

	token := m[2] // the image token

	// Skip checks on the token (these should already be handled by FilterUpdateCandidates,
	// but defense-in-depth)
	if strings.Contains(token, "@sha256:") {
		return origLine, "digest-pinned image"
	}
	if strings.ContainsAny(token, "$") {
		return origLine, "ARG-based dynamic base image"
	}

	// Replace the current version tag with the latest
	// dep.Current is the current tag/version, dep.Latest is the target
	newToken := strings.Replace(token, dep.Current, dep.Latest, 1)
	if newToken == token {
		return origLine, "current version not found in image token"
	}

	return m[1] + newToken + m[3], ""
}

// buildEnvReplacement handles ENV VERSION line replacement.
func buildEnvReplacement(dep freshness.Dependency, origLine string) (string, string) {
	m := envVersionRe.FindStringSubmatch(origLine)
	if m == nil {
		return origLine, "line does not match ENV VERSION pattern"
	}

	// Group 2 is the current value — replace with latest
	if m[2] != dep.Current {
		return origLine, fmt.Sprintf("ENV value %q does not match expected %q", m[2], dep.Current)
	}

	return m[1] + dep.Latest + m[3], ""
}

// applyFileEdits writes the edited lines back to a file, verifying hashes.
func applyFileEdits(absPath string, edits []dockerfileEdit) error {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(content), "\n")

	// Build edit map by line number
	editMap := make(map[int]*dockerfileEdit, len(edits))
	for i := range edits {
		editMap[edits[i].line] = &edits[i]
	}

	// Apply edits with hash verification
	for lineNum, edit := range editMap {
		if lineNum < 1 || lineNum > len(lines) {
			return fmt.Errorf("line %d out of range (file has %d lines)", lineNum, len(lines))
		}

		currentLine := lines[lineNum-1]
		currentHash := sha256.Sum256([]byte(currentLine))
		if currentHash != edit.origHash {
			return fmt.Errorf("line %d has been modified since resolution (hash mismatch)", lineNum)
		}

		lines[lineNum-1] = edit.newLine
	}

	return os.WriteFile(absPath, []byte(strings.Join(lines, "\n")), 0o644)
}

// readLineAt reads a specific 1-based line number from a file.
func readLineAt(absPath string, lineNum int) (string, error) {
	content, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(content), "\n")
	if lineNum < 1 || lineNum > len(lines) {
		return "", fmt.Errorf("line %d out of range (file has %d lines)", lineNum, len(lines))
	}

	return lines[lineNum-1], nil
}
