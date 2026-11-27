package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/config"
)

func cliFleetProfile(t *testing.T, f *cliFixture, name, owner string) string {
	t.Helper()
	raw := fmt.Sprintf(`name: %[1]s
repo: {owner: me, name: app}
service: {name: gha-%[1]s.service}
runner: {name_prefix: %[1]s, labels: [linux]}
docker: {image: image:test, container_name_prefix: gha-%[1]s, cpus: "1", memory: 1g}
loop: {state_file: %[2]s, log_dir: %[3]s}
managed_by: %[4]s
`, name, filepath.Join(f.root, "state", name+".json"), filepath.Join(f.root, "logs", name), owner)
	if err := os.MkdirAll(filepath.Join(f.root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(f.profiles, name+".yaml"), raw)
	return raw
}

func cliUnit(t *testing.T, f *cliFixture, name string, loaded bool) {
	t.Helper()
	if loaded {
		writeCLIFile(t, filepath.Join(f.units, "gha-"+name+".service"), "[Service]\nExecStart=/bin/loop --config "+filepath.Join(f.profiles, name+".yaml")+"\n")
	}
}

func cliShowResponder(loaded, active map[string]bool, f *cliFixture) func(string, []string) ([]byte, error) {
	return func(name string, args []string) ([]byte, error) {
		if name != "systemctl" || len(args) < 2 || args[0] != "show" {
			return nil, nil
		}
		service := args[len(args)-1]
		profile := strings.TrimSuffix(strings.TrimPrefix(service, "gha-"), ".service")
		var b strings.Builder
		if loaded[service] {
			state := "inactive"
			if active[service] {
				state = "active"
			}
			fmt.Fprintf(&b, "LoadState=loaded\nActiveState=%s\nFragmentPath=%s\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config %s ; ignore_errors=no }\n", state, filepath.Join(f.units, service), filepath.Join(f.profiles, profile+".yaml"))
		} else {
			b.WriteString("LoadState=not-found\nActiveState=inactive\n")
		}
		return []byte(b.String()), nil
	}
}

func TestCLIStatusNoGitHubSkipsRequestsAndReportsRestartPending(t *testing.T) {
	f := newCLIFixture(t)
	f.manager.Service.GitHubForProfile = func(config.GlobalConfig, config.Profile) app.GitHubClient {
		t.Fatal("--no-github queried GitHub")
		return nil
	}
	if err := os.MkdirAll(filepath.Join(f.root, "restart-pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIFile(t, filepath.Join(f.root, "restart-pending", "p"), "pending")
	code, out, stderr := f.run(t, "status", "--json", "--no-github")
	var data struct {
		Profiles []struct {
			RestartPending bool `json:"restart_pending"`
			GitHub         struct {
				State string `json:"state"`
				Busy  string `json:"busy"`
			} `json:"github"`
			Errors []string `json:"errors"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatal(err)
	}
	if code != 0 || stderr != "" || len(data.Profiles) != 1 || data.Profiles[0].GitHub.State != "skipped" || data.Profiles[0].GitHub.Busy != "skipped" || !data.Profiles[0].RestartPending || len(data.Profiles[0].Errors) != 0 {
		t.Fatalf("status=%s code=%d err=%q", out, code, stderr)
	}
	if strings.Contains(out, "profile client is not configured") {
		t.Fatalf("no-github leaked an error: %s", out)
	}
}

func TestCLIMatchModeStartAndArgumentRules(t *testing.T) {
	f := newCLIFixture(t)
	f.profile(t, "p")
	f.profile(t, "q")
	f.respond = func(name string, args []string) ([]byte, error) {
		if name != "systemctl" || (args[0] != "start" && args[0] != "show") {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return []byte("ok\n"), nil
	}
	code, out, stderr := f.run(t, "mode", "--match", "p*", "0", "--json")
	var modeOut struct {
		Results []struct {
			Name    string `json:"name"`
			Outcome string `json:"outcome"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &modeOut); err != nil || code != 0 || stderr != "" || len(modeOut.Results) != 1 || modeOut.Results[0].Name != "p" || modeOut.Results[0].Outcome != "done" {
		t.Fatalf("mode=%q code=%d err=%q decode=%v", out, code, stderr, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "p.json.admission.json")); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = f.run(t, "mode", "--match", "none*", "0", "--json")
	if code != 1 || stderr == "" || out != "{\"results\":[]}\n" {
		t.Fatalf("no match=%q code=%d err=%q", out, code, stderr)
	}
	code, out, stderr = f.run(t, "start", "--match", "p*", "--json")
	if err := json.Unmarshal([]byte(out), &modeOut); err != nil || code != 0 || stderr != "" || len(modeOut.Results) != 1 || modeOut.Results[0].Outcome != "done" {
		t.Fatalf("start=%q code=%d err=%q decode=%v", out, code, stderr, err)
	}
	if len(f.calls) != 1 || f.calls[0][0] != "systemctl" || f.calls[0][1] != "start" {
		t.Fatalf("calls=%v", f.calls)
	}
	for _, args := range [][]string{
		{"stop", "--match", "*", "--force"},
		{"start", "p", "--match", "*"},
		{"mode", "--match", "*", "0", "1"},
	} {
		if code, _, stderr := f.run(t, args...); code != 2 || stderr == "" {
			t.Fatalf("%v code=%d err=%q", args, code, stderr)
		}
	}
}

func TestCLIApplyJSONExitCodesPlanExecuteStaleAndPrune(t *testing.T) {
	f := newCLIFixture(t)
	raw := cliFleetProfile(t, f, "p", "fleet")
	cliUnit(t, f, "p", true)
	f.respond = cliShowResponder(map[string]bool{"gha-p.service": true}, map[string]bool{"gha-p.service": true}, f)
	bundle := raw
	code, out, stderr := f.runInput(t, bundle, "apply", "-f", "-", "--owner", "fleet", "--json", "--dry-run")
	var plan app.FleetReport
	if err := json.Unmarshal([]byte(out), &plan); err != nil || code != 0 || stderr != "" || plan.Phase != "plan" || plan.PlanID == "" || len(plan.Results) != 1 {
		t.Fatalf("plan=%s code=%d err=%q decode=%v", out, code, stderr, err)
	}
	code, out, stderr = f.runInput(t, bundle, "apply", "-f", "-", "--owner", "fleet", "--json", "--expect-plan", plan.PlanID)
	var execute app.FleetReport
	if err := json.Unmarshal([]byte(out), &execute); err != nil || code != 0 || stderr != "" || execute.Phase != "execute" || !execute.Complete || execute.Results[0].Action != "unchanged" {
		t.Fatalf("execute=%s code=%d err=%q decode=%v", out, code, stderr, err)
	}
	if writes := cliWriteCalls(f); len(writes) != 0 {
		t.Fatalf("unchanged wrote: %v", writes)
	}
	code, out, stderr = f.runInput(t, bundle, "apply", "-f", "-", "--owner", "fleet", "--json", "--expect-plan", "sha256:stale")
	if code != 1 || stderr == "" {
		t.Fatalf("stale code=%d err=%q out=%s", code, stderr, out)
	}
	var stale app.FleetReport
	if err := json.Unmarshal([]byte(out), &stale); err != nil || stale.Phase != "plan" || stale.Complete {
		t.Fatalf("stale JSON=%s err=%v", out, err)
	}
	code, out, stderr = f.runInput(t, "name: [bad\n", "apply", "-f", "-", "--owner", "fleet", "--json")
	if code != 2 || stderr == "" {
		t.Fatalf("bad input code=%d err=%q out=%s", code, stderr, out)
	}
	var bad app.FleetReport
	if err := json.Unmarshal([]byte(out), &bad); err != nil || bad.Phase != "plan" {
		t.Fatalf("bad JSON=%s err=%v", out, err)
	}
	if writes := cliWriteCalls(f); len(writes) != 0 {
		t.Fatalf("rejections wrote: %v", writes)
	}
	cliFleetProfile(t, f, "local", "local")
	writeCLIFile(t, filepath.Join(f.profiles, "bad.yaml"), "name: [bad\n")
	code, _, stderr = f.runInput(t, bundle, "apply", "-f", "-", "--owner", "fleet", "--json", "--prune")
	if code != 1 || stderr == "" {
		t.Fatalf("local fleet prune code=%d err=%q", code, stderr)
	}
}

func TestCLIAdoptAndDeleteJSON(t *testing.T) {
	f := newCLIFixture(t)
	raw := cliFleetProfile(t, f, "p", "local")
	cliUnit(t, f, "p", true)
	f.respond = cliShowResponder(map[string]bool{"gha-p.service": true}, map[string]bool{"gha-p.service": false}, f)
	code, out, stderr := f.run(t, "adopt", "--owner", "fleet", "--match", "p", "--json", "--dry-run")
	var plan app.FleetReport
	if err := json.Unmarshal([]byte(out), &plan); err != nil || code != 0 || stderr != "" || plan.PlanID == "" || plan.Results[0].Action != "adopt" {
		t.Fatalf("adopt plan=%s code=%d err=%q decode=%v", out, code, stderr, err)
	}
	code, out, stderr = f.run(t, "adopt", "--owner", "fleet", "--match", "p", "--json", "--expect-plan", plan.PlanID)
	var done app.FleetReport
	if err := json.Unmarshal([]byte(out), &done); err != nil || code != 0 || stderr != "" || done.Phase != "execute" || !done.Complete {
		t.Fatalf("adopt=%s code=%d err=%q decode=%v", out, code, stderr, err)
	}
	changed, _ := os.ReadFile(filepath.Join(f.profiles, "p.yaml"))
	if string(changed) != strings.Replace(raw, "managed_by: local", "managed_by: fleet", 1) {
		t.Fatalf("adopt changed more than managed_by:\n%s", changed)
	}
	if writes := cliWriteCalls(f); len(writes) != 0 {
		t.Fatalf("adopt called systemd write: %v", writes)
	}
	const service = "gha-p.service"
	loaded := map[string]bool{service: true}
	show := cliShowResponder(loaded, map[string]bool{service: true}, f)
	f.respond = func(name string, args []string) ([]byte, error) {
		// systemd keeps the cached unit until it rereads the unit directory.
		if name == "systemctl" && args[0] == "daemon-reload" {
			loaded[service] = !errors.Is(checkNoFile(filepath.Join(f.units, service)), os.ErrNotExist)
		}
		return show(name, args)
	}
	code, out, stderr = f.run(t, "delete", "p", "--owner", "fleet", "--json")
	var deleted app.FleetReport
	if err := json.Unmarshal([]byte(out), &deleted); err != nil || code != 0 || stderr != "" || deleted.Phase != "execute" || !deleted.Complete || deleted.Results[0].Action != "delete" {
		t.Fatalf("delete=%s code=%d err=%q decode=%v", out, code, stderr, err)
	}
	if !errors.Is(checkNoFile(filepath.Join(f.profiles, "p.yaml")), os.ErrNotExist) {
		t.Fatalf("profile remains")
	}
	if writes := cliWriteCalls(f); !reflect.DeepEqual(writes, [][]string{
		{"systemctl", "stop", service}, {"systemctl", "disable", service}, {"systemctl", "daemon-reload"},
	}) {
		t.Fatalf("delete write calls: %v", writes)
	}
}

func cliWriteCalls(f *cliFixture) [][]string {
	var writes [][]string
	for _, call := range f.calls {
		if len(call) > 1 && call[0] == "systemctl" && call[1] == "show" {
			continue
		}
		writes = append(writes, call)
	}
	return writes
}

func checkNoFile(path string) error {
	_, err := os.Stat(path)
	return err
}
