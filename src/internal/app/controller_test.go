package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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
	return []byte("ok\n"), nil
}

type scriptedRunner struct {
	calls   []string
	outputs map[string][]byte
	errors  map[string]error
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name
	for _, arg := range args {
		call += " " + arg
	}
	r.calls = append(r.calls, call)
	if err := r.errors[call]; err != nil {
		return r.outputs[call], err
	}
	if out, ok := r.outputs[call]; ok {
		return out, nil
	}
	return []byte("ok\n"), nil
}

const managedPS = `docker --host unix:///var/run/docker.sock ps --all --filter label=io.gha-runner-tui.managed=true --filter label=io.gha-runner-tui.profile=p --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Label "io.gha-runner-tui.profile"}}\t{{.Label "io.gha-runner-tui.runner"}}`

func TestForceRemoveProfileOnlyRemovesOwnSlotHolders(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			r := &scriptedRunner{outputs: map[string][]byte{managedPS: []byte("a\tcreated\timg\tcreated\tp\tp-1\nb\trunning\timg\trunning\tp\tp-2\n\tpaused\timg\tpaused\tp\tp-3\nx\tother\timg\trunning\tother\tother-1\ne\texited\timg\texited\tp\tp-old\n")}, errors: map[string]error{}}
			if fail {
				r.errors["docker --host unix:///var/run/docker.sock rm -f b"] = errors.New("remove failed")
			}
			m := NewRunnerManager("", systemd.NewClient(r), docker.NewClient(r), gh.Client{})
			m.Service.GitHubForProfile = func(config.GlobalConfig, config.Profile) GitHubClient { t.Fatal("force queried GitHub"); return nil }
			removed, err := m.ForceRemoveProfile(context.Background(), config.Profile{Name: "p"})
			want := []string{"a", "b", "paused"}
			calls := []string{managedPS, "docker --host unix:///var/run/docker.sock rm -f a", "docker --host unix:///var/run/docker.sock rm -f b", "docker --host unix:///var/run/docker.sock rm -f paused"}
			if fail {
				want = want[:1]
				calls = calls[:3]
			}
			if (err != nil) != fail || !reflect.DeepEqual(removed, want) || !reflect.DeepEqual(r.calls, calls) {
				t.Fatalf("removed=%v err=%v calls=%v", removed, err, r.calls)
			}
		})
	}
}

func TestProfileSlotHoldersFailsClosed(t *testing.T) {
	r := &scriptedRunner{outputs: map[string][]byte{managedPS: []byte("u\tunknown\timg\tunrecognized\tp\tp-1\nd\tdead\timg\tdead\tp\tp-2\nr\tremoving\timg\tremoving\tp\tp-3\n")}}
	m := NewRunnerManager("", systemd.NewClient(r), docker.NewClient(r), gh.Client{})
	holders, err := m.ProfileSlotHolders(context.Background(), config.Profile{Name: "p"})
	if err != nil || len(holders) != 1 || holders[0].Name != "unknown" {
		t.Fatalf("holders=%v err=%v", holders, err)
	}
	r.errors = map[string]error{managedPS: errors.New("daemon unavailable")}
	removed, err := m.ForceRemoveProfile(context.Background(), config.Profile{Name: "p"})
	if err == nil || len(removed) != 0 || len(r.calls) != 2 {
		t.Fatalf("query failure allowed removal: %v %v %v", removed, err, r.calls)
	}
}

func TestForceRemoveProfileRejectsUnnamedProfile(t *testing.T) {
	r := &recordingRunner{}
	m := NewRunnerManager("", systemd.NewClient(r), docker.NewClient(r), gh.Client{})
	removed, err := m.ForceRemoveProfile(context.Background(), config.Profile{})
	if err == nil || len(removed) != 0 || len(r.calls) != 0 {
		t.Fatalf("unnamed force: %v %v %v", removed, err, r.calls)
	}
}

func TestKillContainerRemainsExplicitForManagedAndLegacy(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			prefixPS := "docker --host unix:///var/run/docker.sock ps --all --filter name=gha-p --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}"
			r := &scriptedRunner{outputs: map[string][]byte{managedPS: []byte("a\tone\timg\trunning\tp\tp-1\nb\ttwo\timg\trunning\tp\tp-1\ne\texited\timg\texited\tp\tp-1\n")}}
			if legacy {
				r.outputs[managedPS] = nil
				r.outputs[prefixPS] = []byte("a\tone\timg\tUp 1 minute\nb\ttwo\timg\tUp 2 minutes\ne\texited\timg\tExited (0)\n")
				for name, status := range map[string]string{"one": "running", "two": "running", "exited": "exited"} {
					r.outputs["docker --host unix:///var/run/docker.sock inspect "+name] = []byte(inspectJSON(name, name, "img", status, "p-1"))
				}
			}
			m := NewRunnerManager("", systemd.NewClient(r), docker.NewClient(r), gh.Client{})
			p := config.Profile{Name: "p", Runner: config.RunnerConfig{NamePrefix: "p-1"}, Docker: config.DockerProfile{ContainerNamePrefix: "gha-p"}}
			if err := m.KillContainer(context.Background(), ProfileSnapshot{Profile: p}); err != nil {
				t.Fatal(err)
			}
			var kills []string
			for _, call := range r.calls {
				if strings.Contains(call, " kill ") {
					kills = append(kills, call)
				}
				if strings.HasPrefix(call, "systemctl ") || strings.Contains(call, " rm ") {
					t.Fatalf("unexpected kill side effect: %s", call)
				}
			}
			want := []string{"docker --host unix:///var/run/docker.sock kill a", "docker --host unix:///var/run/docker.sock kill b"}
			if legacy {
				want = []string{"docker --host unix:///var/run/docker.sock kill one", "docker --host unix:///var/run/docker.sock kill two"}
			}
			if !reflect.DeepEqual(kills, want) {
				t.Fatalf("kills=%v", kills)
			}
		})
	}
}

func TestMigrateExplicitPreservesBackupAndReportsAllFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "profiles")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+dir+"\ngithub: {token_env: MIGRATE_FAKE, env_file: /fake/global.env}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\ndocker:\n  volumes: [/var/run/docker.sock:/var/run/docker.sock]\n")
	pPath := filepath.Join(dir, "p.yaml")
	badPath := filepath.Join(dir, "bad.yml")
	skippedPath := filepath.Join(dir, "skip.yml")
	for path, data := range map[string][]byte{pPath: raw, badPath: []byte("[invalid"), skippedPath: []byte("docker: {access_mode: rootless}\ngithub: {token_env: KEEP, env_file: /fake/keep.env}\n")} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := RunnerManager{ConfigPath: cfgPath}
	results, err := m.Migrate(context.Background())
	if err == nil {
		t.Fatal("bad YAML must fail operation")
	}
	statuses := map[string]map[config.ProfileMigrationStatus]bool{}
	for _, r := range results {
		if statuses[r.Path] == nil {
			statuses[r.Path] = map[config.ProfileMigrationStatus]bool{}
		}
		statuses[r.Path][r.Status] = true
	}
	if !statuses[pPath][config.ProfileMigrationUpdated] || !statuses[badPath][config.ProfileMigrationFailed] || !statuses[skippedPath][config.ProfileMigrationSkipped] {
		t.Fatalf("missing file results: %+v", results)
	}
	data, err := os.ReadFile(pPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "access_mode: host-socket") || !strings.Contains(string(data), "github:") || !strings.Contains(string(data), "MIGRATE_FAKE") {
		t.Fatalf("explicit migration missing: %s", data)
	}
	backup, err := os.ReadFile(pPath + ".bak")
	if err != nil || string(backup) != string(raw) {
		t.Fatalf("backup=%s err=%v", backup, err)
	}
	before := snapshotProfileFiles(t, dir)
	if _, err := m.Migrate(context.Background()); err == nil {
		t.Fatal("bad YAML lost failure on rerun")
	}
	if after := snapshotProfileFiles(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatal("rerun modified backup or migrated file")
	}
	if err := os.Remove(badPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLookupProfileIsExactReadonlyAndDoesNotQueryGitHub(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	dir := filepath.Join(root, "profiles")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "different-filename.yml"), []byte("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\ndocker: {container_name_prefix: gha-p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotProfileFiles(t, dir)
	r := &recordingRunner{}
	m := NewRunnerManager(cfgPath, systemd.NewClient(r), docker.NewClient(r), gh.Client{})
	m.Service.GitHubForProfile = func(config.GlobalConfig, config.Profile) GitHubClient {
		t.Fatal("lifecycle queried GitHub")
		return nil
	}
	p, err := m.LookupProfile("p")
	if err != nil || p.Name != "p" {
		t.Fatalf("lookup=%+v %v", p, err)
	}
	for _, name := range []string{"different-filename", "../p", "P"} {
		if _, err := m.LookupProfile(name); !errors.Is(err, ErrProfileNotFound) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if err := m.StartLoop(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := m.StopLoop(context.Background(), ProfileSnapshot{Profile: p}); err != nil {
		t.Fatal(err)
	}
	if err := m.RestartLoop(context.Background(), ProfileSnapshot{Profile: p}); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl start gha-p.service", "systemctl stop gha-p.service", "systemctl restart gha-p.service"}
	if !reflect.DeepEqual(r.calls, want) || !reflect.DeepEqual(before, snapshotProfileFiles(t, dir)) {
		t.Fatalf("lifecycle side effects: %v", r.calls)
	}
}

func TestLookupProfileFindsLegacyOnlyWithoutModernProfiles(t *testing.T) {
	root := t.TempDir()
	units := filepath.Join(root, "units")
	dir := filepath.Join(root, "profiles")
	if err := os.Mkdir(units, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(root, "legacy.env")
	if err := os.WriteFile(envPath, []byte("REPO_OWNER=me\nREPO_NAME=app\nRUNNER_NAME=legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "gha-legacy.service"), []byte("[Service]\nEnvironmentFile="+envPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := RunnerManager{ConfigPath: cfgPath, SystemdUnitDir: units}
	p, err := m.LookupProfile("legacy")
	if err != nil || p.Name != "legacy" {
		t.Fatalf("legacy lookup: %+v %v", p, err)
	}
	s := Service{ConfigPath: cfgPath, LegacyServiceDir: units, Systemd: fakeSystemd{}, Docker: fakeDocker{}, GitHubForProfile: fixedGitHub(&fakeGitHub{})}
	d, err := s.LoadDashboard(context.Background())
	if err != nil || len(d.Profiles) != 1 || d.Profiles[0].Profile.Name != "legacy" {
		t.Fatalf("legacy display: %+v %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\ndocker: {container_name_prefix: gha-p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LookupProfile("legacy"); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("legacy mixed with modern: %v", err)
	}
}

func TestDockerLogsResolvesRealCurrentOrLatestWithoutGitHub(t *testing.T) {
	for _, tc := range []struct {
		name, last, list, want string
		legacy                 bool
	}{
		{"modern-current", "current", "e\tlatest\timg\texited\tp\tp-2\nc\tcurrent\timg\trunning\tp\tp-1\n", "current", false},
		{"modern-latest", "stale", "e\tlatest\timg\texited\tp\tp-2\n", "latest", false},
		{"legacy-current", "current", "e\tlatest\timg\tExited (0)\nc\tcurrent\timg\tUp 1 minute\n", "current", true},
		{"legacy-latest", "stale", "e\tlatest\timg\tExited (0)\n", "latest", true},
		{"stale-only", "stale", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefixPS := "docker --host unix:///var/run/docker.sock ps --all --filter name=gha-p --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}"
			r := &scriptedRunner{outputs: map[string][]byte{managedPS: []byte(tc.list), prefixPS: nil, "docker --host unix:///var/run/docker.sock inspect latest": []byte(inspectJSON("e", "latest", "img", "exited", "p")), "docker --host unix:///var/run/docker.sock inspect current": []byte(inspectJSON("c", "current", "img", "running", "p")), "docker --host unix:///var/run/docker.sock logs --tail 20 " + tc.want: []byte("safe logs")}, errors: map[string]error{"docker --host unix:///var/run/docker.sock inspect stale": errors.New("not found")}}
			if tc.legacy {
				r.outputs[managedPS] = nil
				r.outputs[prefixPS] = []byte(tc.list)
			}
			m := NewRunnerManager("", systemd.NewClient(r), docker.NewClient(r), gh.Client{})
			m.Service.GitHubForProfile = func(config.GlobalConfig, config.Profile) GitHubClient { t.Fatal("logs queried GitHub"); return nil }
			snapshot := ProfileSnapshot{Profile: config.Profile{Name: "p", Runner: config.RunnerConfig{NamePrefix: "p"}, Docker: config.DockerProfile{ContainerNamePrefix: "gha-p"}}, Loop: state.LoopState{LastContainerName: tc.last}, Container: docker.ContainerInfo{Name: "stale"}}
			out, err := m.DockerLogs(context.Background(), snapshot, 20, false)
			if tc.want == "" {
				if !errors.Is(err, ErrNoCurrentContainer) || out != "" {
					t.Fatalf("stale logs: %s %v", out, err)
				}
				return
			}
			if err != nil || out != "safe logs" || r.calls[len(r.calls)-1] != "docker --host unix:///var/run/docker.sock logs --tail 20 "+tc.want {
				t.Fatalf("logs=%q err=%v calls=%v", out, err, r.calls)
			}
		})
	}
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

func createFixture(t *testing.T) (RunnerManager, *scriptedRunner, config.GlobalConfig, CreateProfileInput) {
	t.Helper()
	root := t.TempDir()
	cfg := config.DefaultGlobalConfig()
	cfg.Paths = config.PathsConfig{ProfilesDir: filepath.Join(root, "profiles"), StateDir: filepath.Join(root, "state"), LogDir: filepath.Join(root, "logs")}
	cfg.GitHub.EnvFile = filepath.Join(root, "global.env")
	cfg.Docker.RootlessSocketPath = filepath.Join(root, "docker.sock")
	cfg.Docker.AutoDetectRootlessSocket = false
	for _, dir := range []string{cfg.Paths.ProfilesDir, cfg.Paths.StateDir, cfg.Paths.LogDir, filepath.Join(root, "units")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.Docker.RootlessSocketPath, []byte("fake socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &scriptedRunner{}
	m := NewRunnerManager(cfgPath, systemd.NewClient(r), docker.NewClient(r), gh.Client{})
	m.Runner = r
	m.SystemdUnitDir = filepath.Join(root, "units")
	input := CreateProfileInput{Name: "model-router", RepoOwner: "me", RepoName: "model-router", RunnerLabels: []string{"self-hosted", "linux", "x64"}, DockerImage: "runner:test", ServiceName: "gha-model-router.service", ContainerNamePrefix: "gha-model-router", CPUs: "0.8", Memory: "512m"}
	return m, r, cfg, input
}

func TestCreateProfileWritesStandardYAML(t *testing.T) {
	for _, noStart := range []bool{false, true} {
		t.Run(fmt.Sprint(noStart), func(t *testing.T) {
			m, r, cfg, input := createFixture(t)
			input.NoStart = noStart
			input.GitHubEnvFile = filepath.Join(filepath.Dir(m.ConfigPath), "profile.env")
			if err := m.CreateProfile(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			p, err := config.LoadProfile(filepath.Join(cfg.Paths.ProfilesDir, "model-router.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !p.Runner.Ephemeral || p.GitHub.EnvFile != input.GitHubEnvFile || p.Docker.CPUs != "0.8" || p.Docker.Memory != "512m" || p.Loop.PollIntervalSeconds != 30 || p.Loop.IdleTimeoutSeconds != 180 {
				t.Fatalf("profile: %+v", p)
			}
			unitPath := filepath.Join(m.SystemdUnitDir, input.ServiceName)
			data, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"EnvironmentFile=" + input.GitHubEnvFile, "KillMode=mixed", "TimeoutStopSec=240"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("unit missing %q: %s", want, data)
				}
			}
			for path, mode := range map[string]os.FileMode{p.Source: 0o640, unitPath: 0o644} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("mode %s: %v %v", path, info, err)
				}
			}
			want := []string{"systemctl daemon-reload", "systemctl enable gha-model-router.service"}
			if !noStart {
				want = append(want, "systemctl start gha-model-router.service")
			}
			if !reflect.DeepEqual(r.calls, want) {
				t.Fatalf("calls: %v", r.calls)
			}
		})
	}
}

func TestCreatePermissionFailureDoesNotEscalateFileWrite(t *testing.T) {
	for _, unit := range []bool{false, true} {
		t.Run(fmt.Sprint(unit), func(t *testing.T) {
			m, r, cfg, input := createFixture(t)
			pPath := filepath.Join(cfg.Paths.ProfilesDir, "model-router.yaml")
			uPath := filepath.Join(m.SystemdUnitDir, input.ServiceName)
			failPath, stage := pPath, "YAML"
			if unit {
				failPath, stage = uPath, "unit"
			}
			m.openNewFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
				wantMode := os.FileMode(0o640)
				if path == uPath {
					wantMode = 0o644
				} else if path != pPath {
					t.Fatalf("unexpected path: %s", path)
				}
				if flags != os.O_WRONLY|os.O_CREATE|os.O_EXCL || mode != wantMode {
					t.Fatalf("open flags/mode: %d %o", flags, mode)
				}
				if path == failPath {
					return nil, &os.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
				}
				return os.OpenFile(path, flags, mode)
			}
			err := m.CreateProfile(context.Background(), input)
			if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "permission denied; run create with sudo (sudo gha-runner-tui create ...)") || !strings.Contains(err.Error(), failPath) || !strings.Contains(err.Error(), stage) {
				t.Fatalf("permission failure: %v", err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("permission failure ran commands: %v", r.calls)
			}
			if _, err := os.Lstat(uPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("unit created: %v", err)
			}
			if unit {
				if _, loadErr := config.LoadProfile(pPath); loadErr != nil {
					t.Fatal(loadErr)
				}
				if !strings.Contains(err.Error(), "retained") || !strings.Contains(err.Error(), pPath) {
					t.Fatalf("partial create not reported: %v", err)
				}
			} else if _, err := os.Lstat(pPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("YAML created: %v", err)
			}
		})
	}
}

func TestCreateProfileRejectsUnsafeManagedNames(t *testing.T) {
	m, r, _, input := createFixture(t)
	input.Name = "../model-router"
	if err := m.CreateProfile(context.Background(), input); !errors.Is(err, ErrInvalidCreateInput) || !strings.Contains(err.Error(), "name") {
		t.Fatalf("unsafe name: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("invalid input ran commands: %v", r.calls)
	}
}

func TestNormalizeCreateDefaultsDoNotInventResources(t *testing.T) {
	cfg := config.DefaultGlobalConfig()
	input := CreateProfileInput{Name: "model-router", RepoOwner: "me", RepoName: "model-router", RunnerLabels: []string{"self-hosted", "linux", "x64"}, DockerImage: "runner:test", CPUs: "0.8", Memory: "512m", GitHubEnvFile: "/etc/gha-runner-tui/model-router.env", NoStart: true}
	got, err := normalizeCreateInput(cfg, input)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != config.TargetScopeRepository || got.ServiceName != "gha-model-router.service" || got.ContainerNamePrefix != "gha-model-router" || !got.Ephemeral || !got.NoStart || got.GitHubEnvFile != input.GitHubEnvFile || got.CPUs != "0.8" || got.Memory != "512m" {
		t.Fatalf("defaults: %+v", got)
	}
	input.GitHubEnvFile = ""
	got, err = normalizeCreateInput(cfg, input)
	if err != nil || got.GitHubEnvFile != cfg.GitHub.EnvFile {
		t.Fatalf("global env default: %+v %v", got, err)
	}
	input.CPUs = ""
	if _, err := normalizeCreateInput(cfg, input); !errors.Is(err, ErrInvalidCreateInput) {
		t.Fatalf("resources must be required: %v", err)
	}
}

func TestNormalizeCreateRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CreateProfileInput)
	}{
		{"cpu", func(i *CreateProfileInput) { i.CPUs = "" }},
		{"memory", func(i *CreateProfileInput) { i.Memory = "  " }},
		{"labels", func(i *CreateProfileInput) { i.RunnerLabels = nil }},
		{"empty-label", func(i *CreateProfileInput) { i.RunnerLabels = []string{" "} }},
		{"image", func(i *CreateProfileInput) { i.DockerImage = "" }},
		{"scope", func(i *CreateProfileInput) { i.Scope = "unknown" }},
		{"name", func(i *CreateProfileInput) { i.Name = "../unsafe" }},
		{"service", func(i *CreateProfileInput) { i.ServiceName = "../unsafe.service" }},
		{"prefix", func(i *CreateProfileInput) { i.ContainerNamePrefix = "../unsafe" }},
		{"owner", func(i *CreateProfileInput) { i.RepoOwner = " " }},
		{"repo", func(i *CreateProfileInput) { i.RepoName = " " }},
		{"missing-name", func(i *CreateProfileInput) { i.Name = "" }},
		{"org", func(i *CreateProfileInput) {
			i.Scope = config.TargetScopeOrganization
			i.Org = ""
			i.Environment = "swift"
		}},
		{"environment", func(i *CreateProfileInput) {
			i.Scope = config.TargetScopeOrganization
			i.Org = "Example Org"
			i.Environment = ""
		}},
		{"docker-access", func(i *CreateProfileInput) { i.DockerAccess = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r, _, input := createFixture(t)
			tc.change(&input)
			if err := m.CreateProfile(context.Background(), input); !errors.Is(err, ErrInvalidCreateInput) {
				t.Fatalf("invalid input: %v", err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("invalid input ran commands: %v", r.calls)
			}
		})
	}
}

func TestNormalizeCreateRejectsLineInjection(t *testing.T) {
	for _, bad := range []string{"\n", "\r", "\x00"} {
		for _, field := range []string{"Name", "RepoOwner", "RepoName", "Org", "Environment", "DockerAccess", "DockerImage", "ServiceName", "ContainerNamePrefix", "CPUs", "Memory", "GitHubEnvFile", "RunnerLabels", "WatchRepositories", "global-env"} {
			t.Run(fmt.Sprintf("%q/%s", bad, field), func(t *testing.T) {
				_, _, cfg, input := createFixture(t)
				switch field {
				case "RunnerLabels":
					input.RunnerLabels = []string{"linux" + bad}
				case "WatchRepositories":
					input.WatchRepositories = []string{"me/app" + bad}
				case "global-env":
					input.GitHubEnvFile = ""
					cfg.GitHub.EnvFile += bad
				default:
					v := reflect.ValueOf(&input).Elem().FieldByName(field)
					v.SetString(v.String() + bad)
				}
				if _, err := normalizeCreateInput(cfg, input); !errors.Is(err, ErrInvalidCreateInput) {
					t.Fatalf("line injection: %v", err)
				}
			})
		}
	}
}

func TestCreateProfileRefusesExistingTargetsBeforeWriting(t *testing.T) {
	for _, unit := range []bool{false, true} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("unit=%t/symlink=%t", unit, symlink), func(t *testing.T) {
				m, r, cfg, input := createFixture(t)
				pPath := filepath.Join(cfg.Paths.ProfilesDir, "model-router.yaml")
				uPath := filepath.Join(m.SystemdUnitDir, input.ServiceName)
				existing, other := pPath, uPath
				if unit {
					existing, other = uPath, pPath
				}
				if symlink {
					if err := os.Symlink("missing-target", existing); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(existing, []byte("keep original"), 0o600); err != nil {
					t.Fatal(err)
				}
				m.openNewFile = func(string, int, os.FileMode) (*os.File, error) { t.Fatal("precheck wrote a file"); return nil, nil }
				err := m.CreateProfile(context.Background(), input)
				if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), existing) {
					t.Fatalf("existing target: %v", err)
				}
				if len(r.calls) != 0 {
					t.Fatalf("existing target ran commands: %v", r.calls)
				}
				if _, err := os.Lstat(other); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("other target created: %v", err)
				}
				assertCreateTargetUnchanged(t, existing, symlink)
			})
		}
	}
}

func assertCreateTargetUnchanged(t *testing.T, path string, symlink bool) {
	t.Helper()
	if symlink {
		link, err := os.Readlink(path)
		if err != nil || link != "missing-target" {
			t.Fatalf("link changed: %q %v", link, err)
		}
	} else {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "keep original" {
			t.Fatalf("existing content changed: %s %v", data, err)
		}
	}
}

func TestCreateProfileExclusiveWriteRejectsRaces(t *testing.T) {
	for _, unit := range []bool{false, true} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("unit=%t/symlink=%t", unit, symlink), func(t *testing.T) {
				m, r, cfg, input := createFixture(t)
				pPath := filepath.Join(cfg.Paths.ProfilesDir, "model-router.yaml")
				uPath := filepath.Join(m.SystemdUnitDir, input.ServiceName)
				racePath := pPath
				if unit {
					racePath = uPath
				}
				m.openNewFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
					if flags != os.O_WRONLY|os.O_CREATE|os.O_EXCL {
						t.Fatalf("nonexclusive flags: %d", flags)
					}
					if path == racePath {
						if symlink {
							if err := os.Symlink("missing-target", path); err != nil {
								t.Fatal(err)
							}
						} else if err := os.WriteFile(path, []byte("keep original"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return os.OpenFile(path, flags, mode)
				}
				err := m.CreateProfile(context.Background(), input)
				if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), racePath) {
					t.Fatalf("race: %v", err)
				}
				if len(r.calls) != 0 {
					t.Fatalf("race ran commands: %v", r.calls)
				}
				assertCreateTargetUnchanged(t, racePath, symlink)
				if unit {
					if _, loadErr := config.LoadProfile(pPath); loadErr != nil {
						t.Fatal(loadErr)
					}
					if !strings.Contains(err.Error(), pPath) || !strings.Contains(err.Error(), "retained") {
						t.Fatalf("partial create not reported: %v", err)
					}
				} else if _, err := os.Lstat(uPath); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("unit created: %v", err)
				}
			})
		}
	}
}

func TestCreateProfileRetainsFilesOnWriteFailure(t *testing.T) {
	for _, unit := range []bool{false, true} {
		t.Run(fmt.Sprint(unit), func(t *testing.T) {
			m, r, cfg, input := createFixture(t)
			pPath := filepath.Join(cfg.Paths.ProfilesDir, "model-router.yaml")
			uPath := filepath.Join(m.SystemdUnitDir, input.ServiceName)
			failPath := pPath
			if unit {
				failPath = uPath
			}
			m.openNewFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
				f, err := os.OpenFile(path, flags, mode)
				if err != nil || path != failPath {
					return f, err
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				return os.Open(path) // A real read-only file makes Write fail after creation/chmod.
			}
			err := m.CreateProfile(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), "retained") || !strings.Contains(err.Error(), failPath) {
				t.Fatalf("write failure: %v", err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("write failure ran commands: %v", r.calls)
			}
			if _, err := os.Lstat(failPath); err != nil {
				t.Fatalf("failed file removed: %v", err)
			}
			if unit {
				if _, err := config.LoadProfile(pPath); err != nil {
					t.Fatal(err)
				}
			} else if _, err := os.Lstat(uPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("unit created: %v", err)
			}
			m.openNewFile = nil
			if err := m.CreateProfile(context.Background(), input); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("retry overwrote partial create: %v", err)
			}
		})
	}
}

func TestCreateProfileSystemdFailureRetainsBothFiles(t *testing.T) {
	allCalls := []string{"systemctl daemon-reload", "systemctl enable gha-model-router.service", "systemctl start gha-model-router.service"}
	for index, call := range allCalls {
		t.Run(call, func(t *testing.T) {
			m, r, cfg, input := createFixture(t)
			cause := errors.New("systemctl failed")
			r.errors = map[string]error{call: cause}
			err := m.CreateProfile(context.Background(), input)
			pPath := filepath.Join(cfg.Paths.ProfilesDir, "model-router.yaml")
			uPath := filepath.Join(m.SystemdUnitDir, input.ServiceName)
			if !errors.Is(err, cause) || !strings.Contains(err.Error(), pPath) || !strings.Contains(err.Error(), uPath) || !strings.Contains(err.Error(), "retained") {
				t.Fatalf("systemd error: %v", err)
			}
			if !reflect.DeepEqual(r.calls, allCalls[:index+1]) {
				t.Fatalf("calls: %v", r.calls)
			}
			if _, err := config.LoadProfile(pPath); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(uPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateProfileMissingRootlessSocketDoesNotFallback(t *testing.T) {
	m, r, cfg, input := createFixture(t)
	if err := os.Remove(cfg.Docker.RootlessSocketPath); err != nil {
		t.Fatal(err)
	}
	err := m.CreateProfile(context.Background(), input)
	if err == nil || errors.Is(err, ErrInvalidCreateInput) || !strings.Contains(err.Error(), "rootless") {
		t.Fatalf("missing socket: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("socket failure ran commands: %v", r.calls)
	}
	if files, err := os.ReadDir(cfg.Paths.ProfilesDir); err != nil || len(files) != 0 {
		t.Fatalf("socket failure wrote profiles: %v %v", files, err)
	}
}

func TestCreateProfileOrganizationHonorsExplicitNamesAndWatch(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, watch := range []bool{false, true} {
			t.Run(fmt.Sprintf("explicit=%t/watch=%t", explicit, watch), func(t *testing.T) {
				m, r, cfg, input := createFixture(t)
				input.Scope, input.Org, input.Environment = config.TargetScopeOrganization, "Example Org", "swift"
				input.Name, input.ServiceName, input.ContainerNamePrefix = "", "", ""
				name, service, prefix := "example-org-swift", "gha-example-org-swift.service", "gha-example-org-swift"
				if explicit {
					input.Name, input.ServiceName, input.ContainerNamePrefix = "custom", "custom-unit.service", "custom-container"
					name, service, prefix = "custom", "custom-unit.service", "custom-container"
				}
				if watch {
					input.WatchRepositories = []string{"me/app", "me/other"}
				}
				m.GitHubAdmin = nil // Creation must not implicitly sync runner groups.
				if err := m.CreateProfile(context.Background(), input); err != nil {
					t.Fatal(err)
				}
				p, err := config.LoadProfile(filepath.Join(cfg.Paths.ProfilesDir, name+".yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if p.Name != name || p.Service.Name != service || p.Docker.ContainerNamePrefix != prefix || p.Runner.NamePrefix != name || p.RunnerGroup.Name != "example-org-swift" || !p.Runner.Ephemeral || p.Docker.CPUs != "0.8" || p.Docker.Memory != "512m" || p.Loop.StateFile != filepath.Join(cfg.Paths.StateDir, name+".json") || p.Loop.LogDir != filepath.Join(cfg.Paths.LogDir, name) || !reflect.DeepEqual(p.Runner.WatchRepositories, input.WatchRepositories) {
					t.Fatalf("org profile: %+v", p)
				}
				if !reflect.DeepEqual(r.calls, []string{"systemctl daemon-reload", "systemctl enable " + service, "systemctl start " + service}) {
					t.Fatalf("calls: %v", r.calls)
				}
			})
		}
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

func TestRestartLoopOnlyRestartsServiceEvenWhenBusy(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{}
	manager := NewRunnerManager(
		"",
		systemd.NewClient(runner),
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)

	snapshot := ProfileSnapshot{
		BusyState: state.BusyYes,
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

	if len(runner.calls) != 1 || runner.calls[0] != "systemctl restart gha-remind-me-exp.service" {
		t.Fatalf("restart must only call systemctl: %v", runner.calls)
	}
}

func TestStopLoopOnlyStopsServiceWithMultipleRunningContainers(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{
		outputs: map[string][]byte{
			"docker --host unix:///var/run/docker.sock ps --all --filter name=gha-remind-me- --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}": []byte(
				"new\tgha-remind-me-200\tgha-runner-base:latest\tUp 1 minute\n" +
					"old\tgha-remind-me-100\tgha-runner-base:latest\tUp 5 minutes\n" +
					"base\tgha-remind-me-050\tgha-runner-base:latest\tUp 10 hours\n",
			),
			"docker --host unix:///var/run/docker.sock inspect gha-remind-me-200": []byte(inspectJSON("new", "gha-remind-me-200", "gha-runner-base:latest", "running", "gha-remind-me-exp")),
			"docker --host unix:///var/run/docker.sock inspect gha-remind-me-100": []byte(inspectJSON("old", "gha-remind-me-100", "gha-runner-base:latest", "running", "gha-remind-me-exp")),
			"docker --host unix:///var/run/docker.sock inspect gha-remind-me-050": []byte(inspectJSON("base", "gha-remind-me-050", "gha-runner-base:latest", "running", "o-tokyo-s2-remind-me-base")),
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

	if err := manager.StopLoop(context.Background(), snapshot); err != nil {
		t.Fatalf("StopLoop returned error: %v", err)
	}

	expected := []string{
		"systemctl stop gha-remind-me-exp.service",
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
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir

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
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir

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
				docker.NewClient(&recordingRunner{}),
				gh.NewClient("", "", "", nil, nil),
			)
			manager.SystemdUnitDir = systemdDir
			tt.input.CPUs = "0.8"
			tt.input.Memory = "512m"

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
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir

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
		docker.NewClient(runner),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir

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
		docker.NewClient(&recordingRunner{}),
		gh.NewClient("", "", "", nil, nil),
	)
	manager.SystemdUnitDir = systemdDir

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
	return fmt.Sprintf(`[{"Id":"%s","Name":"/%s","Created":"2024-01-02T03:04:05Z","Config":{"Image":"%s","Env":["RUNNER_NAME=%s"]},"State":{"Status":"%s","StartedAt":"2024-01-02T03:04:05Z","ExitCode":0}}]`,
		id,
		name,
		image,
		runnerName,
		status,
	)
}

func TestRenderServiceFileIncludesStopSettings(t *testing.T) {
	t.Parallel()

	data, err := renderServiceFile(serviceTemplateData{
		ProfileName:       "remind-me-swift",
		GitHubEnvFile:     "/etc/gha-runner-tui/github.env",
		LoopBinaryPath:    "/usr/local/bin/gha-ephemeral-loop-tui",
		ProfileConfigPath: "/etc/gha-runner-tui/profiles/remind-me-swift.yaml",
	})
	if err != nil {
		t.Fatalf("renderServiceFile returned error: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"KillMode=mixed",
		"TimeoutStopSec=240",
		"EnvironmentFile=/etc/gha-runner-tui/github.env",
		"Restart=always",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in service file:\n%s", want, text)
		}
	}
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
