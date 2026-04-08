package commit

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// DryRunBackend prints the commit plan without side effects.
type DryRunBackend struct {
	RootDir string
}

// Execute simulates the commit and returns what would happen.
func (d *DryRunBackend) Execute(_ context.Context, plan *Plan, conventional bool) (*Result, error) {
	var files []string

	switch plan.StageMode {
	case StageExplicit:
		files = plan.Paths

	case StageAll, StageStaged:
		repo, err := gitstate.OpenRepo(d.RootDir)
		if err != nil {
			return nil, fmt.Errorf("opening repo: %w", err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			return nil, fmt.Errorf("opening worktree: %w", err)
		}
		status, err := wt.Status()
		if err != nil {
			return nil, fmt.Errorf("reading status: %w", err)
		}

		if plan.StageMode == StageAll {
			files = gitstate.AllChangedFiles(status)
		} else {
			files = gitstate.StagedFiles(status)
		}
	}

	return &Result{
		Message: plan.Message(conventional),
		Files:   files,
		NoOp:    len(files) == 0,
		Backend: "dry-run",
	}, nil
}
