package commit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/config"
)

// PlannerOptions holds raw inputs from CLI flags and positional args.
type PlannerOptions struct {
	Type     string
	Scope    string
	Message  string   // positional or --message
	Body     string
	Breaking bool
	SkipCI   *bool    // nil = use config default
	Push     *bool    // nil = use config default
	Paths    []string // from --add flags
	All      bool
	SignOff  bool
	Remote   string
	Refspec  string
}

// BuildPlan merges CLI flags with config defaults, validates, and returns a Plan.
func BuildPlan(opts PlannerOptions, cfg config.CommitConfig, registry *TypeRegistry, rootDir string) (*Plan, error) {
	// 1. Validate summary
	if opts.Message == "" {
		return nil, fmt.Errorf("commit summary is required (-m or positional argument)")
	}

	// 2. Resolve type
	// Priority: explicit -t flag → parsed from message prefix → config default → "chore"
	commitType := opts.Type
	if commitType == "" && cfg.Conventional {
		// Parse conventional commit prefix from message: "feat: summary" → type="feat", message="summary"
		if parsed, scope, rest, bang, ok := parseConventionalPrefix(opts.Message, registry); ok {
			commitType = parsed
			opts.Message = rest
			if scope != "" && opts.Scope == "" {
				opts.Scope = scope
			}
			if bang {
				opts.Breaking = true
			}
		}
	}
	if commitType == "" {
		commitType = cfg.DefaultType
	}
	if commitType == "" {
		commitType = "chore"
	}

	// 3. Resolve alias
	resolvedType, forceBang, err := registry.Resolve(commitType)
	if err != nil {
		return nil, err
	}

	// 4. Merge breaking
	breaking := opts.Breaking || forceBang

	// 5. Determine StageMode
	var stageMode StageMode
	switch {
	case opts.All:
		stageMode = StageAll
	case len(opts.Paths) > 0:
		stageMode = StageExplicit
	default:
		stageMode = StageStaged
	}

	// 6. Normalize paths
	var normalizedPaths []string
	if stageMode == StageExplicit {
		seen := make(map[string]bool)
		for _, p := range opts.Paths {
			expanded, err := expandPath(p, rootDir)
			if err != nil {
				return nil, err
			}
			for _, ep := range expanded {
				if !seen[ep] {
					seen[ep] = true
					normalizedPaths = append(normalizedPaths, ep)
				}
			}
		}
	}

	// 7. Resolve scope
	scope := opts.Scope
	if scope == "" {
		scope = cfg.DefaultScope
	}

	// 8. Merge SkipCI: flag > config
	skipCI := cfg.SkipCI
	if opts.SkipCI != nil {
		skipCI = *opts.SkipCI
	}

	// 9. Merge Push: flag > config
	push := cfg.Push
	if opts.Push != nil {
		push = *opts.Push
	}

	// 10. Build PushOptions
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}

	refspec := opts.Refspec

	// 11. CI refspec auto-detection when push is enabled and no explicit refspec
	if push && refspec == "" {
		if os.Getenv("CI_COMMIT_TAG") != "" {
			// Tag pipeline: push docs to the default branch, not the tag ref
			if defaultBranch := os.Getenv("CI_DEFAULT_BRANCH"); defaultBranch != "" {
				refspec = "HEAD:refs/heads/" + defaultBranch
			}
		} else if ref := os.Getenv("CI_COMMIT_REF_NAME"); ref != "" {
			refspec = "HEAD:refs/heads/" + ref
		} else if branch := os.Getenv("CI_COMMIT_BRANCH"); branch != "" {
			refspec = "HEAD:refs/heads/" + branch
		} else if ref := os.Getenv("GITHUB_REF_NAME"); ref != "" {
			refspec = "HEAD:refs/heads/" + ref
		}
	}

	return &Plan{
		Type:      resolvedType,
		Scope:     scope,
		Summary:   opts.Message,
		Body:      opts.Body,
		Breaking:  breaking,
		SkipCI:    skipCI,
		Paths:     normalizedPaths,
		StageMode: stageMode,
		Push: PushOptions{
			Enabled:         push,
			Remote:          remote,
			Refspec:         refspec,
			RebaseOnDiverge: true, // default: rebase instead of failing on diverge
		},
		SignOff: opts.SignOff,
	}, nil
}

// parseConventionalPrefix extracts a conventional commit type, scope, and breaking
// marker from a message prefix.
// Returns (type, scope, rest-of-message, breaking, true) if a valid prefix was found.
// Handles: "feat: summary", "fix!: summary", "feat(api): summary", "feat(api)!: summary"
// Only matches if the type is known to the registry (prevents false positives).
func parseConventionalPrefix(msg string, registry *TypeRegistry) (string, string, string, bool, bool) {
	colonIdx := strings.Index(msg, ":")
	if colonIdx < 1 || colonIdx > 40 {
		return "", "", "", false, false
	}

	prefix := msg[:colonIdx]
	rest := strings.TrimSpace(msg[colonIdx+1:])
	if rest == "" {
		return "", "", "", false, false
	}

	// Detect and strip bang (breaking change marker)
	bang := strings.HasSuffix(prefix, "!")
	prefix = strings.TrimSuffix(prefix, "!")

	// Extract scope: "feat(api)" → type="feat", scope="api"
	// Rejects: "feat()" (empty scope), "feat(api)extra" (trailing garbage)
	var scope string
	if openParen := strings.Index(prefix, "("); openParen > 0 {
		closeParen := strings.Index(prefix, ")")
		if closeParen <= openParen+1 {
			return "", "", "", false, false // missing or empty scope
		}
		scope = prefix[openParen+1 : closeParen]
		if strings.TrimSpace(scope) == "" {
			return "", "", "", false, false // whitespace-only scope
		}
		if strings.TrimSpace(prefix[closeParen+1:]) != "" {
			return "", "", "", false, false // trailing content after )
		}
		prefix = prefix[:openParen]
	}

	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", "", "", false, false
	}

	// Only accept if the type is known to the registry
	if _, _, err := registry.Resolve(prefix); err != nil {
		return "", "", "", false, false
	}

	return prefix, scope, rest, bang, true
}

// expandPath resolves a single --add path: handles globs, verifies existence,
// and returns repo-relative paths.
func expandPath(p, rootDir string) ([]string, error) {
	// Reject paths that escape the repo
	if filepath.IsAbs(p) {
		abs := filepath.Clean(p)
		rel, err := filepath.Rel(rootDir, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("path %q is outside the repository", p)
		}
		// Check existence
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("path %q does not exist", p)
		}
		return []string{rel}, nil
	}

	if strings.Contains(p, "..") {
		resolved := filepath.Clean(filepath.Join(rootDir, p))
		rel, err := filepath.Rel(rootDir, resolved)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("path %q escapes the repository root", p)
		}
	}

	// Try glob expansion
	absPattern := filepath.Join(rootDir, p)
	matches, err := filepath.Glob(absPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", p, err)
	}

	if len(matches) > 0 {
		var results []string
		for _, m := range matches {
			rel, _ := filepath.Rel(rootDir, m)
			results = append(results, rel)
		}
		return results, nil
	}

	// Not a glob — check if file/dir exists
	absPath := filepath.Join(rootDir, p)
	if _, err := os.Stat(absPath); err != nil {
		return nil, fmt.Errorf("path %q does not exist", p)
	}

	return []string{p}, nil
}
