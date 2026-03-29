package runtime

import (
	"context"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
)

// Capability declares what a backend can do.
// The runtime validates required capabilities during the Validate phase.
type Capability string

const (
	CapReconcile          Capability = "reconcile"
	CapDryRun             Capability = "dry-run"
	CapImpactAnalysis     Capability = "impact-analysis"
	CapClusterAuth        Capability = "cluster-auth"
	CapForgeAuth          Capability = "forge-auth"
	CapStructuredProgress Capability = "structured-progress"
	CapPlanExecute        Capability = "plan-execute"
)

// LifecycleBackend is the full lifecycle contract.
// Backends participate in ALL phases — not just Execute.
//
// Rules:
//  1. Must not write to stdout/stderr — return LifecyclePlan/LifecycleResult instead.
//  2. Must not mutate global state — all state goes through rctx.Resolved.
//  3. Must register cleanup functions via rctx.Resolved.AddCleanup().
type LifecycleBackend interface {
	Name() string
	Capabilities() []Capability

	// Validate checks backend prerequisites (CLI availability, config completeness).
	Validate(ctx context.Context, cfg *config.Config, rctx *RuntimeContext) error

	// Prepare sets up the execution environment (kubeconfig, docker context, etc.).
	// Must register cleanup via rctx.Resolved.AddCleanup().
	Prepare(ctx context.Context, cfg *config.Config, rctx *RuntimeContext) error

	// Plan computes what will be done. Must be deterministic:
	// identical config + runtime inputs → identical output.
	Plan(ctx context.Context, cfg *config.Config, rctx *RuntimeContext) (*LifecyclePlan, error)

	// Execute dispatches planned actions. Must be idempotent for reconciliation backends:
	// repeated execution must converge to the same state without side effects.
	Execute(ctx context.Context, plan *LifecyclePlan, rctx *RuntimeContext) (*LifecycleResult, error)

	// Cleanup removes backend-specific ephemeral state.
	Cleanup(rctx *RuntimeContext)
}

// LifecyclePlan is the output of the Plan phase — what will be done.
// Dry-run renders this without calling Execute.
type LifecyclePlan struct {
	Mode    string
	Backend string
	Actions []PlannedAction
	DryRun  bool
}

// PlannedAction describes a single action to be executed.
type PlannedAction struct {
	Name        string // e.g. "reconcile flux-system/infrastructure" or "anchorage/grafana"
	Description string // human-readable reason (e.g. "IaC files changed since last deployment")
	Order       int
	Action      string            // intended action: "reconcile", "up", "noop", "error"
	Metadata    map[string]string // backend-specific detail (host, hash, drift tier, etc.)
}

// LifecycleResult is the output of the Execute phase.
type LifecycleResult struct {
	Actions []ActionResult
}

// ActionResult describes the outcome of a single executed action.
type ActionResult struct {
	Name     string
	Success  bool
	Duration time.Duration
	Message  string
}
