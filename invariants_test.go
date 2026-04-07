package config_test

// TestInvariant_NoDirectConfigDecodeOutsideConfigPackage enforces that no code
// outside src/config/ constructs a runtime Config via raw YAML decode.
//
// All runtime config MUST go through loadResolved (via LoadWithWarnings or
// LoadWithReport). Direct yaml.Unmarshal / yaml.NewDecoder calls that produce
// a config.Config outside this package bypass preset resolution, validation,
// and normalization — violating the StageFreight config invariant.
//
// See docs/invariants.md for the full contract.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvariant_NoDirectConfigDecodeOutsideConfigPackage(t *testing.T) {
	repoRoot := findRepoRoot(t)
	srcRoot := filepath.Join(repoRoot, "src")

	// Patterns that signal a raw config decode.
	forbidden := []string{
		"yaml.Unmarshal",
		"yaml.NewDecoder",
	}

	// Packages allowed to do raw YAML decoding that touches config types.
	// config/ owns the decode. preset/ owns preset file parsing.
	// All others are unrelated domains (lint, docker, governance, etc.).
	allowed := []string{
		filepath.Join(srcRoot, "config") + string(filepath.Separator),
	}

	err := filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		// Skip allowed packages.
		for _, a := range allowed {
			if strings.HasPrefix(path, a) {
				return nil
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		// Only flag files that both import config AND use raw YAML decode.
		if !bytes.Contains(data, []byte(`"github.com/PrPlanIT/StageFreight/src/config"`)) {
			return nil
		}

		for _, pat := range forbidden {
			if bytes.Contains(data, []byte(pat)) {
				t.Errorf(
					"%s: imports config and uses %s — "+
						"runtime Config must be constructed only via config.LoadWithWarnings or config.LoadWithReport",
					relPath(repoRoot, path), pat,
				)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking src: %v", err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod found)")
		}
		dir = parent
	}
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
