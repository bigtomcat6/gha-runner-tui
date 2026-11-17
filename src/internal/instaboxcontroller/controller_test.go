package instaboxcontroller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testMachineA = "m_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	testMachineB = "m_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	testMachineC = "m_cccccccccccccccccccccccccc"
)

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func testMachineConfig(id string) MachineConfig {
	return MachineConfig{
		MachineID:    id,
		DeadlinePath: "/var/lib/gha-runner-tui/state/session-deadline.json",
		SSHHost:      "runner-a.example.net",
		SSHUser:      "dev",
		SSHPort:      22,
	}
}

func testConfig(t *testing.T, ids ...string) Config {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{testMachineA}
	}
	cfg := Config{StateDir: t.TempDir()}
	for i, id := range ids {
		m := testMachineConfig(id)
		if i == 1 {
			m.SSHHost = "runner-b.example.net"
		}
		cfg.Machines = append(cfg.Machines, m)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return cfg
}

func machineStarted(started time.Time, maxRun int, ready bool) Machine {
	raw := started.UTC().Format(time.RFC3339)
	return Machine{
		ID:            testMachineA,
		State:         "running",
		DesiredState:  "running",
		Runtime:       Runtime{Ready: ready},
		RunStartedAt:  &raw,
		MaxRunMinutes: &maxRun,
	}
}

func runningMachine(id string, started time.Time, maxRun int) Machine {
	m := machineStarted(started, maxRun, true)
	m.ID = id
	return m
}

func suspendedMachine(id string, autoSuspendedAt time.Time) Machine {
	raw := autoSuspendedAt.UTC().Format(time.RFC3339)
	return Machine{
		ID:              id,
		State:           "suspended",
		DesiredState:    "suspended",
		AutoSuspendedAt: &raw,
	}
}

func stringPtr(value string) *string { return &value }

type fakeAPI struct {
	getFunc  func(context.Context, string) (Machine, error)
	wakeFunc func(context.Context, string) (Machine, error)
	gets     int
	wakes    int
	wakeIDs  []string
}

func (f *fakeAPI) GetMachine(ctx context.Context, id string) (Machine, error) {
	f.gets++
	if f.getFunc == nil {
		return Machine{}, errors.New("unexpected GET")
	}
	return f.getFunc(ctx, id)
}

func (f *fakeAPI) WakeMachine(ctx context.Context, id string) (Machine, error) {
	f.wakes++
	f.wakeIDs = append(f.wakeIDs, id)
	if f.wakeFunc == nil {
		return Machine{}, errors.New("unexpected wake")
	}
	return f.wakeFunc(ctx, id)
}

type fakeWriter struct {
	reads      int
	writes     int
	content    map[string]string
	readErr    error
	writeErr   error
	readback   string
	onRead     func(RemoteTarget) error
	onWrite    func(RemoteTarget, string)
	lastTarget RemoteTarget
	lastWrite  string
}

func (w *fakeWriter) ReadDeadline(ctx context.Context, target RemoteTarget) (string, error) {
	w.reads++
	if w.onRead != nil {
		if err := w.onRead(target); err != nil {
			return "", err
		}
	}
	if w.readErr != nil {
		return "", w.readErr
	}
	return w.content[target.key()], nil
}

func (w *fakeWriter) WriteDeadline(ctx context.Context, target RemoteTarget, content string) (string, error) {
	w.writes++
	w.lastTarget = target
	w.lastWrite = content
	if w.onWrite != nil {
		w.onWrite(target, content)
	}
	if w.writeErr != nil {
		return "", w.writeErr
	}
	if w.readback != "" {
		return w.readback, nil
	}
	if w.content == nil {
		w.content = map[string]string{}
	}
	w.content[target.key()] = content
	return content, nil
}

func newController(cfg Config, api MachineAPI, writer DeadlineWriter, now func() time.Time, log *bytes.Buffer) *Controller {
	controller := &Controller{Config: cfg, API: api, Writer: writer, Now: now}
	if log != nil {
		controller.Logger = logAdapter{log}
	}
	return controller
}

type logAdapter struct{ buffer *bytes.Buffer }

func (l logAdapter) Printf(format string, args ...any) {
	l.buffer.WriteString(fmt.Sprintf(format, args...) + "\n")
}

func readStateFile(t *testing.T, dir string) controllerState {
	t.Helper()
	state, err := loadState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return state
}

func TestComputeBudgetCapsAndWindow(t *testing.T) {
	started := testNow
	for _, tc := range []struct {
		name      string
		machine   func() Machine
		now       time.Time
		wantErr   bool
		wantDead  time.Time
		wantReady bool
	}{
		{
			name:      "free plan grown to 480 is capped at two hours",
			machine:   func() Machine { return runningMachine(testMachineA, started, 480) },
			now:       started.Add(30 * time.Minute),
			wantDead:  started.Add(120 * time.Minute),
			wantReady: false, // only 90 minutes left
		},
		{
			name:      "unlimited paid run keeps the two-hour cap",
			machine:   func() Machine { return runningMachine(testMachineA, started, -1) },
			now:       started,
			wantDead:  started.Add(120 * time.Minute),
			wantReady: true,
		},
		{
			name:      "shorter max run limits the deadline",
			machine:   func() Machine { return runningMachine(testMachineA, started, 60) },
			now:       started,
			wantDead:  started.Add(60 * time.Minute),
			wantReady: false,
		},
		{
			name:      "zero max run is already over",
			machine:   func() Machine { return runningMachine(testMachineA, started, 0) },
			now:       started,
			wantDead:  started,
			wantReady: false,
		},
		{
			name:    "missing max run is unknown",
			machine: func() Machine { m := runningMachine(testMachineA, started, 120); m.MaxRunMinutes = nil; return m },
			now:     started,
			wantErr: true,
		},
		{
			name:    "invalid max run is unknown",
			machine: func() Machine { return runningMachine(testMachineA, started, -2) },
			now:     started,
			wantErr: true,
		},
		{
			name:    "missing run start is unknown",
			machine: func() Machine { m := runningMachine(testMachineA, started, 120); m.RunStartedAt = nil; return m },
			now:     started,
			wantErr: true,
		},
		{
			name: "invalid run start is unknown",
			machine: func() Machine {
				m := runningMachine(testMachineA, started, 120)
				m.RunStartedAt = stringPtr("soon")
				return m
			},
			now:     started,
			wantErr: true,
		},
		{
			name:    "far future run start is a clock anomaly",
			machine: func() Machine { return runningMachine(testMachineA, started.Add(10*time.Minute), 120) },
			now:     started,
			wantErr: true,
		},
		{
			name:    "paused machine has no run budget",
			machine: func() Machine { return suspendedMachine(testMachineA, started) },
			now:     started,
			wantErr: true,
		},
		{
			name:    "runtime not ready",
			machine: func() Machine { m := runningMachine(testMachineA, started, 120); m.Runtime.Ready = false; return m },
			now:     started,
			wantErr: true,
		},
		{
			name: "host unreachable",
			machine: func() Machine {
				m := runningMachine(testMachineA, started, 120)
				m.Runtime.HostUnreachable = true
				return m
			},
			now:     started,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget, err := computeBudget(tc.machine(), tc.now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !budget.Deadline.Equal(tc.wantDead) {
				t.Fatalf("deadline = %s, want %s", budget.Deadline, tc.wantDead)
			}
			if got := budgetReady(budget, tc.now); got != tc.wantReady {
				t.Fatalf("ready = %t, want %t (remaining %s)", got, tc.wantReady, budget.remaining(tc.now))
			}
		})
	}
}

func TestBudgetReadyOnlyInsideTheTwentyMinuteWindow(t *testing.T) {
	started := testNow
	budget, err := computeBudget(runningMachine(testMachineA, started, 120), started)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		now  time.Time
		want bool
	}{
		{now: started, want: true},
		{now: started.Add(20 * time.Minute), want: true},
		{now: started.Add(20*time.Minute + time.Second), want: false},
		{now: started.Add(time.Minute), want: true}, // would be 121m only for a future start
		{now: started.Add(-time.Minute), want: false},
	} {
		if got := budgetReady(budget, tc.now); got != tc.want {
			t.Fatalf("now=%s ready=%t, want %t", tc.now, got, tc.want)
		}
	}
}

func TestDeadlinePayloadMatchesTheLoopSchema(t *testing.T) {
	deadline := time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC)
	if got := deadlinePayload(deadline); got != `{"deadline":"2026-11-17T20:00:00Z"}`+"\n" {
		t.Fatalf("payload = %q", got)
	}
}

func TestDefaultConfigurationMakesNoRemoteWritesOrWakes(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	now := testNow
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		if id == testMachineA {
			return runningMachine(testMachineA, now, 120), nil
		}
		return suspendedMachine(testMachineB, now.Add(-10*time.Minute)), nil
	}}
	writer := &fakeWriter{}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, api, writer, func() time.Time { return now }, logBuffer)
	defer controller.Close()

	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 0 || writer.reads != 0 {
		t.Fatalf("writes=%d reads=%d, want none", writer.writes, writer.reads)
	}
	if api.wakes != 0 {
		t.Fatalf("wakes=%d, want none", api.wakes)
	}
	if !strings.Contains(logBuffer.String(), "remote writes disabled") {
		t.Fatalf("log = %s", logBuffer.String())
	}
	if state := readStateFile(t, cfg.StateDir); len(state.WakeAttempts) != 0 {
		t.Fatalf("wake attempts = %+v", state.WakeAttempts)
	}
}

func TestDeliveryOncePerRunAndOnDeadlineChange(t *testing.T) {
	cfg := testConfig(t, testMachineA)
	cfg.EnableRemoteWrite = true
	now := testNow
	started := testNow
	maxRun := 120
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, started, maxRun), nil
	}}
	writer := &fakeWriter{}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, api, writer, func() time.Time { return now }, logBuffer)
	defer controller.Close()

	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d, want 1", writer.writes)
	}
	if want := deadlinePayload(started.Add(120 * time.Minute)); writer.lastWrite != want {
		t.Fatalf("written = %q, want %q", writer.lastWrite, want)
	}
	if !strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("log = %s", logBuffer.String())
	}
	state := readStateFile(t, cfg.StateDir)
	entry := state.Deliveries[testMachineA]
	if entry.Status != "confirmed" || entry.Deadline != started.Add(120*time.Minute).Format(time.RFC3339) {
		t.Fatalf("delivery state = %+v", entry)
	}

	// Same run and deadline: a fresh GET only, no second SSH.
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 || writer.reads != 0 {
		t.Fatalf("second round writes=%d reads=%d, want no SSH", writer.writes, writer.reads)
	}

	// The platform shortened maxRunMinutes during the same run: deliver the
	// earlier deadline even though it is already outside the ready window.
	now = started.Add(5 * time.Minute)
	maxRun = 60
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 2 {
		t.Fatalf("deadline change writes = %d, want 2", writer.writes)
	}
	if want := deadlinePayload(started.Add(60 * time.Minute)); writer.lastWrite != want {
		t.Fatalf("changed write = %q, want %q", writer.lastWrite, want)
	}

	// A new run always gets its own deadline.
	now = started.Add(31 * time.Minute)
	started = started.Add(30 * time.Minute)
	maxRun = 120
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 3 {
		t.Fatalf("new run writes = %d, want 3", writer.writes)
	}
	if want := deadlinePayload(started.Add(120 * time.Minute)); writer.lastWrite != want {
		t.Fatalf("new run write = %q, want %q", writer.lastWrite, want)
	}
}

func TestPostWriteIdentityChangeIsNotReady(t *testing.T) {
	cfg := testConfig(t, testMachineA)
	cfg.EnableRemoteWrite = true
	now := testNow
	getCalls := 0
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		getCalls++
		switch getCalls {
		case 1:
			return runningMachine(testMachineA, now, 120), nil
		case 2:
			return runningMachine(testMachineA, now.Add(-time.Minute), 120), nil
		default:
			return runningMachine(testMachineA, now.Add(-time.Minute), 120), nil
		}
	}}
	writer := &fakeWriter{}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, api, writer, func() time.Time { return now }, logBuffer)
	defer controller.Close()

	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the changed identity to be reported")
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d, want 1", writer.writes)
	}
	if !strings.Contains(logBuffer.String(), "budget-ready=false") {
		t.Fatalf("first round log = %s", logBuffer.String())
	}

	logBuffer.Reset()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 2 {
		t.Fatalf("new run writes = %d, want 2", writer.writes)
	}
	if !strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("second round log = %s", logBuffer.String())
	}
}

func TestPostWriteGETFailureIsNotReadyButDoesNotRewRite(t *testing.T) {
	cfg := testConfig(t, testMachineA)
	cfg.EnableRemoteWrite = true
	now := testNow
	getCalls := 0
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		getCalls++
		if getCalls == 2 {
			return Machine{}, errors.New("network hiccup")
		}
		return runningMachine(testMachineA, now, 120), nil
	}}
	writer := &fakeWriter{}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, api, writer, func() time.Time { return now }, logBuffer)
	defer controller.Close()

	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the post-write GET failure to be reported")
	}
	if writer.writes != 1 || !strings.Contains(logBuffer.String(), "budget-ready=false") {
		t.Fatalf("writes=%d log=%s", writer.writes, logBuffer.String())
	}
	logBuffer.Reset()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("confirmed delivery was rewritten: writes=%d", writer.writes)
	}
	if !strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("log = %s", logBuffer.String())
	}
}

func TestUnknownWriteIsConfirmedOrReplacedOnlyAfterReading(t *testing.T) {
	run := func(t *testing.T, contentAfterAmbiguous string, readErr error, wantReads, wantWrites int, wantReady bool) {
		t.Helper()
		cfg := testConfig(t, testMachineA)
		cfg.EnableRemoteWrite = true
		now := testNow
		api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
			return runningMachine(testMachineA, now, 120), nil
		}}
		writer := &fakeWriter{}
		expected := deadlinePayload(now.Add(120 * time.Minute))
		writer.onWrite = func(target RemoteTarget, content string) {
			if writer.content == nil {
				writer.content = map[string]string{}
			}
			// The write may have landed even though the answer was lost.
			writer.content[target.key()] = contentAfterAmbiguous
		}
		writer.writeErr = errors.New("connection lost")
		logBuffer := &bytes.Buffer{}
		controller := newController(cfg, api, writer, func() time.Time { return now }, logBuffer)
		defer controller.Close()

		if err := controller.RunOnce(context.Background()); err == nil {
			t.Fatal("expected the unknown write to be reported")
		}
		if writer.writes != 1 || !strings.Contains(logBuffer.String(), "budget-ready=false") {
			t.Fatalf("first round writes=%d log=%s", writer.writes, logBuffer.String())
		}
		state := readStateFile(t, cfg.StateDir)
		if state.Deliveries[testMachineA].Status != "pending" {
			t.Fatalf("state = %+v", state.Deliveries[testMachineA])
		}

		writer.writeErr = nil
		writer.onWrite = nil
		writer.readErr = readErr
		if contentAfterAmbiguous != expected {
			writer.content[remoteTarget(cfg.Machines[0]).key()] = contentAfterAmbiguous
		}
		logBuffer.Reset()
		_ = controller.RunOnce(context.Background())
		if writer.reads != wantReads || writer.writes != wantWrites {
			t.Fatalf("reads=%d writes=%d, want %d/%d", writer.reads, writer.writes, wantReads, wantWrites)
		}
		ready := strings.Contains(logBuffer.String(), "budget-ready=true")
		if ready != wantReady {
			t.Fatalf("ready=%t, want %t; log=%s", ready, wantReady, logBuffer.String())
		}
	}

	expected := deadlinePayload(testNow.Add(120 * time.Minute))
	t.Run("matching readback confirms without a second write", func(t *testing.T) {
		run(t, expected, nil, 1, 1, true)
	})
	t.Run("read failure keeps pending and does not overwrite", func(t *testing.T) {
		run(t, expected, errors.New("ssh down"), 1, 1, false)
	})
	t.Run("different content is replaced", func(t *testing.T) {
		run(t, `{"deadline":"2026-11-17T13:00:00Z"}`, nil, 1, 2, true)
	})
}

func TestGETFailureDoesNotExtendOrTriggerWake(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableRemoteWrite = true
	cfg.EnableWake = true
	now := testNow
	failA := false
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		if id == testMachineA {
			if failA {
				return Machine{}, errors.New("GET unavailable")
			}
			return runningMachine(testMachineA, now, 120), nil
		}
		return suspendedMachine(testMachineB, now.Add(-10*time.Minute)), nil
	}}
	writer := &fakeWriter{}
	controller := newController(cfg, api, writer, func() time.Time { return now }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 0 {
		t.Fatalf("wake with plenty of budget: %d", api.wakes)
	}

	failA = true
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the GET failure to be reported")
	}
	if api.wakes != 0 {
		t.Fatalf("GET failure triggered %d wakes", api.wakes)
	}
}

func TestWakeThresholds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		offset   time.Duration
		maxRun   int
		wantWake int
	}{
		{name: "no safe budget-ready machine", offset: 0, maxRun: 60, wantWake: 1},
		{name: "only ready machine has 110 minutes", offset: 10 * time.Minute, maxRun: 120, wantWake: 0},
		{name: "only ready machine has exactly 105 minutes", offset: 15 * time.Minute, maxRun: 120, wantWake: 1},
		{name: "only ready machine has 104 minutes", offset: 16 * time.Minute, maxRun: 120, wantWake: 1},
		{name: "only ready machine has 95 minutes", offset: 25 * time.Minute, maxRun: 120, wantWake: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, testMachineA, testMachineB)
			cfg.EnableRemoteWrite = true
			cfg.EnableWake = true
			started := testNow
			now := started.Add(tc.offset)
			api := &fakeAPI{
				getFunc: func(ctx context.Context, id string) (Machine, error) {
					if id == testMachineA {
						return runningMachine(testMachineA, started, tc.maxRun), nil
					}
					return suspendedMachine(testMachineB, started.Add(-10*time.Minute)), nil
				},
				wakeFunc: func(ctx context.Context, id string) (Machine, error) { return Machine{}, nil },
			}
			writer := &fakeWriter{}
			controller := newController(cfg, api, writer, func() time.Time { return now }, &bytes.Buffer{})
			defer controller.Close()
			if err := controller.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if api.wakes != tc.wantWake {
				t.Fatalf("wakes = %d, want %d", api.wakes, tc.wantWake)
			}
			if tc.wantWake == 1 && (len(api.wakeIDs) != 1 || api.wakeIDs[0] != testMachineB) {
				t.Fatalf("wake ids = %v", api.wakeIDs)
			}
		})
	}
}

func TestWakeWithMoreThanOneReadyMachineDoesNothing(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB, testMachineC)
	cfg.EnableRemoteWrite = true
	cfg.EnableWake = true
	now := testNow
	api := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) {
			if id == testMachineC {
				return suspendedMachine(testMachineC, now.Add(-10*time.Minute)), nil
			}
			return runningMachine(id, now, 120), nil
		},
		wakeFunc: func(ctx context.Context, id string) (Machine, error) {
			t.Fatalf("unexpected wake of %s", id)
			return Machine{}, nil
		},
	}
	controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return now }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 0 {
		t.Fatalf("wakes = %d", api.wakes)
	}
}

func TestWakeSafetySkips(t *testing.T) {
	auto := testNow.Add(-10 * time.Minute)
	for _, tc := range []struct {
		name    string
		machine func() Machine
	}{
		{name: "manual suspend has no autoSuspendedAt", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.AutoSuspendedAt = nil
			return m
		}},
		{name: "manual pause with empty auto marker", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.AutoSuspendedAt = stringPtr("")
			return m
		}},
		{name: "cooldown not over", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.CooldownUntil = stringPtr(testNow.Add(5 * time.Minute).Format(time.RFC3339))
			return m
		}},
		{name: "abuse cooldown reason", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.CooldownReason = stringPtr("abuse_cpu")
			return m
		}},
		{name: "invalid cooldown time", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.CooldownUntil = stringPtr("later")
			return m
		}},
		{name: "host unreachable", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.Runtime.HostUnreachable = true
			return m
		}},
		{name: "active operation", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.OperationID = stringPtr("op_123")
			return m
		}},
		{name: "manual stop", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.State = "stopped"
			m.DesiredState = "stopped"
			return m
		}},
		{name: "error state", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.State = "error"
			return m
		}},
		{name: "resuming", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.State = "resuming"
			return m
		}},
		{name: "desired state still running", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.DesiredState = "running"
			return m
		}},
		{name: "invalid auto time", machine: func() Machine {
			m := suspendedMachine(testMachineB, auto)
			m.AutoSuspendedAt = stringPtr("yesterday")
			return m
		}},
		{name: "future auto time", machine: func() Machine {
			return suspendedMachine(testMachineB, testNow.Add(10*time.Minute))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, testMachineA, testMachineB)
			cfg.EnableWake = true
			started := testNow
			now := started
			api := &fakeAPI{
				getFunc: func(ctx context.Context, id string) (Machine, error) {
					if id == testMachineA {
						return runningMachine(testMachineA, started, 60), nil
					}
					return tc.machine(), nil
				},
				wakeFunc: func(ctx context.Context, id string) (Machine, error) {
					t.Fatalf("unsafe wake of %s", id)
					return Machine{}, nil
				},
			}
			controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return now }, &bytes.Buffer{})
			defer controller.Close()
			if err := controller.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if api.wakes != 0 {
				t.Fatalf("wakes = %d", api.wakes)
			}
		})
	}
}

func TestWakeIsOncePerCycleAndSurvivesRestart(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableWake = true
	started := testNow
	firstCycle := started.Add(-10 * time.Minute)
	secondCycle := started.Add(-5 * time.Minute)
	cycle := firstCycle
	api := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) {
			if id == testMachineA {
				return runningMachine(testMachineA, started, 60), nil
			}
			return suspendedMachine(testMachineB, cycle), nil
		},
		wakeFunc: func(ctx context.Context, id string) (Machine, error) { return Machine{}, nil },
	}
	clear := func() *Controller {
		return newController(cfg, api, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
	}
	controller := clear()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 1 {
		t.Fatalf("first round wakes = %d", api.wakes)
	}
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 1 {
		t.Fatalf("second round wakes = %d", api.wakes)
	}
	controller.Close()

	restarted := clear()
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 1 {
		t.Fatalf("restart resent wake: %d", api.wakes)
	}
	restarted.Close()

	cycle = secondCycle
	aThird := clear()
	if err := aThird.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 2 {
		t.Fatalf("new suspend cycle wakes = %d, want 2", api.wakes)
	}
	aThird.Close()
}

func TestWakeIsAtMostOnePerRound(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB, testMachineC)
	cfg.EnableWake = true
	started := testNow
	api := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) {
			if id == testMachineA {
				return runningMachine(testMachineA, started, 60), nil
			}
			return suspendedMachine(id, started.Add(-10*time.Minute)), nil
		},
		wakeFunc: func(ctx context.Context, id string) (Machine, error) { return Machine{}, nil },
	}
	controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 1 || len(api.wakeIDs) != 1 || api.wakeIDs[0] != testMachineB {
		t.Fatalf("wake ids = %v", api.wakeIDs)
	}
}

func TestUnknownWakeIsNotResent(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableWake = true
	started := testNow
	cycle := started.Add(-10 * time.Minute)
	api := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) {
			if id == testMachineA {
				return runningMachine(testMachineA, started, 60), nil
			}
			return suspendedMachine(testMachineB, cycle), nil
		},
		wakeFunc: func(ctx context.Context, id string) (Machine, error) {
			state, err := loadState(filepath.Join(cfg.StateDir, "state.json"))
			if err != nil {
				t.Fatalf("wake ran before the attempt was persisted: %v", err)
			}
			key := testMachineB + "|" + cycle.Format(time.RFC3339)
			if attempt, ok := state.WakeAttempts[key]; !ok || attempt.Status != "pending" {
				t.Fatalf("wake attempt was not persisted before POST: %+v", state.WakeAttempts)
			}
			return Machine{}, errors.New("context deadline exceeded")
		},
	}
	makeController := func() *Controller {
		return newController(cfg, api, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
	}
	controller := makeController()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the unknown wake to be reported")
	}
	if api.wakes != 1 {
		t.Fatalf("wakes = %d", api.wakes)
	}
	if state := readStateFile(t, cfg.StateDir); state.WakeAttempts[testMachineB+"|"+cycle.Format(time.RFC3339)].Status != "unknown" {
		t.Fatalf("wake attempts = %+v", state.WakeAttempts)
	}
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 1 {
		t.Fatalf("unknown wake was resent: %d", api.wakes)
	}
	controller.Close()
	restarted := makeController()
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 1 {
		t.Fatalf("restart resent an unknown wake: %d", api.wakes)
	}
	restarted.Close()
}

func TestRefusedWakeIsNotRetried(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableWake = true
	started := testNow
	api := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) {
			if id == testMachineA {
				return runningMachine(testMachineA, started, 60), nil
			}
			return suspendedMachine(testMachineB, started.Add(-10*time.Minute)), nil
		},
		wakeFunc: func(ctx context.Context, id string) (Machine, error) {
			return Machine{}, &APIError{Status: 409, Code: "machine_cooldown", Retryable: true}
		},
	}
	controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the refused wake to be reported")
	}
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 1 {
		t.Fatalf("refused wake was retried: %d", api.wakes)
	}
}

func TestWakeNeedsExplicitEnableAndNotDryRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enable bool
		dryRun bool
	}{
		{name: "disabled by default"},
		{name: "dry run", enable: true, dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, testMachineA, testMachineB)
			cfg.EnableWake = tc.enable
			started := testNow
			api := &fakeAPI{
				getFunc: func(ctx context.Context, id string) (Machine, error) {
					if id == testMachineA {
						return runningMachine(testMachineA, started, 60), nil
					}
					return suspendedMachine(testMachineB, started.Add(-10*time.Minute)), nil
				},
				wakeFunc: func(ctx context.Context, id string) (Machine, error) {
					t.Fatalf("unexpected wake of %s", id)
					return Machine{}, nil
				},
			}
			controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
			controller.DryRun = tc.dryRun
			defer controller.Close()
			if err := controller.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if api.wakes != 0 {
				t.Fatalf("wakes = %d", api.wakes)
			}
		})
	}
}

func TestCorruptStateFailsClosedBeforeNetwork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "garbage", content: "not state"},
		{name: "unsupported version", content: `{"version":9,"deliveries":{},"wakeAttempts":{}}`},
		{name: "unknown field", content: `{"version":1,"deliveries":{},"wakeAttempts":{},"extra":true}`},
		{name: "wrong types", content: `{"version":1,"deliveries":[],"wakeAttempts":{}}`},
		{name: "trailing data", content: `{"version":1,"deliveries":{},"wakeAttempts":{}} junk`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, testMachineA)
			cfg.EnableRemoteWrite = true
			cfg.EnableWake = true
			if err := os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
				t.Fatalf("GET for %s happened despite corrupt state", id)
				return Machine{}, nil
			}}
			writer := &fakeWriter{}
			controller := newController(cfg, api, writer, func() time.Time { return testNow }, &bytes.Buffer{})
			defer controller.Close()
			if err := controller.RunOnce(context.Background()); err == nil {
				t.Fatal("expected corrupt state to fail the round")
			}
			if api.gets != 0 || api.wakes != 0 || writer.writes != 0 || writer.reads != 0 {
				t.Fatalf("gets=%d wakes=%d writes=%d reads=%d", api.gets, api.wakes, writer.writes, writer.reads)
			}
		})
	}
}

func TestSecondInstanceCannotWriteOrWake(t *testing.T) {
	cfg := testConfig(t, testMachineA)
	cfg.EnableRemoteWrite = true
	cfg.EnableWake = true
	started := testNow
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, started, 120), nil
	}}
	writer := &fakeWriter{}
	first := newController(cfg, api, writer, func() time.Time { return started }, &bytes.Buffer{})
	if err := first.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("first writes = %d", writer.writes)
	}

	secondAPI := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, started, 120), nil
	}}
	secondWriter := &fakeWriter{}
	second := newController(cfg, secondAPI, secondWriter, func() time.Time { return started }, &bytes.Buffer{})
	if err := second.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("second instance error = %v", err)
	}
	if secondAPI.gets != 0 || secondAPI.wakes != 0 || secondWriter.writes != 0 {
		t.Fatalf("second instance acted while locked: gets=%d wakes=%d writes=%d", secondAPI.gets, secondAPI.wakes, secondWriter.writes)
	}
	first.Close()

	if err := second.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if secondWriter.writes != 0 {
		t.Fatalf("confirmed delivery was written twice: %d", secondWriter.writes)
	}
	second.Close()
}

func TestStateFilePermissions(t *testing.T) {
	cfg := testConfig(t, testMachineA)
	cfg.EnableRemoteWrite = true
	now := testNow
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, now, 120), nil
	}}
	controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return now }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(cfg.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
}

func TestLoadConfigValidation(t *testing.T) {
	validMachine := func(id, host string) string {
		return fmt.Sprintf(`{"machineId":%q,"deadlinePath":"/var/lib/gha-runner-tui/state/session-deadline.json","sshHost":%q,"sshUser":"dev"}`, id, host)
	}
	valid := func(machine string) string {
		return fmt.Sprintf(`{"pollIntervalSeconds":30,"stateDir":"/var/lib/instabox-controller","enableRemoteWrite":false,"enableWake":false,"machines":[%s]}`, machine)
	}
	base := valid(validMachine(testMachineA, "runner.example.net"))
	if _, err := LoadConfig(writeConfigFile(t, base)); err != nil {
		t.Fatalf("base config should be valid: %v", err)
	}

	for _, tc := range []struct {
		name string
		json string
	}{
		{name: "api base URL is not a setting", json: strings.Replace(base, `"pollIntervalSeconds"`, `"apiBaseURL":"http://evil.example","pollIntervalSeconds"`, 1)},
		{name: "unknown machine field", json: strings.Replace(base, `"sshUser":"dev"`, `"sshUser":"dev","strictHostKeyChecking":false`, 1)},
		{name: "relative state dir", json: strings.Replace(base, `"stateDir":"/var/lib/instabox-controller"`, `"stateDir":"var/lib/instabox-controller"`, 1)},
		{name: "state dir with dotdot", json: strings.Replace(base, `"stateDir":"/var/lib/instabox-controller"`, `"stateDir":"/var/lib/../instabox-controller"`, 1)},
		{name: "relative deadline path", json: strings.Replace(base, `"deadlinePath":"/var/lib/gha-runner-tui/state/session-deadline.json"`, `"deadlinePath":"session-deadline.json"`, 1)},
		{name: "deadline path with dotdot", json: strings.Replace(base, `"deadlinePath":"/var/lib/gha-runner-tui/state/session-deadline.json"`, `"deadlinePath":"/var/lib/../session-deadline.json"`, 1)},
		{name: "deadline path with space", json: strings.Replace(base, `"deadlinePath":"/var/lib/gha-runner-tui/state/session-deadline.json"`, `"deadlinePath":"/var/lib/session deadline.json"`, 1)},
		{name: "unsafe machine id", json: valid(validMachine("m_bad", "runner.example.net"))},
		{name: "host as ssh option", json: valid(validMachine(testMachineA, "-oProxyCommand=touch /tmp/x"))},
		{name: "host with space", json: valid(validMachine(testMachineA, "runner example.net"))},
		{name: "host with newline", json: strings.Replace(valid(validMachine(testMachineA, "runner.example.net")), "runner.example.net", `runner\nhost.example.net`, 1)},
		{name: "user with semicolon", json: strings.Replace(base, `"sshUser":"dev"`, `"sshUser":"dev;id"`, 1)},
		{name: "port out of range", json: strings.Replace(base, `"sshUser":"dev"`, `"sshUser":"dev","sshPort":70000`, 1)},
		{name: "relative identity file", json: strings.Replace(base, `"sshUser":"dev"`, `"sshUser":"dev","sshIdentityFile":"id_ed25519"`, 1)},
		{name: "no machines", json: strings.Replace(base, `"machines":[`+validMachine(testMachineA, "runner.example.net")+`]`, `"machines":[]`, 1)},
		{name: "duplicate machine", json: valid(validMachine(testMachineA, "runner.example.net") + "," + validMachine(testMachineA, "runner.example.net"))},
		{name: "poll too fast", json: strings.Replace(base, `"pollIntervalSeconds":30`, `"pollIntervalSeconds":5`, 1)},
		{name: "poll too slow", json: strings.Replace(base, `"pollIntervalSeconds":30`, `"pollIntervalSeconds":7200`, 1)},
		{name: "missing key field", json: `{"stateDir":"/var/lib/instabox-controller"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfigFile(t, tc.json)); err == nil {
				t.Fatal("expected the config to be rejected")
			}
		})
	}
}

func TestLoadConfigDefaultsSSHPort(t *testing.T) {
	raw := `{"stateDir":"/var/lib/instabox-controller","machines":[{"machineId":"` + testMachineA + `","deadlinePath":"/var/lib/session-deadline.json","sshHost":"runner.example.net","sshUser":"dev"}]}`
	cfg, err := LoadConfig(writeConfigFile(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Machines[0].SSHPort != 22 {
		t.Fatalf("port = %d", cfg.Machines[0].SSHPort)
	}
	if cfg.PollInterval().Seconds() != 30 {
		t.Fatalf("poll = %s", cfg.PollInterval())
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected an error")
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSSHArgumentsAndScriptsAreSafe(t *testing.T) {
	target := RemoteTarget{
		Host: "runner.example.net",
		User: "dev",
		Port: 2222,
		Path: "/var/lib/gha-runner-tui/state/session-deadline.json",
	}
	content := deadlinePayload(testNow.Add(120 * time.Minute))
	script, err := writeScript(target, content)
	if err != nil {
		t.Fatal(err)
	}
	args, err := sshArgs(target, script)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=10", "-p 2222", "-l dev", target.Host} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q do not contain %q", joined, want)
		}
	}
	if strings.Contains(joined, "StrictHostKeyChecking") || strings.Contains(joined, "UserKnownHostsFile") {
		t.Fatalf("host key checking was altered: %q", joined)
	}
	if args[len(args)-2] != target.Host {
		t.Fatalf("host is not an argument of its own: %q", args)
	}
	if !strings.Contains(script, base64EncodeForTest(content)) {
		t.Fatal("write script does not carry the deadline as inert base64")
	}
	if strings.Contains(script, `{"deadline"`) {
		t.Fatalf("write script contains raw JSON: %q", script)
	}
	if !strings.Contains(script, ".instabox-controller.tmp") || !strings.Contains(script, "mv -f --") {
		t.Fatalf("write script is not an atomic replacement: %q", script)
	}
	if got := readScript(target); !strings.HasPrefix(got, "cat -- '") {
		t.Fatalf("read script = %q", got)
	}

	badTargets := []RemoteTarget{
		{Host: "-oProxyCommand=touch", User: "dev", Port: 22, Path: "/var/lib/x.json"},
		{Host: "runner.example.net\n-oProxyCommand=id", User: "dev", Port: 22, Path: "/var/lib/x.json"},
		{Host: "runner example.net", User: "dev", Port: 22, Path: "/var/lib/x.json"},
		{Host: "runner.example.net", User: "dev;id", Port: 22, Path: "/var/lib/x.json"},
		{Host: "runner.example.net", User: "dev", Port: 0, Path: "/var/lib/x.json"},
		{Host: "runner.example.net", User: "dev", Port: 22, Path: "../../etc/passwd"},
		{Host: "runner.example.net", User: "dev", Port: 22, Path: "/var/lib/x.json;id"},
		{Host: "runner.example.net", User: "dev", Port: 22, IdentityFile: "relative-key", Path: "/var/lib/x.json"},
	}
	for _, bad := range badTargets {
		if _, err := sshArgs(bad, "true"); err == nil {
			t.Fatalf("unsafe target was accepted: %+v", bad)
		}
		if _, err := writeScript(bad, content); err == nil {
			t.Fatalf("unsafe target got a write script: %+v", bad)
		}
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellQuote = %q", got)
	}
}

func base64EncodeForTest(value string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	data := []byte(value)
	var out strings.Builder
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		out.WriteByte(alphabet[chunk[0]>>2])
		out.WriteByte(alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			out.WriteByte(alphabet[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(alphabet[chunk[2]&0x3f])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

func TestControllerLogsDoNotLeakTheKeyOrRawBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":"invalid_key","message":"bad ibk_secret_value"}}<html>ibk_secret_value</html>`)
	}))
	defer server.Close()
	cfg := testConfig(t, testMachineA)
	api := newAPIClient(server.URL, "ibk_secret_value", server.Client())
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return testNow }, logBuffer)
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the failed GET to be reported")
	}
	logs := logBuffer.String()
	if strings.Contains(logs, "ibk_secret_value") || strings.Contains(logs, "<html>") {
		t.Fatalf("logs leaked the key or raw body: %s", logs)
	}
	if !strings.Contains(logs, "GET failed") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestPostWriteGETFailureBlocksWake(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableRemoteWrite = true
	cfg.EnableWake = true
	started := testNow
	getCallsA := 0
	api := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) {
			if id == testMachineA {
				getCallsA++
				if getCallsA == 2 {
					return Machine{}, errors.New("post-write GET failed")
				}
				return runningMachine(testMachineA, started, 120), nil
			}
			return suspendedMachine(testMachineB, started.Add(-10*time.Minute)), nil
		},
		wakeFunc: func(ctx context.Context, id string) (Machine, error) {
			t.Fatalf("a failed query triggered a wake of %s", id)
			return Machine{}, nil
		},
	}
	controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the post-write GET failure to be reported")
	}
	if api.wakes != 0 {
		t.Fatalf("wakes = %d", api.wakes)
	}
}

func TestPostWriteDeadlineChangeIsNotReadyAndIsRedelivered(t *testing.T) {
	cfg := testConfig(t, testMachineA)
	cfg.EnableRemoteWrite = true
	started := testNow
	getCalls := 0
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		getCalls++
		if getCalls == 1 {
			return runningMachine(testMachineA, started, 120), nil
		}
		return runningMachine(testMachineA, started, 60), nil
	}}
	writer := &fakeWriter{}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, api, writer, func() time.Time { return started }, logBuffer)
	defer controller.Close()

	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the changed post-write deadline to be reported")
	}
	if writer.writes != 1 || !strings.Contains(logBuffer.String(), "budget-ready=false") {
		t.Fatalf("writes=%d log=%s", writer.writes, logBuffer.String())
	}
	logBuffer.Reset()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 2 {
		t.Fatalf("writes = %d, want the shortened deadline delivered", writer.writes)
	}
	if want := deadlinePayload(started.Add(60 * time.Minute)); writer.lastWrite != want {
		t.Fatalf("write = %q, want %q", writer.lastWrite, want)
	}
	if strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("a 60 minute run was reported ready: %s", logBuffer.String())
	}
}

func TestPersistFailureBlocksWake(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableWake = false
	started := testNow
	api := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) {
			if id == testMachineA {
				return runningMachine(testMachineA, started, 60), nil
			}
			return suspendedMachine(testMachineB, started.Add(-10*time.Minute)), nil
		},
		wakeFunc: func(ctx context.Context, id string) (Machine, error) {
			return Machine{}, nil
		},
	}
	controller := newController(cfg, api, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wakes != 0 {
		t.Fatalf("wake before enabling: %d", api.wakes)
	}
	controller.Config.EnableWake = true
	if err := os.Chmod(cfg.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(cfg.StateDir, 0o700)
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the failed attempt record to be reported")
	}
	if api.wakes != 0 {
		t.Fatalf("wake was sent without a persisted attempt: %d", api.wakes)
	}
}
