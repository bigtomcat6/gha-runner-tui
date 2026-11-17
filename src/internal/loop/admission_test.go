package loop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
)

func writeAdmissionFixture(t *testing.T, p config.Profile, data string) {
	t.Helper()
	if err := os.WriteFile(p.Loop.StateFile+".admission.json", []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

type admissionFixture struct {
	s              *Supervisor
	p              config.Profile
	clock          *testClock
	jobs           atomic.Value
	jobsCode       atomic.Int32
	registrations  atomic.Int32
	onRegistration func()
	runs           int
}

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	f := &admissionFixture{p: loopProfile(t), clock: newTestClock(t, time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))}
	f.jobs.Store(`{"jobs":[{"id":21,"status":"queued","labels":["linux"]}]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/me/app/actions/runs":
			if r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":9}]}`)
			} else {
				fmt.Fprint(w, `{"workflow_runs":[]}`)
			}
		case "/repos/me/app/actions/runs/9/jobs":
			if code := f.jobsCode.Load(); code != 0 {
				w.WriteHeader(int(code))
				return
			}
			fmt.Fprint(w, f.jobs.Load().(string))
		case "/repos/me/app/actions/runners/registration-token":
			f.registrations.Add(1)
			if f.onRegistration != nil {
				f.onRegistration()
			}
			fmt.Fprint(w, `{"token":"fake-registration-token"}`)
		default:
			t.Errorf("unexpected HTTP %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	d := &dockerScript{inspect: [][]byte{containerInspectJSON("exited", "2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z", 0)}}
	f.s = cycleSupervisor(t, srv, loopRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch args[2] {
		case "ps":
			return nil, nil
		case "run":
			f.runs++
			return []byte("container-id"), nil
		default:
			return d.Run(ctx, name, args...)
		}
	}))
	f.s.Now, f.s.Sleep = f.clock.Now, f.clock.Sleep
	return f
}

func TestAdmissionPauseDuringRegistrationPreventsLaunch(t *testing.T) {
	f := newAdmissionFixture(t)
	f.onRegistration = func() { writeAdmissionFixture(t, f.p, `{"mode":0,"delay_seconds":60}`) }
	got := f.cycle(t)
	if f.runs != 0 || f.registrations.Load() != 1 || got.state != state.LoopStatus("paused") {
		t.Fatalf("pause during token request ignored: result=%+v runs=%d", got, f.runs)
	}
}

func TestAdmissionPauseResetsDelayAndNormalBypassesIt(t *testing.T) {
	f := newAdmissionFixture(t)
	writeAdmissionFixture(t, f.p, `{"mode":2,"delay_seconds":60}`)
	f.cycle(t)
	f.clock.now = f.clock.now.Add(time.Minute)
	writeAdmissionFixture(t, f.p, `{"mode":0,"delay_seconds":60}`)
	f.cycle(t)
	writeAdmissionFixture(t, f.p, `{"mode":2,"delay_seconds":60}`)
	f.cycle(t)
	if f.runs != 0 {
		t.Fatal("resuming delayed mode reused a pre-pause timer")
	}
	writeAdmissionFixture(t, f.p, `{"mode":1,"delay_seconds":60}`)
	f.cycle(t)
	if f.runs != 1 {
		t.Fatal("normal mode continued waiting for the delay")
	}
}

func TestAdmissionQueueErrorRestartsGracePeriod(t *testing.T) {
	f := newAdmissionFixture(t)
	writeAdmissionFixture(t, f.p, `{"mode":2,"delay_seconds":60}`)
	f.cycle(t)
	f.clock.now = f.clock.now.Add(time.Minute)
	f.jobsCode.Store(503)
	settings, _ := validateLoopProfile(f.p)
	if _, err := f.s.runCycle(context.Background(), f.p, settings); err == nil {
		t.Fatal("queue error hidden")
	}
	f.jobsCode.Store(0)
	f.cycle(t)
	if f.runs != 0 {
		t.Fatal("stale queue evidence survived an API failure")
	}
}

func TestAdmissionPauseBusyRace422DoesNotAuthorizeRemoval(t *testing.T) {
	p := loopProfile(t)
	writeAdmissionFixture(t, p, `{"mode":0,"delay_seconds":60}`)
	clock := newTestClock(t, time.Date(2026, 10, 28, 0, 0, 5, 0, time.UTC))
	d := &dockerScript{inspect: [][]byte{
		runningInspect("2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z"),
		containerInspectJSON("exited", "2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z", 0),
	}}
	g := &ghScript{list: []ghObs{{found: true, id: 7}}, deleteCodes: []int{422}}
	srv := newGHScript(t, g)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, d, srv)
	got, err := s.waitContainer(context.Background(), p, waitInfo(t), 180*time.Second)
	if err != nil || got.forceRemove || got.exitCode == nil || *got.exitCode != 0 || d.rmN != 0 || g.deleteCalls != 1 {
		t.Fatalf("422 interrupted accepted job: result=%+v err=%v deletes=%d rm=%d", got, err, g.deleteCalls, d.rmN)
	}
}

func TestAdmissionRunReloadsModeWhileServiceStaysActive(t *testing.T) {
	f := newAdmissionFixture(t)
	f.s.ProfilePath = writeLoopProfile(t, f.p)
	writeAdmissionFixture(t, f.p, `{"mode":0,"delay_seconds":60}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	f.s.Sleep = func(_ context.Context, d time.Duration) error {
		checks++
		f.clock.now = f.clock.now.Add(d)
		got, err := state.LoadLoopState(f.p.Loop.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		switch checks {
		case 1:
			if string(got.State) != "paused" || f.runs != 0 {
				t.Fatalf("first state=%+v runs=%d", got, f.runs)
			}
			writeAdmissionFixture(t, f.p, `{"mode":2,"delay_seconds":60}`)
		case 2:
			if string(got.State) != "waiting-delay" || f.runs != 0 {
				t.Fatalf("second state=%+v runs=%d", got, f.runs)
			}
			writeAdmissionFixture(t, f.p, `{"mode":1,"delay_seconds":60}`)
		default:
			cancel()
		}
		return nil
	}
	if err := f.s.Run(ctx); err != nil || f.runs != 1 || checks != 3 {
		t.Fatalf("reload err=%v runs=%d checks=%d", err, f.runs, checks)
	}
}

func (f *admissionFixture) cycle(t *testing.T) cycleResult {
	t.Helper()
	settings, err := validateLoopProfile(f.p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.runCycle(context.Background(), f.p, settings)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAdmissionPausedAndNormalAreReloadedWithoutRestart(t *testing.T) {
	f := newAdmissionFixture(t)
	writeAdmissionFixture(t, f.p, `{"mode":0,"delay_seconds":60}`)
	got := f.cycle(t)
	if got.state != state.LoopStatus("paused") || f.runs != 0 || f.registrations.Load() != 0 {
		t.Fatalf("paused cycle=%+v runs=%d registrations=%d", got, f.runs, f.registrations.Load())
	}
	writeAdmissionFixture(t, f.p, `{"mode":1,"delay_seconds":60}`)
	f.cycle(t)
	if f.runs != 1 {
		t.Fatalf("normal mode did not resume: runs=%d", f.runs)
	}
}

func TestAdmissionDelayRequiresSameQueuedJobForFullGrace(t *testing.T) {
	f := newAdmissionFixture(t)
	writeAdmissionFixture(t, f.p, `{"mode":2,"delay_seconds":60}`)
	for _, seconds := range []int{0, 59} {
		f.clock.now = time.Date(2026, 10, 28, 0, 0, seconds, 0, time.UTC)
		if got := f.cycle(t); got.state != state.LoopStatus("waiting-delay") || f.runs != 0 || f.registrations.Load() != 0 {
			t.Fatalf("at %ds cycle=%+v runs=%d", seconds, got, f.runs)
		}
	}
	f.clock.now = f.clock.now.Add(time.Second)
	f.cycle(t)
	if f.runs != 1 {
		t.Fatalf("queued job not served after grace: runs=%d", f.runs)
	}
}

func TestAdmissionNewJobDoesNotInheritOldJobsDelay(t *testing.T) {
	f := newAdmissionFixture(t)
	writeAdmissionFixture(t, f.p, `{"mode":2,"delay_seconds":60}`)
	f.cycle(t)
	f.clock.now = f.clock.now.Add(59 * time.Second)
	f.jobs.Store(`{"jobs":[{"id":22,"status":"queued","labels":["linux"]}]}`)
	f.cycle(t)
	f.clock.now = f.clock.now.Add(time.Second)
	f.cycle(t)
	if f.runs != 0 {
		t.Fatal("new job inherited the previous job's grace period")
	}
	f.clock.now = f.clock.now.Add(59 * time.Second)
	f.cycle(t)
	if f.runs != 1 {
		t.Fatalf("new job never reached eligibility: runs=%d", f.runs)
	}
}

func TestAdmissionJobsTakenElsewhereDoNotStartRunner(t *testing.T) {
	f := newAdmissionFixture(t)
	writeAdmissionFixture(t, f.p, `{"mode":2,"delay_seconds":60}`)
	f.cycle(t)
	f.clock.now = f.clock.now.Add(time.Minute)
	f.jobs.Store(`{"jobs":[]}`)
	f.cycle(t)
	if f.runs != 0 || f.registrations.Load() != 0 {
		t.Fatal("started a fallback runner after the preferred host took the job")
	}
}

func TestAdmissionRecheckedAfterTakingHostLock(t *testing.T) {
	f := newAdmissionFixture(t)
	f.s.TryLock = func() (func() error, bool, error) {
		writeAdmissionFixture(t, f.p, `{"mode":0,"delay_seconds":60}`)
		return func() error { return nil }, true, nil
	}
	f.cycle(t)
	if f.runs != 0 || f.registrations.Load() != 0 {
		t.Fatal("pause during host lock acquisition was ignored")
	}
}

func TestAdmissionMalformedPolicyFailsClosed(t *testing.T) {
	for _, data := range []string{`{`, `{}`, `null`, `{"mode":3}`, `{"mode":2,"delay_seconds":-1}`} {
		t.Run(data, func(t *testing.T) {
			f := newAdmissionFixture(t)
			writeAdmissionFixture(t, f.p, data)
			settings, _ := validateLoopProfile(f.p)
			_, err := f.s.runCycle(context.Background(), f.p, settings)
			if err == nil || f.runs != 0 || f.registrations.Load() != 0 {
				t.Fatalf("invalid policy allowed launch: err=%v runs=%d", err, f.runs)
			}
		})
	}
}

func TestAdmissionPauseDrainsBusyRunnerAndWithdrawsIdleRunner(t *testing.T) {
	for _, busy := range []bool{true, false} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			p := loopProfile(t)
			writeAdmissionFixture(t, p, `{"mode":0,"delay_seconds":60}`)
			clock := newTestClock(t, time.Date(2026, 10, 28, 0, 0, 5, 0, time.UTC))
			d := &dockerScript{inspect: [][]byte{
				runningInspect("2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z"),
				containerInspectJSON("exited", "2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z", 0),
			}}
			g := &ghScript{list: []ghObs{{found: true, id: 7, busy: busy}}}
			srv := newGHScript(t, g)
			defer srv.Close()
			s := newWaitSupervisor(t, clock, d, srv)
			got, err := s.waitContainer(context.Background(), p, waitInfo(t), 180*time.Second)
			if err != nil || g.listCalls != 1 || d.rmN != 0 {
				t.Fatalf("result=%+v err=%v lookups=%d rm=%d", got, err, g.listCalls, d.rmN)
			}
			if busy && (g.deleteCalls != 0 || got.forceRemove || got.exitCode == nil) {
				t.Fatalf("busy runner was interrupted: result=%+v deletes=%d", got, g.deleteCalls)
			}
			if !busy && (g.deleteCalls != 1 || !got.forceRemove || got.idleExpired) {
				t.Fatalf("idle runner not safely withdrawn: result=%+v deletes=%d", got, g.deleteCalls)
			}
		})
	}
}

func TestAdmissionDelayedSwitchWithdrawsPreviouslyIdleRunner(t *testing.T) {
	p := loopProfile(t)
	writeAdmissionFixture(t, p, `{"mode":2,"delay_seconds":60}`)
	clock := newTestClock(t, time.Date(2026, 10, 28, 0, 0, 5, 0, time.UTC))
	d := &dockerScript{inspect: [][]byte{
		runningInspect("2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z"),
		containerInspectJSON("exited", "2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z", 0),
	}}
	g := &ghScript{list: []ghObs{{found: true, id: 7}}}
	srv := newGHScript(t, g)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, d, srv)
	s.admissionPolicy = state.DefaultAdmissionPolicy()
	got, err := s.waitContainer(context.Background(), p, waitInfo(t), 180*time.Second)
	if err != nil || !got.forceRemove || got.idleExpired || g.deleteCalls != 1 {
		t.Fatalf("normal-mode idle runner bypassed delayed admission: result=%+v err=%v deletes=%d", got, err, g.deleteCalls)
	}
}

func TestAdmissionDelayedRunnerThatPassedGraceCanAcceptJob(t *testing.T) {
	p := loopProfile(t)
	writeAdmissionFixture(t, p, `{"mode":2,"delay_seconds":60}`)
	clock := newTestClock(t, time.Date(2026, 10, 28, 0, 0, 5, 0, time.UTC))
	d := &dockerScript{inspect: [][]byte{
		runningInspect("2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z"),
		containerInspectJSON("exited", "2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z", 0),
	}}
	srv := disallowGitHubServer(t)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, d, srv)
	s.admissionPolicy = state.AdmissionPolicy{Mode: state.AdmissionDelayed, DelaySeconds: 60}
	got, err := s.waitContainer(context.Background(), p, waitInfo(t), 180*time.Second)
	if err != nil || got.forceRemove || got.exitCode == nil || *got.exitCode != 0 {
		t.Fatalf("eligible delayed runner withdrawn: result=%+v err=%v", got, err)
	}
}

func TestAdmissionNormalModeDoesNotBypassSessionDeadline(t *testing.T) {
	f := newAdmissionFixture(t)
	path := deadlinePath(t)
	writeSessionDeadline(t, path, f.clock.now.Add(-time.Minute))
	f.p.Loop.SessionDeadlineFile = path
	writeAdmissionFixture(t, f.p, `{"mode":1,"delay_seconds":60}`)
	settings, _ := validateLoopProfile(f.p)
	_, err := f.s.runCycle(context.Background(), f.p, settings)
	if !errors.Is(err, errSessionDeadline) || f.runs != 0 || f.registrations.Load() != 0 {
		t.Fatalf("normal mode bypassed the deadline floor: err=%v runs=%d registrations=%d", err, f.runs, f.registrations.Load())
	}
}

func TestAdmissionWithdrawalMissingRunnerNeverAuthorizesRemoval(t *testing.T) {
	p := loopProfile(t)
	writeAdmissionFixture(t, p, `{"mode":0,"delay_seconds":60}`)
	created, started := "2026-10-28T00:00:00Z", "2026-10-28T00:00:00Z"
	clock := newTestClock(t, time.Date(2026, 10, 28, 0, 0, 5, 0, time.UTC))
	d := &dockerScript{inspect: [][]byte{
		runningInspect(created, started),
		runningInspect(created, started),
		runningInspect(created, started),
		containerInspectJSON("exited", created, started, 0),
	}}
	g := &ghScript{list: []ghObs{{found: true, id: 7, busy: true}, {found: false}}}
	srv := newGHScript(t, g)
	defer srv.Close()
	s := newWaitSupervisor(t, clock, d, srv)
	got, err := s.waitContainer(context.Background(), p, waitInfo(t), 180*time.Second)
	if err != nil || got.forceRemove || got.idleExpired || got.exitCode == nil || *got.exitCode != 0 || g.deleteCalls != 0 || d.rmN != 0 {
		t.Fatalf("missing runner authorized removal during pause: result=%+v err=%v deletes=%d rm=%d", got, err, g.deleteCalls, d.rmN)
	}
}

func TestAdmissionWithdrawalRequiresTrustedDocker(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
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
		{name: "created", docker: &dockerScript{inspect: [][]byte{containerInspectJSON("created", future, "0001-01-01T00:00:00Z", 0)}}},
		{name: "unknown", docker: &dockerScript{inspect: [][]byte{containerInspectJSON("mystery", now.Format(time.RFC3339), now.Format(time.RFC3339), 0)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := loopProfile(t)
			writeAdmissionFixture(t, p, `{"mode":0,"delay_seconds":60}`)
			clock := newTestClock(t, now.Add(5*time.Second))
			script := &ghScript{list: []ghObs{{found: true, id: 7}}, deleteCodes: []int{http.StatusNoContent}}
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
