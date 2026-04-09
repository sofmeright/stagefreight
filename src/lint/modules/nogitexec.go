package modules

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/lint"
)

func init() {
	lint.Register("no-git-exec", func() lint.Module {
		return &noGitExecModule{}
	})
}

// noGitExecModule forbids exec.Command("git") outside git_mirror.go.
// The runtime image has no git binary by design; all git operations must
// use go-git. The sole exception is src/sync/git_mirror.go which retains
// the CLI dependency for mirror push (tracked: forge-sync project).
type noGitExecModule struct{}

func (m *noGitExecModule) Name() string         { return "no-git-exec" }
func (m *noGitExecModule) DefaultEnabled() bool { return true }
func (m *noGitExecModule) AutoDetect() []string { return []string{"**/*.go"} }

var gitExecRe = regexp.MustCompile(`exec\.Command(Context)?\(\s*"git"`)

func (m *noGitExecModule) Check(_ context.Context, file lint.FileInfo) ([]lint.Finding, error) {
	p := filepath.ToSlash(file.Path)
	// Permitted sites:
	//   git_mirror.go     — mirror push retains CLI dependency (tracked: forge-sync)
	//   nogitexec.go      — this module; pattern appears in comments and the error message
	//   *_test.go         — tests run in the builder image which has git, not the runtime image
	if p == "src/sync/git_mirror.go" ||
		p == "src/lint/modules/nogitexec.go" ||
		strings.HasSuffix(p, "_test.go") {
		return nil, nil
	}

	f, err := os.Open(file.AbsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var findings []lint.Finding
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if gitExecRe.MatchString(scanner.Text()) {
			findings = append(findings, lint.Finding{
				File:     file.Path,
				Line:     lineNum,
				Module:   m.Name(),
				Severity: lint.SeverityCritical,
				Message:  `exec.Command("git") is forbidden outside src/sync/git_mirror.go — the runtime image has no git binary; use github.com/PrPlanIT/StageFreight/src/gitstate instead`,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return findings, nil
}
