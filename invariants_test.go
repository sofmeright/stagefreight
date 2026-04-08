package commit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoGitOrSSHShellOuts verifies that no package in the repository shells out
// to the `git` or `ssh` binaries via exec.Command or exec.CommandContext.
//
// After the go-git migration, all git operations go through src/gitstate/ or
// src/commit/ using the go-git library. Any regression that re-introduces a
// shell-out will be caught here and fail CI.
//
// Allowed packages (explicitly exempted):
//   - src/commit/     — CommitEngine and SyncEngine (domain owners)
//   - src/gitstate/   — Transport and repo primitives (domain owners)
//
// All other packages must NOT contain exec.Command("git", ...) or
// exec.Command("ssh", ...) or exec.CommandContext(ctx, "git", ...) etc.
func TestNoGitOrSSHShellOuts(t *testing.T) {
	// Locate the module root by walking up from this file's package
	repoRoot, err := findModuleRoot()
	if err != nil {
		t.Fatalf("cannot locate module root: %v", err)
	}

	srcRoot := filepath.Join(repoRoot, "src")
	if _, err := os.Stat(srcRoot); os.IsNotExist(err) {
		t.Skipf("src/ directory not found at %s", srcRoot)
	}

	// Packages exempt from the check.
	// src/commit and src/gitstate are the domain owners — all git operations live here.
	// No other packages are exempt.
	exempt := map[string]bool{
		"src/commit":   true,
		"src/gitstate": true,
	}

	var violations []string

	err = filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}

		// Determine package dir relative to repoRoot
		relDir := filepath.ToSlash(filepath.Dir(strings.TrimPrefix(path, repoRoot+string(os.PathSeparator))))
		if exempt[relDir] {
			return nil
		}

		violations = append(violations, checkFileForGitShellOuts(t, path, repoRoot)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking src/: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("found %d git/ssh shell-out violation(s) outside domain packages:\n\n%s\n\nAll git operations must go through src/gitstate/ or src/commit/ using go-git.",
			len(violations), strings.Join(violations, "\n"))
	}
}

// checkFileForGitShellOuts parses a Go source file and reports any
// exec.Command or exec.CommandContext calls where the first argument is "git" or "ssh".
func checkFileForGitShellOuts(t *testing.T, path, repoRoot string) []string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		// Not a syntax error we should fail on — just skip unparseable files
		return nil
	}

	relPath := strings.TrimPrefix(path, repoRoot+string(os.PathSeparator))

	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		// Match exec.Command or exec.CommandContext
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg.Name != "exec" {
			return true
		}
		if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
			return true
		}

		// Determine which argument is the command name
		argIdx := 0
		if sel.Sel.Name == "CommandContext" {
			argIdx = 1 // first arg is ctx, second is command name
		}

		if len(call.Args) <= argIdx {
			return true
		}

		lit, ok := call.Args[argIdx].(*ast.BasicLit)
		if !ok {
			return true
		}
		if lit.Kind != token.STRING {
			return true
		}

		cmdName := strings.Trim(lit.Value, `"`)
		if cmdName == "git" || cmdName == "ssh" {
			pos := fset.Position(call.Pos())
			violations = append(violations, fmt.Sprintf(
				"  %s:%d: exec.%s(%q, ...) — use src/gitstate/ or src/commit/ instead",
				relPath, pos.Line, sel.Sel.Name, cmdName,
			))
		}
		return true
	})

	return violations
}

// findModuleRoot walks parent directories until it finds a go.mod file.
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
