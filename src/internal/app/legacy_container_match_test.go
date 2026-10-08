package app

import (
	"context"
	"testing"

	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	"gha-runner-tui/internal/state"
)

type fakeDockerMatcher struct {
	containers []dockerpkg.ContainerInfo
	inspects   map[string]dockerpkg.ContainerDetails
}

func (f fakeDockerMatcher) ListManaged(context.Context, string) ([]dockerpkg.ContainerInfo, error) {
	return nil, nil
}
func (f fakeDockerMatcher) SlotHolders(context.Context) ([]dockerpkg.ContainerInfo, error) {
	return nil, nil
}

func TestMatchContainerUsesManagedLabelsWithoutInspect(t *testing.T) {
	managed := []dockerpkg.ContainerInfo{{Name: "latest", Profile: "p", RunnerName: "p-2", State: state.ContainerExited}, {Name: "current", Profile: "p", RunnerName: "p-1", State: state.ContainerRunning}, {Name: "other", Profile: "other", State: state.ContainerRunning}}
	s := Service{Docker: fakeDocker{managed: managed}}
	p := config.Profile{Name: "p"}
	for _, tc := range []struct{ last, want string }{{"current", "current"}, {"stale", "latest"}, {"", "latest"}} {
		got, matches, err := s.matchContainers(context.Background(), p, state.LoopState{LastContainerName: tc.last})
		if err != nil || got.Name != tc.want || len(matches) != 2 {
			t.Fatalf("labels: %+v %v %v", got, matches, err)
		}
	}
}

func TestLegacyMatchExcludesManagedOtherProfile(t *testing.T) {
	p := config.Profile{Name: "p", Runner: config.RunnerConfig{NamePrefix: "shared"}, Docker: config.DockerProfile{ContainerNamePrefix: "shared"}}
	other := dockerpkg.ContainerDetails{Name: "shared-other", State: state.ContainerRunning, Env: map[string]string{"RUNNER_NAME": "shared"}, Labels: map[string]string{"io.gha-runner-tui.managed": "true", "io.gha-runner-tui.profile": "other"}}
	for _, containers := range [][]dockerpkg.ContainerInfo{nil, {{Name: "shared-other", State: state.ContainerRunning}}} {
		s := Service{Docker: fakeDockerMatcher{containers: containers, inspects: map[string]dockerpkg.ContainerDetails{"shared-other": other}}}
		got, err := s.matchContainer(context.Background(), p, state.LoopState{LastContainerName: "shared-other"})
		if err != nil || got.State != state.ContainerNone {
			t.Fatalf("cross-profile match: %+v %v", got, err)
		}
	}
}

func (f fakeDockerMatcher) CurrentOrLatest(context.Context, string) (dockerpkg.ContainerInfo, error) {
	return dockerpkg.ContainerInfo{}, nil
}

func (f fakeDockerMatcher) ListByPrefix(context.Context, string) ([]dockerpkg.ContainerInfo, error) {
	return f.containers, nil
}

func (f fakeDockerMatcher) Inspect(_ context.Context, name string) (dockerpkg.ContainerDetails, error) {
	return f.inspects[name], nil
}

func TestMatchContainerPrefersRunnerNameWhenPrefixIsShared(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		Name: "remind-me-swift",
		Runner: config.RunnerConfig{
			NamePrefix: "o-tokyo-s2-remind-me-swift",
		},
		Docker: config.DockerProfile{
			ContainerNamePrefix: "gha-remind-me-",
			Image:               "gha-runner-swift:6.2",
		},
	}

	service := Service{
		Docker: fakeDockerMatcher{
			containers: []dockerpkg.ContainerInfo{
				{Name: "gha-remind-me-1", State: state.ContainerRunning, Image: "gha-runner-base:latest"},
				{Name: "gha-remind-me-2", State: state.ContainerRunning, Image: "gha-runner-swift:6.2"},
			},
			inspects: map[string]dockerpkg.ContainerDetails{
				"gha-remind-me-1": {Name: "gha-remind-me-1", Image: "gha-runner-base:latest", Env: map[string]string{"RUNNER_NAME": "o-tokyo-s2-remind-me-base"}},
				"gha-remind-me-2": {Name: "gha-remind-me-2", Image: "gha-runner-swift:6.2", Env: map[string]string{"RUNNER_NAME": "o-tokyo-s2-remind-me-swift"}},
			},
		},
	}

	container, err := service.matchContainer(context.Background(), profile, state.LoopState{})
	if err != nil {
		t.Fatalf("matchContainer returned error: %v", err)
	}
	if container.Name != "gha-remind-me-2" {
		t.Fatalf("expected swift container, got %+v", container)
	}
}

func TestMatchContainerDoesNotFallbackToSharedImageWhenRunnerNameIsMissing(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		Name: "remind-me-exp",
		Runner: config.RunnerConfig{
			NamePrefix: "gha-remind-me-exp",
		},
		Docker: config.DockerProfile{
			ContainerNamePrefix: "gha-remind-me-",
			Image:               "gha-runner-base:latest",
		},
	}

	service := Service{
		Docker: fakeDockerMatcher{
			containers: []dockerpkg.ContainerInfo{
				{Name: "gha-remind-me-1", State: state.ContainerRunning, Image: "gha-runner-base:latest"},
				{Name: "gha-remind-me-2", State: state.ContainerRunning, Image: "gha-runner-swift:6.2"},
			},
			inspects: map[string]dockerpkg.ContainerDetails{
				"gha-remind-me-1": {Name: "gha-remind-me-1", Image: "gha-runner-base:latest", Env: map[string]string{"RUNNER_NAME": "o-tokyo-s2-remind-me-base"}},
				"gha-remind-me-2": {Name: "gha-remind-me-2", Image: "gha-runner-swift:6.2", Env: map[string]string{"RUNNER_NAME": "o-tokyo-s2-remind-me-swift"}},
			},
		},
	}

	container, err := service.matchContainer(context.Background(), profile, state.LoopState{})
	if err != nil {
		t.Fatalf("matchContainer returned error: %v", err)
	}
	if container.State != state.ContainerNone {
		t.Fatalf("expected no container match when runner name is absent, got %+v", container)
	}
}
