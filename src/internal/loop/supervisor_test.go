package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/config"
	dockerpkg "gha-runner-tui/internal/docker"
	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
)

type fakeRegistrationTokenClient struct {
	repoToken string
	orgToken  string
	repoCalls []string
	orgCalls  []string
}

// Task 5 fakes sit below the real Docker/GitHub clients. All calls are local.
type cycleHTTP struct {
	jobs                bool
	runsCode, tokenCode int
	busy                bool
	posts, deletes      atomic.Int32
}

func cycleServer(t *testing.T, f *cycleHTTP) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/me/app/actions/runs":
			if len(q) != 2 || q.Get("per_page") != "100" || (q.Get("status") != "queued" && q.Get("status") != "in_progress") {
				t.Errorf("runs query: %s", r.URL)
			}
			if f.runsCode != 0 {
				w.WriteHeader(f.runsCode)
				return
			}
			if f.jobs {
				fmt.Fprint(w, `{"workflow_runs":[{"id":9}]}`)
			} else {
				fmt.Fprint(w, `{"workflow_runs":[]}`)
			}
		case r.Method == "GET" && r.URL.Path == "/repos/me/app/actions/runs/9/jobs":
			if len(q) != 2 || q.Get("filter") != "latest" || q.Get("per_page") != "100" {
				t.Errorf("jobs query: %s", r.URL)
			}
			fmt.Fprint(w, `{"jobs":[{"id":21,"status":"queued","labels":["self-hosted","linux"]},{"id":22,"status":"queued","labels":["linux"]}]}`)
		case r.Method == "POST" && r.URL.Path == "/repos/me/app/actions/runners/registration-token":
			if len(q) != 0 {
				t.Errorf("token query: %s", r.URL)
			}
			f.posts.Add(1)
			if f.tokenCode != 0 {
				w.WriteHeader(f.tokenCode)
				return
			}
			fmt.Fprint(w, `{"token":"fake-registration-token"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/me/app/actions/runners":
			if len(q) != 1 || q.Get("name") == "" {
				t.Errorf("runner query: %s", r.URL)
			}
			fmt.Fprintf(w, `{"runners":[{"id":7,"name":%q,"status":"online","busy":%t}]}`, q.Get("name"), f.busy)
		case r.Method == "DELETE" && r.URL.Path == "/repos/me/app/actions/runners/7":
			if len(q) != 0 {
				t.Errorf("delete query: %s", r.URL)
			}
			f.deletes.Add(1)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected HTTP %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
}

func cycleSupervisor(t *testing.T, srv *httptest.Server, r command.Runner) *Supervisor {
	t.Helper()
	t.Setenv("TEST_CYCLE_TOKEN", "fake-cycle-token")
	return &Supervisor{Docker: dockerpkg.NewClient(r), GitHub: gh.NewClient(srv.URL, "TEST_CYCLE_TOKEN", "", nil, srv.Client()),
		TryLock: func() (func() error, bool, error) { return func() error { return nil }, true, nil },
		Jitter:  func(d time.Duration) time.Duration { return d },
	}
}

func TestCycleNoJobsDoesNotTakeLock(t *testing.T) {
	f := &cycleHTTP{}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	settings, err := validateLoopProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	queries := 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		assertOwnCleanupQuery(t, args)
		queries++
		return nil, nil
	}))
	s.TryLock = func() (func() error, bool, error) { t.Fatal("lock called without jobs"); return nil, false, nil }
	s.restartCount = 4
	got, err := s.runCycle(context.Background(), p, settings)
	if err != nil || got.state != state.LoopSleeping || s.restartCount != 4 {
		t.Fatalf("cycle=%+v err=%v", got, err)
	}
	if queries != 1 || f.posts.Load() != 0 {
		t.Fatalf("cleanup queries=%d registrations=%d", queries, f.posts.Load())
	}
}

func assertOwnCleanupQuery(t *testing.T, args []string) {
	t.Helper()
	want := []string{"--host", "unix:///var/run/docker.sock", "ps", "--all", "--filter", "label=io.gha-runner-tui.managed=true", "--filter", "label=io.gha-runner-tui.profile=p", "--format", `{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Label "io.gha-runner-tui.profile"}}\t{{.Label "io.gha-runner-tui.runner"}}`}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("cleanup query=%v, want %v", args, want)
	}
}

func ownedInspectJSON(id, name, profile, managed, status string) []byte {
	return []byte(fmt.Sprintf(`[{"Id":%q,"Name":%q,"Created":"2026-10-07T00:00:00Z","Config":{"Labels":{"io.gha-runner-tui.managed":%q,"io.gha-runner-tui.profile":%q},"Env":["RUNNER_TOKEN=fake-token"]},"State":{"Status":%q,"StartedAt":"2026-10-07T00:00:01Z","ExitCode":0}}]`, id, "/"+name, managed, profile, status))
}

func TestCycleTerminalCleanupOwnershipAndReinspection(t *testing.T) {
	for _, tc := range []struct {
		name, listed, listedProfile, profile, managed, inspected string
		wantInspect, wantRemove                                  bool
	}{
		{"exited", "exited", "p", "p", "true", "exited", true, true},
		{"dead", "dead", "p", "p", "true", "dead", true, true},
		{"terminal changed", "exited", "p", "p", "true", "dead", true, true},
		{"unmanaged", "exited", "p", "p", "", "exited", true, false},
		{"managed false", "exited", "p", "p", "false", "exited", true, false},
		{"missing profile label", "exited", "p", "", "true", "exited", true, false},
		{"foreign reinspection", "exited", "p", "other", "true", "exited", true, false},
		{"same prefix sibling", "exited", "p-sibling", "p-sibling", "true", "exited", false, false},
		{"foreign listing", "dead", "other", "other", "true", "dead", false, false},
		{"unknown", "mystery", "p", "p", "true", "exited", false, false},
		{"created", "created", "p", "p", "true", "exited", false, false},
		{"running", "running", "p", "p", "true", "exited", false, false},
		{"paused", "paused", "p", "p", "true", "exited", false, false},
		{"restarting", "restarting", "p", "p", "true", "exited", false, false},
		{"removing", "removing", "p", "p", "true", "exited", false, false},
		{"now running", "exited", "p", "p", "true", "running", true, false},
		{"now created", "dead", "p", "p", "true", "created", true, false},
		{"now unknown", "exited", "p", "p", "true", "mystery", true, false},
		{"now paused", "exited", "p", "p", "true", "paused", true, false},
		{"now restarting", "exited", "p", "p", "true", "restarting", true, false},
		{"now removing", "exited", "p", "p", "true", "removing", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &cycleHTTP{}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			queries, inspects, removes := 0, 0, 0
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, _ string, args ...string) ([]byte, error) {
				obs := observeContext(ctx)
				if obs.err != nil || !obs.hasDeadline || obs.timeout > 10*time.Second {
					t.Fatalf("cleanup context=%+v", obs)
				}
				switch args[2] {
				case "ps":
					assertOwnCleanupQuery(t, args)
					queries++
					return []byte(fmt.Sprintf("short-id\tgha-p-reused-name\timage\t%s\t%s\tp-1\n", tc.listed, tc.listedProfile)), nil
				case "inspect":
					inspects++
					if args[3] != "short-id" {
						t.Fatalf("cleanup inspected reusable name instead of ID: %v", args)
					}
					return ownedInspectJSON("full-id", "different-name", tc.profile, tc.managed, tc.inspected), nil
				case "rm":
					removes++
					if !reflect.DeepEqual(args, []string{"--host", "unix:///var/run/docker.sock", "rm", "full-id"}) {
						t.Fatalf("cleanup must remove inspected ID nonforce: %v", args)
					}
					return nil, nil
				default:
					t.Fatalf("no-jobs cleanup must not log/run: %v", args)
					return nil, nil
				}
			}))
			s.TryLock = func() (func() error, bool, error) { t.Fatal("terminal cleanup took host lock"); return nil, false, nil }
			got, err := s.runCycle(context.Background(), p, settings)
			if err != nil || got.state != state.LoopSleeping || queries != 1 || inspects > 1 || removes > 1 || (inspects == 1) != tc.wantInspect || (removes == 1) != tc.wantRemove || f.posts.Load() != 0 {
				t.Fatalf("cycle=%+v err=%v queries=%d inspect=%d remove=%d posts=%d", got, err, queries, inspects, removes, f.posts.Load())
			}
		})
	}
}

func TestCycleTerminalCleanupFailurePreventsJobQueries(t *testing.T) {
	for _, failure := range []string{"list", "parse", "inspect", "inspect parse", "missing listed ID", "missing inspected ID", "remove race", "missing"} {
		t.Run(failure, func(t *testing.T) {
			srv := disallowGitHubServer(t)
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			removes := 0
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[2] {
				case "ps":
					assertOwnCleanupQuery(t, args)
					if failure == "list" {
						return nil, errors.New("list failed")
					}
					if failure == "parse" {
						return []byte("bad row"), nil
					}
					id := "c"
					if failure == "missing listed ID" {
						id = ""
					}
					return []byte(id + "\tgha-p-1\timage\texited\tp\tp-1\n"), nil
				case "inspect":
					if args[3] != "c" {
						t.Fatalf("inspect without candidate ID: %v", args)
					}
					if failure == "inspect" {
						return nil, errors.New("inspect failed")
					}
					if failure == "inspect parse" {
						return []byte("bad inspect"), nil
					}
					if failure == "missing" {
						return []byte("No such object"), errors.New("missing")
					}
					id := "c"
					if failure == "missing inspected ID" {
						id = ""
					}
					return ownedInspectJSON(id, "gha-p-1", "p", "true", "exited"), nil
				case "rm":
					removes++
					if len(args) != 4 || args[3] != "c" {
						t.Fatalf("race removal must not force or use name: %v", args)
					}
					return nil, errors.New("container is running")
				default:
					t.Fatalf("cleanup failure must not start runner: %v", args)
					return nil, nil
				}
			}))
			s.TryLock = func() (func() error, bool, error) { t.Fatal("cleanup failure took lock"); return nil, false, nil }
			if failure == "missing" {
				// Explicit disappearance allows the ordinary no-jobs poll.
				pollSrv := cycleServer(t, &cycleHTTP{})
				defer pollSrv.Close()
				s.GitHub = gh.NewClient(pollSrv.URL, "TEST_CYCLE_TOKEN", "", nil, pollSrv.Client())
			}
			_, err := s.runCycle(context.Background(), p, settings)
			if (err != nil) != (failure != "missing") || (removes == 1) != (failure == "remove race") {
				t.Fatalf("err=%v removes=%d", err, removes)
			}
		})
	}
}

func TestCycleRetainedNaturalExitCleanedNextCycle(t *testing.T) {
	srv := cycleServer(t, &cycleHTTP{jobs: true})
	defer srv.Close()
	noJobs := cycleServer(t, &cycleHTTP{})
	defer noJobs.Close()
	p := loopProfile(t)
	p.Docker.RemoveAfterExit = false // Also the zero value for a missing setting.
	settings, _ := validateLoopProfile(p)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	id, name := "", ""
	runs, removes, logs := 0, 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			if id == "" {
				return nil, nil
			}
			return []byte(fmt.Sprintf("%s\t%s\timage\texited\tp\tp-1\n", id, name)), nil
		case "run":
			if id != "" {
				t.Fatal("previous natural exit accumulated before next start")
			}
			runs++
			id, name = fmt.Sprintf("id-%d", runs), args[6]
			return []byte(id), nil
		case "inspect":
			if args[3] != id && args[3] != name {
				t.Fatalf("inspect target=%s, id=%s name=%s", args[3], id, name)
			}
			return ownedInspectJSON(id, name, "p", "true", "exited"), nil
		case "logs":
			logs++
			return []byte("safe fake-token"), nil
		case "rm":
			if len(args) != 4 || args[3] != id || logs != runs {
				t.Fatalf("next-cycle removal=%v logs=%d runs=%d", args, logs, runs)
			}
			removes++
			id, name = "", ""
			return nil, nil
		default:
			t.Fatalf("unexpected Docker %v", args)
			return nil, nil
		}
	}))
	s.Now = func() time.Time { return now }
	for cycle := 1; cycle <= 3; cycle++ {
		if _, err := s.runCycle(context.Background(), p, settings); err != nil {
			t.Fatal(err)
		}
		if id == "" || removes != cycle-1 || logs != cycle {
			t.Fatalf("current exit must retain: cycle=%d id=%q removes=%d logs=%d", cycle, id, removes, logs)
		}
		now = now.Add(time.Minute)
	}
	s.GitHub = gh.NewClient(noJobs.URL, "TEST_CYCLE_TOKEN", "", nil, noJobs.Client())
	s.TryLock = func() (func() error, bool, error) { t.Fatal("empty cycle took lock"); return nil, false, nil }
	for cycle := 4; cycle <= 5; cycle++ {
		if _, err := s.runCycle(context.Background(), p, settings); err != nil {
			t.Fatal(err)
		}
	}
	if id != "" || runs != 3 || removes != 3 || logs != 3 {
		t.Fatalf("retained containers accumulated: id=%q runs=%d removes=%d logs=%d", id, runs, removes, logs)
	}
	content := logDirContents(t, p.Loop.LogDir)
	if strings.Contains(content, "fake-token") || !strings.Contains(content, "safe [REDACTED]") {
		t.Fatalf("current-cycle logs not protected: %q", content)
	}
}

func TestCycleTerminalCleanupCancellationAndEmptyProfile(t *testing.T) {
	for _, when := range []string{"before cycle", "during list", "during inspect", "budget exhausted", "empty profile"} {
		t.Run(when, func(t *testing.T) {
			srv := disallowGitHubServer(t)
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
			if when == "before cycle" {
				cancel()
			}
			if when == "empty profile" {
				p.Name = ""
			}
			queries, inspects, removes := 0, 0, 0
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(callCtx context.Context, _ string, args ...string) ([]byte, error) {
				if when == "before cycle" || when == "empty profile" {
					t.Fatalf("invalid/cancelled cycle touched Docker: %v", args)
				}
				obs := observeContext(callCtx)
				if obs.err != nil || !obs.hasDeadline || obs.timeout > 10*time.Second {
					t.Fatalf("cleanup subcall not independently bounded: %+v", obs)
				}
				switch args[2] {
				case "ps":
					assertOwnCleanupQuery(t, args)
					queries++
					if when == "during list" || when == "budget exhausted" {
						cancel()
					}
					return []byte("c\tgha-p-1\timage\texited\tp\tp-1\n"), nil
				case "inspect":
					inspects++
					if args[3] != "c" {
						t.Fatalf("cleanup inspected name: %v", args)
					}
					if when == "during inspect" {
						cancel()
					}
					if when == "budget exhausted" {
						clock.now = clock.now.Add(60 * time.Second)
					}
					return ownedInspectJSON("c", "gha-p-1", "p", "true", "exited"), nil
				case "rm":
					removes++
					if len(args) != 4 || args[3] != "c" {
						t.Fatalf("cancellation cannot force terminal removal: %v", args)
					}
					return nil, nil
				default:
					t.Fatalf("cancelled cleanup must not log/run: %v", args)
					return nil, nil
				}
			}))
			s.Now = clock.Now
			s.Stderr = &bytes.Buffer{}
			s.TryLock = func() (func() error, bool, error) { t.Fatal("cancelled cleanup took lock"); return nil, false, nil }
			_, err := s.runCycle(ctx, p, settings)
			if err == nil || (when != "empty profile" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("cycle error=%v", err)
			}
			wantQueries, wantRemoves := 1, 1
			if when == "before cycle" || when == "empty profile" {
				wantQueries, wantRemoves = 0, 0
			} else if when == "budget exhausted" {
				wantRemoves = 0
			}
			if queries != wantQueries || inspects != wantQueries || removes != wantRemoves {
				t.Fatalf("queries=%d inspects=%d removes=%d", queries, inspects, removes)
			}
		})
	}
}

func TestRunCycleTerminalCleanupErrorsUseBackoff(t *testing.T) {
	srv := cycleServer(t, &cycleHTTP{})
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queries := 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		assertOwnCleanupQuery(t, args)
		queries++
		if queries == 2 || queries == 3 { // Startup succeeds; subsequent cycles fail.
			return nil, errors.New("cleanup unavailable")
		}
		return nil, nil
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.TryLock = func() (func() error, bool, error) { t.Fatal("cleanup/no-jobs took lock"); return nil, false, nil }
	var sleeps []time.Duration
	var statuses []state.LoopStatus
	s.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		got, err := state.LoadLoopState(p.Loop.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, got.State)
		if len(sleeps) == 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if queries != 5 || !reflect.DeepEqual(sleeps, []time.Duration{time.Second, 2 * time.Second, 30 * time.Second}) || !reflect.DeepEqual(statuses, []state.LoopStatus{state.LoopBackoff, state.LoopBackoff, state.LoopSleeping}) {
		t.Fatalf("queries=%d sleeps=%v states=%v", queries, sleeps, statuses)
	}
}

func TestStartupTerminalRestartStillUsesLiveAdoptionLock(t *testing.T) {
	srv := disallowGitHubServer(t)
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	inspects, locks, unlocks, logs, removes := 0, 0, 0, 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			assertOwnCleanupQuery(t, args)
			return []byte("c\tgha-p-1\timage\texited\tp\tp-1\n"), nil
		case "inspect":
			inspects++
			status := "running"
			if inspects == 1 {
				if args[3] != "c" || locks != 0 {
					t.Fatal("terminal reinspection must be by ID without lock")
				}
			} else {
				if args[3] != "gha-p-1" || locks-unlocks != 1 {
					t.Fatal("live adoption lost its lock/target")
				}
				if inspects >= 3 {
					status = "exited"
				}
			}
			return ownedInspectJSON("c", "gha-p-1", "p", "true", status), nil
		case "logs":
			logs++
			return []byte("safe log"), nil
		case "rm":
			removes++
			if len(args) != 4 || args[3] != "gha-p-1" || locks-unlocks != 1 || logs != 1 {
				t.Fatalf("live finish policy/lock changed: %v", args)
			}
			return nil, nil
		default:
			t.Fatalf("unexpected adoption Docker %v", args)
			return nil, nil
		}
	}))
	s.TryLock = func() (func() error, bool, error) { locks++; return func() error { unlocks++; return nil }, true, nil }
	s.restartCount = 4
	if err := s.adoptManaged(context.Background(), p, settings); err != nil {
		t.Fatal(err)
	}
	got, err := state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.RestartCount != 4 || s.restartCount != 4 {
		t.Fatalf("adoption changed attempt count: %+v err=%v count=%d", got, err, s.restartCount)
	}
	if locks != 1 || unlocks != 1 || inspects != 4 || logs != 1 || removes != 1 {
		t.Fatalf("locks=%d unlocks=%d inspect=%d logs=%d removes=%d", locks, unlocks, inspects, logs, removes)
	}
}

func TestCycleGateFailuresReleaseLock(t *testing.T) {
	for _, tc := range []struct {
		name, holder                 string
		lockBusy, lockError, psError bool
		runsCode, tokenCode          int
		want                         state.LoopStatus
		wantErr, acquired            bool
	}{
		{name: "lock busy", lockBusy: true, want: state.LoopWaitingHost},
		{name: "lock open error", lockError: true, wantErr: true},
		{name: "candidate forbidden", runsCode: 403, wantErr: true},
		{name: "ps error", psError: true, wantErr: true, acquired: true},
		{name: "registration error", tokenCode: 403, wantErr: true, acquired: true},
		{name: "running holder", holder: "running", want: state.LoopWaitingHost, acquired: true},
		{name: "created holder", holder: "created", want: state.LoopWaitingHost, acquired: true},
		{name: "restarting holder", holder: "restarting", want: state.LoopWaitingHost, acquired: true},
		{name: "paused holder", holder: "paused", want: state.LoopWaitingHost, acquired: true},
		{name: "unknown holder", holder: "mystery", want: state.LoopWaitingHost, acquired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &cycleHTTP{jobs: true, runsCode: tc.runsCode, tokenCode: tc.tokenCode}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			runs, locks, unlocks := 0, 0, 0
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[2] == "run" {
					runs++
					t.Error("must not run")
					return nil, nil
				}
				if args[2] != "ps" {
					t.Fatalf("unexpected docker %v", args)
				}
				if strings.Contains(strings.Join(args, " "), "label=io.gha-runner-tui.profile=p") {
					assertOwnCleanupQuery(t, args)
					return nil, nil // This test exercises the locked, host-wide slot query.
				}
				if tc.psError {
					return nil, errors.New("ps failed")
				}
				if tc.holder != "" {
					return []byte("c\tother\timage\t" + tc.holder + "\tother\trunner\n"), nil
				}
				return nil, nil
			}))
			s.TryLock = func() (func() error, bool, error) {
				locks++
				if tc.lockError {
					return nil, false, errors.New("open failed")
				}
				if tc.lockBusy {
					return nil, false, nil
				}
				return func() error { unlocks++; return nil }, true, nil
			}
			s.restartCount = 4
			got, err := s.runCycle(context.Background(), p, settings)
			if s.restartCount != 4 {
				t.Fatalf("pre-run gate changed attempt count: %d", s.restartCount)
			}
			if (err != nil) != tc.wantErr || (!tc.wantErr && got.state != tc.want) {
				t.Fatalf("cycle=%+v err=%v", got, err)
			}
			wantUnlock := 0
			if tc.acquired {
				wantUnlock = 1
			}
			if runs != 0 || unlocks != wantUnlock || (tc.runsCode != 0 && locks != 0) {
				t.Fatalf("runs=%d locks=%d unlocks=%d", runs, locks, unlocks)
			}
			if tc.tokenCode == 0 && f.posts.Load() != 0 {
				t.Fatalf("registered despite gate: %d", f.posts.Load())
			}
		})
	}
}

func TestCycleFinishAndRunErrorAdoption(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		exit                                int
		runErr, missing, unknownFirst, idle bool
		wantErr                             bool
	}{
		{name: "normal zero"}, {name: "nonzero", exit: 23, wantErr: true},
		{name: "run error existing", runErr: true}, {name: "run error missing", runErr: true, missing: true, wantErr: true},
		{name: "run error unknown then exists", runErr: true, unknownFirst: true}, {name: "idle 204", idle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &cycleHTTP{jobs: true}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			clock := newTestClock(t, time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC))
			clock.limit = 20
			d := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", tc.exit)}, logs: [][]byte{[]byte("safe log")}}
			if tc.idle {
				d.inspect = [][]byte{runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z")}
			}
			if tc.missing {
				d.inspect = [][]byte{[]byte("No such object")}
				d.inspectErrs = []error{errors.New("missing")}
			}
			if tc.unknownFirst {
				d.inspect = append([][]byte{[]byte("bad")}, d.inspect...)
				d.inspectErrs = []error{errors.New("daemon down"), nil}
			}
			runs, unlocks := 0, 0
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				switch args[2] {
				case "ps":
					return nil, nil
				case "run":
					runs++
					if unlocks != 0 {
						t.Error("unlocked before run")
					}
					joined := strings.Join(args, " ")
					for _, label := range []string{"--sig-proxy=false", "io.gha-runner-tui.managed=true", "io.gha-runner-tui.profile=p", "io.gha-runner-tui.runner=p-20261007-000500"} {
						if !strings.Contains(joined, label) {
							t.Errorf("missing label/flag %s", label)
						}
					}
					if tc.runErr {
						return nil, errors.New("run timeout")
					}
					return []byte("c"), nil
				default:
					if unlocks != 0 {
						t.Error("released lock before finish")
					}
					return d.Run(ctx, name, args...)
				}
			}))
			s.Now, s.Sleep = clock.Now, clock.Sleep
			s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
			s.restartCount = 2
			got, err := s.runCycle(context.Background(), p, settings)
			if (err != nil) != tc.wantErr || runs != 1 || unlocks != 1 {
				t.Fatalf("cycle=%+v err=%v runs=%d unlocks=%d", got, err, runs, unlocks)
			}
			if !tc.wantErr && got.state != state.LoopSleeping {
				t.Fatalf("state=%s", got.state)
			}
			if !tc.missing && (d.logsN != 1 || d.rmN != 1 || d.rmForce[0] != tc.idle) {
				t.Fatalf("finish logs=%d rm=%v", d.logsN, d.rmForce)
			}
			if tc.idle && (f.deletes.Load() != 1 || len(s.skippedJobs) != 2) {
				t.Fatalf("idle deletes=%d skip=%v", f.deletes.Load(), s.skippedJobs)
			}
			if err := s.writeCycle(p, got, err); err != nil {
				t.Fatal(err)
			}
			published, err := state.LoadLoopState(p.Loop.StateFile)
			if err != nil || s.restartCount != 3 || published.RestartCount != 3 {
				t.Fatalf("actual run must count exactly once, even on failure/uncertainty: %+v count=%d err=%v", published, s.restartCount, err)
			}
		})
	}
}

func TestCycleSkipTTLAndFreshSupervisor(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	runs := 0
	d := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", now.Format(time.RFC3339), now.Format(time.RFC3339), 0)}}
	r := loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			runs++
			return []byte("c"), nil
		default:
			return d.Run(ctx, name, args...)
		}
	})
	s := cycleSupervisor(t, srv, r)
	s.Now = func() time.Time { return now }
	s.restartCount = 4
	s.skippedJobs = map[int64]time.Time{21: now.Add(30 * time.Minute), 22: now.Add(30 * time.Minute)}
	now = now.Add(29*time.Minute + 59*time.Second)
	got, err := s.runCycle(context.Background(), p, settings)
	if err != nil || got.state != state.LoopSleeping || runs != 0 || s.restartCount != 4 {
		t.Fatalf("before TTL=%+v err=%v runs=%d", got, err, runs)
	}
	fresh := cycleSupervisor(t, srv, r)
	fresh.Now = s.Now
	if _, err = fresh.runCycle(context.Background(), p, settings); err != nil || runs != 1 || fresh.restartCount != 1 || s.restartCount != 4 {
		t.Fatalf("fresh err=%v runs=%d", err, runs)
	}
	now = now.Add(time.Second)
	if _, err = s.runCycle(context.Background(), p, settings); err != nil || runs != 2 || len(s.skippedJobs) != 0 || s.restartCount != 5 {
		t.Fatalf("expired err=%v runs=%d skip=%v", err, runs, s.skippedJobs)
	}
}

func TestSupervisorsShareRealHostLock(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	lockPath := filepath.Join(t.TempDir(), "lock", "host.lock")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseA := func() { once.Do(func() { close(release) }) }
	defer releaseA()
	var mu sync.Mutex
	live, maxLive, runs := 0, 0, 0
	aProfile, bProfile := loopProfile(t), loopProfile(t)
	aProfile.Name = "a"
	bProfile.Name = "b"
	aProfile.Runner.NamePrefix, aProfile.Docker.ContainerNamePrefix = "a", "gha-a"
	bProfile.Runner.NamePrefix, bProfile.Docker.ContainerNamePrefix = "b", "gha-b"
	makeRunner := func(block bool) command.Runner {
		return loopRunnerFunc(func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			switch args[2] {
			case "ps":
				return nil, nil
			case "run":
				mu.Lock()
				live++
				runs++
				if live > maxLive {
					maxLive = live
				}
				mu.Unlock()
				if block {
					close(entered)
				}
				return []byte("c"), nil
			case "inspect":
				if block {
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", 0), nil
			case "logs":
				return []byte("safe"), nil
			case "rm":
				mu.Lock()
				live--
				mu.Unlock()
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected docker %v", args)
			}
		})
	}
	a, b := cycleSupervisor(t, srv, makeRunner(true)), cycleSupervisor(t, srv, makeRunner(false))
	a.TryLock = func() (func() error, bool, error) { return tryHostLock(lockPath) }
	b.TryLock = a.TryLock
	settings, _ := validateLoopProfile(aProfile)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.runCycle(ctx, aProfile, settings); done <- err; close(done) }()
	defer func() {
		releaseA()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("A never entered run")
	}
	got, err := b.runCycle(ctx, bProfile, settings)
	if err != nil || got.state != state.LoopWaitingHost || f.posts.Load() != 1 {
		t.Fatalf("B while A: %+v err=%v posts=%d", got, err, f.posts.Load())
	}
	releaseA()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("A never finished")
	}
	// Existing too-permissive files and restrictive directories are repaired
	// without replacing the lock inode before the second Supervisor takes it.
	if err := os.Chmod(lockPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = b.runCycle(ctx, bProfile, settings); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runs != 2 || maxLive != 1 || live != 0 {
		t.Fatalf("runs=%d maxLive=%d live=%d", runs, maxLive, live)
	}
	for path, mode := range map[string]os.FileMode{lockPath: 0o600, filepath.Dir(lockPath): 0o755} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode %s: %v %v", path, info, err)
		}
	}
}

func (f *fakeRegistrationTokenClient) CreateRegistrationToken(_ context.Context, owner, repo string) (string, error) {
	f.repoCalls = append(f.repoCalls, owner+"/"+repo)
	return f.repoToken, nil
}

func (f *fakeRegistrationTokenClient) CreateOrgRegistrationToken(_ context.Context, org string) (string, error) {
	f.orgCalls = append(f.orgCalls, org)
	return f.orgToken, nil
}

func writeLoopProfile(t *testing.T, p config.Profile) string {
	t.Helper()
	watch, _ := json.Marshal(p.Runner.WatchRepositories)
	path := filepath.Join(t.TempDir(), "profile.yaml")
	text := fmt.Sprintf("name: %s\nrepo: {owner: me, name: app}\nservice: {name: gha-p.service}\nrunner:\n  ephemeral: %t\n  name_prefix: p\n  labels: [linux]\n  watch_repositories: %s\ndocker:\n  image: runner:test\n  container_name_prefix: gha-p\n  remove_after_exit: true\nloop:\n  state_file: %s\n  log_dir: %s\n  backoff_seconds: 1\n  max_backoff_seconds: 4\n  interval_seconds: 999\n", p.Name, p.Runner.Ephemeral, watch, p.Loop.StateFile, p.Loop.LogDir)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunStartupAdoptsBeforePolling(t *testing.T) {
	for _, status := range []string{"running", "created", "restarting", "paused", "mystery"} {
		t.Run(status, func(t *testing.T) {
			f := &cycleHTTP{}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
			clock := newTestClock(t, start)
			clock.limit = 20
			d := &dockerScript{inspect: [][]byte{containerInspectJSON(status, start.Format(time.RFC3339), start.Format(time.RFC3339), 0), containerInspectJSON("exited", start.Format(time.RFC3339), start.Format(time.RFC3339), 0)}}
			ps, runs, locks := 0, 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if args[2] == "ps" {
					ps++
					if !strings.Contains(strings.Join(args, " "), "label=io.gha-runner-tui.profile=p") {
						t.Error("startup must query profile labels")
					}
					if ps > 1 {
						return nil, nil
					}
					return []byte("c\tgha-p-1\timage\t" + status + "\tp\tp-1\n"), nil
				}
				if args[2] == "run" {
					runs++
					return nil, errors.New("unexpected run")
				}
				return d.Run(ctx, name, args...)
			}))
			s.ProfilePath = writeLoopProfile(t, p)
			s.Now = clock.Now
			s.Sleep = func(ctx context.Context, duration time.Duration) error {
				if duration == 30*time.Second {
					if d.rmN != 1 {
						t.Fatal("polled before adoption finished")
					}
					cancel()
					return ctx.Err()
				}
				return clock.Sleep(ctx, duration)
			}
			s.TryLock = func() (func() error, bool, error) {
				locks++
				if locks == 1 {
					return nil, false, nil
				}
				return func() error { return nil }, true, nil
			}
			// The first lock retry is a poll, not a new jobs/run cycle.
			firstSleep := true
			underlying := s.Sleep
			s.Sleep = func(ctx context.Context, d time.Duration) error {
				if firstSleep {
					firstSleep = false
					if d != 30*time.Second || runs != 0 {
						t.Fatalf("lock retry: d=%s runs=%d", d, runs)
					}
					return clock.Sleep(ctx, d)
				}
				return underlying(ctx, d)
			}
			if err := s.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if runs != 0 || locks != 2 || d.rmN != 1 || d.rmForce[0] || f.posts.Load() != 0 {
				t.Fatalf("runs=%d locks=%d remove=%v posts=%d", runs, locks, d.rmForce, f.posts.Load())
			}
		})
	}
}

func TestRunStartupCleansOnlyOwnTerminalContainers(t *testing.T) {
	srv := cycleServer(t, &cycleHTTP{})
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removed := []string{}
	ps := 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			ps++
			if ps == 1 {
				return []byte("c\town-exited\timage\texited\tp\tp-1\nd\town-dead\timage\tdead\tp\tp-2\nf\tforeign\timage\texited\tother\tother-1\n"), nil
			}
			return nil, nil
		case "inspect":
			status := "exited"
			if args[3] == "d" {
				status = "dead"
			} else if args[3] != "c" {
				t.Fatalf("startup cleanup must inspect ID: %v", args)
			}
			return ownedInspectJSON(args[3], "reused-name", "p", "true", status), nil
		case "rm":
			if len(args) != 4 {
				t.Fatal("terminal cleanup must be nonforced")
			}
			removed = append(removed, args[3])
			return nil, nil
		default:
			t.Fatalf("unexpected Docker %v", args)
			return nil, nil
		}
	}))
	s.TryLock = func() (func() error, bool, error) {
		t.Fatal("startup terminal cleanup took lock")
		return nil, false, nil
	}
	s.ProfilePath = writeLoopProfile(t, p)
	s.Sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"c", "d"}) {
		t.Fatalf("removed=%v", removed)
	}
}

func TestRunStartupAlreadyExitedAdoptionFinishesBeforeBackoff(t *testing.T) {
	srv := disallowGitHubServer(t)
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z", 19)}, logs: [][]byte{[]byte("adopted log")}}
	ps, locks, unlocks := 0, 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[2] == "ps" {
			ps++
			if ps == 1 {
				return []byte("c\tgha-p-1\timage\trunning\tp\tp-1\n"), nil
			}
			return nil, nil
		}
		if unlocks != 0 {
			t.Error("lock released before finishing adoption")
		}
		return d.Run(ctx, name, args...)
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.TryLock = func() (func() error, bool, error) { locks++; return func() error { unlocks++; return nil }, true, nil }
	s.Sleep = func(_ context.Context, duration time.Duration) error {
		got, err := state.LoadLoopState(p.Loop.StateFile)
		if err != nil || got.State != state.LoopBackoff || got.LastContainerName != "gha-p-1" || got.LastExitCode == nil || *got.LastExitCode != 19 || duration != time.Second || d.logsN != 1 || d.rmN != 1 || d.rmForce[0] {
			t.Fatalf("adopted exit: state=%+v err=%v sleep=%s logs=%d rm=%v", got, err, duration, d.logsN, d.rmForce)
		}
		cancel()
		return context.Canceled
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if locks != 1 || unlocks != 1 {
		t.Fatalf("locks=%d unlocks=%d", locks, unlocks)
	}
}

func TestRunBackoffStartupRetryAndPollReset(t *testing.T) {
	f := &cycleHTTP{}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ps := 0
	var sleeps []time.Duration
	var statuses []state.LoopStatus
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] != "ps" {
			t.Fatalf("unexpected Docker %v", args)
		}
		ps++
		if ps <= 4 {
			return nil, errors.New("daemon down")
		}
		return nil, nil
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	jitterCalls := 0
	s.Jitter = func(d time.Duration) time.Duration { jitterCalls++; return d * 8 / 10 }
	s.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		got, err := state.LoadLoopState(p.Loop.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, got.State)
		if len(sleeps) == 6 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 24 * time.Second, 24 * time.Second}) {
		t.Fatalf("sleeps=%v", sleeps)
	}
	if !reflect.DeepEqual(statuses, []state.LoopStatus{state.LoopBackoff, state.LoopBackoff, state.LoopBackoff, state.LoopBackoff, state.LoopSleeping, state.LoopSleeping}) {
		t.Fatalf("states=%v", statuses)
	}
	if jitterCalls != 2 {
		t.Fatalf("jitter called for backoff: calls=%d, want only the two polls", jitterCalls)
	}
}

func TestRunStopDuringDetachedRunIsIndependent(t *testing.T) {
	for _, runError := range []bool{false, true} {
		t.Run(fmt.Sprint(runError), func(t *testing.T) {
			f := &cycleHTTP{jobs: true, busy: true}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
			clock.limit = 15
			entered, allowReturn := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(allowReturn) }) }
			defer release()
			var runs, removes atomic.Int32
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(runCtx context.Context, _ string, args ...string) ([]byte, error) {
				switch args[2] {
				case "ps":
					return nil, nil
				case "run":
					runs.Add(1)
					close(entered)
					cancel()
					obs := observeContext(runCtx)
					if obs.err != nil || !obs.hasDeadline || obs.timeout < 119*time.Second || obs.timeout > 120*time.Second {
						t.Errorf("run ctx=%+v", obs)
					}
					select {
					case <-allowReturn:
					case <-runCtx.Done():
						return nil, runCtx.Err()
					}
					if runError {
						return nil, context.DeadlineExceeded
					}
					return []byte("c"), nil
				case "inspect":
					if runError {
						return containerInspectJSON("created", "2026-10-07T01:00:00Z", "0001-01-01T00:00:00Z", 0), nil
					}
					return runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:00Z"), nil
				case "logs":
					return nil, nil
				case "rm":
					removes.Add(1)
					return nil, nil
				default:
					return nil, fmt.Errorf("unexpected docker %v", args)
				}
			}))
			s.ProfilePath = writeLoopProfile(t, p)
			s.Now, s.Sleep = clock.Now, clock.Sleep
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx); close(done) }()
			deadline, cancelDeadline := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelDeadline()
			defer func() {
				release()
				select {
				case <-done:
				case <-deadline.Done():
				}
			}()
			select {
			case <-entered:
			case <-deadline.Done():
				t.Fatal("run not entered")
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-deadline.Done():
				t.Fatal("drain did not finish")
			}
			published, err := state.LoadLoopState(p.Loop.StateFile)
			if err != nil || published.RestartCount != 1 || s.restartCount != 1 {
				t.Fatalf("cancellation/drain changed attempts: %+v count=%d err=%v", published, s.restartCount, err)
			}
			if runs.Load() != 1 || f.posts.Load() != 1 || removes.Load() != 0 || clock.now.Sub(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)) != 60*time.Second {
				t.Fatalf("runs=%d posts=%d removes=%d clock=%s", runs.Load(), f.posts.Load(), removes.Load(), clock.now)
			}
		})
	}
}

func TestC2CancelledFailedRunMissingContainerPublishesAttempt(t *testing.T) {
	for _, mode := range []string{"current diagnostic", "cleared diagnostic", "post-call write failure"} {
		t.Run(mode, func(t *testing.T) {
			srv := cycleServer(t, &cycleHTTP{jobs: true})
			defer srv.Close()
			p := loopProfile(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
			runs, inspects, managedLists, unlocks := 0, 0, 0, 0
			var before []byte
			var beforeInfo os.FileInfo
			var s *Supervisor
			s = cycleSupervisor(t, srv, loopRunnerFunc(func(callCtx context.Context, _ string, args ...string) ([]byte, error) {
				switch args[2] {
				case "ps":
					if strings.Contains(strings.Join(args, " "), "label=io.gha-runner-tui.profile=p") {
						managedLists++
					}
					return nil, nil // Startup, cycle cleanup, slot, and stop adoption are empty.
				case "run":
					runs++
					got, err := state.LoadLoopState(p.Loop.StateFile)
					if err != nil || got.State != state.LoopStarting || got.RestartCount != 0 || s.restartCount != 1 {
						t.Fatalf("attempt was published before actual call: %+v count=%d err=%v", got, s.restartCount, err)
					}
					cancel() // The independent command context must still be usable.
					if callCtx.Err() != nil {
						t.Fatal("run inherited parent cancellation")
					}
					now = now.Add(time.Hour)
					// Simulate a current disk diagnostic, or its subsequent external
					// clear. The post-call delta must not overwrite these or unknown keys.
					s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "current safe diagnostic")
					data, err := os.ReadFile(p.Loop.StateFile)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(data, &fields); err != nil {
						t.Fatal(err)
					}
					fields["future"] = json.RawMessage(`{"large":9007199254740993}`)
					fields["health"] = json.RawMessage(`"warning"`)
					if mode == "cleared diagnostic" {
						delete(fields, "last_error")
					}
					data, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p.Loop.StateFile, data, 0o640); err != nil {
						t.Fatal(err)
					}
					before, beforeInfo = stateFileSnapshot(t, p.Loop.StateFile)
					if mode == "post-call write failure" {
						if err := os.Mkdir(p.Loop.StateFile+".tmp", 0o700); err != nil {
							t.Fatal(err)
						}
					}
					return nil, context.DeadlineExceeded
				case "inspect":
					inspects++
					if callCtx.Err() != nil {
						t.Fatal("post-call inspect inherited cancellation")
					}
					return []byte("No such object"), errors.New("missing")
				default:
					t.Fatalf("missing failed run must not log/remove: %v", args)
					return nil, nil
				}
			}))
			s.ProfilePath = writeLoopProfile(t, p)
			s.Now = func() time.Time { return now }
			s.Stderr = &bytes.Buffer{}
			s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
			s.Sleep = func(context.Context, time.Duration) error {
				t.Fatal("cancelled failed run entered polling")
				return nil
			}
			err := s.Run(ctx)
			if runs != 1 || inspects != 1 || unlocks != 1 || s.restartCount != 1 {
				t.Fatalf("runs=%d inspects=%d unlocks=%d count=%d", runs, inspects, unlocks, s.restartCount)
			}
			if mode == "post-call write failure" {
				if !errors.Is(err, errStateWrite) {
					t.Errorf("cancellation swallowed fatal post-call state failure: %v", err)
				}
				if managedLists != 2 {
					t.Errorf("fatal state failure continued to adoption: lists=%d", managedLists)
				}
				assertStateFileUnchanged(t, p.Loop.StateFile, before, beforeInfo)
				return
			}
			if err != nil || managedLists != 3 {
				t.Fatalf("Run err=%v managed lists=%d, want empty stop adoption", err, managedLists)
			}
			got, err := state.LoadLoopState(p.Loop.StateFile)
			if err != nil || got.RestartCount != 1 || got.State != state.LoopStarting || got.LastTransitionAt.Format(time.RFC3339) != "2026-10-07T00:00:00Z" || got.Health != "warning" || got.LastRunnerName != "p-20261007-000000" || got.LastContainerName != "gha-p-20261007-000000" {
				t.Fatalf("cancelled failed attempt not durably published without a transition: %+v err=%v", got, err)
			}
			if mode == "cleared diagnostic" {
				if got.LastError != nil {
					t.Fatal("external diagnostic clear was resurrected")
				}
			} else if got.LastError == nil || *got.LastError != "current safe diagnostic" {
				t.Fatal("current diagnostic was overwritten")
			}
			data, err := os.ReadFile(p.Loop.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			var future bytes.Buffer
			if err := json.Compact(&future, fields["future"]); err != nil {
				t.Fatal(err)
			}
			if future.String() != `{"large":9007199254740993}` {
				t.Fatal("unknown forward-compatible data lost")
			}
		})
	}
}

func TestRunCancelledStartupSharesDrainBudget(t *testing.T) {
	f := &cycleHTTP{busy: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	clock.limit = 12
	inspects := 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		if ctx.Err() != nil {
			t.Error("drain inherited cancellation")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("drain subcall unbounded")
		}
		switch args[2] {
		case "ps":
			return []byte("c\tgha-p-1\timage\trunning\tp\tp-1\nd\tgha-p-2\timage\trunning\tp\tp-2\n"), nil
		case "inspect":
			inspects++
			return runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:00Z"), nil
		case "logs":
			return nil, nil
		default:
			t.Fatalf("must not start or delete: %v", args)
			return nil, nil
		}
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.Now, s.Sleep = clock.Now, clock.Sleep
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if clock.sleeps != 12 || inspects > 14 || f.posts.Load() != 0 {
		t.Fatalf("sleeps=%d inspects=%d posts=%d", clock.sleeps, inspects, f.posts.Load())
	}
	got, err := state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.LastError == nil {
		t.Fatalf("stop diagnostic=%+v err=%v", got, err)
	}
}

func TestCycleUnknownRunKeepsLockThroughStop(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	clock.limit = 14
	clock.cancelAt = 2
	clock.cancel = cancel
	runs, unlocks, inspects := 0, 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if unlocks != 0 {
			t.Error("unknown run unlocked before verification/drain")
		}
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			runs++
			return nil, context.DeadlineExceeded
		case "inspect":
			inspects++
			return nil, errors.New("daemon unavailable")
		default:
			t.Fatalf("unknown run must not log/delete: %v", args)
			return nil, nil
		}
	}))
	s.Now, s.Sleep = clock.Now, clock.Sleep
	s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
	s.restartCount = 2
	got, err := s.runCycle(ctx, p, settings)
	if err != nil || runs != 1 || unlocks != 1 || inspects < 3 || got.state != state.LoopRunningJob || len(s.skippedJobs) != 0 || f.deletes.Load() != 0 || s.restartCount != 3 {
		t.Fatalf("cycle=%+v err=%v runs=%d unlocks=%d inspect=%d", got, err, runs, unlocks, inspects)
	}
	if !reflect.DeepEqual(clock.durations[:2], []time.Duration{15 * time.Second, 15 * time.Second}) || clock.now.Sub(s.stopDeadline) != 0 {
		t.Fatalf("clock=%s deadline=%s durations=%v", clock.now, s.stopDeadline, clock.durations)
	}
}

func TestRunStartupCreatedRaceAndMultipleAdoptions(t *testing.T) {
	srv := cycleServer(t, &cycleHTTP{})
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC))
	clock.limit = 8
	d := &dockerScript{inspect: [][]byte{
		containerInspectJSON("created", "2026-10-07T00:00:00Z", "0001-01-01T00:00:00Z", 0),
		containerInspectJSON("created", "2026-10-07T00:00:00Z", "0001-01-01T00:00:00Z", 0),
		runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:05:00Z"),
		containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:05:00Z", 0),
		logEnvInspectJSON("RUNNER_TOKEN=fake-token"),
		runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:05:00Z"),
		containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:05:00Z", 0),
		logEnvInspectJSON("RUNNER_TOKEN=fake-token"),
	}, rmErrs: []error{errors.New("created started before rm"), nil}, logs: [][]byte{[]byte("safe")}}
	ps, locks, unlocks := 0, 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[2] == "ps" {
			ps++
			if ps == 1 {
				return []byte("c\tgha-p-1\timage\tcreated\tp\tp-1\nd\tgha-p-2\timage\trunning\tp\tp-2\n"), nil
			}
			return nil, nil
		}
		if locks-unlocks != 1 {
			t.Error("adoption operation without exclusive lock")
		}
		return d.Run(ctx, name, args...)
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.Now = clock.Now
	s.TryLock = func() (func() error, bool, error) {
		if locks != unlocks {
			t.Error("overlapping adoptions")
		}
		locks++
		return func() error { unlocks++; return nil }, true, nil
	}
	s.Sleep = func(ctx context.Context, duration time.Duration) error {
		if duration == 30*time.Second {
			cancel()
			return context.Canceled
		}
		return clock.Sleep(ctx, duration)
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if locks != 2 || unlocks != 2 || d.logsN != 2 || d.rmN != 3 || !reflect.DeepEqual(d.rmForce, []bool{false, false, false}) {
		t.Fatalf("locks=%d unlocks=%d logs=%d rm=%v", locks, unlocks, d.logsN, d.rmForce)
	}
}

func TestRunStopWithoutCurrentContainerBusyOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name              string
		psError, lockBusy bool
	}{
		{name: "unknown label query", psError: true}, {name: "busy lock", lockBusy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := disallowGitHubServer(t)
			defer srv.Close()
			p := loopProfile(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
			clock.limit = 12
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, _ string, args ...string) ([]byte, error) {
				if args[2] != "ps" {
					t.Fatalf("must leave container alone: %v", args)
				}
				obs := observeContext(ctx)
				if obs.err != nil || !obs.hasDeadline || obs.timeout > 10*time.Second {
					t.Errorf("stop label query=%+v", obs)
				}
				if tc.psError {
					return nil, errors.New("daemon down")
				}
				return []byte("c\tgha-p-1\timage\trunning\tp\tp-1\n"), nil
			}))
			s.ProfilePath = writeLoopProfile(t, p)
			s.Now, s.Sleep = clock.Now, clock.Sleep
			s.TryLock = func() (func() error, bool, error) { return nil, false, nil }
			if err := s.Run(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := state.LoadLoopState(p.Loop.StateFile)
			if err != nil || got.LastError == nil {
				t.Fatalf("diagnostic=%+v err=%v", got, err)
			}
			if tc.lockBusy && clock.sleeps != 12 {
				t.Fatalf("lock stop sleeps=%d", clock.sleeps)
			}
		})
	}
}

type loopHTTPFunc func(*http.Request) (*http.Response, error)

func (f loopHTTPFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestCycleCancellationBeforeRegistrationOrRun(t *testing.T) {
	for _, afterToken := range []bool{false, true} {
		t.Run(fmt.Sprint(afterToken), func(t *testing.T) {
			f := &cycleHTTP{jobs: true}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			unlocks := 0
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[2] != "ps" {
					t.Fatalf("run after cancel: %v", args)
				}
				if !afterToken && !strings.Contains(strings.Join(args, " "), "label=io.gha-runner-tui.profile=p") {
					cancel()
				}
				return nil, nil
			}))
			if afterToken {
				s.GitHub = gh.NewClient(srv.URL, "TEST_CYCLE_TOKEN", "", nil, loopHTTPFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := srv.Client().Do(r)
					if r.Method == "POST" {
						cancel()
					}
					return resp, err
				}))
			}
			s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
			s.restartCount = 4
			_, err := s.runCycle(ctx, p, settings)
			wantPosts := int32(0)
			if afterToken {
				wantPosts = 1
			}
			if !errors.Is(err, context.Canceled) || unlocks != 1 || f.posts.Load() != wantPosts || s.restartCount != 4 {
				t.Fatalf("err=%v unlocks=%d posts=%d", err, unlocks, f.posts.Load())
			}
		})
	}
}

func TestRunCycleErrorsBackoffResetsAfterSuccess(t *testing.T) {
	srv := cycleServer(t, &cycleHTTP{})
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] != "ps" {
			t.Fatalf("unexpected Docker %v", args)
		}
		return nil, nil
	}))
	iteration := 0
	s.GitHub = gh.NewClient(srv.URL, "TEST_CYCLE_TOKEN", "", nil, loopHTTPFunc(func(r *http.Request) (*http.Response, error) {
		if iteration != 2 {
			return nil, errors.New("query unavailable")
		}
		return srv.Client().Do(r)
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	var sleeps []time.Duration
	s.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		iteration++
		if iteration == 5 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Second, 2 * time.Second, 30 * time.Second, time.Second, 2 * time.Second}) {
		t.Fatalf("backoff reset=%v", sleeps)
	}
}

func TestCycleStateWriteFailureLeavesContainerAndUnlocks(t *testing.T) {
	srv := cycleServer(t, &cycleHTTP{jobs: true})
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	runs, unlocks := 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			runs++
			if err := os.Mkdir(p.Loop.StateFile+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
			return []byte("c"), nil
		default:
			t.Fatalf("state failure must not kill/log/wait: %v", args)
			return nil, nil
		}
	}))
	s.TryLock = func() (func() error, bool, error) { return func() error { unlocks++; return nil }, true, nil }
	_, err := s.runCycle(context.Background(), p, settings)
	if !errors.Is(err, errStateWrite) || runs != 1 || unlocks != 1 || s.restartCount != 1 {
		t.Fatalf("err=%v runs=%d unlocks=%d", err, runs, unlocks)
	}
}

func TestDefaultPollJitterRange(t *testing.T) {
	s := Supervisor{}
	s.defaults()
	for i := 0; i < 200; i++ {
		got := s.Jitter(30 * time.Second)
		if got < 24*time.Second || got > 36*time.Second {
			t.Fatalf("jitter=%s", got)
		}
	}
}

func TestRunStateWriteFailureIsFatalEvenIfStorageRecovers(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	runs := 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			runs++
			if err := os.Mkdir(p.Loop.StateFile+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
			return []byte("c"), nil
		default:
			t.Fatalf("state write failure must retain container: %v", args)
			return nil, nil
		}
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.TryLock = func() (func() error, bool, error) {
		return func() error { return os.Remove(p.Loop.StateFile + ".tmp") }, true, nil
	}
	s.Sleep = func(context.Context, time.Duration) error {
		t.Fatal("state write failure entered retry loop instead of returning safety error")
		return nil
	}
	if err := s.Run(context.Background()); err == nil || runs != 1 || f.posts.Load() != 1 {
		t.Fatalf("err=%v runs=%d posts=%d", err, runs, f.posts.Load())
	}
}

func TestRunWaitingHostPollsAtInjectedUpperJitter(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] != "ps" {
			t.Fatalf("must not run while lock busy: %v", args)
		}
		return nil, nil
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.TryLock = func() (func() error, bool, error) { return nil, false, nil }
	s.Jitter = func(d time.Duration) time.Duration { return d * 12 / 10 }
	s.Sleep = func(_ context.Context, d time.Duration) error {
		got, err := state.LoadLoopState(p.Loop.StateFile)
		if err != nil || got.State != state.LoopWaitingHost || d != 36*time.Second {
			t.Fatalf("state=%+v err=%v poll=%s", got, err, d)
		}
		cancel()
		return context.Canceled
	}
	if err := s.Run(ctx); err != nil || f.posts.Load() != 0 {
		t.Fatalf("err=%v registrations=%d", err, f.posts.Load())
	}
}

func TestRunIdleExpirySleepsWithSkipDiagnostic(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprint(adopt), func(t *testing.T) {
			f := &cycleHTTP{jobs: !adopt}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newTestClock(t, time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC))
			clock.limit = 1
			ps := 0
			d := &dockerScript{inspect: [][]byte{runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z")}, logs: [][]byte{[]byte("safe")}}
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				switch args[2] {
				case "ps":
					ps++
					if adopt && ps == 1 {
						return []byte("c\tgha-p-1\timage\trunning\tp\tp-1\n"), nil
					}
					return nil, nil
				case "run":
					return []byte("c"), nil
				default:
					return d.Run(ctx, name, args...)
				}
			}))
			s.ProfilePath = writeLoopProfile(t, p)
			s.Now = clock.Now
			s.Sleep = func(_ context.Context, duration time.Duration) error {
				got, err := state.LoadLoopState(p.Loop.StateFile)
				if err != nil || got.State != state.LoopSleeping || duration != 30*time.Second {
					t.Fatalf("state=%+v err=%v sleep=%s", got, err, duration)
				}
				if !adopt && (got.LastError == nil || !strings.Contains(*got.LastError, "runner group/labels")) {
					t.Fatalf("idle hint=%+v", got)
				}
				cancel()
				return context.Canceled
			}
			if err := s.Run(ctx); err != nil {
				t.Fatal(err)
			}
			wantSkip, wantPosts := 2, int32(1)
			if adopt {
				wantSkip, wantPosts = 0, 0
			}
			if len(s.skippedJobs) != wantSkip || f.posts.Load() != wantPosts || f.deletes.Load() != 1 || d.rmN != 1 || !d.rmForce[0] {
				t.Fatalf("skip=%v posts=%d deletes=%d remove=%v", s.skippedJobs, f.posts.Load(), f.deletes.Load(), d.rmForce)
			}
		})
	}
}

func TestRunnerEnvForRepositoryProfile(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		Repo: config.RepoConfig{Owner: "bigtomcat6", Name: "remind-me"},
		Runner: config.RunnerConfig{
			Labels:    []string{"self-hosted", "linux"},
			Workdir:   "/tmp/actions-runner",
			Ephemeral: true,
		},
		Docker: config.DockerProfile{Env: map[string]string{"RUNNER_ALLOW_RUNASROOT": "1"}},
	}

	env := runnerEnv(profile, "remind-me-1", "token")
	if env["RUNNER_REPO_URL"] != "https://github.com/bigtomcat6/remind-me" {
		t.Fatalf("unexpected repo url: %q", env["RUNNER_REPO_URL"])
	}
	if env["REPO_URL"] != "https://github.com/bigtomcat6/remind-me" {
		t.Fatalf("unexpected legacy repo url: %q", env["REPO_URL"])
	}
	if env["RUNNER_TOKEN"] != "token" {
		t.Fatalf("unexpected runner token: %q", env["RUNNER_TOKEN"])
	}
	if env["REG_TOKEN"] != "token" {
		t.Fatalf("unexpected legacy registration token: %q", env["REG_TOKEN"])
	}
	if _, ok := env["RUNNER_GROUP"]; ok {
		t.Fatalf("did not expect RUNNER_GROUP, got %q", env["RUNNER_GROUP"])
	}
}

func TestRunnerEnvForOrganizationProfile(t *testing.T) {
	t.Parallel()

	profile := config.Profile{
		Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		RunnerGroup: config.RunnerGroupConfig{
			Name:       "example-org-swift",
			Create:     true,
			Visibility: "all",
		},
		Runner: config.RunnerConfig{
			Environment: "swift",
			Labels:      []string{"self-hosted", "linux", "swift"},
			Workdir:     "/tmp/actions-runner",
			Ephemeral:   true,
		},
		Docker: config.DockerProfile{Env: map[string]string{"RUNNER_ALLOW_RUNASROOT": "1"}},
	}

	env := runnerEnv(profile, "example-org-swift-1", "token")
	if env["RUNNER_REPO_URL"] != "https://github.com/example-org" {
		t.Fatalf("unexpected org url: %q", env["RUNNER_REPO_URL"])
	}
	if env["REPO_URL"] != "https://github.com/example-org" {
		t.Fatalf("unexpected legacy org url: %q", env["REPO_URL"])
	}
	if env["REG_TOKEN"] != "token" {
		t.Fatalf("unexpected legacy registration token: %q", env["REG_TOKEN"])
	}
	if env["RUNNER_GROUP"] != "example-org-swift" {
		t.Fatalf("unexpected runner group: %q", env["RUNNER_GROUP"])
	}
}

func TestRegistrationTokenForRepositoryProfile(t *testing.T) {
	t.Parallel()

	client := &fakeRegistrationTokenClient{repoToken: "repo-token"}
	profile := config.Profile{
		Repo: config.RepoConfig{Owner: "bigtomcat6", Name: "remind-me"},
	}

	token, err := registrationTokenForProfile(context.Background(), client, profile)
	if err != nil {
		t.Fatalf("registrationTokenForProfile returned error: %v", err)
	}
	if token != "repo-token" {
		t.Fatalf("expected repo-token, got %q", token)
	}
	if len(client.repoCalls) != 1 || client.repoCalls[0] != "bigtomcat6/remind-me" {
		t.Fatalf("expected repo call, got %v", client.repoCalls)
	}
	if len(client.orgCalls) != 0 {
		t.Fatalf("did not expect org calls, got %v", client.orgCalls)
	}
}

func TestRegistrationTokenForOrganizationProfile(t *testing.T) {
	t.Parallel()

	client := &fakeRegistrationTokenClient{orgToken: "org-token"}
	profile := config.Profile{
		Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
		Runner: config.RunnerConfig{Environment: "swift"},
	}

	token, err := registrationTokenForProfile(context.Background(), client, profile)
	if err != nil {
		t.Fatalf("registrationTokenForProfile returned error: %v", err)
	}
	if token != "org-token" {
		t.Fatalf("expected org-token, got %q", token)
	}
	if len(client.orgCalls) != 1 || client.orgCalls[0] != "example-org" {
		t.Fatalf("expected org call, got %v", client.orgCalls)
	}
	if len(client.repoCalls) != 0 {
		t.Fatalf("did not expect repo calls, got %v", client.repoCalls)
	}
}

func TestLoopSettingsDefaultsAndFloors(t *testing.T) {
	t.Parallel()

	p := config.Profile{
		Repo:   config.RepoConfig{Owner: "me", Name: "app"},
		Runner: config.RunnerConfig{Ephemeral: true},
	}
	got, err := validateLoopProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.watch) != 1 || got.watch[0] != "me/app" ||
		got.poll != 30*time.Second || got.idle != 180*time.Second {
		t.Fatalf("unexpected settings: %+v", got)
	}
	p.Loop.PollIntervalSeconds, p.Loop.IdleTimeoutSeconds = 1, -1
	got, err = validateLoopProfile(p)
	if err != nil || got.poll != 10*time.Second || got.idle != 60*time.Second {
		t.Fatalf("floors: %+v, %v", got, err)
	}
}

func TestLoopSettingsRejectNonEphemeral(t *testing.T) {
	t.Parallel()

	_, err := validateLoopProfile(config.Profile{
		Repo: config.RepoConfig{Owner: "me", Name: "app"},
	})
	if err == nil {
		t.Fatal("expected ephemeral error")
	}
}

func TestLoopSettingsWatchMatrix(t *testing.T) {
	t.Parallel()

	orgProfile := func(watch ...string) config.Profile {
		return config.Profile{
			Target: config.TargetConfig{Scope: config.TargetScopeOrganization, Org: "Example Org"},
			Runner: config.RunnerConfig{Ephemeral: true, WatchRepositories: watch},
		}
	}
	repoProfile := func(watch ...string) config.Profile {
		return config.Profile{
			Repo:   config.RepoConfig{Owner: "me", Name: "app"},
			Runner: config.RunnerConfig{Ephemeral: true, WatchRepositories: watch},
		}
	}

	tests := []struct {
		name    string
		profile config.Profile
		want    []string
		wantErr bool
	}{
		{name: "organization requires watch", profile: orgProfile(), wantErr: true},
		{name: "organization rejects owner only", profile: orgProfile("owner"), wantErr: true},
		{name: "organization rejects missing owner", profile: orgProfile("/repo"), wantErr: true},
		{name: "organization rejects extra slash", profile: orgProfile("a/b/c"), wantErr: true},
		{name: "organization rejects empty entry", profile: orgProfile("   "), wantErr: true},
		{name: "organization accepts multiple", profile: orgProfile("a/app", "b/lib"), want: []string{"a/app", "b/lib"}},
		{name: "repository defaults to own", profile: repoProfile(), want: []string{"me/app"}},
		{name: "repository accepts own", profile: repoProfile("me/app"), want: []string{"me/app"}},
		{name: "repository accepts own case-insensitively", profile: repoProfile("ME/App"), want: []string{"ME/App"}},
		{name: "repository rejects other", profile: repoProfile("other/app"), wantErr: true},
		{name: "repository rejects multiple", profile: repoProfile("me/app", "me/app"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := validateLoopProfile(tt.profile)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got settings %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got.watch, tt.want) {
				t.Fatalf("watch = %v, want %v", got.watch, tt.want)
			}
		})
	}
}

func TestLoopSettingsRejectMalformedDefaultRepositoryWatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		owner string
		repo  string
	}{
		{name: "repo name with slash", owner: "me", repo: "app/extra"},
		{name: "owner with slash", owner: "me/extra", repo: "app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := validateLoopProfile(config.Profile{
				Repo:   config.RepoConfig{Owner: tt.owner, Name: tt.repo},
				Runner: config.RunnerConfig{Ephemeral: true},
			})
			if err == nil {
				t.Fatal("expected default watch format error, got nil")
			}
		})
	}
}

// --- Task 4 wait/finish fixtures ---

type loopRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f loopRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func loopProfile(t *testing.T) config.Profile {
	t.Helper()
	root := t.TempDir()
	return config.Profile{
		Name:    "p",
		Repo:    config.RepoConfig{Owner: "me", Name: "app"},
		Service: config.ServiceConfig{Name: "gha-p.service"},
		Runner:  config.RunnerConfig{Ephemeral: true, NamePrefix: "p", Labels: []string{"linux"}},
		Docker:  config.DockerProfile{Image: "runner:test", ContainerNamePrefix: "gha-p", RemoveAfterExit: true},
		Loop:    config.LoopConfig{StateFile: filepath.Join(root, "p.json"), LogDir: root, BackoffSeconds: 1, MaxBackoffSeconds: 4},
	}
}

// testClock is a deterministic clock. Every Sleep advances it. If a broken
// implementation ignores cancellation and the stop budget, the hard limit
// fails the test from the same goroutine instead of returning an error the
// implementation could ignore (which would leave the clock frozen and hang).
type testClock struct {
	t             *testing.T
	now           time.Time
	sleeps        int
	cancelAt      int
	cancel        func()
	limit         int
	durations     []time.Duration
	advanceFirst  time.Duration
	advancedFirst bool
}

func newTestClock(t *testing.T, start time.Time) *testClock {
	return &testClock{t: t, now: start, limit: 500}
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(_ context.Context, d time.Duration) error {
	c.sleeps++
	if c.limit > 0 && c.sleeps > c.limit {
		c.t.Fatalf("test clock exceeded %d sleeps; stop budget never depleted", c.limit)
	}
	if c.advanceFirst != 0 && !c.advancedFirst {
		c.advancedFirst = true
		d = c.advanceFirst
	}
	c.durations = append(c.durations, d)
	c.now = c.now.Add(d)
	if c.cancelAt > 0 && c.sleeps >= c.cancelAt && c.cancel != nil {
		c.cancel()
		return context.Canceled
	}
	return nil
}

func containerInspectJSON(stateName, created, started string, exitCode int) []byte {
	return []byte(fmt.Sprintf(
		`[{"Id":"c","Name":"/gha-p-1","Created":%q,"Config":{"Labels":{},"Env":[]},"State":{"Status":%q,"StartedAt":%q,"ExitCode":%d}}]`,
		created, stateName, started, exitCode,
	))
}

func logEnvInspectJSON(env ...string) []byte {
	encoded, _ := json.Marshal(env)
	return []byte(fmt.Sprintf(`[{"Config":{"Env":%s}}]`, encoded))
}

func runningInspect(created, started string) []byte {
	return containerInspectJSON("running", created, started, 0)
}

// ctxObservation captures the state and remaining deadline of the context a
// docker command actually received, so tests can prove subcalls are bounded
// and independent of the caller's cancellation.
type ctxObservation struct {
	err         error
	timeout     time.Duration
	hasDeadline bool
}

func observeContext(ctx context.Context) ctxObservation {
	obs := ctxObservation{err: ctx.Err()}
	if deadline, ok := ctx.Deadline(); ok {
		obs.hasDeadline = true
		obs.timeout = time.Until(deadline)
	}
	return obs
}

// dockerScript is a scripted command.Runner for the docker client. Byte/error
// sequences repeat their last element so a single output can stand for the
// whole run.
type dockerScript struct {
	inspect     [][]byte
	inspectErrs []error
	logs        [][]byte
	logsErrs    []error
	rmErrs      []error

	inspectN   int
	logsN      int
	rmN        int
	rmForce    []bool
	order      []string
	inspectObs []ctxObservation
	logsObs    []ctxObservation
	rmObs      []ctxObservation
	// beforeRM runs synchronously just before a docker rm, so tests can
	// assert ordering (e.g. logs already persisted).
	beforeRM func()
}

func (d *dockerScript) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "docker" || len(args) < 3 {
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	switch args[2] {
	case "inspect":
		d.order = append(d.order, "inspect")
		d.inspectObs = append(d.inspectObs, observeContext(ctx))
		out := pickBytes(d.inspect, d.inspectN)
		err := pickErr(d.inspectErrs, d.inspectN)
		d.inspectN++
		return out, err
	case "logs":
		d.order = append(d.order, "logs")
		d.logsObs = append(d.logsObs, observeContext(ctx))
		out := pickBytes(d.logs, d.logsN)
		err := pickErr(d.logsErrs, d.logsN)
		d.logsN++
		return out, err
	case "rm":
		d.order = append(d.order, "rm")
		d.rmObs = append(d.rmObs, observeContext(ctx))
		force := false
		for _, arg := range args {
			if arg == "-f" {
				force = true
			}
		}
		d.rmForce = append(d.rmForce, force)
		d.rmN++
		if d.beforeRM != nil {
			d.beforeRM()
		}
		err := pickErr(d.rmErrs, d.rmN-1)
		return nil, err
	default:
		return nil, fmt.Errorf("unexpected docker subcommand %q", args[2])
	}
}

func pickBytes(seq [][]byte, n int) []byte {
	if len(seq) == 0 {
		return nil
	}
	if n >= len(seq) {
		n = len(seq) - 1
	}
	return seq[n]
}

func pickErr(seq []error, n int) error {
	if len(seq) == 0 {
		return nil
	}
	if n >= len(seq) {
		n = len(seq) - 1
	}
	return seq[n]
}

// ghObs is one scripted response to the exact-name runner query.
type ghObs struct {
	status int // 0 => 200 with the runner below
	found  bool
	id     int64
	busy   bool
}

type ghScript struct {
	list        []ghObs
	deleteCodes []int
	listCalls   int
	deleteCalls int
	deleted     []int64
	now         func() time.Time
	callTimes   []time.Time
}

func newGHScript(t *testing.T, script *ghScript) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/me/app/actions/runners":
			if r.URL.Query().Get("name") != "p-1" {
				t.Errorf("runner name query = %q", r.URL.Query().Get("name"))
			}
			if script.now != nil {
				script.callTimes = append(script.callTimes, script.now())
			}
			idx := script.listCalls
			script.listCalls++
			obs := ghObs{}
			if len(script.list) > 0 {
				if idx >= len(script.list) {
					idx = len(script.list) - 1
				}
				obs = script.list[idx]
			}
			if obs.status != 0 {
				w.WriteHeader(obs.status)
				return
			}
			if !obs.found {
				fmt.Fprint(w, `{"runners":[]}`)
				return
			}
			fmt.Fprintf(w, `{"runners":[{"id":%d,"name":"p-1","status":"online","busy":%t}]}`, obs.id, obs.busy)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/me/app/actions/runners/"):
			if r.URL.RawQuery != "" {
				t.Errorf("delete query = %q", r.URL.RawQuery)
			}
			idx := script.deleteCalls
			script.deleteCalls++
			code := http.StatusNoContent
			if len(script.deleteCodes) > 0 {
				if idx >= len(script.deleteCodes) {
					idx = len(script.deleteCodes) - 1
				}
				if script.deleteCodes[idx] != 0 {
					code = script.deleteCodes[idx]
				}
			}
			parts := strings.Split(r.URL.Path, "/")
			id, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
			script.deleted = append(script.deleted, id)
			w.WriteHeader(code)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
}

func disallowGitHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected github request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}))
}

func newWaitSupervisor(t *testing.T, clock *testClock, runner command.Runner, srv *httptest.Server) Supervisor {
	t.Helper()
	t.Setenv("TEST_WAIT_TOKEN", "fake-wait-token")
	return Supervisor{
		Now:    clock.Now,
		Sleep:  clock.Sleep,
		Docker: dockerpkg.NewClient(runner),
		GitHub: gh.NewClient(srv.URL, "TEST_WAIT_TOKEN", "", nil, srv.Client()),
	}
}

func waitInfo(t *testing.T) dockerpkg.ContainerInfo {
	t.Helper()
	return dockerpkg.ContainerInfo{Name: "gha-p-1", RunnerName: "p-1"}
}

func TestWaitIdleDelete204AuthorizesForceRemove(t *testing.T) {
	t.Setenv("TEST_WAIT_TOKEN", "fake-wait-token")
	var deleted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/me/app/actions/runners":
			if r.URL.Query().Get("name") != "p-1" {
				t.Error("runner name query")
			}
			fmt.Fprint(w, `{"runners":[{"id":7,"name":"p-1","status":"online","busy":false}]}`)
		case r.Method == "DELETE" && r.URL.Path == "/repos/me/app/actions/runners/7":
			if r.URL.RawQuery != "" {
				t.Error("delete query")
			}
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	now := time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	s := Supervisor{
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			now = now.Add(d)
			sleeps++
			if sleeps == 30 {
				cancel()
				return context.Canceled
			}
			return nil
		},
		Docker: dockerpkg.NewClient(loopRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "docker" || len(args) < 3 || args[2] != "inspect" {
				t.Fatalf("unexpected command: %s %v", name, args)
			}
			return []byte(`[{"Id":"c","Name":"/gha-p-1","Created":"2026-10-07T00:00:00Z","Config":{"Labels":{}},"State":{"Status":"running","StartedAt":"2026-10-07T00:00:01Z","ExitCode":0}}]`), nil
		})),
		GitHub: gh.NewClient(srv.URL, "TEST_WAIT_TOKEN", "", nil, srv.Client()),
	}
	got, err := s.waitContainer(ctx, loopProfile(t), dockerpkg.ContainerInfo{Name: "gha-p-1", RunnerName: "p-1"}, 180*time.Second)
	if err != nil || !got.forceRemove || !got.idleExpired || got.retained || deleted.Load() != 1 {
		t.Fatalf("outcome=%+v delete=%d err=%v", got, deleted.Load(), err)
	}
}

func TestWaitExitedEnds(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", "2026-10-07T00:00:00Z", "2026-10-07T00:00:00Z", 0)}}
	srv := disallowGitHubServer(t)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(context.Background(), loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.exitCode == nil || *got.exitCode != 0 {
		t.Fatalf("expected exit code 0, got %+v", got)
	}
	if got.forceRemove || got.idleExpired || got.retained {
		t.Fatalf("unexpected result %+v", got)
	}
	if docker.rmN != 0 {
		t.Fatal("must not remove an exited container here")
	}
}

func TestWaitMissingEnds(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{
		inspect:     [][]byte{[]byte("Error: No such object: gha-p-1")},
		inspectErrs: []error{errors.New("exit status 1")},
	}
	srv := disallowGitHubServer(t)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(context.Background(), loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.exitCode != nil {
		t.Fatalf("missing container must not report an exit code: %+v", got)
	}
	if got.forceRemove || got.retained {
		t.Fatalf("unexpected result %+v", got)
	}
	if docker.rmN != 0 {
		t.Fatal("must not remove a missing container")
	}
}

func TestWaitInspectUnknownRetainsOnStop(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{
		inspect:     [][]byte{[]byte("daemon down"), []byte("not json"), runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")},
		inspectErrs: []error{errors.New("exit status 1"), nil},
	}
	script := &ghScript{}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 2
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.retained {
		t.Fatalf("expected retained after stop budget, got %+v", got)
	}
	if got.forceRemove || docker.rmN != 0 || script.deleteCalls != 0 {
		t.Fatalf("unknown inspect must not delete: result=%+v rm=%d delete=%d", got, docker.rmN, script.deleteCalls)
	}
}

func TestWaitCreatedRemovalWins(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	created := start.Add(-59 * time.Second).UTC().Format(time.RFC3339)
	docker := &dockerScript{inspect: [][]byte{containerInspectJSON("created", created, "0001-01-01T00:00:00Z", 0)}}
	srv := disallowGitHubServer(t)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(context.Background(), loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if docker.inspectN < 2 {
		t.Fatalf("expected a second inspect after the grace period, got %d", docker.inspectN)
	}
	if docker.rmN != 1 {
		t.Fatalf("expected exactly one non-forced rm, got %d", docker.rmN)
	}
	if docker.rmForce[0] {
		t.Fatal("created removal must not be forced")
	}
	if got.forceRemove || got.exitCode != nil || got.retained {
		t.Fatalf("unexpected result %+v", got)
	}
}

func TestWaitCreatedStartWins(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	created := start.Add(-90 * time.Second).UTC().Format(time.RFC3339)
	running := runningInspect(start.Add(-300*time.Second).UTC().Format(time.RFC3339), start.Add(-299*time.Second).UTC().Format(time.RFC3339))
	docker := &dockerScript{
		inspect: [][]byte{containerInspectJSON("created", created, "0001-01-01T00:00:00Z", 0), running},
		rmErrs:  []error{errors.New("rm failed")},
	}
	script := &ghScript{list: []ghObs{{found: true, id: 7, busy: true}}}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 3
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if docker.inspectN < 2 {
		t.Fatalf("must re-inspect after a failed created removal, got %d", docker.inspectN)
	}
	if docker.rmN != 1 || docker.rmForce[0] {
		t.Fatalf("expected one non-forced rm attempt, got n=%d force=%v", docker.rmN, docker.rmForce)
	}
	if got.forceRemove {
		t.Fatalf("failed created removal must never release the slot: %+v", got)
	}
	if script.listCalls == 0 {
		t.Fatal("must fall through to the runner wait after the container starts")
	}
}

func TestWaitCreatedUnknownRetainsOnStop(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	badCreated := []byte(`[{"Id":"c","Name":"/gha-p-1","Created":"bad-time","Config":{"Labels":{},"Env":[]},"State":{"Status":"created","StartedAt":"0001-01-01T00:00:00Z","ExitCode":0}}]`)
	docker := &dockerScript{inspect: [][]byte{badCreated}}
	script := &ghScript{}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 1
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.retained {
		t.Fatalf("unknown created timestamp must retain, got %+v", got)
	}
	if docker.rmN != 0 || script.deleteCalls != 0 {
		t.Fatalf("must not delete on unknown created data: rm=%d delete=%d", docker.rmN, script.deleteCalls)
	}
}

func TestWaitRemovingWaitsForDisappearance(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{
		inspect:     [][]byte{containerInspectJSON("removing", "2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z", 0), []byte("Error: No such object: gha-p-1")},
		inspectErrs: []error{nil, errors.New("exit status 1")},
	}
	// Removing containers must never reach GitHub: a forced DELETE is wrong.
	srv := disallowGitHubServer(t)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(context.Background(), loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.forceRemove || got.retained || docker.rmN != 0 {
		t.Fatalf("removing must end only on disappearance without deletion: %+v rm=%d", got, docker.rmN)
	}
}

func TestWaitIdleDelete422KeepsWaiting(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{
		list:        []ghObs{{found: true, id: 7, busy: false}, {found: true, id: 7, busy: true}},
		deleteCodes: []int{http.StatusUnprocessableEntity},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 40
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.forceRemove {
		t.Fatalf("422 must not authorize removal: %+v", got)
	}
	if docker.rmN != 0 {
		t.Fatal("422 must not remove the container")
	}
	if script.deleteCalls == 0 {
		t.Fatal("expected a DELETE attempt")
	}
	if !got.retained {
		t.Fatalf("expected retained after cancellation, got %+v", got)
	}
}

func TestWaitAPIErrorRetriesWithoutDelete(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{
		list: []ghObs{
			{status: http.StatusForbidden},
			{status: http.StatusTooManyRequests},
			{status: http.StatusInternalServerError},
			{found: true, id: 7, busy: false},
		},
		deleteCodes: []int{http.StatusInternalServerError},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 60
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.forceRemove || docker.rmN != 0 {
		t.Fatalf("API errors must not delete: result=%+v rm=%d", got, docker.rmN)
	}
	if script.listCalls < 4 {
		t.Fatalf("expected retries after API errors, got %d list calls", script.listCalls)
	}
}

func TestWaitSeenThenGone(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{list: []ghObs{
		{found: true, id: 7, busy: true},
		{},
		{},
	}}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 400
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.forceRemove {
		t.Fatalf("two consecutive disappearances must authorize removal: %+v", got)
	}
	if got.idleExpired {
		t.Fatalf("seen-then-gone is not an idle expiry: %+v", got)
	}
	if docker.rmN != 0 {
		t.Fatal("wait must not remove; finish owns removal")
	}
}

func TestWaitMissingInterruptedResets(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{list: []ghObs{
		{found: true, id: 7, busy: true},
		{},
		{status: http.StatusInternalServerError},
		{},
		{found: true, id: 7, busy: true},
		{},
		{},
	}}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 400
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.forceRemove {
		t.Fatalf("expected removal only after two final consecutive misses: %+v", got)
	}
	if script.listCalls < 7 {
		t.Fatalf("expected all scripted observations, got %d", script.listCalls)
	}
	if script.deleteCalls != 0 {
		t.Fatal("must not DELETE a busy runner")
	}
}

func TestWaitUnknownNeverAuthorizesDelete(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{containerInspectJSON("mystery", "2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z", 0)}}
	// Adversarial: GitHub would answer busy=false with DELETE 204, but an
	// unknown container observation must never reach it.
	script := &ghScript{
		list:        []ghObs{{found: true, id: 7, busy: false}},
		deleteCodes: []int{http.StatusNoContent},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 3
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.forceRemove {
		t.Fatalf("unknown observation must not authorize removal: %+v", got)
	}
	if script.listCalls != 0 || script.deleteCalls != 0 {
		t.Fatalf("unknown observation must not reach GitHub: list=%d delete=%d", script.listCalls, script.deleteCalls)
	}
	if docker.rmN != 0 || !got.retained {
		t.Fatalf("unknown observation must be retained, rm=%d result=%+v", docker.rmN, got)
	}
}

func TestWaitCreatedNoForceBeforeRunning(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	// Future creation age keeps the container below the 60s grace window for
	// the whole bounded drain.
	created := start.Add(1 * time.Hour).UTC().Format(time.RFC3339)
	docker := &dockerScript{inspect: [][]byte{containerInspectJSON("created", created, "0001-01-01T00:00:00Z", 0)}}
	script := &ghScript{
		list:        []ghObs{{found: true, id: 7, busy: false}},
		deleteCodes: []int{http.StatusNoContent},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 2
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.forceRemove || script.deleteCalls != 0 || script.listCalls != 0 || docker.rmN != 0 {
		t.Fatalf("created must not reach GitHub force removal: result=%+v list=%d delete=%d rm=%d", got, script.listCalls, script.deleteCalls, docker.rmN)
	}
	if !got.retained {
		t.Fatalf("created stop must retain: %+v", got)
	}
}

func TestWaitDiagnosticPreservesState(t *testing.T) {
	p := loopProfile(t)
	original := `{"profile":"p","repo":"me/app","state":"running-job","health":"running","last_transition_at":"2020-01-01T00:00:00Z","last_runner_name":"old","last_container_id":"abc","last_container_name":"oldc","last_exit_code":7,"last_error":null,"restart_count":3,"custom_field":"keepme"}`
	if err := os.WriteFile(p.Loop.StateFile, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}

	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 2
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	if _, err := s.waitContainer(ctx, p, waitInfo(t), 180*time.Second); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(p.Loop.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("state file lost its trailing newline: %q", data)
	}
	info, err := os.Stat(p.Loop.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("state file mode = %v, want 0640", info.Mode().Perm())
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"custom_field":      `"keepme"`,
		"last_container_id": `"abc"`,
		"last_exit_code":    `7`,
		"restart_count":     `3`,
	}
	for key, want := range checks {
		if got := string(fields[key]); got != want {
			t.Fatalf("%s = %s, want %s (existing state erased)", key, got, want)
		}
	}
	if got := string(fields["last_transition_at"]); got != `"2020-01-01T00:00:00Z"` {
		t.Fatalf("diagnostic changed enum transition time: %q", got)
	}
	if !strings.Contains(string(fields["last_error"]), "runner has not registered") {
		t.Fatalf("last_error not updated safely: %s", fields["last_error"])
	}
}

// C2: a timestamp-only candidate must never replace an unchanged state file.
func publishStateRecord(s *Supervisor, p config.Profile, record stateRecord) error {
	fields, previous, err := readStateFile(p.Loop.StateFile)
	if err != nil {
		return err
	}
	return s.writeState(p, record, fields, previous)
}

func stateFileSnapshot(t *testing.T, path string) ([]byte, os.FileInfo) {
	t.Helper()
	stamp := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return data, info
}

func assertStateFileUnchanged(t *testing.T, path string, data []byte, info os.FileInfo) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, after) || !os.SameFile(info, afterInfo) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("identical state rewritten: contents=%t inode=%t mtime=%s -> %s", bytes.Equal(data, after), os.SameFile(info, afterInfo), info.ModTime(), afterInfo.ModTime())
	}
}

func TestC2IdenticalPublicationDoesNotTouchFile(t *testing.T) {
	for _, status := range []state.LoopStatus{state.LoopSleeping, state.LoopWaitingHost, state.LoopBackoff} {
		t.Run(string(status), func(t *testing.T) {
			p := loopProfile(t)
			s := Supervisor{}
			r := stateRecord{Profile: "p", Repo: "me/app", State: string(status), Health: "running", LastTransitionAt: "2026-10-07T00:00:00Z"}
			if err := publishStateRecord(&s, p, r); err != nil {
				t.Fatal(err)
			}
			data, info := stateFileSnapshot(t, p.Loop.StateFile)
			// A directory here proves a no-op does not even try the temp write.
			if err := os.Mkdir(p.Loop.StateFile+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
			r.LastTransitionAt = "2026-10-07T01:00:00Z"
			if err := publishStateRecord(&s, p, r); err != nil {
				t.Errorf("identical state tried to write: %v", err)
			}
			assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
		})
	}
}

func TestC2SameEnumPersistsKnownChangesAndClearsOptionals(t *testing.T) {
	p := loopProfile(t)
	s := Supervisor{}
	r := stateRecord{Profile: "p", Repo: "me/app", State: "sleeping", Health: "healthy", LastTransitionAt: "2026-10-07T00:00:00Z"}
	if err := publishStateRecord(&s, p, r); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"error appears", "error changes", "error empty", "error clears", "identity", "exit zero", "exit nonzero", "exit clears", "count", "health", "identity clears"} {
		t.Run(change, func(t *testing.T) {
			before, info := stateFileSnapshot(t, p.Loop.StateFile)
			r.LastTransitionAt = "2026-10-07T01:00:00Z"
			switch change {
			case "error appears", "error changes":
				reason := change
				r.LastError = &reason
			case "error empty":
				reason := ""
				r.LastError = &reason
			case "error clears":
				r.LastError = nil
			case "identity":
				r.LastRunnerName, r.LastContainerID, r.LastContainerName = "runner", "id", "container"
			case "exit zero", "exit nonzero":
				code := 0
				if change == "exit nonzero" {
					code = 7
				}
				r.LastExitCode = &code
			case "exit clears":
				r.LastExitCode = nil
			case "count":
				r.RestartCount = 2
			case "health":
				r.Health = "warning"
			case "identity clears":
				r.LastRunnerName, r.LastContainerID, r.LastContainerName = "", "", ""
			}
			if err := publishStateRecord(&s, p, r); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(p.Loop.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(p.Loop.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before, data) || os.SameFile(info, after) {
				t.Fatal("known change was not atomically published")
			}
			var got stateRecord
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			want := r
			want.LastTransitionAt = "2026-10-07T00:00:00Z"
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%+v want=%+v", got, want)
			}
		})
	}
	r.State = "waiting-host"
	r.LastTransitionAt = "2026-10-07T02:00:00Z"
	if err := publishStateRecord(&s, p, r); err != nil {
		t.Fatal(err)
	}
	got, err := state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.State != state.LoopWaitingHost || got.LastTransitionAt.Format(time.RFC3339) != r.LastTransitionAt {
		t.Fatalf("enum transition=%+v err=%v", got, err)
	}
}

func TestC2DiagnosticNoOpAndFreshDiskMerge(t *testing.T) {
	p := loopProfile(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	s := Supervisor{Now: func() time.Time { return now }, Stderr: &stderr}
	r := stateRecord{Profile: "p", Repo: "me/app", State: "running-job", Health: "running", LastTransitionAt: now.Format(time.RFC3339), LastContainerName: "container"}
	if err := publishStateRecord(&s, p, r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "safe reason")
	got, err := state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.LastTransitionAt.Format(time.RFC3339) != r.LastTransitionAt || got.LastError == nil || *got.LastError != "safe reason" {
		t.Errorf("diagnostic=%+v err=%v", got, err)
	}
	data, info := stateFileSnapshot(t, p.Loop.StateFile)
	now = now.Add(time.Hour)
	s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "safe reason")
	assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
	// An external actor adds a forward-compatible field and clears the error.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["custom_field"] = json.RawMessage(`{"large":9007199254740993,"nested":[true,null]}`)
	fields["last_error"] = json.RawMessage(`null`)
	external, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Loop.StateFile, external, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := s.writeCycle(p, cycleResult{state: state.LoopSleeping, containerName: "container"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err = state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.LastError != nil {
		t.Fatalf("stale diagnostic resurrected: %+v err=%v", got, err)
	}
	data, err = os.ReadFile(p.Loop.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var custom bytes.Buffer
	if err := json.Compact(&custom, fields["custom_field"]); err != nil {
		t.Fatal(err)
	}
	if custom.String() != `{"large":9007199254740993,"nested":[true,null]}` {
		t.Fatalf("unknown key lost: %s", fields["custom_field"])
	}
	if _, ok := fields["last_error"]; ok {
		t.Fatal("omitted known error was retained")
	}
	// Clearing by omission, not just explicit null, must also stay cleared even
	// when a diagnostic and normal publication use the same Supervisor.
	s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "safe reason")
	data, err = os.ReadFile(p.Loop.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "last_error")
	external, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Loop.StateFile, external, 0o640); err != nil {
		t.Fatal(err)
	}
	data, info = stateFileSnapshot(t, p.Loop.StateFile)
	if err := s.writeCycle(p, cycleResult{state: state.LoopSleeping, containerName: "container"}, nil); err != nil {
		t.Fatal(err)
	}
	assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
	// Diagnostic must read the externally replaced file, too.
	if err := os.WriteFile(p.Loop.StateFile, []byte(`{"state":"backoff","last_transition_at":"2021-01-01T00:00:00Z","restart_count":9,"actor":true}`), 0o640); err != nil {
		t.Fatal(err)
	}
	s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "safe reason")
	got, err = state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.State != state.LoopBackoff || got.RestartCount != 9 || got.LastTransitionAt.Format(time.RFC3339) != "2021-01-01T00:00:00Z" {
		t.Fatalf("diagnostic used stale snapshot: %+v %v", got, err)
	}
	if strings.Count(stderr.String(), "safe reason") != 4 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestC2RepairStateAndFatalReadFailure(t *testing.T) {
	for _, original := range []string{"", "{bad", "null", `[]`, `{"state":"sleeping","restart_count":"bad","future":true}`, `{"state":"sleeping","last_transition_at":"bad","future":true}`} {
		t.Run(original, func(t *testing.T) {
			p := loopProfile(t)
			if original != "" {
				if err := os.WriteFile(p.Loop.StateFile, []byte(original), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			s := Supervisor{Now: func() time.Time { return time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC) }}
			if err := s.writeCycle(p, cycleResult{state: state.LoopSleeping}, nil); err != nil {
				t.Fatal(err)
			}
			got, err := state.LoadLoopState(p.Loop.StateFile)
			if err != nil || got.State != state.LoopSleeping || got.LastTransitionAt.Format(time.RFC3339) != "2026-10-07T00:00:00Z" {
				t.Fatalf("repair=%+v err=%v", got, err)
			}
			data, err := os.ReadFile(p.Loop.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(original, "future") && !strings.Contains(string(data), `"future": true`) {
				t.Fatal("recoverable unknown key lost")
			}
		})
	}
	p := loopProfile(t)
	if err := os.Mkdir(p.Loop.StateFile, 0o700); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	s := Supervisor{Stderr: &stderr}
	if err := s.writeCycle(p, cycleResult{state: state.LoopBackoff}, errors.New("cycle failed")); !errors.Is(err, errStateWrite) {
		t.Fatalf("read failure not fatal: %v", err)
	}
	s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "safe reason")
	if !strings.Contains(stderr.String(), "safe reason") {
		t.Fatal("diagnostic storage failure suppressed stderr")
	}
	if _, err := os.Stat(p.Loop.StateFile + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed snapshot still attempted temp write: %v", err)
	}
}

func TestC2RunPollsDoNotCountStartAttempts(t *testing.T) {
	for _, mode := range []string{"empty", "waiting-host", "backoff"} {
		t.Run(mode, func(t *testing.T) {
			f := &cycleHTTP{jobs: mode == "waiting-host"}
			srv := cycleServer(t, f)
			defer srv.Close()
			p := loopProfile(t)
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[2] != "ps" {
					t.Fatalf("no attempt expected: %v", args)
				}
				return nil, nil
			}))
			if mode == "backoff" {
				s.GitHub = gh.NewClient(srv.URL, "TEST_CYCLE_TOKEN", "", nil, loopHTTPFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("query unavailable") }))
			}
			s.TryLock = func() (func() error, bool, error) { return nil, false, nil }
			s.ProfilePath = writeLoopProfile(t, p)
			now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			polls := 0
			var data []byte
			var info os.FileInfo
			s.Sleep = func(context.Context, time.Duration) error {
				got, err := state.LoadLoopState(p.Loop.StateFile)
				if err != nil {
					t.Fatal(err)
				}
				if got.RestartCount != 0 {
					t.Errorf("poll counted as start attempt: %d", got.RestartCount)
				}
				if polls == 0 {
					data, info = stateFileSnapshot(t, p.Loop.StateFile)
				} else {
					assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
				}
				polls++
				now = now.Add(time.Minute)
				if polls == 3 {
					cancel()
					return context.Canceled
				}
				return nil
			}
			if err := s.Run(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestC2DiagnosticMissingStateRepeatedNoOp(t *testing.T) {
	p := loopProfile(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	s := Supervisor{Now: func() time.Time { return now }, Stderr: &bytes.Buffer{}}
	s.noteWaitIssue(p, waitInfo(t), "safe reason")
	data, info := stateFileSnapshot(t, p.Loop.StateFile)
	now = now.Add(time.Hour)
	s.noteWaitIssue(p, waitInfo(t), "safe reason")
	assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
}

func TestC2IdenticalKnownValuesIgnoreJSONFormatting(t *testing.T) {
	p := loopProfile(t)
	// An external writer can escape strings differently without changing values.
	original := `{"profile":"\u0070","repo":"me\/app","state":"sleeping","health":"healthy","last_transition_at":"2026-10-07T00:00:00+00:00","last_error":"\u0073afe","restart_count":0,"future":{"large":9007199254740993}}`
	if err := os.WriteFile(p.Loop.StateFile, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	data, info := stateFileSnapshot(t, p.Loop.StateFile)
	s := Supervisor{}
	reason := "safe"
	r := stateRecord{Profile: "p", Repo: "me/app", State: "sleeping", Health: "healthy", LastTransitionAt: "2026-10-07T01:00:00Z", LastError: &reason}
	if err := publishStateRecord(&s, p, r); err != nil {
		t.Fatal(err)
	}
	assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
	s.Stderr = &bytes.Buffer{}
	s.noteWaitIssue(p, dockerpkg.ContainerInfo{}, "safe")
	assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
}

func TestC2NoAttemptOnStartingWriteFailureOrFinalCancellation(t *testing.T) {
	for _, mode := range []string{"starting write failure", "cancel while publishing starting"} {
		t.Run(mode, func(t *testing.T) {
			srv := cycleServer(t, &cycleHTTP{jobs: true})
			defer srv.Close()
			p := loopProfile(t)
			settings, _ := validateLoopProfile(p)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[2] != "ps" {
					t.Fatalf("no run authorized: %v", args)
				}
				return nil, nil
			}))
			s.restartCount = 4
			calls := 0
			s.Now = func() time.Time {
				calls++
				if calls == 3 { // Registering time, runner name, then Starting time.
					if mode == "starting write failure" {
						if err := os.Mkdir(p.Loop.StateFile+".tmp", 0o700); err != nil {
							t.Fatal(err)
						}
					} else {
						cancel()
					}
				}
				return time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
			}
			_, err := s.runCycle(ctx, p, settings)
			wantErr := errStateWrite
			if mode == "cancel while publishing starting" {
				wantErr = context.Canceled
			}
			if !errors.Is(err, wantErr) || s.restartCount != 4 {
				t.Fatalf("err=%v count=%d", err, s.restartCount)
			}
			got, err := state.LoadLoopState(p.Loop.StateFile)
			if err != nil || got.RestartCount != 4 {
				t.Fatalf("failed publication counted a nonexistent call: %+v err=%v", got, err)
			}
		})
	}
}

func TestC2RunResetsCounterOnlyAtEntry(t *testing.T) {
	srv := cycleServer(t, &cycleHTTP{jobs: true})
	defer srv.Close()
	p := loopProfile(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	stopping := false
	runs := 0
	var s *Supervisor
	s = cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			if stopping {
				return []byte("c\tgha-p-adopted\timage\trunning\tp\tp-adopted\n"), nil
			}
			return nil, nil
		case "run":
			runs++
			if s.restartCount != 1 {
				t.Fatalf("count at actual command=%d", s.restartCount)
			}
			return []byte("c"), nil
		case "inspect":
			return ownedInspectJSON("c", args[3], "p", "true", "exited"), nil
		case "logs", "rm":
			return nil, nil
		default:
			t.Fatalf("unexpected Docker %v", args)
			return nil, nil
		}
	}))
	s.Now = func() time.Time { return now }
	s.ProfilePath = writeLoopProfile(t, p)
	s.restartCount = 9
	if err := s.writeCycle(p, cycleResult{state: state.LoopSleeping}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	s.Sleep = func(context.Context, time.Duration) error {
		got, err := state.LoadLoopState(p.Loop.StateFile)
		if err != nil || got.RestartCount != 1 || s.restartCount != 1 {
			t.Fatalf("attempt lost between polls: %+v err=%v count=%d", got, err, s.restartCount)
		}
		polls++
		s.skippedJobs = map[int64]time.Time{21: now.Add(time.Hour), 22: now.Add(time.Hour)}
		if polls == 2 {
			stopping = true
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := state.LoadLoopState(p.Loop.StateFile)
	if err != nil || runs != 1 || polls != 2 || got.RestartCount != 1 || s.restartCount != 1 || got.LastContainerName != "gha-p-adopted" {
		t.Fatalf("stop adoption reset count: %+v err=%v count=%d runs=%d polls=%d", got, err, s.restartCount, runs, polls)
	}
	// Reusing this Supervisor for a fresh process-style Run resets only at entry,
	// including adoption on an already-cancelled context. It never loads disk 1.
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.RestartCount != 0 || s.restartCount != 0 || runs != 1 {
		t.Fatalf("fresh Run adopted previous count: %+v err=%v count=%d runs=%d", got, err, s.restartCount, runs)
	}
}

func TestC2RunSnapshotFailureIsFatalAfterStorageRecovers(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	if err := os.Mkdir(p.Loop.StateFile, 0o700); err != nil {
		t.Fatal(err)
	}
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] != "ps" {
			t.Fatalf("snapshot failure started or removed container: %v", args)
		}
		return nil, nil
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.TryLock = func() (func() error, bool, error) {
		return func() error { return os.Remove(p.Loop.StateFile) }, true, nil
	}
	s.Sleep = func(context.Context, time.Duration) error { t.Fatal("fatal snapshot error retried"); return nil }
	if err := s.Run(context.Background()); !errors.Is(err, errStateWrite) || f.posts.Load() != 0 || s.restartCount != 0 {
		t.Fatalf("err=%v posts=%d count=%d", err, f.posts.Load(), s.restartCount)
	}
	if _, err := os.Stat(p.Loop.StateFile + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable snapshot attempted write: %v", err)
	}
}

// The invalid-configuration branch asks for Done only after publishing failed.
// Notify the test at that exact boundary, without polling or real sleeps.
type failedWaitContext struct {
	context.Context
	waiting chan struct{}
}

func (c failedWaitContext) Done() <-chan struct{} {
	close(c.waiting)
	return c.Context.Done()
}

func TestC2FailedConfigurationInitialCountAndStablePublication(t *testing.T) {
	p := loopProfile(t)
	p.Runner.Ephemeral = false
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	s := Supervisor{ProfilePath: writeLoopProfile(t, p), restartCount: 9, Now: func() time.Time { return now }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- s.Run(failedWaitContext{Context: ctx, waiting: waiting}) }()
	timeout, stopTimeout := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopTimeout()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("invalid configuration did not wait: %v", err)
	case <-timeout.Done():
		t.Fatal("failed state was not published")
	}
	got, err := state.LoadLoopState(p.Loop.StateFile)
	if err != nil || got.State != state.LoopFailed || got.RestartCount != 0 || got.LastError == nil {
		t.Fatalf("failed configuration=%+v err=%v", got, err)
	}
	data, info := stateFileSnapshot(t, p.Loop.StateFile)
	now = now.Add(time.Hour)
	if err := s.writeCycle(p, cycleResult{state: state.LoopFailed}, errors.New("loop requires runner.ephemeral=true")); err != nil {
		t.Fatal(err)
	}
	assertStateFileUnchanged(t, p.Loop.StateFile, data, info)
	select {
	case err := <-done:
		t.Fatalf("failed loop returned before cancellation: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-timeout.Done():
		t.Fatal("failed loop ignored cancellation")
	}
}

func TestWaitStopBeforeIdleDrains(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	// Started exactly now: idle (180s) has not elapsed.
	docker := &dockerScript{inspect: [][]byte{runningInspect(start.Format(time.RFC3339), start.Format(time.RFC3339))}}
	script := &ghScript{
		list:        []ghObs{{found: true, id: 7, busy: false}},
		deleteCodes: []int{http.StatusNoContent},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.forceRemove || got.idleExpired {
		t.Fatalf("stop DELETE 204 must drain before idle without idleExpired: %+v", got)
	}
	if elapsed := clock.now.Sub(start); elapsed >= 180*time.Second {
		t.Fatalf("drained only after idle: %s", elapsed)
	}
}

func TestWaitStopBoundedSubcallContext(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Jump to 4s of remaining budget before the second inspect, forcing the
	// subcall cap to shrink below the 10s ceiling and proving it cannot run
	// past the shared stop deadline. The clock keeps its own hard-limit guard;
	// subsequent sleeps still go through clock.Sleep.
	clock.advanceFirst = waitStopBudget - 4*time.Second
	s := newWaitSupervisor(t, clock, docker, srv)

	if _, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(docker.inspectObs) < 2 {
		t.Fatalf("expected at least two inspects, got %d", len(docker.inspectObs))
	}
	for i, obs := range docker.inspectObs {
		if obs.err != nil {
			t.Fatalf("inspect %d inherited the canceled caller context: %v", i, obs.err)
		}
		if !obs.hasDeadline {
			t.Fatalf("inspect %d has no deadline", i)
		}
	}
	// The 10s ceiling applies while budget remains.
	if got := docker.inspectObs[0].timeout; got <= 9*time.Second || got > waitSubcallTimeout {
		t.Fatalf("first inspect timeout = %s, want (9s, 10s]", got)
	}
	// With only 4s of budget left, the second inspect is capped by the budget.
	if got := docker.inspectObs[1].timeout; got <= 3*time.Second || got > 4*time.Second {
		t.Fatalf("second inspect timeout = %s, want (3s, 4s] (capped by remaining budget)", got)
	}
}

func TestWaitCadenceInspectAndGitHub(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{list: []ghObs{{found: true, id: 7, busy: true}}}
	script.now = clock.Now
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 6
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	if _, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second); err != nil {
		t.Fatal(err)
	}

	if len(clock.durations) < 7 {
		t.Fatalf("expected normal + stop sleeps, got %v", clock.durations)
	}
	for i := 0; i < 6; i++ {
		if clock.durations[i] != waitInspectInterval {
			t.Fatalf("normal sleep %d = %s, want %s", i, clock.durations[i], waitInspectInterval)
		}
	}
	if clock.durations[6] != waitStopInterval {
		t.Fatalf("first stop sleep = %s, want %s", clock.durations[6], waitStopInterval)
	}

	if len(script.callTimes) < 2 {
		t.Fatalf("expected repeated GitHub checks, got %d", len(script.callTimes))
	}
	if got := script.callTimes[1].Sub(script.callTimes[0]); got != waitGitHubInterval {
		t.Fatalf("GitHub cadence = %s, want %s", got, waitGitHubInterval)
	}
	if len(script.callTimes) >= 4 {
		if got := script.callTimes[3].Sub(script.callTimes[2]); got != waitStopInterval {
			t.Fatalf("stopping GitHub cadence = %s, want %s", got, waitStopInterval)
		}
	}
}

func TestWaitNeverSeenRetainsOnStop(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.retained {
		t.Fatalf("never-seen runner must be retained after the stop budget: %+v", got)
	}
	if got.forceRemove || docker.rmN != 0 || script.deleteCalls != 0 {
		t.Fatalf("must not delete a never-seen runner: result=%+v rm=%d delete=%d", got, docker.rmN, script.deleteCalls)
	}
	if elapsed := clock.now.Sub(start); elapsed > 60*time.Second {
		t.Fatalf("stop budget overshot 60s: %s (sleeps=%d)", elapsed, clock.sleeps)
	}
}

func TestWaitJobDoneWithoutExitDrains(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{
		list:        []ghObs{{found: true, id: 7, busy: true}, {found: true, id: 7, busy: false}},
		deleteCodes: []int{http.StatusNoContent},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 400
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.forceRemove {
		t.Fatalf("idle DELETE 204 must authorize removal: %+v", got)
	}
	if script.deleteCalls != 1 {
		t.Fatalf("expected one DELETE, got %d", script.deleteCalls)
	}
}

func TestWaitStopBusyRetains(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{list: []ghObs{{found: true, id: 7, busy: true}}}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 1
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.retained || got.forceRemove {
		t.Fatalf("busy runner must be retained on stop: %+v", got)
	}
	if script.deleteCalls != 0 || docker.rmN != 0 {
		t.Fatalf("busy runner must not be deleted: delete=%d rm=%d", script.deleteCalls, docker.rmN)
	}
}

func TestWaitStopIdle204AuthorizesRemove(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{
		list:        []ghObs{{found: true, id: 7, busy: true}, {found: true, id: 7, busy: false}},
		deleteCodes: []int{http.StatusNoContent},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 1
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.forceRemove || got.idleExpired || got.retained {
		t.Fatalf("stopping DELETE 204 must authorize removal without idle expiry: %+v", got)
	}
}

func TestWaitStopIdle422Retains(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{
		list:        []ghObs{{found: true, id: 7, busy: true}, {found: true, id: 7, busy: false}},
		deleteCodes: []int{http.StatusUnprocessableEntity},
	}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 1
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.retained || got.forceRemove {
		t.Fatalf("stopping DELETE 422 must retain: %+v", got)
	}
}

func TestWaitStopSeenGone(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{list: []ghObs{
		{found: true, id: 7, busy: true},
		{},
		{},
	}}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 1
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.forceRemove {
		t.Fatalf("seen-then-gone during stop must authorize removal: %+v", got)
	}
}

func TestWaitStopNeverSeenRetains(t *testing.T) {
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	clock.cancelAt = 2
	clock.cancel = cancel
	s := newWaitSupervisor(t, clock, docker, srv)

	got, err := s.waitContainer(ctx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.retained || got.forceRemove {
		t.Fatalf("never-seen stop must retain: %+v", got)
	}
	if script.deleteCalls != 0 || docker.rmN != 0 {
		t.Fatalf("never-seen stop must not delete: delete=%d rm=%d", script.deleteCalls, docker.rmN)
	}
}

func TestWaitMultiContainerSharesStopDeadline(t *testing.T) {
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := newTestClock(t, start)
	docker := &dockerScript{inspect: [][]byte{runningInspect("2026-10-06T23:00:00Z", "2026-10-06T23:00:01Z")}}
	script := &ghScript{}
	srv := newGHScript(t, script)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, docker, srv)

	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstCancel()
	if _, err := s.waitContainer(firstCtx, loopProfile(t), waitInfo(t), 180*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := s.stopDeadline
	sleepsAfterFirst := clock.sleeps

	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondCancel()
	got, err := s.waitContainer(secondCtx, loopProfile(t), waitInfo(t), 180*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.retained {
		t.Fatalf("second container must observe the exhausted shared budget: %+v", got)
	}
	if clock.sleeps != sleepsAfterFirst {
		t.Fatalf("second container reset or extended the stop budget: sleeps %d -> %d", sleepsAfterFirst, clock.sleeps)
	}
	if !s.stopDeadline.Equal(deadline) {
		t.Fatalf("stop deadline was reset: %s -> %s", deadline, s.stopDeadline)
	}
}

func logDirContents(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func orderIndex(order []string, want string) int {
	for i, value := range order {
		if value == want {
			return i
		}
	}
	return -1
}

func TestFinishOrderLogsThenRemove(t *testing.T) {
	exitCode := 0
	cases := []struct {
		name            string
		removeAfterExit bool
		result          waitResult
		rmErr           error
		wantRm          bool
		wantForce       bool
		wantErr         bool
	}{
		{name: "force removal ignores config", result: waitResult{forceRemove: true}, wantRm: true, wantForce: true},
		{name: "exited container respects remove after exit", removeAfterExit: true, result: waitResult{exitCode: &exitCode}, wantRm: true},
		{name: "exited container without remove after exit", result: waitResult{exitCode: &exitCode}},
		{name: "no removal without basis", removeAfterExit: true, result: waitResult{}},
		{name: "remove failure returns error", removeAfterExit: true, result: waitResult{forceRemove: true}, rmErr: errors.New("rm failed"), wantRm: true, wantForce: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := loopProfile(t)
			p.Docker.RemoveAfterExit = tc.removeAfterExit
			docker := &dockerScript{
				inspect: [][]byte{logEnvInspectJSON("RUNNER_TOKEN=fake-token")},
				logs:    [][]byte{[]byte("safe log line")},
				rmErrs:  []error{tc.rmErr},
			}
			logPersistedBeforeRemove := false
			docker.beforeRM = func() {
				if logDirContents(t, p.Loop.LogDir) != "" {
					logPersistedBeforeRemove = true
				}
			}
			s := Supervisor{Docker: dockerpkg.NewClient(docker)}

			// Finish must work from an already-canceled parent: it derives its
			// own bounded log/remove contexts internally.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := s.finishContainer(ctx, p, waitInfo(t), tc.result)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantRm && !logPersistedBeforeRemove {
				t.Fatal("logs must already be persisted when remove runs")
			}
			if tc.wantRm {
				if docker.rmN == 0 {
					t.Fatal("expected a remove call")
				}
				if docker.rmForce[0] != tc.wantForce {
					t.Fatalf("force = %v, want %v", docker.rmForce[0], tc.wantForce)
				}
			} else if docker.rmN != 0 {
				t.Fatalf("unexpected remove call: %v", docker.rmForce)
			}
			logsIdx, rmIdx := orderIndex(docker.order, "logs"), orderIndex(docker.order, "rm")
			if logsIdx == -1 {
				t.Fatal("logs must be attempted before removal")
			}
			if rmIdx != -1 && logsIdx > rmIdx {
				t.Fatalf("logs must be persisted before remove: %v", docker.order)
			}
		})
	}
}

func TestFinishUsesIndependentBoundedContexts(t *testing.T) {
	p := loopProfile(t)
	docker := &dockerScript{
		inspect: [][]byte{logEnvInspectJSON("RUNNER_TOKEN=fake-token")},
		logs:    [][]byte{[]byte("safe log line")},
	}
	s := Supervisor{Docker: dockerpkg.NewClient(docker)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.finishContainer(ctx, p, waitInfo(t), waitResult{forceRemove: true}); err != nil {
		t.Fatal(err)
	}

	if len(docker.logsObs) != 1 {
		t.Fatalf("expected exactly one docker logs call, got %d", len(docker.logsObs))
	}
	logObs := docker.logsObs[0]
	if logObs.err != nil {
		t.Fatalf("logs inherited the canceled parent context: %v", logObs.err)
	}
	if !logObs.hasDeadline || logObs.timeout <= 0 || logObs.timeout > waitFinishLogTimeout ||
		logObs.timeout <= waitFinishLogTimeout-2*time.Second {
		t.Fatalf("logs context timeout = %s (hasDeadline=%v), want (%s, %s]",
			logObs.timeout, logObs.hasDeadline, waitFinishLogTimeout-2*time.Second, waitFinishLogTimeout)
	}

	if len(docker.rmObs) != 1 {
		t.Fatalf("expected exactly one docker rm call, got %d", len(docker.rmObs))
	}
	rmObs := docker.rmObs[0]
	if rmObs.err != nil {
		t.Fatalf("remove inherited the canceled parent context: %v", rmObs.err)
	}
	if !rmObs.hasDeadline || rmObs.timeout <= 0 || rmObs.timeout > waitSubcallTimeout ||
		rmObs.timeout <= waitSubcallTimeout-2*time.Second {
		t.Fatalf("remove context timeout = %s (hasDeadline=%v), want (%s, %s]",
			rmObs.timeout, rmObs.hasDeadline, waitSubcallTimeout-2*time.Second, waitSubcallTimeout)
	}
}

func TestFinishLogProtection(t *testing.T) {
	t.Run("bad full inspect still redacts", func(t *testing.T) {
		p := loopProfile(t)
		badInspect := []byte(`[{"Created":"bad-time","Config":{"Env":["RUNNER_TOKEN=fake-runner-token"]},"State":{"Status":17,"StartedAt":"bad-time"}}]`)
		docker := &dockerScript{
			inspect: [][]byte{badInspect},
			logs:    [][]byte{[]byte("token fake-runner-token leaked")},
		}
		s := Supervisor{Docker: dockerpkg.NewClient(docker)}

		if err := s.finishContainer(context.Background(), p, waitInfo(t), waitResult{forceRemove: true}); err != nil {
			t.Fatal(err)
		}
		content := logDirContents(t, p.Loop.LogDir)
		if strings.Contains(content, "fake-runner-token") {
			t.Fatalf("persisted logs leaked a token: %q", content)
		}
		if !strings.Contains(content, "[REDACTED]") {
			t.Fatalf("expected redacted content persisted: %q", content)
		}
		if docker.rmN != 1 || !docker.rmForce[0] {
			t.Fatalf("authoritative force removal must still happen: n=%d", docker.rmN)
		}
	})

	t.Run("nonzero logs still persist safe content", func(t *testing.T) {
		p := loopProfile(t)
		docker := &dockerScript{
			inspect:  [][]byte{logEnvInspectJSON("REG_TOKEN=fake-reg-token")},
			logs:     [][]byte{[]byte("fake-reg-token body")},
			logsErrs: []error{errors.New("exit status 1")},
		}
		s := Supervisor{Docker: dockerpkg.NewClient(docker)}

		if err := s.finishContainer(context.Background(), p, waitInfo(t), waitResult{exitCode: ptrInt(0)}); err != nil {
			t.Fatal(err)
		}
		content := logDirContents(t, p.Loop.LogDir)
		if strings.Contains(content, "fake-reg-token") {
			t.Fatalf("persisted logs leaked a token: %q", content)
		}
		if !strings.Contains(content, "[REDACTED]") {
			t.Fatalf("expected redacted content persisted: %q", content)
		}
	})

	t.Run("env gate failure writes nothing but still removes", func(t *testing.T) {
		p := loopProfile(t)
		docker := &dockerScript{
			inspect: [][]byte{[]byte(`[{"Config":{"Labels":{}}}]`)},
			logs:    [][]byte{[]byte("raw fake-runner-token")},
		}
		s := Supervisor{Docker: dockerpkg.NewClient(docker)}

		if err := s.finishContainer(context.Background(), p, waitInfo(t), waitResult{forceRemove: true}); err != nil {
			t.Fatal(err)
		}
		if content := logDirContents(t, p.Loop.LogDir); content != "" {
			t.Fatalf("env gate failure must not persist logs: %q", content)
		}
		if docker.logsN != 0 {
			t.Fatal("env gate failure must not run docker logs")
		}
		if docker.rmN != 1 || !docker.rmForce[0] {
			t.Fatalf("authoritative removal must still happen: n=%d", docker.rmN)
		}
	})
}

func ptrInt(value int) *int { return &value }

// These tests are synchronous; a file avoids pipe capacity/deadlock concerns.
func captureLoopStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	previous := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = previous }()
	fn()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunRetentionDiagnostics(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprint(registered), func(t *testing.T) {
			p := loopProfile(t)
			start := time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC)
			clock := newTestClock(t, start)
			clock.limit = 12
			script := &ghScript{}
			if registered {
				script.list = []ghObs{{found: true, id: 7, busy: true}}
			}
			srv := newGHScript(t, script)
			defer srv.Close()
			d := &dockerScript{inspect: [][]byte{runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z")}}
			s := newWaitSupervisor(t, clock, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if args[2] == "ps" {
					return []byte("c\tgha-p-1\timage\trunning\tp\tp-1\n"), nil
				}
				return d.Run(ctx, name, args...)
			}), srv)
			s.ProfilePath = writeLoopProfile(t, p)
			s.TryLock = func() (func() error, bool, error) { return func() error { return nil }, true, nil }
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stderr bytes.Buffer
			s.Stderr = &stderr
			if err := s.Run(ctx); err != nil {
				t.Fatal(err)
			}
			output := stderr.String()
			if !strings.Contains(output, "stop budget exhausted; container retained") || !strings.Contains(output, "gha-p-1") || !strings.Contains(output, "p-1") {
				t.Errorf("missing retention diagnostic: %q", output)
			}
			if !registered && !strings.Contains(output, "runner has not registered; container retained") {
				t.Errorf("missing registration diagnostic: %q", output)
			}
			if d.rmN != 0 || d.logsN != 0 || script.deleteCalls != 0 || clock.now.Sub(start) != 60*time.Second {
				t.Fatalf("retention safety/budget changed: rm=%d logs=%d deletes=%d elapsed=%s", d.rmN, d.logsN, script.deleteCalls, clock.now.Sub(start))
			}
		})
	}
}

func TestCycleIdleSkipDiagnosticOutput(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	p := loopProfile(t)
	settings, _ := validateLoopProfile(p)
	clock := newTestClock(t, time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC))
	d := &dockerScript{inspect: [][]byte{runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z")}, logs: [][]byte{[]byte("safe log")}}
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			return []byte("c"), nil
		default:
			return d.Run(ctx, name, args...)
		}
	}))
	s.Now, s.Sleep = clock.Now, clock.Sleep
	var stderr bytes.Buffer
	s.Stderr = &stderr
	got, err := s.runCycle(context.Background(), p, settings)
	if err != nil || got.state != state.LoopSleeping {
		t.Fatalf("cycle=%+v err=%v", got, err)
	}
	if err := s.writeCycle(p, got, nil); err != nil {
		t.Fatal(err)
	}
	output := stderr.String()
	if !strings.Contains(output, "runner group/labels") || !strings.Contains(output, "candidate jobs skipped for 30 minutes") {
		t.Errorf("missing idle skip explanation: %q", output)
	}
	for _, id := range []int64{21, 22} {
		if !s.skippedJobs[id].Equal(clock.now.Add(30 * time.Minute)) {
			t.Errorf("skip TTL changed for %d: %s", id, s.skippedJobs[id])
		}
	}
	if f.deletes.Load() != 1 || d.rmN != 1 || !d.rmForce[0] {
		t.Fatalf("idle authorization changed: deletes=%d rm=%v", f.deletes.Load(), d.rmForce)
	}
}

func TestFinishFailureDiagnosticsBeforeAuthorizedRemoval(t *testing.T) {
	const malicious = "fake-PAT fake-REGTOKEN raw-inspect-body Config.Env"
	for _, tc := range []struct {
		name, want                        string
		gate, malformed, persist, partial bool
	}{
		{name: "env gate", gate: true, want: "log acquisition failed"},
		{name: "malformed env gate", gate: true, malformed: true, want: "log acquisition failed"},
		{name: "empty acquisition", want: "log acquisition failed"},
		{name: "partial acquisition", partial: true, want: "log acquisition failed"},
		{name: "persist", persist: true, want: "log persistence failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := loopProfile(t)
			original := `{"profile":"p","custom_field":"keepme"}`
			if err := os.WriteFile(p.Loop.StateFile, []byte(original), 0o640); err != nil {
				t.Fatal(err)
			}
			d := &dockerScript{inspect: [][]byte{logEnvInspectJSON("RUNNER_TOKEN=fake-REGTOKEN")}}
			if tc.gate {
				d.inspect = [][]byte{[]byte(malicious)}
				d.inspectErrs = []error{errors.New(malicious)}
				if tc.malformed {
					d.inspect = [][]byte{[]byte(`[{"Config":{"Env":"` + malicious + `"}}]`)}
					d.inspectErrs = nil
				}
			} else if tc.persist {
				d.logs = [][]byte{[]byte("safe log")}
				// A non-directory path forces a real write failure even as root;
				// its secret-shaped filename must not enter diagnostics.
				p.Loop.LogDir = filepath.Join(t.TempDir(), malicious)
				if err := os.WriteFile(p.Loop.LogDir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				d.logsErrs = []error{errors.New(malicious)}
				if tc.partial {
					d.logs = [][]byte{[]byte("partial fake-REGTOKEN")}
				}
			}
			var stderr bytes.Buffer
			d.beforeRM = func() {
				if !strings.Contains(stderr.String(), tc.want) {
					t.Errorf("diagnostic missing before rm: %q", stderr.String())
				}
				if tc.partial {
					content := logDirContents(t, p.Loop.LogDir)
					if !strings.Contains(content, "partial [REDACTED]") || strings.Contains(content, "fake-REGTOKEN") {
						t.Errorf("partial safe log not persisted before rm: %q", content)
					}
				}
			}
			s := Supervisor{Docker: dockerpkg.NewClient(d), Stderr: &stderr}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.finishContainer(ctx, p, waitInfo(t), waitResult{forceRemove: true}); err != nil {
				t.Fatal(err)
			}
			output := stderr.String()
			if !strings.Contains(output, tc.want) {
				t.Errorf("missing finish diagnostic: %q", output)
			}
			for _, secret := range strings.Fields(malicious) {
				if strings.Contains(output, secret) {
					t.Errorf("diagnostic leaked %s: %q", secret, output)
				}
			}
			data, err := os.ReadFile(p.Loop.StateFile)
			if err != nil || string(data) != original {
				t.Errorf("finish changed existing state: %q err=%v", data, err)
			}
			if d.rmN != 1 || !d.rmForce[0] || (tc.gate && d.logsN != 0) {
				t.Fatalf("authorized removal/gate changed: rm=%v logs=%d", d.rmForce, d.logsN)
			}
		})
	}
}

func TestWaitDiagnosticDefaultsToStderrDespiteStateFailure(t *testing.T) {
	p := loopProfile(t)
	p.Loop.StateFile = filepath.Join(t.TempDir(), "missing", "state.json")
	s := Supervisor{}
	output := captureLoopStderr(t, func() {
		s.noteWaitIssue(p, waitInfo(t), "stop budget exhausted; container retained")
	})
	if !strings.Contains(output, "stop budget exhausted; container retained") || !strings.Contains(output, "gha-p-1") {
		t.Fatalf("default stderr lost diagnostic on state failure: %q", output)
	}
}

func TestWaitAPIFailureDiagnosticsExcludeBackendBodies(t *testing.T) {
	const malicious = "fake-PAT fake-REGTOKEN raw-inspect-body Config.Env"
	for _, deletion := range []bool{false, true} {
		t.Run(fmt.Sprint(deletion), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if deletion && r.Method == http.MethodGet {
					fmt.Fprint(w, `{"runners":[{"id":7,"name":"p-1","busy":false}]}`)
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, malicious)
			}))
			defer srv.Close()
			clock := newTestClock(t, time.Date(2026, 10, 7, 0, 5, 0, 0, time.UTC))
			clock.limit = 12
			d := &dockerScript{inspect: [][]byte{runningInspect("2026-10-07T00:00:00Z", "2026-10-07T00:00:01Z")}}
			s := newWaitSupervisor(t, clock, d, srv)
			var stderr bytes.Buffer
			s.Stderr = &stderr
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			p := loopProfile(t)
			got, err := s.waitContainer(ctx, p, waitInfo(t), 180*time.Second)
			if err != nil || !got.retained || got.forceRemove || d.rmN != 0 {
				t.Fatalf("API failure changed retention: result=%+v err=%v rm=%d", got, err, d.rmN)
			}
			want := "runner lookup failed; container retained"
			if deletion {
				want = "runner deletion failed; container retained"
			}
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("missing safe API diagnostic: %q", stderr.String())
			}
			data, err := os.ReadFile(p.Loop.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range strings.Fields(malicious) {
				if strings.Contains(stderr.String(), secret) || strings.Contains(string(data), secret) {
					t.Errorf("backend body leaked %s into diagnostic/state", secret)
				}
			}
		})
	}
}

func TestFinishDiagnosticWriteFailureDoesNotChangeRemovalPolicy(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		t.Run(fmt.Sprint(authorized), func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "closed-stderr")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			d := &dockerScript{inspect: [][]byte{[]byte("invalid inspect")}}
			s := Supervisor{Docker: dockerpkg.NewClient(d), Stderr: f}
			if err := s.finishContainer(context.Background(), loopProfile(t), waitInfo(t), waitResult{forceRemove: authorized}); err != nil {
				t.Fatal(err)
			}
			wantRm := 0
			if authorized {
				wantRm = 1
			}
			if d.logsN != 0 || d.rmN != wantRm || (authorized && !d.rmForce[0]) {
				t.Fatalf("diagnostic failure changed gate/removal: logs=%d rm=%v", d.logsN, d.rmForce)
			}
		})
	}
}
