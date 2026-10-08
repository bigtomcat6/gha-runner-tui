package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	managed    []dockerpkg.ContainerInfo
	holders    []dockerpkg.ContainerInfo
	managedErr error
	slotErr    error
	container  dockerpkg.ContainerInfo
	containers []dockerpkg.ContainerInfo
	inspects   map[string]dockerpkg.ContainerDetails
	err        error
}

func (f fakeDocker) ListManaged(context.Context, string) ([]dockerpkg.ContainerInfo, error) {
	return f.managed, f.managedErr
}

func (f fakeDocker) SlotHolders(context.Context) ([]dockerpkg.ContainerInfo, error) {
	return f.holders, f.slotErr
}

func fixedGitHub(client GitHubClient) func(config.GlobalConfig, config.Profile) GitHubClient {
	return func(config.GlobalConfig, config.Profile) GitHubClient { return client }
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
		GitHubForProfile: fixedGitHub(&fakeGitHub{}),
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
		GitHubForProfile: fixedGitHub(&fakeGitHub{}),
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
		GitHubForProfile: fixedGitHub(&fakeGitHub{
			repoRunners: []gh.Runner{
				{Name: "gha-remind-me-exp", Status: state.GitHubOnline},
			},
		}),
	}

	snapshot := service.loadProfile(context.Background(), config.GlobalConfig{}, profile)

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
		GitHubForProfile: fixedGitHub(github),
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

func TestLoadDashboardLeavesLegacyDockerAccessModeReadable(t *testing.T) {
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
		ConfigPath:       cfgPath,
		Systemd:          fakeSystemd{status: systemdpkg.ServiceStatus{Active: state.SystemdInactive}},
		Docker:           fakeDocker{container: dockerpkg.ContainerInfo{State: state.ContainerNone}},
		GitHubForProfile: fixedGitHub(&fakeGitHub{}),
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
	if strings.Contains(string(data), "access_mode:") || strings.Contains(string(data), "github:") {
		t.Fatalf("dashboard migrated profile: %s", data)
	}
}

func TestLoadDashboardDoesNotReportImplicitMigrationWarnings(t *testing.T) {
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
		ConfigPath:       cfgPath,
		Systemd:          fakeSystemd{status: systemdpkg.ServiceStatus{Active: state.SystemdInactive}},
		Docker:           fakeDocker{container: dockerpkg.ContainerInfo{State: state.ContainerNone}},
		GitHubForProfile: fixedGitHub(&fakeGitHub{}),
	}

	dashboard, err := service.LoadDashboard(context.Background())
	if err != nil {
		t.Fatalf("LoadDashboard returned error: %v", err)
	}
	if len(dashboard.Profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(dashboard.Profiles))
	}
	if len(dashboard.MigrationWarnings) != 0 {
		t.Fatalf("unexpected migration warnings: %v", dashboard.MigrationWarnings)
	}
}

type profileFileSnapshot struct {
	Data    []byte
	ModTime time.Time
}

func snapshotProfileFiles(t *testing.T, dir string) map[string]profileFileSnapshot {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]profileFileSnapshot{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = profileFileSnapshot{data, info.ModTime()}
	}
	return files
}

func TestLoadDashboardDoesNotMigrateOrTouchProfileFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profiles")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	raw := []byte("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\ndocker:\n  container_name_prefix: gha-p\n  volumes: [/var/run/docker.sock:/var/run/docker.sock]\n")
	for name, data := range map[string][]byte{"p.yaml": raw, "q.yml": bytes.ReplaceAll(raw, []byte("name: p"), []byte("name: q")), "p.yaml.bak": []byte("original backup"), "notes.txt": []byte("untouched")} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotProfileFiles(t, dir)
	s := Service{ConfigPath: cfgPath, Systemd: fakeSystemd{}, Docker: fakeDocker{}, GitHubForProfile: fixedGitHub(&fakeGitHub{})}
	for i := 0; i < 2; i++ {
		dashboard, err := s.LoadDashboard(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(dashboard.Profiles) != 2 {
			t.Fatalf("legacy YAML not readable: %+v", dashboard)
		}
	}
	if after := snapshotProfileFiles(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatal("dashboard modified profile contents, mtime, or directory entries")
	}
}

func TestDashboardProfileCredentialsAreIndependent(t *testing.T) {
	root := t.TempDir()
	repoEnv, orgEnv, globalEnv := filepath.Join(root, "repo.env"), filepath.Join(root, "org.env"), filepath.Join(root, "global.env")
	for path, token := range map[string]string{repoEnv: "repo-fake", orgEnv: "org-fake", globalEnv: "global-fake"} {
		if err := os.WriteFile(path, []byte("TEST_PROFILE_TOKEN="+token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEST_PROFILE_TOKEN", "wrong-process-fake")
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization")+" "+r.URL.RequestURI())
		fmt.Fprint(w, `{"runners":[],"runner_groups":[{"id":1,"name":"org","visibility":"private"}]}`)
	}))
	defer server.Close()
	dir := filepath.Join(root, "profiles")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"repo": "repo: {owner: me, name: app}\n",
		"org":  "target: {scope: organization, org: Example Org}\nrunner_group: {name: org, visibility: private}\nrunner: {environment: swift}\n",
	} {
		env := repoEnv
		if name == "org" {
			env = orgEnv
		}
		data := "name: " + name + "\nservice: {name: gha-" + name + ".service}\ndocker: {container_name_prefix: gha-" + name + "}\ngithub: {token_env: TEST_PROFILE_TOKEN, env_file: " + env + "}\n" + raw
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+dir+"\ngithub:\n  api_base_url: "+server.URL+"\n  token_env: TEST_PROFILE_TOKEN\n  env_file: "+globalEnv+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewRunnerManager(cfgPath, systemdpkg.Client{}, dockerpkg.Client{}, gh.NewGlobalClient(server.URL, "TEST_PROFILE_TOKEN", globalEnv, nil, server.Client()))
	manager.Service.Systemd = fakeSystemd{}
	manager.Service.Docker = fakeDocker{}
	badPath := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(badPath, []byte("[invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	dashboard, err := manager.Dashboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.ProfileErrors) != 1 || dashboard.ProfileErrors[0].Path != badPath {
		t.Fatalf("bad YAML not localized: %+v", dashboard.ProfileErrors)
	}
	for _, p := range dashboard.Profiles {
		if len(p.Errors) != 0 {
			t.Fatalf("%s: %v", p.Profile.Name, p.Errors)
		}
	}
	want := []string{"Bearer org-fake /orgs/example-org/actions/runners", "Bearer repo-fake /repos/me/app/actions/runners"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("credentials/targets: %v", calls)
	}
	if err := manager.SyncRunnerGroup(context.Background(), dashboard.Profiles[0].Profile); err != nil {
		t.Fatal(err)
	}
	if got := calls[len(calls)-1]; got != "Bearer global-fake /orgs/example-org/actions/runner-groups" {
		t.Fatalf("admin identity: %s", got)
	}
	if err := os.Remove(orgEnv); err != nil {
		t.Fatal(err)
	}
	calls = nil
	dashboard, err = manager.Dashboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Profiles[0].Errors) == 0 || len(dashboard.Profiles[1].Errors) != 0 {
		t.Fatalf("credential error not scoped: %+v", dashboard.Profiles)
	}
	if !reflect.DeepEqual(calls, want[1:]) {
		t.Fatalf("strict token fell back: %v", calls)
	}
}

func TestDashboardLocalizesProfileAndSlotErrors(t *testing.T) {
	root := t.TempDir()
	badDir := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(badDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+badDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Service{ConfigPath: cfgPath, LegacyServiceDir: badDir, Docker: fakeDocker{slotErr: errors.New("slot unavailable")}}
	dashboard, err := s.LoadDashboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.ProfileErrors) != 2 || dashboard.ProfileErrors[0].Path != badDir || dashboard.ProfileErrors[1].Path != badDir {
		t.Fatalf("profile errors: %+v", dashboard.ProfileErrors)
	}
	if dashboard.SlotError == nil || *dashboard.SlotError != "slot unavailable" {
		t.Fatalf("slot error: %+v", dashboard)
	}
	s.Docker = nil
	dashboard, err = s.LoadDashboard(context.Background())
	if err != nil || dashboard.SlotError == nil {
		t.Fatalf("nil Docker must be unknown: %+v %v", dashboard, err)
	}
	if err := os.WriteFile(cfgPath, []byte("[invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadDashboard(context.Background()); err == nil {
		t.Fatal("global config error must be top-level")
	}
}

func TestDashboardShowsCreatedHolderWithStoppedLoop(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profiles")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.yml"), []byte("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\ndocker: {container_name_prefix: gha-p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	holder := dockerpkg.ContainerInfo{Name: "created", Profile: "p", RunnerName: "p-1", State: state.ContainerCreated}
	s := Service{ConfigPath: cfgPath, Systemd: fakeSystemd{status: systemdpkg.ServiceStatus{Active: state.SystemdInactive, Enabled: true}}, Docker: fakeDocker{managed: []dockerpkg.ContainerInfo{holder}, holders: []dockerpkg.ContainerInfo{holder}}, GitHubForProfile: fixedGitHub(&fakeGitHub{repoRunners: []gh.Runner{{Name: "p-1", Status: state.GitHubOnline}}})}
	d, err := s.LoadDashboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.SlotHolders) != 1 || d.SlotError != nil || len(d.ProfileErrors) != 0 || len(d.Profiles) != 1 || d.Profiles[0].Container.Name != "created" || d.Profiles[0].DisplayLoopState != state.LoopStopped {
		t.Fatalf("created holder missing: %+v", d)
	}
	if d.Profiles[0].GitHubRunner == nil || d.Profiles[0].GitHubRunner.Name != "p-1" {
		t.Fatal("managed runner label not used for GitHub match")
	}
	s.Docker = fakeDocker{managed: []dockerpkg.ContainerInfo{holder}, slotErr: errors.New("slot unavailable")}
	d, err = s.LoadDashboard(context.Background())
	if err != nil || d.SlotError == nil || d.Profiles[0].Container.Name != "created" || len(d.Profiles[0].Errors) != 0 {
		t.Fatalf("slot failure contaminated profile: %+v %v", d, err)
	}
}

func TestLoadProfileRejectsMissingGitHubFactoryOrClient(t *testing.T) {
	for _, factory := range []func(config.GlobalConfig, config.Profile) GitHubClient{nil, fixedGitHub(nil)} {
		s := Service{Systemd: fakeSystemd{}, Docker: fakeDocker{}, GitHubForProfile: factory}
		p := s.loadProfile(context.Background(), config.GlobalConfig{}, config.Profile{})
		if !strings.Contains(p.ErrorSummary(), "github:") || p.GitHubState != state.GitHubUnknown {
			t.Fatalf("missing client silently accepted: %+v", p)
		}
	}
}
