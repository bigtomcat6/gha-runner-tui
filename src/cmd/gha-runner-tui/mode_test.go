package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCLIModeReadsAndSwitchesWithoutLifecycleActions(t *testing.T) {
	f := newCLIFixture(t)
	profilePath := filepath.Join(f.profiles, "p.yaml")
	before, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args  []string
		mode  int
		delay int
	}{
		{[]string{"mode", "p"}, 1, 60},
		{[]string{"mode", "p", "0"}, 0, 60},
		{[]string{"mode", "p"}, 0, 60},
		{[]string{"mode", "p", "2"}, 2, 60},
		{[]string{"mode", "p", "2", "--delay-seconds", "90"}, 2, 90},
		{[]string{"mode", "p"}, 2, 90},
		{[]string{"mode", "p", "1"}, 1, 60},
	} {
		code, out, stderr := f.run(t, tc.args...)
		if code != 0 || stderr != "" || !strings.Contains(out, "mode="+strconv.Itoa(tc.mode)) {
			t.Fatalf("%v: code=%d out=%q stderr=%q", tc.args, code, out, stderr)
		}
		if len(tc.args) > 2 {
			data, err := os.ReadFile(filepath.Join(f.root, "p.json.admission.json"))
			if err != nil {
				t.Fatal(err)
			}
			var stored struct {
				Mode  int `json:"mode"`
				Delay int `json:"delay_seconds"`
			}
			if err := json.Unmarshal(data, &stored); err != nil || stored.Mode != tc.mode || stored.Delay != tc.delay {
				t.Fatalf("policy=%s err=%v", data, err)
			}
		}
	}
	after, _ := os.ReadFile(profilePath)
	if string(before) != string(after) || len(f.calls) != 0 {
		t.Fatalf("mode changed profile or invoked external commands: calls=%v", f.calls)
	}
}

func TestCLIStatusReportsAdmissionModeAndPolicyErrors(t *testing.T) {
	f := newCLIFixture(t)
	if code, _, stderr := f.run(t, "mode", "p", "2", "--delay-seconds", "90"); code != 0 {
		t.Fatal(stderr)
	}
	code, out, stderr := f.run(t, "status", "--json")
	var dashboard struct {
		Profiles []struct {
			Admission *struct {
				Mode  int `json:"mode"`
				Delay int `json:"delay_seconds"`
			} `json:"admission"`
			Errors []string `json:"errors"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(out), &dashboard); err != nil || code != 0 || stderr != "" || len(dashboard.Profiles) != 1 {
		t.Fatalf("status code=%d out=%q stderr=%q decode=%v", code, out, stderr, err)
	}
	policy := dashboard.Profiles[0].Admission
	if policy == nil || policy.Mode != 2 || policy.Delay != 90 {
		t.Fatalf("status lost admission mode: %s", out)
	}
	writeCLIFile(t, filepath.Join(f.root, "p.json.admission.json"), `{"mode":9}`)
	code, out, _ = f.run(t, "status", "--json")
	dashboard.Profiles = nil
	if err := json.Unmarshal([]byte(out), &dashboard); err != nil || code != 0 || dashboard.Profiles[0].Admission != nil || !strings.Contains(strings.Join(dashboard.Profiles[0].Errors, " "), "admission:") {
		t.Fatalf("invalid policy not visible in status: %s err=%v", out, err)
	}
}

func TestCLIModeRejectsInvalidArgumentsWithoutWriting(t *testing.T) {
	for _, args := range [][]string{
		{"mode"}, {"mode", "p", "3"}, {"mode", "p", "x"}, {"mode", "p", "0", "extra"},
		{"mode", "p", "2", "--delay-seconds", "0"}, {"mode", "p", "2", "--delay-seconds", "86401"},
		{"mode", "p", "1", "--delay-seconds", "60"}, {"mode", "p", "--delay-seconds", "60"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newCLIFixture(t)
			code, _, stderr := f.run(t, args...)
			if code != 2 || stderr == "" || len(f.calls) != 0 {
				t.Fatalf("code=%d stderr=%q calls=%v", code, stderr, f.calls)
			}
			if _, err := os.Stat(filepath.Join(f.root, "p.json.admission.json")); !os.IsNotExist(err) {
				t.Fatalf("invalid input wrote a policy: %v", err)
			}
		})
	}
}

func TestCLIModeQueryDoesNotCreateState(t *testing.T) {
	f := newCLIFixture(t)
	code, _, stderr := f.run(t, "mode", "p")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.root, "p.json.admission.json")); !os.IsNotExist(err) {
		t.Fatalf("query wrote a policy: %v", err)
	}
}
