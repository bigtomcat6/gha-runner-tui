package state

type SystemdStatus string

const (
	SystemdActive   SystemdStatus = "active"
	SystemdInactive SystemdStatus = "inactive"
	SystemdFailed   SystemdStatus = "failed"
	SystemdUnknown  SystemdStatus = "unknown"
)

type ContainerStatus string

const (
	ContainerNone    ContainerStatus = "none"
	ContainerCreated ContainerStatus = "created"
	ContainerRunning ContainerStatus = "running"
	ContainerExited  ContainerStatus = "exited"
	ContainerDead    ContainerStatus = "dead"
	ContainerUnknown ContainerStatus = "unknown"
)

type GitHubStatus string

const (
	GitHubOnline  GitHubStatus = "online"
	GitHubOffline GitHubStatus = "offline"
	GitHubGone    GitHubStatus = "gone"
	GitHubUnknown GitHubStatus = "unknown"
)

type BusyStatus string

const (
	BusyYes     BusyStatus = "yes"
	BusyNo      BusyStatus = "no"
	BusyUnknown BusyStatus = "unknown"
	BusyNA      BusyStatus = "-"
)

type CombinedHealth string

const (
	HealthHealthy   CombinedHealth = "healthy"
	HealthRunning   CombinedHealth = "running"
	HealthWarning   CombinedHealth = "warning"
	HealthUnhealthy CombinedHealth = "unhealthy"
	HealthUnknown   CombinedHealth = "unknown"
)

type HealthInputs struct {
	Systemd      SystemdStatus
	Loop         LoopStatus
	Container    ContainerStatus
	GitHub       GitHubStatus
	Busy         BusyStatus
	StatePresent bool
	LastExitCode *int
}

func NormalizeSystemdStatus(value string) SystemdStatus {
	switch value {
	case "active":
		return SystemdActive
	case "inactive", "disabled":
		return SystemdInactive
	case "failed":
		return SystemdFailed
	default:
		return SystemdUnknown
	}
}

func NormalizeContainerStatus(value string) ContainerStatus {
	switch value {
	case "created":
		return ContainerCreated
	case "running":
		return ContainerRunning
	case "exited":
		return ContainerExited
	case "dead":
		return ContainerDead
	case "", "none":
		return ContainerNone
	default:
		return ContainerUnknown
	}
}

func NormalizeGitHubStatus(value string) GitHubStatus {
	switch value {
	case "online":
		return GitHubOnline
	case "offline":
		return GitHubOffline
	case "gone":
		return GitHubGone
	default:
		return GitHubUnknown
	}
}

func ResolveHealth(in HealthInputs) CombinedHealth {
	if in.Systemd == SystemdFailed || in.Loop == LoopFailed {
		return HealthUnhealthy
	}

	if in.GitHub == GitHubGone && in.Container == ContainerExited && in.LastExitCode != nil && *in.LastExitCode != 0 {
		return HealthUnhealthy
	}

	if in.GitHub == GitHubGone && in.Loop == LoopSleeping && in.Container == ContainerNone {
		return HealthHealthy
	}

	if in.Container == ContainerRunning && in.Busy == BusyYes {
		return HealthRunning
	}

	if in.GitHub == GitHubOnline && in.Container == ContainerRunning {
		return HealthHealthy
	}

	if in.GitHub == GitHubOffline && in.Container == ContainerRunning {
		return HealthWarning
	}

	if in.GitHub == GitHubOnline && in.Container == ContainerNone {
		return HealthWarning
	}

	if !in.StatePresent && in.Systemd == SystemdActive {
		return HealthWarning
	}

	if !in.StatePresent && in.Systemd == SystemdInactive {
		return HealthUnknown
	}

	return HealthUnknown
}
