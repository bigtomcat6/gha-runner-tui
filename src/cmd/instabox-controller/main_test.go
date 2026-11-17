package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gha-runner-tui/internal/instaboxcontroller"
)

const (
	testMachineA = "m_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	testMachineB = "m_bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fakeAPI struct {
	key      string
	machines map[string]instaboxcontroller.Machine
	getErr   error
	wakeErr  error
	execFunc func(context.Context, string, string) (instaboxcontroller.ExecResult, error)
	execs    int
	gets     int
	wakes    int
	wakeIDs  []string
}

func (f *fakeAPI) GetMachine(ctx context.Context, id string) (instaboxcontroller.Machine, error) {
	f.gets++
	if f.getErr != nil {
		return instaboxcontroller.Machine{}, f.getErr
	}
	return f.machines[id], nil
}

func (f *fakeAPI) WakeMachine(ctx context.Context, id string) (instaboxcontroller.Machine, error) {
	f.wakes++
	f.wakeIDs = append(f.wakeIDs, id)
	if f.wakeErr != nil {
		return instaboxcontroller.Machine{}, f.wakeErr
	}
	return instaboxcontroller.Machine{}, nil
}

func (f *fakeAPI) ExecCommand(ctx context.Context, id, command string) (instaboxcontroller.ExecResult, error) {
	f.execs++
	if f.execFunc != nil {
		return f.execFunc(ctx, id, command)
	}
	return instaboxcontroller.ExecResult{}, errors.New("unexpected exec")
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

func machineEntry(id, host string, apiKeyEnv any) map[string]any {
	entry := map[string]any{
		"machineId":    id,
		"deadlinePath": "/var/lib/gha-runner-tui/state/session-deadline.json",
		"sshHost":      host,
		"sshUser":      "dev",
		"sshPort":      22,
	}
	if apiKeyEnv != nil {
		entry["apiKeyEnv"] = apiKeyEnv
	}
	return entry
}

func writeConfig(t *testing.T, stateDir string, enableWrite, enableWake bool, machines ...map[string]any) string {
	t.Helper()
	cfg := map[string]any{
		"stateDir":            stateDir,
		"pollIntervalSeconds": 30,
		"enableRemoteWrite":   enableWrite,
		"enableWake":          enableWake,
		"machines":            machines,
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

func runningMachine(id string, now time.Time) instaboxcontroller.Machine {
	started := now.UTC().Format(time.RFC3339)
	maxRun := 120
	return instaboxcontroller.Machine{
		ID:            id,
		State:         "running",
		DesiredState:  "running",
		Runtime:       instaboxcontroller.Runtime{Ready: true},
		RunStartedAt:  &started,
		MaxRunMinutes: &maxRun,
	}
}

func TestMissingAPIKeyExitsBeforeNetworkSSHOrState(t *testing.T) {
	for _, key := range []string{"", "   "} {
		stateDir := filepath.Join(t.TempDir(), "state")
		configPath := writeConfig(t, stateDir, true, true, machineEntry(testMachineA, "runner.example.net", nil))
		calls := 0
		stderr := &bytes.Buffer{}
		code := run([]string{"--config", configPath}, &bytes.Buffer{}, stderr, dependencies{
			getenv: func(name string) string {
				if name == instaboxcontroller.DefaultAPIKeyEnv {
					return key
				}
				return ""
			},
			newAPI: func(string) (instaboxcontroller.MachineAPI, error) {
				calls++
				return &fakeAPI{}, nil
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
		if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("key %q created state: %v", key, err)
		}
		if strings.Contains(stderr.String(), "ibk_") {
			t.Fatalf("stderr mentions a key: %s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "INSTABOX_API_KEY") {
			t.Fatalf("stderr = %s", stderr.String())
		}
	}
}

func TestExplicitKeyEnvMissingEmptyOrInvalidFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     string
		configure string
		wantEnv   string
	}{
		{name: "explicit missing", value: "", configure: "INSTABOX_S1_API_KEY", wantEnv: "INSTABOX_S1_API_KEY"},
		{name: "explicit empty", value: "   ", configure: "INSTABOX_S1_API_KEY", wantEnv: "INSTABOX_S1_API_KEY"},
		{name: "explicit invalid name", value: "ibk_unused", configure: "not a valid env name", wantEnv: "apiKeyEnv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			configPath := writeConfig(t, stateDir, true, true, machineEntry(testMachineA, "runner.example.net", tc.configure))
			calls := 0
			stderr := &bytes.Buffer{}
			code := run([]string{"--config", configPath, "--once"}, &bytes.Buffer{}, stderr, dependencies{
				getenv: func(name string) string {
					// A global key exists, but an explicit apiKeyEnv must never
					// silently fall back to it.
					if name == instaboxcontroller.DefaultAPIKeyEnv {
						return "ibk_global_value"
					}
					if name == tc.configure {
						return tc.value
					}
					return ""
				},
				newAPI: func(string) (instaboxcontroller.MachineAPI, error) {
					calls++
					return &fakeAPI{}, nil
				},
				newWriter: func() instaboxcontroller.DeadlineWriter {
					calls++
					return &fakeWriter{}
				},
			})
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr = %s", code, stderr.String())
			}
			if calls != 0 {
				t.Fatalf("created %d clients", calls)
			}
			if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state was created: %v", err)
			}
			if !strings.Contains(stderr.String(), tc.wantEnv) {
				t.Fatalf("stderr = %s, want env name %s", stderr.String(), tc.wantEnv)
			}
			if strings.Contains(stderr.String(), "ibk_") {
				t.Fatalf("stderr leaked a key value: %s", stderr.String())
			}
		})
	}
}

func TestCredentialErrorNamesTheEnvButNeverPrintsAKey(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeConfig(t, stateDir, false, false,
		machineEntry(testMachineA, "runner-a.example.net", "INSTABOX_S1_API_KEY"),
		machineEntry(testMachineB, "runner-b.example.net", "INSTABOX_S2_API_KEY"),
	)
	secretA := "distinctive-secret-value-for-s1"
	calls := 0
	stderr := &bytes.Buffer{}
	code := run([]string{"--config", configPath, "--once"}, &bytes.Buffer{}, stderr, dependencies{
		getenv: func(name string) string {
			switch name {
			case "INSTABOX_S1_API_KEY":
				return secretA
			case "INSTABOX_S2_API_KEY":
				return ""
			default:
				return ""
			}
		},
		newAPI: func(string) (instaboxcontroller.MachineAPI, error) {
			calls++
			return &fakeAPI{}, nil
		},
		newWriter: func() instaboxcontroller.DeadlineWriter {
			calls++
			return &fakeWriter{}
		},
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if calls != 0 {
		t.Fatalf("created %d clients", calls)
	}
	if !strings.Contains(stderr.String(), "INSTABOX_S2_API_KEY") {
		t.Fatalf("stderr does not name the missing variable: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), secretA) {
		t.Fatalf("stderr printed a key value: %s", stderr.String())
	}
}

func TestExplicitKeyEnvWorksWithoutGlobalKey(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeConfig(t, stateDir, false, false,
		machineEntry(testMachineA, "runner-a.example.net", "INSTABOX_S1_API_KEY"),
		machineEntry(testMachineB, "runner-b.example.net", "INSTABOX_S2_API_KEY"),
	)
	now := time.Now()
	apiA := &fakeAPI{key: "ibk_account_s1", machines: map[string]instaboxcontroller.Machine{testMachineA: runningMachine(testMachineA, now)}}
	apiB := &fakeAPI{key: "ibk_account_s2", machines: map[string]instaboxcontroller.Machine{testMachineB: runningMachine(testMachineB, now)}}
	created := map[string]string{}
	code := run([]string{"--config", configPath, "--once", "--dry-run"}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv: func(name string) string {
			switch name {
			case "INSTABOX_S1_API_KEY":
				return apiA.key
			case "INSTABOX_S2_API_KEY":
				return apiB.key
			default:
				return ""
			}
		},
		newAPI: func(key string) (instaboxcontroller.MachineAPI, error) {
			created[key] = key
			if key == apiA.key {
				return apiA, nil
			}
			return apiB, nil
		},
		newWriter: func() instaboxcontroller.DeadlineWriter { return &fakeWriter{} },
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(created) != 2 || created[apiA.key] == "" || created[apiB.key] == "" {
		t.Fatalf("clients = %v", created)
	}
	if apiA.gets != 1 || apiB.gets != 1 {
		t.Fatalf("gets A=%d B=%d", apiA.gets, apiB.gets)
	}
}

func TestLegacyGlobalKeyStillWorks(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeConfig(t, stateDir, false, false, machineEntry(testMachineA, "runner.example.net", nil))
	api := &fakeAPI{machines: map[string]instaboxcontroller.Machine{testMachineA: runningMachine(testMachineA, time.Now())}}
	usedKey := ""
	code := run([]string{"--config", configPath, "--once", "--dry-run"}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv: func(name string) string {
			if name == instaboxcontroller.DefaultAPIKeyEnv {
				return "ibk_legacy_global"
			}
			return ""
		},
		newAPI: func(key string) (instaboxcontroller.MachineAPI, error) {
			usedKey = key
			return api, nil
		},
		newWriter: func() instaboxcontroller.DeadlineWriter { return &fakeWriter{} },
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if usedKey != "ibk_legacy_global" {
		t.Fatalf("legacy key was not used: %q", usedKey)
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
	configPath := writeConfig(t, t.TempDir(), true, true, machineEntry(testMachineA, "runner.example.net", nil))
	writer := &fakeWriter{}
	api := &fakeAPI{machines: map[string]instaboxcontroller.Machine{testMachineA: runningMachine(testMachineA, time.Now())}}
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
	configPath := writeConfig(t, t.TempDir(), true, false, machineEntry(testMachineA, "runner.example.net", nil))
	writer := &fakeWriter{}
	api := &fakeAPI{machines: map[string]instaboxcontroller.Machine{testMachineA: runningMachine(testMachineA, time.Now())}}
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
	configPath := writeConfig(t, t.TempDir(), false, false, machineEntry(testMachineA, "runner.example.net", nil))
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

func TestSameKeyEnvForTwoMachinesSharesOneClient(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeConfig(t, stateDir, false, false,
		machineEntry(testMachineA, "runner-a.example.net", "INSTABOX_S1_API_KEY"),
		machineEntry(testMachineB, "runner-b.example.net", "INSTABOX_S1_API_KEY"),
	)
	now := time.Now()
	api := &fakeAPI{machines: map[string]instaboxcontroller.Machine{
		testMachineA: runningMachine(testMachineA, now),
		testMachineB: runningMachine(testMachineB, now),
	}}
	created := 0
	code := run([]string{"--config", configPath, "--once", "--dry-run"}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv: func(name string) string {
			if name == "INSTABOX_S1_API_KEY" {
				return "ibk_shared_account"
			}
			return ""
		},
		newAPI: func(string) (instaboxcontroller.MachineAPI, error) {
			created++
			return api, nil
		},
		newWriter: func() instaboxcontroller.DeadlineWriter { return &fakeWriter{} },
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if created != 1 {
		t.Fatalf("clients = %d, want one shared account client", created)
	}
	if api.gets != 2 {
		t.Fatalf("gets = %d", api.gets)
	}
}

func endpointPath(command string) (string, bool) {
	for _, marker := range []string{"cat -- '", "mktemp '"} {
		if index := strings.Index(command, marker); index >= 0 {
			rest := command[index+len(marker):]
			if end := strings.Index(rest, "'"); end >= 0 {
				path := rest[:end]
				if marker == "mktemp '" {
					path = filepath.Dir(path)
				}
				return path, true
			}
		}
	}
	return "", false
}

func endpointWriteBody(command string) string {
	index := strings.Index(command, "printf %s '")
	if index < 0 {
		return ""
	}
	rest := command[index+len("printf %s '"):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(rest[:end])
	if err != nil {
		return ""
	}
	return string(decoded)
}

func apiExecAnswer(store map[string]string) func(context.Context, string, string) (instaboxcontroller.ExecResult, error) {
	return func(ctx context.Context, id, command string) (instaboxcontroller.ExecResult, error) {
		path, ok := endpointPath(command)
		if !ok {
			return instaboxcontroller.ExecResult{}, errors.New("unexpected command")
		}
		if strings.HasPrefix(command, "cat -- ") {
			value, exists := store[path]
			if !exists {
				return instaboxcontroller.ExecResult{ExitCode: 1}, nil
			}
			return instaboxcontroller.ExecResult{Stdout: value, ExitCode: 0}, nil
		}
		store[path] = endpointWriteBody(command)
		return instaboxcontroller.ExecResult{Stdout: store[path], ExitCode: 0}, nil
	}
}

func TestAPIExecConfigUsesPerMachineClientAndNeedsNoSSH(t *testing.T) {
	sharedContentA := map[string]string{}
	sharedContentB := map[string]string{}
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeConfig(t, stateDir, true, false,
		map[string]any{
			"machineId":         testMachineA,
			"apiKeyEnv":         "INSTABOX_S1_API_KEY",
			"deliveryTransport": "api-exec",
			"deadlinePath":      "/data/gha-runner-tui/session-deadline.json",
		},
		map[string]any{
			"machineId":         testMachineB,
			"apiKeyEnv":         "INSTABOX_S2_API_KEY",
			"deliveryTransport": "api-exec",
			"deadlinePath":      "/data/gha-runner-tui/session-deadline.json",
		},
	)
	now := time.Now()
	apiA := &fakeAPI{key: "ibk_account_s1", machines: map[string]instaboxcontroller.Machine{testMachineA: runningMachine(testMachineA, now)}}
	apiA.execFunc = apiExecAnswer(sharedContentA)
	apiB := &fakeAPI{key: "ibk_account_s2", machines: map[string]instaboxcontroller.Machine{testMachineB: runningMachine(testMachineB, now)}}
	apiB.execFunc = apiExecAnswer(sharedContentB)
	created := map[string]int{}
	sshWriterCalls := 0
	code := run([]string{"--config", configPath, "--once"}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv: func(name string) string {
			switch name {
			case "INSTABOX_S1_API_KEY":
				return apiA.key
			case "INSTABOX_S2_API_KEY":
				return apiB.key
			default:
				return ""
			}
		},
		newAPI: func(key string) (instaboxcontroller.MachineAPI, error) {
			created[key]++
			if key == apiA.key {
				return apiA, nil
			}
			return apiB, nil
		},
		newWriter: func() instaboxcontroller.DeadlineWriter {
			sshWriterCalls++
			return &fakeWriter{}
		},
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(created) != 2 || created[apiA.key] != 1 || created[apiB.key] != 1 {
		t.Fatalf("clients = %v", created)
	}
	if apiA.gets != 2 || apiB.gets != 2 {
		t.Fatalf("GETs A=%d B=%d", apiA.gets, apiB.gets)
	}
	if apiA.execs != 1 || apiB.execs != 1 {
		t.Fatalf("execs A=%d B=%d, want one write each", apiA.execs, apiB.execs)
	}
	if len(sharedContentA) != 1 || len(sharedContentB) != 1 {
		t.Fatalf("remote content A=%v B=%v", sharedContentA, sharedContentB)
	}
	if sshWriterCalls != 1 {
		// The SSH writer is still constructed once at startup by the existing
		// dependency, but api-exec never calls it.
		t.Fatalf("writer construction calls = %d", sshWriterCalls)
	}
}

func TestAPIExecDryRunStillMakesNoExec(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	configPath := writeConfig(t, stateDir, true, false,
		map[string]any{
			"machineId":         testMachineA,
			"apiKeyEnv":         "INSTABOX_S1_API_KEY",
			"deliveryTransport": "api-exec",
			"deadlinePath":      "/data/gha-runner-tui/session-deadline.json",
		},
	)
	api := &fakeAPI{key: "ibk_account_s1", machines: map[string]instaboxcontroller.Machine{testMachineA: runningMachine(testMachineA, time.Now())}}
	code := run([]string{"--config", configPath, "--once", "--dry-run"}, &bytes.Buffer{}, &bytes.Buffer{}, dependencies{
		getenv: func(name string) string {
			if name == "INSTABOX_S1_API_KEY" {
				return api.key
			}
			return ""
		},
		newAPI:    func(string) (instaboxcontroller.MachineAPI, error) { return api, nil },
		newWriter: func() instaboxcontroller.DeadlineWriter { return &fakeWriter{} },
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if api.execs != 0 {
		t.Fatalf("dry run executed %d commands", api.execs)
	}
}
