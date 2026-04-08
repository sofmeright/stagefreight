package commit

import (
	"fmt"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// Engine orchestrates repository convergence as an explicit state machine.
//
// Every mutation is a validated transition between named states.
// No operation is performed outside of a valid (from, action, to) triple.
//
// Transition table:
//
//	CLEAN_SYNCED  → (no-op)                              → CLEAN_SYNCED
//	CLEAN_BEHIND  → FAST_FORWARD                         → CLEAN_SYNCED
//	CLEAN_AHEAD   → PUSH                                 → CLEAN_SYNCED
//	DIVERGED      → REPLAY → CLEAN_AHEAD → PUSH          → CLEAN_SYNCED
//
// Blocked states (hard stops — no transitions allowed):
//
//	DETACHED_HEAD, DIRTY_WORKTREE, NO_UPSTREAM
//
// When RebaseOnDiverge is false, DIVERGED returns ErrInvalidTransition instead
// of attempting replay — the caller must resolve the divergence manually.
type Engine struct {
	session *gitstate.SyncSession
	opts    EngineOptions
}

// EngineOptions configures Engine behaviour.
type EngineOptions struct {
	// RebaseOnDiverge: when true, DIVERGED state triggers Replay + Push.
	// When false, DIVERGED returns ErrInvalidTransition — the user must
	// resolve the divergence manually before pushing.
	RebaseOnDiverge bool

	// Remote overrides the remote resolved from the session state.
	// Empty means "use whatever the branch tracking config says".
	Remote string

	// Refspec is an optional explicit push refspec (e.g. "HEAD:refs/heads/main").
	// Empty means push the current branch to its upstream.
	Refspec string

	// OnEvent receives a structured event for every state transition.
	// If nil, events are silently dropped.
	// Use this for CI log output, tracing, or UI state panels.
	OnEvent func(gitstate.TransitionEvent)
}

// NewEngine creates an Engine bound to the given SyncSession.
func NewEngine(session *gitstate.SyncSession, opts EngineOptions) *Engine {
	return &Engine{session: session, opts: opts}
}

// Sync drives the repository to CLEAN_SYNCED through the minimum valid
// transition path determined by the current state class.
//
// Algorithm:
//  1. Classify initial state — fail immediately on any X-class blocked state
//  2. Fetch + reclassify (fetch is always required for an accurate view)
//  3. Dispatch the single valid transition for the resulting state class
//
// Returns the SyncResult describing exactly what happened.
func (e *Engine) Sync() (*SyncResult, error) {
	result := &SyncResult{}
	state := e.session.State()
	preClass := gitstate.Classify(state)

	// NO_UPSTREAM is handled as a formal first-push transition before the
	// standard pre-flight checks (RequireValid would block NO_UPSTREAM).
	if preClass == gitstate.StateNoUpstream {
		return e.doFirstPush(result, preClass, state)
	}

	// Pre-flight: blocked states (DETACHED_HEAD, DIRTY_WORKTREE) are hard stops.
	if err := gitstate.RequireAttached(state); err != nil {
		return result, err
	}
	if err := gitstate.RequireClean(state); err != nil {
		return result, err
	}

	// Fetch — always first. Updates remote-tracking refs so the state
	// classification after fetch reflects the real upstream.
	remote := e.remote(state)
	if err := e.session.Fetch(remote); err != nil {
		return result, fmt.Errorf("fetch: %w", err)
	}
	result.ActionsExecuted = append(result.ActionsExecuted, SyncFetch)

	// session.Fetch() calls Refresh() internally on success.
	// Read the updated state for post-fetch classification.
	state = e.session.State()
	class := gitstate.Classify(state)

	// Emit FETCH event once, after reclassification, with the post-fetch state.
	e.emit(gitstate.TransitionEvent{
		From:   preClass,
		Action: "FETCH",
		To:     class,
		Note:   "remote: " + remote,
	})

	// Dispatch the single valid transition for the post-fetch state class.
	switch class {

	case gitstate.StateCleanSynced:
		result.Noop = true
		result.ActionsExecuted = append(result.ActionsExecuted, SyncNoop)
		e.emit(gitstate.TransitionEvent{
			From: class, Action: "NOOP", To: class,
			Note: "already up to date with " + state.UpstreamRef,
		})
		return result, nil

	case gitstate.StateCleanBehind:
		return e.doFastForward(result, class, state)

	case gitstate.StateCleanAhead:
		return e.doPush(result, class, state)

	case gitstate.StateDiverged:
		if !e.opts.RebaseOnDiverge {
			return result, &gitstate.ErrInvalidTransition{
				From:   class,
				Action: "PUSH",
				Allowed: []gitstate.StateClass{
					gitstate.StateCleanSynced,
					gitstate.StateCleanAhead,
				},
			}
		}
		return e.doReplayThenPush(result, class, state)

	default:
		// Post-fetch state became a blocked X-class. This can happen if the
		// worktree was dirtied concurrently — surface it as a clean error.
		return result, &gitstate.ErrInvalidTransition{
			From:    class,
			Action:  "SYNC",
			Allowed: []gitstate.StateClass{gitstate.StateCleanSynced, gitstate.StateCleanAhead, gitstate.StateCleanBehind, gitstate.StateDiverged},
		}
	}
}

// doFastForward executes the CLEAN_BEHIND → FAST_FORWARD → CLEAN_SYNCED transition.
func (e *Engine) doFastForward(result *SyncResult, class gitstate.StateClass, state gitstate.RepoState) (*SyncResult, error) {
	// Explicit guard: FAST_FORWARD is only valid from CLEAN_BEHIND.
	if err := gitstate.RequireState(state, "FAST_FORWARD", gitstate.StateCleanBehind); err != nil {
		return result, err
	}

	e.emit(gitstate.TransitionEvent{
		From: class, Action: "FAST_FORWARD",
		Note: fmt.Sprintf("%d commit(s) behind %s", state.BehindCount, state.UpstreamRef),
	})
	if err := e.session.FastForward(e.remote(state)); err != nil {
		return result, fmt.Errorf("fast-forward: %w", err)
	}
	result.ActionsExecuted = append(result.ActionsExecuted, SyncFastForward)

	if err := e.session.Refresh(); err != nil {
		return result, fmt.Errorf("refreshing state after fast-forward: %w", err)
	}
	newClass := gitstate.Classify(e.session.State())
	e.emit(gitstate.TransitionEvent{From: class, Action: "FAST_FORWARD", To: newClass})
	return result, nil
}

// doPush executes the CLEAN_AHEAD → PUSH → CLEAN_SYNCED transition.
func (e *Engine) doPush(result *SyncResult, class gitstate.StateClass, state gitstate.RepoState) (*SyncResult, error) {
	// Explicit guard: PUSH is only valid from CLEAN_AHEAD.
	if err := gitstate.RequireState(state, "PUSH", gitstate.StateCleanAhead); err != nil {
		return result, err
	}

	e.emit(gitstate.TransitionEvent{
		From: class, Action: "PUSH",
		Note: fmt.Sprintf("%d commit(s) ahead of %s", state.AheadCount, state.UpstreamRef),
	})
	if err := e.session.Push(e.remote(state), e.opts.Refspec, false); err != nil {
		return result, fmt.Errorf("push: %w", err)
	}
	result.ActionsExecuted = append(result.ActionsExecuted, SyncPush)
	result.PushedRef = e.remote(state)

	if err := e.session.Refresh(); err != nil {
		return result, fmt.Errorf("refreshing state after push: %w", err)
	}
	newClass := gitstate.Classify(e.session.State())
	e.emit(gitstate.TransitionEvent{From: class, Action: "PUSH", To: newClass})
	return result, nil
}

// doReplayThenPush executes the DIVERGED → REPLAY → CLEAN_AHEAD → PUSH → CLEAN_SYNCED chain.
func (e *Engine) doReplayThenPush(result *SyncResult, class gitstate.StateClass, state gitstate.RepoState) (*SyncResult, error) {
	// Explicit guard: REPLAY is only valid from DIVERGED.
	if err := gitstate.RequireState(state, "REPLAY", gitstate.StateDiverged); err != nil {
		return result, err
	}

	e.emit(gitstate.TransitionEvent{
		From: class, Action: "REPLAY",
		Note: fmt.Sprintf("%d ahead, %d behind %s", state.AheadCount, state.BehindCount, state.UpstreamRef),
	})
	if err := Replay(e.session); err != nil {
		return result, fmt.Errorf("replay: %w", err)
	}
	result.ActionsExecuted = append(result.ActionsExecuted, SyncRebase)

	if err := e.session.Refresh(); err != nil {
		return result, fmt.Errorf("refreshing state after replay: %w", err)
	}
	postReplayState := e.session.State()
	postReplayClass := gitstate.Classify(postReplayState)
	e.emit(gitstate.TransitionEvent{From: class, Action: "REPLAY", To: postReplayClass})

	// Post-replay state MUST be CLEAN_AHEAD.
	// If it's not, the replay produced an unexpected tree — abort.
	if postReplayClass != gitstate.StateCleanAhead {
		return result, &gitstate.ErrInvalidTransition{
			From:    postReplayClass,
			Action:  "PUSH",
			Allowed: []gitstate.StateClass{gitstate.StateCleanAhead},
		}
	}

	return e.doPush(result, postReplayClass, postReplayState)
}

// doFirstPush executes the NO_UPSTREAM → FIRST_PUSH → CLEAN_SYNCED transition.
// This is the path for branches that have never been pushed — sets upstream tracking.
// A single event is emitted after completion with the post-push state as To.
func (e *Engine) doFirstPush(result *SyncResult, class gitstate.StateClass, state gitstate.RepoState) (*SyncResult, error) {
	remote := e.remote(state)
	if err := e.session.Push(remote, e.opts.Refspec, true); err != nil {
		return result, fmt.Errorf("first push to %s: %w", remote, err)
	}
	result.ActionsExecuted = append(result.ActionsExecuted, SyncSetUpstream, SyncPush)
	result.PushedRef = remote

	if err := e.session.Refresh(); err != nil {
		return result, fmt.Errorf("refreshing state after first push: %w", err)
	}
	newClass := gitstate.Classify(e.session.State())
	e.emit(gitstate.TransitionEvent{
		From:   class,
		Action: "FIRST_PUSH",
		To:     newClass,
		Note:   "upstream configured and pushed to " + remote,
	})
	return result, nil
}

// remote returns the effective remote: explicit override, or from session state.
func (e *Engine) remote(state gitstate.RepoState) string {
	if e.opts.Remote != "" {
		return e.opts.Remote
	}
	if state.RemoteName != "" {
		return state.RemoteName
	}
	return "origin"
}

// emit dispatches a TransitionEvent to the configured handler (no-op if nil).
func (e *Engine) emit(ev gitstate.TransitionEvent) {
	if e.opts.OnEvent != nil {
		e.opts.OnEvent(ev)
	}
}
