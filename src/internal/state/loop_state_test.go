package state

import (
	"testing"
	"time"
)

func TestParseLoopStateParsesContract(t *testing.T) {
	t.Parallel()

	state, err := ParseLoopState([]byte(`{
  "profile": "remind-me-swift",
  "repo": "bigtomcat6/remind-me",
  "state": "sleeping",
  "health": "healthy",
  "last_transition_at": "2026-06-01T12:30:12Z",
  "last_runner_name": "remind-me-swift-20260601-123012",
  "last_container_id": "8f3a12345678",
  "last_container_name": "gha-remind-me-swift-20260601-123012",
  "last_exit_code": 0,
  "last_error": null,
  "restart_count": 12
}`))
	if err != nil {
		t.Fatalf("ParseLoopState returned error: %v", err)
	}

	if state.State != LoopSleeping {
		t.Fatalf("expected sleeping, got %q", state.State)
	}
	if state.LastExitCode == nil || *state.LastExitCode != 0 {
		t.Fatalf("expected last exit code 0, got %#v", state.LastExitCode)
	}
	if !state.LastTransitionAt.Equal(time.Date(2026, 6, 1, 12, 30, 12, 0, time.UTC)) {
		t.Fatalf("unexpected transition time: %v", state.LastTransitionAt)
	}
}

func TestParseLoopStateRejectsUnknownState(t *testing.T) {
	t.Parallel()

	_, err := ParseLoopState([]byte(`{"state":"teleporting"}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
