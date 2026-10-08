package state

import (
	"os"
	"path/filepath"
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

func TestParseLoopStateAcceptsWaitingHost(t *testing.T) {
	t.Parallel()

	state, err := ParseLoopState([]byte(`{"state":"waiting-host"}`))
	if err != nil {
		t.Fatalf("ParseLoopState returned error: %v", err)
	}
	if state.State != LoopWaitingHost {
		t.Fatalf("expected waiting-host, got %q", state.State)
	}
}

func TestLoadLoopStateStableConsumerFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.json")
	for _, status := range []LoopStatus{LoopDisabled, LoopStopped, LoopActive, LoopSleeping, LoopRegistering, LoopStarting, LoopRunningJob, LoopCleaning, LoopBackoff, LoopFailed, LoopWaitingHost} {
		t.Run(string(status), func(t *testing.T) {
			data := `{"profile":"p","repo":"me/app","state":"` + string(status) + `","health":"warning","last_transition_at":"2026-10-07T00:00:00Z","last_runner_name":"runner","last_container_id":"id","last_container_name":"container","last_exit_code":0,"restart_count":2,"future":{"large":9007199254740993}}`
			if err := os.WriteFile(path, []byte(data), 0o640); err != nil {
				t.Fatal(err)
			}
			got, err := LoadLoopState(path)
			if err != nil || got.State != status || got.Profile != "p" || got.Repo != "me/app" || got.Health != "warning" || got.RestartCount != 2 || got.LastRunnerName != "runner" || got.LastContainerID != "id" || got.LastContainerName != "container" || got.LastExitCode == nil || *got.LastExitCode != 0 || got.LastError != nil || got.LastTransitionAt.Format(time.RFC3339) != "2026-10-07T00:00:00Z" {
				t.Fatalf("consumer fields changed: %+v err=%v", got, err)
			}
		})
	}
}
