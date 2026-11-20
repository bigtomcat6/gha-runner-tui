package loop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gha-runner-tui/internal/state"
)

const gibBytes = uint64(1) << 30

func writeHostPolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const dockerGatePolicy = `{"max_jobs":4,"docker_root_dir":"/data/docker","min_docker_free_bytes":4294967296,"docker_reclaim_below_bytes":8589934592}`

func TestHostPolicyDockerDiskValidation(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		wantErr      bool
	}{
		{"absent is legacy", `{"max_jobs":4}`, false},
		{"configured", dockerGatePolicy, false},
		{"threshold without dir", `{"max_jobs":4,"min_docker_free_bytes":1}`, true},
		{"reclaim without dir", `{"max_jobs":4,"docker_reclaim_below_bytes":1}`, true},
		{"relative dir", `{"max_jobs":4,"docker_root_dir":"data/docker"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Supervisor{HostPolicyPath: writeHostPolicy(t, tc.policy)}
			if _, err := s.hostCapacityForLaunch(); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

// dockerGateCycle runs one cycle with one queued job and no other holders.
// free returns the Docker root free bytes before and after any reclaim.
func dockerGateCycle(t *testing.T, policy string, stampAge time.Duration, before, after uint64) (cycleResult, error, []string, int, string) {
	t.Helper()
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	t.Cleanup(srv.Close)
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	now := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	var prunes []string
	runs := 0
	d := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", 0)}, logs: [][]byte{[]byte("safe")}}
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "builder", "image", "volume":
			prunes = append(prunes, strings.Join(args[2:], " "))
			return nil, nil
		case "run":
			runs++
			return []byte("c"), nil
		default:
			return d.Run(ctx, name, args...)
		}
	}))
	s.HostPolicyPath = writeHostPolicy(t, policy)
	s.Now = func() time.Time { return now }
	s.ReclaimStampPath = filepath.Join(t.TempDir(), "reclaim.stamp")
	if stampAge >= 0 {
		if err := os.WriteFile(s.ReclaimStampPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-stampAge)
		if err := os.Chtimes(s.ReclaimStampPath, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	s.Stderr = &stderr
	s.FreeBytes = func(path string) (uint64, error) {
		if path == "/" {
			return 100 * gibBytes, nil
		}
		if path != "/data/docker" {
			t.Errorf("free space checked on %q", path)
		}
		if len(prunes) > 0 {
			return after, nil
		}
		return before, nil
	}
	got, err := s.runCycle(context.Background(), p, settings)
	if runs == 0 && f.posts.Load() != 0 {
		t.Fatalf("refused launch still requested a registration token")
	}
	return got, err, prunes, runs, stderr.String()
}

func TestDockerSpaceGate(t *testing.T) {
	wantPrunes := []string{"builder prune --all --force", "image prune --force", "volume prune --force"}
	t.Run("plenty of space launches without pruning", func(t *testing.T) {
		_, err, prunes, runs, _ := dockerGateCycle(t, dockerGatePolicy, -1, 12*gibBytes, 12*gibBytes)
		if err != nil || runs != 1 || len(prunes) != 0 {
			t.Fatalf("err=%v runs=%d prunes=%v", err, runs, prunes)
		}
	})
	t.Run("reclaim restores space and launches", func(t *testing.T) {
		_, err, prunes, runs, log := dockerGateCycle(t, dockerGatePolicy, -1, 3*gibBytes, 6*gibBytes)
		if err != nil || runs != 1 || strings.Join(prunes, ";") != strings.Join(wantPrunes, ";") {
			t.Fatalf("err=%v runs=%d prunes=%v", err, runs, prunes)
		}
		if !strings.Contains(log, "docker cache reclaim: free 3.0GiB -> 6.0GiB") {
			t.Fatalf("reclaim not logged: %q", log)
		}
	})
	t.Run("still low after reclaim waits for space", func(t *testing.T) {
		_, err, prunes, runs, _ := dockerGateCycle(t, dockerGatePolicy, -1, 1*gibBytes, 2*gibBytes)
		if !errors.Is(err, errDockerSpace) || runs != 0 || len(prunes) != 3 {
			t.Fatalf("err=%v runs=%d prunes=%v", err, runs, prunes)
		}
	})
	t.Run("recent host-wide reclaim is not repeated", func(t *testing.T) {
		_, err, prunes, runs, _ := dockerGateCycle(t, dockerGatePolicy, time.Minute, 1*gibBytes, 9*gibBytes)
		if !errors.Is(err, errDockerSpace) || runs != 0 || len(prunes) != 0 {
			t.Fatalf("err=%v runs=%d prunes=%v", err, runs, prunes)
		}
	})
	t.Run("stale stamp allows reclaim", func(t *testing.T) {
		_, err, prunes, runs, _ := dockerGateCycle(t, dockerGatePolicy, time.Hour, 1*gibBytes, 9*gibBytes)
		if err != nil || runs != 1 || len(prunes) != 3 {
			t.Fatalf("err=%v runs=%d prunes=%v", err, runs, prunes)
		}
	})
	t.Run("gate without reclaim threshold never prunes", func(t *testing.T) {
		policy := `{"max_jobs":4,"docker_root_dir":"/data/docker","min_docker_free_bytes":4294967296}`
		_, err, prunes, runs, _ := dockerGateCycle(t, policy, -1, 1*gibBytes, 9*gibBytes)
		if !errors.Is(err, errDockerSpace) || runs != 0 || len(prunes) != 0 {
			t.Fatalf("err=%v runs=%d prunes=%v", err, runs, prunes)
		}
	})
	t.Run("no docker policy keeps legacy launch", func(t *testing.T) {
		_, err, prunes, runs, _ := dockerGateCycle(t, `{"max_jobs":4}`, -1, 0, 0)
		if err != nil || runs != 1 || len(prunes) != 0 {
			t.Fatalf("err=%v runs=%d prunes=%v", err, runs, prunes)
		}
	})
}

func TestRunDockerSpaceRefusalWaitsAtPollCadence(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps", "builder", "image", "volume":
			return nil, nil
		}
		t.Fatalf("unexpected Docker %v", args)
		return nil, nil
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.HostPolicyPath = writeHostPolicy(t, dockerGatePolicy)
	s.ReclaimStampPath = filepath.Join(t.TempDir(), "reclaim.stamp")
	s.Stderr = &bytes.Buffer{}
	s.FreeBytes = func(path string) (uint64, error) {
		if path == "/" {
			return 100 * gibBytes, nil
		}
		return gibBytes, nil
	}
	sleeps := 0
	s.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps++
		got, err := state.LoadLoopState(p.Loop.StateFile)
		if err != nil || got.State != state.LoopWaitingHost || got.LastError == nil || !strings.Contains(*got.LastError, "docker disk space below launch floor") {
			t.Fatalf("refusal state=%+v err=%v", got, err)
		}
		if d != 30*time.Second {
			t.Fatalf("refusal sleep=%s, want poll cadence", d)
		}
		cancel()
		return context.Canceled
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if sleeps != 1 || f.posts.Load() != 0 {
		t.Fatalf("sleeps=%d registrations=%d", sleeps, f.posts.Load())
	}
}
