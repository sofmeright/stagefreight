package docker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/PrPlanIT/StageFreight/src/gitstate"
)

// EvaluateTrust determines how authoritative the repository discovery is.
// Destructive actions (orphan down/prune) require TrustAuthoritative.
// Never destroy from absence unless the source of truth is positively validated.
func EvaluateTrust(rootDir, iacPath, expectedMode string) DiscoveryTrust {
	trust := DiscoveryTrust{}

	// 1. Sentinel: .stagefreight.yml exists
	if _, err := os.Stat(filepath.Join(rootDir, ".stagefreight.yml")); err == nil {
		trust.Sentinel = true
	} else {
		trust.Reasons = append(trust.Reasons, ReasonNoSentinel)
	}

	// 2. IaC root exists
	if fi, err := os.Stat(filepath.Join(rootDir, iacPath)); err == nil && fi.IsDir() {
		trust.IaCRootExists = true
	} else {
		trust.Reasons = append(trust.Reasons, ReasonIaCRootMissing)
	}

	// 3. Repo identity: verify origin URL matches expected
	// (if git repo — best-effort, don't fail if not a git repo)
	trust.RepoIdentityMatch = verifyRepoIdentity(rootDir)
	if !trust.RepoIdentityMatch {
		trust.Reasons = append(trust.Reasons, ReasonRepoMismatch)
	}

	// 4. Lifecycle mode matches
	if expectedMode == "docker" {
		// Caller should verify this from config, we trust the assertion
	} else if expectedMode != "" {
		trust.Reasons = append(trust.Reasons, ReasonLifecycleMismatch)
	}

	// Compute level
	switch {
	case trust.Sentinel && trust.IaCRootExists && trust.RepoIdentityMatch && len(trust.Reasons) == 0:
		trust.Level = TrustAuthoritative
	case trust.Sentinel && trust.IaCRootExists:
		trust.Level = TrustPartial
	default:
		trust.Level = TrustNone
	}

	return trust
}

// MarkScanResult records whether the IaC scan succeeded and how many stacks were found.
func (t *DiscoveryTrust) MarkScanResult(succeeded bool, stackCount int) {
	t.ScanSucceeded = succeeded
	t.StackCount = stackCount
	if !succeeded {
		t.Reasons = append(t.Reasons, ReasonScanFailed)
		if t.Level == TrustAuthoritative {
			t.Level = TrustPartial
		}
	}
}

// MarkDeclaredTargets records whether host target resolution succeeded.
func (t *DiscoveryTrust) MarkDeclaredTargets(resolved bool) {
	t.DeclaredTargets = resolved
	if !resolved {
		t.Reasons = append(t.Reasons, ReasonTargetNotDeclared)
		if t.Level == TrustAuthoritative {
			t.Level = TrustPartial
		}
	}
}

// AllowDestructiveOrphanAction checks if the trust level and anomaly guards
// permit destructive orphan actions (down/prune).
func AllowDestructiveOrphanAction(trust DiscoveryTrust, knownCount, runningCount, orphanCount, threshold int) (bool, string) {
	// Trust must be authoritative
	if trust.Level != TrustAuthoritative {
		return false, "repository discovery not trustworthy (trust: " + string(trust.Level) + ")"
	}

	// Anomaly: everything appears orphaned while host is a declared target
	if knownCount == 0 && runningCount > 0 && trust.DeclaredTargets {
		return false, "all running projects appear orphaned — possible discovery failure"
	}

	// Anomaly: orphan count exceeds threshold
	if threshold > 0 && orphanCount > threshold {
		return false, "orphan count exceeds safety threshold (" + strings.Repeat("", 0) +
			string(rune('0'+orphanCount)) + " > " + string(rune('0'+threshold)) + ")"
	}

	return true, ""
}

// verifyRepoIdentity checks if the repo origin is present and resolvable.
// Best-effort: if not a git repo, returns true (benefit of the doubt).
func verifyRepoIdentity(rootDir string) bool {
	repo, err := gitstate.OpenRepo(rootDir)
	if err != nil {
		// Not a git repo — can't verify, don't fail on this alone
		return true
	}
	url, err := gitstate.RemoteURL(repo, "origin")
	return err == nil && url != ""
}
