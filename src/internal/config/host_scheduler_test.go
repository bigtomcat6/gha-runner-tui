package config

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestBudgetRequiresBothLayers(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    ResourceBudget
		ok   bool
	}{
		{"fits", ResourceBudget{OuterBytes: 240, InnerBytes: 240, OuterPeak: 200, InnerPeak: 200, BaselineBytes: 100, HostBytes: 1000, CPUs: "1", EvidenceDigest: "audit-1"}, true},
		{"outer-only", ResourceBudget{OuterBytes: 240, OuterPeak: 200, HostBytes: 1000}, false},
		{"over-host", ResourceBudget{OuterBytes: 700, InnerBytes: 700, OuterPeak: 500, InnerPeak: 500, BaselineBytes: 100, HostBytes: 1000, CPUs: "1", EvidenceDigest: "audit-1"}, false},
		{"peak-overflow", ResourceBudget{OuterBytes: math.MaxUint64, InnerBytes: 240, OuterPeak: math.MaxUint64, InnerPeak: 200, HostBytes: math.MaxUint64, CPUs: "1", EvidenceDigest: "audit"}, false},
		{"sum-overflow", ResourceBudget{OuterBytes: math.MaxUint64, InnerBytes: 240, OuterPeak: 200, InnerPeak: 200, HostBytes: math.MaxUint64, CPUs: "1", EvidenceDigest: "audit"}, false},
		{"round-up", ResourceBudget{OuterBytes: 241, InnerBytes: 240, OuterPeak: 201, InnerPeak: 200, HostBytes: 1000, CPUs: "1", EvidenceDigest: "audit"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBudget(tc.b); (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
}

func hostFixture() (GlobalConfig, Profile) {
	outer := DaemonRef{Endpoint: "unix:///run/docker.sock", UID: 0}
	inner := DaemonRef{Endpoint: "unix:///run/user/1001/docker.sock", UID: 1001, Rootless: true}
	g := DefaultGlobalConfig()
	g.Scheduler.Mode = "host-single"
	g.Scheduler.OuterDockerEndpoint = outer.Endpoint
	g.Scheduler.Inventory = HostInventory{Daemons: []DaemonRef{outer, inner}, AuditDigest: "inventory-1"}
	p := Profile{Name: "base", Repo: RepoConfig{Owner: "alice", Name: "project"}, Service: ServiceConfig{Name: "gha-base.service"}, GitHub: GitHubProfile{CredentialID: "personal", ResourceOwner: "alice", TokenEnv: "PERSONAL_TOKEN"}, Runner: RunnerConfig{Ephemeral: true, NamePrefix: "base", Workdir: "/work", Labels: []string{"custom"}}, Docker: DockerProfile{Image: "runner@sha256:" + strings.Repeat("a", 64), ContainerNamePrefix: "gha-base", AccessMode: DockerAccessModeRootless}}
	p.Scheduler = ProfileSchedulerConfig{Enabled: true, Repositories: []RepoRef{{Owner: "alice", Name: "project"}}, Routes: []RouteProof{{WorkflowRef: "alice/project/.github/workflows/ci.yml@main", Labels: []string{"custom"}, EvidenceDigest: "route-1"}}, EvidenceFile: "/etc/gha-runner-tui/evidence/base.json", Budget: ResourceBudget{OuterBytes: 240, InnerBytes: 240, OuterPeak: 200, InnerPeak: 200, BaselineBytes: 100, HostBytes: 1000, CPUs: "1", EvidenceDigest: "budget-1"}, Execution: ExecutionBinding{Outer: outer, Inner: inner, InventoryDigest: "inventory-1", EvidenceIDs: []string{"execution-1"}, MountRefs: []string{"/opt/runner-tools:/tools:ro"}}}
	return g, p
}

func TestHostRejectsOverlappingNames(t *testing.T) {
	g, p := hostFixture()
	if err := ValidateHostConfig(g, []Profile{p}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "service", "container", "runner", "source"} {
		t.Run(field, func(t *testing.T) {
			q := p
			q.Name = "swift"
			q.Service.Name = "gha-swift.service"
			q.Docker.ContainerNamePrefix = "gha-swift"
			q.Runner.NamePrefix = "swift"
			p.Source = "/profiles/base.yaml"
			q.Source = "/profiles/swift.yaml"
			switch field {
			case "id":
				q.Name = p.Name
			case "service":
				q.Service.Name = p.Service.Name
			case "container":
				q.Docker.ContainerNamePrefix = p.Docker.ContainerNamePrefix + "-swift"
			case "runner":
				q.Runner.NamePrefix = p.Runner.NamePrefix + "-swift"
			case "source":
				q.Source = p.Source
			}
			if ValidateHostConfig(g, []Profile{p, q}) == nil {
				t.Fatal("overlap accepted")
			}
		})
	}
}

func TestPersonalScopeCannotExpand(t *testing.T) {
	g, p := hostFixture()
	p.Scheduler.Repositories = append(p.Scheduler.Repositories, RepoRef{Owner: "alice", Name: "other"})
	if ValidateHostConfig(g, []Profile{p}) == nil {
		t.Fatal("personal scope expanded")
	}
}

func TestHostRejectsHostSocket(t *testing.T) {
	for _, mutate := range []func(*Profile){
		func(p *Profile) { p.Docker.AccessMode = DockerAccessModeHostSocket },
		func(p *Profile) { p.Docker.Volumes = []string{"/var/run/docker.sock:/var/run/docker.sock"} },
		func(p *Profile) { p.Scheduler.Execution.MountRefs = []string{"/etc/gha-runner-tui:/control"} },
		func(p *Profile) { p.Docker.Env = map[string]string{"GITHUB_TOKEN": "test-secret"} },
	} {
		g, p := hostFixture()
		mutate(&p)
		if ValidateHostConfig(g, []Profile{p}) == nil {
			t.Fatal("unsafe host input accepted")
		}
	}
}

func TestHostEnabledGate(t *testing.T) {
	for _, mutate := range []func(*GlobalConfig, *Profile){
		func(g *GlobalConfig, p *Profile) { g.Scheduler.Mode = "unknown" },
		func(g *GlobalConfig, p *Profile) { g.Scheduler.PollIntervalSeconds = 14 },
		func(g *GlobalConfig, p *Profile) { g.Scheduler.IdleRunnerTTLSeconds = 301 },
		func(g *GlobalConfig, p *Profile) { g.Scheduler.OuterDockerEndpoint = "" },
		func(g *GlobalConfig, p *Profile) { p.Runner.Ephemeral = false },
		func(g *GlobalConfig, p *Profile) { p.Docker.Image = "runner:latest" },
		func(g *GlobalConfig, p *Profile) { p.GitHub.CredentialID = "" },
		func(g *GlobalConfig, p *Profile) { p.Scheduler.Execution.InventoryDigest = "other" },
		func(g *GlobalConfig, p *Profile) { p.Scheduler.Routes = nil },
		func(g *GlobalConfig, p *Profile) { p.Scheduler.Budget = ResourceBudget{} },
	} {
		g, p := hostFixture()
		mutate(&g, &p)
		if ValidateHostConfig(g, []Profile{p}) == nil {
			t.Fatal("invalid enabled configuration accepted")
		}
	}
	g, p := hostFixture()
	p.Scheduler = ProfileSchedulerConfig{}
	if err := ValidateHostConfig(g, []Profile{p}); err != nil {
		t.Fatalf("dormant rejected: %v", err)
	}
	p.Scheduler.Enabled = true
	if ValidateHostConfig(g, []Profile{p}) == nil {
		t.Fatal("unproven enabled accepted")
	}
}

func writeHostProfile(t *testing.T, dir string, p Profile) Profile {
	t.Helper()
	p.Source = filepath.Join(dir, p.Name+".yaml")
	data, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.Source, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Darwin's /var is a symlink; fixtures use real canonical paths, not a relaxed gate.
func hostTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParticipationPreservesReferences(t *testing.T) {
	for _, org := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "org"}[org], func(t *testing.T) {
			_, p := hostFixture()
			if org {
				p.Target = TargetConfig{Scope: TargetScopeOrganization, Org: "team"}
				p.Repo = RepoConfig{}
				p.Runner.Environment = "base"
				p.RunnerGroup.Name = "team-base"
				p.GitHub.CredentialID = "org"
				p.GitHub.ResourceOwner = "team"
			}
			dir := hostTempDir(t)
			p = writeHostProfile(t, dir, p)
			p, err := LoadProfile(p.Source)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := FreezeParticipation(dir, p)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := SaveParticipationAt(dir, p.Name, binding, false); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LoadProfile(p.Source)
			if err != nil {
				t.Fatal(err)
			}
			p.Scheduler.Enabled = false
			if !reflect.DeepEqual(got, p) {
				t.Fatalf("changed nonparticipation fields: %+v", got)
			}
		})
	}
}

func TestParticipationFrozenSource(t *testing.T) {
	for _, kind := range []string{"changed", "moved", "symlink", "wrong-root", "unknown-field"} {
		t.Run(kind, func(t *testing.T) {
			_, p := hostFixture()
			dir := hostTempDir(t)
			p = writeHostProfile(t, dir, p)
			binding, err := FreezeParticipation(dir, p)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "changed":
				p.Docker.Image = "other@sha256:" + strings.Repeat("b", 64)
				writeHostProfile(t, dir, p)
			case "moved":
				if err := os.Rename(p.Source, filepath.Join(dir, "renamed.yaml")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := writeHostProfile(t, hostTempDir(t), p)
				if err := os.Remove(p.Source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other.Source, p.Source); err != nil {
					t.Fatal(err)
				}
			case "wrong-root":
				dir = hostTempDir(t)
			case "unknown-field":
				f, err := os.OpenFile(p.Source, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("unmodeled: test-secret\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if SaveParticipationAt(dir, p.Name, binding, false) == nil {
				t.Fatal("changed binding accepted")
			}
		})
	}
	_, p := hostFixture()
	dir := hostTempDir(t)
	p = writeHostProfile(t, hostTempDir(t), p)
	if _, err := FreezeParticipation(dir, p); err == nil {
		t.Fatal("source parent treated as authority")
	}
}

func TestLoadCurrentTrustAndIdentity(t *testing.T) {
	for _, kind := range []string{"valid", "yaml", "ref", "empty-items", "symlink-root", "mode", "duplicate", "overlap", "rebound"} {
		t.Run(kind, func(t *testing.T) {
			dir := hostTempDir(t)
			profiles := filepath.Join(dir, "profiles")
			if err := os.Mkdir(profiles, 0700); err != nil {
				t.Fatal(err)
			}
			g, p := hostFixture()
			g.Paths.ProfilesDir = profiles
			p = writeHostProfile(t, profiles, p)
			path := filepath.Join(dir, "global.yaml")
			switch kind {
			case "empty-items":
				for _, name := range []string{"bad-1.yaml", "bad-2.yaml"} {
					if err := os.WriteFile(filepath.Join(profiles, name), []byte("{}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "yaml":
				if err := os.WriteFile(filepath.Join(profiles, "bad.yaml"), []byte("name: ["), 0600); err != nil {
					t.Fatal(err)
				}
			case "ref":
				q := p
				q.Name = "bad"
				q.Service.Name = "gha-bad.service"
				q.Runner.NamePrefix = "bad"
				q.Docker.ContainerNamePrefix = "gha-bad"
				q.GitHub.CredentialID = ""
				writeHostProfile(t, profiles, q)
			case "duplicate":
				data, err := os.ReadFile(p.Source)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(profiles, "duplicate.yaml"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "overlap":
				q := p
				q.Name = "swift"
				q.Service.Name = "gha-swift.service"
				q.Runner.NamePrefix = "swift"
				q.Docker.ContainerNamePrefix = "gha-base-swift"
				writeHostProfile(t, profiles, q)
			case "rebound":
				g.Paths.ProfilesDir = hostTempDir(t)
			}
			data, err := yaml.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink-root" {
				real := profiles + "-real"
				if err := os.Rename(profiles, real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, profiles); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "mode" {
				if err := os.Chmod(profiles, 0777); err != nil {
					t.Fatal(err)
				}
			}
			check := func(path string) error { return checkPathTrust(path, uint32(os.Getuid())) }
			got, err := loadCurrentWithTrust(path, profiles, check)
			switch kind {
			case "valid", "yaml", "ref", "empty-items":
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Profiles) != 1 {
					t.Fatalf("profiles=%d", len(got.Profiles))
				}
				want := 0
				if kind != "valid" {
					want = 1
				}
				if kind == "empty-items" {
					want = 2
				}
				if len(got.Errors) != want {
					t.Fatalf("errors=%v", got.Errors)
				}
				if _, err := got.Find("base"); err != nil {
					t.Fatal(err)
				}
			default:
				if err == nil {
					t.Fatal("broken collection accepted")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, after) {
				t.Fatal("read mutated config")
			}
		})
	}
}

func TestRuntimeSnapshotWhitelist(t *testing.T) {
	_, p := hostFixture()
	dir := hostTempDir(t)
	p = writeHostProfile(t, dir, p)
	ref := CredentialRef{ID: "personal", ResourceOwner: "alice", TokenEnv: "PERSONAL_TOKEN", Repositories: []RepoRef{{Owner: "alice", Name: "project"}}}
	labels := []string{"self-hosted", "linux", "arm64", "custom"}
	snap, err := FreezeRuntime(dir, p, ref, "image-evidence-1", labels)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"target":{"scope":"repository","owner":"alice","repo":"project"`)) {
		t.Fatal("runtime target lacks snake_case wire fields")
	}
	p.Docker.Image = "changed"
	p.GitHub.CredentialID = "changed"
	ref.Repositories[0].Name = "changed"
	labels[0] = "changed"
	p.Scheduler.Execution.MountRefs[0] = "changed"
	p.Scheduler.Execution.EvidenceIDs[0] = "changed"
	p.Scheduler.Repositories[0].Name = "changed"
	p.Scheduler.Routes[0].Labels[0] = "changed"
	after, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, after) {
		t.Fatal("snapshot aliases inputs")
	}
	if snap.Bootstrap.Labels[0] != "self-hosted" || snap.Bootstrap.TargetURL != "https://github.com/alice/project" || !snap.Bootstrap.Ephemeral {
		t.Fatalf("bootstrap incomplete: %+v", snap.Bootstrap)
	}
	_, p = hostFixture()
	p.Docker.Env = map[string]string{"GITHUB_TOKEN": "test-secret"}
	p = writeHostProfile(t, dir, p)
	if _, err := FreezeRuntime(dir, p, ref, "evidence", labels); err == nil {
		t.Fatal("raw environment admitted")
	}
	if bytes.Contains(data, []byte("test-secret")) || bytes.Contains(data, []byte(`"docker":`)) {
		t.Fatal("non-whitelisted profile persisted")
	}
}

func TestHostGlobalReadOnlyDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "global.yaml")
	for _, content := range []string{"github: {}\n", "scheduler:\n  mode: host-single\n"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		g, err := LoadGlobalConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		want := "disabled"
		if strings.Contains(content, "host-single") {
			want = "host-single"
		}
		if g.Scheduler.Mode != want {
			t.Fatalf("mode=%s", g.Scheduler.Mode)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != content || !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("read wrote configuration")
		}
	}
}

func TestParticipationNeverHashesRawEnvironment(t *testing.T) {
	_, p := hostFixture()
	p.Scheduler.Enabled = false
	p.Docker.Env = map[string]string{"TOKEN": "test-secret"}
	dir := hostTempDir(t)
	p = writeHostProfile(t, dir, p)
	if _, err := FreezeParticipation(dir, p); err == nil {
		t.Fatal("raw environment secret was hashed")
	}
}

func TestHostRejectsInheritedCredentialMount(t *testing.T) {
	g, p := hostFixture()
	g.GitHub.EnvFile = "/opt/private/auth.env"
	p.GitHub.TokenEnv = ""
	p.Scheduler.Execution.MountRefs = []string{"/opt/private:/files:ro"}
	if ValidateHostConfig(g, []Profile{p}) == nil {
		t.Fatal("inherited credential mounted into job")
	}
}

func TestHostCredentialOwnerBoundToTarget(t *testing.T) {
	g, p := hostFixture()
	p.GitHub.ResourceOwner = "other-org"
	if ValidateHostConfig(g, []Profile{p}) == nil {
		t.Fatal("credential owner does not match registration target")
	}
}

func TestLoadCurrentReloadsWithoutCache(t *testing.T) {
	dir := hostTempDir(t)
	g, p := hostFixture()
	g.Paths.ProfilesDir = dir
	p = writeHostProfile(t, dir, p)
	path := filepath.Join(dir, "config.conf")
	data, err := yaml.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	check := func(path string) error { return checkPathTrust(path, uint32(os.Getuid())) }
	first, err := loadCurrentWithTrust(path, dir, check)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Profiles) != 1 {
		t.Fatal(first.Errors)
	}
	p.Scheduler.Enabled = false
	writeHostProfile(t, dir, p)
	current, err := loadCurrentWithTrust(path, dir, check)
	if err != nil {
		t.Fatal(err)
	}
	got, err := current.Find("base")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scheduler.Enabled {
		t.Fatal("returned cached enabled profile")
	}
	if err := os.Remove(p.Source); err != nil {
		t.Fatal(err)
	}
	current, err = loadCurrentWithTrust(path, dir, check)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Find("base"); err == nil {
		t.Fatal("deleted profile came from cache")
	}
}

func TestHostDefaultsDisabled(t *testing.T) {
	g, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if g.Scheduler.Mode != "disabled" || g.Scheduler.PollIntervalSeconds != 20 || g.Scheduler.IdleRunnerTTLSeconds != 180 || (Profile{}).Scheduler.Enabled {
		t.Fatalf("unsafe defaults: %+v", g.Scheduler)
	}
}

func TestHostRejectsBoundMounts(t *testing.T) {
	for _, kind := range []string{"outer", "outer-parent", "inventory", "profiles", "source", "state", "log"} {
		t.Run(kind, func(t *testing.T) {
			g, p := hostFixture()
			g.Scheduler.OuterDockerEndpoint = "unix:///opt/docker/control"
			g.Scheduler.Inventory.Daemons[0].Endpoint = g.Scheduler.OuterDockerEndpoint
			p.Scheduler.Execution.Outer.Endpoint = g.Scheduler.OuterDockerEndpoint
			g.Scheduler.Inventory.Daemons = append(g.Scheduler.Inventory.Daemons, DaemonRef{Endpoint: "unix:///opt/other/control", UID: 0})
			g.Paths.ProfilesDir = "/opt/gha/profiles"
			g.Paths.StateDir = "/opt/gha/state"
			g.Paths.LogDir = "/opt/gha/log"
			p.Source = "/opt/custom/profiles/base.yaml"
			path := map[string]string{"outer": "/opt/docker/control", "outer-parent": "/opt/docker", "inventory": "/opt/other/control", "profiles": g.Paths.ProfilesDir, "source": filepath.Dir(p.Source), "state": g.Paths.StateDir, "log": g.Paths.LogDir}[kind]
			p.Scheduler.Execution.MountRefs = []string{path + ":/host:rw"}
			if ValidateHostConfig(g, []Profile{p}) == nil {
				t.Fatal("bound socket/control mount accepted")
			}
		})
	}
	for _, kind := range []string{"outer", "profiles"} {
		t.Run("freeze-"+kind, func(t *testing.T) {
			g, p := hostFixture()
			dir := hostTempDir(t)
			p.Scheduler.Execution.Outer.Endpoint = "unix:///opt/docker/control"
			path := "/opt/docker/control"
			if kind == "profiles" {
				path = dir
			}
			p.Scheduler.Execution.MountRefs = []string{path + ":/host:rw"}
			p = writeHostProfile(t, dir, p)
			if _, err := FreezeRuntime(dir, p, profileCredential(g, p), "image-proof", []string{"custom"}); err == nil {
				t.Fatal("forbidden mount frozen")
			}
		})
	}
}

func TestCredentialReferenceSources(t *testing.T) {
	for _, sources := range []GitHubProfile{
		{EnvFile: "/opt/private/personal.env"},
		{TokenFile: "/opt/private/personal.token"},
		{TokenEnv: "PERSONAL_TOKEN", TokenFile: "/opt/private/personal.token"},
		{TokenFile: "/opt/private/personal.token", EnvFile: "/opt/private/personal.env"},
		{TokenEnv: "PERSONAL_TOKEN", TokenFile: "/opt/private/personal.token", EnvFile: "/opt/private/personal.env"},
	} {
		t.Run(sources.TokenEnv+sources.TokenFile+sources.EnvFile, func(t *testing.T) {
			g, p := hostFixture()
			sources.CredentialID, sources.ResourceOwner = "personal", "alice"
			p.GitHub = sources
			ref := profileCredential(g, p)
			if ref.TokenEnv != sources.TokenEnv || ref.TokenFile != sources.TokenFile || ref.EnvFile != sources.EnvFile {
				t.Errorf("explicit references changed: %+v", ref)
			}
			if err := ValidateHostConfig(g, []Profile{p}); err != nil {
				t.Errorf("host: %v", err)
			}
			dir := hostTempDir(t)
			g.Paths.ProfilesDir = dir
			p = writeHostProfile(t, dir, p)
			path := filepath.Join(dir, "config.conf")
			data, err := yaml.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			current, err := loadCurrentWithTrust(path, dir, func(path string) error { return checkPathTrust(path, uint32(os.Getuid())) })
			if err != nil || len(current.Profiles) != 1 {
				t.Errorf("current: %v %v", err, current.Errors)
			}
			ref = CredentialRef{ID: "personal", ResourceOwner: "alice", TokenEnv: sources.TokenEnv, TokenFile: sources.TokenFile, EnvFile: sources.EnvFile, Repositories: p.Scheduler.Repositories}
			if _, err := FreezeRuntime(dir, p, ref, "image-proof", []string{"custom"}); err != nil {
				t.Errorf("freeze: %v", err)
			}
			if sources.TokenEnv == "" {
				ref.TokenEnv = g.GitHub.TokenEnv
				if _, err := FreezeRuntime(dir, p, ref, "image-proof", []string{"custom"}); err == nil {
					t.Error("global env injected into explicit file source")
				}
			}
		})
	}
}

func TestHostProfileSpecialFileNoStall(t *testing.T) {
	if os.Getenv("GHA_CONFIG_FIFO_TEST") == "1" {
		dir := hostTempDir(t)
		path := filepath.Join(dir, "bad.yaml")
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readHostProfile(path); err == nil {
			t.Fatal("FIFO accepted")
		}
		g, p := hostFixture()
		g.Paths.ProfilesDir = dir
		writeHostProfile(t, dir, p)
		data, err := yaml.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(dir, "config.conf")
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := loadCurrentWithTrust(configPath, dir, func(path string) error { return checkPathTrust(path, uint32(os.Getuid())) })
		if err != nil || len(got.Profiles) != 1 || len(got.Errors) != 1 {
			t.Fatalf("FIFO not isolated: %v %+v", err, got)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostProfileSpecialFileNoStall$", "-test.v")
	cmd.Env = append(os.Environ(), "GHA_CONFIG_FIFO_TEST=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("host reader stalled on FIFO: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, output)
	}
}

func TestLoadCurrentTrustBeforeParse(t *testing.T) {
	dir := hostTempDir(t)
	g, p := hostFixture()
	g.Paths.ProfilesDir = dir
	writeHostProfile(t, dir, p)
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("name: ["), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0666); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.conf")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadCurrentWithTrust(path, dir, func(path string) error { return checkPathTrust(path, uint32(os.Getuid())) })
	if err != nil || len(got.Profiles) != 1 || len(got.Errors) != 1 {
		t.Fatalf("invalid item not isolated: %v %+v", err, got)
	}
	if !strings.Contains(got.Errors[0].Err.Error(), "owner/mode") {
		t.Fatalf("parsed before trust rejection: %v", got.Errors[0])
	}
}

func TestParticipationYAMLShapes(t *testing.T) {
	for _, kind := range []string{"null", "alias", "shared-anchor", "inherited"} {
		t.Run(kind, func(t *testing.T) {
			_, p := hostFixture()
			dir := hostTempDir(t)
			p = writeHostProfile(t, dir, p)
			data, err := os.ReadFile(p.Source)
			if err != nil {
				t.Fatal(err)
			}
			var doc yaml.Node
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			root := doc.Content[0]
			scheduler := mappingValue(root, "scheduler")
			switch kind {
			case "null":
				*scheduler = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
			case "alias", "inherited":
				for i, node := range root.Content {
					if node == scheduler {
						root.Content = append(root.Content[:i-1], root.Content[i+1:]...)
						break
					}
				}
				scheduler.Anchor = "participation"
				merged := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "scheduler"}, scheduler}}
				root.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, merged}, root.Content...)
				if kind == "alias" {
					root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "scheduler"}, &yaml.Node{Kind: yaml.AliasNode, Value: "participation", Alias: scheduler})
				}
			case "shared-anchor":
				field := mappingValue(scheduler, "enabled")
				field.Anchor = "participates"
				*mappingValue(mappingValue(root, "runner"), "ephemeral") = yaml.Node{Kind: yaml.AliasNode, Value: "participates", Alias: field}
			}
			before, err := yaml.Marshal(&doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.Source, before, 0600); err != nil {
				t.Fatal(err)
			}
			p, err = readHostProfile(p.Source)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := FreezeParticipation(dir, p)
			if err != nil {
				t.Fatal(err)
			}
			target := !p.Scheduler.Enabled
			err = SaveParticipationAt(dir, p.Name, binding, target)
			after, readErr := os.ReadFile(p.Source)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err != nil {
				if !bytes.Equal(before, after) {
					t.Fatal("failed write modified source")
				}
				return
			}
			var got Profile
			if err := strictYAML(after, &got); err != nil {
				t.Fatalf("published invalid YAML: %v", err)
			}
			if got.Scheduler.Enabled != target {
				t.Fatal("successful write did not assign requested enabled")
			}
			digest, err := profileDigest(got)
			if err != nil || digest != binding.ConfigDigest {
				t.Fatalf("write changed non-enabled fields: %v", err)
			}
		})
	}
}
