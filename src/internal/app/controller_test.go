package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
	"gha-runner-tui/internal/systemd"
)

type recordingRunner struct {
	calls []string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name
	for _, arg := range args {
		call += " " + arg
	}
	r.calls = append(r.calls, call)
	if name == "install" && len(args) >= 4 {
		data, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		parent := filepath.Dir(args[3])
		if err := os.Chmod(parent, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(args[3], data, 0o644); err != nil {
			return nil, err
		}
		_ = os.Chmod(parent, 0o555)
	}
	return []byte("ok\n"), nil
}

type scriptedRunner struct {
	calls   []string
	outputs map[string][]byte
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name
	for _, arg := range args {
		call += " " + arg
	}
	r.calls = append(r.calls, call)
	if out, ok := r.outputs[call]; ok {
		return out, nil
	}
	return []byte("ok\n"), nil
}

type syncGitHub struct {
	groups         []gh.RunnerGroup
	orgRunners     []gh.Runner
	groupRunners   map[int64][]gh.Runner
	createdGroups  []gh.RunnerGroup
	updatedGroups  []gh.RunnerGroup
	deletedGroupID int64
}

func (g *syncGitHub) ListRepoRunners(context.Context, string, string) ([]gh.Runner, error) {
	return nil, nil
}

func (g *syncGitHub) ListOrgRunners(context.Context, string) ([]gh.Runner, error) {
	return g.orgRunners, nil
}

func (g *syncGitHub) ListOrgRunnerGroups(context.Context, string) ([]gh.RunnerGroup, error) {
	return g.groups, nil
}

func (g *syncGitHub) ListOrgRunnerGroupRunners(_ context.Context, _ string, id int64) ([]gh.Runner, error) {
	return g.groupRunners[id], nil
}

func (g *syncGitHub) CreateOrgRunnerGroup(_ context.Context, _ string, name, visibility string) (gh.RunnerGroup, error) {
	group := gh.RunnerGroup{ID: 42, Name: name, Visibility: visibility, AllowsPublicRepositories: false}
	g.createdGroups = append(g.createdGroups, group)
	return group, nil
}

func (g *syncGitHub) UpdateOrgRunnerGroup(_ context.Context, _ string, id int64, name, visibility string) (gh.RunnerGroup, error) {
	group := gh.RunnerGroup{ID: id, Name: name, Visibility: visibility, AllowsPublicRepositories: false}
	g.updatedGroups = append(g.updatedGroups, group)
	return group, nil
}

func (g *syncGitHub) DeleteOrgRunnerGroup(_ context.Context, _ string, id int64) error {
	g.deletedGroupID = id
	return nil
}

func TestCreateProfileUsesLegacyLayoutWhenLegacyTokenExists(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	legacyDir := filepath.Join(root, "gha-runner")
	systemdDir := filepath.Join(root, "systemd")
	tokenFile := filepath.Join(legacyDir, "github_pat")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll legacyDir returned error: %v", err)
	}
	if err := os.MkdirAll(systemdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll systemdDir returned error: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("WriteFile token returned error: %v", err)
	}

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		filepath.Join(root, "missing-config.yaml"),
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyEnvDir = legacyDir
	manager.LegacyTokenFile = tokenFile
	manager.LegacyLoopBinary = "/usr/local/bin/gha-ephemeral-loop"

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Name:                "bigtomcat6-exp",
		RepoOwner:           "bigtomcat6",
		RepoName:            "bigtomcat6",
		RunnerLabels:        []string{"self-hosted", "linux", "o-tokyo-s2-exp"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-bigtomcat6-exp.service",
		ContainerNamePrefix: "gha-bigtomcat6",
		CPUs:                "1.0",
		Memory:              "1g",
		Ephemeral:           true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	envPath := filepath.Join(legacyDir, "bigtomcat6-exp.env")
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("expected legacy env file, stat returned %v", err)
	}

	servicePath := filepath.Join(systemdDir, "gha-bigtomcat6-exp.service")
	serviceData, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("ReadFile service returned error: %v", err)
	}
	if !contains(string(serviceData), "EnvironmentFile="+envPath) {
		t.Fatalf("expected legacy service EnvironmentFile, got:\n%s", string(serviceData))
	}

	yamlPath := filepath.Join("/etc/gha-runner-tui/profiles", "bigtomcat6-exp.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		t.Fatalf("did not expect yaml profile at %s in legacy mode", yamlPath)
	}

	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 systemd calls, got %v", runner.calls)
	}
}

func TestCreateProfileFallsBackToInstallForProtectedLegacyPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	legacyDir := filepath.Join(root, "gha-runner")
	systemdDir := filepath.Join(root, "systemd")
	tokenFile := filepath.Join(legacyDir, "github_pat")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll legacyDir returned error: %v", err)
	}
	if err := os.MkdirAll(systemdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll systemdDir returned error: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("WriteFile token returned error: %v", err)
	}
	if err := os.Chmod(legacyDir, 0o555); err != nil {
		t.Fatalf("Chmod legacyDir returned error: %v", err)
	}
	if err := os.Chmod(systemdDir, 0o555); err != nil {
		t.Fatalf("Chmod systemdDir returned error: %v", err)
	}
	defer os.Chmod(legacyDir, 0o755)
	defer os.Chmod(systemdDir, 0o755)

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		filepath.Join(root, "missing-config.yaml"),
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.Runner = runner
	manager.SystemdUnitDir = systemdDir
	manager.LegacyEnvDir = legacyDir
	manager.LegacyTokenFile = tokenFile
	manager.LegacyLoopBinary = "/usr/local/bin/gha-ephemeral-loop"

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Name:                "bigtomcat6-exp",
		RepoOwner:           "bigtomcat6",
		RepoName:            "bigtomcat6",
		RunnerLabels:        []string{"self-hosted", "linux", "o-tokyo-s2-exp"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-bigtomcat6-exp.service",
		ContainerNamePrefix: "gha-bigtomcat6",
		CPUs:                "1.0",
		Memory:              "1g",
		Ephemeral:           true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	if len(runner.calls) == 0 {
		t.Fatal("expected fallback runner calls, got none")
	}
}

func TestCreateProfileRejectsUnsafeManagedNamesInLegacyMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	legacyDir := filepath.Join(root, "gha-runner")
	systemdDir := filepath.Join(root, "systemd")
	tokenFile := filepath.Join(legacyDir, "github_pat")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll legacyDir returned error: %v", err)
	}
	if err := os.MkdirAll(systemdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll systemdDir returned error: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("token"), 0o600); err != nil {
		t.Fatalf("WriteFile token returned error: %v", err)
	}

	manager := NewRunnerManager(
		filepath.Join(root, "missing-config.yaml"),
		systemd.NewClient(&recordingRunner{}),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyEnvDir = legacyDir
	manager.LegacyTokenFile = tokenFile
	manager.LegacyLoopBinary = "/usr/local/bin/gha-ephemeral-loop"

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Name:                "../bigtomcat6-exp",
		RepoOwner:           "bigtomcat6",
		RepoName:            "bigtomcat6",
		RunnerLabels:        []string{"self-hosted"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-bigtomcat6-exp.service",
		ContainerNamePrefix: "gha-bigtomcat6",
		Ephemeral:           true,
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name validation error, got %v", err)
	}
}

func TestSyncConfigProfilesMigratesLegacyDockerAccessModeBeforeSync(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll profiles returned error: %v", err)
	}

	cfgPath := filepath.Join(root, "config.yaml")
	profilePath := filepath.Join(profilesDir, "example-org-swift.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+profilesDir+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(`
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
  image: runner:latest
  container_name_prefix: gha-example-org-swift
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
loop:
  state_file: /tmp/example-org-swift.json
`), 0o600); err != nil {
		t.Fatalf("WriteFile profile returned error: %v", err)
	}

	github := &syncGitHub{}
	manager := NewRunnerManager(
		cfgPath,
		systemd.NewClient(&recordingRunner{}),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.GitHubAdmin = github

	if err := manager.SyncConfigProfiles(context.Background()); err != nil {
		t.Fatalf("SyncConfigProfiles returned error: %v", err)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile profile returned error: %v", err)
	}
	if !strings.Contains(string(data), "access_mode: host-socket") {
		t.Fatalf("expected migrated access_mode before sync, got:\n%s", string(data))
	}
}

func TestSyncConfigProfilesMigratesExplicitGitHubConfigBeforeSync(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll profiles returned error: %v", err)
	}

	cfgPath := filepath.Join(root, "config.yaml")
	profilePath := filepath.Join(profilesDir, "example-org-swift.yaml")
	if err := os.WriteFile(cfgPath, []byte("github:\n  token_env: CI_GITHUB_TOKEN\n  env_file: /etc/gha-runner-tui/github.env\npaths:\n  profiles_dir: "+profilesDir+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(`
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
  access_mode: host-socket
  image: runner:latest
  container_name_prefix: gha-example-org-swift
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
loop:
  state_file: /tmp/example-org-swift.json
`), 0o600); err != nil {
		t.Fatalf("WriteFile profile returned error: %v", err)
	}

	github := &syncGitHub{}
	manager := NewRunnerManager(
		cfgPath,
		systemd.NewClient(&recordingRunner{}),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.GitHubAdmin = github

	if err := manager.SyncConfigProfiles(context.Background()); err != nil {
		t.Fatalf("SyncConfigProfiles returned error: %v", err)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile profile returned error: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"github:",
		"token_env: CI_GITHUB_TOKEN",
		"env_file: /etc/gha-runner-tui/github.env",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected migrated github config %q, got:\n%s", want, text)
		}
	}
}

func TestRestartLoopKillsRunningContainerBeforeRestartingService(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		"",
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)

	snapshot := ProfileSnapshot{
		Profile: config.Profile{
			Service: config.ServiceConfig{Name: "gha-remind-me-exp.service"},
		},
		Container: docker.ContainerInfo{
			ID:    "abc123",
			Name:  "gha-remind-me-123",
			State: "running",
		},
	}

	if err := manager.RestartLoop(context.Background(), snapshot); err != nil {
		t.Fatalf("RestartLoop returned error: %v", err)
	}

	if len(runner.calls) < 2 {
		t.Fatalf("expected docker kill and systemctl restart, got %v", runner.calls)
	}
	if runner.calls[0] != "docker kill abc123" {
		t.Fatalf("expected first call to kill container, got %q", runner.calls[0])
	}
	if runner.calls[1] != "systemctl restart gha-remind-me-exp.service" {
		t.Fatalf("expected second call to restart service, got %q", runner.calls[1])
	}
}

func TestRestartLoopKillsAllRunningContainersForMatchingRunnerName(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{
		outputs: map[string][]byte{
			"docker ps --all --filter name=gha-remind-me- --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}": []byte(
				"new\tgha-remind-me-200\tgha-runner-base:latest\tUp 1 minute\n" +
					"old\tgha-remind-me-100\tgha-runner-base:latest\tUp 5 minutes\n" +
					"base\tgha-remind-me-050\tgha-runner-base:latest\tUp 10 hours\n",
			),
			"docker inspect gha-remind-me-200": []byte(inspectJSON("new", "gha-remind-me-200", "gha-runner-base:latest", "running", "gha-remind-me-exp")),
			"docker inspect gha-remind-me-100": []byte(inspectJSON("old", "gha-remind-me-100", "gha-runner-base:latest", "running", "gha-remind-me-exp")),
			"docker inspect gha-remind-me-050": []byte(inspectJSON("base", "gha-remind-me-050", "gha-runner-base:latest", "running", "o-tokyo-s2-remind-me-base")),
		},
	}

	manager := NewRunnerManager(
		"",
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)

	snapshot := ProfileSnapshot{
		Profile: config.Profile{
			Service: config.ServiceConfig{Name: "gha-remind-me-exp.service"},
			Runner:  config.RunnerConfig{NamePrefix: "gha-remind-me-exp"},
			Docker: config.DockerProfile{
				ContainerNamePrefix: "gha-remind-me-",
				Image:               "gha-runner-base:latest",
			},
		},
		Container: docker.ContainerInfo{
			ID:    "new",
			Name:  "gha-remind-me-200",
			State: "running",
		},
	}

	if err := manager.RestartLoop(context.Background(), snapshot); err != nil {
		t.Fatalf("RestartLoop returned error: %v", err)
	}

	expected := []string{
		"docker ps --all --filter name=gha-remind-me- --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}",
		"docker inspect gha-remind-me-200",
		"docker inspect gha-remind-me-100",
		"docker inspect gha-remind-me-050",
		"docker kill new",
		"docker kill old",
		"systemctl restart gha-remind-me-exp.service",
	}
	if len(runner.calls) != len(expected) {
		t.Fatalf("expected calls %v, got %v", expected, runner.calls)
	}
	for i, want := range expected {
		if runner.calls[i] != want {
			t.Fatalf("call %d: expected %q, got %q", i, want, runner.calls[i])
		}
	}
}

func TestSyncRunnerGroupCreatesMissingOrganizationGroup(t *testing.T) {
	t.Parallel()

	github := &syncGitHub{}
	manager := NewRunnerManager("", systemd.Client{}, docker.Client{}, gh.NewClient("", "", "", nil, nil))
	manager.GitHubAdmin = github

	profile := config.Profile{
		Name:        "example-org-swift",
		Target:      config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		RunnerGroup: config.RunnerGroupConfig{Name: "example-org-swift", Create: true, Visibility: "private"},
		Runner:      config.RunnerConfig{Environment: "swift"},
		Service:     config.ServiceConfig{Name: "gha-example-org-swift.service"},
		Docker:      config.DockerProfile{ContainerNamePrefix: "gha-example-org-swift"},
	}

	if err := manager.SyncRunnerGroup(context.Background(), profile); err != nil {
		t.Fatalf("SyncRunnerGroup returned error: %v", err)
	}
	if len(github.createdGroups) != 1 {
		t.Fatalf("expected group creation, got %+v", github.createdGroups)
	}
}

func TestSyncRunnerGroupUpdatesExistingVisibilityPolicy(t *testing.T) {
	t.Parallel()

	github := &syncGitHub{
		groups: []gh.RunnerGroup{{ID: 42, Name: "example-org-swift", Visibility: "all", AllowsPublicRepositories: true}},
	}
	manager := NewRunnerManager("", systemd.Client{}, docker.Client{}, gh.NewClient("", "", "", nil, nil))
	manager.GitHubAdmin = github

	profile := config.Profile{
		Name:        "example-org-swift",
		Target:      config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		RunnerGroup: config.RunnerGroupConfig{Name: "example-org-swift", Create: true, Visibility: "private"},
		Runner:      config.RunnerConfig{Environment: "swift"},
		Service:     config.ServiceConfig{Name: "gha-example-org-swift.service"},
		Docker:      config.DockerProfile{ContainerNamePrefix: "gha-example-org-swift"},
	}

	if err := manager.SyncRunnerGroup(context.Background(), profile); err != nil {
		t.Fatalf("SyncRunnerGroup returned error: %v", err)
	}
	if len(github.updatedGroups) != 1 {
		t.Fatalf("expected group update, got %+v", github.updatedGroups)
	}
	if github.updatedGroups[0].Visibility != "private" {
		t.Fatalf("expected private visibility update, got %+v", github.updatedGroups[0])
	}
}

func TestDeleteRunnerGroupRejectsBusyRunner(t *testing.T) {
	t.Parallel()

	github := &syncGitHub{
		groups: []gh.RunnerGroup{{ID: 42, Name: "example-org-swift", Visibility: "all"}},
		groupRunners: map[int64][]gh.Runner{
			42: {{
				ID: 1, Name: "example-org-swift-1", Status: state.GitHubOnline, Busy: true, RunnerGroupID: 42,
			}},
		},
	}
	manager := NewRunnerManager("", systemd.Client{}, docker.Client{}, gh.NewClient("", "", "", nil, nil))
	manager.GitHubAdmin = github

	profile := config.Profile{
		Name:        "example-org-swift",
		Target:      config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		RunnerGroup: config.RunnerGroupConfig{Name: "example-org-swift", Create: true, Visibility: "private"},
		Runner:      config.RunnerConfig{Environment: "swift"},
		Service:     config.ServiceConfig{Name: "gha-example-org-swift.service"},
		Docker:      config.DockerProfile{ContainerNamePrefix: "gha-example-org-swift"},
	}

	err := manager.DeleteRunnerGroup(context.Background(), profile)
	if err == nil {
		t.Fatal("expected busy runner error, got nil")
	}
}

func TestDeleteRunnerGroupAllowsEmptyGroup(t *testing.T) {
	t.Parallel()

	github := &syncGitHub{
		groups: []gh.RunnerGroup{{ID: 42, Name: "example-org-swift", Visibility: "all"}},
	}
	manager := NewRunnerManager("", systemd.Client{}, docker.Client{}, gh.NewClient("", "", "", nil, nil))
	manager.GitHubAdmin = github

	profile := config.Profile{
		Name:        "example-org-swift",
		Target:      config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		RunnerGroup: config.RunnerGroupConfig{Name: "example-org-swift", Create: true, Visibility: "private"},
		Runner:      config.RunnerConfig{Environment: "swift"},
		Service:     config.ServiceConfig{Name: "gha-example-org-swift.service"},
		Docker:      config.DockerProfile{ContainerNamePrefix: "gha-example-org-swift"},
	}

	if err := manager.DeleteRunnerGroup(context.Background(), profile); err != nil {
		t.Fatalf("DeleteRunnerGroup returned error: %v", err)
	}
	if github.deletedGroupID != 42 {
		t.Fatalf("expected group 42 deletion, got %d", github.deletedGroupID)
	}
}

func TestCreateProfileDerivesOrganizationEnvironmentProfile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	logDir := filepath.Join(root, "logs")
	systemdDir := filepath.Join(root, "systemd")
	socketPath := filepath.Join(root, "docker.sock")
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(socketPath, []byte("socket"), 0o600); err != nil {
		t.Fatalf("WriteFile socket returned error: %v", err)
	}

	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
github:
  env_file: /etc/gha-runner-tui/github.env
paths:
  profiles_dir: %s
  state_dir: %s
  log_dir: %s
systemd:
  loop_binary_path: /usr/local/bin/gha-ephemeral-loop-tui
docker:
  rootless_socket_path: %s
`, profilesDir, stateDir, logDir, socketPath)), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		cfgPath,
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyTokenFile = filepath.Join(root, "missing-token")

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Scope:        config.TargetScopeOrganization,
		Org:          "Example Org",
		Environment:  "swift",
		RunnerLabels: []string{"self-hosted", "linux", "x64", "docker", "swift"},
		DockerImage:  "gha-runner-swift:latest",
		CPUs:         "2",
		Memory:       "4g",
		Ephemeral:    true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	profilePath := filepath.Join(profilesDir, "example-org-swift.yaml")
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("ReadFile profile returned error: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"scope: organization",
		"org: Example Org",
		"name: example-org-swift",
		"github:",
		"token_env: GITHUB_TOKEN",
		"env_file: /etc/gha-runner-tui/github.env",
		"visibility: private",
		"environment: swift",
		"container_name_prefix: gha-example-org-swift",
		"access_mode: rootless",
		socketPath + ":/var/run/docker.sock",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in profile:\n%s", want, text)
		}
	}

	servicePath := filepath.Join(systemdDir, "gha-example-org-swift.service")
	serviceData, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("ReadFile service returned error: %v", err)
	}
	for _, want := range []string{
		"EnvironmentFile=/etc/gha-runner-tui/github.env",
		"ExecStart=/usr/local/bin/gha-ephemeral-loop-tui --config " + profilePath,
	} {
		if !strings.Contains(string(serviceData), want) {
			t.Fatalf("expected %q in service:\n%s", want, string(serviceData))
		}
	}
}

func TestCreateProfileDefaultsToRootlessAccessMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	logDir := filepath.Join(root, "logs")
	systemdDir := filepath.Join(root, "systemd")
	socketPath := filepath.Join(root, "docker.sock")
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(socketPath, []byte("socket"), 0o600); err != nil {
		t.Fatalf("WriteFile socket returned error: %v", err)
	}

	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
github:
  env_file: /etc/gha-runner-tui/github.env
paths:
  profiles_dir: %s
  state_dir: %s
  log_dir: %s
systemd:
  loop_binary_path: /usr/local/bin/gha-ephemeral-loop-tui
docker:
  rootless_socket_path: %s
`, profilesDir, stateDir, logDir, socketPath)), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		cfgPath,
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyTokenFile = filepath.Join(root, "missing-token")

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Name:                "remind-me-swift",
		RepoOwner:           "bigtomcat6",
		RepoName:            "remind-me",
		RunnerLabels:        []string{"self-hosted", "linux", "x64", "docker"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-remind-me-swift.service",
		ContainerNamePrefix: "gha-remind-me-swift",
		CPUs:                "2",
		Memory:              "4g",
		Ephemeral:           true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(profilesDir, "remind-me-swift.yaml"))
	if err != nil {
		t.Fatalf("ReadFile profile returned error: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"access_mode: rootless",
		"github:",
		"token_env: GITHUB_TOKEN",
		"env_file: /etc/gha-runner-tui/github.env",
		socketPath + ":/var/run/docker.sock",
		"DOCKER_HOST: unix:///var/run/docker.sock",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in profile:\n%s", want, text)
		}
	}
}

func TestCreateProfileRejectsUnsafeRepositoryManagedNames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	logDir := filepath.Join(root, "logs")
	systemdDir := filepath.Join(root, "systemd")
	socketPath := filepath.Join(root, "docker.sock")
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(socketPath, []byte("socket"), 0o600); err != nil {
		t.Fatalf("WriteFile socket returned error: %v", err)
	}

	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
github:
  env_file: /etc/gha-runner-tui/github.env
paths:
  profiles_dir: %s
  state_dir: %s
  log_dir: %s
systemd:
  loop_binary_path: /usr/local/bin/gha-ephemeral-loop-tui
docker:
  rootless_socket_path: %s
`, profilesDir, stateDir, logDir, socketPath)), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}

	tests := []struct {
		name  string
		input CreateProfileInput
		want  string
	}{
		{
			name: "unsafe profile name",
			input: CreateProfileInput{
				Name:                "../remind-me-swift",
				RepoOwner:           "bigtomcat6",
				RepoName:            "remind-me",
				RunnerLabels:        []string{"self-hosted"},
				DockerImage:         "gha-runner-base:latest",
				ServiceName:         "gha-remind-me-swift.service",
				ContainerNamePrefix: "gha-remind-me-swift",
				Ephemeral:           true,
			},
			want: "name",
		},
		{
			name: "unsafe service name",
			input: CreateProfileInput{
				Name:                "remind-me-swift",
				RepoOwner:           "bigtomcat6",
				RepoName:            "remind-me",
				RunnerLabels:        []string{"self-hosted"},
				DockerImage:         "gha-runner-base:latest",
				ServiceName:         "../gha-remind-me-swift.service",
				ContainerNamePrefix: "gha-remind-me-swift",
				Ephemeral:           true,
			},
			want: "service.name",
		},
		{
			name: "unsafe container prefix",
			input: CreateProfileInput{
				Name:                "remind-me-swift",
				RepoOwner:           "bigtomcat6",
				RepoName:            "remind-me",
				RunnerLabels:        []string{"self-hosted"},
				DockerImage:         "gha-runner-base:latest",
				ServiceName:         "gha-remind-me-swift.service",
				ContainerNamePrefix: "../gha-remind-me-swift",
				Ephemeral:           true,
			},
			want: "docker.container_name_prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := NewRunnerManager(
				cfgPath,
				systemd.NewClient(&recordingRunner{}),
				docker.NewClient(command.OSRunner{}),
				gh.NewClient("", "", "", nil, nil),
			)
			manager.SystemdUnitDir = systemdDir
			manager.LegacyTokenFile = filepath.Join(root, "missing-token")

			err := manager.CreateProfile(context.Background(), tt.input)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
		})
	}
}

func TestCreateProfileAutoDetectsUniqueRootlessSocket(t *testing.T) {
	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	logDir := filepath.Join(root, "logs")
	systemdDir := filepath.Join(root, "systemd")
	socketPath := filepath.Join(root, "detected.sock")
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(socketPath, []byte("socket"), 0o600); err != nil {
		t.Fatalf("WriteFile socket returned error: %v", err)
	}
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)

	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
paths:
  profiles_dir: %s
  state_dir: %s
  log_dir: %s
systemd:
  loop_binary_path: /usr/local/bin/gha-ephemeral-loop-tui
docker:
  auto_detect_rootless_socket: true
`, profilesDir, stateDir, logDir)), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		cfgPath,
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyTokenFile = filepath.Join(root, "missing-token")

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		Name:                "remind-me-swift",
		RepoOwner:           "bigtomcat6",
		RepoName:            "remind-me",
		RunnerLabels:        []string{"self-hosted", "linux", "x64", "docker"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-remind-me-swift.service",
		ContainerNamePrefix: "gha-remind-me-swift",
		CPUs:                "2",
		Memory:              "4g",
		Ephemeral:           true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(profilesDir, "remind-me-swift.yaml"))
	if err != nil {
		t.Fatalf("ReadFile profile returned error: %v", err)
	}
	if !strings.Contains(string(data), socketPath+":/var/run/docker.sock") {
		t.Fatalf("expected auto-detected socket in profile:\n%s", string(data))
	}
}

func TestCreateProfileAllowsExplicitHostSocketOptIn(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	logDir := filepath.Join(root, "logs")
	systemdDir := filepath.Join(root, "systemd")
	cfgPath := filepath.Join(root, "config.yaml")

	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
paths:
  profiles_dir: %s
  state_dir: %s
  log_dir: %s
systemd:
  loop_binary_path: /usr/local/bin/gha-ephemeral-loop-tui
docker:
  allow_host_socket_opt_in: true
`, profilesDir, stateDir, logDir)), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		cfgPath,
		systemd.NewClient(runner),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyTokenFile = filepath.Join(root, "missing-token")

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		DockerAccess:        "host-socket",
		Name:                "remind-me-swift",
		RepoOwner:           "bigtomcat6",
		RepoName:            "remind-me",
		RunnerLabels:        []string{"self-hosted", "linux", "x64", "docker"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-remind-me-swift.service",
		ContainerNamePrefix: "gha-remind-me-swift",
		CPUs:                "2",
		Memory:              "4g",
		Ephemeral:           true,
	})
	if err != nil {
		t.Fatalf("CreateProfile returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(profilesDir, "remind-me-swift.yaml"))
	if err != nil {
		t.Fatalf("ReadFile profile returned error: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"access_mode: host-socket",
		"/var/run/docker.sock:/var/run/docker.sock",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in profile:\n%s", want, text)
		}
	}
}

func TestCreateProfileRejectsHostSocketWhenDisabled(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	profilesDir := filepath.Join(root, "profiles")
	stateDir := filepath.Join(root, "state")
	logDir := filepath.Join(root, "logs")
	systemdDir := filepath.Join(root, "systemd")
	cfgPath := filepath.Join(root, "config.yaml")

	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
paths:
  profiles_dir: %s
  state_dir: %s
  log_dir: %s
systemd:
  loop_binary_path: /usr/local/bin/gha-ephemeral-loop-tui
docker:
  allow_host_socket_opt_in: false
`, profilesDir, stateDir, logDir)), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}

	manager := NewRunnerManager(
		cfgPath,
		systemd.NewClient(&recordingRunner{}),
		docker.NewClient(command.OSRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir
	manager.LegacyTokenFile = filepath.Join(root, "missing-token")

	err := manager.CreateProfile(context.Background(), CreateProfileInput{
		DockerAccess:        "host-socket",
		Name:                "remind-me-swift",
		RepoOwner:           "bigtomcat6",
		RepoName:            "remind-me",
		RunnerLabels:        []string{"self-hosted", "linux", "x64", "docker"},
		DockerImage:         "gha-runner-base:latest",
		ServiceName:         "gha-remind-me-swift.service",
		ContainerNamePrefix: "gha-remind-me-swift",
		CPUs:                "2",
		Memory:              "4g",
		Ephemeral:           true,
	})
	if err == nil {
		t.Fatal("expected host-socket policy error, got nil")
	}
	if !strings.Contains(err.Error(), "host-socket") {
		t.Fatalf("expected host-socket error, got %v", err)
	}
}

func inspectJSON(id, name, image, status, runnerName string) string {
	return fmt.Sprintf(`[{"Id":"%s","Name":"/%s","Config":{"Image":"%s","Env":["RUNNER_NAME=%s"]},"State":{"Status":"%s"}}]`,
		id,
		name,
		image,
		runnerName,
		status,
	)
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && filepath.Base(needle) != "" && stringContains(haystack, needle))
}

func stringContains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}())
}
