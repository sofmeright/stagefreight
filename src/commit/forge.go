package commit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PrPlanIT/StageFreight/src/forge"
	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// ForgeBackend creates commits purely via forge API (no local git commit).
type ForgeBackend struct {
	RootDir     string
	ForgeClient forge.Forge
	Branch      string
}

// Execute resolves changed files, reads their content, and commits via forge API.
func (f *ForgeBackend) Execute(ctx context.Context, plan *Plan, conventional bool) (*Result, error) {
	// Open repo once — used for all gitstate-based file resolution.
	repo, err := gitstate.OpenRepo(f.RootDir)
	if err != nil {
		return nil, fmt.Errorf("opening repo: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("opening worktree: %w", err)
	}
	status, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("reading worktree status: %w", err)
	}

	// 1. Resolve file list
	var changes []FileChange
	switch plan.StageMode {
	case StageExplicit:
		for _, p := range plan.Paths {
			absPath := filepath.Join(f.RootDir, p)
			info, statErr := os.Stat(absPath)
			if os.IsNotExist(statErr) {
				changes = append(changes, FileChange{Path: p, Deleted: true})
				continue
			}
			if statErr != nil {
				return nil, fmt.Errorf("stat %s: %w", p, statErr)
			}
			if info.IsDir() {
				for _, c := range gitstate.ChangedFilesInDir(status, p) {
					changes = append(changes, FileChange{Path: c.Path, Deleted: c.Deleted})
				}
			} else {
				changes = append(changes, FileChange{Path: p})
			}
		}
	case StageAll:
		for _, c := range gitstate.ChangedFiles(status) {
			changes = append(changes, FileChange{Path: c.Path, Deleted: c.Deleted})
		}
	case StageStaged:
		for _, c := range gitstate.StagedChanges(status) {
			changes = append(changes, FileChange{Path: c.Path, Deleted: c.Deleted})
		}
	}

	// 2. No-op check
	if len(changes) == 0 {
		return &Result{NoOp: true}, nil
	}

	// 3. Build actions
	actions := make([]forge.FileAction, 0, len(changes))
	fileNames := make([]string, 0, len(changes))
	for _, c := range changes {
		fa := forge.FileAction{Path: c.Path, Delete: c.Deleted}
		if !c.Deleted {
			content, readErr := os.ReadFile(filepath.Join(f.RootDir, c.Path))
			if readErr != nil {
				return nil, fmt.Errorf("reading %s: %w", c.Path, readErr)
			}
			fa.Content = content
		}
		actions = append(actions, fa)
		fileNames = append(fileNames, c.Path)
	}

	// 4. Resolve current branch head for optimistic concurrency (forge-native)
	expectedSHA, err := f.ForgeClient.BranchHeadSHA(ctx, f.Branch)
	if err != nil {
		return nil, fmt.Errorf("resolving branch head: %w", err)
	}

	// 5. Commit via forge API
	commitResult, err := f.ForgeClient.CommitFiles(ctx, forge.CommitFilesOptions{
		Branch:      f.Branch,
		Message:     plan.Message(conventional),
		Files:       actions,
		ExpectedSHA: expectedSHA,
	})
	if err != nil {
		return nil, fmt.Errorf("forge commit: %w", err)
	}

	// 6. Return result with real remote SHA
	return &Result{
		SHA:     commitResult.SHA,
		Message: plan.Message(conventional),
		Files:   fileNames,
		Pushed:  true,
		Backend: fmt.Sprintf("forge (%s)", f.ForgeClient.Provider()),
	}, nil
}

// FileChange represents a file with its change status.
type FileChange struct {
	Path    string
	Deleted bool
}
