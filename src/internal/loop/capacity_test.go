package loop

import (
	"context"
	"errors"
	"fmt"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Catch the one-container gate, oversubscription, and holding the launch lock while waiting.
func TestHostCapacityAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, policy, holderState string
		holders, wantRuns         int
		wantErr                   bool
	}{
		{"room for fourth", `{"max_jobs":4}`, "running", 3, 1, false},
		{"root disk floor blocks launch", `{"max_jobs":4,"min_root_free_bytes":18446744073709551615}`, "running", 0, 0, true},
		{"fifth waits", `{"max_jobs":4}`, "running", 4, 0, false},
		{"created reserves slot", `{"max_jobs":4}`, "created", 4, 0, false},
		{"unknown blocks", `{"max_jobs":4}`, "mystery", 1, 0, false},
		{"invalid fails closed", `{"max_jobs":0}`, "running", 0, 0, true},
		{"malformed fails closed", `{`, "running", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &cycleHTTP{jobs: true}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			policy := filepath.Join(t.TempDir(), "host.json")
			os.WriteFile(policy, []byte(tc.policy), 0600)
			runs, unlocks := 0, 0
			d := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", 0)}, logs: [][]byte{[]byte("safe")}}
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				switch args[2] {
				case "ps":
					if strings.Contains(strings.Join(args, " "), "io.gha-runner-tui.profile=") {
						return nil, nil
					}
					var rows strings.Builder
					for i := 0; i < tc.holders; i++ {
						fmt.Fprintf(&rows, "c%d\tother%d\timage\t%s\tother%d\trunner%d\n", i, i, tc.holderState, i, i)
					}
					return []byte(rows.String()), nil
				case "run":
					if unlocks != 0 {
						t.Error("launch is outside atomic capacity check")
					}
					runs++
					return []byte("c"), nil
				default:
					if unlocks != 1 {
						t.Error("capacity lock held while waiting for job")
					}
					return d.Run(ctx, name, args...)
				}
			}))
			s.HostPolicyPath = policy
			s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
			got, err := s.runCycle(context.Background(), p, settings)
			if (err != nil) != tc.wantErr || runs != tc.wantRuns {
				t.Fatalf("result=%+v err=%v runs=%d", got, err, runs)
			}
			if !tc.wantErr && tc.wantRuns == 0 && got.state != state.LoopWaitingHost {
				t.Fatalf("result=%+v", got)
			}
			if unlocks != 1 {
				t.Fatalf("unlocks=%d", unlocks)
			}
		})
	}
}

// Five independent workers for the same repository contend through a real flock.
// Four must wait for job completion concurrently; the fifth must not launch.
func TestFourSameRepositoryWorkersShareHostCapacity(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"max_jobs":4}`), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	live := map[int]bool{}
	peak := 0
	waiting := make(chan int, 5)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	workers := make([]*Supervisor, 5)
	profiles := make([]config.Profile, 5)
	for i := 0; i < 5; i++ {
		p := loopProfile(t)
		p.Name = fmt.Sprintf("worker%d", i)
		p.Runner.NamePrefix = p.Name
		p.Docker.ContainerNamePrefix = "gha-" + p.Name
		profiles[i] = p
		var announced sync.Once
		workers[i] = cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
			switch args[2] {
			case "ps":
				if strings.Contains(strings.Join(args, " "), "io.gha-runner-tui.profile=") {
					return nil, nil
				}
				mu.Lock()
				defer mu.Unlock()
				var b strings.Builder
				for id := range live {
					fmt.Fprintf(&b, "c%d\tw%d\timage\trunning\tw%d\tr%d\n", id, id, id, id)
				}
				return []byte(b.String()), nil
			case "run":
				mu.Lock()
				live[i] = true
				if len(live) > peak {
					peak = len(live)
				}
				mu.Unlock()
				return []byte(fmt.Sprintf("c%d", i)), nil
			case "inspect":
				announced.Do(func() { waiting <- i })
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", 0), nil
			case "logs":
				return []byte("safe"), nil
			case "rm":
				mu.Lock()
				delete(live, i)
				mu.Unlock()
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected docker %v", args)
			}
		}))
		workers[i].HostPolicyPath = policy
		workers[i].TryLock = func() (func() error, bool, error) { return tryHostLock(filepath.Join(dir, "host.lock")) }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settings, _ := validateLoopProfile(profiles[0])
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func(i int) { _, err := workers[i].runCycle(ctx, profiles[i], settings); done <- err }(i)
		select {
		case <-waiting:
		case err := <-done:
			t.Fatalf("worker %d failed to wait: %v", i, err)
		case <-ctx.Done():
			t.Fatal("four jobs could not run concurrently")
		}
	}
	got, err := workers[4].runCycle(ctx, profiles[4], settings)
	if err != nil || got.state != state.LoopWaitingHost {
		t.Fatalf("fifth worker: %+v %v", got, err)
	}
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := workers[4].runCycle(ctx, profiles[4], settings); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 4 || len(live) != 0 || f.posts.Load() != 5 {
		t.Fatalf("peak=%d live=%d registrations=%d", peak, len(live), f.posts.Load())
	}
}

// Adoption must not monopolize the launch gate after a controller restart.
func TestConcurrentAdoptionReleasesHostGate(t *testing.T) {
	f := &cycleHTTP{}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	path := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(path, []byte(`{"max_jobs":4}`), 0600)
	unlocks, inspects := 0, 0
	d := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", 0)}, logs: [][]byte{[]byte("safe")}}
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return []byte("c\tgha-p-1\timage\trunning\tp\trunner\n"), nil
		case "inspect":
			inspects++
			if inspects == 1 {
				return runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z"), nil
			}
			if unlocks != 1 {
				t.Error("adoption blocks other host launches")
			}
		}
		return d.Run(ctx, name, args...)
	}))
	s.HostPolicyPath = path
	s.defaults()
	s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
	if err := s.adoptManaged(context.Background(), p, settings); err != nil {
		t.Fatal(err)
	}
	if unlocks != 1 {
		t.Fatalf("unlocks=%d", unlocks)
	}
}

func TestHostPolicyReadErrorFailsClosed(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{HostPolicyPath: filepath.Join(blocker, "host.json")}
	if capacity, err := s.hostCapacityForLaunch(); err == nil || capacity != 0 {
		t.Fatalf("launch policy read error was ignored: capacity=%d err=%v", capacity, err)
	}
	if capacity, err := s.hostCapacity(); err == nil || capacity != 0 {
		t.Fatalf("adoption policy read error was ignored: capacity=%d err=%v", capacity, err)
	}
}

func TestAmbiguousLaunchFailureKeepsHostGateUntilLifecycleResolves(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"max_jobs":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unlocks := 0
	d := &dockerScript{
		inspect: [][]byte{
			runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z"),
			containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", 0),
		},
		logs: [][]byte{[]byte("safe")},
	}
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			return nil, errors.New("ambiguous launch failure")
		case "inspect":
			if d.inspectN >= 1 && unlocks != 0 {
				t.Error("host gate released before the ambiguous launch was resolved")
			}
		}
		return d.Run(ctx, name, args...)
	}))
	s.HostPolicyPath = policy
	s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
	got, err := s.runCycle(context.Background(), p, settings)
	if err != nil || got.state != state.LoopSleeping {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if d.inspectN != 3 || d.rmN != 1 || unlocks != 1 || f.posts.Load() != 1 {
		t.Fatalf("inspect=%d rm=%d unlocks=%d registrations=%d", d.inspectN, d.rmN, unlocks, f.posts.Load())
	}
}

func TestStateWriteFailureAfterLaunchRetainsContainer(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"max_jobs":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unlocks := 0
	d := &dockerScript{}
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			if err := os.Remove(p.Loop.StateFile); err != nil {
				return nil, err
			}
			if err := os.Mkdir(p.Loop.StateFile, 0o755); err != nil {
				return nil, err
			}
			return []byte("c"), nil
		}
		return d.Run(ctx, name, args...)
	}))
	s.HostPolicyPath = policy
	s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
	_, err := s.runCycle(context.Background(), p, settings)
	if !errors.Is(err, errStateWrite) {
		t.Fatalf("err=%v", err)
	}
	if d.inspectN != 0 || d.rmN != 0 || unlocks != 1 || f.posts.Load() != 1 {
		t.Fatalf("state failure touched the launched container: inspect=%d rm=%d unlocks=%d registrations=%d", d.inspectN, d.rmN, unlocks, f.posts.Load())
	}
}
