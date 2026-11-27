package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	systemdpkg "gha-runner-tui/internal/systemd"
)

func newSystemdClient(r command.Runner) systemdpkg.Client { return systemdpkg.NewClient(r) }
func newDockerClient(r command.Runner) dockerpkg.Client   { return dockerpkg.NewClient(r) }
func newGitHubClient() gh.Client                          { return gh.Client{} }
func parseUnitShowForTest(text string) (systemdpkg.UnitInfo, error) {
	return systemdpkg.ParseUnitShow(text)
}

type fleetUnit struct {
	loadState string
	active    string
	fragment  string
	config    string
	dropIns   string
	noExec    bool
}

type fleetRunner struct {
	units          map[string]fleetUnit
	calls          []string
	shows          []string
	failCall       map[string]error
	showErr        map[int]error
	showOutput     map[int][]byte
	activeOverride map[int]string
	onShow         func(int)
	onReload       func()
	showCount      int
}

func (r *fleetRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name
	for _, arg := range args {
		call += " " + arg
	}
	if name == "systemctl" && len(args) > 1 && args[0] == "show" {
		service := args[len(args)-1]
		r.shows = append(r.shows, call)
		r.showCount++
		if r.onShow != nil {
			r.onShow(r.showCount)
		}
		if err := r.showErr[r.showCount]; err != nil {
			return r.showOutput[r.showCount], err
		}
		unit, ok := r.units[service]
		if !ok {
			unit = fleetUnit{loadState: "not-found"}
		}
		if unit.loadState == "" {
			unit.loadState = "loaded"
		}
		if unit.active == "" {
			unit.active = "active"
		}
		if override, ok := r.activeOverride[r.showCount]; ok {
			unit.active = override
		}
		var out strings.Builder
		fmt.Fprintf(&out, "LoadState=%s\nActiveState=%s\n", unit.loadState, unit.active)
		if unit.loadState == "loaded" {
			fmt.Fprintf(&out, "FragmentPath=%s\n", unit.fragment)
			if !unit.noExec {
				fmt.Fprintf(&out, "ExecStart={ path=/bin/loop ; argv[]=/bin/loop --config %s ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n", unit.config)
			}
			fmt.Fprintf(&out, "DropInPaths=%s\n", unit.dropIns)
		}
		return []byte(out.String()), nil
	}
	r.calls = append(r.calls, call)
	if err := r.failCall[call]; err != nil {
		return []byte("failed"), err
	}
	if name == "systemctl" && len(args) > 0 && args[0] == "daemon-reload" {
		for service, unit := range r.units {
			if unit.fragment != "" && !fileExists(unit.fragment) {
				delete(r.units, service)
			}
		}
		if r.onReload != nil {
			r.onReload()
		}
	}
	return []byte("ok\n"), nil
}

func (r *fleetRunner) writeCalls() []string { return append([]string{}, r.calls...) }

type fleetHarness struct {
	t        *testing.T
	root     string
	cfg      config.GlobalConfig
	cfgPath  string
	manager  RunnerManager
	systemd  *fleetRunner
	profiles string
	units    string
	state    string
}

func newFleetHarness(t *testing.T) *fleetHarness {
	t.Helper()
	root := t.TempDir()
	h := &fleetHarness{t: t, root: root}
	h.profiles = filepath.Join(root, "profiles")
	h.units = filepath.Join(root, "units")
	h.state = filepath.Join(root, "state")
	for _, dir := range []string{h.profiles, h.units, h.state, filepath.Join(root, "logs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.cfgPath = filepath.Join(root, "config.yaml")
	cfgYAML := fmt.Sprintf("paths:\n  profiles_dir: %s\n  state_dir: %s\n  log_dir: %s\nsystemd:\n  loop_binary_path: /bin/loop\ndocker:\n  default_access_mode: host-socket\n", h.profiles, h.state, filepath.Join(root, "logs"))
	if err := os.WriteFile(h.cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	h.cfg, err = config.LoadGlobalConfig(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	h.systemd = &fleetRunner{units: map[string]fleetUnit{}, failCall: map[string]error{}}
	h.manager = NewRunnerManager(h.cfgPath, newSystemdClient(h.systemd), newDockerClient(h.systemd), newGitHubClient())
	h.manager.Runner = h.systemd
	h.manager.SystemdUnitDir = h.units
	h.manager.LockPath = filepath.Join(root, "apply.lock")
	return h
}

func (h *fleetHarness) profileRaw(name string) string {
	return fmt.Sprintf(`name: %[1]s
repo: {owner: me, name: app}
github: {token_env: TEST_TOKEN, env_file: /fake/env}
service: {name: gha-%[1]s.service}
runner: {name_prefix: %[1]s, labels: [linux]}
docker: {image: image:test, container_name_prefix: gha-%[1]s, cpus: "1", memory: 1g}
loop: {state_file: %[2]s, log_dir: %[3]s}
managed_by: fleet
`, name, filepath.Join(h.state, name+".json"), filepath.Join(h.root, "logs", name))
}

func (h *fleetHarness) writeProfile(name, raw string) string {
	h.t.Helper()
	path := filepath.Join(h.profiles, name+".yaml")
	if err := os.WriteFile(path, []byte(raw), 0o640); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *fleetHarness) writeUnit(name string, unit fleetUnit) {
	h.t.Helper()
	service := "gha-" + name + ".service"
	if unit.fragment == "" {
		unit.fragment = filepath.Join(h.units, service)
	}
	if unit.config == "" {
		unit.config = filepath.Join(h.profiles, name+".yaml")
	}
	if unit.loadState == "loaded" {
		raw := []byte("[Service]\nExecStart=/bin/loop --config " + unit.config + "\n")
		if err := os.WriteFile(unit.fragment, raw, 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.systemd.units[service] = unit
}

func (h *fleetHarness) removeUnit(name string) {
	service := "gha-" + name + ".service"
	delete(h.systemd.units, service)
}

func (h *fleetHarness) bundle(names ...string) []byte {
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString(h.profileRaw(name))
	}
	return []byte(b.String())
}

func fleetNames(results []FleetResult) map[string]FleetResult {
	m := map[string]FleetResult{}
	for _, r := range results {
		m[r.Name] = r
	}
	return m
}

func TestExportCompleteAndIncomplete(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p1", h.profileRaw("p1"))
	h.writeProfile("p2", strings.Replace(h.profileRaw("p2"), "managed_by: fleet", "managed_by: local", 1))
	if err := os.WriteFile(filepath.Join(h.profiles, "bad.yaml"), []byte("name: [bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := h.manager.Export(context.Background(), ExportInput{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || len(report.Profiles) != 2 || len(report.Errors) != 1 || !strings.HasSuffix(report.Errors[0].Path, "bad.yaml") {
		t.Fatalf("export=%+v", report)
	}
	fleet, err := h.manager.Export(context.Background(), ExportInput{Owner: config.ManagedByFleet})
	if err != nil || fleet.Complete || len(fleet.Profiles) != 1 || fleet.Profiles[0].Name != "p1" || fleet.Profiles[0].Owner != "fleet" || !strings.Contains(fleet.Profiles[0].YAML, "managed_by: fleet") || len(fleet.Errors) != 1 {
		t.Fatalf("fleet export=%+v err=%v", fleet, err)
	}
	byName, err := h.manager.Export(context.Background(), ExportInput{Names: []string{"p2"}})
	if err != nil || byName.Complete || len(byName.Profiles) != 1 || byName.Profiles[0].Owner != "local" {
		t.Fatalf("named export=%+v err=%v", byName, err)
	}
	for _, bad := range []ExportInput{{Names: []string{"../x"}}, {Owner: "other"}} {
		if _, err := h.manager.Export(context.Background(), bad); !errors.Is(err, ErrFleetInput) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

func TestApplyPlanMixedAndGoldenPlanShape(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p-update-restart", strings.Replace(h.profileRaw("p-update-restart"), "image:test", "image:old", 1))
	h.writeProfile("p-format", "# comment\n"+h.profileRaw("p-format"))
	h.writeProfile("p-unchanged", h.profileRaw("p-unchanged"))
	h.writeProfile("p-restart", h.profileRaw("p-restart"))
	if err := os.MkdirAll(filepath.Join(filepath.Dir(h.state), "restart-pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.state), "restart-pending", "p-restart"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"p-update-restart", "p-format", "p-unchanged", "p-restart"} {
		h.writeUnit(name, fleetUnit{loadState: "loaded"})
	}
	h.writeUnit("p-create", fleetUnit{loadState: "not-found"})
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p-update-restart", "p-format", "p-unchanged", "p-restart", "p-create"), Owner: config.ManagedByFleet, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Phase != FleetPhasePlan || report.PlanID == "" || report.Complete {
		t.Fatalf("plan=%+v", report)
	}
	got := fleetNames(report.Results)
	want := map[string]struct {
		action  string
		restart bool
	}{
		"p-update-restart": {FleetActionUpdate, true},
		"p-format":         {FleetActionUpdate, false},
		"p-unchanged":      {FleetActionUnchanged, false},
		"p-restart":        {FleetActionRestart, true},
		"p-create":         {FleetActionCreate, false},
	}
	for name, w := range want {
		r, ok := got[name]
		if !ok || r.Action != w.action || r.RestartNeeded != w.restart || r.Outcome != FleetOutcomePlanned {
			t.Fatalf("%s: %+v (all %+v)", name, r, report.Results)
		}
	}
	if len(h.systemd.calls) != 0 {
		t.Fatalf("dry-run wrote: %v", h.systemd.calls)
	}
	// The same inputs and disk state produce the same plan_id.
	again, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p-update-restart", "p-format", "p-unchanged", "p-restart", "p-create"), Owner: config.ManagedByFleet, DryRun: true})
	if err != nil || again.PlanID != report.PlanID {
		t.Fatalf("repeat plan: %v %s vs %s", err, again.PlanID, report.PlanID)
	}
}

func TestApplyExecuteOkAndSecondRunUnchanged(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p-update", strings.Replace(h.profileRaw("p-update"), "image:test", "image:old", 1))
	if err := os.Chmod(filepath.Join(h.profiles, "p-update.yaml"), 0o660); err != nil {
		t.Fatal(err)
	}
	h.writeUnit("p-update", fleetUnit{loadState: "loaded"})
	h.writeUnit("p-create", fleetUnit{loadState: "not-found"})
	bundle := h.bundle("p-update", "p-create")
	plan, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: bundle, Owner: config.ManagedByFleet, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: bundle, Owner: config.ManagedByFleet, ExpectPlan: plan.PlanID})
	if err != nil || !report.Complete || report.Phase != FleetPhaseExecute {
		t.Fatalf("execute=%+v err=%v", report, err)
	}
	for _, r := range report.Results {
		if r.Outcome != FleetOutcomeDone {
			t.Fatalf("%+v", report.Results)
		}
	}
	if !reflect.DeepEqual(h.systemd.calls, []string{
		"systemctl daemon-reload", "systemctl enable gha-p-create.service", "systemctl start gha-p-create.service",
		"systemctl restart --no-block gha-p-update.service",
	}) {
		t.Fatalf("calls=%v", h.systemd.calls)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "restart-pending", "p-update")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker remains: %v", err)
	}
	if info, err := os.Stat(filepath.Join(h.profiles, "p-update.yaml")); err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("update did not preserve mode: %v %v", info, err)
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(h.state), "profile-backups", "p-update-*.yaml"))
	if len(backups) != 1 {
		t.Fatalf("backups=%v", backups)
	}
	h.systemd.calls = nil
	h.systemd.units["gha-p-create.service"] = fleetUnit{loadState: "loaded", fragment: filepath.Join(h.units, "gha-p-create.service"), config: filepath.Join(h.profiles, "p-create.yaml")}
	second, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: bundle, Owner: config.ManagedByFleet})
	if err != nil || !second.Complete {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	for _, r := range second.Results {
		if r.Action != FleetActionUnchanged {
			t.Fatalf("second action: %+v", second.Results)
		}
	}
	if len(h.systemd.calls) != 0 {
		t.Fatalf("second wrote: %v", h.systemd.calls)
	}
}

func TestApplyRejectsConflictStalePlanAndBadInputWithoutWrites(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p-local", strings.Replace(h.profileRaw("p-local"), "managed_by: fleet", "managed_by: local", 1))
	h.writeProfile("p-reads-elsewhere", h.profileRaw("p-reads-elsewhere"))
	h.writeUnit("p-local", fleetUnit{loadState: "loaded"})
	h.writeUnit("p-reads-elsewhere", fleetUnit{loadState: "loaded", config: "/etc/other.yaml"})
	before := snapshotTree(t, h.profiles)
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p-local", "p-reads-elsewhere"), Owner: config.ManagedByFleet, DryRun: true})
	if !errors.Is(err, ErrFleetRejected) || report.Complete || report.PlanID == "" {
		t.Fatalf("conflict=%+v err=%v", report, err)
	}
	if got := fleetNames(report.Results); got["p-local"].Message == "" || got["p-reads-elsewhere"].Message == "" {
		t.Fatalf("missing conflict messages: %+v", report.Results)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, h.profiles)) || len(h.systemd.calls) != 0 {
		t.Fatalf("conflict wrote files/calls: %v", h.systemd.calls)
	}
	h.writeProfile("p-ok", h.profileRaw("p-ok"))
	h.writeUnit("p-ok", fleetUnit{loadState: "loaded"})
	plan, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p-ok"), Owner: config.ManagedByFleet, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	report, err = h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p-ok"), Owner: config.ManagedByFleet, ExpectPlan: "sha256:stale"})
	if !errors.Is(err, ErrFleetRejected) || len(h.systemd.calls) != 0 {
		t.Fatalf("stale=%+v err=%v calls=%v", report, err, h.systemd.calls)
	}
	if plan.PlanID == "" || report.PlanID == "" || plan.PlanID == "sha256:stale" {
		t.Fatalf("plan ids: %s %s", plan.PlanID, report.PlanID)
	}
	for _, input := range []ApplyInput{
		{Bundle: []byte("name: p\n"), Owner: "fleet"},
		{Bundle: []byte("---\nname: a\n---\nname: a\n"), Owner: "fleet"},
		{Bundle: h.bundle("p-ok"), Owner: "fleet", Prune: true, DryRun: true},
	} {
		if _, err := h.manager.Apply(context.Background(), input); err == nil {
			t.Fatalf("accepted bad input %+v", input)
		}
	}
}

func TestApplyRestartMarkerFailureAndResume(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p", strings.Replace(h.profileRaw("p"), "image:test", "image:old", 1))
	h.writeProfile("q", h.profileRaw("q"))
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "active"})
	h.writeUnit("q", fleetUnit{loadState: "loaded", active: "active"})
	h.systemd.failCall["systemctl restart --no-block gha-p.service"] = errors.New("restart failed")
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p", "q"), Owner: config.ManagedByFleet})
	if !errors.Is(err, ErrFleetPartial) || report.Phase != FleetPhaseExecute || report.Complete {
		t.Fatalf("restart fail=%+v err=%v", report, err)
	}
	gotResults := fleetNames(report.Results)
	if gotResults["p"].Restart != FleetRestartFailed || gotResults["p"].Outcome != FleetOutcomeFailed || gotResults["q"].Outcome != FleetOutcomeNotRun {
		t.Fatalf("result=%+v", report.Results)
	}
	marker := filepath.Join(filepath.Dir(h.state), "restart-pending", "p")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	h.systemd.failCall = map[string]error{}
	plan, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p", "q"), Owner: config.ManagedByFleet, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if gotPlan := fleetNames(plan.Results); gotPlan["p"].Action != FleetActionRestart || gotPlan["q"].Action != FleetActionUnchanged {
		t.Fatalf("plan after fail=%+v", plan.Results)
	}
	done, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p", "q"), Owner: config.ManagedByFleet})
	if err != nil || !done.Complete || fleetNames(done.Results)["p"].Restart != FleetRestartQueued {
		t.Fatalf("resume=%+v err=%v", done, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker after resume: %v", err)
	}
}

func TestApplyRestartSkipsInactiveAndInterruptedWriteIsResumed(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p", strings.Replace(h.profileRaw("p"), "image:test", "image:old", 1))
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "inactive"})
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet})
	if err != nil || report.Results[0].Restart != FleetRestartSkipped {
		t.Fatalf("inactive=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "restart-pending", "p")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inactive marker: %v", err)
	}
	// Simulate a crash after the durable intent/restart marker but before the
	// profile write, then rerun the same apply.
	h.writeProfile("q", strings.Replace(h.profileRaw("q"), "image:test", "image:old", 1))
	h.writeUnit("q", fleetUnit{loadState: "loaded", active: "active"})
	h.systemd.failCall["systemctl restart --no-block gha-q.service"] = errors.New("connection lost")
	if _, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("q"), Owner: config.ManagedByFleet}); !errors.Is(err, ErrFleetPartial) {
		t.Fatal("expected partial")
	}
	h.systemd.failCall = map[string]error{}
	done, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("q"), Owner: config.ManagedByFleet})
	if err != nil || !done.Complete || done.Results[0].Action != FleetActionRestart {
		t.Fatalf("resume q=%+v err=%v", done, err)
	}
}

func TestAdoptOnlyRewritesManagedByLineAndNeverCallsSystemctlWrites(t *testing.T) {
	h := newFleetHarness(t)
	withLine := strings.Replace(h.profileRaw("p1"), "managed_by: fleet", "managed_by: local", 1)
	h.writeProfile("p1", withLine)
	noLine := strings.Replace(h.profileRaw("p2"), "managed_by: fleet\n", "", 1)
	h.writeProfile("p2", noLine)
	h.writeProfile("p3", strings.Replace(h.profileRaw("p3"), "managed_by: fleet", "managed_by: local", 1))
	h.writeUnit("p1", fleetUnit{loadState: "loaded"})
	h.writeUnit("p2", fleetUnit{loadState: "loaded"})
	h.writeUnit("p3", fleetUnit{loadState: "not-found"})
	plan, err := h.manager.Adopt(context.Background(), AdoptInput{Match: "p*", Owner: config.ManagedByFleet, DryRun: true})
	if err != nil || plan.PlanID == "" || len(plan.Results) != 3 {
		t.Fatalf("adopt plan=%+v err=%v", plan, err)
	}
	if len(h.systemd.calls) != 0 || len(h.systemd.shows) != 3 {
		t.Fatalf("dry-run calls=%v shows=%v", h.systemd.calls, h.systemd.shows)
	}
	report, err := h.manager.Adopt(context.Background(), AdoptInput{Match: "p*", Owner: config.ManagedByFleet, ExpectPlan: plan.PlanID})
	if err != nil || !report.Complete {
		t.Fatalf("adopt=%+v err=%v", report, err)
	}
	if len(h.systemd.calls) != 0 {
		t.Fatalf("adopt wrote systemd: %v", h.systemd.calls)
	}
	raw1, _ := os.ReadFile(filepath.Join(h.profiles, "p1.yaml"))
	raw2, _ := os.ReadFile(filepath.Join(h.profiles, "p2.yaml"))
	if strings.Count(string(raw1), "managed_by:") != 1 || strings.Contains(string(raw1), "managed_by: local") || !strings.Contains(string(raw1), "managed_by: fleet") {
		t.Fatalf("p1=%s", raw1)
	}
	if !strings.HasSuffix(string(raw2), "managed_by: fleet\n") || strings.Count(string(raw2), "managed_by:") != 1 {
		t.Fatalf("p2=%s", raw2)
	}
	if err := os.WriteFile(filepath.Join(h.profiles, "p3.yaml"), []byte(strings.Replace(h.profileRaw("p3"), "managed_by: fleet", `"managed_by": fleet`, 1)), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Adopt(context.Background(), AdoptInput{Names: []string{"p3"}, Owner: config.ManagedByLocal}); !errors.Is(err, ErrFleetRejected) {
		t.Fatalf("quoted key accepted: %v", err)
	}
}

func TestAdoptRejectsWholeBatchWhenOneProfileIsUnsafe(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("good", strings.Replace(h.profileRaw("good"), "managed_by: fleet", "managed_by: local", 1))
	h.writeProfile("bad", strings.Replace(h.profileRaw("bad"), "managed_by: fleet", "managed_by: local", 1))
	h.writeUnit("good", fleetUnit{loadState: "loaded"})
	h.writeUnit("bad", fleetUnit{loadState: "loaded", config: "/etc/other.yaml"})
	before := snapshotTree(t, h.profiles)
	if _, err := h.manager.Adopt(context.Background(), AdoptInput{Names: []string{"good", "bad"}, Owner: config.ManagedByFleet}); !errors.Is(err, ErrFleetRejected) {
		t.Fatalf("batch accepted: %v", err)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, h.profiles)) || len(h.systemd.calls) != 0 {
		t.Fatal("batch rejection wrote files or systemd")
	}
}

func TestDeleteProfileOnlyWhenUnitMissingAndDeleteArchivesWithDropIns(t *testing.T) {
	h := newFleetHarness(t)
	noUnit := strings.Replace(h.profileRaw("no-unit"), "managed_by: fleet", "managed_by: local", 1)
	h.writeProfile("no-unit", noUnit)
	report, err := h.manager.Delete(context.Background(), DeleteInput{Name: "no-unit", Owner: config.ManagedByLocal})
	if err != nil || !report.Complete || len(h.systemd.calls) != 0 {
		t.Fatalf("profile-only delete=%+v err=%v calls=%v", report, err, h.systemd.calls)
	}
	if _, err := os.Stat(filepath.Join(h.profiles, "no-unit.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile remains: %v", err)
	}
	h.writeProfile("full", h.profileRaw("full"))
	h.writeUnit("full", fleetUnit{loadState: "loaded", active: "active"})
	dropDir := filepath.Join(h.units, "gha-full.service.d")
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropDir, "zz.conf"), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.systemd.units["gha-full.service"] = fleetUnit{loadState: "loaded", active: "active", fragment: filepath.Join(h.units, "gha-full.service"), config: filepath.Join(h.profiles, "full.yaml"), dropIns: filepath.Join(dropDir, "zz.conf")}
	report, err = h.manager.Delete(context.Background(), DeleteInput{Name: "full", Owner: config.ManagedByFleet})
	if err != nil || !report.Complete {
		t.Fatalf("delete=%+v err=%v", report, err)
	}
	wantCalls := []string{"systemctl stop gha-full.service", "systemctl disable gha-full.service", "systemctl daemon-reload"}
	if !reflect.DeepEqual(h.systemd.calls, wantCalls) {
		t.Fatalf("calls=%v", h.systemd.calls)
	}
	archives, _ := filepath.Glob(filepath.Join(filepath.Dir(h.state), "profile-backups", "deleted-full-*"))
	if len(archives) != 1 {
		t.Fatalf("archives=%v", archives)
	}
	for _, name := range []string{"full.yaml", "gha-full.service", "gha-full.service.d"} {
		if _, err := os.Stat(filepath.Join(archives[0], name)); err != nil {
			t.Fatalf("archive missing %s: %v", name, err)
		}
	}
	for _, path := range []string{filepath.Join(h.profiles, "full.yaml"), filepath.Join(h.units, "gha-full.service"), dropDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains", path)
		}
	}
}

func TestDeleteStopFailureDoesNotArchiveAndLockBlocksAllWriters(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p", h.profileRaw("p"))
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "active"})
	h.systemd.failCall["systemctl stop gha-p.service"] = errors.New("timeout")
	report, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet})
	if err == nil || report.Complete || len(report.Results) != 1 || report.Results[0].Outcome != FleetOutcomeFailed {
		t.Fatalf("stop failure=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(h.profiles, "p.yaml")); err != nil {
		t.Fatalf("stop failure archived profile: %v", err)
	}
	archives, _ := filepath.Glob(filepath.Join(filepath.Dir(h.state), "profile-backups", "deleted-p-*"))
	if len(archives) != 0 {
		t.Fatalf("stop failure created archive: %v", archives)
	}
	// Hold the lock and prove every writer fails closed with exit-1 class.
	unlock, err := holdFleetLock(h.manager)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet, DryRun: true}); !errors.Is(err, ErrFleetLocked) {
		t.Fatalf("apply lock: %v", err)
	}
	if _, err := h.manager.Adopt(context.Background(), AdoptInput{Names: []string{"p"}, Owner: config.ManagedByFleet, DryRun: true}); !errors.Is(err, ErrFleetLocked) {
		t.Fatalf("adopt lock: %v", err)
	}
	if _, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet}); !errors.Is(err, ErrFleetLocked) {
		t.Fatalf("delete lock: %v", err)
	}
	if err := h.manager.CreateProfile(context.Background(), CreateProfileInput{Name: "locked", RepoOwner: "me", RepoName: "app", RunnerLabels: []string{"x"}, DockerImage: "i", CPUs: "1", Memory: "1g"}); !errors.Is(err, ErrFleetLocked) {
		t.Fatalf("create lock: %v", err)
	}
}

func TestCustomUnitTemplateAndBrokenTemplateWritesNothing(t *testing.T) {
	h := newFleetHarness(t)
	templatePath := filepath.Join(h.root, "unit.tmpl")
	if err := os.WriteFile(templatePath, []byte("[Service]\nExecStart={{.LoopBinaryPath}} --config {{.ProfileConfigPath}}\nSlice=test.slice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgYAML := fmt.Sprintf("paths:\n  profiles_dir: %s\n  state_dir: %s\n  log_dir: %s\nsystemd:\n  loop_binary_path: /bin/loop\n  unit_template: %s\ndocker:\n  default_access_mode: host-socket\n", h.profiles, h.state, filepath.Join(h.root, "logs"), templatePath)
	if err := os.WriteFile(h.cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeUnit("p", fleetUnit{loadState: "not-found"})
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet, NoStart: true})
	if err != nil || !report.Complete {
		t.Fatalf("template create=%+v err=%v", report, err)
	}
	unit, err := os.ReadFile(filepath.Join(h.units, "gha-p.service"))
	if err != nil || !strings.Contains(string(unit), "Slice=test.slice") || strings.Contains(string(unit), "Restart=always") {
		t.Fatalf("custom template not used: %s %v", unit, err)
	}
	if err := os.WriteFile(templatePath, []byte("{{.Broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.removeUnit("q")
	report, err = h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("q"), Owner: config.ManagedByFleet, DryRun: true})
	if !errors.Is(err, ErrFleetRejected) || report.Complete {
		t.Fatalf("broken template=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(h.profiles, "q.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("broken template wrote profile")
	}
}

func TestApplyWithPruneDeletesFleetProfilesAndRejectsBrokenInventory(t *testing.T) {
	h := newFleetHarness(t)
	keep := h.profileRaw("keep")
	h.writeProfile("keep", keep)
	h.writeProfile("stale", h.profileRaw("stale"))
	h.writeProfile("local", strings.Replace(h.profileRaw("local"), "managed_by: fleet", "managed_by: local", 1))
	h.writeUnit("keep", fleetUnit{loadState: "loaded"})
	h.writeUnit("stale", fleetUnit{loadState: "loaded", active: "inactive"})
	plan, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: []byte(keep), Owner: config.ManagedByFleet, Prune: true, DryRun: true})
	if err != nil || plan.PlanID == "" {
		t.Fatalf("prune plan=%+v err=%v", plan, err)
	}
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: []byte(keep), Owner: config.ManagedByFleet, Prune: true, ExpectPlan: plan.PlanID})
	if err != nil || !report.Complete {
		t.Fatalf("prune=%+v err=%v", report, err)
	}
	if got := fleetNames(report.Results); got["stale"].Action != FleetActionDelete || got["keep"].Action != FleetActionUnchanged || got["local"].Action != "" {
		t.Fatalf("prune results=%+v", report.Results)
	}
	if _, err := os.Stat(filepath.Join(h.profiles, "stale.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale fleet profile not deleted")
	}
	if _, err := os.Stat(filepath.Join(h.profiles, "local.yaml")); err != nil {
		t.Fatal("local profile deleted by prune")
	}
	if err := os.WriteFile(filepath.Join(h.profiles, "bad.yaml"), []byte("name: [bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: []byte(keep), Owner: config.ManagedByFleet, Prune: true, DryRun: true}); !errors.Is(err, ErrFleetRejected) {
		t.Fatalf("broken inventory accepted: %v", err)
	}
}

func TestApplyResumesCreateAfterBothFilesExistButDaemonReloadDidNotRun(t *testing.T) {
	h := newFleetHarness(t)
	raw := h.profileRaw("p")
	profilePath := filepath.Join(h.profiles, "p.yaml")
	if err := os.WriteFile(profilePath, []byte(raw), 0o640); err != nil {
		t.Fatal(err)
	}
	rendered, err := renderServiceFile(serviceTemplateData{ProfileName: "p", GitHubEnvFile: "/fake/env", LoopBinaryPath: "/bin/loop", ProfileConfigPath: profilePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.units, "gha-p.service"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	// systemd has not been reloaded yet, so the unit still reads as not-found.
	h.removeUnit("p")
	intent := fleetIntent{Operation: "apply-create", Name: "p", Owner: "fleet", ProfilePath: profilePath,
		UnitPath: filepath.Join(h.units, "gha-p.service"), Service: "gha-p.service", NoStart: false,
		ProfileSHA: shaOfBytes([]byte(raw)), UnitSHA: shaOfBytes(rendered), UnitEvidence: "not-found"}
	if err := writeIntent(h.cfg, intent); err != nil {
		t.Fatal(err)
	}
	plan, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet, DryRun: true})
	if err != nil || plan.Results[0].Action != FleetActionCreate {
		t.Fatalf("resume plan=%+v err=%v", plan, err)
	}
	done, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet})
	if err != nil || !done.Complete || done.Results[0].Outcome != "done" {
		t.Fatalf("resume=%+v err=%v", done, err)
	}
	if !reflect.DeepEqual(h.systemd.calls, []string{"systemctl daemon-reload", "systemctl enable gha-p.service", "systemctl start gha-p.service"}) {
		t.Fatalf("resume calls=%v", h.systemd.calls)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "fleet-intents", "p.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains: %v", err)
	}
}

func TestDeleteRejectsSymlinkUnitsExternalDropInsAndDuplicateClaims(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p", h.profileRaw("p"))
	unitPath := filepath.Join(h.units, "gha-p.service")
	if err := os.Symlink(filepath.Join(h.units, "elsewhere.service"), unitPath); err != nil {
		t.Fatal(err)
	}
	h.systemd.units["gha-p.service"] = fleetUnit{loadState: "loaded", active: "inactive", fragment: unitPath, config: filepath.Join(h.profiles, "p.yaml")}
	if _, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet}); !errors.Is(err, ErrFleetRejected) {
		t.Fatalf("symlink unit accepted: %v", err)
	}
	if len(h.systemd.calls) != 0 {
		t.Fatalf("rejected delete called systemd: %v", h.systemd.calls)
	}
	if err := os.Remove(unitPath); err != nil {
		t.Fatal(err)
	}
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "inactive", dropIns: "/etc/systemd/system/vendor.conf"})
	if _, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet}); !errors.Is(err, ErrFleetRejected) {
		t.Fatalf("external drop-in accepted: %v", err)
	}
	h.writeProfile("other", strings.Replace(h.profileRaw("other"), "gha-other.service", "gha-p.service", 1))
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "inactive"})
	if _, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet}); !errors.Is(err, ErrFleetRejected) {
		t.Fatalf("duplicate claim accepted: %v", err)
	}
}

func TestCreateProfileUsesLockAndTemplate(t *testing.T) {
	h := newFleetHarness(t)
	templatePath := filepath.Join(h.root, "create.tmpl")
	if err := os.WriteFile(templatePath, []byte("[Service]\nSlice=custom.slice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgYAML := fmt.Sprintf("paths:\n  profiles_dir: %s\n  state_dir: %s\n  log_dir: %s\nsystemd:\n  loop_binary_path: /bin/loop\n  unit_template: %s\ndocker:\n  default_access_mode: host-socket\n", h.profiles, h.state, filepath.Join(h.root, "logs"), templatePath)
	if err := os.WriteFile(h.cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	input := CreateProfileInput{Name: "created", RepoOwner: "me", RepoName: "app", RunnerLabels: []string{"linux"}, DockerImage: "image", CPUs: "1", Memory: "1g", NoStart: true}
	if err := h.manager.CreateProfile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(h.units, "gha-created.service"))
	if err != nil || !strings.Contains(string(unit), "Slice=custom.slice") {
		t.Fatalf("create template=%s err=%v", unit, err)
	}
	if !reflect.DeepEqual(h.systemd.calls, []string{"systemctl daemon-reload", "systemctl enable gha-created.service"}) {
		t.Fatalf("calls=%v", h.systemd.calls)
	}
}

func TestConfigAndSystemdPrimitives(t *testing.T) {
	p := config.Profile{Name: "p", Repo: config.RepoConfig{Owner: "me", Name: "app"}, Service: config.ServiceConfig{Name: "gha-p.service"}, Docker: config.DockerProfile{ContainerNamePrefix: "gha-p"}, ManagedBy: "other"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "managed_by") {
		t.Fatalf("invalid managed_by accepted: %v", err)
	}
	for _, value := range []string{"", "local", "fleet"} {
		p.ManagedBy = value
		if err := p.Validate(); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	root := t.TempDir()
	path := filepath.Join(root, "p.yaml")
	if err := os.WriteFile(path, []byte("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\ndocker: {container_name_prefix: gha-p}\nmanaged_by: fleet\nunknown_future_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadProfile(path)
	if err != nil || loaded.ManagedBy != "fleet" || loaded.EffectiveManagedBy() != "fleet" {
		t.Fatalf("legacy decode=%+v err=%v", loaded, err)
	}
	info, err := parseUnitShowForTest("LoadState=loaded\nActiveState=active\nFragmentPath=/etc/systemd/system/gha-p.service\nDropInPaths=/etc/systemd/system/gha-p.service.d/zz.conf\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config /etc/gha-runner-tui/profiles-live/p.yaml ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := info.ConfigPath(); got != "/etc/gha-runner-tui/profiles-live/p.yaml" || len(info.DropInPaths) != 1 {
		t.Fatalf("unit info=%+v", info)
	}
	dup, parseErr := parseUnitShowForTest("LoadState=loaded\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config /a --config /b ; ignore_errors=no }\n")
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if _, err := dup.ConfigPath(); err == nil {
		t.Fatal("duplicate --config accepted")
	}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if info.IsDir() {
			out[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func holdFleetLock(m RunnerManager) (func(), error) {
	f, err := os.OpenFile(m.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func TestContractFixtureShapes(t *testing.T) {
	// These fixtures are the T1 side of the shared contract. T2 copies them
	// into fleet/tests/fixtures under the same names.
	type scenario struct {
		name   string
		report FleetReport
	}
	export := ExportReport{Complete: false, Profiles: []ExportEntry{{Name: "a", Path: "/etc/gha-runner-tui/profiles-live-20261117/a.yaml", Owner: "fleet", YAML: "name: a\n"}}, Errors: []ExportError{{Path: "/etc/gha-runner-tui/profiles-live-20261117/b.yaml", Error: "yaml: line 1: did not find expected node content"}}}
	exportComplete := ExportReport{Complete: true, Profiles: export.Profiles, Errors: []ExportError{}}
	planMixed := FleetReport{Phase: "plan", PlanID: "sha256:1111111111111111111111111111111111111111111111111111111111111111", Results: []FleetResult{
		{Name: "create", Action: "create", Outcome: "planned", Restart: "none", Message: ""},
		{Name: "format-update", Action: "update", Outcome: "planned", Restart: "none", Message: ""},
		{Name: "restart-update", Action: "update", RestartNeeded: true, Outcome: "planned", Restart: "none", Message: ""},
		{Name: "unchanged", Action: "unchanged", Outcome: "planned", Restart: "none", Message: ""},
		{Name: "restart", Action: "restart", RestartNeeded: true, Outcome: "planned", Restart: "none", Message: ""},
	}}
	scenarios := []scenario{
		{"export-complete", FleetReport{}},
		{"export-incomplete", FleetReport{}},
		{"plan-mixed", planMixed},
		{"plan-conflict", FleetReport{Phase: "plan", PlanID: "sha256:4444444444444444444444444444444444444444444444444444444444444444", Results: []FleetResult{{Name: "p", Action: "conflict", Outcome: "planned", Restart: "none", Message: "profile belongs to local; use adopt first"}}}},
		{"plan-stale", FleetReport{Phase: "plan", PlanID: "sha256:2222222222222222222222222222222222222222222222222222222222222222", Results: []FleetResult{{Name: "p", Action: "update", RestartNeeded: true, Outcome: "planned", Restart: "none", Message: "plan_id does not match the approved plan; nothing was written"}}}},
		{"execute-ok", FleetReport{Phase: "execute", PlanID: planMixed.PlanID, Complete: true, Results: []FleetResult{{Name: "a", Action: "update", RestartNeeded: true, Outcome: "done", Restart: "queued", Message: ""}}}},
		{"execute-partial", FleetReport{Phase: "execute", PlanID: planMixed.PlanID, Complete: false, Results: []FleetResult{{Name: "a", Action: "update", RestartNeeded: true, Outcome: "failed", Restart: "failed", Message: "restart failed"}, {Name: "b", Action: "update", Outcome: "not_run", Restart: "none", Message: "not run after an earlier failure"}}}},
		{"adopt-ok", FleetReport{Phase: "execute", PlanID: "sha256:3333333333333333333333333333333333333333333333333333333333333333", Complete: true, Results: []FleetResult{{Name: "a", Action: "adopt", Outcome: "done", Restart: "none", Message: ""}}}},
		{"match-mode", FleetReport{}},
	}
	for _, s := range scenarios {
		var data []byte
		var err error
		switch s.name {
		case "export-complete":
			data, err = json.Marshal(exportComplete)
		case "export-incomplete":
			data, err = json.Marshal(export)
		case "match-mode":
			data, err = json.Marshal(struct {
				Results []struct {
					Name    string `json:"name"`
					Outcome string `json:"outcome"`
					Message string `json:"message"`
				} `json:"results"`
			}{Results: []struct {
				Name    string `json:"name"`
				Outcome string `json:"outcome"`
				Message string `json:"message"`
			}{{Name: "a", Outcome: "done", Message: "profile a: mode=0 delay=60s (0=paused, 1=normal, 2=delayed)"}, {Name: "b", Outcome: "done", Message: "profile b: mode=0 delay=60s (0=paused, 1=normal, 2=delayed)"}}})
		default:
			data, err = json.Marshal(s.report)
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("..", "..", "cmd", "gha-runner-tui", "testdata", "contract", s.name+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("UPDATE_CONTRACT_FIXTURES") != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing fixture %s: %v", path, err)
		}
		var gotJSON, wantJSON any
		if err := json.Unmarshal(append(data, '\n'), &gotJSON); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &wantJSON); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Fatalf("%s: fixture=%s generated=%s", path, want, data)
		}
	}
}

func TestUnitEvidenceDigestBindsEffectiveDropInPaths(t *testing.T) {
	base := unitEvidence{Service: "gha-p.service", LoadState: "loaded", Fragment: "/etc/systemd/system/gha-p.service",
		FragmentSHA: "fragment", ConfigPath: "/etc/gha-runner-tui/p.yaml", UnitFile: "unit", DropInDir: "dropins",
		DropIns: []string{"zz.conf\x00file\x00abc"}, dropInPaths: []string{"/etc/systemd/system/gha-p.service.d/zz.conf"}}
	changed := base
	changed.dropInPaths = []string{"/usr/lib/systemd/system/gha-p.service.d/vendor.conf"}
	if base.digest() == changed.digest() {
		t.Fatal("effective drop-in paths are not bound into unit evidence")
	}
}

func TestWriteNewFileAtomicNeverPublishesPartialFileAndNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "p.yaml")
	interrupted := errors.New("interrupted before publication")
	err := writeNewFileAtomicWith(dir, "p.yaml", []byte("complete"), 0o640, func(old, new string) error {
		if data, readErr := os.ReadFile(old); readErr != nil || string(data) != "complete" {
			t.Fatalf("temporary file was not fully written: %q %v", data, readErr)
		}
		return interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatalf("interrupted publication: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial target published: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary file remains: %v %v", entries, err)
	}
	if err := writeNewFileAtomic(dir, "p.yaml", []byte("complete"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("published file: %v %v", info, err)
	}
	if err := writeNewFileAtomic(dir, "p.yaml", []byte("other"), 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing target was replaceable: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "complete" {
		t.Fatalf("existing target changed: %q %v", data, err)
	}
}

func TestFleetLockPathIsSharedForAllUIDs(t *testing.T) {
	m := RunnerManager{ConfigPath: "/tmp/ignored-config.yaml"}
	if got := fleetLockPath(m); got != DefaultApplyLockPath {
		t.Fatalf("default lock path=%s, want %s", got, DefaultApplyLockPath)
	}
	override := filepath.Join(t.TempDir(), "apply.lock")
	m.LockPath = override
	if got := fleetLockPath(m); got != override {
		t.Fatalf("override lock path=%s, want %s", got, override)
	}
}

func TestFleetLockAccessFailureProvidesSudoHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write any lock directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	m := RunnerManager{LockPath: filepath.Join(dir, "apply.lock")}
	err := m.withFleetLock(func() error {
		t.Fatal("lock body ran without the lock")
		return nil
	})
	if !errors.Is(err, ErrFleetRejected) || !strings.Contains(err.Error(), "sudo") || !strings.Contains(err.Error(), "write access") {
		t.Fatalf("lock access failure=%v", err)
	}
}

func TestApplyPruneRevalidatesUnitAfterEarlierDaemonReload(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("z-retarget", h.profileRaw("z-retarget"))
	unitPath := filepath.Join(h.units, "gha-z-retarget.service")
	dropDir := unitPath + ".d"
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fleetConfig := filepath.Join(h.profiles, "z-retarget.yaml")
	localConfig := filepath.Join(h.root, "local", "z-retarget.yaml")
	if err := os.WriteFile(filepath.Join(dropDir, "zz.conf"), []byte("[Service]\nExecStart=/bin/loop --config "+localConfig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.writeUnit("z-retarget", fleetUnit{loadState: "loaded", active: "active", config: fleetConfig, dropIns: filepath.Join(dropDir, "zz.conf")})
	// A create earlier in the batch reloads systemd; the edited drop-in now
	// retargets the pruned unit to the local profile.
	h.systemd.onReload = func() {
		h.systemd.units["gha-z-retarget.service"] = fleetUnit{loadState: "loaded", active: "active",
			fragment: unitPath, config: localConfig, dropIns: filepath.Join(dropDir, "zz.conf")}
	}
	bundle := h.bundle("a-create")
	plan, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: bundle, Owner: config.ManagedByFleet, Prune: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: bundle, Owner: config.ManagedByFleet, Prune: true, ExpectPlan: plan.PlanID})
	if !errors.Is(err, ErrFleetPartial) || report.Complete {
		t.Fatalf("retargeted prune=%+v err=%v", report, err)
	}
	results := fleetNames(report.Results)
	if results["a-create"].Outcome != FleetOutcomeDone || results["z-retarget"].Outcome != FleetOutcomeFailed {
		t.Fatalf("results=%+v", report.Results)
	}
	for _, call := range h.systemd.calls {
		if strings.Contains(call, "gha-z-retarget") {
			t.Fatalf("destructive call for retargeted unit: %s", call)
		}
	}
	if _, err := os.Stat(filepath.Join(h.profiles, "z-retarget.yaml")); err != nil {
		t.Fatalf("retargeted profile was archived: %v", err)
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("retargeted unit was archived: %v", err)
	}
	archives, _ := filepath.Glob(filepath.Join(backupDir(h.cfg), "deleted-z-retarget-*"))
	if len(archives) != 0 {
		t.Fatalf("retargeted unit created an archive: %v", archives)
	}
}

func TestDeleteResumesPartiallyArchivedDeleteWithoutStop(t *testing.T) {
	testResumeDeleteWithArchivedArtifacts(t, []string{"p.yaml", "gha-p.service"}, []string{"systemctl daemon-reload"})
}

func TestDeleteResumesWithOnlyDropInsArchived(t *testing.T) {
	testResumeDeleteWithArchivedArtifacts(t, []string{"gha-p.service.d"}, []string{
		"systemctl stop gha-p.service", "systemctl disable gha-p.service", "systemctl daemon-reload",
	})
}

func testResumeDeleteWithArchivedArtifacts(t *testing.T, archived, wantCalls []string) {
	t.Helper()
	h := newFleetHarness(t)
	raw := h.profileRaw("p")
	profilePath := h.writeProfile("p", raw)
	unitPath := filepath.Join(h.units, "gha-p.service")
	dropDir := unitPath + ".d"
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropDir, "zz.conf"), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(filepath.Dir(h.state), "restart-pending", "p")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("p\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "active", dropIns: filepath.Join(dropDir, "zz.conf")})
	evidence, err := collectUnitEvidence(context.Background(), h.manager, h.cfg, "gha-p.service", profilePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveDir := filepath.Join(backupDir(h.cfg), "deleted-p-"+evidence.digest()[:12])
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := fleetIntent{Operation: "delete", Name: "p", Owner: config.ManagedByFleet,
		ProfilePath: profilePath, UnitPath: unitPath, Service: "gha-p.service",
		ProfileSHA: shaOfBytes([]byte(raw)), UnitEvidence: evidence.digest(),
		OriginalUnit: evidence.UnitFile, OriginalDropIns: evidence.DropInDir,
		HadUnit: true, HadDropIns: true, HadMarker: true, ArchiveDir: archiveDir,
		FragmentPath: evidence.Fragment, EffectiveConfig: evidence.ConfigPath}
	if err := writeIntent(h.cfg, intent); err != nil {
		t.Fatal(err)
	}
	// Interruption before all artifacts were archived and before daemon-reload.
	for _, path := range []string{profilePath, unitPath, dropDir, markerPath} {
		if name := filepath.Base(path); slices.Contains(archived, name) {
			if err := os.Rename(path, filepath.Join(archiveDir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	report, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet})
	if err != nil || !report.Complete || report.Phase != FleetPhaseExecute {
		t.Fatalf("resume delete=%+v err=%v", report, err)
	}
	if !reflect.DeepEqual(h.systemd.calls, wantCalls) {
		t.Fatalf("resume delete calls=%v", h.systemd.calls)
	}
	for _, name := range []string{"p.yaml", "gha-p.service", "gha-p.service.d", "p"} {
		if _, err := os.Stat(filepath.Join(archiveDir, name)); err != nil {
			t.Fatalf("archive missing %s: %v", name, err)
		}
	}
	for _, path := range []string{profilePath, unitPath, dropDir, markerPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains", path)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "fleet-intents", "p.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains: %v", err)
	}
}

func TestDeleteCompletesArchivedIntentAfterInterruptedReload(t *testing.T) {
	h := newFleetHarness(t)
	raw := h.profileRaw("p")
	profilePath := h.writeProfile("p", raw)
	unitPath := filepath.Join(h.units, "gha-p.service")
	dropDir := unitPath + ".d"
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropDir, "zz.conf"), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(filepath.Dir(h.state), "restart-pending", "p")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("p\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "active", dropIns: filepath.Join(dropDir, "zz.conf")})
	evidence, err := collectUnitEvidence(context.Background(), h.manager, h.cfg, "gha-p.service", profilePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveDir := filepath.Join(backupDir(h.cfg), "deleted-p-"+evidence.digest()[:12])
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := fleetIntent{Operation: "delete", Name: "p", Owner: config.ManagedByFleet,
		ProfilePath: profilePath, UnitPath: unitPath, Service: "gha-p.service",
		ProfileSHA: shaOfBytes([]byte(raw)), UnitEvidence: evidence.digest(),
		OriginalUnit: evidence.UnitFile, OriginalDropIns: evidence.DropInDir,
		HadUnit: true, HadDropIns: true, HadMarker: true, ArchiveDir: archiveDir,
		FragmentPath: evidence.Fragment, EffectiveConfig: evidence.ConfigPath}
	if err := writeIntent(h.cfg, intent); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{profilePath, unitPath, dropDir, markerPath} {
		if err := os.Rename(path, filepath.Join(archiveDir, filepath.Base(path))); err != nil {
			t.Fatal(err)
		}
	}
	h.systemd.failCall["systemctl daemon-reload"] = errors.New("reload interrupted")
	if report, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet}); err == nil || report.Complete {
		t.Fatalf("interrupted reload=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "fleet-intents", "p.json")); err != nil {
		t.Fatalf("intent was removed before reload recovery: %v", err)
	}
	h.systemd.failCall = map[string]error{}
	h.systemd.calls = nil
	report, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet})
	if err != nil || !report.Complete {
		t.Fatalf("reload recovery=%+v err=%v", report, err)
	}
	if !reflect.DeepEqual(h.systemd.calls, []string{"systemctl daemon-reload"}) {
		t.Fatalf("recovery calls=%v", h.systemd.calls)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "fleet-intents", "p.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent remains after recovery: %v", err)
	}
}

func TestDeleteFailsClosedWhenInterruptedArtifactIdentityDiverges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(h *fleetHarness, archiveDir, dropDir string)
	}{
		{
			name: "live drop-ins",
			tamper: func(h *fleetHarness, archiveDir, dropDir string) {
				if err := os.WriteFile(filepath.Join(dropDir, "extra.conf"), []byte("[Service]\n"), 0o644); err != nil {
					h.t.Fatal(err)
				}
			},
		},
		{
			name: "archived unit",
			tamper: func(h *fleetHarness, archiveDir, dropDir string) {
				if err := os.WriteFile(filepath.Join(archiveDir, "gha-p.service"), []byte("changed"), 0o644); err != nil {
					h.t.Fatal(err)
				}
			},
		},
		{
			name: "replacement unit",
			tamper: func(h *fleetHarness, _, dropDir string) {
				h.writeUnit("p", fleetUnit{loadState: "loaded", fragment: filepath.Join(h.root, "gha-p.service"),
					config: filepath.Join(h.profiles, "other.yaml"), dropIns: filepath.Join(dropDir, "zz.conf")})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFleetHarness(t)
			raw := h.profileRaw("p")
			profilePath := h.writeProfile("p", raw)
			unitPath := filepath.Join(h.units, "gha-p.service")
			dropDir := unitPath + ".d"
			if err := os.MkdirAll(dropDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dropDir, "zz.conf"), []byte("[Service]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(filepath.Dir(h.state), "restart-pending", "p")
			if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(markerPath, []byte("p\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			h.writeUnit("p", fleetUnit{loadState: "loaded", active: "active", dropIns: filepath.Join(dropDir, "zz.conf")})
			evidence, err := collectUnitEvidence(context.Background(), h.manager, h.cfg, "gha-p.service", profilePath)
			if err != nil {
				t.Fatal(err)
			}
			archiveDir := filepath.Join(backupDir(h.cfg), "deleted-p-"+evidence.digest()[:12])
			if err := os.MkdirAll(archiveDir, 0o700); err != nil {
				t.Fatal(err)
			}
			intent := fleetIntent{Operation: "delete", Name: "p", Owner: config.ManagedByFleet,
				ProfilePath: profilePath, UnitPath: unitPath, Service: "gha-p.service",
				ProfileSHA: shaOfBytes([]byte(raw)), UnitEvidence: evidence.digest(),
				OriginalUnit: evidence.UnitFile, OriginalDropIns: evidence.DropInDir,
				HadUnit: true, HadDropIns: true, HadMarker: true, ArchiveDir: archiveDir,
				FragmentPath: evidence.Fragment, EffectiveConfig: evidence.ConfigPath}
			if err := writeIntent(h.cfg, intent); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(profilePath, filepath.Join(archiveDir, "p.yaml")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(unitPath, filepath.Join(archiveDir, "gha-p.service")); err != nil {
				t.Fatal(err)
			}
			tc.tamper(h, archiveDir, dropDir)
			report, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet})
			if !errors.Is(err, ErrFleetRejected) || report.Complete {
				t.Fatalf("diverged resume=%+v err=%v", report, err)
			}
			if len(h.systemd.calls) != 0 {
				t.Fatalf("diverged resume called systemd: %v", h.systemd.calls)
			}
			if _, err := os.Stat(dropDir); err != nil {
				t.Fatalf("drop-ins were moved after divergence: %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "fleet-intents", "p.json")); err != nil {
				t.Fatalf("intent removed after divergence: %v", err)
			}
		})
	}
}

func TestDeleteFinalizesWithVendorFragmentAfterArchive(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			h := newFleetHarness(t)
			profilePath := h.writeProfile("p", h.profileRaw("p"))
			h.writeUnit("p", fleetUnit{loadState: "loaded"})
			vendorPath := filepath.Join(h.root, "gha-p.service")
			vendorContent := []byte("[Service]\nExecStart=/bin/vendor\n")
			if err := os.WriteFile(vendorPath, vendorContent, 0o644); err != nil {
				t.Fatal(err)
			}
			h.systemd.onReload = func() {
				h.systemd.units["gha-p.service"] = fleetUnit{loadState: "loaded", fragment: vendorPath, noExec: true}
			}
			wantCalls := []string{"systemctl stop gha-p.service", "systemctl disable gha-p.service", "systemctl daemon-reload"}
			input := DeleteInput{Name: "p", Owner: config.ManagedByFleet}
			if interrupted {
				h.systemd.showErr = map[int]error{3: errors.New("cannot read post-reload status")}
				if report, err := h.manager.Delete(context.Background(), input); err == nil || report.Complete || !slices.Equal(h.systemd.calls, wantCalls) {
					t.Fatalf("interrupted delete=%+v err=%v calls=%v", report, err, h.systemd.calls)
				}
				h.systemd.calls = nil
				wantCalls = []string{"systemctl daemon-reload"}
			}
			report, err := h.manager.Delete(context.Background(), input)
			if err != nil || !report.Complete || !strings.Contains(report.Results[0].Message, vendorPath) {
				t.Fatalf("vendor fallback delete=%+v err=%v", report, err)
			}
			if !slices.Equal(h.systemd.calls, wantCalls) {
				t.Fatalf("systemd calls=%v, want %v", h.systemd.calls, wantCalls)
			}
			if raw, err := os.ReadFile(vendorPath); err != nil || string(raw) != string(vendorContent) {
				t.Fatalf("vendor file was touched: %q, %v", raw, err)
			}
			unitPath := filepath.Join(h.units, "gha-p.service")
			for _, path := range []string{profilePath, unitPath, intentPath(h.cfg, "p")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s remains: %v", path, err)
				}
			}
			for _, name := range []string{"p.yaml", "gha-p.service"} {
				paths, err := filepath.Glob(filepath.Join(backupDir(h.cfg), "deleted-p-*", name))
				if err != nil || len(paths) != 1 {
					t.Fatalf("archive of %s: %v, %v", name, paths, err)
				}
			}
		})
	}
}

func TestDeleteRetainsIntentWhenReloadRecreatesManagedFragment(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p", h.profileRaw("p"))
	h.writeUnit("p", fleetUnit{loadState: "loaded"})
	h.systemd.onReload = func() {
		// Even an identical file at the managed path is not a vendor fallback.
		h.writeUnit("p", fleetUnit{loadState: "loaded"})
	}
	report, err := h.manager.Delete(context.Background(), DeleteInput{Name: "p", Owner: config.ManagedByFleet})
	if err == nil || report.Complete || report.Results[0].Outcome != FleetOutcomeFailed {
		t.Fatalf("delete completed with recreated managed unit: %+v err=%v", report, err)
	}
	for _, path := range []string{filepath.Join(h.units, "gha-p.service"), intentPath(h.cfg, "p")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed: %v", path, err)
		}
	}
}

func TestApplyUpdateSkipsRestartForConfirmedAbsentUnit(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			h := newFleetHarness(t)
			profilePath := h.writeProfile("p", strings.Replace(h.profileRaw("p"), "image:test", "image:old", 1))
			input := ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet}
			if interrupted {
				h.systemd.showErr = map[int]error{2: errors.New("dbus unavailable")}
				if report, err := h.manager.Apply(context.Background(), input); !errors.Is(err, ErrFleetPartial) || report.Results[0].Restart != FleetRestartFailed {
					t.Fatalf("read failure=%+v err=%v", report, err)
				}
				for _, path := range []string{restartPendingPath(h.cfg, "p"), intentPath(h.cfg, "p")} {
					if _, err := os.Stat(path); err != nil {
						t.Fatalf("missing recovery evidence %s: %v", path, err)
					}
				}
			}
			report, err := h.manager.Apply(context.Background(), input)
			if err != nil || !report.Complete || report.Results[0].Restart != FleetRestartSkipped {
				t.Fatalf("absent unit apply=%+v err=%v", report, err)
			}
			if len(h.systemd.calls) != 0 {
				t.Fatalf("absent unit apply called systemd: %v", h.systemd.calls)
			}
			if raw, err := os.ReadFile(profilePath); err != nil || string(raw) != string(input.Bundle) {
				t.Fatalf("profile update: %q, %v", raw, err)
			}
			for _, path := range []string{restartPendingPath(h.cfg, "p"), intentPath(h.cfg, "p")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s remains: %v", path, err)
				}
			}
		})
	}
}

func TestApplyRestartPreservesEvidenceWhenUnitAbsenceChanges(t *testing.T) {
	for _, wasLoaded := range []bool{false, true} {
		t.Run(fmt.Sprintf("was-loaded=%t", wasLoaded), func(t *testing.T) {
			h := newFleetHarness(t)
			h.writeProfile("p", strings.Replace(h.profileRaw("p"), "image:test", "image:old", 1))
			if wasLoaded {
				h.writeUnit("p", fleetUnit{loadState: "loaded"})
			}
			unitPath := filepath.Join(h.units, "gha-p.service")
			h.systemd.onShow = func(n int) {
				if n != 2 {
					return
				}
				if wasLoaded {
					h.removeUnit("p")
					if err := os.Remove(unitPath); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(unitPath, []byte("[Service]\n"), 0o644); err != nil {
					// The new file is not visible to systemd until reload.
					t.Fatal(err)
				}
			}
			input := ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet}
			if report, err := h.manager.Apply(context.Background(), input); !errors.Is(err, ErrFleetPartial) || report.Complete || report.Results[0].Restart != FleetRestartFailed {
				t.Fatalf("changed absence=%+v err=%v", report, err)
			}
			if report, err := h.manager.Apply(context.Background(), input); !errors.Is(err, ErrFleetRejected) || report.Complete {
				t.Fatalf("changed absence retry=%+v err=%v", report, err)
			}
			if len(h.systemd.calls) != 0 {
				t.Fatalf("changed absence called systemd: %v", h.systemd.calls)
			}
			for _, path := range []string{restartPendingPath(h.cfg, "p"), intentPath(h.cfg, "p")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("missing recovery evidence %s: %v", path, err)
				}
			}
		})
	}
}

func TestRestartExecutionActiveStateFailureRetainsMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*fleetRunner)
	}{
		{"transient", func(r *fleetRunner) { r.showErr = map[int]error{2: errors.New("dbus unavailable")} }},
		{"partial output", func(r *fleetRunner) {
			r.showErr = map[int]error{2: errors.New("dbus disconnected")}
			r.showOutput = map[int][]byte{2: []byte("LoadState=loaded\nActiveState=inactive\n")}
		}},
		{"unit disappeared", func(r *fleetRunner) {
			r.showErr = map[int]error{2: errors.New("unit not found")}
			r.showOutput = map[int][]byte{2: []byte("LoadState=not-found\nActiveState=inactive\n")}
		}},
		{"invalid", func(r *fleetRunner) { r.activeOverride = map[int]string{2: ""} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFleetHarness(t)
			h.writeProfile("p", strings.Replace(h.profileRaw("p"), "image:test", "image:old", 1))
			h.writeUnit("p", fleetUnit{loadState: "loaded", active: "active"})
			tc.run(h.systemd)
			report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet})
			if !errors.Is(err, ErrFleetPartial) || report.Complete {
				t.Fatalf("state read failure=%+v err=%v", report, err)
			}
			if result := report.Results[0]; result.Outcome != FleetOutcomeFailed || result.Restart != FleetRestartFailed {
				t.Fatalf("state read result=%+v", result)
			}
			marker := filepath.Join(filepath.Dir(h.state), "restart-pending", "p")
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("restart marker was removed: %v", err)
			}
		})
	}
}

func TestRestartInactiveStillSkipsAndClearsMarker(t *testing.T) {
	h := newFleetHarness(t)
	h.writeProfile("p", strings.Replace(h.profileRaw("p"), "image:test", "image:old", 1))
	h.writeUnit("p", fleetUnit{loadState: "loaded", active: "inactive"})
	report, err := h.manager.Apply(context.Background(), ApplyInput{Bundle: h.bundle("p"), Owner: config.ManagedByFleet})
	if err != nil || !report.Complete || report.Results[0].Restart != FleetRestartSkipped {
		t.Fatalf("inactive restart=%+v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.state), "restart-pending", "p")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inactive marker remains: %v", err)
	}
	if len(h.systemd.calls) != 0 {
		t.Fatalf("inactive restart called systemd: %v", h.systemd.calls)
	}
}

func TestSetManagedByLineForms(t *testing.T) {
	base := "name: p\nrepo: {owner: me, name: app}\n"
	for _, tc := range []struct {
		name string
		raw  string
		want string
		err  bool
	}{
		{"append-no-newline", base[:len(base)-1], base[:len(base)-1] + "\nmanaged_by: fleet\n", false},
		{"replace", base + "managed_by: local\n", base + "managed_by: fleet\n", false},
		{"duplicate", base + "managed_by: local\nmanaged_by: fleet\n", "", true},
		{"quoted-key", base + "\"managed_by\": local\n", "", true},
		{"quoted-value", base + "managed_by: \"local\"\n", "", true},
		{"flow", "{name: p, managed_by: local}\n", "", true},
		{"nested", base + "docker:\n  managed_by: local\n  container_name_prefix: x\n", "", true},
		{"doc-end", base + "...\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := setManagedByLine([]byte(tc.raw), "fleet")
			if (err != nil) != tc.err {
				t.Fatalf("err=%v want %t", err, tc.err)
			}
			if !tc.err && string(got) != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}
