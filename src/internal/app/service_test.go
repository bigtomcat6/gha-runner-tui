package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
	systemdpkg "gha-runner-tui/internal/systemd"
)

type fakeSystemd struct {
	status systemdpkg.ServiceStatus
	err    error
}

func (f fakeSystemd) Status(context.Context, string) (systemdpkg.ServiceStatus, error) {
	return f.status, f.err
}

type fakeDocker struct {
	container  dockerpkg.ContainerInfo
	containers []dockerpkg.ContainerInfo
	inspects   map[string]dockerpkg.ContainerDetails
	err        error
}

func (f fakeDocker) CurrentOrLatest(context.Context, string) (dockerpkg.ContainerInfo, error) {
	return f.container, f.err
}

func (f fakeDocker) ListByPrefix(context.Context, string) ([]dockerpkg.ContainerInfo, error) {
	if f.containers != nil {
		return f.containers, f.err
	}
	if f.container.Name == "" && f.container.State == "" {
		return nil, f.err
	}
	return []dockerpkg.ContainerInfo{f.container}, f.err
}

func (f fakeDocker) Inspect(_ context.Context, idOrName string) (dockerpkg.ContainerDetails, error) {
	if f.inspects == nil {
		return dockerpkg.ContainerDetails{}, f.err
	}
	return f.inspects[idOrName], f.err
}

type fakeGitHub struct {
	repoRunners []gh.Runner
	orgRunners  []gh.Runner
	err         error
	repoCalls   []string
	orgCalls    []string
}

func (f *fakeGitHub) ListRepoRunners(_ context.Context, owner, repo string) ([]gh.Runner, error) {
	f.repoCalls = append(f.repoCalls, owner+"/"+repo)
	return f.repoRunners, f.err
}

func (f *fakeGitHub) ListOrgRunners(_ context.Context, org string) ([]gh.Runner, error) {
	f.orgCalls = append(f.orgCalls, org)
	return f.orgRunners, f.err
}

func TestLoadDashboardBuildsHealthySleepingSnapshot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll profiles returned error: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll state returned error: %v", err)
	}

	cfgPath := filepath.Join(root, "config.yaml")
	profilePath := filepath.Join(profilesDir, "remind-me-swift.yaml")
	statePath := filepath.Join(stateDir, "remind-me-swift.json")

	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+profilesDir+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(`
name: remind-me-swift
repo:
  owner: bigtomcat6
  name: remind-me
service:
  name: gha-remind-me-swift.service
runner:
  name_prefix: remind-me-swift
docker:
  container_name_prefix: gha-remind-me-swift
loop:
  state_file: `+statePath+`
`), 0o600); err != nil {
		t.Fatalf("WriteFile profile returned error: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(`{"profile":"remind-me-swift","state":"sleeping","last_runner_name":"remind-me-swift-1"}`), 0o600); err != nil {
		t.Fatalf("WriteFile state returned error: %v", err)
	}

	service := Service{
		ConfigPath: cfgPath,
		Systemd: fakeSystemd{
			status: systemdpkg.ServiceStatus{Active: state.SystemdActive, Enabled: true},
		},
		Docker: fakeDocker{
			container: dockerpkg.ContainerInfo{State: state.ContainerNone},
		},
		GitHub: &fakeGitHub{},
	}

	dashboard, err := service.LoadDashboard(context.Background())
	if err != nil {
		t.Fatalf("LoadDashboard returned error: %v", err)
	}
	if len(dashboard.Profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(dashboard.Profiles))
	}
	if dashboard.Profiles[0].Health != state.HealthHealthy {
		t.Fatalf("expected healthy, got %q", dashboard.Profiles[0].Health)
	}
	if dashboard.Profiles[0].DisplayLoopState != state.LoopSleeping {
		t.Fatalf("expected sleeping, got %q", dashboard.Profiles[0].DisplayLoopState)
	}
}

func TestLoadDashboardUsesInferredLoopStateWhenStateFileMissing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	cfgPath := filepath.Join(root, "config.yaml")
	profilePath := filepath.Join(profilesDir, "remind-me-swift.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+profilesDir+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(`
name: remind-me-swift
repo:
  owner: bigtomcat6
  name: remind-me
service:
  name: gha-remind-me-swift.service
docker:
  container_name_prefix: gha-remind-me-swift
loop:
  state_file: /tmp/missing.json
`), 0o600); err != nil {
		t.Fatalf("WriteFile profile returned error: %v", err)
	}

	service := Service{
		ConfigPath: cfgPath,
		Systemd: fakeSystemd{
			status: systemdpkg.ServiceStatus{Active: state.SystemdInactive, Enabled: false},
		},
		Docker: fakeDocker{
			container: dockerpkg.ContainerInfo{State: state.ContainerNone},
		},
		GitHub: &fakeGitHub{},
	}

	dashboard, err := service.LoadDashboard(context.Background())
	if err != nil {
		t.Fatalf("LoadDashboard returned error: %v", err)
	}
	if dashboard.Profiles[0].DisplayLoopState != state.LoopDisabled {
		t.Fatalf("expected disabled inferred state, got %q", dashboard.Profiles[0].DisplayLoopState)
	}
}

func TestLoadProfileMarksDuplicateRunningContainersUnhealthy(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		Name: "remind-me-exp",
		Repo: config.RepoConfig{
			Owner: "bigtomcat6",
			Name:  "remind-me",
		},
		Service: config.ServiceConfig{
			Name: "gha-remind-me-exp.service",
		},
		Runner: config.RunnerConfig{
			NamePrefix: "gha-remind-me-exp",
		},
		Docker: config.DockerProfile{
			ContainerNamePrefix: "gha-remind-me-",
			Image:               "gha-runner-base:latest",
		},
	}

	service := Service{
		Systemd: fakeSystemd{
			status: systemdpkg.ServiceStatus{Active: state.SystemdActive, Enabled: true},
		},
		Docker: fakeDocker{
			containers: []dockerpkg.ContainerInfo{
				{Name: "gha-remind-me-200", State: state.ContainerRunning, Image: "gha-runner-base:latest"},
				{Name: "gha-remind-me-100", State: state.ContainerRunning, Image: "gha-runner-base:latest"},
				{Name: "gha-remind-me-050", State: state.ContainerRunning, Image: "gha-runner-base:latest"},
			},
			inspects: map[string]dockerpkg.ContainerDetails{
				"gha-remind-me-200": {
					Name:  "gha-remind-me-200",
					Image: "gha-runner-base:latest",
					State: state.ContainerRunning,
					Env:   map[string]string{"RUNNER_NAME": "gha-remind-me-exp"},
				},
				"gha-remind-me-100": {
					Name:  "gha-remind-me-100",
					Image: "gha-runner-base:latest",
					State: state.ContainerRunning,
					Env:   map[string]string{"RUNNER_NAME": "gha-remind-me-exp"},
				},
				"gha-remind-me-050": {
					Name:  "gha-remind-me-050",
					Image: "gha-runner-base:latest",
					State: state.ContainerRunning,
					Env:   map[string]string{"RUNNER_NAME": "o-tokyo-s2-remind-me-base"},
				},
			},
		},
		GitHub: &fakeGitHub{
			repoRunners: []gh.Runner{
				{Name: "gha-remind-me-exp", Status: state.GitHubOnline},
			},
		},
	}

	snapshot := service.loadProfile(context.Background(), profile)

	if snapshot.Container.Name != "gha-remind-me-200" {
		t.Fatalf("expected latest matching container, got %+v", snapshot.Container)
	}
	if snapshot.Health != state.HealthUnhealthy {
		t.Fatalf("expected unhealthy due to duplicate running containers, got %q", snapshot.Health)
	}
	if !strings.Contains(snapshot.ErrorSummary(), "multiple running containers matched profile") {
		t.Fatalf("expected duplicate-container error, got %q", snapshot.ErrorSummary())
	}
}

func TestLoadDashboardListsOrganizationRunnersForOrganizationProfile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll profiles returned error: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll state returned error: %v", err)
	}

	cfgPath := filepath.Join(root, "config.yaml")
	statePath := filepath.Join(stateDir, "example-org-swift.json")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+profilesDir+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(profilesDir, "example-org-swift.yaml"), []byte(`
name: example-org-swift
target:
  scope: organization
  org: Example Org
runner_group:
  name: example-org-swift
  create: true
  visibility: private
service:
  name: gha-example-org-swift.service
runner:
  environment: swift
  name_prefix: example-org-swift
docker:
  container_name_prefix: gha-example-org-swift
loop:
  state_file: `+statePath+`
`), 0o600); err != nil {
		t.Fatalf("WriteFile profile returned error: %v", err)
	}
	if err := os.WriteFile(statePath, []byte(`{"profile":"example-org-swift","state":"sleeping","last_runner_name":"example-org-swift-1"}`), 0o600); err != nil {
		t.Fatalf("WriteFile state returned error: %v", err)
	}

	github := &fakeGitHub{
		orgRunners: []gh.Runner{{Name: "example-org-swift-1", Status: state.GitHubOnline}},
	}
	service := Service{
		ConfigPath: cfgPath,
		Systemd: fakeSystemd{
			status: systemdpkg.ServiceStatus{Active: state.SystemdActive, Enabled: true},
		},
		Docker: fakeDocker{
			container: dockerpkg.ContainerInfo{State: state.ContainerNone},
		},
		GitHub: github,
	}

	dashboard, err := service.LoadDashboard(context.Background())
	if err != nil {
		t.Fatalf("LoadDashboard returned error: %v", err)
	}
	if len(github.orgCalls) != 1 || github.orgCalls[0] != "example-org" {
		t.Fatalf("expected org runner call, got %v", github.orgCalls)
	}
	if len(github.repoCalls) != 0 {
		t.Fatalf("did not expect repo runner calls, got %v", github.repoCalls)
	}
	if dashboard.Profiles[0].GitHubRunner == nil {
		t.Fatal("expected GitHub runner match")
	}
}

func TestLoadDashboardMigratesLegacyDockerAccessMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll profiles returned error: %v", err)
	}

	cfgPath := filepath.Join(root, "config.yaml")
	profilePath := filepath.Join(profilesDir, "legacy.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+profilesDir+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(`
name: legacy
repo:
  owner: example
  name: repo
service:
  name: gha-legacy.service
runner:
  name_prefix: legacy
docker:
  image: runner:latest
  container_name_prefix: gha-legacy
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
loop:
  state_file: /tmp/legacy.json
`), 0o600); err != nil {
		t.Fatalf("WriteFile profile returned error: %v", err)
	}

	service := Service{
		ConfigPath: cfgPath,
		Systemd:    fakeSystemd{status: systemdpkg.ServiceStatus{Active: state.SystemdInactive}},
		Docker:     fakeDocker{container: dockerpkg.ContainerInfo{State: state.ContainerNone}},
		GitHub:     &fakeGitHub{},
	}

	dashboard, err := service.LoadDashboard(context.Background())
	if err != nil {
		t.Fatalf("LoadDashboard returned error: %v", err)
	}
	if len(dashboard.Profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(dashboard.Profiles))
	}
	if len(dashboard.MigrationWarnings) != 0 {
		t.Fatalf("expected no migration warnings, got %v", dashboard.MigrationWarnings)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile profile returned error: %v", err)
	}
	if !strings.Contains(string(data), "access_mode: host-socket") {
		t.Fatalf("expected migrated access_mode, got:\n%s", string(data))
	}
}

func TestLoadDashboardReportsMigrationWarningsForAmbiguousProfiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll profiles returned error: %v", err)
	}

	cfgPath := filepath.Join(root, "config.yaml")
	profilePath := filepath.Join(profilesDir, "legacy.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+profilesDir+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(`
name: legacy
repo:
  owner: example
  name: repo
service:
  name: gha-legacy.service
runner:
  name_prefix: legacy
docker:
  image: runner:latest
  container_name_prefix: gha-legacy
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
    - /run/user/1001/docker.sock:/var/run/docker.sock
  env:
    DOCKER_HOST: unix:///var/run/docker.sock
loop:
  state_file: /tmp/legacy.json
`), 0o600); err != nil {
		t.Fatalf("WriteFile profile returned error: %v", err)
	}

	service := Service{
		ConfigPath: cfgPath,
		Systemd:    fakeSystemd{status: systemdpkg.ServiceStatus{Active: state.SystemdInactive}},
		Docker:     fakeDocker{container: dockerpkg.ContainerInfo{State: state.ContainerNone}},
		GitHub:     &fakeGitHub{},
	}

	dashboard, err := service.LoadDashboard(context.Background())
	if err != nil {
		t.Fatalf("LoadDashboard returned error: %v", err)
	}
	if len(dashboard.Profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(dashboard.Profiles))
	}
	if len(dashboard.MigrationWarnings) != 1 {
		t.Fatalf("expected 1 migration warning, got %v", dashboard.MigrationWarnings)
	}
	if !strings.Contains(dashboard.MigrationWarnings[0], "ambiguous") {
		t.Fatalf("expected ambiguous migration warning, got %v", dashboard.MigrationWarnings)
	}
}
