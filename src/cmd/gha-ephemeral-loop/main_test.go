package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/buildinfo"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
	systemdpkg "gha-runner-tui/internal/systemd"
)

type runnerFunc func(context.Context, string, ...string) ([]byte, error)

func TestRunVersionBeforeProfileAndClients(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--version", "--config", "/not/read/profile.yaml"}} {
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != 0 || out.String() != buildinfo.Current() || stderr.Len() != 0 {
			t.Fatalf("code=%d out=%q stderr=%q", code, out.String(), stderr.String())
		}
	}
	for _, args := range [][]string{nil, {"--unknown"}, {"--config"}, {"--version", "extra"}} {
		if code := run(args, io.Discard, io.Discard); code != 2 {
			t.Fatalf("%v code=%d", args, code)
		}
	}
	if code := run([]string{"--version"}, loopFailWriter{}, io.Discard); code != 1 {
		t.Fatalf("writer code=%d", code)
	}
}

type loopFailWriter struct{}

func (loopFailWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type statusFake struct{}

func (statusFake) Status(context.Context, string) (systemdpkg.ServiceStatus, error) {
	return systemdpkg.ServiceStatus{Active: state.SystemdActive}, nil
}

func TestRunLoopFailedConfigurationWaitsForCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ephemeral bool
		watch     string
	}{
		{name: "non ephemeral", watch: "[]"},
		{name: "old malformed watch", ephemeral: true, watch: "[wrong]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "profile.yaml")
			statePath := filepath.Join(root, "state", "p.json")
			text := fmt.Sprintf("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\nrunner: {ephemeral: %t, watch_repositories: %s}\ndocker: {container_name_prefix: gha-p, access_mode: rootless}\ngithub: {token_env: TEST_LOOP_MAIN, env_file: '', token_file: ''}\nloop: {state_file: %s, log_dir: %s}\n", tc.ephemeral, tc.watch, statePath, filepath.Join(root, "logs"))
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadProfile(path); err != nil {
				t.Fatalf("read-only old profile rejected: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- runLoop(ctx, path, runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
					t.Error("Docker/credential runner invoked for failed config")
					return nil, nil
				}), doerFunc(func(*http.Request) (*http.Response, error) {
					t.Error("HTTP invoked for failed config")
					return nil, fmt.Errorf("unexpected HTTP")
				}))
				close(done)
			}()
			deadline, cancelDeadline := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelDeadline()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-deadline.Done():
				}
			}()
			// Only filesystem publication needs a real scheduler tick; service time is not slept.
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				got, err := state.LoadLoopState(statePath)
				if err == nil && got.State == state.LoopFailed && got.LastError != nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("returned instead of waiting: %v", err)
				case <-deadline.Done():
					t.Fatal("failed state not written")
				case <-ticker.C:
				}
			}
			select {
			case err := <-done:
				t.Fatalf("returned before cancel: %v", err)
			default:
			}
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(cfgPath, []byte("paths:\n  profiles_dir: "+root+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			dashboard, err := (app.Service{ConfigPath: cfgPath, Systemd: statusFake{}}).LoadDashboard(context.Background())
			if err != nil || len(dashboard.Profiles) != 1 || len(dashboard.ProfileErrors) != 0 || dashboard.Profiles[0].DisplayLoopState != state.LoopFailed {
				t.Fatalf("old YAML app/status read: dashboard=%+v err=%v", dashboard, err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || string(unchanged) != text {
				t.Fatal("app/status read changed profile")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-deadline.Done():
				t.Fatal("cancel did not exit")
			}
		})
	}
}

func TestRunLoopInjectedCancelledSignalContext(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "p.yaml")
	text := fmt.Sprintf("name: p\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\nrunner: {ephemeral: true}\ndocker: {container_name_prefix: gha-p}\nloop: {state_file: %s, log_dir: %s}\n", filepath.Join(root, "p.json"), root)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := runLoop(ctx, path, runnerFunc(func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if ctx.Err() != nil {
			t.Error("label check canceled")
		}
		if args[2] != "ps" {
			t.Errorf("unexpected docker %v", args)
		}
		return nil, nil
	}), doerFunc(func(*http.Request) (*http.Response, error) {
		t.Error("HTTP after cancellation")
		return nil, fmt.Errorf("unexpected")
	}))
	if err != nil || calls != 1 {
		t.Fatalf("err=%v label queries=%d", err, calls)
	}
}
