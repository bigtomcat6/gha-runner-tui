package state

import "testing"

func TestResolveHealthSleepingGoneRunnerIsHealthy(t *testing.T) {
	t.Parallel()

	health := ResolveHealth(HealthInputs{
		Systemd:      SystemdActive,
		Loop:         LoopSleeping,
		Container:    ContainerNone,
		GitHub:       GitHubGone,
		Busy:         BusyNA,
		StatePresent: true,
	})

	if health != HealthHealthy {
		t.Fatalf("expected healthy, got %q", health)
	}
}

func TestResolveHealthBusyRunnerIsRunning(t *testing.T) {
	t.Parallel()

	health := ResolveHealth(HealthInputs{
		Systemd:      SystemdActive,
		Loop:         LoopRunningJob,
		Container:    ContainerRunning,
		GitHub:       GitHubOnline,
		Busy:         BusyYes,
		StatePresent: true,
	})

	if health != HealthRunning {
		t.Fatalf("expected running, got %q", health)
	}
}

func TestResolveHealthMissingStateWithActiveServiceIsWarning(t *testing.T) {
	t.Parallel()

	health := ResolveHealth(HealthInputs{
		Systemd:      SystemdActive,
		Loop:         LoopUnknown,
		Container:    ContainerUnknown,
		GitHub:       GitHubUnknown,
		Busy:         BusyUnknown,
		StatePresent: false,
	})

	if health != HealthWarning {
		t.Fatalf("expected warning, got %q", health)
	}
}

func TestResolveHealthFailedServiceIsUnhealthy(t *testing.T) {
	t.Parallel()

	health := ResolveHealth(HealthInputs{
		Systemd:      SystemdFailed,
		Loop:         LoopUnknown,
		Container:    ContainerNone,
		GitHub:       GitHubGone,
		Busy:         BusyNA,
		StatePresent: true,
	})

	if health != HealthUnhealthy {
		t.Fatalf("expected unhealthy, got %q", health)
	}
}

func TestResolveHealthWaitingHostWithNoContainerIsHealthy(t *testing.T) {
	t.Parallel()

	health := ResolveHealth(HealthInputs{
		Systemd:      SystemdActive,
		Loop:         LoopWaitingHost,
		Container:    ContainerNone,
		GitHub:       GitHubUnknown,
		Busy:         BusyNA,
		StatePresent: true,
	})

	if health != HealthHealthy {
		t.Fatalf("expected healthy, got %q", health)
	}
}

func TestResolveHealthFailedServiceBeatsWaitingHost(t *testing.T) {
	t.Parallel()

	health := ResolveHealth(HealthInputs{
		Systemd:      SystemdFailed,
		Loop:         LoopWaitingHost,
		Container:    ContainerNone,
		GitHub:       GitHubUnknown,
		Busy:         BusyNA,
		StatePresent: true,
	})

	if health != HealthUnhealthy {
		t.Fatalf("expected unhealthy, got %q", health)
	}
}

func TestNormalizeContainerStatus(t *testing.T) {
	t.Parallel()

	cases := map[string]ContainerStatus{
		"created":    ContainerCreated,
		"running":    ContainerRunning,
		"restarting": ContainerRestarting,
		"paused":     ContainerPaused,
		"exited":     ContainerExited,
		"dead":       ContainerDead,
		"removing":   ContainerRemoving,
		"":           ContainerNone,
		"bogus":      ContainerUnknown,
	}
	for in, want := range cases {
		if got := NormalizeContainerStatus(in); got != want {
			t.Errorf("NormalizeContainerStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
