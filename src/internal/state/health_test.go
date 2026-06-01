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
