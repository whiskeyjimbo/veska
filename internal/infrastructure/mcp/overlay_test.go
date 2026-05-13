package mcp

import (
	"slices"
	"testing"
)

// mockDaemonState implements DaemonState for testing.
type mockDaemonState struct {
	syncing     bool
	reconciling bool
}

func (m *mockDaemonState) IsSyncing() bool     { return m.syncing }
func (m *mockDaemonState) IsReconciling() bool { return m.reconciling }

func TestBuildEnvelope_NoStagingRead(t *testing.T) {
	env := BuildEnvelope(false, false, nil)
	if env.IncludedStaging {
		t.Error("expected IncludedStaging=false when stagingRead=false")
	}
	if len(env.DegradedReasons) != 0 {
		t.Errorf("expected no degraded reasons, got %v", env.DegradedReasons)
	}
}

func TestBuildEnvelope_StagingReadOK(t *testing.T) {
	env := BuildEnvelope(true, true, nil)
	if !env.IncludedStaging {
		t.Error("expected IncludedStaging=true when stagingRead=true and stagingOK=true")
	}
	if len(env.DegradedReasons) != 0 {
		t.Errorf("expected no degraded reasons, got %v", env.DegradedReasons)
	}
}

func TestBuildEnvelope_StagingReadFailed(t *testing.T) {
	env := BuildEnvelope(true, false, nil)
	if env.IncludedStaging {
		t.Error("expected IncludedStaging=false when stagingOK=false")
	}
	if len(env.DegradedReasons) != 1 || env.DegradedReasons[0] != "staging_unavailable" {
		t.Errorf("expected degraded_reasons=[staging_unavailable], got %v", env.DegradedReasons)
	}
}

func TestBuildEnvelope_StateIsSyncing(t *testing.T) {
	state := &mockDaemonState{syncing: true}
	env := BuildEnvelope(false, false, state)
	if slices.Contains(env.DegradedReasons, "startup_resync") {
		return
	}
	t.Errorf("expected startup_resync in degraded_reasons, got %v", env.DegradedReasons)
}

func TestBuildEnvelope_StateIsReconciling(t *testing.T) {
	state := &mockDaemonState{reconciling: true}
	env := BuildEnvelope(false, false, state)
	if slices.Contains(env.DegradedReasons, "wake_reconciling") {
		return
	}
	t.Errorf("expected wake_reconciling in degraded_reasons, got %v", env.DegradedReasons)
}

func TestBuildEnvelope_SyncingAndStagingUnavailable(t *testing.T) {
	state := &mockDaemonState{syncing: true}
	env := BuildEnvelope(true, false, state)
	if env.IncludedStaging {
		t.Error("expected IncludedStaging=false")
	}
	hasResync := false
	hasUnavail := false
	for _, r := range env.DegradedReasons {
		if r == "startup_resync" {
			hasResync = true
		}
		if r == "staging_unavailable" {
			hasUnavail = true
		}
	}
	if !hasResync {
		t.Errorf("expected startup_resync in degraded_reasons, got %v", env.DegradedReasons)
	}
	if !hasUnavail {
		t.Errorf("expected staging_unavailable in degraded_reasons, got %v", env.DegradedReasons)
	}
}

func TestBuildEnvelope_NilState(t *testing.T) {
	env := BuildEnvelope(false, false, nil)
	for _, r := range env.DegradedReasons {
		if r == "startup_resync" || r == "wake_reconciling" {
			t.Errorf("unexpected reason %q with nil state", r)
		}
	}
}

func TestAppendDegradedReason_DoesNotMutateOriginal(t *testing.T) {
	original := []string{"foo", "bar"}
	result := AppendDegradedReason(original, "baz")

	if len(original) != 2 {
		t.Errorf("original slice was mutated, now has %d elements", len(original))
	}
	if len(result) != 3 {
		t.Errorf("expected 3 elements in result, got %d", len(result))
	}
	if result[2] != "baz" {
		t.Errorf("expected last element to be 'baz', got %q", result[2])
	}
}

func TestAppendDegradedReason_EmptySlice(t *testing.T) {
	result := AppendDegradedReason([]string{}, "reason1")
	if len(result) != 1 || result[0] != "reason1" {
		t.Errorf("unexpected result: %v", result)
	}
}
