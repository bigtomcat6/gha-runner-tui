package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
	systemdpkg "gha-runner-tui/internal/systemd"
)

type SystemdClient interface {
	Status(ctx context.Context, service string) (systemdpkg.ServiceStatus, error)
}

type DockerClient interface {
	CurrentOrLatest(ctx context.Context, prefix string) (dockerpkg.ContainerInfo, error)
	ListByPrefix(ctx context.Context, prefix string) ([]dockerpkg.ContainerInfo, error)
	Inspect(ctx context.Context, idOrName string) (dockerpkg.ContainerDetails, error)
}

type GitHubClient interface {
	ListRepoRunners(ctx context.Context, owner, repo string) ([]gh.Runner, error)
	ListOrgRunners(ctx context.Context, org string) ([]gh.Runner, error)
}

type ProfileSnapshot struct {
	Profile          config.Profile
	Service          systemdpkg.ServiceStatus
	Loop             state.LoopState
	LoopPresent      bool
	Container        dockerpkg.ContainerInfo
	GitHubRunner     *gh.Runner
	GitHubState      state.GitHubStatus
	BusyState        state.BusyStatus
	Health           state.CombinedHealth
	Errors           []string
	DisplayLoopState state.LoopStatus
}

type Dashboard struct {
	Config            config.GlobalConfig
	Profiles          []ProfileSnapshot
	ProfileErrors     []config.ProfileLoadError
	MigrationWarnings []string
}

type Service struct {
	ConfigPath string
	Systemd    SystemdClient
	Docker     DockerClient
	GitHub     GitHubClient
}

func (s Service) LoadDashboard(ctx context.Context) (Dashboard, error) {
	cfg, err := config.LoadGlobalConfig(s.ConfigPath)
	if err != nil {
		return Dashboard{}, err
	}

	migrationResults, err := config.MigrateProfilesAccessMode(cfg.Paths.ProfilesDir)
	if err != nil {
		return Dashboard{}, err
	}

	profiles, profileErrors, err := config.LoadProfiles(cfg.Paths.ProfilesDir)
	if err != nil {
		return Dashboard{}, err
	}
	if len(profiles) == 0 {
		legacyProfiles, legacyErrors, legacyErr := config.DiscoverLegacyProfiles("")
		if legacyErr != nil {
			return Dashboard{}, legacyErr
		}
		profiles = legacyProfiles
		profileErrors = append(profileErrors, legacyErrors...)
	}

	snapshots := make([]ProfileSnapshot, 0, len(profiles))
	for _, profile := range profiles {
		snapshots = append(snapshots, s.loadProfile(ctx, profile))
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Profile.Name < snapshots[j].Profile.Name
	})

	return Dashboard{
		Config:            cfg,
		Profiles:          snapshots,
		ProfileErrors:     profileErrors,
		MigrationWarnings: migrationWarnings(migrationResults),
	}, nil
}

func (s Service) loadProfile(ctx context.Context, profile config.Profile) ProfileSnapshot {
	snapshot := ProfileSnapshot{
		Profile: profile,
		Container: dockerpkg.ContainerInfo{
			State: state.ContainerNone,
		},
		GitHubState: state.GitHubUnknown,
		BusyState:   state.BusyUnknown,
	}
	var matchedContainers []dockerpkg.ContainerInfo

	serviceStatus, err := s.Systemd.Status(ctx, profile.Service.Name)
	if err != nil {
		snapshot.Errors = append(snapshot.Errors, "systemd: "+err.Error())
		serviceStatus = systemdpkg.ServiceStatus{Active: state.SystemdUnknown}
	}
	snapshot.Service = serviceStatus

	loopState, loopErr := state.LoadLoopState(profile.Loop.StateFile)
	if loopErr == nil {
		snapshot.Loop = loopState
		snapshot.LoopPresent = true
	} else if !errors.Is(loopErr, os.ErrNotExist) {
		snapshot.Errors = append(snapshot.Errors, "state: "+loopErr.Error())
	}

	if snapshot.LoopPresent {
		snapshot.DisplayLoopState = snapshot.Loop.State
	} else {
		snapshot.DisplayLoopState = inferredLoopState(snapshot.Service)
	}

	if s.Docker != nil {
		container, matches, err := s.matchContainers(ctx, profile, snapshot.Loop)
		if err != nil {
			snapshot.Errors = append(snapshot.Errors, "docker: "+err.Error())
		} else {
			snapshot.Container = container
			matchedContainers = matches
		}
	}

	if s.GitHub != nil {
		target, targetErr := profile.ResolveTarget()
		if targetErr != nil {
			snapshot.Errors = append(snapshot.Errors, "config: "+targetErr.Error())
			snapshot.GitHubState = state.GitHubUnknown
			snapshot.BusyState = state.BusyUnknown
		} else {
			var (
				runners []gh.Runner
				err     error
			)
			switch target.Scope {
			case config.TargetScopeOrganization:
				runners, err = s.GitHub.ListOrgRunners(ctx, target.OrgSlug)
			default:
				runners, err = s.GitHub.ListRepoRunners(ctx, target.Owner, target.Repo)
			}
			if err != nil {
				snapshot.Errors = append(snapshot.Errors, "github: "+err.Error())
				snapshot.GitHubState = state.GitHubUnknown
				snapshot.BusyState = state.BusyUnknown
			} else {
				exactRunnerName := snapshot.Loop.LastRunnerName
				if exactRunnerName == "" {
					exactRunnerName = profile.Runner.NamePrefix
				}
				match := gh.MatchRunner(runners, exactRunnerName, profile.Runner.NamePrefix)
				snapshot.GitHubRunner = match
				snapshot.GitHubState = gh.RunnerState(match)
				snapshot.BusyState = gh.BusyState(match)
			}
		}
	}

	lastExitCode := snapshot.Loop.LastExitCode
	snapshot.Health = state.ResolveHealth(state.HealthInputs{
		Systemd:      snapshot.Service.Active,
		Loop:         snapshot.DisplayLoopState,
		Container:    snapshot.Container.State,
		GitHub:       snapshot.GitHubState,
		Busy:         snapshot.BusyState,
		StatePresent: snapshot.LoopPresent,
		LastExitCode: lastExitCode,
	})
	if warning, health := containerMatchWarning(matchedContainers); warning != "" {
		snapshot.Errors = append(snapshot.Errors, warning)
		if health == state.HealthUnhealthy || snapshot.Health != state.HealthUnhealthy {
			snapshot.Health = health
		}
	}
	return snapshot
}

func inferredLoopState(service systemdpkg.ServiceStatus) state.LoopStatus {
	if !service.Enabled {
		return state.LoopDisabled
	}

	switch service.Active {
	case state.SystemdActive:
		return state.LoopActive
	case state.SystemdInactive:
		return state.LoopStopped
	case state.SystemdFailed:
		return state.LoopFailed
	default:
		return state.LoopUnknown
	}
}

func (p ProfileSnapshot) ErrorSummary() string {
	return strings.Join(p.Errors, "; ")
}

func migrationWarnings(results []config.ProfileMigrationResult) []string {
	warnings := make([]string, 0)
	for _, result := range results {
		switch result.Status {
		case config.ProfileMigrationSkipped, config.ProfileMigrationFailed:
			warnings = append(warnings, fmt.Sprintf("%s: %s", result.Path, result.Message))
		}
	}
	return warnings
}

func (s Service) matchContainer(ctx context.Context, profile config.Profile, loopState state.LoopState) (dockerpkg.ContainerInfo, error) {
	container, _, err := s.matchContainers(ctx, profile, loopState)
	return container, err
}

func (s Service) matchContainers(ctx context.Context, profile config.Profile, loopState state.LoopState) (dockerpkg.ContainerInfo, []dockerpkg.ContainerInfo, error) {
	if s.Docker == nil {
		return dockerpkg.ContainerInfo{State: state.ContainerNone}, nil, nil
	}

	expectedRunnerName := loopState.LastRunnerName
	if expectedRunnerName == "" {
		expectedRunnerName = profile.Runner.NamePrefix
	}

	if profile.Docker.ContainerNamePrefix == "" {
		if loopState.LastContainerName == "" {
			return dockerpkg.ContainerInfo{State: state.ContainerNone}, nil, nil
		}
		details, err := s.Docker.Inspect(ctx, loopState.LastContainerName)
		if err != nil {
			return dockerpkg.ContainerInfo{State: state.ContainerNone}, nil, nil
		}
		container := containerFromDetails(details)
		return container, []dockerpkg.ContainerInfo{container}, nil
	}

	containers, err := s.Docker.ListByPrefix(ctx, profile.Docker.ContainerNamePrefix)
	if err != nil {
		return dockerpkg.ContainerInfo{State: state.ContainerNone}, nil, err
	}
	if len(containers) == 0 {
		if loopState.LastContainerName == "" {
			return dockerpkg.ContainerInfo{State: state.ContainerNone}, nil, nil
		}
		details, inspectErr := s.Docker.Inspect(ctx, loopState.LastContainerName)
		if inspectErr != nil {
			return dockerpkg.ContainerInfo{State: state.ContainerNone}, nil, nil
		}
		container := containerFromDetails(details)
		return container, []dockerpkg.ContainerInfo{container}, nil
	}

	matched := make([]dockerpkg.ContainerInfo, 0, len(containers))
	imageMatches := make([]dockerpkg.ContainerInfo, 0, len(containers))
	for _, container := range containers {
		details, err := s.Docker.Inspect(ctx, container.Name)
		if err != nil {
			continue
		}
		container = mergeContainerDetails(container, details)
		if expectedRunnerName != "" && details.Env["RUNNER_NAME"] == expectedRunnerName {
			matched = append(matched, container)
			continue
		}
		if profile.Docker.Image != "" && container.Image == profile.Docker.Image {
			imageMatches = append(imageMatches, container)
		}
	}

	switch {
	case len(matched) > 0:
		return preferContainer(matched, loopState.LastContainerName), matched, nil
	case expectedRunnerName != "":
		return dockerpkg.ContainerInfo{State: state.ContainerNone}, nil, nil
	case len(imageMatches) > 0:
		return preferContainer(imageMatches, loopState.LastContainerName), imageMatches, nil
	default:
		return containers[0], []dockerpkg.ContainerInfo{containers[0]}, nil
	}
}

func mergeContainerDetails(container dockerpkg.ContainerInfo, details dockerpkg.ContainerDetails) dockerpkg.ContainerInfo {
	if details.ID != "" {
		container.ID = details.ID
	}
	if details.Name != "" {
		container.Name = details.Name
	}
	if details.Image != "" {
		container.Image = details.Image
	}
	if details.State != "" {
		container.State = details.State
	}
	return container
}

func containerFromDetails(details dockerpkg.ContainerDetails) dockerpkg.ContainerInfo {
	return dockerpkg.ContainerInfo{
		ID:    details.ID,
		Name:  details.Name,
		Image: details.Image,
		State: details.State,
	}
}

func preferContainer(containers []dockerpkg.ContainerInfo, preferredName string) dockerpkg.ContainerInfo {
	for _, container := range containers {
		if preferredName != "" && container.Name == preferredName {
			return container
		}
	}
	if len(containers) == 0 {
		return dockerpkg.ContainerInfo{State: state.ContainerNone}
	}
	return containers[0]
}

func containerMatchWarning(matches []dockerpkg.ContainerInfo) (string, state.CombinedHealth) {
	if len(matches) <= 1 {
		return "", ""
	}

	names := make([]string, 0, len(matches))
	running := 0
	for _, container := range matches {
		names = append(names, container.Name)
		if container.State == state.ContainerRunning {
			running++
		}
	}

	if running > 1 {
		return fmt.Sprintf("multiple running containers matched profile: %s", strings.Join(names, ", ")), state.HealthUnhealthy
	}
	return fmt.Sprintf("multiple containers matched profile: %s", strings.Join(names, ", ")), state.HealthWarning
}
