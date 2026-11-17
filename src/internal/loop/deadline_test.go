package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "gha-runner-tui/internal/github"
	"gha-runner-tui/internal/state"
)

func writeSessionDeadline(t *testing.T, path string, deadline time.Time) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"deadline": deadline.UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func deadlinePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "session-deadline.json")
}

func TestReadSessionDeadlineFloorAndErrors(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		offset  time.Duration
		wantErr bool
	}{
		{name: "plenty remaining", offset: 200 * time.Minute},
		{name: "exactly the floor", offset: sessionDeadlineMinRemaining},
		{name: "one second below the floor", offset: sessionDeadlineMinRemaining - time.Second, wantErr: true},
		{name: "expired", offset: -time.Minute, wantErr: true},
		{name: "exactly now", offset: 0, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := deadlinePath(t)
			deadline := now.Add(tc.offset)
			writeSessionDeadline(t, path, deadline)
			got, err := readSessionDeadline(path, now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("deadline=%s err=%v", got, err)
			}
			if tc.wantErr && !errors.Is(err, errSessionDeadline) {
				t.Fatalf("expected deadline refusal, got %v", err)
			}
			if got.IsZero() || !got.Equal(deadline) {
				t.Fatalf("deadline=%s, want %s", got, deadline)
			}
		})
	}
}

func TestReadSessionDeadlineUnusableFiles(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		content string
		dir     bool
	}{
		{name: "missing"},
		{name: "directory", dir: true},
		{name: "not json", content: `{"deadline":`},
		{name: "missing key", content: `{"other":"x"}`},
		{name: "empty value", content: `{"deadline":"   "}`},
		{name: "invalid time", content: `{"deadline":"tomorrow"}`},
		{name: "json array", content: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := deadlinePath(t)
			if tc.dir {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			deadline, err := readSessionDeadline(path, now)
			if !errors.Is(err, errSessionDeadline) || !deadline.IsZero() {
				t.Fatalf("deadline=%s err=%v", deadline, err)
			}
			if !sessionDeadlineExhausted(path, now) {
				t.Fatal("configured but unusable deadline must enter the protected withdrawal path")
			}
		})
	}

	t.Run("extra fields and offset", func(t *testing.T) {
		path := deadlinePath(t)
		if err := os.WriteFile(path, []byte(`{"deadline":"2026-11-17T16:00:00+01:00","note":"keep"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readSessionDeadline(path, now)
		if err != nil || !got.Equal(now.Add(3*time.Hour)) {
			t.Fatalf("deadline=%s err=%v", got, err)
		}
	})
}

func TestSessionDeadlineExhaustedBoundary(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if sessionDeadlineExhausted("", now) {
		t.Fatal("unconfigured protection must stay inactive")
	}
	for _, tc := range []struct {
		name   string
		offset time.Duration
		want   bool
	}{
		{name: "far", offset: 200 * time.Minute},
		{name: "floor", offset: sessionDeadlineMinRemaining},
		{name: "below floor", offset: time.Hour, want: true},
		{name: "expired", offset: -time.Minute, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := deadlinePath(t)
			writeSessionDeadline(t, path, now.Add(tc.offset))
			if got := sessionDeadlineExhausted(path, now); got != tc.want {
				t.Fatalf("exhausted=%t want %t", got, tc.want)
			}
		})
	}
}

func TestSessionDeadlineAdmissionDisabled(t *testing.T) {
	p := loopProfile(t)
	if err := sessionDeadlineAdmission(p, time.Now()); err != nil {
		t.Fatalf("unconfigured profile refused: %v", err)
	}
}

func TestCycleSessionDeadlineRefusesBeforeJobQuery(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, now.Add(-time.Minute))
	srv := disallowGitHubServer(t)
	defer srv.Close()
	p := loopProfile(t)
	p.Loop.SessionDeadlineFile = path
	settings, _ := validateLoopProfile(p)
	dockerCalls := 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if args[2] != "ps" {
			t.Fatalf("deadline refusal must not touch a container: %v", args)
		}
		dockerCalls++
		return nil, nil
	}))
	s.Now = func() time.Time { return now }
	s.TryLock = func() (func() error, bool, error) { t.Fatal("deadline refusal took the lock"); return nil, false, nil }
	_, err := s.runCycle(context.Background(), p, settings)
	if !errors.Is(err, errSessionDeadline) {
		t.Fatalf("err=%v", err)
	}
	if dockerCalls != 1 {
		t.Fatalf("cleanup calls=%d, want only the safe terminal cleanup", dockerCalls)
	}
}

func TestCycleSessionDeadlineRechecksAfterLock(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, now.Add(200*time.Minute))
	p := loopProfile(t)
	p.Loop.SessionDeadlineFile = path
	settings, _ := validateLoopProfile(p)
	runs, locks, unlocks := 0, 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "run":
			runs++
			return []byte("c"), nil
		case "ps":
			if strings.Contains(strings.Join(args, " "), "label=io.gha-runner-tui.profile=p") {
				return nil, nil
			}
			writeSessionDeadline(t, path, now.Add(-time.Minute))
			return nil, nil
		default:
			t.Fatalf("refusal after lock must not inspect/wait: %v", args)
			return nil, nil
		}
	}))
	s.Now = func() time.Time { return now }
	s.TryLock = func() (func() error, bool, error) {
		locks++
		return func() error { unlocks++; return nil }, true, nil
	}
	_, err := s.runCycle(context.Background(), p, settings)
	if !errors.Is(err, errSessionDeadline) {
		t.Fatalf("err=%v", err)
	}
	if runs != 0 || locks != 1 || unlocks != 1 || f.posts.Load() != 0 {
		t.Fatalf("runs=%d locks=%d unlocks=%d posts=%d", runs, locks, unlocks, f.posts.Load())
	}
}

func TestCycleSessionDeadlineRechecksAfterToken(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, now.Add(200*time.Minute))
	p := loopProfile(t)
	p.Loop.SessionDeadlineFile = path
	settings, _ := validateLoopProfile(p)
	runs, unlocks := 0, 0
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "run":
			runs++
			return []byte("c"), nil
		case "ps":
			return nil, nil
		default:
			t.Fatalf("refusal after token must not inspect/wait: %v", args)
			return nil, nil
		}
	}))
	s.Now = func() time.Time { return now }
	s.TryLock = func() (func() error, bool, error) {
		return func() error { unlocks++; return nil }, true, nil
	}
	s.GitHub = gh.NewClient(srv.URL, "TEST_CYCLE_TOKEN", "", nil, loopHTTPFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := srv.Client().Do(r)
		if r.Method == http.MethodPost {
			writeSessionDeadline(t, path, now.Add(-time.Minute))
		}
		return resp, err
	}))
	_, err := s.runCycle(context.Background(), p, settings)
	if !errors.Is(err, errSessionDeadline) {
		t.Fatalf("err=%v", err)
	}
	if runs != 0 || unlocks != 1 || f.posts.Load() != 1 {
		t.Fatalf("runs=%d unlocks=%d posts=%d", runs, unlocks, f.posts.Load())
	}
}

func TestRunSessionDeadlineRefusalPollsAndRecovers(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, now.Add(-time.Minute))
	var queries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/me/app/actions/runs" {
			queries.Add(1)
			fmt.Fprint(w, `{"workflow_runs":[]}`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := loopProfile(t)
	p.Loop.SessionDeadlineFile = path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := cycleSupervisor(t, srv, loopRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] != "ps" {
			t.Fatalf("unexpected Docker %v", args)
		}
		return nil, nil
	}))
	s.ProfilePath = writeLoopProfile(t, p)
	s.Now = func() time.Time { return now }
	s.TryLock = func() (func() error, bool, error) { t.Fatal("no jobs while refused"); return nil, false, nil }
	sleeps := 0
	s.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps++
		if sleeps == 1 {
			got, err := state.LoadLoopState(p.Loop.StateFile)
			if err != nil || got.State != state.LoopBackoff || got.LastError == nil || !strings.Contains(*got.LastError, "session deadline protection") {
				t.Fatalf("refusal state=%+v err=%v", got, err)
			}
			if d != 30*time.Second || queries.Load() != 0 {
				t.Fatalf("refusal sleep=%s queries=%d, want poll without job queries", d, queries.Load())
			}
			writeSessionDeadline(t, path, now.Add(200*time.Minute))
			return nil
		}
		cancel()
		return context.Canceled
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if queries.Load() == 0 {
		t.Fatal("updated deadline was not re-read")
	}
}

func TestCycleDeadlineWithdrawalIsNotIdleSkip(t *testing.T) {
	f := &cycleHTTP{jobs: true}
	srv := cycleServer(t, f)
	defer srv.Close()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, now.Add(200*time.Minute))
	p := loopProfile(t)
	p.Loop.SessionDeadlineFile = path
	settings, _ := validateLoopProfile(p)
	clock := newTestClock(t, now)
	d := &dockerScript{
		inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))},
		logs:    [][]byte{[]byte("safe log")},
	}
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
	s.Now = clock.Now
	s.TryLock = func() (func() error, bool, error) { return func() error { return nil }, true, nil }
	s.Sleep = func(ctx context.Context, wait time.Duration) error {
		// Re-read while the runner is already registered and idle: crossing the
		// threshold must withdraw without pretending the idle timeout fired.
		if len(clock.durations) == 0 {
			writeSessionDeadline(t, path, clock.Now().Add(50*time.Minute))
		}
		return clock.Sleep(ctx, wait)
	}
	got, err := s.runCycle(context.Background(), p, settings)
	if err != nil || got.state != state.LoopSleeping {
		t.Fatalf("cycle=%+v err=%v", got, err)
	}
	if f.deletes.Load() != 1 || d.rmN != 1 || !d.rmForce[0] {
		t.Fatalf("deletes=%d rm=%d force=%v", f.deletes.Load(), d.rmN, d.rmForce)
	}
	if len(s.skippedJobs) != 0 {
		t.Fatalf("deadline withdrawal skipped queued jobs: %v", s.skippedJobs)
	}
}

func TestWaitSessionDeadlineWithdrawalSafety(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		list        []ghObs
		deleteCodes []int
		wantRemove  bool
		wantDelete  int
		wantDiag    string
	}{
		{name: "idle 204", list: []ghObs{{found: true, id: 7}}, wantRemove: true, wantDelete: 1},
		{
			name: "busy", list: []ghObs{{found: true, id: 7, busy: true}},
			wantDiag: "withdrawal incomplete; may accept work",
		},
		{
			name: "422 accepted job", list: []ghObs{{found: true, id: 7}},
			deleteCodes: []int{http.StatusUnprocessableEntity}, wantDelete: 1,
			wantDiag: "withdrawal incomplete; may accept work",
		},
		{
			name: "lookup error", list: []ghObs{{status: http.StatusInternalServerError}},
			wantDiag: "withdrawal incomplete; may accept work",
		},
		{
			name: "delete error", list: []ghObs{{found: true, id: 7}},
			deleteCodes: []int{http.StatusInternalServerError}, wantDelete: 1,
			wantDiag: "withdrawal incomplete; may accept work",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := deadlinePath(t)
			writeSessionDeadline(t, path, now.Add(50*time.Minute))
			p := loopProfile(t)
			p.Loop.SessionDeadlineFile = path
			clock := newTestClock(t, now)
			d := &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}}
			script := &ghScript{list: tc.list, deleteCodes: tc.deleteCodes}
			srv := newGHScript(t, script)
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock.cancelAt, clock.cancel = 3, cancel
			s := newWaitSupervisor(t, clock, d, srv)
			got, err := s.waitContainer(ctx, p, waitInfo(t), 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if got.forceRemove != tc.wantRemove {
				t.Fatalf("result=%+v", got)
			}
			if got.idleExpired {
				t.Fatalf("deadline withdrawal counted as idle timeout: %+v", got)
			}
			if script.deleteCalls < tc.wantDelete || d.rmN != 0 {
				t.Fatalf("delete=%d rm=%d", script.deleteCalls, d.rmN)
			}
			if tc.wantDiag != "" {
				saved, err := state.LoadLoopState(p.Loop.StateFile)
				if err != nil || saved.LastError == nil || !strings.Contains(*saved.LastError, tc.wantDiag) {
					t.Fatalf("diagnostic=%+v err=%v", saved, err)
				}
			}
		})
	}
}

func TestWaitSessionDeadlineWithdrawalRequiresTrustedDocker(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name   string
		docker *dockerScript
	}{
		{
			name: "inspect error",
			docker: &dockerScript{
				inspect:     [][]byte{[]byte("daemon down")},
				inspectErrs: []error{errors.New("exit status 1")},
			},
		},
		{
			name:   "created",
			docker: &dockerScript{inspect: [][]byte{containerInspectJSON("created", future, "0001-01-01T00:00:00Z", 0)}},
		},
		{
			name:   "unknown",
			docker: &dockerScript{inspect: [][]byte{containerInspectJSON("mystery", now.Format(time.RFC3339), now.Format(time.RFC3339), 0)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := deadlinePath(t)
			writeSessionDeadline(t, path, now.Add(50*time.Minute))
			p := loopProfile(t)
			p.Loop.SessionDeadlineFile = path
			clock := newTestClock(t, now)
			script := &ghScript{
				list:        []ghObs{{found: true, id: 7, busy: false}},
				deleteCodes: []int{http.StatusNoContent},
			}
			srv := newGHScript(t, script)
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock.cancelAt, clock.cancel = 2, cancel
			s := newWaitSupervisor(t, clock, tc.docker, srv)

			got, err := s.waitContainer(ctx, p, waitInfo(t), 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if got.forceRemove || script.listCalls != 0 || script.deleteCalls != 0 || tc.docker.rmN != 0 {
				t.Fatalf("untrusted Docker observation reached GitHub: result=%+v list=%d delete=%d rm=%d", got, script.listCalls, script.deleteCalls, tc.docker.rmN)
			}
			if !got.retained {
				t.Fatalf("expected retention after the stop budget, got %+v", got)
			}
		})
	}
}

func TestWaitSessionDeadlineMissingRunnerNeverAuthorizesRemoval(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, now.Add(50*time.Minute))
	p := loopProfile(t)
	p.Loop.SessionDeadlineFile = path
	clock := newTestClock(t, now)
	d := &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}}
	// The runner is seen busy first, then disappears twice. Without a DELETE
	// 204, an exhausted deadline must never force-remove the container.
	script := &ghScript{list: []ghObs{{found: true, id: 7, busy: true}, {}}}
	srv := newGHScript(t, script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.cancelAt, clock.cancel = 12, cancel
	s := newWaitSupervisor(t, clock, d, srv)

	got, err := s.waitContainer(ctx, p, waitInfo(t), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.forceRemove || script.deleteCalls != 0 || d.rmN != 0 {
		t.Fatalf("missing runner authorized removal without DELETE: result=%+v delete=%d rm=%d", got, script.deleteCalls, d.rmN)
	}
	if script.listCalls < 3 {
		t.Fatalf("expected repeated deadline observations, got %d", script.listCalls)
	}
	if !got.retained {
		t.Fatalf("expected retention after the stop budget, got %+v", got)
	}
}

func TestAdoptManagedSessionDeadlineWithdrawsIdleRunner(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, now.Add(50*time.Minute))
	p := loopProfile(t)
	p.Loop.SessionDeadlineFile = path
	settings, _ := validateLoopProfile(p)
	clock := newTestClock(t, now)
	d := &dockerScript{
		inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))},
		logs:    [][]byte{[]byte("safe log")},
	}
	ps, locks, unlocks := 0, 0, 0
	fallback := disallowGitHubServer(t)
	defer fallback.Close()
	s := cycleSupervisor(t, fallback, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if args[2] == "ps" {
			ps++
			if ps == 1 {
				return []byte("c\tgha-p-1\timage\trunning\tp\tp-1\n"), nil
			}
			return nil, nil
		}
		return d.Run(ctx, name, args...)
	}))
	script := &ghScript{list: []ghObs{{found: true, id: 7}}}
	srv := newGHScript(t, script)
	defer srv.Close()
	s.GitHub = gh.NewClient(srv.URL, "TEST_CYCLE_TOKEN", "", nil, srv.Client())
	s.Now = clock.Now
	s.TryLock = func() (func() error, bool, error) { locks++; return func() error { unlocks++; return nil }, true, nil }
	if err := s.adoptManaged(context.Background(), p, settings); err != nil {
		t.Fatal(err)
	}
	if script.deleteCalls != 1 || d.logsN != 1 || d.rmN != 1 || !d.rmForce[0] || locks != 1 || unlocks != 1 {
		t.Fatalf("delete=%d logs=%d rm=%v locks=%d unlocks=%d", script.deleteCalls, d.logsN, d.rmForce, locks, unlocks)
	}
	saved, err := state.LoadLoopState(p.Loop.StateFile)
	if err != nil || saved.LastError == nil || !strings.Contains(*saved.LastError, "idle runner withdrawn") {
		t.Fatalf("diagnostic=%+v err=%v", saved, err)
	}
}

// writeUnusableSessionDeadline creates each configured-but-unprovable deadline
// state. None of these may fall back to the unconfigured ordinary two-miss
// force-removal path.
func writeUnusableSessionDeadline(t *testing.T, path, kind string) {
	t.Helper()
	switch kind {
	case "missing":
	case "unreadable":
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	case "invalid json":
		if err := os.WriteFile(path, []byte(`{"deadline":`), 0o600); err != nil {
			t.Fatal(err)
		}
	case "invalid time":
		if err := os.WriteFile(path, []byte(`{"deadline":"tomorrow"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown deadline fixture %q", kind)
	}
}

func TestWaitUnusableSessionDeadlineProtectsOrdinaryModes(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, mode := range []struct {
		name    string
		fixture string
		policy  state.AdmissionPolicy
	}{
		{name: "mode1", policy: state.DefaultAdmissionPolicy()},
		{name: "mode2 grace elapsed", fixture: `{"mode":2,"delay_seconds":60}`, policy: state.AdmissionPolicy{Mode: state.AdmissionDelayed, DelaySeconds: 60}},
	} {
		for _, kind := range []string{"missing", "unreadable", "invalid json", "invalid time"} {
			t.Run(mode.name+" "+kind, func(t *testing.T) {
				path := deadlinePath(t)
				writeUnusableSessionDeadline(t, path, kind)
				p := loopProfile(t)
				p.Loop.SessionDeadlineFile = path
				if mode.fixture != "" {
					writeAdmissionFixture(t, p, mode.fixture)
				}
				clock := newTestClock(t, now)
				d := &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}}
				script := &ghScript{list: []ghObs{{found: true, id: 7, busy: true}, {}}}
				srv := newGHScript(t, script)
				defer srv.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				clock.cancelAt, clock.cancel = 6, cancel
				s := newWaitSupervisor(t, clock, d, srv)
				s.admissionPolicy = mode.policy

				got, err := s.waitContainer(ctx, p, waitInfo(t), 24*time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if got.forceRemove || got.idleExpired || script.deleteCalls != 0 || d.rmN != 0 {
					t.Fatalf("unusable deadline authorized removal: result=%+v deletes=%d rm=%d", got, script.deleteCalls, d.rmN)
				}
				if script.listCalls < 3 || !got.retained {
					t.Fatalf("protected withdrawal was not observed: result=%+v lookups=%d", got, script.listCalls)
				}
				if len(s.skippedJobs) != 0 {
					t.Fatalf("busy job skipped the queue: %v", s.skippedJobs)
				}
				saved, err := state.LoadLoopState(p.Loop.StateFile)
				if err != nil || saved.LastError == nil || !strings.Contains(*saved.LastError, "withdrawal incomplete; may accept work") {
					t.Fatalf("diagnostic=%+v err=%v", saved, err)
				}
			})
		}
	}
}

func TestWaitUnusableSessionDeadlineKeepsSafetyGates(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name       string
		docker     *dockerScript
		list       []ghObs
		deletes    []int
		wantRemove bool
		wantDelete int
		wantNoList bool
		wantDiag   string
	}{
		{
			name:       "valid docker idle delete 204",
			docker:     &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}},
			list:       []ghObs{{found: true, id: 7}},
			deletes:    []int{http.StatusNoContent},
			wantRemove: true, wantDelete: 1, wantDiag: "idle runner withdrawn",
		},
		{
			name:     "busy",
			docker:   &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}},
			list:     []ghObs{{found: true, id: 7, busy: true}},
			wantDiag: "withdrawal incomplete; may accept work",
		},
		{
			name:       "422 accepted job",
			docker:     &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}},
			list:       []ghObs{{found: true, id: 7}},
			deletes:    []int{http.StatusUnprocessableEntity},
			wantDelete: 1, wantDiag: "withdrawal incomplete; may accept work",
		},
		{
			name:     "lookup error",
			docker:   &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}},
			list:     []ghObs{{status: http.StatusInternalServerError}},
			wantDiag: "withdrawal incomplete; may accept work",
		},
		{
			name:       "missing runner",
			docker:     &dockerScript{inspect: [][]byte{runningInspect(now.Format(time.RFC3339), now.Format(time.RFC3339))}},
			list:       []ghObs{{}},
			wantNoList: false,
		},
		{
			name: "inspect error",
			docker: &dockerScript{
				inspect:     [][]byte{[]byte("daemon down")},
				inspectErrs: []error{errors.New("exit status 1")},
			},
			wantNoList: true,
		},
		{
			name:       "created",
			docker:     &dockerScript{inspect: [][]byte{containerInspectJSON("created", future, "0001-01-01T00:00:00Z", 0)}},
			wantNoList: true,
		},
		{
			name:       "unknown",
			docker:     &dockerScript{inspect: [][]byte{containerInspectJSON("mystery", now.Format(time.RFC3339), now.Format(time.RFC3339), 0)}},
			wantNoList: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := deadlinePath(t)
			writeUnusableSessionDeadline(t, path, "invalid json")
			p := loopProfile(t)
			p.Loop.SessionDeadlineFile = path
			clock := newTestClock(t, now)
			script := &ghScript{list: tc.list, deleteCodes: tc.deletes}
			srv := newGHScript(t, script)
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock.cancelAt, clock.cancel = 3, cancel
			s := newWaitSupervisor(t, clock, tc.docker, srv)

			got, err := s.waitContainer(ctx, p, waitInfo(t), 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if got.forceRemove != tc.wantRemove {
				t.Fatalf("result=%+v", got)
			}
			if script.deleteCalls < tc.wantDelete || tc.docker.rmN != 0 {
				t.Fatalf("delete=%d rm=%d", script.deleteCalls, tc.docker.rmN)
			}
			if tc.wantNoList && script.listCalls != 0 {
				t.Fatalf("untrusted Docker observation reached GitHub: lookups=%d", script.listCalls)
			}
			if tc.wantRemove {
				if got.idleExpired || got.retained {
					t.Fatalf("safe withdrawal was misclassified: %+v", got)
				}
			} else if !got.retained {
				t.Fatalf("expected retention, got %+v", got)
			}
			if tc.wantDiag != "" {
				saved, err := state.LoadLoopState(p.Loop.StateFile)
				if err != nil || saved.LastError == nil || !strings.Contains(*saved.LastError, tc.wantDiag) {
					t.Fatalf("diagnostic=%+v err=%v", saved, err)
				}
			}
		})
	}
}
