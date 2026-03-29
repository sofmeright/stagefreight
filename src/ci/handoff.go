package ci

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/diag"
)

// HandoffDecision describes the outcome of a handoff evaluation.
type HandoffDecision int

const (
	// HandoffNone — no handoff needed (continue mode, or no commit created).
	HandoffNone HandoffDecision = iota
	// HandoffRestart — new commit pushed, requesting pipeline restart on repaired revision.
	HandoffRestart
	// HandoffSuppressed — handoff would fire, but this pipeline already originated
	// from a repaired-revision handoff (depth >= 1). One-hop guard prevents infinite loops.
	HandoffSuppressed
	// HandoffFail — repair was needed but policy says fail if handoff can't proceed.
	HandoffFail
)

// HandoffResult describes what happened when deps attempted a pipeline handoff.
type HandoffResult struct {
	Decision  HandoffDecision
	CommitSHA string // SHA of the new commit deps created
	Triggered bool   // true if a new pipeline was triggered via provider API
	Stale     bool   // true if current pipeline SHA != branch HEAD (should stop shipping)
	Depth     int    // current handoff depth from SF_CI_HANDOFF_DEPTH
}

// HandoffDepth reads SF_CI_HANDOFF_DEPTH from the environment.
// Returns 0 when unset or unparseable (original pipeline, not a handoff).
func HandoffDepth() int {
	v := os.Getenv("SF_CI_HANDOFF_DEPTH")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// EvaluateHandoff checks whether a dependency commit requires pipeline handoff.
//
// Handoff fires only when ALL of these are true:
//  1. A new commit SHA was created and pushed
//  2. Handoff mode is restart_pipeline
//  3. Handoff depth is 0 (original pipeline, not already a rerun)
//
// When depth >= 1 and a new commit was still created, the decision is
// HandoffSuppressed — the one-hop guard prevents infinite restart loops.
//
// When handoff is "continue", the decision is always HandoffNone.
// When handoff is "fail" and depth >= 1, the decision is HandoffFail.
func EvaluateHandoff(ciCtx *CIContext, handoff config.DependencyHandoff, commitSHA string) *HandoffResult {
	depth := HandoffDepth()
	result := &HandoffResult{
		CommitSHA: commitSHA,
		Depth:     depth,
	}

	if strings.TrimSpace(commitSHA) == "" {
		result.Decision = HandoffNone
		return result
	}

	if handoff == config.HandoffContinue {
		result.Decision = HandoffNone
		return result
	}

	// Check staleness — applies regardless of depth
	if ciCtx.Branch != "" && ciCtx.SHA != "" {
		headSHA := resolveRemoteHead(ciCtx.Branch)
		if headSHA != "" && headSHA != ciCtx.SHA {
			result.Stale = true
		}
	}

	// One-hop guard: only allow restart from the original pipeline
	if depth >= 1 {
		if handoff == config.HandoffFail {
			result.Decision = HandoffFail
		} else {
			result.Decision = HandoffSuppressed
		}
		return result
	}

	// Depth 0, restart_pipeline mode, commit was created and pushed
	result.Decision = HandoffRestart

	// TODO: provider-specific pipeline trigger API
	// For now, skip_ci: false on the push naturally triggers a new pipeline.
	// When built-in trigger exists, set SF_CI_HANDOFF_DEPTH=1,
	// SF_CI_HANDOFF_FROM_SHA=<ciCtx.SHA>, SF_CI_HANDOFF_TO_SHA=<commitSHA>
	// on the triggered pipeline.

	return result
}

// IsBranchHeadFresh returns true if the CI SHA still matches the remote branch HEAD.
// Shipping actions (release, docs sync, catalog publish) must call this before
// performing externally visible mutations. Returns true when not in CI or when
// the branch cannot be resolved (fail-open for local runs).
func IsBranchHeadFresh(ciCtx *CIContext) bool {
	if !ciCtx.IsCI() {
		return true // local runs are always "fresh"
	}
	if ciCtx.IsTag() {
		return true // tags are immutable, always fresh
	}
	if ciCtx.Branch == "" || ciCtx.SHA == "" {
		return true // can't check, fail open
	}

	headSHA := resolveRemoteHead(ciCtx.Branch)
	if headSHA == "" {
		diag.Warn("freshness: remote lookup failed (branch=%s), allowing execution", ciCtx.Branch)
		return true // can't resolve, fail open
	}

	fresh := headSHA == ciCtx.SHA
	diag.Debug(true, "freshness: branch=%s local=%s remote=%s fresh=%t",
		ciCtx.Branch, shortSHA(ciCtx.SHA), shortSHA(headSHA), fresh)
	return fresh
}

// resolveRemoteHead returns the current HEAD SHA for a branch from the remote.
// Uses git ls-remote to avoid needing a full fetch.
func resolveRemoteHead(branch string) string {
	cmd := exec.Command("git", "ls-remote", "origin", "refs/heads/"+branch)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) < 1 {
		return ""
	}
	return parts[0]
}

// shortSHA safely truncates a SHA to 8 chars. Returns as-is if shorter.
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// FormatHandoffMessage returns a human-readable message for the handoff result.
// Returns empty string when no message is needed.
func FormatHandoffMessage(r *HandoffResult) string {
	switch r.Decision {
	case HandoffRestart:
		return fmt.Sprintf("dependency repair created commit %s; repaired-revision handoff required", r.CommitSHA)
	case HandoffSuppressed:
		return fmt.Sprintf("dependency repair created commit %s, but pipeline handoff is suppressed (depth=%d) — this pipeline already originated from a repaired-revision handoff", r.CommitSHA, r.Depth)
	case HandoffFail:
		return fmt.Sprintf("dependency repair created commit %s at handoff depth %d — failing because repair should not still be needed after handoff", r.CommitSHA, r.Depth)
	default:
		return ""
	}
}
