package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gha-runner-tui/internal/instaboxcontroller"
)

const testMachineA = "m_aaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeAPI struct {
	machine instaboxcontroller.Machine
	getErr  error
	wakeErr error
	gets    int
	wakes   int
	wakeIDs []string
}

func (f *fakeAPI) GetMachine(ctx context.Context, id string) (instaboxcontroller.Machine, error) {
	f.gets++
	if f.getErr != nil {
		return instaboxcontroller.Machine{}, f.getErr
	}
	return f.machine, nil
}

func (f *fakeAPI) WakeMachine(ctx context.Context, id string) (instaboxcontroller.Machine, error) {
	f.wakes++
	f.wakeIDs = append(f.wakeIDs, id)
	if f.wakeErr != nil {
		return instaboxcontroller.Machine{}, f.wakeErr
	}
	return instaboxcontroller.Machine{}, nil
}

type fakeWriter struct {
	writes int
}

func (w *fakeWriter) ReadDeadline(ctx context.Context, target instaboxcontroller.RemoteTarget) (string, error) {
	return "", errors.New("unexpected read")
}

func (w *fakeWriter) WriteDeadline(ctx context.Context, target instaboxcontroller.RemoteTarget, content string) (string, error) {
	w.writes++
	return content, nil
}

func writeConfig(t *testing.T, enableWrite, enableWake bool) string {
	t.Helper()
	cfg := map[string]any{
		"stateDir":            t.TempDir(),
		"pollIntervalSeconds": 30,
		"enableRemoteWrite":   enableWrite,
		"enableWake":          enableWake,
		"machines": []any{map[string]any{
			"machineId":    testMachineA,
			"deadlinePath": "/var/lib/gha-runner-tui/state/session-deadline.json",
			"sshHost":      "runner.example.net",
			"sshUser":      "dev",
			"sshPort":      22,
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "controller.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runningMachine(now time.Time) instaboxcontroller.Machine {
	started := now.UTC().Format(time.RFC3339)
	maxRun := 120
	return instaboxcontroller.Machine{
		ID:            testMachineA,
		State:         "running",
		DesiredState:  "running",
		Runtime:       instaboxcontroller.Runtime{Ready: true},
		RunStartedAt:  &started,
		MaxRunMinutes: &maxRun,
	}
}

func TestMissingAPIKeyExitsBeforeConfigNetworkOrSSH(t *testing.T) {
	for _, key := range []string{"", "   "} {
		calls := 0
		stderr := &bytes.Buffer{}
		code := run([]string{"--config", filepath.Join(t.TempDir(), "missing.json")}, &bytes.Buffer{}, stderr, dependencies{
			getenv: func(name string) string { return key },
			newAPI: func(string) (instaboxcontroller.MachineAPI, error) {
				calls++
				return nil, nil
			},
			newWriter: func() instaboxcontroller.DeadlineWriter {
				calls++
				return &fakeWriter{}
			},
		})
		if code != 2 {
			t.Fatalf("key %q exit = %d, want 2", key, code)
		}
		if calls != 0 {
			t.Fatalf("key %q created %d clients", key, calls)
		}
		if strings.Contains(stderr.String(), "ibk_") {
			t.Fatalf("stderr mentions a key: %s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "INSTABOX_API_KEY") {
			t.Fatalf("stderr = %s", stderr.String())
		}
	}
}

func TestInvalidConfigNeverCreatesAClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.json")
	if err := os.WriteFile(path, []byte(`{"stateDir":"/var/lib/x","apiBaseURL":"http://evil.example","machines":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	code := run([]string{"--config", path}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv: func(string) string { return "ibk_example" },
		newAPI: func(string) (instaboxcontroller.MachineAPI, error) {
			calls++
			return &fakeAPI{}, nil
		},
		newWriter: func() instaboxcontroller.DeadlineWriter {
			calls++
			return &fakeWriter{}
		},
	})
	if code != 2 || calls != 0 {
		t.Fatalf("exit=%d calls=%d", code, calls)
	}
}

func TestOnceDryRunMakesNoWriteOrWake(t *testing.T) {
	configPath := writeConfig(t, true, true)
	writer := &fakeWriter{}
	api := &fakeAPI{machine: runningMachine(time.Now())}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := run([]string{"--config", configPath, "--once", "--dry-run"}, stdout, stderr, dependencies{
		getenv:    func(string) string { return "ibk_example" },
		newAPI:    func(string) (instaboxcontroller.MachineAPI, error) { return api, nil },
		newWriter: func() instaboxcontroller.DeadlineWriter { return writer },
	})
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if writer.writes != 0 || api.wakes != 0 {
		t.Fatalf("writes=%d wakes=%d", writer.writes, api.wakes)
	}
	if !strings.Contains(stderr.String(), "remote writes disabled") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "ibk_example") {
		t.Fatal("stderr leaked the key")
	}
}

func TestOnceWithWritesEnabledUsesTheInjectedWriter(t *testing.T) {
	configPath := writeConfig(t, true, false)
	writer := &fakeWriter{}
	api := &fakeAPI{machine: runningMachine(time.Now())}
	code := run([]string{"--config", configPath, "--once"}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv:    func(string) string { return "ibk_example" },
		newAPI:    func(string) (instaboxcontroller.MachineAPI, error) { return api, nil },
		newWriter: func() instaboxcontroller.DeadlineWriter { return writer },
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d", writer.writes)
	}
	if api.wakes != 0 {
		t.Fatalf("wakes = %d", api.wakes)
	}
}

func TestOnceReportsGetFailure(t *testing.T) {
	configPath := writeConfig(t, false, false)
	api := &fakeAPI{getErr: errors.New("unavailable")}
	code := run([]string{"--config", configPath, "--once"}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv:    func(string) string { return "ibk_example" },
		newAPI:    func(string) (instaboxcontroller.MachineAPI, error) { return api, nil },
		newWriter: func() instaboxcontroller.DeadlineWriter { return &fakeWriter{} },
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}
