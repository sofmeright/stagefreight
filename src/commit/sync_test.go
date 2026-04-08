package commit

import (
	"testing"
)

// ── containsAction ────────────────────────────────────────────────────────────

func TestContainsAction(t *testing.T) {
	actions := []SyncAction{SyncFetch, SyncRebase, SyncPush}
	if !containsAction(actions, SyncRebase) {
		t.Error("expected containsAction to find SyncRebase")
	}
	if containsAction(actions, SyncNoop) {
		t.Error("expected containsAction to NOT find SyncNoop")
	}
	if containsAction(nil, SyncPush) {
		t.Error("expected containsAction to return false for nil slice")
	}
}
