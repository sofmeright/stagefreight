package gitstate

import (
	"fmt"
	"strings"
)

// StateClass classifies a RepoState into a named state for the transition table.
//
// S-class states are stable — transitions are permitted from them.
// X-class states are hard stops — no transitions are allowed until resolved.
//
// Transition table:
//
//	S0 CLEAN_SYNCED  → no-op
//	S1 CLEAN_AHEAD   → PUSH       → S0
//	S2 CLEAN_BEHIND  → FAST_FORWARD → S0
//	S3 DIVERGED      → REPLAY     → S1 → PUSH → S0
//
//	X1 DETACHED_HEAD  → hard stop (checkout a branch)
//	X2 DIRTY_WORKTREE → hard stop (commit or stash)
//	X3 NO_UPSTREAM    → hard stop (set upstream tracking)
type StateClass string

const (
	// Stable states — transitions permitted
	StateCleanSynced StateClass = "CLEAN_SYNCED" // S0: ahead=0, behind=0, clean
	StateCleanAhead  StateClass = "CLEAN_AHEAD"  // S1: ahead>0, behind=0
	StateCleanBehind StateClass = "CLEAN_BEHIND" // S2: ahead=0, behind>0
	StateDiverged    StateClass = "DIVERGED"     // S3: ahead>0, behind>0

	// Blocked states — hard stops
	StateDetachedHEAD  StateClass = "DETACHED_HEAD"   // X1
	StateDirtyWorktree StateClass = "DIRTY_WORKTREE"  // X2
	StateNoUpstream    StateClass = "NO_UPSTREAM"     // X3
)

// IsValid returns true for S-class states where transitions are permitted.
func (c StateClass) IsValid() bool {
	switch c {
	case StateCleanSynced, StateCleanAhead, StateCleanBehind, StateDiverged:
		return true
	}
	return false
}

// String implements Stringer.
func (c StateClass) String() string { return string(c) }

// Classify derives the StateClass from a RepoState snapshot.
// Classification order: blocked states are checked first (fail-fast),
// then stable states by ahead/behind counts.
//
// This function is pure — same input always produces the same output.
func Classify(s RepoState) StateClass {
	if s.DetachedHEAD {
		return StateDetachedHEAD
	}
	if !s.WorktreeClean {
		return StateDirtyWorktree
	}
	if !s.UpstreamConfigured {
		return StateNoUpstream
	}
	switch {
	case s.AheadCount > 0 && s.BehindCount > 0:
		return StateDiverged
	case s.AheadCount > 0:
		return StateCleanAhead
	case s.BehindCount > 0:
		return StateCleanBehind
	default:
		return StateCleanSynced
	}
}

// RequireAttached returns ErrDetachedHEAD if the repo is in detached HEAD state.
func RequireAttached(state RepoState) error {
	if state.DetachedHEAD {
		return ErrDetachedHEAD
	}
	return nil
}

// RequireClean returns ErrDirtyWorktree if the worktree has uncommitted changes.
func RequireClean(state RepoState) error {
	if !state.WorktreeClean {
		return ErrDirtyWorktree
	}
	return nil
}

// RequireUpstream returns ErrNoUpstream if no upstream tracking branch is configured.
func RequireUpstream(state RepoState) error {
	if !state.UpstreamConfigured {
		return ErrNoUpstream
	}
	return nil
}

// RequireValid enforces that none of the X-class blocked states are active.
// This is the universal pre-mutation gate — call it before any operation
// that touches the repository.
func RequireValid(state RepoState) error {
	if err := RequireAttached(state); err != nil {
		return err
	}
	if err := RequireClean(state); err != nil {
		return err
	}
	if err := RequireUpstream(state); err != nil {
		return err
	}
	return nil
}

// RequireState verifies that the current state falls into one of the allowed
// StateClasses for the given action. Returns ErrInvalidTransition if not.
func RequireState(state RepoState, action string, allowed ...StateClass) error {
	class := Classify(state)
	for _, a := range allowed {
		if class == a {
			return nil
		}
	}
	return &ErrInvalidTransition{From: class, Action: action, Allowed: allowed}
}

// ErrInvalidTransition is returned when an operation is requested from a state
// that does not permit it. This is the definitive "impossible transition" error.
type ErrInvalidTransition struct {
	From    StateClass
	Action  string
	Allowed []StateClass
}

func (e *ErrInvalidTransition) Error() string {
	parts := make([]string, len(e.Allowed))
	for i, a := range e.Allowed {
		parts[i] = string(a)
	}
	allowed := strings.Join(parts, ", ")
	if e.Action != "" {
		return fmt.Sprintf(
			"invalid transition: state %s does not permit %s (valid from: %s)",
			e.From, e.Action, allowed,
		)
	}
	return fmt.Sprintf(
		"invalid state %s (expected one of: %s)",
		e.From, allowed,
	)
}

// TransitionEvent is emitted by the Engine for every state transition.
// Consumers may log, forward to a UI, or record for audit.
//
// JSON encoding is intentional — these are the structured facts that CI logs
// and diagnostic tooling consume.
type TransitionEvent struct {
	From   StateClass `json:"from"`
	Action string     `json:"action"`
	To     StateClass `json:"to,omitempty"`
	Note   string     `json:"note,omitempty"`
}
