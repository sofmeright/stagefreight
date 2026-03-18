package postbuild

import (
	"context"

	"github.com/PrPlanIT/StageFreight/src/build"
	"github.com/PrPlanIT/StageFreight/src/diag"
)

// PushRecoveryResult tells the caller whether a push failure was recoverable.
type PushRecoveryResult struct {
	Retry   bool   // true = recovery action succeeded, caller should retry
	Message string // diagnostic message for the caller to log
}

// RecoverPushFailure inspects a push failure and attempts vendor-specific
// recovery (e.g. creating a missing Harbor project). Returns whether the
// caller should retry the failed operation.
//
// execute.go owns retry mechanics (which tags, stderr reset). This function
// owns the vendor decision (is this recoverable? what action to take?).
func RecoverPushFailure(ctx context.Context, registries []build.RegistryTarget, stderr string) PushRecoveryResult {
	// Harbor: project-not-found → auto-create project
	if IsHarborProjectMissingPushError(registries, stderr) {
		if err := EnsureHarborProjects(ctx, registries); err != nil {
			diag.Warn("harbor: auto-create failed: %v", err)
			return PushRecoveryResult{Retry: false, Message: "harbor: project auto-create failed"}
		}
		return PushRecoveryResult{Retry: true, Message: "harbor: created missing project, retrying push"}
	}

	return PushRecoveryResult{Retry: false}
}

// PostPushHooks runs vendor-specific post-push actions (e.g. scan triggers).
// Best-effort — failures are warned, never fatal.
func PostPushHooks(ctx context.Context, registries []build.RegistryTarget) {
	TriggerHarborScans(ctx, registries)
}
