package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/buildinfo"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
	"gha-runner-tui/internal/systemd"
)

func TestParseSyncArgsRequiresProfileOrConfig(t *testing.T) {
	t.Parallel()

	opts, err := parseSyncArgs([]string{"--profile", "/tmp/profile.yaml"})
	if err != nil {
		t.Fatalf("parseSyncArgs returned error: %v", err)
	}
	if opts.profilePath != "/tmp/profile.yaml" {
		t.Fatalf("unexpected profile path: %q", opts.profilePath)
	}
}

func TestNewManagerUsesGlobalCredentialFallbackForAdmin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TEST_MANAGER_TOKEN", "global-fake")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer global-fake" || r.URL.RequestURI() != "/orgs/example/actions/runner-groups" {
			t.Errorf("wrong admin request: %s %s", r.Header.Get("Authorization"), r.URL.RequestURI())
		}
		fmt.Fprint(w, `{"runner_groups":[{"id":1,"name":"example","visibility":"private"}]}`)
	}))
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("github:\n  api_base_url: "+server.URL+"\n  token_env: TEST_MANAGER_TOKEN\n  env_file: "+filepath.Join(root, "missing.env")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newManager(path, root)
	p := config.Profile{Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "example"}, RunnerGroup: config.RunnerGroupConfig{Name: "example", Visibility: "private"}}
	if err := m.SyncRunnerGroup(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
}

type fakeSyncer struct {
	profilePath string
	configCalls int
}

func (f *fakeSyncer) SyncProfilePath(_ context.Context, profilePath string) error {
	f.profilePath = profilePath
	return nil
}

func (f *fakeSyncer) SyncConfigProfiles(context.Context) error {
	f.configCalls++
	return nil
}

func TestRunSyncWithProfilePath(t *testing.T) {
	t.Parallel()

	fake := &fakeSyncer{}
	err := runSyncWith(context.Background(), syncOptions{profilePath: "/tmp/profile.yaml"}, fake)
	if err != nil {
		t.Fatalf("runSyncWith returned error: %v", err)
	}
	if fake.profilePath != "/tmp/profile.yaml" {
		t.Fatalf("expected profile sync, got %q", fake.profilePath)
	}
	if fake.configCalls != 0 {
		t.Fatalf("did not expect config sync, got %d", fake.configCalls)
	}
}

func TestGithubConfigForManagerUsesEnvFileOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("github:\n  token_env: CI_GITHUB_TOKEN\n  env_file: /etc/gha-runner-tui/github.env\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := githubConfigForManager(configPath)
	if cfg.TokenEnv != "CI_GITHUB_TOKEN" {
		t.Fatalf("unexpected token env: %q", cfg.TokenEnv)
	}
	if cfg.EnvFile != "/etc/gha-runner-tui/github.env" {
		t.Fatalf("unexpected env file: %q", cfg.EnvFile)
	}
}

func TestInterspersedFlagsAfterProfile(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		config      string
		docker      bool
		n           int
		positionals []string
	}{
		{[]string{"p", "--docker", "-n", "20", "--config", "/tmp/x.yaml"}, "/tmp/x.yaml", true, 20, []string{"p"}},
		{[]string{"--docker=false", "p", "-config=x", "-n=0"}, "x", false, 0, []string{"p"}},
		{[]string{"--docker", "p", "--", "-n", "20"}, "default", true, 200, []string{"p", "-n", "20"}},
		{[]string{"p", "--config", "-path", "-n", "-1"}, "-path", false, -1, []string{"p"}},
		{[]string{"-", "-docker=false"}, "default", false, 200, []string{"-"}},
	} {
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		configPath := fs.String("config", "default", "")
		dockerLogs := fs.Bool("docker", false, "")
		tail := fs.Int("n", 200, "")
		if err := parseInterspersed(fs, tc.args); err != nil {
			t.Fatal(err)
		}
		if *configPath != tc.config || *dockerLogs != tc.docker || *tail != tc.n || !reflect.DeepEqual(fs.Args(), tc.positionals) {
			t.Fatalf("%v: config=%s docker=%v n=%d args=%v", tc.args, *configPath, *dockerLogs, *tail, fs.Args())
		}
	}
	for _, args := range [][]string{{"p", "--bad"}, {"p", "--config"}, {"--docker=maybe"}, {"---docker"}} {
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("config", "", "")
		fs.Bool("docker", false, "")
		if err := parseInterspersed(fs, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRunDispatchPreservesTUIAndVersion(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		config, units string
	}{
		{nil, "/etc/gha-runner-tui/config.yaml", "/etc/systemd/system"},
		{[]string{"-config", "x"}, "x", "/etc/systemd/system"},
		{[]string{"--config", "x", "-systemd-unit-dir", "y"}, "x", "y"},
	} {
		called := 0
		code := run(tc.args, io.Discard, io.Discard, func(c, u string) error {
			called++
			if c != tc.config || u != tc.units {
				t.Fatalf("got %s %s", c, u)
			}
			return nil
		}, func(string, string) app.RunnerManager {
			t.Fatal("manager constructed during TUI dispatch")
			return app.RunnerManager{}
		})
		if code != 0 || called != 1 {
			t.Fatalf("code=%d calls=%d", code, called)
		}
	}
	var out bytes.Buffer
	if code := run([]string{"version", "--config", "/not/read"}, &out, io.Discard,
		func(string, string) error { t.Fatal("TUI for version"); return nil },
		func(string, string) app.RunnerManager { t.Fatal("manager for version"); return app.RunnerManager{} }); code != 0 || out.String() != buildinfo.Current() {
		t.Fatalf("version: code=%d output=%q", code, out.String())
	}
	if code := run(nil, io.Discard, io.Discard, func(string, string) error { return errors.New("TUI failed") }, nil); code != 1 {
		t.Fatalf("TUI error code=%d", code)
	}
}

func TestCLIRejectsArgumentsBeforeManager(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"status", "p"}, {"status", "--bad"}, {"status", "--config"},
		{"version", "p"}, {"migrate", "p"}, {"start"}, {"stop", "p", "q"}, {"restart", "p", "--force"},
		{"start", ""}, {"logs", " "},
		{"logs", "p", "-n", "-1"}, {"logs", "p", "--docker=false", "false"}, {"logs", "p", "--follow"},
		{"create"}, {"create", "p"}, {"create", "--scope", "bad"}, {"sync", "--bad"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(args, io.Discard, &stderr, func(string, string) error { t.Fatal("TUI invoked"); return nil },
				func(string, string) app.RunnerManager {
					t.Fatal("manager before validation")
					return app.RunnerManager{}
				})
			if code != 2 || stderr.Len() == 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
	for _, args := range [][]string{{"--bad"}, {"--config"}, {"--config", "x", "p"}} {
		if code := run(args, io.Discard, io.Discard, func(string, string) error { t.Fatal("TUI for invalid flags"); return nil }, nil); code != 2 {
			t.Fatalf("%v: %d", args, code)
		}
	}
}

type cliRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f cliRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

type cliHTTPFunc func(*http.Request) (*http.Response, error)

func (f cliHTTPFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type cliFixture struct {
	manager                    app.RunnerManager
	root, cfg, profiles, units string
	calls                      [][]string
	respond                    func(string, []string) ([]byte, error)
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	f := &cliFixture{root: t.TempDir()}
	f.cfg, f.profiles, f.units = filepath.Join(f.root, "config.yaml"), filepath.Join(f.root, "profiles"), filepath.Join(f.root, "units")
	for _, dir := range []string{f.profiles, f.units} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCLIFile(t, f.cfg, fmt.Sprintf("paths:\n  profiles_dir: %s\n  state_dir: %s\n  log_dir: %s\ndocker:\n  default_access_mode: host-socket\ngithub:\n  env_file: %s\n", f.profiles, filepath.Join(f.root, "state"), filepath.Join(f.root, "logs"), filepath.Join(f.root, "fake.env")))
	runner := cliRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		if f.respond != nil {
			return f.respond(name, args)
		}
		if name == "systemctl" {
			switch args[0] {
			case "is-active":
				return []byte("active\n"), nil
			case "is-enabled":
				return []byte("enabled\n"), nil
			}
		}
		if name == "docker" && args[2] != "ps" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return nil, nil
	})
	f.manager = app.NewRunnerManager(f.cfg, systemd.NewClient(runner), docker.NewClient(runner),
		gh.NewGlobalClient("", "FAKE_CLI_PAT", "", runner, cliHTTPFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected GitHub request")
			return nil, errors.New("unexpected HTTP")
		})))
	f.manager.Runner = runner
	f.manager.SystemdUnitDir = f.units
	f.manager.Service.LegacyServiceDir = f.units
	// Status exercises per-profile failure without credentials or real HTTP.
	f.manager.Service.GitHubForProfile = nil
	f.profile(t, "p")
	return f
}

func writeCLIFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *cliFixture) profile(t *testing.T, name string) {
	t.Helper()
	writeCLIFile(t, filepath.Join(f.profiles, name+".yaml"), fmt.Sprintf("name: %s\nrepo: {owner: me, name: app}\nservice: {name: gha-%s.service}\nrunner: {name_prefix: %s}\ndocker:\n  container_name_prefix: gha-%s\n  env: {RUNNER_TOKEN: fake-PAT, REG_TOKEN: fake-REG_TOKEN}\nloop: {state_file: %s}\n", name, name, name, name, filepath.Join(f.root, name+".json")))
}

func (f *cliFixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	args = append(args, "--config", f.cfg)
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), args, &stdout, &stderr, func(c, u string) app.RunnerManager {
		if c != f.cfg || u != "/etc/systemd/system" {
			t.Fatalf("manager config=%s units=%s", c, u)
		}
		return f.manager
	})
	for _, secret := range []string{"fake-PAT", "fake-REG_TOKEN", "RUNNER_TOKEN=", "REG_TOKEN="} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatalf("secret leaked: %q %q", stdout.String(), stderr.String())
		}
	}
	return code, stdout.String(), stderr.String()
}

func TestCLILifecycleUsesOnlySystemctlAndScopedHolders(t *testing.T) {
	for _, cmd := range []string{"start", "restart"} {
		f := newCLIFixture(t)
		code, out, stderr := f.run(t, cmd, "p")
		if code != 0 || !strings.Contains(out, "p") || stderr != "" || !reflect.DeepEqual(f.calls, [][]string{{"systemctl", cmd, "gha-p.service"}}) {
			t.Fatalf("%s code=%d out=%q err=%q calls=%v", cmd, code, out, stderr, f.calls)
		}
	}
	for _, force := range []bool{false, true} {
		f := newCLIFixture(t)
		f.respond = func(name string, args []string) ([]byte, error) {
			if name == "docker" && args[2] == "ps" {
				if !strings.Contains(strings.Join(args, " "), "label=io.gha-runner-tui.profile=p") {
					t.Fatal("unscoped holder query")
				}
				return []byte("id-created\tgha-p-created\timage\tcreated\tp\tp-r\nid-running\tgha-p-running\timage\trunning\tp\tp-r2\nid-other\tgha-other\timage\trunning\tother\tother-r\n"), nil
			}
			return nil, nil
		}
		args := []string{"stop", "p"}
		if force {
			args = append(args, "--force")
		}
		code, out, stderr := f.run(t, args...)
		if code != 0 || f.calls[0][0] != "systemctl" || !reflect.DeepEqual(f.calls[0], []string{"systemctl", "stop", "gha-p.service"}) {
			t.Fatalf("stop: %d %q %q %v", code, out, stderr, f.calls)
		}
		if force {
			if !strings.Contains(out+stderr, "正在跑的 job 会失败") {
				t.Fatalf("no force warning: %q %q", out, stderr)
			}
			var removes [][]string
			for _, c := range f.calls {
				if c[0] == "docker" && c[3] == "rm" {
					removes = append(removes, c)
				}
			}
			want := [][]string{{"docker", "--host", "unix:///var/run/docker.sock", "rm", "-f", "id-created"}, {"docker", "--host", "unix:///var/run/docker.sock", "rm", "-f", "id-running"}}
			if !reflect.DeepEqual(removes, want) {
				t.Fatalf("removes=%v", removes)
			}
		} else {
			if len(f.calls) != 2 || !strings.Contains(out, "created") || !strings.Contains(out, "仍在跑 job，跑完后自动退出") {
				t.Fatalf("nonforce: calls=%v out=%q", f.calls, out)
			}
		}
	}
}

func TestCLILogsJournalIndependentOfDockerAndGitHub(t *testing.T) {
	f := newCLIFixture(t)
	f.respond = func(name string, args []string) ([]byte, error) {
		if name != "journalctl" {
			t.Fatalf("journal invoked %s %v (Docker daemon/Env unavailable)", name, args)
		}
		return []byte("journal line\n"), nil
	}
	for _, n := range []string{"20", "0"} {
		f.calls = nil
		code, out, stderr := f.run(t, "logs", "p", "-n", n, "--docker=false")
		if code != 0 || out != "journal line\n" || stderr != "" || !reflect.DeepEqual(f.calls, [][]string{{"journalctl", "-u", "gha-p.service", "-n", n, "--no-pager"}}) {
			t.Fatalf("code=%d out=%q stderr=%q calls=%v", code, out, stderr, f.calls)
		}
	}
}

func TestCLIDockerLogsSelectCurrentOrLatestAndFailClosed(t *testing.T) {
	for _, mode := range []string{"current", "latest", "none", "bad-env", "inspect-error", "logs-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			if mode == "current" {
				writeCLIFile(t, filepath.Join(f.root, "p.json"), `{"state":"running-job","last_container_name":"gha-p-current"}`)
			}
			logs := 0
			target := "gha-p-latest"
			if mode == "current" {
				target = "gha-p-current"
			}
			f.respond = func(name string, args []string) ([]byte, error) {
				if name != "docker" || args[0] != "--host" || args[1] != "unix:///var/run/docker.sock" {
					t.Fatalf("unexpected %s %v", name, args)
				}
				switch args[2] {
				case "ps":
					if mode == "none" {
						return nil, nil
					}
					return []byte("new\tgha-p-latest\timage\texited\tp\tr-new\nold\tgha-p-current\timage\trunning\tp\tr-old\n"), nil
				case "inspect":
					if args[3] != target {
						t.Fatalf("inspect target=%s", args[3])
					}
					if mode == "bad-env" {
						return []byte(`[{"Config":{"Env":null},"secret":"fake-PAT"}]`), nil
					}
					if mode == "inspect-error" {
						return []byte("fake-PAT fake-REG_TOKEN raw inspect"), errors.New("fake-PAT raw error")
					}
					// Invalid full state/timestamps must not prevent Env-only redaction.
					return []byte(`[{"Created":"bad","State":"bad","Config":{"Env":["RUNNER_TOKEN=fake-PAT","REG_TOKEN=fake-REG_TOKEN"]}}]`), nil
				case "logs":
					logs++
					if !reflect.DeepEqual(args[2:], []string{"logs", "--tail", "20", target}) {
						t.Fatalf("logs args=%v", args)
					}
					if mode == "logs-error" {
						return []byte("fake-PAT fake-REG_TOKEN raw logs"), errors.New("fake-PAT raw error")
					}
					return []byte("tokens: fake-PAT fake-REG_TOKEN\n"), nil
				default:
					t.Fatalf("unexpected docker %v", args)
				}
				return nil, nil
			}
			code, out, stderr := f.run(t, "logs", "p", "--docker", "-n", "20")
			if mode == "current" || mode == "latest" {
				if code != 0 || out != "tokens: [REDACTED] [REDACTED]\n" || stderr != "" || logs != 1 {
					t.Fatalf("code=%d out=%q err=%q logs=%d", code, out, stderr, logs)
				}
			} else {
				if code != 1 || stderr == "" || strings.Contains(out+stderr, "raw error") || strings.Contains(out+stderr, "raw inspect") {
					t.Fatalf("code=%d out=%q err=%q", code, out, stderr)
				}
				if mode != "logs-error" && (logs != 0 || out != "") {
					t.Fatalf("unsafe logs=%d out=%q", logs, out)
				}
			}
		})
	}
}

func TestWriteStatusStrictDTOAndText(t *testing.T) {
	lastError := "loop problem"
	slotError := "ps unavailable"
	d := app.Dashboard{
		SlotHolders: []docker.ContainerInfo{{Profile: "p", Name: "c", RunnerName: "r", State: state.ContainerCreated, ID: "private-id", Image: "private-image"}},
		Profiles: []app.ProfileSnapshot{{Profile: config.Profile{Name: "p", Docker: config.DockerProfile{Env: map[string]string{"PAT": "fake-PAT"}}},
			Service: systemd.ServiceStatus{Active: state.SystemdActive, Enabled: true}, DisplayLoopState: state.LoopRunningJob,
			Loop:      state.LoopState{LastTransitionAt: time.Date(2026, 10, 7, 1, 0, 0, 0, time.FixedZone("+1", 3600)), LastError: &lastError},
			Container: docker.ContainerInfo{Name: "c", State: state.ContainerCreated}, GitHubState: state.GitHubOnline, BusyState: state.BusyYes, Health: state.HealthRunning,
			Errors: []string{"first", "second"}}},
		ProfileErrors: []config.ProfileLoadError{{Path: "bad.yaml", Err: errors.New("bad YAML")}},
	}
	var out bytes.Buffer
	if err := writeStatus(&out, d, true); err != nil {
		t.Fatal(err)
	}
	want := `{"slot_holders":[{"profile":"p","container":"c","runner":"r","state":"created"}],"slot_error":null,"profiles":[{"name":"p","service":{"active":"active","enabled":true},"loop":{"state":"running-job","last_transition_at":"2026-10-07T00:00:00Z","last_error":"loop problem"},"container":{"name":"c","state":"created"},"github":{"state":"online","busy":"yes"},"health":"running","errors":["first","second"]}],"profile_errors":[{"path":"bad.yaml","error":"bad YAML"}]}` + "\n"
	if out.String() != want {
		t.Fatalf("JSON= %s want %s", out.String(), want)
	}
	out.Reset()
	if err := writeStatus(&out, app.Dashboard{}, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"slot_holders\":[],\"slot_error\":null,\"profiles\":[],\"profile_errors\":[]}\n" {
		t.Fatalf("empty JSON=%s", out.String())
	}
	d.Profiles[0].Loop = state.LoopState{}
	d.Profiles[0].Errors = nil
	out.Reset()
	if err := writeStatus(&out, d, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"last_transition_at":"","last_error":null`) || !strings.Contains(out.String(), `"errors":[]`) {
		t.Fatalf("zero fields=%s", out.String())
	}
	for _, tc := range []struct {
		holders []docker.ContainerInfo
		err     *string
		want    string
	}{
		{nil, nil, "SLOT: free"}, {d.SlotHolders, nil, "SLOT: p created"},
		{append(append([]docker.ContainerInfo{}, d.SlotHolders...), docker.ContainerInfo{Profile: "q", Name: "cq", State: state.ContainerRunning}), nil, "SLOT: WARNING"},
		{nil, &slotError, "SLOT: unknown"},
	} {
		out.Reset()
		d.SlotHolders, d.SlotError = tc.holders, tc.err
		d.Profiles[0].Errors = []string{"first", "second"}
		if err := writeStatus(&out, d, false); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if !strings.Contains(text, tc.want) || !strings.Contains(text, "active/enabled") || !strings.Contains(text, "p: first\np: second\n") || !strings.Contains(text, "bad.yaml: bad YAML") {
			t.Fatalf("text=%q", text)
		}
		if tc.err != nil && (strings.Contains(text, "SLOT: free") || !strings.Contains(text, *tc.err)) {
			t.Fatalf("failed open: %q", text)
		}
		for _, c := range tc.holders {
			if !strings.Contains(text, c.Name) {
				t.Fatalf("holder omitted: %s", text)
			}
		}
	}
}

type cliFileSnapshot struct {
	Content string
	ModTime time.Time
	Mode    fs.FileMode
}

func snapshotCLIDirectory(t *testing.T, dir string) map[string]cliFileSnapshot {
	t.Helper()
	files := map[string]cliFileSnapshot{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content := ""
		if !entry.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content = string(raw)
		}
		files[path] = cliFileSnapshot{content, info.ModTime(), info.Mode()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCLIStatusReadOnlyAndPartialFailuresRemainSuccess(t *testing.T) {
	f := newCLIFixture(t)
	f.profile(t, "q")
	writeCLIFile(t, filepath.Join(f.profiles, "bad.yaml"), "name: [broken\n")
	writeCLIFile(t, filepath.Join(f.profiles, "p.yaml.bak"), "unchanged backup")
	before := snapshotCLIDirectory(t, f.profiles)
	f.respond = func(name string, args []string) ([]byte, error) {
		if name == "docker" {
			return []byte("raw inspect fake-PAT"), errors.New("ps unavailable")
		}
		if name == "systemctl" {
			return nil, errors.New("profile service unavailable")
		}
		t.Fatalf("unexpected read command %s %v", name, args)
		return nil, nil
	}
	for _, jsonOutput := range []bool{false, true} {
		args := []string{"status"}
		if jsonOutput {
			args = append(args, "--json")
		}
		code, out, stderr := f.run(t, args...)
		if code != 0 || stderr != "" {
			t.Fatalf("partial status code=%d stderr=%q", code, stderr)
		}
		if jsonOutput {
			var data map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &data); err != nil {
				t.Fatal(err)
			}
			keys := []string{}
			for k := range data {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, []string{"profile_errors", "profiles", "slot_error", "slot_holders"}) || string(data["slot_error"]) != `"ps unavailable"` {
				t.Fatalf("status JSON=%s", out)
			}
		} else if !strings.Contains(out, "SLOT: unknown") || strings.Contains(out, "SLOT: free") || !strings.Contains(out, "bad.yaml") || !strings.Contains(out, "profile service unavailable") {
			t.Fatalf("status text=%q", out)
		}
	}
	if after := snapshotCLIDirectory(t, f.profiles); !reflect.DeepEqual(before, after) {
		t.Fatalf("status wrote profiles: before=%v after=%v", before, after)
	}
	writeCLIFile(t, f.cfg, "paths: [invalid\n")
	f.calls = nil
	if code, _, _ := f.run(t, "status"); code != 1 || len(f.calls) != 0 {
		t.Fatalf("global error code=%d calls=%v", code, f.calls)
	}
}

func validCLICreateArgs() []string {
	return []string{"create", "--name", "new", "--owner", "me", "--repo", "app", "--labels", " linux, , x64 ", "--image", "runner:test", "--cpus", "2", "--memory", "4g"}
}

func TestCLICreateValidationBeforeOperations(t *testing.T) {
	base := validCLICreateArgs()
	cases := [][]string{}
	for _, flagName := range []string{"--name", "--owner", "--repo", "--labels", "--image", "--cpus", "--memory"} {
		args := append([]string{}, base...)
		for i := 1; i < len(args); i += 2 {
			if args[i] == flagName {
				args = append(args[:i], args[i+2:]...)
				break
			}
		}
		cases = append(cases, args)
	}
	cases = append(cases, append(append([]string{}, base...), "--watch", "other/app"), append(append([]string{}, base...), "--watch", "me/app,me/app"), append(append([]string{}, base...), "--watch", "bad"), append(append([]string{}, base...), "--name", "../escape"), append(append([]string{}, base...), "--image", "bad\nvalue"), append(append([]string{}, base...), "--docker-access", "bad"))
	org := []string{"create", "--scope", "organization", "--org", "Example", "--environment", "base", "--labels", "linux", "--image", "image", "--cpus", "1", "--memory", "1g"}
	cases = append(cases, org, append(append([]string{}, org...), "--watch", "bad"), append(append([]string{}, org...), "--watch", "me/app/extra"))
	for _, args := range cases {
		if code := runCLI(context.Background(), args, io.Discard, io.Discard, func(string, string) app.RunnerManager {
			t.Fatalf("manager before create validation %v", args)
			return app.RunnerManager{}
		}); code != 2 {
			t.Fatalf("%v code=%d", args, code)
		}
	}
}

func TestCLICreateMapsFlagsAndRefusesExisting(t *testing.T) {
	f := newCLIFixture(t)
	args := append(validCLICreateArgs(), "--no-start", "--service", "custom.service", "--container-prefix", "custom", "--watch", " me/app , ", "--github-env-file", filepath.Join(f.root, "new.env"))
	code, out, stderr := f.run(t, args...)
	if code != 0 || stderr != "" || !strings.Contains(out, "new") {
		t.Fatalf("create code=%d out=%q stderr=%q", code, out, stderr)
	}
	p, err := config.LoadProfile(filepath.Join(f.profiles, "new.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Docker.CPUs != "2" || p.Docker.Memory != "4g" || p.Docker.Image != "runner:test" || p.Service.Name != "custom.service" || p.Docker.ContainerNamePrefix != "custom" || p.GitHub.EnvFile != filepath.Join(f.root, "new.env") || !p.Runner.Ephemeral || !reflect.DeepEqual(p.Runner.Labels, []string{"linux", "x64"}) || !reflect.DeepEqual(p.Runner.WatchRepositories, []string{"me/app"}) {
		t.Fatalf("create mapping=%+v", p)
	}
	if !reflect.DeepEqual(f.calls, [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "custom.service"}}) {
		t.Fatalf("no-start calls=%v", f.calls)
	}
	before := snapshotCLIDirectory(t, f.profiles)
	f.calls = nil
	if code, _, _ := f.run(t, args...); code != 1 || len(f.calls) != 0 {
		t.Fatalf("existing code=%d calls=%v", code, f.calls)
	}
	if !reflect.DeepEqual(before, snapshotCLIDirectory(t, f.profiles)) {
		t.Fatal("existing profile overwritten")
	}
	org := []string{"create", "--scope", "organization", "--org", "Example", "--environment", "base", "--labels", "linux", "--image", "image", "--cpus", "1", "--memory", "1g", "--watch", " me/app, other/repo "}
	if code, _, stderr := f.run(t, org...); code != 0 {
		t.Fatalf("org create=%d %s", code, stderr)
	}
	p, err = config.LoadProfile(filepath.Join(f.profiles, "example-base.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Target.Scope != config.TargetScopeOrganization || !reflect.DeepEqual(p.Runner.WatchRepositories, []string{"me/app", "other/repo"}) || f.calls[len(f.calls)-1][1] != "start" {
		t.Fatalf("org=%+v calls=%v", p, f.calls)
	}
}

func TestCLIMigrateAndSyncExplicitlyWriteAndReport(t *testing.T) {
	for _, cmd := range []string{"migrate", "sync"} {
		f := newCLIFixture(t)
		if cmd == "migrate" {
			writeCLIFile(t, filepath.Join(f.profiles, "bad.yaml"), "name: [bad\n")
		}
		code, out, stderr := f.run(t, cmd)
		if cmd == "migrate" {
			if code != 1 || !strings.Contains(out, "p.yaml\tupdated\tset explicit github credential config") || !strings.Contains(out, "bad.yaml\tfailed\t") || stderr == "" {
				t.Fatalf("migrate code=%d out=%q err=%q", code, out, stderr)
			}
		} else if code != 0 || out != "runner groups synced\n" || stderr != "" {
			t.Fatalf("sync code=%d out=%q err=%q", code, out, stderr)
		}
		if _, err := os.Stat(filepath.Join(f.profiles, "p.yaml.bak")); err != nil {
			t.Fatal("explicit migration did not retain backup", err)
		}
	}
}

type cliFailWriter struct{}

func (cliFailWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }

func TestCLIWriterFailuresAndOperationErrorClassification(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		if err := writeStatus(cliFailWriter{}, app.Dashboard{}, jsonOutput); err == nil {
			t.Fatal("status ignored writer error")
		}
	}
	for _, err := range []error{fmt.Errorf("permission denied; run create with sudo (sudo gha-runner-tui create ...): %w", fs.ErrPermission), fmt.Errorf("wrapped: %w", app.ErrInvalidCreateInput)} {
		var stderr bytes.Buffer
		code := writeOperationError(&stderr, err)
		want := 1
		if errors.Is(err, app.ErrInvalidCreateInput) {
			want = 2
		}
		if code != want || !strings.Contains(stderr.String(), err.Error()) {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	}
	for _, cmd := range []string{"version", "status", "logs", "start", "restart", "stop", "migrate", "sync", "create"} {
		f := newCLIFixture(t)
		args := []string{cmd}
		if cmd == "logs" || cmd == "start" || cmd == "stop" || cmd == "restart" {
			args = append(args, "p")
		}
		if cmd == "create" {
			args = validCLICreateArgs()
		}
		args = append(args, "--config", f.cfg)
		code := runCLI(context.Background(), args, cliFailWriter{}, io.Discard, func(string, string) app.RunnerManager { return f.manager })
		if code != 1 {
			t.Fatalf("%s ignored stdout failure: %d", cmd, code)
		}
	}
}

func TestCLIGlobalConfigErrorPreventsOperations(t *testing.T) {
	for _, cmd := range []string{"start", "stop", "restart", "logs", "create", "migrate", "sync"} {
		f := newCLIFixture(t)
		writeCLIFile(t, f.cfg, "paths: [bad\n")
		before := snapshotCLIDirectory(t, f.profiles)
		args := []string{cmd}
		if cmd == "start" || cmd == "stop" || cmd == "restart" || cmd == "logs" {
			args = append(args, "p")
		}
		if cmd == "create" {
			args = validCLICreateArgs()
		}
		if cmd == "sync" {
			args = append(args, "--profile", filepath.Join(f.profiles, "p.yaml"))
		}
		code, _, stderr := f.run(t, args...)
		if code != 1 || stderr == "" || len(f.calls) != 0 || !reflect.DeepEqual(before, snapshotCLIDirectory(t, f.profiles)) {
			t.Fatalf("%s code=%d err=%q calls=%v", cmd, code, stderr, f.calls)
		}
	}
}

func TestCLIStatusSlotsThroughRealManager(t *testing.T) {
	for _, tc := range []struct {
		ps, slotText string
		holders      int
	}{
		{"", "SLOT: free", 0},
		{"id\tgha-p-created\timage\tcreated\tp\tp-r\n", "SLOT: p created", 1},
		{"id\tgha-p-created\timage\tcreated\tp\tp-r\nother\tgha-q-running\timage\trunning\tq\tq-r\n", "SLOT: WARNING", 2},
	} {
		f := newCLIFixture(t)
		f.profile(t, "q")
		writeCLIFile(t, filepath.Join(f.root, "fake.env"), "GITHUB_TOKEN=fake-PAT\n")
		f.manager.Service.GitHubForProfile = func(cfg config.GlobalConfig, p config.Profile) app.GitHubClient {
			return gh.NewGlobalClient("", cfg.GitHub.TokenEnv, cfg.GitHub.EnvFile, f.manager.Runner, cliHTTPFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer fake-PAT" {
					t.Fatal("wrong fake credential")
				}
				if p.Name == "p" {
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("fake-PAT private API body"))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"runners":[{"id":1,"name":"q-r","status":"online","busy":true}]}`))}, nil
			}))
		}
		f.respond = func(name string, args []string) ([]byte, error) {
			if name == "systemctl" {
				if args[0] == "is-active" {
					return []byte("active"), nil
				}
				return []byte("enabled"), nil
			}
			if name == "docker" && args[2] == "ps" {
				text := tc.ps
				for i, arg := range args {
					if arg == "--filter" && i+1 < len(args) && strings.HasPrefix(args[i+1], "name=") {
						return nil, nil
					}
				}
				return []byte(text), nil
			}
			t.Fatalf("unexpected command %s %v", name, args)
			return nil, nil
		}
		before := snapshotCLIDirectory(t, f.profiles)
		code, out, stderr := f.run(t, "status")
		if code != 0 || stderr != "" || !strings.Contains(out, tc.slotText) || !strings.Contains(out, "online") || !strings.Contains(out, "github:") {
			t.Fatalf("status code=%d out=%q err=%q", code, out, stderr)
		}
		code, out, stderr = f.run(t, "status", "--json")
		var data struct {
			SlotHolders []map[string]string `json:"slot_holders"`
			Profiles    []struct {
				Name   string `json:"name"`
				GitHub struct {
					State string `json:"state"`
				} `json:"github"`
			} `json:"profiles"`
		}
		if err := json.Unmarshal([]byte(out), &data); err != nil {
			t.Fatal(err)
		}
		if code != 0 || stderr != "" || len(data.SlotHolders) != tc.holders || len(data.Profiles) != 2 || data.Profiles[0].GitHub.State != "unknown" || data.Profiles[1].GitHub.State != "online" {
			t.Fatalf("JSON=%s code=%d stderr=%q", out, code, stderr)
		}
		if !reflect.DeepEqual(before, snapshotCLIDirectory(t, f.profiles)) {
			t.Fatal("status changed profiles")
		}
	}
}

func TestCLIExplicitFalseAndDefaultLogTail(t *testing.T) {
	f := newCLIFixture(t)
	f.respond = func(name string, args []string) ([]byte, error) {
		if name == "docker" && args[2] == "ps" {
			return []byte("id\tgha-p-created\timage\tcreated\tp\tr\n"), nil
		}
		return nil, nil
	}
	code, out, stderr := f.run(t, "stop", "p", "--force=false")
	if code != 0 || len(f.calls) != 2 || !strings.Contains(out, "created") || strings.Contains(out+stderr, "仍在跑 job") || strings.Contains(out+stderr, "会失败") {
		t.Fatalf("force=false code=%d out=%q err=%q calls=%v", code, out, stderr, f.calls)
	}
	f.calls = nil
	f.respond = func(name string, args []string) ([]byte, error) {
		if name != "journalctl" {
			t.Fatal("default logs queried non-journal")
		}
		return []byte("journal"), nil
	}
	code, out, stderr = f.run(t, "logs", "p")
	if code != 0 || out != "journal" || stderr != "" || !reflect.DeepEqual(f.calls, [][]string{{"journalctl", "-u", "gha-p.service", "-n", "200", "--no-pager"}}) {
		t.Fatalf("default logs code=%d out=%q err=%q calls=%v", code, out, stderr, f.calls)
	}
}

func TestCLILifecycleFailureDoesNotEscalate(t *testing.T) {
	for _, cmd := range []string{"start", "stop", "restart"} {
		f := newCLIFixture(t)
		f.respond = func(name string, args []string) ([]byte, error) {
			if name != "systemctl" {
				t.Fatal("Docker after failed systemctl")
			}
			return nil, errors.New("systemctl failed")
		}
		args := []string{cmd, "p"}
		if cmd == "stop" {
			args = append(args, "--force")
		}
		if code, out, stderr := f.run(t, args...); code != 1 || out != "" || !strings.Contains(stderr, "systemctl failed") || len(f.calls) != 1 {
			t.Fatalf("%s code=%d out=%q err=%q calls=%v", cmd, code, out, stderr, f.calls)
		}
	}
	for _, cmd := range []string{"start", "stop", "restart", "logs"} {
		f := newCLIFixture(t)
		if code, out, stderr := f.run(t, cmd, "missing"); code != 1 || out != "" || !strings.Contains(stderr, "profile not found") || len(f.calls) != 0 {
			t.Fatalf("%s code=%d out=%q err=%q calls=%v", cmd, code, out, stderr, f.calls)
		}
	}
	for _, failQuery := range []bool{false, true} {
		f := newCLIFixture(t)
		f.respond = func(name string, args []string) ([]byte, error) {
			if name == "systemctl" {
				return nil, nil
			}
			if args[2] != "ps" {
				t.Fatalf("forced removal after failed query/warning: %v", args)
			}
			if failQuery {
				return []byte("id\tgha-p\timage\trunning\tp\tr\n"), errors.New("query failed")
			}
			return []byte("id\tgha-p\timage\trunning\tp\tr\n"), nil
		}
		code := runCLI(context.Background(), []string{"stop", "p", "--force", "--config", f.cfg}, io.Discard, cliFailWriter{}, func(string, string) app.RunnerManager { return f.manager })
		if code != 1 || len(f.calls) != 2 {
			t.Fatalf("query/warning failure code=%d calls=%v", code, f.calls)
		}
	}
	if code := writeOperationError(cliFailWriter{}, app.ErrInvalidCreateInput); code != 1 {
		t.Fatalf("stderr failure code=%d", code)
	}
}
