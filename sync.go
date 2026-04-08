package commit

import (
	"fmt"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// SyncAction names a single step in the repository convergence plan.
type SyncAction string

const (
	SyncSetUpstream SyncAction = "set-upstream"  // configure tracking branch on first push
	SyncFetch       SyncAction = "fetch"          // fetch remote before rebase or fast-forward
	SyncFastForward SyncAction = "fast-forward"   // merge --ff-only to catch up to upstream
	SyncRebase      SyncAction = "rebase"         // controlled replay of local commits onto upstream
	SyncPush        SyncAction = "push"           // push to remote
	SyncNoop        SyncAction = "noop"           // already up to date, nothing to do
)

// SyncStep is one unit of work in a SyncPlan.
type SyncStep struct {
	Action SyncAction
	Reason string // human-readable rationale for this step
}

// SyncPlan is the resolved sequence of actions to converge local with remote.
// This is a planning / display structure — execution goes through Engine.
type SyncPlan struct {
	Steps           []SyncStep
	Remote          string
	Refspec         string // optional explicit refspec; empty = current branch
	RebaseOnDiverge bool   // when true, rebase instead of failing on diverge
}

// SyncResult is the outcome after executing a SyncPlan via Engine.
type SyncResult struct {
	ActionsExecuted []SyncAction
	PushedRef       string // remote name that was pushed to
	Noop            bool   // true only when SyncNoop was the sole action
}

// PlanSync produces a deterministic sequence of SyncSteps for display and
// dry-run purposes. Execution uses Engine.Sync() — not this plan.
//
// rebaseOnDiverge controls the DIVERGED case: when true the plan includes
// a rebase step; when false PlanSync returns an error describing what manual
// action is required.
func PlanSync(state gitstate.RepoState, remote, refspec string, rebaseOnDiverge bool) (SyncPlan, error) {
	plan := SyncPlan{Remote: remote, Refspec: refspec, RebaseOnDiverge: rebaseOnDiverge}

	if state.DetachedHEAD {
		return plan, fmt.Errorf("push refused: detached HEAD — checkout a named branch first")
	}

	if !state.UpstreamConfigured {
		// First push for this branch — set tracking on the way through.
		plan.Steps = append(plan.Steps,
			SyncStep{Action: SyncSetUpstream, Reason: "no upstream tracking branch configured"},
			SyncStep{Action: SyncPush, Reason: "push and configure tracking"},
		)
		return plan, nil
	}

	class := gitstate.Classify(state)
	switch class {
	case gitstate.StateDiverged:
		if !rebaseOnDiverge {
			return plan, fmt.Errorf(
				"push refused: branch %q has diverged from %s (%d ahead, %d behind) — "+
					"run: git pull --rebase %s",
				state.Branch, state.UpstreamRef, state.AheadCount, state.BehindCount, remote,
			)
		}
		plan.Steps = append(plan.Steps,
			SyncStep{Action: SyncFetch, Reason: fmt.Sprintf("fetch before replay (%d ahead, %d behind %s)", state.AheadCount, state.BehindCount, state.UpstreamRef)},
			SyncStep{Action: SyncRebase, Reason: fmt.Sprintf("replay commits onto %s", state.UpstreamRef)},
			SyncStep{Action: SyncPush, Reason: "push replayed commits"},
		)

	case gitstate.StateCleanBehind:
		// CLEAN_BEHIND means local is behind remote — fast-forward brings us up to date.
		// There is nothing local to push after the fast-forward.
		plan.Steps = append(plan.Steps,
			SyncStep{Action: SyncFetch, Reason: fmt.Sprintf("fetch before fast-forward (%d commit(s) behind %s)", state.BehindCount, state.UpstreamRef)},
			SyncStep{Action: SyncFastForward, Reason: fmt.Sprintf("fast-forward to %s", state.UpstreamRef)},
		)

	case gitstate.StateCleanAhead:
		plan.Steps = append(plan.Steps,
			SyncStep{Action: SyncPush, Reason: fmt.Sprintf("push %d commit(s)", state.AheadCount)},
		)

	default:
		// CLEAN_SYNCED or any unexpected class
		plan.Steps = append(plan.Steps,
			SyncStep{Action: SyncNoop, Reason: "already up to date with " + state.UpstreamRef},
		)
	}

	return plan, nil
}

// Push synchronizes the current branch with its remote using the convergence
// engine: classify state → fetch → dispatch valid transition.
//
// This is the shared implementation for both `commit --push` and `stagefreight push`.
// First-push (NO_UPSTREAM) is handled as a formal engine state — no bypass needed.
func (g *GitBackend) Push(opts PushOptions) (*SyncResult, error) {
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

// onSyncEvent forwards a transition event to the commit line handler if set.
// Events are formatted as "transition: FROM → ACTION → TO [note]" lines.
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

// extractRemote returns the remote name from an upstream ref like "origin/main".
func extractRemote(upstreamRef string) string {
	if idx := strings.Index(upstreamRef, "/"); idx >= 0 {
		return upstreamRef[:idx]
	}
	return "origin"
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
