package commit

import (
	"fmt"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// SyncAction names a single step in the repository convergence plan.
type SyncAction string

const (
	SyncSetUpstream SyncAction = "set-upstream"  // configure tracking branch on first push
	SyncFetch       SyncAction = "fetch"          // fetch remote before rebase or fast-forward
	SyncFastForward SyncAction = "fast-forward"   // merge --ff-only to catch up to upstream
	SyncRebase      SyncAction = "rebase"         // rebase local commits onto upstream
	SyncPush        SyncAction = "push"           // push to remote
	SyncNoop        SyncAction = "noop"           // already up to date, nothing to do
)

// SyncResult is the outcome of a push operation.
type SyncResult struct {
	ActionsExecuted []SyncAction
	PushedRef       string // remote name that was pushed to
	Noop            bool   // true only when SyncNoop was the sole action
}

// containsAction returns true if action appears in the slice.
func containsAction(actions []SyncAction, action SyncAction) bool {
	for _, a := range actions {
		if a == action {
			return true
		}
	}
	return false
}

// Push synchronizes the current branch with its remote via the go-git engine.
func (g *GitBackend) Push(opts PushOptions) (*SyncResult, error) {
	return g.pushViaEngine(opts)
}

// pushViaEngine synchronizes the current branch using go-git — no git binary required.
func (g *GitBackend) pushViaEngine(opts PushOptions) (*SyncResult, error) {
	session, err := gitstate.OpenSyncSession(g.RootDir)
	if err != nil {
		return nil, fmt.Errorf("opening sync session: %w", err)
	}
	engine := NewEngine(session, EngineOptions{
		RebaseOnDiverge: opts.RebaseOnDiverge,
		Remote:          opts.Remote,
		Refspec:         opts.Refspec,
		OnEvent:         g.onSyncEvent,
	})
	return engine.Sync()
}

// onSyncEvent routes a state-machine transition event to OnCommitLine.
// This stays in the presentation callback path — no direct output from backend.
func (g *GitBackend) onSyncEvent(ev gitstate.TransitionEvent) {
	if g.OnCommitLine == nil {
		return
	}
	msg := fmt.Sprintf("transition: %s → %s", ev.From, ev.Action)
	if ev.To != "" {
		msg += fmt.Sprintf(" → %s", ev.To)
	}
	if ev.Note != "" {
		msg += " [" + ev.Note + "]"
	}
	g.OnCommitLine("sync", msg)
}
